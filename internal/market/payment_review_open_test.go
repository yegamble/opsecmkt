package market

import (
	"context"
	"html"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// QA462C: one rule (paymentFlagOpen) decides whether a payment flag is open. For every kind of flag, open on the
// moderation desk ⇔ the disposition form is shown ⇔ recording it answers 303, and the desk gives the flag's reason.
// flagLateDeposits holds a notice episode open (a new deposit after the close is not announced again) exactly for an
// open flag of a late type (not credited, not below the floor, not locked, not spent outside the market): every flag
// holding it is one staff can record, and an open locked, below-floor, regressed or spent-outside flag never silences
// the notice (QA462D-1).

// flagCase is one flagged deposit: the payments columns set besides the defaults (5000, flagged, the order's output
// index), the reason the desk gives, and whether its flag is open.
type flagCase struct {
	name, cols, vals, reason string
	open                     bool
}

// lateEpisodeOpen reports whether flagLateDeposits reads order's flags as an open notice episode: it records a new
// confirmed deposit at addr after the order closed and runs flagLateDeposits, which tells the buyer and the staff only
// when no flag of the order holds the episode.
func lateEpisodeOpen(t *testing.T, e *testEnv, order, cur, addr string, threshold int64) bool {
	t.Helper()
	ctx := context.Background()
	told := func() int {
		return agInt(e, "SELECT count(*) FROM notifications n JOIN orders o ON o.buyer_id=n.user_id WHERE o.id=$1 AND strpos(n.body,'Order '||left(o.id,8)||':')=1 AND strpos(n.body,'after the order closed')>0", order)
	}
	staff := func() int {
		return agInt(e, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,'Payment review needed for order '||left($1::text,8))=1 AND (strpos(n.body,' after this order was ')>0 OR strpos(n.body,'after the order closed')>0)", order)
	}
	before, staffBefore := told(), staff()
	agExec(e, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,after_close) VALUES($1,$2,$3,COALESCE((SELECT max(idx) FROM payments WHERE order_id=$1 AND currency='XMR'),0),$4,5000,$5,true)", order, cur, txidOf("episode probe "+order), addr, threshold)
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", order))
	if err != nil {
		t.Fatal(err)
	}
	if err = e.A.flagLateDeposits(ctx, tx, &o, threshold, false, true); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if (told() > before) != (staff() > staffBefore) {
		t.Fatalf("the buyer was told %d times, the staff %d times", told()-before, staff()-staffBefore)
	}
	switch told() - before {
	case 0:
		return true
	case 1:
		return false
	default:
		t.Fatalf("the buyer was told %d times of one deposit", told()-before)
		return false
	}
}

// flagRow is the moderation desk's payment-review row of txid (any output: a Monero escrow deposit's index names its
// account), or "" when it is not listed.
func flagRow(desk, txid string) string {
	i := strings.Index(desk, txid+":")
	if i < 0 {
		return ""
	}
	lo, hi := strings.LastIndex(desk[:i], `<tr class="payment-review-row">`), strings.Index(desk[i:], "</tr>")
	if lo < 0 || hi < 0 {
		return ""
	}
	return desk[lo : i+hi]
}

// checkFlagCase seeds c on order (a fresh one: the episode is the order's) and checks the desk and the review against
// c.open, and the episode against c.open and the flag's late type.
func checkFlagCase(t *testing.T, e *testEnv, page func(path, sess string) string, mod, order, cur, addr string, threshold int64, c flagCase) {
	t.Helper()
	txid := txidOf("flag matrix " + c.name + " " + order)
	var id string
	if err := e.DB.QueryRow("INSERT INTO payments(order_id,currency,txid,idx,address,amount,flagged,"+c.cols+") VALUES($1,$2,$3,COALESCE((SELECT max(idx) FROM payments WHERE order_id=$1 AND currency='XMR'),0),$4,5000,true,"+c.vals+") RETURNING id::text",
		order, cur, txid, addr).Scan(&id); err != nil {
		t.Fatalf("%s: %v", c.name, err)
	}
	desk := html.UnescapeString(page("/moderator", mod))
	row := flagRow(desk, txid)
	open, form := strings.Contains(row, "<strong>Open:</strong>"), strings.Contains(desk, `name="payment_id" value="`+id+`"`)
	if row == "" || open != c.open || form != c.open || !strings.Contains(row, c.reason) {
		t.Errorf("%s: open %v, form %v (want %v), want reason %q; row: %s", c.name, open, form, c.open, c.reason, row)
	}
	late := agInt(e, "SELECT count(*) FROM payments WHERE id=$1 AND NOT credited AND NOT below_floor AND NOT locked AND NOT spent_outside", id) == 1
	episode := lateEpisodeOpen(t, e, order, cur, addr, threshold)
	if episode != (c.open && late) {
		t.Errorf("%s: flagLateDeposits reads the flag as holding the episode %v (want %v)", c.name, episode, c.open && late)
	}
	if episode && !open {
		t.Errorf("%s: a flag the desk does not open holds the late-deposit episode", c.name)
	}
	want := 409
	if c.open {
		want = 303
	}
	w := e.do("POST", "/moderator/payment-review", mod, url.Values{"payment_id": {id}, "outcome": {"no_action"}, "note": {"Recorded by the flag matrix."}, "password": {testPassword}})
	if w.Code != want || (want == 409) != (agInt(e, "SELECT count(*) FROM payments WHERE id=$1 AND disposition=''", id) == 1) {
		t.Errorf("%s: review answered %d (want %d): %s", c.name, w.Code, want, strings.TrimSpace(w.Body.String()))
	}
}

func TestPaymentReviewEscrowMatrix(t *testing.T) {
	spender := txidOf("flag matrix spender")
	t.Run("bitcoin", func(t *testing.T) {
		x := newEscrowEnv(t)
		_, mod := x.user("fmmod_"+randomToken()[:6], "moderator")
		threshold := x.A.escrow.Load().confirmations
		for _, c := range []flagCase{
			{"after_close", "confirmations,after_close", "3,true", "Open:</strong> Deposit to an escrow after the order closed, not counted; a stray refund from the escrow returns it to the buyer", true},
			{"after_close_conflicted", "confirmations,after_close", "-1,true", "Deposit to an escrow after the order closed, not counted; a stray refund from the escrow returns it to the buyer; now conflicted or missing", true},
			// The NIT: a deposit left out of its settlement before the close is not one "after the order closed".
			{"left_out", "confirmations,escrow_left_out", "3,true", "Open:</strong> Escrow deposit left out of its settlement, not counted; a stray refund from the escrow returns it to the buyer", true},
			// QA462D-2: a credited deposit left out (a refund too few could not spend it) no longer counts: open.
			{"left_out_credited", "confirmations,credited,escrow_left_out", "3,true,true", "Open:</strong> Escrow deposit left out of its settlement, not counted; a stray refund from the escrow returns it to the buyer", true},
			{"left_out_after_close", "confirmations,after_close,escrow_left_out", "3,true,true", "Deposit to an escrow after the order closed, not counted", true},
			{"spent_outside_uncredited", "confirmations,spent_outside,spent_by", "3,true,'" + spender + "'", "Escrow deposit spent by a transaction this market did not make, with no settlement of the market covering it (transaction " + spender + ")", true},
			// QA462C-3: open on the desk, so recordable.
			{"spent_outside_credited", "confirmations,credited,spent_outside,spent_by", "3,true,true,'" + spender + "'", "Escrow deposit spent by a transaction this market did not make", true},
			{"locked_counted", "confirmations,locked", "3,true", "Open:</strong> Locked transfer (unlock time), not counted or paid out", true},
			// The escrow's own confirmation threshold, not the pooled one (no pooled provider here).
			{"regressed_credited", "confirmations,credited,regress_notice", "0,true,true", "Open:</strong> Credited deposit below threshold (0 of 3 confirmations)", true},
			{"regressed_credited_conflicted", "confirmations,credited,regress_notice", "-1,true,true", "Open:</strong> Credited deposit conflicted or missing", true},
			{"below_floor_counted", "confirmations,below_floor", "3,true", "Open:</strong> Below the minimum automatic payout, not paid out", true},
			{"below_floor_credited", "confirmations,credited,below_floor", "3,true,true", "Open:</strong> Below the minimum automatic payout, not paid out", true},
			// Open (below the floor) while it confirms: never "settles it".
			{"below_floor_credited_confirming", "confirmations,credited,below_floor", "1,true,true", "Open:</strong> Credited deposit below threshold (1 of 3 confirmations)", true},
			// QA462C-1: counted for the order, credited or not: not open anywhere.
			{"counted_uncredited", "confirmations", "3", "Escrow deposit counted for the order, though not credited toward its funding (recorded before the order closed); no stray refund returns it", false},
			{"counted_uncredited_conflicted", "confirmations", "-1", "counted for the order, though not credited toward its funding (recorded before the order closed); no stray refund returns it; now conflicted or missing", false},
			// QA462D-3: a credited escrow flag that never regressed (a release too few, an external settlement).
			{"credited_confirmed", "confirmations,credited", "3,true", "Escrow deposit counted for the order (credited toward its funding); the order's release or refund settles it", false},
			{"credited_conflicted", "confirmations,credited", "-2,true", "Escrow deposit counted for the order (credited toward its funding); the order's release or refund settles it; now conflicted or missing", false},
			{"credited_confirming", "confirmations,credited", "1,true", "Escrow deposit counted for the order (credited toward its funding); the order's release or refund settles it; now 1 of 3 confirmations", false},
		} {
			order := x.completed(regtestAddr("flag matrix "+c.name), 100000)
			addr := agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
			checkFlagCase(t, x.testEnv, x.page, mod, order, "BTC", addr, threshold, c)
		}
	})
	t.Run("monero", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		_, mod := x.user("fmxmod_"+randomToken()[:6], "moderator")
		threshold := x.A.escrowXMR.confirmations
		n := strconv.FormatInt(threshold, 10)
		for _, c := range []flagCase{
			{"after_close", "confirmations,after_close", n + ",true", "Open:</strong> Deposit to an escrow after the order closed, not counted; a stray refund from the escrow returns it to the buyer", true},
			{"left_out", "confirmations,escrow_left_out", n + ",true", "Open:</strong> Escrow deposit left out of its settlement, not counted", true},
			{"left_out_credited", "confirmations,credited,escrow_left_out", n + ",true,true", "Open:</strong> Escrow deposit left out of its settlement, not counted", true},
			{"regressed_credited", "confirmations,credited,regress_notice", "5,true,true", "Open:</strong> Credited deposit below threshold (5 of " + n + " confirmations)", true},
			{"below_floor_counted", "confirmations,below_floor", n + ",true", "Open:</strong> Below the minimum automatic payout", true},
			{"counted_uncredited", "confirmations", n, "Escrow deposit counted for the order, though not credited toward its funding", false},
			{"credited_confirmed", "confirmations,credited", n + ",true", "Escrow deposit counted for the order (credited toward its funding); the order's release or refund settles it", false},
		} {
			order, addr, _ := x.completed()
			checkFlagCase(t, x.testEnv, x.page, mod, order, "XMR", addr, threshold, c)
		}
	})
	// QM1, QM2: escrow thresholds that differ from the fallbacks (3 for Bitcoin, xmrMinConfirmations for Monero).
	t.Run("escrow thresholds", func(t *testing.T) {
		x := newEscrowEnv(t)
		_, mod := x.user("fmtmod_"+randomToken()[:6], "moderator")
		order := x.completed(regtestAddr("flag matrix threshold"), 100000)
		addr := agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
		x.A.escrow.Load().confirmations = 4
		checkFlagCase(t, x.testEnv, x.page, mod, order, "BTC", addr, 4, flagCase{"regressed_credited_4", "confirmations,credited,regress_notice", "2,true,true", "Open:</strong> Credited deposit below threshold (2 of 4 confirmations)", true})
		m := newXMREscrowEnv(t)
		m.mustPoll()
		_, xmod := m.user("fmtxmod_"+randomToken()[:6], "moderator")
		xorder, xaddr, _ := m.completed()
		m.A.escrowXMR.confirmations = 12
		checkFlagCase(t, m.testEnv, m.page, xmod, xorder, "XMR", xaddr, 12, flagCase{"regressed_credited_12", "confirmations,credited,regress_notice", "11,true,true", "Open:</strong> Credited deposit below threshold (11 of 12 confirmations)", true})
	})
	t.Run("pooled", func(t *testing.T) {
		p := newPayEnv(t)
		_, mod := p.user("fmpmod_"+randomToken()[:6], "moderator")
		threshold := p.A.confirmationThreshold("BTC")
		for _, c := range []flagCase{
			{"late", "confirmations", "3", "Open:</strong> Deposit after settlement or after the order closed, not paid out", true},
			{"after_close", "confirmations,after_close", "3,true", "Open:</strong> Deposit after settlement or after the order closed, not paid out", true},
			{"locked", "confirmations,locked", "3,true", "Open:</strong> Locked transfer", true},
			{"below_floor", "confirmations,below_floor", "3,true", "Open:</strong> Below the minimum automatic payout", true},
			{"regressed_credited", "confirmations,credited,regress_notice", "1,true,true", "Open:</strong> Credited deposit below threshold (1 of 3 confirmations)", true},
			{"credited_confirmed", "confirmations,credited", "3,true", "Credited deposit confirmed again (3 confirmations)", false},
		} {
			order, addr := p.newOrder(stateCompleted)
			checkFlagCase(t, p.testEnv, p.page, mod, order, "BTC", addr, threshold, c)
		}
		// A pooled uncredited deposit is refundable by hand (an escrow one never is).
		order, addr := p.newOrder(stateCompleted)
		agExec(p.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged) VALUES($1,'BTC',$2,0,$3,5000,3,true)", order, txidOf("pooled refundable "+order), addr)
		if desk := p.page("/moderator", mod); !strings.Contains(desk, `value="refunded"`) {
			t.Errorf("no refund-by-hand form for a pooled late deposit")
		}
	})
}

