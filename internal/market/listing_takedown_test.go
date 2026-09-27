package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A-163: an administrator takes down another vendor's listing with a reason (10–500 characters), confirmed with
// their password. The vendor is notified and sees the reason on /listing-edit and the vendor desk card, can still
// edit the listing but cannot restore it; only an administrator's restore clears it. A takedown stops automatic
// delivery; archiving by a role change never sets a reason.

const takedownReason = "Counterfeit goods: the listing copies a trademarked product."

// takedownForm returns the fields of the /admin/listing form on page for op (takedown or restore) of listing id, as
// the browser would submit them with the administrator's password.
func takedownForm(t *testing.T, page, op, id string) url.Values {
	t.Helper()
	for _, f := range strings.Split(page, "<form ")[1:] {
		f, _, _ = strings.Cut(f, "</form>")
		if !strings.Contains(f, `action="/admin/listing"`) || !strings.Contains(f, `name="action" value="`+op+`"`) || !strings.Contains(f, `name="id" value="`+id+`"`) {
			continue
		}
		mustContain(t, f, `name="password" required`)
		v := url.Values{"password": {testPassword}}
		if op == "takedown" {
			mustContain(t, f, `name="reason" required minlength="10" maxlength="500"`)
		}
		for _, m := range hiddenInput.FindAllStringSubmatch(f, -1) {
			if m[1] != "csrf" {
				v.Set(m[1], m[2])
			}
		}
		return v
	}
	t.Fatalf("no %s form for listing %s", op, id)
	return nil
}

func withField(f url.Values, k, v string) url.Values {
	c := url.Values{}
	for key, vals := range f {
		c[key] = append([]string(nil), vals...)
	}
	c.Set(k, v)
	return c
}

// takenDown reports the listing's archived flag and takedown reason ("" when NULL).
func takenDown(e *testEnv, id string) (bool, string) {
	e.t.Helper()
	var archived bool
	var reason string
	if err := e.DB.QueryRow("SELECT archived,coalesce(admin_archive_reason,'') FROM products WHERE id=$1", id).Scan(&archived, &reason); err != nil {
		e.t.Fatal(err)
	}
	return archived, reason
}

