package market

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// The sole administrator can recover a lost private key without weakening strict
// sign-in for anyone: the host installs public material, and only a real decrypt
// with its replacement private key creates a new authenticated session.
func TestResetAdminFactorsWithPublicKeyRestoresStrictAdminLogin(t *testing.T) {
	e := newTestApp(t)
	ctx := context.Background()
	id, oldSession := e.user("host_recovery_admin", "admin")
	agEnableTOTP(e, id)
	_, oldPublic := testPGPKey(t, "host-old")
	agEnablePGP(e, id, oldPublic)
	agExec(e, "UPDATE settings SET value='false' WHERE key=$1", settingAllowLoginWithoutPGP)
	anon, oldPending := e.pendingSignIn("host_recovery_admin")
	e.check(e.do("POST", "/challenge/pgp", anon, nil, oldPending), http.StatusSeeOther)
	e.check(e.do("POST", "/pgp/challenge", oldSession, url.Values{"kind": {"decrypt"}}), http.StatusSeeOther)
	generation := agInt(e, "SELECT pgp_key_generation FROM users WHERE id=$1", id)
	startAudits := agInt(e, "SELECT count(*) FROM audit_events")
	replacement, public := testPGPKey(t, "host-replacement")
	_, fingerprint, canonical, err := parseCanonicalPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}

	summary, err := ResetAdminFactorsWithPublicKey(ctx, e.DB, "host_recovery_admin", public)
	if err != nil {
		t.Fatal(err)
	}
	_, oldFingerprint, _ := parsePublicKey(oldPublic)
	if !strings.Contains(summary, "PGP public key replaced fingerprint "+oldFingerprint+" -> "+fingerprint+" (ownership unverified)") {
		t.Fatalf("recovery summary %q", summary)
	}
	if agStr(e, "SELECT pgp FROM users WHERE id=$1", id) != canonical ||
		agStr(e, "SELECT pgp_fingerprint FROM users WHERE id=$1", id) != fingerprint ||
		agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND pgp_verified_at IS NULL AND NOT pgp_2fa AND NOT totp_enabled", id) != 1 ||
		agInt(e, "SELECT pgp_key_generation FROM users WHERE id=$1", id) != generation+1 {
		t.Fatal("replacement kept old ownership, optional factors, or key generation")
	}
	for _, table := range []string{"sessions", "pending_logins", "recovery_codes", "pgp_challenges"} {
		if agInt(e, "SELECT count(*) FROM "+table+" WHERE user_id=$1", id) != 0 {
			t.Fatalf("%s survived host recovery", table)
		}
	}
	if agStr(e, "SELECT value FROM settings WHERE key=$1", settingAllowLoginWithoutPGP) != "false" {
		t.Fatal("host recovery weakened mandatory PGP policy")
	}
	if agInt(e, "SELECT count(*) FROM audit_events") != startAudits+1 ||
		!e.auditHas(id, "Second factors reset by host command:") || !e.auditHas(id, fingerprint) ||
		agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", id) != 1 {
		t.Fatal("host recovery did not audit and notify exactly once")
	}
	if loc := e.do("GET", "/admin", oldSession, nil).Header().Get("Location"); loc != "/login?next=%2Fadmin" {
		t.Fatalf("old administrator session survived: %q", loc)
	}
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {"old-code"}}, oldPending), http.StatusUnauthorized)

	anon, pending := e.pendingSignIn("host_recovery_admin")
	if agInt(e, "SELECT count(*) FROM sessions WHERE user_id=$1", id) != 0 {
		t.Fatal("replacement bypassed mandatory PGP sign-in")
	}
	e.check(e.do("POST", "/challenge/pgp", anon, nil, pending), http.StatusSeeOther)
	ciphertext := agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(pending.Value))
	code := testSignInCode(t, replacement, ciphertext)
	w := e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {code}}, pending)
	e.check(w, http.StatusSeeOther)
	session := e.session(w)
	assertPGPAccess(e, id, session, true)
	e.check(e.do("GET", "/admin", session, nil), http.StatusOK)
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND pgp_verified_at IS NOT NULL", id) != 1 {
		t.Fatal("real replacement decrypt did not prove key ownership")
	}
	if agStr(e, "SELECT value FROM settings WHERE key=$1", settingAllowLoginWithoutPGP) != "false" {
		t.Fatal("successful recovery changed strict login")
	}
	// Optional factors being absent does not prevent a later host key recovery.
	_, nextPublic := testPGPKey(t, "host-next")
	if _, err = ResetAdminFactorsWithPublicKey(ctx, e.DB, "host_recovery_admin", nextPublic); err != nil {
		t.Fatalf("lost key without optional factors: %v", err)
	}
}

func TestResetAdminFactorsRejectsPrivateReplacementAtomically(t *testing.T) {
	e := newTestApp(t)
	id, session := e.user("host_reject_admin", "admin")
	agEnableTOTP(e, id)
	pending := agPending(e, id)
	oldPublic := agStr(e, "SELECT pgp FROM users WHERE id=$1", id)
	generation := agInt(e, "SELECT pgp_key_generation FROM users WHERE id=$1", id)
	key, public := testPGPKey(t, "host-invalid")
	var private bytes.Buffer
	w, err := armor.Encode(&private, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = key.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	var mixed bytes.Buffer
	w, err = armor.Encode(&mixed, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	primary := *key
	primary.Subkeys = nil
	if err = primary.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err = key.Subkeys[0].PrivateKey.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err = key.Subkeys[0].Sig.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, replacement string
	}{
		{"private", private.String()},
		{"private subkey in public armor", mixed.String()},
		{"public plus private", public + private.String()},
		{"oversized", strings.Repeat("x", 16385)},
		{"garbage", "not a public key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResetAdminFactorsWithPublicKey(context.Background(), e.DB, "host_reject_admin", tc.replacement); err == nil {
				t.Fatal("invalid recovery key accepted")
			}
			if agStr(e, "SELECT pgp FROM users WHERE id=$1", id) != oldPublic ||
				agInt(e, "SELECT pgp_key_generation FROM users WHERE id=$1", id) != generation ||
				agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND totp_enabled", id) != 1 ||
				agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1", digest(session)) != 1 ||
				agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(pending.Value)) != 1 ||
				agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", id) != 1 ||
				agInt(e, "SELECT count(*) FROM audit_events") != 0 ||
				agInt(e, "SELECT count(*) FROM notifications") != 0 {
				t.Fatal("refused replacement changed account, factors, sessions or records")
			}
		})
	}
}
