package market

import (
	"bytes"
	"crypto"
	"encoding/binary"
	"image"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// testPGPKey generates a Curve25519 key; public returns its armored public key.
func testPGPKey(t *testing.T, name string) (*openpgp.Entity, string) {
	t.Helper()
	e, err := openpgp.NewEntity(name, "", name+"@example.test", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err = e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return e, buf.String()
}

// testPhotoIDKey generates a key as GnuPG exports it after `gpg addphoto` (A-110): its user ID, then a user
// attribute packet holding a JPEG with the owner's positive certification over it, then the subkey. photo is
// that key armored; plain is the same key without the photo ID.
func testPhotoIDKey(t *testing.T, name string) (key *openpgp.Entity, photo, plain string) {
	t.Helper()
	key, plain = testPGPKey(t, name)
	return key, withPhotoID(t, key), plain
}

// withPhotoID armors key with a certified photo ID after its user ID, followed by the extra packets.
func withPhotoID(t *testing.T, key *openpgp.Entity, extra ...[]byte) string {
	t.Helper()
	var head, full bytes.Buffer
	withoutSubkeys := *key
	withoutSubkeys.Subkeys = nil
	if err := withoutSubkeys.Serialize(&head); err != nil {
		t.Fatal(err)
	}
	if err := key.Serialize(&full); err != nil {
		t.Fatal(err)
	}
	uat, err := packet.NewUserAttributePhoto(image.NewGray(image.Rect(0, 0, 8, 8)))
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	for _, sp := range uat.Contents {
		if err = sp.Serialize(&body); err != nil {
			t.Fatal(err)
		}
	}
	// RFC 9580 5.2.4: a certification of a user attribute hashes the primary key, 0xD1, a four-octet length and the body.
	sig := &packet.Signature{Version: 4, SigType: packet.SigTypePositiveCert, PubKeyAlgo: key.PrimaryKey.PubKeyAlgo,
		Hash: crypto.SHA256, CreationTime: time.Now(), IssuerKeyId: &key.PrimaryKey.KeyId}
	h, err := sig.PrepareSign(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = key.PrimaryKey.SerializeForHash(h); err != nil {
		t.Fatal(err)
	}
	h.Write(binary.BigEndian.AppendUint32([]byte{0xD1}, uint32(body.Len())))
	h.Write(body.Bytes())
	if err = sig.Sign(h, key.PrivateKey, nil); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	w.Write(head.Bytes())
	if err = uat.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err = sig.Serialize(w); err != nil {
		t.Fatal(err)
	}
	for _, p := range extra {
		w.Write(p)
	}
	w.Write(full.Bytes()[head.Len():])
	w.Close()
	return buf.String()
}

// A-110: a photo ID is found in a pasted key, and the canonical form of a key that carries one - the
// text shown to others - is the same key without it: every other packet byte for byte, same fingerprint.
func TestPhotoIDFoundAndNeverRepublished(t *testing.T) {
	key, photo, plain := testPhotoIDKey(t, "photo")
	_, fp, err := parsePublicKey(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(armorPackets(t, photo), "\xff\xd8\xff") { // JPEG start-of-image marker
		t.Fatal("fixture carries no JPEG")
	}
	if got, n := keyPhotoIDs(photo); got != fp || n != 1 {
		t.Fatalf("photo key: fingerprint %q, %d photo IDs", got, n)
	}
	if got, n := keyPhotoIDs(plain); got != fp || n != 0 {
		t.Fatalf("plain key: fingerprint %q, %d photo IDs", got, n)
	}
	if _, n := keyPhotoIDs("not a key"); n != 0 {
		t.Fatal("garbage reported as carrying a photo ID")
	}
	parsed, gotFP, canonical, err := parseCanonicalPublicKey(photo)
	if err != nil || gotFP != fp || parsed.PrimaryKey.KeyId != key.PrimaryKey.KeyId {
		t.Fatalf("fp=%q err=%v", gotFP, err)
	}
	if armorPackets(t, canonical) != armorPackets(t, plain) {
		t.Fatal("canonical form of a photo ID key is not the key without its photo ID")
	}
	if _, _, again, _ := parseCanonicalPublicKey(plain); armorPackets(t, again) != armorPackets(t, plain) {
		t.Fatal("a key without a photo ID changed")
	}

	// Only the photo ID's certifications go with it: a direct-key signature placed after them applies to the
	// whole key and stays.
	direct := &packet.Signature{Version: 4, SigType: packet.SigTypeDirectSignature, PubKeyAlgo: key.PrimaryKey.PubKeyAlgo,
		Hash: crypto.SHA256, CreationTime: time.Now(), IssuerKeyId: &key.PrimaryKey.KeyId}
	if err = direct.SignDirectKeyBinding(key.PrimaryKey, key.PrivateKey, nil); err != nil {
		t.Fatal(err)
	}
	var directPacket bytes.Buffer
	if err = direct.Serialize(&directPacket); err != nil {
		t.Fatal(err)
	}
	_, _, canonical, err = parseCanonicalPublicKey(withPhotoID(t, key, directPacket.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got := armorPackets(t, canonical); !strings.Contains(got, directPacket.String()) || strings.Replace(got, directPacket.String(), "", 1) != armorPackets(t, plain) {
		t.Fatal("a direct-key signature after the photo ID was dropped, or the photo ID kept")
	}
}

func testClearsign(t *testing.T, e *openpgp.Entity, text string) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, e.PrivateKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, text)
	w.Close()
	return buf.String()
}

func TestParsePublicKey(t *testing.T) {
	e, pub := testPGPKey(t, "alice")
	key, fp, err := parsePublicKey("\n" + pub + "\n")
	if err != nil || len(fp) != 40 || fp != strings.ToUpper(fp) || key.PrimaryKey.KeyId != e.PrimaryKey.KeyId {
		t.Fatalf("fp=%q err=%v", fp, err)
	}
	if ids := entityKeyIDs(key); len(ids) != 2 || ids[0] != e.PrimaryKey.KeyId {
		t.Fatalf("key ids %v", ids)
	}
	var priv bytes.Buffer
	w, _ := armor.Encode(&priv, openpgp.PrivateKeyType, nil)
	e.SerializePrivate(w, nil)
	w.Close()
	bob, pub2 := testPGPKey(t, "bob")
	var ring bytes.Buffer
	w, _ = armor.Encode(&ring, openpgp.PublicKeyType, nil)
	e.Serialize(w)
	bob.Serialize(w)
	w.Close()
	for name, input := range map[string]string{
		"garbage": "not a key", "private": priv.String(), "empty": "",
		"two blocks": pub + "\n" + pub2, "two keys in one block": ring.String(),
		"truncated": pub[:len(pub)/2] + "\n-----END PGP PUBLIC KEY BLOCK-----",
	} {
		if _, _, err := parsePublicKey(input); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestVerifySignatures(t *testing.T) {
	alice, _ := testPGPKey(t, "alice")
	mallory, _ := testPGPKey(t, "mallory")
	signed := testClearsign(t, alice, "Canary statement\nNo warrants.")
	text, at, err := verifyClearsigned(signed, alice)
	if err != nil || text != "Canary statement\nNo warrants." || time.Since(at) > time.Minute {
		t.Fatalf("text=%q at=%v err=%v", text, at, err)
	}
	if _, _, err = verifyClearsigned(signed, mallory); err == nil {
		t.Fatal("wrong key verified")
	}
	if _, _, err = verifyClearsigned(strings.Replace(signed, "No warrants.", "No warrants!", 1), alice); err == nil {
		t.Fatal("tampered text verified")
	}
	if _, _, err = verifyClearsigned("plain text", alice); err == nil {
		t.Fatal("unsigned text verified")
	}
	var sig bytes.Buffer
	if err = openpgp.ArmoredDetachSignText(&sig, alice, strings.NewReader("nonce-123"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err = verifyDetached("nonce-123", sig.String(), alice); err != nil {
		t.Fatalf("detached: %v", err)
	}
	if _, err = verifyDetached("nonce-124", sig.String(), alice); err == nil {
		t.Fatal("detached signature over other data verified")
	}
	if _, err = verifyDetached("nonce-123", sig.String(), mallory); err == nil {
		t.Fatal("detached signature verified with wrong key")
	}
}

func TestEncryptAndInspect(t *testing.T) {
	alice, _ := testPGPKey(t, "alice")
	msg, err := encryptTo(alice, "challenge-nonce")
	if err != nil || !strings.HasPrefix(msg, "-----BEGIN PGP MESSAGE-----") {
		t.Fatalf("%v %q", err, msg)
	}
	block, _ := armor.Decode(strings.NewReader(msg))
	md, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{alice}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(md.UnverifiedBody); string(b) != "challenge-nonce" {
		t.Fatalf("decrypted %q", b)
	}
	ids, encrypted, err := inspectEncrypted(msg)
	if err != nil || !encrypted || len(ids) != 1 || !slices.Contains(entityKeyIDs(alice), ids[0]) {
		t.Fatalf("ids=%v encrypted=%v err=%v", ids, encrypted, err)
	}
	var plain bytes.Buffer
	w, _ := armor.Encode(&plain, "PGP MESSAGE", nil)
	lit, _ := openpgp.Sign(w, alice, nil, nil)
	io.WriteString(lit, "signed but not encrypted")
	lit.Close()
	w.Close()
	if _, encrypted, err = inspectEncrypted(plain.String()); err != nil || encrypted {
		t.Fatalf("signed plaintext reported encrypted=%v err=%v", encrypted, err)
	}
	var sym bytes.Buffer
	w, _ = armor.Encode(&sym, "PGP MESSAGE", nil)
	pt, _ := openpgp.SymmetricallyEncrypt(w, []byte("passphrase"), nil, nil)
	io.WriteString(pt, "secret")
	pt.Close()
	w.Close()
	if ids, encrypted, err = inspectEncrypted(sym.String()); err != nil || !encrypted || len(ids) != 0 {
		t.Fatalf("symmetric: ids=%v encrypted=%v err=%v", ids, encrypted, err)
	}
	if _, _, err = inspectEncrypted("-----BEGIN PGP MESSAGE-----\nSample encrypted armor\n-----END PGP MESSAGE-----"); err == nil {
		t.Fatal("fake armor accepted")
	}
}

// A-67: the packet stream must be key packets then exactly one encrypted-data packet, read to its end.
func TestInspectEncryptedPacketSequence(t *testing.T) {
	alice, _ := testPGPKey(t, "alice")
	msg, err := encryptTo(alice, "challenge-nonce")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := armor.Decode(strings.NewReader(msg))
	data, _ := io.ReadAll(block.Body)
	r := bytes.NewReader(data)
	if _, err = packet.Read(r); err != nil {
		t.Fatal(err)
	}
	seipd := data[len(data)-r.Len():]
	pkesk := data[:len(data)-r.Len()]
	// Tag 9 (SymmetricallyEncrypted without integrity protection, as `gpg --rfc2440` may emit): a
	// new-format header 0xC9, a one-octet length and opaque ciphertext. Only SEIPD/AEAD count (QA5-2).
	tag9 := append(append([]byte{}, pkesk...), append([]byte{0xC9, 32}, bytes.Repeat([]byte{0xA5}, 32)...)...)
	rearmor := func(b []byte) string {
		var buf bytes.Buffer
		w, _ := armor.Encode(&buf, "PGP MESSAGE", nil)
		w.Write(b)
		w.Close()
		return buf.String()
	}
	if _, encrypted, err := inspectEncrypted(rearmor(data)); err != nil || !encrypted {
		t.Fatalf("re-armored message: encrypted=%v err=%v", encrypted, err)
	}
	for name, b := range map[string][]byte{
		"trailing packets":        append(append([]byte{}, data...), data...),
		"truncated":               data[:len(data)-5],
		"no session key packets":  seipd,
		"no integrity protection": tag9,
	} {
		if _, encrypted, err := inspectEncrypted(rearmor(b)); err == nil || encrypted {
			t.Errorf("%s: encrypted=%v err=%v", name, encrypted, err)
		}
	}
}

func TestSealOpen(t *testing.T) {
	a := &App{keyRoot: testSetupToken}
	ct, err := a.seal("totp", "JBSWY3DPEHPK3PXP")
	if err != nil || strings.Contains(ct, "JBSWY3DPEHPK3PXP") {
		t.Fatalf("%v %q", err, ct)
	}
	if again, _ := a.seal("totp", "JBSWY3DPEHPK3PXP"); again == ct {
		t.Fatal("nonce reused")
	}
	if pt, err := a.open("totp", ct); err != nil || pt != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("%q %v", pt, err)
	}
	if _, err = a.open("other", ct); err == nil {
		t.Fatal("opened under another label")
	}
	if _, err = (&App{keyRoot: strings.Repeat("x", 64)}).open("totp", ct); err == nil {
		t.Fatal("opened after key root rotation")
	}
	if _, err = a.open("totp", ct[:len(ct)-2]); err == nil {
		t.Fatal("truncated ciphertext opened")
	}
	if _, err = (&App{}).seal("totp", "x"); err == nil {
		t.Fatal("sealed without a key root")
	}
}
