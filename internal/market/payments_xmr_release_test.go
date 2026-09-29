package market

import (
	"context"
	"html"
	"net/url"
	"strings"
	"testing"
)

// A-232 (owner decision: match Bitcoin): every Monero payout, releases included, is sent with the network fee taken
// from the amount, so no payout on either coin costs the market its fee; one Monero payout floor (0.001 XMR) applies
// to every kind; listings cannot be priced below it in XMR, and a legacy listing priced lower takes no XMR orders.

// xmrPayEnv adds a fake XMR wallet (10 confirmations) to a payEnv and saves both parties' XMR payout addresses.
func xmrPayEnv(t *testing.T) (*payEnv, *fakeProvider) {
	p := newPayEnv(t)
	xf := newFakeProvider("XMR", 10)
	p.A.payments["XMR"] = xf
	for u, addr := range map[*User]string{p.buyer: "fake-testnet-xbuyer", p.vendor: "fake-testnet-xvendor"} {
		if _, err := p.DB.Exec("UPDATE users SET payout_xmr=$1 WHERE id=$2", addr, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	return p, xf
}

// xmrPaid inserts an XMR order of the payEnv's listing priced at price (piconero), pays it with one confirmed deposit
// of paid and ships it.
func (p *payEnv) xmrPaid(xf *fakeProvider, price, paid int64) string {
	p.t.Helper()
	id := p.testEnv.order(p.buyer.ID, p.productID, "XMR", stateAwaitingPayment)
	addr, err := xf.NewAddress(context.Background(), id)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err = p.DB.Exec("UPDATE orders SET amount=$2 WHERE id=$1", id, price); err != nil {
		p.t.Fatal(err)
	}
	if _, err = p.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'XMR',$2,'fake')", id, addr); err != nil {
		p.t.Fatal(err)
	}
	xf.Deposit(addr, "xmr-paid-"+id[:8], 0, paid, 10)
	p.poll()
	if s := p.state(id); s != statePaid {
		p.t.Fatalf("XMR order not paid: %s", s)
	}
	p.move(id, statePaid, stateShipped, p.vendor)
	return id
}

func TestMoneroReleaseSentWithFeeFromAmount(t *testing.T) {
	p, xf := xmrPayEnv(t)
	const price = 500000000000 // 0.5 XMR
	order := p.xmrPaid(xf, price, price)
	// The complete form and the order page say the vendor bears the fee; nothing says the market pays it.
	buyerPage := html.UnescapeString(p.page("/order?id="+order, p.buyerSess))
	mustContain(t, buyerPage, "Release 0.5 XMR (test network) to "+p.vendor.Handle+" — exactly the price; network fee deducted. This is final.")
	mustNotContain(t, buyerPage, "market wallet pays")
	p.check(p.do("POST", "/orders/complete", p.buyerSess, payoutConfirmed(url.Values{"order_id": {order}}, price)), 303)
	p.poll()
	s := xf.Sends()
	if len(s) != 1 || s[0].To != "fake-testnet-xvendor" || s[0].Amount != price || !s[0].FeeFromAmount {
		t.Fatalf("XMR release sends %+v, want one fee-from-amount send of the price", s)
	}
	for _, sess := range []string{p.buyerSess, p.vendorSess} {
		page := p.page("/order?id="+order, sess)
		mustContain(t, page, "The Monero network fee is deducted from this payout. The destination receives the amount below minus the network fee.")
		mustNotContain(t, page, "market wallet pays")
	}
	mustContain(t, p.page("/admin", p.adminSess), "Every payout, on both coins, is sent with the network fee taken from its amount")
}

