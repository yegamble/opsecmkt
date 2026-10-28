package market

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// staffFactorBody reports whether w is the A-706 refusal (403 and its copy).
func staffFactorBody(w interface{ Result() *http.Response }, body string) bool {
	return w.Result().StatusCode == http.StatusForbidden && strings.Contains(body, staffFactorRequiredMessage)
}

// SEC706-1: PGP sign-in turns on only from a browser that decrypted a challenge to the saved key. An account-wide
// ownership proof from an earlier browser plus the password (all a phisher has) is not enough.
func TestPGPSignInEnableNeedsThisBrowsersProof(t *testing.T) {
	e := newTestApp(t)
	id, owner := e.factorlessStaff("pgp_enable_mod", "moderator") // its fixture session decrypted a challenge
	agExec(e, "UPDATE users SET pgp_verified_at=now() WHERE id=$1", id)
	w := e.do("POST", "/login", randomToken(), url.Values{"handle": {"pgp_enable_mod"}, "password": {testPassword}})
	e.check(w, 303)
	phisher := e.session(w)
	w = e.do("POST", "/pgp/2fa", phisher, url.Values{"enable": {"1"}, "password": {testPassword}})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Decrypt a challenge with your PGP key in this browser") {
		t.Fatalf("password-only browser turned PGP sign-in on: %d %s", w.Code, w.Body.String())
	}
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND pgp_2fa", id) != 0 {
		t.Fatal("pgp_2fa changed")
	}
	if w = e.do("POST", "/account/staff-list", phisher, url.Values{"hide": {"yes"}}); !staffFactorBody(w, w.Body.String()) {
		t.Fatalf("staff action after the refused enablement: %d", w.Code)
	}
	e.check(e.do("POST", "/pgp/2fa", owner, url.Values{"enable": {"1"}, "password": {testPassword}}), 303)
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND pgp_2fa", id) != 1 {
		t.Fatal("the proven browser could not turn PGP sign-in on")
	}
}

// SEC706-3: turning on an account's first second factor ends its other sessions and pending sign-ins, so a session
// started with the password alone gains nothing from the enrolment. This browser stays signed in. Adding a second
// factor to an account that already has one leaves other sessions alone.
func TestFirstFactorEndsOtherSessions(t *testing.T) {
	e := newTestApp(t)
	sessions := func(id string) int { return agInt(e, "SELECT count(*) FROM sessions WHERE user_id=$1", id) }

	// TOTP as the first factor (a staff account: the earlier session must not become a staff session).
	id, sess := e.factorlessStaff("first_totp", "moderator")
	other := agSession(e, id)
	agPending(e, id)
	e.check(e.do("POST", "/totp/enroll", sess, nil), 303)
	code, _ := e.totpCodeFor(id, 0)
	e.check(e.do("POST", "/totp/activate", sess, url.Values{"code": {code}, "password": {testPassword}}), 303)
	if agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1", digest(other)) != 0 || sessions(id) != 1 ||
		agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", id) != 0 {
		t.Fatalf("first TOTP left %d session(s) and %d pending sign-in(s)", sessions(id), agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", id))
	}
	e.check(e.do("GET", "/account", sess, nil), 200)
	if w := e.do("POST", "/account/staff-list", other, url.Values{"hide": {"yes"}}); w.Code == http.StatusSeeOther {
		t.Fatal("the pre-enrolment session took a staff action")
	}
	if !e.auditHas(id, "Ended 1 other session(s) and any pending sign-ins: first second factor turned on") {
		t.Fatalf("not audited: %q", e.audits(id))
	}

	// PGP sign-in as the first factor.
	id, sess = e.factorlessStaff("first_pgp", "admin")
	agExec(e, "UPDATE users SET pgp_verified_at=now() WHERE id=$1", id)
	other = agSession(e, id)
	agPending(e, id)
	e.check(e.do("POST", "/pgp/2fa", sess, url.Values{"enable": {"1"}, "password": {testPassword}}), 303)
	if agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1", digest(other)) != 0 || sessions(id) != 1 ||
		agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", id) != 0 {
		t.Fatal("first PGP sign-in left another session or pending sign-in")
	}
	e.check(e.do("GET", "/account", sess, nil), 200)

	// Not the first factor: TOTP is already on, so PGP sign-in leaves the other session.
	id, sess = e.factorlessStaff("second_factor", "moderator")
	key, _ := agEnableTOTP(e, id)
	agExec(e, "UPDATE users SET pgp_verified_at=now() WHERE id=$1", id)
	other = agSession(e, id)
	e.check(e.do("POST", "/pgp/2fa", sess, url.Values{"enable": {"1"}, "password": {testPassword}, "code": {agTOTPNow(key)}}), 303)
	if agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1", digest(other)) != 1 {
		t.Fatal("a second factor ended another session")
	}
}

