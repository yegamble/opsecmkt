package market

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// Regression tests for the 2026-09-23 review findings (one test per finding).

// F1: a draft keeps the price it was created at; resubmitting checkout and requesting payment both use the
// current listing price.
func TestDraftRepricedOnResubmitAndPay(t *testing.T) {
	p := newPayEnv(t)
	draftAt := func() (string, int64) {
		w := p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"BTC"}})
		p.check(w, 303)
		id := strings.TrimPrefix(w.Header().Get("Location"), "/order?id=")
		var amt int64
		if err := p.DB.QueryRow("SELECT amount FROM orders WHERE id=$1", id).Scan(&amt); err != nil {
			t.Fatal(err)
		}
		return id, amt
	}
	id, amt := draftAt()
	if amt != 100000 {
		t.Fatalf("draft amount %d", amt)
	}
	p.DB.Exec("UPDATE products SET btc=1000000 WHERE id=$1", p.productID)
	if again, amt := draftAt(); again != id || amt != 1000000 {
		t.Fatalf("resubmitted draft %s amount %d, want same draft at 1000000", again[:8], amt)
	}
	// Price changes again without a resubmit: requesting payment records the current price.
	p.DB.Exec("UPDATE products SET btc=2000000 WHERE id=$1", p.productID)
	p.check(p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {id}}), 303)
	var st string
	p.DB.QueryRow("SELECT amount,state FROM orders WHERE id=$1", id).Scan(&amt, &st)
	if st != stateAwaitingPayment || amt != 2000000 {
		t.Fatalf("order %s at %d, listing price 2000000", st, amt)
	}
}

// F2: attacker-chosen rate-limit keys neither fill the shared table nor lock out real users.
func TestLimiterFloodDoesNotLockOutUsers(t *testing.T) {
	a := &App{limits: map[string]bucket{}}
	for i := range 4096 {
		a.allow("junk:"+strconv.Itoa(i), 10)
	}
	if !a.allow("fresh", 10) || len(a.limits) > 4096 {
		t.Fatalf("full table refused a new key (size %d)", len(a.limits))
	}

	e := newTestApp(t)
	_, sess := e.user("victim_user", "buyer")
	anon := randomToken()
	for i := range 4100 {
		pc := &http.Cookie{Name: "pending", Value: randomToken()}
		if w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}}, pc); w.Code != 401 {
			t.Fatalf("challenge flood: %d", w.Code)
		}
		if i >= 50 {
			continue
		}
		if w := e.do("POST", "/challenge/pgp", anon, nil, pc); w.Code != 401 {
			t.Fatalf("pgp code flood: %d", w.Code)
		}
		if w := e.do("POST", "/login", anon, url.Values{"handle": {"bad handle " + randomToken()[:8]}, "password": {testPassword}}); w.Code != 400 {
			t.Fatalf("handle flood: %d", w.Code)
		}
	}
	if n := len(e.A.limits); n > 10 {
		t.Fatalf("unauthenticated junk created %d limiter keys", n)
	}
	e.check(e.do("POST", "/login", randomToken(), url.Values{"handle": {"victim_user"}, "password": {testPassword}}), 303)
	e.check(e.do("POST", "/account", sess, url.Values{"xmpp": {"victim@example.test"}}), 303)
}

// F3a: a buyer may hold at most three orders awaiting payment.
func TestPayCapsOpenAwaitingPaymentOrders(t *testing.T) {
	p := newPayEnv(t) // one awaiting_payment order already
	p.newOrder(stateAwaitingPayment)
	p.newOrder(stateAwaitingPayment)
	w := p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"BTC"}})
	p.check(w, 303)
	id := strings.TrimPrefix(w.Header().Get("Location"), "/order?id=")
	w = p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {id}})
	p.check(w, 409)
	if !strings.Contains(w.Body.String(), "3 orders awaiting payment") {
		t.Fatalf("cap message: %q", w.Body.String())
	}
	if p.state(id) != stateDraft {
		t.Fatal("capped order left draft")
	}
	p.move(p.order, stateAwaitingPayment, stateCancelled, p.buyer)
	p.check(p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {id}}), 303)
}

