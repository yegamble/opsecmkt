package market

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// A-636: the Monero escrow refusals A-565 left without a Monero test once the Bitcoin tests were removed: the
// awaiting-payment cap, the settling forms' missing tick or amount (400) and changed amount (409), /resolve without a
// password or with an authenticator code, and /resolve by an administrator who is party to the order. DB integration
// tests against the simulated Monero wallet-rpc of escrow_xmr_fake_test.go: simulated RPC, not chain evidence.

// footprint is everything a refused order action must leave as it was: the order's state, history, escrow, slot,
// settlements and dispute, and the market's audit rows, notifications and wallet-rpc calls.
func (x *xmrEscrowEnv) footprint(order string) string {
	x.t.Helper()
	return agStr(x.testEnv, `SELECT concat_ws(' | ',
		(SELECT state FROM orders WHERE id=$1),
		(SELECT count(*) FROM order_events WHERE order_id=$1),
		(SELECT count(*) FROM escrows WHERE order_id=$1),
		(SELECT count(*) FROM xmr_escrow_slots WHERE order_id=$1),
		(SELECT count(*) FROM xmr_settlements WHERE order_id=$1),
		(SELECT string_agg(status||'/'||outcome||'/'||resolution, ',') FROM disputes WHERE order_id=$1),
		(SELECT count(*) FROM audit_events),
		(SELECT count(*) FROM notifications))`, order) + " | rpc " + strconv.Itoa(x.fake.count())
}

// refused posts form to path as sess, requires code and a refusal containing msg, and requires the order's footprint
// unchanged.
func (x *xmrEscrowEnv) refused(order, path, sess string, form url.Values, code int, msg string) {
	x.t.Helper()
	before := x.footprint(order)
	got, body := x.xmrPost(path, sess, form)
	if got != code || !strings.Contains(body, msg) {
		x.t.Fatalf("%s: %d %q, want %d %q", path, got, body, code, msg)
	}
	if after := x.footprint(order); after != before {
		x.t.Fatalf("%s refused %d but changed the order: %s -> %s", path, got, before, after)
	}
}

// A fourth Monero escrow checkout while the buyer has three orders awaiting payment is refused 409, with no slot
// claimed, no escrow written, the draft left a draft and no wallet-rpc call; cancelling one of the three lets it through.
func TestXMREscrowAwaitingPaymentCap(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	agExec(x.testEnv, "UPDATE products SET stock=100 WHERE id=$1", x.product)
	var open []string
	for range maxAwaitingPayment {
		order := x.draft(x.product)
		x.mustCheckout(order)
		x.mustPoll() // keep ESCROW_XMR_SLOTS free slots
		open = append(open, order)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM orders WHERE buyer_id=$1 AND state='awaiting_payment'", x.buyer); n != maxAwaitingPayment {
		t.Fatalf("%d orders awaiting payment, want %d", n, maxAwaitingPayment)
	}
	fourth := x.draft(x.product)
	free := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE state='free'")
	refund := url.Values{"order_id": {fourth}, "refund_address": {testXMRAddrOn(x.refundNet(), "refund "+fourth, true)}}
	x.refused(fourth, "/orders/escrow-xmr", x.buyerSess, refund, 409, "You already have 3 orders awaiting payment")
	if x.state(fourth) != stateDraft || agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE state='free'") != free ||
		agInt(x.testEnv, "SELECT count(*) FROM escrow_external_addresses WHERE address=$1", refund.Get("refund_address")) != 0 {
		t.Fatal("the refused checkout claimed a slot, reserved its refund address or moved the draft")
	}
	if code, body := x.xmrPost("/orders/cancel", x.buyerSess, url.Values{"order_id": {open[0]}, "from": {stateAwaitingPayment}}); code != 303 {
		t.Fatalf("cancel an unfunded order awaiting payment: %d %s", code, body)
	}
	if code, body := x.xmrPost("/orders/escrow-xmr", x.buyerSess, refund); code != 303 || x.state(fourth) != stateAwaitingPayment {
		t.Fatalf("checkout under the cap: %d %s (%s)", code, body, x.state(fourth))
	}
}

