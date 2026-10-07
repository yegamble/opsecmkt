package market

import (
	"context"
	"errors"
	"fmt"
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
// that pass records (the refusal, or the unavailable estimate), and staff are told once of each of the two, however
// often the class flips between them (QA5W3B: once per distinct flagged class of the run, not once for the run).
// Before, each estimate failure restarted the refusal's count and each refusal restarted the estimate's, so it waited
// unflagged for good. With the estimate always answering, the refusal keeps its own count (the control).
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
				// Not flagged on the first pass; flagged from the second, once for each distinct flagged class.
				told := map[string]bool{}
				for _, c := range got {
					if xmrWaitFlagged(c) {
						told[c] = true
					}
				}
				flagged, notes, notices := xmrWaitFlagged(x.waitClass(r.ID)), x.flagNotes(order, r.ID), x.staffFlags(order)-flags0
				if want := len(told); flagged != (i > 0) || notes != want || notices != want*x.staffUsers() {
					t.Fatalf("pass %d: classes %q, flagged %v, %d staff notes, %d staff notices (%d staff); events %q", i, got, flagged, notes, notices, x.staffUsers(), x.events(order))
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("classes %q, want %q", got, tc.want)
			}
		})
	}
}

// QA5W2 (NIT): a stray refund already waiting too small (flagged) is not flagged too small again because a restart
// lost the in-memory record of its refusal while the fee estimate is down: it records the unavailable estimate, then
// too small again once the estimate answers. Staff are told of the unavailable estimate once, a class not yet told in
// this run (QA5W3B: once per distinct flagged class, so the order page's "market staff were notified" holds for it),
// and of too small nothing again. The refund that ended too small is untouched.
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
	if !slices.Equal(seq, want) || x.staffFlags(order) != flags0+x.staffUsers() || x.flagNotes(order, stray.ID) != notes0+1 || len(x.events(order)) != events0+1 ||
		x.classNotes(order, stray, xmrFeeEstimateUnavailable) != 1 || x.classNotes(order, stray, xmrTooSmall) != 1 {
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

// classNotes counts the staff notes xmrSettleWaits wrote for settlement s flagging class.
func (x *xmrEscrowEnv) classNotes(order string, s xmrSettlement, class string) int {
	return agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND strpos(note,$2)=1", order,
		fmt.Sprintf("Monero escrow settlement %d (%s) not sent: %s", s.ID, settlementKind(s.Kind), xmrSettleWaitText(class, s.Kind, 10)))
}

// feeWaitPass is what the wallet answers transfer on one pass (code 0: the fake wallet's own handler), and whether the
// fee estimate fails on it.
type feeWaitPass struct {
	code    int64
	feeDown bool
}

// refusalPass runs one watcher pass with the wallet answering transfer as p says.
func (x *xmrEscrowEnv) refusalPass(p feeWaitPass) {
	x.t.Helper()
	msg := map[int64]string{-16: "tx not possible", -17: "not enough money", -37: "not enough unlocked money"}[p.code]
	if p.code != 0 {
		defer x.refuseTransfer(p.code, msg)()
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = p.feeDown })
	x.mustPoll()
}

// QA5W3B-3: a refund told "fee estimate unavailable" whose estimate then answers, while the wallet still refuses for a
// reason that is not the fee, is a different diagnosis staff are told of too: -17 (the wallet sees fewer coins than it
// listed, perhaps spent outside) is an outputs mismatch, -16 a refusing wallet, each with its own note and notice.
// Before, the unavailable estimate's notice hid both for good.
func TestXMREscrowFeeWaitThenDiagnosis(t *testing.T) {
	for _, tc := range []struct {
		code int64
		then string
	}{
		{-17, xmrOutputsMismatch},
		{-16, xmrWalletRefused + " rpc:-16"},
	} {
		t.Run(strconv.FormatInt(tc.code, 10), func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			order, _, _ := x.refundOf(xmrListingPrice)
			flags0 := x.staffFlags(order)
			want := []string{xmrFeeEstimateWaiting, xmrFeeEstimateUnavailable, tc.then, tc.then, tc.then}
			told := []int{0, 1, 2, 2, 2}
			var got []string
			for i, down := range []bool{true, true, false, false, false} {
				x.refusalPass(feeWaitPass{tc.code, down})
				r := x.settlementOf(order, "refund")
				got = append(got, x.waitClass(r.ID))
				if r.State != "requested" || x.flagNotes(order, r.ID) != told[i] || x.staffFlags(order)-flags0 != told[i]*x.staffUsers() {
					t.Fatalf("pass %d: the refund %+v, classes %q, %d staff notes, %d staff notices (%d staff); events %q", i, r, got,
						x.flagNotes(order, r.ID), x.staffFlags(order)-flags0, x.staffUsers(), x.events(order))
				}
			}
			r := x.settlementOf(order, "refund")
			if !slices.Equal(got, want) || x.classNotes(order, r, xmrFeeEstimateUnavailable) != 1 || x.classNotes(order, r, tc.then) != 1 {
				t.Fatalf("classes %q (want %q); %d unavailable notes, %d %s notes; events %q", got, want,
					x.classNotes(order, r, xmrFeeEstimateUnavailable), x.classNotes(order, r, tc.then), tc.then, x.events(order))
			}
		})
	}
}

