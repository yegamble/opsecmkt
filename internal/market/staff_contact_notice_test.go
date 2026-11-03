package market

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strconv"
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
	// A-781 PRIV-2: they are told what naming them shows, while it is true: a key that cannot receive is not shown.
	mustContain(t, body, "Your PGP public key, with its user IDs, is now shown to every signed-in account.")
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
	keyless := agStr(e, "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE $2", keylessID, namedLike)
	mustContain(t, keyless, "cannot receive encrypted messages")
	mustNotContain(t, keyless, "PGP public key")

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

// A-781 PRIV-1: since A-766 the setting stays as saved while its handle is not active staff, so a role or suspension
// change can bring it into force, or end that, with nobody saving it. The account is told in that transaction and the
// audit says so. DB integration through the /admin, /admin/suspend and /admin/ban forms and SetRole; the -set-role
// binary is in cmd/server (TestSetRoleTellsTheSavedStaffContact).

func namedNotices(e *testEnv, id string) int {
	e.t.Helper()
	return agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", id, namedLike)
}

func unnamedNotices(e *testEnv, id string) int {
	e.t.Helper()
	return agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", id, unnamedLike)
}

func roleForm(handle, role string) url.Values {
	return form("action", "role", "password", testPassword, "role", role, "handle", handle)
}

