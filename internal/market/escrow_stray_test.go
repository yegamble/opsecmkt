package market

import (
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Escrow Phase 1, slice P1-c2: the stray refund (ES-8, A-322, A-327). Coins first recorded at an escrow after its
// order closed, and outputs a bounded build left out (A-342), are returned to the buyer's pinned refund address by a
// stray_refund settlement that one party signs and the market co-signs. The escrow watcher requests one when it
// records such coins, on the buyer's "Recover coins at this escrow" (password step-up; the escrow is read on the next
// pass whatever its age) and on a daily check of every escrow. Simulated RPC.

func (x *escrowEnv) recover(order, sess, password string) (int, string) {
	w := x.do("POST", "/escrow/recover", sess, url.Values{"order_id": {order}, "password": {password}})
	return w.Code, w.Body.String()
}

// stray is order's newest stray refund (0 when none).
func (x *escrowEnv) stray(order string) (int64, string) {
	x.t.Helper()
	var id int64
	var state string
	x.DB.QueryRow("SELECT id,state FROM settlements WHERE order_id=$1 AND kind='stray_refund' ORDER BY id DESC LIMIT 1", order).Scan(&id, &state)
	return id, state
}

// A deposit to a closed order's escrow is returned by a stray refund: requested when it confirms, built, signed by
// the buyer, co-signed and broadcast; the order's own refund is untouched, and the moderation desk does not offer
// "refunded by hand" for it (A-330).
func TestEscrowStrayRefundAfterClose(t *testing.T) {
	x := newEscrowEnv(t)
	_, mod := x.user("emod_"+randomToken()[:6], "moderator")
	refund := regtestAddr("stray-refund-1")
	order := x.draft()
	addr := x.mustCheckout(order, refund)
	x.fund(order, order, 3, 40000)
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, "amount_seen": {"40000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	rid, _, _, _ := x.settlement(order)
	x.core.deposit(addr, txidOf("late"), 0, 30000, 1)
	x.poll()
	if id, _ := x.stray(order); id != 0 {
		t.Fatal("a stray refund of an unconfirmed deposit")
	}
	x.core.setConfs(txidOf("late"), 3)
	x.poll()
	sid, state := x.stray(order)
	if sid == 0 || state != "awaiting_signature" {
		t.Fatalf("stray refund: %d %s", sid, state)
	}
	if rows := agStrings(t, x.testEnv, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1", sid); !slices.Equal(rows, []string{"claimant " + refund + " 29682"}) {
		t.Fatalf("stray outputs %q", rows)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1 AND txid=$2", sid, txidOf("late")) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", rid) != 1 {
		t.Fatal("the stray refund spends only the late deposit")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%stray refund%'", x.buyer) == 0 {
		t.Fatal("the buyer is not told of the stray refund")
	}
	page := x.page("/order?id="+order, x.buyerSess)
	for _, want := range []string{"Recovery of coins sent after the order closed", "returned to the buyer&#39;s refund address by a stray refund", `action="/escrow/sign"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("buyer page lacks %q:\n%s", want, page)
		}
	}
	if desk := x.page("/moderator", mod); !strings.Contains(desk, txidOf("late")) || strings.Contains(desk, "Record: refunded by hand") {
		t.Fatalf("desk for an escrow deposit:\n%s", desk)
	}
	var pid string
	x.DB.QueryRow("SELECT id FROM payments WHERE txid=$1", txidOf("late")).Scan(&pid)
	if w := x.do("POST", "/moderator/payment-review", mod, url.Values{"payment_id": {pid}, "outcome": {"refunded"}, "txid": {txidOf("by hand")}, "amount": {"0.0003"}, "password": {testPassword}}); w.Code != 409 {
		t.Fatalf("refunded by hand for an escrow deposit: %d %s", w.Code, w.Body.String())
	}
	x.signAs(order, roleBuyer, sid)
	x.poll()
	if r := x.settleByID(sid); r.State != "broadcast" || r.Cosigned != "market" {
		t.Fatalf("stray refund after the buyer signed: %+v", r)
	}
	if _, _, state, _ := x.settlement(order); state != "awaiting_signature" {
		t.Fatalf("the order's refund changed: %s", state)
	}
	// Another late deposit gets its own stray refund only once the first has ended: one is in progress at a time.
	x.core.deposit(addr, txidOf("later"), 0, 20000, 3)
	x.poll()
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='stray_refund'", order); n != 1 {
		t.Fatalf("%d stray refunds while one is in progress", n)
	}
	x.core.setTxConfs(x.settleByID(sid).TxID, 3, "")
	x.poll()
	x.poll()
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='stray_refund'", order); n != 2 {
		t.Fatalf("%d stray refunds after the first confirmed", n)
	}
}

// The buyer's "Recover coins at this escrow" covers an escrow cancelled while escrow was unavailable (A-322): the
// address is no longer watched, so a deposit to it was never recorded; the buyer's request makes the next pass read it
// and requests the stray refund. Only the buyer, with their password, on a closed order, and not while one is in
// progress; asking again with nothing new says so.
func TestEscrowRecoverAction(t *testing.T) {
	x := newEscrowEnv(t)
	refund := regtestAddr("stray-refund-2")
	addr := x.mustCheckout(x.order, refund)
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {x.order}, "from": {stateAwaitingPayment}}); w.Code != 303 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	x.DB.Exec("UPDATE escrows SET created=now()-interval '31 days' WHERE order_id=$1", x.order)
	x.DB.Exec("UPDATE orders SET updated=now()-interval '2 days' WHERE id=$1", x.order)
	x.core.deposit(addr, txidOf("unseen"), 0, 50000, 6) // sent while escrow was unavailable; never recorded
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1", x.order) != 0 {
		t.Fatal("an unwatched address was read")
	}
	page := x.page("/order?id="+x.order, x.buyerSess)
	if !strings.Contains(page, `action="/escrow/recover"`) || !strings.Contains(page, "Recover coins at this escrow") {
		t.Fatalf("buyer page offers no recovery:\n%s", page)
	}
	if strings.Contains(x.page("/order?id="+x.order, x.vendorSess), `action="/escrow/recover"`) {
		t.Fatal("the vendor is offered the recovery")
	}
	_, stranger := x.user("stranger_"+randomToken()[:6], "buyer")
	open := x.draft()
	x.mustCheckout(open, regtestAddr("stray-refund-3"))
	for _, c := range []struct {
		name, order, sess, password string
		code                        int
	}{
		{"the vendor", x.order, x.vendorSess, testPassword, 403},
		{"a stranger", x.order, stranger, testPassword, 404},
		{"no password", x.order, x.buyerSess, "", 400},
		{"a wrong password", x.order, x.buyerSess, "wrong password 789", 401},
		{"an open order", open, x.buyerSess, testPassword, 409},
	} {
		if code, body := x.recover(c.order, c.sess, c.password); code != c.code {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	if agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE recover_requested IS NOT NULL") != 0 {
		t.Fatal("a refused request was recorded")
	}
	if code, body := x.recover(x.order, x.buyerSess, testPassword); code != 303 {
		t.Fatalf("recover: %d %s", code, body)
	}
	if !strings.Contains(strings.Join(x.events(x.order), "\n"), "asked the market to check this escrow") {
		t.Fatal("no note of the request")
	}
	x.poll()
	sid, state := x.stray(x.order)
	if sid == 0 || state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND after_close AND settlement_id=$2", x.order, sid) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE id=$1 AND requested_by=$2", sid, x.buyer) != 1 {
		t.Fatalf("after the request: %d %s", sid, state)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE recover_requested IS NOT NULL") != 0 {
		t.Fatal("the request was not cleared")
	}
	if code, _ := x.recover(x.order, x.buyerSess, testPassword); code != 409 {
		t.Fatalf("recover while a stray refund is in progress: %d", code)
	}
	x.signAs(x.order, roleBuyer, sid)
	x.poll()
	x.core.setTxConfs(x.settleByID(sid).TxID, 3, "")
	x.poll()
	// Nothing new: the buyer is told nothing was found.
	if code, body := x.recover(x.order, x.buyerSess, testPassword); code != 303 {
		t.Fatalf("recover again: %d %s", code, body)
	}
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%found no coins to recover%'", x.buyer) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='stray_refund'", x.order) != 1 {
		t.Fatal("nothing-found notice")
	}
	// With escrow off the route does not exist.
	t.Setenv("ESCROW_BTC", "off")
	x.restart()
	if code, _ := x.recover(x.order, x.buyerSess, testPassword); code != 404 {
		t.Fatalf("recover with escrow off: %d", code)
	}
}

// The daily check reads every escrow, whatever its age, and requests a stray refund of confirmed coins first recorded
// after an order closed; between checks an unwatched escrow is not read. Outputs a bounded build left out are returned
// the same way once they confirm.
func TestEscrowStrayDailyCheck(t *testing.T) {
	x := newEscrowEnv(t)
	addr := x.mustCheckout(x.order, regtestAddr("stray-refund-4"))
	x.DB.Exec("UPDATE escrows SET created=now()-interval '40 days' WHERE order_id=$1", x.order)
	x.poll() // cancelled at expiry, nothing received; this first pass is also a daily check
	x.DB.Exec("UPDATE orders SET updated=now()-interval '3 days' WHERE id=$1", x.order)
	if x.state(x.order) != stateCancelled {
		t.Fatalf("state %s", x.state(x.order))
	}
	x.core.deposit(addr, txidOf("old"), 0, 25000, 10)
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1", x.order) != 0 {
		t.Fatal("an unwatched escrow was read between daily checks")
	}
	x.DB.Exec("UPDATE settings SET value=to_char(now()-interval '25 hours','YYYY-MM-DD\"T\"HH24:MI:SSOF') WHERE key='escrow_btc_stray_check'")
	x.poll()
	sid, state := x.stray(x.order)
	if sid == 0 || state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key='escrow_btc_stray_check' AND value::timestamptz > now()-interval '1 minute'") != 1 {
		t.Fatalf("daily check: %d %s", sid, state)
	}

	// A-342's left-out output, once confirmed, is returned by a stray refund.
	order := x.draft()
	addr2 := x.mustCheckout(order, regtestAddr("stray-refund-5"))
	x.core.deposit(addr2, txidOf("full2"), 0, 100000, 3)
	x.core.deposit(addr2, txidOf("stuck2"), 0, 30000, 1)
	x.poll()
	x.claim(order, x.vendorSess, regtestAddr("stray-claim-5"))
	x.poll()
	id, _, _, _ := x.settlement(order)
	x.DB.Exec("UPDATE settlements SET created=now()-interval '25 hours' WHERE id=$1", id)
	x.poll()
	if id, _ := x.stray(order); id != 0 {
		t.Fatal("a stray refund of an unconfirmed left-out output")
	}
	x.core.setConfs(txidOf("stuck2"), 3)
	x.poll()
	sid2, state := x.stray(order)
	if sid2 == 0 || state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1 AND txid=$2 AND escrow_left_out", sid2, txidOf("stuck2")) != 1 {
		t.Fatalf("left-out stray refund: %d %s", sid2, state)
	}
}
