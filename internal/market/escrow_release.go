package market

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The release of a physical escrow order (WR-escrow Phase 2, slice P2-b; P2-R1, P2-R2, P2-R6; owner decision ES-11:
// the buyer signs the release). Behind ESCROW_BTC (off by default); regtest only.
//
//  1. Mark shipped (/escrow/ship): the vendor gives a fresh claim address (the claim action's checks) and their
//     password; in one transaction the order moves paid -> shipped, the release is requested to that address (kind
//     release, state building) and escrows.release_at is the escrow's snapshot of release_days from now. The pooled
//     ship form refuses an escrow order.
//  2. The watcher builds and tracks that release while the order is shipped (settlementBuildable), long before the
//     order has an outcome; settlementFits stays terminal-only and gates every market signature.
//  3. The vendor and the buyer sign it through the existing sign action; the buyer's signature confirms receipt, and a
//     buyer who signed may no longer open a dispute. With both signatures the watcher finalizes them alone, checks the
//     transaction and testmempoolaccept, and stores it (cosigned 'parties') while completing the order (system,
//     shipped -> completed) in the same transaction, under the order row lock, rechecked shipped with no dispute
//     (finalizeParties, storeFinal). The market never signs before the outcome: if the two signatures do not verify
//     together, both are discarded (without its own signature the market cannot tell which failed) and the release
//     waits for both again; migration 081 refuses a market final transaction on an order whose state does not fit.
//  4. A deposit that arrives once the order's release or refund is built (past building) is not spent by it: it is
//     left out (payments.escrow_left_out), announced once, and returned by the stray refund once the order is closed
//     (leaveOutTopUps, A-374).

func init() {
	registerAction("/escrow/ship", actionSpec{OwnTx: true, OneAtATime: true, Off: escrowOff, Run: escrowShipAction})
}

var (
	errShipOnlyVendor  = fail(403, "Only the vendor marks this order shipped.")
	errShipPooledForm  = fail(409, "This order is paid into a Bitcoin escrow: mark it shipped with the escrow form on the order page, which takes the claim address the release to you pays, and your password. Nothing was changed.")
	errShipNoDeadline  = fail(409, "This escrow has no release deadline length recorded, so it cannot be marked shipped here. Market staff must check it. Nothing was changed.")
	errEscrowComplete  = fail(409, "This order is paid into a Bitcoin escrow: you confirm receipt by signing the release to the vendor on the order page, not with this form. Nothing was changed.")
	errSignDisputed    = fail(409, "This order is disputed: an administrator decides it, and the decision settles the escrow, so the buyer does not sign the release now. Nothing was changed.")
	errDisputeSigned   = fail(409, "You signed the release to the vendor, which confirms receipt of this order, so it can no longer be disputed. Nothing was changed.")
	errReleaseFinal    = fail(409, "The release to the vendor built when this order was shipped is already final (signed by both parties, or broadcast or spent on the network), so the escrow pays the vendor and the dispute cannot be decided for the buyer. Decide it for the vendor. Nothing was changed.")
	escrowShippedNote  = "Release requested at shipment (Bitcoin escrow): the market builds the release to the vendor, paying the claim address the vendor gave now (less the market fee and the network fee; any amount paid above the price goes back to the buyer's refund address). The vendor signs it; the buyer signs it once the goods have arrived, which confirms receipt and ends the buyer's right to open a dispute. With both signatures the market broadcasts the transaction they signed; the market never signs it before the order is completed. Nothing is sent yet."
	escrowReleasedNote = "Completed: the buyer and the vendor signed the release to the vendor (Bitcoin escrow); the buyer's signature confirms receipt. The market did not sign it."
	escrowSeenNote     = "Completed: the release to the vendor is on the network, signed outside the market by two of the escrow's keys; the market did not sign it."
)

// settlementBuildable reports whether a settlement of kind may be built, signed by the parties and followed for an
// order in state with dispute outcome: everything settlementFits allows, and a physical order's release from the moment
// it is shipped, while it is shipped or disputed (P2-R1, A-376). Only settlementFits allows the market's signature.
func settlementBuildable(kind, state, outcome string) bool {
	return settlementFits(kind, state, outcome) || releaseBeforeOutcome(kind, state)
}

// releaseBeforeOutcome reports whether a settlement of kind on an order in state is a physical order's release before
// the order's outcome (shipped, or disputed and not yet decided): only both parties' signatures complete it.
func releaseBeforeOutcome(kind, state string) bool {
	return kind == "release" && (state == stateShipped || state == stateDisputed)
}

// checkClaimAddress applies the claim action's checks to a vendor's claim address dest: never used on this market (an
// escrow, refund or fee address, a payment address or another settlement's destination) and valid on the escrow node's
// chain.
func checkClaimAddress(c *actionCtx, e *escrowBTC, dest string) error {
	ctx := c.Ctx()
	_, feeAddr, err := escrowFee(ctx, c.Tx, e)
	if err != nil {
		return err
	}
	var used bool
	if err = c.Tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM escrows WHERE address=$1 OR refund_address=$1 OR fee_destination=$1) OR $1=$2
		OR EXISTS(SELECT 1 FROM payment_addresses WHERE address=$1) OR EXISTS(SELECT 1 FROM settlements WHERE destination=$1)`, dest, feeAddr).Scan(&used); err != nil {
		return err
	}
	if used {
		return errClaimAddressUsed
	}
	valid, err := e.validAddress(ctx, dest)
	if err != nil {
		log.Printf("escrow: claim address not checked: %s", errorClassOf(err))
		return errClaimNode
	}
	if !valid {
		return errClaimInvalid(c.A.escrowNetwork())
	}
	return nil
}

// insertRelease requests the release of order to dest (state building), requested by the vendor, mapping the
// database's refusals: a second outcome, or a destination another claim pays.
func insertRelease(c *actionCtx, orderID, dest string) error {
	_, err := c.Tx.ExecContext(c.Ctx(), "INSERT INTO settlements(order_id,kind,destination,requested_by) VALUES($1,'release',$2,$3)", orderID, dest, c.User.ID)
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "settlements_one_outcome":
		return errClaimTwice
	case errors.As(err, &pe) && pe.Code == "23505":
		return errClaimAddressUsed
	}
	return err
}

// escrowShipAction: the vendor of a paid physical escrow order marks it shipped (step 1 of the file comment).
func escrowShipAction(c *actionCtx) (actionResult, error) {
	e := c.A.escrow.Load()
	if e == nil {
		return actionResult{}, errEscrowDown
	}
	note, err := formNote(c, "Marked shipped")
	if err != nil {
		return actionResult{}, err
	}
	dest, ok := escrowAddressForm(c.Form.Get("address"))
	if !ok {
		return actionResult{}, errClaimAddress(c.A.escrowNetwork())
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		o, err := partyOrder(c, c.Form.Get("order_id"))
		switch {
		case err != nil:
			return actionResult{}, err
		case !o.btcEscrow():
			return actionResult{}, errNoOrder
		case o.VendorID != c.User.ID:
			return actionResult{}, errShipOnlyVendor
		case o.State != statePaid:
			return actionResult{}, shipConflict(c, o.ID)
		}
		if err = checkClaimAddress(c, e, dest); err != nil {
			return actionResult{}, err
		}
		// P2-R9: the buyer's shipped notice states the release deadline, the value release_at takes below (now() is the
		// transaction's start).
		var deadline time.Time
		err = c.Tx.QueryRowContext(ctx, "SELECT now()+make_interval(days => release_days) FROM escrows WHERE order_id=$1 AND coin='BTC' AND release_days IS NOT NULL AND release_at IS NULL", o.ID).Scan(&deadline)
		if err == sql.ErrNoRows {
			return actionResult{}, errShipNoDeadline
		}
		if err != nil {
			return actionResult{}, err
		}
		at := utcUntil(deadline)
		ctx = withNoticeDetail(ctx, o.ID, stateShipped, "Release deadline: "+at+". Sign the release on the order page once the goods have arrived; if they have not arrived, open a dispute before then. "+
			"If you do neither, the order completes at that time and the market co-signs the vendor's signature on the release: if you cannot sign, you need do nothing.")
		if _, err = c.A.transition(ctx, c.Tx, o.ID, statePaid, stateShipped, c.User, note); err == errConflict {
			return actionResult{}, shipConflict(c, o.ID) // P2-R5: the buyer's no-ship cancel took the order row first
		} else if err != nil {
			return actionResult{}, err
		}
		res, err := c.Tx.ExecContext(ctx, "UPDATE escrows SET release_at=now()+make_interval(days => release_days) WHERE order_id=$1 AND coin='BTC' AND release_days IS NOT NULL AND release_at IS NULL", o.ID)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return actionResult{}, errShipNoDeadline
		}
		if err = insertRelease(c, o.ID, dest); err != nil {
			return actionResult{}, err
		}
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, stateShipped, escrowShippedNote); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: orderPage(o.ID) + "#escrow-settlement", Notice: "order-shipped",
			Audit: "Marked order " + shortID(o.ID) + " shipped (Bitcoin escrow, " + c.A.escrowNetwork() + ")"}, nil
	})
}

// finalizeParties completes a party-signed release before its order's outcome (step 3 of the file comment): only on a
// shipped order with both parties' signatures; otherwise it waits (for the other party, or for the dispute's
// decision). It never reaches the market's key: no arbiter call is made here.
func (a *App) finalizeParties(ctx context.Context, e *escrowBTC, b *settleBuild) error {
	if b.Kind != "release" || b.OrderState != stateShipped || len(b.Parties) != 2 {
		return nil
	}
	both, err := withPartySigs(b.Stored, b.Parties...)
	if err != nil {
		return err
	}
	rawHex, complete, err := e.finalizePSBT(ctx, both)
	if err != nil {
		return err
	}
	if complete {
		verdict, class, err := a.checkAndAccept(ctx, e, b, rawHex)
		switch {
		case err != nil:
			return err
		case verdict == acceptOK:
			return a.storeFinal(ctx, b, rawHex, "parties", []string{b.Parties[0].Party, b.Parties[1].Party}, nil)
		case verdict == acceptExternal:
			return a.spentBeforeCosign(ctx, e, b)
		case verdict == acceptWait:
			return a.settlementSendWaits(ctx, b.ID, b.OrderID, class)
		}
	}
	// The two do not verify together. Before the outcome the market does not sign, so it cannot find which one failed
	// by co-signing with each (A-339): both are discarded and the release waits for both again (A-372).
	return a.revertSettlementWith(ctx, b, []string{roleBuyer, roleVendor}, []string{
		"The buyer's and the vendor's signatures on the release to the vendor did not verify together when the market finalized the transaction: at least one of them is not a valid signature of this transaction by that party's escrow key for this order. Before the order is completed the market never signs, so it cannot tell which one: both were discarded.",
		"The release waits for both signatures again: the vendor and the buyer each sign it on the order page. Nothing was sent.",
	})
}

// completeShipped completes a shipped escrow order whose release has just been stored with a final transaction made
// without the market (finalizeParties, or the parties' own broadcast), in tx, by the system through the escrow-only
// edge shipped -> completed. The order row was locked and checked by lockShipped first.
func (a *App) completeShipped(ctx context.Context, tx *sql.Tx, orderID, note string) error {
	_, err := a.transition(ctx, tx, orderID, stateShipped, stateCompleted, nil, note)
	return err
}

// lockShipped locks the order row (before the settlement's, the lock order of every order action) and reports whether
// it is still shipped with no dispute.
func lockShipped(ctx context.Context, tx *sql.Tx, orderID string) (bool, error) {
	var state string
	var disputed bool
	err := tx.QueryRowContext(ctx, "SELECT o.state,EXISTS(SELECT 1 FROM disputes d WHERE d.order_id=o.id) FROM orders o WHERE o.id=$1 FOR UPDATE", orderID).Scan(&state, &disputed)
	return state == stateShipped && !disputed, err
}

// leaveOutTopUps marks, for an escrow order whose release or refund is built (past building, not superseded), every
// deposit that counts and that no settlement spends as left out (step 4 of the file comment), in tx under the order
// row lock: one order note, a notice to the buyer and to staff (flagOrder), only for the deposits newly marked, which
// are flagged as a deposit after the order closed is (flagLateDeposits), so staff told of them can read the order. The
// stray refund returns them to the buyer's refund address once the order is closed and they are confirmed.
func (a *App) leaveOutTopUps(ctx context.Context, tx *sql.Tx, o *Order) error {
	var kind string
	err := tx.QueryRowContext(ctx, `SELECT kind FROM settlements WHERE order_id=$1 AND kind IN ('release','refund')
		AND state IN ('awaiting_signature','party_signed','signed','broadcast','confirmed')`, o.ID).Scan(&kind)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE payments SET escrow_left_out=true,flagged=true,updated=now() WHERE order_id=$1 AND settlement_id IS NULL AND confirmations>=0
		AND NOT locked AND NOT after_close AND NOT escrow_left_out AND NOT spent_outside RETURNING amount`, o.ID)
	if err != nil {
		return err
	}
	var n, sum int64
	for rows.Next() {
		var amt int64
		if err = rows.Scan(&amt); err != nil {
			rows.Close()
			return err
		}
		n, sum = n+1, sum+amt
	}
	rows.Close()
	if err = rows.Err(); err != nil || n == 0 {
		return err
	}
	what := "its release to the vendor"
	if kind == "refund" {
		what = "its refund to the buyer"
	}
	count := "1 deposit"
	if n > 1 {
		count = strconv.FormatInt(n, 10) + " deposits"
	}
	note := count + " (" + amount(sum, 8) + " " + a.escrowNetwork() + " BTC) arrived at this order's escrow after " + what + " was built, so that settlement does not spend them. " +
		"They stay in the escrow; once the order is closed and they are confirmed, a stray refund returns them to the buyer's refund address. Do not send more coins to this escrow."
	if err = a.flagOrder(ctx, tx, o, note, ""); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), o.BuyerID, "Order "+shortID(o.ID)+": "+note)
	return err
}

