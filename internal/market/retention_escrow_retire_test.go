package market

import (
	"context"
	"database/sql"
	"encoding/hex"
	"testing"
	"time"
)

func certifiedBTCRetirement(t *testing.T) (*escrowEnv, string, *retentionEscrowRetirement, *sql.Tx) {
	t.Helper()
	x := newEscrowEnv(t)
	order, sid := x.broadcastRefund("retention-full", 40000)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ageRetentionOrder(t, x.testEnv, order, now.Add(-100*24*time.Hour))
	x.core.setTxConfs(x.settleByID(sid).TxID, 110, "")
	x.poll()
	x.poll()
	runRetentionClosure(t, x.testEnv, now)
	tx, err := x.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	at := now.Add(181 * 24 * time.Hour)
	if err = x.A.retentionFinancialClosure(context.Background(), tx, at); err != nil {
		t.Fatal(err)
	}
	r, err := x.A.prepareRetentionEscrowRetirement(context.Background(), tx, order, at)
	if err != nil || r == nil {
		t.Fatalf("prepare retirement: %+v %v", r, err)
	}
	return x, order, r, tx
}

func TestRetentionEscrowRetirementBitcoinErasesMappingsPreservesNonreuse(t *testing.T) {
	x, order, r, tx := certifiedBTCRetirement(t)
	var address string
	if err := tx.QueryRow(`SELECT address FROM escrows WHERE order_id=$1`, order).Scan(&address); err != nil {
		t.Fatal(err)
	}
	keys, err := collectStrings(context.Background(), tx, `SELECT pubkey FROM escrow_pubkeys WHERE escrow_id=(SELECT id FROM escrows WHERE order_id=$1) ORDER BY party`, order)
	if err != nil {
		t.Fatal(err)
	}
	if x.core.count("setlabel") != 0 {
		t.Fatal("preparation changed a wallet before journaling")
	}
	if err = x.A.applyRetentionEscrowRetirement(context.Background(), tx, r); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"orders", "order_events", "escrows", "payments", "settlements", "xmr_settlements", "disputes", "deliveries"} {
		column := "order_id"
		if table == "orders" {
			column = "id"
		}
		if agInt(x.testEnv, `SELECT count(*) FROM `+table+` WHERE `+column+`=$1`, order) != 0 {
			t.Fatalf("%s kept the retired order", table)
		}
	}
	x.core.mu.Lock()
	label := x.core.labels[address]
	x.core.mu.Unlock()
	if label != "retired" {
		t.Fatalf("wallet label %q still links the order", label)
	}
	if err = x.A.reconcileOrphans(context.Background(), x.A.escrow.Load()); err != nil {
		t.Fatal(err)
	}
	if agInt(x.testEnv, `SELECT count(*) FROM escrow_orphans WHERE order_id=$1`, order) != 0 {
		t.Fatal("orphan reconciliation resurrected an intentionally erased mapping")
	}
	used, err := bitcoinAddressUsed(context.Background(), x.DB, address, "", x.buyer, "")
	if err != nil || !used {
		t.Fatalf("retired market address became an external destination: %v %v", used, err)
	}
	check, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer check.Rollback()
	pairs := map[string][]byte{}
	for i, k := range keys {
		b, _ := hex.DecodeString(k)
		pairs[string(rune('a'+i))] = b
	}
	held, err := heldEscrowKeys(context.Background(), check, pairs)
	if err != nil || len(held) != 3 {
		t.Fatalf("retired children became reusable: %v %v", held, err)
	}
	var floor int64
	if err = check.QueryRow(escrowNextChildSQL, x.buyer, 1).Scan(&floor); err != nil || floor < 2 {
		t.Fatalf("party allocation restarted: %d %v", floor, err)
	}
	// Permits expire with the transaction and do not relax another live escrow.
	if agInt(x.testEnv, `SELECT count(*) FROM retention_escrow_permits`) != 0 {
		t.Fatal("an erase capability survived commit")
	}
	// Sparse technical state stores hashes/counters, never the identity links.
	if agInt(x.testEnv, `SELECT count(*) FROM retention_escrow_retired_keys`) != 3 {
		t.Fatal("exact child non-reuse state missing")
	}
}

