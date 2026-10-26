package market

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// SEC72AA-1 (A-647) regressions: a marketplace transaction never waits on the
// global moderation_capacity row. Admission takes it with SKIP LOCKED and, when
// another transaction holds it, leaves only a reference row in
// moderation_deferred, which the next worker pass admits through normal
// admission. Worker and staff transactions take the capacity row first.

// moderationHoldCapacity holds the capacity row in another session, as a worker
// pass, a persist or a staff review does, until release is called.
func moderationHoldCapacity(t *testing.T, e *testEnv) (release func()) {
	t.Helper()
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`SELECT 1 FROM moderation_capacity WHERE singleton FOR UPDATE`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback() }) }
	t.Cleanup(release)
	return release
}

// moderationAwaitLockWait returns once a session of this test's database waits
// on a lock, or once done is closed (the other session finished without waiting).
func moderationAwaitLockWait(t *testing.T, e *testEnv, done <-chan struct{}) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		select {
		case <-done:
			return
		default:
		}
		if agInt(e, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`) > 0 {
			return
		}
	}
	t.Fatal("the concurrent session neither finished nor waited on a lock")
}

// moderationCovered reports whether the current revision of a record is covered
// by a job, a marker, a deferred row or its actor's rescan flag.
func moderationCovered(e *testEnv, kind, target string) bool {
	return agInt(e, `SELECT count(*) FROM moderation_capture_source($1,$2) c WHERE
	 EXISTS(SELECT 1 FROM moderation_jobs WHERE kind=$1 AND target_id=$2 AND content_hash=c.digest)
	 OR EXISTS(SELECT 1 FROM moderation_backfill WHERE kind=$1 AND target_id=$2)
	 OR EXISTS(SELECT 1 FROM moderation_deferred WHERE kind=$1 AND target_id=$2)
	 OR EXISTS(SELECT 1 FROM moderation_actor_allocations WHERE actor_id=c.actor AND rescan)`, kind, target) == 1
}

func moderationIsDeadlock(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "40P01") || strings.Contains(err.Error(), "deadlock"))
}

// moderationCountersExact checks the stored counters against the rows they count.
func moderationCountersExact(t *testing.T, e *testEnv) {
	t.Helper()
	for _, c := range []struct{ name, stored, actual string }{
		{"pending", `SELECT pending FROM moderation_capacity`, `SELECT count(*) FROM moderation_jobs WHERE completed_at IS NULL`},
		{"allocated_records", `SELECT allocated_records FROM moderation_capacity`, `SELECT COALESCE(sum(reserved_results),0) FROM moderation_jobs`},
		{"allocated_bytes", `SELECT allocated_bytes/524288 FROM moderation_capacity`, `SELECT COALESCE(sum(reserved_results),0) FROM moderation_jobs`},
		{"backfill", `SELECT backfill FROM moderation_capacity`, `SELECT (SELECT count(*) FROM moderation_backfill)+(SELECT count(*) FROM moderation_marker_releases)`},
		{"actor markers", `SELECT COALESCE(sum(markers),0) FROM moderation_actor_allocations`, `SELECT count(*) FROM moderation_backfill`},
	} {
		if s, a := agInt(e, c.stored), agInt(e, c.actual); s != a {
			t.Errorf("%s counter=%d, rows=%d", c.name, s, a)
		}
	}
}

func TestModerationAdmissionNeverWaitsOnCapacity(t *testing.T) {
	t.Run("held capacity defers a reference only", func(t *testing.T) {
		e := newTestApp(t)
		moderationEnable(t, e)
		vendor, _ := e.user("nowait_held_vendor", "vendor")
		edited := e.product(vendor, "digital") // admitted: the vendor has an allocation row
		state := `SELECT (SELECT to_jsonb(c)::text FROM moderation_capacity c)||(SELECT COALESCE(sum(occurrences),0)::text FROM moderation_overflow)
		 ||(SELECT COALESCE(jsonb_agg(to_jsonb(a) ORDER BY actor_id)::text,'') FROM moderation_actor_allocations a)
		 ||(SELECT count(*)::text FROM moderation_backfill)||(SELECT count(*)::text FROM moderation_jobs)`
		before := agStr(e, state)
		release := moderationHoldCapacity(t, e)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if _, err := e.DB.ExecContext(ctx, `UPDATE products SET description='Edited while the review queue is busy' WHERE id=$1`, edited); err != nil {
			t.Fatal("a vendor edit waited on the capacity row:", err)
		}
		fresh := randomToken()
		if _, err := e.DB.ExecContext(ctx, `INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES($1,$2,'New listing','Busy queue','Hardware','Worldwide','digital',1,1,1)`, fresh, vendor); err != nil {
			t.Fatal("a new listing waited on the capacity row:", err)
		}
		if agInt(e, `SELECT count(*) FROM moderation_deferred WHERE kind='listing' AND target_id IN ($1,$2)`, edited, fresh) != 2 {
			t.Fatal("a capture refused for a busy capacity row left no deferred reference")
		}
		release()
		if after := agStr(e, state); after != before {
			t.Fatalf("a deferred capture touched counters, overflow notes, allocations, markers or jobs:\n%s\n%s", before, after)
		}
		if got := agStr(e, `SELECT string_agg(column_name,',' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_name='moderation_deferred'`); got != "kind,target_id,noted" {
			t.Fatalf("moderation_deferred columns %q hold more than a reference", got)
		}
		if agInt(e, `SELECT count(*) FROM moderation_deferred d JOIN moderation_actor_allocations a ON a.xmin=d.xmin`)+
			agInt(e, `SELECT count(*) FROM moderation_deferred d JOIN moderation_marker_releases r ON r.xmin=d.xmin`) != 0 {
			t.Fatal("a deferred row shares its transaction with an account record")
		}
	})
	for _, second := range []string{"moderated edit", "stock only"} {
		t.Run(second+" of a second listing", func(t *testing.T) {
			e := newTestApp(t)
			moderationEnable(t, e)
			v1, _ := e.user("nowait_first_vendor", "vendor")
			v2, _ := e.user("nowait_second_vendor", "vendor")
			a, b := e.product(v1, "digital"), e.product(v2, "digital")
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			tx1, err := e.DB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx1.Rollback()
			// tx1 admits one moderated change: it holds the capacity row.
			if _, err = tx1.ExecContext(ctx, `UPDATE products SET description='First admitted revision' WHERE id=$1`, a); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var editErr error
			go func() {
				defer close(done)
				_, editErr = e.DB.ExecContext(ctx, `UPDATE products SET description='Concurrent vendor edit' WHERE id=$1`, b)
			}()
			moderationAwaitLockWait(t, e, done)
			q := `UPDATE products SET title='Second row of the first transaction' WHERE id=$1`
			if second == "stock only" {
				q = `UPDATE products SET stock=stock+1 WHERE id=$1`
			}
			_, err1 := tx1.ExecContext(ctx, q, b)
			if err1 == nil {
				err1 = tx1.Commit()
			}
			<-done
			if err1 != nil || editErr != nil {
				t.Fatalf("first transaction: %v; concurrent vendor edit: %v", err1, editErr)
			}
			for _, id := range []string{a, b} {
				if !moderationCovered(e, "listing", id) {
					t.Fatalf("listing %s's current revision is not covered", id)
				}
			}
		})
	}
}

func TestModerationDeferredDrainIsLossless(t *testing.T) {
	t.Run("archive, delete, unflag and re-edit before the drain", func(t *testing.T) {
		e := newTestApp(t)
		moderationEnable(t, e)
		vendor := func(name string) string { id, _ := e.user(name, "vendor"); return id }
		archived := e.product(vendor("drain_archived_vendor"), "digital")
		deleted := e.product(vendor("drain_deleted_vendor"), "digital")
		twice := e.product(vendor("drain_twice_vendor"), "digital")
		buyer, _ := e.user("drain_buyer", "buyer")
		order := e.order(buyer, e.product(vendor("drain_order_vendor"), "digital"), "XMR", stateCompleted)
		overflow := agInt(e, `SELECT COALESCE(sum(occurrences),0) FROM moderation_overflow`)
		release := moderationHoldCapacity(t, e)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		exec := func(q string, args ...any) {
			t.Helper()
			if _, err := e.DB.ExecContext(ctx, q, args...); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		exec(`UPDATE products SET description='Revision before archiving' WHERE id=$1`, archived)
		exec(`UPDATE products SET archived=true,archived_at=now() WHERE id=$1`, archived)
		exec(`UPDATE products SET description='Revision before deletion' WHERE id=$1`, deleted)
		// Source deletion removes the deferred row without the capacity lock.
		exec(`DELETE FROM products WHERE id=$1`, deleted)
		if agInt(e, `SELECT count(*) FROM moderation_deferred WHERE target_id=$1`, deleted) != 0 {
			t.Fatal("deleting a deferred record left its reference")
		}
		exec(`UPDATE products SET description='First deferred revision' WHERE id=$1`, twice)
		exec(`UPDATE products SET description='Latest deferred revision' WHERE id=$1`, twice)
		var payment string
		if err := e.DB.QueryRowContext(ctx, `INSERT INTO payments(order_id,currency,txid,idx,address,amount,flagged) VALUES($1,'XMR',$2,0,$2,1,true) RETURNING id::text`, order, randomToken()).Scan(&payment); err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE payments SET flagged=false WHERE id::text=$1`, payment)
		if got := agInt(e, `SELECT count(*) FROM moderation_deferred`); got != 3 {
			t.Fatalf("deferred rows=%d, want 3 (archived, twice-edited, unflagged payment)", got)
		}
		release()
		if agInt(e, `SELECT COALESCE(sum(occurrences),0) FROM moderation_overflow`) != overflow {
			t.Fatal("a deferred capture noted an overflow")
		}
		// Disabled capture keeps the references.
		if err := SetModerationEnabled(t.Context(), e.DB, false); err != nil {
			t.Fatal(err)
		}
		if n := agInt(e, `SELECT moderation_deferred_drain(100)`); n != 0 || agInt(e, `SELECT count(*) FROM moderation_deferred`) != 3 {
			t.Fatalf("drain with capture disabled admitted %d or dropped references", n)
		}
		moderationEnable(t, e)
		if n := agInt(e, `SELECT moderation_deferred_drain(100)`); n != 2 {
			t.Fatalf("drain admitted %d records, want 2", n)
		}
		if agInt(e, `SELECT count(*) FROM moderation_deferred`) != 0 {
			t.Fatal("the drain left deferred references")
		}
		for _, id := range []string{archived, twice} {
			if agInt(e, `SELECT count(*) FROM moderation_jobs WHERE kind='listing' AND target_id=$1 AND content_hash=$2`, id, moderationLiveDigest(e, "listing", id)) != 1 {
				t.Fatalf("listing %s's current revision was not captured", id)
			}
		}
		if agInt(e, `SELECT count(*) FROM moderation_jobs WHERE target_id=$1`, twice) != 2 {
			t.Fatal("an intermediate deferred revision was captured")
		}
		if agInt(e, `SELECT count(*) FROM moderation_jobs WHERE target_id IN ($1,$2)`, deleted, payment) != 1 {
			t.Fatal("a deleted record gained a job, or an unflagged payment was captured")
		}
		moderationCountersExact(t, e)
	})
	t.Run("sweep drains before its pending early return", func(t *testing.T) {
		e := newTestApp(t)
		if err := ConfigureModerationLimits(t.Context(), e.DB, ModerationLimits{MaxPending: 2, MaxRecords: 4096, MaxBytes: 256 << 20}); err != nil {
			t.Fatal(err)
		}
		moderationEnable(t, e)
		v1, _ := e.user("drain_sweep_first", "vendor")
		v2, _ := e.user("drain_sweep_second", "vendor")
		e.product(v1, "digital") // pending=1: the sweep's 50% early return applies
		release := moderationHoldCapacity(t, e)
		deferred := randomToken()
		agExec(e, `INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES($1,$2,'Deferred listing','Busy queue','Hardware','Worldwide','digital',1,1,1)`, deferred, v2)
		release()
		if _, err := moderationBackfillSweep(t.Context(), e.DB, 20); err != nil {
			t.Fatal(err)
		}
		if agInt(e, `SELECT count(*) FROM moderation_jobs WHERE target_id=$1`, deferred) != 1 || agInt(e, `SELECT count(*) FROM moderation_deferred`) != 0 {
			t.Fatal("the sweep did not admit the deferred record before its pending early return")
		}
		moderationCountersExact(t, e)
	})
}

