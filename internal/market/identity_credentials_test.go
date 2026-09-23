package market

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Identity audit fixes: traceable role changes, self-service password change and administrator reset of
// another account's second factors. Every test asserts the resulting database state, not only the status.

func TestAdminRoleChangeAuditsBothAccountsAndGuards(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("role_admin", "admin")
	targetID, _ := e.user("role_target", "buyer")
	otherAdminID, _ := e.user("role_admin2", "admin")
	// Existing rows are exported and signed; new behaviour must never rewrite them.
	agExec(e, "INSERT INTO audit_events(user_id,action) VALUES($1,'Changed user role')", adminID)
	legacy := agInt(e, "SELECT id FROM audit_events WHERE action='Changed user role'")

	agExpect(e, "/admin", admin, form("action", "role", "role", "vendor", "user_id", targetID), 303, "")
	if !e.auditExact(adminID, "Changed role of role_target from buyer to vendor") {
		t.Fatal("administrator audit row does not name the account and roles")
	}
	if !e.auditExact(targetID, "Role changed from buyer to vendor by an administrator") {
		t.Fatal("changed account has no audit row of its own")
	}
	if agStr(e, "SELECT role FROM users WHERE id=$1", targetID) != "vendor" {
		t.Fatal("role not changed")
	}
	// Both rows are written in the action's transaction (same timestamp).
	if agInt(e, "SELECT count(DISTINCT created) FROM audit_events WHERE action IN ('Changed role of role_target from buyer to vendor','Role changed from buyer to vendor by an administrator')") != 1 {
		t.Fatal("role audit rows not written in one transaction")
	}
	if agStr(e, "SELECT action FROM audit_events WHERE id=$1", legacy) != "Changed user role" {
		t.Fatal("existing audit row rewritten")
	}

	// The admin audit trail shows the account each row is recorded on.
	body := e.body("GET", "/admin", admin, nil, 200)
	mustContain(t, body, `<th scope="col">Account</th>`,
		"<td>role_admin</td>\n<td>Changed role of role_target from buyer to vendor</td>",
		"<td>role_target</td>\n<td>Role changed from buyer to vendor by an administrator</td>")

	// Re-submitting the same role records nothing on the account.
	agExpect(e, "/admin", admin, form("action", "role", "role", "vendor", "user_id", targetID), 303, "")
	if !e.auditExact(adminID, "Role of role_target unchanged (already vendor)") || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Role changed%'", targetID) != 1 {
		t.Fatal("unchanged role audited incorrectly")
	}

	before := agInt(e, "SELECT count(*) FROM audit_events")
	// Server-side guards (actions_admin.go): administrator is never assignable, and the caller's own role is fixed.
	agExpect(e, "/admin", admin, form("action", "role", "role", "admin", "user_id", targetID), 400, "Choose buyer, vendor, or moderator")
	agExpect(e, "/admin", admin, form("action", "role", "role", "", "user_id", targetID), 400, "Choose buyer, vendor, or moderator")
	agExpect(e, "/admin", admin, form("action", "role", "role", "buyer", "user_id", adminID), 400, "Cannot change your own administrator role")
	// Another administrator is not an eligible target either.
	agExpect(e, "/admin", admin, form("action", "role", "role", "buyer", "user_id", otherAdminID), 404, "Eligible user not found")
	agExpect(e, "/admin", admin, form("action", "role", "role", "buyer", "user_id", "missing"), 404, "Eligible user not found")
	for id, want := range map[string]string{targetID: "vendor", adminID: "admin", otherAdminID: "admin"} {
		if got := agStr(e, "SELECT role FROM users WHERE id=$1", id); got != want {
			t.Fatalf("refused role change altered %s: %s", id, got)
		}
	}
	if agInt(e, "SELECT count(*) FROM audit_events") != before {
		t.Fatal("refused role change wrote audit rows")
	}
}

// pwForm is a password-change form; extra key/value pairs override or add fields.
func pwForm(current, next string, extra ...string) url.Values {
	f := form("password", current, "new_password", next, "new_password_confirm", next)
	for i := 0; i+1 < len(extra); i += 2 {
		f.Set(extra[i], extra[i+1])
	}
	return f
}

func passwordHash(e *testEnv, id string) string {
	return agStr(e, "SELECT password_hash FROM users WHERE id=$1", id)
}

// loginStatus posts /login and returns the status and where it redirects.
func loginStatus(e *testEnv, handle, password string) (int, string) {
	w := e.do("POST", "/login", randomToken(), url.Values{"handle": {handle}, "password": {password}})
	return w.Code, w.Header().Get("Location")
}

