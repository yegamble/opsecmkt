package market

import (
	"context"
	"database/sql"
	"strings"
)

// Dispute decisions on escrow orders (WR-escrow Phase 1, slice P1-c2; ES-4, plan P1-c). A buyer or vendor may dispute
// a funded escrow order as a pooled one; only an administrator decides it (resolveAction refuses a moderator for an
// escrow order), confirming with their password (and authenticator code when enrolled). The decision is paid from
// the escrow by a settlement (escrowSettleTerminal): a refund to the pinned refund address at once, or a release the
// vendor claims with a fresh address. An administrator may change the decision on the order page
// (/escrow/decision) until its settlement has a final transaction (a market co-signature, or both parties'
// signatures): the settlement of the decision it replaces is then superseded and its outputs freed for the new one.
// Once the settlement is signed, broadcast, confirmed or spent outside the market, the decision is fixed.

func init() {
	registerAction("/escrow/decision", actionSpec{Roles: []string{"admin"}, OwnTx: true, Off: escrowOff, Run: escrowDecisionAction})
}

var (
	errEscrowResolveAdmin = fail(403, "Only an administrator decides a dispute on a Bitcoin escrow order: the decision moves coins from the escrow. Nothing was changed.")
	errDecisionOutcome    = fail(400, "Choose the new decision: release to the vendor or refund to the buyer. Nothing was changed.")
	errDecisionReason     = fail(400, "Give the reason for changing the decision in 20–5000 characters. Nothing was changed.")
	errDecisionState      = fail(409, "Only the decision of a resolved escrow order's dispute can be changed. Reload the page. Nothing was changed.")
	errDecisionSame       = fail(409, "That is already the decision of this dispute. Nothing was changed.")
	errDecisionFixed      = fail(409, "The decision is fixed: its settlement has a final transaction (co-signed by the market, or signed by both parties). Nothing was changed.")
	errDecisionParty      = fail(403, "You are a party to this order, so you cannot change its dispute decision.")
)

// decisionText names an outcome in an order note.
func decisionText(outcome string) string {
	if outcome == "release" {
		return "a release to the vendor"
	}
	return "a refund to the buyer"
}

// escrowDisputeOutcome is the outcome recorded on order's dispute ("" if none).
func escrowDisputeOutcome(ctx context.Context, q rowQuerier, orderID string) (string, error) {
	var outcome string
	err := q.QueryRowContext(ctx, "SELECT COALESCE((SELECT outcome FROM disputes WHERE order_id=$1),'')", orderID).Scan(&outcome)
	return outcome, err
}

// escrowDecisionAction changes the decision of a resolved escrow order's dispute (see the file comment).
func escrowDecisionAction(c *actionCtx) (actionResult, error) {
	outcome := c.Form.Get("outcome")
	status, ok := disputeOutcomes[outcome]
	if !ok {
		return actionResult{}, errDecisionOutcome
	}
	reason := strings.TrimSpace(c.Form.Get("reason"))
	if len(reason) < 20 || len(reason) > 5000 {
		return actionResult{}, errDecisionReason
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		o, err := scanOrder(c.Tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", c.Form.Get("order_id")))
		switch {
		case err == sql.ErrNoRows || err == nil && !o.Escrow:
			return actionResult{}, errNoOrder
		case err != nil:
			return actionResult{}, err
		case c.User.ID == o.BuyerID || c.User.ID == o.VendorID:
			return actionResult{}, errDecisionParty
		case o.State != stateResolved:
			return actionResult{}, errDecisionState
		}
		var current string
		if err = c.Tx.QueryRowContext(ctx, "SELECT outcome FROM disputes WHERE order_id=$1 FOR UPDATE", o.ID).Scan(&current); err != nil {
			return actionResult{}, err
		}
		if current == outcome {
			return actionResult{}, errDecisionSame
		}
		var sid int64
		var state string
		err = c.Tx.QueryRowContext(ctx, "SELECT id,state FROM settlements WHERE order_id=$1 AND kind IN ('release','refund') AND state<>'superseded' FOR UPDATE", o.ID).Scan(&sid, &state)
		switch {
		case err == sql.ErrNoRows:
		case err != nil:
			return actionResult{}, err
		case state == "signed" || state == "broadcast" || state == "confirmed" || state == "external":
			return actionResult{}, errDecisionFixed
		default:
			// The settlement of the old decision ends; its outputs are free for the new one.
			if _, err = c.Tx.ExecContext(ctx, "UPDATE settlements SET state='superseded',updated=now() WHERE id=$1", sid); err != nil {
				return actionResult{}, err
			}
			if _, err = c.Tx.ExecContext(ctx, "UPDATE payments SET settlement_id=NULL,updated=now() WHERE settlement_id=$1", sid); err != nil {
				return actionResult{}, err
			}
		}
		if _, err = c.Tx.ExecContext(ctx, "UPDATE disputes SET outcome=$2,status=$3 WHERE order_id=$1", o.ID, outcome, status); err != nil {
			return actionResult{}, err
		}
		note := "An administrator changed the dispute decision from " + decisionText(current) + " to " + decisionText(outcome) + ": " + reason
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", o.ID, o.State, c.User.ID, note); err != nil {
			return actionResult{}, err
		}
		if err = c.A.escrowSettleTerminal(ctx, c.Tx, &o); err != nil {
			return actionResult{}, err
		}
		body := "Order " + shortID(o.ID) + ": its dispute decision was changed to " + decisionText(outcome) + "; the order page shows the settlement that pays it."
		for _, uid := range []string{o.BuyerID, o.VendorID} {
			if _, err = c.Tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, body); err != nil {
				return actionResult{}, err
			}
		}
		return actionResult{Redirect: orderPage(o.ID) + "#escrow-decision", Notice: "escrow-decision",
			Audit: "Changed the escrow dispute decision of order " + shortID(o.ID) + " (" + current + " to " + outcome + ") (" + c.A.escrowNetwork() + ")"}, nil
	})
}