func TestModerationPersistLockOrder(t *testing.T) {
	e := newTestApp(t)
	moderationEnable(t, e)
	v1, _ := e.user("persist_order_first", "vendor")
	v2, _ := e.user("persist_order_second", "vendor")
	b := e.product(v2, "digital") // the worker's job
	a := e.product(v1, "digital")
	w := moderationTestWorker(e)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	job, err := w.claim(ctx)
	if err != nil || job.TargetID != b {
		t.Fatalf("claimed %q (want %q): %v", job.TargetID, b, err)
	}
	tx1, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback()
	if _, err = tx1.ExecContext(ctx, `UPDATE products SET description='Admitted while the worker persists' WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var processErr error
	go func() {
		defer close(done)
		processErr = w.process(ctx, job)
	}()
	moderationAwaitLockWait(t, e, done)
	_, err1 := tx1.ExecContext(ctx, `UPDATE products SET stock=stock+1 WHERE id=$1`, b)
	if err1 == nil {
		err1 = tx1.Commit()
	}
	<-done
	if err1 != nil || processErr != nil {
		t.Fatalf("marketplace transaction: %v; worker persist: %v", err1, processErr)
	}
	if agInt(e, `SELECT count(*) FROM moderation_jobs WHERE id=$1 AND completed_at IS NOT NULL`, job.ID) != 1 {
		t.Fatal("the worker did not record its result")
	}
	moderationCountersExact(t, e)
}

func TestModerationReviewActionLockOrder(t *testing.T) {
	e, _, admin, product, id := moderationAdminFixture(t, 0, false)
	other, _ := e.user("review_order_other_vendor", "vendor")
	a := e.product(other, "digital")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	tx1, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback()
	if _, err = tx1.ExecContext(ctx, `UPDATE products SET description='Admitted while staff review' WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var code int
	go func() {
		defer close(done)
		code = privateHostRequest(e, "POST", moderationReviewPath, admin, moderationReviewForm(id, "accept"), true).Code
	}()
	moderationAwaitLockWait(t, e, done)
	_, err1 := tx1.ExecContext(ctx, `UPDATE products SET stock=stock+1 WHERE id=$1`, product)
	if err1 == nil {
		err1 = tx1.Commit()
	}
	<-done
	if err1 != nil || code != 303 {
		t.Fatalf("marketplace transaction: %v; review status %d", err1, code)
	}
	if agInt(e, `SELECT count(*) FROM moderation_reviews WHERE suggestion_id=$1 AND action='accept'`, id) != 1 {
		t.Fatal("the review was not recorded")
	}
}

// A dispute resolution admits the dispute's change and then locks the order; the
// Monero watcher locks the order and then flags a locked deposit. With capture
// enabled the two never deadlock.
func TestModerationAdmissionResolveVsWatcherFlagNoDeadlock(t *testing.T) {
	w := newDisputeOrderWorld(t)
	_, admin := w.e.user("nowait_resolver_admin_"+randomToken()[:6], "admin")
	order, dispute := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "The licence key was never delivered to me.")
	seen := resolverSeen(w, order)
	agExec(w.e, `INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,locked) SELECT $1,'XMR',$2,idx,address,1000,50,true FROM payments WHERE order_id=$1 LIMIT 1`, order, hex64("nowait-locked-"+order))
	moderationEnable(t, w.e)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	watcher, err := w.e.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Rollback()
	o, err := scanOrder(watcher.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", order))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var code int
	var body string
	go func() {
		defer close(done)
		r := w.e.do("POST", "/resolve", admin, resolverForm(dispute, "refund", seen))
		code, body = r.Code, r.Body.String()
	}()
	moderationAwaitLockWait(t, w.e, done) // the resolution admitted its change and waits for the order
	err = w.e.A.flagLockedXMR(ctx, watcher, &o)
	if err == nil {
		err = watcher.Commit()
	}
	<-done
	if moderationIsDeadlock(err) || strings.Contains(body, "40P01") {
		t.Fatalf("deadlock: watcher %v; resolve %d", err, code)
	}
	if err != nil || code != 303 {
		t.Fatalf("watcher flag: %v; resolve %d %s", err, code, body)
	}
	if w.state(order) != stateResolved || agInt(w.e, `SELECT count(*) FROM payments WHERE order_id=$1 AND locked AND flagged`, order) != 1 {
		t.Fatal("the resolution or the flag was not recorded")
	}
	// The resolution held the capacity row, so the flag left a reference that the
	// next pass admits at the payment's current revision.
	payment := agStr(w.e, `SELECT id::text FROM payments WHERE order_id=$1 AND locked`, order)
	if agInt(w.e, `SELECT count(*) FROM moderation_deferred WHERE kind='payment_flag' AND target_id=$1`, payment) != 1 {
		t.Fatal("the flag that found the capacity row busy left no deferred reference")
	}
	// Capture was enabled after the dispute opened: the resolution is its first capture.
	if agInt(w.e, `SELECT count(*) FROM moderation_jobs WHERE kind='dispute' AND target_id=$1`, dispute)+agInt(w.e, `SELECT count(*) FROM moderation_backfill WHERE kind='dispute' AND target_id=$1`, dispute) != 1 {
		t.Fatal("the resolution's capture was lost")
	}
	agExec(w.e, `SELECT moderation_deferred_drain(10)`)
	if !moderationCovered(w.e, "payment_flag", payment) {
		t.Fatal("the drained flag is covered by no job, marker or rescan")
	}
	moderationCountersExact(t, w.e)
}

// moderationFirstSQL returns the SQL text of the first ExecContext, QueryContext
// or QueryRowContext call, in source order, in the named function of file.
func moderationFirstSQL(t *testing.T, file, function string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != function {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || found != "" {
				return found == ""
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			switch sel.Sel.Name {
			case "ExecContext", "QueryContext", "QueryRowContext":
				if lit, ok := call.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					found, _ = strconv.Unquote(lit.Value)
				} else {
					found = "<non-literal>"
				}
				return false
			}
			return true
		})
	}
	if found == "" {
		t.Fatalf("%s: no SQL call in %s", file, function)
	}
	return found
}

