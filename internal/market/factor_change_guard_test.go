package market

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"
)

// A-152: every sign-in factor, key and recovery-code change needs the current password (plus the enrolled TOTP
// code), even on an account with no factor yet; each change, a recovery-code sign-in and the first failed second
// factor per pending sign-in notify the owner and write one audit row; new "Signed in" rows name the method.

// a152Notes counts the user's notifications containing fragment ("" counts all).
func a152Notes(e *testEnv, userID, fragment string) int {
	e.t.Helper()
	return agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%'||$2||'%'", userID, fragment)
}

// a152Audits counts the user's audit rows starting with prefix.
func a152Audits(e *testEnv, userID, prefix string) int {
	e.t.Helper()
	return agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND starts_with(action,$2)", userID, prefix)
}

// a152Once fails unless exactly one more notification and one more audit row starting with auditPrefix exist
// than before, and the new notification carries the "not you" advice without the given secrets.
func a152Once(e *testEnv, userID string, notesBefore int, auditPrefix, noteFragment string, secrets ...string) {
	e.t.Helper()
	if n := a152Notes(e, userID, ""); n != notesBefore+1 {
		e.t.Fatalf("%q: %d new notifications, want 1", auditPrefix, n-notesBefore)
	}
	if n := a152Audits(e, userID, auditPrefix); n != 1 {
		e.t.Fatalf("%d audit rows starting %q, want 1", n, auditPrefix)
	}
	body := agStr(e, "SELECT body FROM notifications WHERE user_id=$1 ORDER BY created DESC, id LIMIT 1", userID)
	if !strings.Contains(body, noteFragment) || !strings.Contains(body, "If this was not you") {
		e.t.Fatalf("notification %q lacks %q or the advice", body, noteFragment)
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(body, s) {
			e.t.Fatalf("notification %q repeats a secret", body)
		}
	}
}

func TestFactorlessAccountNeedsPasswordToAddTOTP(t *testing.T) {
	e := newTestApp(t)
	uid, stolen := e.user("a152_totp", "buyer")
	e.check(e.do("POST", "/totp/enroll", stolen, nil), 303)
	if !strings.Contains(e.body("GET", "/totp", stolen, nil, 200), `name="password"`) {
		t.Fatal("/totp activation form does not ask for the password on a factor-less account")
	}
	code, _ := e.totpCodeFor(uid, 0)
	agExpect(e, "/totp/activate", stolen, url.Values{"code": {code}}, 400, "Enter your current password")
	agExpect(e, "/totp/activate", stolen, url.Values{"code": {code}, "password": {"wrong password!"}}, 401, "Password incorrect")
	if agStr(e, "SELECT totp_enabled::text FROM users WHERE id=$1", uid) != "false" || agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", uid) != 0 || a152Notes(e, uid, "") != 0 {
		t.Fatal("TOTP activated, codes issued or owner notified without the password")
	}
	// Signing out everywhere leaves the owner in control: password-only sign-in still works.
	e.check(e.do("POST", "/revoke-sessions", stolen, nil), 303)
	if w := e.do("POST", "/login", randomToken(), url.Values{"handle": {"a152_totp"}, "password": {testPassword}}); w.Header().Get("Location") != "/" {
		t.Fatal("owner challenged for a factor they never enrolled")
	}

	owner := agSession(e, uid)
	e.check(e.do("POST", "/totp/activate", owner, url.Values{"code": {code}, "password": {testPassword}}), 303)
	if agStr(e, "SELECT totp_enabled::text FROM users WHERE id=$1", uid) != "true" {
		t.Fatal("confirmed TOTP activation not applied")
	}
	a152Once(e, uid, 0, "Enabled TOTP two-factor authentication; issued 10 recovery codes (confirmed with password)", "Two-step sign-in with an authenticator app (TOTP) was turned on", code)

	// Turning it off (password and code) notifies too.
	next, _ := e.totpCodeFor(uid, 1)
	e.check(e.do("POST", "/totp/disable", owner, url.Values{"code": {next}, "password": {testPassword}}), 303)
	a152Once(e, uid, 1, "Turned off TOTP", "was turned off", next)
}

func TestRecoveryCodeReplacementNeedsPassword(t *testing.T) {
	e := newTestApp(t)
	uid, s := e.user("a152_codes", "buyer")
	key, old := agEnableTOTP(e, uid)
	agExpect(e, "/totp/recovery", s, url.Values{"code": {agTOTPNow(key)}}, 400, "Enter your current password")
	if agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND code_hash=$2", uid, recoveryHash(old)) != 1 || agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", uid) != 1 || a152Notes(e, uid, "") != 0 {
		t.Fatal("recovery codes replaced or owner notified without the password")
	}
	if !strings.Contains(e.body("GET", "/totp", s, nil, 200), `action="/totp/recovery"`) {
		t.Fatal("no replace form")
	}
	body := e.body("GET", "/totp", s, nil, 200)
	form := body[strings.Index(body, `action="/totp/recovery"`):]
	if !strings.Contains(form[:strings.Index(form, "</form>")], `name="password"`) {
		t.Fatal("replace-codes form does not ask for the password")
	}
	e.check(e.do("POST", "/totp/recovery", s, url.Values{"code": {agTOTPNow(key)}, "password": {testPassword}}), 303)
	if agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", uid) != recoveryCodeCount || agInt(e, "SELECT count(*) FROM recovery_codes WHERE code_hash=$1", recoveryHash(old)) != 0 {
		t.Fatal("codes not replaced")
	}
	a152Once(e, uid, 0, "Replaced recovery codes; previous codes invalidated (confirmed with password and authenticator code)", "recovery codes were replaced", old)

	// Without TOTP there is nothing to replace, even with the password.
	id2, s2 := e.user("a152_nocodes", "buyer")
	agExpect(e, "/totp/recovery", s2, url.Values{"password": {testPassword}}, 409, "TOTP is not enabled")
	if agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", id2) != 0 {
		t.Fatal("recovery codes issued to an account without TOTP")
	}
}

