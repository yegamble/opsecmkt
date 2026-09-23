package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// P4 Inventory integration tests (TEST_DATABASE_URL). Fixtures use e.user/e.product (fast, no /setup flow).

func inventoryForm(id string, overrides map[string]string) url.Values {
	f := url.Values{"id": {id}, "title": {"Edited listing"}, "description": {"Edited description"}, "category": {"Digital"}, "region": {"Europe"}, "kind": {"physical"}, "price_btc": {"0.002"}, "price_xmr": {"0.75"}, "stock": {"5"}, "stock_seen": {"5"}}
	for k, v := range overrides {
		f.Set(k, v)
	}
	return f
}

func inventoryAudits(e *testEnv, like string) int {
	e.t.Helper()
	var n int
	if err := e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE action LIKE $1", like).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestListingEditOwnership(t *testing.T) {
	e := newTestApp(t)
	owner, ownerS := e.user("inv_owner", "vendor")
	_, otherS := e.user("inv_other", "vendor")
	_, buyerS := e.user("inv_buyer", "buyer")
	_, modS := e.user("inv_mod", "moderator")
	_, adminS := e.user("inv_admin", "admin")
	p := e.product(owner, "physical")

	// Edit page: owner and admin only; other vendors cannot confirm the listing exists.
	w := e.do("GET", "/listing-edit?id="+p, ownerS, nil)
	e.check(w, 200)
	for _, want := range []string{`value="Test listing"`, `name="stock_seen" value="5"`, `value="0.001"`, `value="0.5"`, "Automatic delivery requires a payment provider", "Stored unencrypted on the server"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("edit page missing %q", want)
		}
	}
	e.check(e.do("GET", "/listing-edit?id="+p, adminS, nil), 200)
	e.check(e.do("GET", "/listing-edit?id="+p, otherS, nil), 404)
	e.check(e.do("GET", "/listing-edit?id=missing", ownerS, nil), 404)
	e.check(e.do("GET", "/listing-edit?id="+p, buyerS, nil), 403)
	e.check(e.do("GET", "/listing-edit?id="+p, randomToken(), nil), 303)

	for _, path := range []string{"/listings/update", "/listings/archive", "/listings/restore"} {
		e.check(e.do("POST", path, otherS, inventoryForm(p, nil)), 404)
		e.check(e.do("POST", path, buyerS, inventoryForm(p, nil)), 403)
		e.check(e.do("POST", path, modS, inventoryForm(p, nil)), 403)
	}
	var title string
	if err := e.DB.QueryRow("SELECT title FROM products WHERE id=$1", p).Scan(&title); err != nil || title != "Test listing" {
		t.Fatalf("unauthorized edit changed the listing: %q %v", title, err)
	}

	w = e.do("POST", "/listings/update", ownerS, inventoryForm(p, nil))
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/listing-edit?id="+p+"&saved=1" {
		t.Fatalf("redirect %q", loc)
	}
	var btc, xmr int64
	var stock int
	var cat, region string
	if err := e.DB.QueryRow("SELECT title,category,region,btc,xmr,stock FROM products WHERE id=$1", p).Scan(&title, &cat, &region, &btc, &xmr, &stock); err != nil {
		t.Fatal(err)
	}
	if title != "Edited listing" || cat != "Digital" || region != "Europe" || btc != 200000 || xmr != 750000000000 || stock != 5 {
		t.Fatalf("update not stored: %s %s %s %d %d %d", title, cat, region, btc, xmr, stock)
	}
	if inventoryAudits(e, "Updated listing "+p[:8]) != 1 {
		t.Fatal("owner update not audited")
	}
	e.check(e.do("POST", "/listings/update", adminS, inventoryForm(p, map[string]string{"title": "Admin edited"})), 303)
	if inventoryAudits(e, "Updated listing "+p[:8]+" as administrator") != 1 {
		t.Fatal("administrator update not audited as such")
	}
	e.check(e.do("POST", "/listings/update", ownerS, inventoryForm("missing", nil)), 404)
}

