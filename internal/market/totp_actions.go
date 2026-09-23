package market

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"
)

func init() {
	registerPage("totp", pageSpec{Protected: true})
	registerLoader("totp", totpLoader)
	registerLoader("account", accountSecurityLoader)
	registerLoader("pgp", accountSecurityLoader) // TOTPEnabled decides whether PGP sign-in changes ask for a code
	registerLoader("totp", pgpLoader)            // PGP sign-in decides whether activation asks for the password
	registerAction("/totp/enroll", actionSpec{Run: totpEnroll})
	registerAction("/totp/activate", actionSpec{OwnTx: true, Run: totpActivate})
	registerAction("/totp/recovery", actionSpec{Run: totpRegenerate})
	registerAction("/totp/disable", actionSpec{OwnTx: true, Run: totpDisable})
	registerPreview("totp", func(d *PageData) {
		secret := "PREVIEWSAMPLESECRETNOTREAL234567"
		d.Title = "Two-factor authentication"
		d.Security = &SecurityView{Pending: true, PendingSecret: groupSecret(secret), OTPAuthURI: otpauthURI("OPSMKT", d.User.Handle, secret)}
	})
	registerPreview("account", func(d *PageData) { d.Security = &SecurityView{} })
	registerPreview("challenge", func(d *PageData) {
		if d.Auth == nil {
			d.Auth = &AuthView{}
		}
		d.Auth.TOTPEnrolled = true
	})
}

// totpLimit bounds code guesses by a signed-in session (e.g. a stolen session trying to disable TOTP).
func totpLimit(c *actionCtx) error {
	if !c.A.allow("totp:"+c.User.ID, 10) {
		return fail(429, "Too many attempts. Try again in ten minutes.")
	}
	return nil
}

func totpEnroll(c *actionCtx) (actionResult, error) {
	ctx := c.Ctx()
	var enabled bool
	if err := c.Tx.QueryRowContext(ctx, "SELECT totp_enabled FROM users WHERE id=$1 FOR UPDATE", c.User.ID).Scan(&enabled); err != nil {
		return actionResult{}, err
	}
	if enabled {
		return actionResult{}, fail(409, "TOTP is already enabled. Turn it off before enrolling a new authenticator.")
	}
	sealed, err := c.A.seal("totp", newTOTPSecret())
	if err != nil {
		return actionResult{}, err
	}
	_, err = c.Tx.ExecContext(ctx, "UPDATE users SET totp_pending=$2 WHERE id=$1", c.User.ID, sealed)
	return actionResult{Redirect: "/totp", Audit: "Started TOTP enrollment (inactive until a code is confirmed)"}, err
}

// totpActivate turns a pending secret on. While PGP sign-in is on this adds a second factor, so it needs the
// current password (A-47); an account with no factor activates with the session and the new code alone.
func totpActivate(c *actionCtx) (actionResult, error) {
	if err := totpLimit(c); err != nil {
		return actionResult{}, err
	}
	p, err := loadPGPAccount(c.Ctx(), c.A.db, c.User.ID, false)
	if err != nil {
		return actionResult{}, err
	}
	return confirmedTx(c, p.TwoFactor, func(c *actionCtx) (actionResult, error) { return activateTOTP(c, p.TwoFactor) })
}

func activateTOTP(c *actionCtx, confirmed bool) (actionResult, error) {
	ctx := c.Ctx()
	var pending string
	var enabled bool
	if err := c.Tx.QueryRowContext(ctx, "SELECT totp_pending,totp_enabled FROM users WHERE id=$1 FOR UPDATE", c.User.ID).Scan(&pending, &enabled); err != nil {
		return actionResult{}, err
	}
	if enabled {
		return actionResult{}, fail(409, "TOTP is already enabled")
	}
	// PGP sign-in may have been turned on since the unlocked read in totpActivate; the row is locked now.
	p, err := loadPGPAccount(ctx, c.Tx, c.User.ID, false)
	if err != nil {
		return actionResult{}, err
	}
	if p.TwoFactor && !confirmed {
		return actionResult{}, fail(400, "Enter your current password to add TOTP while PGP sign-in verification is on.")
	}
	if pending == "" {
		return actionResult{}, fail(409, "Start TOTP enrollment first")
	}
	secret, err := c.A.open("totp", pending)
	if err != nil {
		return actionResult{}, fail(409, "The pending secret can no longer be read. Start enrollment again.")
	}
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return actionResult{}, err
	}
	step, ok := totpMatch(key, c.Form.Get("code"), time.Now(), 0)
	if !ok {
		return actionResult{}, fail(400, "Code incorrect. Check the secret and your device clock, then try again. TOTP remains off.")
	}
	if _, err = c.Tx.ExecContext(ctx, "UPDATE users SET totp_secret=totp_pending,totp_pending='',totp_enabled=true,totp_last_step=$2 WHERE id=$1", c.User.ID, step); err != nil {
		return actionResult{}, err
	}
	if err = c.A.issueRecoveryCodes(ctx, c.Tx, c.User.ID); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/totp", Audit: "Enabled TOTP two-factor authentication; issued 10 recovery codes"}, nil
}

