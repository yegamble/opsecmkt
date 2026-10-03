package market

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

const testXMRSecret = "xmr-wallet-secret-0123456789abcdefABCDEF"

// testMnemonic is a 25-word Monero seed shape; its words are not a real wallet's.
var testMnemonic = strings.TrimSpace(strings.Repeat("abbey ", 24) + "zoo")

// TestXMRWalletPasswordDerivation pins the wallet password to HMAC-SHA256(MONERO_WALLET_SECRET, label NUL name),
// computed independently (Python hmac), and the verifier to its own label (P3-R4).
func TestXMRWalletPasswordDerivation(t *testing.T) {
	if got := xmrWalletPassword(testXMRSecret, "escrow-202610-01"); got != "e9c16b299aef3cfe95dcb7fd03131dcce1a4236db3c40ec7fe8ff44fd10eac13" {
		t.Fatalf("password %s", got)
	}
	if got := xmrSecretVerifier(testXMRSecret); got != "01cbbc5bd6c8e735bec3c758c19a2c8b378f2e49f2a3ff2f664820f2a6caed38" {
		t.Fatalf("verifier %s", got)
	}
	if xmrWalletPassword(testXMRSecret, "escrow-202610-02") == xmrWalletPassword(testXMRSecret, "escrow-202610-01") ||
		xmrWalletPassword(testXMRSecret+"x", "escrow-202610-01") == xmrWalletPassword(testXMRSecret, "escrow-202610-01") {
		t.Fatal("the password must depend on both the wallet name and the secret")
	}
}

// TestXMRWalletSecretRefusals: at least 32 random bytes, and never SETUP_TOKEN or APP_SECRET_KEY; no refusal
// quotes the value.
func TestXMRWalletSecretRefusals(t *testing.T) {
	if err := checkXMRWalletSecret(testXMRSecret, testSetupToken, ""); err != nil {
		t.Fatalf("good secret: %v", err)
	}
	cases := map[string]struct {
		secret, setup, app string
		want               error
	}{
		"empty":                   {"", testSetupToken, "", nil},
		"31 bytes":                {testXMRSecret[:31], testSetupToken, "", nil},
		"placeholder":             {"REPLACE_ME_with_a_long_random_value_0123456789", testSetupToken, "", nil},
		"repeated pattern":        {strings.Repeat("0123456789abcdef", 3), testSetupToken, "", nil},
		"equal to SETUP_TOKEN":    {testXMRSecret, testXMRSecret, "", errXMRSecretIsSetupToken},
		"equal to APP_SECRET_KEY": {testXMRSecret, testSetupToken, testXMRSecret, errXMRSecretIsAppKey},
	}
	for name, c := range cases {
		err := checkXMRWalletSecret(c.secret, c.setup, c.app)
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) || !strings.Contains(err.Error(), "MONERO_WALLET_SECRET") {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && c.secret != "" && strings.Contains(err.Error(), c.secret) {
			t.Errorf("%s: the refusal quotes the secret", name)
		}
	}
}

// testSeedKey generates a Curve25519 key with an encryption subkey; armoredPub is its public key.
func testSeedKey(t *testing.T, cfg *packet.Config) (*openpgp.Entity, string) {
	t.Helper()
	if cfg == nil {
		cfg = &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA}
	}
	e, err := openpgp.NewEntity("seed custody", "", "seeds@example.test", cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e, testArmor(t, e, false)
}

func testArmor(t *testing.T, e *openpgp.Entity, private bool) string {
	t.Helper()
	var buf bytes.Buffer
	kind := openpgp.PublicKeyType
	if private {
		kind = openpgp.PrivateKeyType
	}
	w, err := armor.Encode(&buf, kind, nil)
	if err != nil {
		t.Fatal(err)
	}
	if private {
		err = e.SerializePrivate(w, nil)
	} else {
		err = e.Serialize(w)
	}
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	return buf.String()
}

