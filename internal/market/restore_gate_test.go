package market

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A serving App holds the serving lock on a connection of its own (A-505), so the restore gate host commands refuse
// while it runs. When that session ends (a database restart, an idle-session limit), the keeper takes the lock again
// on a new connection and the commands refuse again.
func TestServingLockRefusesRestoreGateCommandsAndIsTakenAgain(t *testing.T) {
	e := newTestApp(t)
	ctx := context.Background()
	holder := func() []int {
		t.Helper()
		var pids []int
		rows, err := e.DB.Query("SELECT pid FROM pg_locks WHERE locktype='advisory' AND classid=0 AND objid=$1::bigint::oid AND objsubid=1 AND granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())", servingLock)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var pid int
			if err = rows.Scan(&pid); err != nil {
				t.Fatal(err)
			}
			pids = append(pids, pid)
		}
		return pids
	}
	refused := func(when string) {
		t.Helper()
		if _, err := SetRestoreGate(ctx, e.DB); !errors.Is(err, errAppServing) {
			t.Fatalf("%s: set: %v", when, err)
		}
		if _, err := ClearRestoreGate(ctx, e.DB, "anyone", true); !errors.Is(err, errAppServing) {
			t.Fatalf("%s: clear: %v", when, err)
		}
		if n := agInt(e, "SELECT count(*) FROM settings WHERE key='payments_recovery_required'"); n != 0 {
			t.Fatalf("%s: the gate was written", when)
		}
	}
	first := holder()
	if len(first) != 1 {
		t.Fatalf("serving lock holders %v, want one", first)
	}
	refused("while serving")

	agExec(e, "SELECT pg_terminate_backend($1)", first[0])
	deadline := time.Now().Add(10 * time.Second)
	for len(holder()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the serving lock outlived its session")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// With the session gone the commands pass the lock check (and refuse later: no such administrator).
	if _, err := ClearRestoreGate(ctx, e.DB, "anyone", false); err == nil || errors.Is(err, errAppServing) {
		t.Fatalf("clear with the lock lost: %v", err)
	}
	e.A.refreshServingLock(ctx)
	again := holder()
	if len(again) != 1 || again[0] == first[0] {
		t.Fatalf("serving lock holders after the keeper %v (before %v)", again, first)
	}
	refused("after the keeper took the lock again")
	// A check while the lock is held changes nothing.
	e.A.refreshServingLock(ctx)
	if now := holder(); len(now) != 1 || now[0] != again[0] {
		t.Fatalf("serving lock holders after a check %v, want %v", now, again)
	}
}
