package market

import (
	"strings"
	"testing"
)

// Payment intake paused: the buyer's order page offers no escrow form and says why, and the footer and the notices on
// the catalog, listing and checkout do not imply checkout is open. Everything is as before once it is on again.
func TestIntakePausedPagesSayCheckoutIsPaused(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	product := x.digital(x.vendor)
	order := x.draft(product)
	const (
		form     = `action="/orders/escrow-xmr"`
		reason   = "New Monero escrow checkouts are paused by the administrator, so no escrow address can be issued for this draft yet."
		notice   = "New Monero escrow checkouts are paused by the administrator."
		footer   = `<div class="footer-status"><span class="accent">New payments paused</span></div>`
		badge    = `<div class="footer-status"><span class="badge testnet">Monero escrow (local fakechain)</span></div>`
		catalog  = "Eligible listings can be paid through per-order escrow"
		listing  = "Checkout availability is shown below."
		checkout = "Open the order and complete an available escrow checkout"
	)
	page := func(path, sess string) string {
		t.Helper()
		return x.body("GET", path, sess, nil, 200)
	}
	pages := func() (orderPage, home, productPage, checkoutPage string) {
		return page("/order?id="+order, x.buyerSess), page("/", x.buyerSess), page("/product?id="+product, x.buyerSess), page("/checkout?id="+product, x.buyerSess)
	}
	open := func() {
		t.Helper()
		o, h, p, c := pages()
		mustContain(t, o, form, badge)
		mustContain(t, h, catalog, badge)
		mustContain(t, p, listing, badge)
		mustContain(t, c, checkout, badge)
		mustNotContain(t, o+h+p+c, reason, notice, "New payments paused")
	}
	open()

	agExec(x.testEnv, "UPDATE settings SET value=$1 WHERE key=$2", intakePaused, paymentIntakeKey("XMR"))
	o, h, p, c := pages()
	mustContain(t, o, `<p class="order-unavailable" role="note"><strong>Unavailable.</strong> `+reason, footer)
	mustNotContain(t, o, form, `name="refund_address"`, badge)
	for name, b := range map[string]string{"catalog": h, "listing": p, "checkout": c} {
		if !strings.Contains(b, notice) || !strings.Contains(b, footer) {
			t.Fatalf("%s while intake is paused:\n%s", name, b)
		}
	}
	mustNotContain(t, h+p+c, badge, catalog, listing, checkout, "A Monero escrow order pays into its own account")
	// The vendor's view of the same footer, and an anonymous visitor's.
	mustContain(t, page("/vendor-dashboard", x.vendorSess), footer)
	mustContain(t, page("/", ""), footer, notice)
	// A refusal page carries the footer too.
	mustContain(t, refusalPost(x.testEnv, "GET", "/no-such-page", x.buyerSess, nil, "").Body.String(), footer)

	agExec(x.testEnv, "UPDATE settings SET value=$1 WHERE key=$2", intakeEscrow, paymentIntakeKey("XMR"))
	open()
}
