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
	if ok, err := sellerIsVendor(c, f.Get("product_id")); err != nil || !ok {
		if err == nil {
			err = fail(400, sellerNotVendor)
		}
		return actionResult{}, err
	}
	if vendor == c.User.ID {
		return actionResult{}, fail(400, "You cannot order your own listing")
	}
	total := btc
	if currency == "XMR" {
		total = xmr
	}
	id := randomToken()
	err = tx.QueryRowContext(ctx, `INSERT INTO orders(id,buyer_id,product_id,currency,amount) VALUES($1,$2,$3,$4,$5) ON CONFLICT(buyer_id,product_id,currency) WHERE state='draft' DO UPDATE SET amount=excluded.amount RETURNING id`, id, c.User.ID, f.Get("product_id"), currency, total).Scan(&id)
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
	if _, err = tx.ExecContext(ctx, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,$3)", randomToken(), o.ID, reason); err != nil {
		return actionResult{}, err
	}
	if err = notifyDisputeStaff(c, &o); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/disputes?saved=1", Audit: "Opened dispute on order " + shortID(o.ID)}, nil
}

// notifyDisputeStaff tells every moderator and administrator who is not a party to the order that a dispute
// opened. If every staff account is a party, the dispute still stands: the order history records that no
// independent resolver was available and every administrator is told it needs a moderator who is not a party.
// A suspended account cannot sign in, so it counts as neither a resolver nor a recipient (A-118).
func notifyDisputeStaff(c *actionCtx, o *Order) error {
	ctx, tx := c.Ctx(), c.Tx
	staff, err := collectStrings(ctx, tx, "SELECT id FROM users WHERE role IN ('moderator','admin') AND suspended_at IS NULL AND id<>$1 AND id<>$2", o.BuyerID, o.VendorID)
	if err != nil {
		return err
	}
	body := "Dispute opened on order " + shortID(o.ID) + ". Review it on the moderator desk."
	if len(staff) == 0 {
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, stateDisputed, noResolverNote); err != nil {
			return err
		}
		if staff, err = collectStrings(ctx, tx, "SELECT id FROM users WHERE role='admin' AND suspended_at IS NULL"); err != nil {
			return err
		}
		body = "Dispute opened on order " + shortID(o.ID) + " needs a moderator who is not a party to the order. Every moderator and administrator is its buyer or vendor, so nobody can resolve it yet."
	}
	for _, uid := range staff {
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, body); err != nil {
			return err
		}
	}
	return nil
}

const noResolverNote = "No independent resolver was available: every moderator and administrator is a party to this order. An administrator must assign a moderator who is not a party to resolve this dispute."

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
