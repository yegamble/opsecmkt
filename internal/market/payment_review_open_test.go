package market

import (
	"html"
	"net/url"

	"strconv"
	"strings"
	"testing"
)

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
	x.endWindows()
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
