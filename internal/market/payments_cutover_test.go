package market

import (
	"context"
	"errors"
	"html"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// A-444 (P4-R8): the cutover lever. payment_intake_<COIN> takes a third value, 'escrow': pooled payment requests are
// refused (lockPaymentIntake with kindPooled, and the draft -> awaiting_payment guard in transition), escrow checkouts
// are admitted, and the administrator's form sets it with or without a pooled wallet. The readiness panel on /admin
// and the "not available for purchase" reason on listings. DB integration tests with the fake pooled provider and the
// simulated escrow nodes (simulated RPC, not chain evidence).

// setIntake posts the administrator's intake form.
func setIntake(e *testEnv, session, cur, value, password string) *httptest.ResponseRecorder {
	return e.do("POST", "/admin/payment-intake", session, url.Values{"currency": {cur}, "enabled": {value}, "password": {password}})
}

// lockIntakeAs runs lockPaymentIntake for currency as kind in a transaction of its own and rolls it back.
func lockIntakeAs(t *testing.T, e *testEnv, currency string, kind intakeKind) error {
	t.Helper()
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	return lockPaymentIntake(&actionCtx{A: e.A, R: httptest.NewRequest("POST", "/", nil), Tx: tx}, currency, kind)
}

// draftToAwaiting runs transition's draft -> awaiting_payment for order as buyer, as an escrow checkout when escrow is
// set, and rolls it back.
func draftToAwaiting(t *testing.T, e *testEnv, order string, buyer *User, escrow bool) error {
	t.Helper()
	ctx := context.Background()
	if escrow {
		ctx = context.WithValue(ctx, escrowCheckoutCtx{}, true)
	}
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = e.A.transition(ctx, tx, order, stateDraft, stateAwaitingPayment, buyer, "test")
	return err
}

// countingProvider counts the addresses the wallet is asked for.
type countingProvider struct {
	*fakeProvider
	calls atomic.Int32
}

func (p *countingProvider) NewAddress(ctx context.Context, order string) (string, error) {
	p.calls.Add(1)
	return p.fakeProvider.NewAddress(ctx, order)
}

func httpCode(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.Code
	}
	return 0
}

// lockPaymentIntake reads the setting for each kind of caller: 'true' admits both, 'escrow' only escrow checkouts,
// 'false' and any other value (what an older binary does with 'escrow') neither.
func TestPaymentIntakeLockByKind(t *testing.T) {
	e := newTestApp(t)
	for _, tc := range []struct {
		value          string
		pooled, escrow int // 0 = admitted, else the refusal's status
	}{
		{"true", 0, 0},
		{"escrow", 409, 0},
		{"false", 409, 409},
		{"later-value", 409, 409},
		{"", 409, 409},
	} {
		agExec(e, "UPDATE settings SET value=$1 WHERE key='payment_intake_BTC'", tc.value)
		pooled, escrow := lockIntakeAs(t, e, "BTC", kindPooled), lockIntakeAs(t, e, "BTC", kindEscrow)
		if httpCode(pooled) != tc.pooled || (pooled != nil && tc.pooled == 0) || httpCode(escrow) != tc.escrow || (escrow != nil && tc.escrow == 0) {
			t.Fatalf("intake %q: pooled %v, escrow %v", tc.value, pooled, escrow)
		}
		if tc.value == "escrow" && pooled.Error() != escrowOnlyRefusal("BTC") {
			t.Fatalf("pooled refusal under escrow only: %q", pooled.Error())
		}
	}
	if err := lockIntakeAs(t, e, "XMR", kindPooled); err != nil {
		t.Fatalf("XMR intake changed with BTC's: %v", err)
	}
}

