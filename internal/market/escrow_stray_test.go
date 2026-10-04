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
	// A second request before the market has read the escrow is refused and leaves the first one as it was.
	askedAt, notes := agStrings(t, x.testEnv, "SELECT recover_requested::text FROM escrows WHERE order_id=$1", x.order), len(x.events(x.order))
	if len(askedAt) != 1 || askedAt[0] == "" {
		t.Fatalf("the request was not recorded: %q", askedAt)
	}
	if code, body := x.recover(x.order, x.buyerSess, testPassword); code != 409 || !strings.Contains(body, "You already asked") {
		t.Fatalf("a second request: %d %s", code, body)
	}
	if got := agStrings(t, x.testEnv, "SELECT recover_requested::text FROM escrows WHERE order_id=$1", x.order); !slices.Equal(got, askedAt) || len(x.events(x.order)) != notes {
		t.Fatalf("a second request changed the first: %q, was %q (%d notes, was %d)", got, askedAt, len(x.events(x.order)), notes)
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

// uncounted counts the A-428 notes on order (leaveOutUncounted's "not spent by any refund" wording): order notes, the buyer's notices and the
// staff's notices.
func (x *escrowEnv) uncounted(order string) (notes, buyer, staff int) {
	x.t.Helper()
	const part = "not spent by any refund of this order"
	notes = agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, part)
	buyer = agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0 AND strpos(body,$3)>0", x.buyer, shortID(order), part)
	staff = agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,$1)>0 AND strpos(n.body,$2)>0",
		"Payment review needed for order "+shortID(order), part)
	return
}

