package market

import (
	"html"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The A-435 and A-436 fix round (QA on 225f104): the guards that act on a cancelled order only, the latest word on
// coins a too-small refund left out, a database upgraded from before A-435, the dust skip's bounds and the status lines
// that follow a stray refund's state. Simulated RPC.

// escrowAddr is order's Bitcoin escrow address.
func (x *escrowEnv) escrowAddr(order string) string {
	x.t.Helper()
	return agStrings(x.t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
}

// A-435's guards as A-437 extends them (QA REQUIRED-1): a refund's too-small failure leaves its deposits out, and
// holds the coins a stray refund returns, on an order whose coins go back to the buyer: cancelled
// (TestEscrowTooSmallRefundReturned) or resolved with the dispute decided for the buyer, the live decision; never on
// one whose coins are the vendor's, nor through a refund a changed decision superseded.
//   - Decided for the buyer, the refund too few: its deposit is left out and flagged, and no stray refund is requested
//     for it (A-437 changed this leg, which expected it counted, unflagged; TestEscrowTooSmallRefundResolved has the
//     whole journey). A deposit sent after that order closed makes the coins worth more: a stray refund is requested.
//   - Decided for the vendor, the release too few (it fails like a refund): its deposit stays counted and unflagged,
//     the parties are told, and staff once (A-461); a deposit Core reported as conflicted when the dispute was decided and mined since stays
//     counted for the release (no A-428 note); a deposit after the close gets its stray refund, since a release's
//     failure holds nothing (new leg).
//   - Decided for the vendor, then for the buyer, then for the vendor again: a stray refund of a deposit after the
//     close fails too-small and holds its coins; neither the refund the second decision requests (newer than that
//     failure) nor, once the third decision supersedes it, that refund hides the failure, so the same coins are not
//     tried again (new leg; a refund that failed and was then superseded is TestEscrowDecisionChangeUnbuilt's, A-462).
func TestEscrowTooSmallRefundCancelledOnly(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("eadmin_"+randomToken()[:6], "admin")
	resolved := func(name string) string {
		t.Helper()
		order, dispute := x.disputedEscrow(regtestAddr("too few resolved " + name))
		if code, body := x.resolve(admin, dispute, "refund", testPassword); code != 303 || x.state(order) != stateResolved {
			t.Fatalf("%s: resolve: %d %s", name, code, body)
		}
		// Too few to refund: the deposit as recorded is worth 5000 sats.
		x.DB.Exec("UPDATE payments SET amount=5000 WHERE order_id=$1", order)
		x.poll()
		if _, kind, state, class := x.settlement(order); kind != "refund" || state != "failed" || class != settleTooSmall {
			t.Fatalf("%s: the refund: %s %s %s", name, kind, state, class)
		}
		return order
	}
	deposits := func(order string) []string {
		return agStrings(t, x.testEnv, "SELECT amount||' '||escrow_left_out||' '||flagged FROM payments WHERE order_id=$1 ORDER BY amount", order)
	}
	strays := func(order string) []string {
		return agStrings(t, x.testEnv, "SELECT state||' '||build_error||' '||COALESCE(input_total,0) FROM settlements WHERE order_id=$1 AND kind='stray_refund' ORDER BY id", order)
	}

	order := resolved("counted")
	for range 2 {
		x.poll()
	}
	if got := deposits(order); !slices.Equal(got, []string{"5000 true true"}) {
		t.Fatalf("a resolved order's too-small refund: deposits %v; events %q", got, x.events(order))
	}
	if id, state := x.stray(order); id != 0 {
		t.Fatalf("a resolved order's too-small refund: stray refund %d %s", id, state)
	}

	order = resolved("after close")
	x.core.deposit(x.escrowAddr(order), txidOf("after close "+order), 0, 4000, 3)
	for range 2 {
		x.poll()
	}
	if id, _ := x.stray(order); id == 0 {
		t.Fatalf("a deposit after a resolved order closed: no stray refund; events %q", x.events(order))
	}

	// Decided for the vendor: the release fails too-small and leaves everything counted.
	order = x.draft()
	addr := x.mustCheckout(order, regtestAddr("too few release"))
	x.DB.Exec("UPDATE products SET delivery_content='' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order)
	big := txidOf("release conflicted " + order)
	x.core.deposit(addr, big, 0, 500000, -2)
	x.fund(order, order, 3, 100000)
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The content was never delivered to me at all."}}); w.Code != 303 {
		t.Fatalf("release: dispute: %d %s", w.Code, w.Body.String())
	}
	if code, body := x.resolve(admin, x.disputeID(order), "release", testPassword); code != 303 {
		t.Fatalf("release: resolve: %d %s", code, body)
	}
	x.DB.Exec("UPDATE payments SET amount=5000 WHERE order_id=$1 AND txid<>$2", order, big)
	if code, body := x.claim(order, x.vendorSess, regtestAddr("too few release claim")); code != 303 {
		t.Fatalf("release: claim: %d %s", code, body)
	}
	x.poll()
	if _, kind, state, class := x.settlement(order); kind != "release" || state != "failed" || class != settleTooSmall {
		t.Fatalf("release: %s %s %s", kind, state, class)
	}
	x.core.setConfs(big, 3)
	for range 2 {
		x.poll()
	}
	if got := deposits(order); !slices.Equal(got, []string{"5000 false false", "500000 false false"}) {
		t.Fatalf("release: deposits %v; events %q", got, x.events(order))
	}
	if notes, buyer, staff := x.uncounted(order); notes+buyer+staff != 0 || agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0 AND strpos(body,'could not be built')>0", x.vendor, shortID(order)) != 1 {
		t.Fatalf("release: did-not-count notes %d, buyer %d, staff %d; events %q", notes, buyer, staff, x.events(order))
	}
	if id, state := x.stray(order); id != 0 {
		t.Fatalf("release: stray refund %d %s", id, state)
	}
	// A-461: staff are told once, with the note (one), as a Monero release too few is.
	tooFew := "Escrow settlement not built: " + settleErrorText(settleTooSmall, 0)
	if staff, told, notes := agInt(x.testEnv, "SELECT count(*) FROM users WHERE role IN ('moderator','admin') AND suspended_at IS NULL"),
		agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND n.body=$1", "Payment review needed for order "+shortID(order)+": "+tooFew),
		agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2", order, tooFew); staff == 0 || told != staff || notes != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role NOT IN ('moderator','admin') AND strpos(n.body,'Payment review needed')>0") != 0 {
		t.Fatalf("release: staff %d told %d times, %d notes; events %q", staff, told, notes, x.events(order))
	}
	x.core.deposit(addr, txidOf("release after close "+order), 0, 4000, 3)
	x.poll()
	if id, _ := x.stray(order); id == 0 {
		t.Fatalf("release: a deposit after the close: no stray refund; events %q", x.events(order))
	}

	// A refund that is not the live decision: the decision is changed to a refund after a stray refund's too-small
	// failure, and back to a release once that refund is built.
	order, dispute := x.disputedEscrow(regtestAddr("too few changed"))
	if code, body := x.resolve(admin, dispute, "release", testPassword); code != 303 {
		t.Fatalf("changed: resolve: %d %s", code, body)
	}
	x.core.deposit(x.escrowAddr(order), txidOf("changed after close "+order), 0, 3000, 3)
	x.poll()
	if got := strays(order); !slices.Equal(got, []string{"failed " + settleTooSmall + " 3000"}) {
		t.Fatalf("changed: the stray refund: %q", got)
	}
	live := func() []string {
		return agStrings(t, x.testEnv, "SELECT kind||' '||state FROM settlements WHERE order_id=$1 AND kind IN ('release','refund') AND state<>'superseded'", order)
	}
	if code, body := x.changeDecision(admin, order, "refund", testPassword); code != 303 {
		t.Fatalf("changed: to a refund: %d %s", code, body)
	}
	x.poll()
	if got := live(); !slices.Equal(got, []string{"refund awaiting_signature"}) {
		t.Fatalf("changed: the refund %q; stray refunds %q", got, strays(order))
	}
	if code, body := x.changeDecision(admin, order, "release", testPassword); code != 303 {
		t.Fatalf("changed: back to a release: %d %s", code, body)
	}
	for range 3 {
		x.poll()
	}
	if got := strays(order); len(live()) != 0 || !slices.Equal(got, []string{"failed " + settleTooSmall + " 3000"}) || !slices.Equal(deposits(order), []string{"3000 false true", "100000 false false"}) {
		t.Fatalf("changed: live %q, stray refunds %q (the same coins tried again?), deposits %v; events %q", live(), got, deposits(order), x.events(order))
	}
}

