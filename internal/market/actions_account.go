package market

import (
	"net/http"
	"strconv"
	"strings"
)

func init() {
	registerAction("/logout", actionSpec{Run: func(c *actionCtx) (actionResult, error) {
		_, err := c.Tx.ExecContext(c.Ctx(), "DELETE FROM sessions WHERE token_hash=$1", digest(c.Token))
		return actionResult{Redirect: "/login", Audit: "Signed out", AfterCommit: c.clearSession}, err
	}})
	registerAction("/revoke-sessions", actionSpec{Run: func(c *actionCtx) (actionResult, error) {
		_, err := c.Tx.ExecContext(c.Ctx(), "DELETE FROM sessions WHERE user_id=$1", c.User.ID)
		return actionResult{Redirect: "/login", Audit: "Revoked all sessions", AfterCommit: c.clearSession}, err
	}})
	registerAction("/account", actionSpec{OwnTx: true, Run: accountAction})
	registerAction("/account/password", actionSpec{OwnTx: true, Run: passwordChangeAction})
}

func (c *actionCtx) clearSession(w http.ResponseWriter) { c.A.cookie(w, "session", "", -1) }

func accountAction(c *actionCtx) (actionResult, error) {
	f := c.Form
	pgp, xmpp := strings.TrimSpace(f.Get("pgp")), strings.TrimSpace(f.Get("xmpp"))
	if len(pgp) > 16384 || len(xmpp) > 254 {
		return actionResult{}, fail(400, "Profile fields too long")
	}
	// Changing or removing the key while PGP sign-in or TOTP is on needs the current password (and TOTP code if
	// enrolled), so a session alone cannot swap in a key and then add it as a sign-in factor.
	var old string
	var twoFA, totp bool
	if err := c.A.db.QueryRowContext(c.Ctx(), "SELECT pgp,pgp_2fa,totp_enabled FROM users WHERE id=$1", c.User.ID).Scan(&old, &twoFA, &totp); err != nil {
		return actionResult{}, err
	}
	confirm := (twoFA || totp) && !sameProfileKey(old, pgp)
	return confirmedTx(c, confirm, func(c *actionCtx) (actionResult, error) {
		// P2: parse the key, store its fingerprint and reset ownership proof / PGP sign-in when it changes.
		audit, err := saveProfileKey(c, pgp, confirm)
		if err != nil {
			return actionResult{}, err
		}
		_, err = c.Tx.ExecContext(c.Ctx(), "UPDATE users SET xmpp=$1 WHERE id=$2", xmpp, c.User.ID)
		return actionResult{Redirect: "/account?saved=1", Audit: audit}, err
	})
}

// passwordChangeAction changes the signed-in user's password. It needs the current password (plus a current
// authenticator code or an unused recovery code when TOTP is enrolled) and applies the registration password
// rule. The new hash is computed before the transaction, which then ends every other session and pending
// sign-in and rotates this browser's session.
func passwordChangeAction(c *actionCtx) (actionResult, error) {
	next := c.Form.Get("new_password")
	if !validPassword(next) {
		return actionResult{}, fail(400, "Use a new password of 12–72 bytes.")
	}
	if next != c.Form.Get("new_password_confirm") {
		return actionResult{}, fail(400, "The new passwords do not match.")
	}
	if next == c.Form.Get("password") {
		return actionResult{}, fail(400, "Choose a new password that differs from your current one.")
	}
	var hash, token string
	opts := &confirmation{Recovery: true, AfterPassword: func() (err error) { hash, err = hashPassword(next); return err }}
	res, err := confirmedTxWith(c, opts, func(c *actionCtx) (actionResult, error) {
		ctx, tx, uid := c.Ctx(), c.Tx, c.User.ID
		if _, err := tx.ExecContext(ctx, "UPDATE users SET password_hash=$2 WHERE id=$1", uid, hash); err != nil {
			return actionResult{}, err
		}
		ended, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=$1 AND token_hash<>$2", uid, digest(c.Token))
		if err != nil {
			return actionResult{}, err
		}
		n, _ := ended.RowsAffected()
		if _, err = tx.ExecContext(ctx, "DELETE FROM pending_logins WHERE user_id=$1", uid); err != nil {
			return actionResult{}, err
		}
		// Rotate this browser's session: the old token stops working and the new cookie is set after commit.
		token = randomToken()
		// confirmedTxWith locks the account row, so concurrent changes run one at a time; one that committed
		// first has ended this session, and this change must not proceed on a revoked session.
		own, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash=$1", digest(c.Token))
		if err != nil {
			return actionResult{}, err
		}
		if gone, _ := own.RowsAffected(); gone != 1 {
			return actionResult{}, fail(401, "Your session ended while changing the password. Sign in again.")
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '12 hours')", digest(token), uid); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/account?saved=1", Audit: "Changed password; ended " + strconv.FormatInt(n, 10) + " other session(s) and any pending sign-ins"}, nil
	})
	if err != nil {
		return actionResult{}, err
	}
	res.AfterCommit = func(w http.ResponseWriter) { c.A.cookie(w, "session", token, 43200) }
	return res, nil
}
