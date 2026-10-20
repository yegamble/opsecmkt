package market

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

func init() {
	registerAction("/orders/message", actionSpec{FormNonce: true, Run: orderMessageAction})
	registerAction("/orders/delivery-update", actionSpec{FormNonce: true, Run: orderDeliveryUpdateAction})
	registerLoader("order", loadOrderChannels)
	registerPreview("order", func(d *PageData) {
		if d.Order == nil || d.Order.State == stateDraft || d.User == nil {
			return
		}
		d.OrderCorrespondence, d.CanOrderMessage = true, true
		d.OrderMessageRecipient = d.Order.Vendor
		d.OrderMessages = []Message{{ID: "sample-order-message", Sender: d.Order.Buyer, Recipient: d.Order.Vendor, Body: "-----BEGIN PGP MESSAGE-----\n(sample preview — not real ciphertext)\n-----END PGP MESSAGE-----", Created: "Sample", Encrypted: true, RecipientMatch: matchYes}}
	})
}

// orderPrivateWindow is shared by display and writes. A purge marker is final
// even when retention is disabled later. With retention enabled, new private
// content cannot extend the original thirty-day financial-closure window.
func orderPrivateWindow(ctx context.Context, q rowQuerier, a *App, id string) (bool, error) {
	var open bool
	err := q.QueryRowContext(ctx, `SELECT private_expired_at IS NULL AND ($2::boolean=false OR financial_closed_at IS NULL OR financial_closed_at>now()-interval '30 days') FROM orders WHERE id=$1`, id, a.retention != nil).Scan(&open)
	return open, err
}

// orderChannelParty serializes a private write with transitions and purges.
// An abandoned draft never hands a vendor its buyer's correspondence or key.
func orderChannelParty(c *actionCtx) (Order, error) {
	o, err := partyOrderLocked(c, c.Form.Get("order_id"))
	if err != nil {
		return o, err
	}
	var visible bool
	if err = c.Tx.QueryRowContext(c.Ctx(), `SELECT `+vendorSeesOrder("o")+` FROM orders o WHERE o.id=$1`, o.ID).Scan(&visible); err != nil {
		return o, err
	}
	if !visible {
		if c.User.ID != o.BuyerID {
			return o, errNoOrder
		}
		return o, fail(409, "Order correspondence is available after checkout, while this order's private records remain available. Nothing was stored.")
	}
	open, err := orderPrivateWindow(c.Ctx(), c.Tx, c.A, o.ID)
	if err != nil {
		return o, err
	}
	if !open {
		return o, fail(409, "This order's private retention window has ended. New correspondence and replacement content cannot be stored.")
	}
	return o, nil
}

func orderMessageAction(c *actionCtx) (actionResult, error) {
	o, err := orderChannelParty(c)
	if err != nil {
		return actionResult{}, err
	}
	body := strings.TrimSpace(c.Form.Get("body"))
	if len(body) < 60 || len(body) > 20000 {
		return actionResult{}, fail(400, "Encrypt the message locally and paste the complete armored PGP message (up to 20 KB).")
	}
	if !c.A.allowWriteBudget("msg:"+c.User.ID, messageBudget) {
		return actionResult{}, fail(429, "Message not sent: an account can send at most 20 messages in 10 minutes. Nothing was stored. Try again later.")
	}
	recipient, handle := o.VendorID, o.Vendor
	if c.User.ID == o.VendorID {
		recipient, handle = o.BuyerID, o.Buyer
	}
	// Do not use messageRecipient: demotion may remove public vendor status but
	// must not stop actual historical order partners corresponding on this order.
	var banned bool
	if err = c.Tx.QueryRowContext(c.Ctx(), `SELECT banned_at IS NOT NULL FROM users WHERE id=$1 FOR SHARE`, recipient).Scan(&banned); err != nil {
		return actionResult{}, err
	}
	if banned {
		return actionResult{}, fail(409, "This order partner's account is closed to new messages. Nothing was stored.")
	}
	body, match, extra, known, err := inspectMessage(c, body, recipient, handle)
	if err != nil {
		return actionResult{}, err
	}
	if len(body) > 20000 {
		return actionResult{}, fail(400, "The normalized PGP message exceeds 20 KB. Send a shorter encrypted message. Nothing was stored.")
	}
	var extraValue any
	if known {
		extraValue = extra
	}
	if _, err = c.Tx.ExecContext(c.Ctx(), `INSERT INTO order_messages(id,order_id,sender_id,recipient_id,body,recipient_match,recipient_extra_keys) VALUES($1,$2,$3,$4,$5,$6,$7)`, randomToken(), o.ID, c.User.ID, recipient, body, match, extraValue); err != nil {
		return actionResult{}, err
	}
	if err = notifyOrderChannel(c, recipient, o.ID, "New correspondence on order "); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: orderPage(o.ID) + "#order-messages", Notice: "message-sent", Audit: "Sent encrypted correspondence on order " + shortID(o.ID)}, nil
}

func notifyOrderChannel(c *actionCtx, recipient, order, prefix string) error {
	_, err := c.Tx.ExecContext(c.Ctx(), `INSERT INTO notifications(id,user_id,body,order_id) VALUES($1,$2,$3,$4)`, randomToken(), recipient, prefix+shortID(order), order)
	return err
}

