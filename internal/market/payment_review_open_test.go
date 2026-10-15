package market

import (
	"context"
	"html"
	"net/url"

	"strconv"
	"strings"
	"testing"
)

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
	if err = e.A.flagLateDeposits(ctx, tx, &o, threshold); err != nil {
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
