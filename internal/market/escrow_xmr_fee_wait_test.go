package market

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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

// flagNotes counts the staff notes xmrSettleWaits wrote for settlement id (one per flag event).
func (x *xmrEscrowEnv) flagNotes(order string, id int64) int {
	return agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0 AND strpos(note,'Moderator review required')>0",
		order, "Monero escrow settlement "+strconv.FormatInt(id, 10)+" (")
}

// staffUsers counts the active staff a flag notifies.
func (x *xmrEscrowEnv) staffUsers() int {
	return agInt(x.testEnv, "SELECT count(*) FROM users WHERE role IN ('moderator','admin') AND suspended_at IS NULL")
}

// QA5W2-1: a refund the wallet refuses for a reason that is not the fee (-16 tx not possible, -17 not enough money),
// whose fee estimate fails on every other pass, is flagged on its second pass in a row without a build, whichever class
// that pass records (the refusal, or the unavailable estimate), and staff are told once for the episode, however often
// the class flips between those already flagged. Before, each estimate failure restarted the refusal's count and each
// refusal restarted the estimate's, so it waited unflagged for good. With the estimate always answering, the refusal
// keeps its own count (the control).
func TestXMREscrowRefundFeeEstimateIntermittent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int64
		msg     string
		flicker bool
		want    []string
	}{
		{"-16 estimate alternating", -16, "tx not possible", true, []string{xmrFeeEstimateWaiting, xmrWalletRefused + " rpc:-16",
			xmrFeeEstimateUnavailable, xmrWalletRefused + " rpc:-16", xmrFeeEstimateUnavailable, xmrWalletRefused + " rpc:-16"}},
		{"-17 estimate alternating", -17, "not enough money", true, []string{xmrFeeEstimateWaiting, xmrOutputsMismatch,
			xmrFeeEstimateUnavailable, xmrOutputsMismatch, xmrFeeEstimateUnavailable, xmrOutputsMismatch}},
		{"-16 estimate answering", -16, "tx not possible", false, []string{xmrWalletRefusing + " rpc:-16", xmrWalletRefused + " rpc:-16",
			xmrWalletRefused + " rpc:-16", xmrWalletRefused + " rpc:-16"}},
		{"-17 estimate answering", -17, "not enough money", false, []string{xmrWaitOutputs, xmrOutputsMismatch, xmrOutputsMismatch}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			order, _, _ := x.refundOf(xmrListingPrice)
			defer x.refuseTransfer(tc.code, tc.msg)()
			flags0 := x.staffFlags(order)
			var got []string
			for i := range tc.want {
				fail := tc.flicker && i%2 == 0
				x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = fail })
				if err := x.poll(); err != nil {
					t.Fatalf("pass %d: %v", i, err)
				}
				r := x.settlementOf(order, "refund")
				got = append(got, x.waitClass(r.ID))
				if r.State != "requested" {
					t.Fatalf("pass %d: the refund %+v", i, r)
				}
				// Not flagged on the first pass; flagged from the second, once.
				flagged, notes, notices := xmrWaitFlagged(x.waitClass(r.ID)), x.flagNotes(order, r.ID), x.staffFlags(order)-flags0
				if want := min(i, 1); flagged != (i > 0) || notes != want || notices != want*x.staffUsers() {
					t.Fatalf("pass %d: classes %q, flagged %v, %d staff notes, %d staff notices (%d staff); events %q", i, got, flagged, notes, notices, x.staffUsers(), x.events(order))
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("classes %q, want %q", got, tc.want)
			}
		})
	}
}

