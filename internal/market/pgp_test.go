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
	body, cert := testPhotoID(t, key, 8)
	return key, testKeyWith(t, key, append(testPacket(17, body), cert...), nil), plain
}

// testPhotoID returns the body of a user attribute packet holding a size x size JPEG and the owner's positive
// certification over it.
func testPhotoID(t *testing.T, key *openpgp.Entity, size int) (body, cert []byte) {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, size, size))
	for i, seed := range img.Pix {
		img.Pix[i] = seed + byte(i*i*31+i*7) // noise, so a large photo stays large as a JPEG
	}
	uat, err := packet.NewUserAttributePhoto(img)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	for _, sp := range uat.Contents {
		if err = sp.Serialize(&b); err != nil {
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
	h.Write(binary.BigEndian.AppendUint32([]byte{0xD1}, uint32(b.Len())))
	h.Write(b.Bytes())
	if err = sig.Sign(h, key.PrivateKey, nil); err != nil {
		t.Fatal(err)
	}
	var c bytes.Buffer
	if err = sig.Serialize(&c); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), c.Bytes()
}

// testPacket encodes a new-format OpenPGP packet with a five-octet length.
func testPacket(tag byte, body []byte) []byte {
	return append(binary.BigEndian.AppendUint32([]byte{0xC0 | tag, 0xFF}, uint32(len(body))), body...)
}

// testPartialPacket encodes a new-format OpenPGP packet in 512-octet partial body lengths.
func testPartialPacket(tag byte, body []byte) []byte {
	out := []byte{0xC0 | tag}
	for len(body) > 512 {
		out = append(append(out, 0xE9), body[:512]...)
		body = body[512:]
	}
	return append(binary.BigEndian.AppendUint32(append(out, 0xFF), uint32(len(body))), body...)
}

// testKeyWith armors key with the middle packets after its user ID and the after packets after its subkey.
func testKeyWith(t *testing.T, key *openpgp.Entity, middle, after []byte) string {
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
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	w.Write(head.Bytes())
	w.Write(middle)
	w.Write(full.Bytes()[head.Len():])
	w.Write(after)
	w.Close()
	return buf.String()
}

// testKeyInterleaved armors key with afterUID between its user ID and the self-signature, and afterSubkey
// between its subkey and the binding signature.
func testKeyInterleaved(t *testing.T, key *openpgp.Entity, afterUID, afterSubkey []byte) string {
	t.Helper()
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	key.PrimaryKey.Serialize(w)
	for _, id := range key.Identities {
		id.UserId.Serialize(w)
		w.Write(afterUID)
		for _, sig := range id.Signatures {
			sig.Serialize(w)
		}
	}
	for _, sub := range key.Subkeys {
		sub.PublicKey.Serialize(w)
		w.Write(afterSubkey)
		sub.Sig.Serialize(w)
	}
	w.Close()
	return buf.String()
}

// testSplitKeyVariants places a marker packet (tag 10) or a non-critical private packet (tag 60), which
// OpenPGP readers skip, between a user ID or subkey and its signature (A-110 review): the key parses, but
// without those packets the signature that follows is dropped with them.
func testSplitKeyVariants(t *testing.T, key *openpgp.Entity) map[string]string {
	t.Helper()
	marker, private := testPacket(10, []byte("PGP")), testPacket(60, []byte("private"))
	return map[string]string{
		"marker uid->sig":  testKeyInterleaved(t, key, marker, nil),
		"marker sub->bind": testKeyInterleaved(t, key, nil, marker),
		"tag60 uid->sig":   testKeyInterleaved(t, key, private, nil),
		"tag60 sub->bind":  testKeyInterleaved(t, key, nil, private),
	}
}

type photoIDVariant struct {
	armored       string
	photos, other int // user attribute packets, other packets outside the allowlist
}

// testPhotoIDVariants arms key with photo IDs in every placement a reviewer found or probed (A-110): one or two
// photo IDs, a photo ID after the subkey, a photo ID in partial body lengths, and a photo ID hidden inside a
// compressed packet (algorithm 0) or a non-critical private packet (tag 60) that OpenPGP readers skip.
func testPhotoIDVariants(t *testing.T, key *openpgp.Entity) map[string]photoIDVariant {
	t.Helper()
	body, cert := testPhotoID(t, key, 8)
	photo := append(testPacket(17, body), cert...)
	body2, cert2 := testPhotoID(t, key, 64)
	photo2 := append(testPacket(17, body2), cert2...)
	if len(body2) <= 1024 {
		t.Fatalf("large photo is only %d bytes", len(body2))
	}
	both := append(append([]byte{}, photo...), photo2...)
	return map[string]photoIDVariant{
		"photo ID":                {testKeyWith(t, key, photo, nil), 1, 0},
		"two photo IDs":           {testKeyWith(t, key, both, nil), 2, 0},
		"photo ID after subkey":   {testKeyWith(t, key, nil, photo), 1, 0},
		"partial-length photo ID": {testKeyWith(t, key, append(testPartialPacket(17, body2), cert2...), nil), 1, 0},
		"photo ID in compressed":  {testKeyWith(t, key, testPacket(8, append([]byte{0}, photo...)), nil), 0, 1},
		"photo ID in tag 60":      {testKeyWith(t, key, testPacket(60, photo), nil), 0, 1},
		"compressed after subkey": {testKeyWith(t, key, nil, testPacket(8, append([]byte{0}, photo...))), 0, 1},
	}
}

