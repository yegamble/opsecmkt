package market

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

func assertPGPAccess(e *testEnv, userID, token string, want bool) {
	e.t.Helper()
	got, err := pgpAccessVerified(context.Background(), e.DB, userID, token)
	if err != nil || got != want {
		e.t.Fatalf("PGP access = %v, %v; want %v", got, err, want)
	}
}

func accessChallengeCode(e *testEnv, userID, token string, key *openpgp.Entity) string {
	e.t.Helper()
	e.check(e.do("POST", "/pgp/challenge", token, url.Values{"kind": {"decrypt"}}), 303)
	armored := agStr(e, "SELECT challenge FROM pgp_challenges WHERE user_id=$1 AND session_hash=$2", userID, digest(token))
	plaintext := testDecrypt(e.t, key, armored)
	for _, want := range []string{"Purpose: authorize purchases and messages", "PGP authentication code for ", "Official addresses:", "valid for at most 30 minutes"} {
		if !strings.Contains(plaintext, want) {
			e.t.Fatalf("authentication challenge is missing %q", want)
		}
	}
	_, code := signInLines(e.t, plaintext)
	return code
}

func proveAccess(e *testEnv, userID, token string, key *openpgp.Entity) {
	e.t.Helper()
	code := accessChallengeCode(e, userID, token, key)
	e.check(e.do("POST", "/pgp/verify", token, url.Values{"response": {code}}), 303)
	assertPGPAccess(e, userID, token, true)
}

func TestPGPAccessRequiresDecryptionInCurrentBrowser(t *testing.T) {
	e := newTestApp(t)
	userID, token := e.keylessUser("access_buyer", "buyer")
	key, public := testPGPKey(t, "access-owner")
	assertPGPAccess(e, userID, token, false)
	agExpect(e, "/messages", token, nil, 403, "This browser has not completed PGP authentication")
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {public}, "password": {testPassword}}), 303)
	assertPGPAccess(e, userID, token, false)
	agExpect(e, "/messages", token, nil, 403, "This browser has not completed PGP authentication")

	// Signing proves account ownership; it cannot stand in for decryption.
	e.check(e.do("POST", "/pgp/challenge", token, url.Values{"kind": {"sign"}}), 303)
	challenge := agStr(e, "SELECT challenge FROM pgp_challenges WHERE user_id=$1 AND session_hash=$2", userID, digest(token))
	e.check(e.do("POST", "/pgp/verify", token, url.Values{"signature": {testClearsign(t, key, challenge)}}), 303)
	assertPGPAccess(e, userID, token, false)
	agExpect(e, "/messages", token, nil, 403, "This browser has not completed PGP authentication")

	proveAccess(e, userID, token, key)
	// The guard now passes and message validation, rather than PGP access, refuses
	// the intentionally empty message. Browsing alone never reaches this point.
	agExpect(e, "/messages", token, nil, 400, "Encrypt the message locally")
}

func TestPGPAccessChallengesAreSeparateSingleUseAndExpire(t *testing.T) {
	e := newTestApp(t)
	userID, first := e.keylessUser("access_sessions", "buyer")
	key, public := testPGPKey(t, "access-sessions")
	e.check(e.do("POST", "/account", first, url.Values{"pgp": {public}, "password": {testPassword}}), 303)
	second := agSession(e, userID)
	firstCode := accessChallengeCode(e, userID, first, key)
	secondCode := accessChallengeCode(e, userID, second, key)
	if agInt(e, "SELECT count(*) FROM pgp_challenges WHERE user_id=$1", userID) != 2 {
		t.Fatal("one browser replaced another browser's challenge")
	}
	e.check(e.do("POST", "/pgp/verify", first, url.Values{"response": {secondCode}}), 400)
	assertPGPAccess(e, userID, first, false)
	e.check(e.do("POST", "/pgp/verify", first, url.Values{"response": {firstCode}}), 303)
	assertPGPAccess(e, userID, first, true)
	assertPGPAccess(e, userID, second, false)
	if agInt(e, "SELECT count(*) FROM pgp_challenges WHERE user_id=$1", userID) != 1 {
		t.Fatal("finishing one browser consumed another browser's challenge")
	}
	e.check(e.do("POST", "/pgp/verify", second, url.Values{"response": {secondCode}}), 303)
	assertPGPAccess(e, userID, second, true)
	e.check(e.do("POST", "/pgp/verify", second, url.Values{"response": {secondCode}}), 409)

	third := agSession(e, userID)
	code := accessChallengeCode(e, userID, third, key)
	agExec(e, "UPDATE pgp_challenges SET expires=now()-interval '1 second' WHERE user_id=$1 AND session_hash=$2", userID, digest(third))
	e.check(e.do("POST", "/pgp/verify", third, url.Values{"response": {code}}), 409)
	assertPGPAccess(e, userID, third, false)
}

