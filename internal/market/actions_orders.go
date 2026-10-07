package market

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"unicode/utf8"
)

func init() {
	registerAction("/orders", actionSpec{Run: orderDraftAction})
	registerAction("/disputes", actionSpec{Run: disputeAction})
	registerAction("/disputes/reply", actionSpec{Run: disputeReplyAction})
	registerAction("/resolve", actionSpec{Roles: []string{"moderator", "admin"}, OwnTx: true, Run: resolveAction})
	registerLoader("disputes", resolveConfirmLoader)
}

// resolveConfirmLoader tells the resolve form on /disputes whether to ask for an authenticator code (the moderation
// desk loads it with adminConfirmLoader). Only moderators and administrators see that form.
func resolveConfirmLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.User == nil || (d.User.Role != "moderator" && d.User.Role != "admin") {
		return nil
	}
	return adminConfirmLoader(ctx, a, r, d)
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
	// A-444: while the currency is escrow only, a listing that cannot use its escrow takes no draft in it.
	if err = c.A.purchaseBlock(ctx, tx, f.Get("product_id"), currency); err != nil {
		return actionResult{}, err
	}
	total := btc
	if currency == "XMR" {
		total = xmr
	}
	if total < payoutFloor(currency, "release") {
		return actionResult{}, fail(400, subFloorListing(currency))
	}
	id := randomToken()
	err = tx.QueryRowContext(ctx, `INSERT INTO orders(id,buyer_id,product_id,currency,amount) VALUES($1,$2,$3,$4,$5) ON CONFLICT(buyer_id,product_id,currency) WHERE state='draft' DO UPDATE SET amount=excluded.amount RETURNING id`, id, c.User.ID, f.Get("product_id"), currency, total).Scan(&id)
	return actionResult{Redirect: "/order?id=" + id, Audit: "Saved unfunded order draft"}, err
}

// subFloorListing refuses an order in currency on a listing priced below that currency's payout floor (A-216 for
// Bitcoin, A-232 for Monero). validateListing refuses such prices (Bitcoin since A-99, Monero since A-232), but a
// listing priced earlier keeps its price: its release would never be sent automatically and an overpayment on it
// could not be returned separately (splitPayout).
func subFloorListing(currency string) string {
	coin, other := "Bitcoin", "XMR"
	if currency == "XMR" {
		coin, other = "Monero", "BTC"
	}
	return "This listing's " + coin + " price is below the minimum automatic payout of " + amount(payoutFloor(currency, "release"), currencyDecimals(currency)) + " " + currency +
		" (it was priced before that minimum applied), so it cannot be ordered in " + currency + ". Order in " + other + ", or ask the vendor to raise the " + coin + " price. Nothing was changed."
}