// A Monero release below the floor (only a legacy listing priced below it can make one) is flagged for staff via
// the A-99 path and never sent.
func TestMoneroReleaseBelowFloorFlagged(t *testing.T) {
	p, xf := xmrPayEnv(t)
	order := p.xmrPaid(xf, 1000, 1000)
	mustContain(t, html.UnescapeString(p.page("/order?id="+order, p.buyerSess)),
		"Release 0.000000001 XMR (test network) to "+p.vendor.Handle+" — exactly the price; below the minimum automatic payout of 0.001 XMR: not sent automatically, staff are asked to review it. This is final.")
	p.check(p.do("POST", "/orders/complete", p.buyerSess, payoutConfirmed(url.Values{"order_id": {order}}, 1000)), 303)
	p.poll()
	p.poll()
	if s := p.state(order); s != stateCompleted {
		t.Fatalf("state %s", s)
	}
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", order); n != 0 || len(xf.Sends()) != 0 {
		t.Fatalf("sub-floor XMR release queued: %d rows, sends %+v", n, xf.Sends())
	}
	if n := p.floorFlags(order); n != 1 || p.reviewNotices(p.mod.ID, order) != 1 {
		t.Fatalf("flag notes %d, moderator notifications %d", n, p.reviewNotices(p.mod.ID, order))
	}
	note := p.str("SELECT note FROM order_events WHERE order_id=$1 AND note LIKE '%below the minimum automatic%'", order)
	mustContain(t, note, "TESTNET release of 0.000000001 XMR to the vendor", "minimum automatic payout of 0.001 XMR")
}

func TestListingRefusesSubFloorXMRPrice(t *testing.T) {
	e := newTestApp(t)
	owner, s := e.user("inv_xfloor", "vendor")
	p := e.product(owner, "physical")
	for _, price := range []string{"0.000000000001", "0.000999999999"} {
		w := e.do("POST", "/listings", s, inventoryForm("", map[string]string{"price_xmr": price}))
		e.check(w, 400)
		if !strings.Contains(w.Body.String(), "The Monero price must be at least 0.001 XMR") {
			t.Fatalf("create at %s: reason missing: %s", price, w.Body.String())
		}
		e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"price_xmr": price})), 400)
	}
	var xmr int64
	if err := e.DB.QueryRow("SELECT xmr FROM products WHERE id=$1", p).Scan(&xmr); err != nil || xmr != 500000000000 {
		t.Fatalf("price changed by a refused edit: %d %v", xmr, err)
	}
	e.check(e.do("POST", "/listings", s, inventoryForm("", map[string]string{"price_xmr": "0.001"})), 303)
	e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"price_xmr": "0.001"})), 303)
	// The listing forms state the minimum before submitting.
	mustContain(t, e.do("GET", "/listing-edit?id="+p, s, nil).Body.String(), "the Monero price at least 0.001 XMR")
}

func TestLegacySubFloorXMRListingRefusesXMROrder(t *testing.T) {
	p := newPayEnv(t)
	if _, err := p.DB.Exec("UPDATE products SET xmr=1000 WHERE id=$1", p.productID); err != nil {
		t.Fatal(err)
	}
	wallet := &countingWallet{fakeProvider: newFakeProvider("XMR", 10)}
	p.A.payments["XMR"] = wallet
	stock := func() int { return p.count("SELECT stock FROM products WHERE id=$1", p.productID) }

	// A new XMR draft is refused, naming the Monero floor; nothing is stored.
	w := p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"XMR"}})
	body := html.UnescapeString(w.Body.String())
	if w.Code != 400 || !strings.Contains(body, "Monero price is below the minimum Monero payout of 0.001 XMR") || !strings.Contains(body, "cannot be ordered in XMR") {
		t.Fatalf("XMR draft: status=%d body=%q, want 400 naming 0.001 XMR", w.Code, body)
	}
	if n := p.count("SELECT count(*) FROM orders WHERE buyer_id=$1 AND product_id=$2 AND currency='XMR'", p.buyer.ID, p.productID); n != 0 {
		t.Fatalf("refused XMR draft stored %d orders", n)
	}

	// An XMR draft saved before the floor cannot request a payment address: no reservation, no wallet call.
	draft := p.testEnv.order(p.buyer.ID, p.productID, "XMR", stateDraft)
	before := stock()
	w = p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {draft}})
	if w.Code != 400 || !strings.Contains(html.UnescapeString(w.Body.String()), "0.001 XMR") {
		t.Fatalf("XMR pay: status=%d body=%q, want 400 naming 0.001 XMR", w.Code, w.Body.String())
	}
	if wallet.addresses.Load() != 0 || stock() != before || p.state(draft) != stateDraft || p.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", draft) != 0 {
		t.Fatal("a refused XMR payment request asked the wallet, reserved stock, stored an address or moved the order")
	}

	// Bitcoin on the same listing is unaffected.
	w = p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"BTC"}})
	p.check(w, 303)

	// A listing at the floor takes XMR orders again.
	if _, err := p.DB.Exec("UPDATE products SET xmr=$1 WHERE id=$2", minPayoutXMR, p.productID); err != nil {
		t.Fatal(err)
	}
	p.check(p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {draft}}), 303)
	if wallet.addresses.Load() != 1 || p.state(draft) != stateAwaitingPayment {
		t.Fatal("an XMR order at the floor did not get an address")
	}
}

