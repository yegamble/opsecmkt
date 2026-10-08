package market

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strconv"
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
	// Activation issues recovery codes, so it locks them before the account row, like every such change (A-154).
	return confirmedTxWith(c, &confirmation{Before: lockOwnRecoveryCodes}, activateTOTP)
}

// lockOwnRecoveryCodes is a confirmation.Before for a change that replaces or deletes the signed-in user's codes.
func lockOwnRecoveryCodes(c *actionCtx) error { return lockRecoveryCodes(c.Ctx(), c.Tx, c.User.ID) }

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
	return confirmedTxWith(c, &confirmation{Before: lockOwnRecoveryCodes}, func(c *actionCtx) (actionResult, error) {
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

// totpDisable needs the password (bcrypt runs before any transaction) and a current code or recovery code; a wrong
// code after the right password spends the confirmation budget, as in confirmedTxWith (A-274).
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
	// The recovery codes are locked before the account row, in the order /challenge takes them (A-154).
	if err = lockRecoveryCodes(ctx, tx, uid); err != nil {
		return actionResult{}, err
	}
	var enabled, suspended bool
	var role string
	if err = tx.QueryRowContext(ctx, "SELECT totp_enabled,role,suspended_at IS NOT NULL FROM users WHERE id=$1 FOR NO KEY UPDATE", uid).Scan(&enabled, &role, &suspended); err != nil {
		if err == sql.ErrNoRows {
			return actionResult{}, errAccountChanged
		}
		return actionResult{}, err
	}
	if role != c.User.Role || suspended {
		return actionResult{}, errAccountChanged
	}
	if !enabled {
		return actionResult{}, fail(409, "TOTP is not enabled for this account")
	}
	// The code is checked under the account row lock, as a confirmation's code is: a wrong one spends the confirmation
	// budget the correct password gave back (A-274, QA19-2).
	how := "authenticator code"
	if normalizeRecoveryCode(code) != "" {
		how = "recovery code"
		err = a.confirmCode(uid, func() error { _, err := a.useRecoveryCode(ctx, tx, uid, code); return err })
	} else {
		err = a.confirmCode(uid, func() error { return a.checkTOTP(ctx, tx, uid, code) })
	}
	if err != nil {
		return actionResult{}, err
	}
	if err = turnOffTOTP(ctx, tx, uid); err != nil {
		return actionResult{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", uid, "Turned off TOTP two-factor authentication (confirmed with password and "+how+")"); err != nil {
		return actionResult{}, err
	}
	if err = notifyOwner(ctx, tx, uid, "Two-step sign-in with an authenticator app (TOTP) was turned off for your account and its recovery codes were deleted."); err != nil {
		return actionResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return actionResult{}, commitOutcomeUnknown{err}
	}
	return actionResult{Redirect: "/totp"}, nil
}

// turnOffTOTP turns the user's TOTP off inside tx: its secret, pending secret, step counter, recovery codes and any
// unrevealed codes go. The codes are deleted before the account row is updated (the A-154 lock order).
func turnOffTOTP(ctx context.Context, tx *sql.Tx, userID string) error {
	for _, q := range []string{
		"DELETE FROM recovery_codes WHERE user_id=$1",
		"UPDATE users SET totp_enabled=false,totp_secret='',totp_pending='',totp_last_step=0,recovery_reveal='',recovery_reveal_until=NULL WHERE id=$1",
	} {
		if _, err := tx.ExecContext(ctx, q, userID); err != nil {
			return err
		}
	}
	return nil
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

// recoverySignInNotice is what /totp says after a recovery-code sign-in (/challenge lands there, A-154): how many
// codes remain and what to do without the authenticator, or that TOTP was turned off. signin is "recovery" or
// "totp-off", from the signin- notice /totp consumed (A-287). It matches the current state only; when that changed
// meanwhile it returns "" and the notice's own text stays. staffContact is the staff-contact line (A-119, A-279) for
// the last code.
func recoverySignInNotice(signin string, s *SecurityView, staffContact string) string {
	switch {
	case signin == "totp-off" && !s.TOTPEnabled:
		return "You signed in with a recovery code and turned off two-step sign-in with an authenticator app (TOTP); your other recovery codes were deleted. Set up TOTP again below with your new authenticator."
	case signin != "recovery" || !s.TOTPEnabled:
		return ""
	case s.RecoveryRemaining == 0:
		return "You signed in with a recovery code. 0 recovery codes remain. Without your authenticator you cannot turn TOTP off yourself: ask the market staff for help before you sign out. " + staffContact
	case s.RecoveryRemaining == 1:
		return "You signed in with a recovery code. 1 recovery code remains. If you lost your authenticator, turn TOTP off below with your password and that code, then set it up again."
	}
	return "You signed in with a recovery code. " + strconv.Itoa(s.RecoveryRemaining) + " recovery codes remain. If you lost your authenticator, turn TOTP off below with your password and one of them, then set it up again."
}

func totpLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	d.Title = "Two-factor authentication"
	s, pending, reveal, err := a.securityView(ctx, d.User.ID)
	if err != nil {
		return err
	}
	d.Security = s
	// A recovery-code sign-in's one-shot notice (A-287), detailed from the current state.
	if signin, ok := strings.CutPrefix(d.noticeCode, "signin-"); ok {
		if n := recoverySignInNotice(signin, s, staffContactSentence(d.Settings[settingStaffContact])); n != "" {
			d.Notice = n
		}
		// The page the sign-in was for (A-126): a link on, only beside the notice. next is kept in the URL: it
		// selects the link's target (checked again by signInNext), not an outcome, and shows nothing without the
		// notice, so a crafted or replayed /totp?next= shows no link.
		if d.Next = a.signInNext(r.URL.Query().Get("next")); d.Next != "" {
			d.NextTitle = nextTitle(d.Next)
		}
	}
	if s.Pending {
		if secret, err := a.open("totp", pending); err != nil {
			s.Error = "The pending secret can no longer be read (the server key changed). Start enrollment again."
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
