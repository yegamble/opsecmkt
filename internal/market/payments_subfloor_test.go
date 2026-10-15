package market

import (
	"html"
	"net/url"
	"testing"
)

func TestLegacyServiceCannotBeOrdered(t *testing.T) {
	p := newPayEnv(t)
	agExec(p.testEnv, "UPDATE products SET kind='service' WHERE id=$1", p.productID)
	for _, route := range []string{"/product?id=" + p.productID, "/checkout?id=" + p.productID} {
		body := p.page(route, p.buyerSess)
		mustContain(t, body, "This legacy service listing is no longer available for new orders")
		mustNotContain(t, body, `action="/orders"`)
	}
	p.check(p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"XMR"}}), 409)
}

func TestLegacySubFloorMoneroListing(t *testing.T) {
	p := newPayEnv(t)
	agExec(p.testEnv, "UPDATE products SET xmr=$1 WHERE id=$2", minPayoutXMR-1, p.productID)
	w := p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"XMR"}})
	p.check(w, 400)
	body := html.UnescapeString(w.Body.String())
	mustContain(t, body, "below the escrow settlement minimum", "Ask the vendor to raise the Monero price")
	mustNotContain(t, body, "Order in BTC")
	for _, route := range []string{"/product?id=" + p.productID, "/checkout?id=" + p.productID} {
		body = html.UnescapeString(p.page(route, p.buyerSess))
		mustContain(t, body, "cannot be ordered in XMR")
		mustNotContain(t, body, `action="/orders"`)
	}
	// At the supported floor a paused intake can save an unfunded draft.
	agExec(p.testEnv, "UPDATE products SET xmr=$1 WHERE id=$2", minPayoutXMR, p.productID)
	p.check(p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"XMR"}}), 303)
}