func TestStaffContactPromotionIntoSavedHandleTells(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("p113_admin", "admin")
	laterID, laterSess := e.user("p113_later", "buyer")
	e.staffFixtureFactor(laterSess)
	otherID, otherSess := e.user("p113_other", "buyer")
	e.staffFixtureFactor(otherSess)
	_, readerSess := e.user("p113_reader", "buyer")
	const keyBlock = "-----BEGIN PGP PUBLIC KEY BLOCK-----"

	// Saving the handle of a buyer tells no one.
	e.check(e.do("POST", "/admin/staff-contact", admin, staffContactForm("p113_later")), 303)
	if n := namedNotices(e, laterID); n != 0 {
		t.Fatalf("%d notices from saving a buyer's handle", n)
	}

	// Promoting another account tells no one, and its audit claims nothing.
	e.check(e.do("POST", "/admin", admin, roleForm("p113_other", "moderator")), 303)
	if namedNotices(e, otherID) != 0 || namedNotices(e, laterID) != 0 ||
		!e.auditExact(adminID, "Changed role of p113_other from buyer to moderator (confirmed with password)") {
		t.Fatal("another account's promotion told someone, or its audit claims so")
	}

	// A refused promotion tells no one.
	e.check(e.do("POST", "/admin", admin, form("action", "role", "password", "wrong-password", "role", "moderator", "handle", "p113_later")), 401)
	if n := namedNotices(e, laterID); n != 0 {
		t.Fatalf("%d notices from a refused promotion", n)
	}

	// The promotion that brings the saved contact into force tells it once and audits both accounts. A replayed form is
	// the one change, and posting the same role again is no change.
	replay := roleForm("p113_later", "moderator")
	replay.Set(formNonceField, formNonceFor("/admin"))
	e.check(e.do("POST", "/admin", admin, replay), 303)
	e.do("POST", "/admin", admin, replay)
	e.check(e.do("POST", "/admin", admin, roleForm("p113_later", "moderator")), 303)
	if n := namedNotices(e, laterID); n != 1 {
		t.Fatalf("%d named notices after a promotion, its replay and a repeat", n)
	}
	if !e.auditExact(adminID, "Changed role of p113_later from buyer to moderator; told the staff member named (confirmed with password)") ||
		!e.auditExact(laterID, "Told the staff member named: role changed to moderator by administrator p113_admin") {
		t.Fatal("the promotion's audit rows do not say the account was told")
	}
	body := agStr(e, "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE $2", laterID, namedLike)
	mustContain(t, body, "Your PGP public key, with its user IDs, is now shown to every signed-in account.")
	mustNotContain(t, body, "p113_", adminID, "cannot receive encrypted messages")
	// What the sentence says is what another account now reads.
	mustContain(t, e.body("GET", "/messages?to=p113_later", readerSess, nil, 200), keyBlock)

	// A move between staff roles changes nothing about being named, in either direction.
	for _, role := range []string{"admin", "moderator"} {
		if _, err := SetRole(context.Background(), e.DB, "p113_later", role); err != nil {
			t.Fatal(err)
		}
	}
	if namedNotices(e, laterID) != 1 || unnamedNotices(e, laterID) != 0 {
		t.Fatal("a move between staff roles told the contact")
	}

	// Demotion ends it: told once, audited, and a repeated post adds none.
	e.check(e.do("POST", "/admin", admin, roleForm("p113_later", "buyer")), 303)
	e.check(e.do("POST", "/admin", admin, roleForm("p113_later", "buyer")), 303)
	if n := unnamedNotices(e, laterID); n != 1 {
		t.Fatalf("%d un-named notices after a demotion and a repeat", n)
	}
	if !e.auditExact(adminID, "Changed role of p113_later from moderator to buyer; told the staff member no longer named (confirmed with password)") {
		t.Fatal("the demotion's audit does not say the account was told")
	}
	// Demoting another staff member tells no one.
	e.check(e.do("POST", "/admin", admin, roleForm("p113_other", "buyer")), 303)
	if namedNotices(e, otherID)+unnamedNotices(e, otherID) != 0 {
		t.Fatal("another account's demotion told it")
	}

	// A key that cannot receive: told that, with no claim about a key, and readers see none.
	keylessID, _ := e.keylessUser("p113_keyless", "buyer")
	agExec(e, "UPDATE users SET totp_enabled=true WHERE id=$1", keylessID)
	e.check(e.do("POST", "/admin/staff-contact", admin, staffContactForm("p113_keyless")), 303)
	e.check(e.do("POST", "/admin", admin, roleForm("p113_keyless", "moderator")), 303)
	body = agStr(e, "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE $2", keylessID, namedLike)
	mustContain(t, body, "cannot receive encrypted messages")
	mustNotContain(t, body, "PGP public key")
	mustNotContain(t, e.body("GET", "/messages?to=p113_keyless", readerSess, nil, 200), keyBlock)

	// Free text that merely mentions a handle names no account.
	freeID, freeSess := e.user("p113_free", "buyer")
	e.staffFixtureFactor(freeSess)
	e.check(e.do("POST", "/admin/staff-contact", admin, staffContactForm("p113_free at the desk")), 303)
	e.check(e.do("POST", "/admin", admin, roleForm("p113_free", "moderator")), 303)
	if n := namedNotices(e, freeID); n != 0 {
		t.Fatalf("%d notices from free text", n)
	}
}

