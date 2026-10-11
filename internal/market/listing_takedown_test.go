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
		if len(v.Get(formNonceField)) != 32 {
			t.Fatal("admin listing form has no 32-character receipt nonce")
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
	freshTake := takedownForm(t, e.body("GET", edit, admin, nil, 200), "takedown", p)
	if freshTake.Get(formNonceField) == take.Get(formNonceField) {
		t.Fatal("separately rendered takedown forms shared a receipt nonce")
	}
	freshTake.Set("reason", takedownReason)
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
	takeSubmitted := withField(take, "reason", "  "+takedownReason+"\r\n")
	w := e.do("POST", "/admin/listing", admin, takeSubmitted)
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != edit {
		t.Fatalf("redirect %q", loc)
	}
	takeReplay := e.do("POST", "/admin/listing", admin, takeSubmitted)
	e.check(takeReplay, 303)
	if takeReplay.Header().Get("Location") != w.Header().Get("Location") {
		t.Fatalf("takedown replay location %q, original %q", takeReplay.Header().Get("Location"), w.Header().Get("Location"))
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
	agExpect(e, "/admin/listing", admin, freshTake, 409, "already taken down")

	// The reason stays off public pages; the vendor sees it on the edit page and the desk card, with no restore control.
	e.check(e.do("GET", "/product?id="+p, randomToken(), nil), 404)
	for _, s := range []string{randomToken(), buyer, vendor} {
		for _, path := range []string{"/", "/vendor?id=" + vendorID} {
			mustNotContain(t, e.body("GET", path, s, nil, 200), takedownReason)
		}
	}
	vendorPage = e.body("GET", edit, vendor, nil, 200)
	mustContain(t, vendorPage, "Taken down by an administrator", takedownReason, "ask an administrator to review it. "+askOperatorLine)
	mustNotContain(t, vendorPage, `action="/listings/restore"`, `action="/admin/listing"`)
	desk := e.body("GET", "/vendor-dashboard", vendor, nil, 200)
	mustContain(t, desk, "Taken down", takedownReason, "1 archived")
	mustNotContain(t, desk, `action="/listings/restore"`)

	// The vendor's restore is refused with the reason; editing is still allowed and keeps the takedown.
	agExpect(e, "/listings/restore", vendor, form("id", p), 409, takedownReason)
	agExpect(e, "/listings/restore", vendor, form("id", p), 409, "ask an administrator to review it. "+askOperatorLine)
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
	freshRestore := takedownForm(t, e.body("GET", edit, admin, nil, 200), "restore", p)
	if freshRestore.Get(formNonceField) == restore.Get(formNonceField) {
		t.Fatal("separately rendered restore forms shared a receipt nonce")
	}
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
	restoreReplay := e.do("POST", "/admin/listing", admin, restore)
	e.check(restoreReplay, 303)
	if restoreReplay.Header().Get("Location") != w.Header().Get("Location") {
		t.Fatalf("restore replay location %q, original %q", restoreReplay.Header().Get("Location"), w.Header().Get("Location"))
	}
	agExpect(e, "/admin/listing", admin, freshRestore, 409, "not taken down")
	oldTakedownReplay := e.do("POST", "/admin/listing", admin, takeSubmitted)
	e.check(oldTakedownReplay, 303)
	if archived, reason := takenDown(e, p); archived || reason != "" {
		t.Fatalf("old takedown receipt re-executed after restore: archived=%v reason=%q", archived, reason)
	}
	if n := agInt(e, `SELECT count(*) FROM audit_events WHERE
		(user_id=$1 AND action IN ($3,$4)) OR (user_id=$2 AND action IN ($5,$6))`, adminID, vendorID,
		"Took down listing "+short+" of td_vendor: "+takedownReason+" (confirmed with password)",
		"Restored listing "+short+" of td_vendor after a takedown (confirmed with password)",
		"Listing "+short+" taken down by administrator td_admin: "+takedownReason,
		"Listing "+short+" restored by administrator td_admin"); n != 4 {
		t.Fatalf("takedown/restore replays left %d matching audits, want 4", n)
	}
	if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", vendorID); n != 2 {
		t.Fatalf("vendor notifications after restore: %d", n)
	}
	e.check(e.do("GET", "/product?id="+p, randomToken(), nil), 200)
	// An administrator's own listing is archived from the vendor desk, not taken down.
	own := e.product(adminID, "physical")
	agExpect(e, "/admin/listing", admin, withField(withField(take, "reason", takedownReason), "id", own), 400, "your own listing")
}

