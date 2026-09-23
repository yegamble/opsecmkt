package market

import (
	"database/sql"
	"strings"
)

func init() {
	registerAction("/orders", actionSpec{Run: orderDraftAction})
	registerAction("/disputes", actionSpec{Run: disputeAction})
	registerAction("/resolve", actionSpec{Roles: []string{"moderator", "admin"}, Run: resolveAction})
}

func orderDraftAction(c *actionCtx) (actionResult, error) {
	ctx, f, tx := c.Ctx(), c.Form, c.Tx
	currency := f.Get("currency")
	if currency != "BTC" && currency != "XMR" {
		return actionResult{}, fail(400, "Choose BTC or XMR")
	}
	var btc, xmr int64
	var vendor string
	var stock int
	var archived bool
	err := tx.QueryRowContext(ctx, "SELECT btc,xmr,vendor_id,stock,archived FROM products WHERE id=$1", f.Get("product_id")).Scan(&btc, &xmr, &vendor, &stock, &archived)
	if err != nil || stock < 1 {
		return actionResult{}, fail(400, "Product unavailable")
	}
	if archived {
		return actionResult{}, fail(400, "This listing is archived and accepts no new orders.")
	}
	if vendor == c.User.ID {
		return actionResult{}, fail(400, "You cannot order your own listing")
	}
	total := btc
	if currency == "XMR" {
		total = xmr
	}
	id := randomToken()
	err = tx.QueryRowContext(ctx, `INSERT INTO orders(id,buyer_id,product_id,currency,amount) VALUES($1,$2,$3,$4,$5) ON CONFLICT(buyer_id,product_id,currency) WHERE state='draft' DO UPDATE SET buyer_id=excluded.buyer_id RETURNING id`, id, c.User.ID, f.Get("product_id"), currency, total).Scan(&id)
	return actionResult{Redirect: "/order?id=" + id, Audit: "Saved unfunded order draft"}, err
}

// disputeAction opens a dispute by moving a paid, shipped or delivered order to disputed (P3).
func disputeAction(c *actionCtx) (actionResult, error) {
	ctx, f, tx := c.Ctx(), c.Form, c.Tx
	reason := strings.TrimSpace(f.Get("reason"))
	if len(reason) < 20 || len(reason) > 5000 {
		return actionResult{}, fail(400, "Describe the issue in 20–5000 characters")
	}
	o, err := partyOrder(c, f.Get("order_id"))
	if err != nil {
		return actionResult{}, err
	}
	if o.State != statePaid && o.State != stateShipped && o.State != stateDelivered {
		return actionResult{}, fail(409, "Only paid, shipped, or delivered orders can be disputed. Draft orders contain no funds.")
	}
	if _, err = c.A.transition(ctx, tx, o.ID, o.State, stateDisputed, c.User, "Dispute opened"); err != nil {
		return actionResult{}, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,$3)", randomToken(), o.ID, reason)
	return actionResult{Redirect: "/disputes?saved=1", Audit: "Opened dispute on order " + shortID(o.ID)}, err
}

var disputeOutcomes = map[string]string{"release": "Resolved — release to vendor", "refund": "Resolved — refund to buyer"}

// resolveAction records the outcome, then moves disputed -> resolved as moderator (P3). The outcome is
// written first so transition hooks (payouts) read it in the same transaction. A moderator who is a party
// to the order is refused by transition().
func resolveAction(c *actionCtx) (actionResult, error) {
	outcome := c.Form.Get("outcome")
	status, ok := disputeOutcomes[outcome]
	if !ok {
		return actionResult{}, fail(400, "Choose an outcome: release to vendor or refund to buyer")
	}
	resolution := strings.TrimSpace(c.Form.Get("resolution"))
	if len(resolution) < 20 || len(resolution) > 5000 {
		return actionResult{}, fail(400, "Provide a decision of 20–5000 characters")
	}
	var orderID string
	err := c.Tx.QueryRowContext(c.Ctx(), "UPDATE disputes SET resolution=$1,outcome=$2,status=$3 WHERE id=$4 AND status='Open' AND outcome='' RETURNING order_id", resolution, outcome, status, c.Form.Get("id")).Scan(&orderID)
	if err == sql.ErrNoRows {
		return actionResult{}, fail(409, "Open dispute not found")
	}
	if err != nil {
		return actionResult{}, err
	}
	if _, err = c.A.transition(c.Ctx(), c.Tx, orderID, stateDisputed, stateResolved, c.User, status); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/moderator?saved=1", Audit: "Resolved dispute on order " + shortID(orderID) + " (outcome: " + outcome + ")"}, nil
}