// SEC706-2: the role form does not make an account without a second factor a moderator; demotions stay allowed.
func TestRoleFormRefusesFactorlessStaffAppointment(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("appoint_admin", "admin")
	candidate, _ := e.user("appoint_candidate", "buyer")
	form := url.Values{"action": {"role"}, "handle": {"appoint_candidate"}, "role": {"moderator"}, "password": {testPassword}}
	before := agInt(e, "SELECT count(*) FROM audit_events")
	w := e.do("POST", "/admin", admin, form)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "appoint_candidate has no second factor") {
		t.Fatalf("factor-less appointment: %d %s", w.Code, w.Body.String())
	}
	if agStr(e, "SELECT role FROM users WHERE id=$1", candidate) != "buyer" || agInt(e, "SELECT count(*) FROM audit_events") != before {
		t.Fatal("a refused appointment changed or recorded something")
	}
	agEnableTOTP(e, candidate)
	e.check(e.do("POST", "/admin", admin, form), 303)
	if agStr(e, "SELECT role FROM users WHERE id=$1", candidate) != "moderator" {
		t.Fatal("appointment with a factor failed")
	}
	// A factor-less moderator can still be demoted.
	mod, _ := e.factorlessStaff("appoint_demoted", "moderator")
	e.check(e.do("POST", "/admin", admin, url.Values{"action": {"role"}, "handle": {"appoint_demoted"}, "role": {"buyer"}, "password": {testPassword}}), 303)
	if agStr(e, "SELECT role FROM users WHERE id=$1", mod) != "buyer" {
		t.Fatal("demotion of a factor-less moderator failed")
	}
}

// SEC706-2: -set-role does not make an account without a second factor a moderator or administrator. Leaving staff,
// and an administrator becoming a moderator, stay allowed.
func TestSetRoleRefusesFactorlessStaffAppointment(t *testing.T) {
	e := newTestApp(t)
	e.user("setrole_keeper", "admin")
	id, _ := e.user("setrole_candidate", "buyer")
	for _, role := range []string{"moderator", "admin"} {
		if _, err := SetRole(context.Background(), e.DB, "setrole_candidate", role); err == nil || !strings.Contains(err.Error(), "has no second factor") {
			t.Fatalf("factor-less %s appointment: %v", role, err)
		}
	}
	if agStr(e, "SELECT role FROM users WHERE id=$1", id) != "buyer" {
		t.Fatal("a refused -set-role changed the role")
	}
	mod, _ := e.factorlessStaff("setrole_mod", "moderator")
	if _, err := SetRole(context.Background(), e.DB, "setrole_mod", "admin"); err == nil {
		t.Fatal("a factor-less moderator was made an administrator")
	}
	if _, err := SetRole(context.Background(), e.DB, "setrole_mod", "buyer"); err != nil || agStr(e, "SELECT role FROM users WHERE id=$1", mod) != "buyer" {
		t.Fatalf("demotion: %v", err)
	}
	e.factorlessStaff("setrole_admin", "admin")
	if _, err := SetRole(context.Background(), e.DB, "setrole_admin", "moderator"); err != nil {
		t.Fatalf("administrator to moderator: %v", err)
	}
	agEnableTOTP(e, id)
	if _, err := SetRole(context.Background(), e.DB, "setrole_candidate", "admin"); err != nil {
		t.Fatalf("appointment with a factor: %v", err)
	}
}