// QA462C-1, QA462C-4 (A-211 on a Monero escrow): a deposit still in the pool at funding counts for the order but is
// never credited; when the fee cap holds, the stray refund of a deposit after the close waits, and the wait flags the
// deposits it could spend (xmrSettleWaits) in the same pass, before the deposit is announced (flagLateDeposits). The
// counted deposit's flag is not open anywhere, the after-close deposit is announced to the buyer exactly once and to
// the staff, and once staff record every flag the desk offers (the counted one is refused), a new deposit after the
// close is announced again.
func TestXMRSettleWaitsCountedEpisode(t *testing.T) {
	x := newXMREscrowEnv(t)
	t.Setenv("ESCROW_XMR_MAX_FEE", strconv.FormatInt(fakeXMRFee(1, 2)-1, 10))
	x.restart()
	x.mustPoll()
	_, mod := x.user("xcmod_"+randomToken()[:6], "moderator")
	order, addr := x.freshCheckout(x.digital(x.vendor))
	funded, pool, late, later := hex64("counted funded "+order), hex64("counted pool "+order), hex64("counted late "+order), hex64("counted later "+order)
	x.fake.payAddress(addr, funded, xmrListingPrice, 10)
	x.fake.payAddress(addr, pool, xmrListingPrice/4, 0)
	x.mustPoll()
	if x.state(order) != stateCompleted {
		t.Fatalf("funded order: %s", x.state(order))
	}
	x.fake.setConfs(pool, 10)
	x.mustPoll()
	x.fake.payAddress(addr, late, xmrListingPrice/5, 10)
	x.mustPoll()
	rows := func() []string {
		return agStrings(t, x.testEnv, "SELECT left(txid,8)||' credited='||credited||' after_close='||after_close||' flagged='||flagged||' disp='||disposition FROM payments WHERE order_id=$1 ORDER BY id", order)
	}
	told := lateNotices(t, x.testEnv, order)
	if told.buyer != 1 || told.staff < 1 {
		t.Fatalf("the deposit after the close: buyer told %d times, staff %d; rows %q; events %q", told.buyer, told.staff, rows(), x.events(order))
	}
	id, _ := x.release(order)
	x.mustPoll()
	x.mustPoll()
	if x.waitClass(id) != xmrFeeCap {
		t.Fatalf("release wait %q; rows %q", x.waitClass(id), rows())
	}
	desk := html.UnescapeString(x.page("/moderator", mod))
	pid := func(txid string) string {
		return agStrings(t, x.testEnv, "SELECT id::text FROM payments WHERE order_id=$1 AND txid=$2", order, txid)[0]
	}
	if row := flagRow(desk, late); !strings.Contains(row, "<strong>Open:</strong> Deposit to an escrow after the order closed") || !strings.Contains(desk, `name="payment_id" value="`+pid(late)+`"`) {
		t.Errorf("the deposit after the close on the desk: %s", row)
	}
	if row := flagRow(desk, pool); row == "" || strings.Contains(row, "Open:") || !strings.Contains(row, "counted for the order") || strings.Contains(desk, `name="payment_id" value="`+pid(pool)+`"`) {
		t.Errorf("the counted deposit on the desk: %s", row)
	}
	for _, r := range []struct {
		txid string
		want int
	}{{late, 303}, {pool, 409}} {
		if w := x.do("POST", "/moderator/payment-review", mod, url.Values{"payment_id": {pid(r.txid)}, "outcome": {"no_action"}, "note": {"Recorded by the counted episode check."}, "password": {testPassword}}); w.Code != r.want {
			t.Errorf("review of %s: %d (want %d) %s", r.txid[:8], w.Code, r.want, strings.TrimSpace(w.Body.String()))
		}
	}
	x.fake.payAddress(addr, later, xmrListingPrice/5, 10)
	x.mustPoll()
	if now := lateNotices(t, x.testEnv, order); now.buyer != told.buyer+1 || now.staff <= told.staff {
		t.Errorf("A-211: the deposit after staff recorded every flag: buyer told %d then %d, staff %d then %d; rows %q; events %q", told.buyer, now.buyer, told.staff, now.staff, rows(), x.events(order))
	}
}

