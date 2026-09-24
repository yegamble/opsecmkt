package market

import (
	"bytes"
	"database/sql"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

func (e *testEnv) auditExact(userID, action string) bool {
	e.t.Helper()
	var n int
	if err := e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", userID, action).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n > 0
}

func (e *testEnv) body(method, path, token string, form url.Values, want int, extra ...*http.Cookie) string {
	e.t.Helper()
	w := e.do(method, path, token, form, extra...)
	e.check(w, want)
	return w.Body.String()
}

func TestPGPKeyOwnershipAndSecondFactor(t *testing.T) {
	e := newTestApp(t)
	alice, alicePub := testPGPKey(t, "alice")
	mallory, _ := testPGPKey(t, "mallory")
	bob, bobPub := testPGPKey(t, "bob")
	_, aliceFP, _ := parsePublicKey(alicePub)
	_, bobFP, _ := parsePublicKey(bobPub)
	uid, s := e.user("alice_pgp", "buyer")
	status := func() (fp string, verified, twoFA bool) {
		t.Helper()
		if err := e.DB.QueryRow("SELECT pgp_fingerprint,pgp_verified_at IS NOT NULL,pgp_2fa FROM users WHERE id=$1", uid).Scan(&fp, &verified, &twoFA); err != nil {
			t.Fatal(err)
		}
		return
	}

	// Key parsing: garbage, private keys and keyrings are rejected; nothing is stored.
	var priv bytes.Buffer
	w, _ := armor.Encode(&priv, openpgp.PrivateKeyType, nil)
	alice.SerializePrivate(w, nil)
	w.Close()
	for name, key := range map[string]string{
		"garbage": "-----BEGIN PGP PUBLIC KEY BLOCK-----\nnot a key\n-----END PGP PUBLIC KEY BLOCK-----",
		"private": priv.String(), "two keys": alicePub + "\n" + bobPub,
	} {
		if b := e.body("POST", "/account", s, url.Values{"pgp": {key}}, 400); !strings.Contains(b, "PGP key rejected") {
			t.Errorf("%s: %s", name, b)
		}
	}
	if fp, _, _ := status(); fp != "" {
		t.Fatal("rejected key stored")
	}

	// Saving a key stores its fingerprint, unverified; the page says so and PGP sign-in is refused.
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {alicePub}}), 303)
	if fp, verified, _ := status(); fp != aliceFP || verified {
		t.Fatalf("fp=%s verified=%v", fp, verified)
	}
	if !e.auditExact(uid, "Updated PGP key (fingerprint "+aliceFP+"; ownership unverified)") {
		t.Fatal("key change not audited")
	}
	page := e.body("GET", "/account", s, nil, 200)
	for _, want := range []string{formatFingerprint(aliceFP), "Not verified", "Requires a verified key", `href="/pgp"`} {
		if !strings.Contains(page, want) {
			t.Errorf("account page missing %q", want)
		}
	}
	if strings.Contains(page, ">Verified") {
		t.Fatal("unverified key shown as verified")
	}
	if b := e.body("POST", "/pgp/2fa", s, url.Values{"enable": {"1"}}, 409); !strings.Contains(b, "Verify ownership") {
		t.Fatal(b)
	}
	e.check(e.do("POST", "/pgp/verify", s, url.Values{"signature": {"x"}}), 409)
	e.check(e.do("POST", "/pgp/challenge", s, url.Values{"kind": {"other"}}), 400)

	// Sign challenge: wrong key, wrong text and unsigned text fail; the real signature verifies.
	e.check(e.do("POST", "/pgp/challenge", s, url.Values{"kind": {"sign"}}), 303)
	var challenge string
	if err := e.DB.QueryRow("SELECT challenge FROM pgp_challenges WHERE user_id=$1 AND kind='sign'", uid).Scan(&challenge); err != nil {
		t.Fatal(err)
	}
	if page = e.body("GET", "/pgp", s, nil, 200); !strings.Contains(page, challenge) || !strings.Contains(page, "Unavailable until ownership") {
		t.Fatal("challenge not shown")
	}
	for _, bad := range []string{testClearsign(t, mallory, challenge), testClearsign(t, alice, challenge+"!"), challenge} {
		if b := e.body("POST", "/pgp/verify", s, url.Values{"signature": {bad}}, 400); !strings.Contains(b, "Not verified") {
			t.Fatal(b)
		}
	}
	if _, verified, _ := status(); verified {
		t.Fatal("failed proof marked verified")
	}
	e.check(e.do("POST", "/pgp/verify", s, url.Values{"signature": {testClearsign(t, alice, challenge)}}), 303)
	if _, verified, _ := status(); !verified {
		t.Fatal("valid signature not recorded")
	}
	if !e.auditExact(uid, "Verified PGP key ownership (fingerprint "+aliceFP+", sign challenge)") {
		t.Fatal("verification not audited")
	}
	e.check(e.do("POST", "/pgp/verify", s, url.Values{"signature": {testClearsign(t, alice, challenge)}}), 409) // challenge consumed
	if page = e.body("GET", "/account", s, nil, 200); !strings.Contains(page, "Verified 20") || !strings.Contains(page, "UTC") {
		t.Fatal("account does not show verification date")
	}

	// Re-saving the same key keeps the proof.
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {alicePub + "\n"}, "xmpp": {"alice@example.test"}}), 303)
	if _, verified, _ := status(); !verified {
		t.Fatal("unchanged key lost verification")
	}

	// Decrypt challenge: only the plaintext nonce's hash is stored; a wrong answer fails.
	e.check(e.do("POST", "/pgp/challenge", s, url.Values{"kind": {"decrypt"}}), 303)
	var armored, nonceHash string
	if err := e.DB.QueryRow("SELECT challenge,nonce_hash FROM pgp_challenges WHERE user_id=$1 AND kind='decrypt'", uid).Scan(&armored, &nonceHash); err != nil {
		t.Fatal(err)
	}
	nonce := testDecrypt(t, alice, armored)
	if nonceHash != digest(nonce) || strings.Contains(armored, nonce) {
		t.Fatal("nonce stored in plaintext")
	}
	e.check(e.do("POST", "/pgp/verify", s, url.Values{"response": {"0123456789abcdef0123456789abcdef"}}), 400)
	e.check(e.do("POST", "/pgp/verify", s, url.Values{"response": {" " + nonce + "\n"}}), 303)
	if !e.auditExact(uid, "Verified PGP key ownership (fingerprint "+aliceFP+", decrypt challenge)") {
		t.Fatal("decrypt verification not audited")
	}

	// PGP second factor: enabling requires the verified key; sign-in then needs the decrypted code.
	e.check(e.do("POST", "/pgp/2fa", s, url.Values{"enable": {"1"}}), 303)
	if _, _, twoFA := status(); !twoFA {
		t.Fatal("2fa not enabled")
	}
	if !strings.Contains(e.body("GET", "/account", s, nil, 200), "<dt>PGP sign-in verification</dt>\n<dd>Enabled</dd>") {
		t.Fatal("account does not show PGP sign-in enabled")
	}
	anon := randomToken()
	w2 := e.do("POST", "/login", anon, url.Values{"handle": {"alice_pgp"}, "password": {testPassword}})
	e.check(w2, 303)
	pending := e.cookie(w2, "pending")
	if w2.Header().Get("Location") != "/challenge" || pending == "" || e.cookie(w2, "session") != "" {
		t.Fatal("PGP-enrolled login skipped the challenge")
	}
	pc := &http.Cookie{Name: "pending", Value: pending}
	if page = e.body("GET", "/challenge", anon, nil, 200, pc); !strings.Contains(page, "Create encrypted code") || strings.Contains(page, "BEGIN PGP MESSAGE") {
		t.Fatal("challenge page state")
	}
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {"anything"}}, pc), 401) // no code yet
	e.check(e.do("POST", "/challenge/pgp", anon, nil), 401)                                                     // no pending cookie
	e.check(e.do("POST", "/challenge/pgp", anon, nil, pc), 303)
	var loginCT, loginHash string
	if err := e.DB.QueryRow("SELECT pgp_challenge,pgp_nonce FROM pending_logins WHERE token_hash=$1", digest(pending)).Scan(&loginCT, &loginHash); err != nil {
		t.Fatal(err)
	}
	if page = e.body("GET", "/challenge", anon, nil, 200, pc); !strings.Contains(page, "BEGIN PGP MESSAGE") || !strings.Contains(page, `name="pgp_code"`) {
		t.Fatal("encrypted code not shown")
	}
	code := testDecrypt(t, alice, loginCT)
	if loginHash == code || len(loginHash) != 64 {
		t.Fatal("login code stored in plaintext")
	}
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {"0123456789abcdef0123456789abcdef"}}, pc), 401)
	w2 = e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {code}}, pc)
	e.check(w2, 303)
	e.check(e.do("GET", "/account", e.session(w2), nil), 200)
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {code}}, pc), 401) // single use

	// Changing the key resets verification and 2FA; a challenge for the old key is discarded.
	e.check(e.do("POST", "/pgp/challenge", s, url.Values{"kind": {"sign"}}), 303)
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {bobPub}}), 400) // PGP sign-in on: current password required
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {bobPub}, "password": {testPassword}}), 303)
	if fp, verified, twoFA := status(); fp != bobFP || verified || twoFA {
		t.Fatalf("key change kept proof: fp=%s verified=%v 2fa=%v", fp, verified, twoFA)
	}
	e.check(e.do("POST", "/pgp/verify", s, url.Values{"signature": {testClearsign(t, alice, challenge)}}), 409)
	w2 = e.do("POST", "/login", randomToken(), url.Values{"handle": {"alice_pgp"}, "password": {testPassword}})
	e.check(w2, 303)
	if w2.Header().Get("Location") != "/" {
		t.Fatal("reset 2FA still challenged")
	}
	// A challenge issued for bob's key cannot be answered after the key changes back.
	e.check(e.do("POST", "/pgp/challenge", s, url.Values{"kind": {"decrypt"}}), 303)
	if err := e.DB.QueryRow("SELECT challenge FROM pgp_challenges WHERE user_id=$1", uid).Scan(&armored); err != nil {
		t.Fatal(err)
	}
	bobNonce := testDecrypt(t, bob, armored)
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {alicePub}}), 303)
	e.check(e.do("POST", "/pgp/verify", s, url.Values{"response": {bobNonce}}), 409)

	// Removing the key clears everything.
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {""}}), 303)
	if fp, verified, _ := status(); fp != "" || verified {
		t.Fatal("removed key kept state")
	}
	if !strings.Contains(e.body("GET", "/pgp", s, nil, 200), "No public key saved") {
		t.Fatal("pgp page without key")
	}
	e.check(e.do("POST", "/pgp/challenge", s, url.Values{"kind": {"sign"}}), 409)
}

