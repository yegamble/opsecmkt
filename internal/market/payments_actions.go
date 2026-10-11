package market

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
)

func init() {
	registerLoader("*", paymentsGlobalLoader)
	registerLoader("order", paymentsOrderLoader)
	registerLoader("account", paymentsAccountLoader)
	registerLoader("vendor-dashboard", unsentPayoutsLoader)
	registerLoader("admin", paymentsAdminLoader)
	registerPreview("admin", func(d *PageData) {
		d.Providers = nil
	})
}

func paymentsGlobalLoader(_ context.Context, a *App, _ *http.Request, d *PageData) error {
	d.PaymentsEnabled = false
	d.PaymentNetworks = ""
	d.LiveNetwork = a.live
	d.BitcoinPriceLabel = a.bitcoinPriceLabel()
	d.EscrowOn, d.EscrowAvailable = a.escrowConfigured, a.escrow.Load() != nil
	if a.escrowNetKnown() {
		d.EscrowNet = a.escrowNetwork()
	}
	d.DispatchOn = !physicalEscrowOff(a)
	d.XMREscrowOn, d.XMREscrowAvailable, d.XMRCustody, d.XMREscrowNet = a.escrowXMR != nil, a.xmrEscrowReady(), xmrCustody, a.xmrEscrowNetwork()
	d.XMREscrowNetKnown = a.escrowXMR != nil && a.escrowXMR.formNet() != ""
	return nil
}

// paymentsOrderLoader builds PaymentView only from payment_addresses and the payments ledger.
// load.go has already restricted the order to its buyer, its vendor and staff reviewers (staffReviewer).
func paymentsOrderLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.Order == nil {
		return nil
	}
	o := d.Order
	pv := &PaymentView{Currency: o.Currency, Required: o.Amount, Testnet: !a.live}
	d.Payment = pv
	dec := currencyDecimals(o.Currency)
	// The payouts are read before the provider checks: each recipient learns what happens next even while the
	// wallet is unavailable (A-120). Only the recipient sees a payout's destination and the reason it waits; the
	// other party and staff reviewers see a neutral label (the full detail is on /admin). Only the recipient and
	// staff reviewers see its transaction ID (A-214). An order has a release and a return of any excess, or a
	// refund: each viewer is the recipient of at most one of them.
	var payouts []PayoutRow
	rows, err := a.db.QueryContext(ctx, "SELECT kind,state,txid,amount,currency,user_id,address,error,send_ambiguous,"+stuckSending+","+conflictedDeposit+" FROM payouts WHERE order_id=$1 ORDER BY id", o.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var row PayoutRow
		var amt int64
		var recipient, payoutErr string
		var ambiguous, stuck, conflicted bool
		if err = rows.Scan(&row.Kind, &row.State, &row.TxID, &amt, &row.Currency, &recipient, &row.Address, &payoutErr, &ambiguous, &stuck, &conflicted); err != nil {
			rows.Close()
			return err
		}
		row.Amount, row.StateLabel = amount(amt, dec), orderPayoutLabel(row.State, payoutErr, conflicted)
		if d.User != nil && d.User.ID == recipient {
			if row.State != "sent" {
				mine := recipientPayout(o.ID, row.Kind, row.Currency, pv.Network, row.Amount, row.Address, row.State, payoutErr, ambiguous, stuck, conflicted)
				row.StateLabel, d.MyPayout = mine.Status, &mine
			}
		} else {
			row.Address = ""
			// On Bitcoin the txid reveals the destination: the other party never sees it; staff reviewers do (A-214).
			if d.User != nil && (d.User.ID == o.BuyerID || d.User.ID == o.VendorID) {
				row.TxID = ""
			}
		}
		payouts = append(payouts, row)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var issued bool
	err = a.db.QueryRowContext(ctx, "SELECT true FROM payment_addresses WHERE order_id=$1", o.ID).Scan(&issued)
	if err == sql.ErrNoRows {
		pv.Payouts = payouts
		return nil
	}
	if err != nil {
		return err
	}
	pv.Issued = true
	pv.Status = "Historical pooled payment. Automated monitoring and payouts have been retired."
	pv.Payouts = payouts
	// Preserve the recorded deposit ledger without presenting its address as a payment request.
	rows, err = a.db.QueryContext(ctx, `SELECT txid,idx,amount,confirmations,credited,locked,disposition,
		COALESCE(to_char(disposed_at,'YYYY-MM-DD'),'') FROM payments WHERE order_id=$1 ORDER BY created,id`, o.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var dep PaymentDeposit
		var atomic int64
		var credited, locked bool
		var disposition, disposed string
		if err := rows.Scan(&dep.TxID, &dep.Index, &atomic, &dep.Confirmations, &credited, &locked, &disposition, &disposed); err != nil {
			return err
		}
		if d.User != nil && d.User.ID == o.VendorID {
			dep.TxID = ""
		}
		dep.Amount = amount(atomic, dec)
		switch {
		case disposition == "refunded":
			dep.State = "Refunded by hand on " + disposed
		case locked:
			dep.State = "Locked transfer — not counted"
		case dep.Confirmations < 0:
			dep.State = "Conflicted or missing"
		case credited:
			dep.State = "Counted in historical settlement"
		default:
			dep.State = "Recorded; not counted in settlement"
		}
		pv.Deposits = append(pv.Deposits, dep)
	}
	return rows.Err()
}