func TestStaffContactRestoreFromSuspensionTells(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("p113r_admin", "admin")
	modID, _ := e.user("p113r_mod", "moderator")
	otherID, _ := e.user("p113r_other", "moderator")
	buyerID, buyerSess := e.user("p113r_buyer", "buyer")
	e.staffFixtureFactor(buyerSess)

	e.check(e.do("POST", "/admin/staff-contact", admin, staffContactForm("p113r_mod")), 303)
	if n := namedNotices(e, modID); n != 1 {
		t.Fatalf("%d notices from naming", n)
	}

	// Suspending the named staff member ends it: told once, audited; a repeat is refused and adds none.
	e.check(e.do("POST", "/admin/suspend", admin, suspendForm("suspend", "p113r_mod")), 303)
	e.check(e.do("POST", "/admin/suspend", admin, suspendForm("suspend", "p113r_mod")), 409)
	if n := unnamedNotices(e, modID); n != 1 {
		t.Fatalf("%d un-named notices after a suspension and a repeat", n)
	}
	if !e.auditExact(adminID, "Suspended account p113r_mod; ended 1 session(s) and any pending sign-ins; told the staff member no longer named (confirmed with password)") {
		t.Fatal("the suspension's audit does not say the account was told")
	}

	// Restoring brings it back into force: told once, audited; a repeat is refused and adds none.
	e.check(e.do("POST", "/admin/suspend", admin, suspendForm("restore", "p113r_mod")), 303)
	e.check(e.do("POST", "/admin/suspend", admin, suspendForm("restore", "p113r_mod")), 409)
	if n := namedNotices(e, modID); n != 2 {
		t.Fatalf("%d named notices after a restore and a repeat (one from naming)", n)
	}
	if !e.auditExact(adminID, "Restored account p113r_mod; told the staff member named (confirmed with password)") ||
		!e.auditExact(modID, "Account restored by administrator p113r_admin; told the staff member named") {
		t.Fatal("the restore's audit rows do not say the account was told")
	}

	// Another staff member's suspension and restore tell no one.
	for _, choice := range []string{"suspend", "restore"} {
		e.check(e.do("POST", "/admin/suspend", admin, suspendForm(choice, "p113r_other")), 303)
	}
	if namedNotices(e, otherID)+unnamedNotices(e, otherID) != 0 || !e.auditExact(adminID, "Restored account p113r_other (confirmed with password)") {
		t.Fatal("another staff member's restore told them, or its audit claims so")
	}

	// A named account that is not staff is no one's contact: its suspension and restore tell no one.
	e.check(e.do("POST", "/admin/staff-contact", admin, staffContactForm("p113r_buyer")), 303)
	for _, choice := range []string{"suspend", "restore"} {
		e.check(e.do("POST", "/admin/suspend", admin, suspendForm(choice, "p113r_buyer")), 303)
	}
	if namedNotices(e, buyerID)+unnamedNotices(e, buyerID) != 0 {
		t.Fatal("a named buyer's suspension or restore told it")
	}

	// Promoted while suspended, nothing comes into force until the restore, which tells once.
	e.check(e.do("POST", "/admin/suspend", admin, suspendForm("suspend", "p113r_buyer")), 303)
	e.check(e.do("POST", "/admin", admin, roleForm("p113r_buyer", "moderator")), 303)
	if n := namedNotices(e, buyerID); n != 0 {
		t.Fatalf("%d notices from promoting a suspended account", n)
	}
	e.check(e.do("POST", "/admin/suspend", admin, suspendForm("restore", "p113r_buyer")), 303)
	if n := namedNotices(e, buyerID); n != 1 {
		t.Fatalf("%d notices after restoring the promoted account", n)
	}
	// Demoted while suspended, it was not in force, so nothing ends a second time.
	e.check(e.do("POST", "/admin/suspend", admin, suspendForm("suspend", "p113r_buyer")), 303)
	before := unnamedNotices(e, buyerID)
	e.check(e.do("POST", "/admin", admin, roleForm("p113r_buyer", "buyer")), 303)
	if n := unnamedNotices(e, buyerID); n != before {
		t.Fatal("demoting a suspended account told it")
	}

	// A ban takes a named moderator out of force too, once; lifting it leaves a buyer, who is no one's contact.
	bannedID, _ := e.user("p113r_banned", "moderator")
	e.check(e.do("POST", "/admin/staff-contact", admin, staffContactForm("p113r_banned")), 303)
	e.check(e.do("POST", "/admin/ban", admin, banForm("ban", "p113r_banned", testBanReason)), 303)
	if n := unnamedNotices(e, bannedID); n != 1 {
		t.Fatalf("%d un-named notices after a ban", n)
	}
	if n := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE '%p113r\\_banned%; told the staff member no longer named%'", adminID); n != 1 {
		t.Fatalf("%d administrator audit rows say the banned account was told", n)
	}
	e.check(e.do("POST", "/admin/ban", admin, banForm("unban", "p113r_banned", testBanReason)), 303)
	if n := namedNotices(e, bannedID); n != 1 {
		t.Fatalf("lifting a ban told the account: %d named notices (one from naming)", n)
	}
}

