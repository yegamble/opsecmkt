package market

import (
	"slices"
	"strings"
	"testing"
)

// A-729: what docs/testnet-runbook.md ("Orders around the backup point") says of coins spent after the backup. A refund
// or release sent after the backup leaves the restored records with an order and no settlement, and the wallet with no
// coins at the order's account. The market does not notice until someone asks it to send: the order stays as restored,
// the vendor can still mark it shipped, and each later request (release, claim or refund) waits as an outputs mismatch,
// flagged to staff, with nothing built or sent. Simulated wallet RPC, not chain evidence.
func TestXMREscrowSpentAfterBackupWaitsAsOutputsMismatch(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	spent := func(addr string) {
		t.Helper()
		var images []string
		for _, o := range x.fake.outputs(addr) {
			images = append(images, o.KeyImage)
		}
		if len(images) != 1 {
			t.Fatalf("the escrow has %d outputs", len(images))
		}
		x.fake.spendOutside(images...)
		x.fake.mine(10)
	}
	waitsMismatch := func(name, order, kind string) {
		t.Helper()
		s := x.settlementOf(order, kind)
		x.mustPoll()
		if got := x.waitClass(s.ID); got != xmrWaitOutputs {
			t.Fatalf("%s: the %s waits with %q, want %q", name, kind, got, xmrWaitOutputs)
		}
		x.mustPoll()
		s = x.settlementOf(order, kind)
		if got := x.waitClass(s.ID); got != xmrOutputsMismatch || s.State != "requested" || s.TxHash != "" {
			t.Fatalf("%s: the %s %+v waits with %q, want %q", name, kind, s, got, xmrOutputsMismatch)
		}
		if got := x.fake.countOf("transfer"); got != 0 {
			t.Fatalf("%s: the wallet was asked to build %d transaction(s)", name, got)
		}
		if !anyContains(x.events(order), xmrSettleWaitText(xmrOutputsMismatch, kind, 10)) {
			t.Fatalf("%s: staff are not told: %q", name, x.events(order))
		}
		x.mustPoll()
		if got := x.xmrSettlements(order); len(got) != 1 || !strings.HasPrefix(got[0], kind+" requested ") {
			t.Fatalf("%s: settlements %v", name, got)
		}
	}
	physicalPaid := func() (order, addr, sess string) {
		t.Helper()
		order, addr = x.freshCheckout(x.physical(x.vendor))
		x.fake.payAddress(addr, hex64("records lag "+order), xmrListingPrice, 10)
		x.mustPoll()
		if x.state(order) != statePaid {
			t.Fatalf("funded physical order is %s", x.state(order))
		}
		return order, addr, x.sessOf(order, roleBuyer)
	}

	// Refunded after the backup: restored as paid. Nothing tells anyone; the vendor can still ship it, and neither a
	// release nor a claim ever pays.
	order, addr, sess := physicalPaid()
	before := len(x.events(order))
	spent(addr)
	x.mustPoll()
	x.mustPoll()
	if x.state(order) != statePaid || len(x.events(order)) != before || len(x.xmrSettlements(order)) != 0 {
		t.Fatalf("the watcher noticed: %s %q %v", x.state(order), x.events(order), x.xmrSettlements(order))
	}
	if page := x.page("/order?id="+order, x.vendorSess); !strings.Contains(page, `action="/escrow-xmr/ship"`) {
		t.Fatalf("the vendor is not offered Mark shipped:\n%s", page)
	}
	claim := testXMRAddr("claim "+order, true)
	if code, body := x.xmrShip(order, x.vendorSess, claim, testPassword); code != 303 {
		t.Fatalf("Mark shipped: %d %s", code, body)
	}
	if code, body := x.xmrConfirm(order, sess, testPassword); code != 303 || x.state(order) != stateCompleted {
		t.Fatalf("Confirm receipt: %d %s; order %s", code, body, x.state(order))
	}
	waitsMismatch("shipped and confirmed", order, "release")

	// Refunded after the backup and cancelled again: the records move to cancelled, and its refund never pays twice.
	cancelled, addr, _ := physicalPaid()
	spent(addr)
	var cancel *pageForm
	for _, f := range x.orderForms(cancelled, x.vendorSess) {
		if f.action == "/orders/cancel" {
			cancel = &f
		}
	}
	if cancel == nil {
		t.Fatal("no cancel form for the vendor")
	}
	if w := x.do("POST", cancel.action, x.vendorSess, cancel.values); w.Code != 303 || x.state(cancelled) != stateCancelled {
		t.Fatalf("the vendor's cancel: %d %s", w.Code, w.Body.String())
	}
	waitsMismatch("cancelled", cancelled, "refund")

	// Claimed after the backup: restored as completed with no settlement, the claim form is offered, and a second claim
	// pays nothing.
	digital, addr := x.freshCheckout(x.digital(x.vendor))
	x.fake.payAddress(addr, hex64("records lag "+digital), xmrListingPrice, 10)
	x.mustPoll()
	if code, body := x.xmrConfirm(digital, x.sessOf(digital, roleBuyer), testPassword); code != 303 || x.state(digital) != stateCompleted {
		t.Fatalf("Confirm receipt of the digital order: %d %s; order %s", code, body, x.state(digital))
	}
	spent(addr)
	if code, body := x.xmrClaim(digital, x.vendorSess, testXMRAddr("claim "+digital, true), testPassword); code != 303 {
		t.Fatalf("the second claim: %d %s", code, body)
	}
	waitsMismatch("claimed", digital, "release")
	if got := agStrings(t, x.testEnv, "SELECT DISTINCT state FROM xmr_settlements"); !slices.Equal(got, []string{"requested"}) {
		t.Fatalf("settlement states %v", got)
	}
}
