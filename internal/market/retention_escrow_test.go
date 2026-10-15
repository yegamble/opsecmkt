package market

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func runRetentionClosure(t *testing.T, e *testEnv, now time.Time) {
	t.Helper()
	tx, err := e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = e.A.retentionFinancialClosure(context.Background(), tx, now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func retentionClosureAt(t *testing.T, e *testEnv, order string) sql.NullTime {
	t.Helper()
	var at sql.NullTime
	if err := e.DB.QueryRow("SELECT financial_closed_at FROM orders WHERE id=$1", order).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func ageRetentionOrder(t *testing.T, e *testEnv, order string, at time.Time) {
	t.Helper()
	agExec(e, "UPDATE orders SET updated=$2 WHERE id=$1", order, at)
	agExec(e, "UPDATE order_events SET created=$2 WHERE order_id=$1 AND from_state<>to_state", order, at)
}

func retentionExceptionReason(t *testing.T, e *testEnv, order string) string {
	t.Helper()
	var reason string
	if err := e.DB.QueryRow("SELECT reason FROM retention_escrow_exceptions WHERE order_id=$1 AND resolved_at IS NULL", order).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	return reason
}

func TestRetentionFinancialClosureUnfunded(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("retention_closure_vendor", "vendor")
	buyer, _ := e.user("retention_closure_buyer", "buyer")
	product := e.product(vendor, "digital")
	closed := e.order(buyer, product, "XMR", stateCancelled)
	open := e.order(buyer, product, "XMR", stateDraft)
	now := time.Now().UTC()
	at := now.Add(-200 * 24 * time.Hour).Truncate(time.Microsecond)
	ageRetentionOrder(t, e, closed, at)
	runRetentionClosure(t, e, now)
	if got := retentionClosureAt(t, e, closed); !got.Valid || !got.Time.Equal(at) {
		t.Fatalf("cancelled unfunded order closure %v; want original terminal time %v", got, at)
	}
	if retentionClosureAt(t, e, open).Valid {
		t.Fatal("an open draft was marked financially closed")
	}
	// A later legal hold/release does not restart the original closure deadline.
	agExec(e, `INSERT INTO retention_holds(id,scope,record_id,authority,reason,owner,readers,release_condition,began_at,review_at)
		VALUES('retention_clock_hold','order',$1,'test','specific records','operator','operator','case closed',$2,$3)`, closed, now, now.Add(24*time.Hour))
	runRetentionClosure(t, e, now.Add(time.Hour))
	if got := retentionClosureAt(t, e, closed); !got.Time.Equal(at) {
		t.Fatal("a preservation hold moved the established financial deadline")
	}
}

func TestRetentionFinancialClosureXMRWaitsForGuardedDestruction(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _ := x.settled()
	name := x.walletOf(order)
	var terminal time.Time
	if err := x.DB.QueryRow("SELECT updated FROM orders WHERE id=$1", order).Scan(&terminal); err != nil {
		t.Fatal(err)
	}
	now := terminal.Add(90 * 24 * time.Hour)
	runRetentionClosure(t, x.testEnv, now)
	if retentionClosureAt(t, x.testEnv, order).Valid || retentionExceptionReason(t, x.testEnv, order) != "monero_wallet_not_destroyed" {
		t.Fatal("an open retained Monero wallet was considered retired")
	}
	x.rotate()
	x.days(name, 91)
	x.mustPoll()
	if x.walletState(name) != "destroyed" {
		t.Fatalf("wallet still %s", x.walletState(name))
	}
	runRetentionClosure(t, x.testEnv, x.now)
	closed := retentionClosureAt(t, x.testEnv, order)
	var destroyed time.Time
	if err := x.DB.QueryRow("SELECT destroyed_at FROM xmr_escrow_wallets WHERE name=$1", name).Scan(&destroyed); err != nil {
		t.Fatal(err)
	}
	if !closed.Valid || !closed.Time.Equal(destroyed) {
		t.Fatalf("closure %v did not use guarded wallet destruction %v", closed, destroyed)
	}
	runRetentionClosure(t, x.testEnv, x.now.Add(181*24*time.Hour))
	if retentionExceptionReason(t, x.testEnv, order) != "monero_financial_retirement_required" {
		t.Fatal("Monero financial retirement blocker was invisible")
	}
	if _, err := x.DB.Exec("DELETE FROM xmr_escrow_wallets WHERE name=$1", name); err == nil {
		t.Fatal("destruction of key material incorrectly authorized deleting wallet records")
	}
}

func TestRetentionFinancialClosureWaitsForWatcherLock(t *testing.T) {
	e := newTestApp(t)
	conn, err := e.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_lock($1)", escrowWatcherLock); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", escrowWatcherLock)
	tx, err := e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := e.A.retentionFinancialClosure(context.Background(), tx, time.Now()); !errors.Is(err, errRetentionEscrowBusy) {
		t.Fatalf("busy watcher: %v", err)
	}
}
