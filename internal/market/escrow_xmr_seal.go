package market

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// Monero escrow secrets and seeds (WR-escrow Phase 3, P3-R4).
//
// Every escrow wallet's password derives from MONERO_WALLET_SECRET and the wallet's name, so the database never
// holds a wallet password. The secret is its own: it must not be SETUP_TOKEN or APP_SECRET_KEY, which derive the
// app's other keys and which an operator may rotate. A verifier (an HMAC of a fixed label, never a password) is
// pinned in settings on the first start; a different secret later disables Monero escrow rather than create
// wallets nobody can open with the old one.
//
// Each wallet's seed is sealed to ESCROW_XMR_SEED_KEY, the path of an armored OpenPGP public key the operator keeps
// the private half of offline. It is read from that file only: never the canary's operator key or any key an
// administrator can change from /admin, so taking over an administrator account cannot redirect future seeds. Its
// fingerprint is pinned in settings on first use; a different key later stops wallet creation until the operator
// re-pins it (docs/operator-guide.md, "Monero escrow"). Seeds are never re-sealed automatically.

const (
	xmrPasswordLabel   = "opsecmkt xmr escrow wallet\x00" // + the wallet name
	xmrVerifierLabel   = "opsecmkt xmr escrow secret verifier"
	xmrVerifierSetting = "escrow_xmr_secret_verifier"      // settings key
	xmrSeedKeySetting  = "escrow_xmr_seed_key_fingerprint" // settings key
	xmrSeedKeyMaxBytes = 64 << 10
)

var (
	errXMRSecretIsSetupToken  = errors.New("MONERO_WALLET_SECRET must be a secret of its own, not the value of SETUP_TOKEN; generate one with `openssl rand -hex 32`")
	errXMRSecretIsAppKey      = errors.New("MONERO_WALLET_SECRET must be a secret of its own, not the value of APP_SECRET_KEY; generate one with `openssl rand -hex 32`")
	errXMRSeedKeyFile         = errors.New("ESCROW_XMR_SEED_KEY must be the path of a readable file (at most 64 KiB) holding the armored OpenPGP public key Monero escrow seeds are sealed to")
	errXMRSeedKeyPrivate      = errors.New("ESCROW_XMR_SEED_KEY holds a private key block; give the app the public key only and keep the private key offline")
	errXMRSeedKeyUnreadable   = errors.New("ESCROW_XMR_SEED_KEY must hold exactly one armored, unrevoked OpenPGP public key block (gpg --armor --export <fingerprint>)")
	errXMRSeedKeyNoEncryption = errors.New("ESCROW_XMR_SEED_KEY has no encryption key usable now (it is signing-only or expired); add or renew an encryption subkey and export the public key again")
	errXMRSeal                = errors.New("sealing the wallet seed to ESCROW_XMR_SEED_KEY failed (is the key's encryption subkey still valid?)")
)

// xmrWalletPassword is the password of escrow wallet name: hex HMAC-SHA256(secret, xmrPasswordLabel + name).
func xmrWalletPassword(secret, name string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(xmrPasswordLabel + name))
	return hex.EncodeToString(m.Sum(nil))
}

// xmrSecretVerifier is what settings pins for MONERO_WALLET_SECRET: an HMAC of a fixed label, never a password
// (the password label ends in NUL and a name, which this label cannot match).
func xmrSecretVerifier(secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(xmrVerifierLabel))
	return hex.EncodeToString(m.Sum(nil))
}

// checkXMRWalletSecret refuses a MONERO_WALLET_SECRET shorter than 32 bytes or not random (checkSecret), or equal
// to SETUP_TOKEN or APP_SECRET_KEY. No refusal quotes a value.
func checkXMRWalletSecret(secret, setupToken, appSecretKey string) error {
	if err := checkSecret("MONERO_WALLET_SECRET", secret); err != nil {
		return err
	}
	if setupToken != "" && hmac.Equal([]byte(secret), []byte(setupToken)) {
		return errXMRSecretIsSetupToken
	}
	if appSecretKey != "" && hmac.Equal([]byte(secret), []byte(appSecretKey)) {
		return errXMRSecretIsAppKey
	}
	return nil
}

// xmrSeedKey is the parsed ESCROW_XMR_SEED_KEY: a public key that can be encrypted to, and its fingerprint
// (uppercase hex).
type xmrSeedKey struct {
	key *openpgp.Entity
	fpr string
}

// loadXMRSeedKey reads and parses the ESCROW_XMR_SEED_KEY file. Errors name the setting, never the path.
func loadXMRSeedKey(path string) (*xmrSeedKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errXMRSeedKeyFile
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, xmrSeedKeyMaxBytes+1))
	if err != nil || len(data) > xmrSeedKeyMaxBytes {
		return nil, errXMRSeedKeyFile
	}
	armored := string(data)
	if strings.Contains(armored, "-----BEGIN PGP PRIVATE KEY BLOCK-----") {
		return nil, errXMRSeedKeyPrivate
	}
	e, fpr, err := parsePublicKey(armored) // one unrevoked public key; refuses private key packets in a public block
	if err != nil {
		return nil, errXMRSeedKeyUnreadable
	}
	if !canReceiveEncrypted(e, time.Now()) {
		return nil, errXMRSeedKeyNoEncryption
	}
	return &xmrSeedKey{key: e, fpr: fpr}, nil
}

// sealSeed encrypts a wallet's mnemonic to the seed key and returns the armored message with the key's fingerprint,
// which the wallet row records. The error is fixed: it never carries the mnemonic.
func (k *xmrSeedKey) sealSeed(mnemonic string) (armored, fpr string, err error) {
	if strings.TrimSpace(mnemonic) == "" {
		return "", "", errXMRSeal
	}
	if armored, err = encryptTo(k.key, mnemonic); err != nil {
		return "", "", errXMRSeal
	}
	return armored, k.fpr, nil
}

// pinXMRSetting records value under key the first time and reports whether value equals the recorded one.
// Concurrent first startups agree on whichever row was written first (pinArbiterFingerprint's pattern).
func pinXMRSetting(ctx context.Context, db *sql.DB, key, value string) (bool, error) {
	if _, err := db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO NOTHING", key, value); err != nil {
		return false, err
	}
	var pinned string
	if err := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", key).Scan(&pinned); err != nil {
		return false, err
	}
	return hmac.Equal([]byte(pinned), []byte(value)), nil
}
