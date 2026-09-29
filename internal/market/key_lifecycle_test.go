package market

import (
	"context"
	"html"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// A-155: the owner sees when their key and its encryption subkey expire, is told once before and once after
// encryption to it stops, and can renew the same key (same fingerprint, new signatures) without losing its ownership
// proof or PGP sign-in, unless the update drops a revocation or rolls back the encryption subkey. A-225: where the
// owner manages the key they are told when nothing can be encrypted to it.

const (
	keyRenewedNotice   = "ownership proof and PGP sign-in were kept"
	keyClearedAlert    = "saved as a changed key, not a renewal"
	keyRevocationWhy   = "missing a revocation signature"
	keySubkeyWhy       = "older encryption subkey"
	keyCannotEncrypt   = "Buyers cannot encrypt to this key"
	keyRenewCommand    = "gpg --quick-set-expire"
	keyNoOtherMethod   = "only verification method"
	keyExpiredRefusal  = "A sign-in code cannot be encrypted to your key"
	keyExpiryNoteSoon  = "Encryption to your PGP key stops on "
	keyExpiryNoteAfter = "Encryption to your PGP key stopped on "
)

// lifecycleKey generates an Ed25519/Curve25519 key created at created whose primary key and encryption subkey have
// the given lifetimes (0: never expires).
func lifecycleKey(t *testing.T, name string, created time.Time, primaryLife, subLife uint32) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity(name, "", name+"@example.test", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Time: func() time.Time { return created }, KeyLifetimeSecs: primaryLife})
	if err != nil {
		t.Fatal(err)
	}
	if subLife != 0 {
		sub := &e.Subkeys[0]
		sub.Sig.KeyLifetimeSecs = &subLife
		if err = sub.Sig.SignKey(sub.PublicKey, e.PrivateKey, &packet.Config{Time: func() time.Time { return created }}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// renewKey adds new self-signatures made at "at" giving the primary key and every subkey new lifetimes, as
// gpg --quick-set-expire does: the fingerprint is unchanged and the newest self-signatures win.
func renewKey(t *testing.T, e *openpgp.Entity, at time.Time, primaryLife, subLife uint32) {
	t.Helper()
	cfg := &packet.Config{Time: func() time.Time { return at }}
	for name, id := range e.Identities {
		sig := *id.SelfSignature
		sig.CreationTime, sig.KeyLifetimeSecs = at, &primaryLife
		if err := sig.SignUserId(name, e.PrimaryKey, e.PrivateKey, cfg); err != nil {
			t.Fatal(err)
		}
		id.Signatures = append(id.Signatures, &sig)
		id.SelfSignature = &sig
	}
	for i := range e.Subkeys {
		sub := &e.Subkeys[i]
		sig := *sub.Sig
		sig.CreationTime, sig.KeyLifetimeSecs = at, &subLife
		if err := sig.SignKey(sub.PublicKey, e.PrivateKey, cfg); err != nil {
			t.Fatal(err)
		}
		sub.Sig = &sig
	}
}

// provePGP saves armored with the password, proves ownership with a signing challenge and turns PGP sign-in on.
func provePGP(e *testEnv, uid, sess string, key *openpgp.Entity, armored string) {
	e.t.Helper()
	e.check(e.do("POST", "/account", sess, url.Values{"pgp": {armored}, "password": {testPassword}}), 303)
	e.check(e.do("POST", "/pgp/challenge", sess, url.Values{"kind": {"sign"}}), 303)
	challenge := agStr(e, "SELECT challenge FROM pgp_challenges WHERE user_id=$1", uid)
	e.check(e.do("POST", "/pgp/verify", sess, url.Values{"signature": {testClearsign(e.t, key, challenge)}}), 303)
	e.check(e.do("POST", "/pgp/2fa", sess, url.Values{"enable": {"1"}, "password": {testPassword}}), 303)
	if keyProof(e, uid) != "verified 2fa" {
		e.t.Fatal("PGP sign-in not set up")
	}
}

// keyProof reports the ownership proof and PGP sign-in state of uid's saved key.
func keyProof(e *testEnv, uid string) string {
	e.t.Helper()
	return agStr(e, "SELECT CASE WHEN pgp_verified_at IS NULL THEN 'unverified' ELSE 'verified' END || CASE WHEN pgp_2fa THEN ' 2fa' ELSE '' END FROM users WHERE id=$1", uid)
}

func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// TestKeyExpiryShownAndRenewalKeepsSignIn: /account and /pgp show both expiry dates; the same key renewed, with an
// extra user ID or with a third-party certification, needs the password (A-111 pins) and then keeps the proof and PGP
// sign-in, with a notice that says so and one owner notification.
func TestKeyExpiryShownAndRenewalKeepsSignIn(t *testing.T) {
	e := newTestApp(t)
	uid, sess := e.user("a155_renew", "vendor")
	created := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	// Three minutes from expiry (A-155): the primary key at +2h3m, the encryption subkey at +2h5m.
	key := lifecycleKey(t, "a155_renew", created, 2*3600+180, 2*3600+300)
	provePGP(e, uid, sess, key, armoredPublicKey(t, key))
	fp := agStr(e, "SELECT pgp_fingerprint FROM users WHERE id=$1", uid)
	primaryDay, subDay := utcDay(created.Add((2*3600+180)*time.Second)), utcDay(created.Add((2*3600+300)*time.Second))
	for _, page := range []string{"/account", "/pgp"} {
		b := html.UnescapeString(e.body("GET", page, sess, nil, 200))
		mustContain(t, b, "Primary key expires", primaryDay+" UTC", "Encryption subkey expires", subDay+" UTC", "expires within 30 days", keyRenewCommand)
		mustNotContain(t, b, keyCannotEncrypt)
	}

	// Renewed for two years (same fingerprint): a session alone cannot save it; with the password it keeps everything.
	renewKey(t, key, time.Now().Truncate(time.Second), 2*365*86400, 2*365*86400)
	renewed := armoredPublicKey(t, key)
	agExpect(e, "/account", sess, url.Values{"pgp": {renewed}}, 400, "Enter your current password")
	if keyProof(e, uid) != "verified 2fa" || sameProfileKey(agStr(e, "SELECT pgp FROM users WHERE id=$1", uid), renewed) {
		t.Fatal("renewed key saved, or proof changed, by a session alone")
	}
	notes, changed := a152Notes(e, uid, ""), agStr(e, "SELECT pgp_changed_at::text FROM users WHERE id=$1", uid)
	w := e.do("POST", "/account", sess, url.Values{"pgp": {renewed}, "password": {testPassword}})
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/account?key=renewed" {
		t.Fatalf("renewal redirected to %q", loc)
	}
	if keyProof(e, uid) != "verified 2fa" || !sameProfileKey(agStr(e, "SELECT pgp FROM users WHERE id=$1", uid), renewed) {
		t.Fatalf("renewal lost the proof or was not stored: %s", keyProof(e, uid))
	}
	if agStr(e, "SELECT pgp_changed_at::text FROM users WHERE id=$1", uid) != changed {
		t.Fatal("a renewal that keeps the proof was recorded as a key change (A-158)")
	}
	if !e.auditExact(uid, "Updated PGP key (same fingerprint "+fp+"; ownership proof and PGP sign-in kept) (confirmed with password)") {
		t.Fatal("renewal not audited")
	}
	if a152Notes(e, uid, "") != notes+1 || a152Notes(e, uid, "renewed expiry") != 1 || a152Notes(e, uid, fp) != 0 {
		t.Fatal("renewal not notified exactly once, or the notification names the fingerprint")
	}
	b := html.UnescapeString(e.body("GET", "/account?key=renewed", sess, nil, 200))
	mustContain(t, b, keyRenewedNotice, utcDay(created.Add(2*365*86400*time.Second))+" UTC")
	mustNotContain(t, b, "Changes saved.", "expires within 30 days")

	// A-111: the same key with an extra user ID, and with a third-party certification, while PGP sign-in is on.
	if err := key.AddUserId("a155 second", "", "second@example.test", nil); err != nil {
		t.Fatal(err)
	}
	bob, _ := testPGPKey(t, "bob")
	if err := key.SignIdentity("a155_renew <a155_renew@example.test>", bob, nil); err != nil {
		t.Fatal(err)
	}
	extra := armoredPublicKey(t, key)
	agExpect(e, "/account", sess, url.Values{"pgp": {extra}}, 400, "Enter your current password")
	if sameProfileKey(agStr(e, "SELECT pgp FROM users WHERE id=$1", uid), extra) {
		t.Fatal("key with an extra user ID and certification saved by a session alone")
	}
	e.check(e.do("POST", "/account", sess, url.Values{"pgp": {extra}, "password": {testPassword}}), 303)
	if keyProof(e, uid) != "verified 2fa" || !strings.Contains(e.body("GET", "/account", sess, nil, 200), "second@example.test") {
		t.Fatal("same key with an extra user ID and certification lost the proof or was not stored")
	}

	// PGP sign-in still works with the renewed key.
	anon, pc := e.pendingSignIn("a155_renew")
	e.check(e.do("POST", "/challenge/pgp", anon, nil, pc), 303)
}

// twoSubkeyKey returns a key whose first encryption subkey (created at "created") is older than its second (created
// an hour later), so encryptTo selects the second.
func twoSubkeyKey(t *testing.T, name string, created time.Time) *openpgp.Entity {
	t.Helper()
	key := lifecycleKey(t, name, created, 0, 0)
	later := created.Add(time.Hour)
	if err := key.AddEncryptionSubkey(&packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Time: func() time.Time { return later }}); err != nil {
		t.Fatal(err)
	}
	return key
}

