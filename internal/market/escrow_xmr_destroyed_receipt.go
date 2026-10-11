package market

import "strconv"

// xmrDestroyedReceiptAuthorize lets an administrator retrieve a saved confirmation only while the wallet record
// still exists and the current actor is not a party to any order that used it. State and destruction-date checks stay
// in xmrConfirmDestroyedAction: an exact replay must return its saved response after the first confirmation or restore.
func xmrDestroyedReceiptAuthorize(c *actionCtx) error {
	id, err := strconv.ParseInt(c.Form.Get("wallet_id"), 10, 64)
	if err != nil || id <= 0 {
		return fail(400, "Unknown wallet")
	}
	var exists, party bool
	err = c.Tx.QueryRowContext(c.Ctx(), `SELECT
		EXISTS(SELECT 1 FROM xmr_escrow_wallets WHERE id=$1),
		EXISTS(SELECT 1 FROM xmr_escrow_slots s JOIN orders o ON o.id=s.order_id JOIN products p ON p.id=o.product_id
			WHERE s.wallet_id=$1 AND (o.buyer_id=$2 OR p.vendor_id=$2))`, id, c.User.ID).Scan(&exists, &party)
	if err != nil {
		return err
	}
	if !exists {
		return fail(404, "Wallet not found")
	}
	if party {
		return fail(403, "You are a party to an order of this wallet, so you cannot confirm its destruction.")
	}
	return nil
}
