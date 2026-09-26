package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// The XMR money path end to end through the HTTP handlers with the deterministic fake wallet: payout-address
// save (password-confirmed), payment request, deposits in 12-decimal amounts, the confirmation threshold,
// release and refund payouts in exact atomic units and their formatting on every page that shows them.

// xmrPrice is 1.234567890123 XMR: every one of the 12 decimals is significant.
const xmrPrice = int64(1234567890123)

func TestGapXMRMoneyPathEndToEnd(t *testing.T) {
	e := newTestApp(t)
	fake := newFakeProvider("XMR", 10)
	e.A.payments["XMR"] = fake
	l := &lifecycle{testEnv: e, t: t, fake: fake}
	suffix := randomToken()[:6]
	vendorID, vendor := e.user("xvendor_"+suffix, "vendor")
	buyerID, buyer := e.user("xbuyer_"+suffix, "buyer")
	_, admin := e.user("xadmin_"+suffix, "admin")
	product := e.product(vendorID, "physical")
	if _, err := e.DB.Exec("UPDATE products SET xmr=$1 WHERE id=$2", xmrPrice, product); err != nil {
		t.Fatal(err)
	}
	payoutXMR := func(id string) string {
		var a, b string
		if err := e.DB.QueryRow("SELECT payout_xmr,payout_btc FROM users WHERE id=$1", id).Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		if b != "" {
			t.Fatalf("BTC payout address changed by an XMR request: %q", b)
		}
		return a
	}
	savePayout := func(sess, addr, password string) (int, string) {
		w := e.do("POST", "/account/payout", sess, url.Values{"currency": {"XMR"}, "address": {addr}, "password": {password}})
		return w.Code, w.Body.String()
	}

	// Payout address: only the XMR form is offered; invalid addresses are refused even with the right
	// password, a missing or wrong password refuses a valid one, and nothing is stored until all pass.
	account := l.get("/account", vendor, 200)
	mustContain(t, account, "Monero payout address", "TESTNET fake", "Bitcoin payouts are unavailable")
	for _, bad := range []string{payMainnetPrimary, payStagenetSub, "fake-testnet-" + strings.Repeat("v", 60)} {
		if code, body := savePayout(vendor, bad, testPassword); code != 400 || !strings.Contains(body, "Enter a valid XMR address for the fake test network") {
			t.Fatalf("invalid XMR address %q: %d %s", bad, code, body)
		}
	}
	if code, body := savePayout(vendor, "fake-testnet-xvendor", ""); code != 400 || !strings.Contains(body, "Enter your current password") {
		t.Fatalf("no password: %d %s", code, body)
	}
	if code, _ := savePayout(vendor, "fake-testnet-xvendor", "wrong-password-here"); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	if got := payoutXMR(vendorID); got != "" {
		t.Fatalf("refused requests stored %q", got)
	}
	if code, body := savePayout(vendor, " fake-testnet-xvendor ", testPassword); code != 303 {
		t.Fatalf("valid save: %d %s", code, body)
	}
	if got := payoutXMR(vendorID); got != "fake-testnet-xvendor" {
		t.Fatalf("saved address %q", got)
	}
	if n := l.count("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Saved XMR payout address (TESTNET fake) (confirmed with password)'", vendorID); n != 1 {
		t.Fatalf("audit rows %d", n)
	}
	mustContain(t, l.get("/account", vendor, 200), `value="fake-testnet-xvendor"`, "XMR saved")

	// Draft, payment request and address.
	loc := l.post("/orders", buyer, form("product_id", product, "currency", "XMR"), 303)
	order := strings.TrimPrefix(loc, "/order?id=")
	l.post("/orders/pay", buyer, form("order_id", order), 303)
	var addr, provider string
	var total int64
	if err := e.DB.QueryRow("SELECT pa.address,pa.provider,o.amount FROM payment_addresses pa JOIN orders o ON o.id=pa.order_id WHERE o.id=$1 AND o.currency='XMR'", order).Scan(&addr, &provider, &total); err != nil {
		t.Fatal(err)
	}
	if addr != "fake-testnet-"+order[:8] || provider != "xmr-fake" || total != xmrPrice || l.state(order) != stateAwaitingPayment {
		t.Fatalf("payment request: %s %s %d %s", addr, provider, total, l.state(order))
	}
	mustContain(t, l.get("/order?id="+order, buyer, 200), "1.234567890123 XMR", addr, "Waiting for a deposit")

	// Two outputs: 1.000000000001 + 0.234567890122 = 1.234567890123. Nine confirmations are not enough.
	fake.Deposit(addr, "xmr-dep-a", 0, 1000000000001, 9)
	fake.Deposit(addr, "xmr-dep-b", 1, 234567890122, 10)
	l.poll()
	if s := l.state(order); s != stateAwaitingPayment {
		t.Fatalf("paid with one output at 9 of 10 confirmations: %s", s)
	}
	mustContain(t, l.get("/order?id="+order, buyer, 200),
		"Deposit seen; waiting for 10 confirmations", "1.000000000001 XMR", "0.234567890122 XMR", "xmr-dep-a:0", "xmr-dep-b:1")
	fake.SetConfirmations("xmr-dep-a", 10)
	l.poll()
	if s := l.state(order); s != statePaid {
		t.Fatalf("not paid at the threshold: %s", s)
	}
	if n := l.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND to_state='paid' AND note='TESTNET fake payment confirmed: 1.234567890123 XMR in 2 output(s) with at least 10 confirmations'", order); n != 1 {
		t.Fatal("paid event does not carry the exact XMR amount")
	}
	if n := l.count("SELECT count(*) FROM payments WHERE order_id=$1 AND credited AND currency='XMR' AND amount IN (1000000000001,234567890122)", order); n != 2 {
		t.Fatalf("credited ledger rows: %d", n)
	}

	// Release: exact atomic units queued, sent once, and formatted with all 12 decimals.
	l.post("/orders/ship", vendor, form("order_id", order), 303)
	l.post("/orders/complete", buyer, payoutConfirmed(form("order_id", order), amountSeen(t, l.get("/order?id="+order, buyer, 200))[0]), 303)
	var kind, to, state string
	var amt int64
	if err := e.DB.QueryRow("SELECT kind,address,state,amount FROM payouts WHERE order_id=$1 AND currency='XMR'", order).Scan(&kind, &to, &state, &amt); err != nil {
		t.Fatal(err)
	}
	if kind != "release" || to != "fake-testnet-xvendor" || state != "pending" || amt != xmrPrice {
		t.Fatalf("queued release %s %s %s %d", kind, to, state, amt)
	}
	if n := l.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note='Queued TESTNET release of 1.234567890123 XMR to the vendor (single attempt).'", order); n != 1 {
		t.Fatal("queue event does not carry the exact XMR amount")
	}
	l.poll()
	l.poll()
	sends := fake.Sends()
	if len(sends) != 1 || sends[0].To != "fake-testnet-xvendor" || sends[0].Amount != xmrPrice {
		t.Fatalf("XMR sends %+v", sends)
	}
	if n := l.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%TESTNET payout of 1.234567890123 XMR sent: "+sends[0].TxID+"'", vendorID); n != 1 {
		t.Fatal("vendor not notified of the exact XMR payout")
	}
	mustContain(t, l.get("/order?id="+order, vendor, 200), "Release to vendor", "1.234567890123 XMR", sends[0].TxID)
	mustContain(t, l.get("/admin", admin, 200), "1.234567890123 XMR", "TESTNET fake", sends[0].TxID)

	// Refund: a confirmed partial deposit on a cancelled order is refunded in exact units; the refund is
	// blocked until the buyer saves an XMR payout address and then sent once.
	loc = l.post("/orders", buyer, form("product_id", product, "currency", "XMR"), 303)
	second := strings.TrimPrefix(loc, "/order?id=")
	l.post("/orders/pay", buyer, form("order_id", second), 303)
	var addr2 string
	if err := e.DB.QueryRow("SELECT address FROM payment_addresses WHERE order_id=$1", second).Scan(&addr2); err != nil {
		t.Fatal(err)
	}
	fake.Deposit(addr2, "xmr-partial", 0, 700000000001, 10)
	l.poll()
	if s := l.state(second); s != stateAwaitingPayment {
		t.Fatalf("partial deposit: %s", s)
	}
	mustContain(t, l.get("/order?id="+second, buyer, 200), "Confirmed deposits are below the required amount", "0.700000000001 XMR")
	l.post("/orders/cancel", buyer, form("order_id", second, "from", stateAwaitingPayment), 303)
	if err := e.DB.QueryRow("SELECT kind,address,state,amount FROM payouts WHERE order_id=$1", second).Scan(&kind, &to, &state, &amt); err != nil {
		t.Fatal(err)
	}
	if kind != "refund" || to != "" || state != "blocked" || amt != 700000000001 {
		t.Fatalf("blocked refund %s %q %s %d", kind, to, state, amt)
	}
	if n := l.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%add a XMR payout address on your account page to receive 0.700000000001 XMR (TESTNET)%'", buyerID); n != 1 {
		t.Fatal("buyer not told to add an XMR payout address")
	}
	l.poll()
	if len(fake.Sends()) != 1 {
		t.Fatal("blocked refund sent")
	}
	mustContain(t, l.get("/account", buyer, 200), "1 payout(s) are waiting for a payout address")
	if code, body := savePayout(buyer, "fake-testnet-xbuyer", testPassword); code != 303 {
		t.Fatalf("buyer save: %d %s", code, body)
	}
	if n := l.count("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Saved XMR payout address (TESTNET fake); 1 waiting payout(s) now use it (confirmed with password)'", buyerID); n != 1 {
		t.Fatal("buyer audit row")
	}
	if err := e.A.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	sends = fake.Sends()
	if len(sends) != 2 || sends[1].To != "fake-testnet-xbuyer" || sends[1].Amount != 700000000001 {
		t.Fatalf("refund sends %+v", sends)
	}
	mustContain(t, l.get("/order?id="+second, buyer, 200), "Refund to buyer", "0.700000000001 XMR", sends[1].TxID)
}
