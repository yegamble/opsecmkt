package market

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestRetentionInstanceBindingCannotBeReplaced(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	pin := agStr(e, `SELECT value FROM settings WHERE key='retention_instance_id'`)
	other := &App{db: e.DB, retention: &retentionService{state: retentionState{InstanceID: randomToken()}}}
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = other.bindRetentionInstance(t.Context(), tx); !errors.Is(err, errRetentionStateUnavailable) {
		t.Fatalf("binding allowed a different retention instance: %v", err)
	}
	tx.Rollback()
	if agStr(e, `SELECT value FROM settings WHERE key='retention_instance_id'`) != pin {
		t.Fatal("failed identity binding replaced the database instance")
	}
	tx, err = e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = e.A.bindRetentionInstance(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionEqualSequenceWrongInstanceCannotMutate(t *testing.T) {
	e, cfg := uiRetentionEnv(t)
	uid, _ := e.user("retention_wrong_instance_user", "buyer")
	s := e.A.retention
	unlock, err := s.lock()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.read(); err == nil {
		s.state.InstanceID = randomToken()
		err = s.write()
	}
	sequence := s.state.Sequence
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, `UPDATE settings SET value=$1 WHERE key='retention_manifest_sequence'`, strconv.FormatUint(sequence, 10))
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.A.requestAccountDeletionTx(t.Context(), tx, uid)
	tx.Rollback()
	if !errors.Is(err, errRetentionStateUnavailable) {
		t.Fatalf("equal-sequence ledger from another instance accepted deletion: %v", err)
	}
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND deletion_requested_at IS NOT NULL`, uid) != 0 || agInt(e, `SELECT count(*) FROM retention_operations`) != 0 {
		t.Fatal("rejected foreign-instance request changed database state")
	}
	if err = e.A.runRetention(t.Context()); !errors.Is(err, errRetentionStateUnavailable) {
		t.Fatalf("maintenance accepted foreign-instance ledger: %v", err)
	}
	if err = e.A.finalizeRetentionOperation(t.Context(), "unknown"); !errors.Is(err, errRetentionStateUnavailable) {
		t.Fatalf("finalization accepted foreign-instance ledger: %v", err)
	}
	if err = e.A.abortRetentionOperation(t.Context(), "unknown"); !errors.Is(err, errRetentionStateUnavailable) {
		t.Fatalf("abort accepted foreign-instance ledger: %v", err)
	}
	if err = e.A.initRetention(t.Context(), cfg); err == nil {
		t.Fatal("startup accepted foreign-instance ledger")
	}
}

func TestRetentionConcurrentFirstEnrollmentBindsExactlyOneInstance(t *testing.T) {
	e := newTestApp(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	gate, err := e.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	if _, err = gate.ExecContext(ctx, `SELECT pg_advisory_lock(198273641)`); err != nil {
		t.Fatal(err)
	}
	defer gate.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(198273641)`)
	agExec(e, `CREATE FUNCTION retention_test_first_bind_gate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='retention_instance_id' THEN PERFORM pg_advisory_xact_lock(198273641); END IF; RETURN NEW; END $$`)
	agExec(e, `CREATE TRIGGER retention_test_first_bind_gate BEFORE INSERT ON settings FOR EACH ROW EXECUTE FUNCTION retention_test_first_bind_gate()`)
	apps := []*App{{db: e.DB}, {db: e.DB}}
	configs := []retentionConfig{{Enabled: true, StateDir: retentionPrivateTestDir(t)}, {Enabled: true, StateDir: retentionPrivateTestDir(t)}}
	results := make(chan error, 2)
	for index, app := range apps {
		go func(a *App, cfg retentionConfig) { results <- a.initRetention(ctx, cfg) }(app, configs[index])
	}
	for {
		var waiting int
		if err = e.DB.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=198273641 AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 2 {
			break
		}
		select {
		case err = <-results:
			t.Fatalf("initial enrollment ended before both binding attempts overlapped: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, err = gate.ExecContext(ctx, `SELECT pg_advisory_unlock(198273641)`); err != nil {
		t.Fatal(err)
	}
	won, refused := 0, 0
	for range apps {
		err = <-results
		if err == nil {
			won++
		} else if errors.Is(err, errRetentionStateUnavailable) {
			refused++
		} else {
			t.Fatalf("unexpected concurrent enrollment failure: %v", err)
		}
	}
	if won != 1 || refused != 1 {
		t.Fatalf("concurrent enrollment had %d successes and %d identity refusals", won, refused)
	}
	pin := agStr(e, `SELECT value FROM settings WHERE key='retention_instance_id'`)
	if apps[0].retention.state.InstanceID == apps[1].retention.state.InstanceID || pin != apps[0].retention.state.InstanceID && pin != apps[1].retention.state.InstanceID {
		t.Fatal("first enrollment failed to preserve exactly one distinct instance binding")
	}
}