// Every Monero action that settles an escrow's coins needs the ticked confirmation and the amount the form stated: a
// missing tick or an unreadable amount is 400, an amount that changed since the page was shown is 409, and none writes
// anything. The same forms with the tick and the right amount then succeed, so each refusal is the field's.
func TestXMRSettlingFormsNeedTickAndAmount(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, adminSess := x.user("xtick_admin_"+randomToken()[:6], "admin")
	physical := x.physical(x.vendor)
	agExec(x.testEnv, "UPDATE products SET stock=100 WHERE id=$1", physical)
	const tick, unread, changed = "Tick the box confirming the amount", "The form did not say which amount you reviewed", "The amount changed to"
	price := strconv.FormatInt(xmrListingPrice, 10)
	disputed := func() (string, string) {
		order, sess := x.undeliveredXMR()
		if code, body := x.xmrDispute(order, sess); code != 303 {
			t.Fatalf("dispute: %d %s", code, body)
		}
		return order, agStr(x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)
	}
	for _, c := range []struct {
		name  string
		setup func() (order, sess string, form url.Values) // a form complete but for the confirmation fields
		after string                                       // the order's state once the confirmed form succeeds
	}{
		{"administrator's decision", func() (string, string, url.Values) {
			order, dispute := disputed()
			return order, adminSess, url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"Decided from the evidence both parties sent."},
				"reply_seen": {"0"}, "password": {testPassword}}
		}, stateResolved},
		{"buyer's concession", func() (string, string, url.Values) {
			order, dispute := disputed()
			return order, x.sessOf(order, roleBuyer), url.Values{"id": {dispute}, "outcome": {"release"}, "password": {testPassword}}
		}, stateCompleted},
		{"vendor's concession", func() (string, string, url.Values) {
			order, dispute := disputed()
			return order, x.sessOf(order, roleVendor), url.Values{"id": {dispute}, "outcome": {"refund"}, "password": {testPassword}}
		}, stateCancelled},
		{"vendor's cancel of a paid order", func() (string, string, url.Values) {
			order, _ := x.paidXMRPhysical(physical)
			return order, x.sessOf(order, roleVendor), url.Values{"order_id": {order}, "from": {statePaid}}
		}, stateCancelled},
		{"buyer's no-ship cancel", func() (string, string, url.Values) {
			order, sess := x.paidXMRPhysical(physical)
			x.forceDeadline(order, "ship_by=now()-interval '1 minute'")
			return order, sess, url.Values{"order_id": {order}, "from": {statePaid}}
		}, stateCancelled},
		{"cancel of an unpaid escrow with a deposit", func() (string, string, url.Values) {
			order, addr := x.freshCheckoutOf(physical, 2)
			x.fake.payAddress(addr, hex64("part "+order), xmrListingPrice, 10) // one unit's price: an underpayment, it stays awaiting payment
			x.mustPoll()
			if x.state(order) != stateAwaitingPayment {
				t.Fatalf("underpaid order is %s", x.state(order))
			}
			return order, x.sessOf(order, roleBuyer), url.Values{"order_id": {order}, "from": {stateAwaitingPayment}}
		}, stateCancelled},
	} {
		t.Run(c.name, func(t *testing.T) {
			order, sess, form := c.setup()
			with := func(kv ...string) url.Values {
				f := url.Values{}
				for k, v := range form {
					f[k] = v
				}
				for i := 0; i+1 < len(kv); i += 2 {
					f.Set(kv[i], kv[i+1])
				}
				return f
			}
			path := "/orders/cancel"
			if form.Has("id") {
				path = "/disputes/concede"
				if form.Has("resolution") {
					path = "/resolve"
				}
			}
			x.refused(order, path, sess, with("amount_seen", price), 400, tick)
			x.refused(order, path, sess, with("payout_confirmed", "yes", "amount_seen", price), 400, tick)
			if form.Get("from") == stateAwaitingPayment {
				// A deposit that arrived after the page was shown: the form had no amount to state, so it is stale (409).
				x.refused(order, path, sess, with("payout_confirmed", "confirmed"), 409, "A deposit arrived for this order after the page was shown")
			} else {
				x.refused(order, path, sess, with("payout_confirmed", "confirmed"), 400, unread)
			}
			x.refused(order, path, sess, with("payout_confirmed", "confirmed", "amount_seen", "half"), 400, unread)
			x.refused(order, path, sess, with("payout_confirmed", "confirmed", "amount_seen", "-1"), 400, unread)
			x.refused(order, path, sess, with("payout_confirmed", "confirmed", "amount_seen", strconv.FormatInt(xmrListingPrice-1, 10)), 409, changed)
			if code, body := x.xmrPost(path, sess, with("payout_confirmed", "confirmed", "amount_seen", price)); code != 303 || x.state(order) != c.after {
				t.Fatalf("the confirmed form: %d %s (%s, want %s)", code, body, x.state(order), c.after)
			}
		})
	}
}

