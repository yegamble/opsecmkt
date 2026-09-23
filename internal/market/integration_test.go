package market

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// This test creates an isolated schema; TEST_DATABASE_URL must be a disposable test database.
func TestPostgresMarketplaceFlow(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for isolated PostgreSQL integration tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	name := "test_" + randomToken()[:16]
	if _, err = db.Exec("CREATE SCHEMA " + name); err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP SCHEMA " + name + " CASCADE")
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", name)
	parsed.RawQuery = q.Encode()
	t.Setenv("DATABASE_URL", parsed.String())
	t.Setenv("SETUP_TOKEN", strings.Repeat("test", 16))
	t.Setenv("COOKIE_SECURE", "false")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir("../.."); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	a, err := New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	do := func(method, path, token string, form url.Values) *httptest.ResponseRecorder {
		if form == nil {
			form = url.Values{}
		}
		if method == "POST" {
			form.Set("csrf", a.csrf(token))
		}
		r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "session", Value: token})
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	check := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("status=%d want=%d body=%s", w.Code, want, w.Body.String())
		}
	}
	session := func(w *httptest.ResponseRecorder) string {
		for _, c := range w.Result().Cookies() {
			if c.Name == "session" {
				return c.Value
			}
		}
		t.Fatal("no session")
		return ""
	}
	anon := randomToken()
	check(do("GET", "/", anon, nil), 303)
	setup := url.Values{"token": {strings.Repeat("test", 16)}, "handle": {"admin_user"}, "password": {"a-long-test-password"}, "site_name": {"Integration Market"}}
	w := do("POST", "/setup", anon, setup)
	check(w, 303)
	admin := session(w)
	check(do("POST", "/setup", anon, setup), 403)
	check(do("GET", "/setup", admin, nil), 403)
	w = do("POST", "/register", anon, url.Values{"handle": {"buyer_one"}, "password": {"a-long-buyer-password"}})
	check(w, 303)
	buyer := session(w)
	w = do("POST", "/register", anon, url.Values{"handle": {"buyer_two"}, "password": {"another-long-password"}})
	check(w, 303)
	other := session(w)
	check(do("GET", "/admin", buyer, nil), 403)
	listing := url.Values{"title": {"Encrypted <script>alert(1)</script> storage"}, "description": {"A test storage device"}, "category": {"Hardware"}, "region": {"Worldwide"}, "kind": {"physical"}, "price_btc": {"0.00000001"}, "price_xmr": {"0.000000000001"}, "stock": {"2"}}
	check(do("POST", "/listings", buyer, listing), 403)
	check(do("POST", "/listings", admin, listing), 303)
	var product string
	if err = a.db.QueryRow("SELECT id FROM products").Scan(&product); err != nil {
		t.Fatal(err)
	}
	order := url.Values{"product_id": {product}, "currency": {"BTC"}}
	w = do("POST", "/orders", buyer, order)
	check(w, 303)
	location := w.Header().Get("Location")
	w = do("POST", "/orders", buyer, order)
	check(w, 303)
	if w.Header().Get("Location") != location {
		t.Fatal("duplicate draft created")
	}
	w = do("GET", location, buyer, nil)
	check(w, 200)
	if strings.Contains(w.Body.String(), "<script>") {
		t.Fatal("listing title rendered executable markup")
	}
	if !strings.Contains(w.Body.String(), "&lt;script&gt;") {
		t.Fatal("expected escaped listing title")
	}
	check(do("GET", location, other, nil), 404)
	id := strings.TrimPrefix(location, "/order?id=")
	check(do("POST", "/disputes", buyer, url.Values{"order_id": {id}, "reason": {"This is an unfunded draft, so a dispute must not open."}}), 409)
	check(do("POST", "/messages", buyer, url.Values{"recipient": {"admin_user"}, "body": {"plaintext must not be sent"}}), 400)
	body := "-----BEGIN PGP MESSAGE-----\nSample encrypted armor for the storage boundary test only.\n-----END PGP MESSAGE-----"
	check(do("POST", "/messages", buyer, url.Values{"recipient": {"admin_user"}, "body": {body}}), 303)
	w = do("GET", "/messages", other, nil)
	check(w, 200)
	if strings.Contains(w.Body.String(), "Sample encrypted armor") {
		t.Fatal("message leaked to unrelated account")
	}
	check(do("POST", "/admin", buyer, url.Values{"action": {"role"}, "role": {"admin"}}), 403)
	check(do("POST", "/admin", admin, url.Values{"action": {"role"}, "user_id": {"missing"}, "role": {"vendor"}}), 404)
	for _, path := range []string{"/", "/product?id=" + product, "/vendor-dashboard", "/messages", "/notifications", "/account", "/moderator", "/admin", "/canary"} {
		t.Run(path, func(t *testing.T) { check(do("GET", path, admin, nil), 200) })
	}
	check(do("POST", "/revoke-sessions", buyer, nil), 303)
	check(do("GET", "/account", buyer, nil), 303)
	a.Close()
	a, err = New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var n int
	if err = a.db.QueryRow("SELECT count(*) FROM orders").Scan(&n); err != nil || n != 1 {
		t.Fatalf("restart persistence: %d %v", n, err)
	}
}
