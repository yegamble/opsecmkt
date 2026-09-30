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
// honest-claim copy and the tracing notice, the funding figures and each deposit. On a draft that can use escrow the
// buyer is offered escrow checkout beside the pooled payment request. Nothing here makes an RPC.

func init() {
	registerRaw("/escrow-descriptor", escrowDescriptorDownload)
}

// escrowOrder fills the escrow panel of an escrow order, or the escrow checkout offer on a draft, for loadOrderDetail
// (which has already resolved the viewer). With ESCROW_BTC off nothing is added, so the page is as before.
func (a *App) escrowOrder(ctx context.Context, d *PageData) error {
	o := d.Order
	if !a.escrowConfigured {
		return nil
	}
	if !o.Escrow {
		return a.escrowCheckoutOffer(ctx, d)
	}
	v := &EscrowOrderView{Available: a.escrow != nil, Required: o.Amount, Threshold: 3, Open: o.State == stateAwaitingPayment}
	if a.escrow != nil {
		v.Threshold = a.escrow.confirmations
	}
	if d.OrderViewer == roleBuyer || d.OrderViewer == roleVendor {
		v.Own = d.OrderViewer
	}
	var refund string
	var feeBps int64
	var noted bool
	var windowFrom time.Time
	if err := a.db.QueryRowContext(ctx, `SELECT es.address,es.refund_address,es.fee_bps,es.underpaid_noted IS NOT NULL,
		GREATEST(es.created,(SELECT max(created) FROM payments WHERE order_id=es.order_id AND confirmations>=0 AND NOT locked))
		FROM escrows es WHERE es.order_id=$1`, o.ID).Scan(&v.Address, &refund, &feeBps, &noted, &windowFrom); err != nil {
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
	var received, pending int64
	required, err := parseAmount(o.Amount, 8)
	if err != nil {
		return err
	}
	remaining := required
	rows, err := a.db.QueryContext(ctx, "SELECT txid,idx,amount,confirmations,locked,after_close FROM payments WHERE order_id=$1 ORDER BY created,id", o.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var dep PaymentDeposit
		var amt int64
		var locked, afterClose bool
		if err = rows.Scan(&dep.TxID, &dep.Index, &amt, &dep.Confirmations, &locked, &afterClose); err != nil {
			rows.Close()
			return err
		}
		dep.Amount = amount(amt, 8)
		switch {
		case dep.Confirmations < 0 || locked:
			dep.State = "Conflicted or missing — not counted"
		case afterClose:
			dep.State = "Arrived after the order closed — not counted; recovering it is part of the settlement step, which this build does not have yet"
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
	const claimLater = "The vendor claims the escrowed coins by signing a settlement; that step is not available in this build yet."
	switch {
	case o.State == stateAwaitingPayment && len(v.Deposits) == 0:
		v.Status = "Waiting for a deposit"
	case o.State == stateAwaitingPayment && pending > 0:
		v.Status = "Deposit seen. Waiting for " + strconv.FormatInt(v.Threshold, 10) + " confirmations."
	case o.State == stateAwaitingPayment:
		v.Status = "Confirmed deposits are below the price"
		if noted {
			v.NextStep = "The payment window has ended. Refunding an escrow is part of the settlement step, which this build does not have yet; a top-up to the full price still completes the order."
		}
	case o.State == statePaid:
		v.Status, v.NextStep = "Funded", "The content could not be delivered automatically: the order completes when the vendor delivers it. "+claimLater
	case o.State == stateCompleted:
		v.Status, v.NextStep = "Funded and completed", claimLater
	case o.State == stateCancelled && received+pending == 0:
		v.Status = "Cancelled; nothing was received in this escrow"
	default:
		v.Status = stateLabel(o.State)
	}
	d.Escrow = v
	// Moves that would return or award the escrowed coins need a settlement (escrowTransitionHook refuses them).
	funded := received+pending > 0 || o.State != stateAwaitingPayment
	for i, t := range d.Transitions {
		if t.To == stateDisputed || t.To == stateCancelled && funded {
			d.Transitions[i].Action, d.Transitions[i].Payout = "", nil
			d.Transitions[i].Label = errEscrowSettlementLater.Error()
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
	case a.escrow == nil:
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