// An existing order on a legacy sub-floor XMR listing: an excess more than the price holds the single release under
// largeHold, and the copy names the Monero floor.
func TestSubFloorXMRPriceOverpaymentIsHeld(t *testing.T) {
	p, xf := xmrPayEnv(t)
	const price, paid = 1000, 500000000000
	order := p.xmrPaid(xf, price, paid)
	cause := "the price is below the minimum payout of 0.001 XMR"
	buyerPage := html.UnescapeString(p.page("/order?id="+order, p.buyerSess))
	mustContain(t, buyerPage, "Completing releases everything to the vendor: "+cause+", so the amount paid over it cannot be returned separately. An administrator reviews the release before it is sent.",
		"Release 0.5 XMR (test network) to "+p.vendor.Handle+" — 0.499999999 XMR more than the price; "+cause+", so the amount paid over it cannot be returned separately; an administrator reviews the release before it is sent; network fee deducted. This is final.",
		"The network fee is deducted from each payout, the release to the vendor included.")
	mustContain(t, html.UnescapeString(p.page("/order?id="+order, p.vendorSess)), "Completing releases everything to you: "+cause, "The network fee is deducted from each payout, your release included.")
	p.check(p.do("POST", "/orders/complete", p.buyerSess, payoutConfirmed(url.Values{"order_id": {order}}, paid)), 303)
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", order); n != 1 {
		t.Fatalf("payouts %d, want one release", n)
	}
	if st, errText := p.str("SELECT state FROM payouts WHERE order_id=$1", order), p.str("SELECT error FROM payouts WHERE order_id=$1", order); st != "held" || errText != largeHold {
		t.Fatalf("release %s %q, want held with largeHold", st, errText)
	}
	for _, u := range []*User{p.buyer, p.vendor} {
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%'||$2||'%'", u.ID, cause); n != 1 {
			t.Fatalf("%s not told the price is the cause: %d", u.Handle, n)
		}
	}
	p.poll()
	if s := xf.Sends(); len(s) != 0 {
		t.Fatalf("held release sent: %+v", s)
	}
}

// The real Monero adapter sends a release through sendPayouts with subtract_fee_from_outputs (httptest wallet).
func TestMoneroReleaseTransferSubtractsFee(t *testing.T) {
	p := newPayEnv(t)
	g, _ := gapXMR(t, p, 1)
	if _, err := p.DB.Exec("UPDATE users SET payout_xmr=$1 WHERE id=$2", payStagenetSub, p.vendor.ID); err != nil {
		t.Fatal(err)
	}
	addr := "7gapxmrrelease" + strings.Repeat("r", 81)
	g.subaddress(addr, 40)
	order := p.gapOrder("XMR", addr) // priced at minPayoutXMR
	g.deposit(addr, "xmr-release", minPayoutXMR, 1)
	p.poll()
	p.complete(order)
	p.poll()
	if st, _, _, _ := p.payout(order); st != "sent" {
		t.Fatalf("release %s", st)
	}
	params := string(g.s.lastParams("transfer"))
	if !strings.Contains(params, `"subtract_fee_from_outputs":[0]`) || !strings.Contains(params, `"amount":1000000000`) {
		t.Fatalf("release transfer params %s", params)
	}
}
