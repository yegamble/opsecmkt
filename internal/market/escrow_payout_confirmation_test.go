package market

import (
	"net/url"
	"strings"
	"testing"
)

// A-493 (A-161, A-122 on escrow): every action that settles an escrow order's coins (an administrator's decision, a
// vendor's cancellation of a paid order, a buyer's no-ship cancellation, a cancellation of an unpaid escrow that already
// received a deposit) needs the ticked confirmation and the amount the form stated (payoutConfirmation); the settlement
// step compares that amount with what it counts under its row locks (checkPayoutSeen). A missing tick or amount is a 400
// and a changed amount a 409, and neither writes anything. Fake escrow Core: simulated RPC, not chain evidence.

// escrowUnchanged is everything a refused settling action must leave as it was on order.
func (x *escrowEnv) escrowUnchanged(order string) string {
	x.t.Helper()
	return agStr(x.testEnv, `SELECT o.state||' events='||(SELECT count(*) FROM order_events WHERE order_id=o.id)||
		' settlements='||(SELECT count(*) FROM settlements WHERE order_id=o.id)||
		' disputes='||coalesce((SELECT string_agg(status||':'||outcome,',') FROM disputes WHERE order_id=o.id),'')||
		' notifications='||(SELECT count(*) FROM notifications)||' receipts='||(SELECT count(*) FROM form_submissions)||
		' audit='||(SELECT count(*) FROM audit_events)
		FROM orders o WHERE o.id=$1`, order)
}

func TestEscrowResolveNeedsConfirmationAndUnchangedAmount(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("econfirm_admin_"+randomToken()[:6], "admin")
	order, dispute := x.disputedEscrow(regtestAddr("confirm-resolve-refund"))
	if seen := amountSeen(t, x.page("/moderator", admin)); len(seen) != 1 || seen[0] != 100000 {
		t.Fatalf("the desk states amount_seen %v, want [100000]", seen)
	}
	decision := url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"The content was never delivered; refund the buyer."}, "reply_seen": {"0"}, "password": {testPassword}}
	before := x.escrowUnchanged(order)
	refuse := func(name string, f url.Values, code int, want string) {
		t.Helper()
		w := x.do("POST", "/resolve", admin, f)
		if w.Code != code || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		if after := x.escrowUnchanged(order); after != before {
			t.Fatalf("%s wrote something:\n before %s\n after  %s", name, before, after)
		}
	}
	noTick := payoutConfirmed(cloneValues(decision), 100000)
	noTick.Del("payout_confirmed")
	refuse("no confirmation", noTick, 400, "Tick the box confirming the amount")
	noAmount := payoutConfirmed(cloneValues(decision), 100000)
	noAmount.Del("amount_seen")
	refuse("no amount_seen", noAmount, 400, "did not say which amount you reviewed")
	negative := payoutConfirmed(cloneValues(decision), 100000)
	negative.Set("amount_seen", "-1")
	refuse("negative amount_seen", negative, 400, "did not say which amount you reviewed")

	// Another confirmed deposit reaches the escrow after the desk was shown: the decision the administrator reviewed no
	// longer describes what the refund returns.
	x.core.deposit(x.escrowAddr(order), txidOf("confirm-resolve-extra"), 0, 50000, 3)
	x.poll()
	before = x.escrowUnchanged(order)
	refuse("changed amount", payoutConfirmed(cloneValues(decision), 100000), 409, "The amount changed to 0.0015 BTC — review again")
	refuse("forged amount", payoutConfirmed(cloneValues(decision), 150001), 409, "The amount changed to 0.0015 BTC — review again")

	seen := amountSeen(t, x.page("/moderator", admin))
	if len(seen) != 1 || seen[0] != 150000 {
		t.Fatalf("the desk states amount_seen %v after the deposit, want [150000]", seen)
	}
	x.check(x.do("POST", "/resolve", admin, payoutConfirmed(cloneValues(decision), seen[0])), 303)
	if _, kind, state := x.mainSettlement(order); kind != "refund" || state != "building" || x.state(order) != stateResolved ||
		agInt(x.testEnv, "SELECT count(*) FROM disputes WHERE id=$1 AND outcome='refund' AND status<>'Open'", dispute) != 1 {
		t.Fatalf("the confirmed decision: %s %s, order %s", kind, state, x.state(order))
	}
}