// F3b: unpaid orders expire after PAYMENT_EXPIRY and return their stock; orders with a deposit in flight stay open.
func TestWatcherExpiresUnpaidOrders(t *testing.T) {
	for _, bad := range []string{"abc", "1m", "1000h"} {
		t.Setenv("PAYMENT_EXPIRY", bad)
		if _, err := paymentExpiry(); err == nil {
			t.Fatalf("PAYMENT_EXPIRY=%s accepted", bad)
		}
	}
	t.Setenv("PAYMENT_EXPIRY", "2h")
	p := newPayEnv(t)
	stock := func() int { return p.count("SELECT stock FROM products WHERE id=$1", p.productID) }
	before := stock()
	pending, pendingAddr := p.newOrder(stateAwaitingPayment)
	conflicted, conflictedAddr := p.newOrder(stateAwaitingPayment)
	fresh, _ := p.newOrder(stateAwaitingPayment)
	p.fake.Deposit(pendingAddr, "tx-slow", 0, 100000, 0)
	p.fake.Deposit(conflictedAddr, "tx-gone", 0, 100000, -1)
	p.DB.Exec("UPDATE payment_addresses SET created=now()-interval '3 hours' WHERE order_id<>$1", fresh)
	p.poll()
	p.poll()
	for id, want := range map[string]string{p.order: stateCancelled, conflicted: stateCancelled, pending: stateAwaitingPayment, fresh: stateAwaitingPayment} {
		if got := p.state(id); got != want {
			t.Errorf("order %s: %s want %s", id[:8], got, want)
		}
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND to_state='cancelled' AND actor_id IS NULL AND note='payment window expired'", p.order); n != 1 {
		t.Fatalf("expiry events: %d", n)
	}
	if got := stock(); got != before+2 {
		t.Fatalf("stock %d, want %d", got, before+2)
	}
	if n := p.count("SELECT count(*) FROM payouts"); n != 0 {
		t.Fatal("expired unpaid orders queued a payout")
	}
}

// F4: payout addresses and PGP sign-in need the current password (and a TOTP code when TOTP is on).
func TestSensitiveChangesNeedPassword(t *testing.T) {
	p := newPayEnv(t)
	payout := func(extra ...string) int {
		f := url.Values{"currency": {"BTC"}, "address": {"fake-testnet-mine"}}
		for i := 0; i < len(extra); i += 2 {
			f.Set(extra[i], extra[i+1])
		}
		return p.do("POST", "/account/payout", p.buyerSess, f).Code
	}
	if c := payout(); c != 400 {
		t.Fatalf("payout without password: %d", c)
	}
	if c := payout("password", "wrong-password-123"); c != 401 {
		t.Fatalf("payout with wrong password: %d", c)
	}
	if c := payout("password", testPassword); c != 303 {
		t.Fatalf("payout with password: %d", c)
	}
	sealed, _ := p.A.seal("totp", newTOTPSecret())
	p.DB.Exec("UPDATE users SET totp_secret=$1,totp_enabled=true WHERE id=$2", sealed, p.buyer.ID)
	if c := payout("password", testPassword); c != 401 {
		t.Fatalf("payout without TOTP code while enrolled: %d", c)
	}
	if body := p.page("/account", p.buyerSess); !strings.Contains(body, `name="password" required`) || !strings.Contains(body, "Authenticator code") {
		t.Fatal("payout form lacks the password and code fields")
	}
	code, _ := p.totpCodeFor(p.buyer.ID, 0)
	if c := payout("password", testPassword, "code", code); c != 303 {
		t.Fatalf("payout with password and code: %d", c)
	}

	// PGP sign-in on (verified key), no TOTP.
	_, alicePub := testPGPKey(t, "alice")
	_, bobPub := testPGPKey(t, "bob")
	_, fp, _ := parsePublicKey(alicePub)
	v, vs := p.vendor.ID, p.vendorSess
	p.DB.Exec("UPDATE users SET pgp=$1,pgp_fingerprint=$2,pgp_verified_at=now(),pgp_2fa=true WHERE id=$3", strings.TrimSpace(alicePub), fp, v)
	twoFA := func() bool { return p.count("SELECT count(*) FROM users WHERE id=$1 AND pgp_2fa", v) == 1 }
	if !strings.Contains(p.page("/pgp", vs), `autocomplete="current-password"`) || !strings.Contains(p.page("/account", vs), "pgp-confirm-help") {
		t.Fatal("PGP forms lack the password field")
	}
	p.check(p.do("POST", "/account", vs, url.Values{"pgp": {alicePub}, "xmpp": {"v@example.test"}}), 303) // key unchanged
	p.check(p.do("POST", "/account", vs, url.Values{"pgp": {bobPub}}), 400)
	p.check(p.do("POST", "/account", vs, url.Values{"pgp": {""}, "password": {"wrong-password-123"}}), 401)
	p.check(p.do("POST", "/pgp/2fa", vs, url.Values{"enable": {"0"}}), 400)
	p.check(p.do("POST", "/pgp/2fa", vs, url.Values{"enable": {"0"}, "password": {"wrong-password-123"}}), 401)
	if !twoFA() {
		t.Fatal("PGP sign-in turned off without the password")
	}
	p.check(p.do("POST", "/pgp/2fa", vs, url.Values{"enable": {"0"}, "password": {testPassword}}), 303)
	if twoFA() {
		t.Fatal("PGP sign-in still on")
	}
	p.DB.Exec("UPDATE users SET pgp_2fa=true WHERE id=$1", v)
	p.check(p.do("POST", "/account", vs, url.Values{"pgp": {bobPub}, "password": {testPassword}}), 303)
	if twoFA() {
		t.Fatal("key change kept PGP sign-in")
	}
	if !p.testEnv.auditHas(v, "confirmed with password") {
		t.Fatal("confirmation not audited")
	}
}