// A-428 (QA's probe through expiry; the Bitcoin analogue of A-424): a deposit Core reports as conflicted (negative
// confirmations), or no longer lists (markMissing records -1), when the payment window ends does not count, so the
// order is cancelled unpaid and no refund is requested. When that transaction is mined after all, the deposit is left
// out and flagged once it has the confirmations a deposit needs, the buyer and the staff are told once, and a stray
// refund returns it, less the network fee, to the refund address pinned at checkout. In the last leg the stray refund
// is not built in the pass that leaves the deposit out (the restore gate pauses builds), so the deposit stays unspent
// by any settlement over three passes: it is left out, noted and told once all the same (QA428-3).
func TestEscrowConflictedDepositThroughExpiry(t *testing.T) {
	x := newEscrowEnv(t)
	x.user("emod_"+randomToken()[:6], "moderator")
	conflicted := func(_, txid string) { x.core.setConfs(txid, -2) }
	mined := func(_, txid string) { x.core.setConfs(txid, 1) }
	for _, c := range []struct {
		what       string
		lose, back func(addr, txid string)
		gate       bool
	}{
		{"conflicted", conflicted, mined, false},
		{"no longer listed", func(_, txid string) {
			x.core.mu.Lock()
			x.core.deposits = slices.DeleteFunc(x.core.deposits, func(d fakeEscrowDeposit) bool { return d.TxID == txid })
			x.core.mu.Unlock()
		}, func(addr, txid string) { x.core.deposit(addr, txid, 0, 100000, 1) }, false},
		{"conflicted, built a pass later", conflicted, mined, true},
	} {
		refund := regtestAddr("uncounted " + c.what)
		order := x.draft()
		addr := x.mustCheckout(order, refund)
		txid := txidOf("uncounted " + order)
		x.core.deposit(addr, txid, 0, 100000, 0)
		x.poll()
		c.lose(addr, txid)
		x.poll()
		if got := agStrings(t, x.testEnv, "SELECT (confirmations<0)::text FROM payments WHERE order_id=$1", order); !slices.Equal(got, []string{"true"}) {
			t.Fatalf("%s: deposit recorded %v", c.what, got)
		}
		x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", order)
		x.DB.Exec("UPDATE payments SET created=now()-interval '25 hours' WHERE order_id=$1", order)
		x.poll()
		if n := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", order); x.state(order) != stateCancelled || n != 0 {
			t.Fatalf("%s: at the end of the window: %s, %d settlements", c.what, x.state(order), n)
		}
		// Mined, below the confirmations a deposit needs: nothing yet.
		c.back(addr, txid)
		x.poll()
		if notes, buyer, staff := x.uncounted(order); notes+buyer+staff != 0 || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND (escrow_left_out OR flagged)", order) != 0 {
			t.Fatalf("%s: left out at 1 confirmation: notes %d, buyer %d, staff %d", c.what, notes, buyer, staff)
		}
		x.core.setConfs(txid, 3)
		x.setGate(c.gate)
		x.poll()
		if got := agStrings(t, x.testEnv, "SELECT confirmations||' '||credited||' '||flagged||' '||after_close||' '||escrow_left_out FROM payments WHERE order_id=$1", order); !slices.Equal(got, []string{"3 false true false true"}) {
			t.Fatalf("%s: mined after the cancellation: deposit %v", c.what, got)
		}
		notes, buyer, staff := x.uncounted(order)
		if notes != 1 || buyer != 1 || staff == 0 {
			t.Fatalf("%s: notes %d, buyer notices %d, staff notices %d", c.what, notes, buyer, staff)
		}
		const said = "1 deposit (0.001 BTC, TESTNET regtest) to this order's escrow, now confirmed, is not spent by any refund of this order: the escrow node reported it as conflicted"
		if agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, said) != 1 {
			t.Fatalf("%s: the note: %q", c.what, x.events(order))
		}
		if c.gate {
			// Requested but not built: no settlement spends the deposit yet, and it is not left out (nor told) again.
			x.poll()
			x.poll()
			if n, b, s := x.uncounted(order); n != 1 || b != 1 || s != staff || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id IS NULL", order) != 1 {
				t.Fatalf("%s: over three passes: notes %d, buyer notices %d, staff notices %d (was %d)", c.what, n, b, s, staff)
			}
			if sid, state := x.stray(order); sid == 0 || state != "building" {
				t.Fatalf("%s: stray refund while builds pause: %d %s", c.what, sid, state)
			}
			x.setGate(false)
			x.poll()
		}
		sid, state := x.stray(order)
		if sid == 0 || state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE id=$1 AND destination=$2", sid, refund) != 1 {
			t.Fatalf("%s: stray refund %d %s", c.what, sid, state)
		}
		if rows := agStrings(t, x.testEnv, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1", sid); !slices.Equal(rows, []string{"claimant " + refund + " 99682"}) ||
			agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1 AND txid=$2", sid, txid) != 1 ||
			agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,'Stray refund requested')>0 AND strpos(body,$2)>0", x.buyer, shortID(order)) != 1 {
			t.Fatalf("%s: stray refund outputs %q", c.what, rows)
		}
		if page := x.page("/order?id="+order, x.buyerSess); !strings.Contains(page, "Cancelled; a deposit to this escrow did not count for the order. It is held at the escrow address") ||
			!strings.Contains(page, "Left out — not counted for the order; returned to the buyer&#39;s refund address by a stray refund once confirmed") ||
			strings.Contains(page, "once the order is closed") || strings.Contains(page, "nothing was received") || strings.Contains(page, "after waiting") {
			t.Fatalf("%s: buyer page:\n%s", c.what, page)
		}
		x.signAs(order, roleBuyer, sid)
		x.poll()
		if r := x.settleByID(sid); r.State != "broadcast" || r.Cosigned != "market" {
			t.Fatalf("%s: stray refund after the buyer signed: %+v", c.what, r)
		}
		x.poll()
		if notes, buyer, staff := x.uncounted(order); notes != 1 || buyer != 1 || agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", order) != 1 {
			t.Fatalf("%s: told again (notes %d, buyer %d, staff %d), or another settlement", c.what, notes, buyer, staff)
		}
	}
}

