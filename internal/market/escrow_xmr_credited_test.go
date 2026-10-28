package market

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-554 (PAY66-1, PAY66-2): a Monero release or refund never builds from the confirmed subset of its order's deposits
// while a deposit credited toward the order's funding is below the confirmation threshold again (a reorganisation), and
// never leaves a credited deposit out for the stray refund to the buyer. A credited deposit the wallet no longer lists
// (-1) is flagged once and waited on for 2 × threshold blocks; then the settlement is built without it. A settlement
// whose credited deposits are all missing waits rather than failing nothing-to-settle. DB integration tests against
// the simulated wallet-rpc of escrow_xmr_fake_test.go (simulated RPC, not chain evidence:
// TestXMREscrowRegtestFundingReorg is the real-chain leg).

// fundedTwice is a digital order funded with its price in two deposits, the first deeper than the second, both at the
// confirmation threshold or above (both credited; delivered on funding and completed when its dispute window ends), with the vendor's claim requested through the
// claim form: the order, the two deposits and the release.
func (x *xmrEscrowEnv) fundedTwice() (order, first, second string, release int64) {
	x.t.Helper()
	order, addr := x.freshCheckout(x.digital(x.vendor))
	first, second = hex64("first "+order), hex64("second "+order)
	thr := x.A.escrowXMR.confirmations
	x.fake.payAddress(addr, first, xmrListingPrice/2, thr+10)
	x.fake.payAddress(addr, second, xmrListingPrice-xmrListingPrice/2, thr+2)
	x.mustPoll()
	x.endWindows()
	if x.state(order) != stateCompleted || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND credited", order) != 2 {
		x.t.Fatalf("funded in two deposits: %s, deposits %v", x.state(order), x.creditedRows(order))
	}
	return order, first, second, x.claimed(order)
}

// claimed posts the vendor's claim of order to a fresh address and returns the release it requested.
func (x *xmrEscrowEnv) claimed(order string) int64 {
	x.t.Helper()
	if code, body := x.xmrClaim(order, x.vendorSess, testXMRAddr("claim "+order, true), testPassword); code != 303 {
		x.t.Fatalf("claim: %d %s", code, body)
	}
	return x.settlementOf(order, "release").ID
}

// creditedRows lists order's deposits as confirmations credited flagged regress_notice left_out claimed, deepest first.
func (x *xmrEscrowEnv) creditedRows(order string) []string {
	return agStrings(x.t, x.testEnv, `SELECT confirmations||' '||credited||' '||flagged||' '||regress_notice||' '||escrow_left_out||' '||(xmr_settlement_id IS NOT NULL)
		FROM payments WHERE order_id=$1 ORDER BY confirmations DESC,id`, order)
}

// dropTransfer removes txid from every fake wallet file: the wallet no longer lists it (a reorganisation that took it
// out of the chain and a daemon that no longer has it in its pool).
func (x *xmrEscrowEnv) dropTransfer(txid string) {
	x.fake.edit(func(f *fakeXMREscrow) {
		for _, w := range f.files {
			w.transfers = slices.DeleteFunc(w.transfers, func(t *fakeXMRTransfer) bool { return t.TxID == txid })
		}
	})
}