func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	e := newTestApp(t)
	id, current := e.user("pw_user", "buyer")
	other := agSession(e, id)
	pending := agPending(e, id)
	bystanderID, bystander := e.user("pw_bystander", "buyer")
	const next = "a-brand-new-password"
	oldHash := passwordHash(e, id)

	// Denied before any change: no session, a forged CSRF token, and a cross-site request.
	agExpect(e, "/account/password", randomToken(), pwForm(testPassword, next), 401, "Sign in required")
	for _, tc := range []struct{ csrf, site string }{
		{e.A.csrf(randomToken()), ""},     // token for another session
		{"", ""},                          // missing
		{e.A.csrf(current), "cross-site"}, // valid token, cross-site request
	} {
		body := pwForm(testPassword, next)
		body.Set("csrf", tc.csrf)
		r := httptest.NewRequest("POST", "/account/password", strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", tc.site)
		r.AddCookie(&http.Cookie{Name: "session", Value: current})
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		e.check(w, 403)
	}

	// Invalid input is refused before the password is checked (no limiter use) and changes nothing.
	for _, tc := range []struct {
		f   url.Values
		msg string
	}{
		{pwForm(testPassword, "short-pass"), "12–72 bytes"},
		{pwForm(testPassword, strings.Repeat("x", 73)), "12–72 bytes"},
		{pwForm(testPassword, next, "new_password_confirm", next+"-typo"), "do not match"},
		{pwForm(testPassword, testPassword), "differs from your current one"},
		{pwForm("", next), "Enter your current password"},
	} {
		agExpect(e, "/account/password", current, tc.f, 400, tc.msg)
	}
	agExpect(e, "/account/password", current, pwForm("wrong-password-123", next), 401, "Password incorrect")
	if passwordHash(e, id) != oldHash || agUserSessions(e, id) != 2 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", id) != 1 {
		t.Fatal("refused password change altered the account")
	}

	w := e.do("POST", "/account/password", current, pwForm(testPassword, next))
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/account?saved=1" {
		t.Fatalf("redirect %q", loc)
	}
	rotated := e.session(w)
	if rotated == current {
		t.Fatal("session not rotated")
	}
	e.check(e.do("GET", "/account", rotated, nil), 200)
	for _, s := range []string{current, other} {
		if loc := e.do("GET", "/account", s, nil).Header().Get("Location"); loc != "/login" {
			t.Fatalf("old session still authenticates (location %q)", loc)
		}
	}
	if agUserSessions(e, id) != 1 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", id) != 0 {
		t.Fatal("other sessions or pending sign-ins survived")
	}
	// The pending sign-in cookie from before the change can no longer complete a challenge.
	agExpect(e, "/challenge", randomToken(), url.Values{"method": {"totp"}, "code": {"000000"}}, 401, "expired", pending)
	if agUserSessions(e, bystanderID) != 1 {
		t.Fatal("another user's session was revoked")
	}
	e.check(e.do("GET", "/account", bystander, nil), 200)
	if !e.auditExact(id, "Changed password; ended 1 other session(s) and any pending sign-ins (confirmed with password)") {
		t.Fatal("password change not audited")
	}
	if code, _ := loginStatus(e, "pw_user", testPassword); code != 401 {
		t.Fatalf("old password still signs in: %d", code)
	}
	if code, loc := loginStatus(e, "pw_user", next); code != 303 || loc != "/" {
		t.Fatalf("new password sign-in: %d %q", code, loc)
	}
	// The account page renders the form after reload.
	mustContain(t, e.body("GET", "/account", rotated, nil, 200), `action="/account/password"`, `name="new_password"`, "Change password")
}