func TestListingUpdateValidation(t *testing.T) {
	e := newTestApp(t)
	owner, s := e.user("inv_valid", "vendor")
	p := e.product(owner, "physical")
	for name, bad := range map[string]map[string]string{
		"service kind":            {"kind": "service"},
		"unknown kind":            {"kind": "weapon"},
		"price precision":         {"price_btc": "0.000000001"},
		"zero price":              {"price_xmr": "0"},
		"negative stock":          {"stock": "-1"},
		"short title":             {"title": "ab"},
		"bad category":            {"category": "Other"},
		"delivery on physical":    {"delivery_content": "secret key"},
		"delivery too long":       {"kind": "digital", "delivery_content": strings.Repeat("x", 32001)},
		"missing stock_seen":      {"stock_seen": ""},
		"stock_seen not a number": {"stock_seen": "x"},
	} {
		t.Run(name, func(t *testing.T) { e.check(e.do("POST", "/listings/update", s, inventoryForm(p, bad)), 400) })
	}
	// Create shares the validator.
	create := inventoryForm("", map[string]string{"kind": "service"})
	e.check(e.do("POST", "/listings", s, create), 400)
	create.Set("kind", "digital")
	create.Set("delivery_content", "created-with-content")
	e.check(e.do("POST", "/listings", s, create), 303)
	var dc string
	if err := e.DB.QueryRow("SELECT delivery_content FROM products WHERE vendor_id=$1 AND id<>$2", owner, p).Scan(&dc); err != nil || dc != "created-with-content" {
		t.Fatalf("create did not store delivery content: %q %v", dc, err)
	}

	// Digital listing with delivery content: stored, shown to the owner, never on public pages.
	secret := "LICENSE-KEY-<b>7731</b>"
	e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"kind": "digital", "delivery_content": secret})), 303)
	if err := e.DB.QueryRow("SELECT delivery_content FROM products WHERE id=$1", p).Scan(&dc); err != nil || dc != secret {
		t.Fatalf("delivery content %q %v", dc, err)
	}
	w := e.do("GET", "/listing-edit?id="+p, s, nil)
	e.check(w, 200)
	if !strings.Contains(w.Body.String(), "LICENSE-KEY-&lt;b&gt;7731&lt;/b&gt;") {
		t.Fatal("owner edit page does not show escaped delivery content")
	}
	_, buyerS := e.user("inv_valid_buyer", "buyer")
	for _, path := range []string{"/", "/product?id=" + p, "/checkout?id=" + p, "/vendor?id=" + owner, "/vendor-dashboard"} {
		sess := buyerS
		if path == "/vendor-dashboard" {
			sess = s
		}
		w := e.do("GET", path, sess, nil)
		e.check(w, 200)
		if strings.Contains(w.Body.String(), "7731") {
			t.Fatalf("delivery content leaked on %s", path)
		}
	}
	// Whitespace-only content is stored as empty (no automatic delivery).
	e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"kind": "digital", "delivery_content": " \n "})), 303)
	if err := e.DB.QueryRow("SELECT delivery_content FROM products WHERE id=$1", p).Scan(&dc); err != nil || dc != "" {
		t.Fatalf("blank delivery content stored as %q %v", dc, err)
	}
}

func TestListingStockEditDoesNotOverwriteReservations(t *testing.T) {
	e := newTestApp(t)
	owner, s := e.user("inv_stock", "vendor")
	p := e.product(owner, "physical") // stock 5
	stockOf := func() int {
		var n int
		if err := e.DB.QueryRow("SELECT stock FROM products WHERE id=$1", p).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// A reservation happens while the vendor has the form open (stock_seen=5).
	if _, err := e.DB.Exec("UPDATE products SET stock=stock-1 WHERE id=$1", p); err != nil {
		t.Fatal(err)
	}
	// Unchanged stock field: other edits save, the reservation is kept.
	e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"title": "Retitled"})), 303)
	if stockOf() != 4 {
		t.Fatalf("stock overwritten: %d", stockOf())
	}
	// Changed stock field based on a stale value: refused with the current value.
	w := e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"stock": "9"}))
	e.check(w, 409)
	if !strings.Contains(w.Body.String(), "Stock changed to 4") || stockOf() != 4 {
		t.Fatalf("stale stock edit: %s stock=%d", w.Body.String(), stockOf())
	}
	e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"stock": "9", "stock_seen": "4"})), 303)
	if stockOf() != 9 {
		t.Fatalf("stock not adjusted: %d", stockOf())
	}
	e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"stock": "0", "stock_seen": "9"})), 303)
	if stockOf() != 0 {
		t.Fatalf("stock not zeroed: %d", stockOf())
	}
	w = e.do("GET", "/product?id="+p, s, nil)
	e.check(w, 200)
	if !strings.Contains(w.Body.String(), "Out of stock.") || strings.Contains(w.Body.String(), "/checkout?id="+p) {
		t.Fatal("out-of-stock listing still offers a draft")
	}
}