func TestFactorlessAccountNeedsPasswordForKeyAndPGPSignIn(t *testing.T) {
	e := newTestApp(t)
	uid, stolen := e.user("a152_vendor", "vendor")
	alice, alicePub := testPGPKey(t, "alice")
	_, malloryPub := testPGPKey(t, "mallory")
	_, aliceFP, _ := parsePublicKey(alicePub)
	if !strings.Contains(e.body("GET", "/account", stolen, nil, 200), `name="password"`) {
		t.Fatal("/account does not offer the password field for key changes on a factor-less account")
	}

	// Adding the first key, then changing or removing it, needs the password.
	agExpect(e, "/account", stolen, url.Values{"pgp": {alicePub}}, 400, "Enter your current password")
	if agStr(e, "SELECT pgp FROM users WHERE id=$1", uid) != "" || a152Notes(e, uid, "") != 0 {
		t.Fatal("key saved by a session alone")
	}
	e.check(e.do("POST", "/account", stolen, url.Values{"pgp": {alicePub}, "password": {testPassword}}), 303)
	a152Once(e, uid, 0, "Updated PGP key", "A PGP public key was added to your account", aliceFP)

	agExpect(e, "/account", stolen, url.Values{"pgp": {malloryPub}}, 400, "Enter your current password")
	agExpect(e, "/account", stolen, url.Values{"pgp": {""}}, 400, "Enter your current password")
	if agStr(e, "SELECT pgp_fingerprint FROM users WHERE id=$1", uid) != aliceFP || a152Notes(e, uid, "") != 1 {
		t.Fatal("key swapped or removed by a session alone")
	}
	// Other fields, and the same key re-saved, need no password and notify nobody.
	e.check(e.do("POST", "/account", stolen, url.Values{"pgp": {alicePub + "\n"}, "xmpp": {"v@example.org"}}), 303)
	if a152Notes(e, uid, "") != 1 || agStr(e, "SELECT xmpp FROM users WHERE id=$1", uid) != "v@example.org" {
		t.Fatal("unchanged key save notified or other fields not saved")
	}

	// Proving ownership notifies once.
	e.check(e.do("POST", "/pgp/challenge", stolen, url.Values{"kind": {"sign"}}), 303)
	challenge := agStr(e, "SELECT challenge FROM pgp_challenges WHERE user_id=$1", uid)
	e.check(e.do("POST", "/pgp/verify", stolen, url.Values{"signature": {testClearsign(t, alice, challenge)}}), 303)
	a152Once(e, uid, 1, "Verified PGP key ownership", "Ownership of the PGP key on your account was proved")

	// Turning PGP sign-in on needs the password; the form asks for it.
	if !strings.Contains(e.body("GET", "/pgp", stolen, nil, 200), `name="password"`) {
		t.Fatal("/pgp does not ask for the password before turning PGP sign-in on")
	}
	agExpect(e, "/pgp/2fa", stolen, url.Values{"enable": {"1"}}, 400, "Enter your current password")
	if agStr(e, "SELECT pgp_2fa::text FROM users WHERE id=$1", uid) != "false" || a152Notes(e, uid, "") != 2 {
		t.Fatal("PGP sign-in turned on by a session alone")
	}
	e.check(e.do("POST", "/pgp/2fa", stolen, url.Values{"enable": {"1"}, "password": {testPassword}}), 303)
	a152Once(e, uid, 2, "Turned on PGP sign-in verification", "PGP sign-in verification was turned on")
	e.check(e.do("POST", "/pgp/2fa", stolen, url.Values{"enable": {"0"}, "password": {testPassword}}), 303)
	a152Once(e, uid, 3, "Turned off PGP sign-in verification", "PGP sign-in verification was turned off")

	// Replacing the key, then removing it, notifies once each.
	_, malloryFP, _ := parsePublicKey(malloryPub)
	e.check(e.do("POST", "/account", stolen, url.Values{"pgp": {malloryPub}, "password": {testPassword}}), 303)
	if a152Notes(e, uid, "") != 5 || a152Notes(e, uid, "public key on your account was changed") != 1 || a152Notes(e, uid, malloryFP) != 0 {
		t.Fatal("key replacement not notified once, or the notification names the fingerprint")
	}
	e.check(e.do("POST", "/account", stolen, url.Values{"pgp": {""}, "password": {testPassword}}), 303)
	a152Once(e, uid, 5, "Removed PGP key", "public key on your account was removed")
}

