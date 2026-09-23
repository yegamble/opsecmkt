package market

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// P3 Orders integration tests (PostgreSQL). Each test gets its own schema via newTestApp.

type orderWorld struct {
	e                                 *testEnv
	buyerID, vendorID, otherID, modID string
	buyer, vendor, other, mod         string // sessions
}

func newOrderWorld(t *testing.T) *orderWorld {
	w := &orderWorld{e: newTestApp(t)}
	suffix := randomToken()[:6]
	w.buyerID, w.buyer = w.e.user("buyer_"+suffix, "buyer")
	w.vendorID, w.vendor = w.e.user("vendor_"+suffix, "vendor")
	w.otherID, w.other = w.e.user("other_"+suffix, "buyer")
	w.modID, w.mod = w.e.user("mod_"+suffix, "moderator")
	return w
}

func (w *orderWorld) post(path, session string, form url.Values) *httptest.ResponseRecorder {
	return w.e.do("POST", path, session, form)
}

func (w *orderWorld) expect(path, session string, form url.Values, code int, msg string) {
	w.e.t.Helper()
	r := w.post(path, session, form)
	if r.Code != code || !strings.Contains(r.Body.String(), msg) {
		w.e.t.Fatalf("POST %s %v: status=%d body=%q, want %d containing %q", path, form, r.Code, r.Body.String(), code, msg)
	}
}

func (w *orderWorld) state(order string) string {
	w.e.t.Helper()
	var s string
	if err := w.e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&s); err != nil {
		w.e.t.Fatal(err)
	}
	return s
}

func (w *orderWorld) count(query string, args ...any) int {
	w.e.t.Helper()
	var n int
	if err := w.e.DB.QueryRow(query, args...).Scan(&n); err != nil {
		w.e.t.Fatal(err)
	}
	return n
}

func (w *orderWorld) stock(product string) int {
	return w.count("SELECT stock FROM products WHERE id=$1", product)
}

func (w *orderWorld) page(path, session string, code int) string {
	w.e.t.Helper()
	r := w.e.do("GET", path, session, nil)
	w.e.check(r, code)
	return r.Body.String()
}

