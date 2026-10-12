package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

// An administrator can suspend a compromised or abusive account and later restore it (A-27): suspension ends
// the account's sessions and pending sign-ins and blocks every path that creates a session; the counterparty's
// open orders and the account's listings are untouched.

// With no staff contact set (A-119), the refusal points to the operator.
const suspendedMsg = "This account is suspended. Ask the operator of this market."

// signedOut reports whether session no longer authenticates (a protected page redirects to /login).
func signedOut(e *testEnv, session string) bool {
	e.t.Helper()
	w := e.do("GET", "/account", session, nil)
	return w.Code == 303 && w.Header().Get("Location") == "/login?next=%2Faccount"
}

func suspendForm(action, handle string, extra ...string) url.Values {
	return form(append([]string{"action", action, "handle", handle, "password", testPassword}, extra...)...)
}

func TestAdminSuspendsAccount(t *testing.T) {
	x := newEscrowEnv(t)
	e := x.testEnv
	world := &disputeOrderWorld{orderWorld: &orderWorld{e: e}, x: x, keySalt: 2}
	adminID, admin := e.user("susp_admin", "admin")
	vendorID, vendor := e.user("susp_vendor", "vendor")
	other := agSession(e, vendorID)
	agPending(e, vendorID)
	buyerID, buyer := e.user("susp_buyer", "buyer")
	shipped := world.fundedPhysicalFor(buyerID, buyer, vendorID, vendor)
	disputed := world.fundedPhysicalFor(buyerID, buyer, vendorID, vendor)
	for _, order := range []string{shipped, disputed} {
		if code, body := x.shipEscrow(order, vendor, regtestAddr("suspend-claim-"+order[:8]), testPassword); code != 303 {
			t.Fatalf("ship escrow before suspension: %d %s", code, body)
		}
	}

	page := e.body("GET", "/admin", admin, nil, 200)
	f := adminForm(t, page, "/admin/suspend", "")
	mustContain(t, f, `name="handle"`, `pattern="[a-zA-Z0-9_]{3,32}"`, `name="password" required`, `value="suspend"`, `value="restore"`)
	mustContain(t, page, "No account is suspended.")

	agExpect(e, "/admin/suspend", admin, suspendForm("suspend", " susp_vendor "), 303, "")
	w := e.do("POST", "/admin/suspend", admin, suspendForm("suspend", "susp_vendor"))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "already suspended") {
		t.Fatalf("second suspension: %d %q", w.Code, w.Body.String())
	}
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND suspended_at IS NOT NULL", vendorID) != 1 {
		t.Fatal("account not suspended")
	}
	// Every session and pending sign-in ended; the next request with an old cookie is signed out.
	if agUserSessions(e, vendorID) != 0 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", vendorID) != 0 {
		t.Fatal("sessions or pending sign-ins survived suspension")
	}
	for _, s := range []string{vendor, other} {
		if !signedOut(e, s) {
			t.Fatal("old session still authenticates after suspension")
		}
	}
	if !e.auditExact(adminID, "Suspended account susp_vendor; ended 2 session(s) and any pending sign-ins (confirmed with password)") ||
		!e.auditExact(vendorID, "Account suspended by administrator susp_admin; ended 2 session(s) and any pending sign-ins") {
		t.Fatal("suspension not audited on both accounts")
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator suspended your account%' AND body NOT LIKE '%http%'", vendorID) != 1 {
		t.Fatal("suspended account not notified")
	}
	page = e.body("GET", "/admin", admin, nil, 200)
	mustContain(t, page, "susp_vendor · vendor · suspended ", "factors: none (password only)", "Restoring hands the account back to whoever has its password")
	mustNotContain(t, page, "No account is suspended.")

	// Password sign-in: a wrong password answers exactly as before (no handle oracle); the right one is refused
	// with the reason, and no session or pending sign-in is created.
	agExpect(e, "/login", randomToken(), form("handle", "susp_vendor", "password", "not-the-password-at-all"), 401, "Invalid handle or password")
	w = e.do("POST", "/login", randomToken(), form("handle", "susp_vendor", "password", testPassword))
	if w.Code != 403 || refusalText(w.Body.String()) != suspendedMsg || e.cookie(w, "session") != "" || e.cookie(w, "pending") != "" {
		t.Fatalf("suspended password sign-in: %d %q", w.Code, w.Body.String())
	}
	// TOTP challenge: a pending sign-in that survived (created in a race with the suspension) cannot finish.
	key, _ := agEnableTOTP(e, vendorID)
	agExpect(e, "/login", randomToken(), form("handle", "susp_vendor", "password", testPassword), 403, suspendedMsg)
	agExpect(e, "/challenge", randomToken(), form("method", "totp", "code", agTOTPNow(key)), 403, suspendedMsg, agPending(e, vendorID))
	// PGP sign-in challenge, likewise.
	agExec(e, "UPDATE users SET totp_enabled=false WHERE id=$1", vendorID)
	alice, alicePub := testPGPKey(t, "alice")
	agEnablePGP(e, vendorID, alicePub)
	agExec(e, "UPDATE users SET totp_enabled=true WHERE id=$1", vendorID)
	page = e.body("GET", "/admin", admin, nil, 200)
	mustContain(t, page, "susp_vendor · vendor · suspended ", "factors: TOTP and PGP sign-in")
	agExec(e, "UPDATE users SET totp_enabled=false WHERE id=$1", vendorID)
	pc := agPending(e, vendorID)
	anon := randomToken()
	e.check(e.do("POST", "/challenge/pgp", anon, nil, pc), 303)
	code := testDecrypt(t, alice, agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(pc.Value)))
	agExpect(e, "/challenge", anon, form("method", "pgp", "pgp_code", code), 403, suspendedMsg, pc)
	if agUserSessions(e, vendorID) != 0 {
		t.Fatal("a suspended account got a session")
	}
	agExec(e, "UPDATE users SET pgp_2fa=false WHERE id=$1", vendorID)
	agExec(e, "DELETE FROM pending_logins WHERE user_id=$1", vendorID)

	// The buyer's open escrow orders with the suspended vendor remain readable and disputable; listings are unchanged.
	e.check(e.do("GET", "/order?id="+shipped, buyer, nil), 200)
	agExpect(e, "/disputes", buyer, form("order_id", disputed, "reason", "The parcel never arrived at the agreed drop."), 303, "")
	if agStr(e, "SELECT state FROM orders WHERE id=$1", shipped) != stateShipped || agStr(e, "SELECT state FROM orders WHERE id=$1", disputed) != stateDisputed {
		t.Fatal("buyer could not continue orders with a suspended vendor")
	}
	if agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1 AND NOT archived", vendorID) != 2 || agStr(e, "SELECT role FROM users WHERE id=$1", vendorID) != "vendor" {
		t.Fatal("suspension changed the vendor's listings or role")
	}

	// Restore re-enables sign-in and is audited and notified; restoring again changes nothing.
	agExpect(e, "/admin/suspend", admin, suspendForm("restore", "susp_vendor"), 303, "")
	agExpect(e, "/admin/suspend", admin, suspendForm("restore", "susp_vendor"), 409, "is not suspended")
	if !e.auditExact(adminID, "Restored account susp_vendor (confirmed with password)") ||
		!e.auditExact(vendorID, "Account restored by administrator susp_admin") {
		t.Fatal("restore not audited on both accounts")
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator restored your account%'", vendorID) != 1 {
		t.Fatal("restored account not notified")
	}
	w = e.do("POST", "/login", randomToken(), form("handle", "susp_vendor", "password", testPassword))
	e.check(w, 303)
	s := e.session(w)
	e.check(e.do("GET", "/account", s, nil), 200)
	mustContain(t, e.body("GET", "/admin", admin, nil, 200), "No account is suspended.")
}

