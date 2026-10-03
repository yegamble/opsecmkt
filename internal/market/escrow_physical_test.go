package market

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// Escrow Phase 2, slice P2-a (WR-escrow A-247, A-378; P2-R3 setting, P2-R5 field and snapshot, P2-R8): migration 079,
// the administrator's release deadline length, the listing's dispatch window and physical escrow checkout. DB
// integration tests against the simulated Core of escrow_fake_test.go (simulated RPC, not chain evidence).

func physString(t *testing.T, e *testEnv, q string, args ...any) string {
	t.Helper()
	var s string
	if err := e.DB.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// physicalListing is a physical listing of vendor with a dispatch window of days (0 = none set).
func (x *escrowEnv) physicalListing(vendor string, days int) string {
	x.t.Helper()
	id := x.testEnv.product(vendor, "physical")
	if days > 0 {
		agExec(x.testEnv, "UPDATE products SET dispatch_days=$2 WHERE id=$1", id, days)
	}
	return id
}

// physicalDraft is the buyer's BTC draft on a new physical listing with a dispatch window of days, whose vendor can
// receive encrypted messages.
func (x *escrowEnv) physicalDraft(days int) string {
	x.t.Helper()
	_, pub := testPGPKey(x.t, "physvendor"+randomToken()[:6])
	x.setKey(x.vendor, pub)
	return x.testEnv.order(x.buyer, x.physicalListing(x.vendor, days), "BTC", stateDraft)
}

// snapshots is the escrow's release_days, dispatch_days and whether its deadline columns are still unset.
func (x *escrowEnv) snapshots(order string) string {
	x.t.Helper()
	return physString(x.t, x.testEnv, `SELECT coalesce(release_days::text,'-')||' '||coalesce(dispatch_days::text,'-')||' '||(ship_by IS NULL)||' '||(release_at IS NULL)||' '||(reminded_at IS NULL)||' '||release_recheck
		FROM escrows WHERE order_id=$1`, order)
}

// A physical listing with a dispatch window reaches escrow checkout; the escrow row snapshots the release deadline
// length in force and the listing's window, and neither a later change of the setting nor of the listing reaches it.
// Digital escrow keeps no snapshot; a physical listing without a window, or one taken down, takes no escrow.
func TestEscrowPhysicalCheckout(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("eadmin_"+randomToken()[:6], "admin")
	// Without a window the listing takes no escrow: nothing is offered and the checkout is refused.
	none := x.physicalDraft(0)
	if body := x.page("/order?id="+none, x.buyerSess); strings.Contains(body, "Pay into an escrow") {
		t.Fatal("escrow offered for a physical listing without a dispatch window")
	}
	if w := x.checkout(none, regtestAddr("phys-refund-none")); w.Code != 409 || !strings.Contains(w.Body.String(), "dispatch window") {
		t.Fatalf("no window: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(none)

	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('escrow_release_days','21')")
	order := x.physicalDraft(5)
	body := x.page("/order?id="+order, x.buyerSess)
	for _, want := range []string{"Pay into an escrow", `action="/orders/escrow"`, "shipped by the vendor"} {
		if !strings.Contains(body, want) {
			t.Fatalf("offer lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "delivers its content automatically") {
		t.Fatal("the physical offer says the content is delivered automatically")
	}
	x.mustCheckout(order, regtestAddr("phys-refund-1"))
	if got := x.snapshots(order); got != "21 5 true true true false" {
		t.Fatalf("snapshots %q", got)
	}
	if x.state(order) != stateAwaitingPayment {
		t.Fatal("not awaiting payment")
	}
	// Funded, the physical order waits for its shipment: the page does not say content could not be delivered.
	x.fund(order, "phys-fund-1", 3, 100000)
	if x.state(order) != statePaid {
		t.Fatalf("funded physical escrow order is %s", x.state(order))
	}
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		body := x.page("/order?id="+order, sess)
		if !strings.Contains(body, "The vendor ships the goods") || strings.Contains(body, "content could not be delivered") {
			t.Fatalf("funded physical escrow order page:\n%s", body)
		}
	}
	// A later setting and a later listing edit change only later escrows.
	if w := x.do("POST", "/admin/escrow-release-days", admin, url.Values{"release_days": {"10"}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("setting: %d %s", w.Code, w.Body.String())
	}
	product := physString(t, x.testEnv, "SELECT product_id FROM orders WHERE id=$1", order)
	agExec(x.testEnv, "UPDATE products SET dispatch_days=9 WHERE id=$1", product)
	if got := x.snapshots(order); got != "21 5 true true true false" {
		t.Fatalf("snapshots after the changes %q", got)
	}
	second := x.testEnv.order(x.buyer, product, "BTC", stateDraft)
	x.mustCheckout(second, regtestAddr("phys-refund-2"))
	if got := x.snapshots(second); got != "10 9 true true true false" {
		t.Fatalf("second escrow snapshots %q", got)
	}
	// Digital escrow keeps its Phase 1 row: no snapshot.
	digital := x.draft()
	x.mustCheckout(digital, regtestAddr("phys-refund-3"))
	if got := x.snapshots(digital); got != "- - true true true false" {
		t.Fatalf("digital escrow snapshots %q", got)
	}
	// A taken-down physical listing takes no escrow.
	down := x.physicalDraft(3)
	agExec(x.testEnv, "UPDATE products SET archived=true,admin_archive_reason='test takedown reason' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", down)
	if w := x.checkout(down, regtestAddr("phys-refund-4")); w.Code != 409 {
		t.Fatalf("taken down: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(down)
}

// A-83 for escrow: a physical order's escrow checkout is refused, before any escrow RPC, when the vendor has no key
// that can receive an encrypted shipping address; the order page gives the reason instead of the form.
func TestEscrowCheckoutVendorKeyUnusable(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.physicalDraft(3)
	variants := map[string]string{"no key": ""}
	for name, armored := range testUnusableKeys(t, "escrowvendor") {
		variants[name] = armored
	}
	for variant, armored := range variants {
		x.setKey(x.vendor, armored)
		calls, imports := len(x.core.methods()), len(x.core.importedDescriptors())
		w := x.checkout(order, regtestAddr("phys-key-"+variant))
		if w.Code != 409 || !strings.Contains(w.Body.String(), noVendorKeyReason) {
			t.Fatalf("%s: %d %s", variant, w.Code, w.Body.String())
		}
		if got := x.core.methods()[calls:]; len(got) != 0 {
			t.Fatalf("%s: the node was called: %v", variant, got)
		}
		if len(x.core.importedDescriptors()) != imports {
			t.Fatalf("%s: a descriptor was imported", variant)
		}
		x.nothingWritten(order)
		body := x.page("/order?id="+order, x.buyerSess)
		if !strings.Contains(strings.ReplaceAll(body, "&#39;", "'"), noVendorKeyReason) || strings.Contains(body, `action="/orders/escrow"`) {
			t.Fatalf("%s: the order page does not give the reason instead of the form:\n%s", variant, body)
		}
	}
	// A digital escrow of the same vendor needs no shipping address: unaffected by the key.
	x.mustCheckout(x.draft(), regtestAddr("phys-key-digital"))
	// A usable key restores the checkout.
	_, pub := testPGPKey(t, "usable_escrow_vendor")
	x.setKey(x.vendor, pub)
	x.mustCheckout(order, regtestAddr("phys-key-usable"))
}

// P2-R3, the setting part: the administrator sets the release deadline length (7 to 30 whole days, default 14) on
// /admin with a password; the audit row names the old and the new days only; an escrow already created keeps its own.
func TestEscrowReleaseDaysSetting(t *testing.T) {
	x := newEscrowEnv(t)
	adminID, admin := x.user("eadmin_"+randomToken()[:6], "admin")
	_, mod := x.user("emod_"+randomToken()[:6], "moderator")
	page := x.page("/admin", admin)
	for _, want := range []string{`action="/admin/escrow-release-days"`, `name="release_days"`, "14 days", "Applies to escrow orders placed from now on; existing orders keep theirs."} {
		if !strings.Contains(page, want) {
			t.Fatalf("admin page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "shipped from now on") {
		t.Fatal("the help text says the setting applies to orders shipped from now on")
	}
	post := func(sess, days, password string) (int, string) {
		w := x.do("POST", "/admin/escrow-release-days", sess, url.Values{"release_days": {days}, "password": {password}})
		return w.Code, w.Body.String()
	}
	for _, c := range []struct {
		name, sess, days, password string
		code                       int
	}{
		{"a moderator", mod, "21", testPassword, 403},
		{"a vendor", x.vendorSess, "21", testPassword, 403},
		{"six days", admin, "6", testPassword, 400},
		{"thirty-one days", admin, "31", testPassword, 400},
		{"zero", admin, "0", testPassword, 400},
		{"negative", admin, "-7", testPassword, 400},
		{"fractional", admin, "14.5", testPassword, 400},
		{"not a number", admin, "two weeks", testPassword, 400},
		{"empty", admin, "", testPassword, 400},
		{"a wrong password", admin, "21", "wrong password 321", 401},
		{"no change", admin, "14", testPassword, 409},
	} {
		if code, body := post(c.sess, c.days, c.password); code != c.code {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	if agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key='escrow_release_days'") != 0 {
		t.Fatal("a refused save was stored")
	}
	before := x.physicalDraft(3)
	x.mustCheckout(before, regtestAddr("days-refund-0"))
	w := x.do("POST", "/admin/escrow-release-days", admin, url.Values{"release_days": {"21"}, "password": {testPassword}})
	if w.Code != 303 || w.Header().Get("Location") != "/admin#escrow-release-days" || x.cookie(w, "notice") != "escrow-release-days" {
		t.Fatalf("save: %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	if got := agStrings(t, x.testEnv, "SELECT action FROM audit_events WHERE user_id=$1 AND action LIKE '%release deadline%'", adminID); len(got) != 1 ||
		got[0] != "Changed the Bitcoin escrow release deadline (regtest): 14 → 21 days (confirmed with password)" {
		t.Fatalf("audit %q", got)
	}
	if page := x.page("/admin", admin); !strings.Contains(page, "21 days") {
		t.Fatalf("admin page after the save:\n%s", page)
	}
	x.mustCheckout(x.physicalDraft(3), regtestAddr("days-refund-1"))
	if got := x.snapshots(before); got != "14 3 true true true false" {
		t.Fatalf("an existing escrow's snapshots changed: %q", got)
	}
	if got := agStrings(t, x.testEnv, "SELECT release_days::text FROM escrows WHERE release_days IS NOT NULL ORDER BY id"); strings.Join(got, ",") != "14,21" {
		t.Fatalf("release_days %v, want the old escrow's 14 and the new one's 21", got)
	}
	// The ends of the range are accepted.
	for _, days := range []string{"7", "30"} {
		if code, body := post(admin, days, testPassword); code != 303 {
			t.Fatalf("%s days: %d %s", days, code, body)
		}
		if physString(t, x.testEnv, "SELECT value FROM settings WHERE key='escrow_release_days'") != days {
			t.Fatalf("%s days not stored", days)
		}
	}
	// An unreadable stored value stops a physical checkout instead of guessing a deadline.
	agExec(x.testEnv, "UPDATE settings SET value='5' WHERE key='escrow_release_days'")
	order := x.physicalDraft(3)
	if w := x.checkout(order, regtestAddr("days-refund-2")); w.Code != 503 {
		t.Fatalf("unreadable setting: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(order)
}

// Migration 079's trigger: once set, release_days, dispatch_days and ship_by never change, and release_at changes
// only to a later time in the statement that sets release_recheck; the columns' CHECKs hold the ranges.
func TestEscrowMigration079Frozen(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.physicalDraft(4)
	x.mustCheckout(order, regtestAddr("frozen-refund-1"))
	mustFail := func(name, set string) {
		t.Helper()
		if _, err := x.DB.Exec("UPDATE escrows SET "+set+" WHERE order_id=$1", order); err == nil {
			t.Errorf("%s: the update was accepted", name)
		}
	}
	mustWork := func(name, set string) {
		t.Helper()
		if _, err := x.DB.Exec("UPDATE escrows SET "+set+" WHERE order_id=$1", order); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The snapshot taken at checkout is frozen.
	mustFail("release_days changed", "release_days=release_days+1")
	mustFail("release_days cleared", "release_days=NULL")
	mustFail("dispatch_days changed", "dispatch_days=dispatch_days+1")
	mustFail("dispatch_days cleared", "dispatch_days=NULL")
	mustWork("the same snapshot written again", "release_days=release_days,dispatch_days=dispatch_days")
	// A deadline is written once.
	mustWork("ship_by set", "ship_by=now()+interval '5 days'")
	mustFail("ship_by moved", "ship_by=ship_by+interval '1 day'")
	mustFail("ship_by cleared", "ship_by=NULL")
	mustWork("release_at set", "release_at=now()+interval '14 days'")
	mustFail("release_at moved earlier", "release_at=release_at-interval '1 day'")
	mustFail("release_at moved later without a recheck", "release_at=release_at+interval '1 day'")
	mustFail("release_at cleared", "release_at=NULL,release_recheck=true")
	mustFail("release_at moved earlier with a recheck", "release_at=release_at-interval '1 day',release_recheck=true")
	mustWork("the recheck moves release_at later", "release_at=release_at+interval '3 days',release_recheck=true")
	mustFail("release_at moved again once rechecked", "release_at=release_at+interval '1 day',release_recheck=true")
	mustWork("reminded_at is not frozen", "reminded_at=now()")
	// The columns of a digital escrow are NULL; a value may be written once, like any other.
	digital := x.draft()
	x.mustCheckout(digital, regtestAddr("frozen-refund-2"))
	if _, err := x.DB.Exec("UPDATE escrows SET release_days=14 WHERE order_id=$1", digital); err != nil {
		t.Fatalf("a first snapshot: %v", err)
	}
	// The ranges.
	for i, set := range []string{"release_days=6", "release_days=31", "dispatch_days=0", "dispatch_days=31"} {
		agExec(x.testEnv, "UPDATE orders SET state='cancelled' WHERE buyer_id=$1 AND state='awaiting_payment'", x.buyer) // the cap on open orders
		fresh := x.draft()
		x.mustCheckout(fresh, regtestAddr("frozen-range-"+strconv.Itoa(i)))
		if _, err := x.DB.Exec("UPDATE escrows SET "+set+" WHERE order_id=$1", fresh); err == nil {
			t.Errorf("%s accepted", set)
		}
	}
	for _, set := range []string{"dispatch_days=0", "dispatch_days=31"} {
		if _, err := x.DB.Exec("UPDATE products SET "+set+" WHERE id=$1", x.product); err == nil {
			t.Errorf("products %s accepted", set)
		}
	}
}

// The listing's dispatch window (A-378): a physical listing takes 1 to 30 days, 3 when none is given; a digital
// listing ignores the field; the create and edit forms offer it and show the stored value.
func TestListingDispatchDaysField(t *testing.T) {
	e := newTestApp(t)
	owner, s := e.user("dispatch_vendor", "vendor")
	stored := func(id string) string {
		return physString(t, e, "SELECT coalesce(dispatch_days::text,'none') FROM products WHERE id=$1", id)
	}
	create := func(kind string, extra map[string]string) (int, string) {
		form := inventoryForm("", map[string]string{"kind": kind, "title": "Dispatch " + randomToken()[:6]})
		if kind == "digital" {
			form.Set("category", "Digital")
		}
		for k, v := range extra {
			form.Set(k, v)
		}
		w := e.do("POST", "/listings", s, form)
		if w.Code != 303 {
			return w.Code, ""
		}
		id, _, _ := strings.Cut(strings.TrimPrefix(w.Header().Get("Location"), "/listing-edit?id="), "#")
		return w.Code, id
	}
	if code, id := create("physical", map[string]string{"dispatch_days": "7"}); code != 303 || stored(id) != "7" {
		t.Fatalf("physical with 7 days: %d %q", code, id)
	}
	if code, id := create("physical", nil); code != 303 || stored(id) != "3" {
		t.Fatalf("physical without the field: %d %q", code, id)
	}
	for _, days := range []string{"1", "30"} {
		if code, id := create("physical", map[string]string{"dispatch_days": days}); code != 303 || stored(id) != days {
			t.Fatalf("physical with %s days: %d", days, code)
		}
	}
	before := agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1", owner)
	for _, bad := range []string{"0", "31", "-1", "3.5", "soon", "99999999999999999999"} {
		if code, _ := create("physical", map[string]string{"dispatch_days": bad}); code != 400 {
			t.Errorf("dispatch_days %q: %d, want 400", bad, code)
		}
	}
	if agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1", owner) != before {
		t.Fatal("a refused listing was stored")
	}
	if code, id := create("digital", map[string]string{"dispatch_days": "9"}); code != 303 || stored(id) != "none" {
		t.Fatalf("a digital listing keeps a window: %d %q", code, id)
	}
	if code, id := create("digital", map[string]string{"dispatch_days": "bad"}); code != 303 || stored(id) != "none" {
		t.Fatalf("a digital listing refuses an ignored field: %d %q", code, id)
	}
	// Edit: the stored value is shown; a change is saved; kind changes set or clear it.
	_, id := create("physical", map[string]string{"dispatch_days": "12"})
	edit := func(extra map[string]string) int {
		form := inventoryForm(id, map[string]string{"kind": "physical", "category": "Hardware"})
		for k, v := range extra {
			form.Set(k, v)
		}
		return e.do("POST", "/listings/update", s, form).Code
	}
	page := e.do("GET", "/listing-edit?id="+id, s, nil).Body.String()
	if !strings.Contains(page, `name="dispatch_days"`) || !strings.Contains(page, `value="12"`) || !strings.Contains(page, "Dispatch within (days)") {
		t.Fatalf("edit page lacks the dispatch field:\n%s", page)
	}
	if !strings.Contains(e.do("GET", "/vendor-dashboard", s, nil).Body.String(), `name="dispatch_days"`) {
		t.Fatal("the create form lacks the dispatch field")
	}
	if edit(map[string]string{"dispatch_days": "20"}) != 303 || stored(id) != "20" {
		t.Fatalf("edit to 20: %s", stored(id))
	}
	if edit(map[string]string{"dispatch_days": "31"}) != 400 || stored(id) != "20" {
		t.Fatalf("edit to 31: %s", stored(id))
	}
	if edit(map[string]string{"dispatch_days": ""}) != 303 || stored(id) != "20" {
		t.Fatalf("an omitted field changed the window: %s", stored(id))
	}
	if edit(map[string]string{"kind": "digital", "category": "Digital", "dispatch_days": "5"}) != 303 || stored(id) != "none" {
		t.Fatalf("physical to digital keeps a window: %s", stored(id))
	}
	if edit(map[string]string{"dispatch_days": ""}) != 303 || stored(id) != "3" {
		t.Fatalf("digital to physical without a window: %s", stored(id))
	}
	// A physical listing saved before migration 079 has no window: the form offers the default, saving sets it.
	old := e.product(owner, "physical")
	if stored(old) != "none" {
		t.Fatal("the fixture has a window")
	}
	if page := e.do("GET", "/listing-edit?id="+old, s, nil).Body.String(); !strings.Contains(page, `name="dispatch_days"`) || !strings.Contains(page, `value="3"`) {
		t.Fatalf("a listing without a window does not offer the default:\n%s", page)
	}
	form := inventoryForm(old, map[string]string{"kind": "physical", "category": "Hardware"})
	if w := e.do("POST", "/listings/update", s, form); w.Code != 303 || stored(old) != "3" {
		t.Fatalf("saving a listing without a window: %d %s", w.Code, stored(old))
	}
}
