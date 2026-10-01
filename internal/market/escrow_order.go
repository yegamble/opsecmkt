package market

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"time"
)

// The escrow on the order page (WR-escrow Phase 1, slice P1-b; ES-3, ES-10): the escrow address, the escrow
// descriptor with its checksum (the reader's own key with its origin, the other two bare) and a download of it, the
// honest-claim copy and the tracing notice, the funding figures and each deposit, and the order's settlement
// (escrowSettlementView, P1-c1). On a draft that can use escrow the buyer is offered escrow checkout beside the pooled
// payment request. Nothing here makes an RPC.

func init() {
	registerRaw("/escrow-descriptor", escrowDescriptorDownload)
}

// escrowOrder fills the escrow panel of an escrow order, or the escrow checkout offer on a draft, for loadOrderDetail
// (which has already resolved the viewer). An escrow order always gets its panel: with ESCROW_BTC switched off, or
// escrow down, it shows the escrow address with the unavailable copy, never the pooled payment panel, which would say
// no funds are held (A-315). Any other order with ESCROW_BTC off gets nothing, so its page is as before.
func (a *App) escrowOrder(ctx context.Context, d *PageData) error {
	o := d.Order
	if !o.Escrow {
		if !a.escrowConfigured {
			return nil
		}
		return a.escrowCheckoutOffer(ctx, d)
	}
	e := a.escrow.Load()
	v := &EscrowOrderView{Available: e != nil, Download: a.escrowConfigured, Required: o.Amount, Threshold: 3, Open: o.State == stateAwaitingPayment}
	if e != nil {
		v.Threshold = e.confirmations
	}
	if d.OrderViewer == roleBuyer || d.OrderViewer == roleVendor {
		v.Own = d.OrderViewer
	}
	var refund string
	var feeBps int64
	var watched bool
	var windowFrom time.Time
	if err := a.db.QueryRowContext(ctx, `SELECT es.address,es.refund_address,es.fee_bps,
		GREATEST(es.created,(SELECT max(created) FROM payments WHERE order_id=es.order_id AND confirmations>=0 AND NOT locked)),
		`+escrowWatchedSQL+`
		FROM escrows es JOIN orders o ON o.id=es.order_id WHERE es.order_id=$1`, o.ID).Scan(&v.Address, &refund, &feeBps, &windowFrom, &watched); err != nil {
		return err
	}
	if v.Own == roleBuyer {
		v.RefundAddress = refund
	}
	if feeBps > 0 {
		v.Fee = strconv.FormatFloat(float64(feeBps)/100, 'f', -1, 64) + "% of the price, fixed when the escrow was created; taken when the vendor claims the escrow"
	}
	desc, err := escrowDescriptorFor(ctx, a.db, o.ID, v.Own)
	if err != nil {
		return err
	}
	v.Descriptor = desc
	var received, pending, afterCloseSeen int64
	required, err := parseAmount(o.Amount, 8)
	if err != nil {
		return err
	}
	remaining := required
	rows, err := a.db.QueryContext(ctx, "SELECT txid,idx,amount,confirmations,locked,after_close,escrow_left_out FROM payments WHERE order_id=$1 ORDER BY created,id", o.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var dep PaymentDeposit
		var amt int64
		var locked, afterClose, leftOut bool
		if err = rows.Scan(&dep.TxID, &dep.Index, &amt, &dep.Confirmations, &locked, &afterClose, &leftOut); err != nil {
			rows.Close()
			return err
		}
		dep.Amount = amount(amt, 8)
		switch {
		case dep.Confirmations < 0 || locked:
			dep.State = "Conflicted or missing — not counted"
		case afterClose:
			dep.State = "Arrived after the order closed — not counted; returned to the buyer's refund address by a stray refund"
			afterCloseSeen++
		case leftOut:
			dep.State = "Left out of the settlement after waiting " + escrowBuildWaitText + " — returned to the buyer's refund address by a stray refund once confirmed"
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
	v.Confirmed, v.Unconfirmed, v.Remaining, v.NeedsTopUp = amount(received, 8), amount(pending, 8), amount(remaining, 8), remaining > 0
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
	case o.State == stateAwaitingPayment:
		v.Status = "Confirmed deposits are below the price"
	case o.State == statePaid:
		v.Status, v.NextStep = "Funded", "The content could not be delivered automatically: the order completes when the vendor delivers it by hand. If the vendor cancels it instead, the escrow is refunded to the buyer."
	case o.State == stateCompleted:
		v.Status, v.NextStep = "Funded and completed", "The vendor claims the escrowed coins from this page."
	case o.State == stateDisputed:
		v.Status, v.NextStep = "Disputed", "An administrator of this market decides the dispute. The decision is paid from this escrow by a settlement: a refund to the buyer's refund address, or the vendor's claim."
	case o.State == stateCancelled && received+pending > 0:
		v.Status = "Cancelled; the coins in this escrow go back to the buyer's refund address through the refund settlement below."
	case o.State == stateCancelled && afterCloseSeen > 0:
		// Coins sent after the cancellation are never counted, but they are at the escrow address (QA31-1).
		v.Status = "Cancelled; a deposit arrived after the order closed. It is not counted and is held at the escrow address."
	case o.State == stateCancelled && e == nil:
		// Nothing is read while escrow is not enabled, so an empty ledger proves nothing (QA31-1), nor do zero
		// figures (A-338).
		v.Status = "Cancelled. This escrow cannot be checked right now because Bitcoin escrow is unavailable, so a deposit to it may not be shown here."
		v.Unread = "Not read while Bitcoin escrow is unavailable"
	case o.State == stateCancelled && !watched:
		// The watcher stopped reading this address (escrowWatchedSQL), so an empty ledger proves nothing (A-332), nor
		// do zero figures (A-338).
		v.Status = "Cancelled. This escrow address is no longer watched: deposits are watched for " + escrowCreatedWatch + " after checkout (and for " + escrowClosedWatch + " after the order closes), so a deposit to it may not be shown here."
		v.Unread = "Not watched any more"
	case o.State == stateCancelled:
		v.Status = "Cancelled; nothing was received in this escrow"
	default:
		v.Status = stateLabel(o.State)
	}
	if v.RestorePaused, err = restoreGateSet(ctx, a.db); err != nil {
		return err
	}
	d.Escrow = v
	if err = a.escrowSettlementView(ctx, d, v); err != nil {
		return err
	}
	if err = a.escrowDecisionView(ctx, d, v); err != nil {
		return err
	}
	// Cancelling an order that holds coins requests its refund, so the cancel form states it and carries its amount,
	// as a pooled refund's form does (checkPayoutSeen, escrowSettleTerminal).
	counted := received + pending
	for i, t := range d.Transitions {
		switch {
		case t.To == stateCancelled && counted > 0:
			d.Transitions[i].Payout = &PayoutPreview{Seen: counted, Confirm: "Cancelling refunds the " + amount(counted, 8) +
				" " + a.escrowNetwork() + " BTC in this escrow, less the network fee, to the buyer's refund address given at checkout. The market builds the refund transaction; the buyer or the vendor then signs it on this page. This is final."}
		}
	}
	return nil
}

// escrowDescriptorFor is the escrow descriptor of order as reader (buyer, vendor or "") sees it: the bare keys of
// wsh(sortedmulti(2, buyer, vendor, market)), the reader's own with its origin, and the checksum.
func escrowDescriptorFor(ctx context.Context, db *sql.DB, orderID, reader string) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT p.party,p.pubkey,p.origin_fingerprint,p.origin_path,p.derivation_suffix FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id
		WHERE es.order_id=$1 ORDER BY CASE p.party WHEN 'buyer' THEN 0 WHEN 'vendor' THEN 1 ELSE 2 END`, orderID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var party, pub, fp, origin, suffix string
		if err = rows.Scan(&party, &pub, &fp, &origin, &suffix); err != nil {
			return "", err
		}
		if party == reader {
			pub = "[" + fp + origin + suffix + "]" + pub
		}
		keys = append(keys, pub)
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if len(keys) != 3 {
		return "", sql.ErrNoRows
	}
	return withChecksum("wsh(sortedmulti(2," + keys[0] + "," + keys[1] + "," + keys[2] + "))"), nil
}

// escrowCheckoutOffer offers escrow checkout to the buyer of a BTC draft on a listing that takes escrow, or says why
// it cannot be used yet. Any other order is left as it was.
func (a *App) escrowCheckoutOffer(ctx context.Context, d *PageData) error {
	o := d.Order
	if o.State != stateDraft || d.OrderViewer != roleBuyer || o.Currency != "BTC" {
		return nil
	}
	eligible, _, err := escrowEligible(ctx, a.db, o)
	if err != nil || !eligible {
		return err
	}
	v := &EscrowCheckoutView{}
	var buyerActive, vendorActive bool
	if err = a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL),
		EXISTS(SELECT 1 FROM user_escrow_keys WHERE user_id=$2 AND active_at IS NOT NULL)`, o.BuyerID, o.VendorID).Scan(&buyerActive, &vendorActive); err != nil {
		return err
	}
	switch {
	case a.escrow.Load() == nil:
		v.Reason = errEscrowDown.Error()
	case o.BuyerID == o.VendorID:
		v.Reason = errEscrowSelf.Error()
	case !buyerActive:
		v.Reason, v.KeyLink = "Activate your Bitcoin escrow key first: register it and sign its practice transaction.", true
	case !vendorActive:
		v.Reason = "The vendor has no active Bitcoin escrow key, so this order cannot use escrow."
	}
	d.EscrowCheckout = v
	return nil
}

// escrowDescriptorDownload serves GET /escrow-descriptor?id=<order>: the escrow descriptor as the order page shows it
// to its reader, as a text file. With ESCROW_BTC off, for anyone but the order's buyer and vendor, and for an order
// without an escrow it answers as an unknown page does.
func escrowDescriptorDownload(a *App, w http.ResponseWriter, r *http.Request, u *User) {
	if !a.escrowConfigured || a.db == nil || u == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	o, err := scanOrder(a.db.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 AND (o.buyer_id=$2 OR p.vendor_id=$2)", r.URL.Query().Get("id"), u.ID))
	if err == sql.ErrNoRows || err == nil && !o.Escrow {
		http.NotFound(w, r)
		return
	}
	reader := roleVendor
	if u.ID == o.BuyerID {
		reader = roleBuyer
	}
	var desc string
	if err == nil {
		desc, err = escrowDescriptorFor(ctx, a.db, o.ID, reader)
	}
	if err != nil {
		serverError(w, r, 503, "Service unavailable", errorCause(err))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="escrow-`+shortID(o.ID)+`.txt"`)
	w.Write([]byte(desc + "\n"))
}
