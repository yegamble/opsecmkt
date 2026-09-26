package market

import (
	"context"
	"database/sql"
	"net/url"
	"strconv"
	"time"
)

func init() { registerFactor(totpFactor{}) }

// totpFactor is the "totp" second factor: a current authenticator code or one unused recovery code.
type totpFactor struct{}

func (totpFactor) Name() string { return "totp" }

func (totpFactor) Enrolled(ctx context.Context, q rowQuerier, userID string) (bool, error) {
	var on bool
	err := q.QueryRowContext(ctx, "SELECT totp_enabled FROM users WHERE id=$1", userID).Scan(&on)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return on, err
}

func (totpFactor) Verify(c *actionCtx, userID string) error {
	// Per account, independent of the pending-login token, so repeated password logins do not multiply guesses.
	if !c.A.allow("totp-verify:"+userID, 10) {
		return fail(429, "Too many verification attempts. Try again in ten minutes.")
	}
	ctx, tx := c.Ctx(), c.Tx
	if code := c.Form.Get("recovery"); code != "" {
		off := turnOffTOTPAtSignIn(c.Form)
		if off { // all the codes are deleted below: lock them first, before the account row (A-154)
			if err := lockRecoveryCodes(ctx, tx, userID); err != nil {
				return err
			}
		}
		left, err := c.A.useRecoveryCode(ctx, tx, userID, code)
		if err != nil {
			return err
		}
		n := strconv.Itoa(left)
		if off {
			// A-154: someone who lost the authenticator turns TOTP off with this code, as /totp/disable allows with the
			// password and a recovery code; a suspended account's sign-in fails later and rolls all of it back.
			if err = turnOffTOTP(ctx, tx, userID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", userID, "TOTP turned off at sign-in with a recovery code ("+n+" remaining)"); err != nil {
				return err
			}
			return notifyOwner(ctx, tx, userID, "A recovery code was used to sign in to your account and two-step sign-in with an authenticator app (TOTP) was turned off and its recovery codes were deleted ("+n+" remaining).")
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", userID, "Used a recovery code to sign in ("+n+" remaining)"); err != nil {
			return err
		}
		return notifyOwner(ctx, tx, userID, "A recovery code was used to sign in to your account ("+n+" remaining).")
	}
	return c.A.checkTOTP(ctx, tx, userID, c.Form.Get("code"))
}

// turnOffTOTPAtSignIn reports whether a /challenge recovery-code sign-in also asks to turn TOTP off (the "I lost my
// authenticator" checkbox). It is ignored with an authenticator code.
func turnOffTOTPAtSignIn(f url.Values) bool {
	return f.Get("recovery") != "" && f.Get("turn_off_totp") != ""
}

var errTOTPWrong = fail(401, "Verification code incorrect, expired or already used")

// checkTOTP verifies code against the user's active secret inside tx and advances the replay guard.
func (a *App) checkTOTP(ctx context.Context, tx *sql.Tx, userID, code string) error {
	var sealed string
	var enabled bool
	var last int64
	err := tx.QueryRowContext(ctx, "SELECT totp_secret,totp_enabled,totp_last_step FROM users WHERE id=$1 FOR NO KEY UPDATE", userID).Scan(&sealed, &enabled, &last)
	if err != nil {
		return err
	}
	if !enabled || sealed == "" {
		return fail(409, "TOTP is not enabled for this account")
	}
	secret, err := a.open("totp", sealed)
	if err != nil {
		return fail(500, "TOTP secret cannot be read on this server; SETUP_TOKEN may have changed. Use a recovery code.")
	}
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return err
	}
	step, ok := totpMatch(key, code, time.Now(), last)
	if !ok {
		return errTOTPWrong
	}
	_, err = tx.ExecContext(ctx, "UPDATE users SET totp_last_step=$2 WHERE id=$1", userID, step)
	return err
}

// useRecoveryCode marks one unused recovery code as used inside tx and returns how many remain.
func (a *App) useRecoveryCode(ctx context.Context, tx *sql.Tx, userID, code string) (int, error) {
	if normalizeRecoveryCode(code) == "" {
		return 0, fail(401, "Recovery code incorrect or already used")
	}
	res, err := tx.ExecContext(ctx, "UPDATE recovery_codes SET used_at=now() WHERE user_id=$1 AND code_hash=$2 AND used_at IS NULL", userID, recoveryHash(code))
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, fail(401, "Recovery code incorrect or already used")
	}
	var left int
	err = tx.QueryRowContext(ctx, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND used_at IS NULL", userID).Scan(&left)
	return left, err
}