// With 'escrow' a pooled payment request is refused before any wallet call (409, the draft unchanged, no
// payment_addresses row, no stock reserved), and so is the bare transition; an escrow checkout's transition is let
// through. An order whose pooled address was issued before the flip is still credited and paid out. The form needs
// the administrator's password, audits each change and is reversible.
func TestPaymentIntakeEscrowOnlyRefusesPooled(t *testing.T) {
	p := newPayEnv(t)
	value := func() string { return p.str("SELECT value FROM settings WHERE key='payment_intake_BTC'") }
	if setIntake(p.testEnv, p.buyerSess, "BTC", "escrow", testPassword).Code != 403 ||
		setIntake(p.testEnv, p.adminSess, "BTC", "escrow", "wrong-password").Code != 401 ||
		setIntake(p.testEnv, p.adminSess, "BTC", "sideways", testPassword).Code != 400 || value() != "true" {
		t.Fatal("an intake change was not refused without the administrator's role, password and a known value")
	}
	opened := p.order // awaiting payment at a pooled address issued before the flip
	w := setIntake(p.testEnv, p.adminSess, "BTC", "escrow", testPassword)
	if w.Code != 303 || value() != "escrow" || p.cookie(w, "notice") != "intake-escrow" {
		t.Fatalf("escrow only: %d %q %q", w.Code, value(), p.cookie(w, "notice"))
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Set new BTC payments to escrow only%'"); n != 1 {
		t.Fatalf("%d audit rows for escrow only", n)
	}
	stock := p.count("SELECT stock FROM products WHERE id=$1", p.productID)
	addresses := p.count("SELECT count(*) FROM payment_addresses")
	counting := &countingProvider{fakeProvider: p.fake}
	p.A.payments["BTC"] = counting
	draft := p.testEnv.order(p.buyer.ID, p.productID, "BTC", stateDraft)
	w = p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {draft}})
	if w.Code != 409 || !strings.Contains(html.UnescapeString(w.Body.String()), escrowOnlyRefusal("BTC")) {
		t.Fatalf("pooled request under escrow only: %d %s", w.Code, w.Body.String())
	}
	if p.state(draft) != stateDraft || p.count("SELECT count(*) FROM payment_addresses") != addresses ||
		p.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", draft) != 0 ||
		p.count("SELECT stock FROM products WHERE id=$1", p.productID) != stock || counting.calls.Load() != 0 {
		t.Fatal("a refused pooled request changed the order, stock or addresses, or asked the wallet")
	}
	if err := draftToAwaiting(t, p.testEnv, draft, p.buyer, false); httpCode(err) != 409 || err.Error() != escrowOnlyRefusal("BTC") {
		t.Fatalf("transition let a pooled request through under escrow only: %v", err)
	}
	if err := draftToAwaiting(t, p.testEnv, draft, p.buyer, true); err != nil {
		t.Fatalf("transition refused an escrow checkout under escrow only: %v", err)
	}
	page := html.UnescapeString(p.page("/order?id="+draft, p.buyerSess))
	if !strings.Contains(page, "New payments use escrow only") || strings.Contains(page, `action="/orders/pay"`) {
		t.Fatal("the draft page offers a pooled payment request under escrow only")
	}
	if admin := html.UnescapeString(p.page("/admin", p.adminSess)); !strings.Contains(admin, "BTC intake: Escrow only") {
		t.Fatal("the admin page does not show escrow only")
	}

	// The pooled order opened before the flip: the watcher still reads its deposit and the payout is sent.
	p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"BTC"}, "address": {"fake-testnet-vendor"}, "password": {testPassword}}), 303)
	p.fake.Deposit(p.addr, "tx-before-flip", 0, 100000, 3)
	p.poll()
	p.complete(opened)
	p.poll()
	if state, _, _, _ := p.payout(opened); state != "sent" || len(p.fake.Sends()) != 1 {
		t.Fatalf("pooled order opened before the flip: payout %s, %d sends", state, len(p.fake.Sends()))
	}

	// Reversible: paused, then on again, each audited; on admits the pooled request.
	for _, v := range []string{"false", "true"} {
		if setIntake(p.testEnv, p.adminSess, "BTC", v, testPassword).Code != 303 || value() != v {
			t.Fatalf("could not set intake %q", v)
		}
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE '%new BTC payment%'"); n != 3 {
		t.Fatalf("%d intake audit rows, want 3", n)
	}
	p.check(p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {draft}}), 303)
}

