package market

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"
)

// ResetAdminFactors is the operator's host-only break-glass for an administrator's lost second factors
// (cmd/server -reset-admin-factors, A-137): after a SETUP_TOKEN/APP_SECRET_KEY rotation the stored TOTP secret
// can no longer be read, and the web reset refuses administrators. In one transaction it turns TOTP, recovery
// codes and PGP sign-in off, ends the administrator's sessions and pending sign-ins, audits and notifies.
func TestResetAdminFactors(t *testing.T) {
	e := newTestApp(t)
	ctx := context.Background()
	id, current := e.user("root_admin", "admin")
	other := agSession(e, id)
	pending := agPending(e, id)
	agEnableTOTP(e, id)
	keyOwner, pub := testPGPKey(t, "root")
	agEnablePGP(e, id, pub)
	// The TOTP secret was sealed under the key in use before a rotation, so it can no longer be opened.
	agExec(e, "UPDATE users SET totp_secret='sealed-under-the-old-key',totp_last_step=99,totp_pending='sealed-pending',recovery_reveal='sealed-codes',recovery_reveal_until=now()+interval '10 minutes' WHERE id=$1", id)
	otherAdminID, otherAdmin := e.user("second_admin", "admin")
	agEnableTOTP(e, otherAdminID)
	e.factorlessStaff("plain_admin", "admin")
	vendorID, vendor := e.user("factor_vendor", "vendor")
	agEnableTOTP(e, vendorID)

	audits := func() int { return agInt(e, "SELECT count(*) FROM audit_events") }
	auditsBefore := audits()
	unchanged := func(when string) {
		t.Helper()
		if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND totp_enabled AND pgp_2fa AND recovery_reveal<>''", id) != 1 ||
			agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", id) != 1 ||
			agUserSessions(e, id) != 2 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", id) != 1 {
			t.Fatalf("%s: a refused reset changed the administrator", when)
		}
		if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND totp_enabled", vendorID) != 1 || agUserSessions(e, vendorID) != 1 {
			t.Fatalf("%s: a refused reset changed the vendor", when)
		}
		if audits() != auditsBefore || agInt(e, "SELECT count(*) FROM notifications") != 0 {
			t.Fatalf("%s: a refused reset was audited or notified", when)
		}
	}
	// Before the reset the administrator is locked out: the password leads to a challenge it cannot answer.
	if code, loc := loginStatus(e, "root_admin", testPassword); code != 303 || loc != "/challenge" {
		t.Fatalf("enrolled administrator skipped the challenge: %d %q", code, loc)
	}
	agExec(e, "DELETE FROM pending_logins WHERE user_id=$1 AND token_hash<>$2", id, digest(pending.Value))

	for _, tc := range []struct{ handle, msg string }{
		{"factor_vendor", "Admin → Reset a user's second factors"},
		{"nobody_here", "no account has the handle"},
		{"ROOT_ADMIN", "no account has the handle"}, // handles are exact and case-sensitive
		{"", "no account has the handle"},
		{"plain_admin", "no second factor enrolled"},
	} {
		if _, err := ResetAdminFactors(ctx, e.DB, tc.handle); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Fatalf("ResetAdminFactors(%q) = %v, want %q", tc.handle, err, tc.msg)
		}
		unchanged(tc.handle)
	}
	// A database upgraded by a newer release is refused, like server startup, before anything changes.
	agExec(e, "INSERT INTO schema_migrations(version,name) VALUES(999,'from_the_future')")
	if _, err := ResetAdminFactors(ctx, e.DB, "root_admin"); err == nil || !strings.Contains(err.Error(), "999_from_the_future") {
		t.Fatalf("unknown migration not refused: %v", err)
	}
	unchanged("unknown migration")
	agExec(e, "DELETE FROM schema_migrations WHERE version=999")

	got, err := ResetAdminFactors(ctx, e.DB, "root_admin")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Second factors of administrator root_admin reset: TOTP and recovery codes and PGP sign-in turned off; ended 2 session(s) and any pending sign-ins" {
		t.Fatalf("summary %q", got)
	}
	var totp, pgp, verified, revealUntilNull bool
	var secret, pend, reveal, key string
	var step int
	if err = e.DB.QueryRow(`SELECT totp_enabled,totp_secret,totp_pending,totp_last_step,recovery_reveal,recovery_reveal_until IS NULL,pgp_2fa,pgp,pgp_verified_at IS NOT NULL FROM users WHERE id=$1`, id).
		Scan(&totp, &secret, &pend, &step, &reveal, &revealUntilNull, &pgp, &key, &verified); err != nil {
		t.Fatal(err)
	}
	if totp || secret != "" || pend != "" || step != 0 || reveal != "" || !revealUntilNull || pgp {
		t.Fatalf("second factors not cleared: totp=%v secret=%q pending=%q step=%d reveal=%q pgp=%v", totp, secret, pend, step, reveal, pgp)
	}
	if key == "" || !verified {
		t.Fatal("the reset removed the PGP key or its verification")
	}
	if agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", id) != 0 || agUserSessions(e, id) != 0 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", id) != 0 {
		t.Fatal("recovery codes, sessions or pending sign-ins survived the reset")
	}
	for _, s := range []string{current, other} {
		if loc := e.do("GET", "/admin", s, nil).Header().Get("Location"); loc != "/login?next=%2Fadmin" {
			t.Fatalf("old session still authenticates (location %q)", loc)
		}
	}
	agExpect(e, "/challenge", randomToken(), url.Values{"method": {"totp"}, "code": {"000000"}}, 401, "expired", pending)
	if !e.auditExact(id, "Second factors reset by host command: TOTP and recovery codes and PGP sign-in turned off; ended 2 session(s) and any pending sign-ins") {
		t.Fatal("reset not audited")
	}
	if audits() != auditsBefore+1 {
		t.Fatal("the reset wrote more than one audit row")
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'The operator reset your two-factor sign-in on the host%' AND NOT is_read", id) != 1 {
		t.Fatal("administrator not notified")
	}
	// Other accounts keep their factors and sessions.
	e.check(e.do("GET", "/admin", otherAdmin, nil), 200)
	e.check(e.do("GET", "/account", vendor, nil), 200)
	if agInt(e, "SELECT count(*) FROM users WHERE id IN ($1,$2) AND totp_enabled", otherAdminID, vendorID) != 2 || agUserSessions(e, otherAdminID) != 1 {
		t.Fatal("another account was changed")
	}

	// Repeating the reset finds nothing to do and records nothing.
	auditsBefore = audits()
	if _, err = ResetAdminFactors(ctx, e.DB, "root_admin"); err == nil || !strings.Contains(err.Error(), "no second factor enrolled") {
		t.Fatalf("repeated reset: %v", err)
	}
	if audits() != auditsBefore {
		t.Fatal("repeated reset wrote an audit row")
	}

	// The administrator signs in with the password alone, sees the notification and enrols TOTP again.
	w := e.do("POST", "/login", randomToken(), url.Values{"handle": {"root_admin"}, "password": {testPassword}})
	e.check(w, 303)
	if w.Header().Get("Location") != "/" || e.cookie(w, "pending") != "" {
		t.Fatalf("password-only sign-in after reset: %v", w.Header())
	}
	s := e.session(w)
	mustContain(t, e.body("GET", "/notifications", s, nil, 200), "The operator reset your two-factor sign-in on the host")
	e.check(e.do("GET", "/admin", s, nil), 200)
	// A reset keeps the public key; its owner must decrypt before enrolling a
	// first authenticator again, so password possession alone cannot replace it.
	proveAccess(e, id, s, keyOwner)
	e.check(e.do("POST", "/totp/enroll", s, nil), 303)
	code, _ := e.totpCodeFor(id, 0)
	e.check(e.do("POST", "/totp/activate", s, url.Values{"code": {code}, "password": {testPassword}}), 303)
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND totp_enabled", id) != 1 || agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", id) != 10 {
		t.Fatal("TOTP re-enrolment after the reset failed")
	}
	e.pendingSignIn("root_admin")
}

