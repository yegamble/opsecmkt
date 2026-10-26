package market

import (
	"strconv"
	"testing"
)

// SEC72V regressions: refused captures stay recoverable through archive/hide,
// cannot be deferred or starved by an actor's own refused backlog, and
// context-only staleness does not re-capture an unchanged revision.

func moderationEnable(t *testing.T, e *testEnv) {
	t.Helper()
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
}

func moderationPasses(t *testing.T, w *ModerationWorker, n int) {
	t.Helper()
	for range n {
		if _, err := w.Pass(t.Context(), 20); err != nil {
			t.Fatal(err)
		}
	}
}

func moderationScamCaptured(e *testEnv, kind, target string) bool {
	return agInt(e, `SELECT count(*) FROM moderation_suggestions WHERE kind=$1 AND target_id=$2 AND content_hash=$3 AND failure_class='' AND reason_codes?'SCAM_TEMPLATE'`, kind, target, moderationLiveDigest(e, kind, target)) == 1
}

func TestModerationArchivedRestoreRecaptured(t *testing.T) {
	for _, disabledRestore := range []bool{false, true} {
		t.Run(map[bool]string{false: "enabled", true: "restored while disabled"}[disabledRestore], func(t *testing.T) {
			e := newTestApp(t)
			vendor, _ := e.user("archive_restore_vendor", "vendor")
			moderationEnable(t, e)
			for range 3 {
				e.product(vendor, "digital")
			}
			scam := e.product(vendor, "digital")
			agExec(e, `UPDATE products SET description=$2 WHERE id=$1`, scam, moderationScamText)
			if agInt(e, `SELECT count(*) FROM moderation_backfill WHERE target_id=$1`, scam) != 1 {
				t.Fatal("precondition: actor cap left no marker")
			}
			agExec(e, `UPDATE products SET archived=true,archived_at=now() WHERE id=$1`, scam)
			w := moderationTestWorker(e)
			moderationPasses(t, w, 3)
			// Archiving does not withdraw a record from capture.
			if !moderationScamCaptured(e, "listing", scam) {
				t.Fatal("archived scam listing was not captured")
			}
			if disabledRestore {
				if err := SetModerationEnabled(t.Context(), e.DB, false); err != nil {
					t.Fatal(err)
				}
			}
			agExec(e, `UPDATE products SET archived=false,archived_at=NULL WHERE id=$1`, scam)
			if disabledRestore {
				moderationEnable(t, e)
			}
			moderationDrain(t, e, w)
			if !moderationScamCaptured(e, "listing", scam) {
				t.Fatal("restored scam listing was never evaluated")
			}
			if agInt(e, `SELECT count(*) FROM moderation_backfill`) != 0 {
				t.Fatal("captured marker remained")
			}
		})
	}
}

func TestModerationHiddenReviewUnhideRecaptured(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("unhide_vendor", "vendor")
	buyer, _ := e.user("unhide_buyer", "buyer")
	product := e.product(vendor, "digital")
	orders := make([]string, 4)
	for i := range orders {
		orders[i] = e.order(buyer, product, "XMR", stateCompleted)
	}
	moderationEnable(t, e)
	for i := range 3 {
		agExec(e, `INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body) VALUES($1,$2,$3,$4,4,$5)`, randomToken(), orders[i], product, buyer, "Arrived as described "+strconv.Itoa(i))
	}
	review := randomToken()
	agExec(e, `INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body) VALUES($1,$2,$3,$4,5,$5)`, review, orders[3], product, buyer, moderationScamText)
	if agInt(e, `SELECT count(*) FROM moderation_backfill WHERE kind='review' AND target_id=$1`, review) != 1 {
		t.Fatal("precondition: actor cap left no marker")
	}
	agExec(e, `UPDATE reviews SET hidden_at=now() WHERE id=$1`, review)
	w := moderationTestWorker(e)
	moderationPasses(t, w, 3)
	// Hiding does not withdraw a review from capture.
	if !moderationScamCaptured(e, "review", review) {
		t.Fatal("hidden scam review was not captured")
	}
	agExec(e, `UPDATE reviews SET hidden_at=NULL WHERE id=$1`, review)
	moderationDrain(t, e, w)
	if !moderationScamCaptured(e, "review", review) {
		t.Fatal("unhidden scam review was never evaluated")
	}
}