// lateCount is how often a deposit after an escrow order closed was announced: to the order's buyer and to the staff.
type lateCount struct{ buyer, staff int }

func lateNotices(t *testing.T, e *testEnv, order string) lateCount {
	t.Helper()
	const late = "arrived at this order's escrow address after the order closed"
	return lateCount{
		buyer: agInt(e, "SELECT count(*) FROM notifications n JOIN orders o ON o.buyer_id=n.user_id WHERE o.id=$1 AND strpos(n.body,'Order '||left(o.id,8)||':')=1 AND strpos(n.body,$2)>0", order, late),
		staff: agInt(e, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,'Payment review needed for order '||left($1::text,8))=1 AND strpos(n.body,$2)>0", order, late),
	}
}

// QA462C-4: the buyer is told of a Monero deposit after the close exactly once and the staff at least once, whether
// a waiting settlement (xmrSettleWaits) flags the order's deposits before it confirms (the release waits on the fee
// cap while it is still in the pool) or after it was announced.
func TestXMRLateNoticeEitherOrder(t *testing.T) {
	feeCap := strconv.FormatInt(fakeXMRFee(1, 2)-1, 10)
	t.Run("wait first", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		t.Setenv("ESCROW_XMR_MAX_FEE", feeCap)
		x.restart()
		x.mustPoll()
		order, addr, _ := x.completed()
		late := hex64("wait first late " + order)
		x.fake.payAddress(addr, late, xmrListingPrice/5, 0)
		x.mustPoll()
		id, _ := x.release(order)
		x.mustPoll()
		x.mustPoll()
		if x.waitClass(id) != xmrFeeCap {
			t.Fatalf("release wait %q", x.waitClass(id))
		}
		x.fake.setConfs(late, 10)
		x.mustPoll()
		x.mustPoll()
		if told := lateNotices(t, x.testEnv, order); told.buyer != 1 || told.staff < 1 {
			t.Fatalf("buyer told %d times, staff %d; events %q", told.buyer, told.staff, x.events(order))
		}
	})
	t.Run("announced first", func(t *testing.T) {
		// No fee cap yet: the deposit is announced and its stray refund sent before the release waits.
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, addr, _ := x.completed()
		late := hex64("announced first late " + order)
		x.fake.payAddress(addr, late, xmrListingPrice/5, 10)
		x.mustPoll()
		if told := lateNotices(t, x.testEnv, order); told.buyer != 1 || told.staff < 1 {
			t.Fatalf("buyer told %d times, staff %d; events %q", told.buyer, told.staff, x.events(order))
		}
		t.Setenv("ESCROW_XMR_MAX_FEE", feeCap)
		x.restart()
		id, _ := x.release(order)
		x.mustPoll()
		x.mustPoll()
		if x.waitClass(id) != xmrFeeCap {
			t.Fatalf("release wait %q", x.waitClass(id))
		}
		if told := lateNotices(t, x.testEnv, order); told.buyer != 1 || told.staff < 1 {
			t.Fatalf("after the release's wait: buyer told %d times, staff %d; events %q", told.buyer, told.staff, x.events(order))
		}
	})
}