// A credited funding deposit back below the threshold holds the release (waiting-confirmations, no transfer call),
// flags staff once, leaves nothing out and requests no stray refund; once it confirms again the release spends both
// deposits, the price.
func TestXMREscrowCreditedRegressionHoldsRelease(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, second, id := x.fundedTwice()
	flags := x.staffFlags(order)
	x.fake.setConfs(second, 3) // a reorganisation: the shallower deposit is at 3 confirmations again
	x.mustPoll()
	x.mustPoll()
	x.mustPoll()
	if s := x.settlement(id); s.State != "requested" || x.waitClass(id) != xmrWaitConfirm || x.fake.countOf("transfer") != 0 {
		t.Fatalf("with a credited deposit at 3 confirmations: %+v %q, transfer %d; deposits %v", s, x.waitClass(id), x.fake.countOf("transfer"), x.creditedRows(order))
	}
	if got := x.staffFlags(order) - flags; got != 1 {
		t.Fatalf("staff were told %d times of the regression; events %q", got, x.events(order))
	}
	if got := x.creditedRows(order); !slices.Equal(got, []string{"20 true false false false false", "3 true true true false false"}) {
		t.Fatalf("deposits while held: %v", got)
	}
	if got := x.xmrSettlements(order); len(got) != 1 {
		t.Fatalf("settlements while held: %v", got)
	}
	x.fake.setConfs(second, x.A.escrowXMR.confirmations)
	x.mustPoll()
	s := x.settlement(id)
	if s.State != "relayed" || s.InputTotal != xmrListingPrice || len(s.KeyImages) != 2 || x.fake.countOf("transfer") != 1 {
		t.Fatalf("confirmed again: %+v, transfer %d; deposits %v", s, x.fake.countOf("transfer"), x.creditedRows(order))
	}
	x.mustPoll()
	if got := x.creditedRows(order); !slices.Equal(got, []string{"20 true false false false true", "10 true true false false true"}) {
		t.Fatalf("deposits after the release: %v", got)
	}
	if got := x.xmrSettlements(order); len(got) != 1 || x.staffFlags(order)-flags != 1 {
		t.Fatalf("after the release: settlements %v, %d staff flags", got, x.staffFlags(order)-flags)
	}
	for _, n := range x.events(order) {
		if strings.Contains(n, "arrived at this order's Monero escrow after") {
			t.Fatalf("a funding deposit was called a top-up: %q", n)
		}
	}
}

// A release whose only (credited) deposit the wallet no longer lists waits and is flagged once, never failed
// nothing-to-settle, even past the bounded wait (there is nothing to build without it); listed again at 30
// confirmations it is built and relayed; the vendor still cannot claim twice.
func TestXMREscrowMissingDepositHoldsRelease(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, addr, txid := x.completed()
	id := x.claimed(order)
	flags := x.staffFlags(order)
	x.dropTransfer(txid)
	x.mustPoll()
	x.mustPoll()
	if s := x.settlement(id); s.State != "requested" || x.waitClass(id) != xmrWaitMissing || x.fake.countOf("transfer") != 0 {
		t.Fatalf("its deposit missing: %+v %q; deposits %v", s, x.waitClass(id), x.creditedRows(order))
	}
	if got := x.staffFlags(order) - flags; got != 1 {
		t.Fatalf("staff were told %d times; events %q", got, x.events(order))
	}
	x.fake.mine(int(3 * x.A.escrowXMR.confirmations))
	x.mustPoll()
	x.mustPoll()
	if s := x.settlement(id); s.State != "requested" || x.waitClass(id) != xmrWaitMissing || x.staffFlags(order)-flags != 1 {
		t.Fatalf("past the bounded wait with nothing else to spend: %+v %q, %d staff flags", s, x.waitClass(id), x.staffFlags(order)-flags)
	}
	if !strings.Contains(x.panel(order, x.vendorSess), "no longer listed by the escrow wallet") {
		t.Fatalf("the vendor's panel does not say why the release waits:\n%s", x.panel(order, x.vendorSess))
	}
	x.fake.payAddress(addr, txid, xmrListingPrice, 30)
	x.mustPoll()
	if s := x.settlement(id); s.State != "relayed" || s.InputTotal != xmrListingPrice || x.fake.countOf("transfer") != 1 {
		t.Fatalf("listed again at 30 confirmations: %+v; deposits %v", s, x.creditedRows(order))
	}
	if code, body := x.xmrClaim(order, x.vendorSess, testXMRAddr("claim again "+order, true), testPassword); code != 409 || !strings.Contains(body, errClaimTwice.Error()) {
		t.Fatalf("a second claim: %d %s", code, body)
	}
}

