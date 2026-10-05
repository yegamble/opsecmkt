package market

import (
	"strings"
	"testing"
)

// Escrow Phase 1, slice P1-d1: recovery and reconciliation (ES-6), against the fake escrow Core (simulated RPC, not
// chain evidence: scripts/test-escrow-regtest.sh has the real-chain legs).

// confirmedSettlement is a vendor's claim co-signed, broadcast and confirmed at 3 confirmations: the order and the
// settlement.
func (x *escrowEnv) confirmedSettlement(label string) (string, int64) {
	x.t.Helper()
	order, id := x.built(x.netAddr("refund-"+label), x.netAddr("claim-"+label), 100000)
	x.signAs(order, roleVendor, id)
	x.poll()
	x.core.setTxConfs(x.settleByID(id).TxID, 3, "")
	x.poll()
	if r := x.settleByID(id); r.State != "confirmed" {
		x.t.Fatalf("%s: %+v", label, r)
	}
	return order, id
}

// Migration 077: a settlement with a final transaction moves only forward, except confirmed -> broadcast after a
// reorganisation (and confirmed -> external when a conflicting transaction was mined instead); external is final. The
// columns and the table reconciliation writes exist, and a settlement found on the network may be recorded with
// cosigned 'network'.
func TestEscrowMigration077SettlementMoves(t *testing.T) {
	x := newEscrowEnv(t)
	_, id := x.confirmedSettlement("m77")
	for _, q := range []string{
		"UPDATE settlements SET state='signed' WHERE id=$1",
		"UPDATE settlements SET state='signed',confirmed_at=NULL WHERE id=$1",
	} {
		if _, err := x.DB.Exec(q, id); err == nil {
			t.Fatalf("the database allowed confirmed -> signed: %s", q)
		}
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='broadcast',confirmed_at=NULL WHERE id=$1", id); err != nil {
		t.Fatalf("confirmed -> broadcast (a reorganisation) refused: %v", err)
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='signed' WHERE id=$1", id); err == nil {
		t.Fatal("the database allowed broadcast -> signed")
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='confirmed',confirmed_at=now(),reorg_safe_at=now() WHERE id=$1", id); err != nil {
		t.Fatalf("broadcast -> confirmed: %v", err)
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='broadcast',confirmed_at=NULL WHERE id=$1", id); err == nil {
		t.Fatal("the database allowed a settlement past its reorg depth back to broadcast with reorg_safe_at set")
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='external',reorg_safe_at=NULL WHERE id=$1", id); err != nil {
		t.Fatalf("confirmed -> external: %v", err)
	}
	for _, to := range []string{"broadcast", "confirmed", "signed"} {
		if _, err := x.DB.Exec("UPDATE settlements SET state=$2,confirmed_at=now() WHERE id=$1", id, to); err == nil {
			t.Fatalf("the database allowed external -> %s", to)
		}
	}
	// A-365 (Q6): a signed settlement (stored, not yet broadcast) never moves straight to confirmed: it is broadcast first.
	orderQ, idQ := x.built(regtestAddr("refund-m77q"), regtestAddr("claim-m77q"), 100000)
	x.signAs(orderQ, roleVendor, idQ)
	x.A.escrowPollOnce(x.killAtSend())
	x.sendNormally()
	if r := x.settleByID(idQ); r.State != "signed" {
		t.Fatalf("stored, its broadcast killed: %+v", r)
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='confirmed',broadcast_at=now(),confirmed_at=now() WHERE id=$1", idQ); err == nil ||
		!strings.Contains(err.Error(), "cannot move from signed to confirmed") {
		t.Fatalf("the database allowed signed -> confirmed (or refused it for another reason): %v", err)
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='broadcast',broadcast_at=now() WHERE id=$1", idQ); err != nil {
		t.Fatalf("signed -> broadcast: %v", err)
	}
	// A settlement external before any final transaction is final too.
	order2, id2 := x.built(regtestAddr("refund-m77b"), regtestAddr("claim-m77b"), 100000)
	x.signAs(order2, roleVendor, id2)
	x.core.spend(txidOf(order2), 0)
	x.poll()
	if r := x.settleByID(id2); r.State != "external" {
		t.Fatalf("party-signed, input gone: %+v", r)
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='party_signed' WHERE id=$1", id2); err == nil {
		t.Fatal("the database allowed external -> party_signed")
	}
	if _, err := x.DB.Exec("UPDATE settlements SET cosigned='someone' WHERE id=$1", id); err == nil {
		t.Fatal("the database allowed an unknown cosigned value")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM information_schema.columns WHERE table_name='settlements' AND column_name='reorg_safe_at'") != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM information_schema.columns WHERE table_name='payments' AND column_name IN ('spent_outside','spent_by')") != 2 ||
		agInt(x.testEnv, "SELECT count(*) FROM information_schema.tables WHERE table_name='escrow_orphans'") != 1 {
		t.Fatal("migration 077's columns or table are missing")
	}
	if _, err := x.DB.Exec("UPDATE payments SET spent_by=$1 WHERE settlement_id=$2", txidOf("x"), id2); err == nil {
		t.Fatal("the database allowed spent_by on an output not spent outside the market")
	}
	for _, c := range []string{"market", "parties", "network"} {
		var ok bool
		if err := x.DB.QueryRow("SELECT pg_get_constraintdef(oid) LIKE '%'''||$1||'''%' FROM pg_constraint WHERE conname='settlements_cosigned_check'", c).Scan(&ok); err != nil || !ok {
			t.Fatalf("cosigned %q not allowed: %v", c, err)
		}
	}
}