// A-435 (QA SHOULD-1): a stray refund requested while a cancelled order's refund still waits (here for a deposit to
// confirm) is older than that refund's failure. When the refund then fails too-small, its failure is the latest word
// on the coins it left out: no stray refund of the same coins is requested, so they fail too-small once, with one
// note. The buyer's request is answered as the refund's failure, and builds once. A second leg (QA435B-2) has the
// stray refund signed and confirmed only after the refund failed.
func TestEscrowTooSmallRefundAfterStray(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.draft()
	addr := x.mustCheckout(order, regtestAddr("too few after stray"))
	x.core.deposit(addr, txidOf("after stray tiny "+order), 0, 5000, 3)
	x.core.deposit(addr, txidOf("after stray stuck "+order), 0, 50000, 1)
	x.poll()
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, "amount_seen": {"55000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	rid, _, state, class := x.settlement(order)
	if state != "building" || class != settleWaiting {
		t.Fatalf("the refund waits: %s %s", state, class)
	}
	x.core.deposit(addr, txidOf("after stray late "+order), 0, 30000, 3)
	x.poll()
	sid, state := x.stray(order)
	if state != "awaiting_signature" {
		t.Fatalf("the stray refund of the deposit after close: %d %s", sid, state)
	}
	x.signAs(order, roleBuyer, sid)
	x.poll()
	x.core.setTxConfs(x.settleByID(sid).TxID, 3, "")
	x.poll()
	if r := x.settleByID(sid); r.State != "confirmed" {
		t.Fatalf("the stray refund: %+v", r)
	}
	x.DB.Exec("UPDATE settlements SET created=now()-interval '25 hours' WHERE id=$1", rid)
	for range 4 {
		x.poll()
	}
	all := func() []string {
		return agStrings(t, x.testEnv, "SELECT kind||' '||state||' '||build_error||' '||COALESCE(input_total,0) FROM settlements WHERE order_id=$1 ORDER BY id", order)
	}
	if got := all(); !slices.Equal(got, []string{"refund failed " + settleTooSmall + " 5000", "stray_refund confirmed  30000"}) {
		t.Fatalf("after the refund failed: settlements %q; events %q", got, x.events(order))
	}
	tooFew := "Escrow settlement not built: " + settleErrorText(settleTooSmall, 0)
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2", order, tooFew); n != 1 {
		t.Fatalf("%d too-few notes; events %q", n, x.events(order))
	}
	if page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess)); !strings.Contains(page, tooFewStatus+heldWant) {
		t.Fatalf("held page:\n%s", page)
	}
	if code, body := x.recover(order, x.buyerSess, testPassword); code != 303 {
		t.Fatalf("recover: %d %s", code, body)
	}
	for range 3 {
		x.poll()
	}
	if got := all(); !slices.Equal(got, []string{"refund failed " + settleTooSmall + " 5000", "stray_refund confirmed  30000", "stray_refund failed " + settleTooSmall + " 5000"}) {
		t.Fatalf("after the buyer's request: settlements %q", got)
	}
	if !slices.ContainsFunc(x.events(order), func(e string) bool {
		return strings.Contains(e, "At the buyer's request the market tries again: the refund could not be built because the coins were too few")
	}) {
		t.Fatalf("the request's note: %q", x.events(order))
	}

	// QA435B-2: as above, but the refund fails while the stray refund still waits for a signature, and the buyer signs
	// after it: the stray refund's updated (signed, broadcast, confirmed) is then later than the refund's failure, which
	// still holds the coins it left out (the latest word is not by updated).
	order = x.draft()
	addr = x.mustCheckout(order, regtestAddr("too few after stray signed after"))
	x.core.deposit(addr, txidOf("signed after tiny "+order), 0, 5000, 3)
	x.core.deposit(addr, txidOf("signed after stuck "+order), 0, 50000, 1)
	x.poll()
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, "amount_seen": {"55000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("signed after: cancel: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	rid, _, state, class = x.settlement(order)
	if state != "building" || class != settleWaiting {
		t.Fatalf("signed after: the refund waits: %s %s", state, class)
	}
	x.core.deposit(addr, txidOf("signed after late "+order), 0, 30000, 3)
	x.poll()
	sid, state = x.stray(order)
	if state != "awaiting_signature" {
		t.Fatalf("signed after: the stray refund of the deposit after close: %d %s", sid, state)
	}
	x.DB.Exec("UPDATE settlements SET created=now()-interval '25 hours' WHERE id=$1", rid)
	x.poll()
	if got := all(); !slices.Equal(got, []string{"refund failed " + settleTooSmall + " 5000", "stray_refund awaiting_signature  30000"}) {
		t.Fatalf("signed after: the refund failed while the stray refund waits: settlements %q; events %q", got, x.events(order))
	}
	x.signAs(order, roleBuyer, sid)
	x.poll()
	x.core.setTxConfs(x.settleByID(sid).TxID, 3, "")
	for range 4 {
		x.poll()
	}
	if r := x.settleByID(sid); r.State != "confirmed" {
		t.Fatalf("signed after: the stray refund: %+v", r)
	}
	if got := all(); !slices.Equal(got, []string{"refund failed " + settleTooSmall + " 5000", "stray_refund confirmed  30000"}) {
		t.Fatalf("signed after: the refund's coins were tried again: settlements %q; events %q", got, x.events(order))
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2", order, tooFew); n != 1 {
		t.Fatalf("signed after: %d too-few notes; events %q", n, x.events(order))
	}
	if page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess)); !strings.Contains(page, tooFewStatus+heldWant) {
		t.Fatalf("signed after: held page:\n%s", page)
	}
}