func TestPGPSecondFactorNeedsVerifiedKey(t *testing.T) {
	e := newTestApp(t)
	_, alicePub := testPGPKey(t, "alice")
	_, fp, _ := parsePublicKey(alicePub)
	uid, _ := e.user("forged_2fa", "buyer")
	// Even if pgp_2fa were set directly, an unverified or mismatched key is not an enrolled factor.
	if _, err := e.DB.Exec("UPDATE users SET pgp=$1,pgp_fingerprint=$2,pgp_2fa=true WHERE id=$3", alicePub, fp, uid); err != nil {
		t.Fatal(err)
	}
	if ok, err := (pgpFactor{}).Enrolled(t.Context(), e.DB, uid); err != nil || ok {
		t.Fatalf("unverified key enrolled: %v %v", ok, err)
	}
	if _, err := e.DB.Exec("UPDATE users SET pgp_verified_at=now(),pgp_fingerprint='0000' WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	if ok, _ := (pgpFactor{}).Enrolled(t.Context(), e.DB, uid); ok {
		t.Fatal("proof for another fingerprint enrolled")
	}
	if _, err := e.DB.Exec("UPDATE users SET pgp_fingerprint=$1 WHERE id=$2", fp, uid); err != nil {
		t.Fatal(err)
	}
	if ok, _ := (pgpFactor{}).Enrolled(t.Context(), e.DB, uid); !ok {
		t.Fatal("verified key with 2FA not enrolled")
	}
	if ok, err := (pgpFactor{}).Enrolled(t.Context(), e.DB, "missing"); err != nil || ok {
		t.Fatal("missing user")
	}
}

