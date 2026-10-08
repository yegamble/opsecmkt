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
		if _, err = c.A.transition(withConcessionTransition(withPayoutSeen(ctx, orderID, seen), orderID, to, c.User.ID), c.Tx, orderID, stateDisputed, to, c.User, status); err != nil {
			return actionResult{}, err
		}
		if err = notifyConcededStaff(ctx, c.Tx, orderID, status); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: orderPage(orderID) + "#order-dispute", Notice: "dispute-conceded",
			Audit: "Conceded dispute on order " + shortID(orderID) + " (" + c.Form.Get("outcome") + ")"}, nil
	})
}

// Opening a dispute stores no durable recipient relation, only short-ID
// notification text. Tell staff who can act now, with the same independent
// unsuspended-staff predicate and no-resolver admin fallback as its opening
// notice. A newly appointed staff member may hear the closure; a former or
// suspended staff member does not.
func notifyConcededStaff(ctx context.Context, tx *sql.Tx, orderID, status string) error {
	var buyerID, vendorID string
	if err := tx.QueryRowContext(ctx, `SELECT o.buyer_id,p.vendor_id FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=$1`, orderID).Scan(&buyerID, &vendorID); err != nil {
		return err
	}
	staff, err := collectStrings(ctx, tx, "SELECT id FROM users WHERE role IN ('moderator','admin') AND suspended_at IS NULL AND id<>$1 AND id<>$2", buyerID, vendorID)
	if err != nil {
		return err
	}
	if len(staff) == 0 {
		staff, err = collectStrings(ctx, tx, "SELECT id FROM users WHERE role='admin' AND suspended_at IS NULL")
		if err != nil {
			return err
		}
	}
	for _, uid := range staff {
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid,
			"Dispute on order "+shortID(orderID)+" is now "+status+". It no longer needs a resolution; review the order history."); err != nil {
			return err
		}
	}
	return nil
}

// concessionPreview uses the same counted amount and outcome descriptions as
// the staff resolve form. Only the current party's outcome is exposed on the
// order page; no payout address is revealed to the other party.
func (a *App) concessionPreview(ctx context.Context, o *Order, party string) (*ConcessionPreview, error) {
	view := &ConcessionPreview{Confirm: "I checked the amount and recipient above. Conceding this dispute is final."}
	kind, recipientID, recipient := "release", o.VendorID, o.Vendor
	if party == roleVendor {
		kind, recipientID, recipient = "refund", o.BuyerID, o.Buyer
	}
	view.Outcome = kind
	if o.Escrow {
		p, err := a.escrowDisputePreview(ctx, o)
		if err != nil {
			return nil, err
		}
		view.Seen, view.Statement = p.Seen, p.Release
		if kind == "refund" {
			view.Statement = p.Refund
		}
		return view, nil
	}
	_, sum, _, err := a.payoutBasis(ctx, a.db, o, false)
	if err != nil {
		return nil, err
	}
	view.Seen = sum
	view.Statement = payoutStatement(kind, recipient, "the "+map[string]string{"release": "vendor", "refund": "buyer"}[kind], a.netName(), o, sum)
	column := "payout_btc"
	if o.Currency == "XMR" {
		column = "payout_xmr"
	}
	var noAddress, suspended bool
	if err = a.db.QueryRowContext(ctx, "SELECT "+column+"='',suspended_at IS NOT NULL FROM users WHERE id=$1", recipientID).Scan(&noAddress, &suspended); err != nil {
		return nil, err
	}
	if suspended {
		view.Statement += "; account suspended: an administrator must check the address"
	} else if noAddress {
		view.Statement += "; no payout address: the payout waits for one"
	}
	return view, nil
}
