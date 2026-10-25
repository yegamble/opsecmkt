package market

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// SEC72Z regressions: archiving or hiding a record no longer withdraws it from
// capture, and deleting a marked record never takes the global capacity lock
// mid-transaction.

func TestModerationArchiveDuringWindowStillCaptured(t *testing.T) {
	for _, rescan := range []bool{false, true} {
		t.Run(map[bool]string{false: "marker", true: "rescan"}[rescan], func(t *testing.T) {
			e := newTestApp(t)
			vendor, _ := e.user("archive_window_vendor", "vendor")
			moderationEnable(t, e)
			if err := ConfigureModerationLimits(t.Context(), e.DB, ModerationLimits{MaxPending: 256, MaxRecords: 400, MaxBytes: 256 << 20}); err != nil {
				t.Fatal(err)
			}
			if rescan {
				agExec(e, `UPDATE moderation_capacity SET max_actor_markers=0`)
			}
			w := moderationTestWorker(e)
			for range 12 {
				e.product(vendor, "digital")
			}
			moderationPasses(t, w, 6) // spends the share
			scam := e.product(vendor, "digital")
			agExec(e, `UPDATE products SET description=$2 WHERE id=$1`, scam, moderationScamText)
			// S is archived while the next window's share is spent, then restored.
			agExec(e, `UPDATE products SET archived=true,archived_at=now() WHERE id=$1`, scam)
			agExec(e, `UPDATE moderation_actor_allocations SET window_start=now()-interval '25 hours' WHERE actor_id=$1`, vendor)
			moderationPasses(t, w, 1)
			for _, n := range []int{3, 3, 2} {
				for range n {
					e.product(vendor, "digital")
				}
				moderationPasses(t, w, 1)
			}
			agExec(e, `UPDATE products SET archived=false,archived_at=NULL WHERE id=$1`, scam)
			moderationPasses(t, w, 2)
			if !moderationScamCaptured(e, "listing", scam) {
				t.Fatal("archiving the scam listing during its window kept it uncaptured")
			}
		})
	}
	t.Run("honest archived record", func(t *testing.T) {
		e := newTestApp(t)
		vendor, _ := e.user("archive_honest_vendor", "vendor")
		buyer, _ := e.user("archive_honest_buyer", "buyer")
		product := e.product(vendor, "digital")
		orders := []string{e.order(buyer, product, "XMR", stateCompleted), e.order(buyer, product, "XMR", stateCompleted), e.order(buyer, product, "XMR", stateCompleted), e.order(buyer, product, "XMR", stateCompleted)}
		moderationEnable(t, e)
		for range 3 {
			e.product(vendor, "digital")
		}
		archived := e.product(vendor, "digital")
		agExec(e, `UPDATE products SET archived=true,archived_at=now() WHERE id=$1`, archived)
		for i := range 3 {
			agExec(e, `INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body) VALUES($1,$2,$3,$4,4,'Fine')`, randomToken(), orders[i], product, buyer)
		}
		hidden := randomToken()
		agExec(e, `INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body) VALUES($1,$2,$3,$4,2,'Late again')`, hidden, orders[3], product, buyer)
		agExec(e, `UPDATE reviews SET hidden_at=now() WHERE id=$1`, hidden)
		w := moderationTestWorker(e)
		moderationDrain(t, e, w)
		if agInt(e, `SELECT count(*) FROM moderation_jobs WHERE target_id=$1 AND content_hash=$2`, archived, moderationLiveDigest(e, "listing", archived)) != 1 {
			t.Fatal("an archived listing was not captured")
		}
		if agInt(e, `SELECT count(*) FROM moderation_jobs WHERE target_id=$1 AND content_hash=$2`, hidden, moderationLiveDigest(e, "review", hidden)) != 1 {
			t.Fatal("a hidden review was not captured")
		}
		if agInt(e, `SELECT count(*) FROM moderation_backfill`) != 0 {
			t.Fatal("an archived or hidden record stayed waiting and kept the account's backlog")
		}
		next := e.product(vendor, "digital")
		if agInt(e, `SELECT count(*) FROM moderation_jobs WHERE target_id=$1`, next) != 1 {
			t.Fatal("the account's next write was deferred after its archived record")
		}
	})
}

