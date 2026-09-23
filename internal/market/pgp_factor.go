package market

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"net/http"
	"strings"
)

// P2 PGP second factor. After the password step, the user asks for a one-time code (POST
// /challenge/pgp; GET never changes state); the server encrypts a random code to the user's verified
// key and stores only its sha256 in pending_logins. POST /challenge with method=pgp compares the
// decrypted code. The pending row, and with it the code, is deleted when sign-in completes.

type pgpFactor struct{}

func init() {
	registerFactor(pgpFactor{})
	registerAction("/challenge/pgp", actionSpec{Public: true, Run: pgpLoginCodeAction})
	registerLoader("challenge", pgpChallengeLoader)
	registerPreview("challenge", func(d *PageData) {
		if d.Auth == nil {
			d.Auth = &AuthView{}
		}
		d.Auth.PGPEnrolled = true
	})
}

func (pgpFactor) Name() string { return "pgp" }

// Enrolled requires PGP sign-in to be on and ownership of the currently saved key to be proven.
func (pgpFactor) Enrolled(ctx context.Context, db *sql.DB, userID string) (bool, error) {
	p, err := loadPGPAccount(ctx, db, userID, false)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return p.TwoFactor, nil
}

// Bind the code to the exact key ownership proof. Replacing and re-verifying a
// key invalidates old codes, including when the same key is later restored.
func pgpLoginDigest(code string, p *pgpAccount) string {
	return digest(code + "\x00" + p.Fingerprint + "\x00" + p.ProofVersion)
}

func (pgpFactor) Verify(c *actionCtx, userID string) error {
	p, err := loadPGPAccount(c.Ctx(), c.Tx, userID, true)
	if err != nil {
		return err
	}
	if !p.TwoFactor {
		return fail(401, "PGP sign-in verification is no longer enabled. Sign in again.")
	}
	var hash sql.NullString
	err = c.Tx.QueryRowContext(c.Ctx(), "SELECT pgp_nonce FROM pending_logins WHERE token_hash=$1 AND user_id=$2", digest(pendingCookie(c.R)), userID).Scan(&hash)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if !hash.Valid {
		return fail(401, "Create an encrypted sign-in code first, then paste the code it contains.")
	}
	answer := strings.ToLower(strings.TrimSpace(c.Form.Get("pgp_code")))
	if answer == "" || subtle.ConstantTimeCompare([]byte(pgpLoginDigest(answer, p)), []byte(hash.String)) != 1 {
		return fail(401, "The decrypted code does not match.")
	}
	return nil
}

// pgpLoginCodeAction encrypts a fresh one-time code to the pending user's verified key.
func pgpLoginCodeAction(c *actionCtx) (actionResult, error) {
	ctx, tx := c.Ctx(), c.Tx
	pending := pendingCookie(c.R)
	if pending == "" {
		return actionResult{}, fail(401, "Sign-in verification expired. Sign in again.")
	}
	var userID string
	err := tx.QueryRowContext(ctx, "SELECT user_id FROM pending_logins WHERE token_hash=$1 AND expires>now() FOR UPDATE", digest(pending)).Scan(&userID)
	if err == sql.ErrNoRows {
		return actionResult{}, fail(401, "Sign-in verification expired. Sign in again.")
	}
	if err != nil {
		return actionResult{}, err
	}
	if !c.A.allow("pgp-login:"+digest(pending), 10) { // only after the pending login exists
		return actionResult{}, fail(429, "Too many codes requested. Try again in ten minutes.")
	}
	p, err := loadPGPAccount(ctx, tx, userID, true)
	if err != nil {
		return actionResult{}, err
	}
	if !p.TwoFactor {
		return actionResult{}, fail(400, "PGP sign-in verification is not enabled for this account")
	}
	code := randomToken()[:32]
	armored, err := encryptTo(p.Key, code)
	if err != nil {
		return actionResult{}, fail(409, "A sign-in code cannot be encrypted to your key (expired or sign-only). Use another verification method.")
	}
	_, err = tx.ExecContext(ctx, "UPDATE pending_logins SET pgp_nonce=$1,pgp_challenge=$2 WHERE token_hash=$3", pgpLoginDigest(code, p), armored, digest(pending))
	return actionResult{Redirect: "/challenge"}, err
}

// pgpChallengeLoader shows the encrypted code for this pending login, if one was requested.
func pgpChallengeLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	pending := pendingCookie(r)
	if pending == "" {
		return nil
	}
	var armored sql.NullString
	err := a.db.QueryRowContext(ctx, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1 AND expires>now()", digest(pending)).Scan(&armored)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	d.PGP = &PGPView{EncryptedChallenge: armored.String}
	return nil
}