func TestMessageEncryptionStatus(t *testing.T) {
	e := newTestApp(t)
	alice, alicePub := testPGPKey(t, "alice")
	bob, _ := testPGPKey(t, "bob")
	senderID, sender := e.user("msg_sender", "buyer")
	aliceID, aliceSession := e.user("msg_alice", "vendor")
	e.user("msg_nokey", "vendor")
	e.check(e.do("POST", "/account", aliceSession, url.Values{"pgp": {alicePub}}), 303)

	toAlice, _ := encryptTo(alice, "for alice")
	toBob, _ := encryptTo(bob, "for bob")
	var signed bytes.Buffer
	w, _ := armor.Encode(&signed, "PGP MESSAGE", nil)
	lit, _ := openpgp.Sign(w, alice, nil, nil)
	lit.Write([]byte("signed plaintext, long enough to pass the length check of the form"))
	lit.Close()
	w.Close()
	fake := "-----BEGIN PGP MESSAGE-----\nSample encrypted armor that is not an OpenPGP packet stream at all.\n-----END PGP MESSAGE-----"
	for name, body := range map[string]string{"plaintext": "hello, this is plaintext and must be rejected by the server", "signed only": signed.String(), "fake armor": fake} {
		if b := e.body("POST", "/messages", sender, url.Values{"recipient": {"msg_alice"}, "body": {body}}, 400); !strings.Contains(b, "not an OpenPGP-encrypted message") && name != "plaintext" {
			t.Errorf("%s: %s", name, b)
		}
	}
	send := func(to, body string) {
		t.Helper()
		e.check(e.do("POST", "/messages", sender, url.Values{"recipient": {to}, "body": {body}}), 303)
	}
	send("msg_alice", toAlice)
	send("msg_alice", toBob)
	send("msg_nokey", toBob)
	rows, err := e.DB.Query("SELECT u.handle,m.body=$2,m.encrypted,m.recipient_match FROM messages m JOIN users u ON u.id=m.recipient_id WHERE m.sender_id=$1", senderID, toAlice)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for rows.Next() {
		var to, match string
		var isAlice bool
		var enc sql.NullBool
		if err = rows.Scan(&to, &isAlice, &enc, &match); err != nil {
			t.Fatal(err)
		}
		if !enc.Valid || !enc.Bool {
			t.Errorf("stored encrypted=%v", enc)
		}
		got[to+"/"+match]++
	}
	rows.Close()
	if got["msg_alice/yes"] != 1 || got["msg_alice/no"] != 1 || got["msg_nokey/unknown"] != 1 || len(got) != 3 {
		t.Fatalf("stored statuses %v", got)
	}
	if !e.auditExact(senderID, "Sent OpenPGP-encrypted message (recipient key match: no)") {
		t.Fatal("audit")
	}

	// Rows from before inspection (encrypted NULL) are inspected at render time.
	for _, body := range []string{fake, toAlice} {
		if _, err = e.DB.Exec("INSERT INTO messages(id,sender_id,recipient_id,body) VALUES($1,$2,$3,$4)", randomToken(), senderID, aliceID, body); err != nil {
			t.Fatal(err)
		}
	}
	page := e.body("GET", "/messages", sender, nil, 200)
	for label, n := range map[string]int{
		"Encrypted (to recipient’s key)":               1,
		"Encrypted (NOT to recipient’s key)":           1,
		"Encrypted (recipient unknown)":                2, // no key + legacy ciphertext
		"Not encrypted (plaintext or invalid OpenPGP)": 1, // legacy fake armor
	} {
		if c := strings.Count(page, label); c != n {
			t.Errorf("%q shown %d times, want %d", label, c, n)
		}
	}
	if c := strings.Count(page, "badge green\">Encrypted"); c != 1 {
		t.Errorf("green encrypted badges: %d", c)
	}
	// The recipient sees the same stored statuses.
	if !strings.Contains(e.body("GET", "/messages", aliceSession, nil, 200), "Encrypted (NOT to recipient’s key)") {
		t.Fatal("recipient view")
	}
}

