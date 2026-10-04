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
	var refund string
	var feeBps int64
	var shipByOpen, underpaid bool
	var windowFrom time.Time
	var shipBy sql.NullTime
	if err := a.db.QueryRowContext(ctx, `SELECT es.address,es.refund_address,es.fee_bps,
		GREATEST(es.created,(SELECT max(created) FROM payments WHERE order_id=es.order_id AND confirmations>=0 AND NOT locked)),
		es.ship_by,COALESCE(es.ship_by<=now(),false),es.underpaid_noted IS NOT NULL
		FROM escrows es WHERE es.order_id=$1 AND es.coin='XMR'`, o.ID).Scan(&v.Address, &refund, &feeBps, &windowFrom, &shipBy, &shipByOpen, &underpaid); err != nil {
		return err
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
	var received, pending, afterCloseSeen int64
	remaining := required
	rows, err := a.db.QueryContext(ctx, "SELECT txid,amount,confirmations,locked,after_close FROM payments WHERE order_id=$1 ORDER BY created,id", o.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var dep PaymentDeposit
		var amt int64
		var locked, afterClose bool
		if err = rows.Scan(&dep.TxID, &amt, &dep.Confirmations, &locked, &afterClose); err != nil {
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
		case dep.Confirmations >= v.Threshold:
			dep.State, received = "Confirmed", received+amt
		default:
			dep.State, pending = "Waiting for confirmations", pending+amt
		}
		if dep.Confirmations >= 0 && !locked && !afterClose {
			remaining -= min(remaining, amt)
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
		v.CancelAfter = windowFrom.Add(expiryWindow()).UTC().Truncate(time.Minute).Format("2006-01-02 15:04 UTC")
	}
	switch {
	case o.State == stateAwaitingPayment && len(v.Deposits) == 0:
		v.Status = "Waiting for a deposit"
	case o.State == stateAwaitingPayment && pending > 0:
		v.Status = "Deposit seen. Waiting for " + strconv.FormatInt(v.Threshold, 10) + " confirmations."
	case o.State == stateAwaitingPayment && underpaid:
		v.Status = "The payment window ended with confirmed deposits below the price. Send the remaining amount to the same address to complete the order."
	case o.State == stateAwaitingPayment:
		v.Status = "Confirmed deposits are below the price"
	case o.State == statePaid && o.Kind == "physical":
		v.Status, v.NextStep = "Funded", "The vendor ships the goods; the order stays open until it is shipped and settled. If the vendor cancels it instead, the escrow is refunded to the buyer."
		if shipBy.Valid {
			v.NextStep += " Ship by " + utcFrom(shipBy.Time) + " (the listing's dispatch window plus 48 hours, from payment): if the order is not marked shipped by then, the buyer may cancel it and the escrow is refunded to the buyer."
		}
	case o.State == statePaid:
		v.Status, v.NextStep = "Funded", "The content could not be delivered automatically: the order completes when the vendor delivers it by hand. If the vendor cancels it instead, the escrow is refunded to the buyer."
	case o.State == stateShipped:
		v.Status = "Funded and shipped"
	case o.State == stateCompleted:
		v.Status, v.NextStep = "Funded and completed", "The escrowed coins are released to the vendor by a transaction the market builds and sends."
	case o.State == stateDisputed:
		v.Status, v.NextStep = "Disputed", "An administrator of this market decides the dispute. The decision is paid from this escrow: a refund to the buyer's refund address, or a release to the vendor."
	case o.State == stateCancelled && received+pending > 0:
		v.Status = "Cancelled; the coins in this escrow go back to the buyer's refund address by a refund the market builds and sends."
	case o.State == stateCancelled && afterCloseSeen > 0:
		v.Status = "Cancelled; a deposit arrived after the order closed. It is not counted and is returned to the buyer's refund address."
	case o.State == stateCancelled && !v.Available:
		// Nothing is read while Monero escrow is unavailable, so an empty ledger proves nothing.
		v.Status = "Cancelled. This escrow cannot be checked right now because Monero escrow is unavailable, so a deposit to it may not be shown here."
		v.Unread = "Not read while Monero escrow is unavailable"
	case o.State == stateCancelled:
		v.Status = "Cancelled; nothing was received in this escrow"
	default:
		v.Status = stateLabel(o.State)
	}
	if v.RestorePaused, err = restoreGateSet(ctx, a.db); err != nil {
		return err
	}
	d.XMREscrow = v
	if err = a.xmrReleaseView(ctx, d, v); err != nil {
		return err
	}
	// Only the actions a Monero escrow order can take (escrowOrder's rules): no pooled "Mark shipped" (the panel has the
	// Monero ship form, with the claim address and a password step-up); the buyer of a paid order cancels only a
	// physical one, from its ship-by time; a cancel that refunds coins states and carries their amount.
	counted := received + pending
	kept := d.Transitions[:0]
	for _, t := range d.Transitions {
		switch {
		case t.To == stateShipped:
			continue
		case t.To == stateCancelled && o.State == statePaid && d.OrderViewer == roleBuyer && !shipBy.Valid:
			continue
		case t.To == stateCancelled && o.State == statePaid && d.OrderViewer == roleBuyer && !shipByOpen:
			t.Action, t.Label = "", "You can cancel from "+utcFrom(shipBy.Time)+" if the vendor has not shipped the order by then; the escrow is then refunded to your refund address."
		case t.To == stateCancelled && counted > 0:
			t.Payout = &PayoutPreview{Seen: counted, Confirm: "Cancelling refunds the " + amount(counted, 12) + " XMR in this escrow, less the network fee, to the buyer's refund address given at checkout. The market builds and sends the refund transaction. This is final."}
		}
		kept = append(kept, t)
	}
	d.Transitions = kept
	return nil
}

// xmrEscrowCheckoutOffer offers Monero escrow checkout to the buyer of an XMR draft on a listing that takes escrow, or
// says why it cannot be used now. Any other order, and every order while ESCROW_XMR is off, is left as it was.
func (a *App) xmrEscrowCheckoutOffer(ctx context.Context, d *PageData) error {
	o := d.Order
	if a.escrowXMR == nil || o.State != stateDraft || d.OrderViewer != roleBuyer || o.Currency != "XMR" {
		return nil
	}
	eligible, _, _, err := escrowEligible(ctx, a.db, o)
	if err != nil || !eligible {
		return err
	}
	v := &XMREscrowCheckoutView{Physical: o.Kind == "physical", Custody: xmrCustody}
	vendorCanEncrypt := true
	if v.Physical {
		if vendorCanEncrypt, err = userCanReceiveEncrypted(ctx, a.db, o.VendorID); err != nil {
			return err
		}
	}
	var gate, sweep, free bool
	if err = a.db.QueryRowContext(ctx, `SELECT `+restoreGateSQL+`,EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value='true'),
		EXISTS(SELECT 1 FROM xmr_escrow_slots s JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE s.state='free' AND w.state='active')`, xmrEscrowSweepKey).Scan(&gate, &sweep, &free); err != nil {
		return err
	}
	switch {
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
	var releaseAt sql.NullTime
	var left sql.NullFloat64
	var shipByOpen, rechecked, live bool
	if err := a.db.QueryRowContext(ctx, `SELECT COALESCE(es.ship_by<=now(),false),es.release_at,extract(epoch FROM es.release_at-now()),es.release_recheck,
		EXISTS(SELECT 1 FROM xmr_settlements s WHERE s.order_id=es.order_id AND s.kind IN ('release','refund') AND s.state<>'superseded')
		FROM escrows es WHERE es.order_id=$1 AND es.coin='XMR'`, o.ID).Scan(&shipByOpen, &releaseAt, &left, &rechecked, &live); err != nil {
		return err
	}
	outcome, err := escrowDisputeOutcome(ctx, a.db, o.ID)
	if err != nil {
		return err
	}
	switch {
	case o.State == statePaid && o.Kind == "physical" && shipByOpen:
		v.NextStep += " The ship-by time has passed and the order is not marked shipped: the buyer may now cancel it, and the escrow is then refunded to the buyer's refund address."
	case o.State == stateShipped && releaseAt.Valid:
		remaining := time.Duration(left.Float64 * float64(time.Second))
		v.Deadline = &EscrowDeadlineView{At: utcUntil(releaseAt.Time), Left: timeLeft(remaining), Due: remaining <= 0, Rechecked: rechecked}
		v.NextStep = xmrDeadlineNextStep(d.OrderViewer, v.Deadline, v.RestorePaused)
	case o.State == stateDisputed && releaseAt.Valid:
		v.NextStep += " The release deadline of " + utcUntil(releaseAt.Time) + " no longer applies: opening the dispute cancelled it for good."
	case o.State == stateCompleted && !live:
		v.NextStep = "Who acts next: the vendor, who claims the escrowed coins on this page with a fresh address; the market then builds the release from this order's escrow account and sends it."
	case o.State == stateResolved && outcome == "release" && live:
		v.NextStep = "The dispute was decided for the vendor: the release to the vendor requested when the order was shipped pays the vendor; the market builds it from this order's escrow account and sends it. The release deadline no longer applies."
	case o.State == stateResolved && outcome == "release":
		v.NextStep = "The dispute was decided for the vendor: the vendor claims the escrowed coins on this page with a fresh address; the market then builds the release and sends it."
	case o.State == stateResolved && outcome == "refund":
		v.NextStep = "The dispute was decided for the buyer: the coins in this escrow go back to the buyer's refund address by a refund the market builds and sends."
	}
	v.Ship = d.OrderViewer == roleVendor && o.State == statePaid && o.Kind == "physical"
	v.Confirm = d.OrderViewer == roleBuyer && o.State == stateShipped
	v.Claim = d.OrderViewer == roleVendor && settlementFits("release", o.State, outcome) && !live
	if (v.Ship || v.Confirm || v.Claim) && d.Security == nil {
		if d.Security, _, _, err = a.securityView(ctx, d.User.ID); err != nil {
			return err
		}
	}
	return nil
}

// xmrDeadlineNextStep is the "who acts next" line of a shipped Monero escrow order with its release deadline, for viewer.
func xmrDeadlineNextStep(viewer string, dl *EscrowDeadlineView, paused bool) string {
	switch {
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
