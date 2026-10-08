package market

import (
	"html"
	"net/url"
	"strings"
	"testing"
)

// A-216: listings priced below the Bitcoin payout floor before A-99 added the listing floor. New BTC orders on them
// are refused, and on an existing such order an overpayment cannot be split (the vendor's share alone would be
// below the floor), so a single release whose excess is more than the price waits for an administrator.

const subFloorPrice = 5000 // satoshi, below minPayoutBTC; the listing form refuses it, so only legacy rows have it

func TestLegacyServiceCannotBeOrderedOrFunded(t *testing.T) {
	p := newPayEnv(t)
	if _, err := p.DB.Exec("UPDATE products SET kind='service' WHERE id=$1", p.productID); err != nil {
		t.Fatal(err)
	}
	const unavailable = "This legacy service listing is no longer available for new orders. Existing orders keep their recorded service history; ask the vendor to offer a supported physical or digital listing instead."
	for _, path := range []string{"/product?id=" + p.productID, "/checkout?id=" + p.productID} {
		page := p.page(path, p.buyerSess)
		if !strings.Contains(page, unavailable) || strings.Contains(page, `action="/orders"`) || strings.Contains(page, `href="/checkout?id=`) {
			t.Fatalf("%s offers an unsupported service order:\n%s", path, page)
		}
	}
	for _, currency := range []string{"BTC", "XMR"} {
		w := p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {currency}})
		if w.Code != 409 || !strings.Contains(w.Body.String(), "cannot accept new orders") {
			t.Fatalf("new %s service draft: %d %s", currency, w.Code, w.Body.String())
		}
		if n := p.count("SELECT count(*) FROM orders WHERE buyer_id=$1 AND product_id=$2 AND currency=$3 AND state='draft'", p.buyer.ID, p.productID, currency); n != 0 {
			t.Fatalf("refused service listing created %d %s drafts", n, currency)
		}
	}
	btcWallet := &countingWallet{fakeProvider: p.fake}
	xmrWallet := &countingWallet{fakeProvider: newFakeProvider("XMR", 10)}
	p.A.payments["BTC"], p.A.payments["XMR"] = btcWallet, xmrWallet
	stock := p.count("SELECT stock FROM products WHERE id=$1", p.productID)
	for _, currency := range []string{"BTC", "XMR"} {
		legacyDraft := p.testEnv.order(p.buyer.ID, p.productID, currency, stateDraft)
		orderPage := p.page("/order?id="+legacyDraft, p.buyerSess)
		if !strings.Contains(orderPage, "legacy service order cannot request a new payment address") || strings.Contains(orderPage, `action="/orders/pay"`) {
			t.Fatalf("legacy service draft offers payment:\n%s", orderPage)
		}
		w := p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {legacyDraft}})
		if w.Code != 409 || !strings.Contains(w.Body.String(), "legacy service order cannot request") {
			t.Fatalf("legacy service %s payment request: %d %s", currency, w.Code, w.Body.String())
		}
		wallet := btcWallet
		if currency == "XMR" {
			wallet = xmrWallet
		}
		if wallet.addresses.Load() != 0 || p.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", legacyDraft) != 0 || p.state(legacyDraft) != stateDraft || p.count("SELECT stock FROM products WHERE id=$1", p.productID) != stock {
			t.Fatalf("refused legacy service %s funding called the wallet, reserved stock, stored an address or moved the draft", currency)
		}
	}
}

// legacySubFloor prices the payEnv listing and its order at subFloorPrice, as a database from before A-99 may.
func (p *payEnv) legacySubFloor() {
	p.t.Helper()
	if _, err := p.DB.Exec("UPDATE products SET btc=$1 WHERE id=$2", subFloorPrice, p.productID); err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.DB.Exec("UPDATE orders SET amount=$1 WHERE id=$2", subFloorPrice, p.order); err != nil {
		p.t.Fatal(err)
	}
}

