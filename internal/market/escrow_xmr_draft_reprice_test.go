package market

import (
	"html"
	"net/url"
	"strings"
	"testing"
)

// A-792 (b), the Monero port of the retired Bitcoin TestDraftRepricedOnResubmitAndEscrowCheckout (2026-09-23 review
// F1): a draft is never checked out at a stale price. Resubmitting the listing's order form after the vendor changed the
// price saves the same draft at the current price (orderDraftAction's upsert); a checkout form rendered before a later
// change, and posted after it, takes the escrow at the price current under the checkout's locks (xmrEscrowCheckoutAction
// writes it before the order leaves draft), and the escrow page then states that price as required. Once the escrow
// exists its amount is fixed: a later change neither reprices it nor lets a resubmitted listing form touch it. A price
// edited during the checkout itself is refused 409 (TestXMRQuantityCheckoutRejectsPriceChangeBeforeStockLock). DB
// integration test against the simulated Monero wallet RPC, not chain evidence.
func TestXMRDraftRepricedOnResubmitAndCheckout(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	product := x.digital(x.vendor)
	price := func(n int64) { agExec(x.testEnv, "UPDATE products SET xmr=$2 WHERE id=$1", product, n) }
	draftAt := func() (string, int64) {
		t.Helper()
		w := x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {product}, "currency": {"XMR"}})
		x.check(w, 303)
		id := strings.TrimPrefix(w.Header().Get("Location"), "/order?id=")
		return id, agInt64(t, x.testEnv, "SELECT amount FROM orders WHERE id=$1", id)
	}
	id, amt := draftAt()
	if amt != xmrListingPrice {
		t.Fatalf("draft amount %d, want %d", amt, xmrListingPrice)
	}
	price(2 * xmrListingPrice)
	if again, amt := draftAt(); again != id || amt != 2*xmrListingPrice {
		t.Fatalf("resubmitted draft %s at %d, want the same draft %s at %d", shortID(again), amt, shortID(id), 2*xmrListingPrice)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM orders WHERE buyer_id=$1 AND product_id=$2", x.buyer, product); n != 1 {
		t.Fatalf("the resubmitted listing form left %d orders", n)
	}

	// The checkout form is rendered at 1 XMR; the vendor raises the price to 1.5 XMR before the buyer posts it.
	page := html.UnescapeString(x.page(orderPage(id), x.buyerSess))
	mustContain(t, page, "Current checkout total for 1 unit:", amount(2*xmrListingPrice, 12)+" XMR", "the current unit price are checked again when you request the escrow")
	price(3 * xmrListingPrice)
	checkout := url.Values{"order_id": {id}, "refund_address": {testXMRAddr("repriced refund "+id, true)}, formNonceField: {renderedFormNonce(t, page, "/orders/escrow-xmr")}}
	x.check(x.do("POST", "/orders/escrow-xmr", x.buyerSess, checkout), 303)
	if st, amt := x.state(id), agInt64(t, x.testEnv, "SELECT amount FROM orders WHERE id=$1", id); st != stateAwaitingPayment || amt != 3*xmrListingPrice {
		t.Fatalf("checkout: order %s at %d, want %s at the current %d", st, amt, stateAwaitingPayment, 3*xmrListingPrice)
	}
	required := func() string {
		t.Helper()
		page := html.UnescapeString(x.page(orderPage(id), x.buyerSess))
		_, rest, ok := strings.Cut(page, "<dt>Required</dt>")
		if !ok {
			t.Fatalf("no Required line on the escrow page:\n%s", page)
		}
		dd, _, _ := strings.Cut(rest, "</dd>")
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(dd), "<dd>"))
	}
	if r := required(); !strings.HasPrefix(r, amount(3*xmrListingPrice, 12)+" XMR") {
		t.Fatalf("the escrow page requires %q, want the current price %s XMR", r, amount(3*xmrListingPrice, 12))
	}

	// Fixed once the escrow exists: a later change, or a resubmitted listing form, never reprices it.
	price(4 * xmrListingPrice)
	if fresh, amt := draftAt(); fresh == id || amt != 4*xmrListingPrice {
		t.Fatalf("a listing form after checkout: draft %s at %d", shortID(fresh), amt)
	}
	if amt := agInt64(t, x.testEnv, "SELECT amount FROM orders WHERE id=$1", id); amt != 3*xmrListingPrice || !strings.HasPrefix(required(), amount(3*xmrListingPrice, 12)+" XMR") {
		t.Fatalf("the escrowed order was repriced to %d after checkout", amt)
	}
}
