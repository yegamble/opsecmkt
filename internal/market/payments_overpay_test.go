package market

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"rsc.io/qr"
)

// Overpayments are returned accurately without letting anyone profit from the market's wallet: a release pays the
// vendor exactly the price and returns the excess to the buyer with the network fee taken from it; an excess
// below the minimum automatic refund goes to the vendor; a refund or return larger than the price waits for an
// administrator (the pooled wallet must not become a coin swap); refunds and returns always pay their own fee.

func TestSplitPayoutRules(t *testing.T) {
	o := &Order{BuyerID: "b", VendorID: "v", Currency: "BTC"}
	const price = 100000
	for _, tc := range []struct {
		name, kind, currency string
		sum                  int64
		want                 []payoutPart
	}{
		{"exact release", "release", "BTC", price, []payoutPart{{"release", "v", "vendor", price, false}}},
		{"underpaid release", "release", "BTC", 90000, []payoutPart{{"release", "v", "vendor", 90000, false}}},
		{"excess below the floor stays with the vendor", "release", "BTC", price + minPayoutBTC - 1, []payoutPart{{"release", "v", "vendor", price + minPayoutBTC - 1, false}}},
		{"excess at the floor is returned", "release", "BTC", price + minPayoutBTC, []payoutPart{{"release", "v", "vendor", price, false}, {"return", "b", "buyer", minPayoutBTC, false}}},
		{"excess equal to the price is returned", "release", "BTC", 2 * price, []payoutPart{{"release", "v", "vendor", price, false}, {"return", "b", "buyer", price, false}}},
		{"excess above the price is held", "release", "BTC", 2*price + 1, []payoutPart{{"release", "v", "vendor", price, false}, {"return", "b", "buyer", price + 1, true}}},
		{"refund up to the price", "refund", "BTC", price, []payoutPart{{"refund", "b", "buyer", price, false}}},
		{"refund above the price is held", "refund", "BTC", price + 1, []payoutPart{{"refund", "b", "buyer", price + 1, true}}},
		{"XMR excess below its floor stays with the vendor", "release", "XMR", price + minAutoRefundXMR - 1, []payoutPart{{"release", "v", "vendor", price + minAutoRefundXMR - 1, false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o.Currency = tc.currency
			got := splitPayout(tc.kind, o, tc.sum, price)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v", got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("part %d: got %+v want %+v", i, got[i], tc.want[i])
				}
			}
			var total int64
			for _, part := range got {
				total += part.amount
			}
			if total != tc.sum {
				t.Fatalf("parts pay %d of %d", total, tc.sum)
			}
		})
	}
}

// An overpayment worth returning: the vendor gets the price with Send, the buyer the excess with the fee taken from it.
func TestOverpaymentReturnedToBuyer(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.paidByWatcherAmount(150000)
	// A-215: the shipped order's page says, for this order, that completing returns the excess (fee deducted).
	buyerPage, vendorPage := p.overpaidPages()
	mustContain(t, buyerPage, "You sent 0.0005 BTC more than the price (test network). Completing releases the price to the vendor and returns the excess to your saved BTC payout address, with the network fee deducted.",
		"Release 0.001 BTC (test network) to "+p.vendor.Handle+" (the price) and return the 0.0005 BTC paid over the price to "+p.buyer.Handle+", network fee deducted. This is final.")
	mustNotContain(t, buyerPage, "the return waits", "below the minimum automatic refund", "administrator reviews its return")
	mustContain(t, vendorPage, "The buyer sent 0.0005 BTC more than the price (test network). Completing releases the price to you and returns the excess to the buyer, with the network fee deducted.")
	mustNotContain(t, vendorPage, "your saved", "payout address saved", "the return waits")
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	if st, _, addr, amt := p.payout(p.order); st != "pending" || addr != "fake-testnet-vendor" || amt != 100000 {
		t.Fatalf("release %s %s %d", st, addr, amt)
	}
	if st, errText, amt := p.returnPayout(p.order); st != "pending" || errText != "" || amt != 50000 {
		t.Fatalf("return %s %q %d", st, errText, amt)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%0.0005 BTC you paid over the price is being returned%'", p.buyer.ID); n != 1 {
		t.Fatalf("buyer return notices %d", n)
	}
	p.poll()
	sends := p.fake.Sends()
	if len(sends) != 2 {
		t.Fatalf("sends %+v", sends)
	}
	for _, s := range sends {
		switch s.To {
		case "fake-testnet-vendor":
			if s.Amount != 100000 || s.FeeFromAmount {
				t.Fatalf("release sent %+v", s)
			}
		case "fake-testnet-buyer":
			if s.Amount != 50000 || !s.FeeFromAmount {
				t.Fatalf("return sent %+v", s)
			}
		default:
			t.Fatalf("unexpected send %+v", s)
		}
	}
	// Repeated passes send nothing more; the buyer sees the return on the order page.
	p.poll()
	if len(p.fake.Sends()) != 2 {
		t.Fatal("payouts sent twice")
	}
	mustContain(t, p.page("/order?id="+p.order, p.buyerSess), "Return of the amount paid over the price", "Return to buyer", "Release to vendor")
}

