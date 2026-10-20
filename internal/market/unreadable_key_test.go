package market

import (
	"context"
	"encoding/hex"
	"html"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// A-266: a key whose user IDs make the OpenPGP library's identity helpers dereference nil (go-crypto v1.4.1
// shouldPreferIdentity: a revoked user ID that keeps its self-signature next to revoked user IDs carrying only their
// revocation) is refused when saved; one already stored (an older database) is unreadable everywhere, so it cannot
// crash the expiry sweep, the process or a page.

const keyUserIDsRefusal = "a revoked user ID in the key has only its revocation, without its self-signature"

// mixedRevokedUserIDsKey returns such a key, armored, with its fingerprint: the primary user ID revoked with its
// self-signature kept, and four more user IDs each with only their revocation.
func mixedRevokedUserIDsKey(t *testing.T, name string) (string, string) {
	t.Helper()
	k, _ := testPGPKey(t, name)
	revokeUserID(t, k, name+" <"+name+"@example.test>")
	for _, extra := range []string{"b", "c", "d", "e"} {
		uid := name + " " + extra + " <" + extra + "@example.test>"
		if err := k.AddUserId(name+" "+extra, "", extra+"@example.test", nil); err != nil {
			t.Fatal(err)
		}
		revokeUserID(t, k, uid)
		id := k.Identities[uid]
		id.Signatures = id.Revocations // drop its self-signature
	}
	return armoredPublicKey(t, k), strings.ToUpper(hex.EncodeToString(k.PrimaryKey.Fingerprint))
}

// minimalExportKey returns a key as gpg --export-options export-minimal writes it after gpg --quick-revoke-uid: the
// revoked user ID carries only its revocation, the primary user ID its self-signature.
func minimalExportKey(t *testing.T, name string) (*openpgp.Entity, string) {
	t.Helper()
	k, _ := testPGPKey(t, name)
	uid := name + " second <second@example.test>"
	if err := k.AddUserId(name+" second", "", "second@example.test", nil); err != nil {
		t.Fatal(err)
	}
	revokeUserID(t, k, uid)
	k.Identities[uid].Signatures = k.Identities[uid].Revocations
	return k, armoredPublicKey(t, k)
}

// storeKeyBySQL stores armored as userID's verified key with PGP sign-in on, as an older database could hold it.
func storeKeyBySQL(e *testEnv, userID, armored, fp string) {
	e.t.Helper()
	agExec(e, "UPDATE users SET pgp=$2,pgp_fingerprint=$3,pgp_verified_at=now(),pgp_2fa=true WHERE id=$1", userID, armored, fp)
}

// TestKeyWithUnreadableUserIDsRefused: /account refuses the key with a 400 naming the problem and stores nothing; a
// minimal export with a revoked user ID (real gpg output) is still saved and usable.
func TestKeyWithUnreadableUserIDsRefused(t *testing.T) {
	e := newTestApp(t)
	uid, sess := e.keylessUser("a266_save", "buyer")
	armored, _ := mixedRevokedUserIDsKey(t, "a266_save")
	agExpect(e, "/account", sess, url.Values{"pgp": {armored}, "password": {testPassword}}, 400, keyUserIDsRefusal)
	if agStr(e, "SELECT pgp||'/'||pgp_fingerprint FROM users WHERE id=$1", uid) != "/" {
		t.Fatal("the refused key was stored")
	}

	minimal, pub := minimalExportKey(t, "a266_minimal")
	e.check(e.do("POST", "/account", sess, url.Values{"pgp": {pub}, "password": {testPassword}}), 303)
	if !sameProfileKey(agStr(e, "SELECT pgp FROM users WHERE id=$1", uid), pub) {
		t.Fatal("a minimal export with a revoked user ID was not saved")
	}
	key, _, err := parsePublicKey(pub)
	if err != nil || !canReceiveEncrypted(key, time.Now()) || key.PrimaryKey.KeyId != minimal.PrimaryKey.KeyId {
		t.Fatalf("a minimal export with a revoked user ID is not usable: %v", err)
	}
}

// TestKeyExpirySweepSkipsUnreadableStoredKey: a sweep over the key stored directly (an older database) completes, logs
// a fixed line without key material and still notifies the other owners.
func TestKeyExpirySweepSkipsUnreadableStoredKey(t *testing.T) {
	e := newTestApp(t)
	logs := captureLog(t)
	now := time.Now().Truncate(time.Second)
	badID, _ := e.user("a266_sweep_bad", "vendor")
	armored, fp := mixedRevokedUserIDsKey(t, "a266_sweep_bad")
	storeKeyBySQL(e, badID, armored, fp)
	okID, _ := e.user("a266_sweep_ok", "vendor")
	e.setKey(okID, armoredPublicKey(t, lifecycleKey(t, "a266_sweep_ok", now.Add(-time.Hour), 3600+10*86400, 0)))
	if err := e.A.sweepKeyExpiry(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if a152Notes(e, okID, keyExpiryNoteSoon) != 1 || a152Notes(e, badID, "") != 0 {
		t.Fatal("the sweep did not notify the other owner exactly once, or notified the unreadable key's owner")
	}
	skipped := logs.lines("pgp expiry sweep: skipped one key that could not be read")
	if len(skipped) != 1 {
		t.Fatalf("skip not logged once: %q", logs.String())
	}
	for _, leak := range []string{fp, "a266_sweep_bad", badID} {
		if strings.Contains(logs.String(), leak) {
			t.Fatal("the sweep log names the key or its owner")
		}
	}
}

// TestUnreadableStoredKeyPagesRender: with the key stored directly, the owner's pages and the pages that show it to
// others answer normally and treat it as unreadable.
func TestUnreadableStoredKeyPagesRender(t *testing.T) {
	e := newTestApp(t)
	buyerID, buyer := e.user("a266_buyer", "buyer")
	vendorID, vendor := e.user("a266_vendor", "vendor")
	armored, fp := mixedRevokedUserIDsKey(t, "a266_vendor")
	storeKeyBySQL(e, vendorID, armored, fp)
	paid := e.order(buyerID, e.product(vendorID, "physical"), "XMR", statePaid)
	for name, page := range map[string]string{
		"vendor page": e.body("GET", "/vendor?id="+vendorID, "", nil, 200),
		"messages":    e.body("GET", "/messages?to=a266_vendor", buyer, nil, 200),
		"order":       e.body("GET", "/order?id="+paid, buyer, nil, 200),
	} {
		page = html.UnescapeString(page)
		if keys := shownKeys(t, page); len(keys) != 0 {
			t.Fatalf("%s: the unreadable key is offered as a contact key", name)
		}
		mustContain(t, page, "No usable key.", "can no longer be read")
		mustNotContain(t, page, formatFingerprint(fp))
	}
	mustContain(t, html.UnescapeString(e.body("GET", "/account", vendor, nil, 200)), "Unreadable — use verified key recovery")
	mustContain(t, html.UnescapeString(e.body("GET", "/pgp", vendor, nil, 200)), "The saved key cannot be used: "+keyUserIDsRefusal)
}

// TestProofSignedBySubkeyOfKeyWithoutUserIDSelfSignatureRefused: a key whose every user ID is revoked and carries only
// its revocation (a minimal export) can be saved, but a proof signed by its signing subkey made the OpenPGP library
// dereference a nil primary self-signature; it is answered as a signature that does not verify.
func TestProofSignedBySubkeyOfKeyWithoutUserIDSelfSignatureRefused(t *testing.T) {
	e := newTestApp(t)
	uid, sess := e.keylessUser("a266_proof", "buyer")
	k, _ := testPGPKey(t, "a266_proof")
	if err := k.AddSigningSubkey(nil); err != nil {
		t.Fatal(err)
	}
	for name, id := range k.Identities {
		revokeUserID(t, k, name)
		id.Signatures = id.Revocations
	}
	e.check(e.do("POST", "/account", sess, url.Values{"pgp": {armoredPublicKey(t, k)}, "password": {testPassword}}), 303)
	e.check(e.do("POST", "/pgp/challenge", sess, url.Values{"kind": {"sign"}}), 303)
	challenge := agStr(e, "SELECT challenge FROM pgp_challenges WHERE user_id=$1", uid)
	signer := *k
	signer.PrivateKey = k.Subkeys[len(k.Subkeys)-1].PrivateKey // clearsign with the signing subkey
	agExpect(e, "/pgp/verify", sess, url.Values{"signature": {testClearsign(t, &signer, challenge)}}, 400, "Not verified")
	if keyProof(e, uid) != "unverified" {
		t.Fatal("proof recorded")
	}
}
