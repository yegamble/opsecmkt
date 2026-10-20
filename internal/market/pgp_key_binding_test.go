package market

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func TestPGPBindingPasswordAloneCannotReplaceOrRemoveAnyExistingKey(t *testing.T) {
	_, replacement := testPGPKey(t, "binding-replacement")
	_, valid := testPGPKey(t, "binding-valid")
	expired := armoredPublicKey(t, lifecycleKey(t, "binding-expired", time.Now().Add(-2*time.Hour), 3600, 3600))
	revokedKey, _ := testPGPKey(t, "binding-revoked")
	if err := revokedKey.RevokeKey(packet.KeyCompromised, "test revocation", nil); err != nil {
		t.Fatal(err)
	}
	for name, stored := range map[string]string{
		"valid": valid, "expired": expired, "revoked": armoredPublicKey(t, revokedKey), "unreadable": "legacy-unreadable-public-key",
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestApp(t)
			userID, token := e.keylessUser("binding_"+name, "buyer")
			agExec(e, "UPDATE users SET pgp=$2 WHERE id=$1", userID, stored)
			generation := agInt(e, "SELECT pgp_key_generation FROM users WHERE id=$1", userID)
			audits := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", userID)
			for _, public := range []string{replacement, ""} {
				agExpect(e, "/account", token, url.Values{"pgp": {public}, "password": {testPassword}}, 403, "existing PGP key requires")
				if agStr(e, "SELECT pgp FROM users WHERE id=$1", userID) != stored ||
					agInt(e, "SELECT pgp_key_generation FROM users WHERE id=$1", userID) != generation {
					t.Fatal("password-only key edit changed the protected binding")
				}
			}
			agExpect(e, "/totp/enroll", token, nil, 403, "existing PGP key")
			if agStr(e, "SELECT totp_pending FROM users WHERE id=$1", userID) != "" ||
				agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", userID) != audits {
				t.Fatal("a refused bootstrap wrote a pending factor or audit")
			}
		})
	}
}

func TestPGPBindingOldKeyProofAndPreexistingAuthenticatorAuthorizeRotation(t *testing.T) {
	e := newTestApp(t)
	userID, token := e.keylessUser("binding_owner", "buyer")
	oldKey, oldPublic := testPGPKey(t, "binding-owner-old")
	newKey, newPublic := testPGPKey(t, "binding-owner-new")
	// First public-key enrollment does not need an older PGP binding.
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {oldPublic}, "password": {testPassword}}), 303)
	proveAccess(e, userID, token, oldKey)
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {newPublic}, "password": {testPassword}}), 303)
	assertPGPAccess(e, userID, token, false)
	proveAccess(e, userID, token, newKey)
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {""}, "password": {testPassword}}), 303)
	if agStr(e, "SELECT pgp FROM users WHERE id=$1", userID) != "" {
		t.Fatal("old-key possession did not authorize removal")
	}

	// Model an existing, previously enrolled authenticator on a separate bound
	// account. The actual edit must confirm a fresh code from that same factor.
	otherID, other := e.keylessUser("binding_totp", "buyer")
	agExec(e, "UPDATE users SET pgp=$2 WHERE id=$1", otherID, "legacy-unreadable-public-key")
	secret, _ := agEnableTOTP(e, otherID)
	w := e.do("POST", "/account", other, url.Values{"pgp": {newPublic}, "password": {testPassword}, "code": {"not-an-authenticator-code"}})
	if w.Code == 303 || agStr(e, "SELECT pgp FROM users WHERE id=$1", otherID) != "legacy-unreadable-public-key" {
		t.Fatal("a wrong authenticator code authorized a key replacement")
	}
	e.check(e.do("POST", "/account", other, url.Values{"pgp": {newPublic}, "password": {testPassword}, "code": {agTOTPNow(secret)}}), 303)
	_, _, canonical, _ := parseCanonicalPublicKey(newPublic)
	if agStr(e, "SELECT pgp FROM users WHERE id=$1", otherID) != canonical {
		t.Fatal("a preexisting freshly confirmed authenticator could not authorize replacement")
	}
	assertPGPAccess(e, otherID, other, false)
}