func TestPGPAccessClearsOnKeyChangesAndSessionRotation(t *testing.T) {
	e := newTestApp(t)
	userID, token := e.keylessUser("access_keychange", "buyer")
	key, public := testPGPKey(t, "access-old-key")
	replacementKey, replacement := testPGPKey(t, "access-replacement")
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {public}, "password": {testPassword}}), 303)
	proveAccess(e, userID, token, key)
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {replacement}, "password": {testPassword}}), 303)
	assertPGPAccess(e, userID, token, false)
	proveAccess(e, userID, token, replacementKey)
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {public}, "password": {testPassword}}), 303)
	assertPGPAccess(e, userID, token, false) // restoring the original key cannot restore old proof
	proveAccess(e, userID, token, key)

	nextPassword := "a new unique account password"
	w := e.do("POST", "/account/password", token, url.Values{"password": {testPassword}, "new_password": {nextPassword}, "new_password_confirm": {nextPassword}})
	e.check(w, 303)
	rotated := e.session(w)
	if rotated == "" || rotated == token {
		t.Fatal("password change did not rotate the session")
	}
	assertPGPAccess(e, userID, token, false)
	assertPGPAccess(e, userID, rotated, false)
}

func TestPGPStrictLoginCannotBeReplacedByTOTPOrRecovery(t *testing.T) {
	e := newTestApp(t)
	userID, token := e.keylessUser("access_strict", "buyer")
	key, public := testPGPKey(t, "access-strict")
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {public}, "password": {testPassword}}), 303)
	totp, recovery := agEnableTOTP(e, userID)
	agExec(e, "UPDATE settings SET value='false' WHERE key=$1", settingAllowLoginWithoutPGP)

	anon := randomToken()
	w := e.do("POST", "/login", anon, url.Values{"handle": {"access_strict"}, "password": {testPassword}, "rescue_code": {agTOTPNow(totp)}})
	e.check(w, 303)
	if e.cookie(w, "session") != "" {
		t.Fatal("inline authenticator bypassed mandatory PGP decryption")
	}
	pending := &http.Cookie{Name: "pending", Value: e.cookie(w, "pending")}
	if pending.Value == "" {
		t.Fatal("strict sign-in did not create a pending decryption")
	}
	agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(totp)}}, 403, "requires PGP authentication", pending)
	agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}, "code": {recovery}}, 403, "requires PGP authentication", pending)
	if agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND used_at IS NOT NULL", userID) != 0 {
		t.Fatal("refused PGP bypass consumed a recovery code")
	}
	e.check(e.do("POST", "/challenge/pgp", anon, nil, pending), 303)
	armored := agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(pending.Value))
	code := testSignInCode(t, key, armored)
	w = e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {code}}, pending)
	e.check(w, 303)
	assertPGPAccess(e, userID, e.session(w), true)
	assertPGPAccess(e, userID, token, false)

	_, _ = e.keylessUser("access_nokey", "buyer")
	agExpect(e, "/login", randomToken(), url.Values{"handle": {"access_nokey"}, "password": {testPassword}}, 409, "requires PGP decryption at every sign-in")
}

func TestPGPPolicyRequiresAdministratorProofBeforeStrictLogin(t *testing.T) {
	e := newTestApp(t)
	userID, token := e.keylessUser("access_admin", "admin")
	agExpect(e, "/admin/pgp-access", token, url.Values{settingAllowLoginWithoutPGP: {"false"}, "password": {testPassword}}, 403, "This browser has not completed PGP authentication")
	if agStr(e, "SELECT value FROM settings WHERE key=$1", settingAllowLoginWithoutPGP) != "true" {
		t.Fatal("an administrator without PGP proof enabled strict login")
	}
	key, public := testPGPKey(t, "access-admin")
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {public}, "password": {testPassword}}), 303)
	proveAccess(e, userID, token, key)
	e.check(e.do("POST", "/admin/pgp-access", token, url.Values{settingAllowLoginWithoutPGP: {"false"}, "password": {testPassword}}), 303)
	if agStr(e, "SELECT value FROM settings WHERE key=$1", settingAllowLoginWithoutPGP) != "false" {
		t.Fatal("strict login policy was not saved")
	}

	// Keyless registration must fail while strict login is enabled; including a
	// public key creates the account but still requires actual decryption.
	agExpect(e, "/register", randomToken(), url.Values{"handle": {"strict_new"}, "password": {testPassword}}, 400, "A usable PGP public key is required")
	w := e.do("POST", "/register", randomToken(), url.Values{"handle": {"strict_new"}, "password": {testPassword}, "pgp": {public}})
	e.check(w, 303)
	if e.cookie(w, "session") != "" || e.cookie(w, "pending") == "" {
		t.Fatal("strict registration did not require PGP decryption")
	}
}

