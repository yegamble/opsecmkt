package market

import (
	"html"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

var pooledPayoutCopy = []string{"payout address", "minimum automatic payout", "administrator reviews", "Completing releases",
	"paid over the price", "save a payout address", "payment watcher sends", "not sent automatically"}

func handleOf(e *testEnv, uid string) string {
	return agStr(e, "SELECT handle FROM users WHERE id=$1", uid)
}

const escrowRefundSentence = "less the network fee, to the buyer's refund address given at checkout"

// paidEscrow is a new escrow order of the buyer's whose content is not delivered automatically, funded with sats at 3
// confirmations and left paid, and its refund address.
func (x *escrowEnv) paidEscrow(label string, sats ...int64) (string, string) {
	x.t.Helper()
	order, refund := x.draft(), regtestAddr(label+"-refund")
	x.mustCheckout(order, refund)
	agExec(x.testEnv, "UPDATE products SET delivery_content='' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order)
	x.fund(order, label, 3, sats...)
	if x.state(order) != statePaid {
		x.t.Fatalf("funded %s order is %s", label, x.state(order))
	}
	return order, refund
}

// outputs lists order's release or refund outputs as "role address sats".
func (x *escrowEnv) outputs(order string) []string {
	x.t.Helper()
	id, _, _, _ := x.settlement(order)
	return agStrings(x.t, x.testEnv, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
}

// xmrPaidEscrow is a funded digital Monero escrow order left paid (its listing was taken down after checkout, so it is
// not delivered automatically), funded with amount in one deposit at 10 confirmations: the order, its buyer's session
// and its refund address.
func (x *xmrEscrowEnv) xmrPaidEscrow(amount int64) (string, string, string) {
	x.t.Helper()
	p := x.digital(x.vendor)
	order, addr := x.freshCheckout(p)
	agExec(x.testEnv, "UPDATE products SET archived=true,admin_archive_reason='Taken down for review.' WHERE id=$1", p)
	x.fake.payAddress(addr, hex64("fund "+order), amount, 10)
	x.mustPoll()
	if x.state(order) != statePaid {
		x.t.Fatalf("funded Monero order is %s", x.state(order))
	}
	return order, x.sessOf(order, roleBuyer), agStrings(x.t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0]
}

func TestEscrowOrderPageStatesTheMoneroSettlement(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	net := x.A.xmrEscrowNetKnown()
	if net == "" {
		t.Fatal("the Monero escrow network is not known to the test market")
	}
	coin := " " + net + " XMR"
	_, admin := x.user("xcopy_admin_"+randomToken()[:6], "admin")
	vendor := handleOf(x.testEnv, x.vendor)
	text := func(path, sess string) string {
		t.Helper()
		body := html.UnescapeString(x.page(path, sess))
		mustNotContain(t, body, pooledPayoutCopy...)
		return body
	}
	dispute := func(order, buyerSess string) {
		t.Helper()
		if code, body := x.xmrDispute(order, buyerSess); code != 303 {
			t.Fatalf("dispute: %d %s", code, body)
		}
	}
	// releaseAndBuild decides order's open dispute for the vendor, has the vendor claim, builds and sends the release and
	// returns its outputs.
	releaseAndBuild := func(order string, seen int64) []string {
		t.Helper()
		id := agStrings(t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)[0]
		x.check(x.do("POST", "/resolve", admin, payoutConfirmed(url.Values{"id": {id}, "outcome": {"release"},
			"resolution": {"Decided from the evidence both parties sent."}, "reply_seen": {"0"}, "password": {testPassword}}, seen)), 303)
		if code, body := x.xmrClaim(order, x.vendorSess, testXMRAddr("claim "+order, true), testPassword); code != 303 {
			t.Fatalf("claim: %d %s", code, body)
		}
		x.mustPoll()
		return x.outputsOf(x.settlementOf(order, "release").ID)
	}

	// Exactly the price: no overpaid notice, and the release states no return.
	exact, exactBuyer, _ := x.xmrPaidEscrow(xmrListingPrice)
	for _, sess := range []string{exactBuyer, x.vendorSess} {
		mustNotContain(t, text(orderPage(exact), sess), "Overpaid.", "paid above the price")
	}
	mustContain(t, text(orderPage(exact), x.vendorSess), "Cancelling refunds the 0.5"+coin+" in this escrow, "+escrowRefundSentence)
	dispute(exact, exactBuyer)
	release := "Release to " + vendor + ": the vendor claims the escrowed 0.5" + coin + " with a fresh address, and the market builds the release from this order's escrow account and sends it (less the market fee and the network fee)"
	mustContain(t, text("/moderator", admin), release)
	mustContain(t, text(orderPage(exact), exactBuyer), release+". This closes the dispute")

	// Paid 0.1 XMR above the price: the release returns it to the refund address in full.
	over, overBuyer, overRefund := x.xmrPaidEscrow(xmrListingPrice + 100000000000)
	back := "the 0.1" + coin + " paid above the price goes back in full to the buyer's refund address, as its own output of the release"
	notice := "<strong>Overpaid.</strong> This escrow holds 0.1" + coin + " more than the price. On a release to the vendor, " + back +
		". A refund instead returns everything in this escrow (0.6" + coin + "), less the network fee, to the buyer's refund address."
	for _, sess := range []string{overBuyer, x.vendorSess} {
		mustContain(t, text(orderPage(over), sess), notice)
	}
	mustContain(t, text(orderPage(over), x.vendorSess), "Cancelling refunds the 0.6"+coin+" in this escrow, "+escrowRefundSentence)
	dispute(over, overBuyer)
	mustContain(t, text("/moderator", admin), "(less the market fee and the network fee; "+back+")")
	mustContain(t, text(orderPage(over), overBuyer), notice, "(less the market fee and the network fee; "+back+"). This closes the dispute")
	if got := releaseAndBuild(over, xmrListingPrice+100000000000); len(got) != 2 || !strings.HasPrefix(got[0], "claimant:") ||
		got[1] != "buyer_return:"+overRefund+":100000000000" {
		t.Fatalf("the release of the overpaid order builds %q, not the stated return of 0.1 XMR to %s", got, overRefund)
	}

	// Paid 0.0005 XMR above the price, below the 0.001 XMR minimum output: the release adds it to the vendor's output.
	dust, dustBuyer, _ := x.xmrPaidEscrow(xmrListingPrice + minPayoutXMR/2)
	kept := "the 0.0005" + coin + " paid above the price is below the minimum output of 0.001 XMR, so the release adds it to the vendor's output"
	mustContain(t, text(orderPage(dust), dustBuyer), "<strong>Overpaid.</strong> This escrow holds 0.0005"+coin+" more than the price. On a release to the vendor, "+kept+".")
	dispute(dust, dustBuyer)
	mustContain(t, text("/moderator", admin), "(less the market fee and the network fee; "+kept+")")
	if got := releaseAndBuild(dust, xmrListingPrice+minPayoutXMR/2); len(got) != 1 || !strings.HasPrefix(got[0], "claimant:"+testXMRAddr("claim "+dust, true)+":") {
		t.Fatalf("the release of the order overpaid below the floor builds %q, not one output to the vendor", got)
	}

	// A refund of a paid, overpaid order: everything, less the network fee, to the refund address.
	refunded, _, refundTo := x.xmrPaidEscrow(xmrListingPrice + 100000000000)
	x.check(x.do("POST", "/orders/cancel", x.vendorSess, payoutConfirmed(url.Values{"order_id": {refunded}, "from": {statePaid}}, xmrListingPrice+100000000000)), 303)
	x.mustPoll()
	got := x.outputsOf(x.settlementOf(refunded, "refund").ID)
	if len(got) != 1 || !strings.HasPrefix(got[0], "claimant:"+refundTo+":") {
		t.Fatalf("the refund builds %q, not one output to %s", got, refundTo)
	}
	if n, _ := strconv.ParseInt(got[0][strings.LastIndex(got[0], ":")+1:], 10, 64); n >= xmrListingPrice+100000000000 || n < xmrListingPrice+90000000000 {
		t.Fatalf("the refund pays %d, not 0.6 XMR less the network fee", n)
	}

	// An underpaid order awaiting payment: cancelling refunds what arrived.
	under, addr := x.freshCheckout(x.digital(x.vendor))
	x.fake.payAddress(addr, hex64("under "+under), xmrListingPrice/5, 10)
	x.mustPoll()
	page := text(orderPage(under), x.sessOf(under, roleBuyer))
	mustContain(t, page, "Cancelling refunds the 0.1"+coin+" in this escrow, "+escrowRefundSentence)
	mustNotContain(t, page, "Overpaid.")
}

func TestEscrowCopyStatesTheSettlementFloorMonero(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("xfloor_admin_"+randomToken()[:6], "admin")
	rule := "any amount paid above the price goes back to the buyer's refund address, except that one below 0.001 XMR goes to the vendor"
	page := func(order, sess string) string {
		t.Helper()
		body := html.UnescapeString(x.page(orderPage(order), sess))
		mustNotContain(t, body, pooledPayoutCopy...)
		return body
	}
	notes := func(order string) string { return html.UnescapeString(strings.Join(x.events(order), "\n")) }
	resolveRelease := func(order string) {
		t.Helper()
		id := agStrings(t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)[0]
		x.check(x.do("POST", "/resolve", admin, payoutConfirmed(url.Values{"id": {id}, "outcome": {"release"},
			"resolution": {"Decided from the evidence both parties sent."}, "reply_seen": {"0"}, "password": {testPassword}}, xmrListingPrice)), 303)
	}
	physical := x.physical(x.vendor)

	// The ship help and the release requested at shipment.
	paid, _ := x.paidXMRPhysical(physical)
	mustContain(t, page(paid, x.vendorSess), `id="xmr-escrow-ship-help"`, rule)
	shipped, shippedBuyer, _ := x.shippedXMR(physical)
	mustContain(t, notes(shipped), "Release requested at shipment (Monero escrow)", rule)

	// A decision for the vendor over that release.
	if code, body := x.xmrDispute(shipped, shippedBuyer); code != 303 {
		t.Fatalf("dispute: %d %s", code, body)
	}
	resolveRelease(shipped)
	mustContain(t, notes(shipped), "Dispute decided for the vendor: the release to the vendor requested when the order was shipped", rule)

	// A decision for the vendor with no release yet: the claim help, the decision's note and the claim's note.
	decided, decidedBuyer, _ := x.xmrPaidEscrow(xmrListingPrice)
	if code, body := x.xmrDispute(decided, decidedBuyer); code != 303 {
		t.Fatalf("dispute: %d %s", code, body)
	}
	resolveRelease(decided)
	mustContain(t, notes(decided), "Dispute decided for the vendor: the vendor claims the escrowed 0.5", rule)
	mustContain(t, page(decided, x.vendorSess), `id="xmr-escrow-claim-help"`, rule)
	if code, body := x.xmrClaim(decided, x.vendorSess, testXMRAddr("claim "+decided, true), testPassword); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	mustContain(t, notes(decided), "Vendor requested the escrow claim (Monero escrow)", rule)

	message := subFloorListing("XMR", 0)
	mustContain(t, message, "below the escrow settlement minimum of 0.001 XMR")
	mustNotContain(t, message, pooledPayoutCopy...)
}

func TestEscrowExcessFloorBoundaryMonero(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	net := x.A.xmrEscrowNetKnown()
	coin := " " + net + " XMR"
	_, admin := x.user("xefb_admin_"+randomToken()[:6], "admin")
	last := func(s string) int64 {
		n, _ := strconv.ParseInt(s[strings.LastIndex(s, ":")+1:], 10, 64)
		return n
	}
	for _, c := range []struct {
		name   string
		excess int64
		own    bool
		notice string
	}{
		{"at the floor", minPayoutXMR, true, "the 0.001" + coin + " paid above the price goes back in full to the buyer's refund address, as its own output of the release"},
		{"one atom under", minPayoutXMR - 1, false, "the 0.000999999999" + coin + " paid above the price is below the minimum output of 0.001 XMR, so the release adds it to the vendor's output"},
	} {
		order, buyer, refund := x.xmrPaidEscrow(xmrListingPrice + c.excess)
		page := html.UnescapeString(x.page(orderPage(order), buyer))
		mustContain(t, page, "<strong>Overpaid.</strong>", "On a release to the vendor, "+c.notice+".")
		if code, body := x.xmrDispute(order, buyer); code != 303 {
			t.Fatalf("%s: dispute: %d %s", c.name, code, body)
		}
		id := agStrings(t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)[0]
		mustContain(t, html.UnescapeString(x.page("/moderator", admin)), "(less the market fee and the network fee; "+c.notice+")")
		x.check(x.do("POST", "/resolve", admin, payoutConfirmed(url.Values{"id": {id}, "outcome": {"release"},
			"resolution": {"Decided from the evidence both parties sent."}, "reply_seen": {"0"}, "password": {testPassword}}, xmrListingPrice+c.excess)), 303)
		claimTo := testXMRAddr("claim "+order, true)
		if code, body := x.xmrClaim(order, x.vendorSess, claimTo, testPassword); code != 303 {
			t.Fatalf("%s: claim: %d %s", c.name, code, body)
		}
		x.mustPoll()
		release := x.settlementOf(order, "release")
		got := x.outputsOf(release.ID)
		if !strings.HasPrefix(got[0], "claimant:"+claimTo+":") {
			t.Fatalf("%s: outputs %q", c.name, got)
		}
		claimant, fee := last(got[0]), int64(agInt(x.testEnv, "SELECT fee FROM xmr_settlements WHERE id=$1", release.ID))
		if c.own {
			// The excess is its own output; the vendor gets the price less the network fee.
			if len(got) != 2 || got[1] != "buyer_return:"+refund+":"+strconv.FormatInt(c.excess, 10) || claimant != xmrListingPrice-fee {
				t.Fatalf("%s: outputs %q (fee %d), not the vendor's claim and a return of %d to %s", c.name, got, fee, c.excess, refund)
			}
		} else if len(got) != 1 || claimant != xmrListingPrice+c.excess-fee {
			// The vendor's output holds the price and the excess, less the network fee.
			t.Fatalf("%s: outputs %q (fee %d), not one claimant output holding the excess", c.name, got, fee)
		}
	}
}
