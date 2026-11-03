package market

import (
	"net/url"
	"strings"
	"testing"
)

// A-794: viewing /pgp (and /account, which shares its loader) issues no ownership challenge: only POST
// /pgp/challenge does.
func TestPGPPageViewCreatesNoChallenge(t *testing.T) {
	e := newTestApp(t)
	uid, s := e.user("pgp_view_only", "buyer")
	for _, path := range []string{"/pgp", "/account", "/pgp"} {
		e.check(e.do("GET", path, s, nil), 200)
	}
	if n := agInt(e, "SELECT count(*) FROM pgp_challenges"); n != 0 {
		t.Fatalf("GET /pgp created %d challenge rows", n)
	}
	e.check(e.do("POST", "/pgp/challenge", s, url.Values{"kind": {"sign"}}), 303)
	if n := agInt(e, "SELECT count(*) FROM pgp_challenges WHERE user_id=$1", uid); n != 1 {
		t.Fatalf("POST /pgp/challenge made %d rows", n)
	}
}

// A-794: the ownership challenge's nonce is stored only as its SHA-256 (nonce_hash), the PGP sign-in code only as
// pgpLoginDigest (SHA-256 of the code bound to the key and its proof), and neither plaintext appears in any column
// of its row.
func TestPGPCodesStoredOnlyAsSHA256(t *testing.T) {
	e := newTestApp(t)
	key, pub := testPGPKey(t, "pgp_hashed_codes")
	uid, s := e.keylessUser("pgp_hashed_codes", "buyer")
	agEnablePGP(e, uid, pub)
	e.pgpSessionKey(s)

	e.check(e.do("POST", "/pgp/challenge", s, url.Values{"kind": {"decrypt"}}), 303)
	nonce := testSignInCode(t, key, agStr(e, "SELECT challenge FROM pgp_challenges WHERE user_id=$1", uid))
	if got := agStr(e, "SELECT nonce_hash FROM pgp_challenges WHERE user_id=$1", uid); got != digest(nonce) {
		t.Fatalf("nonce_hash=%q, want sha256 of the nonce", got)
	}
	if row := agStr(e, "SELECT row_to_json(c)::text FROM pgp_challenges c WHERE user_id=$1", uid); strings.Contains(strings.ToLower(row), nonce) {
		t.Fatal("the ownership challenge row holds its nonce in plaintext")
	}

	pc := agPending(e, uid)
	e.check(e.do("POST", "/challenge/pgp", randomToken(), nil, pc), 303)
	code := testSignInCode(t, key, agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(pc.Value)))
	p, err := loadPGPAccount(t.Context(), e.DB, uid, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := agStr(e, "SELECT pgp_nonce FROM pending_logins WHERE token_hash=$1", digest(pc.Value)); got != pgpLoginDigest(code, p) || len(got) != 64 {
		t.Fatalf("pgp_nonce=%q, want the key-bound sha256 of the code", got)
	}
	if row := agStr(e, "SELECT row_to_json(p)::text FROM pending_logins p WHERE token_hash=$1", digest(pc.Value)); strings.Contains(strings.ToLower(row), code) {
		t.Fatal("the pending sign-in row holds its code in plaintext")
	}
}

// A-794: removing the PGP key while PGP sign-in is on and both an ownership challenge and a PGP sign-in code are open
// is allowed with the password and this browser's key proof (authorizeProfileKeyChange). It turns PGP sign-in off with
// the owner told, deletes the open challenge and leaves neither secret usable, even after the same key is saved and
// proved again.
func TestPGPKeyRemovalWithSignInOnEndsOpenChallenges(t *testing.T) {
	e := newTestApp(t)
	key, pub := testPGPKey(t, "pgp_removed_on")
	uid, s := e.keylessUser("pgp_removed_on", "buyer")
	agEnablePGP(e, uid, pub)
	e.pgpSessionKey(s)

	e.check(e.do("POST", "/pgp/challenge", s, url.Values{"kind": {"decrypt"}}), 303)
	nonce := testSignInCode(t, key, agStr(e, "SELECT challenge FROM pgp_challenges WHERE user_id=$1", uid))
	pc, anon := agPending(e, uid), randomToken()
	e.check(e.do("POST", "/challenge/pgp", anon, nil, pc), 303)
	code := testSignInCode(t, key, agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(pc.Value)))

	// Without the password nothing changes.
	agExpect(e, "/account", s, url.Values{"pgp": {""}}, 400, "Enter your current password")
	if agStr(e, "SELECT pgp_2fa::text FROM users WHERE id=$1", uid) != "true" || agInt(e, "SELECT count(*) FROM pgp_challenges WHERE user_id=$1", uid) != 1 {
		t.Fatal("a refused removal changed PGP sign-in or the open challenge")
	}
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {""}, "password": {testPassword}}), 303)
	if agStr(e, "SELECT concat_ws(',',pgp,pgp_fingerprint,pgp_2fa::text,(pgp_verified_at IS NULL)::text) FROM users WHERE id=$1", uid) != ",,false,true" {
		t.Fatal("removal kept the key, its proof or PGP sign-in")
	}
	if agInt(e, "SELECT count(*) FROM pgp_challenges WHERE user_id=$1", uid) != 0 {
		t.Fatal("removal kept the open ownership challenge")
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'The PGP public key on your account was removed; its ownership proof and PGP sign-in verification were cleared.%'", uid) != 1 {
		t.Fatal("the owner was not told PGP sign-in was cleared")
	}
	if !e.auditHas(uid, "Removed PGP key (ownership proof and PGP sign-in cleared)") {
		t.Fatal("removal not audited")
	}
	// With PGP sign-in off (and the market's default policy not requiring it) the account has no PGP factor to answer.
	stale := func(when string) {
		t.Helper()
		agExpect(e, "/pgp/verify", s, url.Values{"response": {nonce}}, 409, "No open challenge")
		agExpect(e, "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {code}}, 400, "Choose a verification method", pc)
		if agUserSessions(e, uid) != 1 || agStr(e, "SELECT (pgp_verified_at IS NULL)::text FROM users WHERE id=$1", uid) != "true" {
			t.Fatalf("%s: a stale challenge signed in or proved the key", when)
		}
	}
	stale("after removal")

	// Saving and proving the same key again revives neither: they were bound to the removed binding.
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {pub}, "password": {testPassword}}), 303)
	e.pgpSessionKey(s)
	stale("after the same key was saved again")
	agExec(e, "UPDATE users SET pgp_verified_at=now(),pgp_2fa=true WHERE id=$1", uid)
	if agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(pc.Value)) != 1 {
		t.Fatal("the pending sign-in holding the stale code is gone, so the check below would prove nothing")
	}
	agExpect(e, "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {code}}, 401, "does not match", pc)
	if agUserSessions(e, uid) != 1 {
		t.Fatal("a code issued before removal signed in after the key was proved again")
	}
	// A code issued now completes sign-in: the refusals above were the stale binding, not a broken flow.
	fresh := agPending(e, uid)
	e.check(e.do("POST", "/challenge/pgp", anon, nil, fresh), 303)
	now := testSignInCode(t, key, agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(fresh.Value)))
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {now}}, fresh), 303)
	if agUserSessions(e, uid) != 2 {
		t.Fatal("a fresh PGP sign-in code did not sign in")
	}
}