// A dust excess is not sent back (it would cost more than it is worth); the vendor receives it with the release.
func TestSmallExcessStaysWithVendor(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.paidByWatcherAmount(100000 + minPayoutBTC - 1)
	// A-215: the page names the floor and says the excess is not returned; nothing waits for a payout address.
	buyerPage, vendorPage := p.overpaidPages()
	mustContain(t, buyerPage, "You sent 0.00009999 BTC more than the price (test network). Completing releases everything to the vendor: the excess is below the minimum automatic refund of 0.0001 BTC, so it is not returned.",
		"Release 0.00109999 BTC (test network) to "+p.vendor.Handle+" — 0.00009999 BTC more than the price, below the minimum automatic refund, so it is not returned. This is final.")
	mustNotContain(t, buyerPage, "returns the excess", "the return waits", "your saved")
	mustContain(t, vendorPage, "The buyer sent 0.00009999 BTC more than the price (test network). Completing releases everything to you: the excess is below the minimum automatic refund of 0.0001 BTC, so it is not returned.")
	mustNotContain(t, vendorPage, "returns the excess", "your saved")
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	if st, _, _, amt := p.payout(p.order); st != "pending" || amt != 100000+minPayoutBTC-1 {
		t.Fatalf("release %s %d", st, amt)
	}
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 1 {
		t.Fatalf("payouts %d", n)
	}
	for _, u := range []*User{p.buyer, p.vendor} {
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%below the minimum automatic refund of 0.0001 BTC, so it is not returned%'", u.ID); n != 1 {
			t.Fatalf("%s not told the excess is kept: %d", u.Handle, n)
		}
	}
}