func TestRetentionEscrowRetirementHoldAndUnfinishedFundsRefuse(t *testing.T) {
	x, order, r, tx := certifiedBTCRetirement(t)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	now := r.CertifiedAt
	agExec(x.testEnv, `INSERT INTO retention_holds(id,scope,record_id,authority,reason,owner,readers,release_condition,began_at,review_at)
	 VALUES('retirement_hold','order',$1,'test duty','specific record','operator','operator','released',$2,$3)`, order, now, now.Add(24*time.Hour))
	blocked, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Rollback()
	if payload, err := x.A.prepareRetentionEscrowRetirement(context.Background(), blocked, order, now); err != nil || payload != nil {
		t.Fatalf("held order received a retirement certificate: %+v %v", payload, err)
	}
	blocked.Rollback()
	agExec(x.testEnv, `UPDATE retention_holds SET released_at=$1 WHERE id='retirement_hold'`, now)
	address := agStrings(t, x.testEnv, `SELECT address FROM escrows WHERE order_id=$1`, order)[0]
	x.core.deposit(address, "retirement-unknown-funds", 0, 9000, 0)
	blocked, err = x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Rollback()
	if payload, err := x.A.prepareRetentionEscrowRetirement(context.Background(), blocked, order, now); err != nil || payload != nil {
		t.Fatalf("new unrecorded funds received a retirement certificate: %+v %v", payload, err)
	}
	if x.core.count("setlabel") != 0 {
		t.Fatal("refused certificate changed wallet labels")
	}
}

func TestRetentionEscrowRetirementRechecksHistoricalFinality(t *testing.T) {
	x, order, r, tx := certifiedBTCRetirement(t)
	tx.Rollback()
	var txid string
	if err := x.DB.QueryRow(`SELECT txid FROM settlements WHERE order_id=$1 AND state='confirmed'`, order).Scan(&txid); err != nil {
		t.Fatal(err)
	}
	// A deep reorganisation after the historical reorg-safe observation leaves
	// the input spent in the mempool. gettxout=null alone cannot authorize erasure.
	x.core.setTxConfs(txid, 0, "")
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if got, err := x.A.prepareRetentionEscrowRetirement(context.Background(), tx, order, r.CertifiedAt); err != nil || got != nil {
		t.Fatalf("historical finality accepted after a fresh reorganisation: %+v %v", got, err)
	}
	if x.core.count("setlabel") != 0 {
		t.Fatal("a reorganisation refusal changed wallet labels")
	}
}

func TestRetentionEscrowExternalSpendNeedsFreshFinalSpender(t *testing.T) {
	x := newEscrowEnv(t)
	order, sid := x.built(regtestAddr("retention-external-refund"), regtestAddr("retention-external-claim"), 100000)
	x.signAs(order, roleVendor, sid)
	x.poll()
	own := x.settleByID(sid)
	outside := txidOf("retention external spender")
	x.core.mu.Lock()
	x.core.broadcast[outside] = own.Raw
	x.core.txConfs[outside] = 0
	x.core.mu.Unlock()
	x.core.setTxConfs(own.TxID, -2, outside)
	x.poll()
	if x.settleByID(sid).State != "external" {
		t.Fatal("fixture did not recognize the external spend")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	ageRetentionOrder(t, x.testEnv, order, now.Add(-100*24*time.Hour))
	runRetentionClosure(t, x.testEnv, now)
	if retentionClosureAt(t, x.testEnv, order).Valid {
		t.Fatal("an external mempool spender was considered final")
	}
	x.core.setTxConfs(outside, 110, "")
	runRetentionClosure(t, x.testEnv, now.Add(31*time.Minute))
	if !retentionClosureAt(t, x.testEnv, order).Valid {
		t.Fatal("a known external spender confirmed100deep remained an indefinite closure blocker")
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