func TestPGPBindingSafeRenewalAllowedButUnsafeSameFingerprintNeedsProof(t *testing.T) {
	e := newTestApp(t)
	userID, token := e.keylessUser("binding_renewal", "buyer")
	created := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	key := lifecycleKey(t, "binding-renewal", created, 3600, 3600)
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {armoredPublicKey(t, key)}, "password": {testPassword}}), 303)
	renewKey(t, key, time.Now().Truncate(time.Second), 365*86400, 365*86400)
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {armoredPublicKey(t, key)}, "password": {testPassword}}), 303)
	assertPGPAccess(e, userID, token, false)

	for _, variant := range []string{"subkey-rollback", "revocation-removal"} {
		t.Run(variant, func(t *testing.T) {
			uid, session := e.keylessUser("binding_"+strings.ReplaceAll(variant, "-", "_"), "buyer")
			k := twoSubkeyKey(t, "binding-"+variant, created)
			if variant == "revocation-removal" {
				if err := k.RevokeSubkey(&k.Subkeys[0], packet.KeyCompromised, "test", nil); err != nil {
					t.Fatal(err)
				}
			}
			stored := armoredPublicKey(t, k)
			e.check(e.do("POST", "/account", session, url.Values{"pgp": {stored}, "password": {testPassword}}), 303)
			var candidate string
			if variant == "subkey-rollback" {
				all := k.Subkeys
				k.Subkeys = all[:1]
				candidate = armoredPublicKey(t, k)
				k.Subkeys = all
			} else {
				revs := k.Subkeys[0].Revocations
				k.Subkeys[0].Revocations = nil
				candidate = armoredPublicKey(t, k)
				k.Subkeys[0].Revocations = revs
			}
			agExpect(e, "/account", session, url.Values{"pgp": {candidate}, "password": {testPassword}}, 403, "existing PGP key requires")
			_, _, canonical, _ := parseCanonicalPublicKey(stored)
			if agStr(e, "SELECT pgp FROM users WHERE id=$1", uid) != canonical {
				t.Fatal("an unsafe same-fingerprint copy replaced the binding without proof")
			}
			proveAccess(e, uid, session, k)
			e.check(e.do("POST", "/account", session, url.Values{"pgp": {candidate}, "password": {testPassword}}), 303)
		})
	}
}

func TestPGPBindingFirstTOTPAndMaterialDisclosureRequireOldKeyProof(t *testing.T) {
	e := newTestApp(t)
	userID, owner := e.keylessUser("binding_first_totp", "buyer")
	key, public := testPGPKey(t, "binding-first-totp")
	e.check(e.do("POST", "/account", owner, url.Values{"pgp": {public}, "password": {testPassword}}), 303)
	thief := agSession(e, userID)
	agExpect(e, "/totp/enroll", thief, nil, 403, "existing PGP key")

	// A stale or otherwise known pending secret must not authorize activation.
	knownSecret := newTOTPSecret()
	knownSealed, err := e.A.seal("totp", knownSecret)
	if err != nil {
		t.Fatal(err)
	}
	knownKey, _ := decodeTOTPSecret(knownSecret)
	agExec(e, "UPDATE users SET totp_pending=$2 WHERE id=$1", userID, knownSealed)
	agExpect(e, "/totp/activate", thief, url.Values{"password": {testPassword}, "code": {agTOTPNow(knownKey)}}, 403, "existing PGP key")
	proveAccess(e, userID, owner, key)
	e.check(e.do("POST", "/totp/enroll", owner, nil), 303)
	pending := agStr(e, "SELECT totp_pending FROM users WHERE id=$1", userID)
	secret, err := e.A.open("totp", pending)
	if err != nil {
		t.Fatal(err)
	}
	totp, _ := decodeTOTPSecret(secret)
	page := e.body("GET", "/totp", thief, nil, 200)
	mustNotContain(t, page, groupSecret(secret), "secret="+secret)
	mustContain(t, page, "before viewing authenticator setup or recovery codes")
	if agStr(e, "SELECT totp_pending FROM users WHERE id=$1", userID) != pending {
		t.Fatal("an unproven browser consumed pending owner material")
	}
	agExpect(e, "/totp/activate", thief, url.Values{"password": {testPassword}, "code": {agTOTPNow(totp)}}, 403, "existing PGP key")
	e.check(e.do("POST", "/totp/activate", owner, url.Values{"password": {testPassword}, "code": {agTOTPNow(totp)}}), 303)
	reveal := agStr(e, "SELECT recovery_reveal FROM users WHERE id=$1", userID)
	codes, err := e.A.open("recovery", reveal)
	if err != nil {
		t.Fatal(err)
	}
	page = e.body("GET", "/totp", thief, nil, 200)
	for _, code := range strings.Split(codes, "\n") {
		mustNotContain(t, page, code)
	}
	if agStr(e, "SELECT recovery_reveal FROM users WHERE id=$1", userID) != reveal {
		t.Fatal("an unproven browser consumed newly issued recovery codes")
	}
	mustContain(t, e.body("GET", "/totp", owner, nil, 200), strings.Split(codes, "\n")[0])
}