func TestPGPAccessRejectsExpiredSessionsAndUnusableKeys(t *testing.T) {
	e := newTestApp(t)
	userID, token := e.user("access_lifecycle", "buyer")
	assertPGPAccess(e, userID, token, true)
	agExec(e, "UPDATE sessions SET expires=now()-interval '1 second' WHERE token_hash=$1", digest(token))
	assertPGPAccess(e, userID, token, false)
	agExec(e, "UPDATE sessions SET expires=now()+interval '1 hour' WHERE token_hash=$1", digest(token))
	assertPGPAccess(e, userID, token, true)

	// Even a matching stored proof must never authorize a malformed or sign-only
	// key (for example, a legacy row repaired outside the normal account form).
	agExec(e, "UPDATE users SET pgp='not a public key' WHERE id=$1", userID)
	agExec(e, "UPDATE sessions SET pgp_access_key=$2 WHERE token_hash=$1", digest(token), digest("not a public key"))
	assertPGPAccess(e, userID, token, false)
	public := agSignOnlyKey(t)
	_, _, canonical, err := parseCanonicalPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, "UPDATE users SET pgp=$2 WHERE id=$1", userID, public)
	agExec(e, "UPDATE sessions SET pgp_access_key=$2 WHERE token_hash=$1", digest(token), digest(canonical))
	assertPGPAccess(e, userID, token, false)
}

func TestPGPAccessIsRequiredForDisputeOpeningAndReplies(t *testing.T) {
	w := newDisputeOrderWorld(t)
	e := w.e
	buyerID, buyer, vendorID := w.buyerID, w.buyer, w.vendorID
	orderID := w.fundedFor(w.buyerID, w.buyer, w.vendorID, w.vendor)
	// Model a legacy buyer missing a key after this existing order was funded.
	agExec(e, "UPDATE users SET pgp='' WHERE id=$1", buyerID)
	agExec(e, "UPDATE sessions SET pgp_access_key='' WHERE user_id=$1", buyerID)
	reason := "This order was not delivered as described."
	agExpect(e, "/disputes", buyer, url.Values{"order_id": {orderID}, "reason": {reason}}, 403, "This browser has not completed PGP authentication")
	if agStr(e, "SELECT state FROM orders WHERE id=$1", orderID) != statePaid || agInt(e, "SELECT count(*) FROM disputes WHERE order_id=$1", orderID) != 0 {
		t.Fatal("unauthenticated dispute opening changed the order")
	}
	buyerKey, buyerPublic := testPGPKey(t, "dispute-access-buyer")
	e.check(e.do("POST", "/account", buyer, url.Values{"pgp": {buyerPublic}, "password": {testPassword}}), 303)
	proveAccess(e, buyerID, buyer, buyerKey)
	e.check(e.do("POST", "/disputes", buyer, url.Values{"order_id": {orderID}, "reason": {reason}}), 303)

	vendorKey, vendorPublic := testPGPKey(t, "dispute-access-vendor")
	e.check(e.do("POST", "/account", w.vendor, url.Values{"pgp": {vendorPublic}, "password": {testPassword}}), 303)
	vendor := agSession(e, vendorID)
	reply := "I can provide the delivery record for this order."
	agExpect(e, "/disputes/reply", vendor, url.Values{"order_id": {orderID}, "reply": {reply}}, 403, "This browser has not completed PGP authentication")
	proveAccess(e, vendorID, vendor, vendorKey)
	e.check(e.do("POST", "/disputes/reply", vendor, url.Values{"order_id": {orderID}, "reply": {reply}}), 303)
}

func TestPGPAccessIsRequiredAtUnfundedDraftMutation(t *testing.T) {
	e := newTestApp(t)
	buyerID, buyer := e.keylessUser("access_purchase", "buyer")
	vendorID, _ := e.user("access_purchase_vendor", "vendor")
	productID := e.product(vendorID, "digital")
	orderForm := url.Values{"product_id": {productID}, "currency": {"XMR"}}
	agExpect(e, "/orders", buyer, orderForm, 403, "This browser has not completed PGP authentication")
	if agInt(e, "SELECT count(*) FROM orders WHERE buyer_id=$1", buyerID) != 0 {
		t.Fatal("a browse-only account created an order")
	}
	key, public := testPGPKey(t, "access-purchase")
	e.check(e.do("POST", "/account", buyer, url.Values{"pgp": {public}, "password": {testPassword}}), 303)
	agExpect(e, "/orders", buyer, orderForm, 403, "This browser has not completed PGP authentication")
	proveAccess(e, buyerID, buyer, key)
	e.check(e.do("POST", "/orders", buyer, orderForm), 303)
	if agInt(e, "SELECT count(*) FROM orders WHERE buyer_id=$1 AND state='draft'", buyerID) != 1 {
		t.Fatal("a PGP-authenticated buyer could not create its draft")
	}
}