func TestAdminTakedownSurvivesVendorRestore(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("td_admin", "admin")
	vendorID, vendor := e.user("td_vendor", "vendor")
	_, buyer := e.user("td_buyer", "buyer")
	_, moderator := e.user("td_mod", "moderator")
	p := e.product(vendorID, "physical")
	edit := "/listing-edit?id=" + p

	// The administrator is offered a takedown form (reason and password) instead of the plain archive button; the
	// vendor keeps the plain archive button and is not offered the administrator form.
	adminPage := e.body("GET", edit, admin, nil, 200)
	take := takedownForm(t, adminPage, "takedown", p)
	mustNotContain(t, adminPage, `action="/listings/archive"`)
	vendorPage := e.body("GET", edit, vendor, nil, 200)
	mustContain(t, vendorPage, `action="/listings/archive"`)
	mustNotContain(t, vendorPage, `action="/admin/listing"`)

	// Refusals change nothing: other roles, a reason too short or too long (counted in characters after trimming),
	// a missing or wrong password, an unknown choice or listing, and the plain archive by an administrator.
	audits, notes := agInt(e, "SELECT count(*) FROM audit_events"), agInt(e, "SELECT count(*) FROM notifications")
	for _, s := range []string{vendor, buyer, moderator} {
		agExpect(e, "/admin/listing", s, withField(take, "reason", takedownReason), 403, "")
	}
	agExpect(e, "/admin/listing", admin, withField(take, "reason", " \r\n"+strings.Repeat("x", 9)+"\r\n "), 400, "10–500 characters")
	agExpect(e, "/admin/listing", admin, withField(take, "reason", strings.Repeat("é", 501)), 400, "10–500 characters")
	agExpect(e, "/admin/listing", admin, withField(withField(take, "reason", takedownReason), "password", ""), 400, "Enter your current password")
	agExpect(e, "/admin/listing", admin, withField(withField(take, "reason", takedownReason), "password", "wrong-password-here"), 401, "Password incorrect")
	agExpect(e, "/admin/listing", admin, withField(withField(take, "reason", takedownReason), "action", "delete"), 400, "Choose")
	agExpect(e, "/admin/listing", admin, withField(withField(take, "reason", takedownReason), "id", "missing"), 404, "Listing not found")
	agExpect(e, "/listings/archive", admin, form("id", p), 400, "reason")
	if archived, reason := takenDown(e, p); archived || reason != "" {
		t.Fatalf("refused takedown changed the listing: archived=%v reason=%q", archived, reason)
	}
	if agInt(e, "SELECT count(*) FROM audit_events") != audits || agInt(e, "SELECT count(*) FROM notifications") != notes {
		t.Fatal("refused takedown wrote an audit row or notification")
	}

	// The takedown: archived with the trimmed reason, audited on both accounts and notified once, link-free.
	w := e.do("POST", "/admin/listing", admin, withField(take, "reason", "  "+takedownReason+"\r\n"))
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != edit+"&saved=1" {
		t.Fatalf("redirect %q", loc)
	}
	if archived, reason := takenDown(e, p); !archived || reason != takedownReason {
		t.Fatalf("takedown not stored: archived=%v reason=%q", archived, reason)
	}
	short := p[:8]
	if !e.auditExact(adminID, "Took down listing "+short+" of td_vendor: "+takedownReason+" (confirmed with password)") {
		t.Fatal("administrator audit row missing")
	}
	if !e.auditExact(vendorID, "Listing "+short+" taken down by administrator td_admin: "+takedownReason) {
		t.Fatal("vendor audit row missing")
	}
	if n := agInt(e, "SELECT count(*) FROM audit_events"); n != audits+2 {
		t.Fatalf("takedown wrote %d audit rows, want 2", n-audits)
	}
	if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", vendorID); n != 1 {
		t.Fatalf("vendor notifications: %d", n)
	}
	note := agStr(e, "SELECT body FROM notifications WHERE user_id=$1", vendorID)
	mustContain(t, note, takedownReason, "Test listing", short, "contact the market staff", "not delivered automatically")
	mustNotContain(t, note, "http", "/listing", "?id=", "<")
	agExpect(e, "/admin/listing", admin, withField(take, "reason", takedownReason), 409, "already taken down")

	// The reason stays off public pages; the vendor sees it on the edit page and the desk card, with no restore control.
	e.check(e.do("GET", "/product?id="+p, randomToken(), nil), 404)
	for _, s := range []string{randomToken(), buyer, vendor} {
		for _, path := range []string{"/", "/vendor?id=" + vendorID} {
			mustNotContain(t, e.body("GET", path, s, nil, 200), takedownReason)
		}
	}
	vendorPage = e.body("GET", edit, vendor, nil, 200)
	mustContain(t, vendorPage, "Taken down by an administrator", takedownReason, "contact the market staff")
	mustNotContain(t, vendorPage, `action="/listings/restore"`, `action="/admin/listing"`)
	desk := e.body("GET", "/vendor-dashboard", vendor, nil, 200)
	mustContain(t, desk, "Taken down", takedownReason, "1 archived")
	mustNotContain(t, desk, `action="/listings/restore"`)

	// The vendor's restore is refused with the reason; editing is still allowed and keeps the takedown.
	agExpect(e, "/listings/restore", vendor, form("id", p), 409, takedownReason)
	agExpect(e, "/listings/restore", vendor, form("id", p), 409, "contact the market staff")
	e.check(e.do("POST", "/listings/update", vendor, inventoryForm(p, nil)), 303)
	if archived, reason := takenDown(e, p); !archived || reason != takedownReason {
		t.Fatalf("vendor edit changed the takedown: archived=%v reason=%q", archived, reason)
	}
	e.check(e.do("POST", "/listings/archive", vendor, form("id", p)), 409)
	// An administrator restores only through the confirmed form.
	agExpect(e, "/listings/restore", admin, form("id", p), 409, "password")
	if archived, _ := takenDown(e, p); !archived {
		t.Fatal("plain restore cleared a takedown")
	}

	adminPage = e.body("GET", edit, admin, nil, 200)
	mustContain(t, adminPage, takedownReason)
	restore := takedownForm(t, adminPage, "restore", p)
	for _, s := range []string{vendor, buyer, moderator} {
		agExpect(e, "/admin/listing", s, restore, 403, "")
	}
	w = e.do("POST", "/admin/listing", admin, restore)
	e.check(w, 303)
	if archived, reason := takenDown(e, p); archived || reason != "" {
		t.Fatalf("restore not stored: archived=%v reason=%q", archived, reason)
	}
	if !e.auditExact(adminID, "Restored listing "+short+" of td_vendor after a takedown (confirmed with password)") ||
		!e.auditExact(vendorID, "Listing "+short+" restored by administrator td_admin") {
		t.Fatal("restore audit rows missing")
	}
	if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", vendorID); n != 2 {
		t.Fatalf("vendor notifications after restore: %d", n)
	}
	e.check(e.do("GET", "/product?id="+p, randomToken(), nil), 200)
	agExpect(e, "/admin/listing", admin, restore, 409, "not taken down")
	// An administrator's own listing is archived from the vendor desk, not taken down.
	own := e.product(adminID, "physical")
	agExpect(e, "/admin/listing", admin, withField(withField(take, "reason", takedownReason), "id", own), 400, "your own listing")
}