// QA462C-2 (QA462-3's promise): a release too few that tried only a counted deposit not credited at funding (the
// credited one is conflicted since) still flags the order's credited deposits, so staff told to review it can open
// the order; the flag is not open, so the desk offers no action on it.
func TestEscrowReleaseTooFewCreditedConflicted(t *testing.T) {
	x := newEscrowEnv(t)
	_, mod := x.user("ecmod_"+randomToken()[:6], "moderator")
	order := x.draft()
	addr := x.mustCheckout(order, regtestAddr("too few conflicted"))
	funded, confirming := txidOf("too few conflicted funded "+order), txidOf("too few conflicted confirming "+order)
	x.core.deposit(addr, funded, 0, 100000, 3)
	x.core.deposit(addr, confirming, 0, 2000, 0)
	x.poll()
	if x.state(order) != stateCompleted {
		t.Fatalf("order %s after funding", x.state(order))
	}
	x.core.setConfs(confirming, 3)
	x.poll()
	x.core.setConfs(funded, -2)
	x.poll()
	if code, body := x.claim(order, x.vendorSess, regtestAddr("too few conflicted claim")); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	x.poll()
	x.poll()
	if _, kind, state, class := x.settlement(order); kind != "release" || state != "failed" || class != settleTooSmall {
		t.Fatalf("the release: %s %s %s", kind, state, class)
	}
	if told := agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.handle LIKE 'ecmod_%' AND strpos(n.body,$1)=1", "Payment review needed for order "+shortID(order)); told != 1 {
		t.Fatalf("the moderator was told %d times", told)
	}
	if w := x.do("GET", "/order?id="+order, mod, nil); w.Code != 200 || !strings.Contains(w.Body.String(), order) {
		t.Fatalf("the moderator told to review order %s cannot open it: %d", shortID(order), w.Code)
	}
	desk := x.page("/moderator", mod)
	if row := deskRow(desk, funded); row == "" || strings.Contains(row, "Open:") || strings.Contains(desk, `name="payment_id"`) {
		t.Fatalf("the credited deposit's flag on the desk: %s", row)
	}
	// QA462D-4: the release's note names the deposit it flagged though it tried another, so the desk dates the flag.
	if row := deskRow(desk, funded); !flaggedAt.MatchString(row) || strings.Contains(row, "confirmed again") {
		t.Fatalf("the credited deposit's flag is not dated, or says it confirmed again: %s\nevents %q", row, x.events(order))
	}
	// A review posted anyway is refused without saying the deposit "confirmed again": it never regressed.
	pid := agStrings(t, x.testEnv, "SELECT id::text FROM payments WHERE order_id=$1 AND txid=$2", order, funded)[0]
	if w := x.do("POST", "/moderator/payment-review", mod, url.Values{"payment_id": {pid}, "outcome": {"no_action"}, "note": {"Nothing to do for this one."}, "password": {testPassword}}); w.Code != 409 ||
		strings.Contains(w.Body.String(), "confirmed again") || !strings.Contains(w.Body.String(), "credited") {
		t.Fatalf("a review of the credited deposit's flag: %d %s", w.Code, w.Body.String())
	}
	if got := agStrings(t, x.testEnv, "SELECT amount||' '||flagged||' '||credited FROM payments WHERE order_id=$1 ORDER BY amount", order); strings.Join(got, ",") != "2000 false false,100000 true true" {
		t.Fatalf("the deposits: %q", got)
	}
}