// F5: a deposit that confirms after a cancellation (and after the 24-hour window) is refunded.
func TestLateDepositAfterCloseIsHandled(t *testing.T) {
	p := newPayEnv(t)
	p.fake.Deposit(p.addr, "tx-slow", 0, 100000, 1)
	p.poll()
	p.move(p.order, stateAwaitingPayment, stateCancelled, p.buyer)
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 0 {
		t.Fatal("refund queued for an unconfirmed deposit")
	}
	p.DB.Exec("UPDATE orders SET updated=now()-interval '3 days' WHERE id=$1", p.order)
	p.fake.SetConfirmations("tx-slow", 3)
	p.poll()
	if st, _, _, amt := p.payout(p.order); st != "blocked" || amt != 100000 {
		t.Fatalf("late refund %s %d", st, amt)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%confirmed after this order was cancelled%'", p.buyer.ID); n != 1 {
		t.Fatalf("buyer late-refund notifications: %d", n)
	}
	p.check(p.do("POST", "/account/payout", p.buyerSess, url.Values{"currency": {"BTC"}, "address": {"fake-testnet-buyer"}, "password": {testPassword}}), 303)
	p.poll()
	if st, _, _, _ := p.payout(p.order); st != "sent" {
		t.Fatalf("late refund not sent: %s", st)
	}

	// Completed order: an extra deposit seen unconfirmed, confirming days later, is flagged, never paid.
	o2, a2 := p.newOrder(stateAwaitingPayment)
	p.paidByWatcher(o2, a2, "tx-main")
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.move(o2, statePaid, stateShipped, p.vendor)
	p.move(o2, stateShipped, stateCompleted, p.buyer)
	p.fake.Deposit(a2, "tx-extra", 0, 7000, 1)
	p.poll()
	p.DB.Exec("UPDATE orders SET updated=now()-interval '3 days' WHERE id=$1", o2)
	p.fake.SetConfirmations("tx-extra", 3)
	p.poll()
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%arrived after this order was settled%'", o2); n != 1 {
		t.Fatalf("extra deposit notes: %d", n)
	}
	sends := len(p.fake.Sends())
	// Outside the 30-day window nothing is polled any more.
	o3, a3 := p.newOrder(stateAwaitingPayment)
	p.fake.Deposit(a3, "tx-old", 0, 100000, 1)
	p.poll()
	p.move(o3, stateAwaitingPayment, stateCancelled, p.buyer)
	p.DB.Exec("UPDATE orders SET updated=now()-interval '40 days' WHERE id=$1", o3)
	p.DB.Exec("UPDATE payment_addresses SET created=now()-interval '40 days' WHERE order_id=$1", o3)
	p.fake.SetConfirmations("tx-old", 3)
	p.poll()
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", o3); n != 0 || len(p.fake.Sends()) != sends {
		t.Fatal("order outside the watch window was settled")
	}
}

