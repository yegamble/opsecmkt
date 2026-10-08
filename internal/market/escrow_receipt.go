package market

import (
	"database/sql"
	"strconv"
)

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
		if c.R.URL.Path == "/escrow/recover" {
			return errRecoverOnlyBuyer
		}
		return errConfirmOnlyBuyer
	}
	if party == "vendor" && c.User.ID != vendor {
		if c.R.URL.Path == "/escrow-xmr/ship" || c.R.URL.Path == "/escrow/ship" {
			return errShipOnlyVendor
		}
		return errClaimOnlyVendor
	}
	return nil
}

func btcClaimReceiptAuthorize(c *actionCtx) error {
	return escrowPartyReceiptAuthorize(c, "BTC", "vendor")
}
func btcShipReceiptAuthorize(c *actionCtx) error {
	return escrowPartyReceiptAuthorize(c, "BTC", "vendor")
}
func btcRecoverReceiptAuthorize(c *actionCtx) error {
	return escrowPartyReceiptAuthorize(c, "BTC", "buyer")
}

// The saved bump response remains available only to the current claimant of
// this order's Bitcoin settlement. Its broadcast state and current fee are
// intentionally checked only by the original callback for a new request.
func btcBumpReceiptAuthorize(c *actionCtx) error {
	o, err := partyOrder(c, c.Form.Get("order_id"))
	if err != nil {
		return err
	}
	if !o.btcEscrow() {
		return errNoOrder
	}
	sid, err := strconv.ParseInt(c.Form.Get("settlement_id"), 10, 64)
	if err != nil {
		return errSettleNone
	}
	var kind string
	err = c.Tx.QueryRowContext(c.Ctx(), "SELECT kind FROM settlements WHERE id=$1 AND order_id=$2", sid, o.ID).Scan(&kind)
	if err == sql.ErrNoRows {
		return errSettleNone
	}
	if err != nil {
		return err
	}
	claimant := claimantOf(kind)
	party := roleBuyer
	if c.User.ID == o.VendorID {
		party = roleVendor
	}
	if party != claimant {
		return fail(403, "Only the "+claimant+" raises the network fee of "+settlementKind(kind)+": the fee comes from the "+claimant+"'s output. Nothing was changed.")
	}
	return nil
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