// QA462C chair ruling: an open locked flag and an open below-floor flag on a pooled order are open on the desk, yet a
// later deposit after the close is announced to the buyer and the staff, by the watcher, as before QA462C.
func TestLateDepositAnnouncedBesideOpenFlag(t *testing.T) {
	p := newPayEnv(t)
	for _, c := range []struct{ name, cols, vals string }{
		{"locked", "confirmations,locked", "3,true"},
		{"below_floor", "confirmations,below_floor", "3,true"},
	} {
		order, addr := p.newOrder(stateCompleted)
		agExec(p.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,flagged,"+c.cols+") VALUES($1,'BTC',$2,0,$3,5000,true,"+c.vals+")", order, txidOf("beside open "+c.name+" "+order), addr)
		p.fake.Deposit(addr, txidOf("beside open late "+c.name+" "+order), 0, 7000, 3)
		p.poll()
		buyer := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)=1 AND strpos(body,'after the order closed')>0", p.buyer.ID, "Order "+shortID(order)+":")
		staff := p.count("SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,$1)=1 AND strpos(n.body,' after this order was ')>0", "Payment review needed for order "+shortID(order))
		if buyer != 1 || staff < 1 {
			t.Errorf("%s: the deposit after the close beside an open %s flag: buyer told %d times, staff %d; rows %q", c.name, c.name, buyer, staff,
				agStrings(t, p.testEnv, "SELECT amount||' flagged='||flagged||' after_close='||after_close||' confs='||confirmations FROM payments WHERE order_id=$1 ORDER BY id", order))
		}
	}
}

