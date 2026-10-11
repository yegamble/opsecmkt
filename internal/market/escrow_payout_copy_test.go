package market

import (
	"html"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// A-508: what an escrow order's page says about its coins is the settlement the code builds (planSettlement,
// planXMRSettlement): a release pays the vendor the escrow less the market fee and the network fee, and an amount paid
// above the price goes back in full to the buyer's refund address as its own output of the release, or is added to the
// vendor's output when it is below the minimum output (payoutFloor); a refund returns everything, less the network fee,
// to the buyer's refund address. Nothing names a saved payout address, a minimum automatic payout, an administrator's
// review of a return, or a separate return payout: those were the pooled wallet's (removed, A-492). Each text is checked
// against the outputs the settlement then builds. Fake escrow Core and wallet-rpc: simulated RPC, not chain evidence.

// pooledPayoutCopy is copy only the removed pooled payouts used; no escrow order page may show it.
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

func TestEscrowOrderPageStatesTheBitcoinSettlement(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("ecopy_admin_"+randomToken()[:6], "admin")
	vendor, buyer := handleOf(x.testEnv, x.vendor), handleOf(x.testEnv, x.buyer)
	text := func(path, sess string) string {
		t.Helper()
		body := html.UnescapeString(x.page(path, sess))
		mustNotContain(t, body, pooledPayoutCopy...)
		return body
	}
	dispute := func(order string) {
		t.Helper()
		x.check(x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The content was never delivered to me at all."}}), 303)
	}
	// releaseAndBuild decides order's open dispute for the vendor, has the vendor claim, builds the claim and returns its
	// outputs.
	releaseAndBuild := func(order string, seen int64) []string {
		t.Helper()
		id := agStr(x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)
		x.check(x.do("POST", "/resolve", admin, payoutConfirmed(url.Values{"id": {id}, "outcome": {"release"},
			"resolution": {"Decided from the evidence both parties sent."}, "reply_seen": {"0"}, "password": {testPassword}}, seen)), 303)
		if code, body := x.claim(order, x.vendorSess, regtestAddr(order+"-claim")); code != 303 {
			t.Fatalf("claim: %d %s", code, body)
		}
		x.poll()
		return x.outputs(order)
	}

	// Exactly the price: no overpaid notice, and the release states no return.
	exact, _ := x.paidEscrow("copy-exact", 100000)
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		mustNotContain(t, text(orderPage(exact), sess), "Overpaid.", "paid above the price")
	}
	mustContain(t, text(orderPage(exact), x.vendorSess), "Cancelling refunds the 0.001 regtest BTC in this escrow, "+escrowRefundSentence)
	dispute(exact)
	release := "Release to " + vendor + ": the vendor claims the escrowed 0.001 regtest BTC by a settlement from the escrow (less the market fee and the network fee)"
	refund := "Refund to " + buyer + ": the escrowed 0.001 regtest BTC, less the network fee, go back to the buyer's refund address by a settlement from the escrow"
	mustContain(t, text("/moderator", admin), release, refund)
	mustContain(t, text(orderPage(exact), x.buyerSess), release+". This closes the dispute")
	mustContain(t, text(orderPage(exact), x.vendorSess), refund+". This closes the dispute")

	// Paid 0.0005 BTC above the price, worth its own output: the release returns it to the refund address in full.
	over, overRefund := x.paidEscrow("copy-over", 100000, 50000)
	back := "the 0.0005 regtest BTC paid above the price goes back in full to the buyer's refund address, as its own output of the release"
	notice := "<strong>Overpaid.</strong> This escrow holds 0.0005 regtest BTC more than the price. On a release to the vendor, " + back +
		". A refund instead returns everything in this escrow (0.0015 regtest BTC), less the network fee, to the buyer's refund address."
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		mustContain(t, text(orderPage(over), sess), notice)
	}
	mustContain(t, text(orderPage(over), x.vendorSess), "Cancelling refunds the 0.0015 regtest BTC in this escrow, "+escrowRefundSentence)
	dispute(over)
	mustContain(t, text("/moderator", admin), "Release to "+vendor+": the vendor claims the escrowed 0.0015 regtest BTC by a settlement from the escrow (less the market fee and the network fee; "+back+")")
	mustContain(t, text(orderPage(over), x.buyerSess), notice, "(less the market fee and the network fee; "+back+"). This closes the dispute")
	if got := releaseAndBuild(over, 150000); len(got) != 2 || !strings.HasPrefix(got[0], "claimant ") || got[1] != "buyer_return "+overRefund+" 50000" {
		t.Fatalf("the release of the overpaid order builds %q, not the stated return of 50000 to %s", got, overRefund)
	}

	// Paid 0.00005 BTC above the price, below the 0.0001 BTC minimum output: the release adds it to the vendor's output.
	dust, _ := x.paidEscrow("copy-dust", 100000, 5000)
	kept := "the 0.00005 regtest BTC paid above the price is below the minimum output of 0.0001 BTC, so the release adds it to the vendor's output"
	mustContain(t, text(orderPage(dust), x.buyerSess), "<strong>Overpaid.</strong> This escrow holds 0.00005 regtest BTC more than the price. On a release to the vendor, "+kept+".")
	dispute(dust)
	mustContain(t, text("/moderator", admin), "(less the market fee and the network fee; "+kept+")")
	if got := releaseAndBuild(dust, 105000); len(got) != 1 || !strings.HasPrefix(got[0], "claimant "+regtestAddr(dust+"-claim")+" ") {
		t.Fatalf("the release of the order overpaid below the floor builds %q, not one output to the vendor", got)
	}

	// A refund of a paid, overpaid order: everything in the escrow, less the network fee, to the refund address.
	refunded, refundTo := x.paidEscrow("copy-refund", 100000, 50000)
	x.check(x.do("POST", "/orders/cancel", x.vendorSess, payoutConfirmed(url.Values{"order_id": {refunded}, "from": {statePaid}}, 150000)), 303)
	x.poll()
	got := x.outputs(refunded)
	if len(got) != 1 || !strings.HasPrefix(got[0], "claimant "+refundTo+" ") {
		t.Fatalf("the refund builds %q, not one output to %s", got, refundTo)
	}
	if sats, _ := strconv.ParseInt(got[0][strings.LastIndex(got[0], " ")+1:], 10, 64); sats >= 150000 || sats < 149000 {
		t.Fatalf("the refund pays %d, not 0.0015 BTC less the network fee", sats)
	}
	mustNotContain(t, text(orderPage(refunded), x.buyerSess), "Overpaid.")

	// An underpaid order awaiting payment: cancelling refunds what arrived.
	under := x.draft()
	x.mustCheckout(under, regtestAddr("copy-under-refund"))
	x.fund(under, "copy-under", 3, 40000)
	page := text(orderPage(under), x.buyerSess)
	mustContain(t, page, "Cancelling refunds the 0.0004 regtest BTC in this escrow, "+escrowRefundSentence)
	mustNotContain(t, page, "Overpaid.")
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

// A-509: the short statements of what a release does with an amount paid above the price (the ship and claim help and
// the order notes) carry the floor planSettlement and planXMRSettlement apply: an excess below the minimum output is
// added to the vendor's output, not returned. They never call it a "minimum automatic payout" (pooled wording).
func TestEscrowCopyStatesTheSettlementFloorBitcoin(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("efloor_admin_"+randomToken()[:6], "admin")
	rule := "any amount paid above the price goes back to the buyer's refund address, except that one below 0.0001 BTC goes to the vendor"
	page := func(order, sess string) string {
		t.Helper()
		body := html.UnescapeString(x.page(orderPage(order), sess))
		mustNotContain(t, body, pooledPayoutCopy...)
		return body
	}
	notes := func(order string) string { return html.UnescapeString(strings.Join(x.events(order), "\n")) }

	// The ship help and the release requested at shipment.
	shipped := x.paidPhysical(regtestAddr("floor-ship-refund"), 100000)
	mustContain(t, page(shipped, x.vendorSess), `id="escrow-ship-help"`, rule)
	if code, body := x.shipEscrow(shipped, x.vendorSess, regtestAddr("floor-ship-claim"), testPassword); code != 303 {
		t.Fatalf("ship: %d %s", code, body)
	}
	mustContain(t, notes(shipped), "Release requested at shipment (Bitcoin escrow)", rule)

	// A decision for the vendor: the claim help and the decision's note.
	decided, _ := x.paidEscrow("floor-decided", 100000)
	x.check(x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {decided}, "reason": {"The content was never delivered to me at all."}}), 303)
	id := agStr(x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", decided)
	x.check(x.do("POST", "/resolve", admin, payoutConfirmed(url.Values{"id": {id}, "outcome": {"release"},
		"resolution": {"Decided from the evidence both parties sent."}, "reply_seen": {"0"}, "password": {testPassword}}, 100000)), 303)
	mustContain(t, notes(decided), "Dispute decided for the vendor: the vendor claims the escrowed 0.001 BTC", rule)
	mustContain(t, page(decided, x.vendorSess), `id="escrow-claim-help"`, rule)

	// A decision for the vendor over a release built at shipment.
	live, _ := x.shippedPhysical(regtestAddr("floor-live-refund"), regtestAddr("floor-live-claim"), 100000)
	x.check(x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {live}, "reason": {"The goods never arrived at my address at all."}}), 303)
	id = agStr(x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", live)
	x.check(x.do("POST", "/resolve", admin, payoutConfirmed(url.Values{"id": {id}, "outcome": {"release"},
		"resolution": {"Decided from the evidence both parties sent."}, "reply_seen": {"0"}, "password": {testPassword}}, 100000)), 303)
	mustContain(t, notes(live), "Dispute decided for the vendor: the release to the vendor built when the order was shipped", rule)

	// The listing refusal names the escrow floor, not an automatic payout.
	message := subFloorListing("BTC", 0)
	mustContain(t, message, "below the escrow settlement minimum of 0.0001 BTC")
	mustNotContain(t, message, pooledPayoutCopy...)
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

// A-509: once a dispute's release is final (its transaction is signed or stored, or spent on the network) the escrow
// pays the vendor and the resolve form refuses a refund, so the overpaid notice must not offer one.
func TestEscrowOverpaidNoticeWithAFinalReleaseOffersNoRefund(t *testing.T) {
	const refundSentence = "A refund instead returns everything in this escrow"
	const final = "The release to the vendor is already final, so a refund is no longer possible."
	t.Run("bitcoin", func(t *testing.T) {
		x := newEscrowEnv(t)
		order, id := x.shippedPhysical(regtestAddr("final-refund"), regtestAddr("final-claim"), 100000, 50000)
		x.check(x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The goods never arrived at my address at all."}}), 303)
		for _, sess := range []string{x.buyerSess, x.vendorSess} {
			mustContain(t, html.UnescapeString(x.page(orderPage(order), sess)), "<strong>Overpaid.</strong>", refundSentence)
		}
		agExec(x.testEnv, "UPDATE settlements SET state='signed',raw_tx='00',cosigned='parties',final_at=now() WHERE id=$1", id)
		for _, sess := range []string{x.buyerSess, x.vendorSess} {
			body := html.UnescapeString(x.page(orderPage(order), sess))
			mustContain(t, body, "<strong>Overpaid.</strong> This escrow holds 0.0005 regtest BTC more than the price. On a release to the vendor, ", final)
			mustNotContain(t, body, refundSentence)
		}
	})
	t.Run("monero", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, buyer, _ := x.shippedXMR(x.physical(x.vendor))
		addr := agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1 AND coin='XMR'", order)[0]
		x.fake.payAddress(addr, hex64("top-up "+order), 100000000000, 10)
		x.mustPoll()
		if code, body := x.xmrDispute(order, buyer); code != 303 {
			t.Fatalf("dispute: %d %s", code, body)
		}
		for _, sess := range []string{buyer, x.vendorSess} {
			mustContain(t, html.UnescapeString(x.page(orderPage(order), sess)), "<strong>Overpaid.</strong>", refundSentence)
		}
		// Written past the order state the database requires for a stored transaction, as TestXMREscrowDisputeSupersedes does.
		agExec(x.testEnv, "UPDATE orders SET state='completed' WHERE id=$1", order)
		agExec(x.testEnv, "UPDATE xmr_settlements SET state='built',tx_hash=$2,tx_metadata='ab01',tx_blob='cd02',fee=30000000,input_total=600000000000,key_images=ARRAY[$2] WHERE order_id=$1",
			order, txidOf("final "+order))
		agExec(x.testEnv, "UPDATE orders SET state='disputed' WHERE id=$1", order)
		for _, sess := range []string{buyer, x.vendorSess} {
			body := html.UnescapeString(x.page(orderPage(order), sess))
			mustContain(t, body, "<strong>Overpaid.</strong> This escrow holds 0.1", final)
			mustNotContain(t, body, refundSentence)
		}
	})
}

// A-509 (QA59c): the floor is inclusive. An excess of exactly the minimum output is its own buyer_return output of the
// release; one satoshi (atom) less is added to the vendor's claimant output. The order page's notice says which, and the
// built settlement_outputs agree.
func TestEscrowExcessFloorBoundaryBitcoin(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("efb_admin_"+randomToken()[:6], "admin")
	last := func(s, sep string) int64 {
		n, _ := strconv.ParseInt(s[strings.LastIndex(s, sep)+1:], 10, 64)
		return n
	}
	for _, c := range []struct {
		name   string
		excess int64
		own    bool
		notice string
	}{
		{"at the floor", minPayoutBTC, true, "the 0.0001 regtest BTC paid above the price goes back in full to the buyer's refund address, as its own output of the release"},
		{"one satoshi under", minPayoutBTC - 1, false, "the 0.00009999 regtest BTC paid above the price is below the minimum output of 0.0001 BTC, so the release adds it to the vendor's output"},
	} {
		order, refund := x.paidEscrow("boundary-"+c.name, 100000, c.excess)
		page := html.UnescapeString(x.page(orderPage(order), x.buyerSess))
		mustContain(t, page, "<strong>Overpaid.</strong>", "On a release to the vendor, "+c.notice+".")
		x.check(x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The content was never delivered to me at all."}}), 303)
		id := agStr(x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)
		mustContain(t, html.UnescapeString(x.page("/moderator", admin)), "(less the market fee and the network fee; "+c.notice+")")
		x.check(x.do("POST", "/resolve", admin, payoutConfirmed(url.Values{"id": {id}, "outcome": {"release"},
			"resolution": {"Decided from the evidence both parties sent."}, "reply_seen": {"0"}, "password": {testPassword}}, 100000+c.excess)), 303)
		if code, body := x.claim(order, x.vendorSess, regtestAddr(order+"-claim")); code != 303 {
			t.Fatalf("%s: claim: %d %s", c.name, code, body)
		}
		x.poll()
		got := x.outputs(order)
		claimant := last(got[0], " ")
		if !strings.HasPrefix(got[0], "claimant ") {
			t.Fatalf("%s: outputs %q", c.name, got)
		}
		if c.own {
			// The excess is its own output; the vendor gets the price less the network fee.
			if len(got) != 2 || got[1] != "buyer_return "+refund+" "+strconv.FormatInt(c.excess, 10) || claimant >= 100000 || claimant < 99000 {
				t.Fatalf("%s: outputs %q, not the vendor's claim and a return of %d to %s", c.name, got, c.excess, refund)
			}
		} else if len(got) != 1 || claimant <= 100000+c.excess-1000 || claimant >= 100000+c.excess {
			// The vendor's output holds the price and the excess, less the network fee.
			t.Fatalf("%s: outputs %q, not one claimant output holding the excess", c.name, got)
		}
	}
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

// A-509 (QA59c): the overpaid notice counts the deposits the settlement counts (escrowCountedFrom): a deposit still
// confirming counts; a conflicted one, a locked transfer, one after the close and one left out of the release do not,
// and a left-out one is not part of the release's excess (sum less left) although a refund-sized total includes it.
func TestEscrowOverpaidNoticeCountsOnlyCountedDeposits(t *testing.T) {
	type tc struct {
		name, set string // set is the UPDATE applied to the extra deposit
		notice    bool
	}
	cases := []tc{
		{"counted", "confirmations=3", true},
		{"still confirming", "confirmations=0", true},
		{"conflicted", "confirmations=-1", false},
		{"locked transfer", "locked=true", false},
		{"after the close", "after_close=true", false},
		{"left out of the release", "escrow_left_out=true", false},
	}
	t.Run("bitcoin", func(t *testing.T) {
		x := newEscrowEnv(t)
		for _, c := range cases {
			order, _ := x.paidEscrow("counted-"+c.name, 100000, 50000)
			agExec(x.testEnv, "UPDATE payments SET "+c.set+" WHERE order_id=$1 AND amount=50000", order)
			page := html.UnescapeString(x.page(orderPage(order), x.buyerSess))
			if got := strings.Contains(page, "<strong>Overpaid.</strong> This escrow holds 0.0005 regtest BTC more than the price."); got != c.notice {
				t.Errorf("%s: overpaid notice shown %v, want %v", c.name, got, c.notice)
			}
		}
		// A deposit left out beside one that counts: the notice holds the counted excess only, and the refund sentence the
		// counted total only.
		order, _ := x.paidEscrow("counted-mixed", 100000, 20000, 50000)
		agExec(x.testEnv, "UPDATE payments SET escrow_left_out=true WHERE order_id=$1 AND amount=50000", order)
		mustContain(t, html.UnescapeString(x.page(orderPage(order), x.buyerSess)),
			"<strong>Overpaid.</strong> This escrow holds 0.0002 regtest BTC more than the price.",
			"A refund instead returns everything in this escrow (0.0012 regtest BTC)")
	})
	t.Run("monero", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		const extra = 100000000000
		topUp := func(order string, amount int64) {
			t.Helper()
			addr := agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1 AND coin='XMR'", order)[0]
			x.fake.payAddress(addr, hex64("extra "+order+strconv.FormatInt(amount, 10)), amount, 10)
			x.mustPoll()
		}
		coin := " " + x.A.xmrEscrowNetKnown() + " XMR"
		for _, c := range cases {
			order, buyer, _ := x.xmrPaidEscrow(xmrListingPrice)
			topUp(order, extra)
			agExec(x.testEnv, "UPDATE payments SET "+c.set+" WHERE order_id=$1 AND amount=$2", order, int64(extra))
			page := html.UnescapeString(x.page(orderPage(order), buyer))
			if got := strings.Contains(page, "<strong>Overpaid.</strong> This escrow holds 0.1"+coin+" more than the price."); got != c.notice {
				t.Errorf("%s: overpaid notice shown %v, want %v", c.name, got, c.notice)
			}
		}
		order, buyer, _ := x.xmrPaidEscrow(xmrListingPrice)
		topUp(order, extra/5)
		topUp(order, extra)
		agExec(x.testEnv, "UPDATE payments SET escrow_left_out=true WHERE order_id=$1 AND amount=$2", order, int64(extra))
		mustContain(t, html.UnescapeString(x.page(orderPage(order), buyer)),
			"<strong>Overpaid.</strong> This escrow holds 0.02"+coin+" more than the price.",
			"A refund instead returns everything in this escrow (0.52"+coin+")")
	})
}
