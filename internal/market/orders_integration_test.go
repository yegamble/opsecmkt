package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
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

func TestPausedDraftCanBeReadAndCancelledWithoutPayment(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	order := w.e.order(w.buyerID, product, "BTC", stateDraft)
	if body := w.page("/order?id="+order, w.buyer, 200); !strings.Contains(body, "Unfunded draft") {
		t.Fatalf("draft page: %s", body)
	}
	for _, session := range []string{w.buyer, w.vendor, w.other} {
		if r := w.post("/orders/pay", session, form("order_id", order)); r.Code != 404 {
			t.Fatalf("retired payment route: %d", r.Code)
		}
	}
	w.expect("/orders/cancel", w.vendor, form("order_id", order, "from", stateDraft), 403, "")
	w.expect("/orders/cancel", w.other, form("order_id", order, "from", stateDraft), 404, "")
	w.expect("/orders/cancel", w.buyer, form("order_id", order, "from", stateDraft), 303, "")
	if w.state(order) != stateCancelled || w.stock(product) != 5 || w.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", order) != 0 {
		t.Fatal("cancelled draft changed stock or acquired payment address")
	}
	w.expect("/orders/cancel", w.buyer, form("order_id", order, "from", stateDraft), 409, "")
}

func TestHistoricalDeliveryReaderRestrictsContentToParties(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "digital")
	order := w.e.order(w.buyerID, product, "XMR", stateDelivered)
	const secret = "LICENCE-KEY-7f3a-only-for-the-buyer"
	w.e.mustExec("INSERT INTO deliveries(order_id,content) VALUES($1,$2)", order, secret)
	for _, session := range []string{w.buyer, w.vendor} {
		mustContain(t, w.page("/order?id="+order, session, 200), secret)
	}
	for _, session := range []string{w.other, w.mod} {
		mustNotContain(t, w.page("/order?id="+order, session, 404), secret)
	}
	if r := w.post("/orders/deliver", w.vendor, form("order_id", order, "content", "second")); r.Code == 303 {
		t.Fatal("historical order delivered again")
	}
}

func TestHistoricalDisputeStaysReadableWithoutResolveForm(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	order := w.e.order(w.buyerID, product, "BTC", stateDisputed)
	dispute := randomToken()
	w.e.mustExec("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Historical parcel dispute')", dispute, order)
	for _, session := range []string{w.buyer, w.vendor, w.mod} {
		mustContain(t, w.page("/disputes", session, 200), "Historical parcel dispute")
	}
	desk := w.page("/moderator", w.mod, 200)
	mustContain(t, desk, "Historical dispute", "read only")
	mustNotContain(t, desk, `action="/resolve"`)
	if r := w.post("/resolve", w.mod, resolveConfirmed(form("id", dispute, "outcome", "refund", "resolution", "Historical outcome"), 0)); r.Code == 303 {
		t.Fatal("historical dispute resolved")
	}
	if w.state(order) != stateDisputed {
		t.Fatal("historical dispute state changed")
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
		for _, s := range []string{"Completed order (test network)", "Arrived as described. &lt;b&gt;not bold&lt;/b&gt;", "<strong>4.0</strong> / 5 · 1 review", "Rated 4 out of 5"} {
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
	// A-80: the review help says the vendor sees the review with the buyer's handle on the order page.
	if body := w.page("/order?id="+done, w.vendor, 200); !strings.Contains(body, "Arrived as described. &lt;b&gt;not bold&lt;/b&gt;") || !strings.Contains(body, "<dd>"+buyerHandle+"</dd>") {
		t.Fatal("the vendor's order page does not show the review beside the buyer's handle")
	}
}
