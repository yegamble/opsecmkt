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
// the buyer signs the release). Behind ESCROW_BTC (off by default); test networks only.
//
//  1. Mark shipped (/escrow/ship): the vendor gives a fresh claim address (the claim action's checks) and their
//     password; in one transaction the order moves paid -> shipped, the release is requested to that address (kind
//     release, state building) and escrows.release_at is the escrow's snapshot of release_days from now.
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
	registerAction("/escrow/ship", actionSpec{Roles: escrowPartyRoles, OwnTx: true, OneAtATime: true, Off: escrowOff,
		ConfirmedFormNonce: true, ReceiptAuthorize: btcShipReceiptAuthorize, Run: escrowShipAction})
}

var (
	errShipOnlyVendor = fail(403, "Only the vendor marks this order shipped.")
	errShipPlainForm  = fail(409, "This order is paid into a Bitcoin escrow: mark it shipped with the escrow form on the order page, which takes the claim address the release to you pays, and your password. Nothing was changed.")
	errShipNoDeadline = fail(409, "This escrow has no release deadline length recorded, so it cannot be marked shipped here. Market staff must check it. Nothing was changed.")
	errEscrowComplete = fail(409, "This order is paid into a Bitcoin escrow: you confirm receipt by signing the release to the vendor on the order page, not with this form. Nothing was changed.")
	errSignDisputed   = fail(409, "This order is disputed: an administrator decides it, and the decision settles the escrow, so the buyer does not sign the release now. Nothing was changed.")
	errDisputeSigned  = fail(409, "You signed the release to the vendor, which confirms receipt of this order, so it can no longer be disputed. Nothing was changed.")
	errReleaseFinal   = fail(409, "The release to the vendor built when this order was shipped is already final (signed by both parties, or broadcast or spent on the network), so the escrow pays the vendor and the dispute cannot be decided for the buyer. Decide it for the vendor. Nothing was changed.")
	escrowShippedNote = "Release requested at shipment (Bitcoin escrow): the market builds the release to the vendor, paying the claim address the vendor gave now (less the market fee and the network fee; " + escrowExcessRule("BTC") + "). The vendor signs it; the buyer signs it once the goods have arrived, which confirms receipt and ends the buyer's right to open a dispute. With both signatures the market broadcasts the transaction they signed; the market never signs it before the order is completed. Nothing is sent yet."
)

// releaseBeforeOutcome reports whether a settlement of kind on an order in state is a physical order's release before
// the order's outcome (shipped, or disputed and not yet decided): only both parties' signatures complete it.
func releaseBeforeOutcome(kind, state string) bool {
	return kind == "release" && (state == stateShipped || state == stateDisputed)
}

// checkClaimAddress refuses infrastructure addresses, this order's buyer refund
// and the claimant account's previous destinations, and checks the node's chain.
func checkClaimAddress(c *actionCtx, e *escrowBTC, orderID, dest string) error {
	ctx := c.Ctx()
	_, feeAddr, err := escrowFee(ctx, c.Tx, e)
	if err != nil {
		return err
	}
	used, err := bitcoinAddressUsed(ctx, c.Tx, dest, feeAddr, c.User.ID, orderID)
	if err != nil {
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
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		e := c.A.escrow.Load()
		if e == nil {
			return actionResult{}, errEscrowDown
		}
		note, err := formNote(c, "Marked shipped")
		if err != nil {
			return actionResult{}, err
		}
		dest, ok := escrowAddressForm(e.network, c.Form.Get("address"))
		if !ok {
			return actionResult{}, errClaimAddress(c.A.escrowNetwork())
		}
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
		if err = checkClaimAddress(c, e, o.ID, dest); err != nil {
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

// countAgain counts for o again, in tx, the deposits refund's too-small failure left out (leaveOutTooFew, and
// leaveOutUncounted on an upgraded database) when a changed decision supersedes refund (A-462): no longer left out or
// flagged, and with no recorded review (QA462E-1: should they be left out again, that is a new flag staff record anew;
// the order history and the audit log keep the earlier review), so no stray refund returns them and the release
// includes them. A deposit left out for any other reason (by a
// bounded build, as a top-up, or because it did not count when the dispute was decided), one a stray refund already
// spends and one spent outside the market stay as they are; a stray refund being built from one meanwhile does not
// claim it (storeSettlement, QA462-1). It returns the order note on those it counted ("" if none).
func (a *App) countAgain(ctx context.Context, tx *sql.Tx, o *Order, refund int64) (string, error) {
	var n, sum int64
	if err := tx.QueryRowContext(ctx, `WITH m AS (UPDATE payments SET escrow_left_out=false,flagged=false,disposition='',disposition_note='',disposed_at=NULL,escrow_too_few_by=NULL,updated=now()
			WHERE order_id=$1 AND escrow_too_few_by=$2 AND settlement_id IS NULL AND NOT spent_outside RETURNING amount)
		SELECT count(*),COALESCE(sum(amount),0) FROM m`, o.ID, refund).Scan(&n, &sum); err != nil || n == 0 {
		return "", err
	}
	count, is, it := "1 deposit", "is", "it"
	if n > 1 {
		count, is, it = strconv.FormatInt(n, 10)+" deposits", "are", "them"
	}
	return count + " (" + amount(sum, 8) + " BTC, " + a.netTag() + " " + a.escrowNetwork() + ") to this order's escrow, left out for a stray refund when the refund of the earlier decision could not be built because the coins were too few, " +
		is + " counted for the order again: the release to the vendor includes " + it + ", and no stray refund returns " + it + ".", nil
}

// coinsToBuyer reports whether o's escrowed coins go back to the buyer by its refund: o is cancelled, or resolved with
// its dispute's live decision for the buyer (A-437). Then a refund that fails too-small leaves its deposits out for the
// stray refund, and so does a deposit that did not count and confirms later (leaveOutTooFew, leaveOutUncounted,
// xmrLeaveOutUncounted). On an order completed or resolved for the vendor the coins are the vendor's, paid by the
// release.
func coinsToBuyer(ctx context.Context, q rowQuerier, o *Order) (bool, error) {
	switch o.State {
	case stateCancelled:
		return true, nil
	case stateResolved:
		outcome, err := escrowDisputeOutcome(ctx, q, o.ID)
		return outcome == "refund", err
	}
	return false, nil
}

// physicalReleaseNotSent is the honest line on a release that completes only with both parties' signatures (a
// physical order's release before its outcome), naming the escrow node's chain.
func physicalReleaseNotSent(net string) string {
	return "Nothing has been sent yet: the market never signs a release before the order is completed. Once both the vendor and the buyer have signed it, the market broadcasts the transaction they signed to the " + net + " network and the order is completed."
}