func TestAdminTakedownStopsAutoDelivery(t *testing.T) {
	p := newPayEnv(t)
	const secret = "AUTO-DELIVERY-SECRET-A163"
	p.productID = p.product(p.vendor.ID, "digital")
	if _, err := p.DB.Exec("UPDATE products SET delivery_content=$2 WHERE id=$1", p.productID, secret); err != nil {
		t.Fatal(err)
	}
	deliver, deliverAddr := p.newOrder(stateAwaitingPayment)
	cancel, cancelAddr := p.newOrder(stateAwaitingPayment)

	take := takedownForm(t, p.page("/listing-edit?id="+p.productID, p.adminSess), "takedown", p.productID)
	p.check(p.do("POST", "/admin/listing", p.adminSess, withField(take, "reason", takedownReason)), 303)

	// Payment confirms: the orders become paid, but nothing is released and the history says why.
	p.paidByWatcher(deliver, deliverAddr, "a163-deliver")
	p.paidByWatcher(cancel, cancelAddr, "a163-cancel")
	for _, o := range []string{deliver, cancel} {
		if p.state(o) != statePaid {
			t.Fatalf("order %s: %s", o[:8], p.state(o))
		}
		if n := p.count("SELECT count(*) FROM deliveries WHERE order_id=$1", o); n != 0 {
			t.Fatalf("taken-down listing released its content for %s", o[:8])
		}
		if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND note LIKE 'Not delivered automatically%'", o); n != 1 {
			t.Fatalf("withheld-delivery note for %s: %d", o[:8], n)
		}
		mustNotContain(t, p.page("/order?id="+o, p.buyerSess), secret, takedownReason)
	}

	// The vendor delivers one by hand and cancels (refunds) the other.
	p.check(p.do("POST", "/orders/deliver", p.vendorSess, form("order_id", deliver, "content", "Delivered by hand.")), 303)
	if p.state(deliver) != stateDelivered || p.str("SELECT content FROM deliveries WHERE order_id=$1", deliver) != "Delivered by hand." {
		t.Fatal("manual delivery after a takedown failed")
	}
	p.check(p.do("POST", "/orders/cancel", p.vendorSess, payoutConfirmed(form("order_id", cancel, "from", statePaid), 100000)), 303)
	if p.state(cancel) != stateCancelled {
		t.Fatalf("cancel after a takedown: %s", p.state(cancel))
	}

	// Once an administrator restores the listing, automatic delivery works again.
	restore := takedownForm(t, p.page("/listing-edit?id="+p.productID, p.adminSess), "restore", p.productID)
	p.check(p.do("POST", "/admin/listing", p.adminSess, restore), 303)
	again, againAddr := p.newOrder(stateAwaitingPayment)
	p.fake.Deposit(againAddr, "a163-again", 0, 100000, 3)
	p.poll()
	if p.state(again) != stateDelivered || p.str("SELECT content FROM deliveries WHERE order_id=$1", again) != secret {
		t.Fatalf("restored listing not delivered automatically: %s", p.state(again))
	}
}