// QA5W2 (NIT): a stray refund already waiting too small (flagged) is not flagged again because a restart lost the
// in-memory record of its refusal while the fee estimate is down: it records the unavailable estimate, then too small
// again once the estimate answers, and staff are told nothing new. The refund that ended too small is untouched.
func TestXMREscrowStrayRestartEstimateDown(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.refundOf(minPayoutXMR)
	x.mustPoll()
	x.mustPoll()
	x.mustPoll()
	refund, stray := x.settlementOf(order, "refund"), x.settlementOf(order, "stray_refund")
	if refund.State != "failed" || x.waitClass(refund.ID) != xmrTooSmall || stray.State != "requested" || x.waitClass(stray.ID) != xmrTooSmall {
		t.Fatalf("the refund %+v %q, the stray refund %+v %q", refund, x.waitClass(refund.ID), stray, x.waitClass(stray.ID))
	}
	flags0, notes0, events0 := x.staffFlags(order), x.flagNotes(order, stray.ID), len(x.events(order))
	x.restart()
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = true })
	var seq []string
	for i := 0; i < 3; i++ {
		x.mustPoll()
		seq = append(seq, x.waitClass(stray.ID))
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = false })
	for i := 0; i < 2; i++ {
		x.mustPoll()
		seq = append(seq, x.waitClass(stray.ID))
	}
	want := []string{xmrFeeEstimateUnavailable, xmrFeeEstimateUnavailable, xmrFeeEstimateUnavailable, xmrTooSmall, xmrTooSmall}
	if !slices.Equal(seq, want) || x.staffFlags(order) != flags0 || x.flagNotes(order, stray.ID) != notes0 || len(x.events(order)) != events0 {
		t.Fatalf("classes %q (want %q); staff notices +%d, staff notes +%d, order events +%d: %q", seq, want, x.staffFlags(order)-flags0,
			x.flagNotes(order, stray.ID)-notes0, len(x.events(order))-events0, x.events(order))
	}
	if r := x.settlement(refund.ID); r.State != "failed" || x.waitClass(refund.ID) != xmrTooSmall {
		t.Fatalf("the ended refund: %+v %q", r, x.waitClass(refund.ID))
	}
}

// QA5W2-2: the fee estimate's -13 (the wallet-rpc lost its open wallet; the next pass opens it again) and a cancelled
// pass are returned, as xmrBuildRefused returns them, and recorded on nothing: the refund's wait is left as it was, not
// counted as a failed estimate.
func TestXMREscrowRefundFeeEstimateNotOpen(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.refundOf(1500000000)
	r := x.settlementOf(order, "refund")
	x.fake.fault("estimate_tx_size_and_weight", fakeXMRNotOpen)
	err := x.poll()
	var re *xmrRPCError
	if !errors.As(err, &re) || re.Code != -13 || x.waitClass(r.ID) != "" || x.settlement(r.ID).State != "requested" {
		t.Fatalf("-13 from the estimate: pass %v, the refund %+v wait %q", err, x.settlement(r.ID), x.waitClass(r.ID))
	}
	// Once waiting on a failed estimate, a -13 does not count as its second failure.
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = true })
	x.mustPoll()
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = false })
	flags, events := x.staffFlags(order), len(x.events(order))
	x.fake.fault("get_default_fee_priority", fakeXMRNotOpen)
	if err = x.poll(); !errors.As(err, &re) || re.Code != -13 || x.waitClass(r.ID) != xmrFeeEstimateWaiting || x.staffFlags(order) != flags || len(x.events(order)) != events {
		t.Fatalf("-13 while waiting: pass %v, wait %q, staff notices +%d, events +%d", err, x.waitClass(r.ID), x.staffFlags(order)-flags, len(x.events(order))-events)
	}
	// A pass cancelled during the estimate returns the cancellation and records nothing, nor logs a failed estimate.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := captureLog(t)
	x.fake.hook("estimate_tx_size_and_weight", cancel)
	err = x.A.xmrEscrowPollOnce(ctx)
	x.fake.hook("estimate_tx_size_and_weight", nil)
	if !errors.Is(err, context.Canceled) || x.waitClass(r.ID) != xmrFeeEstimateWaiting || x.staffFlags(order) != flags || len(x.events(order)) != events ||
		strings.Contains(logs.String(), "the network fee estimate failed") {
		t.Fatalf("cancelled: pass %v, wait %q, staff notices +%d, events +%d; log:\n%s", err, x.waitClass(r.ID), x.staffFlags(order)-flags, len(x.events(order))-events, logs.String())
	}
	// The estimate answering on the next pass: too few, as A-467 ends it.
	x.mustPoll()
	if s := x.settlement(r.ID); s.State != "failed" || x.waitClass(r.ID) != xmrTooSmall {
		t.Fatalf("the estimate answering after: the refund %+v wait %q", s, x.waitClass(r.ID))
	}
}

