package market

import "strings"

func init() {
	registerAction("/messages", actionSpec{Run: messageAction})
	registerAction("/notifications", actionSpec{Run: func(c *actionCtx) (actionResult, error) {
		_, err := c.Tx.ExecContext(c.Ctx(), "UPDATE notifications SET is_read=true WHERE id=$1 AND user_id=$2", c.Form.Get("id"), c.User.ID)
		return actionResult{Redirect: "/notifications", Audit: "Marked notification read"}, err
	}})
}

func messageAction(c *actionCtx) (actionResult, error) {
	ctx, tx, u := c.Ctx(), c.Tx, c.User
	body := strings.TrimSpace(c.Form.Get("body"))
	if len(body) < 60 || len(body) > 20000 || !strings.HasPrefix(body, "-----BEGIN PGP MESSAGE-----") || !strings.HasSuffix(body, "-----END PGP MESSAGE-----") {
		return actionResult{}, fail(400, "Encrypt the message locally and paste the complete armored PGP message (up to 20 KB).")
	}
	var recipient string
	err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE handle=$1", c.Form.Get("recipient")).Scan(&recipient)
	if err != nil {
		return actionResult{}, fail(400, "Recipient not found")
	}
	// P2: packet inspection rejects anything that is not OpenPGP-encrypted and records the recipient-key
	// match; only the canonical re-armor of the packets is stored (A-67).
	body, match, err := inspectMessage(c, body, recipient)
	if err != nil {
		return actionResult{}, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO messages(id,sender_id,recipient_id,body,encrypted,recipient_match) VALUES($1,$2,$3,$4,true,$5)", randomToken(), u.ID, recipient, body, match)
	if err == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), recipient, "New message from "+u.Handle)
	}
	return actionResult{Redirect: "/messages?saved=1", Audit: "Sent OpenPGP-encrypted message (recipient key match: " + match + ")"}, err
}
