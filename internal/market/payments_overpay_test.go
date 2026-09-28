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
	p.complete(p.order)
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
	p.complete(p.order)
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
	p.complete(p.order)
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
