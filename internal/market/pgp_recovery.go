package market

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"
)

// pgpBindingProven checks possession already proved in this live browser for the
// saved key. Unlike purchase access, a subsequent expiry must not erase the
// owner's ability to replace that same binding.
func pgpBindingProven(c *actionCtx, p *pgpAccount) (bool, error) {
	if p.Canonical == "" {
		return false, nil
	}
	var proof string
	err := c.Tx.QueryRowContext(c.Ctx(), "SELECT pgp_access_key FROM sessions WHERE user_id=$1 AND token_hash=$2 AND expires>clock_timestamp() FOR SHARE", c.User.ID, digest(c.Token)).Scan(&proof)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return proof != "" && proof == digest(p.Canonical), err
}

// Account edits run only after accountAction's password and fresh enrolled-TOTP
// confirmation, under the same account lock. Initial TOTP activation on a keyed
// account also needs this binding proof, so a password thief cannot bootstrap a
// new authenticator and then replace the owner's public key.
func authorizeProfileKeyChange(c *actionCtx, p *pgpAccount) error {
	if p.Armored == "" {
		return nil
	}
	var totp bool
	if err := c.Tx.QueryRowContext(c.Ctx(), "SELECT totp_enabled FROM users WHERE id=$1", c.User.ID).Scan(&totp); err != nil {
		return err
	}
	if totp {
		return nil // confirmedTx just checked this preexisting factor
	}
	proved, err := pgpBindingProven(c, p)
	if err != nil {
		return err
	}
	if !proved {
		return fail(http.StatusForbidden, "Replacing or removing an existing PGP key requires decryption with that key in this browser, or confirmation with your previously enrolled authenticator. This also protects expired, revoked and unreadable keys. If both are unavailable, ask staff for identity-verified key recovery.")
	}
	return nil
}

func requirePGPForFirstTOTP(c *actionCtx) error {
	p, err := loadPGPAccount(c.Ctx(), c.Tx, c.User.ID, true)
	if err != nil || p.Armored == "" {
		return err
	}
	proved, err := pgpBindingProven(c, p)
	if err != nil {
		return err
	}
	if !proved {
		return fail(http.StatusForbidden, "Decrypt a challenge with your existing PGP key in this browser before enabling a first authenticator. If that key is unavailable, ask staff for identity-verified key recovery.")
	}
	return nil
}

func validateRecoveryPublicKey(public string) (canonical, fingerprint string, err error) {
	public = strings.TrimSpace(public)
	if public == "" || len(public) > 16384 {
		return "", "", fail(http.StatusBadRequest, "Supply a replacement PGP public key of at most 16 KB. Never submit a private key.")
	}
	if _, photos, other := keyExtraPackets(public); photos > 0 || other > 0 {
		return "", "", fail(http.StatusBadRequest, "The replacement must contain public key packets only, without photo IDs, private subkeys or unsupported packets.")
	}
	key, fingerprint, canonical, err := parseCanonicalPublicKey(public)
	if err == nil {
		// Authorize the key that will actually be stored, never packets that
		// canonicalization may have discarded from the submitted armor.
		key, _, err = parsePublicKey(canonical)
	}
	if err != nil || !canReceiveEncrypted(key, time.Now()) {
		return "", "", fail(http.StatusBadRequest, "The replacement must be a valid PGP public key with a usable encryption subkey. Private keys, expired keys and sign-only keys are refused.")
	}
	return canonical, fingerprint, nil
}

// errRecoveryNote refuses a key replacement without a usable verification note (A-704).
var errRecoveryNote = fail(http.StatusBadRequest, "Give a verification note of 10–500 characters, on one line, saying how you checked that the request comes from the account holder (see the operator guide, Replace a lost PGP key). Nothing was changed.")

// replacedFingerprintSQL selects, in a query on users, the fingerprint a key replacement records as the old one
// (A-704): the saved fingerprint, "unreadable key" for a saved key without one, or "none".
const replacedFingerprintSQL = "CASE WHEN pgp_fingerprint<>'' THEN pgp_fingerprint WHEN pgp<>'' THEN 'unreadable key' ELSE 'none' END"

// replaceRecoveryPublicKey is privileged recovery after an operator/administrator
// verifies identity out of band. The caller deletes pending logins before locking
// the target, holds that account lock, deletes all sessions and writes its actor
// audit in this transaction. Ownership is deliberately unverified after recovery.
func replaceRecoveryPublicKey(ctx context.Context, tx *sql.Tx, userID, public string) error {
	canonical, fingerprint, err := validateRecoveryPublicKey(public)
	if err != nil {
		return err
	}
	if err = clearPGPAccess(ctx, tx, userID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE users SET pgp=$2,pgp_fingerprint=$3,pgp_verified_at=NULL,pgp_2fa=false,pgp_changed_at=now() WHERE id=$1", userID, canonical, fingerprint)
	return err
}