// F6: a payout waits while a credited deposit is below the threshold (reorg) and resumes once it re-confirms.
func TestPayoutHeldWhileCreditedDepositReconfirms(t *testing.T) {
	p := newPayEnv(t)
	p.paidByWatcher(p.order, p.addr, "tx-reorg")
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	p.fake.SetConfirmations("tx-reorg", 0)
	p.poll()
	if st, _, _, _ := p.payout(p.order); st != "held" || len(p.fake.Sends()) != 0 {
		t.Fatalf("payout during reorg: %s sends=%d", st, len(p.fake.Sends()))
	}
	p.fake.SetConfirmations("tx-reorg", 3)
	p.poll()
	if st, _, _, _ := p.payout(p.order); st != "sent" || len(p.fake.Sends()) != 1 {
		t.Fatalf("payout after re-confirmation: %s sends=%d", st, len(p.fake.Sends()))
	}
}

// F7: Monero transfers with a non-zero unlock_time are reported but never credited.
func TestMoneroLockedTransfersAreNotCredited(t *testing.T) {
	ctx := context.Background()
	w, s := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	mp, err := newMoneroProvider(ctx, s.url(""), "", "stagenet", 10)
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := mp.NewAddress(ctx, "order-locked")
	w.transfers = map[string]any{"in": []map[string]any{
		{"txid": "in-locked", "amount": json.Number("500000000000"), "confirmations": 12, "unlock_time": 3000000, "subaddr_index": map[string]any{"major": 0, "minor": 1}},
		{"txid": "in-free", "amount": json.Number("5"), "confirmations": 12, "unlock_time": 0, "subaddr_index": map[string]any{"major": 0, "minor": 1}},
	}}
	in, err := mp.Incoming(ctx, []string{addr})
	if err != nil || len(in) != 2 || !in[0].Locked || in[1].Locked {
		t.Fatalf("incoming %+v %v", in, err)
	}

	p := newPayEnv(t)
	p.fake.mu.Lock()
	p.fake.outputs = append(p.fake.outputs, Incoming{Address: p.addr, TxID: "tx-locked", Amount: 100000, Confirmations: 5, Locked: true})
	p.fake.mu.Unlock()
	p.poll()
	p.poll()
	if st := p.state(p.order); st != stateAwaitingPayment {
		t.Fatalf("locked transfer paid the order: %s", st)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%locked transfer ignored%'", p.order); n != 1 {
		t.Fatalf("locked notes: %d", n)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%locked transfer ignored%'", p.buyer.ID); n != 1 {
		t.Fatalf("buyer locked notifications: %d", n)
	}
	if body := p.page("/order?id="+p.order, p.buyerSess); !strings.Contains(body, "Locked transfer — not counted") {
		t.Fatal("order page does not show the locked transfer")
	}
	p.move(p.order, stateAwaitingPayment, stateCancelled, p.buyer)
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 0 {
		t.Fatal("locked transfer refunded automatically")
	}
}

// F8: an upgraded account whose stored key no longer parses can still save its profile unchanged.
func TestLegacyUnparseableKeySavesUnchanged(t *testing.T) {
	e := newTestApp(t)
	uid, s := e.user("legacy_user", "buyer")
	const legacy = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nlegacy key the parser rejects\n-----END PGP PUBLIC KEY BLOCK-----"
	e.DB.Exec("UPDATE users SET pgp=$1 WHERE id=$2", legacy, uid)
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {legacy}, "xmpp": {"legacy@example.test"}}), 303)
	var xmpp string
	e.DB.QueryRow("SELECT xmpp FROM users WHERE id=$1", uid).Scan(&xmpp)
	if xmpp != "legacy@example.test" {
		t.Fatal("profile not saved")
	}
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {legacy + "\nchanged"}}), 400)
}