// QA5W1-4 / QA5W2 (chair addendum): a recorded fee-estimate wait lets its wallet's pass go on, retention included
// (xmrWalletPass runs xmrDestroyIfDue only when the settlements returned no error). October's wallet, retained and past
// its destroy_after on the pass after the order's refund was decided, holds that refund while the estimate fails: the
// administrators are told once that it is kept (a live settlement keeps it, xmrDestroyableSQL; destruction never waits
// on another wallet's settlements, which are passed separately), the refund waits, then is flagged unavailable, and is
// otherwise untouched. Once the refund is built and 100 confirmations deep, the wallet is destroyed (its password
// changed, as TestXMREscrowDestroyPreconditions checks).
func TestXMREscrowFeeWaitRetentionRuns(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	p := x.physical(x.vendor)
	agExec(x.testEnv, "UPDATE products SET xmr=$2 WHERE id=$1", p, xmrListingPrice)
	order, addr := x.freshCheckout(p)
	x.fake.payAddress(addr, hex64("retained fee wait "+order), xmrListingPrice, 10)
	x.mustPoll()
	name := x.walletOf(order)
	x.rotate()
	if x.state(order) != statePaid || x.walletState(name) != "closing" {
		t.Fatalf("order %s, wallet %s", x.state(order), x.walletState(name))
	}
	x.disputeAndResolve(order, "refund")
	at := agStrings(t, x.testEnv, "SELECT to_char(max(created) AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') FROM order_events WHERE order_id=$1 AND from_state='disputed' AND to_state='resolved'", order)[0]
	closed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		t.Fatalf("close of %s: %q", order, at)
	}
	x.now = closed.Add(91 * 24 * time.Hour)
	restore := x.refuseTransfer(-16, "tx not possible")
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = true })
	const held = "is past its destruction date (90 days after its last order closed) but the market keeps it"
	derived := xmrWalletPassword(testXMRSecret, name)
	refund := x.settlementOf(order, "refund")
	for i, class := range []string{xmrFeeEstimateWaiting, xmrFeeEstimateUnavailable} {
		err := x.poll()
		file, _ := x.fake.file(name)
		if x.notified(x.admin, held) != 1 || x.walletState(name) != "retained" || file.password != derived {
			t.Fatalf("pass %d: %d held notices, wallet %s, password changed %v", i, x.notified(x.admin, held), x.walletState(name), file.password != derived)
		}
		if r := x.settlement(refund.ID); r.State != "requested" || x.waitClass(r.ID) != class || r.TxHash != "" {
			t.Fatalf("pass %d: the refund %+v wait %q, want %q", i, r, x.waitClass(r.ID), class)
		}
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if n := x.notified(x.admin, "account "); n != 1 {
		t.Fatalf("%d held notices name the account holding the refund's coins", n)
	}
	restore()
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = false })
	changes := x.fake.countOf("change_wallet_password")
	x.mustPoll()
	x.fake.mine(10)
	x.mustPoll()
	if r := x.settlement(refund.ID); r.State != "confirmed" || x.walletState(name) != "retained" {
		t.Fatalf("the refund once the wallet builds it: %+v wait %q; wallet %s", r, x.waitClass(r.ID), x.walletState(name))
	}
	x.fake.mine(100)
	x.mustPoll()
	file, _ := x.fake.file(name)
	if x.walletState(name) != "destroyed" || file.password == derived || x.fake.countOf("change_wallet_password") != changes+1 {
		t.Fatalf("wallet %s, password changed %v, change_wallet_password +%d", x.walletState(name), file.password != derived, x.fake.countOf("change_wallet_password")-changes)
	}
}