// Session load refuses a suspended account even if a session row exists (defence in depth).
func TestSuspendedAccountSessionRefused(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("susp_direct", "buyer")
	e.check(e.do("GET", "/account", s, nil), 200)
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", id)
	if !signedOut(e, s) {
		t.Fatal("a suspended account's session still authenticates")
	}
	agExpect(e, "/logout", s, nil, 401, "Sign in required")
}

func TestAdminSuspendRefusals(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("susp_admin", "admin")
	e.user("susp_admin2", "admin")
	_, moderator := e.user("susp_moderator", "moderator")
	targetID, target := e.user("susp_target", "buyer")
	before := agInt(e, "SELECT count(*) FROM audit_events")
	unchanged := func(when string) {
		t.Helper()
		if agInt(e, "SELECT count(*) FROM users WHERE suspended_at IS NOT NULL") != 0 || agUserSessions(e, targetID) != 1 || agInt(e, "SELECT count(*) FROM audit_events") != before {
			t.Fatalf("%s: refused suspension changed state", when)
		}
	}

	// Staff other than administrators cannot use it.
	agExpect(e, "/admin/suspend", moderator, suspendForm("suspend", "susp_target"), 403, "")
	agExpect(e, "/admin/suspend", target, suspendForm("suspend", "susp_admin"), 403, "")
	unchanged("non-admin")

	// An unknown handle answers 404 with the admin page, the form keeping what was typed.
	for _, typed := range []string{"no_such_account", "Susp_Target"} {
		w := e.do("POST", "/admin/suspend", admin, suspendForm("restore", typed))
		if w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("unknown handle %q: status %d", typed, w.Code)
		}
		body := w.Body.String()
		mustContain(t, body, `<div class="alert error" role="alert">No account has the handle &#34;`+typed+`&#34;`, "Control room")
		mustContain(t, adminForm(t, body, "/admin/suspend", ""), `value="`+typed+`"`)
		mustNotContain(t, adminForm(t, body, "/admin/reset-factors", ""), `value="`+typed+`"`)
	}
	agExpect(e, "/admin/suspend", admin, suspendForm("suspend", ""), 400, "Enter an account handle")
	agExpect(e, "/admin/suspend", admin, suspendForm("disable", "susp_target"), 400, "Choose suspend or restore")
	agExpect(e, "/admin/suspend", admin, suspendForm("suspend", "susp_admin"), 400, "You cannot suspend your own account")
	agExpect(e, "/admin/suspend", admin, suspendForm("suspend", "susp_admin2"), 403, "Administrator accounts cannot be suspended")
	unchanged("refused target")

	// The administrator's own password (and authenticator code when enrolled) is required.
	agExpect(e, "/admin/suspend", admin, form("action", "suspend", "handle", "susp_target"), 400, "Enter your current password")
	agExpect(e, "/admin/suspend", admin, form("action", "suspend", "handle", "susp_target", "password", "wrong-password-here"), 401, "Password incorrect")
	key, _ := agEnableTOTP(e, adminID)
	agExpect(e, "/admin/suspend", admin, suspendForm("suspend", "susp_target"), 401, "")
	agExpect(e, "/admin/suspend", admin, suspendForm("suspend", "susp_target", "code", e.wrongTOTP(adminID)), 401, "")
	unchanged("confirmation")
	agExpect(e, "/admin/suspend", admin, suspendForm("suspend", "susp_target", "code", agTOTPNow(key)), 303, "")
	if !e.auditExact(adminID, "Suspended account susp_target; ended 1 session(s) and any pending sign-ins (confirmed with password and authenticator code)") {
		t.Fatal("TOTP-confirmed suspension not audited")
	}
	if !signedOut(e, target) {
		t.Fatal("suspended target still signed in")
	}
}