func TestSignInAuditNamesMethodAndRecoverySignInNotifies(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", testAuditSeed)
	e := newTestApp(t)
	// Existing rows, including a legacy plain "Signed in", export to the same bytes after new sign-ins.
	uid, _ := e.user("a152_signin", "buyer")
	agExec(e, "INSERT INTO audit_events(user_id,action) VALUES($1,'Signed in')", uid)
	upto := int64(agInt(e, "SELECT max(id) FROM audit_events"))
	before, err := auditExport(context.Background(), e.DB, upto)
	if err != nil {
		t.Fatal(err)
	}

	w := e.do("POST", "/login", randomToken(), url.Values{"handle": {"a152_signin"}, "password": {testPassword}})
	e.check(w, 303)
	if a152Audits(e, uid, "Signed in with password") != 1 {
		t.Fatal("password-only sign-in does not name its method")
	}

	key, recovery := agEnableTOTP(e, uid)
	anon, pc := e.passwordLogin("a152_signin")
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, pc), 303)
	if a152Audits(e, uid, "Signed in with password and TOTP") != 1 {
		t.Fatal("TOTP sign-in does not name its method")
	}
	notes := a152Notes(e, uid, "")
	anon, pc = e.passwordLogin("a152_signin")
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}}, pc), 303)
	if a152Audits(e, uid, "Signed in with password and recovery code") != 1 {
		t.Fatal("recovery-code sign-in does not name its method")
	}
	a152Once(e, uid, notes, "Used a recovery code to sign in (0 remaining)", "A recovery code was used to sign in to your account (0 remaining)", recovery)

	alice, alicePub := testPGPKey(t, "alice")
	agExec(e, "UPDATE users SET totp_enabled=false,totp_secret='' WHERE id=$1", uid)
	agEnablePGP(e, uid, alicePub)
	anon, pc = e.passwordLogin("a152_signin")
	e.check(e.do("POST", "/challenge/pgp", anon, nil, pc), 303)
	armored := agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(pc.Value))
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {testDecrypt(t, alice, armored)}}, pc), 303)
	if a152Audits(e, uid, "Signed in with password and PGP code") != 1 {
		t.Fatal("PGP sign-in does not name its method")
	}

	after, err := auditExport(context.Background(), e.DB, upto)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Signed in'", uid) != 1 {
		t.Fatal("existing audit rows changed")
	}
}

func TestFailedSecondFactorRecordedOncePerPendingSignIn(t *testing.T) {
	e := newTestApp(t)
	uid, _ := e.user("a152_fail", "buyer")
	key, recovery := agEnableTOTP(e, uid)
	wrong := e.wrongTOTP(uid)
	const prefix = "Correct password but failed second sign-in step"

	anon, pc := e.passwordLogin("a152_fail")
	agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}, "code": {wrong}}, 401, "Verification code incorrect", pc)
	agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {"aaaa-bbbb-cccc-dddd"}}, 401, "Recovery code incorrect", pc)
	a152Once(e, uid, 0, prefix, "Someone entered your password correctly but failed the second sign-in step", wrong, recovery)
	// The failure did not end the pending sign-in: the right code still completes it.
	w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, pc)
	e.check(w, 303)
	if e.cookie(w, "session") == "" {
		t.Fatal("correct code after a failure did not sign in")
	}

	// A new pending sign-in records its own first failure.
	anon, pc = e.passwordLogin("a152_fail")
	agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}, "code": {wrong}}, 401, "", pc)
	if a152Audits(e, uid, prefix) != 2 || a152Notes(e, uid, "failed the second sign-in step") != 2 {
		t.Fatal("second pending sign-in's failure not recorded once")
	}
	// A request that never reaches a factor check (no method) records nothing.
	agExpect(e, "/challenge", anon, url.Values{"method": {"nope"}}, 400, "", pc)
	if a152Audits(e, uid, prefix) != 2 {
		t.Fatal("a request without a factor check was recorded as a failure")
	}
	if n := agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", uid); n != 1 {
		t.Fatalf("pending sign-ins=%d, want 1", n)
	}
}

// The pending cookie of a real sign-in on a PGP account also records a failed PGP code once.
func TestFailedPGPSecondFactorRecorded(t *testing.T) {
	e := newTestApp(t)
	uid, _ := e.user("a152_pgpfail", "buyer")
	_, pub := testPGPKey(t, "alice")
	agEnablePGP(e, uid, pub)
	anon, pc := e.passwordLogin("a152_pgpfail")
	e.check(e.do("POST", "/challenge/pgp", anon, nil, pc), 303)
	for range 2 {
		agExpect(e, "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {"0123456789abcdef0123456789abcdef"}}, 401, "does not match", pc)
	}
	a152Once(e, uid, 0, "Correct password but failed second sign-in step (PGP code)", "failed the second sign-in step")
}