// A-428's bounds: the "did not count" path leaves out only a cancelled order's deposit that no refund spends or will
// spend. A requested refund (still building) keeps the deposit and spends it; a refund that failed as too small keeps
// its deposits; a built refund's deposit is left out once, by leaveOutTopUps; a completed or resolved order's deposit stays the order's (the vendor's claim spends it); a
// deposit first recorded after the order closed is flagged by flagLateDeposits alone.
func TestEscrowUncountedDepositBounds(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("eadmin_"+randomToken()[:6], "admin")
	cancel := func(order, seen string) {
		t.Helper()
		if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, "amount_seen": {seen}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
			t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
		}
	}
	untouched := func(what, order, txid string) {
		t.Helper()
		if notes, buyer, staff := x.uncounted(order); notes+buyer+staff != 0 || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE txid=$1 AND (escrow_left_out OR flagged)", txid) != 0 {
			t.Fatalf("%s: the deposit was left out: notes %d, buyer %d, staff %d", what, notes, buyer, staff)
		}
	}

	// A refund still building (waiting on a deposit that is confirming) keeps the deposit that was conflicted at the
	// cancellation and has confirmed since; once built, it spends all three.
	order := x.draft()
	addr := x.mustCheckout(order, regtestAddr("bounds building"))
	x.core.deposit(addr, txidOf("counted "+order), 0, 40000, 3)
	x.core.deposit(addr, txidOf("confirming "+order), 0, 10000, 0)
	x.core.deposit(addr, txidOf("conflicted "+order), 0, 30000, -2)
	x.poll()
	cancel(order, "50000")
	x.poll()
	if _, kind, state, class := x.settlement(order); kind != "refund" || state != "building" {
		t.Fatalf("refund after the cancellation: %s %s %s", kind, state, class)
	}
	x.core.setConfs(txidOf("conflicted "+order), 3)
	x.poll()
	untouched("a refund building", order, txidOf("conflicted "+order))
	x.core.setConfs(txidOf("confirming "+order), 3)
	x.poll()
	if id, _, state, class := x.settlement(order); state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", id) != 3 {
		t.Fatalf("the refund, built: %s %s, inputs %d", state, class, agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", id))
	}
	untouched("a refund built", order, txidOf("conflicted "+order))

	// A refund that failed because the coins are too few to pay its fee and output keeps them: they counted when the
	// order was cancelled, and its note says they stay in the escrow. They are not called uncounted, and no stray
	// refund (which would fail the same way on every pass) is requested.
	order = x.draft()
	addr = x.mustCheckout(order, regtestAddr("bounds too small"))
	x.core.deposit(addr, txidOf("tiny "+order), 0, 5000, 3)
	x.poll()
	cancel(order, "5000")
	x.poll()
	x.poll()
	if _, _, state, class := x.settlement(order); state != "failed" || class != settleTooSmall {
		t.Fatalf("a too-small refund: %s %s", state, class)
	}
	untouched("a too-small refund", order, txidOf("tiny "+order))
	if id, _ := x.stray(order); id != 0 {
		t.Fatal("a stray refund of a too-small refund's deposit")
	}

	// A built refund's deposit that was conflicted at the cancellation is left out once, by leaveOutTopUps.
	order = x.draft()
	addr = x.mustCheckout(order, regtestAddr("bounds built"))
	x.core.deposit(addr, txidOf("counted "+order), 0, 40000, 3)
	x.core.deposit(addr, txidOf("conflicted "+order), 0, 30000, -2)
	x.poll()
	cancel(order, "40000")
	x.poll()
	if _, _, state, class := x.settlement(order); state != "awaiting_signature" {
		t.Fatalf("refund after the cancellation: %s %s", state, class)
	}
	x.core.setConfs(txidOf("conflicted "+order), 3)
	x.poll()
	x.poll()
	const topUp = "arrived at this order's escrow after its refund to the buyer was built"
	if notes, buyer, staff := x.uncounted(order); notes+buyer+staff != 0 ||
		agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, topUp) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0 AND strpos(body,$3)>0", x.buyer, shortID(order), topUp) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE txid=$1 AND escrow_left_out AND flagged", txidOf("conflicted "+order)) != 1 {
		t.Fatalf("a built refund's deposit: did-not-count notes %d, buyer %d, staff %d; events %q", notes, buyer, staff, x.events(order))
	}

	// A completed order's deposit, conflicted at funding and confirmed since, stays the order's: the claim spends it.
	order = x.draft()
	addr = x.mustCheckout(order, regtestAddr("bounds completed"))
	x.core.deposit(addr, txidOf("counted "+order), 0, 100000, 3)
	x.core.deposit(addr, txidOf("conflicted "+order), 0, 20000, -2)
	x.poll()
	if x.state(order) != stateCompleted {
		t.Fatalf("funded: %s", x.state(order))
	}
	x.core.setConfs(txidOf("conflicted "+order), 3)
	x.poll()
	untouched("a completed order", order, txidOf("conflicted "+order))
	if code, body := x.claim(order, x.vendorSess, regtestAddr("bounds claim")); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	x.poll()
	if id, kind, state, class := x.settlement(order); kind != "release" || state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", id) != 2 {
		t.Fatalf("the claim: %s %s %s", kind, state, class)
	}

	// A resolved order decided for the vendor, whose claim is not yet given, keeps such a deposit too.
	order = x.draft()
	addr = x.mustCheckout(order, regtestAddr("bounds resolved"))
	x.DB.Exec("UPDATE products SET delivery_content='' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order)
	x.core.deposit(addr, txidOf("counted "+order), 0, 100000, 3)
	x.core.deposit(addr, txidOf("conflicted "+order), 0, 20000, -2)
	x.poll()
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The content was never delivered to me at all."}}); w.Code != 303 || x.state(order) != stateDisputed {
		t.Fatalf("dispute: %d %s (%s)", w.Code, w.Body.String(), x.state(order))
	}
	var dispute string
	x.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order).Scan(&dispute)
	if code, body := x.resolve(admin, dispute, "release", testPassword); code != 303 || x.state(order) != stateResolved {
		t.Fatalf("resolve: %d %s (%s)", code, body, x.state(order))
	}
	x.core.setConfs(txidOf("conflicted "+order), 3)
	x.poll()
	untouched("a resolved order", order, txidOf("conflicted "+order))

	// A deposit first recorded after an unpaid cancellation is flagged once, by flagLateDeposits, and not as uncounted.
	order = x.draft()
	addr = x.mustCheckout(order, regtestAddr("bounds after close"))
	cancel(order, "")
	x.core.deposit(addr, txidOf("late "+order), 0, 30000, 3)
	x.poll()
	const late = "arrived at this order's escrow address after the order closed"
	if notes, buyer, staff := x.uncounted(order); notes+buyer+staff != 0 ||
		agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, late) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE txid=$1 AND after_close AND flagged AND NOT escrow_left_out", txidOf("late "+order)) != 1 {
		t.Fatalf("a deposit after close: did-not-count notes %d, buyer %d, staff %d; events %q", notes, buyer, staff, x.events(order))
	}
}

