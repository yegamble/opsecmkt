package market

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// Rate limits, session lifecycle and second-factor error paths (audit findings B5, B8, B9).
// Every test asserts the resulting database state, not only the status code.

func agExec(e *testEnv, q string, args ...any) {
	e.t.Helper()
	if _, err := e.DB.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

func agInt(e *testEnv, q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.DB.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func agStr(e *testEnv, q string, args ...any) string {
	e.t.Helper()
	var s string
	if err := e.DB.QueryRow(q, args...).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// agExpect posts and checks both the status and a fragment of the plain-text reason.
func agExpect(e *testEnv, path, token string, form url.Values, code int, msg string, extra ...*http.Cookie) {
	e.t.Helper()
	w := e.do("POST", path, token, form, extra...)
	if w.Code != code || !strings.Contains(w.Body.String(), msg) {
		e.t.Fatalf("POST %s: status=%d body=%q, want %d containing %q", path, w.Code, w.Body.String(), code, msg)
	}
}

// agLimited reports whether the in-memory limiter holds an entry for key.
func agLimited(e *testEnv, key string) bool {
	e.A.mu.Lock()
	defer e.A.mu.Unlock()
	_, ok := e.A.limits[key]
	return ok
}

// agSession adds another live session for userID and returns its token.
func agSession(e *testEnv, userID string) string {
	e.t.Helper()
	s := randomToken()
	agExec(e, "INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '1 hour')", digest(s), userID)
	return s
}

// agPending inserts a 10-minute pending login for userID (as after a correct password) and returns its cookie.
func agPending(e *testEnv, userID string) *http.Cookie {
	e.t.Helper()
	p := randomToken()
	agExec(e, "INSERT INTO pending_logins(token_hash,user_id,expires) VALUES($1,$2,now()+interval '10 minutes')", digest(p), userID)
	return &http.Cookie{Name: "pending", Value: p}
}

// agEnableTOTP turns TOTP on with a fresh sealed secret and one recovery code; it returns the key and the code.
func agEnableTOTP(e *testEnv, userID string) ([]byte, string) {
	e.t.Helper()
	secret := newTOTPSecret()
	sealed, err := e.A.seal("totp", secret)
	if err != nil {
		e.t.Fatal(err)
	}
	agExec(e, "UPDATE users SET totp_secret=$2,totp_enabled=true,totp_last_step=0 WHERE id=$1", userID, sealed)
	code := newRecoveryCode()
	agExec(e, "INSERT INTO recovery_codes(code_hash,user_id) VALUES($1,$2)", recoveryHash(code), userID)
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		e.t.Fatal(err)
	}
	return key, code
}

func agTOTPNow(key []byte) string { return hotp(key, uint64(totpStep(time.Now())), totpDigits) }

// agEnablePGP stores pub as a proven key with PGP sign-in on (the state /pgp/verify and /pgp/2fa leave).
func agEnablePGP(e *testEnv, userID, pub string) {
	e.t.Helper()
	_, fp, err := parsePublicKey(pub)
	if err != nil {
		e.t.Fatal(err)
	}
	agExec(e, "UPDATE users SET pgp=$2,pgp_fingerprint=$3,pgp_verified_at=now(),pgp_2fa=true WHERE id=$1", userID, pub, fp)
}

// agSignOnlyKey is a valid public key without an encryption subkey (nothing can be encrypted to it).
func agSignOnlyKey(t *testing.T) string {
	t.Helper()
	ent, err := openpgp.NewEntity("signer", "", "signer@example.test", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	ent.Subkeys = nil
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err = ent.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, _, err = parsePublicKey(buf.String()); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func agUserSessions(e *testEnv, userID string) int {
	return agInt(e, "SELECT count(*) FROM sessions WHERE user_id=$1", userID)
}

// --- B5: rate limits ---

func TestRateLimitLoginAndRegisterPerHandle(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("rl_login", "buyer")
	e.user("rl_other", "buyer")
	for i := range 10 {
		agExpect(e, "/login", randomToken(), url.Values{"handle": {"rl_login"}, "password": {"wrong-password-" + string(rune('a'+i))}}, 401, "Invalid handle or password")
	}
	// The 11th attempt is refused before bcrypt, even with the right password; the key ignores case.
	w := e.do("POST", "/login", randomToken(), url.Values{"handle": {"RL_LOGIN"}, "password": {testPassword}})
	e.check(w, 429)
	if !strings.Contains(w.Body.String(), "Too many attempts") || e.cookie(w, "session") != "" || e.cookie(w, "pending") != "" {
		t.Fatalf("limited login issued credentials: %v %q", w.Header(), w.Body.String())
	}
	if n := agUserSessions(e, id); n != 1 {
		t.Fatalf("limited login created a session: %d", n)
	}
	// /register shares the per-handle key: the exhausted handle is refused without creating anything.
	agExpect(e, "/register", randomToken(), url.Values{"handle": {"rl_login"}, "password": {"a-new-long-password"}}, 429, "Too many attempts")
	// A different handle is unaffected.
	w = e.do("POST", "/login", randomToken(), url.Values{"handle": {"rl_other"}, "password": {testPassword}})
	e.check(w, 303)
	e.session(w)

	// Register on its own: ten attempts for a taken handle, then 429; no account is ever created.
	e.user("rl_taken", "buyer")
	for range 10 {
		agExpect(e, "/register", randomToken(), url.Values{"handle": {"rl_taken"}, "password": {"a-new-long-password"}}, 400, "Handle unavailable")
	}
	agExpect(e, "/register", randomToken(), url.Values{"handle": {"rl_taken"}, "password": {"a-new-long-password"}}, 429, "Too many attempts")
	if n := agInt(e, "SELECT count(*) FROM users"); n != 3 {
		t.Fatalf("users=%d", n)
	}
	// Malformed handles are rejected before the limiter, so they create no limiter entries.
	agExpect(e, "/login", randomToken(), url.Values{"handle": {"x!"}, "password": {testPassword}}, 400, "handle")
	if agLimited(e, "auth:x!") {
		t.Fatal("malformed handle created a limiter entry")
	}
}

func TestRateLimitConfirmPassword(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("rl_confirm", "buyer")
	otherID, other := e.user("rl_confirm2", "buyer")
	agExec(e, "UPDATE users SET pgp_2fa=true WHERE id IN ($1,$2)", id, otherID)
	off := func(pw string) url.Values { return url.Values{"enable": {"0"}, "password": {pw}} }
	for range 10 {
		agExpect(e, "/pgp/2fa", s, off("wrong-password-123"), 401, "Password incorrect")
	}
	agExpect(e, "/pgp/2fa", s, off(testPassword), 429, "Too many attempts")
	if agStr(e, "SELECT pgp_2fa::text FROM users WHERE id=$1", id) != "true" {
		t.Fatal("limited confirmation changed the setting")
	}
	if e.auditHas(id, "Turned off PGP sign-in") {
		t.Fatal("limited confirmation audited a change")
	}
	// The limit is per user.
	agExpect(e, "/pgp/2fa", other, off(testPassword), 303, "")
	if agStr(e, "SELECT pgp_2fa::text FROM users WHERE id=$1", otherID) != "false" || !e.auditHas(otherID, "Turned off PGP sign-in verification (confirmed with password)") {
		t.Fatal("other user's confirmed change not applied")
	}
}

func TestRateLimitChallengePerPendingLogin(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("rl_chal", "buyer")
	key, _ := agEnableTOTP(e, id)
	p1 := agPending(e, id)
	anon := randomToken()
	// An invalid method still counts against this pending login (the limit sits before method dispatch).
	for range 10 {
		agExpect(e, "/challenge", anon, url.Values{"method": {"bogus"}}, 400, "Choose a verification method", p1)
	}
	agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, 429, "Too many attempts. Try again", p1)
	if agUserSessions(e, id) != 1 || agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(p1.Value)) != 1 {
		t.Fatal("limited challenge signed in or consumed the pending login")
	}
	if agInt(e, "SELECT totp_last_step::int FROM users WHERE id=$1", id) != 0 {
		t.Fatal("limited challenge consumed the TOTP code")
	}
	// Unknown pending cookies create no limiter entries.
	ghost := &http.Cookie{Name: "pending", Value: randomToken()}
	agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}}, 401, "expired", ghost)
	if agLimited(e, "challenge:"+digest(ghost.Value)) {
		t.Fatal("unknown pending login created a limiter entry")
	}
	// A new pending login for the same account has its own budget.
	w := e.do("POST", "/challenge", randomToken(), url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, agPending(e, id))
	e.check(w, 303)
	if agUserSessions(e, id) != 2 || agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1 AND user_id=$2", digest(e.session(w)), id) != 1 {
		t.Fatal("fresh pending login did not sign in")
	}
}

