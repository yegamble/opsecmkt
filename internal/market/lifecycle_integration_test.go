package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// Cross-package lifecycle (P3 orders + P4 listings + P5 payments): every step goes through the HTTP
// handlers except deposits, which come from the deterministic fake wallet, and watcher passes (pollOnce).

type lifecycle struct {
	*testEnv
	t    *testing.T
	fake *fakeProvider
}

func (l *lifecycle) post(path, session string, form url.Values, want int) string {
	l.t.Helper()
	w := l.do("POST", path, session, form)
	if w.Code != want {
		l.t.Fatalf("POST %s: status=%d want=%d body=%q", path, w.Code, want, w.Body.String())
	}
	return w.Header().Get("Location")
}

func (l *lifecycle) get(path, session string, want int) string {
	l.t.Helper()
	w := l.do("GET", path, session, nil)
	if w.Code != want {
		l.t.Fatalf("GET %s: status=%d want=%d", path, w.Code, want)
	}
	return w.Body.String()
}

func (l *lifecycle) poll() {
	l.t.Helper()
	if err := l.A.pollOnce(context.Background()); err != nil {
		l.t.Fatalf("pollOnce: %v", err)
	}
}

func (l *lifecycle) state(order string) string {
	l.t.Helper()
	var s string
	if err := l.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&s); err != nil {
		l.t.Fatal(err)
	}
	return s
}

func (l *lifecycle) count(query string, args ...any) int {
	l.t.Helper()
	var n int
	if err := l.DB.QueryRow(query, args...).Scan(&n); err != nil {
		l.t.Fatal(err)
	}
	return n
}

// listing creates a listing through /listings and returns its id.
func (l *lifecycle) listing(vendorID, vendorSess, title, kind, category, delivery string) string {
	l.t.Helper()
	l.post("/listings", vendorSess, form("title", title, "description", "Lifecycle fixture", "category", category, "region", "Worldwide",
		"kind", kind, "price_btc", "0.001", "price_xmr", "0.5", "stock", "3", "delivery_content", delivery), 303)
	var id string
	if err := l.DB.QueryRow("SELECT id FROM products WHERE vendor_id=$1 AND title=$2", vendorID, title).Scan(&id); err != nil {
		l.t.Fatal(err)
	}
	return id
}

// draftAndPay creates a BTC draft through /orders and requests payment through /orders/pay.
func (l *lifecycle) draftAndPay(buyerSess, productID string) (order, addr string) {
	l.t.Helper()
	loc := l.post("/orders", buyerSess, form("product_id", productID, "currency", "BTC"), 303)
	order = strings.TrimPrefix(loc, "/order?id=")
	if order == loc || l.state(order) != stateDraft {
		l.t.Fatalf("draft not created: %q", loc)
	}
	l.post("/orders/pay", buyerSess, form("order_id", order), 303)
	if s := l.state(order); s != stateAwaitingPayment {
		l.t.Fatalf("after /orders/pay: %s", s)
	}
	if err := l.DB.QueryRow("SELECT address FROM payment_addresses WHERE order_id=$1", order).Scan(&addr); err != nil {
		l.t.Fatal(err)
	}
	return order, addr
}