// A return larger than the price is never sent automatically: administrators are told, and releasing it needs
// an explicit review confirmation.
func TestLargeReturnHeldUntilAdminReview(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	adminID := p.str("SELECT id FROM users WHERE role='admin'")
	p.paidByWatcherAmount(350000)
	// A-215: the page says the return waits for an administrator, not that it goes back automatically.
	buyerPage, vendorPage := p.overpaidPages()
	mustContain(t, buyerPage, "You sent 0.0025 BTC more than the price (test network). Completing releases the price to the vendor. The excess is more than the price, so an administrator reviews its return to your saved BTC payout address (network fee deducted) before it is sent.",
		"return the 0.0025 BTC paid over the price to "+p.buyer.Handle+", network fee deducted; the return is more than the price, so an administrator reviews it before it is sent. This is final.")
	mustNotContain(t, buyerPage, "returns the excess", "the return waits")
	mustContain(t, vendorPage, "The buyer sent 0.0025 BTC more than the price (test network). Completing releases the price to you. The excess is more than the price, so an administrator reviews its return to the buyer (network fee deducted) before it is sent.")
	mustNotContain(t, vendorPage, "returns the excess", "your saved")
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	if st, errText, amt := p.returnPayout(p.order); st != "held" || errText != largeHold || amt != 250000 {
		t.Fatalf("return %s %q %d", st, errText, amt)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Payout review needed for order%more than the order price%'", adminID); n != 1 {
		t.Fatalf("administrator notices %d", n)
	}
	p.poll()
	p.poll()
	if s := p.fake.Sends(); len(s) != 1 || s[0].To != "fake-testnet-vendor" {
		t.Fatalf("held return sent: %+v", s)
	}
	admin := p.page("/admin", p.adminSess)
	mustContain(t, admin, "Held: larger than the order price — review before releasing", "Return of overpayment", `name="large_reviewed" value="confirmed" required`)
	id := p.str("SELECT id::text FROM payouts WHERE order_id=$1 AND kind='return'", p.order)
	release := url.Values{"payout_id": {id}, "op": {"release"}, "password": {testPassword}}
	if w := p.do("POST", "/admin/payout", p.adminSess, release); w.Code != 409 || !strings.Contains(w.Body.String(), "larger than the order price") {
		t.Fatalf("release without review: %d", w.Code)
	}
	if st, _, _ := p.returnPayout(p.order); st != "held" {
		t.Fatalf("unreviewed release changed the return: %s", st)
	}
	release.Set("large_reviewed", "confirmed")
	p.check(p.do("POST", "/admin/payout", p.adminSess, release), 303)
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE '%larger than the order price, administrator reviewed it%'"); n != 1 {
		t.Fatalf("audit rows %d", n)
	}
	p.poll()
	if st, _, amt := p.returnPayout(p.order); st != "sent" || amt != 250000 {
		t.Fatalf("return after review %s %d", st, amt)
	}
	if s := p.fake.Sends(); len(s) != 2 || s[1].To != "fake-testnet-buyer" || !s[1].FeeFromAmount {
		t.Fatalf("sends %+v", s)
	}
}

// Suspending the recipient keeps the large hold: releasing needs both the address check and the review.
func TestSuspensionKeepsLargeHold(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.paidByWatcherAmount(100000)
	p.move(p.order, statePaid, stateCancelled, p.vendor)
	if _, err := p.DB.Exec("UPDATE payouts SET amount=150000,state='held',error=$2 WHERE order_id=$1", p.order, largeHold); err != nil {
		t.Fatal(err)
	}
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.buyer.Handle)), 303)
	if st, _, _, _ := p.payout(p.order); st != "held" {
		t.Fatalf("state %s", st)
	}
	if got := p.str("SELECT error FROM payouts WHERE order_id=$1", p.order); got != suspendedHold+" "+largeHold {
		t.Fatalf("hold %q", got)
	}
	id := p.payoutID(p.order)
	release := url.Values{"payout_id": {id}, "op": {"release"}, "password": {testPassword}, "address_checked": {"confirmed"}}
	if w := p.do("POST", "/admin/payout", p.adminSess, release); w.Code != 409 || !strings.Contains(w.Body.String(), "larger than the order price") {
		t.Fatalf("release with only the address check: %d", w.Code)
	}
	release.Del("address_checked")
	release.Set("large_reviewed", "confirmed")
	if w := p.do("POST", "/admin/payout", p.adminSess, release); w.Code != 409 || !strings.Contains(w.Body.String(), "suspended") {
		t.Fatalf("release with only the review: %d", w.Code)
	}
	release.Set("address_checked", "confirmed")
	p.check(p.do("POST", "/admin/payout", p.adminSess, release), 303)
	if st, _, _, _ := p.payout(p.order); st != "pending" {
		t.Fatalf("state after both checks %s", st)
	}
}