func TestListingKindLockedWhileOrdersOpen(t *testing.T) {
	e := newTestApp(t)
	owner, s := e.user("inv_kind", "vendor")
	buyer, _ := e.user("inv_kind_buyer", "buyer")
	p := e.product(owner, "physical")
	o := e.order(buyer, p, "BTC", statePaid)
	w := e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"kind": "digital"}))
	e.check(w, 409)
	var kind string
	e.DB.QueryRow("SELECT kind FROM products WHERE id=$1", p).Scan(&kind)
	if kind != "physical" {
		t.Fatalf("kind changed under an open order: %s", kind)
	}
	if !strings.Contains(e.do("GET", "/listing-edit?id="+p, s, nil).Body.String(), "1 open order(s)") {
		t.Fatal("edit page does not explain the open order")
	}
	// Other fields still save; the order keeps its recorded amount.
	e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"price_btc": "0.5"})), 303)
	var amt int64
	var state string
	if err := e.DB.QueryRow("SELECT amount,state FROM orders WHERE id=$1", o).Scan(&amt, &state); err != nil || amt != 100000 || state != statePaid {
		t.Fatalf("existing order changed: %d %s %v", amt, state, err)
	}
	if _, err := e.DB.Exec("UPDATE orders SET state='completed' WHERE id=$1", o); err != nil {
		t.Fatal(err)
	}
	e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"kind": "digital"})), 303)
}

func TestArchiveHidesListingAndRefusesDrafts(t *testing.T) {
	e := newTestApp(t)
	owner, s := e.user("inv_archive", "vendor")
	buyer, buyerS := e.user("inv_archive_buyer", "buyer")
	_, adminS := e.user("inv_archive_admin", "admin")
	p := e.product(owner, "physical")
	keep := e.product(owner, "digital")
	if _, err := e.DB.Exec("UPDATE products SET title='Still listed' WHERE id=$1", keep); err != nil {
		t.Fatal(err)
	}
	existing := e.order(buyer, p, "BTC", stateAwaitingPayment)
	draft := url.Values{"product_id": {p}, "currency": {"XMR"}}

	w := e.do("POST", "/listings/archive", s, url.Values{"id": {p}})
	e.check(w, 303)
	if w.Header().Get("Location") != "/vendor-dashboard?saved=1" {
		t.Fatalf("redirect %q", w.Header().Get("Location"))
	}
	e.check(e.do("POST", "/listings/archive", s, url.Values{"id": {p}}), 409)
	var archived bool
	var archivedAt *string
	if err := e.DB.QueryRow("SELECT archived,archived_at::text FROM products WHERE id=$1", p).Scan(&archived, &archivedAt); err != nil || !archived || archivedAt == nil {
		t.Fatalf("archive not stored: %v %v %v", archived, archivedAt, err)
	}

	// Hidden from every public listing surface, for buyers and for the owner alike.
	for _, sess := range []string{buyerS, s} {
		w = e.do("GET", "/", sess, nil)
		e.check(w, 200)
		if strings.Contains(w.Body.String(), "/product?id="+p) || !strings.Contains(w.Body.String(), "Still listed") {
			t.Fatal("catalog shows the archived listing or lost the active one")
		}
		w = e.do("GET", "/?q=Test+listing", sess, nil)
		if strings.Contains(w.Body.String(), p) {
			t.Fatal("search shows the archived listing")
		}
		e.check(e.do("GET", "/product?id="+p, sess, nil), 404)
		e.check(e.do("GET", "/checkout?id="+p, sess, nil), 404)
		w = e.do("GET", "/vendor?id="+owner, sess, nil)
		e.check(w, 200)
		if strings.Contains(w.Body.String(), p) {
			t.Fatal("vendor page shows the archived listing")
		}
	}
	// The owner still sees it on the vendor desk with a badge and a restore control.
	w = e.do("GET", "/vendor-dashboard", s, nil)
	e.check(w, 200)
	for _, want := range []string{"/listing-edit?id=" + p, "Archived", `action="/listings/restore"`, "1 archived"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("vendor desk missing %q", want)
		}
	}
	w = e.do("GET", "/listing-edit?id="+p, s, nil)
	e.check(w, 200)
	if !strings.Contains(w.Body.String(), "Restore listing") {
		t.Fatal("edit page does not offer restore")
	}

	// No new drafts; the existing order is untouched and still visible to its buyer.
	w = e.do("POST", "/orders", buyerS, draft)
	e.check(w, 400)
	if !strings.Contains(w.Body.String(), "archived") {
		t.Fatalf("draft refusal reason: %s", w.Body.String())
	}
	var n int
	e.DB.QueryRow("SELECT count(*) FROM orders WHERE product_id=$1", p).Scan(&n)
	if n != 1 {
		t.Fatalf("orders on archived listing: %d", n)
	}
	var state string
	if err := e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", existing).Scan(&state); err != nil || state != stateAwaitingPayment {
		t.Fatalf("existing order changed: %s %v", state, err)
	}
	e.check(e.do("GET", "/order?id="+existing, buyerS, nil), 200)

	// Editing while archived keeps it archived.
	e.check(e.do("POST", "/listings/update", s, inventoryForm(p, nil)), 303)
	e.check(e.do("GET", "/product?id="+p, buyerS, nil), 404)

	// Restore: visible again and drafts work.
	e.check(e.do("POST", "/listings/restore", s, url.Values{"id": {p}}), 303)
	e.check(e.do("POST", "/listings/restore", s, url.Values{"id": {p}}), 409)
	if err := e.DB.QueryRow("SELECT archived,archived_at::text FROM products WHERE id=$1", p).Scan(&archived, &archivedAt); err != nil || archived || archivedAt != nil {
		t.Fatalf("restore not stored: %v %v %v", archived, archivedAt, err)
	}
	e.check(e.do("GET", "/product?id="+p, buyerS, nil), 200)
	e.check(e.do("POST", "/orders", buyerS, draft), 303)

	// An administrator may archive another vendor's listing; the audit says so.
	w = e.do("POST", "/listings/archive", adminS, url.Values{"id": {keep}})
	e.check(w, 303)
	if w.Header().Get("Location") != "/listing-edit?id="+keep+"&saved=1" {
		t.Fatalf("admin redirect %q", w.Header().Get("Location"))
	}
	for like, want := range map[string]int{"Archived listing " + p[:8]: 1, "Restored listing " + p[:8]: 1, "Archived listing " + keep[:8] + " as administrator": 1} {
		if got := inventoryAudits(e, like); got != want {
			t.Errorf("audit %q: %d", like, got)
		}
	}
}

