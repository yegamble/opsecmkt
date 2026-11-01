package market

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

// The Monero escrow on the order page (WR-escrow Phase 3, slice X-c; P3-R11): the escrow address, the price in XMR,
// the confirmations a deposit needs, each deposit, the market fee, for the buyer the pinned refund address, the
// custody sentence (owner decision O-2, A-402) and the escrow's state, including its unavailable and restore-paused
// states. On a draft that can use it the buyer is offered Monero escrow checkout (escrow_xmr_create.go). The order's
// actions are narrowed to those a Monero escrow order can take, as escrowOrder does for a Bitcoin one. Nothing here
// makes an RPC.

// xmrEscrowOrder fills the Monero escrow panel of a Monero escrow order for loadOrderDetail. A Monero escrow order
// always gets it, with ESCROW_XMR off too (it then says Monero escrow is unavailable), never the pooled payment panel.
func (a *App) xmrEscrowOrder(ctx context.Context, d *PageData) error {
	o := d.Order
	v := &XMREscrowOrderView{Available: a.xmrEscrowReady(), Custody: xmrCustody, Required: o.Amount, Threshold: xmrMinConfirmations, Open: o.State == stateAwaitingPayment}
	if a.escrowXMR != nil {
		v.Threshold = a.escrowXMR.confirmations
	}
	var refund, strayState, strayWait string
	var feeBps, openStrays int64
	var shipByOpen, tooFew, refundAsked, recheck, outage bool
	var issued, windowFrom time.Time
	var firstSeen sql.NullTime
	var shipBy, destroyed, cancelWait sql.NullTime
	// tooFew: the refund ended because its coins were too few, and they were left out for a stray refund (A-437).
	if err := a.db.QueryRowContext(ctx, `SELECT es.address,es.refund_address,es.fee_bps,
		es.created,GREATEST(es.created,(SELECT max(created) FROM payments WHERE order_id=es.order_id AND confirmations>=0 AND NOT locked AND NOT after_close AND NOT escrow_left_out AND NOT spent_outside)),
		(SELECT min(created) FROM payments WHERE order_id=es.order_id AND NOT locked AND NOT after_close),
		es.ship_by,COALESCE(es.ship_by<=now(),false),w.destroyed_at,COALESCE(x.state,''),COALESCE(x.send_error,''),
		EXISTS(SELECT 1 FROM xmr_settlements r WHERE r.order_id=es.order_id AND r.kind='refund' AND r.state='failed' AND r.send_error=$2),
		EXISTS(SELECT 1 FROM xmr_settlements r WHERE r.order_id=es.order_id AND r.kind='refund'),
		(SELECT count(*) FROM xmr_settlements r WHERE r.order_id=es.order_id AND r.kind='stray_refund' AND r.state IN ('requested','built','relayed')),
		EXISTS(SELECT 1 FROM settings WHERE key=$3),`+xmrCancelWaitSQL+`,`+xmrOutageCauseSQL+`
		FROM escrows es JOIN xmr_escrow_slots s ON s.id=es.xmr_slot_id JOIN xmr_escrow_wallets w ON w.id=s.wallet_id
		LEFT JOIN LATERAL (SELECT x.state,x.send_error FROM xmr_settlements x WHERE x.order_id=es.order_id AND x.kind='stray_refund' ORDER BY x.id DESC LIMIT 1) x ON true
		WHERE es.order_id=$1 AND es.coin='XMR'`, o.ID, xmrTooSmall, xmrReleaseRecheckKey).Scan(&v.Address, &refund, &feeBps, &issued, &windowFrom, &firstSeen, &shipBy, &shipByOpen, &destroyed, &strayState, &strayWait, &tooFew, &refundAsked, &openStrays, &recheck, &cancelWait, &outage); err != nil {
		return err
	}
	if destroyed.Valid {
		v.Unrecoverable = xmrAfterDestroyText(destroyed.Time)
	}
	if v.Net = xmrAddressNet(v.Address, a.live); v.Net == "" {
		v.Net = a.xmrEscrowNetwork()
	}
	if d.OrderViewer == roleBuyer {
		v.RefundAddress = refund
	}
	if feeBps > 0 {
		v.Fee = strconv.FormatFloat(float64(feeBps)/100, 'f', -1, 64) + "% of the price, fixed when the escrow was created; taken from the release to the vendor"
	}
	required, err := parseAmount(o.Amount, 12)
	if err != nil {
		return err
	}
	// unspent counts the deposits that do not count for the order and that no settlement spends yet (A-435).
	var received, pending, afterCloseSeen, leftOutSeen, unspent int64
	remaining := required
	rows, err := a.db.QueryContext(ctx, "SELECT txid,amount,confirmations,locked,after_close,escrow_left_out,spent_outside,xmr_settlement_id IS NOT NULL FROM payments WHERE order_id=$1 ORDER BY created,id", o.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var dep PaymentDeposit
		var amt int64
		var locked, afterClose, leftOut, outside, settled bool
		if err = rows.Scan(&dep.TxID, &amt, &dep.Confirmations, &locked, &afterClose, &leftOut, &outside, &settled); err != nil {
			rows.Close()
			return err
		}
		dep.Amount = amount(amt, 12)
		switch {
		case locked:
			dep.State = "Locked (it has an unlock time) — not counted; market staff were notified"
		case dep.Confirmations < 0:
			dep.State = "Conflicted or missing — not counted"
		case afterClose:
			dep.State = "Arrived after the order closed — not counted; returned to the buyer's refund address"
			afterCloseSeen++
		case leftOut:
			// A-435: left out after a build (xmrLeaveOutTopUps), or not counted when the order was cancelled and mined
			// since (A-424): a stray refund returns it, never the order's settlement.
			dep.State = "Left out — not counted for the order; returned to the buyer's refund address by a stray refund once confirmed"
			if !isTerminal(o.State) {
				dep.State = "Left out — not counted for the order; returned to the buyer's refund address by a stray refund once the order is closed and it is confirmed"
			}
			leftOutSeen++
		case dep.Confirmations >= xmrConfirmationsCeiling(v.Threshold):
			// Stored confirmations stop at the ceiling (xmrConfirmationsCeiling).
			dep.State, received = "Confirmed ("+strconv.FormatInt(dep.Confirmations, 10)+" confirmations or more)", received+amt
		case dep.Confirmations >= v.Threshold:
			dep.State, received = "Confirmed", received+amt
		default:
			dep.State, pending = "Waiting for confirmations", pending+amt
		}
		if dep.Confirmations >= 0 && !locked && !afterClose {
			remaining -= min(remaining, amt)
		}
		if (afterClose || leftOut) && !settled && !outside && !locked && dep.Confirmations >= 0 {
			unspent++
		}
		v.Deposits = append(v.Deposits, dep)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	v.Confirmed, v.Unconfirmed, v.Remaining, v.NeedsTopUp = amount(received, 12), amount(pending, 12), amount(remaining, 12), remaining > 0
	for _, s := range []*string{&v.Confirmed, &v.Unconfirmed, &v.Remaining} {
		if *s == "" {
			*s = "0"
		}
	}
	if v.Open {
		deadline := windowFrom.Add(expiryWindow())
		if pending > 0 && firstSeen.Valid {
			deadline = confirmationDeadline(issued, firstSeen.Time)
			v.ConfirmationWait = true
		}
		v.CancelAfter = deadline.UTC().Truncate(time.Minute).Format("2006-01-02 15:04 UTC")
		// The watcher waits for a successful read after this time, even if the
		// daemon is syncing or the wallet is unavailable (A-449).
		v.WindowPassed = time.Now().After(deadline)
	}
	switch {
	case o.State == stateAwaitingPayment && len(v.Deposits) == 0:
		v.Status = "Waiting for a deposit"
	case o.State == stateAwaitingPayment && pending > 0:
		v.Status = "Deposit seen. Waiting for " + strconv.FormatInt(v.Threshold, 10) + " confirmations."
	case o.State == stateAwaitingPayment:
		v.Status = "Confirmed deposits are below the price"
	case o.State == statePaid && o.Kind == "physical":
		v.Status, v.NextStep = "Funded", "The vendor ships the goods; the order stays open until it is shipped and settled. If the vendor cancels it instead, the escrow is refunded to the buyer."
		if shipBy.Valid {
			v.NextStep += " Ship by " + utcFrom(shipBy.Time) + " (the listing's dispatch window plus 48 hours, from payment): if the order is not marked shipped by then, the buyer may cancel it and the escrow is refunded to the buyer."
		}
	case o.State == statePaid:
		// A-709: the delivery deadline (ship_by, escrowTransitionHook) and the dispute window that follows delivery.
		v.Status, v.NextStep = "Funded", "The vendor delivers the digital content; the buyer then has "+strconv.Itoa(disputeWindowHours)+" hours to check it and open a dispute, or to confirm receipt, before the order completes and the vendor can claim the coins. If the vendor cancels it instead, the escrow is refunded to the buyer."
		if shipBy.Valid {
			v.NextStep += " Deliver by " + utcFrom(shipBy.Time) + " (" + strconv.Itoa(digitalDeliveryHours) + " hours from payment): if the content is not delivered by then, the buyer may cancel the order and the escrow is refunded to the buyer."
		}
	case o.State == stateShipped:
		v.Status = "Funded and shipped"
	case o.State == stateDelivered:
		v.Status = "Funded and delivered"
	case o.State == stateCompleted:
		v.Status, v.NextStep = "Funded and completed", "The escrowed coins are released to the vendor by a transaction the market builds and sends."
	case o.State == stateDisputed:
		v.Status, v.NextStep = "Disputed", "An administrator of this market decides the dispute. The decision is paid from this escrow: a refund to the buyer's refund address, or a release to the vendor."
	case o.State == stateCancelled && received+pending > 0 && refundAsked: // BE103V-2: with none, one that returned goes by a stray refund
		v.Status = "Cancelled; the coins in this escrow go back to the buyer's refund address by a refund the market builds and sends."
	case o.State == stateCancelled && tooFew && leftOutSeen > 0:
		// A-437: the refund's coins were too few, so it ended and they were left out for a stray refund (xmrRefundTooFew).
		v.Status = "Cancelled; the market could not send the refund because the coins were too few. The escrowed amount "
		if tail := xmrStrayStatus(strayState, strayWait, unspent, openStrays); tail != "" {
			v.Status += tail
		} else {
			v.Status += "stays in the escrow account until a stray refund returns it, less the network fee, to the buyer's refund address."
		}
	case o.State == stateCancelled && afterCloseSeen > 0:
		v.Status = "Cancelled; a deposit arrived after the order closed. It is not counted and is returned to the buyer's refund address."
	case o.State == stateCancelled && leftOutSeen > 0:
		v.Status = "Cancelled; a deposit to this escrow did not count for the order. It "
		if tail := xmrStrayStatus(strayState, strayWait, unspent, openStrays); tail != "" {
			v.Status += tail
		} else {
			v.Status += "stays in the escrow account until a stray refund returns it, less the network fee, to the buyer's refund address."
		}
	case o.State == stateCancelled && !v.Available:
		// Nothing is read while Monero escrow is unavailable, so an empty ledger proves nothing.
		v.Status = "Cancelled. This escrow cannot be checked right now because Monero escrow is unavailable, so a deposit to it may not be shown here."
		v.Unread = "Not read while Monero escrow is unavailable"
	case o.State == stateCancelled && len(v.Deposits) > 0: // BE103-1: seen, but none counted, so not "nothing was received"
		v.Status = "Cancelled; no deposit to this escrow counted when the order was cancelled, so nothing was refunded. A deposit that returns goes back, less the network fee, to the buyer's refund address by a stray refund once it has confirmed."
	case o.State == stateCancelled:
		v.Status = "Cancelled; nothing was received in this escrow"
	default:
		v.Status = stateLabel(o.State)
	}
	if v.RestorePaused, err = restoreGateSet(ctx, a.db); err != nil {
		return err
	}
	if v.Settlements, err = a.xmrSettlementViews(ctx, d, v); err != nil {
		return err
	}
	d.XMREscrow = v
	if err = a.xmrReleaseView(ctx, d, v); err != nil {
		return err
	}
	if err = a.holdRefundView(ctx, d); err != nil { // A-652f
		return err
	}
	// Only the actions a Monero escrow order can take (escrowOrder's rules): no pooled "Mark shipped" (the panel has the
	// Monero ship form, with the claim address and a password step-up); the buyer of a paid order cancels from its
	// ship-by time (a digital one's delivery deadline, A-709) unless a restore's recheck is pending (A-714); no dispute
	// once the release deadline or dispute window has passed (PM-4, lateDispute); a cancel that refunds coins states and
	// carries their amount.
	counted := received + pending
	kept := d.Transitions[:0]
	for _, t := range d.Transitions {
		switch {
		case t.To == stateShipped:
			continue
		case t.To == stateDisputed && v.Deadline != nil && v.Deadline.closed:
			continue
		case t.To == stateCancelled && o.State == statePaid && d.OrderViewer == roleBuyer && !shipBy.Valid:
			continue
		case t.To == stateCancelled && o.State == statePaid && d.OrderViewer == roleBuyer && recheck && (shipByOpen || cancelWait.Valid) && outage: // A-730: noShipCancel's refusal
			t.Action, t.Label = "", "You cannot cancel yet: "+outageWhy+", and the vendor may not have been able to ship or deliver this order in that time. The cancel is offered 72 hours after the market has rechecked its escrow deadlines, if the order has not been shipped or delivered by then; reload this page later."
		case t.To == stateCancelled && o.State == statePaid && d.OrderViewer == roleBuyer && recheck && (shipByOpen || cancelWait.Valid): // A-714: noShipCancel's refusal
			t.Action, t.Label = "", "You cannot cancel yet: this market's records are being reconciled after a restore from backup, and the vendor may have shipped or delivered this order after that backup was taken. The cancel is offered 72 hours after the market has rechecked its escrow deadlines, if the order has not been shipped or delivered by then; reload this page later."
		case t.To == stateCancelled && o.State == statePaid && d.OrderViewer == roleBuyer && cancelWait.Valid: // A-720: noShipCancel's refusal
			done := "shipped"
			if o.Kind == "digital" {
				done = "delivered"
			}
			if outage { // A-730
				t.Action, t.Label = "", "You can cancel from "+utcFrom(cancelWait.Time)+" if the vendor has not "+done+" this order by then: "+outageWhy+", and the vendor may not have been able to in that time. The escrow is then refunded to your refund address."
				break
			}
			t.Action, t.Label = "", "You can cancel from "+utcFrom(cancelWait.Time)+" if the vendor has not "+done+" this order by then: this market restored its records from a backup, and the vendor may have "+done+" it after that backup was taken. The escrow is then refunded to your refund address."
		case t.To == stateCancelled && o.State == statePaid && d.OrderViewer == roleBuyer && !shipByOpen && o.Kind == "digital":
			t.Action, t.Label = "", "You can cancel from "+utcFrom(shipBy.Time)+" if the vendor has not delivered the content by then; the escrow is then refunded to your refund address."
		case t.To == stateCancelled && o.State == statePaid && d.OrderViewer == roleBuyer && !shipByOpen:
			t.Action, t.Label = "", "You can cancel from "+utcFrom(shipBy.Time)+" if the vendor has not shipped the order by then; the escrow is then refunded to your refund address."
		case t.To == stateCancelled && counted > 0:
			t.Payout = &PayoutPreview{Seen: counted, Confirm: "Cancelling refunds the " + a.escrowAmount(o, counted) + " in this escrow, less the network fee, to the buyer's refund address given at checkout. The market builds and sends the refund transaction. This is final."}
		case t.To == stateCancelled && o.State == statePaid: // BE103-1: a cancel of a paid order always needs the confirmation (cancelAction)
			t.Payout = &PayoutPreview{Confirm: "No counted coins are in this escrow, so cancelling refunds nothing unless the deposit returns: the market sends nothing now, and a deposit that returns after the cancel goes back, less the network fee, to the buyer's refund address by a stray refund once it has confirmed. This is final."}
		}
		kept = append(kept, t)
	}
	d.Transitions = kept
	return nil
}

// xmrStrayStatus ends a cancelled Monero order's status about a deposit that did not count (A-435), after "It", by the
// state of the order's latest stray refund, as strayStatus does for a Bitcoin one: requested or built (the market sends
// it), relayed (sent), confirmed. It is "" when there is none of these, or when a deposit that does not count is not
// spent by it yet (unspent; a requested stray refund claims its deposits only once built), so the status says what it
// said before: the deposit stays until a stray refund returns it. A requested one that waits because the coins are too
// few (wait, xmrTooSmall) is one the market cannot send, as its panel says (QA435B-6). While more than one is in
// progress (open: a re-request of a failed stray refund beside the first-generation stray refund of a later deposit,
// A-616), the latest one's state is not the whole story, so it says that each returns its own deposits and is sent on
// its own, as its panel below says.
func xmrStrayStatus(state, wait string, unspent, open int64) string {
	switch {
	case open > 1:
		return "is returned, less the network fee, to the buyer's refund address by the " + strconv.FormatInt(open, 10) + " stray refunds below, which are in progress at once: each returns its own deposits and is sent on its own, so one may be sent while another still waits, as each says under it."
	case state == "requested" && wait == xmrTooSmall:
		return "stays in the escrow account, because the coins there are too few to pay the network fee and an output of at least " + amount(minPayoutXMR, 12) + " XMR, so the market cannot send the stray refund below."
	case state == "requested":
	case unspent > 0:
		return ""
	case state == "built":
	case state == "relayed":
		return "is being returned, less the network fee, to the buyer's refund address by the stray refund below, which is sent and waits for confirmations."
	case state == "confirmed":
		return "is back at the buyer's refund address: the stray refund below that returned it is confirmed."
	default:
		return ""
	}
	return "stays in the escrow account until the market sends the stray refund below, which returns it, less the network fee, to the buyer's refund address."
}

// xmrEscrowCheckoutOffer offers Monero escrow checkout to the buyer of an XMR draft on a listing that takes escrow, or
// says why it cannot be used now. Any other order, and every order while ESCROW_XMR is off, is left as it was.
func (a *App) xmrEscrowCheckoutOffer(ctx context.Context, d *PageData) error {
	o := d.Order
	if a.escrowXMR == nil || o.State != stateDraft || d.OrderViewer != roleBuyer || o.Currency != "XMR" {
		return nil
	}
	net := a.xmrEscrowNetwork()
	v := &XMREscrowCheckoutView{Physical: o.Kind == "physical", Custody: xmrCustody, Net: net, Prefixes: xmrPrefixes(net), NetKnown: a.xmrEscrowNetKnown() != ""}
	eligible, price, _, err := escrowEligible(ctx, a.db, o)
	if refusal, ok := err.(*httpError); ok {
		v.Reason = refusal.Msg
		d.XMREscrowCheckout = v
		return nil
	}
	if err != nil {
		return err
	}
	if !eligible { // A-711: the buyer is told why, not shown nothing
		v.Reason, err = checkoutIneligibleReason(ctx, a.db, o)
		d.XMREscrowCheckout = v
		return err
	}
	v.Total = amount(price, 12)
	if v.Reason, err = quantityCheckoutReason(ctx, a.db, o); err != nil || v.Reason != "" {
		d.XMREscrowCheckout = v
		return err
	}
	if o.Kind == "digital" {
		var mode string
		if err = a.db.QueryRowContext(ctx, "SELECT digital_delivery_mode FROM products WHERE id=$1", o.ProductID).Scan(&mode); err != nil {
			return err
		}
		v.ManualDigital = mode == "manual"
	}
	if price < payoutFloor(o.Currency, "release") {
		v.Reason, err = subFloorListingForProduct(ctx, a.db, o.ProductID, o.Currency)
		if err != nil {
			return err
		}
		d.XMREscrowCheckout = v
		return nil
	}
	vendorCanEncrypt := true
	if v.Physical {
		if vendorCanEncrypt, err = userCanReceiveEncrypted(ctx, a.db, o.VendorID); err != nil {
			return err
		}
	}
	var gate, sweep, free bool
	if err = a.db.QueryRowContext(ctx, `SELECT `+restoreGateSQL+`,EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value='true'),
		EXISTS(SELECT 1 FROM xmr_escrow_slots s JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE s.state='free' AND w.state='active' AND w.id=$2)`,
		xmrEscrowSweepKey, a.escrowXMR.reconciled.Load()).Scan(&gate, &sweep, &free); err != nil {
		return err
	}
	switch {
	case d.Settings[paymentIntakeKey(o.Currency)] != intakeEscrow: // as lockPaymentIntake refuses the checkout (A-722)
		v.Reason = "New Monero escrow checkouts are paused by the administrator, so no escrow address can be issued for this draft yet. Keep the draft and come back later, or cancel it."
	case !a.xmrEscrowReady():
		v.Reason = "Monero escrow is unavailable on this market right now. Try again later."
	case gate:
		v.Reason = "Monero escrow is paused while this market restores its records from a backup, so no escrow can be created until the operator has finished. Try again later."
	case sweep:
		v.Reason = "Monero escrow is paused until this market has checked its escrow wallets after restoring its records from a backup, so no escrow can be created yet. Try again later."
	case o.BuyerID == o.VendorID:
		v.Reason = "You cannot buy your own listing."
	case v.Physical && !vendorCanEncrypt:
		v.Reason = noVendorKeyReason // A-83: the buyer could not send the vendor an encrypted shipping address
	case !free:
		v.Reason = "No Monero escrow account is free right now; the market prepares more within a minute. Reload this page shortly."
	}
	d.XMREscrowCheckout = v
	return nil
}

// xmrReleaseView fills the release part of the Monero escrow panel (slice X-e; escrow_xmr_release.go,
// escrow_xmr_deadline.go): the forms the viewer may use (Mark shipped, Confirm receipt, the claim), a shipped order's
// release deadline with the time left when the page loaded, and who acts next once the order is past its ship-by time,
// shipped, disputed, completed with no release yet, or decided.
func (a *App) xmrReleaseView(ctx context.Context, d *PageData, v *XMREscrowOrderView) error {
	o := d.Order
	var releaseAt, cancelWait sql.NullTime
	var left sql.NullFloat64
	var shipByOpen, rechecked, live, tooFew, claimDue, recheckAsked, outage bool
	if err := a.db.QueryRowContext(ctx, `SELECT COALESCE(es.ship_by<=now(),false),es.release_at,extract(epoch FROM es.release_at-now()),es.release_recheck,
		`+xmrLiveOutcomeSQL("es.order_id")+`,
		EXISTS(SELECT 1 FROM xmr_settlements s WHERE s.order_id=es.order_id AND s.kind='refund' AND s.state='failed' AND s.send_error=$2),`+xmrClaimDueSQL("o")+`,
		EXISTS(SELECT 1 FROM settings WHERE key=$3),`+xmrCancelWaitSQL+`,`+xmrOutageCauseSQL+`
		FROM escrows es JOIN orders o ON o.id=es.order_id WHERE es.order_id=$1 AND es.coin='XMR'`, o.ID, xmrTooSmall, xmrReleaseRecheckKey).Scan(&shipByOpen, &releaseAt, &left, &rechecked, &live, &tooFew, &claimDue, &recheckAsked, &cancelWait, &outage); err != nil {
		return err
	}
	outcome, err := escrowDisputeOutcome(ctx, a.db, o.ID)
	if err != nil {
		return err
	}
	switch {
	case o.State == statePaid && recheckAsked && (shipByOpen || cancelWait.Valid) && outage: // A-730
		v.NextStep += " The buyer's cancel waits: " + outageWhy + ", and the vendor may not have been able to ship or deliver this order in that time. It is offered 72 hours after the market has rechecked its escrow deadlines, if the order has not been shipped or delivered by then; reload this page later."
	case o.State == statePaid && recheckAsked && (shipByOpen || cancelWait.Valid): // A-714: the buyer's cancel waits (noShipCancel)
		v.NextStep += " The buyer's cancel waits while this market's records are being reconciled after a restore from backup: the vendor may have shipped or delivered this order after that backup was taken. It is offered 72 hours after the market has rechecked its escrow deadlines, if the order has not been shipped or delivered by then; reload this page later."
	case o.State == statePaid && cancelWait.Valid && outage: // A-730
		v.NextStep += " The buyer's cancel waits until " + utcFrom(cancelWait.Time) + ": " + outageWhy + ", and the vendor may not have been able to ship or deliver this order in that time, so has until then to do so. If that is not done by then, the buyer may cancel the order and the escrow is refunded to the buyer's refund address."
	case o.State == statePaid && cancelWait.Valid: // A-720: so does it in the vendor's grace after the recheck
		v.NextStep += " The buyer's cancel waits until " + utcFrom(cancelWait.Time) + ", after this market's restore from backup: the vendor may have shipped or delivered this order after that backup was taken, and has until then to do so again. If that is not done by then, the buyer may cancel the order and the escrow is refunded to the buyer's refund address."
	case o.State == statePaid && o.Kind == "physical" && shipByOpen:
		v.NextStep += " The ship-by time has passed and the order is not marked shipped: the buyer may now cancel it, and the escrow is then refunded to the buyer's refund address."
	case o.State == statePaid && shipByOpen:
		v.NextStep += " The delivery deadline has passed and the content is not delivered: the buyer may now cancel it, and the escrow is then refunded to the buyer's refund address."
	case (o.State == stateShipped || o.State == stateDelivered) && releaseAt.Valid:
		remaining := time.Duration(left.Float64 * float64(time.Second))
		// closed: lateDispute's rule, so the page offers no dispute the action would refuse (PM-4).
		v.Deadline = &EscrowDeadlineView{At: utcUntil(releaseAt.Time), Left: timeLeft(remaining), Due: remaining <= 0, Rechecked: rechecked,
			Digital: o.State == stateDelivered, Recheck: remaining <= 0 && recheckAsked, Outage: outage, closed: remaining <= 0 && !recheckAsked}
		if v.Deadline.VendorClosed, err = orderVendorBanned(ctx, a.db, o.ID); err != nil {
			return err
		}
		if !rechecked { // VEN103-1: release_recheck says a restore moved it; an outage's recheck resets that
			if v.Deadline.OutageMoved, err = xmrMovedByOutage(ctx, a.db, o.ID, o.State, v.Deadline.At); err != nil {
				return err
			}
		}
		if v.Deadline.VendorClosed { // PAY90-3: the buyer may still be refunded once the hold has lasted (A-652f)
			if v.Deadline.HoldRefundDays, err = holdRefundDaysShown(ctx, a.db); err != nil {
				return err
			}
		}
		v.NextStep = xmrDeadlineNextStep(d.OrderViewer, v.Deadline, v.RestorePaused)
	case o.State == stateDisputed && releaseAt.Valid:
		v.NextStep += " The release deadline of " + utcUntil(releaseAt.Time) + " no longer applies: opening the dispute cancelled it for good."
	case o.State == stateCompleted && !live:
		v.NextStep = "Who acts next: the vendor, who claims the escrowed coins on this page with a fresh address; the market then builds the release from this order's escrow account and sends it."
	case o.State == stateResolved && outcome == "release" && live:
		v.NextStep = "The dispute was decided for the vendor: the release to the vendor requested when the order was shipped pays the vendor; the market builds it from this order's escrow account and sends it. The release deadline no longer applies."
	case o.State == stateResolved && outcome == "release":
		v.NextStep = "The dispute was decided for the vendor: the vendor claims the escrowed coins on this page with a fresh address; the market then builds the release and sends it."
	case o.State == stateResolved && outcome == "refund" && tooFew:
		// A-437: the refund's coins were too few, so it ended and they were left out for a stray refund (xmrRefundTooFew).
		v.NextStep = "The dispute was decided for the buyer, but the market could not send the refund because the coins were too few to pay the network fee and an output of at least " + amount(minPayoutXMR, 12) + " XMR. A stray refund returns them, together with any later deposit, less the network fee, to the buyer's refund address once together they can pay the network fee."
	case o.State == stateResolved && outcome == "refund":
		v.NextStep = "The dispute was decided for the buyer: the coins in this escrow go back to the buyer's refund address by a refund the market builds and sends."
	}
	// The forms need ESCROW_XMR on (their actions do not exist otherwise), not a reachable node: they make no RPC.
	on := a.escrowXMR != nil
	v.Off = !on
	v.Ship = on && d.OrderViewer == roleVendor && o.State == statePaid && o.Kind == "physical"
	v.Confirm = on && d.OrderViewer == roleBuyer && (o.State == stateShipped || o.State == stateDelivered)
	v.Claim = on && d.OrderViewer == roleVendor && claimDue // the vendor desk's "Claim your payment" (A-710)
	if (v.Ship || v.Confirm || v.Claim) && d.Security == nil {
		if d.Security, _, _, err = a.securityView(ctx, d.User.ID); err != nil {
			return err
		}
	}
	return nil
}

// xmrDeadlineNextStep is the "who acts next" line of a shipped Monero escrow order with its release deadline, for viewer.
func xmrDeadlineNextStep(viewer string, dl *EscrowDeadlineView, paused bool) string {
	if dl.Digital {
		return xmrWindowNextStep(viewer, dl, paused)
	}
	if dl.VendorClosed { // A-652d: the release is held while the vendor is banned (xmrReleaseHeld)
		switch {
		case dl.Due:
			return xmrDeadlineNextStep(viewer, &EscrowDeadlineView{At: dl.At, Due: true, Recheck: dl.Recheck, Outage: dl.Outage}, paused) + " The vendor's account is closed, so the market then holds its payment: it sends nothing to the vendor while the account is closed, and no other address is ever paid."
		case viewer == roleBuyer:
			return "Who acts next: you. The vendor's account is closed: open a dispute before " + dl.At + " if the goods have not arrived, or use Confirm receipt below if they have. If you do neither by then, the order completes at that time and the market holds its payment: it sends nothing to the vendor while the account is closed, and no other address is ever paid."
		case viewer == roleVendor:
			return "Who acts next: the buyer, who confirms receipt or opens a dispute before " + dl.At + ". If the buyer does neither, the order completes at that time, and its release is held while your account is closed."
		}
		return "Who acts next: the buyer, who confirms receipt or opens a dispute before " + dl.At + ". The vendor's account is closed: if the buyer does neither, the order completes at that time and the market holds its payment; no other address is ever paid."
	}
	switch {
	case dl.Recheck && dl.Outage: // A-730
		return "The release deadline (" + dl.At + ") has passed, after an outage: it moves to 72 hours after the market has rechecked its escrow deadlines, and a dispute can still be opened until then."
	case dl.Recheck: // A-720 (PAY-5): xmrRecheckReleaseDeadlines will move it; lateDispute's exception holds until then
		return "The release deadline (" + dl.At + ") has passed in this market's restored records: it moves to 72 hours after the market has rechecked its escrow deadlines, and a dispute can still be opened until then."
	case dl.Due && paused:
		return "The release deadline (" + dl.At + ") has passed: the release is due, and the order completes once the market has finished reconciling its restore from backup."
	case dl.Due:
		return "The release deadline (" + dl.At + ") has passed: the release is due, and the order completes on the market's next pass."
	case viewer == roleBuyer:
		return "Who acts next: you. Use Confirm receipt below once the goods have arrived, or open a dispute before " + dl.At + " if they have not. If you cannot confirm receipt, you need do nothing: if you neither confirm receipt nor open a dispute by then, the order completes at that time and the market releases the coins to the vendor."
	case viewer == roleVendor:
		return "Who acts next: the buyer, who confirms receipt or opens a dispute before " + dl.At + ". If the buyer does neither, the order completes at that time and the market releases the coins to you; nothing is built or sent before the order is completed."
	}
	return "Who acts next: the buyer, who confirms receipt or opens a dispute before " + dl.At + ". If the buyer does neither, the order completes at that time and the market releases the coins to the vendor."
}

// xmrWindowNextStep is xmrDeadlineNextStep for a delivered digital order in its dispute window (A-709): the buyer checks
// the content and confirms receipt or opens a dispute before it ends; then the order completes and the vendor claims.
func xmrWindowNextStep(viewer string, dl *EscrowDeadlineView, paused bool) string {
	outcome := "the vendor can claim the coins in this escrow"
	if dl.VendorClosed { // A-652d: the release is held while the vendor is banned (xmrReleaseHeld)
		outcome = "the market holds its payment: it sends nothing to the vendor while the account is closed, and no other address is ever paid"
	}
	switch {
	case dl.Recheck && dl.Outage: // A-730
		return "The dispute window ended at " + dl.At + ", after an outage: it moves to 72 hours after the market has rechecked its escrow deadlines, and a dispute can still be opened until then. If the buyer neither confirms receipt nor opens a dispute by the moved end, the order completes at that time and " + outcome + "."
	case dl.Recheck: // A-720 (PAY-5): as in xmrDeadlineNextStep
		return "The dispute window ended at " + dl.At + " in this market's restored records: it moves to 72 hours after the market has rechecked its escrow deadlines, and a dispute can still be opened until then. If the buyer neither confirms receipt nor opens a dispute by the moved end, the order completes at that time and " + outcome + "."
	case dl.Due && paused:
		return "The dispute window ended at " + dl.At + ": the order completes once the market has finished reconciling its restore from backup, and then " + outcome + "."
	case dl.Due:
		return "The dispute window ended at " + dl.At + ": the order completes on the market's next pass, and then " + outcome + "."
	case viewer == roleBuyer:
		return "Who acts next: you. Check the delivered content now. Use Confirm receipt below once it works, or open a dispute before " + dl.At + " if it does not. If you do neither by then, the order completes at that time and " + outcome + "."
	case viewer == roleVendor && dl.VendorClosed:
		return "Who acts next: the buyer, who confirms receipt or opens a dispute before " + dl.At + ". If the buyer does neither, the order completes at that time, and its payment is held while your account is closed."
	case viewer == roleVendor:
		return "Who acts next: the buyer, who confirms receipt or opens a dispute before " + dl.At + ". If the buyer does neither, the order completes at that time and you can claim the coins in this escrow on this page; no claim is possible before then."
	}
	return "Who acts next: the buyer, who confirms receipt or opens a dispute before " + dl.At + ". If the buyer does neither, the order completes at that time and " + outcome + "."
}