func TestEscrowCancelNeedsConfirmationAndUnchangedAmount(t *testing.T) {
	x := newEscrowEnv(t)
	_, pub := testPGPKey(t, "econfirmvendor"+randomToken()[:6])
	x.setKey(x.vendor, pub)
	physical := func(label string) string {
		t.Helper()
		order := x.testEnv.order(x.buyer, x.physicalListing(x.vendor, 3), "BTC", stateDraft)
		x.mustCheckout(order, regtestAddr(label))
		return order
	}
	refuse := func(name, order, sess string, f url.Values, code int, want string) {
		t.Helper()
		before := x.escrowUnchanged(order)
		w := x.do("POST", "/orders/cancel", sess, f)
		if w.Code != code || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		if after := x.escrowUnchanged(order); after != before {
			t.Fatalf("%s wrote something:\n before %s\n after  %s", name, before, after)
		}
	}
	refunded := func(name, order string, sum int64) {
		t.Helper()
		if _, kind, state := x.mainSettlement(order); kind != "refund" || state != "building" || x.state(order) != stateCancelled {
			t.Fatalf("%s: %s %s, order %s", name, kind, state, x.state(order))
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE $2", order, "Escrow refund requested: the market builds a transaction returning the escrowed "+amount(sum, 8)+" BTC%"); n != 1 {
			t.Fatalf("%s: %d refund notes for %d sats", name, n, sum)
		}
	}

	// The vendor cancels a paid order: the refund returns every counted deposit.
	paid := physical("confirm-cancel-paid")
	x.fund(paid, paid, 3, 100000)
	cancel := url.Values{"order_id": {paid}, "from": {statePaid}}
	noTick := payoutConfirmed(cloneValues(cancel), 100000)
	noTick.Del("payout_confirmed")
	refuse("vendor cancel without confirmation", paid, x.vendorSess, noTick, 400, "Tick the box confirming the amount")
	refuse("vendor cancel without amount_seen", paid, x.vendorSess, url.Values{"order_id": {paid}, "from": {statePaid}, "payout_confirmed": {"confirmed"}}, 400, "did not say which amount you reviewed")
	x.core.deposit(x.escrowAddr(paid), txidOf("confirm-cancel-extra"), 0, 20000, 3)
	x.poll()
	refuse("vendor cancel of a changed amount", paid, x.vendorSess, payoutConfirmed(cloneValues(cancel), 100000), 409, "The amount changed to 0.0012 BTC — review again")
	x.check(x.do("POST", "/orders/cancel", x.vendorSess, payoutConfirmed(cloneValues(cancel), 120000)), 303)
	refunded("vendor cancel", paid, 120000)

	// The buyer's no-ship cancellation once the ship-by time has passed (P2-R5) refunds the escrow too.
	late := physical("confirm-cancel-noship")
	x.fund(late, late, 3, 100000)
	x.forceDeadline(late, "ship_by=now()-interval '1 minute'")
	noShip := url.Values{"order_id": {late}, "from": {statePaid}}
	noTick = payoutConfirmed(cloneValues(noShip), 100000)
	noTick.Del("payout_confirmed")
	refuse("no-ship cancel without confirmation", late, x.buyerSess, noTick, 400, "Tick the box confirming the amount")
	refuse("no-ship cancel of another amount", late, x.buyerSess, payoutConfirmed(cloneValues(noShip), 99999), 409, "The amount changed to 0.001 BTC — review again")
	x.check(x.do("POST", "/orders/cancel", x.buyerSess, payoutConfirmed(cloneValues(noShip), 100000)), 303)
	refunded("no-ship cancel", late, 100000)

	// An unpaid escrow that received part of its price: cancelling it refunds that deposit, so it is confirmed too, and a
	// form shown before the deposit (no amount_seen) is told to reload.
	under := physical("confirm-cancel-under")
	x.fund(under, under, 3, 40000)
	if x.state(under) != stateAwaitingPayment {
		t.Fatalf("underpaid escrow is %s", x.state(under))
	}
	unpaid := url.Values{"order_id": {under}, "from": {stateAwaitingPayment}}
	refuse("cancel shown before the deposit", under, x.buyerSess, cloneValues(unpaid), 409, "A deposit arrived for this order after the page was shown")
	noTick = payoutConfirmed(cloneValues(unpaid), 40000)
	noTick.Del("payout_confirmed")
	refuse("underpaid cancel without confirmation", under, x.buyerSess, noTick, 400, "Tick the box confirming the amount")
	refuse("underpaid cancel of another amount", under, x.vendorSess, payoutConfirmed(cloneValues(unpaid), 0), 409, "The amount changed to 0.0004 BTC — review again")
	x.check(x.do("POST", "/orders/cancel", x.vendorSess, payoutConfirmed(cloneValues(unpaid), 40000)), 303)
	refunded("underpaid cancel", under, 40000)
}