func form(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func TestOrderPaymentRequestReservesStock(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	order := w.e.order(w.buyerID, product, "BTC", stateDraft)
	pay := form("order_id", order)

	w.expect("/orders/pay", w.buyer, pay, 409, "Payment unavailable for BTC")
	if !strings.Contains(w.page("/order?id="+order, w.buyer, 200), "Payment unavailable for BTC: no BTC test-network wallet") {
		t.Fatal("order page does not show the payment step as unavailable")
	}
	if w.state(order) != stateDraft || w.stock(product) != 5 || w.count("SELECT count(*) FROM payment_addresses") != 0 {
		t.Fatal("refused payment request changed data")
	}

	stub := &stubProvider{currency: "BTC", broken: true}
	w.e.A.payments["BTC"] = stub
	w.expect("/orders/pay", w.buyer, pay, 503, "did not return a usable address")
	if w.state(order) != stateDraft || w.stock(product) != 5 {
		t.Fatal("wallet failure left a partial change")
	}
	stub.broken = false
	w.expect("/orders/pay", w.vendor, pay, 403, "cannot make that change")
	w.expect("/orders/pay", w.other, pay, 404, "Order not found")
	if len(stub.created) != 0 {
		t.Fatal("wallet asked for an address on a refused request")
	}
	w.expect("/orders/pay", w.buyer, pay, 303, "")
	var addr, provider string
	if err := w.e.DB.QueryRow("SELECT address,provider FROM payment_addresses WHERE order_id=$1", order).Scan(&addr, &provider); err != nil || addr != "stub-"+order[:8] || provider != "btc-regtest" {
		t.Fatalf("payment address %q %q %v", addr, provider, err)
	}
	if w.state(order) != stateAwaitingPayment || w.stock(product) != 4 {
		t.Fatalf("state=%s stock=%d", w.state(order), w.stock(product))
	}
	w.expect("/orders/pay", w.buyer, pay, 409, "no longer in that state")
	if w.stock(product) != 4 || len(stub.created) != 1 {
		t.Fatal("repeated payment request reserved stock or created an address again")
	}

	// Cancelling a reserved order returns the unit; a stale form is a conflict.
	w.expect("/orders/cancel", w.buyer, form("order_id", order, "from", stateDraft), 409, "no longer in that state")
	w.expect("/orders/cancel", w.buyer, form("order_id", order, "from", stateAwaitingPayment, "note", "Changed my mind"), 303, "")
	if w.state(order) != stateCancelled || w.stock(product) != 5 {
		t.Fatalf("cancel: state=%s stock=%d", w.state(order), w.stock(product))
	}
	w.expect("/orders/cancel", w.buyer, form("order_id", order, "from", stateAwaitingPayment), 409, "")
	w.expect("/orders/cancel", w.buyer, form("order_id", order, "from", stateCompleted), 400, "")
	body := w.page("/order?id="+order, w.buyer, 200)
	for _, s := range []string{"Draft — unfunded → Awaiting payment", "Awaiting payment → Cancelled", "Changed my mind", "(buyer)", "This order is closed"} {
		if !strings.Contains(body, s) {
			t.Errorf("order history missing %q", s)
		}
	}

	// No stock: the reservation fails and nothing changes.
	w.e.DB.Exec("UPDATE products SET stock=0 WHERE id=$1", product)
	second := w.e.order(w.buyerID, product, "BTC", stateDraft)
	w.expect("/orders/pay", w.buyer, form("order_id", second), 409, "out of stock")
	if w.state(second) != stateDraft || w.count("SELECT count(*) FROM order_events WHERE order_id=$1", second) != 0 {
		t.Fatal("out-of-stock request changed the order")
	}
	// Archived listings cannot be reserved either.
	w.e.DB.Exec("UPDATE products SET stock=3, archived=true WHERE id=$1", product)
	w.expect("/orders/pay", w.buyer, form("order_id", second), 409, "out of stock")
	// A draft cancelled by its buyer never touched stock.
	w.expect("/orders/cancel", w.vendor, form("order_id", second, "from", stateDraft), 403, "")
	w.expect("/orders/cancel", w.buyer, form("order_id", second, "from", stateDraft), 303, "")
	if w.stock(product) != 3 {
		t.Fatal("draft cancellation changed stock")
	}
}

func TestPhysicalOrderTransitionsEnforceRoles(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	order := w.e.order(w.buyerID, product, "BTC", statePaid)
	id := form("order_id", order)

	w.expect("/orders/ship", w.buyer, id, 403, "")
	w.expect("/orders/ship", w.other, id, 404, "")
	w.expect("/orders/deliver", w.vendor, form("order_id", order, "content", "physical goods have no content"), 403, "only available for digital orders")
	w.expect("/orders/cancel", w.buyer, form("order_id", order, "from", statePaid), 403, "")
	w.expect("/orders/complete", w.buyer, id, 409, "")
	w.expect("/orders/ship", w.vendor, form("order_id", order, "note", "Tracking: TEST-123"), 303, "")
	w.expect("/orders/ship", w.vendor, id, 409, "")
	w.expect("/orders/complete", w.vendor, id, 403, "")
	w.expect("/orders/complete", w.other, id, 404, "")
	w.expect("/orders/cancel", w.vendor, form("order_id", order, "from", statePaid), 409, "")
	w.expect("/orders/complete", w.buyer, id, 303, "")
	w.expect("/orders/complete", w.buyer, id, 409, "")
	w.expect("/disputes", w.buyer, form("order_id", order, "reason", "Completed orders are final and cannot be disputed."), 409, "")
	if w.state(order) != stateCompleted {
		t.Fatal(w.state(order))
	}
	var actors []string
	rows, _ := w.e.DB.Query("SELECT to_state||':'||COALESCE(actor_id,'system') FROM order_events WHERE order_id=$1 ORDER BY id", order)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		actors = append(actors, s)
	}
	rows.Close()
	if strings.Join(actors, ",") != "shipped:"+w.vendorID+",completed:"+w.buyerID {
		t.Fatalf("events %v", actors)
	}
	if !strings.Contains(w.page("/order?id="+order, w.vendor, 200), "Tracking: TEST-123") {
		t.Fatal("shipping note not shown")
	}

	// A vendor may cancel a paid order; its reserved unit returns.
	paid := w.e.order(w.buyerID, product, "BTC", statePaid)
	w.expect("/orders/cancel", w.vendor, form("order_id", paid, "from", statePaid), 303, "")
	if w.stock(product) != 6 || w.state(paid) != stateCancelled {
		t.Fatalf("vendor cancel: stock=%d state=%s", w.stock(product), w.state(paid))
	}

	// The vendor dashboard lists incoming orders; orders shows the viewer's side.
	w.e.order(w.buyerID, product, "BTC", statePaid)
	if body := w.page("/vendor-dashboard", w.vendor, 200); !strings.Contains(body, "Paid orders awaiting shipment or delivery") || !strings.Contains(body, "To fulfil</div>\n<strong>1</strong>") {
		t.Fatal("vendor dashboard does not count the paid order")
	}
	if !strings.Contains(w.page("/orders", w.buyer, 200), "Buying") || !strings.Contains(w.page("/orders", w.vendor, 200), "Selling") {
		t.Fatal("orders page does not show the viewer's role")
	}
}

