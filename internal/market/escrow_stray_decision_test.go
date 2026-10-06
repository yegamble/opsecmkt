package market

import (
	"html"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-437: a refund that fails too-small on an order whose dispute was decided for the buyer is handled as a cancelled
// order's (A-435). Simulated RPC.

// smallDispute is a disputed escrow order priced and funded at sats (one deposit at 3 confirmations), with, when
// conflicted > 0, a deposit of that many sats Core reports as conflicted when it is funded, so it does not count: the
// order, its dispute, its escrow address and the conflicted deposit's transaction.
func (x *escrowEnv) smallDispute(refund string, sats, conflicted int64) (order, dispute, addr, big string) {
	x.t.Helper()
	order = x.draft()
	x.DB.Exec("UPDATE products SET btc=$2 WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order, sats)
	addr = x.mustCheckout(order, refund)
	x.DB.Exec("UPDATE products SET delivery_content='' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order)
	x.core.deposit(addr, txidOf("small dispute tiny "+order), 0, sats, 3)
	if conflicted > 0 {
		big = txidOf("small dispute big " + order)
		x.core.deposit(addr, big, 0, conflicted, -2)
	}
	x.poll()
	if x.state(order) != statePaid {
		x.t.Fatalf("funded order %s", x.state(order))
	}
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The content was never delivered to me at all."}}); w.Code != 303 {
		x.t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
	if err := x.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order).Scan(&dispute); err != nil {
		x.t.Fatal(err)
	}
	return order, dispute, addr, big
}

// decide has the administrator decide dispute (of an order whose counted deposits are worth seen sats) for outcome.
func (x *escrowEnv) decide(sess, dispute, outcome string, seen int64) {
	x.t.Helper()
	w := x.do("POST", "/resolve", sess, url.Values{"id": {dispute}, "outcome": {outcome}, "resolution": {"Decided from the evidence both parties sent."},
		"payout_confirmed": {"confirmed"}, "amount_seen": {strconv.FormatInt(seen, 10)}, "reply_seen": {"0"}, "password": {testPassword}})
	if w.Code != 303 {
		x.t.Fatalf("resolve %s: %d %s", outcome, w.Code, w.Body.String())
	}
}

// decidedTooFew is the order page's line on a dispute decided for the buyer whose refund failed too-small (A-437).
const decidedTooFew = "The dispute was decided for the buyer, but the market could not build the refund: the coins in this escrow were too few to pay the network fee and an output of at least 0.0001 BTC. A stray refund returns them, together with any later deposit, to the buyer's refund address once together they can pay the network fee; each deposit below says where it stands."

// A-437: the refund of a dispute decided for the buyer fails because its coins are too few to pay the network fee and
// the minimum output. As on a cancelled order (TestEscrowTooSmallRefundReturned), its deposits are left out in the same
// step (left out, flagged, one order note and one buyer notice worded for the decision, staff told); the refund's
// failure holds them, so no stray refund of the same coins is requested and nobody is told again; a deposit that did
// not count when the dispute was decided (Core reported it as conflicted) and is mined since is left out (A-428) and
// the stray refund returns both. In a second leg a deposit sent after the order closed makes the coins worth more, and
// the stray refund returns both; in a third, the rows of a database from before A-437 (the deposit still counted) are
// left out on the next pass, with the same note.
func TestEscrowTooSmallRefundResolved(t *testing.T) {
	x := newEscrowEnv(t)
	x.user("emod_"+randomToken()[:6], "moderator")
	_, admin := x.user("eadmin_"+randomToken()[:6], "admin")
	refund := regtestAddr("too few decided")
	order, dispute, _, big := x.smallDispute(refund, 10000, 500000)
	x.decide(admin, dispute, "refund", 10000)
	x.poll()
	if _, kind, state, class := x.settlement(order); x.state(order) != stateResolved || kind != "refund" || state != "failed" || class != settleTooSmall {
		t.Fatalf("the refund: %s %s %s (%s)", kind, state, class, x.state(order))
	}
	const tried = "the market could not build the refund because the coins were too few"
	told := func(order string) (notes, buyer, staff int) {
		t.Helper()
		notes = agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, tried)
		buyer = agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0 AND strpos(body,$3)>0", x.buyer, shortID(order), tried)
		staff = agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,$1)>0 AND strpos(n.body,$2)>0",
			"Payment review needed for order "+shortID(order), tried)
		return
	}
	all := func() (notes, buyer, vendor int) {
		notes = agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", order)
		buyer = agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0", x.buyer, shortID(order))
		vendor = agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0", x.vendor, shortID(order))
		return
	}
	deposits := func(order string) []string {
		return agStrings(t, x.testEnv, "SELECT amount||' '||flagged||' '||escrow_left_out||' '||(settlement_id IS NOT NULL) FROM payments WHERE order_id=$1 ORDER BY amount", order)
	}
	if got := deposits(order); !slices.Equal(got, []string{"10000 true true false", "500000 false false false"}) {
		t.Fatalf("after the refund failed: deposits %v; events %q", got, x.events(order))
	}
	if notes, buyer, staff := told(order); notes != 1 || buyer != 1 || staff == 0 {
		t.Fatalf("told of the coins left out: notes %d, buyer %d, staff %d; events %q", notes, buyer, staff, x.events(order))
	}
	// Exactly one payment-review notice per staff member, the one on the coins left out: a refund's too-small failure
	// itself does not tell staff (only a release's does, A-461; QA462-2).
	if staff, reviews, reviewed := agInt(x.testEnv, "SELECT count(*) FROM users WHERE role IN ('moderator','admin') AND suspended_at IS NULL"),
		agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE strpos(body,$1)>0", "Payment review needed for order "+shortID(order)),
		agInt(x.testEnv, "SELECT count(DISTINCT n.user_id) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,$1)>0", "Payment review needed for order "+shortID(order)); staff < 2 || reviews != staff || reviewed != staff {
		t.Fatalf("payment-review notices: %d to %d of %d staff; events %q", reviews, reviewed, staff, x.events(order))
	}
	const note = "1 deposit (0.0001 BTC, TESTNET regtest) to this order's escrow is not spent by the refund its dispute decision requested: the market could not build the refund because the coins were too few to pay the network fee and an output of at least 0.0001 BTC. " +
		"It stays in the escrow; a stray refund returns it, together with any later deposit to this escrow, less the network fee, to the buyer's refund address given at checkout once together they can pay the network fee."
	if agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2", order, note) != 1 {
		t.Fatalf("the note: %q", x.events(order))
	}
	notes, buyer, vendor := all()
	for range 3 {
		x.poll()
	}
	if id, _ := x.stray(order); id != 0 {
		t.Fatalf("a stray refund of the same coins: %q", agStrings(t, x.testEnv, "SELECT state||' '||build_error FROM settlements WHERE order_id=$1 AND kind='stray_refund'", order))
	}
	if n, b, v := all(); n != notes || b != buyer || v != vendor {
		t.Fatalf("over three passes: notes %d (was %d), buyer %d (was %d), vendor %d (was %d); events %q", n, notes, b, buyer, v, vendor, x.events(order))
	}
	page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
	for _, want := range []string{decidedTooFew, "Left out — not counted for the order; " + heldWant} {
		if !strings.Contains(page, want) {
			t.Fatalf("held page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "through the refund settlement below") {
		t.Fatalf("held page:\n%s", page)
	}

	// The conflicted deposit is mined: left out once (A-428, worded for the decision), and the stray refund returns both.
	x.core.setConfs(big, 3)
	x.poll()
	if n, b, _ := x.uncounted(order); n != 1 || b != 1 || !slices.ContainsFunc(x.events(order), func(e string) bool {
		return strings.Contains(e, "when the dispute was decided or while its refund was being built")
	}) {
		t.Fatalf("the deposit mined since: notes %d, buyer %d; events %q", n, b, x.events(order))
	}
	sid, state := x.stray(order)
	if state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE id=$1 AND destination=$2", sid, refund) != 1 {
		t.Fatalf("stray refund: %d %s; events %q", sid, state, x.events(order))
	}
	if rows := agStrings(t, x.testEnv, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1", sid); !slices.Equal(rows, []string{"claimant " + refund + " 509472"}) ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", sid) != 2 {
		t.Fatalf("stray refund outputs %q", rows)
	}
	tooFew := "Escrow settlement not built: " + settleErrorText(settleTooSmall, 0)
	if notes, buyer, _ := told(order); notes != 1 || buyer != 1 || agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2", order, tooFew) != 1 {
		t.Fatalf("told again (notes %d, buyer %d), or a second too-small failure; events %q", notes, buyer, x.events(order))
	}

	// A deposit after the close: the coins are worth more, and the stray refund returns both.
	order, dispute, addr, _ := x.smallDispute(regtestAddr("too few decided after close"), 10000, 0)
	x.decide(admin, dispute, "refund", 10000)
	x.poll()
	if got := deposits(order); !slices.Equal(got, []string{"10000 true true false"}) {
		t.Fatalf("after close: after the refund failed: deposits %v", got)
	}
	x.core.deposit(addr, txidOf("decided after close "+order), 0, 30000, 3)
	for range 2 {
		x.poll()
	}
	if sid, state := x.stray(order); state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", sid) != 2 {
		t.Fatalf("after close: stray refund %d %s; events %q", sid, state, x.events(order))
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2", order, tooFew); n != 1 {
		t.Fatalf("after close: %d too-small failures; events %q", n, x.events(order))
	}

	// A database from before A-437: the refund failed too-small and its deposit still counts. The next pass leaves it
	// out with the same note, once; it stays held.
	order, dispute, _, _ = x.smallDispute(regtestAddr("too few decided upgraded"), 10000, 0)
	x.decide(admin, dispute, "refund", 10000)
	x.poll()
	x.DB.Exec("UPDATE payments SET escrow_left_out=false,flagged=false,escrow_too_few_by=NULL WHERE order_id=$1", order)
	n0, b0, _ := told(order)
	for range 3 {
		x.poll()
	}
	if got := deposits(order); !slices.Equal(got, []string{"10000 true true false"}) {
		t.Fatalf("upgraded: deposits %v", got)
	}
	if n, b, _ := told(order); n != n0+1 || b != b0+1 || agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2", order, note) != 2 {
		t.Fatalf("upgraded: notes %d (were %d), buyer %d (was %d); events %q", n, n0, b, b0, x.events(order))
	}
	if notes, buyer, staff := x.uncounted(order); notes+buyer+staff != 0 {
		t.Fatalf("upgraded: did-not-count notes %d, buyer %d, staff %d", notes, buyer, staff)
	}
	if id, state := x.stray(order); id != 0 {
		t.Fatalf("upgraded: stray refund %d %s", id, state)
	}
}