func TestAdminTakedownStopsAutoDelivery(t *testing.T) {
	x := newEscrowEnv(t)
	e := x.testEnv
	_, admin := e.user("td_auto_admin", "admin")
	const secret = "AUTO-DELIVERY-SECRET-A163"
	product := x.product
	if _, err := e.DB.Exec("UPDATE products SET delivery_content=$2 WHERE id=$1", product, secret); err != nil {
		t.Fatal(err)
	}
	deliver := e.order(x.buyer, product, "BTC", stateDraft)
	deliverAddr := x.mustCheckout(deliver, regtestAddr("takedown-deliver-refund"))
	cancel := e.order(x.buyer, product, "BTC", stateDraft)
	cancelAddr := x.mustCheckout(cancel, regtestAddr("takedown-cancel-refund"))

	take := takedownForm(t, e.body("GET", "/listing-edit?id="+product, admin, nil, 200), "takedown", product)
	e.check(e.do("POST", "/admin/listing", admin, withField(take, "reason", takedownReason)), 303)

	// Escrow funding confirms: the orders become paid, but no digital content is delivered automatically.
	x.core.deposit(deliverAddr, txidOf("a163-deliver"), 0, 100000, 3)
	x.core.deposit(cancelAddr, txidOf("a163-cancel"), 0, 100000, 3)
	x.poll()
	for _, o := range []string{deliver, cancel} {
		if x.state(o) != statePaid {
			t.Fatalf("order %s: %s", o[:8], x.state(o))
		}
		if n := agInt(e, "SELECT count(*) FROM deliveries WHERE order_id=$1", o); n != 0 {
			t.Fatalf("taken-down listing released its content for %s", o[:8])
		}
		if n := agInt(e, "SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND note LIKE 'Not delivered automatically%'", o); n != 1 {
			t.Fatalf("withheld-delivery note for %s: %d", o[:8], n)
		}
		mustNotContain(t, x.page("/order?id="+o, x.buyerSess), secret, takedownReason)
	}

	// The vendor delivers one by hand and requests an escrow refund for the other.
	e.check(e.do("POST", "/orders/deliver", x.vendorSess, form("order_id", deliver, "content", "Delivered by hand.")), 303)
	if x.state(deliver) != stateCompleted || agStr(e, "SELECT content FROM deliveries WHERE order_id=$1", deliver) != "Delivered by hand." {
		t.Fatal("manual delivery after a takedown failed")
	}
	e.check(e.do("POST", "/orders/cancel", x.vendorSess, payoutConfirmed(form("order_id", cancel, "from", statePaid), 100000)), 303)
	if x.state(cancel) != stateCancelled || agInt(e, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='refund'", cancel) != 1 {
		t.Fatalf("escrow refund request after a takedown: %s", x.state(cancel))
	}

	// Once an administrator restores the listing, automatic delivery works again.
	restore := takedownForm(t, e.body("GET", "/listing-edit?id="+product, admin, nil, 200), "restore", product)
	e.check(e.do("POST", "/admin/listing", admin, restore), 303)
	again := e.order(x.buyer, product, "BTC", stateDraft)
	againAddr := x.mustCheckout(again, regtestAddr("takedown-restored-refund"))
	x.core.deposit(againAddr, txidOf("a163-again"), 0, 100000, 3)
	x.poll()
	if x.state(again) != stateCompleted || agStr(e, "SELECT content FROM deliveries WHERE order_id=$1", again) != secret {
		t.Fatalf("restored listing not delivered automatically: %s", x.state(again))
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
	agExpect(e, "/admin", admin, form("action", "role", "password", testPassword, "role", "buyer", "handle", "tdd_vendor"), 303, "")
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
	agExpect(e, "/admin", admin, form("action", "role", "password", testPassword, "role", "vendor", "handle", "tdd_vendor"), 303, "")
	mustContain(t, e.body("GET", "/listing-edit?id="+plain, vendor, nil, 200), `action="/listings/restore"`)
	agExpect(e, "/listings/restore", vendor, form("id", plain), 303, "")
	agExpect(e, "/listings/restore", vendor, form("id", taken), 409, reason)
	if archived, _ := takenDown(e, plain); archived {
		t.Fatal("vendor restore of a demotion-archived listing failed")
	}
}

// PR #27 NIT-1: PostgreSQL refuses a NUL byte in text, so a takedown reason carrying one gets a 400 before the
// password is checked, not a 500 from the update; since QA5W3B-5 it is the form's own refusal, before the handler.
func TestAdminTakedownReasonWithNULRefused(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("td_nul_admin", "admin")
	vendorID, _ := e.user("td_nul_vendor", "vendor")
	p := e.product(vendorID, "physical")
	take := takedownForm(t, e.body("GET", "/listing-edit?id="+p, admin, nil, 200), "takedown", p)
	agExpect(e, "/admin/listing", admin, withField(take, "reason", "Counterfeit \x00 goods in this listing."), 400, invalidTextMsg)
	if archived, reason := takenDown(e, p); archived || reason != "" {
		t.Fatalf("after a NUL reason: archived=%v reason=%q", archived, reason)
	}
}

// PR #24 re-check N-1: an administrator's takedown locked the listing and then, through the foreign keys of its
// audit and notification inserts, the vendor's users row; a role change holding that row and then archiving the
// vendor's listings deadlocked with it (40P01, a 500). The takedown now takes the users rows first, as lockListing
// does, and waits for the role change instead.
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