// A-434 (QA428-2): a stray refund whose deposits are too few to pay the network fee and the minimum output fails
// too-small once. While the deposits it could spend do not grow, no other is requested: the same coins would fail the
// same way, so the order is not noted and the buyer and the vendor are not told again on every pass, nor when a
// deposit gains confirmations. The buyer's "Recover coins at this escrow" is answered, once, that the coins are too
// few to return. A deposit that makes them more is tried again: here one recorded before the failure (conflicted when
// the order was cancelled, so left out once mined, A-428) or one sent after it.
func TestEscrowStrayRefundTooSmall(t *testing.T) {
	x := newEscrowEnv(t)
	for _, c := range []struct {
		what string
		// setup leaves order cancelled with tiny (5000 sats) confirmed and not the order's; more makes big (100000
		// sats) the order's to return too.
		setup func(order, addr, tiny, big string)
		more  func(addr, big string)
	}{
		{"left out", func(order, addr, tiny, big string) {
			x.core.deposit(addr, tiny, 0, 5000, -2)
			x.core.deposit(addr, big, 0, 100000, -2)
			x.poll()
			x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", order)
			x.DB.Exec("UPDATE payments SET created=now()-interval '25 hours' WHERE order_id=$1", order)
			x.poll()
			x.core.setConfs(tiny, 3)
		}, func(_, big string) { x.core.setConfs(big, 3) }},
		{"after close", func(order, addr, tiny, _ string) {
			if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}}); w.Code != 303 {
				t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
			}
			x.core.deposit(addr, tiny, 0, 5000, 3)
		}, func(addr, big string) { x.core.deposit(addr, big, 0, 100000, 3) }},
	} {
		refund := regtestAddr("too small " + c.what)
		order := x.draft()
		addr := x.mustCheckout(order, refund)
		tiny, big := txidOf("tiny "+order), txidOf("big "+order)
		c.setup(order, addr, tiny, big)
		told := func() (notes, buyer, vendor int) {
			t.Helper()
			notes = agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", order)
			buyer = agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0", x.buyer, shortID(order))
			vendor = agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0", x.vendor, shortID(order))
			return
		}
		x.poll()
		if x.state(order) != stateCancelled {
			t.Fatalf("%s: %s", c.what, x.state(order))
		}
		notes, buyer, vendor := told()
		for pass := 2; pass <= 4; pass++ {
			if pass == 4 {
				x.core.setConfs(tiny, 4) // a deposit gaining confirmations is not a deposit more
			}
			x.poll()
			if n, b, v := told(); n != notes || b != buyer || v != vendor {
				t.Fatalf("%s: pass %d: notes %d (was %d), buyer notices %d (was %d), vendor notices %d (was %d); events %q", c.what, pass, n, notes, b, buyer, v, vendor, x.events(order))
			}
		}
		if got := agStrings(t, x.testEnv, "SELECT state||' '||build_error FROM settlements WHERE order_id=$1 AND kind='stray_refund'", order); !slices.Equal(got, []string{"failed " + settleTooSmall}) {
			t.Fatalf("%s: stray refunds %q", c.what, got)
		}
		for _, w := range []struct {
			user, part string
		}{{x.buyer, "Stray refund requested"}, {x.buyer, "its escrow settlement could not be built"}, {x.vendor, "its escrow settlement could not be built"}} {
			if n := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0 AND strpos(body,$3)>0", w.user, shortID(order), w.part); n != 1 {
				t.Fatalf("%s: %d notices %q", c.what, n, w.part)
			}
		}

		// The buyer asks to recover the coins: told once that they are too few to return; nothing else changes.
		if code, body := x.recover(order, x.buyerSess, testPassword); code != 303 {
			t.Fatalf("%s: recover: %d %s", c.what, code, body)
		}
		x.poll()
		x.poll()
		const tooFew = "too few to return after the network fee"
		if n := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0 AND strpos(body,$3)>0", x.buyer, shortID(order), tooFew); n != 1 ||
			agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, tooFew) != 1 ||
			agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,'found no coins')>0", x.buyer) != 0 ||
			agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND recover_requested IS NOT NULL", order) != 0 ||
			agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='stray_refund'", order) != 1 {
			t.Fatalf("%s: the answer to the buyer's request (%d notices); events %q", c.what, n, x.events(order))
		}

		// A deposit more: the next stray refund spends both.
		c.more(addr, big)
		x.poll()
		sid, state := x.stray(order)
		if n := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='stray_refund'", order); n != 2 || state != "awaiting_signature" {
			t.Fatalf("%s: after a deposit more: %d stray refunds, the newest %s; events %q", c.what, n, state, x.events(order))
		}
		if rows := agStrings(t, x.testEnv, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1", sid); !slices.Equal(rows, []string{"claimant " + refund + " 104472"}) ||
			agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", sid) != 2 {
			t.Fatalf("%s: stray refund outputs %q", c.what, rows)
		}
	}
}
