package market

import (
	"html"
	"net/url"
	"strings"
	"testing"
)

const subFloorPrice = 5000

func TestLegacyServiceCannotBeOrdered(t *testing.T) {
	p := newPayEnv(t)
	agExec(p.testEnv, "UPDATE products SET kind='service' WHERE id=$1", p.productID)
	for _, route := range []string{"/product?id=" + p.productID, "/checkout?id=" + p.productID} {
		body := p.page(route, p.buyerSess)
		mustContain(t, body, "This legacy service listing is no longer available for new orders")
		mustNotContain(t, body, `action="/orders"`)
	}
	for _, cur := range []string{"BTC", "XMR"} {
		w := p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {cur}})
		if w.Code != 409 {
			t.Fatalf("%s service draft: %d %s", cur, w.Code, w.Body)
		}
	}
}
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
	w := p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"BTC"}})
	if w.Code != 400 || !strings.Contains(html.UnescapeString(w.Body.String()), "below the minimum automatic payout") {
		t.Fatalf("BTC subfloor draft: %d %s", w.Code, w.Body)
	}
	if n := p.count("SELECT count(*) FROM orders WHERE buyer_id=$1 AND product_id=$2 AND currency='BTC' AND state='draft'", p.buyer.ID, p.productID); n != 1 {
		t.Fatalf("unexpected BTC drafts: %d", n)
	} // fixture's prior draft only
	w = p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"XMR"}})
	p.check(w, 303)
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

	btcDraft := p.order
	xmrBuyer, xmrSession := p.user("subfloor_xmr_"+randomToken()[:8], "buyer")
	xmrDraft := p.testEnv.order(xmrBuyer, p.productID, "XMR", stateDraft)
	for _, draft := range []struct{ order, session string }{{btcDraft, p.buyerSess}, {xmrDraft, xmrSession}} {
		page = html.UnescapeString(p.body("GET", "/order?id="+draft.order, draft.session, nil, 200))
		mustContain(t, page, draft.order, "Draft")
		mustNotContain(t, page, `action="/orders/escrow"`, `action="/orders/escrow-xmr"`)
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
	mustContain(t, page, btcDraft, "Draft")
	mustNotContain(t, page, `action="/orders/pay"`, `action="/orders/escrow"`)

	// The XMR refusal is symmetric when only BTC is above its floor.
	if _, err := p.DB.Exec("UPDATE products SET btc=$1,xmr=$2 WHERE id=$3", payoutFloor("BTC", "release"), payoutFloor("XMR", "release")-1, p.productID); err != nil {
		t.Fatal(err)
	}
	page = product()
	mustContain(t, page, "cannot be ordered in XMR", "Order in BTC")
	page = checkout()
	mustContain(t, page, `<option value="BTC">`)
	mustNotContain(t, page, `<option value="XMR">`)
	page = html.UnescapeString(p.body("GET", "/order?id="+xmrDraft, xmrSession, nil, 200))
	mustContain(t, page, xmrDraft, "Draft")
	mustNotContain(t, page, `action="/orders/pay"`, `action="/orders/escrow-xmr"`)
}

// An overpayment more than the price of a legacy sub-floor order is held for an administrator as one release;
// the parties are told the price is the cause, and the complete form states the same outcome.
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