// A-435 (QA435B-1): a stray refund requested while a cancelled order's refund waits, and built in the same pass right
// after that refund failed too-small, spends the coins the refund left out (with the deposit after the close) and is
// confirmed. The refund's failure is then no longer the word on any coins: a later deposit, refundable on its own, gets
// its stray refund, and the page does not say the coins are too few. At 50 sat/vB.
func TestEscrowTooSmallRefundSpentByStray(t *testing.T) {
	x := newEscrowEnv(t)
	e := x.A.escrow.Load()
	t.Cleanup(func() { e.feeRate = escrowDefaultFeeRate })
	e.feeRate = 50
	order := x.draft()
	addr := x.mustCheckout(order, regtestAddr("too few spent by stray"))
	x.core.deposit(addr, txidOf("spent by stray c1 "+order), 0, 10000, 3)
	x.core.deposit(addr, txidOf("spent by stray c2 "+order), 0, 10000, 3)
	x.core.deposit(addr, txidOf("spent by stray stuck "+order), 0, 50000, 1)
	x.poll()
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, "amount_seen": {"70000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	rid, _, state, class := x.settlement(order)
	if state != "building" || class != settleWaiting {
		t.Fatalf("the refund waits: %s %s", state, class)
	}
	// The deposit after the close and the end of the refund's 24-hour wait in one pass: the stray refund is requested,
	// then the refund is built and fails too-small, then the stray refund is built, spending the refund's coins too.
	x.core.deposit(addr, txidOf("spent by stray after "+order), 0, 30000, 3)
	x.DB.Exec("UPDATE settlements SET created=now()-interval '25 hours' WHERE id=$1", rid)
	x.poll()
	all := func() []string {
		return agStrings(t, x.testEnv, "SELECT kind||' '||state||' '||build_error||' '||COALESCE(input_total,0)||' '||(SELECT count(*) FROM payments p WHERE p.settlement_id=s.id) FROM settlements s WHERE order_id=$1 ORDER BY id", order)
	}
	if got := all(); !slices.Equal(got, []string{"refund failed " + settleTooSmall + " 20000 0", "stray_refund awaiting_signature  50000 3"}) {
		t.Fatalf("the coinciding pass: settlements %q; events %q", got, x.events(order))
	}
	sid, _ := x.stray(order)
	x.signAs(order, roleBuyer, sid)
	x.poll()
	x.core.setTxConfs(x.settleByID(sid).TxID, 3, "")
	x.poll()
	if r := x.settleByID(sid); r.State != "confirmed" {
		t.Fatalf("the stray refund: %+v", r)
	}
	// The 50000-sat deposit the refund's build left out (still at 1 confirmation) stays until a stray refund returns it.
	const stays = "held at the escrow address until a stray refund returns them to the buyer's refund address."
	if page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess)); !strings.Contains(page, tooFewStatus+stays) {
		t.Fatalf("returned page:\n%s", page)
	}
	// A later deposit worth less than the refund's coins and refundable on its own (one input): not held.
	x.core.deposit(addr, txidOf("spent by stray later "+order), 0, 19000, 3)
	for range 3 {
		x.poll()
	}
	if got := all(); !slices.Equal(got, []string{"refund failed " + settleTooSmall + " 20000 0", "stray_refund confirmed  50000 3", "stray_refund awaiting_signature  19000 1"}) {
		t.Fatalf("a later deposit: settlements %q; events %q", got, x.events(order))
	}
	tooFew := "Escrow settlement not built: " + settleErrorText(settleTooSmall, 0)
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2", order, tooFew); n != 1 {
		t.Fatalf("%d too-few notes; events %q", n, x.events(order))
	}
	page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
	if !strings.Contains(page, tooFewStatus+stays) || strings.Contains(page, heldWant) {
		t.Fatalf("a later deposit's page:\n%s", page)
	}
}

