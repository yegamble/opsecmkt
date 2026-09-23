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
	if pgp != "" && (!strings.HasPrefix(pgp, "-----BEGIN PGP PUBLIC KEY BLOCK-----") || !strings.HasSuffix(pgp, "-----END PGP PUBLIC KEY BLOCK-----")) {
		return actionResult{}, fail(400, "Paste an armored public key only. Never upload your private key.")
	}
	_, err := c.Tx.ExecContext(c.Ctx(), "UPDATE users SET pgp=$1,xmpp=$2 WHERE id=$3", pgp, xmpp, c.User.ID)
	return actionResult{Redirect: "/account?saved=1", Audit: "Updated profile (key not cryptographically verified)"}, err
}
