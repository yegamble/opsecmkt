package market

import "strings"

// messageBudget is how many messages an account may send in ten minutes (msg:<user>), apart from its general write
// budget (A-516).
const messageBudget = 20

func init() {
	registerAction("/messages", actionSpec{FormNonce: true, Run: messageAction})
	registerAction("/notifications", actionSpec{FormNonce: true, Run: func(c *actionCtx) (actionResult, error) {
		if c.Form.Get("mark_all") == "1" {
			result, err := c.Tx.ExecContext(c.Ctx(), "UPDATE notifications SET is_read=true WHERE user_id=$1 AND NOT is_read", c.User.ID)
			if err != nil {
				return actionResult{}, err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return actionResult{}, err
			}
			audit := ""
			if changed > 0 {
				audit = "Marked all notifications read"
			}
			return actionResult{Redirect: "/notifications", Audit: audit}, nil
		}
		_, err := c.Tx.ExecContext(c.Ctx(), "UPDATE notifications SET is_read=true WHERE id=$1 AND user_id=$2", c.Form.Get("id"), c.User.ID)
		return actionResult{Redirect: "/notifications"}, err
	}})
}

func messageAction(c *actionCtx) (actionResult, error) {
	ctx, tx, u := c.Ctx(), c.Tx, c.User
	body := strings.TrimSpace(c.Form.Get("body"))
	if len(body) < 60 || len(body) > 20000 || !strings.HasPrefix(body, "-----BEGIN PGP MESSAGE-----") || !strings.HasSuffix(body, "-----END PGP MESSAGE-----") {
		return actionResult{}, fail(400, "Encrypt the message locally and paste the complete armored PGP message (up to 20 KB).")
	}
	// Counted before the recipient lookup and packet inspection, so neither a flood of messages nor probing which
	// handles exist outlasts it (A-516). A refused send writes nothing.
	if !c.A.allowWriteBudget("msg:"+u.ID, messageBudget) {
		return actionResult{}, fail(429, "Message not sent: an account can send at most 20 messages in 10 minutes. Nothing was stored. Try again later.")
	}
	var recipient string
	err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE handle=$1", c.Form.Get("recipient")).Scan(&recipient)
	if err != nil {
		return actionResult{}, fail(400, "Recipient not found")
	}
	var restricted bool
	if err = tx.QueryRowContext(ctx, `SELECT retention_restricted_at IS NOT NULL FROM users WHERE id=$1 FOR SHARE`, u.ID).Scan(&restricted); err != nil {
		return actionResult{}, err
	}
	if restricted {
		var necessary bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$2 AND role IN('admin','moderator') AND suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL)
		 OR EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id WHERE o.financial_closed_at IS NULL AND ((o.buyer_id=$1 AND p.vendor_id=$2) OR (o.buyer_id=$2 AND p.vendor_id=$1)))`, u.ID, recipient).Scan(&necessary); err != nil {
			return actionResult{}, err
		}
		if !necessary {
			return actionResult{}, fail(403, "While deletion waits for funds, messages may go only to an existing order partner or current staff.")
		}
	}
	// P2: packet inspection rejects anything that is not OpenPGP-encrypted and records the recipient-key
	// match; only the canonical re-armor of the packets is stored (A-67).
	body, match, extraKeys, extraKeysKnown, err := inspectMessage(c, body, recipient)
	if err != nil {
		return actionResult{}, err
	}
	var extraKeysValue any
	if extraKeysKnown {
		extraKeysValue = extraKeys
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO messages(id,sender_id,recipient_id,body,encrypted,recipient_match,recipient_extra_keys) VALUES($1,$2,$3,$4,true,$5,$6)", randomToken(), u.ID, recipient, body, match, extraKeysValue)
	if err == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), recipient, "New message from "+u.Handle)
	}
	return actionResult{Redirect: "/messages", Notice: "message-sent", Audit: "Sent OpenPGP-encrypted message (recipient key match: " + match + ")"}, err
}