// QA5W3B-3: a stray refund whose fee estimate is down when it is first refused is told "fee estimate unavailable"; once
// the estimate answers and finds its coins too few, staff are told that as well, once.
func TestXMREscrowStrayUnavailableThenTooSmall(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.refundOf(minPayoutXMR)
	x.mustPoll()
	if r := x.settlementOf(order, "refund"); r.State != "failed" || x.waitClass(r.ID) != xmrTooSmall {
		t.Fatalf("the refund %+v %q", r, x.waitClass(r.ID))
	}
	flags0 := x.staffFlags(order)
	want := []string{xmrFeeEstimateWaiting, xmrFeeEstimateUnavailable, xmrFeeEstimateUnavailable, xmrTooSmall, xmrTooSmall}
	told := []int{0, 1, 1, 2, 2}
	var got []string
	for i, down := range []bool{true, true, true, false, false} {
		x.refusalPass(feeWaitPass{0, down})
		s := x.settlementOf(order, "stray_refund")
		got = append(got, x.waitClass(s.ID))
		if s.State != "requested" || x.flagNotes(order, s.ID) != told[i] || x.staffFlags(order)-flags0 != told[i]*x.staffUsers() {
			t.Fatalf("pass %d: the stray refund %+v, classes %q, %d staff notes, %d staff notices (%d staff); events %q", i, s, got,
				x.flagNotes(order, s.ID), x.staffFlags(order)-flags0, x.staffUsers(), x.events(order))
		}
	}
	s := x.settlementOf(order, "stray_refund")
	if !slices.Equal(got, want) || x.classNotes(order, s, xmrFeeEstimateUnavailable) != 1 || x.classNotes(order, s, xmrTooSmall) != 1 {
		t.Fatalf("classes %q (want %q); %d unavailable notes, %d too-small notes; events %q", got, want,
			x.classNotes(order, s, xmrFeeEstimateUnavailable), x.classNotes(order, s, xmrTooSmall), x.events(order))
	}
}

// QA5W3B-4: a -37 (outputs-waiting, a wait never flagged) between failed estimates neither ends the run nor makes the
// next flagged class news: over 10 passes staff are told once of each distinct flagged class, not on every second
// pass. Before, the -16/-37 alternation notified each staff member on every unavailable pass.
func TestXMREscrowFeeWaitMinus37Between(t *testing.T) {
	refused := xmrWalletRefused + " rpc:-16"
	for _, tc := range []struct {
		name   string
		cycle  []feeWaitPass
		want   []string
		tagged []string // the distinct flagged classes, each told once
	}{
		{"-16 estimate down, -37", []feeWaitPass{{-16, true}, {-37, true}},
			[]string{xmrFeeEstimateWaiting, xmrWaitOutputs, xmrFeeEstimateUnavailable, xmrWaitOutputs, xmrFeeEstimateUnavailable,
				xmrWaitOutputs, xmrFeeEstimateUnavailable, xmrWaitOutputs, xmrFeeEstimateUnavailable, xmrWaitOutputs},
			[]string{xmrFeeEstimateUnavailable}},
		{"-16 estimate down, -16 estimate up, -37", []feeWaitPass{{-16, true}, {-16, false}, {-37, false}},
			[]string{xmrFeeEstimateWaiting, refused, xmrWaitOutputs, xmrFeeEstimateUnavailable, refused, xmrWaitOutputs,
				xmrFeeEstimateUnavailable, refused, xmrWaitOutputs, xmrFeeEstimateUnavailable},
			[]string{refused, xmrFeeEstimateUnavailable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			order, _, _ := x.refundOf(xmrListingPrice)
			flags0 := x.staffFlags(order)
			var got []string
			told := map[string]bool{}
			for i := range tc.want {
				x.refusalPass(tc.cycle[i%len(tc.cycle)])
				r := x.settlementOf(order, "refund")
				class := x.waitClass(r.ID)
				got = append(got, class)
				if xmrWaitFlagged(class) {
					told[class] = true
				}
				if r.State != "requested" || x.flagNotes(order, r.ID) != len(told) || x.staffFlags(order)-flags0 != len(told)*x.staffUsers() {
					t.Fatalf("pass %d: the refund %+v, classes %q, %d staff notes, %d staff notices (%d staff); events %q", i, r, got,
						x.flagNotes(order, r.ID), x.staffFlags(order)-flags0, x.staffUsers(), x.events(order))
				}
			}
			r := x.settlementOf(order, "refund")
			if !slices.Equal(got, tc.want) || len(told) != len(tc.tagged) {
				t.Fatalf("classes %q, want %q; flagged %v", got, tc.want, told)
			}
			for _, class := range tc.tagged {
				if n := x.classNotes(order, r, class); n != 1 {
					t.Fatalf("%d %s notes, want 1; events %q", n, class, x.events(order))
				}
			}
		})
	}
}