func TestRateLimitTOTPVerifyPerAccount(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("rl_verify", "buyer")
	key, recovery := agEnableTOTP(e, id)
	otherID, _ := e.user("rl_verify2", "buyer")
	otherKey, _ := agEnableTOTP(e, otherID)
	wrong := e.wrongTOTP(id)
	// Ten wrong codes spread over two pending logins (so the per-pending limit is not what trips).
	p1, p2 := agPending(e, id), agPending(e, id)
	for i := range 10 {
		p := p1
		if i >= 6 {
			p = p2
		}
		agExpect(e, "/challenge", randomToken(), url.Values{"method": {"totp"}, "code": {wrong}}, 401, "incorrect", p)
	}
	// The 11th is refused even with a correct code or a valid recovery code, on either pending login.
	agExpect(e, "/challenge", randomToken(), url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, 429, "Too many verification attempts", p2)
	agExpect(e, "/challenge", randomToken(), url.Values{"method": {"totp"}, "recovery": {recovery}}, 429, "Too many verification attempts", agPending(e, id))
	if agUserSessions(e, id) != 1 || agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND used_at IS NULL", id) != 1 {
		t.Fatal("limited verification signed in or consumed the recovery code")
	}
	// Another account is unaffected.
	e.check(e.do("POST", "/challenge", randomToken(), url.Values{"method": {"totp"}, "code": {agTOTPNow(otherKey)}}, agPending(e, otherID)), 303)
	if agUserSessions(e, otherID) != 2 {
		t.Fatal("other account did not sign in")
	}
}

func TestRateLimitTOTPManagementActions(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("rl_totp", "buyer")
	e.check(e.do("POST", "/totp/enroll", s, nil), 303)
	wrong := e.wrongTOTP(id)
	for range 10 {
		agExpect(e, "/totp/activate", s, url.Values{"code": {wrong}}, 400, "Code incorrect")
	}
	code, _ := e.totpCodeFor(id, 0)
	agExpect(e, "/totp/activate", s, url.Values{"code": {code}}, 429, "Too many attempts")
	// The same totp:<user> budget covers recovery regeneration and disabling.
	agExpect(e, "/totp/recovery", s, url.Values{"code": {code}}, 429, "Too many attempts")
	agExpect(e, "/totp/disable", s, url.Values{"password": {testPassword}, "code": {code}}, 429, "Too many attempts")
	if agStr(e, "SELECT totp_enabled::text FROM users WHERE id=$1", id) != "false" || agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", id) != 0 {
		t.Fatal("limited activation enabled TOTP")
	}
	if agLimited(e, "confirm:"+id) {
		t.Fatal("limited disable reached the password check")
	}
	// Another user can still enroll and activate.
	otherID, other := e.user("rl_totp2", "buyer")
	e.check(e.do("POST", "/totp/enroll", other, nil), 303)
	c2, _ := e.totpCodeFor(otherID, 0)
	e.check(e.do("POST", "/totp/activate", other, url.Values{"code": {c2}}), 303)
	if agStr(e, "SELECT totp_enabled::text FROM users WHERE id=$1", otherID) != "true" {
		t.Fatal("other user's activation failed")
	}
}