// leaveOutUncounted marks, for a cancelled Bitcoin escrow order with no refund that spends or will spend its deposits
// (none requested, or only failed, superseded or external ones), every deposit that has reached the confirmation
// threshold, that no settlement spends and that was neither recorded after the order closed nor already left out
// (A-428): one Core reported as conflicted (negative confirmations) or no longer listed (markMissing) when the order
// was cancelled, so it did not count, mined since; or one a refund did not spend because Core reported it so while the
// refund was being built (which then failed with nothing to settle, or too-small: A-435, whose own deposits
// leaveOutTooFew left out when it failed), or because that refund ended external before it was built (QA428-4). As
// leaveOutTopUps does, in tx under the order row lock: one order note, a notice to the buyer and to staff, only for the
// deposits newly marked. The stray refund returns them to the buyer's refund address, with a too-small refund's coins.
// xmrLeaveOutUncounted is the Monero escrow's (A-424).
//
// On a database upgraded from before A-435, a refund that failed too-small left the deposits it tried counted, not
// left out (QA NIT-1). They did count, so they are left out here with leaveOutTooFew's note instead. Such a failure is
// known by the order having no deposit left out and flagged other than after the order closed: since A-435 the failure
// itself leaves its deposits out and flags them, and before it nothing else on such an order did (leaveOutTopUps acts
// on a built refund only; a bounded build's leaveOut does not flag).
func (a *App) leaveOutUncounted(ctx context.Context, tx *sql.Tx, o *Order, threshold int64) error {
	if o.State != stateCancelled {
		return nil
	}
	var n, sum int64
	var upgraded bool
	if err := tx.QueryRowContext(ctx, `WITH m AS (UPDATE payments SET escrow_left_out=true,flagged=true,updated=now() WHERE order_id=$1 AND settlement_id IS NULL
			AND confirmations>=$2 AND NOT locked AND NOT after_close AND NOT escrow_left_out AND NOT spent_outside
			AND NOT EXISTS (SELECT 1 FROM settlements WHERE order_id=$1 AND kind='refund'
				AND state IN ('building','awaiting_signature','party_signed','signed','broadcast','confirmed')) RETURNING amount)
		SELECT count(*),COALESCE(sum(amount),0),
			EXISTS(SELECT 1 FROM settlements WHERE order_id=$1 AND kind='refund' AND state='failed' AND build_error=$3)
			AND NOT EXISTS(SELECT 1 FROM payments WHERE order_id=$1 AND escrow_left_out AND flagged AND NOT after_close) FROM m`, o.ID, threshold, settleTooSmall).Scan(&n, &sum, &upgraded); err != nil || n == 0 {
		return err
	}
	count, is, it, stays := "1 deposit", "is", "it", "It stays"
	if n > 1 {
		count, is, it, stays = strconv.FormatInt(n, 10)+" deposits", "are", "them", "They stay"
	}
	note := count + " (" + amount(sum, 8) + " BTC, " + a.netTag() + " " + a.escrowNetwork() + ") to this order's escrow, now confirmed, " + is + " not spent by any refund of this order: the escrow node reported " + it + " as conflicted, or no longer listed " + it + ", when the order was cancelled or while its refund was being built, or that refund ended without spending " + it + ". " +
		stays + " in the escrow; a stray refund returns " + it + ", less the network fee, to the buyer's refund address given at checkout. Do not send more coins to this escrow."
	if upgraded {
		note = a.tooFewNote(n, sum)
	}
	if err := a.flagOrder(ctx, tx, o, note, ""); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), o.BuyerID, "Order "+shortID(o.ID)+": "+note)
	return err
}

