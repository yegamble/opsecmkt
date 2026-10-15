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
	closed := e.order(buyer, product, "BTC", stateCancelled)
	open := e.order(buyer, product, "BTC", stateDraft)
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

func TestRetentionFinancialClosureBTCRequiresFreshProof(t *testing.T) {
	x := newEscrowEnv(t)
	admin, _ := x.user("retention_btc_admin", "admin")
	order, settlement := x.broadcastRefund("retention-closure", 40000)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ageRetentionOrder(t, x.testEnv, order, now.Add(-100*24*time.Hour))
	runRetentionClosure(t, x.testEnv, now)
	if retentionClosureAt(t, x.testEnv, order).Valid || retentionExceptionReason(t, x.testEnv, order) != "settlement_pending" {
		t.Fatal("broadcast settlement was considered final")
	}
	txid := x.settleByID(settlement).TxID
	x.core.setTxConfs(txid, 3, "")
	x.poll()
	runRetentionClosure(t, x.testEnv, now)
	if retentionClosureAt(t, x.testEnv, order).Valid {
		t.Fatal("three confirmations were mistaken for completed reorganisation monitoring")
	}
	x.core.setTxConfs(txid, 110, "")
	x.poll()
	beforeSends, beforeSigns := x.core.count("sendrawtransaction"), x.core.count("walletprocesspsbt")
	runRetentionClosure(t, x.testEnv, now)
	at := retentionClosureAt(t, x.testEnv, order)
	if !at.Valid || !at.Time.Equal(now) {
		t.Fatalf("freshly checked settled escrow not closed: %v", at)
	}
	if x.core.count("sendrawtransaction") != beforeSends || x.core.count("walletprocesspsbt") != beforeSigns {
		t.Fatal("retention performed a payment or signing operation")
	}
	// Loss of the adapter cannot prove that funds reopened or move a deadline.
	adapter := x.A.escrow.Load()
	x.A.escrow.Store(nil)
	runRetentionClosure(t, x.testEnv, now.Add(time.Hour))
	if got := retentionClosureAt(t, x.testEnv, order); !got.Valid || !got.Time.Equal(at.Time) {
		t.Fatal("a temporary wallet outage restarted the closure deadline")
	}
	if retentionExceptionReason(t, x.testEnv, order) != "wallet_unavailable" {
		t.Fatal("unavailable wallet did not create a scoped operational exception")
	}
	x.A.escrow.Store(adapter)
	// A late deposit not yet in PostgreSQL is detected by the fresh wallet read.
	address := agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
	x.core.deposit(address, "retention-late", 0, 7000, 1)
	runRetentionClosure(t, x.testEnv, now.Add(2*time.Hour))
	if retentionClosureAt(t, x.testEnv, order).Valid || retentionExceptionReason(t, x.testEnv, order) != "deposit_unreconciled" {
		t.Fatal("a fresh unrecorded deposit did not reopen financial work")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND txid='retention-late'", order) != 0 {
		t.Fatal("retention silently wrote the escrow payment ledger")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%retention is overdue%'", admin) != 3 {
		t.Fatal("operational reasons were not notified once per change")
	}
}

func TestRetentionEscrowExceptionReviewAndRetirement(t *testing.T) {
	x := newEscrowEnv(t)
	order, settlement := x.broadcastRefund("retention-review", 40000)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ageRetentionOrder(t, x.testEnv, order, now.Add(-100*24*time.Hour))
	x.core.setTxConfs(x.settleByID(settlement).TxID, 110, "")
	x.poll()
	x.poll()
	runRetentionClosure(t, x.testEnv, now)
	runRetentionClosure(t, x.testEnv, now.Add(181*24*time.Hour))
	if retentionExceptionReason(t, x.testEnv, order) != "bitcoin_wallet_retirement_required" {
		t.Fatal("overdue wallet retirement was invisible")
	}
	due := now.Add(212 * 24 * time.Hour)
	list, err := LoadRetentionEscrowExceptions(context.Background(), x.DB, due, 100)
	if err != nil || len(list) != 1 || !list[0].ReviewOverdue {
		t.Fatalf("review alert %+v, %v", list, err)
	}
	if err := ReviewRetentionEscrowException(context.Background(), x.DB, order, due); err != nil {
		t.Fatal(err)
	}
	list, err = LoadRetentionEscrowExceptions(context.Background(), x.DB, due, 100)
	if err != nil || len(list) != 1 || list[0].ReviewOverdue {
		t.Fatalf("review renewal %+v, %v", list, err)
	}
	if err := ReviewRetentionEscrowException(context.Background(), x.DB, "unknown-order", due); err == nil {
		t.Fatal("an unknown exception was renewed")
	}
	// Review receipt permits no deletion, rebroadcast or allocation reset.
	if _, err := x.DB.Exec("DELETE FROM escrows WHERE order_id=$1", order); err == nil {
		t.Fatal("retention weakened the escrow deletion guard")
	}
	if !retentionClosureAt(t, x.testEnv, order).Time.Equal(now) {
		t.Fatal("review renewed the financial retention deadline")
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