func TestRateLimitPGPChallengeAndVerify(t *testing.T) {
	e := newTestApp(t)
	alice, alicePub := testPGPKey(t, "alice")
	bob, bobPub := testPGPKey(t, "bob")
	id, s := e.user("rl_pgp", "buyer")
	otherID, other := e.user("rl_pgp2", "buyer")
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {alicePub}}), 303)
	e.check(e.do("POST", "/account", other, url.Values{"pgp": {bobPub}}), 303)

	// An invalid kind is rejected before the limiter; then exactly 20 challenges are issued.
	agExpect(e, "/pgp/challenge", s, url.Values{"kind": {"bogus"}}, 400, "Choose to sign")
	for range 20 {
		e.check(e.do("POST", "/pgp/challenge", s, url.Values{"kind": {"sign"}}), 303)
	}
	challenge := agStr(e, "SELECT challenge FROM pgp_challenges WHERE user_id=$1", id)
	agExpect(e, "/pgp/challenge", s, url.Values{"kind": {"sign"}}, 429, "Too many challenges")
	if agStr(e, "SELECT challenge FROM pgp_challenges WHERE user_id=$1", id) != challenge || agInt(e, "SELECT count(*) FROM pgp_challenges WHERE user_id=$1", id) != 1 {
		t.Fatal("limited request replaced the open challenge")
	}

	// Verification: ten bad proofs, then even a correct proof is refused.
	for range 10 {
		agExpect(e, "/pgp/verify", s, url.Values{"signature": {"not a signature"}}, 400, "Not verified")
	}
	agExpect(e, "/pgp/verify", s, url.Values{"signature": {testClearsign(t, alice, challenge)}}, 429, "Too many attempts")
	if agStr(e, "SELECT (pgp_verified_at IS NULL)::text FROM users WHERE id=$1", id) != "true" || agInt(e, "SELECT count(*) FROM pgp_challenges WHERE user_id=$1", id) != 1 {
		t.Fatal("limited verification recorded proof or consumed the challenge")
	}

	// Another user is unaffected by either limit.
	e.check(e.do("POST", "/pgp/challenge", other, url.Values{"kind": {"sign"}}), 303)
	c2 := agStr(e, "SELECT challenge FROM pgp_challenges WHERE user_id=$1", otherID)
	e.check(e.do("POST", "/pgp/verify", other, url.Values{"signature": {testClearsign(t, bob, c2)}}), 303)
	if agStr(e, "SELECT (pgp_verified_at IS NOT NULL)::text FROM users WHERE id=$1", otherID) != "true" {
		t.Fatal("other user's verification failed")
	}
}

func TestRateLimitPGPLoginCodes(t *testing.T) {
	e := newTestApp(t)
	_, pub := testPGPKey(t, "carol")
	id, _ := e.user("rl_pgplogin", "buyer")
	agEnablePGP(e, id, pub)
	p1 := agPending(e, id)
	anon := randomToken()
	for range 10 {
		e.check(e.do("POST", "/challenge/pgp", anon, nil, p1), 303)
	}
	nonce := agStr(e, "SELECT pgp_nonce FROM pending_logins WHERE token_hash=$1", digest(p1.Value))
	agExpect(e, "/challenge/pgp", anon, nil, 429, "Too many codes requested", p1)
	if agStr(e, "SELECT pgp_nonce FROM pending_logins WHERE token_hash=$1", digest(p1.Value)) != nonce {
		t.Fatal("limited request replaced the sign-in code")
	}
	// Unknown pending cookies create no limiter entries; a new pending login has its own budget.
	ghost := &http.Cookie{Name: "pending", Value: randomToken()}
	agExpect(e, "/challenge/pgp", anon, nil, 401, "expired", ghost)
	if agLimited(e, "pgp-login:"+digest(ghost.Value)) {
		t.Fatal("unknown pending login created a limiter entry")
	}
	p2 := agPending(e, id)
	e.check(e.do("POST", "/challenge/pgp", anon, nil, p2), 303)
	if agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1 AND pgp_nonce IS NOT NULL", digest(p2.Value)) != 1 {
		t.Fatal("fresh pending login got no code")
	}
}

func TestRateLimitWritesPerUser(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("rl_writer", "buyer")
	otherID, other := e.user("rl_writer2", "buyer")
	mine, theirs := randomToken(), randomToken()
	agExec(e, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,'a'),($3,$4,'b')", mine, id, theirs, otherID)
	for range 100 {
		e.check(e.do("POST", "/notifications", s, url.Values{"id": {"missing"}}), 303)
	}
	agExpect(e, "/notifications", s, url.Values{"id": {mine}}, 429, "Too many changes")
	if agStr(e, "SELECT is_read::text FROM notifications WHERE id=$1", mine) != "false" {
		t.Fatal("limited write was applied")
	}
	if n := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Marked notification read'", id); n != 100 {
		t.Fatalf("audited writes=%d", n)
	}
	e.check(e.do("POST", "/notifications", other, url.Values{"id": {theirs}}), 303)
	if agStr(e, "SELECT is_read::text FROM notifications WHERE id=$1", theirs) != "true" {
		t.Fatal("other user's write was refused")
	}
}

func TestPasswordWorkBusyReturns503(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("busy_user", "buyer")
	agExec(e, "UPDATE users SET pgp_2fa=true WHERE id=$1", id)
	if len(passwordWork) != 0 {
		t.Fatal("password work slots already taken")
	}
	for range cap(passwordWork) {
		passwordWork <- struct{}{}
	}
	drained := false
	drain := func() {
		if !drained {
			for range cap(passwordWork) {
				<-passwordWork
			}
			drained = true
		}
	}
	t.Cleanup(drain)
	creds := url.Values{"handle": {"busy_user"}, "password": {testPassword}}
	agExpect(e, "/login", randomToken(), creds, 503, "Authentication is busy")
	agExpect(e, "/register", randomToken(), url.Values{"handle": {"busy_new"}, "password": {testPassword}}, 503, "Authentication is busy")
	agExpect(e, "/setup", randomToken(), url.Values{"handle": {"busy_admin"}, "password": {testPassword}, "token": {testSetupToken}}, 503, "Authentication is busy")
	agExpect(e, "/pgp/2fa", s, url.Values{"enable": {"0"}, "password": {testPassword}}, 503, "Authentication is busy")
	// Refused before any per-handle budget is spent and before anything is written.
	if agLimited(e, "auth:busy_user") || agLimited(e, "auth:busy_new") {
		t.Fatal("busy refusal consumed the per-handle limit")
	}
	if agUserSessions(e, id) != 1 || agInt(e, "SELECT count(*) FROM users") != 1 || agStr(e, "SELECT pgp_2fa::text FROM users WHERE id=$1", id) != "true" {
		t.Fatal("busy refusal changed data")
	}
	drain()
	w := e.do("POST", "/login", randomToken(), creds)
	e.check(w, 303)
	e.session(w)
}