// QA5W3B (chair, with QA5W2's restart case): what staff were told in a run is read from the order's notes, so a restart
// mid-run re-announces nothing, whichever class the passes after it record.
func TestXMREscrowFeeWaitRestartMidRun(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.refundOf(xmrListingPrice)
	refused := xmrWalletRefused + " rpc:-16"
	for _, p := range []feeWaitPass{{-16, true}, {-16, false}, {-16, true}} {
		x.refusalPass(p)
	}
	r := x.settlementOf(order, "refund")
	if x.waitClass(r.ID) != xmrFeeEstimateUnavailable || x.classNotes(order, r, refused) != 1 || x.classNotes(order, r, xmrFeeEstimateUnavailable) != 1 {
		t.Fatalf("before the restart: wait %q; events %q", x.waitClass(r.ID), x.events(order))
	}
	flags0, notes0 := x.staffFlags(order), x.flagNotes(order, r.ID)
	x.restart()
	var got []string
	for _, p := range []feeWaitPass{{-16, false}, {-37, false}, {-16, true}, {-17, false}, {-16, false}, {-16, true}} {
		x.refusalPass(p)
		got = append(got, x.waitClass(r.ID))
	}
	want := []string{refused, xmrWaitOutputs, xmrFeeEstimateUnavailable, xmrOutputsMismatch, xmrWalletRefusing + " rpc:-16", xmrFeeEstimateUnavailable}
	if !slices.Equal(got, want) || x.settlement(r.ID).State != "requested" {
		t.Fatalf("after the restart: classes %q, want %q; the refund %+v", got, want, x.settlement(r.ID))
	}
	// The outputs mismatch is the run's one new class: told once; nothing already told is told again.
	if x.flagNotes(order, r.ID) != notes0+1 || x.classNotes(order, r, xmrOutputsMismatch) != 1 || x.staffFlags(order) != flags0+x.staffUsers() {
		t.Fatalf("after the restart: staff notes +%d, mismatch notes %d, staff notices +%d (%d staff); events %q", x.flagNotes(order, r.ID)-notes0,
			x.classNotes(order, r, xmrOutputsMismatch), x.staffFlags(order)-flags0, x.staffUsers(), x.events(order))
	}
}

// QA5W4B-9: what staff were told is read per settlement. A refund told "fee estimate unavailable" that ends too small
// leaves its coins to a stray refund (another settlement, its own run); when that stray refund's estimate fails
// after a restart, staff are told of it once, although the order's notes already hold the same class for the refund.
// A lookup that ignored the settlement ID would hide it for good (mutant MH).
func TestXMREscrowUnavailableAcrossSettlements(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = true })
	order, _, _ := x.refundOf(minPayoutXMR)
	flags0 := x.staffFlags(order)
	var got []string
	var told []int
	record := func(kind string) {
		got = append(got, kind+":"+x.waitClass(x.settlementOf(order, kind).ID))
		told = append(told, (x.staffFlags(order)-flags0)/x.staffUsers())
	}
	for i := 0; i < 3; i++ {
		x.mustPoll()
		record("refund")
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = false })
	for i := 0; i < 3; i++ {
		x.mustPoll()
	}
	record("refund")
	record("stray_refund")
	x.restart()
	x.fake.edit(func(f *fakeXMREscrow) { f.feeErr = true })
	for i := 0; i < 3; i++ {
		x.mustPoll()
		record("stray_refund")
	}
	wantGot := []string{"refund:" + xmrFeeEstimateWaiting, "refund:" + xmrFeeEstimateUnavailable, "refund:" + xmrFeeEstimateUnavailable,
		"refund:" + xmrTooSmall, "stray_refund:" + xmrTooSmall, "stray_refund:" + xmrFeeEstimateUnavailable,
		"stray_refund:" + xmrFeeEstimateUnavailable, "stray_refund:" + xmrFeeEstimateUnavailable}
	// Notices in staff-member units: the refund's unavailable estimate, its too small and the stray refund's too
	// small, then the stray refund's unavailable estimate, once.
	wantTold := []int{0, 1, 1, 3, 3, 4, 4, 4}
	refund, stray := x.settlementOf(order, "refund"), x.settlementOf(order, "stray_refund")
	if !slices.Equal(got, wantGot) || !slices.Equal(told, wantTold) || x.classNotes(order, refund, xmrFeeEstimateUnavailable) != 1 ||
		x.classNotes(order, stray, xmrFeeEstimateUnavailable) != 1 {
		t.Fatalf("classes %q (want %q), staff told %v (want %v); unavailable notes: refund %d, stray refund %d; events %q", got, wantGot, told, wantTold,
			x.classNotes(order, refund, xmrFeeEstimateUnavailable), x.classNotes(order, stray, xmrFeeEstimateUnavailable), x.events(order))
	}
}