// Refunds pay their own network fee, so repeated pay-and-cancel costs the market nothing.
func TestRefundIsSentWithFeeFromAmount(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.fake.Deposit(p.addr, "tx-part", 0, 40000, 3)
	p.poll()
	p.check(p.do("POST", "/orders/cancel", p.buyerSess, payoutConfirmed(form("order_id", p.order, "from", stateAwaitingPayment), 40000)), 303)
	p.poll()
	if s := p.fake.Sends(); len(s) != 1 || s[0].Amount != 40000 || !s[0].FeeFromAmount {
		t.Fatalf("refund sends %+v", s)
	}
}

// The payout kinds are constrained by the schema: one of each per order.
func TestPayoutKindsPerOrder(t *testing.T) {
	p := newPayEnv(t)
	insert := func(kind string) error {
		_, err := p.DB.Exec("INSERT INTO payouts(order_id,kind,user_id,currency,amount) VALUES($1,$2,$3,'BTC',1000)", p.order, kind, p.buyer.ID)
		return err
	}
	if err := insert("release"); err != nil {
		t.Fatal(err)
	}
	if err := insert("return"); err != nil {
		t.Fatal(err)
	}
	if err := insert("return"); err == nil {
		t.Fatal("second return for one order accepted")
	}
	if err := insert("bonus"); err == nil {
		t.Fatal("unknown payout kind accepted")
	}
}

// A-215: without a payout address in the order's currency the buyer is told the return waits until they save one;
// the vendor is not told about the buyer's addresses.
func TestOverpaidAlertWithoutBuyerPayoutAddress(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	if _, err := p.DB.Exec("UPDATE users SET payout_xmr='fake-testnet-xmr' WHERE id=$1", p.buyer.ID); err != nil {
		t.Fatal(err) // an address in another currency does not count
	}
	p.paidByWatcherAmount(150000)
	buyerPage, vendorPage := p.overpaidPages()
	mustContain(t, buyerPage, "Completing releases the price to the vendor and returns the excess to you, with the network fee deducted. You have no BTC payout address saved, so the return waits until you save one:",
		`<a class="text-link" href="/account#payout">save a payout address</a>`)
	mustNotContain(t, buyerPage, "your saved BTC payout address")
	mustContain(t, vendorPage, "Completing releases the price to you and returns the excess to the buyer, with the network fee deducted.")
	mustNotContain(t, vendorPage, "your saved", "payout address saved", "the return waits")
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	if st, _, amt := p.returnPayout(p.order); st != "blocked" || amt != 50000 {
		t.Fatalf("return %s %d", st, amt)
	}
}

// A-215, Monero: the alert names the XMR floor, and a Bitcoin payout address does not count for an XMR return.
func TestOverpaidXMRAlertNamesItsFloorAndAddress(t *testing.T) {
	p := newPayEnv(t)
	xf := newFakeProvider("XMR", 10)
	p.A.payments["XMR"] = xf
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	for _, tc := range []struct {
		deposit    int64
		want, deny string
	}{
		{500000000000 + minAutoRefundXMR - 1, "Completing releases everything to the vendor: the excess is below the minimum automatic refund of 0.001 XMR, so it is not returned.", "payout address saved"},
		{500000000000 + minAutoRefundXMR, "returns the excess to you, with the network fee deducted. You have no XMR payout address saved, so the return waits until you save one:", "your saved"},
	} {
		id := p.testEnv.order(p.buyer.ID, p.productID, "XMR", stateAwaitingPayment)
		addr, err := xf.NewAddress(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = p.DB.Exec("UPDATE orders SET amount=500000000000 WHERE id=$1", id); err != nil {
			t.Fatal(err)
		}
		if _, err = p.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'XMR',$2,'fake')", id, addr); err != nil {
			t.Fatal(err)
		}
		xf.Deposit(addr, "xmr-"+strconv.FormatInt(tc.deposit, 10), 0, tc.deposit, 10)
		p.poll()
		p.move(id, statePaid, stateShipped, p.vendor)
		page := p.page("/order?id="+id, p.buyerSess)
		mustContain(t, page, tc.want)
		mustNotContain(t, page, tc.deny, "whole counted amount")
	}
}