func TestAutoDeliveryCopyFollowsProviders(t *testing.T) {
	e := newTestApp(t)
	owner, s := e.user("inv_auto", "vendor")
	p := e.product(owner, "digital")
	for _, path := range []string{"/listing-edit?id=" + p, "/vendor-dashboard"} {
		w := e.do("GET", path, s, nil)
		e.check(w, 200)
		if !strings.Contains(w.Body.String(), "Automatic delivery requires a payment provider") {
			t.Fatalf("%s: unavailable copy missing without providers", path)
		}
	}
	e.A.payments["XMR"] = inventoryStubProvider{"XMR"}
	e.A.payments["BTC"] = inventoryStubProvider{"BTC"}
	for _, path := range []string{"/listing-edit?id=" + p, "/vendor-dashboard"} {
		w := e.do("GET", path, s, nil)
		e.check(w, 200)
		if strings.Contains(w.Body.String(), "Automatic delivery requires a payment provider") || !strings.Contains(w.Body.String(), "Payment confirmation is configured for BTC, XMR") {
			t.Fatalf("%s: provider copy wrong", path)
		}
	}
}

func TestInventoryMigrationColumns(t *testing.T) {
	e := newTestApp(t)
	var n int
	if err := e.DB.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='products' AND column_name IN ('updated','archived_at','archived','delivery_content')`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("inventory columns: %d %v", n, err)
	}
	if err := e.DB.QueryRow("SELECT count(*) FROM schema_migrations WHERE version=40").Scan(&n); err != nil || n != 1 {
		t.Fatalf("migration 040 not recorded: %d %v", n, err)
	}
}

// inventoryStubProvider only reports a currency; it is never asked to move funds in these tests.
type inventoryStubProvider struct{ currency string }

func (p inventoryStubProvider) Currency() string   { return p.currency }
func (p inventoryStubProvider) Network() string    { return "testnet" }
func (p inventoryStubProvider) Confirmations() int { return 1 }
func (p inventoryStubProvider) NewAddress(context.Context, string) (string, error) {
	return "", fail(409, "stub")
}
func (p inventoryStubProvider) Incoming(context.Context, []string) ([]Incoming, error) {
	return nil, nil
}
func (p inventoryStubProvider) Send(context.Context, string, int64) (string, error) {
	return "", fail(409, "stub")
}
func (p inventoryStubProvider) ValidAddress(string) bool { return false }