func TestLegacySubFloorListingRefusesBTCOrder(t *testing.T) {
	p := newPayEnv(t)
	p.legacySubFloor()
	wallet := &countingWallet{fakeProvider: p.fake}
	p.A.payments["BTC"] = wallet
	p.A.payments["XMR"] = newFakeProvider("XMR", 10)
	stock := func() int { return p.count("SELECT stock FROM products WHERE id=$1", p.productID) }
	drafts := func(currency string) int {
		return p.count("SELECT count(*) FROM orders WHERE buyer_id=$1 AND product_id=$2 AND currency=$3 AND state='draft'", p.buyer.ID, p.productID, currency)
	}

	// A new BTC draft is refused, naming the floor; nothing is stored.
	const btcRefusal = "This listing's Bitcoin price is below the minimum automatic payout of 0.0001 BTC (it was priced before that minimum applied), so it cannot be ordered in BTC."
	w := p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"BTC"}})
	if w.Code != 400 || !strings.Contains(html.UnescapeString(w.Body.String()), btcRefusal) {
		t.Fatalf("BTC draft: status=%d body=%q, want 400 naming 0.0001 BTC", w.Code, w.Body.String())
	}
	if n := drafts("BTC"); n != 0 {
		t.Fatalf("refused BTC draft stored %d drafts", n)
	}

	// A BTC draft saved before the floor cannot request a payment address: no reservation, no wallet call.
	draft := p.testEnv.order(p.buyer.ID, p.productID, "BTC", stateDraft)
	before, addresses := stock(), wallet.addresses.Load()
	w = p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {draft}})
	if w.Code != 400 || !strings.Contains(html.UnescapeString(w.Body.String()), btcRefusal) {
		t.Fatalf("BTC pay: status=%d body=%q, want 400 naming 0.0001 BTC", w.Code, w.Body.String())
	}
	if wallet.addresses.Load() != addresses {
		t.Fatal("the wallet was asked for an address")
	}
	if stock() != before || p.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", draft) != 0 || p.state(draft) != stateDraft {
		t.Fatal("a refused payment request reserved stock, stored an address or moved the order")
	}

	// Paused intake is still reported first (the order page's reason).
	if _, err := p.DB.Exec("UPDATE settings SET value='false' WHERE key='payment_intake_BTC'"); err != nil {
		t.Fatal(err)
	}
	w = p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {draft}})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "paused") {
		t.Fatalf("paused BTC pay: status=%d, want 409 paused", w.Code)
	}
	if _, err := p.DB.Exec("UPDATE settings SET value='true' WHERE key='payment_intake_BTC'"); err != nil {
		t.Fatal(err)
	}

	// Monero on the same listing is unaffected.
	w = p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"XMR"}})
	p.check(w, 303)
	xmrDraft := p.str("SELECT id FROM orders WHERE buyer_id=$1 AND product_id=$2 AND currency='XMR' AND state='draft'", p.buyer.ID, p.productID)
	before = stock()
	p.check(p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {xmrDraft}}), 303)
	if stock() != before-1 || p.state(xmrDraft) != stateAwaitingPayment {
		t.Fatal("an XMR payment request on the legacy listing was refused")
	}
	if wallet.addresses.Load() != addresses {
		t.Fatal("the BTC wallet was asked for an address for an XMR order")
	}

	// A listing at the floor takes BTC orders again.
	if _, err := p.DB.Exec("UPDATE products SET btc=$1 WHERE id=$2", minPayoutBTC, p.productID); err != nil {
		t.Fatal(err)
	}
	p.check(p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {draft}}), 303)
	if wallet.addresses.Load() != addresses+1 {
		t.Fatal("a BTC order at the floor did not get an address")
	}
}