// TestKeyUpdateThatDropsRevocationOrRollsBackSubkeyClearsProof: a same-fingerprint update without a revocation the
// saved key had, or one that would make others encrypt to an older subkey, is saved as a changed key: the proof and
// PGP sign-in are cleared, the change time is recorded (A-158) and the owner gets an explicit alert, never plain
// "Changes saved.".
func TestKeyUpdateThatDropsRevocationOrRollsBackSubkeyClearsProof(t *testing.T) {
	e := newTestApp(t)
	created := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name, why string
		stored    func(*openpgp.Entity) string
		pasted    func(*openpgp.Entity) string
	}{
		{"revocation", keyRevocationWhy,
			func(k *openpgp.Entity) string {
				if err := k.RevokeSubkey(&k.Subkeys[0], packet.KeyCompromised, "lost", nil); err != nil {
					t.Fatal(err)
				}
				return armoredPublicKey(t, k)
			},
			func(k *openpgp.Entity) string {
				revs := k.Subkeys[0].Revocations
				k.Subkeys[0].Revocations = nil
				defer func() { k.Subkeys[0].Revocations = revs }()
				return armoredPublicKey(t, k)
			}},
		{"subkey", keySubkeyWhy,
			func(k *openpgp.Entity) string { return armoredPublicKey(t, k) },
			func(k *openpgp.Entity) string {
				all := k.Subkeys
				k.Subkeys = all[:1]
				defer func() { k.Subkeys = all }()
				return armoredPublicKey(t, k)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid, sess := e.user("a155_"+tc.name, "vendor")
			key := twoSubkeyKey(t, "a155_"+tc.name, created)
			provePGP(e, uid, sess, key, tc.stored(key))
			pasted := tc.pasted(key)
			if _, _, err := parsePublicKey(pasted); err != nil {
				t.Fatalf("the pasted variant does not parse: %v", err)
			}
			notes := a152Notes(e, uid, "")
			w := e.do("POST", "/account", sess, url.Values{"pgp": {pasted}, "password": {testPassword}})
			e.check(w, 303)
			loc := w.Header().Get("Location")
			if loc != "/account?key="+tc.name {
				t.Fatalf("redirected to %q", loc)
			}
			if keyProof(e, uid) != "unverified" || agStr(e, "SELECT (pgp_changed_at IS NOT NULL)::text FROM users WHERE id=$1", uid) != "true" {
				t.Fatalf("proof kept or change time not recorded: %s", keyProof(e, uid))
			}
			if !sameProfileKey(agStr(e, "SELECT pgp FROM users WHERE id=$1", uid), pasted) {
				t.Fatal("pasted key not stored")
			}
			if a152Notes(e, uid, "") != notes+1 || a152Notes(e, uid, "ownership proof and PGP sign-in verification were cleared") != 1 {
				t.Fatal("clearing not notified exactly once")
			}
			b := html.UnescapeString(e.body("GET", loc, sess, nil, 200))
			mustContain(t, b, `role="alert"`, keyClearedAlert, tc.why)
			mustNotContain(t, b, "Changes saved.")
		})
	}
}

