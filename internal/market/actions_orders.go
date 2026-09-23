package market

import "strings"

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
	err := tx.QueryRowContext(ctx, "SELECT btc,xmr,vendor_id,stock FROM products WHERE id=$1", f.Get("product_id")).Scan(&btc, &xmr, &vendor, &stock)
	if err != nil || stock < 1 {
		return actionResult{}, fail(400, "Product unavailable")
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

func disputeAction(c *actionCtx) (actionResult, error) {
	ctx, f, tx := c.Ctx(), c.Form, c.Tx
	reason := strings.TrimSpace(f.Get("reason"))
	if len(reason) < 20 || len(reason) > 5000 {
		return actionResult{}, fail(400, "Describe the issue in 20–5000 characters")
	}
	var state string
	err := tx.QueryRowContext(ctx, `SELECT o.state FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=$1 AND (o.buyer_id=$2 OR p.vendor_id=$2)`, f.Get("order_id"), c.User.ID).Scan(&state)
	if err != nil {
		return actionResult{}, fail(404, "Order not found")
	}
	if state != statePaid && state != stateShipped && state != stateDelivered {
		return actionResult{}, fail(409, "Only paid, shipped, or delivered orders can be disputed. Draft orders contain no funds.")
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,$3)", randomToken(), f.Get("order_id"), reason)
	return actionResult{Redirect: "/disputes?saved=1", Audit: "Opened dispute"}, err
}

func resolveAction(c *actionCtx) (actionResult, error) {
	resolution := strings.TrimSpace(c.Form.Get("resolution"))
	if len(resolution) < 20 || len(resolution) > 5000 {
		return actionResult{}, fail(400, "Provide a decision of 20–5000 characters")
	}
	result, err := c.Tx.ExecContext(c.Ctx(), "UPDATE disputes SET resolution=$1,status='Decision recorded — settlement pending' WHERE id=$2 AND status='Open'", resolution, c.Form.Get("id"))
	if err == nil {
		if n, _ := result.RowsAffected(); n != 1 {
			return actionResult{}, fail(409, "Open dispute not found")
		}
	}
	return actionResult{Redirect: "/moderator?saved=1", Audit: "Recorded dispute decision; no fund transfer"}, err
}
