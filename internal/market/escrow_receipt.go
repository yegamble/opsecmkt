package market

import "database/sql"

// All four account roles could already reach these actions; ownership of the
// order, not role, decides which party may use one. The explicit list satisfies
// the confirmed-receipt guard without changing that eligibility.
var escrowPartyRoles = []string{"buyer", "vendor", "moderator", "admin"}

// Current ownership and coin must still match before either a new claim or a
// saved success can be returned. This read does not lock the order ahead of the
// original callback's established lock order. State and settlement existence
// are intentionally absent: a successful first request changes those values.
func escrowPartyReceiptAuthorize(c *actionCtx, coin, party string) error {
	var buyer, vendor string
	err := c.Tx.QueryRowContext(c.Ctx(), `SELECT o.buyer_id,p.vendor_id FROM orders o
		JOIN products p ON p.id=o.product_id
		JOIN escrows e ON e.order_id=o.id AND e.coin=$2
		WHERE o.id=$1`, c.Form.Get("order_id"), coin).Scan(&buyer, &vendor)
	if err == sql.ErrNoRows {
		return errNoOrder
	}
	if err != nil {
		return err
	}
	if c.User.ID != buyer && c.User.ID != vendor {
		return errNoOrder
	}
	if party == "buyer" && c.User.ID != buyer {
		return errConfirmOnlyBuyer
	}
	if party == "vendor" && c.User.ID != vendor {
		if c.R.URL.Path == "/escrow-xmr/ship" {
			return errShipOnlyVendor
		}
		return errClaimOnlyVendor
	}
	return nil
}

func btcClaimReceiptAuthorize(c *actionCtx) error {
	return escrowPartyReceiptAuthorize(c, "BTC", "vendor")
}
func xmrClaimReceiptAuthorize(c *actionCtx) error {
	return escrowPartyReceiptAuthorize(c, "XMR", "vendor")
}
func xmrShipReceiptAuthorize(c *actionCtx) error {
	return escrowPartyReceiptAuthorize(c, "XMR", "vendor")
}
func xmrConfirmReceiptAuthorize(c *actionCtx) error {
	return escrowPartyReceiptAuthorize(c, "XMR", "buyer")
}
