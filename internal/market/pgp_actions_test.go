package market

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// testDecrypt decrypts an armored message with a test key (the user's side of a challenge).
func testDecrypt(t *testing.T, key *openpgp.Entity, armored string) string {
	t.Helper()
	block, err := armor.Decode(strings.NewReader(armored))
	if err != nil {
		t.Fatal(err)
	}
	md, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{key}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(md.UnverifiedBody)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFormatFingerprint(t *testing.T) {
	if got := formatFingerprint("0123456789ABCDEF0123456789ABCDEF01234567"); got != "0123 4567 89AB CDEF 0123 4567 89AB CDEF 0123 4567" {
		t.Fatal(got)
	}
	if formatFingerprint("") != "" || formatFingerprint("ABCDEF") != "ABCD EF" {
		t.Fatal("short input")
	}
}

func TestRecipientMatch(t *testing.T) {
	alice, _ := testPGPKey(t, "alice")
	ids := entityKeyIDs(alice)
	for name, tc := range map[string]struct {
		ids  []uint64
		key  *openpgp.Entity
		want string
	}{
		"no recipient key":       {[]uint64{ids[1]}, nil, matchUnknown},
		"subkey":                 {[]uint64{ids[1]}, alice, matchYes},
		"primary":                {[]uint64{ids[0]}, alice, matchYes},
		"also to others":         {[]uint64{42, ids[1]}, alice, matchYes},
		"other key only":         {[]uint64{42}, alice, matchNo},
		"hidden recipient":       {[]uint64{0}, alice, matchUnknown},
		"hidden and other":       {[]uint64{42, 0}, alice, matchUnknown},
		"passphrase (no pkesk)":  {nil, alice, matchUnknown},
		"passphrase without key": {nil, nil, matchUnknown},
	} {
		if got := recipientMatch(tc.ids, tc.key); got != tc.want {
			t.Errorf("%s: %s want %s", name, got, tc.want)
		}
	}
}

func TestMessageStatus(t *testing.T) {
	alice, alicePub := testPGPKey(t, "alice")
	_, bobPub := testPGPKey(t, "bob")
	toAlice, err := encryptTo(alice, "hello")
	if err != nil {
		t.Fatal(err)
	}
	var signed bytes.Buffer
	w, _ := armor.Encode(&signed, "PGP MESSAGE", nil)
	lit, _ := openpgp.Sign(w, alice, nil, nil)
	io.WriteString(lit, "signed but not encrypted")
	lit.Close()
	w.Close()
	var sym bytes.Buffer
	w, _ = armor.Encode(&sym, "PGP MESSAGE", nil)
	pt, _ := openpgp.SymmetricallyEncrypt(w, []byte("passphrase"), nil, nil)
	io.WriteString(pt, "secret")
	pt.Close()
	w.Close()
	for name, tc := range map[string]struct {
		body, key string
		encrypted bool
		match     string
	}{
		"to recipient":        {toAlice, alicePub, true, matchYes},
		"to another key":      {toAlice, bobPub, true, matchNo},
		"recipient has none":  {toAlice, "", true, matchUnknown},
		"recipient key bad":   {toAlice, "garbage", true, matchUnknown},
		"passphrase only":     {sym.String(), alicePub, true, matchUnknown},
		"signed plaintext":    {signed.String(), alicePub, false, ""},
		"fake armor":          {"-----BEGIN PGP MESSAGE-----\nnot really\n-----END PGP MESSAGE-----", alicePub, false, ""},
		"plaintext":           {"hello there", alicePub, false, ""},
		"public key as body":  {alicePub, alicePub, false, ""},
		"truncated encrypted": {toAlice[:len(toAlice)/2] + "\n-----END PGP MESSAGE-----", alicePub, false, ""},
	} {
		enc, match := messageStatus(tc.body, tc.key)
		if enc != tc.encrypted || match != tc.match {
			t.Errorf("%s: encrypted=%v match=%q", name, enc, match)
		}
	}
}

func TestCheckSignedProof(t *testing.T) {
	alice, _ := testPGPKey(t, "alice")
	mallory, _ := testPGPKey(t, "mallory")
	challenge := "OPSECMKT key ownership proof for alice: 0123456789abcdef0123456789abcdef"
	detached := func(signer *openpgp.Entity, data string) string {
		var b bytes.Buffer
		if err := openpgp.ArmoredDetachSign(&b, signer, strings.NewReader(data), nil); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	for name, sub := range map[string]string{
		"clearsigned":                testClearsign(t, alice, challenge),
		"clearsigned file (newline)": testClearsign(t, alice, challenge+"\n"),
		"detached exact":             detached(alice, challenge),
		"detached file (newline)":    detached(alice, challenge+"\n"),
		"detached CRLF":              detached(alice, challenge+"\r\n"),
		"surrounding whitespace":     "\n  " + testClearsign(t, alice, challenge) + "\n",
	} {
		if err := checkSignedProof(challenge, sub, alice); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}
	for name, sub := range map[string]string{
		"wrong key clearsigned": testClearsign(t, mallory, challenge),
		"wrong key detached":    detached(mallory, challenge),
		"other text":            testClearsign(t, alice, challenge+" extra"),
		"old challenge":         testClearsign(t, alice, "OPSECMKT key ownership proof for alice: ffffffffffffffffffffffffffffffff"),
		"detached other data":   detached(alice, challenge+"x"),
		"tampered":              strings.Replace(testClearsign(t, alice, challenge), "alice:", "alicf:", 1),
		"bare text":             challenge,
		"empty":                 "",
	} {
		if err := checkSignedProof(challenge, sub, alice); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