// QA462D-1 (real path): a deposit still confirming at funding counts but is never credited; it confirms, then the
// parties spend it together outside the market. Its spent-outside flag is open on the desk, yet the next deposit after
// the close is announced to the buyer and the staff.
func TestLateDepositAnnouncedBesideUncreditedOutsideSpend(t *testing.T) {
	x := newEscrowEnv(t)
	_, mod := x.user("esomod_"+randomToken()[:6], "moderator")
	order := x.draft()
	addr := x.mustCheckout(order, regtestAddr("outside uncredited"))
	funded, pool := txidOf("outside uncredited funded "+order), txidOf("outside uncredited pool "+order)
	x.core.deposit(addr, funded, 0, 100000, 3)
	x.core.deposit(addr, pool, 0, 2000, 0)
	x.poll()
	if x.state(order) != stateCompleted {
		t.Fatalf("order %s after funding", x.state(order))
	}
	x.core.setConfs(pool, 3)
	x.poll()
	x.coSpend(pool, 0, 1)
	x.poll()
	rows := func() []string {
		return agStrings(t, x.testEnv, "SELECT amount||' credited='||credited||' flagged='||flagged||' after_close='||after_close||' outside='||spent_outside FROM payments WHERE order_id=$1 ORDER BY id", order)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND txid=$2 AND spent_outside AND NOT credited AND flagged AND NOT after_close", order, pool) != 1 {
		t.Fatalf("the spent-outside deposit: %q", rows())
	}
	if row := deskRow(x.page("/moderator", mod), pool); !strings.Contains(row, "<strong>Open:</strong> Escrow deposit spent by a transaction this market did not make") {
		t.Fatalf("the spent-outside flag on the desk: %s", row)
	}
	x.core.deposit(addr, txidOf("outside uncredited late "+order), 0, 7000, 3)
	x.poll()
	if n := lateNotices(t, x.testEnv, order); n.buyer != 1 || n.staff < 1 {
		t.Fatalf("the deposit after the close beside an uncredited spent-outside flag: buyer told %d times, staff %d; rows %q; events %q", n.buyer, n.staff, rows(), x.events(order))
	}
}

