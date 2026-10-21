package market

import (
	"strconv"
	"testing"
)

// SEC72Y regressions: the rescan order is a key the account cannot move, a
// deleted source releases its marker count at once, and stopping capture
// clears the rescan position.

// An account that edits its other listings and bumps the scam listing's
// change time (digital units, archive/restore, a photo, a price edit) cannot
// push it back in its own rescan: the walk is by creation time.
func TestModerationRescanOrderNotSteerableByChangeTime(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("steer_vendor", "vendor")
	moderationEnable(t, e)
	if err := ConfigureModerationLimits(t.Context(), e.DB, ModerationLimits{MaxPending: 256, MaxRecords: 400, MaxBytes: 256 << 20}); err != nil {
		t.Fatal(err)
	}
	agExec(e, `UPDATE moderation_capacity SET max_actor_markers=0`)
	const share = 8
	w := moderationTestWorker(e)
	var decoys []string
	for range 12 {
		decoys = append(decoys, e.product(vendor, "digital"))
	}
	moderationPasses(t, w, 6) // spends the share on the oldest decoys
	scam := e.product(vendor, "digital")
	agExec(e, `UPDATE products SET description=$2 WHERE id=$1`, scam, moderationScamText)
	// k: the account's records created before S that have no job when the rescan reaches them.
	k := agInt(e, `SELECT count(*) FROM products p WHERE p.vendor_id=$1 AND p.id<>$2 AND NOT EXISTS(SELECT 1 FROM moderation_jobs j WHERE j.target_id=p.id)`, vendor, scam)
	windows := (k + share - 1) / share
	if windows < 1 {
		windows = 1
	}
	for window := range windows + 1 {
		for i, d := range decoys {
			agExec(e, `UPDATE products SET description=$2,updated=now() WHERE id=$1`, d, "Decoy revision "+strconv.Itoa(window)+"-"+strconv.Itoa(i))
		}
		// Change-time bumps without new content (digital units, a photo on a
		// pending listing, archive and restore), and a 1-piconero re-edit.
		agExec(e, `UPDATE products SET stock=stock+1,updated=now() WHERE id=$1`, scam)
		agExec(e, `UPDATE products SET archived=true,archived_at=now(),updated=now() WHERE id=$1`, scam)
		agExec(e, `UPDATE products SET archived=false,archived_at=NULL,updated=now() WHERE id=$1`, scam)
		agExec(e, `UPDATE products SET updated=now() WHERE id=$1`, scam)
		agExec(e, `UPDATE products SET xmr=xmr+1 WHERE id=$1`, scam)
		agExec(e, `UPDATE moderation_actor_allocations SET window_start=now()-interval '25 hours' WHERE actor_id=$1`, vendor)
		moderationPasses(t, w, 4)
		if moderationScamCaptured(e, "listing", scam) {
			if window+1 > windows {
				t.Fatalf("scam listing captured in window %d, bound ceil(k/share)=%d (k=%d)", window+1, windows, k)
			}
			return
		}
	}
	t.Fatalf("scam listing not captured within ceil(k/share)=%d windows (k=%d)", windows, k)
}

// Deleting a marked record releases the account's marker count (and parked
// count) in the same transaction, so no phantom backlog defers its writes.
func TestModerationDeletedSourceReleasesMarkerCount(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("release_vendor", "vendor")
	moderationEnable(t, e)
	for range 3 {
		e.product(vendor, "digital")
	}
	active := e.product(vendor, "digital")
	parked := e.product(vendor, "digital")
	agExec(e, `UPDATE products SET archived=true,archived_at=now() WHERE id=$1`, parked)
	agExec(e, `UPDATE moderation_backfill SET parked=true WHERE target_id=$1`, parked)
	agExec(e, `UPDATE moderation_actor_allocations SET parked=1 WHERE actor_id=$1`, vendor)
	counts := func() string {
		return agStr(e, `SELECT markers||'/'||parked FROM moderation_actor_allocations WHERE actor_id=$1`, vendor)
	}
	if got := counts(); got != "2/1" {
		t.Fatalf("precondition: counts=%s want 2/1", got)
	}
	agExec(e, `DELETE FROM products WHERE id=$1`, active)
	if got := counts(); got != "1/1" {
		t.Fatalf("deleting an actively marked listing left counts=%s, want 1/1", got)
	}
	agExec(e, `DELETE FROM products WHERE id=$1`, parked)
	if got := counts(); got != "0/0" {
		t.Fatalf("deleting a parked listing left counts=%s, want 0/0", got)
	}
	if agInt(e, `SELECT count(*) FROM moderation_backfill`) != 0 || agInt(e, `SELECT backfill FROM moderation_capacity`) != 0 {
		t.Fatal("deleted sources kept their markers")
	}
	// With no phantom backlog, the account's next write is admitted directly
	// once it has an active slot.
	agExec(e, `UPDATE moderation_jobs SET completed_at=now() WHERE completed_at IS NULL`)
	next := e.product(vendor, "digital")
	if agInt(e, `SELECT count(*) FROM moderation_jobs WHERE target_id=$1`, next) != 1 {
		t.Fatal("a released account's write was still deferred")
	}
}

// Stopping capture clears the rescan position but keeps the rescan, which
// restarts from the beginning on re-enable without duplicating jobs.
func TestModerationDisableClearsRescanPosition(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("disable_cursor_vendor", "vendor")
	agExec(e, `UPDATE moderation_capacity SET max_actor_markers=0`)
	moderationEnable(t, e)
	var listings []string
	for range 6 {
		listings = append(listings, e.product(vendor, "digital"))
	}
	w := moderationTestWorker(e)
	moderationPasses(t, w, 2)
	agExec(e, `UPDATE moderation_actor_allocations SET rescan=true,rescan_ts=p.created,rescan_kind='listing',rescan_id=p.id FROM products p WHERE actor_id=$1 AND p.id=$2`, vendor, listings[0])
	if err := SetModerationEnabled(t.Context(), e.DB, false); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM moderation_actor_allocations WHERE actor_id=$1 AND rescan AND rescan_ts IS NULL AND rescan_kind IS NULL AND rescan_id IS NULL`, vendor) != 1 {
		t.Fatal("stopping capture kept the rescan position or dropped the rescan")
	}
	moderationEnable(t, e)
	moderationDrain(t, e, w)
	if agInt(e, `SELECT count(*) FROM (SELECT 1 FROM moderation_jobs GROUP BY kind,target_id,content_hash HAVING count(*)>1) d`) != 0 {
		t.Fatal("restarting the rescan duplicated jobs")
	}
	for _, p := range listings {
		if agInt(e, `SELECT count(*) FROM moderation_jobs WHERE target_id=$1 AND content_hash=$2`, p, moderationLiveDigest(e, "listing", p)) != 1 {
			t.Fatalf("listing %s not captured exactly once", p)
		}
	}
}
