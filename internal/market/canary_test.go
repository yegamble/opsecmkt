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
	if v.Fingerprint != groupFingerprint(fp) || strings.ReplaceAll(v.Fingerprint, " ", "") != fp || v.Clearsigned != signed || v.OperatorKey != strings.TrimSpace(operatorPub) {
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

// A-201: previously stored photo/private packets must never be published with the canary key.
// Stripping must keep its fingerprint and real statement verification; unpublishable keys show no key.
func TestCanaryStripsLegacyOperatorKeyPackets(t *testing.T) {
	key, photo, plain := testPhotoIDKey(t, "legacy_operator")
	signed := canarySign(t, key, "Operator statement.", time.Now())
	_, fp, canonical, err := parseCanonicalPublicKey(plain)
	if err != nil {
		t.Fatal(err)
	}
	variants := testPhotoIDVariants(t, key)
	variants["original photo"] = photoIDVariant{armored: photo}
	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			v := canaryView(variant.armored, signed, time.Now())
			if !v.Verified || v.OperatorKey != canonical || v.Fingerprint != groupFingerprint(fp) {
				t.Fatalf("legacy operator key leaked packets or lost verification: %+v", v)
			}
			if _, photos, other := keyExtraPackets(v.OperatorKey); photos != 0 || other != 0 {
				t.Fatalf("published key has %d photo and %d extra packets", photos, other)
			}
		})
	}
	for name, armored := range testSplitKeyVariants(t, key) {
		t.Run(name, func(t *testing.T) {
			v := canaryView(armored, signed, time.Now())
			if v.OperatorKey != "" || v.Verified || v.KeyError == "" || v.Error == "" || v.Clearsigned != signed {
				t.Fatalf("unpublishable operator key leaked or masked invalid statement: %+v", v)
			}
		})
	}
}

// expiringCanaryKey generates an operator key created at canaryKeyTime that expires one day later.
func expiringCanaryKey(t *testing.T) (*openpgp.Entity, string) {
	t.Helper()
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Time: func() time.Time { return canaryKeyTime }, KeyLifetimeSecs: 86400}
	e, err := openpgp.NewEntity("expiring", "", "expiring@example.test", cfg)
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

// canarySignWith clearsigns text with the given (sub)key and a signature created at `at`.
func canarySignWith(t *testing.T, k *packet.PrivateKey, text string, at time.Time) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, k, &packet.Config{Time: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, text)
	w.Close()
	return buf.String()
}

// canarySignExpiring clearsigns text with a signature created at `at` that expires after lifetime.
func canarySignExpiring(t *testing.T, e *openpgp.Entity, text string, at time.Time, lifetime time.Duration) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, e.PrivateKey, &packet.Config{Time: func() time.Time { return at }, SigLifetimeSecs: uint32(lifetime / time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, text)
	w.Close()
	return buf.String()
}

// A-69: a genuine signature that is no longer (or not yet) current is refused with its own reason and what to
// do, never with the forgery reason; a forgery or another key's signature still gets the forgery reason.
func TestCanaryExpiryReasons(t *testing.T) {
	now := time.Now()
	operator, operatorPub := canaryTestKey(t, "operator")
	other, _ := canaryTestKey(t, "other")
	expiring, expiringPub := expiringCanaryKey(t)
	forgery := "does not verify with the configured key"
	signed := canarySign(t, operator, "No warrants.", now.Add(-time.Hour))
	cases := map[string]struct{ key, text, reason string }{
		"expired key": {expiringPub, canarySign(t, expiring, "No warrants.", canaryKeyTime.Add(time.Hour)),
			"the configured key expired on 2026-01-03 03:04 UTC; extend its expiry"},
		"expired signature": {operatorPub, canarySignExpiring(t, operator, "No warrants.", now.Add(-48*time.Hour), 24*time.Hour),
			"the signature expired on " + formatUTC(now.Add(-24*time.Hour)) + "; sign a new statement"},
		"clock skew": {operatorPub, canarySign(t, operator, "No warrants.", now.Add(2*time.Minute)),
			"after this server's clock"},
		"forged":                    {operatorPub, strings.Replace(signed, "No warrants", "No subpoenas", 1), forgery},
		"other key":                 {operatorPub, canarySign(t, other, "No warrants.", now.Add(-time.Hour)), forgery},
		"other key, future-dated":   {operatorPub, canarySign(t, other, "No warrants.", now.Add(2*time.Minute)), forgery},
		"forged, expired signature": {operatorPub, strings.Replace(canarySignExpiring(t, operator, "No warrants.", now.Add(-48*time.Hour), 24*time.Hour), "No warrants", "No subpoenas", 1), forgery},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := verifyCanary(tc.key, tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("got %v; want %q", err, tc.reason)
			}
			if tc.reason != forgery && strings.Contains(err.Error(), forgery) {
				t.Fatalf("genuine signature reported as forgery: %v", err)
			}
			if v := canaryView(tc.key, tc.text, now); v.Verified || !strings.Contains(v.Error, tc.reason) {
				t.Fatalf("view: %+v", v)
			}
		})
	}
	if _, _, _, err := verifyCanary(operatorPub, signed); err != nil {
		t.Fatalf("current canary: %v", err)
	}

	// A canary signed by a signing subkey that was revoked afterwards.
	revoking, _ := canaryTestKey(t, "revoking")
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Time: func() time.Time { return canaryKeyTime }}
	if err := revoking.AddSigningSubkey(cfg); err != nil {
		t.Fatal(err)
	}
	sub := &revoking.Subkeys[len(revoking.Subkeys)-1]
	bySubkey := canarySignWith(t, sub.PrivateKey, "No warrants.", now.Add(-time.Hour))
	if err := revoking.RevokeSubkey(sub, packet.KeyCompromised, "", cfg); err != nil {
		t.Fatal(err)
	}
	var revokedPub bytes.Buffer
	w, _ := armor.Encode(&revokedPub, openpgp.PublicKeyType, nil)
	if err := revoking.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, _, _, err := verifyCanary(revokedPub.String(), bySubkey); err == nil || !strings.Contains(err.Error(), "is revoked; configure a new key") || strings.Contains(err.Error(), forgery) {
		t.Fatalf("revoked signing subkey: %v", err)
	}

	// The key status shows an expired operator key as expired, and a current key as not expired.
	if v := canaryView(expiringPub, "", time.Time{}); v.KeyExpired != "2026-01-03 03:04 UTC" || v.KeyError != "" || v.Fingerprint == "" {
		t.Fatalf("expired operator key status: %+v", v)
	}
	if v := canaryView(operatorPub, "", time.Time{}); v.KeyExpired != "" {
		t.Fatalf("non-expiring operator key shown expired: %+v", v)
	}
}