// Worker and staff transactions take the capacity row before any job or
// marketplace row.
func TestModerationCapacityLockIsFirstStatement(t *testing.T) {
	const lock = `SELECT 1 FROM moderation_capacity WHERE singleton FOR UPDATE`
	for _, c := range []struct{ file, function string }{
		{"internal/market/moderation_worker.go", "persist"},
		{"internal/market/moderation_admin.go", "moderationReviewAction"},
		{"internal/market/moderation_token.go", "moderationOfflineAction"},
		{"internal/market/moderation_budget.go", "moderationBudgetAction"},
	} {
		if got := moderationFirstSQL(t, c.file, c.function); got != lock {
			t.Errorf("%s: the first statement of %s is %q, not the capacity lock", c.file, c.function, got)
		}
	}
	// SetModerationEnabled first reads the catalog (no row lock) to learn which tables exist.
	if got := moderationFirstSQL(t, "internal/market/moderation_worker.go", "SetModerationEnabled"); !strings.HasPrefix(got, "SELECT to_regclass(") {
		t.Errorf("SetModerationEnabled first statement %q", got)
	}
	raw, err := os.ReadFile("internal/market/moderation_worker.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "func SetModerationEnabled(")
	end := strings.Index(src[start:], "\n}\n")
	body := src[start : start+end]
	if l, s := strings.Index(body, lock), strings.Index(body, "UPDATE settings"); l < 0 || s < 0 || l > s {
		t.Error("SetModerationEnabled does not take the capacity row before the settings row")
	}
}