// --- B9: sessions and pending logins ---

func TestExpiredSessionIsSignedOut(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("exp_user", "buyer")
	n := randomToken()
	agExec(e, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,'x')", n, id)
	agExec(e, "UPDATE sessions SET expires=now()-interval '1 second' WHERE user_id=$1", id)
	w := e.do("GET", "/account", s, nil)
	e.check(w, 303)
	if w.Header().Get("Location") != "/login" {
		t.Fatalf("expired session redirected to %q", w.Header().Get("Location"))
	}
	if strings.Contains(e.body("GET", "/", s, nil, 200), "exp_user") {
		t.Fatal("expired session still shown as signed in")
	}
	agExpect(e, "/notifications", s, url.Values{"id": {n}}, 401, "Sign in required")
	if agStr(e, "SELECT is_read::text FROM notifications WHERE id=$1", n) != "false" {
		t.Fatal("expired session changed data")
	}
}

func TestExpiredPendingLoginRejected(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("exp_pending", "buyer")
	key, recovery := agEnableTOTP(e, id)
	_, pub := testPGPKey(t, "dave")
	agEnablePGP(e, id, pub)
	p := agPending(e, id)
	agExec(e, "UPDATE pending_logins SET expires=now()-interval '1 second' WHERE token_hash=$1", digest(p.Value))
	anon := randomToken()
	agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, 401, "expired", p)
	agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}}, 401, "expired", p)
	agExpect(e, "/challenge/pgp", anon, nil, 401, "expired", p)
	if agUserSessions(e, id) != 1 || agInt(e, "SELECT totp_last_step::int FROM users WHERE id=$1", id) != 0 ||
		agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND used_at IS NULL", id) != 1 ||
		agInt(e, "SELECT count(*) FROM pending_logins WHERE pgp_nonce IS NOT NULL") != 0 {
		t.Fatal("expired pending login changed data")
	}
	if b := e.body("GET", "/challenge", anon, nil, 200, p); strings.Contains(b, `value="totp"`) {
		t.Fatal("expired pending login still offered a verification form")
	}
}

func TestLoginRotatesSessionToken(t *testing.T) {
	e := newTestApp(t)
	id, old := e.user("rot_user", "buyer")
	creds := url.Values{"handle": {"rot_user"}, "password": {testPassword}}
	w := e.do("POST", "/login", old, creds)
	e.check(w, 303)
	fresh := e.session(w)
	if fresh == old {
		t.Fatal("login kept the previous session token")
	}
	e.check(e.do("GET", "/account", fresh, nil), 200)
	if loc := e.do("GET", "/account", old, nil).Header().Get("Location"); loc != "/login" {
		t.Fatalf("old token still authenticates (location %q)", loc)
	}
	if agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1", digest(old)) != 0 || agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1 AND user_id=$2", digest(fresh), id) != 1 {
		t.Fatal("session rows not rotated")
	}
	// Session fixation: an anonymous cookie chosen before sign-in never becomes the session.
	anon := randomToken()
	w = e.do("POST", "/login", anon, creds)
	e.check(w, 303)
	if s := e.session(w); s == anon || agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1", digest(anon)) != 0 {
		t.Fatal("pre-login cookie became the session")
	}
	// The same holds when sign-in completes through the second-factor challenge.
	key, _ := agEnableTOTP(e, id)
	anon = randomToken()
	w = e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, agPending(e, id))
	e.check(w, 303)
	if s := e.session(w); s == anon || agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1", digest(s)) != 1 {
		t.Fatal("challenge did not issue a fresh session")
	}
	if !agClearedCookie(w, "pending") {
		t.Fatal("pending cookie not cleared after sign-in")
	}
}

func agClearedCookie(w interface{ Result() *http.Response }, name string) bool {
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

func TestLogoutAndRevokeSessionsDeleteRows(t *testing.T) {
	e := newTestApp(t)
	id, s1 := e.user("lo_user", "buyer")
	s2 := agSession(e, id)
	otherID, other := e.user("lo_other", "buyer")

	w := e.do("POST", "/logout", s1, nil)
	e.check(w, 303)
	if w.Header().Get("Location") != "/login" || !agClearedCookie(w, "session") {
		t.Fatalf("logout response %v", w.Header())
	}
	if agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1", digest(s1)) != 0 || agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1", digest(s2)) != 1 {
		t.Fatal("logout must delete only the current session")
	}
	e.check(e.do("GET", "/account", s1, nil), 303)
	e.check(e.do("GET", "/account", s2, nil), 200)
	if !e.auditHas(id, "Signed out") {
		t.Fatal("logout not audited")
	}

	s3 := agSession(e, id)
	w = e.do("POST", "/revoke-sessions", s2, nil)
	e.check(w, 303)
	if !agClearedCookie(w, "session") || agUserSessions(e, id) != 0 {
		t.Fatalf("revoke left %d sessions", agUserSessions(e, id))
	}
	for _, s := range []string{s2, s3} {
		e.check(e.do("GET", "/account", s, nil), 303)
	}
	if agUserSessions(e, otherID) != 1 {
		t.Fatal("revoking one user's sessions touched another user")
	}
	e.check(e.do("GET", "/account", other, nil), 200)
	if !e.auditHas(id, "Revoked all sessions") {
		t.Fatal("revoke not audited")
	}
}