// With no pooled provider configured the form still sets escrow only and paused (not on), and an escrow checkout
// succeeds under escrow only and is refused while paused; a pooled request is refused with escrow only's reason.
func TestEscrowOnlyIntakeWithoutPooledWallet(t *testing.T) {
	x := newEscrowEnv(t) // BITCOIN_RPC_URL and MONERO_WALLET_RPC_URL blank: no pooled provider
	_, admin := x.user("cutadmin_"+randomToken()[:6], "admin")
	value := func(cur string) string {
		return agStr(x.testEnv, "SELECT value FROM settings WHERE key=$1", paymentIntakeKey(cur))
	}
	page := html.UnescapeString(x.page("/admin", admin))
	if !strings.Contains(page, `value="escrow"`) || !strings.Contains(page, `value="false"`) || strings.Contains(page, `name="enabled" value="true"`) {
		t.Fatal("the intake form without a pooled wallet does not offer escrow only and paused (and only those)")
	}
	for _, cur := range []string{"BTC", "XMR"} {
		if w := setIntake(x.testEnv, admin, cur, "true", testPassword); w.Code != 409 || value(cur) != "true" {
			t.Fatalf("%s on without a pooled wallet: %d", cur, w.Code)
		}
		for _, v := range []string{"false", "escrow"} {
			if w := setIntake(x.testEnv, admin, cur, v, testPassword); w.Code != 303 || value(cur) != v {
				t.Fatalf("%s %q without a pooled wallet: %d %s", cur, v, w.Code, w.Body.String())
			}
		}
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE action LIKE '%new ___ payment%'"); n != 4 {
		t.Fatalf("%d intake audit rows, want 4", n)
	}
	if w := x.do("POST", "/orders/pay", x.buyerSess, url.Values{"order_id": {x.order}}); w.Code != 409 || !strings.Contains(html.UnescapeString(w.Body.String()), escrowOnlyRefusal("BTC")) {
		t.Fatalf("pooled request: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(x.order)
	agExec(x.testEnv, "UPDATE settings SET value='false' WHERE key='payment_intake_BTC'")
	if w := x.checkout(x.order, "bcrt1qbuyerrefund00000000000000000000001"); w.Code != 409 {
		t.Fatalf("escrow checkout while paused: %d", w.Code)
	}
	x.nothingWritten(x.order)
	agExec(x.testEnv, "UPDATE settings SET value='escrow' WHERE key='payment_intake_BTC'")
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	if x.state(x.order) != stateAwaitingPayment {
		t.Fatal("escrow checkout under escrow only did not move the order")
	}
}

// Monero escrow checkout is admitted under escrow only too.
func TestXMREscrowCheckoutUnderEscrowOnly(t *testing.T) {
	x := newXMREscrowEnv(t)
	agExec(x.testEnv, "UPDATE settings SET value='escrow' WHERE key='payment_intake_XMR'")
	x.mustPoll()
	order := x.draft(x.product)
	if w := x.do("POST", "/orders/pay", x.buyerSess, url.Values{"order_id": {order}}); w.Code != 409 {
		t.Fatalf("pooled XMR request under escrow only: %d", w.Code)
	}
	x.mustCheckout(order)
}

var readinessCount = regexp.MustCompile(`id="(escrow-readiness-[a-z]+)"><strong>(\d+)</strong>`)

// readinessCounts reads the panel's counts by element id.
func readinessCounts(page string) map[string]int {
	out := map[string]int{}
	for _, m := range readinessCount.FindAllStringSubmatch(page, -1) {
		out[m[1]], _ = strconv.Atoi(m[2])
	}
	return out
}

// The readiness panel's counts equal what escrowEligible and SQL say over the active listings: listings that cannot
// use escrow, and vendors with active listings but no active Bitcoin escrow key. It lists them, and says per coin
// whether escrow is on.
func TestEscrowReadinessPanelCounts(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("readyadmin_"+randomToken()[:6], "admin")
	other, _ := x.user("nokeyvendor_"+randomToken()[:6], "vendor")
	inactive, _ := x.user("inactivevendor_"+randomToken()[:6], "vendor")
	archivedOnly, _ := x.user("archivedvendor_"+randomToken()[:6], "vendor")
	x.activeKey(inactive, altEscrowKey(t, testBIP48Key, "1a2b3c4d", 2))
	agExec(x.testEnv, "UPDATE user_escrow_keys SET active_at=NULL WHERE user_id=$1", inactive)
	ineligible := map[string]bool{}
	digital := func(vendor, content string) string {
		id := x.testEnv.product(vendor, "digital")
		agExec(x.testEnv, "UPDATE products SET delivery_content=$2 WHERE id=$1", id, content)
		return id
	}
	physical := func(vendor string, dispatch int) string {
		id := x.testEnv.product(vendor, "physical")
		if dispatch > 0 {
			agExec(x.testEnv, "UPDATE products SET dispatch_days=$2 WHERE id=$1", id, dispatch)
		}
		return id
	}
	digital(x.vendor, "key 1")
	physical(x.vendor, 3)
	ineligible[digital(x.vendor, "")] = true
	ineligible[digital(other, "　 \n ")] = true // only Unicode white space: no automatic delivery
	ineligible[digital(other, strings.Repeat("x", maxDeliveryChars+1))] = true
	digital(other, strings.Repeat("y", maxDeliveryChars))
	ineligible[physical(other, 0)] = true
	ineligible[x.testEnv.product(inactive, "service")] = true
	gone := digital(archivedOnly, "")
	agExec(x.testEnv, "UPDATE products SET archived=true WHERE id=$1", gone)
	down := digital(x.vendor, "")
	agExec(x.testEnv, "UPDATE products SET archived=true, admin_archive_reason='test takedown' WHERE id=$1", down)

	// escrowEligible is the truth for each active listing.
	want := 0
	for _, id := range agStrings(t, x.testEnv, "SELECT id FROM products WHERE NOT archived") {
		var kind string
		if err := x.DB.QueryRow("SELECT kind FROM products WHERE id=$1", id).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		ok, _, _, err := escrowEligible(context.Background(), x.DB, &Order{ProductID: id, Kind: kind, Currency: "BTC"})
		if err != nil {
			t.Fatal(err)
		}
		if ok == ineligible[id] {
			t.Fatalf("fixture %s: escrowEligible %v", id[:8], ok)
		}
		if !ok {
			want++
		}
	}
	noKey := agInt(x.testEnv, `SELECT count(DISTINCT p.vendor_id) FROM products p WHERE NOT p.archived
		AND NOT EXISTS(SELECT 1 FROM user_escrow_keys k WHERE k.user_id=p.vendor_id AND k.active_at IS NOT NULL)`)
	if want != 5 || noKey != 2 {
		t.Fatalf("fixture: %d ineligible, %d vendors without a key", want, noKey)
	}
	page := html.UnescapeString(x.page("/admin", admin))
	got := readinessCounts(page)
	if got["escrow-readiness-listings"] != want || got["escrow-readiness-vendors"] != noKey {
		t.Fatalf("panel counts %v, want %d listings and %d vendors", got, want, noKey)
	}
	panel := page[strings.Index(page, `id="escrow-readiness"`):]
	for id := range ineligible {
		if !strings.Contains(panel, "/product?id="+id) {
			t.Fatalf("ineligible listing %s not listed", id[:8])
		}
	}
	for _, s := range []string{"no automatic delivery content", "no dispatch window", "service listing", "nokeyvendor_", "inactivevendor_",
		"BTC escrow: on · ", "XMR escrow: off · "} {
		if !strings.Contains(panel, s) {
			t.Fatalf("panel lacks %q", s)
		}
	}
	if strings.Contains(panel, "archivedvendor_") || strings.Contains(panel, "/product?id="+gone) || strings.Contains(panel, "/product?id="+down) {
		t.Fatal("the panel counts archived or taken-down listings")
	}
	// Bitcoin escrow on but not ready (its checks not passed): the line says so instead of a plain "on" (QA4W4B-1).
	ready := x.A.escrow.Load()
	x.A.escrow.Store(nil)
	notReady := html.UnescapeString(x.page("/admin", admin))
	x.A.escrow.Store(ready)
	if !strings.Contains(notReady, "BTC escrow: on (not ready: its startup checks have not passed; the server log names which) · ") {
		t.Fatal("the readiness panel says plain on while Bitcoin escrow is not ready")
	}
}

// A listing that cannot use escrow, while its coin is escrow only, says so on its page with the reason and offers
// no buy form; its vendor also reads how to fix it. Draft creation in that coin is refused. With one coin escrow
// only and the other on, the listing can still be bought in the other coin.
func TestEscrowOnlyListingNotAvailable(t *testing.T) {
	x := newEscrowEnv(t)
	plain := x.testEnv.product(x.vendor, "digital") // no delivery content: cannot use escrow
	buyHref := `href="/checkout?id=` + plain + `"`
	agExec(x.testEnv, "UPDATE settings SET value='escrow' WHERE key IN ('payment_intake_BTC','payment_intake_XMR')")
	buyer := html.UnescapeString(x.page("/product?id="+plain, x.buyerSess))
	if !strings.Contains(buyer, "Not available for purchase") || !strings.Contains(buyer, "no automatic delivery content") || strings.Contains(buyer, buyHref) {
		t.Fatalf("buyer's view of an ineligible listing under escrow only:\n%s", buyer)
	}
	if strings.Contains(buyer, "Edit listing to add delivery content") {
		t.Fatal("a buyer was shown the vendor's fix")
	}
	vendor := html.UnescapeString(x.page("/product?id="+plain, x.vendorSess))
	if !strings.Contains(vendor, "Not available for purchase") || !strings.Contains(vendor, "Edit listing to add delivery content") {
		t.Fatal("the vendor's listing page does not say why it cannot be bought")
	}
	checkout := html.UnescapeString(x.page("/checkout?id="+plain, x.buyerSess))
	if !strings.Contains(checkout, "Not available for purchase") || strings.Contains(checkout, `action="/orders"`) {
		t.Fatal("the checkout page offers a draft for an ineligible listing under escrow only")
	}
	for _, cur := range []string{"BTC", "XMR"} {
		before := agInt(x.testEnv, "SELECT count(*) FROM orders WHERE product_id=$1", plain)
		w := x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {plain}, "currency": {cur}})
		if w.Code != 409 || !strings.Contains(html.UnescapeString(w.Body.String()), "Not available for purchase in "+cur) || agInt(x.testEnv, "SELECT count(*) FROM orders WHERE product_id=$1", plain) != before {
			t.Fatalf("%s draft of an ineligible listing: %d %s", cur, w.Code, w.Body.String())
		}
	}
	// The eligible listing keeps its buy link under escrow only (Bitcoin escrow on; Monero escrow off on this market,
	// so only BTC is offered).
	ok := html.UnescapeString(x.page("/product?id="+x.product, x.buyerSess))
	if !strings.Contains(ok, `href="/checkout?id=`+x.product+`"`) {
		t.Fatal("an eligible listing lost its buy link under escrow only")
	}
	if c := html.UnescapeString(x.page("/checkout?id="+x.product, x.buyerSess)); !strings.Contains(c, `<option value="BTC">`) || strings.Contains(c, `<option value="XMR">`) {
		t.Fatal("checkout under escrow only offers a coin whose escrow is off, or not the one that is on")
	}
	// BTC escrow only, XMR on: still purchasable in XMR, with the BTC reason shown.
	agExec(x.testEnv, "UPDATE settings SET value='true' WHERE key='payment_intake_XMR'")
	mixed := html.UnescapeString(x.page("/product?id="+plain, x.buyerSess))
	if !strings.Contains(mixed, buyHref) || !strings.Contains(mixed, "Not available for purchase in BTC") {
		t.Fatal("one escrow-only coin hid a listing still purchasable in the other")
	}
	if c := html.UnescapeString(x.page("/checkout?id="+plain, x.buyerSess)); strings.Contains(c, `<option value="BTC">`) || !strings.Contains(c, `<option value="XMR">`) {
		t.Fatal("checkout offers the escrow-only coin the listing cannot use")
	}
	// Intake on for both: no reason anywhere.
	agExec(x.testEnv, "UPDATE settings SET value='true' WHERE key='payment_intake_BTC'")
	if on := html.UnescapeString(x.page("/product?id="+plain, x.buyerSess)); strings.Contains(on, "Not available for purchase") || !strings.Contains(on, buyHref) {
		t.Fatal("a reason shows while intake is on")
	}
	// An existing draft under escrow only on an ineligible listing: the order page says why it cannot be paid.
	draft := x.testEnv.order(x.buyer, plain, "BTC", stateDraft)
	agExec(x.testEnv, "UPDATE settings SET value='escrow' WHERE key='payment_intake_BTC'")
	op := html.UnescapeString(x.page("/order?id="+draft, x.buyerSess))
	if !strings.Contains(op, "New payments use escrow only") || !strings.Contains(op, "no automatic delivery content") || strings.Contains(op, `action="/orders/pay"`) || strings.Contains(op, `action="/orders/escrow"`) {
		t.Fatal("the draft page of an ineligible listing under escrow only does not say why it cannot be paid")
	}
}