// escrowDecisionView shows a resolved escrow order's dispute decision, and offers an administrator who is not a party
// the form to change it while its settlement has no final transaction.
func (a *App) escrowDecisionView(ctx context.Context, d *PageData, v *EscrowOrderView) error {
	o := d.Order
	if o.State != stateResolved {
		return nil
	}
	outcome, err := escrowDisputeOutcome(ctx, a.db, o.ID)
	if err != nil || outcome == "" {
		return err
	}
	dv := &EscrowDecisionView{Outcome: outcome, Text: "Dispute decided: " + decisionText(outcome) + "."}
	var fixed bool
	if err = a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM settlements WHERE order_id=$1 AND kind IN ('release','refund')
		AND state IN ('signed','broadcast','confirmed','external'))`, o.ID).Scan(&fixed); err != nil {
		return err
	}
	if fixed {
		dv.Text += " It is fixed: its settlement has a final transaction."
	} else {
		dv.Text += " An administrator may change it until its settlement has a final transaction."
	}
	if outcome == "release" {
		v.NextStep = "The dispute was decided for the vendor: the vendor claims the escrowed coins from this page."
	} else {
		v.NextStep = "The dispute was decided for the buyer: the coins in this escrow go back to the buyer's refund address through the refund settlement below."
	}
	u := d.User
	if !fixed && u != nil && u.Role == "admin" && u.ID != o.BuyerID && u.ID != o.VendorID && a.escrowConfigured {
		dv.CanChange = true
		if d.Security == nil {
			if d.Security, _, _, err = a.securityView(ctx, u.ID); err != nil {
				return err
			}
		}
	}
	v.Decision = dv
	return nil
}

// escrowDisputePreview is the resolve form's statement of each outcome for an escrow order: the counted escrowed amount
// (as escrowSettleTerminal counts it) and the settlement each outcome builds.
func (a *App) escrowDisputePreview(ctx context.Context, o *Order) (PayoutPreview, error) {
	var sum int64
	if err := a.db.QueryRowContext(ctx, `SELECT COALESCE(sum(amount),0) FROM payments WHERE order_id=$1 AND confirmations>=0 AND NOT locked AND NOT after_close`, o.ID).Scan(&sum); err != nil {
		return PayoutPreview{}, err
	}
	btc := amount(sum, 8) + " " + a.escrowNetwork() + " BTC"
	return PayoutPreview{Seen: sum, Confirm: resolveConfirm,
		Release: "Release to " + o.Vendor + ": the vendor claims the escrowed " + btc + " by a settlement from the escrow (less the market fee and the network fee; any amount paid above the price goes back to the buyer)",
		Refund:  "Refund to " + o.Buyer + ": the escrowed " + btc + ", less the network fee, go back to the buyer's refund address by a settlement from the escrow"}, nil
}
