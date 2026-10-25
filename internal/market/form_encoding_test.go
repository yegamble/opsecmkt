package market

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"
)

// QA5W3B-5 (A-481 root cause): a form field that is not valid UTF-8 or holds a NUL byte is refused with 400 where
// ServeHTTP parses the form, before any handler runs. On a UTF8 database such text made the insert fail with a 500
// "Unable to save changes", and a confirmed action had accepted the password first. DB integration.

const invalidTextMsg = "not valid UTF-8 or contains a NUL character"

// QA5W4B-5: a GET or HEAD whose query string holds a name or value that is not valid UTF-8 or holds a NUL byte is
// refused with 400 in ServeHTTP, before any page, raw route or image route reads it. Before, the loaders passed it to
// PostgreSQL, which refused it (SQLSTATE 22021): anonymous /product?id=%ff answered 500. Every page and raw route is
// asked, by every role, with each such value in every common parameter and in all of them at once: no 5xx, and every
// query-string case is the 400. The image routes take their id from the path, which this check does not read: they
// must not answer 5xx either.
func TestGetRefusesInvalidQueryText(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("getq_admin", "admin")
	vendorID, vendor := e.user("getq_vendor", "vendor")
	buyerID, buyer := e.user("getq_buyer", "buyer")
	_, mod := e.user("getq_mod", "moderator")
	e.order(buyerID, e.product(vendorID, "physical"), "XMR", "paid")
	var paths []string
	for p := range pages {
		paths = append(paths, "/"+p)
	}
	for p := range raws {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	params := []string{"id", "q", "category", "region", "currency", "page", "to", "next", "s", "b", "upto", "practice", "scanner", "sig"}
	sessions := map[string]string{"anon": "", "admin": admin, "vendor": vendor, "buyer": buyer, "moderator": mod}
	send := func(method, target, session string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		if session != "" {
			r.AddCookie(&http.Cookie{Name: "session", Value: session})
		}
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		return w
	}
	fivexx, other := 0, 0
	for _, v := range []string{"%ff", "%00", "a%c3", "%c0%80"} {
		var all, queries []string
		for _, p := range params {
			all = append(all, p+"="+v)
			queries = append(queries, p+"="+v)
		}
		queries = append(queries, strings.Join(all, "&"), v+"=1", "id=1&"+v)
		for _, path := range paths {
			for role, session := range sessions {
				for _, q := range queries {
					for _, method := range []string{"GET", "HEAD"} {
						w := send(method, path+"?"+q, session)
						switch {
						case moderationPrivateRoute(path):
							// Private moderation refuses public ingress before parsing
							// any query/body or looking up a session, even without ADMIN_SOCKET.
							if w.Code != 404 || w.Header().Get("Set-Cookie") != "" {
								t.Errorf("public private-only route %s %s: %d", method, path, w.Code)
							}
						case w.Code >= 500:
							fivexx++
							t.Errorf("%s %s?%s as %s: %d %q", method, path, q, role, w.Code, w.Body.String())
						case w.Code != 400 || method == "GET" && refusalText(w.Body.String()) != invalidQueryText:
							other++
							if other <= 10 {
								t.Errorf("%s %s?%s as %s: %d %q, want 400 %q", method, path, q, role, w.Code, w.Body.String(), invalidQueryText)
							}
						}
					}
				}
			}
		}
	}
	for _, path := range []string{"/img/%ff", "/img/%00", "/desk/img/%ff", "/desk/img/%00", "/img/a%c3-t"} {
		for role, session := range sessions {
			if w := send("GET", path, session); w.Code >= 500 {
				fivexx++
				t.Errorf("GET %s as %s: %d %q", path, role, w.Code, w.Body.String())
			}
		}
	}
	if fivexx != 0 || other != 0 {
		t.Fatalf("%d answers 5xx, %d query-string cases not the 400", fivexx, other)
	}
	// Valid UTF-8 in a query still reaches the page.
	if w := send("GET", "/?q="+url.QueryEscape("Crème 東京"), ""); w.Code != 200 {
		t.Fatalf("a valid query: %d %q", w.Code, w.Body.String())
	}
}

// The buyer's review (orders_actions.go reviewAction) of a completed order: neither value reaches the handler.
func TestFormRefusesInvalidTextReview(t *testing.T) {
	w := newOrderWorld(t)
	done := w.e.order(w.buyerID, w.e.product(w.vendorID, "physical"), "XMR", stateCompleted)
	for name, body := range map[string]string{"0xff": "Great \xff\xfe seller", "NUL": "Great \x00 seller"} {
		w.expect("/reviews", w.buyer, form("order_id", done, "rating", "5", "body", body), 400, invalidTextMsg)
		if n := w.count("SELECT count(*) FROM reviews WHERE order_id=$1", done); n != 0 {
			t.Fatalf("%s: %d reviews written", name, n)
		}
	}
	// QA5W4B-8: a field name that is not UTF-8, and a query string on the post (ParseForm merges it into r.Form,
	// which handlers read through FormValue), are refused alike.
	w.expect("/reviews", w.buyer, form("order_id", done, "rating", "5", "body", "Great seller", "\xff", "1"), 400, invalidTextMsg)
	w.expect("/reviews?x=%ff", w.buyer, form("order_id", done, "rating", "5", "body", "Great seller"), 400, invalidTextMsg)
	if n := w.count("SELECT count(*) FROM reviews WHERE order_id=$1", done); n != 0 {
		t.Fatalf("a bad field name or query string: %d reviews written", n)
	}
	// Valid text in the same form is saved, so nothing else refused it.
	w.expect("/reviews", w.buyer, form("order_id", done, "rating", "5", "body", "Great seller, é and 東京."), 303, "")
}

// The A-480 disposition with the right password: refused with 400, the record stays open, no audit row, and none of
// the moderator's confirmation budget is spent.
func TestFormRefusesInvalidTextCollisionDisposition(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	modID, mod := x.user("encmod_"+randomToken()[:6], "moderator")
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	tx := hex64("invalid text " + order)
	x.fake.payAddress(addr, tx, xmrListingPrice, 10)
	x.refusePass(order)
	id := agStrings(t, x.testEnv, "SELECT id::text FROM xmr_escrow_collisions WHERE order_id=$1", order)[0]
	budget := func() int { x.A.mu.Lock(); defer x.A.mu.Unlock(); return x.A.limits["confirm:"+modID].Count }
	for name, note := range map[string]string{"0xff": "Refunded by hand \xff\xfe in the wallet.", "NUL": "Refunded by hand \x00 in the wallet."} {
		if code, body := x.recordCollision(mod, id, note); code != 400 || !strings.Contains(body, invalidTextMsg) {
			t.Fatalf("%s note: %d %s", name, code, body)
		}
	}
	if got := x.collisions(order); !slices.Equal(got, []string{tx + " open"}) || x.collisionAudits(modID) != 0 || budget() != 0 {
		t.Fatalf("refused notes changed something: %q, %d audit rows, budget %d", got, x.collisionAudits(modID), budget())
	}
	if code, loc := x.recordCollision(mod, id, "Refunded by hand in the wallet."); code != 303 || loc != "/moderator" {
		t.Fatalf("a valid note: %d %s", code, loc)
	}
}

// A staff member who is a party to the order is refused before the password check, so twelve wrong passwords give
// twelve 403s and spend none of the confirmation budget (QA5W3B-7: without the check before confirmedTx they gave
// ten 401s and then 429).
func TestXMREscrowCollisionStaffPartyBudget(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("staff party budget "+order), xmrListingPrice, 10)
	x.refusePass(order)
	id := agStrings(t, x.testEnv, "SELECT id::text FROM xmr_escrow_collisions WHERE order_id=$1", order)[0]
	agExec(x.testEnv, "UPDATE users SET role='moderator' WHERE id=$1", x.vendor)
	var codes []int
	for i := 0; i < 12; i++ {
		c, _ := x.recordCollision(x.vendorSess, id, "Staff party tries to record it.", "password", "wrong-password-123")
		codes = append(codes, c)
	}
	x.A.mu.Lock()
	spent := x.A.limits["confirm:"+x.vendor].Count
	x.A.mu.Unlock()
	if !slices.Equal(codes, slices.Repeat([]int{403}, 12)) || spent != 0 {
		t.Fatalf("a staff party with a wrong password twelve times: %v, budget spent %d", codes, spent)
	}
	if c, body := x.recordCollision(x.vendorSess, id, "Staff party tries to record it."); c != 403 || !strings.Contains(body, "party") {
		t.Fatalf("then the right password: %d %s", c, body)
	}
}