func TestPGPBindingFirstKeyDiscardsPreviouslyExposedInactiveTOTP(t *testing.T) {
	e := newTestApp(t)
	userID, token := e.keylessUser("binding_keyless_pending", "buyer")
	e.check(e.do("POST", "/totp/enroll", token, nil), 303)
	sealed := agStr(e, "SELECT totp_pending FROM users WHERE id=$1", userID)
	secret, err := e.A.open("totp", sealed)
	if err != nil {
		t.Fatal(err)
	}
	oldSecret, _ := decodeTOTPSecret(secret)
	key, public := testPGPKey(t, "binding-keyless-pending")
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {public}, "password": {testPassword}}), 303)
	if agStr(e, "SELECT totp_pending FROM users WHERE id=$1", userID) != "" {
		t.Fatal("first PGP binding retained a previously password-visible secret")
	}
	proveAccess(e, userID, token, key)
	agExpect(e, "/totp/activate", token, url.Values{"password": {testPassword}, "code": {agTOTPNow(oldSecret)}}, 409, "Start TOTP enrollment first")

	// First enrollment remains available on a genuinely keyless account.
	plainID, plain := e.keylessUser("binding_truly_keyless", "buyer")
	e.check(e.do("POST", "/totp/enroll", plain, nil), 303)
	pending := agStr(e, "SELECT totp_pending FROM users WHERE id=$1", plainID)
	plainSecret, err := e.A.open("totp", pending)
	if err != nil {
		t.Fatal(err)
	}
	plainKey, _ := decodeTOTPSecret(plainSecret)
	e.check(e.do("POST", "/totp/activate", plain, url.Values{"password": {testPassword}, "code": {agTOTPNow(plainKey)}}), 303)
}

func TestPGPBindingIdentityVerifiedWebReplacementRecoversStrictAccount(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("binding_recovery_admin", "admin")
	userID, oldSession := e.keylessUser("binding_lost_key", "buyer")
	_, oldPublic := testPGPKey(t, "binding-lost-key")
	e.check(e.do("POST", "/account", oldSession, url.Values{"pgp": {oldPublic}, "password": {testPassword}}), 303)
	_ = agSession(e, userID)
	_ = agPending(e, userID)
	e.check(e.do("POST", "/pgp/challenge", oldSession, url.Values{"kind": {"decrypt"}}), 303)
	generation := agInt(e, "SELECT pgp_key_generation FROM users WHERE id=$1", userID)
	agExec(e, "UPDATE settings SET value='false' WHERE key=$1", settingAllowLoginWithoutPGP)
	newKey, newPublic := testPGPKey(t, "binding-recovery-new")
	form := url.Values{"handle": {"binding_lost_key"}, "password": {testPassword}, "replacement_pgp": {newPublic}}
	agExpect(e, "/admin/reset-factors", admin, form, 400, "identity was verified out of band")
	form.Set("recovery_identity_verified", "yes")
	var private bytes.Buffer
	armored, err := armor.Encode(&private, "PGP PRIVATE KEY BLOCK", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := newKey.SerializePrivate(armored, nil); err != nil {
		t.Fatal(err)
	}
	if err := armored.Close(); err != nil {
		t.Fatal(err)
	}
	form.Set("replacement_pgp", private.String())
	e.check(e.do("POST", "/admin/reset-factors", admin, form), 400)
	if agInt(e, "SELECT pgp_key_generation FROM users WHERE id=$1", userID) != generation || agUserSessions(e, userID) != 2 {
		t.Fatal("invalid recovery changed the protected account")
	}
	form.Set("replacement_pgp", newPublic)
	e.check(e.do("POST", "/admin/reset-factors", admin, form), 303)
	if agInt(e, "SELECT pgp_key_generation FROM users WHERE id=$1", userID) != generation+1 ||
		agUserSessions(e, userID) != 0 ||
		agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", userID) != 0 ||
		agInt(e, "SELECT count(*) FROM pgp_challenges WHERE user_id=$1", userID) != 0 ||
		agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND (pgp_verified_at IS NOT NULL OR pgp_2fa)", userID) != 0 {
		t.Fatal("identity-verified recovery failed to invalidate all prior authentication")
	}
	if agStr(e, "SELECT value FROM settings WHERE key=$1", settingAllowLoginWithoutPGP) != "false" {
		t.Fatal("replacement-key recovery weakened strict login")
	}
	_, fingerprint, canonical, _ := parseCanonicalPublicKey(newPublic)
	if agStr(e, "SELECT pgp FROM users WHERE id=$1", userID) != canonical {
		t.Fatal("replacement public key was not canonicalized")
	}
	if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2 AND action LIKE '%identity verified out of band%'", adminID, "%"+fingerprint+"%") != 1 {
		t.Fatal("the privileged replacement was not audited on the administrator")
	}
	if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2 AND action LIKE '%identity verified out of band%'", userID, "%"+fingerprint+"%") != 1 {
		t.Fatal("the privileged replacement was not audited on the target")
	}
	anon := randomToken()
	w := e.do("POST", "/login", anon, url.Values{"handle": {"binding_lost_key"}, "password": {testPassword}})
	e.check(w, 303)
	pending := &http.Cookie{Name: "pending", Value: e.cookie(w, "pending")}
	if pending.Value == "" || e.cookie(w, "session") != "" {
		t.Fatal("strict recovery skipped proof of the replacement key")
	}
	e.check(e.do("POST", "/challenge/pgp", anon, nil, pending), 303)
	code := testSignInCode(t, newKey, agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(pending.Value)))
	w = e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {code}}, pending)
	e.check(w, 303)
	assertPGPAccess(e, userID, e.session(w), true)
}
