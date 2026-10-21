package market

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// SEC72W regressions: an actor with a capture backlog cannot jump its own
// queue with fresh writes; the sweep survives sources removed under it;
// disabling capture never deadlocks with the sweep or account erasure; and a
// rescan step costs O(budget), not O(all of an actor's records).

func TestModerationBacklogActorCannotJumpOwnQueue(t *testing.T) {
	for _, rescan := range []bool{false, true} {
		t.Run(map[bool]string{false: "marker", true: "rescan"}[rescan], func(t *testing.T) {
			e := newTestApp(t)
			vendor, _ := e.user("queue_jump_vendor", "vendor")
			moderationEnable(t, e)
			if err := ConfigureModerationLimits(t.Context(), e.DB, ModerationLimits{MaxPending: 256, MaxRecords: 400, MaxBytes: 256 << 20}); err != nil {
				t.Fatal(err)
			}
			if rescan {
				agExec(e, `UPDATE moderation_capacity SET max_actor_markers=0`)
			}
			w := moderationTestWorker(e)
			for range 8 {
				e.product(vendor, "digital")
				moderationPasses(t, w, 1)
			}
			scam := e.product(vendor, "digital")
			agExec(e, `UPDATE products SET description=$2 WHERE id=$1`, scam, moderationScamText)
			if rescan {
				if agInt(e, `SELECT count(*) FROM moderation_actor_allocations WHERE actor_id=$1 AND rescan`, vendor) != 1 {
					t.Fatal("precondition: the refused scam listing should be under a rescan")
				}
			} else if agInt(e, `SELECT count(*) FROM moderation_backfill WHERE target_id=$1`, scam) != 1 {
				t.Fatal("precondition: the refused scam listing should have a marker")
			}
			// Cycle 0: the window resets and the vendor writes decoys before
			// each sweep, as the attack did.
			agExec(e, `UPDATE moderation_actor_allocations SET window_start=now()-interval '25 hours' WHERE actor_id=$1`, vendor)
			var decoys []string
			for _, n := range []int{3, 3, 2} {
				for range n {
					decoys = append(decoys, e.product(vendor, "digital"))
				}
				moderationPasses(t, w, 1)
			}
			if !moderationScamCaptured(e, "listing", scam) {
				t.Fatal("decoys written after the reset kept the scam listing from being captured")
			}
			scamJob := agInt(e, `SELECT min(id) FROM moderation_jobs WHERE target_id=$1`, scam)
			for _, d := range decoys {
				if id := agInt(e, `SELECT coalesce(min(id),0) FROM moderation_jobs WHERE target_id=$1`, d); id != 0 && id < scamJob {
					t.Fatal("a decoy written after the reset was queued ahead of the scam listing")
				}
			}
		})
	}
}

// A capture whose actor is unknown (its source vanished) is skipped, never an
// error that stops the worker.
func TestModerationAdmissionSkipsUnknownActor(t *testing.T) {
	e := newTestApp(t)
	moderationEnable(t, e)
	if _, err := e.DB.Exec(`INSERT INTO moderation_jobs(kind,target_id,actor_id,content_hash,snapshot,metadata,actor_ref,capture_version) VALUES('listing','vanished',NULL,$1,'{}','{}',$1,2)`, strings.Repeat("a", 64)); err != nil {
		t.Fatal("admission of an unknown actor raised:", err)
	}
	if agInt(e, `SELECT count(*) FROM moderation_jobs`)+agInt(e, `SELECT count(*) FROM moderation_actor_allocations`) != 0 {
		t.Fatal("an unknown actor left a job or allocation")
	}
}

func TestModerationSweepSurvivesConcurrentSourceRemoval(t *testing.T) {
	e := newTestApp(t)
	limits := DefaultModerationLimits()
	limits.MaxPending = 1
	if err := ConfigureModerationLimits(t.Context(), e.DB, limits); err != nil {
		t.Fatal(err)
	}
	moderationEnable(t, e)
	first, _ := e.user("removal_race_first", "vendor")
	e.product(first, "digital")
	var targets []string
	for i := range 24 {
		v, _ := e.user("removal_race_"+strconv.Itoa(i), "vendor")
		targets = append(targets, e.product(v, "digital"))
	}
	agExec(e, `UPDATE moderation_jobs SET completed_at=now() WHERE completed_at IS NULL`)
	limits.MaxPending = 256
	if err := ConfigureModerationLimits(t.Context(), e.DB, limits); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 64)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 8 {
				if _, err := moderationBackfillSweep(t.Context(), e.DB, 2); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i, p := range targets {
			q := `DELETE FROM products WHERE id=$1`
			if i%2 == 1 {
				q = `UPDATE products SET archived=true,archived_at=now() WHERE id=$1`
			}
			if _, err := e.DB.Exec(q, p); err != nil {
				errs <- err
			}
		}
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal("sweep or removal failed:", err)
	}
	if _, err := moderationBackfillSweep(t.Context(), e.DB, 32); err != nil {
		t.Fatal(err)
	}
	if got := agInt(e, `SELECT count(*) FROM moderation_backfill WHERE NOT parked`); got != 0 {
		t.Fatalf("%d active markers left for removed sources", got)
	}
}