// disputeAction opens a dispute by moving a paid, shipped or delivered order to disputed (P3).
func disputeAction(c *actionCtx) (actionResult, error) {
	ctx, f, tx := c.Ctx(), c.Form, c.Tx
	reason := strings.TrimSpace(f.Get("reason"))
	if len(reason) < 20 || len(reason) > 5000 {
		return actionResult{}, fail(400, "Describe the issue in 20–5000 characters")
	}
	// A-386: the order row first, as the sign action takes it, so the check below sees a buyer's signature committed
	// before this dispute, and a signature after it sees the order disputed.
	o, err := partyOrderLocked(c, f.Get("order_id"))
	if err != nil {
		return actionResult{}, err
	}
	if o.State != statePaid && o.State != stateShipped && o.State != stateDelivered {
		return actionResult{}, fail(409, "Only paid, shipped, or delivered orders can be disputed. Draft orders contain no funds.")
	}
	if o.Escrow && o.BuyerID == c.User.ID {
		// P2-R2: the buyer's signature on the release to the vendor confirms receipt and ends the buyer's right to dispute.
		var signed bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM settlement_signatures g JOIN settlements s ON s.id=g.settlement_id
			WHERE s.order_id=$1 AND s.kind='release' AND s.state<>'superseded' AND g.party='buyer')`, o.ID).Scan(&signed); err != nil {
			return actionResult{}, err
		}
		if signed {
			return actionResult{}, errDisputeSigned
		}
	}
	// P2-R3: after a shipped escrow order's release deadline the release is due and no dispute is opened; before it,
	// the other party is told the deadline no longer applies.
	detail, err := lateDispute(c, &o)
	if err != nil {
		return actionResult{}, err
	}
	if detail != "" {
		ctx = withNoticeDetail(ctx, o.ID, stateDisputed, detail)
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
	return actionResult{Redirect: "/disputes", Notice: "dispute-opened", Audit: "Opened dispute on order " + shortID(o.ID)}, nil
}

// disputeReplyAction (A-124) puts the reply of the party who did not open a dispute on record: once, while the dispute
// is open, as a disputed -> disputed order event (no state change). The dispute row is locked first, so a concurrent
// second reply, or a resolution, waits and then sees this one (or the resolution) and is refused with 409.
func disputeReplyAction(c *actionCtx) (actionResult, error) {
	ctx, tx := c.Ctx(), c.Tx
	reply := strings.TrimSpace(c.Form.Get("reply"))
	if n := utf8.RuneCountInString(reply); n < 20 || n > 5000 {
		return actionResult{}, fail(400, "Write your reply in 20–5000 characters. Nothing was changed.")
	}
	o, err := partyOrder(c, c.Form.Get("order_id"))
	if err != nil {
		return actionResult{}, err
	}
	var status string
	err = tx.QueryRowContext(ctx, "SELECT status FROM disputes WHERE order_id=$1 FOR UPDATE", o.ID).Scan(&status)
	if err == sql.ErrNoRows {
		return actionResult{}, fail(409, "This order has no dispute to reply to. Nothing was changed.")
	}
	if err != nil {
		return actionResult{}, err
	}
	if status != "Open" {
		return actionResult{}, fail(409, "This dispute is resolved, so no reply can be added. Nothing was changed.")
	}
	// A new statement after the lock: it sees a reply committed by a request that held the lock before this one.
	var d Dispute
	if err = tx.QueryRowContext(ctx, `SELECT `+disputeRecordCols+` FROM disputes d JOIN orders o ON o.id=d.order_id JOIN products p ON p.id=o.product_id`+disputeReplyJoin+
		` WHERE d.order_id=$1`, o.ID).Scan(&d.Opener, &d.ReplyRole, &d.Reply, &d.ReplyCreated); err != nil {
		return actionResult{}, err
	}
	role := roleBuyer
	if c.User.ID == o.VendorID {
		role = roleVendor
	}
	switch {
	case d.Opener == "":
		return actionResult{}, fail(409, "Who opened this dispute is not recorded, so no reply can be added. Send your evidence encrypted to dispute staff. Nothing was changed.")
	case d.Opener == role:
		return actionResult{}, fail(403, "You opened this dispute; only the other party can reply. Send further evidence encrypted to dispute staff from the order page.")
	case d.Reply != "":
		return actionResult{}, fail(409, "You already replied to this dispute; a reply cannot be changed or added to. Nothing was changed.")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", o.ID, stateDisputed, c.User.ID, disputeReplyPrefix+reply); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: orderPage(o.ID), Notice: "dispute-reply", Audit: "Replied to the dispute on order " + shortID(o.ID)}, nil
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
// to the order is refused by transition(). The form's payout confirmation is required (400), and the payout
// hook refuses the whole resolution (409, the dispute stays Open) when the counted amount is not amount_seen.
// Resolving queues a payout the watcher sends by itself, so it needs the resolver's current password, and an
// authenticator code when TOTP is enrolled (confirmedTx, A-32); the audit row says how it was confirmed.
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
	seen, err := payoutConfirmation(c)
	if err != nil {
		return actionResult{}, err
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		var orderID string
		err := c.Tx.QueryRowContext(c.Ctx(), "UPDATE disputes SET resolution=$1,outcome=$2,status=$3 WHERE id=$4 AND status='Open' AND outcome='' RETURNING order_id", resolution, outcome, status, c.Form.Get("id")).Scan(&orderID)
		if err == sql.ErrNoRows {
			return actionResult{}, fail(409, "Open dispute not found")
		}
		if err != nil {
			return actionResult{}, err
		}
		// A-280: the dispute row is locked (the reply takes the same lock), so a new statement sees any reply on
		// record. A form that did not show it (reply_seen) is refused and the desk shown again with the reply.
		var replyRole string
		if err = c.Tx.QueryRowContext(c.Ctx(), `SELECT COALESCE(r.role,'') FROM disputes d JOIN orders o ON o.id=d.order_id JOIN products p ON p.id=o.product_id`+disputeReplyJoin+
			` WHERE d.id=$1`, c.Form.Get("id")).Scan(&replyRole); err != nil {
			return actionResult{}, err
		}
		if replyRole != "" && c.Form.Get("reply_seen") != "1" {
			return actionResult{}, staleResolveForm(c, replyRole, resolution)
		}
		// An escrow order's decision moves coins from its escrow: only an administrator makes it (ES-4, SEC-8).
		var coin string
		if err = c.Tx.QueryRowContext(c.Ctx(), "SELECT COALESCE((SELECT coin FROM escrows WHERE order_id=$1),'')", orderID).Scan(&coin); err != nil {
			return actionResult{}, err
		}
		switch {
		case coin == "XMR" && c.User.Role != "admin":
			return actionResult{}, errXMREscrowResolveAdmin
		case coin != "" && c.User.Role != "admin":
			return actionResult{}, errEscrowResolveAdmin
		}
		_, err = c.A.transition(withPayoutSeen(c.Ctx(), orderID, seen), c.Tx, orderID, stateDisputed, stateResolved, c.User, status)
		if err == errReleaseFinal && coin == "XMR" {
			return actionResult{}, errXMRReleaseFinal // escrowSettleTerminal: the release requested at shipment has its transaction
		}
		if err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/moderator", Notice: "dispute-resolved", Audit: "Resolved dispute on order " + shortID(orderID) + " (outcome: " + outcome + ")"}, nil
	})
}

// staleResolveForm answers a resolve form loaded before the other party's reply (A-280) with 409 and the
// moderation desk again, after the rollback: the reply shown, the reason in its error alert and the typed decision
// kept for that dispute; the outcome and the password are chosen and typed again. When the desk cannot be
// loaded, the answer is plain text asking for a reload.
func staleResolveForm(c *actionCtx, replyRole, resolution string) error {
	msg := "The " + replyRole + " replied to this dispute after the form was loaded. Read the reply below and decide again. Nothing was changed."
	return &httpError{Code: 409, Msg: msg, Render: func(w http.ResponseWriter) {
		a := c.A
		d := a.pageData(c.R, "moderator", c.Token, c.User)
		if err := a.load(c.R, &d); err != nil {
			http.Error(w, "The "+replyRole+" replied to this dispute after the form was loaded. Reload the moderation desk, read the reply and decide again. Nothing was changed.", 409)
			return
		}
		d.Error, d.ResolveDispute, d.ResolveText = msg, c.Form.Get("id"), resolution
		a.renderStatus(w, c.R, d, 409)
	}}
}
