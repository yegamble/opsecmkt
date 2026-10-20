package market

import (
	"context"
	"database/sql"
	"strings"
	"time"
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
	// Both participants must be able to receive encrypted correspondence. A
	// sender without a public encryption key cannot receive a safe reply.
	senderPGP, err := loadPGPAccount(ctx, tx, u.ID, false)
	if err != nil {
		return actionResult{}, err
	}
	if !canReceiveEncrypted(senderPGP.Key, time.Now()) {
		return actionResult{}, fail(409, "Add a usable PGP public encryption key on your account before sending private messages. Staff and dispute correspondence must also be encrypted.")
	}
	messageContext, err := resolveMessageContext(ctx, tx, u.ID, recipient, c.Form.Get("order_id"), c.Form.Get("dispute_id"))
	if err != nil {
		return actionResult{}, err
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
	messageID := randomToken()
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(id,sender_id,recipient_id,body,encrypted,recipient_match,recipient_extra_keys,order_id,dispute_id)
		VALUES($1,$2,$3,$4,true,$5,$6,NULLIF($7,''),NULLIF($8,''))`, messageID, u.ID, recipient, body, match, extraKeysValue, messageContext.OrderID, messageContext.DisputeID)
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO notifications(id,user_id,body,order_id,message_id) VALUES($1,$2,$3,NULLIF($4,''),$5)`,
			randomToken(), recipient, "New message from "+u.Handle+messageContext.Suffix(), messageContext.LinkedOrderID, messageID)
	}
	return actionResult{Redirect: "/messages", Notice: "message-sent", Audit: "Sent OpenPGP-encrypted message (recipient key match: " + match + ")"}, err
}

// messageNotPermitted is shared by sends and recipient-key lookups. Unknown handles and accounts the sender
// cannot contact answer alike, so neither route exposes a directory of private buyers.
const messageNotPermitted = "No account you can message has this handle. Contact an available vendor, your existing vendor or customer, or listed market staff for support."

// messageRecipient returns an authorized recipient ID, or an empty string. The sender's current database role,
// never the request's User snapshot, decides staff and vendor contact privileges. Share locks keep both roles and
// recipient preferences stable through a send transaction. NOWAIT avoids actor/target lock inversions with confirmed
// role or availability changes; real contention is retried as a database-busy response.
// Other accounts contact a public vendor or eligible staff; a vendor also answers known customers. An old inbound
// message never grants buyer-to-buyer access. Hidden vendors remain reachable by established purchasers, including
// vendor accounts buying for themselves, and by sellers through vendor-visible customer orders. Restricted vendors
// require an unresolved financial order. The recipient's away preference still applies to every non-staff path.
// q is the send transaction or a short key-read transaction; display-only availability checks also use the database.
func messageRecipient(ctx context.Context, q rowQuerier, sender *User, handle string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT r.id FROM users s JOIN users r ON r.handle=$2
		WHERE s.id=$1 AND s.suspended_at IS NULL AND s.closed_at IS NULL AND s.banned_at IS NULL
		AND r.id<>s.id AND r.banned_at IS NULL AND (s.role IN ('moderator','admin')
		OR ((r.role IN ('vendor','admin') OR r.vendor_approved) AND r.suspended_at IS NULL AND r.closed_at IS NULL
			AND `+vendorAcceptsBuyerMessagesSQL("r")+`
			AND ((`+activeVendorSQL("r")+` AND (r.role NOT IN ('moderator','admin') OR EXISTS(
				SELECT 1 FROM products contact_listing WHERE contact_listing.vendor_id=r.id AND NOT contact_listing.archived AND contact_listing.admin_archive_reason IS NULL AND contact_listing.review_state='approved')))
				OR (s.role IN ('buyer','vendor') AND EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id
					WHERE o.buyer_id=s.id AND p.vendor_id=r.id
						AND (r.retention_restricted_at IS NULL OR o.financial_closed_at IS NULL)))
				OR (s.role='buyer' AND r.retention_restricted_at IS NULL
					AND EXISTS(SELECT 1 FROM messages m WHERE m.recipient_id=s.id AND m.sender_id=r.id))
				OR ((s.role IN ('vendor','admin') OR s.vendor_approved) AND EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id
					WHERE o.buyer_id=r.id AND p.vendor_id=s.id AND `+vendorSeesOrder("o")+`
						AND (r.retention_restricted_at IS NULL OR o.financial_closed_at IS NULL)))))
		OR ((s.role IN ('vendor','admin') OR s.vendor_approved) AND r.role='buyer' AND r.suspended_at IS NULL AND r.closed_at IS NULL AND (
			EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id WHERE o.buyer_id=r.id AND p.vendor_id=s.id AND `+vendorSeesOrder("o")+`)
			OR EXISTS(SELECT 1 FROM messages m WHERE m.recipient_id=s.id AND m.sender_id=r.id)))

		OR (r.role IN ('moderator','admin') AND r.suspended_at IS NULL AND r.closed_at IS NULL AND r.retention_restricted_at IS NULL AND (
			(NOT r.staff_hidden AND EXISTS(SELECT 1 FROM settings WHERE key=$3 AND value='true'))
			OR EXISTS(SELECT 1 FROM settings WHERE key=$4 AND value=r.handle)
			OR EXISTS(SELECT 1 FROM messages m WHERE m.recipient_id=s.id AND m.sender_id=r.id)
			OR EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id WHERE o.state IN ($5,$6) AND (o.buyer_id=s.id OR p.vendor_id=s.id))))) FOR SHARE OF s,r NOWAIT`,
		sender.ID, handle, settingStaffListPublic, settingStaffContact, stateDisputed, stateResolved).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}