// TestKeyExpiryNotifiedOnce: the daily sweep notifies once when encryption to the key stops within 30 days and once
// when it has stopped; a renewal re-arms both; keys far from expiry, never expiring or unusable get nothing.
func TestKeyExpiryNotifiedOnce(t *testing.T) {
	e := newTestApp(t)
	now := time.Now().Truncate(time.Second)
	created := now.Add(-time.Hour)
	soonID, _ := e.user("a155_soon", "vendor")
	farID, _ := e.user("a155_far", "vendor")
	neverID, _ := e.user("a155_never", "buyer")
	signOnlyID, _ := e.user("a155_signonly", "buyer")
	soonKey := lifecycleKey(t, "a155_soon", created, 3600+10*86400, 0) // encryption stops in 10 days
	e.setKey(soonID, armoredPublicKey(t, soonKey))
	e.setKey(farID, armoredPublicKey(t, lifecycleKey(t, "a155_far", created, 0, 3600+90*86400)))
	e.setKey(neverID, armoredPublicKey(t, lifecycleKey(t, "a155_never", created, 0, 0)))
	e.setKey(signOnlyID, agSignOnlyKey(t))
	sweep := func(at time.Time) {
		t.Helper()
		if err := e.A.sweepKeyExpiry(context.Background(), at); err != nil {
			t.Fatal(err)
		}
	}
	stopDay := utcDay(created.Add((3600 + 10*86400) * time.Second))
	fp := agStr(e, "SELECT pgp_fingerprint FROM users WHERE id=$1", soonID)

	sweep(now)
	sweep(now)
	sweep(now.Add(24 * time.Hour))
	if a152Notes(e, soonID, "") != 1 || a152Notes(e, soonID, keyExpiryNoteSoon+stopDay+" UTC") != 1 || a152Notes(e, soonID, keyRenewCommand) != 1 {
		t.Fatalf("near-expiry notice not sent exactly once: %d", a152Notes(e, soonID, ""))
	}
	if a152Notes(e, soonID, fp) != 0 {
		t.Fatal("the expiry notice names the fingerprint")
	}
	for _, id := range []string{farID, neverID, signOnlyID} {
		if a152Notes(e, id, "") != 0 {
			t.Fatal("a key far from expiry, never expiring or unable to encrypt was notified")
		}
	}

	sweep(now.Add(11 * 24 * time.Hour))
	sweep(now.Add(12 * 24 * time.Hour))
	if a152Notes(e, soonID, "") != 2 || a152Notes(e, soonID, keyExpiryNoteAfter+stopDay+" UTC") != 1 {
		t.Fatalf("expired notice not sent exactly once: %d", a152Notes(e, soonID, ""))
	}

	// Renewed for a year: the next expiry is notified again, once.
	renewKey(t, soonKey, now, 3600+365*86400, 0)
	e.setKey(soonID, armoredPublicKey(t, soonKey))
	sweep(now.Add(12 * 24 * time.Hour))
	if a152Notes(e, soonID, "") != 2 {
		t.Fatal("renewed key far from expiry notified")
	}
	sweep(now.Add(350 * 24 * time.Hour))
	sweep(now.Add(351 * 24 * time.Hour))
	if a152Notes(e, soonID, keyExpiryNoteSoon) != 2 {
		t.Fatal("the renewed key's expiry not notified once")
	}
}