func TestPasswordChangeNeedsSecondFactorWhenEnrolled(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("pw_totp", "buyer")
	key, recovery := agEnableTOTP(e, id)
	oldHash := passwordHash(e, id)
	const next = "second-factor-new-password"

	// The form asks for a code or recovery code while TOTP is on.
	mustContain(t, e.body("GET", "/account", s, nil, 200), "Authenticator code or recovery code")
	agExpect(e, "/account/password", s, pwForm(testPassword, next), 401, "Verification code incorrect")
	agExpect(e, "/account/password", s, pwForm(testPassword, next, "code", e.wrongTOTP(id)), 401, "Verification code incorrect")
	agExpect(e, "/account/password", s, pwForm(testPassword, next, "code", "aaaa-bbbb-cccc-dddd"), 401, "Recovery code incorrect")
	// A correct code with the wrong password is refused before the code is spent.
	agExpect(e, "/account/password", s, pwForm("wrong-password-123", next, "code", agTOTPNow(key)), 401, "Password incorrect")
	if passwordHash(e, id) != oldHash || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
		t.Fatal("refused change altered the password or spent a code")
	}

	// A recovery code works once.
	w := e.do("POST", "/account/password", s, pwForm(testPassword, next, "code", recovery))
	e.check(w, 303)
	s = e.session(w)
	if passwordHash(e, id) == oldHash || agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND used_at IS NOT NULL", id) != 1 {
		t.Fatal("recovery-code change not applied")
	}
	if !e.auditExact(id, "Changed password; ended 0 other session(s) and any pending sign-ins (confirmed with password and recovery code)") {
		t.Fatal("recovery-code change not audited")
	}
	agExpect(e, "/account/password", s, pwForm(next, testPassword, "code", recovery), 401, "Recovery code incorrect or already used")

	// A current authenticator code works; TOTP stays enabled and sign-in still needs the challenge.
	w = e.do("POST", "/account/password", s, pwForm(next, testPassword, "code", agTOTPNow(key)))
	e.check(w, 303)
	if !e.auditExact(id, "Changed password; ended 0 other session(s) and any pending sign-ins (confirmed with password and authenticator code)") {
		t.Fatal("code change not audited")
	}
	if code, loc := loginStatus(e, "pw_totp", testPassword); code != 303 || loc != "/challenge" {
		t.Fatalf("TOTP account signed in without the challenge: %d %q", code, loc)
	}
}

// resetForm is the admin second-factor reset form for target, confirmed with the test password.
func resetForm(target string, extra ...string) url.Values {
	f := form("user_id", target, "password", testPassword)
	for i := 0; i+1 < len(extra); i += 2 {
		f.Set(extra[i], extra[i+1])
	}
	return f
}

