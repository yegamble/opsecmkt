package market

import (
	"context"
	"database/sql"
)

// Dispute decisions on escrow orders (WR-escrow Phase 1, slice P1-c2; ES-4, plan P1-c). A buyer or vendor may dispute
// a funded escrow order as a pooled one; only an administrator decides it (resolveAction refuses a moderator for an
// escrow order), confirming with their password (and authenticator code when enrolled). The decision is paid from
// the escrow by a settlement (escrowSettleTerminal): a refund to the pinned refund address at once, or a release the
// vendor claims with a fresh address. An administrator may change the decision on the order page
// (/escrow/decision) until its settlement has a final transaction (a market co-signature, or both parties'
// signatures): the settlement of the decision it replaces is then superseded and its outputs freed for the new one,
// whether it was built or not (still building, or failed: migration 087, A-462); the deposits a refund's too-small
// failure left out for a stray refund count for the order again (countAgain). Once the settlement is signed,
// broadcast, confirmed or spent outside the market, the decision is fixed.

func init() {
	registerAction("/escrow/decision", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, ReceiptAuthorize: btcDecisionReceiptAuthorize, Off: escrowOff, Run: escrowDecisionAction})
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
	reason := normalizedFormText(c.Form.Get("reason"))
	if !textLength(reason, 20, 5000) {
		return actionResult{}, errDecisionReason
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		o, err := scanOrder(c.Tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", c.Form.Get("order_id")))
		switch {
		case err == sql.ErrNoRows || err == nil && !o.btcEscrow():
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
		var state, again string
		err = c.Tx.QueryRowContext(ctx, "SELECT id,state FROM settlements WHERE order_id=$1 AND kind IN ('release','refund') AND state<>'superseded' FOR UPDATE", o.ID).Scan(&sid, &state)
		switch {
		case err == sql.ErrNoRows:
		case err != nil:
			return actionResult{}, err
		case state == "signed" || state == "broadcast" || state == "confirmed" || state == "external":
			return actionResult{}, errDecisionFixed
		default:
			// The settlement of the old decision ends, built or not (migration 087); its outputs are free for the new
			// one, and so are the deposits its too-small failure left out for a stray refund (A-462).
			if _, err = c.Tx.ExecContext(ctx, "UPDATE settlements SET state='superseded',updated=now() WHERE id=$1", sid); err != nil {
				return actionResult{}, err
			}
			if _, err = c.Tx.ExecContext(ctx, "UPDATE payments SET settlement_id=NULL,updated=now() WHERE settlement_id=$1", sid); err != nil {
				return actionResult{}, err
			}
			if again, err = c.A.countAgain(ctx, c.Tx, &o, sid); err != nil {
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
		if again != "" {
			if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, o.State, again); err != nil {
				return actionResult{}, err
			}
		}
		if err = c.A.escrowSettleTerminal(ctx, c.Tx, &o); err != nil {
			return actionResult{}, err
		}
		body := "Order " + shortID(o.ID) + ": its dispute decision was changed to " + decisionText(outcome) + "; the order page shows the settlement that pays it."
		for _, uid := range []string{o.BuyerID, o.VendorID} {
			if _, err = insertNotification(ctx, c.Tx, uid, body, o.ID); err != nil {
				return actionResult{}, err
			}
		}
		if err = notifyDisputeObservers(ctx, c.Tx, o.ID, c.User.ID, "Dispute on order "+shortID(o.ID)+" has an updated administrator decision. Review the order history."); err != nil {
			return actionResult{}, err
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
	// P2-R4: a live release older than the dispute is the one built when the order was shipped (a claim comes after).
	// A-437: a failed refund is the live decision's (a refund a changed decision superseded, built or not, is in state
	// superseded: migration 087), and only its too-small failure left the coins out.
	var shipped, tooFew bool
	if err = a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM settlements s JOIN disputes d ON d.order_id=s.order_id WHERE s.order_id=$1
		AND s.kind='release' AND s.state<>'superseded' AND s.created<d.created),
		EXISTS(SELECT 1 FROM settlements WHERE order_id=$1 AND kind='refund' AND state='failed' AND build_error=$2)`, o.ID, settleTooSmall).Scan(&shipped, &tooFew); err != nil {
		return err
	}
	switch {
	case outcome == "release" && shipped:
		// P2-R4: the release built when the order was shipped is the decision's settlement; no new claim is asked for.
		v.NextStep = "The dispute was decided for the vendor: the release to the vendor built when the order was shipped pays the vendor; it is shown below. The release deadline no longer applies."
	case outcome == "release":
		v.NextStep = "The dispute was decided for the vendor: the vendor claims the escrowed coins from this page."
	case tooFew:
		// A-437: the refund's coins were left out for a stray refund (leaveOutTooFew); each deposit line says where it is.
		v.NextStep = "The dispute was decided for the buyer, but the market could not build the refund: the coins in this escrow were too few to pay the network fee and an output of at least 0.0001 BTC. A stray refund returns them, together with any later deposit, to the buyer's refund address once together they can pay the network fee; each deposit below says where it stands."
	default:
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

// escrowAmount states n atomic units of o's escrow coin with its network: the Bitcoin escrow node's chain, or the
// network Monero escrow was verified on (none named before one was, A-472: it said fakechain, wrong on stagenet).
func (a *App) escrowAmount(o *Order, n int64) string {
	if o.Currency == "XMR" {
		if net := a.xmrEscrowNetKnown(); net != "" {
			return amount(n, currencyDecimals(o.Currency)) + " " + net + " XMR"
		}
		return amount(n, currencyDecimals(o.Currency)) + " XMR"
	}
	return amount(n, currencyDecimals(o.Currency)) + " " + a.escrowNetwork() + " BTC"
}

// escrowDisputePreview is the resolve form's statement of each outcome for an escrow order: the counted escrowed amount
// (as escrowSettleTerminal counts it) and the settlement each outcome builds, with what a release does with an amount
// paid above the price (escrowExcess, A-508).
func (a *App) escrowDisputePreview(ctx context.Context, o *Order) (PayoutPreview, error) {
	if o.xmrEscrow() {
		return a.xmrDisputePreview(ctx, o)
	}
	var sum, left int64
	if err := a.db.QueryRowContext(ctx, escrowCountedSQL, o.ID).Scan(&sum, &left); err != nil {
		return PayoutPreview{}, err
	}
	escrowed, fees := a.escrowAmount(o, sum), escrowReleaseFees(a.escrowExcess(o, sum-left))
	p := PayoutPreview{Seen: sum, Confirm: resolveConfirm,
		Release: "Release to " + o.Vendor + ": the vendor claims the escrowed " + escrowed + " by a settlement from the escrow " + fees,
		Refund:  "Refund to " + o.Buyer + ": the escrowed " + escrowed + ", less the network fee, go back to the buyer's refund address by a settlement from the escrow"}
	// P2-R4: a physical order's release built when it was shipped.
	var release, final bool
	if err := a.db.QueryRowContext(ctx, `SELECT count(*)>0,COALESCE(bool_or(raw_tx IS NOT NULL OR state='external'),false) FROM settlements
		WHERE order_id=$1 AND kind='release' AND state<>'superseded'`, o.ID).Scan(&release, &final); err != nil {
		return PayoutPreview{}, err
	}
	switch {
	case final:
		p.Release = "Release to " + o.Vendor + ": the release to the vendor built when the order was shipped is already final, so the escrow pays the vendor"
		p.Refund = "Refund to " + o.Buyer + ": refused, because the release to the vendor built when the order was shipped is already final"
	case release:
		p.Release = "Release to " + o.Vendor + ": the release to the vendor built when the order was shipped pays the escrowed " + escrowed + " to the claim address the vendor gave then " + fees
		p.Refund += "; the release to the vendor built when the order was shipped is cancelled"
	}
	if left > 0 && release && !final {
		// A-374, A-387: deposits left out of the built release are not spent by the decision's settlement; a stray refund
		// returns them to the buyer once the order is closed.
		settled, back := a.escrowAmount(o, sum-left), "the "+a.escrowAmount(o, left)+" left out of the release (deposits that arrived after it was built) goes back to the buyer's refund address by a stray refund once the order is closed"
		p.Release = "Release to " + o.Vendor + ": the release to the vendor built when the order was shipped pays " + settled + " to the claim address the vendor gave then " + fees + "; " + back
		p.Refund = "Refund to " + o.Buyer + ": the release to the vendor built when the order was shipped is cancelled, and the refund settlement goes back " + settled + ", less the network fee, to the buyer's refund address; " + back
	}
	return p, nil
}

// escrowReleaseFees is what a release statement says it pays less, with excess (escrowExcess) when there is one.
func escrowReleaseFees(excess string) string {
	if excess == "" {
		return "(less the market fee and the network fee)"
	}
	return "(less the market fee and the network fee; " + excess + ")"
}