// overpaidPages ships the paid fixture order and returns its buyer's and vendor's order pages; the buyer's shows
// the complete form. Neither says completing releases the whole counted amount (A-215).
func (p *payEnv) overpaidPages() (buyer, vendor string) {
	p.t.Helper()
	p.move(p.order, statePaid, stateShipped, p.vendor)
	buyer, vendor = p.page("/order?id="+p.order, p.buyerSess), p.page("/order?id="+p.order, p.vendorSess)
	mustContain(p.t, buyer, "Confirms you received the order. Completing queues the payouts stated below; the payment watcher sends them automatically unless the statement says otherwise.", `name="payout_confirmed"`)
	for _, page := range []string{buyer, vendor} {
		mustNotContain(p.t, page, "whole counted amount")
	}
	return buyer, vendor
}

// paidByWatcherAmount pays the fixture order with one confirmed deposit of amt satoshi.
func (p *payEnv) paidByWatcherAmount(amt int64) {
	p.t.Helper()
	p.fake.Deposit(p.addr, "tx-paid-"+strconv.FormatInt(amt, 10), 0, amt, 3)
	p.poll()
	if s := p.state(p.order); s != statePaid {
		p.t.Fatalf("order not paid: %s", s)
	}
}

func TestPaymentURI(t *testing.T) {
	if got := paymentURI("BTC", "tb1qdeposit", 60000); got != "bitcoin:tb1qdeposit?amount=0.0006" {
		t.Fatal(got)
	}
	if got := paymentURI("BTC", "tb1qdeposit", 100000000); got != "bitcoin:tb1qdeposit?amount=1" {
		t.Fatal(got)
	}
	if got := paymentURI("XMR", "7abc", 500000000001); got != "monero:7abc?tx_amount=0.500000000001" {
		t.Fatal(got)
	}
}

