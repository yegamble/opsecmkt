package market

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func certifiedXMRRetirement(t *testing.T) (*xmrEscrowEnv, string, *retentionEscrowRetirement, *sql.Tx) {
	t.Helper()
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _ := x.settled()
	name := x.walletOf(order)
	x.rotate()
	x.days(name, 91)
	x.mustPoll()
	if x.walletState(name) != "destroyed" {
		t.Fatal("fixture wallet not destroyed")
	}
	runRetentionClosure(t, x.testEnv, x.now)
	tx, err := x.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	at := x.now.Add(181 * 24 * time.Hour)
	if err = x.A.retentionFinancialClosure(t.Context(), tx, at); err != nil {
		t.Fatal(err)
	}
	r, err := x.A.prepareRetentionEscrowRetirement(t.Context(), tx, order, at)
	if err != nil || r == nil {
		t.Fatalf("prepare XMR retirement: %+v %v", r, err)
	}
	return x, order, r, tx
}

func TestRetentionEscrowRetirementMoneroHoldRefuses(t *testing.T) {
	x, order, r, tx := certifiedXMRRetirement(t)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	now := r.CertifiedAt
	agExec(x.testEnv, `INSERT INTO retention_holds(id,scope,record_id,authority,reason,owner,readers,release_condition,began_at,review_at)
 VALUES('retirement_hold','order',$1,'test duty','specific record','operator','operator','released',$2,$3)`, order, now, now.Add(24*time.Hour))
	blocked, err := x.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Rollback()
	before := x.opens(r.WalletName)
	if payload, err := x.A.prepareRetentionEscrowRetirement(t.Context(), blocked, order, now); err != nil || payload != nil {
		t.Fatalf("held Monero order received a retirement certificate: %+v %v", payload, err)
	}
	blocked.Rollback()
	if x.opens(r.WalletName) != before {
		t.Fatal("refused certificate accessed destroyed wallet")
	}
	agExec(x.testEnv, `UPDATE retention_holds SET released_at=$1 WHERE id='retirement_hold'`, now)
	allowed, err := x.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer allowed.Rollback()
	if payload, err := x.A.prepareRetentionEscrowRetirement(t.Context(), allowed, order, now); err != nil || payload == nil {
		t.Fatalf("released hold kept Monero retirement blocked: %+v %v", payload, err)
	}
}

// A-613: an order whose failed stray refund was dismissed as nothing owed (xmr_settlement_hand_settled, migration
// 128) closes and retires like any other: the dismissed record no longer raises an escrow alert, and the certified
// retirement deletes it with the rest of the order's escrow records (its trigger allows that under the permit only).
func TestRetentionEscrowRetirementMoneroDeletesDismissedSettlement(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _ := x.settled()
	var stray int64
	if err := x.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) SELECT order_id,'stray_refund',refund_address FROM escrows WHERE order_id=$1 RETURNING id", order).Scan(&stray); err != nil {
		t.Fatal(err)
	}
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='failed',send_error=$2 WHERE id=$1", stray, xmrNothing)
	agExec(x.testEnv, "INSERT INTO xmr_settlement_hand_settled(settlement_id,kind,note,settled_by) VALUES($1,'nothing-owed','No deposit counted; nothing owed now.',$2)", stray, x.admin)
	if _, err := x.DB.Exec("DELETE FROM xmr_settlement_hand_settled WHERE settlement_id=$1", stray); err == nil {
		t.Fatal("the record was deleted without a retirement permit")
	}
	name := x.walletOf(order)
	x.rotate()
	x.days(name, 91)
	x.mustPoll()
	if x.walletState(name) != "destroyed" {
		t.Fatal("fixture wallet not destroyed")
	}
	runRetentionClosure(t, x.testEnv, x.now)
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	at := x.now.Add(181 * 24 * time.Hour)
	if err = x.A.retentionFinancialClosure(context.Background(), tx, at); err != nil {
		t.Fatal(err)
	}
	r, err := x.A.prepareRetentionEscrowRetirement(context.Background(), tx, order, at)
	if err != nil || r == nil {
		t.Fatalf("XMR certificate with a dismissed settlement: %+v %v", r, err)
	}
	if err = x.A.applyRetentionEscrowRetirement(context.Background(), tx, r); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if agInt(x.testEnv, `SELECT count(*) FROM xmr_settlement_hand_settled WHERE settlement_id=$1`, stray) != 0 || agInt(x.testEnv, `SELECT count(*) FROM xmr_settlements WHERE order_id=$1`, order) != 0 {
		t.Fatal("the dismissed settlement's records remain after retirement")
	}
}

func TestRetentionEscrowRetirementMoneroDeletesDestroyedFinancialState(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _ := x.settled()
	name := x.walletOf(order)
	x.rotate()
	x.days(name, 91)
	x.mustPoll()
	if x.walletState(name) != "destroyed" {
		t.Fatal("fixture wallet not destroyed")
	}
	runRetentionClosure(t, x.testEnv, x.now)
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	at := x.now.Add(181 * 24 * time.Hour)
	if err = x.A.retentionFinancialClosure(context.Background(), tx, at); err != nil {
		t.Fatal(err)
	}
	r, err := x.A.prepareRetentionEscrowRetirement(context.Background(), tx, order, at)
	if err != nil || r == nil {
		t.Fatalf("XMR certificate %+v %v", r, err)
	}
	if err = x.A.applyRetentionEscrowRetirement(context.Background(), tx, r); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if agInt(x.testEnv, `SELECT count(*) FROM orders WHERE id=$1`, order) != 0 || agInt(x.testEnv, `SELECT count(*) FROM xmr_settlements WHERE order_id=$1`, order) != 0 || agInt(x.testEnv, `SELECT count(*) FROM xmr_escrow_wallets WHERE name=$1`, name) != 0 {
		t.Fatal("retired Monero financial mappings remain")
	}
	if agInt(x.testEnv, `SELECT count(*) FROM retention_xmr_wallet_names WHERE month=$1::date AND next_n>1`, r.WalletMonth) != 1 {
		t.Fatal("wallet name allocation can restart after deletion")
	}
	opens := x.opens(name)
	if err = x.A.xmrSweepProbe(context.Background(), x.now); err != nil {
		t.Fatal(err)
	}
	if x.opens(name) != opens || agInt(x.testEnv, `SELECT count(*) FROM xmr_escrow_wallets WHERE name=$1`, name) != 0 {
		t.Fatal("restore sweep reopened/re-created deliberately retired wallet files")
	}
	// The certificate also reconstitutes non-reuse state when the original rows
	// are absent from an older restore; replay is idempotent.
	tx, err = x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = x.A.applyRetentionEscrowRetirement(context.Background(), tx, r); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
