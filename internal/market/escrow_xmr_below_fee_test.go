package market

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"testing"
)

// A-467 (A-463 confirmed on real monero-wallet-rpc 0.18.5.1, fakechain): a Monero refund whose whole balance is at or
// above the 0.001 XMR floor but at or below the network fee is refused by transfer (subtract_fee_from_outputs) with -17
// "not enough money" (0.001 and 0.0015 XMR) or -16 "tx not possible" (0.0018 XMR), never built, so checkXMRBuild never
// classes it too-small: the refund waited for good and staff were told of an "outputs mismatch" or a refusing wallet.
// On those codes the market now compares the refund's coins with the network fee the wallet would pay (the wallet's
// fee priority and transaction weight, the daemon's fee per byte) and, when what is left is below the floor, the refund
// is too few: it ends, its deposits are left out and the one stray refund returns them with a later deposit. DB
// integration tests against the simulated wallet-rpc (escrow_xmr_fake_test.go, which refuses as 0.18.5.1 did);
// TestXMREscrowRegtestRefundBelowFee is the real-chain leg.

// xmrFeeEstimate is the fee wallet2 computes from the fake's answers for a refund spending nIn outputs at priority p (1
// to 4): the weight of nIn inputs and two outputs times the fee per byte, rounded up to the quantization mask.
func xmrFeeEstimate(nIn, p int) int64 {
	return (fakeXMRWeight(nIn, 2)*fakeXMRFeePerByte[p-1] + fakeXMRMask - 1) / fakeXMRMask * fakeXMRMask
}

// refundOf is a physical Monero escrow order paid with amount piconero (its price) at 10 confirmations whose
// dispute is decided for the buyer, so its refund is requested; the order and its deposit.
func (x *xmrEscrowEnv) refundOf(amount int64) (order, addr, txid string) {
	x.t.Helper()
	p := x.physical(x.vendor)
	agExec(x.testEnv, "UPDATE products SET xmr=$2 WHERE id=$1", p, amount)
	order, addr = x.freshCheckout(p)
	txid = hex64("below fee " + order)
	x.fake.payAddress(addr, txid, amount, 10)
	x.mustPoll()
	if x.state(order) != statePaid {
		x.t.Fatalf("funded with %d: %s", amount, x.state(order))
	}
	x.disputeAndResolve(order, "refund")
	return order, addr, txid
}

// noFalseClass checks that order's notes and staff notices never said its coins mismatched or the wallet refused.
func (x *xmrEscrowEnv) noFalseClass(what, order string) {
	x.t.Helper()
	for _, text := range []string{xmrSettleWaitText(xmrOutputsMismatch, "refund", 10), "refused to build", "keeps refusing to build"} {
		if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, text) +
			agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE strpos(body,$1)>0 AND strpos(body,$2)>0", shortID(order), text); n != 0 {
			x.t.Fatalf("%s: %d notes or notices say %q; events %q", what, n, text, x.events(order))
		}
	}
}