func TestNotificationMarkReadOnlyOwnRows(t *testing.T) {
	e := newTestApp(t)
	aID, _ := e.user("notif_a", "buyer")
	bID, b := e.user("notif_b", "buyer")
	na, nb := randomToken(), randomToken()
	agExec(e, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,'for a'),($3,$4,'for b')", na, aID, nb, bID)
	e.check(e.do("POST", "/notifications", b, url.Values{"id": {na}}), 303)
	if agStr(e, "SELECT is_read::text FROM notifications WHERE id=$1", na) != "false" {
		t.Fatal("a user marked another user's notification read")
	}
	e.check(e.do("POST", "/notifications", b, url.Values{"id": {nb}}), 303)
	if agStr(e, "SELECT is_read::text FROM notifications WHERE id=$1", nb) != "true" || agStr(e, "SELECT is_read::text FROM notifications WHERE id=$1", na) != "false" {
		t.Fatal("mark-read touched the wrong rows")
	}
}

// --- B9: TOTP and PGP factor error paths ---

func TestTOTPActivateWhenAlreadyEnabled(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("totp_twice", "buyer")
	agEnableTOTP(e, id)
	secret := agStr(e, "SELECT totp_secret FROM users WHERE id=$1", id)
	// A pending secret left behind (e.g. from before activation) must not replace the active one.
	pending, _ := e.A.seal("totp", newTOTPSecret())
	agExec(e, "UPDATE users SET totp_pending=$2 WHERE id=$1", id, pending)
	code, _ := e.totpCodeFor(id, 0) // code for the active secret
	agExpect(e, "/totp/activate", s, url.Values{"code": {code}}, 409, "already enabled")
	agExpect(e, "/totp/enroll", s, nil, 409, "already enabled")
	if agStr(e, "SELECT totp_secret FROM users WHERE id=$1", id) != secret || agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", id) != 1 {
		t.Fatal("activation while enabled replaced the secret or recovery codes")
	}
}

func TestTOTPDisableInputValidation(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("totp_dis", "buyer")
	_, recovery := agEnableTOTP(e, id)
	agExpect(e, "/totp/disable", s, url.Values{"password": {testPassword}}, 400, "Enter your password and a current code")
	agExpect(e, "/totp/disable", s, url.Values{"password": {strings.Repeat("p", 73)}, "code": {recovery}}, 400, "Enter your password and a current code")
	if agLimited(e, "confirm:"+id) {
		t.Fatal("malformed disable reached the password check")
	}
	agExpect(e, "/totp/disable", s, url.Values{"code": {recovery}}, 400, "Enter your current password")
	agExpect(e, "/totp/disable", s, url.Values{"password": {testPassword}, "code": {"12345"}}, 401, "incorrect")
	if agStr(e, "SELECT totp_enabled::text FROM users WHERE id=$1", id) != "true" || agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND used_at IS NULL", id) != 1 {
		t.Fatal("rejected disable changed data")
	}
	// Disabling when TOTP is not enabled is a conflict after the password check.
	_, plain := e.user("totp_none", "buyer")
	agExpect(e, "/totp/disable", plain, url.Values{"password": {testPassword}, "code": {"123456"}}, 409, "not enabled")
}

func TestTOTPUnreadableSecret(t *testing.T) {
	e := newTestApp(t)
	foreign := &App{setupToken: strings.Repeat("z", 64)} // another server's SETUP_TOKEN
	id, _ := e.user("totp_lost", "buyer")
	_, recovery := agEnableTOTP(e, id)
	sealed, err := foreign.seal("totp", newTOTPSecret())
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, "UPDATE users SET totp_secret=$2 WHERE id=$1", id, sealed)
	p := agPending(e, id)
	anon := randomToken()
	agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}, "code": {"123456"}}, 500, "Use a recovery code", p)
	if agUserSessions(e, id) != 1 || agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(p.Value)) != 1 {
		t.Fatal("unreadable secret signed in or dropped the pending login")
	}
	// The advertised way out works.
	w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}}, p)
	e.check(w, 303)
	e.session(w)

	// A pending (not yet active) secret that can no longer be read cannot be activated.
	id2, s2 := e.user("totp_lost2", "buyer")
	agExec(e, "UPDATE users SET totp_pending=$2 WHERE id=$1", id2, sealed)
	agExpect(e, "/totp/activate", s2, url.Values{"code": {"123456"}}, 409, "Start enrollment again")
	if agStr(e, "SELECT totp_enabled::text FROM users WHERE id=$1", id2) != "false" {
		t.Fatal("unreadable pending secret activated")
	}
	if !strings.Contains(e.body("GET", "/totp", s2, nil, 200), "can no longer be read") {
		t.Fatal("totp page does not explain the unreadable pending secret")
	}
}