func TestDigitalDeliveryIsReleasedOnlyByDelivery(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "digital")
	order := w.e.order(w.buyerID, product, "XMR", statePaid)
	const secret = "LICENCE-KEY-7f3a-only-for-the-buyer"
	path := "/order?id=" + order

	if strings.Contains(w.page(path, w.buyer, 200), secret) || !strings.Contains(w.page(path, w.buyer, 200), "Nothing has been delivered") {
		t.Fatal("undelivered order shows content")
	}
	w.expect("/orders/deliver", w.buyer, form("order_id", order, "content", secret), 403, "")
	w.expect("/orders/deliver", w.other, form("order_id", order, "content", secret), 404, "")
	w.expect("/orders/deliver", w.vendor, form("order_id", order, "content", "   "), 400, "1–32000")
	w.expect("/orders/deliver", w.vendor, form("order_id", order, "content", strings.Repeat("x", 32001)), 400, "1–32000")
	w.expect("/orders/ship", w.vendor, form("order_id", order), 403, "only available for physical orders")
	if w.count("SELECT count(*) FROM deliveries") != 0 {
		t.Fatal("refused delivery stored content")
	}
	w.expect("/orders/deliver", w.vendor, form("order_id", order, "content", secret), 303, "")
	w.expect("/orders/deliver", w.vendor, form("order_id", order, "content", "second copy"), 409, "")
	if w.state(order) != stateDelivered {
		t.Fatal(w.state(order))
	}
	for _, s := range []string{w.buyer, w.vendor} {
		if !strings.Contains(w.page(path, s, 200), secret) {
			t.Fatal("party cannot read delivered content")
		}
	}
	if body := w.page(path, w.other, 404); strings.Contains(body, secret) {
		t.Fatal("third party saw delivered content")
	}
	if body := w.page(path, w.mod, 404); strings.Contains(body, secret) {
		t.Fatal("moderator saw delivered content through the order page")
	}
}