// The SVG draws exactly the encoder's dark modules, offset by the quiet zone, on a white square.
func TestQRSVGMatchesEncoding(t *testing.T) {
	text := "bitcoin:tb1qorderaddress000000000000000000000?amount=0.0006"
	svg, err := qrSVG(text, `pay "0.0006" <BTC>`)
	if err != nil {
		t.Fatal(err)
	}
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		t.Fatal(err)
	}
	s := string(svg)
	n := code.Size + 2*qrQuiet
	if !strings.Contains(s, `viewBox="0 0 `+strconv.Itoa(n)+` `+strconv.Itoa(n)+`"`) || !strings.Contains(s, `aria-label="pay &#34;0.0006&#34; &lt;BTC&gt;"`) || !strings.Contains(s, `fill="#fff"`) {
		t.Fatalf("svg header %.200s", s)
	}
	d := regexp.MustCompile(` d="([^"]*)"`).FindStringSubmatch(s)
	if d == nil {
		t.Fatal("no path")
	}
	drawn := map[[2]int]bool{}
	for _, m := range regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-(\d+)z`).FindAllStringSubmatch(d[1], -1) {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		w, _ := strconv.Atoi(m[3])
		if m[3] != m[4] {
			t.Fatalf("run %q not closed", m[0])
		}
		for i := range w {
			drawn[[2]int{x + i, y}] = true
		}
	}
	for y := range n {
		for x := range n {
			want := x >= qrQuiet && y >= qrQuiet && x < qrQuiet+code.Size && y < qrQuiet+code.Size && code.Black(x-qrQuiet, y-qrQuiet)
			if drawn[[2]int{x, y}] != want {
				t.Fatalf("module %d,%d drawn=%v want %v", x, y, drawn[[2]int{x, y}], want)
			}
		}
	}
}

// The order page shows the buyer a QR code for exactly the amount still due, and nothing once it is covered.
func TestOrderPageQRForAmountDue(t *testing.T) {
	p := newPayEnv(t)
	uri := func(due string) string { return "bitcoin:" + p.addr + "?amount=" + due }
	page := p.page("/order?id="+p.order, p.buyerSess)
	mustContain(t, page, `<figure class="payment-qr"><svg`, uri("0.001"), "Scan to pay exactly <strong>0.001 BTC</strong>")
	mustNotContain(t, p.page("/order?id="+p.order, p.vendorSess), `class="payment-qr"`)
	// A deposit seen (even unconfirmed) lowers the amount asked for.
	p.fake.Deposit(p.addr, "tx-part", 0, 40000, 1)
	p.poll()
	page = p.page("/order?id="+p.order, p.buyerSess)
	mustContain(t, page, uri("0.0006"), "Scan to pay exactly <strong>0.0006 BTC</strong>")
	mustNotContain(t, page, uri("0.001"))
	svg := strings.TrimPrefix(regexp.MustCompile(`(?s)<figure class="payment-qr"><svg.*?</svg>`).FindString(page), `<figure class="payment-qr">`)
	want, err := qrSVG(uri("0.0006"), "QR code: pay 0.0006 BTC to the deposit address")
	if err != nil || svg != string(want) {
		t.Fatalf("page QR code does not encode the amount due:\npage %.200s\nwant %.200s", svg, want)
	}
	// Once the full amount is seen, no QR code invites a second payment.
	p.fake.Deposit(p.addr, "tx-rest", 0, 60000, 1)
	p.poll()
	mustNotContain(t, p.page("/order?id="+p.order, p.buyerSess), `class="payment-qr"`, "bitcoin:")
}

func TestMoneroRefundSubtractsFee(t *testing.T) {
	ctx := context.Background()
	w, s := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	w.valid[payStagenetSub] = "stagenet"
	p, err := newMoneroProvider(ctx, s.url(""), "", "stagenet", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Send(ctx, payStagenetSub, 42); err != nil || strings.Contains(string(s.lastParams("transfer")), "subtract_fee_from_outputs") {
		t.Fatalf("release subtracts the fee: %s %v", s.lastParams("transfer"), err)
	}
	if _, err = p.SendFeeFromAmount(ctx, payStagenetSub, 42); err != nil || !strings.Contains(string(s.lastParams("transfer")), `"subtract_fee_from_outputs":[0]`) {
		t.Fatalf("refund params %s %v", s.lastParams("transfer"), err)
	}
	var _ feeFromAmountSender = p
	var _ feeFromAmountSender = &bitcoinProvider{}
}

// A restore from backup keeps the large hold behind its own marker: releasing the restored payout still needs
// the review as well as the "no broadcast" wallet check.
func TestRestoreKeepsLargeHold(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.paidByWatcherAmount(350000)
	p.complete(p.order)
	for _, q := range restorePayoutSQL {
		if _, err := p.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if st, errText, _ := p.returnPayout(p.order); st != "held" || errText != restoredHold+" "+largeHold {
		t.Fatalf("restored return %s %q", st, errText)
	}
	id := p.str("SELECT id::text FROM payouts WHERE order_id=$1 AND kind='return'", p.order)
	release := url.Values{"payout_id": {id}, "op": {"release"}, "password": {testPassword}, "not_broadcast": {"confirmed"}}
	if w := p.do("POST", "/admin/payout", p.adminSess, release); w.Code != 409 || !strings.Contains(w.Body.String(), "larger than the order price") {
		t.Fatalf("restored large return released without review: %d", w.Code)
	}
	mustContain(t, p.page("/admin", p.adminSess), `name="large_reviewed" value="confirmed" required`, `name="not_broadcast" value="confirmed" required`)
	release.Set("large_reviewed", "confirmed")
	p.check(p.do("POST", "/admin/payout", p.adminSess, release), 303)
	if st, _, _ := p.returnPayout(p.order); st != "pending" {
		t.Fatalf("state after both checks %s", st)
	}
}