// A-67: plaintext pasted in or around the armor (a leading fake block, lines inside the base64, text
// after the checksum or after the END line) is refused; an armor header is dropped, never stored.
func TestMessagePlaintextInsideArmorRejected(t *testing.T) {
	e := newTestApp(t)
	alice, alicePub := testPGPKey(t, "alice")
	senderID, sender := e.user("armor_buyer", "buyer")
	_, aliceSession := e.user("armor_vendor", "vendor")
	e.check(e.do("POST", "/account", aliceSession, url.Values{"pgp": {alicePub}}), 303)

	ct, err := encryptTo(alice, strings.Repeat("order notes ", 300))
	if err != nil {
		t.Fatal(err)
	}
	const address = "SHIP TO 12 Elm Street Springfield"
	const begin, end = "-----BEGIN PGP MESSAGE-----\n", "-----END PGP MESSAGE-----"
	block := strings.TrimSpace(ct)
	lines := strings.Split(block, "\n")
	withLine := func(at int, line string) string {
		return strings.Join(append(append(append([]string{}, lines[:at]...), line), lines[at:]...), "\n")
	}
	for name, body := range map[string]string{
		// A fake leading block whose first line has no colon: a lenient decoder skips to the next BEGIN.
		"leading fake block": begin + address + "\n" + ct,
		// Plaintext lines in the middle of the ciphertext, after the encrypted-data packet header.
		"inside base64": withLine(len(lines)/2, address),
		// Text between the checksum and END lines, which a lenient decoder never reads.
		"after checksum": withLine(len(lines)-1, address),
		// Sandwiches: a valid block, plaintext, then another block or a bare END line.
		"block text block": block + "\n\n" + address + "\n\n" + block,
		"block text END":   block + "\n" + address + "\n" + end,
	} {
		b := e.body("POST", "/messages", sender, url.Values{"recipient": {"armor_vendor"}, "body": {body}}, 400)
		if !strings.Contains(b, "not an OpenPGP-encrypted message") {
			t.Errorf("%s: %s", name, b)
		}
	}
	var n int
	if err = e.DB.QueryRow("SELECT count(*) FROM messages WHERE sender_id=$1", senderID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stored %d messages (%v)", n, err)
	}

	// An armor header carrying plaintext: the ciphertext is accepted, the header is not stored.
	e.check(e.do("POST", "/messages", sender, url.Values{"recipient": {"armor_vendor"}, "body": {strings.Replace(ct, begin, begin+"Comment: "+address+"\n", 1)}}), 303)
	var stored string
	if err = e.DB.QueryRow("SELECT body FROM messages WHERE sender_id=$1 AND encrypted AND recipient_match='yes'", senderID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != block {
		t.Fatalf("stored body is not the canonical armor:\n%s", stored)
	}
	page := e.body("GET", "/messages", aliceSession, nil, 200)
	if strings.Contains(page, address) || strings.Count(page, "Encrypted (to recipient’s key)") != 1 {
		t.Fatal("recipient page shows the header text or wrong badges")
	}

	// A sandwich stored as encrypted before this check is re-inspected and no longer badged Encrypted.
	var aliceID string
	if err = e.DB.QueryRow("SELECT id FROM users WHERE handle='armor_vendor'").Scan(&aliceID); err != nil {
		t.Fatal(err)
	}
	if _, err = e.DB.Exec("INSERT INTO messages(id,sender_id,recipient_id,body,encrypted,recipient_match) VALUES($1,$2,$3,$4,true,'yes')", randomToken(), senderID, aliceID, block+"\n"+address+"\n"+end); err != nil {
		t.Fatal(err)
	}
	page = e.body("GET", "/messages", aliceSession, nil, 200)
	if strings.Count(page, "Encrypted (to recipient’s key)") != 1 || strings.Count(page, "Not encrypted (plaintext or invalid OpenPGP)") != 1 {
		t.Fatal("stored sandwich still badged encrypted")
	}
}

// gpgFixtureKey and gpgFixtureMessage were produced by GnuPG 2.2.41: an ed25519/cv25519 key for
// carol@example.test, and `gpg --armor --comment "Ship to: 1 Header Lane" -r carol -R dave --encrypt`
// (carol named, a second recipient hidden).
const gpgFixtureKey = `-----BEGIN PGP PUBLIC KEY BLOCK-----

mDMEarSZCRYJKwYBBAHaRw8BAQdAtbkMhniQPBoMvoGlEhN3Gl6KJR5bcKgBjOzw
lJY5BO60IkNhcm9sIEZpeHR1cmUgPGNhcm9sQGV4YW1wbGUudGVzdD6IkAQTFggA
OBYhBAYHYy1wLSUM8uPU9CvzJcRIvKzlBQJqtJkJAhsDBQsJCAcCBhUKCQgLAgQW
AgMBAh4BAheAAAoJECvzJcRIvKzl80cA/jp5XpR2irExB9VYeDlW4asr2r4V9YQ/
nihF9Xt3QZKiAQCue+Li0SUHtSEl/TAJF0bfWGy/Inz9wOz6G/Z7DsjlBLg4BGq0
mQoSCisGAQQBl1UBBQEBB0CLNHSysMR5/MMjIHk7yB6lvhI+B4W8R0vlL5A3Ox3m
cAMBCAeIeAQYFggAIBYhBAYHYy1wLSUM8uPU9CvzJcRIvKzlBQJqtJkKAhsMAAoJ
ECvzJcRIvKzldEAA/2zy/idAkEDpxqvvlAh3Lks/iVdnGRZs2oxJTk3fOobeAQCZ
mg7g8j1HtPw6z6mXGRwi8jEIge0uzx/1lz+hrcBPBw==
=wtYQ
-----END PGP PUBLIC KEY BLOCK-----
`

const gpgFixtureMessage = `-----BEGIN PGP MESSAGE-----
Comment: Ship to: 1 Header Lane

hF4DYHOX22HHAIgSAQdAuGk3nVvYKuvzZDifAyf/J3MHxJE67K7JqIi/lHGdMTYw
WYrBA2dZcgObB8Hd0L8IRnkZZU38V1gl9HVwEpWyVZLvOa4cNzqI1KMiJ552TF2u
hF4DAAAAAAAAAAASAQdAzEveOC/SCAE+woHUhw031Jpm9DAV5785uxrl0Ub4oTgw
9xlWdD41jLxPxvbYCRlTw2DDTkBH9dbFcYJhiE0/H3MakEOpsBXWuCLXZ1IC2+lo
0nQB1psnjtHfGwWx90306oiOZGyi4hD6uB9MGFEW8sWA6SW48+TeVLGgnYV29eMp
TG/+P0tCYKMB+sD4B9LCY8X6eH774kSpV2786A0Vw5rqxeMnNyg7p0CyQC/pU/BO
Ln7p/Gjem7sonpCykqmKanSeoIi9Gw==
=cYPA
-----END PGP MESSAGE-----
`

// A-67: real OpenPGP output is still accepted - Version/Comment headers, CRLF line endings, several
// and hidden recipients - and stored as a canonical re-armor of the same packets, without headers.
func TestMessageArmorCanonicalised(t *testing.T) {
	e := newTestApp(t)
	alice, alicePub := testPGPKey(t, "alice")
	bob, _ := testPGPKey(t, "bob")
	senderID, sender := e.user("canon_buyer", "buyer")
	_, aliceSession := e.user("canon_alice", "vendor")
	_, carolSession := e.user("canon_carol", "vendor")
	e.check(e.do("POST", "/account", aliceSession, url.Values{"pgp": {alicePub}}), 303)
	e.check(e.do("POST", "/account", carolSession, url.Values{"pgp": {gpgFixtureKey}}), 303)

	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, "PGP MESSAGE", map[string]string{"Version": "GnuPG v2", "Comment": "Ship to: 2 Header Road"})
	pt, err := openpgp.Encrypt(w, []*openpgp.Entity{bob, alice}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(pt, "two recipients")
	pt.Close()
	w.Close()
	crlf := strings.ReplaceAll(buf.String(), "\n", "\r\n")
	packets := func(armored string) string {
		t.Helper()
		b, err := armor.Decode(strings.NewReader(armored))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(b.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	sent := map[string]string{} // recipient/match -> packets of the pasted message
	for _, m := range []struct{ to, body, match string }{
		{"canon_alice", crlf, "yes"},                  // CRLF, Version and Comment headers, two recipients
		{"canon_carol", gpgFixtureMessage, "yes"},     // gpg output naming carol
		{"canon_alice", gpgFixtureMessage, "unknown"}, // only a hidden recipient could be alice
	} {
		e.check(e.do("POST", "/messages", sender, url.Values{"recipient": {m.to}, "body": {m.body}}), 303)
		sent[m.to+"/"+m.match] = packets(m.body)
	}
	rows, err := e.DB.Query("SELECT u.handle,m.body,m.recipient_match FROM messages m JOIN users u ON u.id=m.recipient_id WHERE m.sender_id=$1 AND m.encrypted", senderID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var to, body, match string
		if err = rows.Scan(&to, &body, &match); err != nil {
			t.Fatal(err)
		}
		n++
		if strings.Contains(body, "Header") || strings.Contains(body, "Version") || strings.Contains(body, "\r") || !strings.HasPrefix(body, "-----BEGIN PGP MESSAGE-----\n\n") {
			t.Errorf("%s: stored body keeps headers or line endings:\n%q", to, body)
		}
		if want, ok := sent[to+"/"+match]; !ok || packets(body) != want {
			t.Errorf("%s/%s: stored packets differ from the sent message (known=%v)", to, match, ok)
		}
	}
	if err = rows.Err(); err != nil || n != 3 {
		t.Fatalf("stored %d encrypted messages (%v)", n, err)
	}
	page := e.body("GET", "/messages", sender, nil, 200)
	if strings.Count(page, "Encrypted (to recipient’s key)") != 2 || strings.Count(page, "Encrypted (recipient unknown)") != 1 || strings.Contains(page, "Header") {
		t.Fatal("sender page badges or header text wrong")
	}
}
