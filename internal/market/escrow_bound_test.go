package market

import (
	"slices"
	"strings"
	"testing"
)

// A-342 (refines A-336): a claim or refund build waits at most escrowBuildWaitMax (24 hours from the request) for an
// escrow output recorded before completion that does not confirm, or while the escrow has more outputs than one
// settlement spends; then it is built without the unconfirmed outputs and beyond the escrowMaxInputs largest, which
// are marked left out for the stray refund. The order gets a note and the buyer is told. Simulated RPC.
func TestEscrowBuildWaitIsBounded(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.draft()
	addr := x.mustCheckout(order, regtestAddr("bound-refund-1"))
	x.core.deposit(addr, txidOf("full"), 0, 100000, 3)
	x.core.deposit(addr, txidOf("stuck"), 0, 30000, 1) // recorded before completion, never confirms
	x.poll()
	if x.state(order) != stateCompleted {
		t.Fatalf("state %s", x.state(order))
	}
	x.claim(order, x.vendorSess, regtestAddr("bound-claim-1"))
	x.poll()
	id, _, state, class := x.settlement(order)
	x.DB.Exec("UPDATE settlements SET created=now()-interval '23 hours' WHERE id=$1", id)
	x.poll()
	if _, _, state, class = x.settlement(order); state != "building" || class != "waiting-confirmations" {
		t.Fatalf("within the bound: %s %s", state, class)
	}
	x.DB.Exec("UPDATE settlements SET created=now()-interval '25 hours' WHERE id=$1", id)
	x.poll()
	if _, _, state, class = x.settlement(order); state != "awaiting_signature" {
		t.Fatalf("after the bound: %s %s", state, class)
	}
	if rows := agStrings(t, x.testEnv, "SELECT role||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id); !slices.Equal(rows, []string{"claimant 99682"}) {
		t.Fatalf("outputs %q", rows)
	}
	if got := agStrings(t, x.testEnv, "SELECT txid||' '||escrow_left_out||' '||(settlement_id IS NULL) FROM payments WHERE order_id=$1 ORDER BY amount", order); !slices.Equal(got,
		[]string{txidOf("stuck") + " true true", txidOf("full") + " false false"}) {
		t.Fatalf("payments %q", got)
	}
	const left = "built without 1 deposit (0.0003 BTC) to this escrow"
	if ev := strings.Join(x.events(order), "\n"); !strings.Contains(ev, left) || !strings.Contains(ev, "stray refund") {
		t.Fatalf("no note:\n%s", ev)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%"+left+"%stray refund%'", x.buyer) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%"+left+"%'", x.vendor) != 0 {
		t.Fatal("the buyer (only) is told")
	}

	// More outputs than one settlement spends: after the bound the 50 largest are spent and the rest left out.
	order2 := x.completed(regtestAddr("bound-refund-2"), append(slices.Repeat([]int64{2000}, 50), 1000)...)
	x.claim(order2, x.vendorSess, regtestAddr("bound-claim-2"))
	x.poll()
	id2, _, state, class := x.settlement(order2)
	if state != "building" || class != "too-many-inputs" {
		t.Fatalf("51 outputs within the bound: %s %s", state, class)
	}
	x.DB.Exec("UPDATE settlements SET created=now()-interval '25 hours' WHERE id=$1", id2)
	x.poll()
	if _, _, state, _ = x.settlement(order2); state != "awaiting_signature" ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", id2) != 50 ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out AND amount=1000 AND settlement_id IS NULL", order2) != 1 {
		t.Fatalf("51 outputs after the bound: %s", state)
	}
	if rows := agStrings(t, x.testEnv, "SELECT role||' '||sats FROM settlement_outputs WHERE settlement_id=$1", id2); !slices.Equal(rows, []string{"claimant 89392"}) {
		t.Fatalf("outputs %q", rows)
	}

	// A refund whose only deposit never confirms: after the bound nothing is left to spend; it fails and the deposit
	// is left out for a stray refund.
	order3 := x.draft()
	addr3 := x.mustCheckout(order3, regtestAddr("bound-refund-3"))
	x.core.deposit(addr3, txidOf("never"), 0, 40000, 0)
	x.poll()
	if w := x.do("POST", "/orders/cancel", x.buyerSess, map[string][]string{"order_id": {order3}, "from": {stateAwaitingPayment}, "amount_seen": {"40000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	id3, _, _, _ := x.settlement(order3)
	x.DB.Exec("UPDATE settlements SET created=now()-interval '25 hours' WHERE id=$1", id3)
	x.poll()
	if _, _, state, class := x.settlement(order3); state != "failed" || class != "nothing-to-settle" ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out", order3) != 1 {
		t.Fatalf("refund of an unconfirmed deposit after the bound: %s %s", state, class)
	}
}
