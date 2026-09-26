package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestPostgresMarketplaceFlow(t *testing.T) {
	e := newTestApp(t)
	do, check, session := e.do, e.check, e.session
	a := e.A
	var err error
	anon := randomToken()
	check(do("GET", "/", anon, nil), 303)
	setup := url.Values{"token": {testSetupToken}, "handle": {"admin_user"}, "password": {"a-long-test-password"}, "site_name": {"Integration Market"}}
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
	listing := url.Values{"title": {"Encrypted <script>alert(1)</script> storage"}, "description": {"A test storage device"}, "category": {"Hardware"}, "region": {"Worldwide"}, "kind": {"physical"}, "price_btc": {"0.0001"}, "price_xmr": {"0.000000000001"}, "stock": {"2"}}
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
	if !strings.Contains(w.Body.String(), "Draft — unfunded") {
		t.Fatal("expected order state label")
	}
	check(do("GET", location, other, nil), 404)
	id := strings.TrimPrefix(location, "/order?id=")
	check(do("POST", "/disputes", buyer, url.Values{"order_id": {id}, "reason": {"This is an unfunded draft, so a dispute must not open."}}), 409)
	check(do("POST", "/messages", buyer, url.Values{"recipient": {"admin_user"}, "body": {"plaintext must not be sent"}}), 400)
	check(do("POST", "/messages", buyer, url.Values{"recipient": {"admin_user"}, "body": {"-----BEGIN PGP MESSAGE-----\nSample encrypted armor for the storage boundary test only.\n-----END PGP MESSAGE-----"}}), 400)
	msgKey, _ := testPGPKey(t, "recipient")
	body, err := encryptTo(msgKey, "storage boundary test")
	if err != nil {
		t.Fatal(err)
	}
	check(do("POST", "/messages", buyer, url.Values{"recipient": {"admin_user"}, "body": {body}}), 303)
	w = do("GET", "/messages", other, nil)
	check(w, 200)
	if strings.Contains(w.Body.String(), strings.Split(body, "\n")[2]) {
		t.Fatal("message leaked to unrelated account")
	}
	check(do("POST", "/admin", buyer, url.Values{"action": {"role"}, "role": {"admin"}}), 403)
	check(do("POST", "/admin", admin, url.Values{"action": {"role"}, "handle": {"missing"}, "role": {"vendor"}}), 404)
	for _, path := range []string{"/", "/product?id=" + product, "/checkout?id=" + product, "/orders", "/disputes", "/vendor-dashboard", "/messages", "/notifications", "/account", "/moderator", "/admin", "/canary", "/challenge"} {
		t.Run(path, func(t *testing.T) { check(do("GET", path, admin, nil), 200) })
	}
	check(do("POST", "/revoke-sessions", buyer, nil), 303)
	check(do("GET", "/account", buyer, nil), 303)
	e.restart()
	var n int
	if err = e.DB.QueryRow("SELECT count(*) FROM orders").Scan(&n); err != nil || n != 1 {
		t.Fatalf("restart persistence: %d %v", n, err)
	}
}