// TestXMREscrowRefundBelowFee: at 0.001 and 0.0018 XMR (decided for the buyer) and 0.0015 XMR (cancelled at the end of
// its payment window, underpaid), each refund the wallet refuses (-17, -17, -16) is too few: failed with the too-small
// class, its deposit left out and flagged with the one too-few note, the stray refund requested and waiting too-small
// (it is refused the same way), and no note or notice of an outputs mismatch or a refusing wallet. A later deposit lets
// that one stray refund return both.
func TestXMREscrowRefundBelowFee(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	for _, band := range []struct {
		amount  int64
		decided bool
		code    int
	}{{1000000000, true, -17}, {1500000000, false, -17}, {1800000000, true, -16}, {fakeXMRFee(1, 2), true, -46}} {
		what := amount(band.amount, 12) + " XMR"
		var order, addr, txid string
		if band.decided {
			order, addr, txid = x.refundOf(band.amount)
		} else {
			order, addr = x.freshCheckout(x.digital(x.vendor))
			txid = hex64("below fee underpaid " + order)
			x.fake.payAddress(addr, txid, band.amount, 20)
			x.mustPoll()
			agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", order)
			agExec(x.testEnv, "UPDATE payments SET created=now()-interval '3 days' WHERE order_id=$1", order)
			x.mustPoll()
			if x.state(order) != stateCancelled {
				t.Fatalf("%s: underpaid: %s", what, x.state(order))
			}
		}
		calls := x.fake.countOf("transfer")
		x.mustPoll()
		r := x.settlementOf(order, "refund")
		if r.State != "failed" || x.waitClass(r.ID) != xmrTooSmall || x.fake.countOf("transfer") != calls+1 {
			t.Fatalf("%s: the refund %+v wait %q (transfer calls %d, before %d); events %q", what, r, x.waitClass(r.ID), x.fake.countOf("transfer"), calls, x.events(order))
		}
		// The wallet's own answer, as 0.18.5.1 gave it on the fakechain.
		if _, err := x.A.escrowXMR.rpc.buildTransfer(t.Context(), agInt64(t, x.testEnv, "SELECT sl.account_index FROM escrows es JOIN xmr_escrow_slots sl ON sl.id=es.xmr_slot_id WHERE es.order_id=$1", order),
			[]xmrDestination{{testXMRAddr("probe", true), band.amount}}); !xmrRefusedWith(err, band.code) {
			t.Fatalf("%s: the wallet answers %v, want %d", what, err, band.code)
		}
		if got := agStrings(t, x.testEnv, "SELECT amount||' '||flagged||' '||escrow_left_out||' '||after_close||' '||COALESCE(xmr_settlement_id,0) FROM payments WHERE order_id=$1 ORDER BY id", order); !slices.Equal(got, []string{strconv.FormatInt(band.amount, 10) + " true true false 0"}) {
			t.Fatalf("%s: deposits %v", what, got)
		}
		note := xmrTooFewNote(1, band.amount, band.decided, "deposit "+truncate(txid, 20)+" ("+amount(band.amount, 12)+" XMR)")
		if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2", order, note); n != 1 {
			t.Fatalf("%s: %d too-few notes; events %q", what, n, x.events(order))
		}
		x.mustPoll()
		x.mustPoll()
		stray := x.settlementOf(order, "stray_refund")
		if stray.State != "requested" || x.waitClass(stray.ID) != xmrTooSmall {
			t.Fatalf("%s: the stray refund %+v wait %q; events %q", what, stray, x.waitClass(stray.ID), x.events(order))
		}
		x.noFalseClass(what, order)
		later := hex64("below fee later " + order)
		x.fake.payAddress(addr, later, 3*minPayoutXMR, 20)
		x.mustPoll()
		x.mustPoll()
		s := x.settlement(stray.ID)
		if want := []string{"claimant:" + agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0] + ":" + strconv.FormatInt(band.amount+3*minPayoutXMR-s.Fee, 10)}; s.State != "relayed" || s.InputTotal != band.amount+3*minPayoutXMR || len(s.KeyImages) != 2 || !slices.Equal(x.outputsOf(s.ID), want) {
			t.Fatalf("%s: the stray refund %+v (wait %q) outputs %v, want %v", what, s, x.waitClass(s.ID), x.outputsOf(s.ID), want)
		}
		if got := x.xmrSettlements(order); len(got) != 2 {
			t.Fatalf("%s: settlements %v", what, got)
		}
		x.noFalseClass(what, order)
	}
}

// xmrRefusedWith reports whether err is the escrow wallet's refusal with code.
func xmrRefusedWith(err error, code int) bool {
	var re *xmrRPCError
	return errors.As(err, &re) && re.Service == "wallet" && re.Code == int64(code)
}