// QA462D-1 (second trigger): a deposit after the close that another transaction spends in the pass that records it
// is flagged as spent outside the market (findOutsideSpends, before settleEscrow), never announced as a late deposit;
// its flag does not silence the next deposit after the close.
func TestLateDepositAnnouncedAfterOutsideSpentLateDeposit(t *testing.T) {
	x := newEscrowEnv(t)
	x.user("eolmod_"+randomToken()[:6], "moderator")
	order := x.completed(regtestAddr("outside late"), 100000)
	addr := agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
	gone := txidOf("outside late gone " + order)
	x.core.deposit(addr, gone, 0, 7000, 3)
	x.coSpend(gone, 0, 1)
	x.poll()
	rows := func() []string {
		return agStrings(t, x.testEnv, "SELECT amount||' credited='||credited||' flagged='||flagged||' after_close='||after_close||' outside='||spent_outside FROM payments WHERE order_id=$1 ORDER BY id", order)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND txid=$2 AND spent_outside AND after_close AND flagged", order, gone) != 1 {
		t.Fatalf("the deposit spent outside the market: %q; events %q", rows(), x.events(order))
	}
	before := lateNotices(t, x.testEnv, order)
	x.core.deposit(addr, txidOf("outside late next "+order), 0, 8000, 3)
	x.poll()
	if n := lateNotices(t, x.testEnv, order); n.buyer != before.buyer+1 || n.staff <= before.staff {
		t.Fatalf("the next deposit after the close: buyer told %d then %d, staff %d then %d; rows %q; events %q", before.buyer, n.buyer, before.staff, n.staff, rows(), x.events(order))
	}
}