func TestLegacySubFloorCopyOffersOnlyUsableAlternateCurrency(t *testing.T) {
	p := newPayEnv(t)
	p.legacySubFloor()
	if _, err := p.DB.Exec("UPDATE products SET xmr=$1 WHERE id=$2", payoutFloor("XMR", "release")-1, p.productID); err != nil {
		t.Fatal(err)
	}
	for _, currency := range []string{"BTC", "XMR"} {
		w := p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {currency}})
		if w.Code != 400 {
			t.Fatalf("%s draft status=%d, want 400", currency, w.Code)
		}
		body := html.UnescapeString(w.Body.String())
		mustContain(t, body, "cannot be ordered in "+currency, "Ask the vendor to raise the")
		other := "BTC"
		if currency == "BTC" {
			other = "XMR"
		}
		mustNotContain(t, body, "Order in "+other)
	}
	if _, err := p.DB.Exec("UPDATE products SET xmr=$1 WHERE id=$2", payoutFloor("XMR", "release"), p.productID); err != nil {
		t.Fatal(err)
	}
	w := p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"BTC"}})
	if w.Code != 400 || !strings.Contains(html.UnescapeString(w.Body.String()), "Order in XMR, or ask the vendor") {
		t.Fatalf("BTC draft with usable XMR alternate: status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestLegacySubFloorProductCheckoutAndDraftPagesGateCurrencies(t *testing.T) {
	p := newPayEnv(t)
	p.legacySubFloor()
	p.A.payments["XMR"] = newFakeProvider("XMR", 10)
	if _, err := p.DB.Exec("UPDATE products SET xmr=$1 WHERE id=$2", payoutFloor("XMR", "release")-1, p.productID); err != nil {
		t.Fatal(err)
	}
	product := func() string {
		return html.UnescapeString(p.body("GET", "/product?id="+p.productID, p.buyerSess, nil, 200))
	}
	checkout := func() string {
		return html.UnescapeString(p.body("GET", "/checkout?id="+p.productID, p.buyerSess, nil, 200))
	}
	page := product()
	mustContain(t, page, "cannot be ordered in BTC", "cannot be ordered in XMR")
	mustNotContain(t, page, "Review order draft")
	page = checkout()
	mustContain(t, page, "cannot be ordered in BTC", "cannot be ordered in XMR", "Neither BTC nor XMR is available")
	mustNotContain(t, page, `<option value="BTC">`, `<option value="XMR">`, `action="/orders"`)

	btcDraft := p.testEnv.order(p.buyer.ID, p.productID, "BTC", stateDraft)
	xmrDraft := p.testEnv.order(p.buyer.ID, p.productID, "XMR", stateDraft)
	for _, order := range []string{btcDraft, xmrDraft} {
		page = html.UnescapeString(p.body("GET", "/order?id="+order, p.buyerSess, nil, 200))
		mustContain(t, page, "Unavailable.", "below the minimum automatic payout")
		mustNotContain(t, page, `action="/orders/pay"`)
	}

	// Once XMR meets its floor, the BTC warning points to it and checkout offers only XMR.
	if _, err := p.DB.Exec("UPDATE products SET xmr=$1 WHERE id=$2", payoutFloor("XMR", "release"), p.productID); err != nil {
		t.Fatal(err)
	}
	page = product()
	mustContain(t, page, "below the minimum automatic payout", "Order in XMR")
	page = checkout()
	mustContain(t, page, `<option value="XMR">`)
	mustNotContain(t, page, `<option value="BTC">`)
	page = html.UnescapeString(p.body("GET", "/order?id="+btcDraft, p.buyerSess, nil, 200))
	mustContain(t, page, "cannot be ordered in BTC", "Order in XMR")
	mustNotContain(t, page, `action="/orders/pay"`)

	// The XMR refusal is symmetric when only BTC is above its floor.
	if _, err := p.DB.Exec("UPDATE products SET btc=$1,xmr=$2 WHERE id=$3", payoutFloor("BTC", "release"), payoutFloor("XMR", "release")-1, p.productID); err != nil {
		t.Fatal(err)
	}
	page = product()
	mustContain(t, page, "cannot be ordered in XMR", "Order in BTC")
	page = checkout()
	mustContain(t, page, `<option value="BTC">`)
	mustNotContain(t, page, `<option value="XMR">`)
	page = html.UnescapeString(p.body("GET", "/order?id="+xmrDraft, p.buyerSess, nil, 200))
	mustContain(t, page, "cannot be ordered in XMR", "Order in BTC")
	mustNotContain(t, page, `action="/orders/pay"`)
}

// An overpayment more than the price of a legacy sub-floor order is held for an administrator as one release;
// the parties are told the price is the cause, and the complete form states the same outcome.
func TestSubFloorPriceOverpaymentIsHeld(t *testing.T) {
	p := newPayEnv(t)
	p.legacySubFloor()
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	adminID := p.str("SELECT id FROM users WHERE role='admin'")
	p.paidByWatcherAmount(100000000)
	p.move(p.order, statePaid, stateShipped, p.vendor)
	cause := "the price is below the minimum automatic payout of 0.0001 BTC"
	statement := "Release 1 BTC (test network) to " + p.vendor.Handle + " — 0.99995 BTC more than the price; " + cause +
		", so the amount paid over it cannot be returned separately; an administrator reviews the release before it is sent; network fee deducted. This is final."
	buyerPage := html.UnescapeString(p.page("/order?id="+p.order, p.buyerSess))
	mustContain(t, buyerPage, statement)
	mustNotContain(t, buyerPage, "below the minimum automatic payout, so it is not returned")
	// A-215: the Overpaid alert and the complete help say the same for both parties.
	mustContain(t, buyerPage, "Completing releases everything to the vendor: "+cause+", so the amount paid over it cannot be returned separately. An administrator reviews the release before it is sent.",
		"Completing queues the payouts stated below; the payment watcher sends them automatically unless the statement says otherwise.")
	vendorPage := html.UnescapeString(p.page("/order?id="+p.order, p.vendorSess))
	mustContain(t, vendorPage, "Completing releases everything to you: "+cause+", so the amount paid over it cannot be returned separately. An administrator reviews the release before it is sent.")
	for _, page := range []string{buyerPage, vendorPage} {
		// The price, not the excess, is the cause: no excess-below-the-floor reason and no below-floor release.
		mustNotContain(t, page, "the excess is below", "so it is not returned", "not sent automatically", "returns the excess", "whole counted amount", "the return waits")
	}
	p.check(p.do("POST", "/orders/complete", p.buyerSess, payoutConfirmed(url.Values{"order_id": {p.order}}, 100000000)), 303)

	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 1 {
		t.Fatalf("payouts %d, want one release", n)
	}
	st, _, addr, amt := p.payout(p.order)
	if errText := p.str("SELECT error FROM payouts WHERE order_id=$1", p.order); st != "held" || errText != largeHold || addr != "fake-testnet-vendor" || amt != 100000000 {
		t.Fatalf("release %s %q %s %d, want held with largeHold", st, errText, addr, amt)
	}
	for _, u := range []*User{p.buyer, p.vendor} {
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%'||$2||'%'", u.ID, cause); n != 1 {
			t.Fatalf("%s not told the price is the cause: %d", u.Handle, n)
		}
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%so it is not returned%'", u.ID); n != 0 {
			t.Fatalf("%s told the excess is the cause", u.Handle)
		}
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Payout review needed for order%'", adminID); n != 1 {
		t.Fatalf("administrator notices %d", n)
	}
	p.poll()
	p.poll()
	if s := p.fake.Sends(); len(s) != 0 {
		t.Fatalf("held release sent: %+v", s)
	}
	// The existing large-hold release path: refused without the review, sent once after it.
	id := p.payoutID(p.order)
	adminPage := html.UnescapeString(p.body("GET", "/admin", p.adminSess, nil, 200))
	mustContain(t, adminPage, "This is a release to the vendor, not a refund.", "there is no separate buyer-return payout", "send it manually from the pooled wallet", "Mark sent only for this vendor release and its transaction ID")
	mustNotContain(t, adminPage, "This refund is larger", "this payment is a genuine mistake and may be returned")
	release := url.Values{"payout_id": {id}, "op": {"release"}, "password": {testPassword}}
	if w := p.do("POST", "/admin/payout", p.adminSess, release); w.Code != 409 || !strings.Contains(w.Body.String(), "larger than the order price") {
		t.Fatalf("release without review: %d", w.Code)
	}
	release.Set("large_reviewed", "confirmed")
	p.check(p.do("POST", "/admin/payout", p.adminSess, release), 303)
	p.poll()
	if s := p.fake.Sends(); len(s) != 1 || s[0].To != "fake-testnet-vendor" || s[0].Amount != 100000000 {
		t.Fatalf("sends %+v", s)
	}
}