func TestLifecycleDigitalAndDisputedPhysical(t *testing.T) {
	e := newTestApp(t)
	l := &lifecycle{testEnv: e, t: t, fake: newFakeProvider("BTC", 3)}
	e.A.payments["BTC"] = l.fake
	suffix := randomToken()[:6]
	vendorID, vendor := e.user("lvendor_"+suffix, "vendor")
	buyerID, buyer := e.user("lbuyer_"+suffix, "buyer")
	_, other := e.user("lother_"+suffix, "buyer")
	_, mod := e.user("lmod_"+suffix, "moderator")
	const vendorAddr, buyerAddr = "fake-testnet-vendor-payout", "fake-testnet-buyer-payout"
	const secret = "LICENCE-KEY-7Q2M-lifecycle"

	// Vendor lists a digital item with automatic delivery content and saves a payout address.
	digital := l.listing(vendorID, vendor, "Lifecycle digital item", "digital", "Digital", secret)
	l.post("/account/payout", vendor, form("currency", "BTC", "address", vendorAddr), 303)
	if strings.Contains(l.get("/product?id="+digital, other, 200), secret) {
		t.Fatal("delivery content rendered on the public product page")
	}

	// Buyer drafts and requests payment: the provider's address is shown with a TESTNET label.
	order, addr := l.draftAndPay(buyer, digital)
	page := l.get("/order?id="+order, buyer, 200)
	for _, want := range []string{addr, "TESTNET fake deposit address", "Waiting for a deposit"} {
		if !strings.Contains(page, want) {
			t.Fatalf("order page missing %q", want)
		}
	}
	if n := l.count("SELECT stock FROM products WHERE id=$1", digital); n != 2 {
		t.Fatalf("stock after payment request: %d", n)
	}

	// Below the confirmation threshold the order keeps waiting; at the threshold one pass marks it paid and
	// auto-delivers the listing content as the system actor.
	l.fake.Deposit(addr, "tx-digital", 0, 100000, 1)
	l.poll()
	if s := l.state(order); s != stateAwaitingPayment {
		t.Fatalf("paid below threshold: %s", s)
	}
	if !strings.Contains(l.get("/order?id="+order, buyer, 200), "Deposit seen; waiting for 3 confirmations") {
		t.Fatal("unconfirmed deposit status missing")
	}
	l.fake.SetConfirmations("tx-digital", 3)
	l.poll()
	if s := l.state(order); s != stateDelivered {
		t.Fatalf("after confirmed payment: %s", s)
	}
	if n := l.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND to_state IN ('paid','delivered') AND from_state<>to_state", order); n != 2 {
		t.Fatalf("system paid/delivered events: %d", n)
	}
	if !strings.Contains(l.get("/order?id="+order, buyer, 200), secret) {
		t.Fatal("buyer cannot see delivered content")
	}
	l.get("/order?id="+order, other, 404)
	l.get("/order?id="+order, mod, 404)

	// Completion queues exactly one release to the vendor's address; repeated passes never resend.
	l.post("/orders/complete", buyer, form("order_id", order), 303)
	l.post("/orders/complete", buyer, form("order_id", order), 409)
	l.poll()
	l.poll()
	sends := l.fake.Sends()
	if len(sends) != 1 || sends[0].To != vendorAddr || sends[0].Amount != 100000 {
		t.Fatalf("release sends: %+v", sends)
	}
	var kind, pstate, txid string
	if err := e.DB.QueryRow("SELECT kind,state,txid FROM payouts WHERE order_id=$1", order).Scan(&kind, &pstate, &txid); err != nil {
		t.Fatal(err)
	}
	if kind != "release" || pstate != "sent" || txid != sends[0].TxID {
		t.Fatalf("release payout: kind=%s state=%s txid=%s", kind, pstate, txid)
	}
	if !strings.Contains(l.get("/order?id="+order, vendor, 200), txid) {
		t.Fatal("payout transaction not shown to the vendor")
	}

	// One verified review per completed order.
	l.post("/reviews", buyer, form("order_id", order, "rating", "5", "body", "Key worked first time."), 303)
	l.post("/reviews", buyer, form("order_id", order, "rating", "1", "body", "Second attempt"), 409)
	if !strings.Contains(l.get("/product?id="+digital, other, 200), "Key worked first time.") {
		t.Fatal("review not published on the product page")
	}

	// Physical item: paid, disputed by the buyer, resolved by a moderator with a refund to the buyer.
	physical := l.listing(vendorID, vendor, "Lifecycle physical item", "physical", "Hardware", "")
	l.post("/account/payout", buyer, form("currency", "BTC", "address", buyerAddr), 303)
	order2, addr2 := l.draftAndPay(buyer, physical)
	l.fake.Deposit(addr2, "tx-physical", 1, 100000, 3)
	l.poll()
	if s := l.state(order2); s != statePaid {
		t.Fatalf("physical order after confirmed payment: %s", s)
	}
	l.post("/disputes", buyer, form("order_id", order2, "reason", "The parcel never arrived at the agreed drop."), 303)
	if s := l.state(order2); s != stateDisputed {
		t.Fatalf("after dispute: %s", s)
	}
	var disputeID string
	if err := e.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order2).Scan(&disputeID); err != nil {
		t.Fatal(err)
	}
	l.post("/resolve", mod, form("id", disputeID, "outcome", "refund", "resolution", "No proof of shipment; refund the buyer in full."), 303)
	if s := l.state(order2); s != stateResolved {
		t.Fatalf("after resolution: %s", s)
	}
	l.poll()
	l.poll()
	sends = l.fake.Sends()
	if len(sends) != 2 || sends[1].To != buyerAddr || sends[1].Amount != 100000 {
		t.Fatalf("refund sends: %+v", sends)
	}
	if err := e.DB.QueryRow("SELECT kind,state FROM payouts WHERE order_id=$1 AND user_id=$2", order2, buyerID).Scan(&kind, &pstate); err != nil {
		t.Fatal(err)
	}
	if kind != "refund" || pstate != "sent" {
		t.Fatalf("refund payout: kind=%s state=%s", kind, pstate)
	}
	if n := l.count("SELECT count(*) FROM payouts"); n != 2 {
		t.Fatalf("payout rows: %d", n)
	}
}