func writeSeedKey(t *testing.T, armored string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed-key.asc")
	if err := os.WriteFile(path, []byte(armored), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestXMRSeedKeyRefusals: ESCROW_XMR_SEED_KEY must be one armored public key that can be encrypted to now; a
// private key block, a signing-only key, an expired key, two keys, junk and a missing file are refused.
func TestXMRSeedKeyRefusals(t *testing.T) {
	e, pub := testSeedKey(t, nil)
	k, err := loadXMRSeedKey(writeSeedKey(t, pub))
	if err != nil || k.fpr != strings.ToUpper(fingerprintHex(e)) {
		t.Fatalf("good key: %+v %v", k, err)
	}
	signing, _ := testSeedKey(t, nil)
	signing.Subkeys = nil
	past := time.Now().Add(-48 * time.Hour)
	expired, _ := testSeedKey(t, &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Time: func() time.Time { return past }, KeyLifetimeSecs: 3600})
	_, other := testSeedKey(t, nil)
	cases := map[string]struct {
		armored string
		want    error
	}{
		"private key block": {testArmor(t, e, true), errXMRSeedKeyPrivate},
		"signing-only key":  {testArmor(t, signing, false), errXMRSeedKeyNoEncryption},
		"expired key":       {testArmor(t, expired, false), errXMRSeedKeyNoEncryption},
		"two keys":          {pub + "\n" + other, errXMRSeedKeyUnreadable},
		"junk":              {"not a key", errXMRSeedKeyUnreadable},
		"empty file":        {"", errXMRSeedKeyUnreadable},
	}
	for name, c := range cases {
		if _, err := loadXMRSeedKey(writeSeedKey(t, c.armored)); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
	missing := filepath.Join(t.TempDir(), "absent.asc")
	if _, err := loadXMRSeedKey(missing); !errors.Is(err, errXMRSeedKeyFile) || strings.Contains(err.Error(), missing) {
		t.Errorf("missing file: %v", err)
	}
	big := writeSeedKey(t, pub+strings.Repeat("\n", xmrSeedKeyMaxBytes))
	if _, err := loadXMRSeedKey(big); !errors.Is(err, errXMRSeedKeyFile) {
		t.Errorf("oversized file: %v", err)
	}
}

func fingerprintHex(e *openpgp.Entity) string {
	const digits = "0123456789ABCDEF"
	var b strings.Builder
	for _, c := range e.PrimaryKey.Fingerprint {
		b.WriteByte(digits[c>>4])
		b.WriteByte(digits[c&15])
	}
	return b.String()
}

// TestXMRSealSeedRoundTrip: the sealed seed decrypts with the custody key's private half (generated here) and only
// there; neither the armor nor any error carries the mnemonic.
func TestXMRSealSeedRoundTrip(t *testing.T) {
	e, pub := testSeedKey(t, nil)
	k, err := loadXMRSeedKey(writeSeedKey(t, pub))
	if err != nil {
		t.Fatal(err)
	}
	sealed, fpr, err := k.sealSeed(testMnemonic)
	if err != nil || fpr != k.fpr || fpr != fingerprintHex(e) {
		t.Fatalf("seal: %q %v", fpr, err)
	}
	if strings.Contains(sealed, "abbey") || !strings.HasPrefix(sealed, "-----BEGIN PGP MESSAGE-----") {
		t.Fatalf("sealed seed is not an armored message hiding the mnemonic:\n%s", sealed)
	}
	decrypt := func(key *openpgp.Entity) (string, error) {
		block, err := armor.Decode(strings.NewReader(sealed))
		if err != nil {
			return "", err
		}
		md, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{key}, nil, nil)
		if err != nil {
			return "", err
		}
		b, err := io.ReadAll(md.UnverifiedBody)
		return string(b), err
	}
	if got, err := decrypt(e); err != nil || got != testMnemonic {
		t.Fatalf("decrypt: %q %v", got, err)
	}
	stranger, _ := testSeedKey(t, nil)
	if _, err := decrypt(stranger); err == nil {
		t.Fatal("another key decrypted the seal")
	}
	if _, _, err := k.sealSeed(""); err == nil {
		t.Fatal("sealed an empty mnemonic")
	}
	// A key that cannot encrypt by the time it is used: the error is fixed and never carries the mnemonic.
	broken := &xmrSeedKey{key: e, fpr: k.fpr}
	stripped := *e
	stripped.Subkeys = nil
	broken.key = &stripped
	if _, _, err := broken.sealSeed(testMnemonic); !errors.Is(err, errXMRSeal) || strings.Contains(err.Error(), "abbey") {
		t.Fatalf("failed seal: %v", err)
	}
}

// TestXMRPinSetting: the first value is pinned; a later different value is reported, never written. DB integration.
func TestXMRPinSetting(t *testing.T) {
	x := newTestApp(t)
	ctx := context.Background()
	for i, c := range []struct {
		value string
		want  bool
	}{{"first", true}, {"first", true}, {"second", false}} {
		ok, err := pinXMRSetting(ctx, x.DB, "escrow_xmr_test_pin", c.value)
		if err != nil || ok != c.want {
			t.Fatalf("step %d: %v %v", i, ok, err)
		}
	}
	var v string
	if err := x.DB.QueryRow("SELECT value FROM settings WHERE key='escrow_xmr_test_pin'").Scan(&v); err != nil || v != "first" {
		t.Fatalf("pinned %q %v", v, err)
	}
}