// A-464 (QA437-4): the decision line says the coins were too few only for a refund that failed too-small. A refund
// decided for the buyer that failed for another reason (here nothing to settle: Core reports the deposit as conflicted
// before the refund is built, so no deposit counts) gets the ordinary line, never the too-few one.
func TestEscrowDecidedRefundFailedOtherwise(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("eadmin_"+randomToken()[:6], "admin")
	order, dispute, _, _ := x.smallDispute(regtestAddr("decided nothing"), 100000, 0)
	x.decide(admin, dispute, "refund", 100000)
	x.core.setConfs(txidOf("small dispute tiny "+order), -2)
	x.poll()
	if _, kind, state, class := x.settlement(order); kind != "refund" || state != "failed" || class != settleNothing {
		t.Fatalf("the refund: %s %s %s; events %q", kind, state, class, x.events(order))
	}
	for _, sess := range []string{x.buyerSess, admin} {
		page := html.UnescapeString(x.page("/order?id="+order, sess))
		if strings.Contains(page, decidedTooFew) || strings.Contains(page, "too few") ||
			!strings.Contains(page, "The dispute was decided for the buyer: the coins in this escrow go back to the buyer's refund address through the refund settlement below.") {
			t.Fatalf("the decision line of a refund that failed with nothing to settle:\n%s", page)
		}
	}
}