// An excess not above a sub-floor price keeps its behaviour: one unheld release, the dust notice.
func TestSubFloorPriceSmallExcessUnchanged(t *testing.T) {
	p := newPayEnv(t)
	p.legacySubFloor()
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	// An excess equal to the price (5,000 sat): the sum, 10,000 sat, reaches the release floor.
	p.paidByWatcherAmount(2 * subFloorPrice)
	p.move(p.order, statePaid, stateShipped, p.vendor)
	mustContain(t, html.UnescapeString(p.page("/order?id="+p.order, p.buyerSess)),
		"Release 0.0001 BTC (test network) to "+p.vendor.Handle+" — 0.00005 BTC more than the price, below the minimum automatic payout, so it is not returned; network fee deducted. This is final.")
	p.check(p.do("POST", "/orders/complete", p.buyerSess, payoutConfirmed(url.Values{"order_id": {p.order}}, 2*subFloorPrice)), 303)
	if st, _, _, amt := p.payout(p.order); st != "pending" || amt != 2*subFloorPrice {
		t.Fatalf("release %s %d, want pending", st, amt)
	}
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 1 {
		t.Fatalf("payouts %d", n)
	}
	for _, u := range []*User{p.buyer, p.vendor} {
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%below the minimum automatic payout of 0.0001 BTC, so it is not returned; the network fee is taken from the release.'", u.ID); n != 1 {
			t.Fatalf("%s not told the excess is kept: %d", u.Handle, n)
		}
	}
}

