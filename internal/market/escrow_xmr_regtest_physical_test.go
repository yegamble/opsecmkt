package market

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// Escrow Phase 3, slice X-e: the real-chain leg of physical Monero escrow orders (scripts/test-escrow-xmr-regtest.sh;
// run by the escrow-xmr-regtest CI job), on X-c's and X-d's helpers: the App's escrow wallet-rpc behind X-d's counting proxy
// (newXMRSettleRegtest), and claim and refund addresses that are subaddresses of the funder's wallet, so every
// settlement pays a real wallet.

// physicalReal takes a Monero escrow for a new buyer's draft on product with a real refund address (a funder
// subaddress): the order, the buyer's session and the refund address.
func (r *xmrRegtest) physicalReal(product string) (order, sess, refund, addr string) {
	r.t.Helper()
	buyer, sess := r.user("xr_pb_"+randomToken()[:8], "buyer")
	order, refund = r.order(buyer, product, "XMR", stateDraft), r.funderAddress()
	if w := r.checkout(sess, order, refund); w.Code != 303 {
		r.t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
	}
	return order, sess, refund, agStrings(r.t, r.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
}

// releasable reports what the Monero escrow builder's gate (settlementFits) says of order's settlement of kind, from the
// order's own state and dispute outcome.
func (r *xmrRegtest) releasable(order, kind string) bool {
	r.t.Helper()
	outcome, err := escrowDisputeOutcome(r.t.Context(), r.DB, order)
	if err != nil {
		r.t.Fatal(err)
	}
	return settlementFits(kind, r.state(order), outcome)
}

// TestXMREscrowRegtestPhysical: four physical Monero escrow orders funded in one real transaction (paid after 10
// blocks). Three are marked shipped to real subaddresses: the proxy sees the watcher read the wallet (get_transfers)
// but no transfer or relay_tx, and the gate refuses each release in its real state. Then each outcome moves the coins
// on the next passes, and only after it: Confirm receipt, and the release deadline, each complete an order whose
// release is built, relayed and confirmed to the claim address; a dispute decided for the buyer supersedes the third's
// release and its refund is relayed and confirmed to the refund address; the buyer's no-ship cancel of the fourth does
// the same. Each relayed transaction is decoded by the daemon (RingCT type 6, ring 16, fee within the cap).
func TestXMREscrowRegtestPhysical(t *testing.T) {
	r, p := newXMRSettleRegtest(t)
	spends := func() int { return p.count("transfer") + p.count("relay_tx") }
	physical := r.physical(r.vendor)
	_, adminSess := r.user("xr_padmin_"+randomToken()[:6], "admin")
	type leg struct{ order, sess, refund, claim string }
	var legs [4]leg
	amounts := map[string]int64{}
	for i := range legs {
		order, sess, refund, addr := r.physicalReal(physical)
		legs[i] = leg{order: order, sess: sess, refund: refund}
		amounts[addr] = xmrListingPrice
	}
	tx := r.pay(amounts)
	r.mineTo(tx, 10)
	heights := r.synced()
	r.mustPoll()
	for i, l := range legs {
		if r.state(l.order) != statePaid {
			t.Fatalf("order %d at 10 confirmations (%s): %s; its deposits (confirmations credited): %v", i, heights, r.state(l.order),
				agStrings(t, r.testEnv, "SELECT confirmations||' '||credited FROM payments WHERE order_id=$1", l.order))
		}
	}
	t.Logf("transaction %s funded 4 physical Monero escrow orders; all paid at 10 confirmations (%s)", tx, heights)
	for i := range legs[:3] {
		legs[i].claim = r.funderAddress()
		if code, body := r.xmrShip(legs[i].order, r.vendorSess, legs[i].claim, testPassword); code != 303 {
			t.Fatalf("ship: %d %s", code, body)
		}
	}
	r.mine(2)
	r.mustPoll()
	r.mustPoll()
	for i, l := range legs[:3] {
		if got := r.xmrSettlements(l.order); r.state(l.order) != stateShipped || len(got) != 1 || got[0] != "release requested "+l.claim || r.releasable(l.order, "release") {
			t.Fatalf("order %d after shipping: %s %v", i, r.state(l.order), got)
		}
	}
	if p.count("get_transfers") == 0 || spends() != 0 {
		t.Fatalf("through shipped: get_transfers %d, transfer %d, relay_tx %d", p.count("get_transfers"), p.count("transfer"), p.count("relay_tx"))
	}
	t.Logf("3 orders shipped, releases requested and refused by the gate; escrow wallet-rpc get_transfers %d, transfer 0, relay_tx 0", p.count("get_transfers"))

	// moved checks that the outcome just reached on order paid its settlement of kind to dest, on the next pass and with
	// one transfer and one relay_tx more than before.
	moved := func(order, kind, dest string, before int) {
		t.Helper()
		if !r.releasable(order, kind) {
			t.Fatalf("order %s (%s): its %s is not buildable", shortID(order), r.state(order), kind)
		}
		r.mustPoll()
		s := r.settlementOf(order, kind)
		if s.State != "relayed" || p.count("transfer")+p.count("relay_tx") != before+2 ||
			agStrings(t, r.testEnv, "SELECT destination FROM xmr_settlements WHERE id=$1", s.ID)[0] != dest {
			t.Fatalf("order %s: %s %+v; transfer %d, relay_tx %d (spending calls before: %d)", shortID(order), kind, s, p.count("transfer"), p.count("relay_tx"), before)
		}
		r.xmrShape(s)
		r.confirmReal(s.ID)
		t.Logf("order %s: %s %s relayed to its %s and confirmed", shortID(order), kind, s.TxHash, map[bool]string{true: "claim address", false: "refund address"}[kind == "release"])
	}

	// Confirm receipt.
	first := legs[0]
	if code, body := r.xmrConfirm(first.order, first.sess, testPassword); code != 303 {
		t.Fatalf("Confirm receipt: %d %s", code, body)
	}
	moved(first.order, "release", first.claim, 0)

	// The release deadline: the pass that completes the order builds nothing (its deadline step runs last); the next does.
	second := legs[1]
	r.forceDeadline(second.order, "release_at=now()-interval '1 minute'")
	before := spends()
	r.mustPoll()
	if r.state(second.order) != stateCompleted || !anyContains(r.events(second.order), "auto-release deadline passed") || spends() != before {
		t.Fatalf("at the deadline: %s, spending calls %d (before %d)", r.state(second.order), spends(), before)
	}
	moved(second.order, "release", second.claim, before)

	// A dispute decided for the buyer.
	third := legs[2]
	if code, body := r.xmrDispute(third.order, third.sess); code != 303 {
		t.Fatalf("dispute: %d %s", code, body)
	}
	before = spends()
	r.mustPoll()
	if spends() != before || r.releasable(third.order, "release") {
		t.Fatal("a disputed order's release moved")
	}
	dispute := agStrings(t, r.testEnv, "SELECT id FROM disputes WHERE order_id=$1", third.order)[0]
	if code, body := r.xmrPost("/resolve", adminSess, url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"Decided from the evidence both parties sent."},
		"payout_confirmed": {"confirmed"}, "amount_seen": {strconv.FormatInt(xmrListingPrice, 10)}, "reply_seen": {"0"}, "password": {testPassword}}); code != 303 {
		t.Fatalf("resolve: %d %s", code, body)
	}
	if got := r.xmrSettlements(third.order); len(got) != 2 || !strings.HasPrefix(got[0], "release superseded ") {
		t.Fatalf("after deciding for the buyer: %v", got)
	}
	moved(third.order, "refund", third.refund, before)

	// The buyer's no-ship cancel.
	fourth := legs[3]
	r.forceDeadline(fourth.order, "ship_by=now()-interval '1 minute'")
	if code, body := r.xmrBuyerCancel(fourth.order, fourth.sess); code != 303 {
		t.Fatalf("no-ship cancel: %d %s", code, body)
	}
	moved(fourth.order, "refund", fourth.refund, spends())
	if p.count("transfer") != 4 || p.count("relay_tx") != 4 {
		t.Fatalf("over the leg: transfer %d, relay_tx %d, want 4 each", p.count("transfer"), p.count("relay_tx"))
	}
}