// A role change or restore racing a save of the same handle makes one notice in either order: never none (each read the
// other's old state), never two, and neither waits on the other into a deadlock.
func TestStaffContactChangeRacingASaveTellsOnce(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("race_admin", "admin")
	_, second := e.user("race_second", "admin")
	race := func(t *testing.T, round int, id, handle string, change func() int) {
		t.Helper()
		var changed, saved int
		var wg sync.WaitGroup
		wg.Go(func() { changed = change() })
		wg.Go(func() { saved = e.do("POST", "/admin/staff-contact", second, staffContactForm(handle)).Code })
		wg.Wait()
		if changed != 303 || saved != 303 {
			t.Fatalf("round %d: change %d, save %d", round, changed, saved)
		}
		if n := namedNotices(e, id); n != 1 {
			t.Fatalf("round %d: %d named notices", round, n)
		}
	}
	buyer := func(name string, round int) (id, handle string) {
		t.Helper()
		handle = name + "_" + strconv.Itoa(round)
		id, sess := e.user(handle, "buyer")
		e.staffFixtureFactor(sess)
		return id, handle
	}
	const rounds = 8
	t.Run("promotion", func(t *testing.T) {
		for i := range rounds {
			id, handle := buyer("race_p", i)
			race(t, i, id, handle, func() int { return e.do("POST", "/admin", admin, roleForm(handle, "moderator")).Code })
		}
	})
	t.Run("restore", func(t *testing.T) {
		for i := range rounds {
			id, handle := buyer("race_r", i)
			e.check(e.do("POST", "/admin", admin, roleForm(handle, "moderator")), 303)
			e.check(e.do("POST", "/admin/suspend", admin, suspendForm("suspend", handle)), 303)
			race(t, i, id, handle, func() int { return e.do("POST", "/admin/suspend", admin, suspendForm("restore", handle)).Code })
		}
	})
	t.Run("host command", func(t *testing.T) {
		for i := range rounds {
			id, handle := buyer("race_h", i)
			race(t, i, id, handle, func() int {
				if _, err := SetRole(context.Background(), e.DB, handle, "moderator"); err != nil {
					t.Errorf("SetRole: %v", err)
					return 500
				}
				return 303
			})
		}
	})
	// Before any save the setting has no row, and FOR SHARE locks nothing that is absent.
	t.Run("first save", func(t *testing.T) {
		for i := range rounds {
			id, handle := buyer("race_f", i)
			agExec(e, "DELETE FROM settings WHERE key=$1", settingStaffContact)
			race(t, i, id, handle, func() int { return e.do("POST", "/admin", admin, roleForm(handle, "moderator")).Code })
		}
	})
	// The host command changing the administrator who is saving the contact at that moment: the save holds that account's
	// row and waits for the setting's, so the host command must not hold the setting's while it waits for the account's.
	t.Run("host command on the saver", func(t *testing.T) {
		for i := range rounds {
			handle := "race_s_" + strconv.Itoa(i)
			_, saver := e.user(handle, "admin")
			var saved int
			var hostErr error
			var wg sync.WaitGroup
			wg.Go(func() {
				saved = e.do("POST", "/admin/staff-contact", saver, staffContactForm("xmpp:round-"+strconv.Itoa(i)+"@staff-example.onion")).Code
			})
			wg.Go(func() { _, hostErr = SetRole(context.Background(), e.DB, handle, "moderator") })
			wg.Wait()
			// The save may lose to the demotion (errAccountChanged); neither may fail on a lock.
			if hostErr != nil || saved >= 500 {
				t.Fatalf("round %d: save %d, host command %v", i, saved, hostErr)
			}
		}
	})
}