// /resolve on a Monero escrow dispute needs the administrator's current password (400 without it), and an
// authenticator code once one is enrolled: without a code, with a wrong one or with one already used it is 401, and
// nothing is written (order, dispute, settlements, history, audit rows, notifications). The right code resolves it and
// the audit row says how it was confirmed.
func TestXMRResolveNeedsPasswordAndCode(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	adminID, admin := x.user("xresolve_admin_"+randomToken()[:6], "admin")
	form := func(dispute string, kv ...string) url.Values {
		f := resolveConfirmed(url.Values{"id": {dispute}, "outcome": {"refund"}, "reply_seen": {"0"}, "resolution": {"Decided from the evidence both parties sent."}}, xmrListingPrice)
		for i := 0; i+1 < len(kv); i += 2 {
			f.Set(kv[i], kv[i+1])
		}
		return f
	}
	order, dispute := budgetDispute(x)
	x.refused(order, "/resolve", admin, form(dispute, "password", ""), 400, "Enter your current password")

	key, _ := agEnableTOTP(x.testEnv, adminID)
	const codeWrong = "Verification code incorrect, expired or already used"
	x.refused(order, "/resolve", admin, form(dispute), 401, codeWrong)
	x.refused(order, "/resolve", admin, form(dispute, "code", x.wrongTOTP(adminID)), 401, codeWrong)
	code := agTOTPNow(key)
	if got, body := x.xmrPost("/resolve", admin, form(dispute, "code", code)); got != 303 || x.state(order) != stateResolved {
		t.Fatalf("resolve with the password and a current code: %d %s (%s)", got, body, x.state(order))
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", adminID,
		"Resolved dispute on order "+shortID(order)+" (outcome: refund) (confirmed with password and authenticator code)"); n != 1 {
		t.Fatalf("%d audit rows recording the code confirmation", n)
	}
	// The same code again, on another dispute: it is spent.
	second, secondDispute := budgetDispute(x)
	x.refused(second, "/resolve", admin, form(secondDispute, "code", code), 401, codeWrong)
}

// An administrator who is party to a Monero escrow order (its buyer or its vendor) cannot decide its dispute: /resolve
// is refused 403 for either outcome with nothing written, while an independent administrator decides it. /resolve with
// no outcome is refused 400.
func TestXMRResolveRefusedToPartyAdministrator(t *testing.T) {
	w := newDisputeOrderWorld(t)
	_, independent := w.e.user("party_resolve_admin_"+randomToken()[:6], "admin")
	for _, party := range []string{roleBuyer, roleVendor} {
		t.Run("the "+party, func(t *testing.T) {
			order, dispute := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "The licence key was never delivered to me.")
			seen := resolverSeen(w, order)
			partyID := w.x.partyOf(order, party)
			agExec(w.e, "UPDATE users SET role='admin',pgp_2fa=true WHERE id=$1", partyID)
			defer agExec(w.e, "UPDATE users SET role=$2,pgp_2fa=false WHERE id=$1", partyID, party)
			sess := w.x.sessOf(order, party)
			for _, outcome := range []string{"release", "refund"} {
				w.x.refused(order, "/resolve", sess, resolverForm(dispute, outcome, seen), 403, "You cannot make that change to this order")
			}
			// Each guard alone, as TestXMREscrowModeratorCannotDecideDispute calls them: the receipt guard every
			// /resolve runs first, and the action (transition's party rule) without it.
			tx, err := w.e.DB.Begin()
			if err != nil {
				t.Fatal(err)
			}
			err = resolveReceiptAuthorize(&actionCtx{A: w.e.A, Tx: tx, R: httptest.NewRequest("POST", "/resolve", nil), User: w.e.loadUser(partyID), Form: url.Values{"id": {dispute}}})
			_ = tx.Rollback()
			if err != errForbidden {
				t.Fatalf("resolveReceiptAuthorize for the party administrator: %v", err)
			}
			before := w.x.footprint(order)
			if _, err = resolveAction(&actionCtx{A: w.e.A, R: httptest.NewRequest("POST", "/resolve", nil), User: w.e.loadUser(partyID), Token: sess,
				Form: resolverForm(dispute, "refund", seen)}); err != errForbidden {
				t.Fatalf("resolveAction for the party administrator: %v", err)
			}
			if after := w.x.footprint(order); after != before {
				t.Fatalf("resolveAction for the party administrator changed the order: %s -> %s", before, after)
			}
			w.x.refused(order, "/resolve", independent, resolverForm(dispute, "", seen), 400, "Choose an outcome")
			if code, body := w.x.xmrPost("/resolve", independent, resolverForm(dispute, "refund", seen)); code != 303 || w.state(order) != stateResolved {
				t.Fatalf("the independent administrator: %d %s (%s)", code, body, w.state(order))
			}
		})
	}
}