func payoutStateLabel(s string) string {
	switch s {
	case "blocked":
		return "Blocked — recipient has no payout address"
	case "held":
		return "Held — deposit conflicted or re-confirming"
	case "pending":
		return "Queued"
	case "sending":
		return "Sending (single attempt)"
	case "sent":
		return "Sent"
	case "failed":
		return "Failed — not retried"
	}
	return s
}

// conflictedDeposit selects, for a payouts row, whether a credited deposit of its order is conflicted or missing
// from the wallet (confirmations < 0): the watcher's hold then waits for market staff, since a double-spent
// deposit never confirms again (A-213).
const conflictedDeposit = "EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=payouts.order_id AND pm.credited AND pm.confirmations<0)"

// orderPayoutLabel is the payout status on the order page for everyone but its recipient: the other party and
// staff reviewers. A hold other than the watcher's (whose deposit the payment panel already shows) is not
// explained there, so neither a suspension nor a restore from backup is disclosed (A-120, A-107, A-112).
// conflicted: a credited deposit of the order is conflicted or missing (conflictedDeposit).
func orderPayoutLabel(state, payoutErr string, conflicted bool) string {
	if state == "held" && payoutErr != heldReason {
		return "Held — waiting for an administrator check"
	}
	if state == "held" && conflicted {
		return "Held — a deposit is conflicted or missing; market staff review it"
	}
	if state == "held" {
		return "Held — historical deposit confirmation needs operator review"
	}
	return payoutStateLabel(state)
}

// recipientPayout describes an unsent payout to its recipient (A-120) in fixed copy: why it waits and what
// happens next. payoutErr (the wallet's text or the hold's marker) only selects the copy and is never shown.
// stuck: a send claimed longer ago than stuckSending allows, which may have been broadcast like an ambiguous
// failure. A definite failure says administrators were notified: recordPayout notifies every administrator in
// the transaction that records it. conflicted: a credited deposit of the order is conflicted or missing
// (conflictedDeposit), so the watcher's hold is not described as lifting by itself (A-213).
func recipientPayout(orderID, kind, cur, network, amt, address, state, payoutErr string, ambiguous, stuck, conflicted bool) RecipientPayout {
	r := RecipientPayout{OrderID: orderID, OrderShort: shortID(orderID), Kind: kind, Currency: cur, Network: network, Amount: amt, Address: address}
	r.Status = payoutStateLabel(state)
	r.NextStep = "This is a historical record. Contact the operator to investigate its recorded state."
	return r
}

