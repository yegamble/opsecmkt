package market

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
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

// strayHeldLine ends a deposit line or the status of an order whose coins a stray refund returns are held (A-434): why
// they stay, and what tries again (requestStray; the operator guide's Stray refunds).
const strayHeldLine = "held at the escrow address, because the coins there are too few to return after the network fee. The buyer's recover request, more coins, or a lower network fee rate set by the market tries again."

// escrowOrder fills the escrow panel of an escrow order, or the escrow checkout offer on a draft, for loadOrderDetail
// (which has already resolved the viewer). An escrow order always gets its panel: with ESCROW_BTC switched off, or
// escrow down, it shows the escrow address with the unavailable copy, never the pooled payment panel, which would say
// no funds are held (A-315). Any other order with ESCROW_BTC off gets nothing, so its page is as before. A Monero
// escrow order (migration 084) gets its Monero escrow panel instead (xmrEscrowOrder), and an XMR draft the Monero
// escrow checkout offer (xmrEscrowCheckoutOffer).
func (a *App) escrowOrder(ctx context.Context, d *PageData) error {
	o := d.Order
	if !o.Escrow {
		if err := a.xmrEscrowCheckoutOffer(ctx, d); err != nil || !a.escrowConfigured {
			return err
		}
		return a.escrowCheckoutOffer(ctx, d)
	}
	if !o.btcEscrow() {
		return a.xmrEscrowOrder(ctx, d)
	}
	e := a.escrow.Load()
	v := &EscrowOrderView{Available: e != nil, Download: a.escrowConfigured, Required: o.Amount, Threshold: 3, Open: o.State == stateAwaitingPayment}
	if e != nil {
		v.Threshold = e.confirmations
	}
	if d.OrderViewer == roleBuyer || d.OrderViewer == roleVendor {
		v.Own = d.OrderViewer
	}
	var refund, strayState string
	var feeBps int64
	var watched, shipByOpen, rechecked, tooFew bool
	var windowFrom time.Time
	var shipBy, releaseAt sql.NullTime
	var releaseLeft sql.NullFloat64
	// A-434: held is requestStray's rule (strayHeldSQL), so the deposit lines and the status say the coins a stray
	// refund returns are held, and why, rather than promise one. The fee rate is unknown (0) while escrow is down.
	var rate int64
	if e != nil {
		rate = e.feeRate
	}
	if err := a.db.QueryRowContext(ctx, `SELECT es.address,es.refund_address,es.fee_bps,
		GREATEST(es.created,(SELECT max(created) FROM payments WHERE order_id=es.order_id AND confirmations>=0 AND NOT locked)),
		`+escrowWatchedSQL+`,es.ship_by,COALESCE(es.ship_by<=now(),false),es.release_at,extract(epoch FROM es.release_at-now()),es.release_recheck,
		o.state IN ('completed','cancelled','resolved') AND `+strayHeldSQL+`,
		EXISTS(SELECT 1 FROM settlements s WHERE s.order_id=es.order_id AND s.kind='refund' AND s.state='failed' AND s.build_error=$3),
		COALESCE((SELECT s.state FROM settlements s WHERE s.order_id=es.order_id AND s.kind='stray_refund' ORDER BY s.id DESC LIMIT 1),'')
		FROM escrows es JOIN orders o ON o.id=es.order_id WHERE es.order_id=$1 AND es.coin='BTC'`, o.ID, v.Threshold, settleTooSmall, rate).Scan(&v.Address, &refund, &feeBps, &windowFrom, &watched,
		&shipBy, &shipByOpen, &releaseAt, &releaseLeft, &rechecked, &v.Held, &tooFew, &strayState); err != nil {
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
	// A-456b: a per-order escrow's parties sign in an order wallet, made from its setup page (escrow_setup.go).
	var marketSuffix string
	if err = a.db.QueryRowContext(ctx, "SELECT ep.derivation_suffix FROM escrow_pubkeys ep JOIN escrows es ON es.id=ep.escrow_id WHERE es.order_id=$1 AND es.coin='BTC' AND ep.party='market'", o.ID).Scan(&marketSuffix); err != nil && err != sql.ErrNoRows {
		return err
	}
	v.PerOrder = isPerOrderKey(escrowKeyRow{Suffix: marketSuffix})
	if v.Own != "" {
		// A party signs on this page, so the keypoolrefill advice of /escrow-key is repeated here when this escrow's
		// child key lies past escrowRangeAdvice: past its descriptor range a Core wallet cannot sign (P2-R10, A-397).
		var suffix string
		err := a.db.QueryRowContext(ctx, "SELECT ep.derivation_suffix FROM escrow_pubkeys ep JOIN escrows es ON es.id=ep.escrow_id WHERE es.order_id=$1 AND es.coin='BTC' AND ep.party=$2", o.ID, v.Own).Scan(&suffix)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if i := strings.LastIndexByte(suffix, '/'); i >= 0 {
			if n, err := strconv.ParseInt(strings.TrimSuffix(suffix[i+1:], "h"), 10, 64); err == nil && n > escrowRangeAdvice {
				v.RangePath, v.KeypoolSize = suffix, n+1000
			}
		}
	}
	// unspent counts the deposits that do not count for the order and that no settlement spends yet (A-435).
	var received, pending, afterCloseSeen, leftOutSeen, unspent int64
	required, err := parseAmount(o.Amount, 8)
	if err != nil {
		return err
	}
	remaining := required
	rows, err := a.db.QueryContext(ctx, "SELECT txid,idx,amount,confirmations,locked,after_close,escrow_left_out,spent_outside,COALESCE(spent_by,''),settlement_id IS NOT NULL FROM payments WHERE order_id=$1 ORDER BY created,id", o.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var dep PaymentDeposit
		var amt int64
		var locked, afterClose, leftOut, outside, settled bool
		var spentBy string
		if err = rows.Scan(&dep.TxID, &dep.Index, &amt, &dep.Confirmations, &locked, &afterClose, &leftOut, &outside, &spentBy, &settled); err != nil {
			rows.Close()
			return err
		}
		dep.Amount = amount(amt, 8)
		// One of the deposits a stray refund would return while they are held (A-434).
		held := v.Held && !settled && dep.Confirmations >= v.Threshold
		switch {
		case outside && spentBy != "":
			dep.State = "Spent outside the market by transaction " + spentBy + ", a transaction this market did not make; market staff were notified"
		case outside:
			dep.State = "Spent outside the market by a transaction this market did not make; market staff were notified"
		case dep.Confirmations < 0 || locked:
			dep.State = "Conflicted or missing — not counted"
		case afterClose && held:
			dep.State = "Arrived after the order closed — not counted; " + strayHeldLine
			afterCloseSeen++
		case afterClose:
			dep.State = "Arrived after the order closed — not counted; returned to the buyer's refund address by a stray refund"
			afterCloseSeen++
		case leftOut:
			// Left out of a bounded build (A-342), of a release or refund built before it arrived (A-374), or not counted
			// when the order was cancelled and confirmed since (A-428). A stray refund is requested only once the order
			// is closed (QA428-5).
			dep.State = "Left out — not counted for the order; returned to the buyer's refund address by a stray refund once confirmed"
			switch {
			case held:
				dep.State = "Left out — not counted for the order; " + strayHeldLine
			case !isTerminal(o.State):
				dep.State = "Left out — not counted for the order; returned to the buyer's refund address by a stray refund once the order is closed and it is confirmed"
			}
			leftOutSeen++
		case dep.Confirmations >= v.Threshold:
			dep.State, received = "Confirmed", received+amt
		default:
			dep.State, pending = "Waiting for confirmations", pending+amt
		}
		if dep.Confirmations >= 0 && !locked && !afterClose && !outside {
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
	v.Confirmed, v.Unconfirmed, v.Remaining, v.NeedsTopUp = amount(received, 8), amount(pending, 8), amount(remaining, 8), remaining > 0
	for _, s := range []*string{&v.Confirmed, &v.Unconfirmed, &v.Remaining} {
		if *s == "" {
			*s = "0"
		}
	}
	if v.Open {
		v.CancelAfter = windowFrom.Add(expiryWindow()).UTC().Truncate(time.Minute).Format("2006-01-02 15:04 UTC")
		// QA4W1-5: past the window with no deposit waiting for confirmations, only a watcher pass that read the node
		// cancels the order. While the watcher last found the node not synced (A-439's state) the page says the order
		// stays open; in the usual gap before the next pass of a synced node it keeps the usual wording.
		notSynced, _ := a.escrowSync.Load().(string)
		v.WindowPassed = notSynced != "" && pending == 0 && time.Now().After(windowFrom.Add(expiryWindow()))
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
			// P2-R5 (escrow_deadline.go): the vendor's dispatch window plus 48 hours, from payment.
			v.NextStep += " Ship by " + utcFrom(shipBy.Time) + " (the listing's dispatch window plus 48 hours, from payment): if the order is not marked shipped by then, the buyer may cancel it and the escrow is refunded to the buyer."
		}
	case o.State == statePaid:
		v.Status, v.NextStep = "Funded", "The content could not be delivered automatically: the order completes when the vendor delivers it by hand. If the vendor cancels it instead, the escrow is refunded to the buyer."
	case o.State == stateShipped:
		// The release's "who acts next" line replaces this once it exists (escrowSettlementView, P2-R2).
		v.Status, v.NextStep = "Funded and shipped", "The vendor marked the order shipped; the market builds the release to the vendor, which the vendor and the buyer each sign."
	case o.State == stateCompleted && o.Kind == "physical":
		v.Status, v.NextStep = "Funded and completed", "The order is completed: the release to the vendor built when it was shipped pays the escrowed coins to the vendor."
	case o.State == stateCompleted:
		v.Status, v.NextStep = "Funded and completed", "The vendor claims the escrowed coins from this page."
	case o.State == stateDisputed:
		v.Status, v.NextStep = "Disputed", "An administrator of this market decides the dispute. The decision is paid from this escrow by a settlement: a refund to the buyer's refund address, or the vendor's claim."
	case o.State == stateCancelled && received+pending > 0:
		v.Status = "Cancelled; the coins in this escrow go back to the buyer's refund address through the refund settlement below."
	case o.State == stateCancelled && tooFew && leftOutSeen > 0 && (v.Held || unspent > 0 || strayStatus(strayState, unspent, "them") != ""):
		// A-435: the refund failed too-small and its coins were left out for a stray refund (leaveOutTooFew).
		v.Status = "Cancelled; the market could not build the refund. The coins in this escrow are "
		switch tail := strayStatus(strayState, unspent, "them"); {
		case tail != "":
			v.Status += tail
		case v.Held:
			v.Status += strayHeldLine
		default:
			v.Status += "held at the escrow address until a stray refund returns them to the buyer's refund address."
		}
	case o.State == stateCancelled && afterCloseSeen > 0 && strayStatus(strayState, unspent, "it") != "":
		v.Status = "Cancelled; a deposit arrived after the order closed. It is not counted and is " + strayStatus(strayState, unspent, "it")
	case o.State == stateCancelled && leftOutSeen > 0 && strayStatus(strayState, unspent, "it") != "":
		v.Status = "Cancelled; a deposit to this escrow did not count for the order. It is " + strayStatus(strayState, unspent, "it")
	case o.State == stateCancelled && afterCloseSeen > 0:
		// Coins sent after the cancellation are never counted, but they are at the escrow address (QA31-1).
		v.Status = "Cancelled; a deposit arrived after the order closed. It is not counted and is held at the escrow address."
	case o.State == stateCancelled && leftOutSeen > 0 && v.Held:
		v.Status = "Cancelled; a deposit to this escrow did not count for the order. It is " + strayHeldLine
	case o.State == stateCancelled && leftOutSeen > 0:
		// A-428: a deposit that did not count when the order was cancelled, confirmed since and left out.
		v.Status = "Cancelled; a deposit to this escrow did not count for the order. It is held at the escrow address and returned to the buyer's refund address by a stray refund."
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
	if o.State == stateShipped && releaseAt.Valid {
		// P2-R3, P2-R9 (escrow_deadline.go): the release deadline, and the time left when this page loaded.
		left := time.Duration(releaseLeft.Float64 * float64(time.Second))
		v.Deadline = &EscrowDeadlineView{At: utcUntil(releaseAt.Time), Left: timeLeft(left), Due: left <= 0, Rechecked: rechecked}
	}
	d.Escrow = v
	if err = a.escrowSettlementView(ctx, d, v); err != nil {
		return err
	}
	if v.Deadline != nil {
		var buyerSigned, vendorSigned bool
		if err = a.db.QueryRowContext(ctx, `SELECT COALESCE(bool_or(g.party='buyer'),false),COALESCE(bool_or(g.party='vendor'),false) FROM settlement_signatures g JOIN settlements s ON s.id=g.settlement_id
			WHERE s.order_id=$1 AND s.kind='release' AND s.state<>'superseded'`, o.ID).Scan(&buyerSigned, &vendorSigned); err != nil {
			return err
		}
		v.NextStep += " " + deadlineNextStep(d.OrderViewer, v.Deadline, v.RestorePaused, buyerSigned, vendorSigned)
	}
	if err = a.escrowDecisionView(ctx, d, v); err != nil {
		return err
	}
	// Cancelling an order that holds coins requests its refund, so the cancel form states it and carries its amount,
	// as a pooled refund's form does (checkPayoutSeen, escrowSettleTerminal).
	counted := received + pending
	// P2-R5 (escrow_deadline.go): the buyer of a paid physical escrow order may cancel it from its ship-by time; before
	// then the page says when. A paid digital escrow order has none, so its buyer is offered no cancel.
	kept := d.Transitions[:0]
	for _, t := range d.Transitions {
		if t.To == stateCancelled && o.State == statePaid && d.OrderViewer == roleBuyer {
			switch {
			case !shipBy.Valid:
				continue
			case !shipByOpen:
				t.Action, t.Label = "", "You can cancel from "+utcFrom(shipBy.Time)+" if the vendor has not shipped the order by then; the escrow is then refunded to your refund address."
			}
		}
		kept = append(kept, t)
	}
	d.Transitions = kept
	for i, t := range d.Transitions {
		switch {
		case t.To == stateShipped:
			// P2-R1: an escrow order is marked shipped with the claim address its release pays and the password.
			d.Transitions[i].Action = "/escrow/ship"
			if e == nil || !a.escrowConfigured {
				d.Transitions[i].Action, d.Transitions[i].Label = "", errEscrowDown.Error()
			} else if d.Security == nil {
				if d.Security, _, _, err = a.securityView(ctx, d.User.ID); err != nil {
					return err
				}
			}
		case t.To == stateCancelled && counted > 0:
			d.Transitions[i].Payout = &PayoutPreview{Seen: counted, Confirm: "Cancelling refunds the " + amount(counted, 8) +
				" " + a.escrowNetwork() + " BTC in this escrow, less the network fee, to the buyer's refund address given at checkout. The market builds the refund transaction; the buyer or the vendor then signs it on this page. This is final."}
		}
	}
	return nil
}

// strayStatus ends a cancelled order's status about the coins a stray refund returns (A-435), after "It is" or "The coins
// in this escrow are", by the state of the order's latest stray refund: requested or awaiting signatures, broadcast or
// confirmed. It is "" when there is none of these, or when a deposit that does not count is not spent by it yet
// (unspent; a stray refund still building claims its deposits only once built), so the status says what it said
// before: the coins are held.
func strayStatus(state string, unspent int64, it string) string {
	switch {
	case state == "building":
	case unspent > 0:
		return ""
	case state == "awaiting_signature" || state == "party_signed" || state == "signed":
	case state == "broadcast":
		return "being returned to the buyer's refund address by the stray refund below, which is broadcast and waits for confirmations."
	case state == "confirmed":
		return "back at the buyer's refund address: the stray refund below that returned " + it + " is confirmed."
	default:
		return ""
	}
	return "held at the escrow address until the stray refund below, which returns " + it + " to the buyer's refund address, is signed and broadcast."
}

// escrowDescriptorFor is the escrow descriptor of order as reader (buyer, vendor or "") sees it: the bare keys of
// wsh(sortedmulti(2, buyer, vendor, market)), the reader's own with its origin, and the checksum.
func escrowDescriptorFor(ctx context.Context, db *sql.DB, orderID, reader string) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT p.party,p.pubkey,p.origin_fingerprint,p.origin_path,p.derivation_suffix FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id
		WHERE es.order_id=$1 AND es.coin='BTC' ORDER BY CASE p.party WHEN 'buyer' THEN 0 WHEN 'vendor' THEN 1 ELSE 2 END`, orderID)
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
	eligible, _, _, err := escrowEligible(ctx, a.db, o)
	if err != nil || !eligible {
		return err
	}
	v := &EscrowCheckoutView{Physical: o.Kind == "physical"}
	var buyerActive, vendorActive, buyerPerOrder, vendorPerOrder bool
	if err = a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL),
		EXISTS(SELECT 1 FROM user_escrow_keys WHERE user_id=$2 AND active_at IS NOT NULL),
		EXISTS(SELECT 1 FROM user_escrow_keys WHERE user_id=$1 AND layout<>'core'),EXISTS(SELECT 1 FROM user_escrow_keys WHERE user_id=$2 AND layout<>'core')`,
		o.BuyerID, o.VendorID).Scan(&buyerActive, &vendorActive, &buyerPerOrder, &vendorPerOrder); err != nil {
		return err
	}
	vendorCanEncrypt := true
	if v.Physical {
		if vendorCanEncrypt, err = userCanReceiveEncrypted(ctx, a.db, o.VendorID); err != nil {
			return err
		}
	}
	gate, err := restoreGateSet(ctx, a.db)
	if err != nil {
		return err
	}
	sweep, err := escrowSweepRequired(ctx, a.db)
	if err != nil {
		return err
	}
	switch {
	case a.escrow.Load() == nil:
		v.Reason = errEscrowDown.Error()
	case gate:
		v.Reason = "Bitcoin escrow is paused while this market restores its records from a backup, so no escrow can be created until the operator has finished. Try again later."
	case sweep:
		v.Reason = "Bitcoin escrow is paused until this market has checked its escrow wallet after restoring its records from a backup, so no escrow can be created yet. Try again later."
	case o.BuyerID == o.VendorID:
		v.Reason = errEscrowSelf.Error()
	case !buyerActive:
		v.Reason, v.KeyLink = "Activate your Bitcoin escrow key first: register it and sign its practice transaction.", true
	case !vendorActive:
		v.Reason = "The vendor has no active Bitcoin escrow key, so this order cannot use escrow."
	// P4-R18, as escrowParties refuses them: a mixed pair, naming the vendor's key type (a buyer can register a key of
	// the vendor's kind: a Bitcoin Core key always, a per-order one once per-order escrows are on), and a pair of
	// per-order keys while the per-order switch is off (A-456b turns them on).
	case vendorPerOrder && !buyerPerOrder:
		v.Reason, v.KeyLink = strings.TrimSuffix(errEscrowMixedVendorPerOrder.Error(), " Nothing was changed."), perOrderEscrows.Load()
	case buyerPerOrder && !vendorPerOrder:
		v.Reason, v.KeyLink = strings.TrimSuffix(errEscrowMixedVendorCore.Error(), " Nothing was changed.")+" Register a Bitcoin Core escrow key and sign its practice transaction to use escrow.", true
	case buyerPerOrder && !perOrderEscrows.Load():
		v.Reason, v.KeyLink = strings.TrimSuffix(errEscrowBuyerPerOrder.Error(), " Nothing was changed.")+" Register a Bitcoin Core escrow key and sign its practice transaction to use escrow.", true
	case v.Physical && !vendorCanEncrypt:
		v.Reason = noVendorKeyReason // A-83: the buyer could not send the vendor an encrypted shipping address
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
	if err == sql.ErrNoRows || err == nil && !o.btcEscrow() {
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