// A credited deposit read as double-spent (-1) holds the release for 2 × threshold blocks of tip advance from when it
// was first seen missing, then the release is built from the other deposit alone with an accurate note; the missing
// deposit is not left out, stays flagged for staff (told once) and no stray refund is requested.
func TestXMREscrowDoubleSpentDepositBoundedWait(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, first, second, id := x.fundedTwice()
	flags := x.staffFlags(order)
	x.fake.setTransfer(second, func(t *fakeXMRTransfer) { t.DoubleSpend = true })
	x.mustPoll()
	bound := int(2 * x.A.escrowXMR.confirmations)
	x.fake.mine(bound - 1)
	x.mustPoll()
	if s := x.settlement(id); s.State != "requested" || x.waitClass(id) != xmrWaitMissing || x.fake.countOf("transfer") != 0 || x.staffFlags(order)-flags != 1 {
		t.Fatalf("%d blocks after its deposit went missing: %+v %q, %d staff flags; deposits %v", bound-1, s, x.waitClass(id), x.staffFlags(order)-flags, x.creditedRows(order))
	}
	x.fake.mine(1)
	x.mustPoll()
	s := x.settlement(id)
	outs := x.fake.outputs(agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0])
	if s.State != "relayed" || s.InputTotal != xmrListingPrice/2 || len(s.KeyImages) != 1 || outs[0].TxID != first || s.KeyImages[0] != outs[0].KeyImage {
		t.Fatalf("after the bounded wait: %+v; deposits %v", s, x.creditedRows(order))
	}
	x.mustPoll()
	x.mustPoll()
	if got := x.creditedRows(order); !slices.Equal(got, []string{"20 true false false false true", "-1 true true true false false"}) {
		t.Fatalf("deposits after the release: %v", got)
	}
	if got := x.xmrSettlements(order); len(got) != 1 || x.staffFlags(order)-flags != 1 {
		t.Fatalf("after the release: settlements %v, %d staff flags", got, x.staffFlags(order)-flags)
	}
	ev := strings.Join(x.events(order), "\n")
	want := "This release was built without " + depositName(second, xmrListingPrice-xmrListingPrice/2, "XMR") + ", which was credited toward this order's funding: the escrow wallet has not listed it for " +
		strconv.Itoa(bound) + " blocks"
	if !strings.Contains(ev, want) || strings.Contains(ev, "arrived at this order's Monero escrow after") {
		t.Fatalf("the order notes do not say why the release left the deposit out:\n%s", ev)
	}
	// Listed again and confirmed after the release was built without it: still never left out for a stray refund to the
	// buyer; it stays in the escrow account, its flag open for staff.
	x.fake.setTransfer(second, func(t *fakeXMRTransfer) { t.DoubleSpend = false })
	x.mustPoll()
	x.mustPoll()
	if got := x.creditedRows(order); !slices.Equal(got, []string{"20 true false false false true", "12 true true true false false"}) {
		t.Fatalf("deposits once the missing one is listed again: %v", got)
	}
	if got := x.xmrSettlements(order); len(got) != 1 || x.staffFlags(order)-flags != 1 || x.fake.countOf("transfer") != 1 {
		t.Fatalf("once the missing deposit is listed again: settlements %v, %d staff flags, transfer %d", got, x.staffFlags(order)-flags, x.fake.countOf("transfer"))
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM payments p WHERE p.order_id=$1 AND p.txid=$2 AND "+paymentFlagOpen("p"), order, second); n != 1 {
		t.Fatal("the deposit the release was built without has no open flag for staff")
	}
}