func agInt64(t *testing.T, e *testEnv, q string, args ...any) int64 {
	t.Helper()
	v, err := strconv.ParseInt(agStrings(t, e, "SELECT ("+q+")::text", args...)[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// refuseTransfer makes the fake's transfer answer code for every build until the returned function restores it.
func (x *xmrEscrowEnv) refuseTransfer(code int64, msg string) func() {
	x.fake.handle("transfer", func(*fakeXMREscrow, json.RawMessage) (any, int64, string) { return nil, code, msg })
	return func() { x.fake.handle("transfer", fakeXMRWalletMethods["transfer"]) }
}

// A-467, the other side: -17 and -16 also mean other things (coins the wallet no longer sees as unlocked or confirmed,
// a wallet behind the chain), so a refund whose coins can pay the fee and the floor keeps the wallet's classing: a -17
// for a deposit the wallet reads as unconfirmed again (a reorganisation since incoming_transfers) waits once and is then
// an outputs mismatch; a -16 is a refusing wallet. Nothing is left out, and once the wallet builds it the refund is
// relayed. At the band's edge the claimant's output after the estimated fee exactly at the floor is not too few (the
// wallet's checked build accepts it); one piconero less is.
func TestXMREscrowRefundRefusedNotBelowFee(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	left := func(order string) int {
		return agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out", order)
	}

	// -17 for coins the wallet reads as unconfirmed again, while incoming_transfers still listed them unlocked.
	order, _, txid := x.refundOf(xmrListingPrice)
	x.fake.hook("transfer", func() { x.fake.setTransfer(txid, func(t *fakeXMRTransfer) { t.Pool = true }) })
	x.mustPoll()
	r := x.settlementOf(order, "refund")
	if r.State != "requested" || x.waitClass(r.ID) != xmrWaitOutputs || left(order) != 0 {
		t.Fatalf("unconfirmed again: the refund %+v wait %q, %d left out", r, x.waitClass(r.ID), left(order))
	}
	x.fake.setTransfer(txid, func(t *fakeXMRTransfer) { t.Pool = false })
	x.mustPoll()
	if r = x.settlementOf(order, "refund"); r.State != "requested" || x.waitClass(r.ID) != xmrOutputsMismatch || left(order) != 0 {
		t.Fatalf("unconfirmed again, a second pass: the refund %+v wait %q, %d left out", r, x.waitClass(r.ID), left(order))
	}
	x.fake.hook("transfer", nil)
	x.fake.setTransfer(txid, func(t *fakeXMRTransfer) { t.Pool = false })
	x.mustPoll()
	if r = x.settlementOf(order, "refund"); r.State != "relayed" {
		t.Fatalf("once the wallet builds it: the refund %+v wait %q", r, x.waitClass(r.ID))
	}

	// -16 for another reason: a refusing wallet.
	order, _, _ = x.refundOf(xmrListingPrice)
	restore := x.refuseTransfer(-16, "tx not possible")
	x.mustPoll()
	r = x.settlementOf(order, "refund")
	if r.State != "requested" || x.waitClass(r.ID) != xmrWalletRefusing+" rpc:-16" || left(order) != 0 {
		t.Fatalf("-16: the refund %+v wait %q, %d left out", r, x.waitClass(r.ID), left(order))
	}
	restore()
	x.mustPoll()
	if r = x.settlementOf(order, "refund"); r.State != "relayed" {
		t.Fatalf("-16, then built: the refund %+v wait %q", r, x.waitClass(r.ID))
	}

	// The band's edge: the claimant's output after the estimated fee at the floor, and one piconero below it. The fake
	// would build both, so it refuses them here (-17) to put the comparison itself to the test.
	edge := xmrFeeEstimate(1, 1) + minPayoutXMR
	order, _, _ = x.refundOf(edge)
	restore = x.refuseTransfer(-17, "not enough money")
	x.mustPoll()
	if r = x.settlementOf(order, "refund"); r.State != "requested" || x.waitClass(r.ID) != xmrWaitOutputs || left(order) != 0 {
		t.Fatalf("at the edge (%d): the refund %+v wait %q, %d left out", edge, r, x.waitClass(r.ID), left(order))
	}
	restore()
	order, _, _ = x.refundOf(edge - 1)
	restore = x.refuseTransfer(-17, "not enough money")
	x.mustPoll()
	if r = x.settlementOf(order, "refund"); r.State != "failed" || x.waitClass(r.ID) != xmrTooSmall || left(order) != 1 {
		t.Fatalf("below the edge (%d): the refund %+v wait %q, %d left out", edge-1, r, x.waitClass(r.ID), left(order))
	}
	restore()

	// Only the codes of coins below the fee are compared: a 0.001 XMR refund whose refund address the wallet refuses
	// (-2) is a refused destination, not too few.
	order, _, _ = x.refundOf(minPayoutXMR)
	x.fake.edit(func(f *fakeXMREscrow) {
		f.invalid[agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0]] = true
	})
	x.mustPoll()
	if r = x.settlementOf(order, "refund"); r.State != "requested" || x.waitClass(r.ID) != xmrDestRefused+"-claimant" || left(order) != 0 {
		t.Fatalf("a refused refund address: the refund %+v wait %q, %d left out", r, x.waitClass(r.ID), left(order))
	}

	// The fee is the one of the inputs the refund spends: two deposits of 0.0015 XMR are enough for one input's fee and
	// the floor, but not for two inputs' (refused here, -17, as above).
	two := int64(3000000000)
	if xmrFeeEstimate(1, 1)+minPayoutXMR > two || xmrFeeEstimate(2, 1)+minPayoutXMR <= two {
		t.Fatalf("fees %d and %d do not put %d between them", xmrFeeEstimate(1, 1), xmrFeeEstimate(2, 1), two)
	}
	p := x.physical(x.vendor)
	agExec(x.testEnv, "UPDATE products SET xmr=$2 WHERE id=$1", p, two)
	order, addr := x.freshCheckout(p)
	x.fake.payAddress(addr, hex64("two inputs a "+order), two/2, 10)
	x.fake.payAddress(addr, hex64("two inputs b "+order), two/2, 10)
	x.mustPoll()
	if x.state(order) != statePaid {
		t.Fatalf("two deposits: %s", x.state(order))
	}
	x.disputeAndResolve(order, "refund")
	restore = x.refuseTransfer(-17, "not enough money")
	x.mustPoll()
	if r = x.settlementOf(order, "refund"); r.State != "failed" || x.waitClass(r.ID) != xmrTooSmall || left(order) != 2 {
		t.Fatalf("two inputs: the refund %+v wait %q, %d left out", r, x.waitClass(r.ID), left(order))
	}
	restore()
}

// A-467: the fee compared is the one the wallet pays: at its default priority (get_default_fee_priority; here 2, as
// under a pool backlog, about four times priority 1's fee), and at priority 1 when that call fails as 0.18.5.1 fails it once
// adjust_priority falls back to 0 (transfer then pays priority 1's fee). A daemon whose fee estimate fails records
// nothing: the refund is tried again on the next pass.
func TestXMREscrowRefundBelowFeePriority(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	// 0.005 XMR: enough at priority 1, too few at priority 2.
	const amt = 5 * minPayoutXMR
	if xmrFeeEstimate(1, 1)+minPayoutXMR > amt || xmrFeeEstimate(1, 2)+minPayoutXMR <= amt {
		t.Fatalf("fees %d and %d do not put %d between them", xmrFeeEstimate(1, 1), xmrFeeEstimate(1, 2), int64(amt))
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.priority = 2 })
	order, _, _ := x.refundOf(amt)
	x.mustPoll()
	if r := x.settlementOf(order, "refund"); r.State != "failed" || x.waitClass(r.ID) != xmrTooSmall {
		t.Fatalf("at priority 2: the refund %+v wait %q", r, x.waitClass(r.ID))
	}

	// The daemon's estimate fails: the refund waits for it, never too few on a guess (QA5W1-4,
	// TestXMREscrowRefundFeeEstimateUnavailable); then it answers, and at priority 1 (the wallet's -1) the 0.0015 XMR
	// refund is too few.
	x.fake.edit(func(f *fakeXMREscrow) { f.priority, f.priorityErr = 0, true })
	order, _, _ = x.refundOf(1500000000)
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = true })
	x.mustPoll()
	if r := x.settlementOf(order, "refund"); r.State != "requested" || x.waitClass(r.ID) != xmrFeeEstimateWaiting {
		t.Fatalf("a failing fee estimate: the refund %+v wait %q", r, x.waitClass(r.ID))
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = false })
	x.mustPoll()
	if r := x.settlementOf(order, "refund"); r.State != "failed" || x.waitClass(r.ID) != xmrTooSmall {
		t.Fatalf("at priority 1: the refund %+v wait %q", r, x.waitClass(r.ID))
	}
}

