package market

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

func init() {
	registerAction("/logout", actionSpec{Run: func(c *actionCtx) (actionResult, error) {
		_, err := c.Tx.ExecContext(c.Ctx(), "DELETE FROM sessions WHERE token_hash=$1", digest(c.Token))
		return actionResult{Redirect: "/login", Notice: "signed-out", Audit: "Signed out", AfterCommit: c.clearSession}, err
	}})
	registerAction("/revoke-sessions", actionSpec{Run: revokeSessionsAction})
	registerAction("/account", actionSpec{OwnTx: true, Run: accountAction})
	registerAction("/account/password", actionSpec{OwnTx: true, Run: passwordChangeAction})
	registerLoader("account", accountSessionsLoader)
	registerPreview("account", func(d *PageData) {
		d.Sessions = []AccountSession{{Started: "2026-01-02 09:30 UTC", Current: true}, {Started: "2026-01-01 21:05 UTC"}}
		d.SessionsTotal = len(d.Sessions)
	})
}

func (c *actionCtx) clearSession(w http.ResponseWriter) { c.A.cookie(w, "session", "", -1) }

// revokeSessionsAction ("Sign out everywhere") ends every session of the account, this one included, and every
// sign-in in progress, so someone parked at /challenge after the password step cannot finish (A-157). The pending
// sign-ins go first, the order /challenge takes its locks in (A-154). It does not stop someone who knows the
// password from signing in again; only a password change does.
func revokeSessionsAction(c *actionCtx) (actionResult, error) {
	ctx, uid := c.Ctx(), c.User.ID
	pending, err := c.Tx.ExecContext(ctx, "DELETE FROM pending_logins WHERE user_id=$1", uid)
	if err != nil {
		return actionResult{}, err
	}
	sessions, err := c.Tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=$1", uid)
	if err != nil {
		return actionResult{}, err
	}
	p, _ := pending.RowsAffected()
	s, _ := sessions.RowsAffected()
	return actionResult{Redirect: "/login", Notice: "signed-out-all", AfterCommit: c.clearSession,
		Audit: "Revoked all sessions: ended " + strconv.FormatInt(s, 10) + " session(s) and " + strconv.FormatInt(p, 10) + " pending sign-in(s)"}, nil
}

// sessionsListed bounds the sessions /account lists: a correct password spends no sign-in budget, so someone who
// knows it can open many.
const sessionsListed = 20

// accountSessionsLoader lists the account's live sessions on /account: this browser's first (marked), then the
// newest others, at most sessionsListed, with the total in SessionsTotal. A session row holds only its token's
// hash, the account and when it started and ends: no client data (A-157).
func accountSessionsLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	rows, err := a.db.QueryContext(ctx, `SELECT to_char(created,'YYYY-MM-DD HH24:MI "UTC"'),token_hash=$2,count(*) OVER () FROM sessions
		WHERE user_id=$1 AND expires>now() ORDER BY token_hash=$2 DESC,created DESC,token_hash LIMIT $3`, d.User.ID, digest(sessionToken(r)), sessionsListed)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s AccountSession
		if err = rows.Scan(&s.Started, &s.Current, &d.SessionsTotal); err != nil {
			return err
		}
		d.Sessions = append(d.Sessions, s)
	}
	return rows.Err()
}

func accountAction(c *actionCtx) (actionResult, error) {
	f := c.Form
	pgp, xmpp := strings.TrimSpace(f.Get("pgp")), strings.TrimSpace(f.Get("xmpp"))
	if len(pgp) > 16384 || len(xmpp) > 254 {
		return actionResult{}, fail(400, "Profile fields too long")
	}
	// A-110: a photo ID (usually the owner's face) would be shown with the key to everyone who can see it, and
	// any packet other than public keys, user IDs and signatures could carry one. Refused before any password
	// is checked; nothing is saved.
	if fp, photos, other := keyExtraPackets(pgp); photos > 0 {
		return actionResult{}, fail(400, "PGP key rejected: it contains a photo ID, an image that would be shown with your key to everyone who can see it. "+
			"Remove it: run gpg --edit-key "+fp+", then at the gpg> prompt enter uid N (N is the number of the “[jpeg image of size …]” line), deluid and save. "+
			"Then export the key again with gpg --armor --export "+fp+" and paste it here.")
	} else if other > 0 {
		return actionResult{}, fail(400, "PGP key rejected: it contains an OpenPGP packet type this market does not publish. Only public keys, subkeys, user IDs and signatures are accepted; "+
			"a compressed or private packet can hide a photo ID. Export the key again with gpg --armor --export "+fp+", without any photo ID, and paste it here.")
	}
	// Adding, changing or removing the key needs the current password (and TOTP code if enrolled) on every account
	// (A-152): a session alone must not redirect buyers' shipping addresses or set up a sign-in factor.
	var old string
	if err := c.A.db.QueryRowContext(c.Ctx(), "SELECT pgp FROM users WHERE id=$1", c.User.ID).Scan(&old); err != nil {
		return actionResult{}, err
	}
	confirm := !sameProfileKey(old, pgp)
	return confirmedTx(c, confirm, func(c *actionCtx) (actionResult, error) {
		// P2: parse the key, store its fingerprint and reset ownership proof / PGP sign-in when it changes.
		audit, outcome, err := saveProfileKey(c, pgp, confirm)
		if err != nil {
			return actionResult{}, err
		}
		res := actionResult{Redirect: "/account", Notice: "profile", Audit: audit}
		if outcome != "" { // a new version of the saved key: say whether its ownership proof was kept (A-155)
			res = actionResult{Redirect: "/account?key=" + outcome, Audit: audit}
		}
		_, err = c.Tx.ExecContext(c.Ctx(), "UPDATE users SET xmpp=$1 WHERE id=$2", xmpp, c.User.ID)
		return res, err
	})
}

// passwordChangeAction changes the signed-in user's password. It needs the current password (plus a current
// authenticator code or an unused recovery code when TOTP is enrolled) and applies the registration password
// rule. The new hash is computed before the transaction, which then ends every pending sign-in and other session
// and rotates this browser's session.
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
	opts := &confirmation{Recovery: true, AfterPassword: func() (err error) { hash, err = hashPassword(next); return err },
		// Pending sign-ins end before the account row is locked, in the order /challenge takes them (A-154).
		Before: func(c *actionCtx) error {
			_, err := c.Tx.ExecContext(c.Ctx(), "DELETE FROM pending_logins WHERE user_id=$1", c.User.ID)
			return err
		}}
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
		return actionResult{Redirect: "/account", Notice: "password", Audit: "Changed password; ended " + strconv.FormatInt(n, 10) + " other session(s) and any pending sign-ins"}, nil
	})
	if err != nil {
		return actionResult{}, err
	}
	res.AfterCommit = func(w http.ResponseWriter) { c.A.cookie(w, "session", token, 43200) }
	return res, nil
}