func TestPGPLoginCodeErrorPaths(t *testing.T) {
	e := newTestApp(t)
	anon := randomToken()
	agExpect(e, "/challenge/pgp", anon, nil, 401, "expired") // no pending cookie at all

	// PGP sign-in not enabled (TOTP only): no code is created and method=pgp is refused.
	totpID, _ := e.user("pgp_off", "buyer")
	agEnableTOTP(e, totpID)
	p := agPending(e, totpID)
	agExpect(e, "/challenge/pgp", anon, nil, 400, "not enabled", p)
	agExpect(e, "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {"anything"}}, 400, "Choose a verification method", p)
	if agInt(e, "SELECT count(*) FROM pending_logins WHERE pgp_nonce IS NOT NULL") != 0 || agUserSessions(e, totpID) != 1 {
		t.Fatal("PGP code issued or sign-in completed for an account without PGP sign-in")
	}

	// A proven key that cannot receive encrypted messages: refused, nothing stored.
	signID, signSess := e.user("pgp_signonly", "buyer")
	agEnablePGP(e, signID, agSignOnlyKey(t))
	p = agPending(e, signID)
	agExpect(e, "/challenge/pgp", anon, nil, 409, "cannot be encrypted", p)
	if agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1 AND pgp_nonce IS NULL AND pgp_challenge IS NULL", digest(p.Value)) != 1 {
		t.Fatal("unusable key stored a code")
	}
	// Turning PGP sign-in on is refused for such a key as well.
	agExec(e, "UPDATE users SET pgp_2fa=false WHERE id=$1", signID)
	agExpect(e, "/pgp/2fa", signSess, url.Values{"enable": {"1"}}, 409, "no usable encryption subkey")
	if agStr(e, "SELECT pgp_2fa::text FROM users WHERE id=$1", signID) != "false" {
		t.Fatal("PGP sign-in enabled for a sign-only key")
	}

	// Enabled with a usable key: answering before requesting a code, or with a wrong code, fails.
	key, pub := testPGPKey(t, "erin")
	id, _ := e.user("pgp_on", "buyer")
	agEnablePGP(e, id, pub)
	p = agPending(e, id)
	agExpect(e, "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {"guess"}}, 401, "Create an encrypted sign-in code first", p)
	e.check(e.do("POST", "/challenge/pgp", anon, nil, p), 303)
	armored := agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(p.Value))
	code := testDecrypt(t, key, armored)
	wrong := "0" + code[1:]
	if wrong == code {
		wrong = "1" + code[1:]
	}
	agExpect(e, "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {wrong}}, 401, "does not match", p)
	agExpect(e, "/challenge", anon, url.Values{"method": {"pgp"}}, 401, "does not match", p)
	if agUserSessions(e, id) != 1 {
		t.Fatal("wrong PGP code signed in")
	}
	w := e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {strings.ToUpper(code)}}, p)
	e.check(w, 303)
	if agUserSessions(e, id) != 2 || agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(p.Value)) != 0 {
		t.Fatal("correct PGP code did not complete sign-in and consume the pending login")
	}
}

func TestPGPVerifyRejectsChangedKey(t *testing.T) {
	e := newTestApp(t)
	alice, alicePub := testPGPKey(t, "alice")
	_, bobPub := testPGPKey(t, "bob")
	_, bobFP, _ := parsePublicKey(bobPub)
	id, s := e.user("pgp_swap", "buyer")
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {alicePub}}), 303)
	e.check(e.do("POST", "/pgp/challenge", s, url.Values{"kind": {"sign"}}), 303)
	challenge := agStr(e, "SELECT challenge FROM pgp_challenges WHERE user_id=$1", id)
	proof := testClearsign(t, alice, challenge)

	// The saved key changes underneath an open challenge (the /account path also purges challenges).
	agExec(e, "UPDATE users SET pgp=$2,pgp_fingerprint=$3 WHERE id=$1", id, bobPub, bobFP)
	agExpect(e, "/pgp/verify", s, url.Values{"signature": {proof}}, 409, "changed since this challenge")
	// A saved key that no longer parses is treated the same way.
	agExec(e, "UPDATE users SET pgp='-----BEGIN PGP PUBLIC KEY BLOCK-----\nbroken\n-----END PGP PUBLIC KEY BLOCK-----' WHERE id=$1", id)
	agExpect(e, "/pgp/verify", s, url.Values{"signature": {proof}}, 409, "changed since this challenge")
	if agStr(e, "SELECT (pgp_verified_at IS NULL)::text FROM users WHERE id=$1", id) != "true" || e.auditHas(id, "Verified PGP key ownership") {
		t.Fatal("proof for the old key verified the new one")
	}
	// Saving through /account purges the stale challenge, so the old proof has nothing to answer.
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {alicePub}}), 303)
	if agInt(e, "SELECT count(*) FROM pgp_challenges WHERE user_id=$1", id) != 0 {
		t.Fatal("key change kept the open challenge")
	}
	agExpect(e, "/pgp/verify", s, url.Values{"signature": {proof}}, 409, "No open challenge")
}

// --- A-47: adding a second factor needs re-authentication once one is enrolled ---

// A session holder who knows the password but not the TOTP code (e.g. a phished password plus a stolen
// session) must not swap in their own PGP key or turn PGP sign-in on: otherwise the password alone plus
// their key completes sign-in after /revoke-sessions, although the victim's TOTP is still enrolled.
func TestStolenSessionCannotAddPGPFactorToTOTPAccount(t *testing.T) {
	e := newTestApp(t)
	uid, stolen := e.user("victim_totp", "vendor")
	agEnableTOTP(e, uid)
	mallory, malloryPub := testPGPKey(t, "mallory")

	// Saving a key needs the password and a current authenticator code while TOTP is enrolled.
	agExpect(e, "/account", stolen, url.Values{"pgp": {malloryPub}}, 400, "Enter your current password")
	agExpect(e, "/account", stolen, url.Values{"pgp": {malloryPub}, "password": {testPassword}}, 401, "")
	if agStr(e, "SELECT pgp FROM users WHERE id=$1", uid) != "" {
		t.Fatal("key saved on a TOTP account without full confirmation")
	}
	// Other profile fields still save without confirmation when the key is unchanged.
	e.check(e.do("POST", "/account", stolen, url.Values{"xmpp": {"v@example.org"}}), 303)

	// Even with a proven key already on file (as if saved earlier), turning PGP sign-in on needs both.
	_, fp, _ := parsePublicKey(malloryPub)
	agExec(e, "UPDATE users SET pgp=$2,pgp_fingerprint=$3,pgp_verified_at=now(),pgp_2fa=false WHERE id=$1", uid, malloryPub, fp)
	agExpect(e, "/pgp/2fa", stolen, url.Values{"enable": {"1"}}, 400, "Enter your current password")
	agExpect(e, "/pgp/2fa", stolen, url.Values{"enable": {"1"}, "password": {testPassword}}, 401, "")
	if agStr(e, "SELECT pgp_2fa::text FROM users WHERE id=$1", uid) != "false" {
		t.Fatal("PGP sign-in turned on for a TOTP account without full confirmation")
	}
	body := e.body("GET", "/pgp", stolen, nil, 200)
	if !strings.Contains(body, `name="password"`) || !strings.Contains(body, `name="code"`) {
		t.Fatal("/pgp does not ask for the password and authenticator code before turning PGP sign-in on")
	}

	// With the stolen session gone, the password plus mallory's key does not complete sign-in.
	e.check(e.do("POST", "/revoke-sessions", stolen, nil), 303)
	anon, pc := e.passwordLogin("victim_totp")
	e.do("POST", "/challenge/pgp", anon, nil, pc)
	answer := "guess"
	if armored := agStr(e, "SELECT COALESCE(pgp_challenge,'') FROM pending_logins WHERE token_hash=$1", digest(pc.Value)); armored != "" {
		answer = testDecrypt(t, mallory, armored)
	}
	w := e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {answer}}, pc)
	if w.Code == 303 && e.cookie(w, "session") != "" {
		t.Fatal("signed in to a TOTP-protected account without a TOTP code")
	}

	// The owner, with the password and a current code, can do both; the audit says how it was confirmed.
	owner := agSession(e, uid)
	_, ownerPub := testPGPKey(t, "owner")
	code, _ := e.totpCodeFor(uid, 0)
	e.check(e.do("POST", "/account", owner, url.Values{"pgp": {ownerPub}, "password": {testPassword}, "code": {code}}), 303)
	if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Updated PGP key (fingerprint %(confirmed with password and authenticator code)'", uid) != 1 {
		t.Fatal("confirmed key change not audited with its confirmation")
	}
	agExec(e, "UPDATE users SET pgp_verified_at=now() WHERE id=$1", uid)
	next, _ := e.totpCodeFor(uid, 1)
	e.check(e.do("POST", "/pgp/2fa", owner, url.Values{"enable": {"1"}, "password": {testPassword}, "code": {next}}), 303)
	if agStr(e, "SELECT pgp_2fa::text FROM users WHERE id=$1", uid) != "true" || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Turned on PGP sign-in verification%(confirmed with password and authenticator code)'", uid) != 1 {
		t.Fatal("confirmed PGP sign-in enable not applied or not audited with its confirmation")
	}
}