// A-467 keeps releases as they were: the coins are the vendor's, and the comparison is for refunds only. A release of
// 0.0015 XMR the wallet refuses (-17) waits for the outputs and is then an outputs mismatch, never too few.
func TestXMREscrowReleaseBelowFeeUnchanged(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	p := x.digital(x.vendor)
	agExec(x.testEnv, "UPDATE products SET xmr=1500000000 WHERE id=$1", p)
	order, addr := x.freshCheckout(p)
	x.fake.payAddress(addr, hex64("release below fee "+order), 1500000000, 10)
	x.mustPoll()
	if x.state(order) != stateCompleted {
		t.Fatalf("funded: %s", x.state(order))
	}
	id, _ := x.release(order)
	x.mustPoll()
	if got := x.waitClass(id); got != xmrWaitOutputs {
		t.Fatalf("the release waits with %q, want %q", got, xmrWaitOutputs)
	}
	x.mustPoll()
	if got, s := x.waitClass(id), x.settlement(id); got != xmrOutputsMismatch || s.State != "requested" ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out", order) != 0 {
		t.Fatalf("the release %+v waits with %q, want %q", s, got, xmrOutputsMismatch)
	}
}

// A-467: estimateFee reads the wallet's priority and weight and the daemon's fee per byte and mask, and computes
// wallet2's fee from them; any reply out of shape is a bad reply, never a fee. Unit test against the simulated nodes.
func TestXMREscrowEstimateFee(t *testing.T) {
	f := newFakeXMREscrow(t)
	r, err := newXMREscrowRPC(f.wallet.URL, f.daemon.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err = r.createWallet(ctx, "escrow-202610-01", "pw"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		nIn, priority int
		priorityErr   bool
		want          int64
	}{{1, 1, false, 1843200000}, {2, 1, false, 2658000000}, {1, 2, false, 7219200000}, {1, 3, true, 1843200000}} { // recorded weights 1536 and 2215 (QA5W1-5)
		f.edit(func(f *fakeXMREscrow) { f.priority, f.priorityErr = c.priority, c.priorityErr })
		if got, err := r.estimateFee(ctx, c.nIn, 2); err != nil || got != c.want || got != xmrFeeEstimate(c.nIn, map[bool]int{false: c.priority, true: 1}[c.priorityErr]) {
			t.Errorf("%d inputs at priority %d (error %v): %d %v, want %d", c.nIn, c.priority, c.priorityErr, got, err, c.want)
		}
	}
	f.edit(func(f *fakeXMREscrow) { f.priority, f.priorityErr = 0, false })
	if got := xmrFeeFromWeight(1563, 1200000, 10000); got != 1875600000 {
		t.Errorf("rounding: %d", got)
	}
	if got := xmrFeeFromWeight(1, 3, 10000); got != 10000 {
		t.Errorf("rounding up to the mask: %d", got)
	}
	f.edit(func(f *fakeXMREscrow) { f.feeErr = true })
	if _, err = r.estimateFee(ctx, 1, 2); xmrErrorClass(err) != "get_fee_estimate rpc:-5" {
		t.Errorf("a failing daemon: %v", err)
	}
	f.edit(func(f *fakeXMREscrow) { f.feeErr = false })
	for name, h := range map[string]func(*fakeXMREscrow, json.RawMessage) (any, int64, string){
		"get_default_fee_priority": func(*fakeXMREscrow, json.RawMessage) (any, int64, string) {
			return map[string]any{"priority": 5}, 0, ""
		},
		"estimate_tx_size_and_weight": func(*fakeXMREscrow, json.RawMessage) (any, int64, string) {
			return map[string]any{"size": 1, "weight": 0}, 0, ""
		},
	} {
		f.handle(name, h)
		if _, err = r.estimateFee(ctx, 1, 2); xmrErrorClass(err) != name+" bad-reply" {
			t.Errorf("%s out of shape: %v", name, err)
		}
		f.handle(name, fakeXMRWalletMethods[name])
	}
	f.handle("get_default_fee_priority", func(*fakeXMREscrow, json.RawMessage) (any, int64, string) { return nil, -13, "No wallet file" })
	if _, err = r.estimateFee(ctx, 1, 2); !xmrNotOpen(err) {
		t.Errorf("no wallet open: %v", err)
	}
	f.handle("get_default_fee_priority", fakeXMRWalletMethods["get_default_fee_priority"])
	if _, err = r.estimateFee(ctx, 0, 2); err != errXMRDestination {
		t.Errorf("no input: %v", err)
	}
	// The fake's builds take the measured weights too (QA5W2-4): 1514 bytes for one input and two outputs, 666 more per
	// input, at 1.2e6 piconero per byte, each below the estimate above, which is wallet2's (1536, 679 more per input).
	r.net.Store("fakechain") // as startEscrowXMR leaves it once the daemon is verified (A-449)
	for nIn, want := range map[int]int64{1: 1816800000, 2: 2616000000, 3: 3415200000} {
		account, addr, err := r.createAccount(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := range nIn {
			f.payAddress(addr, hex64("weight "+strconv.Itoa(nIn)+" "+strconv.Itoa(i)), 1_000_000_000_000, 12)
		}
		tx, err := r.buildTransfer(ctx, account, []xmrDestination{{testXMRAddr("weight claimant", true), int64(nIn) * 1_000_000_000_000}})
		if err != nil || tx.Fee != want || len(tx.SpentKeyImages) != nIn || tx.Fee > xmrFeeEstimate(nIn, 1) {
			t.Errorf("%d input(s): fee %d from %d inputs (%v), want %d, at most the estimate %d", nIn, tx.Fee, len(tx.SpentKeyImages), err, want, xmrFeeEstimate(nIn, 1))
		}
	}
}

// A-467 on upgrade: a refund below the fee that the market before A-467 left flagged as an outputs mismatch (its deposit
// flagged by that note) ends too few on the next pass, its deposit left out with the one too-few note.
func TestXMREscrowRefundBelowFeeUpgrade(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, txid := x.refundOf(1000000000)
	r := x.settlementOf(order, "refund")
	agExec(x.testEnv, "UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", r.ID, xmrOutputsMismatch)
	agExec(x.testEnv, "UPDATE payments SET flagged=true WHERE order_id=$1", order)
	x.mustPoll()
	note := xmrTooFewNote(1, 1000000000, true, "deposit "+truncate(txid, 20)+" ("+amount(1000000000, 12)+" XMR)")
	if r = x.settlement(r.ID); r.State != "failed" || x.waitClass(r.ID) != xmrTooSmall ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out AND flagged", order) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2", order, note) != 1 {
		t.Fatalf("after the upgrade: the refund %+v wait %q; events %q", r, x.waitClass(r.ID), x.events(order))
	}
}