func TestModerationDeleteTriggerNoDeadlock(t *testing.T) {
	e := newTestApp(t)
	moderationEnable(t, e)
	// Deterministic lock order: a transaction that deletes a marked listing and
	// then touches listing L must not deadlock with a vendor editing L.
	owner, _ := e.user("delete_order_owner", "vendor")
	for range 3 {
		e.product(owner, "digital")
	}
	marked := e.product(owner, "digital")
	other, _ := e.user("delete_order_other", "vendor")
	l := e.product(other, "digital")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	tx1, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback()
	if _, err = tx1.ExecContext(ctx, `DELETE FROM products WHERE id=$1`, marked); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	go func() {
		_, err := e.DB.ExecContext(ctx, `UPDATE products SET description='Concurrent vendor edit' WHERE id=$1`, l)
		errs <- err
	}()
	time.Sleep(500 * time.Millisecond)
	if _, err = tx1.ExecContext(ctx, `UPDATE products SET title='Withdrawn listing',description='' WHERE id=$1`, l); err == nil {
		err = tx1.Commit()
	}
	if err != nil {
		t.Fatal("deleting transaction failed:", err)
	}
	if err = <-errs; err != nil {
		t.Fatal("concurrent vendor edit failed:", err)
	}

	// Stress: worker passes, retention-like delete+withdraw batches, inserts,
	// edits and capture toggles; five short runs, no deadlock.
	var vendors []string
	for i := range 12 {
		v, _ := e.user("delete_stress_"+string(rune('a'+i)), "vendor")
		vendors = append(vendors, v)
		for range 6 {
			e.product(v, "digital")
		}
	}
	var deadlocks atomic.Int64
	note := func(ctx context.Context, err error) {
		if err != nil && ctx.Err() == nil && strings.Contains(err.Error(), "40P01") {
			deadlocks.Add(1)
			t.Log(err)
		}
	}
	for range 5 {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := moderationTestWorker(e)
				for ctx.Err() == nil {
					_, err := w.Pass(ctx, 5)
					note(ctx, err)
				}
			}()
		}
		for g := range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := g; ctx.Err() == nil; i += 3 {
					tx, err := e.DB.BeginTx(ctx, nil)
					if err != nil {
						return
					}
					_, err = tx.ExecContext(ctx, `DELETE FROM products WHERE id=(SELECT id FROM products WHERE vendor_id=$1 ORDER BY random() LIMIT 1)`, vendors[i%len(vendors)])
					if err == nil {
						_, err = tx.ExecContext(ctx, `UPDATE products SET title='Withdrawn listing',description=md5(random()::text) WHERE id=(SELECT id FROM products WHERE vendor_id=$1 ORDER BY random() LIMIT 1)`, vendors[(i+5)%len(vendors)])
					}
					if err == nil {
						err = tx.Commit()
					} else {
						_ = tx.Rollback()
					}
					note(ctx, err)
					_, err = e.DB.ExecContext(ctx, `INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES(md5(random()::text),$1,'t','d','Hardware','Worldwide','digital',1,500000000000,1)`, vendors[i%len(vendors)])
					note(ctx, err)
				}
			}()
		}
		for g := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := g; ctx.Err() == nil; i++ {
					_, err := e.DB.ExecContext(ctx, `UPDATE products SET description=md5(random()::text) WHERE id=(SELECT id FROM products WHERE vendor_id=$1 ORDER BY random() LIMIT 1)`, vendors[i%len(vendors)])
					note(ctx, err)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			on := false
			for ctx.Err() == nil {
				note(ctx, SetModerationEnabled(ctx, e.DB, on))
				on = !on
				time.Sleep(50 * time.Millisecond)
			}
		}()
		wg.Wait()
		cancel()
	}
	if n := deadlocks.Load(); n != 0 {
		t.Fatalf("%d deadlocks", n)
	}
	// After a cleanup pass, stored counts match the live markers.
	moderationEnable(t, e)
	if _, err := moderationBackfillSweep(t.Context(), e.DB, 0); err != nil {
		t.Fatal(err)
	}
	if got := agInt(e, `SELECT count(*) FROM moderation_actor_allocations a WHERE a.markers<>moderation_actor_marker_count(a.actor_id,false) OR a.parked<>moderation_actor_marker_count(a.actor_id,true)`); got != 0 {
		t.Fatalf("%d accounts' marker counts drifted", got)
	}
	if agInt(e, `SELECT backfill FROM moderation_capacity`) != agInt(e, `SELECT count(*) FROM moderation_backfill`) {
		t.Fatal("the marker counter drifted from the marker table")
	}
}