// The mirror case: TOTP cannot be activated on a PGP-sign-in account from a session without the password.
func TestStolenSessionCannotEnrollTOTPOnPGPAccount(t *testing.T) {
	e := newTestApp(t)
	uid, stolen := e.user("victim_pgp", "vendor")
	_, pub := testPGPKey(t, "victim")
	agEnablePGP(e, uid, pub)

	// Starting enrollment only stores an inactive secret; activation is the gate.
	e.check(e.do("POST", "/totp/enroll", stolen, nil), 303)
	if !strings.Contains(e.body("GET", "/totp", stolen, nil, 200), `name="password"`) {
		t.Fatal("/totp does not ask for the password before activation on a PGP-sign-in account")
	}
	code, _ := e.totpCodeFor(uid, 0)
	agExpect(e, "/totp/activate", stolen, url.Values{"code": {code}}, 400, "Enter your current password")
	agExpect(e, "/totp/activate", stolen, url.Values{"code": {code}, "password": {"wrong password!"}}, 401, "Password incorrect")
	if agStr(e, "SELECT totp_enabled::text FROM users WHERE id=$1", uid) != "false" || agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", uid) != 0 {
		t.Fatal("TOTP activated on a PGP-sign-in account without the password")
	}

	e.check(e.do("POST", "/revoke-sessions", stolen, nil), 303)
	anon, pc := e.passwordLogin("victim_pgp")
	next, _ := e.totpCodeFor(uid, 1)
	w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {next}}, pc)
	if w.Code == 303 && e.cookie(w, "session") != "" {
		t.Fatal("signed in to a PGP-sign-in account with an authenticator added from a session")
	}

	// The owner activates with the password; the audit records it.
	owner := agSession(e, uid)
	e.check(e.do("POST", "/totp/activate", owner, url.Values{"code": {code}, "password": {testPassword}}), 303)
	if agStr(e, "SELECT totp_enabled::text FROM users WHERE id=$1", uid) != "true" || !e.auditHas(uid, "Enabled TOTP two-factor authentication; issued 10 recovery codes (confirmed with password)") {
		t.Fatal("confirmed TOTP activation not applied or not audited with its confirmation")
	}
}

// An account with no second factor keeps the session-only flow for its first factor and for key changes.
func TestFirstFactorNeedsNoConfirmation(t *testing.T) {
	e := newTestApp(t)
	uid, s := e.user("first_factor", "buyer")
	_, pub := testPGPKey(t, "first")
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {pub}}), 303)
	agExec(e, "UPDATE users SET pgp_verified_at=now() WHERE id=$1", uid)
	if strings.Contains(e.body("GET", "/pgp", s, nil, 200), `name="password"`) {
		t.Fatal("/pgp asks for a password before the first factor")
	}
	e.check(e.do("POST", "/pgp/2fa", s, url.Values{"enable": {"1"}}), 303)
	if !e.auditHas(uid, "Turned on PGP sign-in verification") || e.auditHas(uid, "confirmed with") {
		t.Fatal("first-factor PGP enable not applied as before")
	}

	id2, s2 := e.user("first_totp", "buyer")
	e.check(e.do("POST", "/totp/enroll", s2, nil), 303)
	if strings.Contains(e.body("GET", "/totp", s2, nil, 200), `name="password"`) {
		t.Fatal("/totp asks for a password before the first factor")
	}
	code, _ := e.totpCodeFor(id2, 0)
	e.check(e.do("POST", "/totp/activate", s2, url.Values{"code": {code}}), 303)
	if agStr(e, "SELECT totp_enabled::text FROM users WHERE id=$1", id2) != "true" || e.auditHas(id2, "confirmed with") {
		t.Fatal("first-factor TOTP activation not applied as before")
	}
}

