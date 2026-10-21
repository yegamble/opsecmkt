package market

import (
	"context"
	"errors"
	"strings"
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
	// The serving session names itself, and a refusal says how many sessions hold the lock, not where they are.
	if n := agInt(e, "SELECT count(*) FROM pg_stat_activity WHERE pid=$1 AND application_name='opsecmkt-serving'", again[0]); n != 1 {
		t.Fatal("the serving session is not named opsecmkt-serving")
	}
	if _, err := SetRestoreGate(ctx, e.DB); err == nil || !strings.Contains(err.Error(), ": 1 database session(s) hold the serving lock (session start, UTC: 20") {
		t.Fatalf("refusal: %v", err)
	}
}

// The serving connection ignores the session limits a role or database may set (SRE-1): with the role's lock_timeout
// at 1 s a start waits for a restore gate host command up to MIGRATION_LOCK_TIMEOUT and then says why, and with the
// database's idle_session_timeout at 2 s the idle serving session, and with it the lock, outlives the limit (no
// keeper runs here: the App is not started).
func TestServingLockIgnoresRoleSessionLimits(t *testing.T) {
	dsn := testMigratedDSN(t)
	admin, err := OpenDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var name string
	if err = admin.QueryRow("SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "opsecmkt_copy_") {
		t.Skip("needs a database of its own (CREATEDB) to change its settings")
	}
	for _, q := range []string{
		"ALTER ROLE CURRENT_USER IN DATABASE " + name + " SET lock_timeout = '1s'",
		"ALTER DATABASE " + name + " SET idle_session_timeout = '2s'",
	} {
		if _, err = admin.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { admin.Exec("ALTER ROLE CURRENT_USER IN DATABASE " + name + " RESET ALL") })
	check, err := OpenDB(dsn) // new sessions, under the limits
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	check.SetMaxIdleConns(0)
	holders := func() int {
		t.Helper()
		var n int
		if err := check.QueryRow("SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND classid=0 AND objid=782499 AND objsubid=1 AND granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A host command holds the lock exclusively: the start waits past the role's 1 s lock_timeout, up to
	// MIGRATION_LOCK_TIMEOUT, and names the restore gate host command.
	ctx := context.Background()
	command, err := check.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// (This stand-in for the command's transaction must outlive the idle-session limit itself.)
	if _, err = command.ExecContext(ctx, "SET idle_session_timeout = 0; SELECT pg_advisory_lock(782499)"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("SETUP_TOKEN", testSetupToken)
	t.Setenv("APP_SECRET_KEY", "")
	t.Setenv("COMPOSE_FILE", "")
	t.Setenv("COOKIE_SECURE", "false")
	t.Setenv("MIGRATION_LOCK_TIMEOUT", "3s")
	began := time.Now()
	a, err := New(ctx, false)
	if err == nil {
		a.Close()
		t.Fatal("started while a restore gate host command held the serving lock")
	}
	if waited := time.Since(began); waited < 2500*time.Millisecond || !strings.Contains(err.Error(), "waiting for a restore gate host command to finish timed out after 3s") {
		t.Fatalf("start after %s: %v", waited, err)
	}
	if _, err = command.ExecContext(ctx, "SELECT pg_advisory_unlock(782499)"); err != nil {
		t.Fatal(err)
	}
	command.Close()

	// Served past the idle-session limit, the lock is still held and the commands refuse.
	startTestApp(t, dsn)
	time.Sleep(3500 * time.Millisecond)
	if n := holders(); n != 1 {
		t.Fatalf("serving lock holders after the idle-session limit: %d", n)
	}
	if _, err := SetRestoreGate(ctx, check); !errors.Is(err, errAppServing) {
		t.Fatalf("set while serving past the idle-session limit: %v", err)
	}
	if _, err := ClearRestoreGate(ctx, check, "anyone", false); !errors.Is(err, errAppServing) {
		t.Fatalf("clear while serving past the idle-session limit: %v", err)
	}
}
