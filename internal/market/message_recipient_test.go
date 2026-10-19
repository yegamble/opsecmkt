package market

import (
	"bytes"
	"encoding/binary"
	"html"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// A-569 (policy: docs/data-retention-privacy-policy.md, "Require a usable recipient encryption key"): /messages stores
// only ciphertext its recipient can be expected to read. A recipient without a usable key is refused with 409, a
// passphrase-only message and one whose visible recipient key IDs are all other keys with 400, and a message whose
// recipient IDs are all hidden only after the sender ticks a confirmation, stored as "recipient unknown". Each
// refusal stores nothing and notifies nobody.
func TestMessageRecipientEnforcement(t *testing.T) {
	e := newTestApp(t)
	alice, alicePub := testPGPKey(t, "enforce_alice")
	bob, _ := testPGPKey(t, "enforce_bob")
	_, alicePrint, err := parsePublicKey(alicePub)
	if err != nil {
		t.Fatal(err)
	}
	senderID, sender := e.user("enforce_sender", "buyer")
	_, moderator := e.user("enforce_moderator", "moderator")
	keyedID, _ := e.user("enforce_keyed", "vendor")
	_, _ = e.user("enforce_nokey", "vendor")
	unusableID, _ := e.user("enforce_unusable", "vendor")
	e.setKey(keyedID, alicePub)
	e.setKey(unusableID, testUnusableKeys(t, "enforce_unusable")["signing-only key"])
	unreadableID, _ := e.user("enforce_unreadable", "vendor")
	agExec(e, "UPDATE users SET pgp=$1 WHERE id=$2", "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nbm90IGEga2V5\n-----END PGP PUBLIC KEY BLOCK-----", unreadableID)

	toAlice, err := encryptTo(alice, "for alice")
	if err != nil {
		t.Fatal(err)
	}
	toBob, err := encryptTo(bob, "for bob")
	if err != nil {
		t.Fatal(err)
	}
	toBoth := encryptToRecipients(t, "for alice and bob", alice, bob)
	hidden := hideRecipientIDs(t, toAlice, alice)
	bobAndHidden := hideRecipientIDs(t, encryptToRecipients(t, "bob visible, alice hidden", bob, alice), alice, alice.PrimaryKey.KeyId, alice.Subkeys[0].PublicKey.KeyId)
	aliceAndHidden := hideRecipientIDs(t, encryptToRecipients(t, "alice visible, bob hidden", alice, bob), bob, bob.PrimaryKey.KeyId, bob.Subkeys[0].PublicKey.KeyId)
	passphrase := symmetricMessage(t, "passphrase only")

	fingerprint := formatFingerprint(alicePrint)
	toKey := "Encrypt it to enforce_keyed’s key " + fingerprint
	written := func() [3]int {
		t.Helper()
		return [3]int{abuseCount(e, "SELECT count(*) FROM messages"), abuseCount(e, "SELECT count(*) FROM notifications"),
			abuseCount(e, "SELECT count(*) FROM audit_events WHERE action LIKE 'Sent OpenPGP-encrypted message%'")}
	}
	for _, c := range []struct {
		name, session, to, body string
		confirm                 bool
		code                    int
		msg, match              string // the refusal's words; the stored recipient match when sent
	}{
		{"no key saved", sender, "enforce_nokey", toAlice, false, 409, "enforce_nokey has not saved a PGP public key", ""},
		{"no key saved, from staff", moderator, "enforce_nokey", toAlice, false, 409, "Ask enforce_nokey to add a public key that can receive encrypted messages", ""},
		{"no usable key", sender, "enforce_unusable", toAlice, false, 409, "enforce_unusable’s saved PGP key cannot receive encrypted messages", ""},
		{"unreadable saved key", sender, "enforce_unreadable", toAlice, false, 409, "enforce_unreadable’s saved PGP key can no longer be read", ""},
		{"no usable key, confirmation ticked", sender, "enforce_unusable", hidden, true, 409, "cannot receive encrypted messages", ""},
		{"passphrase only", sender, "enforce_keyed", passphrase, false, 400, "encrypted with a passphrase only", ""},
		{"passphrase only, confirmation ticked", sender, "enforce_keyed", passphrase, true, 400, toKey, ""},
		{"another visible key", sender, "enforce_keyed", toBob, false, 400, "names only other keys", ""},
		{"another visible key, confirmation ticked", sender, "enforce_keyed", toBob, true, 400, toKey, ""},
		{"another visible key and a hidden one", sender, "enforce_keyed", bobAndHidden, true, 400, "names only other keys", ""},
		{"hidden recipients, not confirmed", sender, "enforce_keyed", hidden, false, 400, "recipient key IDs are hidden", ""},
		{"hidden recipients, confirmed", sender, "enforce_keyed", hidden, true, 303, "", matchUnknown},
		{"recipient's key", sender, "enforce_keyed", toAlice, false, 303, "", matchYes},
		{"recipient's key and another", sender, "enforce_keyed", toBoth, false, 303, "", matchYes},
		{"recipient's key and a hidden one", sender, "enforce_keyed", aliceAndHidden, false, 303, "", matchYes},
		{"recipient's key, confirmation ticked", sender, "enforce_keyed", toAlice, true, 303, "", matchYes},
	} {
		form := url.Values{"recipient": {c.to}, "body": {c.body}}
		if c.confirm {
			form.Set("hidden_recipients", "confirmed")
		}
		before := written()
		w := e.do("POST", "/messages", c.session, form)
		after := written()
		if w.Code != c.code {
			t.Fatalf("%s: status %d, want %d\n%s", c.name, w.Code, c.code, w.Body.String())
		}
		if c.code != 303 {
			if got := refusalText(w.Body.String()); !strings.Contains(got, c.msg) || !strings.Contains(got, "Nothing was stored") {
				t.Fatalf("%s: refusal %q does not say %q and that nothing was stored", c.name, got, c.msg)
			}
			if after != before {
				t.Fatalf("%s: a refused message wrote messages/notifications/audit %v -> %v", c.name, before, after)
			}
			continue
		}
		if after != [3]int{before[0] + 1, before[1] + 1, before[2] + 1} {
			t.Fatalf("%s: wrote %v -> %v, want one message, one notification and one audit row", c.name, before, after)
		}
		var match string
		if err = e.DB.QueryRow("SELECT recipient_match FROM messages WHERE sender_id=$1 ORDER BY created DESC, id LIMIT 1", senderID).Scan(&match); err != nil {
			t.Fatal(err)
		}
		if match != c.match {
			t.Fatalf("%s: stored recipient match %q, want %q", c.name, match, c.match)
		}
		if !e.auditExact(senderID, "Sent OpenPGP-encrypted message (recipient key match: "+c.match+")") {
			t.Fatalf("%s: no audit row for the match", c.name)
		}
	}

	// Without the confirmation, the compose form comes back with the ciphertext kept, the recipient chosen, the
	// explanation in its alert and the confirmation box, so a browser without JavaScript can tick it and send again.
	w := e.do("POST", "/messages", sender, url.Values{"recipient": {"enforce_keyed"}, "body": {hidden}})
	page := w.Body.String()
	if w.Code != 400 {
		t.Fatalf("unconfirmed hidden recipients: status %d", w.Code)
	}
	mustContain(t, page, `<input type="hidden" name="recipient" value="enforce_keyed">`, `<input type="checkbox" name="hidden_recipients" value="confirmed" required`,
		`<p class="message-to">To: <strong>enforce_keyed</strong></p>`, fingerprint, `<h2>Compose a message</h2>`)
	mustContain(t, refusalText(page), "The recipient key IDs are hidden", "fingerprint "+fingerprint, "tick the confirmation")
	kept := regexp.MustCompile(`(?s)<textarea name="body"[^>]*>\n(.*?)</textarea>`).FindStringSubmatch(page)
	canonical, err := canonicalMessage(hidden)
	if err != nil {
		t.Fatal(err)
	}
	if kept == nil || html.UnescapeString(kept[1]) != canonical {
		t.Fatalf("the hidden-recipient ciphertext is not kept in the form:\n%s", page)
	}
	// The ordinary compose form has no confirmation box.
	mustNotContain(t, e.body("GET", "/messages?to=enforce_keyed", sender, nil, 200), `name="hidden_recipients"`)
	// The compose page says when the recipient has no usable key; one with a key has no such note.
	for _, to := range []string{"enforce_nokey", "enforce_unusable"} {
		mustContain(t, e.body("GET", "/messages?to="+to, sender, nil, 200), "Messages to "+to+" cannot be sent until they save a PGP public key that can receive encrypted messages.")
	}
	mustNotContain(t, e.body("GET", "/messages?to=enforce_keyed", sender, nil, 200), "cannot be sent until")

	// The sender's page shows the stored statuses after reload; nothing was stored for the refused recipients.
	list := e.body("GET", "/messages", sender, nil, 200)
	if n := strings.Count(list, "Encrypted (to recipient’s key)"); n != 4 {
		t.Fatalf("to-recipient badges: %d", n)
	}
	if n := strings.Count(list, "Encrypted (recipient unknown)"); n != 1 {
		t.Fatalf("recipient-unknown badges: %d", n)
	}
	mustNotContain(t, list, "Encrypted (NOT to recipient’s key)", "enforce_sender → enforce_nokey", "enforce_sender → enforce_unusable", "enforce_sender → enforce_unreadable")
}

// hideRecipientIDs rewrites armored so that its public-key session packets name a wildcard key ID, as
// `gpg --throw-keyids` writes them; with ids, only the packets naming one of those key IDs are hidden. key must
// (when given) must still decrypt the result, which proves the rewrite left a real hidden-recipient message.
func hideRecipientIDs(t *testing.T, armored string, key *openpgp.Entity, ids ...uint64) string {
	t.Helper()
	data := armorPackets(t, armored)
	out := []byte(data)
	for i := 0; i < len(out); {
		tag := out[i]
		if tag&0x80 == 0 {
			t.Fatalf("hideRecipientIDs: not a packet header: %#x", tag)
		}
		var typ byte
		var hdr, length int
		if tag&0x40 != 0 { // new format (go-crypto)
			typ, length, hdr = tag&0x3f, int(out[i+1]), 2
			switch {
			case length < 192:
			case length < 224:
				hdr, length = 3, (length-192)<<8+int(out[i+2])+192
			case length == 255:
				hdr, length = 6, int(binary.BigEndian.Uint32(out[i+2:]))
			default:
				t.Fatal("hideRecipientIDs: a partial-length packet")
			}
		} else { // old format (GnuPG)
			typ = tag >> 2 & 0x0f
			switch tag & 3 {
			case 0:
				hdr, length = 2, int(out[i+1])
			case 1:
				hdr, length = 3, int(binary.BigEndian.Uint16(out[i+1:]))
			case 2:
				hdr, length = 5, int(binary.BigEndian.Uint32(out[i+1:]))
			default:
				t.Fatal("hideRecipientIDs: an indeterminate-length packet")
			}
		}
		if typ != 1 { // the encrypted data follows the session key packets
			break
		}
		if body := out[i+hdr:]; body[0] == 3 && (len(ids) == 0 || slices.Contains(ids, binary.BigEndian.Uint64(body[1:9]))) {
			clear(body[1:9])
		}
		i += hdr + length
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write(out); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if key != nil {
		md, err := openpgp.ReadMessage(bytes.NewReader(out), openpgp.EntityList{key}, nil, nil)
		if err != nil {
			t.Fatalf("hidden-recipient message does not decrypt: %v", err)
		}
		if _, err = io.ReadAll(md.UnverifiedBody); err != nil {
			t.Fatal(err)
		}
	}
	if ids, encrypted, err := inspectEncrypted(buf.String()); err != nil || !encrypted || len(ids) == 0 {
		t.Fatalf("rewritten message: ids=%v encrypted=%v err=%v", ids, encrypted, err)
	}
	return buf.String()
}

// symmetricMessage returns plaintext encrypted with a passphrase only: a symmetric-key session packet and no
// public-key session packet.
func symmetricMessage(t *testing.T, plaintext string) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := openpgp.SymmetricallyEncrypt(w, []byte("a passphrase the recipient does not have"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(pt, plaintext); err != nil {
		t.Fatal(err)
	}
	if err = pt.Close(); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