func TestPGPStrictPendingCodeDoesNotReviveWhenUnverifiedKeyIsRestored(t *testing.T) {
	e := newTestApp(t)
	userID, token := e.keylessUser("access_restore_pending", "buyer")
	key, public := testPGPKey(t, "access-restore-pending")
	_, replacement := testPGPKey(t, "access-restore-other")
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {public}, "password": {testPassword}}), 303)
	// Arrange a genuinely preexisting authenticator; this test isolates pending
	// code generations, while first-enrollment bootstrap is covered separately.
	totp, _ := agEnableTOTP(e, userID)
	step := uint64(totpStep(time.Now()))
	agExec(e, "UPDATE settings SET value='false' WHERE key=$1", settingAllowLoginWithoutPGP)
	anon := randomToken()
	w := e.do("POST", "/login", anon, url.Values{"handle": {"access_restore_pending"}, "password": {testPassword}})
	e.check(w, 303)
	pending := &http.Cookie{Name: "pending", Value: e.cookie(w, "pending")}
	e.check(e.do("POST", "/challenge/pgp", anon, nil, pending), 303)
	oldCode := testSignInCode(t, key, agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(pending.Value)))
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {replacement}, "password": {testPassword}, "code": {hotp(totp, step, totpDigits)}}), 303)
	e.check(e.do("POST", "/account", token, url.Values{"pgp": {public}, "password": {testPassword}, "code": {hotp(totp, step+1, totpDigits)}}), 303)
	agExpect(e, "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {oldCode}}, 401, "does not match", pending)
	assertPGPAccess(e, userID, token, false)

	e.check(e.do("POST", "/challenge/pgp", anon, nil, pending), 303)
	newCode := testSignInCode(t, key, agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(pending.Value)))
	w = e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {newCode}}, pending)
	e.check(w, 303)
	assertPGPAccess(e, userID, e.session(w), true)
}

// strictRecoveryLine is how a strict-policy refusal names replacement-key recovery (A-702).
const strictRecoveryLine = "renew your key in your own PGP software (gpg --quick-set-expire keeps the same fingerprint) or, if its private key is lost or none is saved, make a new key, and send the public key to the market staff"

// TestStrictLoginExpiredOrMissingKeyRefusalNamesStaffContact (A-702): on a market that requires PGP at every sign-in,
// a correct password for an account without a usable key (none saved, or it expired) is refused with 409, no session
// and no pending sign-in, and the refusal names replacement-key recovery and the configured staff contact (or the
// operator while none is set), with what to have ready.
func TestStrictLoginExpiredOrMissingKeyRefusalNamesStaffContact(t *testing.T) {
	e := newTestApp(t)
	e.keylessUser("a702_nokey", "buyer")
	expiredID, _ := e.user("a702_expiredkey", "buyer")
	created := time.Now().Add(-4 * time.Hour).Truncate(time.Second)
	key := lifecycleKey(t, "a702_expiredkey", created, 0, 0)
	renewKey(t, key, created.Add(time.Hour), 3*3600-1800, 0) // expired an hour and a half ago
	e.setKey(expiredID, armoredPublicKey(t, key))
	agExec(e, "UPDATE settings SET value='false' WHERE key=$1", settingAllowLoginWithoutPGP)
	refusal := func(handle string) string {
		t.Helper()
		w := e.do("POST", "/login", randomToken(), url.Values{"handle": {handle}, "password": {testPassword}})
		if w.Code != 409 || e.cookie(w, "session") != "" || e.cookie(w, "pending") != "" {
			t.Fatalf("strict sign-in of %s without a usable key: %d %q", handle, w.Code, w.Body.String())
		}
		return refusalText(w.Body.String())
	}
	for _, handle := range []string{"a702_nokey", "a702_expiredkey"} {
		agExec(e, "DELETE FROM settings WHERE key='staff_contact'")
		b := refusal(handle)
		mustContain(t, b, "This market requires PGP decryption at every sign-in.", "cannot replace it", strictRecoveryLine, askOperatorLine+" "+staffReadyLine)
		mustNotContain(t, b, "Contact the market staff:", "contact market staff for account recovery")
		agExec(e, "INSERT INTO settings(key,value) VALUES('staff_contact',$1)", staffContactValue)
		b = refusal(handle)
		mustContain(t, b, strictRecoveryLine, "Contact the market staff: "+staffContactValue+". "+staffReadyLine)
		mustNotContain(t, b, askOperatorLine, staffNewAccountLine)
	}
}
