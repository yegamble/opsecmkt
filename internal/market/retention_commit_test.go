package market

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// Simulate a maintenance worker obtaining the manifest lock immediately after
// the verified deletion request's DB commit, before HTTP finalization can run.
func TestRetentionCommittedRequestRemainsAcceptedDuringFinalizeContention(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	uid, session := e.user("retention_finalize_contention", "buyer")
	base, err := stdlib.GetDefaultDriver().(driver.DriverContext).OpenConnector(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	var release func()
	fired := false
	wrapped := sql.OpenDB(&retentionCommitConnector{Connector: base, after: func() {
		if fired || agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND deletion_requested_at IS NOT NULL`, uid) == 0 {
			return
		}
		fired = true
		var lockErr error
		release, lockErr = e.A.retention.lock()
		if lockErr != nil {
			t.Fatal(lockErr)
		}
	}})
	original := e.A.db
	e.A.db = wrapped
	t.Cleanup(func() {
		if release != nil {
			release()
		}
		original.Close()
	})
	e.check(e.do("POST", "/account/delete", session, url.Values{"confirm_delete": {"yes"}, "password": {testPassword}}), 303)
	if !fired || release == nil {
		t.Fatal("test did not contend after the durable request commit")
	}
	if agInt(e, `SELECT count(*) FROM retention_operations`) != 1 || agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND deletion_requested_at IS NOT NULL`, uid) != 1 {
		t.Fatal("successful HTTP response was not backed by a committed request marker")
	}
	release()
	release = nil
	if err = e.A.runRetention(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(e.A.retention.state.Pending) != 0 || len(e.A.retention.state.Requests) != 1 {
		t.Fatal("maintenance did not finalize the accepted durable request")
	}
}

type retentionCommitConnector struct {
	driver.Connector
	after func()
}

func (c *retentionCommitConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &retentionCommitConn{lostCommitConn: &lostCommitConn{Conn: conn, lost: &atomic.Bool{}}, after: c.after}, nil
}

type retentionCommitConn struct {
	*lostCommitConn
	after func()
}

func (c *retentionCommitConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &retentionCommitTx{Tx: tx, after: c.after}, nil
}
func (c *retentionCommitConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

type retentionCommitTx struct {
	driver.Tx
	after func()
}

func (tx *retentionCommitTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	tx.after()
	return nil
}

func TestRetentionStateFailurePausesMutationAndMarksHealthUnhealthy(t *testing.T) {
	e, cfg := uiRetentionEnv(t)
	_, session := e.user("retention_state_failure_user", "buyer")
	if err := os.Remove(cfg.StateDir + "/retention-state.json"); err != nil {
		t.Fatal(err)
	}
	if err := e.A.runRetention(t.Context()); err == nil {
		t.Fatal("missing external state did not fail maintenance")
	}
	if e.A.retentionMaintenanceError() != "state_unavailable" {
		t.Fatal("external-state failure did not become an actionable maintenance class")
	}
	e.check(e.do("POST", "/revoke-sessions", session, nil), 503)
	e.check(e.do("GET", "/healthz", session, nil), 503)
}

func TestRetentionStateDirectoryFailurePausesMutationAndMarksHealthUnhealthy(t *testing.T) {
	e, cfg := uiRetentionEnv(t)
	_, session := e.user("retention_directory_failure_user", "buyer")
	if err := os.Rename(cfg.StateDir, cfg.StateDir+"-unavailable"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Rename(cfg.StateDir+"-unavailable", cfg.StateDir) })
	if err := e.A.runRetention(t.Context()); !errors.Is(err, errRetentionStateUnavailable) {
		t.Fatalf("unavailable lock directory did not fail closed: %v", err)
	}
	e.check(e.do("POST", "/revoke-sessions", session, nil), 503)
	e.check(e.do("GET", "/healthz", session, nil), 503)
}

func TestRetentionUnavailableStateStaysPausedThroughBusyRetry(t *testing.T) {
	e, cfg := uiRetentionEnv(t)
	_, session := e.user("retention_sticky_state_failure_user", "buyer")
	path := cfg.StateDir + "/retention-state.json"
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = e.A.runRetention(t.Context()); !errors.Is(err, errRetentionStateUnavailable) {
		t.Fatalf("missing manifest did not fail closed: %v", err)
	}
	unlock, err := e.A.retention.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	for _, repaired := range []bool{false, true} {
		if repaired {
			if err = os.WriteFile(path, current, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err = e.A.runRetention(t.Context()); !errors.Is(err, errRetentionBusy) {
			t.Fatalf("expected lock contention before validation: %v", err)
		}
		if e.A.retentionMaintenanceError() != "state_unavailable" || agStr(e, `SELECT value FROM settings WHERE key='retention_last_error'`) != "state_unavailable" {
			t.Fatal("busy retry downgraded an unvalidated external-state failure")
		}
		e.check(e.do("POST", "/revoke-sessions", session, nil), 503)
		e.check(e.do("GET", "/healthz", session, nil), 503)
	}
	unlock()
	unlock = nil
	if err = e.A.runRetention(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.A.retentionMaintenanceError() != "" || agStr(e, `SELECT value FROM settings WHERE key='retention_last_error'`) != "" {
		t.Fatal("successful maintenance failed to clear a repaired state fault")
	}
	e.check(e.do("GET", "/healthz", session, nil), 200)
	e.check(e.do("POST", "/revoke-sessions", session, nil), 303)
}

func TestRetentionCrossVendorObligationsRefuseLockContentionWithoutDeadlock(t *testing.T) {
	e := newTestApp(t)
	first, _ := e.user("retention_cross_vendor_first", "vendor")
	second, _ := e.user("retention_cross_vendor_second", "vendor")
	firstProduct := e.product(first, "physical")
	secondProduct := e.product(second, "physical")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	one, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Rollback()
	two, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Rollback()
	for _, actor := range []struct {
		tx *sql.Tx
		id string
	}{{one, first}, {two, second}} {
		if _, err = actor.tx.ExecContext(ctx, `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, actor.id); err != nil {
			t.Fatal(err)
		}
	}
	results := make(chan error, 2)
	for _, draft := range []struct {
		tx             *sql.Tx
		buyer, product string
	}{{one, first, secondProduct}, {two, second, firstProduct}} {
		go func() {
			_, err := draft.tx.ExecContext(ctx, `INSERT INTO orders(id,buyer_id,product_id,currency,amount) VALUES($1,$2,$3,'XMR',100000)`, randomToken(), draft.buyer, draft.product)
			results <- err
		}()
	}
	contended := 0
	for i := 0; i < 2; i++ {
		err = <-results
		// PostgreSQL releases an aborted transaction's row locks, so the other
		// checkout can legitimately succeed after the first NOWAIT refusal.
		if err == nil {
			continue
		}
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "55P03" || !dbBusyForContext(ctx, err) {
			t.Fatalf("cross checkout should return retriable lock contention, got %v", err)
		}
		contended++
	}
	if contended == 0 {
		t.Fatal("cross-locked checkouts never exercised contention")
	}
}