// TestKeyExpirySweepRunsInBackground: the sweep runs when the app starts.
func TestKeyExpirySweepRunsInBackground(t *testing.T) {
	e := newTestApp(t)
	uid, _ := e.user("a155_bg", "buyer")
	e.setKey(uid, armoredPublicKey(t, lifecycleKey(t, "a155_bg", time.Now().Add(-time.Hour), 3600+5*86400, 0)))
	e.A.Start(context.Background())
	deadline := time.Now().Add(10 * time.Second)
	for a152Notes(e, uid, keyExpiryNoteSoon) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the background sweep did not send the near-expiry notice")
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.A.stop()
	e.A.background.Wait()
	if a152Notes(e, uid, "") != 1 {
		t.Fatal("the background sweep notified more than once")
	}
}

// TestExpiredKeySignInPointsToLostFactorHelp: after the key expires, /challenge/pgp says why and what the account
// can still do: another method when one is enrolled, otherwise the administrator reset; and A-225's warning shows
// where the owner manages the key.
func TestExpiredKeySignInPointsToLostFactorHelp(t *testing.T) {
	e := newTestApp(t)
	for _, withTOTP := range []bool{false, true} {
		handle := "a155_expired"
		if withTOTP {
			handle += "_totp"
		}
		uid, sess := e.user(handle, "vendor")
		created := time.Now().Add(-4 * time.Hour).Truncate(time.Second)
		key := lifecycleKey(t, handle, created, 0, 0)
		provePGP(e, uid, sess, key, armoredPublicKey(t, key))
		// The same key, expired an hour and a half ago (a newer self-signature with a short lifetime).
		renewKey(t, key, created.Add(time.Hour), 3*3600-1800, 0)
		agExec(e, "UPDATE users SET pgp=$2 WHERE id=$1", uid, armoredPublicKey(t, key))
		if withTOTP {
			agEnableTOTP(e, uid)
		}
		for _, page := range []string{"/account", "/pgp"} {
			mustContain(t, html.UnescapeString(e.body("GET", page, sess, nil, 200)), keyCannotEncrypt, "Expired "+utcDay(created.Add(2*time.Hour+30*time.Minute))+" UTC")
		}
		anon, pc := e.pendingSignIn(handle)
		b := e.body("POST", "/challenge/pgp", anon, nil, 409, pc)
		mustContain(t, b, keyExpiredRefusal)
		mustNotContain(t, b, "Use another verification method")
		if withTOTP {
			mustContain(t, b, "authenticator code or a recovery code")
			mustNotContain(t, b, keyNoOtherMethod)
		} else {
			mustContain(t, b, keyNoOtherMethod, "administrator", "Reset a user's second factors")
		}
	}
}