// leaveOutTooFew marks tried, the deposits a cancelled Bitcoin escrow order's refund could not spend because they were
// too few to pay the network fee and the minimum output, as left out and flagged (A-435), in tx with that refund's
// failure (failSettlement): one order note, a notice to the buyer and to staff, only for the deposits newly marked. A
// stray refund returns them with any deposit left out later (leaveOutUncounted); until the coins grow, the fee rate
// falls or the buyer asks, the refund's failure holds them (strayHeldSQL), so the same coins are not tried again.
func (a *App) leaveOutTooFew(ctx context.Context, tx *sql.Tx, o *Order, tried []settleInput) error {
	if o.State != stateCancelled || len(tried) == 0 {
		return nil
	}
	ids := make([]int64, len(tried))
	for i, in := range tried {
		ids[i] = in.ID
	}
	var n, sum int64
	if err := tx.QueryRowContext(ctx, `WITH m AS (UPDATE payments SET escrow_left_out=true,flagged=true,updated=now() WHERE order_id=$1 AND id=ANY($2) AND settlement_id IS NULL
			AND NOT after_close AND NOT escrow_left_out AND NOT spent_outside RETURNING amount)
		SELECT count(*),COALESCE(sum(amount),0) FROM m`, o.ID, ids).Scan(&n, &sum); err != nil || n == 0 {
		return err
	}
	note := a.tooFewNote(n, sum)
	if err := a.flagOrder(ctx, tx, o, note, ""); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), o.BuyerID, "Order "+shortID(o.ID)+": "+note)
	return err
}