// A-435 (QA NIT-1): on a database upgraded from before A-435, a cancelled order's refund that failed too-small left
// its deposits counted, not left out. The next pass leaves them out with leaveOutTooFew's wording (one order note, one
// buyer notice, staff told), not A-428's "did not count" note, which is not true of them; they stay held. A deposit that
// did not count at the cancellation and is mined later still gets A-428's note, once.
func TestEscrowTooSmallRefundUpgraded(t *testing.T) {
	x := newEscrowEnv(t)
	x.user("emod_"+randomToken()[:6], "moderator")
	order := x.draft()
	addr := x.mustCheckout(order, regtestAddr("too few upgraded"))
	tiny, big := txidOf("upgraded tiny "+order), txidOf("upgraded big "+order)
	x.core.deposit(addr, tiny, 0, 5000, 3)
	x.core.deposit(addr, big, 0, 500000, -2)
	x.poll()
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, "amount_seen": {"5000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	if _, _, state, class := x.settlement(order); state != "failed" || class != settleTooSmall {
		t.Fatalf("the refund: %s %s", state, class)
	}
	// The rows as a database from before A-435 has them: the refund failed too-small and its deposit still counts.
	x.DB.Exec("UPDATE payments SET escrow_left_out=false,flagged=false,escrow_too_few_by=NULL WHERE order_id=$1", order)
	const counted = "not spent by its refund: the market could not build the refund because the coins were too few"
	told := func() (notes, buyer, staff int) {
		t.Helper()
		notes = agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, counted)
		buyer = agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0 AND strpos(body,$3)>0", x.buyer, shortID(order), counted)
		staff = agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,$1)>0 AND strpos(n.body,$2)>0",
			"Payment review needed for order "+shortID(order), counted)
		return
	}
	n0, b0, s0 := told()
	for range 3 {
		x.poll()
	}
	deposits := func() []string {
		return agStrings(t, x.testEnv, "SELECT amount||' '||escrow_left_out||' '||flagged FROM payments WHERE order_id=$1 ORDER BY amount", order)
	}
	if got := deposits(); !slices.Equal(got, []string{"5000 true true", "500000 false false"}) {
		t.Fatalf("after the upgrade: deposits %v", got)
	}
	if n, b, s := told(); n != n0+1 || b != b0+1 || s <= s0 {
		t.Fatalf("after the upgrade: too-few notes %d (were %d), buyer %d (was %d), staff %d (was %d); events %q", n, n0, b, b0, s, s0, x.events(order))
	}
	if notes, buyer, staff := x.uncounted(order); notes+buyer+staff != 0 {
		t.Fatalf("after the upgrade: did-not-count notes %d, buyer %d, staff %d; events %q", notes, buyer, staff, x.events(order))
	}
	if id, state := x.stray(order); id != 0 {
		t.Fatalf("after the upgrade: stray refund %d %s", id, state)
	}

	// The conflicted deposit is mined: it did not count, so A-428's note says so, once; the stray refund returns both.
	n1, b1, s1 := told()
	x.core.setConfs(big, 3)
	x.poll()
	x.poll()
	if notes, buyer, _ := x.uncounted(order); notes != 1 || buyer != 1 {
		t.Fatalf("the deposit mined since: did-not-count notes %d, buyer %d; events %q", notes, buyer, x.events(order))
	}
	if n, b, s := told(); n != n1 || b != b1 || s != s1 {
		t.Fatalf("the deposit mined since: too-few notes %d (were %d), buyer %d (was %d), staff %d (was %d)", n, n1, b, b1, s, s1)
	}
	if sid, state := x.stray(order); state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", sid) != 2 {
		t.Fatalf("the stray refund of both: %d %s", sid, state)
	}

	// A refund built after its 24-hour wait (A-342) that failed too-small before A-435: the deposit it could not use
	// was left out then (not flagged), the one it tried still counts. That one gets the too-few note too.
	order = x.draft()
	addr = x.mustCheckout(order, regtestAddr("too few upgraded bounded"))
	tiny = txidOf("upgraded bounded tiny " + order)
	x.core.deposit(addr, tiny, 0, 5000, 3)
	x.core.deposit(addr, txidOf("upgraded bounded stuck "+order), 0, 50000, 1)
	x.poll()
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, "amount_seen": {"55000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("bounded: cancel: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	rid, _, _, _ := x.settlement(order)
	x.DB.Exec("UPDATE settlements SET created=now()-interval '25 hours' WHERE id=$1", rid)
	x.poll()
	if _, _, state, class := x.settlement(order); state != "failed" || class != settleTooSmall {
		t.Fatalf("bounded: the refund: %s %s", state, class)
	}
	x.DB.Exec("UPDATE payments SET escrow_left_out=false,flagged=false,escrow_too_few_by=NULL WHERE txid=$1", tiny)
	n0, b0, _ = told()
	x.poll()
	if got := deposits(); !slices.Equal(got, []string{"5000 true true", "50000 true false"}) {
		t.Fatalf("bounded: after the upgrade: deposits %v", got)
	}
	if n, b, _ := told(); n != n0+1 || b != b0+1 {
		t.Fatalf("bounded: after the upgrade: too-few notes %d (were %d), buyer %d (was %d); events %q", n, n0, b, b0, x.events(order))
	}
	if notes, buyer, staff := x.uncounted(order); notes+buyer+staff != 0 {
		t.Fatalf("bounded: after the upgrade: did-not-count notes %d, buyer %d, staff %d", notes, buyer, staff)
	}
}