// A-48: a cross-site link arrives without the SameSite=Strict cookies. The response must not store a
// fresh anonymous `session` cookie over the real one, or the user is signed out on the next direct visit.
func TestCrossSiteLinkClobbersSessionCookie(t *testing.T) {
	e := newTestApp(t)
	_, s := e.user("linked_buyer", "buyer")
	jar := map[string]string{"session": s}
	e.check(e.do("GET", "/account", jar["session"], nil), 200)

	// Cross-site click: the Strict cookie is withheld.
	r := httptest.NewRequest("GET", "/account", nil)
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.Name == "session" && c.SameSite == http.SameSiteStrictMode && c.Path == "/" {
			jar["session"] = c.Value // top-level navigation response: stored, overwriting the real session
		}
	}

	// The user now opens the market directly (same-site): the stored session is gone.
	if w2 := e.do("GET", "/account", jar["session"], nil); w2.Code != 200 {
		t.Fatalf("signed out after following a cross-site link: GET /account = %d %s (session cookie replaced: %v)", w2.Code, w2.Header().Get("Location"), jar["session"] != s)
	}
}

var csrfField = regexp.MustCompile(`name="csrf" value="([0-9a-f]{64})"`)

// a48Do sends a request carrying exactly the given cookies, form (csrf included by the caller) and headers.
func a48Do(e *testEnv, method, path string, form url.Values, cookies map[string]string, header ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	for name, value := range cookies {
		r.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	return w
}

// A-48: a sign-in form rendered on a cookieless (cross-site) landing page carries a CSRF token and a
// CAPTCHA bound to the separate pre-login `anon` cookie. The next same-site POST sends both the real
// `session` and that `anon` cookie; it is served anonymously, as the page was rendered, and still submits.
func TestAnonymousLandingFormPostsWhileSignedIn(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("landing_buyer", "buyer")
	agExec(e, "UPDATE settings SET value='true' WHERE key='captcha_required'")

	// Cross-site landing on /login: no cookies sent; only the pre-login cookie is issued.
	w := a48Do(e, "GET", "/login", nil, nil)
	e.check(w, 200)
	if e.cookie(w, "session") != "" {
		t.Fatal("a cookieless request set the session cookie")
	}
	anon := e.cookie(w, "anon")
	m, cm := csrfField.FindStringSubmatch(w.Body.String()), captchaField.FindStringSubmatch(w.Body.String())
	if len(anon) != 64 || m == nil || cm == nil || m[1] != e.A.csrf(anon) {
		t.Fatalf("landing page: anon=%q csrf=%v captcha=%v", anon, m, cm)
	}
	both := map[string]string{"session": s, "anon": anon}
	// The page's CAPTCHA image is a same-site request carrying both cookies.
	e.check(a48Do(e, "GET", "/captcha?id="+cm[1], nil, both), 200)
	// A still-signed-in reload renders as the session user and sets no cookie.
	if w := a48Do(e, "GET", "/account", nil, both); w.Code != 200 || len(w.Result().Cookies()) != 0 {
		t.Fatalf("signed-in GET: %d cookies=%v", w.Code, w.Result().Cookies())
	}

	// The anon-derived token never acts as the signed-in user: a protected action is refused.
	e.check(a48Do(e, "POST", "/account", url.Values{"csrf": {m[1]}, "xmpp": {"x@example.org"}}, both), 401)
	// Another browser's anon token, a cross-site POST and a foreign Origin are still rejected.
	creds := url.Values{"csrf": {m[1]}, "handle": {"landing_buyer"}, "password": {testPassword}, "captcha_id": {cm[1]}, "captcha": {e.A.captchaAnswer(cm[1])}}
	e.check(a48Do(e, "POST", "/login", creds, map[string]string{"session": s, "anon": randomToken()}), 403)
	e.check(a48Do(e, "POST", "/login", creds, both, "Sec-Fetch-Site", "cross-site"), 403)
	e.check(a48Do(e, "POST", "/login", creds, both, "Origin", "http://forum.example"), 403)

	// The landing page's sign-in form submits: CSRF and CAPTCHA match the anon cookie it was rendered for.
	w = a48Do(e, "POST", "/login", creds, both, "Origin", "http://example.com", "Sec-Fetch-Site", "same-origin")
	e.check(w, 303)
	fresh := e.session(w)
	if fresh == s || fresh == anon || agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1 AND user_id=$2", digest(fresh), id) != 1 {
		t.Fatal("sign-in from the landing page did not issue a fresh session")
	}
	e.check(a48Do(e, "GET", "/account", nil, map[string]string{"session": fresh, "anon": anon}), 200)
}

// A-48: a browser with no cookies gets only the pre-login cookie; registering with it sets the session,
// and a pre-upgrade anonymous `session` cookie still backs CSRF until sign-in replaces it.
func TestPreLoginCookieSeparateFromSession(t *testing.T) {
	e := newTestApp(t)
	e.user("prelogin_admin", "admin") // marks the market installed
	w := a48Do(e, "GET", "/register", nil, nil)
	e.check(w, 200)
	anon := e.cookie(w, "anon")
	if e.cookie(w, "session") != "" || len(anon) != 64 {
		t.Fatalf("cookieless GET cookies: %v", w.Result().Cookies())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "anon" && (!c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/") {
			t.Fatalf("anon cookie attributes: %+v", c)
		}
	}
	// The same browser (anon cookie only) gets no new cookie on later GETs.
	if w := a48Do(e, "GET", "/", nil, map[string]string{"anon": anon}); len(w.Result().Cookies()) != 0 {
		t.Fatalf("anon GET reissued cookies: %v", w.Result().Cookies())
	}
	w = a48Do(e, "POST", "/register", url.Values{"csrf": {e.A.csrf(anon)}, "handle": {"prelogin_new"}, "password": {"a-long-buyer-password"}}, map[string]string{"anon": anon})
	e.check(w, 303)
	s := e.session(w)
	if s == anon || e.cookie(w, "anon") != "" {
		t.Fatal("register reused or reissued the pre-login token")
	}
	e.check(a48Do(e, "GET", "/account", nil, map[string]string{"session": s, "anon": anon}), 200)
	legacy := randomToken()
	w = e.do("POST", "/login", legacy, url.Values{"handle": {"prelogin_new"}, "password": {"a-long-buyer-password"}})
	e.check(w, 303)
	if s2 := e.session(w); s2 == legacy {
		t.Fatal("legacy anonymous token became the session")
	}
}