func totpRegenerate(c *actionCtx) (actionResult, error) {
	if err := totpLimit(c); err != nil {
		return actionResult{}, err
	}
	ctx := c.Ctx()
	if err := c.A.checkTOTP(ctx, c.Tx, c.User.ID, c.Form.Get("code")); err != nil {
		return actionResult{}, err
	}
	if err := c.A.issueRecoveryCodes(ctx, c.Tx, c.User.ID); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/totp", Audit: "Replaced recovery codes; previous codes invalidated"}, nil
}

// totpDisable needs the password (bcrypt runs before any transaction) and a current code or recovery code.
func totpDisable(c *actionCtx) (actionResult, error) {
	if err := totpLimit(c); err != nil {
		return actionResult{}, err
	}
	a, ctx, uid := c.A, c.Ctx(), c.User.ID
	password, code := c.Form.Get("password"), c.Form.Get("code")
	if len(password) > 72 || code == "" {
		return actionResult{}, fail(400, "Enter your password and a current code or recovery code")
	}
	if err := a.confirmPassword(ctx, uid, password); err != nil {
		return actionResult{}, err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return actionResult{}, fail(503, "Service unavailable")
	}
	defer tx.Rollback()
	how := "authenticator code"
	if normalizeRecoveryCode(code) != "" {
		how = "recovery code"
		_, err = a.useRecoveryCode(ctx, tx, uid, code)
	} else {
		err = a.checkTOTP(ctx, tx, uid, code)
	}
	if err != nil {
		return actionResult{}, err
	}
	for _, q := range []string{
		"UPDATE users SET totp_enabled=false,totp_secret='',totp_pending='',totp_last_step=0,recovery_reveal='',recovery_reveal_until=NULL WHERE id=$1",
		"DELETE FROM recovery_codes WHERE user_id=$1",
		"INSERT INTO audit_events(user_id,action) VALUES($1,'Turned off TOTP two-factor authentication (confirmed with password and " + how + ")')",
	} {
		if _, err = tx.ExecContext(ctx, q, uid); err != nil {
			return actionResult{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/totp"}, nil
}

// issueRecoveryCodes replaces all of the user's recovery codes and keeps the plaintext, sealed, for one
// display on the next /totp view (at most 10 minutes).
func (a *App) issueRecoveryCodes(ctx context.Context, tx *sql.Tx, userID string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id=$1", userID); err != nil {
		return err
	}
	codes := make([]string, recoveryCodeCount)
	for i := range codes {
		codes[i] = newRecoveryCode()
		if _, err := tx.ExecContext(ctx, "INSERT INTO recovery_codes(code_hash,user_id) VALUES($1,$2)", recoveryHash(codes[i]), userID); err != nil {
			return err
		}
	}
	sealed, err := a.seal("recovery", strings.Join(codes, "\n"))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE users SET recovery_reveal=$2,recovery_reveal_until=now()+interval '10 minutes' WHERE id=$1", userID, sealed)
	return err
}

func (a *App) securityView(ctx context.Context, userID string) (*SecurityView, string, string, error) {
	s := &SecurityView{}
	var pending, reveal string
	err := a.db.QueryRowContext(ctx, `SELECT totp_enabled,totp_pending,CASE WHEN recovery_reveal_until>now() THEN recovery_reveal ELSE '' END,
		(SELECT count(*) FROM recovery_codes r WHERE r.user_id=u.id AND r.used_at IS NULL) FROM users u WHERE id=$1`, userID).Scan(&s.TOTPEnabled, &pending, &reveal, &s.RecoveryRemaining)
	s.Pending = !s.TOTPEnabled && pending != ""
	return s, pending, reveal, err
}

func accountSecurityLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	s, _, _, err := a.securityView(ctx, d.User.ID)
	d.Security = s
	return err
}

func totpLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	d.Title = "Two-factor authentication"
	s, pending, reveal, err := a.securityView(ctx, d.User.ID)
	if err != nil {
		return err
	}
	d.Security = s
	if s.Pending {
		if secret, err := a.open("totp", pending); err != nil {
			s.Error = "The pending secret can no longer be read (SETUP_TOKEN changed). Start enrollment again."
		} else {
			issuer := d.Settings["site_name"]
			if issuer == "" {
				issuer = "OPSMKT"
			}
			s.PendingSecret, s.OTPAuthURI = groupSecret(secret), otpauthURI(issuer, d.User.Handle, secret)
		}
	}
	if reveal != "" {
		// Clear first; only the request that clears the value may display it.
		res, err := a.db.ExecContext(ctx, "UPDATE users SET recovery_reveal='',recovery_reveal_until=NULL WHERE id=$1 AND recovery_reveal=$2", d.User.ID, reveal)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			if codes, err := a.open("recovery", reveal); err == nil {
				s.RecoveryCodes = strings.Split(codes, "\n")
			}
		}
	}
	return nil
}