func TestAutomaticDeliveryAfterPayment(t *testing.T) {
	w := newOrderWorld(t)
	auto := w.e.product(w.vendorID, "digital")
	w.e.DB.Exec("UPDATE products SET delivery_content='AUTO-CONTENT-42' WHERE id=$1", auto)
	manual := w.e.product(w.vendorID, "digital")
	physical := w.e.product(w.vendorID, "physical")
	w.e.DB.Exec("UPDATE products SET delivery_content='never released for physical' WHERE id=$1", physical)
	markPaid := func(order string) *Order {
		t.Helper()
		ctx := context.Background()
		tx, err := w.e.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		o, err := w.e.A.transition(ctx, tx, order, stateAwaitingPayment, statePaid, nil, "confirmed")
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return o
	}
	a := w.e.order(w.buyerID, auto, "BTC", stateAwaitingPayment)
	if o := markPaid(a); o.State != stateDelivered || w.state(a) != stateDelivered {
		t.Fatalf("automatic delivery: returned %s stored %s", o.State, w.state(a))
	}
	var content string
	w.e.DB.QueryRow("SELECT content FROM deliveries WHERE order_id=$1", a).Scan(&content)
	if content != "AUTO-CONTENT-42" || w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NULL", a) != 2 {
		t.Fatalf("delivery %q", content)
	}
	if body := w.page("/order?id="+a, w.buyer, 200); !strings.Contains(body, "AUTO-CONTENT-42") || !strings.Contains(body, "Automatic delivery after confirmed payment") {
		t.Fatal("automatic delivery not visible to the buyer")
	}
	for _, p := range []string{manual, physical} {
		o := w.e.order(w.buyerID, p, "BTC", stateAwaitingPayment)
		if markPaid(o).State != statePaid || w.count("SELECT count(*) FROM deliveries WHERE order_id=$1", o) != 0 {
			t.Fatal("order without automatic content was delivered")
		}
	}
}

func TestConcurrentCompletionSucceedsOnce(t *testing.T) {
	w := newOrderWorld(t)
	order := w.e.order(w.buyerID, w.e.product(w.vendorID, "digital"), "BTC", stateDelivered)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- w.post("/orders/complete", w.buyer, form("order_id", order)).Code
		}()
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[303] != 1 || got[409] != 7 || w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND to_state='completed'", order) != 1 {
		t.Fatalf("status counts %v", got)
	}
}

func TestReviewsRequireACompletedOrder(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	shipped := w.e.order(w.buyerID, product, "BTC", stateShipped)
	done := w.e.order(w.buyerID, product, "BTC", stateCompleted)
	review := func(order, rating, body string) url.Values {
		return form("order_id", order, "rating", rating, "body", body)
	}
	w.expect("/reviews", w.buyer, review(shipped, "5", "Too early"), 409, "Only completed orders")
	w.expect("/reviews", w.vendor, review(done, "5", "Self review"), 403, "Only the buyer")
	w.expect("/reviews", w.other, review(done, "5", "Stranger"), 404, "")
	for _, bad := range []string{"0", "6", "x", ""} {
		w.expect("/reviews", w.buyer, review(done, bad, "Bad rating"), 400, "rating from 1 to 5")
	}
	w.expect("/reviews", w.buyer, review(done, "4", strings.Repeat("y", 2001)), 400, "2000")
	if !strings.Contains(w.page("/order?id="+done, w.buyer, 200), "Publish review") {
		t.Fatal("review form missing on a completed order")
	}
	w.expect("/reviews", w.buyer, review(done, "4", "Arrived as described. <b>not bold</b>"), 303, "")
	w.expect("/reviews", w.buyer, review(done, "1", "Changed my mind"), 409, "already reviewed")
	if w.count("SELECT count(*) FROM reviews") != 1 {
		t.Fatal("second review stored")
	}
	if strings.Contains(w.page("/order?id="+done, w.buyer, 200), "Publish review") {
		t.Fatal("review form still offered")
	}
	var buyerHandle string
	w.e.DB.QueryRow("SELECT handle FROM users WHERE id=$1", w.buyerID).Scan(&buyerHandle)
	for _, path := range []string{"/product?id=" + product, "/vendor?id=" + w.vendorID} {
		body := w.page(path, randomToken(), 200)
		for _, s := range []string{"Verified purchase", "Arrived as described. &lt;b&gt;not bold&lt;/b&gt;", "<strong>4.0</strong> / 5 · 1 review", "Rated 4 out of 5"} {
			if !strings.Contains(body, s) {
				t.Errorf("%s missing %q", path, s)
			}
		}
		if strings.Contains(body, buyerHandle) || strings.Contains(body, done) {
			t.Errorf("%s exposes the reviewer handle or order id", path)
		}
	}
	other := w.e.product(w.vendorID, "physical")
	if !strings.Contains(w.page("/product?id="+other, randomToken(), 200), "No verified reviews yet") {
		t.Fatal("empty review state missing")
	}
}