// unsentPayouts lists every payout to userID that is not sent, oldest first and uncapped (A-120).
func unsentPayouts(ctx context.Context, a *App, userID string) ([]RecipientPayout, error) {
	rows, err := a.db.QueryContext(ctx, "SELECT id,order_id,kind,currency,amount,address,state,error,send_ambiguous,"+stuckSending+","+conflictedDeposit+
		" FROM payouts WHERE user_id=$1 AND state<>'sent' ORDER BY id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecipientPayout
	for rows.Next() {
		var order, kind, cur, address, state, payoutErr, network string
		var payoutID int64
		var amt int64
		var ambiguous, stuck, conflicted bool
		if err = rows.Scan(&payoutID, &order, &kind, &cur, &amt, &address, &state, &payoutErr, &ambiguous, &stuck, &conflicted); err != nil {
			return nil, err
		}
		network = "historical"
		payout := recipientPayout(order, kind, cur, network, amount(amt, currencyDecimals(cur)), address, state, payoutErr, ambiguous, stuck, conflicted)
		payout.ID = payoutID
		out = append(out, payout)
	}
	return out, rows.Err()
}

// unsentPayoutsLoader counts the viewer's unsent payouts for the vendor desk, which links to the list.
func unsentPayoutsLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	if err := a.db.QueryRowContext(ctx, "SELECT count(*) FROM payouts WHERE user_id=$1 AND state<>'sent'", d.User.ID).Scan(&d.UnsentPayouts); err != nil {
		return err
	}
	// The viewer's escrow orders (as buyer or vendor), whatever ESCROW_BTC says now (QA31-2): all but a cancelled one
	// with nothing recorded, and that one too while escrow is not enabled, since its deposits are then not read.
	if err := a.db.QueryRowContext(ctx, `SELECT count(*) FROM escrows es JOIN orders o ON o.id=es.order_id JOIN products p ON p.id=o.product_id
		WHERE es.coin='BTC' AND (o.buyer_id=$1 OR p.vendor_id=$1) AND ($2 OR o.state<>'cancelled'
			OR EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id AND pm.confirmations>=0 AND NOT pm.locked AND pm.settlement_id IS NULL))`,
		d.User.ID, a.escrow.Load() == nil).Scan(&d.EscrowOrders); err != nil {
		return err
	}
	// The same for the viewer's Monero escrow orders (A-423): all but a cancelled one with no deposit recorded.
	return a.db.QueryRowContext(ctx, `SELECT count(*) FROM escrows es JOIN orders o ON o.id=es.order_id JOIN products p ON p.id=o.product_id
		WHERE es.coin='XMR' AND (o.buyer_id=$1 OR p.vendor_id=$1) AND (o.state<>'cancelled'
			OR EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id AND pm.confirmations>=0 AND NOT pm.locked AND pm.xmr_settlement_id IS NULL))`,
		d.User.ID).Scan(&d.XMREscrowOrders)
}

func paymentsAccountLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	v := &PayoutView{}
	if err := a.db.QueryRowContext(ctx, "SELECT payout_btc,payout_xmr,(SELECT count(*) FROM payouts WHERE user_id=$1 AND state='blocked') FROM users WHERE id=$1", d.User.ID).Scan(&v.BTC, &v.XMR, &v.Blocked); err != nil {
		return err
	}
	unsent, err := unsentPayouts(ctx, a, d.User.ID)
	if err != nil {
		return err
	}
	v.Unsent = unsent
	d.Payout = v
	return nil
}

// payoutHistoryLimit caps the payouts on /admin that need no attention. Payouts needing attention are
// never capped: their resolve forms exist only there.
const payoutHistoryLimit = 50

func paymentsAdminLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	// One statement, so now() (and so the attention flag) is the same for both halves: every payout needing
	// attention (oldest first; its resolve form is only on this page), then the most recent others up to
	// payoutHistoryLimit. OrderLink: the order page opens for this administrator (a party, or a disputed,
	// resolved or payment-flagged order read as a reviewer; see staffReviewer).
	uid := ""
	if d.User != nil {
		uid = d.User.ID
	}
	rows, err := a.db.QueryContext(ctx, `WITH v AS (SELECT p.id,p.order_id,p.kind,u.handle,p.currency,p.amount,p.address,p.state,p.txid,p.error,p.updated AS updated_sort,to_char(p.updated,'YYYY-MM-DD HH24:MI "UTC"') AS updated,
		`+payoutNeedsAttention("p")+` AS attention,p.send_ambiguous,u.suspended_at IS NOT NULL AS recipient_suspended,
		(o.buyer_id=$1 OR pr.vendor_id=$1 OR EXISTS(SELECT 1 FROM disputes d WHERE d.order_id=o.id) OR EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id AND pm.flagged)) AS order_link,
		CASE p.currency WHEN 'XMR' THEN u.payout_xmr ELSE u.payout_btc END AS current_address,
		COALESCE(to_char(CASE p.currency WHEN 'XMR' THEN u.payout_xmr_changed ELSE u.payout_btc_changed END,'YYYY-MM-DD HH24:MI "UTC"'),'') AS current_changed
		FROM payouts p JOIN users u ON u.id=p.user_id JOIN orders o ON o.id=p.order_id JOIN products pr ON pr.id=o.product_id)
		SELECT id,order_id,kind,handle,currency,amount,address,state,txid,error,updated,attention,send_ambiguous,recipient_suspended,order_link,current_address,current_changed
		FROM (SELECT * FROM v WHERE attention OR state='pending' UNION ALL (SELECT * FROM v WHERE NOT attention AND state<>'pending' ORDER BY updated_sort DESC,id DESC LIMIT $2)) s
		ORDER BY attention DESC, CASE WHEN attention THEN id END, CASE WHEN NOT attention THEN updated_sort END DESC, id DESC`, uid, payoutHistoryLimit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	others := 0
	for rows.Next() {
		var r PayoutRow
		var amt int64
		var sendAmbiguous, recipientSuspended bool
		if err = rows.Scan(&r.ID, &r.OrderID, &r.Kind, &r.Recipient, &r.Currency, &amt, &r.Address, &r.State, &r.TxID, &r.Error, &r.Updated, &r.Attention, &sendAmbiguous, &recipientSuspended, &r.OrderLink, &r.CurrentAddress, &r.CurrentChanged); err != nil {
			return err
		}
		if r.Attention {
			d.PayoutsAttention++
		} else if r.State != "pending" {
			others++
			if others > payoutHistoryLimit {
				d.PayoutHistoryLimit = payoutHistoryLimit
				continue
			}
		}
		r.Amount, r.StateLabel, r.OrderShort = amount(amt, currencyDecimals(r.Currency)), payoutStateLabel(r.State), shortID(r.OrderID)
		r.Repoint = false
		r.RequeueAddressCheck = (r.State == "failed" || r.State == "sending" && r.Attention) && false
		switch {
		case r.State == "sending" && r.Attention:
			r.StateLabel, r.Ambiguous = "Stuck in sending — never retried; may have been broadcast, check the wallet", true
		case r.State == "failed" && strings.HasPrefix(r.Error, restoredHoldPrefix):
			r.StateLabel, r.Ambiguous = "Failed after a restore from backup — may have been requeued and sent after the backup, check the wallet", true
		case r.State == "failed" && sendAmbiguous:
			r.StateLabel, r.Ambiguous = r.StateLabel+"; outcome unknown, may have been broadcast", true
		case r.State == "failed":
			r.StateLabel += "; the wallet reported a pre-broadcast error, nothing broadcast"
		case r.State == "held" && strings.HasPrefix(r.Error, restoredHoldPrefix):
			r.StateLabel, r.Ambiguous = "Held after a restore from backup — may already have been sent, check the wallet", true
		}
		if r.RequeueAddressCheck {
			r.StateLabel += "; payout address check required before requeue"
		}
		if heldForSuspension(r.State, r.Error) {
			r.StateLabel, r.AddressCheck = "Held: account suspended — check the payout address before releasing", true
			if r.Ambiguous {
				r.StateLabel = "Held after a restore from backup and for an account suspension — may already have been sent; check the wallet and the payout address before releasing"
			}
		}
		if heldForHand(r.State, r.Error) {
			r.StateLabel, r.HandHold = "Held to be paid by hand — record it sent once paid, or release it", true
			switch {
			case r.AddressCheck:
				r.StateLabel = "Held to be paid by hand and for an account suspension — record it sent once paid, or check the payout address before releasing it"
			case r.Ambiguous:
				r.StateLabel = "Held to be paid by hand, then restored from backup — may already have been paid or sent; check the wallet before releasing it"
			}
		}
		if heldLarge(r.State, r.Error) {
			r.LargeCheck = true
			if !r.AddressCheck && !r.Ambiguous {
				r.StateLabel = "Held: larger than the order price — review before releasing"
			}
		}
		r.Error = "Historical wallet error; ask the operator to inspect the original record."
		d.Payouts = append(d.Payouts, r)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	// A payout the watcher holds offers no release while a credited deposit of its order is still below the
	// threshold (the release would be refused): the row names the deposits instead.
	for i := range d.Payouts {
		r := &d.Payouts[i]
		if r.State != "held" || r.Error != heldReason {
			continue
		}
		r.Threshold = 0
		deps, err := a.db.QueryContext(ctx, "SELECT txid,idx,confirmations,address FROM payments WHERE order_id=$1 AND credited AND confirmations<$2 ORDER BY id", r.OrderID, r.Threshold)
		if err != nil {
			return err
		}
		for deps.Next() {
			var h HeldDeposit
			if err = deps.Scan(&h.TxID, &h.Index, &h.Confirmations, &h.Address); err != nil {
				deps.Close()
				return err
			}
			r.Waiting = append(r.Waiting, h)
		}
		deps.Close()
		if err = deps.Err(); err != nil {
			return err
		}
		// Keep heldReason as the stored watcher-recognition key (including historical rows), but display
		// conditional recovery guidance instead of its old promise that every conflicted deposit returns.
		r.Error = "A credited deposit is conflicted or below the confirmation threshold; automatic payment monitoring has retired. The operator must reconcile the historical wallet and recorded payout state."
	}
	return nil
}
