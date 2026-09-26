package market

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"
	"time"
)

func init() {
	registerPage("totp", pageSpec{Protected: true})
	registerLoader("totp", totpLoader)
	registerLoader("account", accountSecurityLoader)
	registerLoader("pgp", accountSecurityLoader) // TOTPEnabled decides whether PGP sign-in changes ask for a code
	registerAction("/totp/enroll", actionSpec{Run: totpEnroll})
	registerAction("/totp/activate", actionSpec{OwnTx: true, Run: totpActivate})
	registerAction("/totp/recovery", actionSpec{OwnTx: true, Run: totpRegenerate})
	registerAction("/totp/disable", actionSpec{OwnTx: true, Run: totpDisable})
	registerBackground(recoveryRevealSweeper)
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

// totpActivate turns a pending secret on. It always needs the current password (A-152), also on an account with no
// factor yet: otherwise a stolen session could add a factor only the thief holds and lock the owner out even after
// they sign out every session.
func totpActivate(c *actionCtx) (actionResult, error) {
	if err := totpLimit(c); err != nil {
		return actionResult{}, err
	}
	var enabled bool
	if err := c.A.db.QueryRowContext(c.Ctx(), "SELECT totp_enabled FROM users WHERE id=$1", c.User.ID).Scan(&enabled); err != nil {
		return actionResult{}, err
	}
	if enabled { // before the password check; activateTOTP re-checks under the row lock
		return actionResult{}, fail(409, "TOTP is already enabled")
	}
	return confirmedTx(c, true, activateTOTP)
}

func activateTOTP(c *actionCtx) (actionResult, error) {
	ctx := c.Ctx()
	var pending string
	var enabled bool
	if err := c.Tx.QueryRowContext(ctx, "SELECT totp_pending,totp_enabled FROM users WHERE id=$1 FOR UPDATE", c.User.ID).Scan(&pending, &enabled); err != nil {
		return actionResult{}, err
	}
	if enabled {
		return actionResult{}, fail(409, "TOTP is already enabled")
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
	if err = notifyOwner(ctx, c.Tx, c.User.ID, "Two-step sign-in with an authenticator app (TOTP) was turned on for your account and 10 new recovery codes were issued."); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/totp", Audit: "Enabled TOTP two-factor authentication; issued 10 recovery codes"}, nil
}

// totpRegenerate replaces the recovery codes. It needs the current password and a current authenticator code
// (confirmedTx checks the code because TOTP is on), so a stolen session cannot take the codes (A-152).
func totpRegenerate(c *actionCtx) (actionResult, error) {
	if err := totpLimit(c); err != nil {
		return actionResult{}, err
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		var enabled bool
		// confirmedTx has locked the row; without TOTP it checked no code, and there are no codes to replace.
		if err := c.Tx.QueryRowContext(ctx, "SELECT totp_enabled FROM users WHERE id=$1", c.User.ID).Scan(&enabled); err != nil {
			return actionResult{}, err
		}
		if !enabled {
			return actionResult{}, fail(409, "TOTP is not enabled for this account")
		}
		if err := c.A.issueRecoveryCodes(ctx, c.Tx, c.User.ID); err != nil {
			return actionResult{}, err
		}
		if err := notifyOwner(ctx, c.Tx, c.User.ID, "Your recovery codes were replaced; the previous codes no longer work."); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/totp", Audit: "Replaced recovery codes; previous codes invalidated"}, nil
	})
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
	if err = notifyOwner(ctx, tx, uid, "Two-step sign-in with an authenticator app (TOTP) was turned off for your account and its recovery codes were deleted."); err != nil {
		return actionResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/totp"}, nil
}

// issueRecoveryCodes replaces all of the user's recovery codes and keeps the plaintext, sealed, for one
// display on the next /totp view (at most 10 minutes; recoveryRevealSweeper clears it afterwards).
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

// revealExpired matches users whose sealed recovery codes are past their display window.
const revealExpired = "recovery_reveal<>'' AND (recovery_reveal_until IS NULL OR recovery_reveal_until<=now())"

// recoveryRevealSweepInterval is how often every running process clears expired reveals, so sealed plaintext
// codes outlive their 10-minute window by at most this long while the app runs (and until its next start
// otherwise). A variable only so tests can shorten it.
var recoveryRevealSweepInterval = time.Minute

func recoveryRevealSweeper(ctx context.Context, a *App) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := a.sweepRecoveryReveals(ctx); err != nil && ctx.Err() == nil {
			log.Printf("recovery code reveal sweep: %s", errorCause(err)) // a class, never the driver's text (A-170)
		}
		t.Reset(recoveryRevealSweepInterval)
	}
}

// sweepRecoveryReveals clears every expired reveal; a reveal still within its window is untouched.
func (a *App) sweepRecoveryReveals(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := a.db.ExecContext(ctx, "UPDATE users SET recovery_reveal='',recovery_reveal_until=NULL WHERE "+revealExpired)
	return err
}

// securityView also clears the account's reveal once it has expired (on /account, /pgp, /admin and /totp).
func (a *App) securityView(ctx context.Context, userID string) (*SecurityView, string, string, error) {
	s := &SecurityView{}
	var pending, reveal string
	var expired bool
	err := a.db.QueryRowContext(ctx, `SELECT totp_enabled,totp_pending,CASE WHEN recovery_reveal_until>now() THEN recovery_reveal ELSE '' END,`+revealExpired+`,
		(SELECT count(*) FROM recovery_codes r WHERE r.user_id=u.id AND r.used_at IS NULL) FROM users u WHERE id=$1`, userID).Scan(&s.TOTPEnabled, &pending, &reveal, &expired, &s.RecoveryRemaining)
	s.Pending = !s.TOTPEnabled && pending != ""
	if err == nil && expired {
		_, err = a.db.ExecContext(ctx, "UPDATE users SET recovery_reveal='',recovery_reveal_until=NULL WHERE id=$1 AND "+revealExpired, userID)
	}
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