// SEC706-4: a shared route's staff branch (access by role, not ownership or party) needs the factor too, on the
// locked actor row; owners and ticket openers are unaffected.
func TestFactorlessStaffCannotUseStaffBranchesOfSharedRoutes(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, vendor := e.user("shared_vendor", "vendor")
	_, admin := e.factorlessStaff("shared_admin", "admin")
	adminID := agStr(e, "SELECT id FROM users WHERE handle='shared_admin'")
	listing := e.product(vendorID, "physical")
	own := e.product(adminID, "physical")
	if w := e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, smallJPEG(t))...); w.Code != 303 {
		t.Fatalf("vendor's photo: %d %s", w.Code, w.Body.String())
	}
	photo := agStr(e, "SELECT id FROM images WHERE product_id=$1", listing)
	for _, tc := range []struct {
		path string
		form url.Values
	}{
		{"/listings/update", inventoryForm(listing, nil)},
		{"/listings/archive", url.Values{"id": {listing}}},
		{"/listings/restore", url.Values{"id": {listing}}},
		{"/listings/photo/remove", url.Values{"id": {photo}}},
	} {
		before := e.dbFingerprint()
		w := e.do("POST", tc.path, admin, tc.form)
		mustBeStaffFactorRefusal(t, "admin "+tc.path, w, w.Body.String())
		if e.dbFingerprint() != before {
			t.Fatalf("%s: the refused post changed the database", tc.path)
		}
	}
	if w := e.upload("/listings/photo", admin, e.photoParts(admin, listing, smallJPEG(t))...); !staffFactorBody(w, w.Body.String()) {
		t.Fatalf("administrator's photo upload to another vendor's listing: %d %s", w.Code, w.Body.String())
	}
	if agInt(e, "SELECT count(*) FROM images WHERE product_id=$1", listing) != 1 {
		t.Fatal("photo stored or removed")
	}
	// Owners are unaffected: the vendor and the administrator's own listing.
	e.check(e.do("POST", "/listings/update", vendor, inventoryForm(listing, nil)), 303)
	e.check(e.do("POST", "/listings/update", admin, inventoryForm(own, nil)), 303)

	// Tickets: a factor-less moderator assigned the ticket cannot reply or change its status; the opener can close it.
	tw := newTicketTestWorld(t)
	ticket, _ := tw.open("mod")
	agExec(tw.e, "UPDATE users SET pgp_2fa=false,totp_enabled=false WHERE id=$1", tw.ids["mod"])
	w := tw.e.do("POST", "/tickets/reply", tw.sessions["mod"], url.Values{"id": {ticket}, "body": {tw.ciphertext("mod", "A staff reply that must not be sent.")}})
	mustBeStaffFactorRefusal(t, "/tickets/reply", w, w.Body.String())
	w = tw.e.do("POST", "/tickets/status", tw.sessions["mod"], url.Values{"id": {ticket}, "status": {"ignored"}})
	mustBeStaffFactorRefusal(t, "/tickets/status", w, w.Body.String())
	if agStr(tw.e, "SELECT status FROM tickets WHERE id=$1", ticket) != "open" {
		t.Fatal("ticket status changed")
	}
	tw.e.check(tw.e.do("POST", "/tickets/status", tw.sessions["opener"], url.Values{"id": {ticket}, "status": {"closed"}}), 303)

	// Messages: staff may write to any account; without a factor only the ordinary contact rules apply.
	e.user("shared_private_buyer", "buyer")
	armored := "-----BEGIN PGP MESSAGE-----\n" + strings.Repeat("A", 80) + "\n-----END PGP MESSAGE-----"
	w = e.do("POST", "/messages", admin, url.Values{"recipient": {"shared_private_buyer"}, "body": {armored}})
	mustBeStaffFactorRefusal(t, "/messages", w, w.Body.String())
	agExec(e, "UPDATE users SET pgp_2fa=true WHERE id=$1", adminID)
	if w = e.do("POST", "/messages", admin, url.Values{"recipient": {"shared_private_buyer"}, "body": {armored}}); staffFactorBody(w, w.Body.String()) || strings.Contains(w.Body.String(), messageNotPermitted) {
		t.Fatalf("enrolled staff message refused for the recipient: %d %s", w.Code, w.Body.String())
	}
}