// PAY66-6: a refund is built without a credited deposit read as double-spent past the bounded wait; when the wallet
// lists that deposit again it is the buyer's, so it is left out (with a note saying it was credited and listed again,
// never "arrived after") and one stray refund returns it to the buyer's refund address. For a refund decided in a
// dispute and for a vendor's cancellation of a paid order. (A release keeps it for staff:
// TestXMREscrowDoubleSpentDepositBoundedWait.)
func TestXMREscrowRefundWithoutMissingDepositReturnsIt(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	thr := x.A.escrowXMR.confirmations
	half := int64(xmrListingPrice / 2)
	for _, c := range []struct {
		name   string
		refund func(order string)
	}{
		{"dispute decided for the buyer", func(order string) { x.disputeAndResolve(order, "refund") }},
		{"vendor cancels the paid order", func(order string) {
			vsess := x.sessOf(order, roleVendor)
			var cancel *pageForm
			for _, f := range x.orderForms(order, vsess) {
				if f.action == "/orders/cancel" {
					cancel = &f
				}
			}
			if cancel == nil {
				t.Fatal("no cancel form for the vendor")
			}
			if w := x.do("POST", cancel.action, vsess, cancel.values); w.Code != 303 || x.state(order) != stateCancelled {
				t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
			}
		}},
	} {
		order, addr := x.freshCheckout(x.physical(x.vendor))
		first, second := hex64("refund first "+order), hex64("refund second "+order)
		x.fake.payAddress(addr, first, half, thr+10)
		x.fake.payAddress(addr, second, xmrListingPrice-half, thr+2)
		x.mustPoll()
		if x.state(order) != statePaid {
			t.Fatalf("%s: funded order %s", c.name, x.state(order))
		}
		c.refund(order)
		x.fake.setTransfer(second, func(t *fakeXMRTransfer) { t.DoubleSpend = true })
		x.mustPoll()
		x.fake.mine(int(2 * thr))
		x.mustPoll()
		refundAddr := agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0]
		r := x.settlementOf(order, "refund")
		if r.State != "relayed" || r.InputTotal != half || !slices.Equal(x.outputsOf(r.ID), []string{"claimant:" + refundAddr + ":" + strconv.FormatInt(half-r.Fee, 10)}) {
			t.Fatalf("%s: the refund after the bounded wait: %+v %v; deposits %v", c.name, r, x.outputsOf(r.ID), x.creditedRows(order))
		}
		x.fake.setTransfer(second, func(t *fakeXMRTransfer) { t.DoubleSpend = false })
		for range 3 {
			x.mustPoll()
		}
		if got := x.creditedRows(order); !slices.Equal(got, []string{"20 true false false false true", "12 true true true true true"}) {
			t.Fatalf("%s: deposits once the missing one is listed again: %v", c.name, got)
		}
		stray := x.settlementOf(order, "stray_refund")
		if stray.State != "relayed" || stray.InputTotal != xmrListingPrice-half ||
			!slices.Equal(x.outputsOf(stray.ID), []string{"claimant:" + refundAddr + ":" + strconv.FormatInt(xmrListingPrice-half-stray.Fee, 10)}) || len(x.xmrSettlements(order)) != 2 {
			t.Fatalf("%s: stray refund %+v %v; settlements %v", c.name, stray, x.outputsOf(stray.ID), x.xmrSettlements(order))
		}
		ev := strings.Join(x.events(order), "\n")
		if !strings.Contains(ev, "1 deposit ("+amount(xmrListingPrice-half, 12)+" XMR, TESTNET fakechain) credited toward this order's funding, were listed again by the escrow wallet after its refund to the buyer was built without them") ||
			strings.Contains(ev, "arrived at this order's Monero escrow after") {
			t.Fatalf("%s: the order notes do not say why the deposit was left out:\n%s", c.name, ev)
		}
	}
}

// PAY66-8: a credited deposit listed again ends its missing episode, so going missing again restarts the bounded wait
// from the new height: 2 × threshold − 1 blocks after the second disappearance the release still waits, though the first
// disappearance was longer ago than that; one block later it is built from the other deposit.
func TestXMREscrowMissingDepositWaitRestartsWhenListed(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, second, id := x.fundedTwice()
	addr := agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
	thr := int(x.A.escrowXMR.confirmations)
	x.dropTransfer(second)
	x.mustPoll()
	x.fake.mine(thr + 5)
	x.fake.payAddress(addr, second, xmrListingPrice-xmrListingPrice/2, 3)
	x.mustPoll()
	if s := x.settlement(id); s.State != "requested" || x.waitClass(id) != xmrWaitConfirm {
		t.Fatalf("listed again at 3 confirmations: %+v %q; deposits %v", s, x.waitClass(id), x.creditedRows(order))
	}
	x.dropTransfer(second)
	x.mustPoll()
	x.fake.mine(2*thr - 1)
	x.mustPoll()
	if s := x.settlement(id); s.State != "requested" || x.waitClass(id) != xmrWaitMissing || x.fake.countOf("transfer") != 0 {
		t.Fatalf("%d blocks after it went missing again: %+v %q", 2*thr-1, s, x.waitClass(id))
	}
	x.fake.mine(1)
	x.mustPoll()
	if s := x.settlement(id); s.State != "relayed" || s.InputTotal != xmrListingPrice/2 {
		t.Fatalf("%d blocks after it went missing again: %+v; deposits %v", 2*thr, s, x.creditedRows(order))
	}
}
