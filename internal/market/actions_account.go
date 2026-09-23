package market

import (
	"net/http"
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
	registerAction("/account", actionSpec{Run: accountAction})
}

func (c *actionCtx) clearSession(w http.ResponseWriter) { c.A.cookie(w, "session", "", -1) }

func accountAction(c *actionCtx) (actionResult, error) {
	f := c.Form
	pgp, xmpp := strings.TrimSpace(f.Get("pgp")), strings.TrimSpace(f.Get("xmpp"))
	if len(pgp) > 16384 || len(xmpp) > 254 {
		return actionResult{}, fail(400, "Profile fields too long")
	}
	// P2: parse the key, store its fingerprint and reset ownership proof / PGP sign-in when it changes.
	audit, err := saveProfileKey(c, pgp)
	if err != nil {
		return actionResult{}, err
	}
	_, err = c.Tx.ExecContext(c.Ctx(), "UPDATE users SET xmpp=$1 WHERE id=$2", xmpp, c.User.ID)
	return actionResult{Redirect: "/account?saved=1", Audit: audit}, err
}
