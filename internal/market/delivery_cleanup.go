package market

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
)

// A-129: former vendors keep their order history, but can explicitly erase an unused automatic-delivery template
// from an archived listing. The page and audit trail never load or repeat the template itself.
const deliveryCleanupPageSize = 50

type DeliveryCleanupListing struct {
	ID, Title       string
	AwaitingPayment bool
}

func init() {
	registerPage("delivery-cleanup", pageSpec{Protected: true})
	registerLoader("delivery-cleanup", accountSecurityLoader)
	registerLoader("delivery-cleanup", deliveryCleanupLoader)
	registerLoader("account", deliveryCleanupAccountLoader)
	registerAction("/delivery-cleanup", actionSpec{Roles: []string{"buyer", "vendor", "moderator", "admin"}, OwnTx: true, ConfirmedFormNonce: true, ReceiptAuthorize: deliveryCleanupReceiptAuthorize, Run: deliveryCleanupAction})
}

// deliveryCleanupReceiptAuthorize retains the current owner check on a saved replay without
// requiring the archived/template state changed by the original cleanup.
func deliveryCleanupReceiptAuthorize(c *actionCtx) error {
	var owner string
	err := c.Tx.QueryRowContext(c.Ctx(), "SELECT vendor_id FROM products WHERE id=$1", strings.TrimSpace(c.Form.Get("id"))).Scan(&owner)
	if err == sql.ErrNoRows || err == nil && owner != c.User.ID {
		return fail(404, "Archived listing not found")
	}
	return err
}

func deliveryCleanupAccountLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	return a.db.QueryRowContext(ctx, `SELECT count(*) FROM products
		WHERE vendor_id=$1 AND archived AND (delivery_content<>'' OR EXISTS(SELECT 1 FROM digital_units du WHERE du.product_id=products.id AND du.order_id IS NULL))`, d.User.ID).Scan(&d.DeliveryCleanupCount)
}

func deliveryCleanupLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	var total int
	if err := a.db.QueryRowContext(ctx, `SELECT count(*) FROM products
		WHERE vendor_id=$1 AND archived AND (delivery_content<>'' OR EXISTS(SELECT 1 FROM digital_units du WHERE du.product_id=products.id AND du.order_id IS NULL))`, d.User.ID).Scan(&total); err != nil {
		return err
	}
	d.DeliveryCleanupCount = total
	p := activityPagingWithKey("/delivery-cleanup", "page", total, r.URL.Query().Get("page"), nil)
	d.DeliveryCleanupPaging = p
	d.DeliveryCleanup = make([]DeliveryCleanupListing, 0, deliveryCleanupPageSize)
	rows, err := a.db.QueryContext(ctx, `SELECT p.id,p.title,EXISTS (
		SELECT 1 FROM orders o WHERE o.product_id=p.id AND o.state='awaiting_payment'
		) FROM products p WHERE p.vendor_id=$1 AND p.archived AND (p.delivery_content<>'' OR EXISTS(SELECT 1 FROM digital_units du WHERE du.product_id=p.id AND du.order_id IS NULL))
		ORDER BY p.created,p.id LIMIT $2 OFFSET $3`, d.User.ID, deliveryCleanupPageSize, p.Offset())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item DeliveryCleanupListing
		if err = rows.Scan(&item.ID, &item.Title, &item.AwaitingPayment); err != nil {
			return err
		}
		d.DeliveryCleanup = append(d.DeliveryCleanup, item)
	}
	return rows.Err()
}

func deliveryCleanupAction(c *actionCtx) (actionResult, error) {
	id := strings.TrimSpace(c.Form.Get("id"))
	if id == "" || len(id) > 128 {
		return actionResult{}, fail(400, "Choose an archived listing to clear.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		// confirmedTx holds and revalidates the actual owner's account and
		// session. Privacy cleanup remains available after selling approval is
		// removed, unlike listing edits, so do not use selling-only lockListing.
		if err := waitForWalletLocks(c.Ctx(), c.Tx); err != nil {
			return actionResult{}, err
		}
		var archived bool
		err := c.Tx.QueryRowContext(c.Ctx(), "SELECT archived FROM products WHERE id=$1 AND vendor_id=$2 FOR UPDATE", id, c.User.ID).Scan(&archived)
		// Administrators have no bypass here: cleanup can erase only the
		// caller's own archived template, never another vendor's secret.
		if err == sql.ErrNoRows || err == nil && !archived {
			return actionResult{}, fail(404, "Archived listing not found")
		}
		if err != nil {
			return actionResult{}, err
		}
		var hasContent bool
		if err = c.Tx.QueryRowContext(c.Ctx(), "SELECT delivery_content<>'' OR EXISTS(SELECT 1 FROM digital_units WHERE product_id=$1 AND order_id IS NULL) FROM products WHERE id=$1", id).Scan(&hasContent); err != nil {
			return actionResult{}, err
		}
		if !hasContent {
			return actionResult{}, fail(409, "This listing has no stored delivery text to clear.")
		}
		var awaiting bool
		if err = c.Tx.QueryRowContext(c.Ctx(), `SELECT EXISTS(SELECT 1 FROM orders WHERE product_id=$1 AND state='awaiting_payment')`, id).Scan(&awaiting); err != nil {
			return actionResult{}, err
		}
		if awaiting {
			return actionResult{}, fail(409, "This archived listing has an order awaiting payment. The stored delivery text is kept until that order no longer needs it.")
		}
		res, err := c.Tx.ExecContext(c.Ctx(), `UPDATE products SET delivery_content='',stock=CASE WHEN digital_delivery_mode='inventory' THEN 0 ELSE stock END
			WHERE id=$1 AND vendor_id=$2 AND archived AND (delivery_content<>'' OR EXISTS(SELECT 1 FROM digital_units du WHERE du.product_id=products.id AND du.order_id IS NULL))`, id, c.User.ID)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return actionResult{}, fail(409, "This listing's delivery text changed. Reload and try again.")
		}
		if _, err = c.Tx.ExecContext(c.Ctx(), "DELETE FROM digital_units WHERE product_id=$1 AND order_id IS NULL", id); err != nil {
			return actionResult{}, err
		}
		short := id[:min(len(id), 8)]
		if _, err = c.Tx.ExecContext(c.Ctx(), "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), c.User.ID,
			"Stored automatic-delivery text was cleared from archived listing "+short+". Existing orders and their saved deliveries were not changed."); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/delivery-cleanup", Notice: "delivery-cleared", Audit: "Cleared stored automatic delivery from archived listing " + short}, nil
	})
}
