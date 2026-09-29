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
	w := p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"BTC"}})
	if w.Code != 400 || !strings.Contains(html.UnescapeString(w.Body.String()), "0.0001 BTC") {
		t.Fatalf("BTC draft: status=%d body=%q, want 400 naming 0.0001 BTC", w.Code, w.Body.String())
	}
	if n := drafts("BTC"); n != 0 {
		t.Fatalf("refused BTC draft stored %d drafts", n)
	}

	// A BTC draft saved before the floor cannot request a payment address: no reservation, no wallet call.
	draft := p.testEnv.order(p.buyer.ID, p.productID, "BTC", stateDraft)
	before, addresses := stock(), wallet.addresses.Load()
	w = p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {draft}})
	if w.Code != 400 || !strings.Contains(html.UnescapeString(w.Body.String()), "0.0001 BTC") {
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
	cause := "the price is below the minimum payout of 0.0001 BTC"
	statement := "Release 1 BTC (test network) to " + p.vendor.Handle + " — 0.99995 BTC more than the price; " + cause +
		", so the amount paid over it cannot be returned separately; an administrator reviews the release before it is sent; network fee deducted. This is final."
	buyerPage := html.UnescapeString(p.page("/order?id="+p.order, p.buyerSess))
	mustContain(t, buyerPage, statement)
	mustNotContain(t, buyerPage, "below the minimum automatic refund, so it is not returned")
	// A-215: the Overpaid alert and the complete help say the same for both parties.
	mustContain(t, buyerPage, "Completing releases everything to the vendor: "+cause+", so the amount paid over it cannot be returned separately. An administrator reviews the release before it is sent.",
		"Completing queues the payouts stated below; the payment watcher sends them automatically unless the statement says otherwise.")
	vendorPage := html.UnescapeString(p.page("/order?id="+p.order, p.vendorSess))
	mustContain(t, vendorPage, "Completing releases everything to you: "+cause+", so the amount paid over it cannot be returned separately. An administrator reviews the release before it is sent.")
	for _, page := range []string{buyerPage, vendorPage} {
		mustNotContain(t, page, "below the minimum automatic refund of", "returns the excess", "whole counted amount", "the return waits")
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
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%minimum automatic%'", u.ID); n != 0 {
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
		"Release 0.0001 BTC (test network) to "+p.vendor.Handle+" — 0.00005 BTC more than the price, below the minimum automatic refund, so it is not returned; network fee deducted. This is final.")
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
