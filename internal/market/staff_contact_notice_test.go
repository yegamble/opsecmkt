package market

import (
	"html"
	"net/http"
	"sync"
	"testing"
)

// PM109-4: a staff member named as the contact, or no longer named, is told in the saving transaction, and the audit row
// says so. DB integration through the /admin/staff-contact form.

const (
	namedLike   = "An administrator named you as this market's staff contact.%"
	unnamedLike = "An administrator removed you as this market's staff contact.%"
)

func TestStaffContactNamingTellsTheStaffMember(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("pm4_admin", "admin")
	firstID, firstSess := e.user("pm4_first", "moderator")
	secondID, _ := e.user("pm4_second", "admin")
	keylessID, _ := e.keylessUser("pm4_keyless", "moderator")
	buyerID, buyerSess := e.user("pm4_buyer", "buyer")
	suspendedID, _ := e.user("pm4_suspended", "moderator")
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", suspendedID)
	notices := func(id, like string) int {
		t.Helper()
		return agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", id, like)
	}
	total := func(id string) int {
		t.Helper()
		return agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND (body LIKE $2 OR body LIKE $3)", id, namedLike, unnamedLike)
	}
	save := func(v string, code int) {
		t.Helper()
		e.check(e.do("POST", "/admin/staff-contact", admin, staffContactForm(v)), code)
	}

	// A non-administrator's post is refused and tells no one.
	e.check(e.do("POST", "/admin/staff-contact", firstSess, staffContactForm("pm4_first")), http.StatusForbidden)
	e.check(e.do("POST", "/admin/staff-contact", buyerSess, staffContactForm("pm4_first")), http.StatusForbidden)
	// Nor does an unconfirmed save.
	e.check(e.do("POST", "/admin/staff-contact", admin, form("staff_contact", "pm4_first", "password", "wrong-password")), http.StatusUnauthorized)
	if n := total(firstID); n != 0 {
		t.Fatalf("%d notices from refused saves", n)
	}

	// Naming: told once, with the audit row saying so. The notice stays through a reload and names no one.
	save("pm4_first", 303)
	if notices(firstID, namedLike) != 1 || !e.auditExact(adminID, "Staff contact changed from (not set) to “pm4_first”; told the staff member named (confirmed with password)") {
		t.Fatalf("naming: %d notices", notices(firstID, namedLike))
	}
	for range 2 {
		mustContain(t, html.UnescapeString(e.body("GET", "/notifications", firstSess, nil, 200)), "An administrator named you as this market's staff contact.")
	}
	body := agStr(e, "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE $2", firstID, namedLike)
	mustNotContain(t, body, "pm4_", adminID, "contact the market staff", "cannot receive encrypted messages")
	mustNotContain(t, e.body("GET", "/notifications", buyerSess, nil, 200), "staff contact")

	// Saving the same value changes nothing and tells no one again.
	save("pm4_first", 409)
	if n := total(firstID); n != 1 {
		t.Fatalf("%d notices after a repeat save", n)
	}

	// A replayed form is one change.
	replay := staffContactForm("pm4_second")
	replay.Set(formNonceField, formNonceFor("/admin/staff-contact"))
	e.check(e.do("POST", "/admin/staff-contact", admin, replay), 303)
	e.do("POST", "/admin/staff-contact", admin, replay)
	if notices(secondID, namedLike) != 1 || notices(firstID, unnamedLike) != 1 {
		t.Fatalf("replay: named %d, un-named %d", notices(secondID, namedLike), notices(firstID, unnamedLike))
	}
	if !e.auditExact(adminID, "Staff contact changed from “pm4_first” to “pm4_second”; told the staff member named; told the staff member no longer named (confirmed with password)") {
		t.Fatal("no audit row for a change between two staff members")
	}
	body = agStr(e, "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE $2", firstID, unnamedLike)
	mustNotContain(t, body, "pm4_", adminID, "contact the market staff")

	// Un-naming in favour of an address tells only the one who was named.
	save("xmpp:help@staff-example.onion", 303)
	if notices(secondID, unnamedLike) != 1 || !e.auditExact(adminID, "Staff contact changed from “pm4_second” to “xmpp:help@staff-example.onion”; told the staff member no longer named (confirmed with password)") {
		t.Fatalf("un-naming: %d notices", notices(secondID, unnamedLike))
	}

	// Staff without a usable key are told what that means.
	save("pm4_keyless", 303)
	mustContain(t, agStr(e, "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE $2", keylessID, namedLike), "cannot receive encrypted messages")

	// A handle that is not active staff is no one's staff contact (A-766), so it tells no one, in either direction.
	save("pm4_buyer", 303)
	save("pm4_suspended", 303)
	save("", 303)
	if total(buyerID) != 0 || total(suspendedID) != 0 {
		t.Fatalf("not staff told: buyer %d, suspended %d", total(buyerID), total(suspendedID))
	}
	if !e.auditExact(adminID, "Staff contact changed from “pm4_suspended” to (not set) (confirmed with password)") {
		t.Fatal("audit claims someone was told")
	}

	// An administrator naming themselves has nothing to be told; the audit row does not claim they were.
	save("pm4_admin", 303)
	save("", 303)
	if n := total(adminID); n != 0 {
		t.Fatalf("self-naming: %d notices", n)
	}
	if !e.auditExact(adminID, "Staff contact changed from (not set) to “pm4_admin” (confirmed with password)") {
		t.Fatal("self-naming audit")
	}

	// Concurrent saves of one value serialise on the setting: one change, one notice.
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Go(func() { codes[i] = e.do("POST", "/admin/staff-contact", admin, staffContactForm("pm4_first")).Code })
	}
	wg.Wait()
	saved := 0
	for _, code := range codes {
		if code == 303 {
			saved++
		} else if code != 409 {
			t.Fatalf("concurrent save: %v", codes)
		}
	}
	if saved != 1 || notices(firstID, namedLike) != 2 {
		t.Fatalf("concurrent saves: %v, %d named notices (one from before)", codes, notices(firstID, namedLike))
	}
}
