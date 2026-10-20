package market

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// xmppNotificationBody extends the same frontend summary only when both the
// administrator and recipient currently permit contents, the enqueue-time
// consent/key snapshot permits them, and the exact linked source belongs to this
// recipient. It never reads plaintext listing delivery_content and never infers
// a source from notification text. The caller holds the recipient's SHARE lock
// throughout this check and submission so a key/consent change cannot race it.
func xmppNotificationBody(ctx context.Context, q rowQuerier, notificationID, userID, summary string, includeContents bool, recipientFingerprint string) (string, error) {
	if !includeContents || recipientFingerprint == "" {
		return summary, nil
	}
	cfg, err := readStoredXMPPConfig(ctx, q)
	if err != nil {
		return summary, err
	}
	if !cfg.Enabled || !cfg.AllowContents {
		return summary, nil
	}
	var audience, messageID, deliveryOrderID, noticeOrderID, ticketMessageID, noticeTicketID string
	var userConsent bool
	err = q.QueryRowContext(ctx, `SELECT n.audience,COALESCE(n.message_id,''),COALESCE(n.delivery_order_id,''),COALESCE(n.order_id,''),u.xmpp_include_contents,COALESCE(n.ticket_message_id,''),COALESCE(n.ticket_id,'')
		FROM notifications n JOIN users u ON u.id=n.user_id WHERE n.id=$1 AND u.id=$2
		AND u.xmpp_enabled AND u.suspended_at IS NULL AND u.closed_at IS NULL AND u.retention_restricted_at IS NULL`, notificationID, userID).
		Scan(&audience, &messageID, &deliveryOrderID, &noticeOrderID, &userConsent, &ticketMessageID, &noticeTicketID)
	if errors.Is(err, sql.ErrNoRows) {
		return summary, nil
	}
	if err != nil {
		return summary, err
	}
	sources := 0
	for _, source := range []string{messageID, deliveryOrderID, ticketMessageID} {
		if source != "" {
			sources++
		}
	}
	contentAudience := audience == "personal" || audience == "ticket" && ticketMessageID != ""
	if !userConsent || !contentAudience || sources != 1 {
		return summary, nil
	}
	p, err := loadPGPAccount(ctx, q, userID, false)
	if err != nil {
		return summary, err
	}
	if p.Fingerprint != recipientFingerprint || p.Stored != p.Fingerprint || !canReceiveEncrypted(p.Key, time.Now()) {
		return summary, nil
	}
	var content, orderID, disputeID, ticketID, label string
	if messageID != "" {
		// Message ownership is independent of staff roles. Context IDs are read
		// from the saved message and included only while the recipient can read
		// that context. A staff account never gains another user's message body.
		err = q.QueryRowContext(ctx, `SELECT m.body,COALESCE(m.order_id,d.order_id,''),COALESCE(m.dispute_id,'')
			FROM messages m JOIN users u ON u.id=m.recipient_id LEFT JOIN disputes d ON d.id=m.dispute_id
			WHERE m.id=$1 AND m.recipient_id=$2 AND m.encrypted IS TRUE AND m.recipient_match='yes'
			AND (m.order_id IS NULL OR EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id
				WHERE o.id=m.order_id AND `+orderReadable("$2", "u.role")+`))
			AND (m.dispute_id IS NULL OR (d.id IS NOT NULL AND (m.order_id IS NULL OR m.order_id=d.order_id)
				AND EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id
					WHERE o.id=d.order_id AND `+orderReadable("$2", "u.role")+`))) FOR SHARE OF m NOWAIT`, messageID, userID).
			Scan(&content, &orderID, &disputeID)
		label = "Encrypted message"
	} else if ticketMessageID != "" {
		if noticeTicketID == "" {
			return summary, nil
		}
		// A global staff ticket-reading grant is not permission to forward its
		// private body externally. Only the current opener or assignee receives
		// checked ciphertext; reassignment/escalation is serialized by the ticket
		// share lock held through the caller's submission transaction.
		err = q.QueryRowContext(ctx, `SELECT m.body FROM ticket_messages m JOIN tickets t ON t.id=m.ticket_id JOIN users u ON u.id=$2
			WHERE m.id=$1 AND m.ticket_id=$3 AND m.encrypted
			AND u.id IN(t.opener_id,t.assigned_to) AND `+ticketReadableSQL("t", "u.id", "u.role")+` FOR SHARE OF m,t NOWAIT`, ticketMessageID, userID, noticeTicketID).Scan(&content)
		ticketID, label = noticeTicketID, "Encrypted ticket message"
	} else {
		if noticeOrderID != deliveryOrderID {
			return summary, nil
		}
		// Delivery content is for its buyer only, even when the viewer is a
		// vendor or staff member who may otherwise read an order/dispute.
		err = q.QueryRowContext(ctx, `SELECT d.content FROM deliveries d JOIN orders o ON o.id=d.order_id JOIN products p ON p.id=o.product_id
			WHERE d.order_id=$1 AND o.buyer_id=$2 AND d.encrypted AND d.recipient_fingerprint=$3
			AND COALESCE(o.listing_kind,p.kind)='digital' FOR SHARE OF d NOWAIT`, deliveryOrderID, userID, recipientFingerprint).Scan(&content)
		orderID, label = deliveryOrderID, "Encrypted digital delivery"
	}
	if errors.Is(err, sql.ErrNoRows) {
		return summary, nil
	}
	if err != nil {
		return summary, err
	}
	canonical, ok := xmppCiphertextForKey(content, p.Key, time.Now())
	if !ok {
		return summary, nil
	}
	if ticketID != "" {
		label = "Ticket " + ticketID + ": " + label
	} else if disputeID != "" {
		label = "Dispute " + shortID(disputeID) + " (order " + shortID(orderID) + "): " + label
	} else if orderID != "" {
		label = "Order " + shortID(orderID) + ": " + label
	}
	body := summary + "\n\n" + label + ":\n" + canonical
	if len(body) > xmppMaxBodyBytes {
		return summary, nil
	}
	return body, nil
}

// xmppCiphertextForKey checks actual encrypted packet structure and the current
// usable encryption key rather than trusting stored flags or armor labels. The
// market has no recipient private key: this cannot verify encrypted payload
// integrity or prove decryption. Hidden recipients and expired/replaced subkeys
// conservatively keep the summary only. Canonical re-armor strips every armor
// header, so an old Comment/Version header cannot expose plaintext externally.
func xmppCiphertextForKey(body string, key *openpgp.Entity, now time.Time) (string, bool) {
	if len(body) > xmppMaxBodyBytes || !canReceiveEncrypted(key, now) {
		return "", false
	}
	canonical, err := canonicalMessage(body)
	if err != nil || len(canonical) > xmppMaxBodyBytes {
		return "", false
	}
	ids, encrypted, err := inspectEncrypted(canonical)
	if err != nil || !encrypted || len(ids) == 0 || slices.Contains(ids, uint64(0)) {
		return "", false
	}
	selected, ok := key.EncryptionKey(now)
	if !ok || !slices.Contains(ids, selected.PublicKey.KeyId) {
		return "", false
	}
	return strings.TrimSpace(canonical), true
}