func orderDeliveryUpdateAction(c *actionCtx) (actionResult, error) {
	o, err := orderChannelParty(c)
	if err != nil {
		return actionResult{}, err
	}
	if c.User.ID != o.VendorID {
		return actionResult{}, fail(403, "Only this order's vendor can add replacement delivery content.")
	}
	if o.Kind != "digital" || (o.State != stateDelivered && o.State != stateCompleted && o.State != stateDisputed && o.State != stateResolved) {
		return actionResult{}, fail(409, "Replacement content can be added only to a digital order that has already been delivered.")
	}
	var delivered bool
	if err = c.Tx.QueryRowContext(c.Ctx(), `SELECT EXISTS(SELECT 1 FROM deliveries WHERE order_id=$1)`, o.ID).Scan(&delivered); err != nil {
		return actionResult{}, err
	}
	if !delivered {
		return actionResult{}, fail(409, "No original delivery is available for this order. Nothing was stored.")
	}
	content := c.Form.Get("content")
	if strings.TrimSpace(content) == "" || utf8.RuneCountInString(content) > maxDeliveryChars {
		return actionResult{}, fail(400, "Delivery content must contain 1–32000 characters")
	}
	if !c.A.allowWriteBudget("delivery-update:"+c.User.ID, messageBudget) {
		return actionResult{}, fail(429, "Too many delivery updates. Nothing was stored. Try again later.")
	}
	if _, err = c.Tx.ExecContext(c.Ctx(), `INSERT INTO order_delivery_updates(id,order_id,author_id,content) VALUES($1,$2,$3,$4)`, randomToken(), o.ID, c.User.ID, content); err != nil {
		return actionResult{}, err
	}
	if err = notifyOrderChannel(c, o.BuyerID, o.ID, "Delivery update on order "); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: orderPage(o.ID) + "#order-delivery", Notice: "order-delivered", Audit: "Added replacement delivery content for order " + shortID(o.ID)}, nil
}

func loadOrderChannels(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	o, u := d.Order, d.User
	if o == nil || u == nil {
		return nil
	}
	party := u.ID == o.BuyerID || u.ID == o.VendorID
	var visible bool
	if err := a.db.QueryRowContext(ctx, `SELECT `+vendorSeesOrder("o")+` FROM orders o WHERE o.id=$1`, o.ID).Scan(&visible); err != nil {
		return err
	}
	open, err := orderPrivateWindow(ctx, a.db, a, o.ID)
	if err != nil {
		return err
	}
	d.OrderPrivateExpired = !open
	if party && visible {
		d.OrderCorrespondence = true
		d.CanOrderMessage = open
		d.OrderMessageRecipient = o.Vendor
		if u.ID == o.VendorID {
			d.OrderMessageRecipient = o.Buyer
		}
		var total int
		if err = a.db.QueryRowContext(ctx, `SELECT count(*) FROM order_messages WHERE order_id=$1`, o.ID).Scan(&total); err != nil {
			return err
		}
		d.OrderMessagesPaging = activityPagingWithKey("/order", "message_page", total, r.URL.Query().Get("message_page"), url.Values{"id": {o.ID}})
		rows, err := a.db.QueryContext(ctx, `SELECT m.id,s.handle,r.handle,m.body,to_char(m.created,'YYYY-MM-DD HH24:MI "UTC"'),m.recipient_match,m.recipient_extra_keys FROM order_messages m JOIN users s ON s.id=m.sender_id JOIN users r ON r.id=m.recipient_id WHERE m.order_id=$1 ORDER BY m.created,m.id LIMIT $2 OFFSET $3`, o.ID, activityPageSize, (d.OrderMessagesPaging.Page-1)*activityPageSize)
		if err != nil {
			return err
		}
		for rows.Next() {
			var m Message
			var extra sql.NullInt64
			if err = rows.Scan(&m.ID, &m.Sender, &m.Recipient, &m.Body, &m.Created, &m.RecipientMatch, &extra); err != nil {
				rows.Close()
				return err
			}
			m.Encrypted = true
			if extra.Valid {
				m.RecipientExtraKeys, m.RecipientExtraKeysSet = int(extra.Int64), true
			}
			d.OrderMessages = append(d.OrderMessages, m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	// Staff may read delivery updates under exactly the original-delivery policy:
	// only a current/past dispute, never an unrelated payment flag alone.
	deliveryVisible := party
	if !party && (u.Role == roleModerator || u.Role == "admin") && !u.RetentionRestricted {
		if err = a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM disputes WHERE order_id=$1)`, o.ID).Scan(&deliveryVisible); err != nil {
			return err
		}
	}
	if !deliveryVisible {
		return nil
	}
	var total int
	if err = a.db.QueryRowContext(ctx, `SELECT count(*) FROM order_delivery_updates WHERE order_id=$1`, o.ID).Scan(&total); err != nil {
		return err
	}
	d.DeliveryUpdatesPaging = activityPagingWithKey("/order", "delivery_page", total, r.URL.Query().Get("delivery_page"), url.Values{"id": {o.ID}})
	rows, err := a.db.QueryContext(ctx, `SELECT d.content,to_char(d.created,'YYYY-MM-DD HH24:MI "UTC"'),u.handle FROM order_delivery_updates d JOIN users u ON u.id=d.author_id WHERE d.order_id=$1 ORDER BY d.created,d.id LIMIT $2 OFFSET $3`, o.ID, activityPageSize, (d.DeliveryUpdatesPaging.Page-1)*activityPageSize)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v DeliveryUpdateView
		if err = rows.Scan(&v.Content, &v.Created, &v.Author); err != nil {
			rows.Close()
			return err
		}
		d.DeliveryUpdates = append(d.DeliveryUpdates, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if party && u.ID == o.VendorID && open && o.Kind == "digital" && (o.State == stateDelivered || o.State == stateCompleted || o.State == stateDisputed || o.State == stateResolved) {
		if err = a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deliveries WHERE order_id=$1)`, o.ID).Scan(&d.CanDeliveryUpdate); err != nil {
			return err
		}
	}
	return nil
}
