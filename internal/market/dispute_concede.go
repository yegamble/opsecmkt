package market

import (
	"context"
	"database/sql"
)

// A-166: a party can concede an open dispute only to the other party. The
// dispute compare-and-set precedes the order lock, like /resolve; every effect
// (including the payout request and notices) commits in that same transaction.
func init() {
	registerAction("/disputes/concede", actionSpec{Roles: []string{"buyer", "vendor", "moderator", "admin"}, OwnTx: true,
		ConfirmedFormNonce: true, ReceiptAuthorize: concedeReceiptAuthorize, Run: concedeAction})
}

const (
	concededByBuyer  = "Withdrawn — conceded by buyer"
	concededByVendor = "Withdrawn — conceded by vendor"
)

// Only concedeAction can mark this transition. The normal order actions must
// never be able to take the same state edge without the dispute CAS, amount
// confirmation, step-up and receipt, even if their form fields are forged.
type concessionTransitionKey struct{}
type concessionTransition struct{ orderID, to, actorID string }

func withConcessionTransition(ctx context.Context, orderID, to, actorID string) context.Context {
	return context.WithValue(ctx, concessionTransitionKey{}, concessionTransition{orderID, to, actorID})
}

func concedeChoice(outcome string) (party, to, status string, ok bool) {
	switch outcome {
	case "release":
		return roleBuyer, stateCompleted, concededByBuyer, true
	case "refund":
		return roleVendor, stateCancelled, concededByVendor, true
	}
	return "", "", "", false
}

// The replay gate checks immutable identity/party eligibility, not the open
// state: a successful concession itself closes that state. It takes no order
// or dispute lock before confirmedTx claims the receipt.
func concedeReceiptAuthorize(c *actionCtx) error {
	party, _, _, ok := concedeChoice(c.Form.Get("outcome"))
	if !ok {
		return fail(400, "Choose whether the buyer releases to the vendor or the vendor refunds the buyer. Nothing was changed.")
	}
	var buyerID, vendorID string
	err := c.Tx.QueryRowContext(c.Ctx(), `SELECT o.buyer_id,p.vendor_id FROM disputes d
		JOIN orders o ON o.id=d.order_id JOIN products p ON p.id=o.product_id WHERE d.id=$1`, c.Form.Get("id")).Scan(&buyerID, &vendorID)
	if err == sql.ErrNoRows {
		return errNoOrder
	}
	if err != nil {
		return err
	}
	if buyerID == vendorID || party == roleBuyer && c.User.ID != buyerID || party == roleVendor && c.User.ID != vendorID {
		return errForbidden
	}
	return nil
}

func concedeAction(c *actionCtx) (actionResult, error) {
	party, to, status, ok := concedeChoice(c.Form.Get("outcome"))
	if !ok {
		return actionResult{}, fail(400, "Choose whether the buyer releases to the vendor or the vendor refunds the buyer. Nothing was changed.")
	}
	seen, err := payoutConfirmation(c)
	if err != nil {
		return actionResult{}, err
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		var orderID string
		err := c.Tx.QueryRowContext(ctx, `UPDATE disputes SET status=$1,outcome=$2,resolution=$3
			WHERE id=$4 AND status='Open' AND outcome='' RETURNING order_id`, status, c.Form.Get("outcome"),
			"The "+party+" conceded the dispute. The "+map[string]string{"release": "vendor receives the release", "refund": "buyer receives the refund"}[c.Form.Get("outcome")]+".", c.Form.Get("id")).Scan(&orderID)
		if err == sql.ErrNoRows {
			return actionResult{}, fail(409, "Open dispute not found")
		}
		if err != nil {
			return actionResult{}, err
		}
		// The current party is checked again by transition's locked order row.
		tctx := withConcessionTransition(withPayoutSeen(ctx, orderID, seen), orderID, to, c.User.ID)
		if to == stateCompleted { // A-710: the buyer's concession leaves an unshipped order's release for the vendor to claim
			if tctx, err = xmrClaimCueCtx(tctx, c.Tx, orderID, to); err != nil {
				return actionResult{}, err
			}
		}
		if _, err = c.A.transition(tctx, c.Tx, orderID, stateDisputed, to, c.User, status); err != nil {
			return actionResult{}, err
		}
		if err = notifyConcededStaff(ctx, c.Tx, orderID, status, c.User.ID); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: orderPage(orderID) + "#order-dispute", Notice: "dispute-conceded",
			Audit: "Conceded dispute on order " + shortID(orderID) + " (" + c.Form.Get("outcome") + ")"}, nil
	})
}

// Tell current dispute observers, including moderators who follow an escrow dispute without deciding it.
func notifyConcededStaff(ctx context.Context, tx *sql.Tx, orderID, status, actorID string) error {
	return notifyDisputeObservers(ctx, tx, orderID, actorID,
		"Dispute on order "+shortID(orderID)+" is now "+status+". It no longer needs a resolution; review the order history.")
}

// concessionPreview uses the same counted amount and outcome descriptions as
// the staff resolve form (escrowDisputePreview). Only the current party's
// outcome is exposed on the order page. An order without escrow holds no coins
// (pooled payments are removed, A-492), so its concession moves none.
func (a *App) concessionPreview(ctx context.Context, o *Order, party string) (*ConcessionPreview, error) {
	view := &ConcessionPreview{Confirm: "I checked the amount and recipient above. Conceding this dispute is final.", Outcome: "release"}
	if party == roleVendor {
		view.Outcome = "refund"
	}
	if !o.Escrow {
		view.Statement = "No escrow holds coins for this order, so conceding sends nothing"
		return view, nil
	}
	p, err := a.escrowDisputePreview(ctx, o)
	if err != nil {
		return nil, err
	}
	view.Seen, view.Statement = p.Seen, p.Release
	if view.Outcome == "refund" {
		view.Statement = p.Refund
	}
	return view, nil
}
