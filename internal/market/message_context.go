package market

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"
)

// Message context is explicit, never inferred from handles or the most recent
// order. Both correspondents must be authorized to read the referenced record.
type messageContext struct {
	OrderID, DisputeID, LinkedOrderID string
}

func (c messageContext) Suffix() string {
	if c.DisputeID != "" {
		return " regarding Dispute #" + c.DisputeID
	}
	if c.OrderID != "" {
		return " for Order #" + c.OrderID
	}
	return ""
}

func resolveMessageContext(ctx context.Context, q rowQuerier, senderID, recipientID, orderID, disputeID string) (messageContext, error) {
	var c messageContext
	if orderID == "" && disputeID == "" {
		return c, nil
	}
	if orderID != "" && disputeID != "" {
		return c, fail(400, "Choose one order or dispute context for this message.")
	}
	if len(orderID) > 128 || len(disputeID) > 128 {
		return c, fail(404, "Message context is not available to both correspondents.")
	}
	where := "o.id=$3"
	id := orderID
	if disputeID != "" {
		where = "EXISTS(SELECT 1 FROM disputes d WHERE d.id=$3 AND d.order_id=o.id)"
		id = disputeID
	}
	var buyerID, vendorID string
	err := q.QueryRowContext(ctx, `SELECT o.id,o.buyer_id,p.vendor_id FROM orders o JOIN products p ON p.id=o.product_id
		JOIN users s ON s.id=$1 JOIN users r ON r.id=$2 WHERE `+where+` AND `+
		orderReadable("s.id", "s.role")+` AND `+orderReadable("r.id", "r.role"), senderID, recipientID, id).Scan(&c.LinkedOrderID, &buyerID, &vendorID)
	if errors.Is(err, sql.ErrNoRows) {
		return c, fail(404, "Message context is not available to both correspondents.")
	}
	if err != nil {
		return c, err
	}
	if (senderID == buyerID && recipientID == vendorID) || (senderID == vendorID && recipientID == buyerID) {
		return messageContext{}, fail(409, "Send correspondence between this order's buyer and vendor from the order status page: "+orderPage(c.LinkedOrderID)+"#order-messages. Nothing was stored in the direct inbox.")
	}
	c.OrderID, c.DisputeID = orderID, disputeID
	return c, nil
}

func init() {
	registerLoader("messages", messageContextLoader)
	registerLoader("messages", messageOwnKeyLoader)
}

func messageOwnKeyLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	p, err := loadPGPAccount(ctx, a.db, d.User.ID, false)
	if err != nil {
		return err
	}
	d.MessageOwnKeyUnusable = !canReceiveEncrypted(p.Key, time.Now())
	return nil
}

func messageContextLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if len(d.Messages) > 0 {
		ids := make([]string, len(d.Messages))
		for i := range d.Messages {
			ids[i] = d.Messages[i].ID
		}
		// Historical contextual inbox records stay where they were stored. New
		// correspondence between actual parties belongs to the order's lifecycle.
		rows, err := a.db.QueryContext(ctx, `SELECT m.id,o.id FROM messages m JOIN orders o ON
			(o.id=m.order_id OR EXISTS(SELECT 1 FROM disputes d WHERE d.id=m.dispute_id AND d.order_id=o.id))
			JOIN products p ON p.id=o.product_id WHERE m.id=ANY($1) AND
			((m.sender_id=o.buyer_id AND m.recipient_id=p.vendor_id) OR (m.recipient_id=o.buyer_id AND m.sender_id=p.vendor_id))`, ids)
		if err != nil {
			return err
		}
		replies := make(map[string]string)
		for rows.Next() {
			var id, orderID string
			if err := rows.Scan(&id, &orderID); err != nil {
				rows.Close()
				return err
			}
			replies[id] = orderID
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for i := range d.Messages {
			d.Messages[i].ReplyOrderID = replies[d.Messages[i].ID]
		}
	}
	orderID, disputeID := r.URL.Query().Get("order_id"), r.URL.Query().Get("dispute_id")
	if d.User == nil || orderID == "" && disputeID == "" {
		return nil
	}
	// Resolve the recipient independently of loader registration order.
	recipientID, err := messageRecipient(ctx, a.db, d.User, r.URL.Query().Get("to"))
	if err != nil {
		return err
	}
	if recipientID == "" {
		return fail(404, "Message context is not available to both correspondents.")
	}
	c, err := resolveMessageContext(ctx, a.db, d.User.ID, recipientID, orderID, disputeID)
	if err != nil {
		return err
	}
	d.MessageOrderID, d.MessageDisputeID = c.OrderID, c.DisputeID
	return nil
}