// The "no active Bitcoin escrow key" gate (QA4W4B-2) applies to BTC only, and only while BTC is escrow only: the
// buyer reads the reason without the vendor's fix, the vendor reads the fix, a BTC draft is refused while an XMR one
// (XMR intake on) is taken; with BTC intake on there is no gate; an inactive key counts as none, an active one lifts it.
func TestEscrowOnlyVendorKeyGateBTCOnly(t *testing.T) {
	x := newEscrowEnv(t)
	nokey, nokeySess := x.user("gatenokey_"+randomToken()[:6], "vendor")
	listing := x.autoListing(nokey) // escrow-eligible listing, vendor without any escrow key
	buyHref := `href="/checkout?id=` + listing + `"`
	agExec(x.testEnv, "UPDATE settings SET value='escrow' WHERE key='payment_intake_BTC'")
	buyer := html.UnescapeString(x.page("/product?id="+listing, x.buyerSess))
	if !strings.Contains(buyer, "Not available for purchase in BTC") || !strings.Contains(buyer, "no active Bitcoin escrow key") ||
		strings.Contains(buyer, "Not available for purchase in XMR") || !strings.Contains(buyer, buyHref) {
		t.Fatalf("buyer's view (BTC escrow only, XMR on):\n%s", buyer)
	}
	if strings.Contains(buyer, "Register your Bitcoin escrow key") {
		t.Fatal("the buyer was shown the vendor's fix")
	}
	if v := html.UnescapeString(x.page("/product?id="+listing, nokeySess)); !strings.Contains(v, "Register your Bitcoin escrow key") {
		t.Fatal("the vendor does not read the fix")
	}
	if w := x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {listing}, "currency": {"BTC"}}); w.Code != 409 ||
		!strings.Contains(html.UnescapeString(w.Body.String()), "no active Bitcoin escrow key") {
		t.Fatalf("BTC draft from a vendor without a key under BTC escrow only: %d", w.Code)
	}
	if w := x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {listing}, "currency": {"XMR"}}); w.Code != 303 {
		t.Fatalf("XMR draft (XMR intake on) refused: %d %s", w.Code, w.Body.String())
	}
	// BTC intake on: no gate whatever the key.
	agExec(x.testEnv, "UPDATE settings SET value='true' WHERE key='payment_intake_BTC'")
	if w := x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {listing}, "currency": {"BTC"}}); w.Code != 303 {
		t.Fatalf("BTC draft with intake on: %d", w.Code)
	}
	// An inactive key counts as none; an active one lifts the gate.
	x.activeKey(nokey, altEscrowKey(t, testBIP48Key, "0badc0de", 3))
	agExec(x.testEnv, "UPDATE user_escrow_keys SET active_at=NULL WHERE user_id=$1", nokey)
	agExec(x.testEnv, "UPDATE settings SET value='escrow' WHERE key='payment_intake_BTC'")
	if b := html.UnescapeString(x.page("/product?id="+listing, x.buyerSess)); !strings.Contains(b, "no active Bitcoin escrow key") {
		t.Fatal("an inactive key lifted the gate")
	}
	agExec(x.testEnv, "UPDATE user_escrow_keys SET active_at=now() WHERE user_id=$1", nokey)
	if b := html.UnescapeString(x.page("/product?id="+listing, x.buyerSess)); strings.Contains(b, "Not available for purchase") {
		t.Fatal("an active key did not lift the gate")
	}
}

