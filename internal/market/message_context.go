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
	err := q.QueryRowContext(ctx, `SELECT o.id FROM orders o JOIN products p ON p.id=o.product_id
		JOIN users s ON s.id=$1 JOIN users r ON r.id=$2 WHERE `+where+` AND `+
		orderReadable("s.id", "s.role")+` AND `+orderReadable("r.id", "r.role"), senderID, recipientID, id).Scan(&c.LinkedOrderID)
	if errors.Is(err, sql.ErrNoRows) {
		return c, fail(404, "Message context is not available to both correspondents.")
	}
	if err != nil {
		return c, err
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
