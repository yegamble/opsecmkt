package market

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"
)

const settingAllowLoginWithoutPGP = "allow_login_without_pgp"

const pgpAccessRequiredMessage = "Save a usable PGP public key on your account and decrypt a fresh challenge on the PGP verification page before purchasing or sending messages. This browser has not completed PGP authentication."

func init() {
	registerLoader("*", loadPGPAccess)
	registerAction("/admin/pgp-access", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: pgpAccessPolicyAction})
	registerPreview("*", func(d *PageData) { d.PGPAccessVerified = d.User != nil })
}

// Existing installations retain browse-only sign-ins. Missing or malformed policy
// fails closed; the migration installs the explicit, administrator-editable default.
func pgpLoginRequired(ctx context.Context, q rowQuerier) (bool, error) {
	var required bool
	err := q.QueryRowContext(ctx, "SELECT COALESCE((SELECT value FROM settings WHERE key=$1),'false')<>'true'", settingAllowLoginWithoutPGP).Scan(&required)
	return required, err
}

func pgpLoginRequiredLocked(ctx context.Context, tx *sql.Tx) (bool, error) {
	var value string
	err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1 FOR SHARE", settingAllowLoginWithoutPGP).Scan(&value)
	if err == sql.ErrNoRows {
		return true, nil
	}
	return value != "true", err
}

// pgpAccessVerified is a view-only check. Mutations use requirePGPAccess so a key
// replacement, sign-out or expiry cannot race the final authorization.
func pgpAccessVerified(ctx context.Context, q rowQuerier, userID, token string) (bool, error) {
	return pgpAccessState(ctx, q, userID, token, false)
}

func pgpAccessState(ctx context.Context, q rowQuerier, userID, token string, lock bool) (bool, error) {
	query := "SELECT u.pgp,s.pgp_access_key FROM users u JOIN sessions s ON s.user_id=u.id WHERE u.id=$1 AND s.token_hash=$2 AND s.expires>clock_timestamp() AND u.suspended_at IS NULL AND u.closed_at IS NULL AND u.banned_at IS NULL"
	if lock {
		query += " FOR SHARE OF u,s"
	}
	var armored, proof string
	err := q.QueryRowContext(ctx, query, userID, digest(token)).Scan(&armored, &proof)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	key, _, canonical, err := parseCanonicalPublicKey(armored)
	return err == nil && canReceiveEncrypted(key, time.Now()) && proof != "" && proof == digest(canonical), nil
}

func requirePGPAccess(c *actionCtx) error {
	if c.User == nil || c.Tx == nil {
		return fail(http.StatusForbidden, pgpAccessRequiredMessage)
	}
	ok, err := pgpAccessState(c.Ctx(), c.Tx, c.User.ID, c.Token, true)
	if err != nil {
		return err
	}
	if !ok {
		return fail(http.StatusForbidden, pgpAccessRequiredMessage)
	}
	return nil
}

// grantPGPAccess is called only after a successful session-bound decryption, or
// a PGP sign-in challenge verified in the same transaction. Signatures alone do
// not authorize purchasing or messages. No private-key material is accepted.
func grantPGPAccess(ctx context.Context, tx *sql.Tx, userID, token string) error {
	p, err := loadPGPAccount(ctx, tx, userID, true)
	if err != nil {
		return err
	}
	if p.Key == nil || !canReceiveEncrypted(p.Key, time.Now()) {
		return fail(http.StatusConflict, "Your current public key cannot receive encrypted messages. Save a usable public key and start a new decryption challenge.")
	}
	result, err := tx.ExecContext(ctx, "UPDATE sessions SET pgp_access_key=$3 WHERE user_id=$1 AND token_hash=$2 AND expires>clock_timestamp()", userID, digest(token), digest(p.Canonical))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fail(http.StatusUnauthorized, "Your session expired. Sign in and decrypt a new challenge.")
	}
	_, err = tx.ExecContext(ctx, "UPDATE users SET pgp_fingerprint=$2,pgp_verified_at=CASE WHEN pgp_fingerprint=$2 AND pgp_verified_at IS NOT NULL THEN pgp_verified_at ELSE now() END WHERE id=$1", userID, p.Fingerprint)
	return err
}

func clearPGPAccess(ctx context.Context, tx *sql.Tx, userID string) error {
	// A generation counter also invalidates pending login codes if an unverified
	// key is replaced and later restored. Do not take pending-login locks here:
	// verification locks its pending row before this account row.
	if _, err := tx.ExecContext(ctx, "UPDATE users SET pgp_key_generation=pgp_key_generation+1 WHERE id=$1", userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET pgp_access_key='' WHERE user_id=$1 AND pgp_access_key<>''", userID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM pgp_challenges WHERE user_id=$1", userID)
	return err
}

func loadPGPAccess(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	ok, err := pgpAccessVerified(ctx, a.db, d.User.ID, sessionToken(r))
	d.PGPAccessVerified = ok
	return err
}

func pgpAccessPolicyAction(c *actionCtx) (actionResult, error) {
	value := c.Form.Get(settingAllowLoginWithoutPGP)
	if value != "true" && value != "false" {
		return actionResult{}, fail(http.StatusBadRequest, "Choose whether to allow browse-only sign-in without PGP.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		// An administrator must first demonstrate that their own key works before
		// making it mandatory for subsequent sign-ins.
		if value == "false" {
			if err := requirePGPAccess(c); err != nil {
				return actionResult{}, err
			}
		}
		if _, err := c.Tx.ExecContext(c.Ctx(), "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value", settingAllowLoginWithoutPGP, value); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/admin/security#pgp-access", Audit: "Set browse-only sign-in without PGP to " + value}, nil
	})
}

func pgpAccessChallengeText(handle, market string, addresses []string, privateAdmin bool, requested, expires time.Time, code string) string {
	text := pgpSignInText(handle, market, addresses, privateAdmin, requested, expires, code)
	text = strings.Replace(text, "Sign-in code for ", "PGP authentication code for ", 1)
	text = strings.Replace(text, "valid for at most 10 minutes", "valid for at most 30 minutes", 1)
	return "Purpose: authorize purchases and messages in the browser that requested this code.\n" + text
}