// Monero escrow on, XMR escrow only, a vendor without any Bitcoin escrow key (QA4W4B-2): Monero escrow needs no vendor
// key, so the listing stays purchasable in XMR and the escrow checkout succeeds.
func TestXMREscrowOnlyIgnoresBTCKey(t *testing.T) {
	x := newXMREscrowEnv(t)
	agExec(x.testEnv, "UPDATE settings SET value='escrow' WHERE key='payment_intake_XMR'")
	x.mustPoll()
	if n := agInt(x.testEnv, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1", x.vendor); n != 0 {
		t.Fatalf("fixture: the vendor has %d escrow keys", n)
	}
	if page := html.UnescapeString(x.page("/product?id="+x.product, x.buyerSess)); strings.Contains(page, "Not available for purchase in XMR") {
		t.Fatal("XMR blocked by the missing Bitcoin escrow key")
	}
	w := x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {x.product}, "currency": {"XMR"}})
	if w.Code != 303 {
		t.Fatalf("XMR draft under XMR escrow only refused: %d %s", w.Code, w.Body.String())
	}
	order := strings.TrimPrefix(w.Header().Get("Location"), "/order?id=")
	x.mustCheckout(order)
	if s := x.state(order); s != stateAwaitingPayment {
		t.Fatalf("after the Monero escrow checkout: %s", s)
	}
}