// A-110: photo IDs and packets outside the allowlist are found in a pasted key, and the canonical form of
// a key that carries them - the text shown to others - is the same key without them: every other packet
// byte for byte, same fingerprint, no JPEG.
func TestPhotoIDFoundAndNeverRepublished(t *testing.T) {
	key, plain := testPGPKey(t, "photo")
	_, fp, err := parsePublicKey(plain)
	if err != nil {
		t.Fatal(err)
	}
	const jpeg = "\xff\xd8\xff" // JPEG start-of-image marker
	for name, v := range testPhotoIDVariants(t, key) {
		if !strings.Contains(armorPackets(t, v.armored), jpeg) {
			t.Fatalf("%s: fixture carries no JPEG", name)
		}
		if got, photos, other := keyExtraPackets(v.armored); got != fp || photos != v.photos || other != v.other {
			t.Errorf("%s: fingerprint %q, %d photo IDs, %d other packets", name, got, photos, other)
		}
		parsed, gotFP, canonical, err := parseCanonicalPublicKey(v.armored)
		if err != nil || gotFP != fp || parsed.PrimaryKey.KeyId != key.PrimaryKey.KeyId {
			t.Fatalf("%s: fp=%q err=%v", name, gotFP, err)
		}
		if got := armorPackets(t, canonical); got != armorPackets(t, plain) || strings.Contains(got, jpeg) {
			t.Errorf("%s: canonical form is not the key without its photo ID", name)
		}
	}
	// A keyring trust packet between a user ID and its self-signature is refused and dropped; the signature stays.
	trusted := testKeyInterleaved(t, key, testPacket(12, []byte{0, 0}), nil)
	if _, photos, other := keyExtraPackets(trusted); photos != 0 || other != 1 {
		t.Fatalf("trust packet: %d photo IDs, %d other packets", photos, other)
	}
	if _, _, canonical, err := parseCanonicalPublicKey(trusted); err != nil || armorPackets(t, canonical) != armorPackets(t, plain) {
		t.Fatalf("trust packet: canonical form is not the plain key (%v)", err)
	}
	// A skipped packet between a user ID or subkey and its signature: the key parses, but the canonical form would
	// not, so the key is reported as unusable rather than shown broken.
	for name, armored := range testSplitKeyVariants(t, key) {
		if _, photos, other := keyExtraPackets(armored); photos != 0 || other != 1 {
			t.Errorf("%s: %d photo IDs, %d other packets", name, photos, other)
		}
		if _, _, _, err := parseCanonicalPublicKey(armored); err == nil {
			t.Errorf("%s: a canonical form that does not parse was accepted", name)
		}
	}
	if got, photos, other := keyExtraPackets(plain); got != fp || photos != 0 || other != 0 {
		t.Fatalf("plain key: fingerprint %q, %d photo IDs, %d other packets", got, photos, other)
	}
	if _, photos, other := keyExtraPackets("not a key"); photos != 0 || other != 0 {
		t.Fatal("garbage reported as carrying extra packets")
	}
	if _, _, again, _ := parseCanonicalPublicKey(plain); armorPackets(t, again) != armorPackets(t, plain) {
		t.Fatal("a key without a photo ID changed")
	}
	if _, _, again, _ := parseCanonicalPublicKey(gpgFixtureKey); armorPackets(t, again) != armorPackets(t, gpgFixtureKey) {
		t.Fatal("a GnuPG-exported key changed")
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
	body, cert := testPhotoID(t, key, 8)
	photo := append(testPacket(17, body), cert...)
	var primary bytes.Buffer
	if err = key.PrimaryKey.Serialize(&primary); err != nil {
		t.Fatal(err)
	}
	rest := armorPackets(t, plain)[primary.Len():]
	var directFirst bytes.Buffer
	w, _ := armor.Encode(&directFirst, openpgp.PublicKeyType, nil)
	for _, p := range [][]byte{primary.Bytes(), photo, directPacket.Bytes(), []byte(rest)} {
		w.Write(p)
	}
	w.Close()
	_, _, canonical, err := parseCanonicalPublicKey(directFirst.String())
	if err != nil {
		t.Fatal(err)
	}
	if armorPackets(t, canonical) != primary.String()+directPacket.String()+rest {
		t.Fatal("a direct-key signature after the photo ID was dropped, or the photo ID kept")
	}
	// Placed after a photo ID that follows the user ID, it would land among the user ID's signatures once the
	// photo ID goes; that published form does not parse, so the key is refused rather than shown broken.
	if _, _, _, err = parseCanonicalPublicKey(testKeyWith(t, key, append(photo, directPacket.Bytes()...), nil)); err == nil {
		t.Fatal("a canonical form that does not parse was accepted")
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
