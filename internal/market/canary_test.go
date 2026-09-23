package market

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

var canaryKeyTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// canaryTestKey generates an operator key created at canaryKeyTime and returns it with its armored public key.
func canaryTestKey(t *testing.T, name string) (*openpgp.Entity, string) {
	t.Helper()
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Time: func() time.Time { return canaryKeyTime }}
	e, err := openpgp.NewEntity(name, "", name+"@example.test", cfg)
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

// canarySign clearsigns text with a signature created at `at`.
func canarySign(t *testing.T, e *openpgp.Entity, text string, at time.Time) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, e.PrivateKey, &packet.Config{Time: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, text)
	w.Close()
	return buf.String()
}

func TestCanaryView(t *testing.T) {
	operator, operatorPub := canaryTestKey(t, "operator")
	other, otherPub := canaryTestKey(t, "other")
	statement := "As of 2026-01-02 no warrants have been served.\nNext update by 2026-02-01."
	signedAt := canaryKeyTime.Add(90 * time.Minute)
	signed := canarySign(t, operator, statement, signedAt)
	posted := time.Date(2026, 1, 3, 0, 0, 0, 0, time.FixedZone("x", 3600))
	_, fp, _ := parsePublicKey(operatorPub)

	v := canaryView(operatorPub, signed, posted)
	if !v.Verified || v.Error != "" || v.Statement != statement || v.SignedAt != "2026-01-02 04:34 UTC" || v.Posted != "2026-01-02 23:00 UTC" {
		t.Fatalf("valid canary: %+v", v)
	}
	if v.Fingerprint != groupFingerprint(fp) || strings.ReplaceAll(v.Fingerprint, " ", "") != fp || v.Clearsigned != signed || v.OperatorKey != operatorPub {
		t.Fatalf("fingerprint/raw text: %+v", v)
	}
	// Browsers submit textarea content with CRLF line endings.
	if v := canaryView(operatorPub, strings.ReplaceAll(signed, "\n", "\r\n"), posted); !v.Verified || v.Statement != statement {
		t.Fatalf("CRLF canary: %+v", v)
	}

	invalid := map[string]struct{ key, text, reason string }{
		"tampered statement": {operatorPub, strings.Replace(signed, "no warrants", "two warrants", 1), "does not verify"},
		"different key":      {otherPub, signed, "does not verify"},
		"signed by other":    {operatorPub, canarySign(t, other, statement, signedAt), "does not verify"},
		"trailing text":      {operatorPub, signed + "\nAlso: all is well.", "after the signature"},
		"leading text":       {operatorPub, "Unsigned preface\n" + signed, "nothing before it"},
		"two messages":       {operatorPub, signed + "\n" + signed, "exactly one"},
		"not clearsigned":    {operatorPub, "-----BEGIN PGP SIGNED MESSAGE-----\nplain text", "well-formed"},
		"unusable key":       {"-----BEGIN PGP PUBLIC KEY BLOCK-----\ngarbage\n-----END PGP PUBLIC KEY BLOCK-----", signed, "unusable"},
	}
	for name, tc := range invalid {
		t.Run(name, func(t *testing.T) {
			v := canaryView(tc.key, tc.text, posted)
			if v.Verified || v.Statement != "" || v.SignedAt != "" || !strings.Contains(v.Error, tc.reason) || v.Clearsigned != tc.text || v.Posted == "" {
				t.Fatalf("got %+v; want invalid with %q", v, tc.reason)
			}
		})
	}

	// Missing key or missing statement: "No signed canary published" (nothing verified, nothing invalid).
	for name, v := range map[string]*CanaryView{
		"no key":         canaryView("", signed, posted),
		"blank key":      canaryView("  \n", signed, posted),
		"no statement":   canaryView(operatorPub, "", time.Time{}),
		"nothing at all": canaryView("", "", time.Time{}),
	} {
		if v.Verified || v.Error != "" || v.Statement != "" || v.Clearsigned != "" {
			t.Errorf("%s: %+v", name, v)
		}
	}
	if v := canaryView(operatorPub, "", time.Time{}); v.Fingerprint == "" || v.KeyError != "" {
		t.Errorf("configured key without statement should still show its fingerprint: %+v", v)
	}
	if _, _, _, err := verifyCanary("", signed); err == nil || !strings.Contains(err.Error(), "no operator PGP key") {
		t.Errorf("missing key: %v", err)
	}
	if _, _, _, err := verifyCanary(operatorPub, "-----BEGIN PGP SIGNED MESSAGE-----\n"+strings.Repeat("a", maxCanaryBytes)); err == nil || !strings.Contains(err.Error(), "longer") {
		t.Errorf("oversized: %v", err)
	}
	if groupFingerprint("0123456789ABCDEF01") != "0123 4567 89AB CDEF 01" {
		t.Error(groupFingerprint("0123456789ABCDEF01"))
	}
}

func TestPreviewCanaryIsRealAndLabelled(t *testing.T) {
	v := previewCanary()
	if !v.Verified || !strings.HasPrefix(v.Statement, "PREVIEW SAMPLE") || v.Fingerprint == "" {
		t.Fatalf("preview canary: %+v", v)
	}
}
