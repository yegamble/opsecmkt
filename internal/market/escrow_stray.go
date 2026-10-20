package market

// The stray refund (WR-escrow Phase 1, slice P1-c2; ES-8, A-322, A-327). Bitcoin escrows stay imported in the
// watch-only wallet with no age limit. Coins first recorded at an escrow after its order closed (payments.after_close)
// and outputs a bounded build left out (payments.escrow_left_out, A-342) are not the order's: once confirmed, the
// escrow watcher requests a stray_refund settlement returning them to the buyer's pinned refund address, which either
// party signs and the market co-signs (the same pipeline as a claim or refund). One is in progress per order at a
// time (migration 076); a later deposit gets the next one.
//
// The watcher reads closed escrows only within its horizon (escrowWatchedSQL), so a deposit to an older escrow, or to
// one cancelled while escrow was unavailable, is found by:
//   - the buyer's "Recover coins at this escrow" (/escrow/recover, password step-up): the next pass reads that escrow
//     and requests the stray refund of what it finds, or tells the buyer it found nothing;
//   - the daily check: once every escrowStrayCheck the pass reads every escrow, whatever its age. The first check runs
//     escrowStrayCheck after escrow first runs on a database (settings key escrow_btc_stray_check).

func init() {
	registerAction("/escrow/recover", actionSpec{Roles: escrowPartyRoles, OwnTx: true, OneAtATime: true, Off: escrowOff,
		ConfirmedFormNonce: true, ReceiptAuthorize: btcRecoverReceiptAuthorize, Run: escrowRecoverAction})
}

var (
	errRecoverOnlyBuyer = fail(403, "Only the buyer recovers coins sent to this escrow: they go back to the buyer's refund address.")
	errRecoverState     = fail(409, "Coins at this escrow are recovered once its order is closed (completed, cancelled or resolved). Nothing was changed.")
	errRecoverOpen      = fail(409, "A recovery of coins at this escrow is already in progress; it is shown on the order page. Nothing was changed.")
	errRecoverAsked     = fail(409, "You already asked; the market checks this escrow on its next pass. Reload the page in a minute. Nothing was changed.")
)

// escrowRecoverAction is the buyer's "Recover coins at this escrow" (see the file comment): confirmed with the buyer's
// password (and authenticator code when enrolled), on a closed escrow order with no stray refund in progress.
func escrowRecoverAction(c *actionCtx) (actionResult, error) {
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		if c.A.escrow.Load() == nil {
			return actionResult{}, errEscrowDown
		}
		ctx := c.Ctx()
		o, err := partyOrderLocked(c, c.Form.Get("order_id"))
		switch {
		case err != nil:
			return actionResult{}, err
		case !o.btcEscrow():
			return actionResult{}, errNoOrder
		case o.BuyerID != c.User.ID:
			return actionResult{}, errRecoverOnlyBuyer
		case !isTerminal(o.State):
			return actionResult{}, errRecoverState
		}
		var open bool
		if err = c.Tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM settlements WHERE order_id=$1 AND kind='stray_refund'
			AND state IN ('building','awaiting_signature','party_signed','signed','broadcast'))`, o.ID).Scan(&open); err != nil {
			return actionResult{}, err
		}
		if open {
			return actionResult{}, errRecoverOpen
		}
		res, err := c.Tx.ExecContext(ctx, "UPDATE escrows SET recover_requested=now() WHERE order_id=$1 AND coin='BTC' AND recover_requested IS NULL", o.ID)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return actionResult{}, errRecoverAsked
		}
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", o.ID, o.State, c.User.ID,
			"The buyer asked the market to check this escrow for coins that do not count for the order. On its next pass the market reads it and requests a stray refund of what it finds to the buyer's refund address; coins too few to pay the network fee stay held."); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: orderPage(o.ID) + "#escrow-recover", Notice: "escrow-recover", Audit: "Asked to recover coins at the escrow of order " + shortID(o.ID) + " (" + c.A.escrowNetwork() + ")"}, nil
	})
}