// tooFewNote is the order note on n deposits worth sum sats that a cancelled order's refund could not spend because
// they were too few (leaveOutTooFew, and leaveOutUncounted on a database upgraded from before A-435).
func (a *App) tooFewNote(n, sum int64) string {
	count, is, it, stays := "1 deposit", "is", "it", "It stays"
	if n > 1 {
		count, is, it, stays = strconv.FormatInt(n, 10)+" deposits", "are", "them", "They stay"
	}
	return count + " (" + amount(sum, 8) + " BTC, " + a.netTag() + " " + a.escrowNetwork() + ") to this order's escrow " + is + " not spent by its refund: the market could not build the refund because the coins were too few to pay the network fee and an output of at least 0.0001 BTC. " +
		stays + " in the escrow; a stray refund returns " + it + ", together with any later deposit to this escrow, less the network fee, to the buyer's refund address given at checkout once together they can pay the network fee."
}

// physicalReleaseNotSent is the honest line on a release that completes only with both parties' signatures (a
// physical order's release before its outcome), naming the escrow node's chain.
func physicalReleaseNotSent(net string) string {
	return "Nothing has been sent yet: the market never signs a release before the order is completed. Once both the vendor and the buyer have signed it, the market broadcasts the transaction they signed to the " + net + " network and the order is completed."
}

// releaseNextStep is the "who acts next" line of a shipped order's release in state, with the parties who signed it.
func releaseNextStep(state string, signed map[string]bool) string {
	switch {
	case state == "building":
		return "Who acts next: the market builds the release to the vendor on its next pass; then the vendor and the buyer each sign it."
	case state != "awaiting_signature" && state != "party_signed":
		return ""
	case signed[roleBuyer] && signed[roleVendor]:
		return "The buyer and the vendor have both signed the release: the market broadcasts it on its next pass, and the order is completed."
	case signed[roleVendor]:
		return "Who acts next: the buyer signs the release once the goods have arrived; the vendor waits. If the goods did not arrive, the buyer opens a dispute instead of signing."
	case signed[roleBuyer]:
		return "Who acts next: the vendor signs the release; the buyer has signed it and waits."
	}
	return "Who acts next: the vendor signs the release now; the buyer signs it once the goods have arrived. The market does not sign it."
}
