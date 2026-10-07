package market

import (
	"strings"
	"testing"
)

// QA5W1-4 (A-467 follow-up): a fee estimate that fails for a refund the wallet refused is recorded on that settlement
// alone, as a wait (fee-estimate-waiting), and flagged to staff once when it persists (fee-estimate-unavailable). The
// wallet's pass goes on: before, the error went up the whole pass every time, so the wallet counted as unread, its unpaid
// orders never expired and nothing told anyone. The refund is never classed too few on a guess, and once the estimate
// answers it ends too few as A-467 does.
func TestXMREscrowRefundFeeEstimateUnavailable(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.refundOf(1500000000)
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = true })
	unpaid := x.draft(x.digital(x.vendor))
	x.mustCheckout(unpaid)
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", unpaid)
	flags := x.staffFlags(order)
	left := func() int {
		return agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out", order)
	}
	notes := func() int {
		return agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, xmrSettleWaitText(xmrFeeEstimateUnavailable, "refund", 10))
	}
	for i, class := range []string{xmrFeeEstimateWaiting, xmrFeeEstimateUnavailable, xmrFeeEstimateUnavailable} {
		if err := x.poll(); err != nil {
			t.Fatalf("pass %d: the wallet's pass failed: %v", i, err)
		}
		r := x.settlementOf(order, "refund")
		if r.State != "requested" || x.waitClass(r.ID) != class || left() != 0 || notes() != min(i, 1) {
			t.Fatalf("pass %d: the refund %+v wait %q (want %q), %d left out, %d staff notes; events %q", i, r, x.waitClass(r.ID), class, left(), notes(), x.events(order))
		}
		// Told once, on the second pass: no notice before it, none again after it.
		switch got := x.staffFlags(order); {
		case i == 1 && got <= flags, i != 1 && got != flags:
			t.Fatalf("pass %d: %d staff notices, %d before", i, got, flags)
		default:
			flags = got
		}
		if got := x.state(unpaid); got != stateCancelled {
			t.Fatalf("pass %d: the unpaid order in the same wallet is %s past its payment window", i, got)
		}
	}
	for _, class := range []string{xmrFeeEstimateWaiting, xmrFeeEstimateUnavailable} {
		if xmrSettleWaitText(class, "refund", 10) == "" {
			t.Errorf("%s has no text for the order page", class)
		}
	}
	if !xmrWaitFlagged(xmrFeeEstimateUnavailable) || xmrWaitFlagged(xmrFeeEstimateWaiting) {
		t.Errorf("flagged: waiting %v, unavailable %v", xmrWaitFlagged(xmrFeeEstimateWaiting), xmrWaitFlagged(xmrFeeEstimateUnavailable))
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = false })
	x.mustPoll()
	if r := x.settlementOf(order, "refund"); r.State != "failed" || x.waitClass(r.ID) != xmrTooSmall || left() != 1 {
		t.Fatalf("once the estimate answers: the refund %+v wait %q, %d left out", r, x.waitClass(r.ID), left())
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, xmrSettleWaitText(xmrOutputsMismatch, "refund", 10)); n != 0 {
		t.Fatalf("%d outputs-mismatch notes; events %q", n, x.events(order))
	}
}

// QA5W1-6: a stray refund waiting too few that the wallet keeps refusing logs the refusal once, not on every pass, and
// asks for no fee estimate again while its inputs are unchanged (transfer is still asked each pass, so a later deposit
// that lets it build is seen). A later deposit that changes its inputs but leaves it too few is estimated once more.
func TestXMREscrowStrayBelowFeeQuiet(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	logs := captureLog(t)
	order, addr, _ := x.refundOf(minPayoutXMR)
	x.mustPoll()
	x.mustPoll()
	x.mustPoll()
	stray := x.settlementOf(order, "stray_refund")
	if stray.State != "requested" || x.waitClass(stray.ID) != xmrTooSmall {
		t.Fatalf("the stray refund %+v wait %q", stray, x.waitClass(stray.ID))
	}
	refused := func() int {
		return strings.Count(logs.String(), "escrow: Monero escrow settlement of order "+shortID(order)+": the wallet refused to build it")
	}
	lines, estimates, transfers := refused(), x.fake.countOf("get_fee_estimate"), x.fake.countOf("transfer")
	if lines != 2 {
		t.Fatalf("%d refusal lines for the refund and the stray refund, want 2:\n%s", lines, logs.String())
	}
	for i := 0; i < 3; i++ {
		x.mustPoll()
	}
	if got := x.fake.countOf("transfer"); got != transfers+3 {
		t.Fatalf("transfer asked %d times in 3 passes, want 3", got-transfers)
	}
	if got, e := refused(), x.fake.countOf("get_fee_estimate"); got != lines || e != estimates {
		t.Fatalf("3 more passes: %d more refusal lines, %d more fee estimates, want none:\n%s", got-lines, e-estimates, logs.String())
	}
	// A second deposit too small to pay the fee with the first: new inputs, one estimate, still too few, still quiet.
	x.fake.payAddress(addr, hex64("quiet stray second "+order), minPayoutXMR/2, 20)
	x.mustPoll()
	x.mustPoll()
	x.mustPoll()
	if s := x.settlement(stray.ID); s.State != "requested" || x.waitClass(s.ID) != xmrTooSmall {
		t.Fatalf("a second small deposit: the stray refund %+v wait %q", s, x.waitClass(s.ID))
	}
	if got, e := refused(), x.fake.countOf("get_fee_estimate"); got != lines || e != estimates+1 {
		t.Fatalf("a second small deposit: %d more refusal lines (want 0), %d more fee estimates (want 1)", got-lines, e-estimates)
	}
}