func TestDisputeOpensAndModeratorResolves(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	order := w.e.order(w.buyerID, product, "BTC", stateShipped)
	reason := "The parcel arrived empty and the seal was broken."

	w.expect("/disputes", w.other, form("order_id", order, "reason", reason), 404, "")
	w.expect("/disputes", w.buyer, form("order_id", order, "reason", "short"), 400, "")
	if !strings.Contains(w.page("/disputes", w.buyer, 200), `<option value="`+order+`">`) {
		t.Fatal("disputes page does not offer the shipped order")
	}
	w.expect("/disputes", w.buyer, form("order_id", order, "reason", reason), 303, "")
	w.expect("/disputes", w.vendor, form("order_id", order, "reason", reason), 409, "")
	w.expect("/orders/complete", w.buyer, form("order_id", order), 409, "")
	if w.state(order) != stateDisputed || w.count("SELECT count(*) FROM disputes WHERE order_id=$1", order) != 1 {
		t.Fatal("dispute not opened")
	}
	var disputeID string
	w.e.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order).Scan(&disputeID)

	modPage := w.page("/moderator", w.mod, 200)
	if !strings.Contains(modPage, `name="outcome" value="refund"`) || !strings.Contains(modPage, "Test listing") {
		t.Fatal("moderator desk lacks the outcome form or order summary")
	}
	decision := "Evidence supports the buyer; refund the full amount."
	w.expect("/resolve", w.buyer, form("id", disputeID, "outcome", "refund", "resolution", decision), 403, "")
	w.expect("/resolve", w.mod, form("id", disputeID, "resolution", decision), 400, "Choose an outcome")
	w.expect("/resolve", w.mod, form("id", disputeID, "outcome", "split", "resolution", decision), 400, "Choose an outcome")

	// Payout hooks (P5) must be able to read the outcome inside the resolving transaction.
	var seen string
	transitionHooks = append(transitionHooks, func(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
		if to == stateResolved {
			return tx.QueryRowContext(ctx, "SELECT outcome FROM disputes WHERE order_id=$1", o.ID).Scan(&seen)
		}
		return nil
	})
	t.Cleanup(func() { transitionHooks = transitionHooks[:len(transitionHooks)-1] })
	w.expect("/resolve", w.mod, form("id", disputeID, "outcome", "refund", "resolution", decision), 303, "")
	w.expect("/resolve", w.mod, form("id", disputeID, "outcome", "release", "resolution", decision), 409, "")
	var outcome, status string
	w.e.DB.QueryRow("SELECT outcome,status FROM disputes WHERE id=$1", disputeID).Scan(&outcome, &status)
	if seen != "refund" || outcome != "refund" || status != "Resolved — refund to buyer" || w.state(order) != stateResolved {
		t.Fatalf("seen=%q outcome=%q status=%q state=%s", seen, outcome, status, w.state(order))
	}
	if !strings.Contains(w.page("/order?id="+order, w.buyer, 200), "(moderator)") {
		t.Fatal("resolution not attributed to the moderator in the order history")
	}

	// An administrator who is the vendor cannot resolve a dispute on their own order.
	adminID, admin := w.e.user("admin_"+randomToken()[:6], "admin")
	own := w.e.order(w.buyerID, w.e.product(adminID, "physical"), "BTC", stateShipped)
	w.expect("/disputes", w.buyer, form("order_id", own, "reason", reason), 303, "")
	var ownDispute string
	w.e.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", own).Scan(&ownDispute)
	if !strings.Contains(w.page("/moderator", admin, 200), "You are a party to this order") {
		t.Fatal("moderator desk offers a conflicted resolution")
	}
	w.expect("/resolve", admin, form("id", ownDispute, "outcome", "release", "resolution", decision), 403, "")
	w.e.DB.QueryRow("SELECT outcome,status FROM disputes WHERE id=$1", ownDispute).Scan(&outcome, &status)
	if outcome != "" || status != "Open" || w.state(own) != stateDisputed {
		t.Fatal("refused resolution was partially written")
	}
}