// A database the server has never set up is refused with a clear message instead of a relation error.
func TestResetAdminFactorsNeedsInstalledSchema(t *testing.T) {
	db, err := sql.Open("pgx", testSchemaDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = ResetAdminFactors(context.Background(), db, "root_admin"); err == nil || !strings.Contains(err.Error(), "start the server") {
		t.Fatalf("empty database: %v", err)
	}
}

// The host reset takes the sign-in's lock order (pending sign-in, recovery codes, account row; A-154): against a
// recovery-code sign-in that turns TOTP off, both waiting on a held account row, the reset commits and the sign-in
// finds its pending sign-in gone, with no deadlock.
func TestResetAdminFactorsVsSignInNoDeadlock(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("a137_root", "admin")
	_, recovery := agEnableTOTP(e, id)
	anon, pc := e.pendingSignIn("a137_root")
	var cliErr error
	cli, ch := a154Ordered(t, e, id, func() int {
		if _, cliErr = ResetAdminFactors(context.Background(), e.DB, "a137_root"); cliErr != nil {
			return 500
		}
		return 0
	}, func() int {
		return e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}, "turn_off_totp": {"1"}}, pc).Code
	})
	if cli != 0 || ch != 401 {
		t.Fatalf("reset-admin-factors %v, sign-in %d; want success and 401", cliErr, ch)
	}
	if on, _, _, _, _, codes := a154TOTP(e, id); on || codes != 0 || agUserSessions(e, id) != 0 {
		t.Fatal("the reset is incomplete or the sign-in created a session")
	}
}
