package market

import (
	"context"
	"database/sql"
	"net/http"
)

func init() { registerLoader("order", orderSuspensionLoader) }

const (
	accountSuspendedStatus = "is suspended and cannot sign in"
	accountRestoredStatus  = "was restored and can sign in again"
	accountClosedStatus    = "is closed and cannot sign in"
)

func orderSuspensionLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.Order == nil {
		return nil
	}
	var buyer, vendor, buyerBanned, vendorBanned bool
	if err := a.db.QueryRowContext(ctx, `SELECT b.suspended_at IS NOT NULL,v.suspended_at IS NOT NULL,b.banned_at IS NOT NULL,v.banned_at IS NOT NULL
	 FROM users b,users v WHERE b.id=$1 AND v.id=$2`, d.Order.BuyerID, d.Order.VendorID).Scan(&buyer, &vendor, &buyerBanned, &vendorBanned); err != nil {
		return err
	}
	// A banned party is suspended too (migration 123), but is shown as closed (A-546).
	for _, p := range []struct {
		party             string
		suspended, banned bool
	}{{"buyer", buyer, buyerBanned}, {"vendor", vendor, vendorBanned}} {
		switch {
		case p.banned:
			d.OrderClosedParties = append(d.OrderClosedParties, p.party)
		case p.suspended:
			d.OrderSuspendedParties = append(d.OrderSuspendedParties, p.party)
		}
	}
	return nil
}

// One account-level notice per counterparty, even if several active orders connect them. A-165
// keeps abandoned drafts quiet. Account-level notices deliberately take no order FK lock: suspension
// holds the account row while a payment transition may hold an order and wait for that account.
// status completes "The account H, with which you have an active order, …": accountSuspendedStatus, accountRestoredStatus
// or accountClosedStatus (a ban, A-546).
func notifyActiveCounterparties(ctx context.Context, tx *sql.Tx, target, handle, status string) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT CASE WHEN o.buyer_id=$1 THEN p.vendor_id ELSE o.buyer_id END
	 FROM orders o JOIN products p ON p.id=o.product_id
	 WHERE (o.buyer_id=$1 OR p.vendor_id=$1) AND o.state NOT IN ('draft','completed','resolved','cancelled')`, target)
	if err != nil {
		return err
	}
	var recipients []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if id != target {
			recipients = append(recipients, id)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	body := "The account " + handle + ", with which you have an active order, " + status + ". Your orders remain on record. Check your orders for the available next steps."
	for _, id := range recipients {
		if _, err = insertNotification(ctx, tx, id, body, ""); err != nil {
			return err
		}
	}
	return nil
}