// Disabling capture, the sweep and real account erasure order their row locks
// alike: a bounded stress run sees no deadlock.
func TestModerationDisableSweepNoDeadlock(t *testing.T) {
	e := newTestApp(t)
	moderationEnable(t, e)
	var vendors, idle []string
	for i := range 12 {
		v, _ := e.user("deadlock_vendor_"+strconv.Itoa(i), "vendor")
		for range 5 {
			e.product(v, "digital")
		}
		vendors = append(vendors, v)
		u, _ := e.user("deadlock_idle_"+strconv.Itoa(i), "buyer")
		agExec(e, `DELETE FROM sessions WHERE user_id=$1`, u)
		agExec(e, `INSERT INTO moderation_actor_allocations(actor_id,window_start,allocations,markers,rescan) VALUES($1,now()-interval '2 days',3,0,true)`, u)
		idle = append(idle, u)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	var deadlocks, erased atomic.Int64
	note := func(err error) {
		if err != nil && (strings.Contains(err.Error(), "40P01") || strings.Contains(err.Error(), "deadlock")) {
			deadlocks.Add(1)
			t.Log(err)
		}
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_, err := moderationBackfillSweep(ctx, e.DB, 20)
				note(err)
				_, err = e.DB.ExecContext(ctx, `UPDATE moderation_jobs SET completed_at=now() WHERE id IN (SELECT id FROM moderation_jobs WHERE completed_at IS NULL LIMIT 5)`)
				note(err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		on := false
		for ctx.Err() == nil {
			note(SetModerationEnabled(ctx, e.DB, on))
			on = !on
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Like a retention batch: erase an account (cascading its moderation
		// row), then withdraw another vendor's listing in the same transaction.
		for i, u := range idle {
			if ctx.Err() != nil {
				return
			}
			tx, err := e.DB.BeginTx(ctx, nil)
			if err != nil {
				return
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, u)
			if err == nil {
				_, err = tx.ExecContext(ctx, `UPDATE products SET title='Withdrawn listing',description='' WHERE id=(SELECT id FROM products WHERE vendor_id=$1 ORDER BY id LIMIT 1)`, vendors[i])
			}
			if err == nil {
				if err = tx.Commit(); err == nil {
					erased.Add(1)
				}
			} else {
				_ = tx.Rollback()
			}
			note(err)
			time.Sleep(100 * time.Millisecond)
		}
	}()
	wg.Wait()
	if n := deadlocks.Load(); n != 0 {
		t.Fatalf("%d deadlocks between disable, sweep and erasure", n)
	}
	if erased.Load() == 0 {
		t.Fatal("precondition: no erasure transaction committed")
	}
}

// Opt-in load measurement: MODERATION_LOAD_TEST=1. Four rescanning vendors with
// 50000 listings each; a sweep must stay well inside its 5 s timeout.
func TestModerationRescanLoad(t *testing.T) {
	if os.Getenv("MODERATION_LOAD_TEST") != "1" {
		t.Skip("set MODERATION_LOAD_TEST=1 to measure rescan cost")
	}
	e := newTestApp(t)
	var actors []string
	for i := range 4 {
		v, _ := e.user("rescan_load_"+strconv.Itoa(i), "vendor")
		actors = append(actors, v)
		for c := 0; c < 50000; c += 2500 {
			agExec(e, `INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
			 SELECT md5($1||g::text),$1,'Bulk listing','Bulk fixture '||g,'Hardware','Worldwide','digital',100000,500000000000,5 FROM generate_series($2::int+1,$2::int+2500) g`, v, c)
		}
	}
	moderationEnable(t, e)
	for _, v := range actors {
		agExec(e, `INSERT INTO moderation_actor_allocations(actor_id,rescan) VALUES($1,true)`, v)
	}
	var worst time.Duration
	for i := range 5 {
		start := time.Now()
		n, err := moderationBackfillSweep(t.Context(), e.DB, 20)
		took := time.Since(start)
		worst = max(worst, took)
		t.Logf("sweep %d: admitted=%d err=%v took=%s", i, n, err, took)
		if err != nil {
			t.Fatal(err)
		}
		agExec(e, `UPDATE moderation_jobs SET completed_at=now() WHERE completed_at IS NULL`)
	}
	if worst > time.Second {
		t.Fatalf("worst sweep took %s with 4 x 50000 rescanning listings", worst)
	}
}
