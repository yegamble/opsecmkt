package market

import (
	"context"
	"database/sql"
	"strings"
)

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
	if err := requirePGPAccess(c); err != nil {
		return actionResult{}, err
	}
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
	// An unknown handle and one this sender may not write to are refused alike, so the refusal tells nothing about
	// which handles exist (A-543).
	recipient, err := messageRecipient(ctx, tx, u, c.Form.Get("recipient"))
	if err != nil {
		return actionResult{}, err
	}
	if recipient == "" {
		return actionResult{}, fail(403, messageNotPermitted+" Nothing was stored.")
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
	// match; only the canonical re-armor of the packets is stored (A-67). A-569: a recipient without a usable key,
	// passphrase-only ciphertext and ciphertext naming only other keys are refused; hidden recipients need the
	// sender's confirmation.
	body, match, extraKeys, extraKeysKnown, err := inspectMessage(c, body, recipient, c.Form.Get("recipient"))
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

// messageNotPermitted is shared by sends and recipient-key lookups. Unknown handles and accounts the sender
// cannot contact answer alike, so neither route exposes a directory of private buyers.
const messageNotPermitted = "No account you can message has this handle. Contact an available vendor, your existing vendor or customer, or listed market staff for support."

// messageRecipient returns an authorized recipient ID, or an empty string. Staff can contact any unbanned account.
// Other accounts contact a public vendor or eligible staff; a vendor also answers known customers. An old inbound
// message never grants buyer-to-buyer access. Hidden vendors remain reachable by existing buyers only when they
// allow messages while away, and that preference applies even when an order or earlier message exists.
// q is the send transaction or, for the /messages?to= key lookup, the database.
func messageRecipient(ctx context.Context, q rowQuerier, sender *User, handle string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT r.id FROM users r WHERE r.handle=$2 AND r.id<>$1 AND r.banned_at IS NULL AND ($3::boolean
		OR (r.role='vendor' AND r.suspended_at IS NULL AND r.closed_at IS NULL AND r.retention_restricted_at IS NULL
			AND `+vendorAcceptsBuyerMessagesSQL("r")+`
			AND (`+activeVendorSQL("r")+`
				OR ($4='buyer' AND (
					EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id WHERE o.buyer_id=$1 AND p.vendor_id=r.id)
					OR EXISTS(SELECT 1 FROM messages m WHERE m.recipient_id=$1 AND m.sender_id=r.id)))))
		OR ($4='vendor' AND r.role='buyer' AND r.suspended_at IS NULL AND r.closed_at IS NULL AND (
			EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id WHERE o.buyer_id=r.id AND p.vendor_id=$1 AND `+vendorSeesOrder("o")+`)
			OR EXISTS(SELECT 1 FROM messages m WHERE m.recipient_id=$1 AND m.sender_id=r.id)))
		OR (r.role IN ('moderator','admin') AND r.suspended_at IS NULL AND r.closed_at IS NULL AND r.retention_restricted_at IS NULL AND (
			(NOT r.staff_hidden AND EXISTS(SELECT 1 FROM settings WHERE key=$5 AND value='true'))
			OR EXISTS(SELECT 1 FROM settings WHERE key=$6 AND value=r.handle)
			OR EXISTS(SELECT 1 FROM messages m WHERE m.recipient_id=$1 AND m.sender_id=r.id)
			OR EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id WHERE o.state IN ($7,$8) AND (o.buyer_id=$1 OR p.vendor_id=$1))))) FOR SHARE OF r`,
		sender.ID, handle, sender.Role == "moderator" || sender.Role == "admin", sender.Role, settingStaffListPublic, settingStaffContact, stateDisputed, stateResolved).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}