// An over-share vendor bumping its refused scam listing behind fresh decoys
// before each window reset must not defer it: first-refusal order holds.
func TestModerationEditDoesNotDeferOwnMarker(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("defer_vendor", "vendor")
	moderationEnable(t, e)
	if err := ConfigureModerationLimits(t.Context(), e.DB, ModerationLimits{MaxPending: 256, MaxRecords: 400, MaxBytes: 256 << 20}); err != nil {
		t.Fatal(err)
	}
	const share = 8
	w := moderationTestWorker(e)
	for range share {
		e.product(vendor, "digital")
		moderationPasses(t, w, 1)
	}
	scam := e.product(vendor, "digital")
	agExec(e, `UPDATE products SET description=$2 WHERE id=$1`, scam, moderationScamText)
	for range share + 2 {
		e.product(vendor, "digital")
	}
	agExec(e, `UPDATE products SET xmr=xmr+1 WHERE id=$1`, scam)
	agExec(e, `UPDATE moderation_actor_allocations SET window_start=now()-interval '25 hours' WHERE actor_id=$1`, vendor)
	moderationPasses(t, w, 1)
	if !moderationScamCaptured(e, "listing", scam) {
		t.Fatal("the first window with share available did not capture the oldest refused target")
	}
}

// An actor past its marker quota leaves one rescan flag instead of more
// markers; the rescan still captures every live target. One actor never
// reaches backfill_capacity.
func TestModerationActorMarkerQuotaRescan(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("quota_vendor", "vendor")
	agExec(e, `UPDATE moderation_capacity SET max_backfill=3`)
	moderationEnable(t, e)
	var listings []string
	for range 3 {
		listings = append(listings, e.product(vendor, "digital"))
	}
	for i := range 7 {
		p := e.product(vendor, "digital")
		if i == 6 {
			agExec(e, `UPDATE products SET description=$2 WHERE id=$1`, p, moderationScamText)
		}
		listings = append(listings, p)
	}
	if got := agInt(e, `SELECT count(*) FROM moderation_backfill`); got != 2 {
		t.Fatalf("one actor holds %d markers, quota is max_backfill-1=2", got)
	}
	if agInt(e, `SELECT count(*) FROM moderation_actor_allocations WHERE actor_id=$1 AND rescan AND markers=2`, vendor) != 1 {
		t.Fatal("quota overflow did not set one actor-level rescan")
	}
	if agInt(e, `SELECT occurrences FROM moderation_overflow WHERE kind='listing' AND reason='backfill_capacity'`) != 0 {
		t.Fatal("one actor reached backfill_capacity")
	}
	moderationDrain(t, e, moderationTestWorker(e))
	for _, p := range listings {
		if agInt(e, `SELECT count(*) FROM moderation_jobs WHERE target_id=$1 AND content_hash=$2`, p, moderationLiveDigest(e, "listing", p)) != 1 {
			t.Fatalf("listing %s was never captured", p)
		}
	}
	if !moderationScamCaptured(e, "listing", listings[len(listings)-1]) {
		t.Fatal("scam listing past the quota was never evaluated")
	}
	if agInt(e, `SELECT count(*) FROM moderation_actor_allocations WHERE rescan OR markers<>0`) != 0 {
		t.Fatal("rescan flag or marker count survived the rescan")
	}
}

