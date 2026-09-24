package market

import (
	"bytes"
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
		"trailing packets":       append(append([]byte{}, data...), data...),
		"truncated":              data[:len(data)-5],
		"no session key packets": seipd,
	} {
		if _, encrypted, err := inspectEncrypted(rearmor(b)); err == nil || encrypted {
			t.Errorf("%s: encrypted=%v err=%v", name, encrypted, err)
		}
	}
}

func TestSealOpen(t *testing.T) {
	a := &App{setupToken: testSetupToken}
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
	if _, err = (&App{setupToken: strings.Repeat("x", 64)}).open("totp", ct); err == nil {
		t.Fatal("opened after SETUP_TOKEN rotation")
	}
	if _, err = a.open("totp", ct[:len(ct)-2]); err == nil {
		t.Fatal("truncated ciphertext opened")
	}
	if _, err = (&App{}).seal("totp", "x"); err == nil {
		t.Fatal("sealed without SETUP_TOKEN")
	}
}