func TestDemotionArchiveIsNotATakedown(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("tdd_admin", "admin")
	vendorID, vendor := e.user("tdd_vendor", "vendor")
	plain := e.product(vendorID, "physical")
	taken := e.product(vendorID, "physical")
	const reason = "Ten chars!" // the shortest accepted reason
	take := takedownForm(t, e.body("GET", "/listing-edit?id="+taken, admin, nil, 200), "takedown", taken)
	agExpect(e, "/admin/listing", admin, withField(take, "reason", reason), 303, "")

	// Demotion archives the active listing without a reason and leaves the takedown as it was.
	agExpect(e, "/admin", admin, form("action", "role", "role", "buyer", "handle", "tdd_vendor"), 303, "")
	if archived, r := takenDown(e, plain); !archived || r != "" {
		t.Fatalf("demotion archive: archived=%v reason=%q", archived, r)
	}
	if _, r := takenDown(e, taken); r != reason {
		t.Fatalf("demotion changed the takedown reason to %q", r)
	}
	// An archived listing without a reason still offers the administrator a takedown beside the plain restore.
	archivedPage := e.body("GET", "/listing-edit?id="+plain, admin, nil, 200)
	takedownForm(t, archivedPage, "takedown", plain)
	mustContain(t, archivedPage, `action="/listings/restore"`)
	// An administrator cannot restore a listing whose owner is not a vendor; the takedown stays.
	restore := takedownForm(t, e.body("GET", "/listing-edit?id="+taken, admin, nil, 200), "restore", taken)
	agExpect(e, "/admin/listing", admin, restore, 409, "no longer a vendor")
	if _, r := takenDown(e, taken); r != reason {
		t.Fatal("refused restore cleared the takedown")
	}

	// Promoted again, the vendor restores the demotion-archived listing as before, but not the taken-down one.
	agExpect(e, "/admin", admin, form("action", "role", "role", "vendor", "handle", "tdd_vendor"), 303, "")
	mustContain(t, e.body("GET", "/listing-edit?id="+plain, vendor, nil, 200), `action="/listings/restore"`)
	agExpect(e, "/listings/restore", vendor, form("id", plain), 303, "")
	agExpect(e, "/listings/restore", vendor, form("id", taken), 409, reason)
	if archived, _ := takenDown(e, plain); archived {
		t.Fatal("vendor restore of a demotion-archived listing failed")
	}
}

// PR #24 re-check N-1: an administrator's takedown locked the listing and then, through the foreign keys of its
// audit and notification inserts, the vendor's users row; a role change holding that row and then archiving the
// vendor's listings deadlocked with it (40P01, a 500). The takedown now takes the users rows first, as lockListing
// does, and waits for the role change instead.
// PR #27 NIT-1: PostgreSQL refuses a NUL byte in text, so a takedown reason carrying one gets the reason's 400,
// before the password is checked, not a 500 from the update.
func TestAdminTakedownReasonWithNULRefused(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("td_nul_admin", "admin")
	vendorID, _ := e.user("td_nul_vendor", "vendor")
	p := e.product(vendorID, "physical")
	take := takedownForm(t, e.body("GET", "/listing-edit?id="+p, admin, nil, 200), "takedown", p)
	agExpect(e, "/admin/listing", admin, withField(take, "reason", "Counterfeit \x00 goods in this listing."), 400, "10–500 characters")
	if archived, reason := takenDown(e, p); archived || reason != "" {
		t.Fatalf("after a NUL reason: archived=%v reason=%q", archived, reason)
	}
}

func TestAdminTakedownDoesNotDeadlockWithRoleChange(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("td_deadlock_admin", "admin")
	vendorID, _ := e.user("td_deadlock_vendor", "vendor")
	p := e.product(vendorID, "physical")
	take := withField(takedownForm(t, e.body("GET", "/listing-edit?id="+p, admin, nil, 200), "takedown", p), "reason", takedownReason)
	w := besideRoleChange(t, e, vendorID, func() *httptest.ResponseRecorder { return e.do("POST", "/admin/listing", admin, take) })
	if w.Code != 303 {
		t.Fatalf("takedown beside a role change: %d %s", w.Code, w.Body.String())
	}
	if archived, reason := takenDown(e, p); !archived || reason != takedownReason {
		t.Fatalf("after the takedown: archived=%v reason=%q", archived, reason)
	}
}
