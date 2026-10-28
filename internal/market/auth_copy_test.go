package market

import (
	"testing"
	"time"
)

// A-159(f): the expiry and recovery copy on the sign-in challenge (challenge.html, factor-pgp.html), the PGP page
// (pgp.html) and the administrator's factor-reset panel (admin-auth.html) is rendered as written, and the two expiry
// figures it states match the lifetimes the database rows are given (10 minutes pending, 30 minutes challenge).
func TestAuthExpiryAndRecoveryCopyRenders(t *testing.T) {
	e := newTestApp(t)

	uid, sess := e.user("copy_pgp", "buyer") // also completes first-run setup
	// challenge.html with nothing pending.
	mustContain(t, e.body("GET", "/challenge", randomToken(), nil, 200),
		"No sign-in verification is pending. Pending sign-ins expire after ten minutes.")

	key := lifecycleKey(t, "copy_pgp", time.Now().Add(-time.Hour), 0, 0)
	provePGP(e, uid, sess, key, armoredPublicKey(t, key))

	// challenge.html with a PGP-only pending sign-in: the only method, the lost-key recovery line and the
	// gpg --decrypt instruction once the code is requested.
	anon, pc := e.pendingSignIn("copy_pgp")
	page := e.body("GET", "/challenge", anon, nil, 200, pc)
	mustContain(t, page,
		"Your password was accepted. Complete the verification method below to finish signing in.",
		"Lost your PGP key? Only a market administrator can turn your second factors off, or, for an administrator account, the operator on the server.")
	mustNotContain(t, page, "Either your authenticator or your verified PGP key can complete sign-in.")
	e.check(e.do("POST", "/challenge/pgp", anon, nil, pc), 303)
	mustContain(t, e.body("GET", "/challenge", anon, nil, 200, pc),
		"run <code>gpg --decrypt challenge.asc</code>. It names your handle, this market, when the code was requested, until when it is valid and the market",
		"Paste only the code from its last line, and only if your browser", "address bar shows one of those addresses",
		"If it says no official address is configured, check the address with the market")
	if secs := agInt(e, "SELECT EXTRACT(EPOCH FROM expires-now())::int FROM pending_logins WHERE token_hash=$1", digest(pc.Value)); secs < 540 || secs > 600 {
		t.Fatalf("a pending sign-in lives %d s, but the page says it expires after ten minutes", secs)
	}

	// With an authenticator as well, either factor completes sign-in and the lost-factor line names both.
	agEnableTOTP(e, uid)
	anon, pc = e.pendingSignIn("copy_pgp")
	mustContain(t, e.body("GET", "/challenge", anon, nil, 200, pc),
		"Your password was accepted. Either your authenticator or your verified PGP key can complete sign-in.",
		"Lost your authenticator and recovery codes? Only a market administrator can turn your second factors off")

	// pgp.html: the lifetime of an ownership challenge, each challenge's instruction and its expiry line, and the
	// warning shown beside the sign-in switch when it is off.
	mustContain(t, e.body("GET", "/pgp", sess, nil, 200), "A new challenge replaces any open one. Challenges expire after 30 minutes.")
	e.check(e.do("POST", "/pgp/challenge", sess, map[string][]string{"kind": {"sign"}}), 303)
	page = e.body("GET", "/pgp", sess, nil, 200)
	mustContain(t, page, "gpg --clearsign challenge.txt", "Expires 20")
	e.check(e.do("POST", "/pgp/challenge", sess, map[string][]string{"kind": {"decrypt"}}), 303)
	page = e.body("GET", "/pgp", sess, nil, 200)
	mustContain(t, page, "run <code>gpg --decrypt challenge.asc</code> and paste the code it prints. Expires 20")
	if mins := agInt(e, "SELECT EXTRACT(EPOCH FROM expires-now())::int/60 FROM pgp_challenges WHERE user_id=$1", uid); mins < 29 || mins > 30 {
		t.Fatalf("an ownership challenge lives %d minutes, but the page says it expires after 30 minutes", mins)
	}
	agExec(e, "UPDATE users SET pgp_2fa=false WHERE id=$1", uid)
	mustContain(t, e.body("GET", "/pgp", sess, nil, 200),
		"If the key expires or you lose it, you cannot sign in unless another verification method is enrolled.")

	// admin-auth.html: how staff check a factor reset and what a restore hands back.
	_, admin := e.user("copy_admin", "admin")
	mustContain(t, e.body("GET", "/admin/users", admin, nil, 200),
		"When the account holder can still use their verified PGP key, ask them to sign a fresh message and verify it against the key already saved on their account. Do not accept a replacement key merely because it was supplied with the reset request; verify identity out of band first. If they cannot use that key, follow <em>Replace a lost PGP key</em> in the operator guide (<code>docs/operator-guide.md#replace-a-lost-pgp-key</code>) before resetting.",
		"Check the request really comes from the account holder first: anyone who knows their password can then sign in.",
		"Keep your recovery codes offline; an administrator lockout is recovered by the server operator (see the operator guide).",
		"Restoring hands the account back to whoever has its password.")
}