// A-436's bounds (QA SHOULD-2). At 50 sat/vB one 2-of-3 P2WSH input costs settleInputVB×50 = 5250 sats, and a stray
// refund of one input to one output settleVSize(1,1)×50 = 7950 sats. A refund spends every input, an uneconomic one
// too (the skip is for stray refunds only: mutant D5). A stray refund skips an input worth exactly its cost (the bound
// is inclusive: D4), and keeps one worth more than its cost though less than a one-input transaction's (the cost is
// the input's, not the whole transaction's: D6).
func TestEscrowDustSkipBounds(t *testing.T) {
	x := newEscrowEnv(t)
	e := x.A.escrow.Load()
	t.Cleanup(func() { e.feeRate = escrowDefaultFeeRate })
	e.feeRate = 50
	cost, whole := settleInputVB*e.feeRate, settleVSize(1, 1)*e.feeRate
	if cost != 5250 || whole != 7950 || settleVSize(2, 1)*e.feeRate != 13200 {
		t.Fatalf("the size bounds changed (input %d, one input %d): adjust the amounts below", cost, whole)
	}
	spent := func(sid int64) []string {
		return agStrings(t, x.testEnv, "SELECT p.amount::text FROM payments p WHERE p.settlement_id=$1 ORDER BY p.amount", sid)
	}
	outputs := func(sid int64) []string {
		return agStrings(t, x.testEnv, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1", sid)
	}

	// A refund: 5000 + 30000 sats, both spent, fee 13200.
	refund := regtestAddr("dust bounds refund")
	order := x.draft()
	addr := x.mustCheckout(order, refund)
	x.core.deposit(addr, txidOf("bounds dust "+order), 0, 5000, 3)
	x.core.deposit(addr, txidOf("bounds coin "+order), 0, 30000, 3)
	x.poll()
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, "amount_seen": {"35000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	rid, kind, state, class := x.settlement(order)
	if kind != "refund" || state != "awaiting_signature" || !slices.Equal(spent(rid), []string{"5000", "30000"}) || !slices.Equal(outputs(rid), []string{"claimant " + refund + " 21800"}) {
		t.Fatalf("the refund: %s %s %s, spends %v, outputs %v", kind, state, class, spent(rid), outputs(rid))
	}

	for _, c := range []struct {
		what  string
		sats  int64
		spent []string
		out   int64
	}{
		{"an input worth exactly its cost", cost, []string{"30000"}, 30000 - whole},
		{"an input worth more than its cost, less than a one-input transaction's", 6000, []string{"6000", "30000"}, 36000 - settleVSize(2, 1)*e.feeRate},
	} {
		refund := regtestAddr("dust bounds " + c.what)
		order := x.draft()
		addr := x.mustCheckout(order, refund)
		if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}}); w.Code != 303 {
			t.Fatalf("%s: cancel: %d %s", c.what, w.Code, w.Body.String())
		}
		small := txidOf("bounds small " + order)
		x.core.deposit(addr, small, 0, c.sats, 3)
		x.core.deposit(addr, txidOf("bounds large "+order), 0, 30000, 3)
		x.poll()
		sid, state := x.stray(order)
		if state != "awaiting_signature" || !slices.Equal(spent(sid), c.spent) || !slices.Equal(outputs(sid), []string{"claimant " + refund + " " + strconv.FormatInt(c.out, 10)}) {
			t.Fatalf("%s: stray refund %d %s, spends %v, outputs %v", c.what, sid, state, spent(sid), outputs(sid))
		}
		if len(c.spent) == 1 && agInt(x.testEnv, "SELECT count(*) FROM payments WHERE txid=$1 AND settlement_id IS NULL AND after_close", small) != 1 {
			t.Fatalf("%s: the skipped input is not still eligible", c.what)
		}
	}
}