func TestAdminResetsSecondFactors(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("reset_admin", "admin")
	targetID, targetSess := e.user("reset_target", "vendor")
	second := agSession(e, targetID)
	agPending(e, targetID)
	_, recovery := agEnableTOTP(e, targetID)
	_, pub := testPGPKey(t, "target")
	agEnablePGP(e, targetID, pub)
	agExec(e, "UPDATE users SET totp_last_step=99,totp_pending='sealed-pending',recovery_reveal='sealed-codes',recovery_reveal_until=now()+interval '10 minutes' WHERE id=$1", targetID)
	plainID, _ := e.user("reset_plain", "buyer")
	otherAdminID, _ := e.user("reset_admin2", "admin")
	agEnableTOTP(e, otherAdminID)
	_, buyer := e.user("reset_buyer", "buyer")
	_, mod := e.user("reset_mod", "moderator")

	// The admin page offers only non-administrator accounts that have a second factor.
	resetOptions := func() string {
		t.Helper()
		_, f, ok := strings.Cut(e.body("GET", "/admin", admin, nil, 200), `action="/admin/reset-factors"`)
		if !ok {
			t.Fatal("reset form missing")
		}
		f, _, _ = strings.Cut(f, "</form>")
		return f
	}
	opts := resetOptions()
	mustContain(t, opts, "reset_target · TOTP and PGP sign-in", `name="password" required`)
	mustNotContain(t, opts, "reset_admin2", "reset_plain", "reset_admin ", `name="code"`)

	untouched := func() {
		t.Helper()
		if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND totp_enabled AND pgp_2fa", targetID) != 1 || agUserSessions(e, targetID) != 2 || agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", targetID) != 1 {
			t.Fatal("refused reset changed the account")
		}
	}
	// Non-administrators and anonymous callers are refused.
	agExpect(e, "/admin/reset-factors", buyer, resetForm(targetID), 403, "Administrator access required")
	agExpect(e, "/admin/reset-factors", mod, resetForm(targetID), 403, "Administrator access required")
	agExpect(e, "/admin/reset-factors", targetSess, resetForm(targetID), 403, "Administrator access required")
	agExpect(e, "/admin/reset-factors", randomToken(), resetForm(targetID), 401, "Sign in required")
	// The administrator must confirm with their own password.
	agExpect(e, "/admin/reset-factors", admin, resetForm(targetID, "password", ""), 400, "Enter your current password")
	agExpect(e, "/admin/reset-factors", admin, resetForm(targetID, "password", "wrong-password-123"), 401, "Password incorrect")
	// Never the administrator's own account, never another administrator.
	agExpect(e, "/admin/reset-factors", admin, resetForm(adminID), 400, "cannot reset your own second factors")
	agExpect(e, "/admin/reset-factors", admin, resetForm(otherAdminID), 403, "Administrator second factors cannot be reset")
	agExpect(e, "/admin/reset-factors", admin, resetForm(""), 400, "Choose an account")
	agExpect(e, "/admin/reset-factors", admin, resetForm("missing"), 404, "Account not found")
	agExpect(e, "/admin/reset-factors", admin, resetForm(plainID), 409, "no second factor enrolled")
	untouched()
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND totp_enabled", otherAdminID) != 1 {
		t.Fatal("other administrator changed")
	}

	// With TOTP on the administrator's account, the reset also needs a current code.
	adminKey, _ := agEnableTOTP(e, adminID)
	agExpect(e, "/admin/reset-factors", admin, resetForm(targetID), 401, "Verification code incorrect")
	agExpect(e, "/admin/reset-factors", admin, resetForm(targetID, "code", e.wrongTOTP(adminID)), 401, "Verification code incorrect")
	// Recovery codes are not accepted for this confirmation.
	agExpect(e, "/admin/reset-factors", admin, resetForm(targetID, "code", recovery), 401, "Verification code incorrect")
	untouched()
	mustContain(t, resetOptions(), `name="code" required`)

	agExpect(e, "/admin/reset-factors", admin, resetForm(targetID, "code", agTOTPNow(adminKey)), 303, "")
	var totp, pgp, verified bool
	var secret, pending, reveal, key string
	var step int
	if err := e.DB.QueryRow(`SELECT totp_enabled,totp_secret,totp_pending,totp_last_step,recovery_reveal,recovery_reveal_until IS NULL,pgp_2fa,pgp,pgp_verified_at IS NOT NULL FROM users WHERE id=$1`, targetID).
		Scan(&totp, &secret, &pending, &step, &reveal, new(bool), &pgp, &key, &verified); err != nil {
		t.Fatal(err)
	}
	if totp || secret != "" || pending != "" || step != 0 || reveal != "" || pgp {
		t.Fatalf("second factors not cleared: totp=%v secret=%q pending=%q step=%d reveal=%q pgp=%v", totp, secret, pending, step, reveal, pgp)
	}
	if key == "" || !verified {
		t.Fatal("reset removed the PGP key or its verification")
	}
	if agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", targetID) != 0 || agUserSessions(e, targetID) != 0 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", targetID) != 0 {
		t.Fatal("recovery codes, sessions or pending sign-ins survived the reset")
	}
	for _, s := range []string{targetSess, second} {
		if loc := e.do("GET", "/account", s, nil).Header().Get("Location"); loc != "/login" {
			t.Fatalf("target session still authenticates (location %q)", loc)
		}
	}
	if !e.auditExact(targetID, "Second factors reset by administrator reset_admin: TOTP and recovery codes and PGP sign-in turned off; ended 2 session(s)") {
		t.Fatal("target audit row missing")
	}
	if !e.auditExact(adminID, "Reset second factors of reset_target: TOTP and recovery codes and PGP sign-in turned off; ended 2 session(s) (confirmed with password and authenticator code)") {
		t.Fatal("administrator audit row missing")
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator reset your two-factor sign-in%' AND NOT is_read", targetID) != 1 {
		t.Fatal("target not notified")
	}
	// The administrator's own factors and sessions are untouched.
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND totp_enabled", adminID) != 1 || agUserSessions(e, adminID) != 1 {
		t.Fatal("reset touched the administrator")
	}

	// The user now signs in with the password only and sees the notification.
	w := e.do("POST", "/login", randomToken(), url.Values{"handle": {"reset_target"}, "password": {testPassword}})
	e.check(w, 303)
	if w.Header().Get("Location") != "/" || e.cookie(w, "pending") != "" {
		t.Fatalf("password-only sign-in after reset: %v", w.Header())
	}
	mustContain(t, e.body("GET", "/notifications", e.session(w), nil, 200), "An administrator reset your two-factor sign-in")

	// Repeating the reset finds nothing to do and records nothing.
	// (Ten password confirmations per ten minutes: this is the administrator's ninth.)
	before := agInt(e, "SELECT count(*) FROM audit_events")
	adminCode, _ := e.totpCodeFor(adminID, 1)
	agExpect(e, "/admin/reset-factors", admin, resetForm(targetID, "code", adminCode), 409, "no second factor enrolled")
	if agInt(e, "SELECT count(*) FROM audit_events") != before {
		t.Fatal("repeated reset wrote audit rows")
	}
	// Nobody is left to reset: the form is replaced by a note.
	mustContain(t, e.body("GET", "/admin", admin, nil, 200), "No other account has a second factor enrolled.")
}