// Refused actors' markers ahead of an honest one do not spend the sweep's
// budget: the honest marker is captured in one pass.
func TestModerationRefusedBacklogDoesNotStarveSweep(t *testing.T) {
	e := newTestApp(t)
	moderationEnable(t, e)
	for i := range 3 {
		a, _ := e.user("backlog_actor_"+strconv.Itoa(i), "vendor")
		for range 3 + 16 {
			e.product(a, "digital")
		}
	}
	honest, _ := e.user("backlog_honest", "vendor")
	for range 3 {
		e.product(honest, "digital")
	}
	target := e.product(honest, "digital")
	if got := agInt(e, `SELECT count(*) FROM moderation_backfill`); got != 49 {
		t.Fatalf("precondition: markers=%d want 48 refused + 1 honest", got)
	}
	// The honest actor's own active jobs finish; the backlog actors' stay active.
	agExec(e, `UPDATE moderation_jobs SET completed_at=now() WHERE completed_at IS NULL AND actor_id=$1`, honest)
	if _, err := moderationBackfillSweep(t.Context(), e.DB, 20); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM moderation_jobs WHERE target_id=$1 AND content_hash=$2`, target, moderationLiveDigest(e, "listing", target)) != 1 {
		t.Fatal("a refused backlog starved the honest marker")
	}
}

// A refusal during an active rescan, for a record the scan already passed,
// repeats the scan instead of losing that record.
func TestModerationActorRescanRepeatsAfterRefusalDuringScan(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("rescan_repeat_vendor", "vendor")
	agExec(e, `UPDATE moderation_capacity SET max_backfill=3`)
	moderationEnable(t, e)
	for range 5 {
		e.product(vendor, "digital")
	}
	target := e.product(vendor, "digital")
	if agInt(e, `SELECT count(*) FROM moderation_actor_allocations WHERE actor_id=$1 AND rescan AND NOT rescan_more`, vendor) != 1 {
		t.Fatal("precondition: one fresh rescan expected")
	}
	agExec(e, `UPDATE moderation_actor_allocations a SET rescan_ts=p.updated,rescan_kind='listing',rescan_id=p.id FROM products p WHERE a.actor_id=$1 AND p.id=$2`, vendor, target)
	agExec(e, `UPDATE products SET description=$2 WHERE id=$1`, target, moderationScamText)
	if agInt(e, `SELECT count(*) FROM moderation_actor_allocations WHERE actor_id=$1 AND rescan_more`, vendor) != 1 {
		t.Fatal("a refusal during the rescan did not request another cycle")
	}
	moderationDrain(t, e, moderationTestWorker(e))
	if !moderationScamCaptured(e, "listing", target) {
		t.Fatal("a record refused behind the rescan position was lost")
	}
}

// A category-median change while a listing job is evaluated makes it stale
// while the victim's own revision is unchanged. That stays a HOLD for humans; it
// must not re-capture the victim, spend its share or feed its abuse-rate signal.
// Listings never reach a model (A-651), so the change lands between the
// worker's input read and its persist, where inference used to run.
func TestModerationContextStaleDoesNotRecapture(t *testing.T) {
	e := newTestApp(t)
	victim, _ := e.user("context_stale_victim", "vendor")
	other, _ := e.user("context_stale_other", "vendor")
	otherListing := e.product(other, "digital")
	moderationEnable(t, e)
	listing := e.product(victim, "digital")
	w := moderationTestWorker(e)
	w.WorkerID = "context-stale-worker"
	j, err := w.claim(t.Context())
	if err != nil || j.TargetID != listing {
		t.Fatalf("claim: %+v %v", j, err)
	}
	in, err := w.input(t.Context(), j)
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, `UPDATE products SET xmr=400000000000 WHERE id=$1`, otherListing)
	d, err := w.Engine.Evaluate(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.persist(t.Context(), j, moderationPolicyHash(w.Engine.Config), in, d); err != nil {
		t.Fatal(err)
	}
	moderationPasses(t, w, 8)
	if agInt(e, `SELECT count(*) FROM moderation_suggestions WHERE target_id=$1 AND failure_class='stale_snapshot'`, listing) != 1 {
		t.Fatal("precondition: the median change did not make the victim's job stale")
	}
	if agInt(e, `SELECT count(*) FROM moderation_backfill WHERE target_id=$1`, listing) != 0 {
		t.Fatal("context-only staleness left a marker for an unchanged revision")
	}
	if got := agInt(e, `SELECT count(*) FROM moderation_jobs WHERE target_id=$1`, listing); got != 1 {
		t.Fatalf("unchanged victim listing captured %d times", got)
	}
	if got := agInt(e, `SELECT allocations FROM moderation_actor_allocations WHERE actor_id=$1`, victim); got != 1 {
		t.Fatalf("victim share spent %d times", got)
	}
	if agInt(e, `SELECT count(*) FROM moderation_suggestions WHERE target_id=$1 AND reason_codes?'ABUSE_RATE'`, listing) != 0 {
		t.Fatal("context-only staleness fed the victim's abuse-rate signal")
	}
}