// QA462E-1: a deposit a refund too few left out, whose review staff recorded (no action, or escalated to an
// administrator), counts again when the decision changes to a release (countAgain); a second change back to a refund
// that fails too few again leaves it out again. That is a new flag: open, dated, with a form the moderator can use, and
// recorded with 303; the order history keeps the first review.
func TestEscrowLeftOutAgainIsANewFlag(t *testing.T) {
	for _, first := range []string{"no_action", "escalated"} {
		t.Run(first, func(t *testing.T) {
			x := newEscrowEnv(t)
			_, mod := x.user("lamod_"+randomToken()[:6], "moderator")
			_, admin := x.user("laadm_"+randomToken()[:6], "admin")
			_, admin2 := x.user("laadm2_"+randomToken()[:6], "admin")
			order, dispute, _, _ := x.smallDispute(regtestAddr("left out again "+first), 10000, 0)
			x.decide(admin, dispute, "refund", 10000)
			x.poll()
			tiny := txidOf("small dispute tiny " + order)
			pid := agStrings(t, x.testEnv, "SELECT id::text FROM payments WHERE order_id=$1 AND txid=$2", order, tiny)[0]
			if w := x.do("POST", "/moderator/payment-review", mod, url.Values{"payment_id": {pid}, "outcome": {first}, "note": {"Recorded before the decision changed."}, "password": {testPassword}}); w.Code != 303 {
				t.Fatalf("first review: %d %s", w.Code, w.Body.String())
			}
			if code, body := x.changeDecision(admin2, order, "release", testPassword); code != 303 {
				t.Fatalf("change to release: %d %s", code, strings.TrimSpace(body))
			}
			x.poll()
			if got := agStrings(t, x.testEnv, "SELECT flagged||' '||escrow_left_out||' '||disposition FROM payments WHERE id=$1", pid); got[0] != "false false " {
				t.Fatalf("counted again: flagged, left out, disposition %q", got)
			}
			if code, body := x.claim(order, x.vendorSess, regtestAddr("left out again claim "+first)); code != 303 {
				t.Fatalf("claim: %d %s", code, body)
			}
			x.poll()
			x.poll()
			if code, body := x.changeDecision(admin2, order, "refund", testPassword); code != 303 {
				t.Fatalf("change back to refund: %d %s", code, strings.TrimSpace(body))
			}
			x.poll()
			x.poll()
			desk := html.UnescapeString(x.page("/moderator", mod))
			row := deskRow(desk, tiny)
			if !flaggedAt.MatchString(row) || !strings.Contains(row, "<strong>Open:</strong> Escrow deposit left out of its settlement") || strings.Contains(row, "escalated") ||
				!strings.Contains(desk, `name="payment_id" value="`+pid+`"`) {
				t.Fatalf("left out again: %s; deposit %q; events %q", row, agStrings(t, x.testEnv, "SELECT flagged||' '||escrow_left_out||' '||disposition FROM payments WHERE id=$1", pid), x.events(order))
			}
			if w := x.do("POST", "/moderator/payment-review", mod, url.Values{"payment_id": {pid}, "outcome": {"no_action"}, "note": {"Recorded after it was left out again."}, "password": {testPassword}}); w.Code != 303 {
				t.Fatalf("second review: %d %s", w.Code, w.Body.String())
			}
			if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NOT NULL AND strpos(note,'Payment review of')=1", order); n != 2 {
				t.Fatalf("%d review events in the order history, want both", n)
			}
		})
	}
}

// QA462D-4, QA462E-5: a refund too few that leaves out two credited deposits names both in one note; each desk row is
// dated and open; no party notice asks for a moderator review.
func TestEscrowTooFewNamesEveryDeposit(t *testing.T) {
	x := newEscrowEnv(t)
	_, mod := x.user("tfmod_"+randomToken()[:6], "moderator")
	_, admin := x.user("tfadm_"+randomToken()[:6], "admin")
	order := x.draft()
	x.DB.Exec("UPDATE products SET btc=10000 WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order)
	addr := x.mustCheckout(order, regtestAddr("too few two"))
	x.DB.Exec("UPDATE products SET delivery_content='' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order)
	a1, a2 := txidOf("too few two a "+order), txidOf("too few two b "+order)
	x.core.deposit(addr, a1, 0, 6000, 3)
	x.core.deposit(addr, a2, 0, 4000, 3)
	x.poll()
	if x.state(order) != statePaid {
		t.Fatalf("state %s", x.state(order))
	}
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The content was never delivered to me at all."}}); w.Code != 303 {
		t.Fatalf("dispute: %d", w.Code)
	}
	var dispute string
	x.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order).Scan(&dispute)
	x.decide(admin, dispute, "refund", 10000)
	x.poll()
	if _, kind, state, class := x.settlement(order); kind != "refund" || state != "failed" || class != settleTooSmall {
		t.Fatalf("the refund: %s %s %s; deposits %q", kind, state, class, x.decisionDeposits(order))
	}
	desk := html.UnescapeString(x.page("/moderator", mod))
	for _, tx := range []string{a1, a2} {
		if row := deskRow(desk, tx); !flaggedAt.MatchString(row) || !strings.Contains(row, "<strong>Open:</strong> Escrow deposit left out") {
			t.Errorf("desk row of %s: %s", tx[:8], row)
		}
	}
	names := []string{"deposit " + truncate(a1, 20) + " (0.00006 BTC)", "deposit " + truncate(a2, 20) + " (0.00004 BTC)"}
	slices.Sort(names)
	notes := agStrings(t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1 AND strpos(note,'The deposits:')>0", order)
	if len(notes) != 1 || !strings.Contains(notes[0], "2 deposits") || !strings.Contains(notes[0], "The deposits: "+strings.Join(names, ", ")+". Moderator review required.") {
		t.Errorf("the note does not name both: %q", notes)
	}
	for _, uid := range []string{x.buyer, x.vendor} {
		for _, b := range agStrings(t, x.testEnv, "SELECT body FROM notifications WHERE user_id=$1 AND strpos(body,$2)=1", uid, "Order "+shortID(order)) {
			if strings.Contains(b, "Moderator review required") {
				t.Errorf("a party's notice asks for a moderator review: %q", b)
			}
		}
	}
}