// A sign-in that passed the password step before the suspension committed cannot turn into a session.
func TestSuspendedAccountCannotCompleteSignIn(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("susp_racer", "buyer")
	key, _ := agEnableTOTP(e, id)
	anon, pc := e.pendingSignIn("susp_racer")
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", id)
	w := e.do("POST", "/challenge", anon, form("method", "totp", "code", agTOTPNow(key)), pc)
	if w.Code != 403 || refusalText(w.Body.String()) != suspendedMsg || e.cookie(w, "session") != "" {
		t.Fatalf("challenge for a suspended account: %d %q", w.Code, w.Body.String())
	}
	if agUserSessions(e, id) != 1 { // only the fixture session e.user created
		t.Fatal("a session was created for a suspended account")
	}
}

// A-108: a sign-in that reaches startSession while suspension is deleting existing sessions must wait on the
// account row, then observe the committed suspension. Without startSession's FOR SHARE, the password login reads
// the old visible row, inserts a new session after the suspension's DELETE, and leaves it valid after commit.
func TestSuspendDuringSignInLeavesNoSession(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("a108_susp_race", "buyer")

	suspension, err := e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer suspension.Rollback()
	var suspensionPID int
	if err = suspension.QueryRow("SELECT pg_backend_pid() FROM users WHERE id=$1 FOR NO KEY UPDATE", id).Scan(&suspensionPID); err != nil {
		t.Fatal(err)
	}
	if _, err = suspension.Exec("UPDATE users SET suspended_at=now() WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if _, err = suspension.Exec("DELETE FROM sessions WHERE user_id=$1", id); err != nil {
		t.Fatal(err)
	}

	type loginResult struct {
		code    int
		body    string
		session string
	}
	finished := make(chan loginResult, 1)
	go func() {
		w := e.do("POST", "/login", randomToken(), form("handle", "a108_susp_race", "password", testPassword))
		finished <- loginResult{code: w.Code, body: refusalText(w.Body.String()), session: e.cookie(w, "session")}
	}()

	// Observe the specific PostgreSQL row-lock wait or the competing login's completion. The transaction is only
	// released after this evidence, so the test does not depend on scheduling sleeps to create the race.
	var early *loginResult
	deadline := time.NewTimer(10 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer tick.Stop()
	blocked := false
	for !blocked && early == nil {
		blocked = agInt(e, `SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			AND $1=ANY(pg_blocking_pids(pid))
			AND query LIKE 'SELECT suspended_at IS NOT NULL,password_hash FROM users WHERE id=$1 FOR SHARE%'`, suspensionPID) > 0
		if blocked {
			break
		}
		select {
		case result := <-finished:
			early = &result
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("sign-in neither waited for suspension's account lock nor finished")
		}
	}

	if err = suspension.Commit(); err != nil {
		t.Fatal(err)
	}
	var result loginResult
	if early != nil {
		result = *early
	} else {
		select {
		case result = <-finished:
		case <-time.After(10 * time.Second):
			t.Fatal("sign-in did not finish after suspension committed")
		}
	}
	if result.code != 403 || result.body != suspendedMsg || result.session != "" {
		t.Fatalf("sign-in during suspension: status=%d body=%q session-cookie=%t blocked-on-row=%t", result.code, result.body, result.session != "", blocked)
	}
	if n := agUserSessions(e, id); n != 0 {
		t.Fatalf("suspension left %d session(s) after sign-in completed", n)
	}
}
