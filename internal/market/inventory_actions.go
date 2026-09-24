package market

import (
	"database/sql"
	"strconv"
)

// P4 Inventory: vendors edit, archive and restore their own listings; administrators may manage any listing
// except its automatic delivery content, which only the listing's vendor can read or change.

func init() {
	managers := []string{"vendor", "admin"}
	registerAction("/listings/update", actionSpec{Roles: managers, Run: listingUpdateAction})
	registerAction("/listings/archive", actionSpec{Roles: managers, Run: func(c *actionCtx) (actionResult, error) { return listingArchiveAction(c, true) }})
	registerAction("/listings/restore", actionSpec{Roles: managers, Run: func(c *actionCtx) (actionResult, error) { return listingArchiveAction(c, false) }})
}

type managedListing struct {
	ID, VendorID, Kind string
	Stock              int
	Archived           bool
}

// lockListing row-locks a listing the actor may manage (its vendor, or any administrator).
// Anything else is reported as not found so listing ids of other vendors are not confirmed.
func lockListing(c *actionCtx, id string) (managedListing, error) {
	l := managedListing{ID: id}
	err := c.Tx.QueryRowContext(c.Ctx(), `SELECT vendor_id,kind,stock,archived FROM products WHERE id=$1 AND (vendor_id=$2 OR $3='admin') FOR UPDATE`, id, c.User.ID, c.User.Role).Scan(&l.VendorID, &l.Kind, &l.Stock, &l.Archived)
	if err == sql.ErrNoRows {
		return l, fail(404, "Listing not found")
	}
	return l, err
}

// listingAudit names the listing by a short id and flags administrator changes to another vendor's listing.
func listingAudit(action string, c *actionCtx, l managedListing) string {
	s := action + " listing " + l.ID[:min(8, len(l.ID))]
	if l.VendorID != c.User.ID {
		s += " as administrator"
	}
	return s
}

// openOrdersSQL counts orders that still depend on the listing's fulfillment type.
const openOrdersSQL = `SELECT count(*) FROM orders WHERE product_id=$1 AND state NOT IN ('completed','resolved','cancelled')`

func listingUpdateAction(c *actionCtx) (actionResult, error) {
	ctx := c.Ctx()
	in, err := validateListing(c.Form)
	if err != nil {
		return actionResult{}, err
	}
	seen, err := strconv.Atoi(c.Form.Get("stock_seen"))
	if err != nil {
		return actionResult{}, fail(400, "Reload the edit form and try again.")
	}
	l, err := lockListing(c, c.Form.Get("id"))
	if err != nil {
		return actionResult{}, err
	}
	if l.VendorID != c.User.ID {
		// Automatic delivery content is what every buyer receives in the vendor's name. Other editors
		// (administrators) never see it, so their saves keep the stored content whatever the form sends.
		if err = c.Tx.QueryRowContext(ctx, `SELECT delivery_content FROM products WHERE id=$1`, l.ID).Scan(&in.DeliveryContent); err != nil {
			return actionResult{}, err
		}
		if in.DeliveryContent != "" && in.Kind != "digital" {
			return actionResult{}, fail(400, "This listing has automatic delivery content that only its vendor can remove, so it must stay digital.")
		}
	}
	if in.Kind != l.Kind {
		// Orders read the fulfillment type from the listing; changing it would change their shipping/delivery path.
		var open int
		if err = c.Tx.QueryRowContext(ctx, openOrdersSQL, l.ID).Scan(&open); err != nil {
			return actionResult{}, err
		}
		if open > 0 {
			return actionResult{}, fail(409, "The fulfillment type cannot change while this listing has open orders.")
		}
	}
	// Stock is only written when the editor changed it. Order payment requests reserve stock concurrently,
	// so a change based on a stale value is refused instead of silently overwriting a reservation.
	stock := l.Stock
	if in.Stock != seen {
		if l.Stock != seen {
			return actionResult{}, fail(409, "Stock changed to "+strconv.Itoa(l.Stock)+" while you were editing (orders reserve stock). Reload and try again.")
		}
		stock = in.Stock
	}
	_, err = c.Tx.ExecContext(ctx, `UPDATE products SET title=$2,description=$3,category=$4,region=$5,kind=$6,btc=$7,xmr=$8,stock=$9,delivery_content=$10,updated=now() WHERE id=$1`, l.ID, in.Title, in.Description, in.Category, in.Region, in.Kind, in.BTC, in.XMR, stock, in.DeliveryContent)
	return actionResult{Redirect: "/listing-edit?id=" + l.ID + "&saved=1", Audit: listingAudit("Updated", c, l)}, err
}

// listingArchiveAction archives (hides from the catalog and refuses new drafts) or restores a listing.
// Existing orders are not changed.
func listingArchiveAction(c *actionCtx, archive bool) (actionResult, error) {
	l, err := lockListing(c, c.Form.Get("id"))
	if err != nil {
		return actionResult{}, err
	}
	if l.Archived == archive {
		if archive {
			return actionResult{}, fail(409, "This listing is already archived.")
		}
		return actionResult{}, fail(409, "This listing is not archived.")
	}
	if !archive {
		if ok, err := sellerIsVendor(c, l.ID); err != nil || !ok {
			if err == nil {
				err = fail(409, "This listing's owner is no longer a vendor, so it cannot be restored.")
			}
			return actionResult{}, err
		}
	}
	if _, err = c.Tx.ExecContext(c.Ctx(), `UPDATE products SET archived=$2,archived_at=CASE WHEN $2 THEN now() END,updated=now() WHERE id=$1`, l.ID, archive); err != nil {
		return actionResult{}, err
	}
	action, redirect := "Restored", "/vendor-dashboard?saved=1"
	if archive {
		action = "Archived"
	}
	if l.VendorID != c.User.ID {
		redirect = "/listing-edit?id=" + l.ID + "&saved=1"
	}
	return actionResult{Redirect: redirect, Audit: listingAudit(action, c, l)}, nil
}