func TestSplitPayoutSubFloorPrice(t *testing.T) {
	o := &Order{BuyerID: "b", VendorID: "v", Currency: "BTC"}
	for _, tc := range []struct {
		name string
		sum  int64
		want payoutPart
	}{
		{"exact price", subFloorPrice, payoutPart{"release", "v", "vendor", subFloorPrice, false}},
		{"excess equal to the price", 2 * subFloorPrice, payoutPart{"release", "v", "vendor", 2 * subFloorPrice, false}},
		{"excess above the price is held", 2*subFloorPrice + 1, payoutPart{"release", "v", "vendor", 2*subFloorPrice + 1, true}},
		{"excess worth returning is held, not split", subFloorPrice + 100000000, payoutPart{"release", "v", "vendor", subFloorPrice + 100000000, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitPayout("release", o, tc.sum, subFloorPrice); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
	// A-232: the same rule for a legacy XMR price below the Monero payout floor (1000 piconero here).
	o.Currency = "XMR"
	const xmrSubFloor = 1000
	for _, tc := range []struct {
		name string
		sum  int64
		want payoutPart
	}{
		{"XMR exact price", xmrSubFloor, payoutPart{"release", "v", "vendor", xmrSubFloor, false}},
		{"XMR excess equal to the price", 2 * xmrSubFloor, payoutPart{"release", "v", "vendor", 2 * xmrSubFloor, false}},
		{"XMR excess above the price is held", 2*xmrSubFloor + 1, payoutPart{"release", "v", "vendor", 2*xmrSubFloor + 1, true}},
		{"XMR excess worth returning is held, not split", xmrSubFloor + minPayoutXMR, payoutPart{"release", "v", "vendor", xmrSubFloor + minPayoutXMR, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitPayout("release", o, tc.sum, xmrSubFloor); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}
