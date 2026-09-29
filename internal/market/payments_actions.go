package market

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"strings"
)

func init() {
	registerAction("/account/payout", actionSpec{OwnTx: true, Run: payoutAddressAction})
	registerLoader("*", paymentsGlobalLoader)
	registerLoader("order", paymentsOrderLoader)
	registerLoader("account", paymentsAccountLoader)
	registerLoader("vendor-dashboard", unsentPayoutsLoader)
	registerLoader("admin", paymentsAdminLoader)
}

// payoutAddressAction saves (or clears) the user's payout address for one currency. Saving requires a live
// provider and an address valid for its network; blocked payouts for that currency are then released.
// Every change needs the current password (and a TOTP code when enrolled), checked after the cheap validation,
// and notifies the account.
func payoutAddressAction(c *actionCtx) (actionResult, error) {
	cur := c.Form.Get("currency")
	if cur != "BTC" && cur != "XMR" {
		return actionResult{}, fail(400, "Choose BTC or XMR")
	}
	addr := strings.TrimSpace(c.Form.Get("address"))
	p := c.A.provider(cur)
	if addr != "" {
		if p == nil {
			return actionResult{}, fail(409, cur+" payments are not configured on this market, so a payout address cannot be saved.")
		}
		if len(addr) > 128 || !p.ValidAddress(addr) {
			return actionResult{}, fail(400, payoutAddressHint(cur, p.Network()))
		}
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) { return savePayoutAddress(c, cur, addr, p) })
}

func savePayoutAddress(c *actionCtx, cur, addr string, p PaymentProvider) (actionResult, error) {
	column := "payout_btc"
	if cur == "XMR" {
		column = "payout_xmr"
	}
	ctx := c.Ctx()
	// The change time is shown next to the address when an administrator moves a payout to it (A-121).
	if _, err := c.Tx.ExecContext(ctx, "UPDATE users SET "+column+"=$1,"+column+"_changed=CASE WHEN "+column+"=$1 THEN "+column+"_changed ELSE now() END WHERE id=$2", addr, c.User.ID); err != nil {
		return actionResult{}, err
	}
	audit := "Removed " + cur + " payout address"
	note := "Your " + cur + " payout address was removed"
	if addr != "" {
		// Blocked payouts become pending; held payouts that had no address keep their hold but gain the address.
		res, err := c.Tx.ExecContext(ctx, `UPDATE payouts SET address=$1,state=CASE WHEN state='blocked' THEN 'pending' ELSE state END,
			error=CASE WHEN state='blocked' THEN '' ELSE error END,updated=now()
			WHERE user_id=$2 AND currency=$3 AND (state='blocked' OR (state='held' AND address=''))`, addr, c.User.ID, cur)
		if err != nil {
			return actionResult{}, err
		}
		n, _ := res.RowsAffected()
		audit = "Saved " + cur + " payout address (" + c.A.netTag() + " " + p.Network() + ")"
		note = "Your " + cur + " payout address was changed"
		if n > 0 {
			audit += "; " + strconv.FormatInt(n, 10) + " waiting payout(s) now use it"
			note += "; " + strconv.FormatInt(n, 10) + " waiting payout(s) now use it"
		}
		// An unsent payout that already has an address keeps it (A-121): someone who knows the password must not
		// re-point money already on its way. The owner learns which orders still use a previous address; an
		// administrator moves a held or definitely rejected one after checking the new address with them.
		movable, others, err := previousAddressPayouts(ctx, c.Tx, c.User.ID, cur, addr)
		if err != nil {
			return actionResult{}, err
		}
		if len(movable) > 0 {
			note += "; " + strconv.Itoa(len(movable)) + " unsent payout(s) still use your previous address (" + ordersList(movable) + "); an administrator must confirm the change"
		}
		if len(others) > 0 {
			note += "; " + strconv.Itoa(len(others)) + " other unsent payout(s) are queued, being sent or may already have been sent, and stay on your previous address (" + ordersList(others) + ")"
		}
	}
	// The owner hears of every change, so one made by someone who knows the password does not go unseen (A-102).
	// The address itself is not repeated.
	note += ". If you did not make this change, someone else may know your password: change it and contact the market staff."
	if _, err := c.Tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), c.User.ID, note); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/account?saved=1", Audit: audit}, nil
}

// previousAddressPayouts returns the short order IDs of the user's unsent payouts in cur that use an address other
// than addr, oldest first: those an administrator may move (repointable), and the others (queued, being sent, or
// possibly broadcast).
func previousAddressPayouts(ctx context.Context, tx *sql.Tx, userID, cur, addr string) (movable, others []string, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT order_id,state,error,send_ambiguous FROM payouts WHERE user_id=$1 AND currency=$2
		AND address<>'' AND address<>$3 AND state IN ('held','failed','pending','sending') ORDER BY id`, userID, cur, addr)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var order, state, payoutErr string
		var ambiguous bool
		if err = rows.Scan(&order, &state, &payoutErr, &ambiguous); err != nil {
			return nil, nil, err
		}
		if repointable(state, payoutErr, ambiguous) {
			movable = append(movable, shortID(order))
		} else {
			others = append(others, shortID(order))
		}
	}
	return movable, others, rows.Err()
}

// ordersList names short order IDs in a notification: "order 1a2b3c4d" or "orders 1a2b3c4d, 5e6f7a8b".
func ordersList(ids []string) string {
	if len(ids) == 1 {
		return "order " + ids[0]
	}
	return "orders " + strings.Join(ids, ", ")
}

// payoutAddressHint is the error for a payout address that is not valid for network.
func payoutAddressHint(cur, network string) string {
	if isLiveNetwork(network) {
		return "Enter a valid " + cur + " mainnet address. Test-network addresses are rejected."
	}
	return "Enter a valid " + cur + " address for the " + network + " test network. Mainnet addresses are rejected."
}

func paymentsGlobalLoader(_ context.Context, a *App, _ *http.Request, d *PageData) error {
	var nets []string
	ready := a.providers()
	for _, cur := range sortedCurrencies(ready) {
		nets = append(nets, cur+" "+ready[cur].Network())
	}
	d.PaymentsEnabled = len(nets) > 0
	d.PaymentNetworks = strings.Join(nets, ", ")
	d.LiveNetwork = a.live
	return nil
}

// paymentsOrderLoader builds PaymentView only from payment_addresses and the payments ledger.
// load.go has already restricted the order to its buyer, its vendor and staff reviewers (staffReviewer).
func paymentsOrderLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.Order == nil {
		return nil
	}
	o := d.Order
	p := a.provider(o.Currency)
	pv := &PaymentView{Currency: o.Currency, Required: o.Amount, Testnet: !a.live, Available: p != nil}
	if p == nil {
		msg, _ := a.unavailableError(o.Currency)
		pv.Degraded = msg != ""
	}
	d.Payment = pv
	if p != nil {
		pv.Network, pv.Threshold = p.Network(), p.Confirmations()
	}
	dec := currencyDecimals(o.Currency)
	// The payouts are read before the provider checks: each recipient learns what happens next even while the
	// wallet is unavailable (A-120). Only the recipient sees a payout's destination and the reason it waits; the
	// other party and staff reviewers see a neutral label (the full detail is on /admin). An order has a release
	// and a return of any excess, or a refund: each viewer is the recipient of at most one of them.
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
		}
		payouts = append(payouts, row)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var addr string
	err = a.db.QueryRowContext(ctx, "SELECT address FROM payment_addresses WHERE order_id=$1", o.ID).Scan(&addr)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	pv.Issued = true
	pv.Monitored = p != nil
	if !pv.Monitored {
		pv.Status = "Not monitored: " + o.Currency + " payments are not configured on this server."
		if pv.Degraded {
			pv.Status = "Not monitored right now: the " + o.Currency + " " + a.netAdj() + " wallet is unavailable."
		}
		return nil
	}
	pv.Open = o.State == stateAwaitingPayment
	if pv.Open {
		pv.Address = addr
	}
	rows, err = a.db.QueryContext(ctx, "SELECT txid,idx,amount,confirmations,credited,locked,regress_notice FROM payments WHERE order_id=$1 ORDER BY created,id", o.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var received, pending int64
	required, err := parseAmount(o.Amount, dec)
	if err != nil {
		return err
	}
	remaining := required
	minConfs := int64(-1)
	// conflicted/regressed: a credited deposit is conflicted, or back below the threshold; notified: that
	// regression was announced to the parties and staff (settleOrder).
	conflicted, regressed, notified := false, false, false
	for rows.Next() {
		var dep PaymentDeposit
		var amt int64
		var credited, locked, notice bool
		if err = rows.Scan(&dep.TxID, &dep.Index, &amt, &dep.Confirmations, &credited, &locked, &notice); err != nil {
			return err
		}
		if !locked && dep.Confirmations >= 0 {
			remaining -= min(remaining, amt)
		}
		dep.Amount = amount(amt, dec)
		switch {
		case locked:
			dep.State = "Locked transfer — not counted"
		case dep.Confirmations < 0:
			dep.State = "Conflicted or missing — not counted"
			conflicted = conflicted || credited
			notified = notified || (credited && notice)
		case dep.Confirmations >= int64(pv.Threshold):
			dep.State = "Confirmed"
			received += amt
		default:
			dep.State = "Waiting for confirmations"
			pending += amt
			regressed = regressed || credited
			notified = notified || (credited && notice)
		}
		if !locked && dep.Confirmations >= 0 && (minConfs < 0 || dep.Confirmations < minConfs) {
			minConfs = dep.Confirmations
		}
		pv.Deposits = append(pv.Deposits, dep)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	pv.Received, pv.Unconfirmed = amount(received, dec), amount(pending, dec)
	pv.Remaining, pv.NeedsTopUp = amount(remaining, dec), remaining > 0
	if remaining == 0 {
		pv.Remaining = "0"
	}
	// The payment request and its QR code ask for exactly what is still due (deposits seen, confirmed or not,
	// are already subtracted), and only while something is.
	if pv.Open && pv.NeedsTopUp {
		pv.PaymentURI = paymentURI(o.Currency, pv.Address, remaining)
		if pv.QR, err = qrSVG(pv.PaymentURI, "QR code: pay "+pv.Remaining+" "+o.Currency+" to the deposit address"); err != nil {
			log.Printf("payments: payment QR code not rendered: %v", err)
			pv.QR = ""
		}
	}
	if received == 0 {
		pv.Received = "0"
	}
	if pending == 0 {
		pv.Unconfirmed = "0"
	}
	if minConfs > 0 {
		pv.Confirmations = int(minConfs)
	}
	switch {
	case conflicted || regressed:
		pv.Status = "A credited deposit is back below " + strconv.Itoa(pv.Threshold) + " confirmations; unsent payouts wait until it confirms again"
		if conflicted {
			pv.Status = "A credited deposit is conflicted or missing; unsent payouts wait until it confirms again"
		}
		if notified {
			pv.Status += "; market staff have been notified"
		}
	case o.State == stateAwaitingPayment && len(pv.Deposits) == 0:
		pv.Status = "Waiting for a deposit"
	case o.State == stateAwaitingPayment && pending > 0:
		pv.Status = "Deposit seen; waiting for " + strconv.Itoa(pv.Threshold) + " confirmations"
	case o.State == stateAwaitingPayment:
		pv.Status = "Confirmed deposits are below the required amount"
	case o.State == stateDraft:
		pv.Status = "Not requested"
	default:
		pv.Status = "Deposit address closed (order " + stateLabel(o.State) + ")"
	}
	pv.Payouts = payouts
	return nil
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
		return "Held — a deposit is re-confirming; released automatically once it confirms again"
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
	restored := strings.HasPrefix(payoutErr, restoredHoldPrefix)
	switch {
	case state == "blocked":
		r.Status, r.NextStep = "Waiting for your payout address", "Save a "+cur+" payout address on your account page; the payout is then queued and sent automatically."
	case state == "failed" && !ambiguous && !restored:
		r.Status, r.NextStep = "Failed — nothing was sent", "The market wallet refused the send before broadcasting; administrators have been notified. It is never retried automatically: only an administrator can queue it again."
	case state == "failed" || stuck:
		r.Status, r.NextStep = "Outcome unknown", "The send may have been broadcast; an administrator checks the wallet before any resend, so it is not paid twice."
	case heldForSuspension(state, payoutErr):
		r.Status, r.NextStep = "Held for a payout address check", "Held because your account was suspended; an administrator checks your payout address before releasing it."
	case state == "held" && restored:
		r.Status, r.NextStep = "Held for a wallet check", "Held after the market restored its records from a backup; an administrator checks the market wallet before releasing it, so it is not paid twice."
	case heldLarge(state, payoutErr):
		r.Status, r.NextStep = "Held for review", "It is more than the order price, so an administrator reviews it before it is sent."
	case state == "held" && payoutErr == heldReason && conflicted:
		r.Status, r.NextStep = "Held for review", "A deposit for this order is conflicted or missing from the market wallet, so this payout is held and market staff review it."
	case state == "held" && payoutErr == heldReason:
		r.Status, r.NextStep = "Held until a deposit confirms", "A deposit for this order is re-confirming; the payment watcher releases the payout by itself once it confirms again."
	case state == "held":
		r.Status, r.NextStep = "Held for an administrator check", "An administrator reviews it before it is released."
	case state == "sending":
		r.Status, r.NextStep = "Being sent", "The market wallet is sending it now, in a single attempt."
	default:
		r.Status, r.NextStep = "Queued", "The payment watcher sends it automatically, in a single attempt."
	}
	if address == "" && state == "held" {
		r.NextStep += " No " + cur + " payout address is saved yet: save one on your account page so it can be paid."
	}
	return r
}

// unsentPayouts lists every payout to userID that is not sent, oldest first and uncapped (A-120).
func unsentPayouts(ctx context.Context, a *App, userID string) ([]RecipientPayout, error) {
	rows, err := a.db.QueryContext(ctx, "SELECT order_id,kind,currency,amount,address,state,error,send_ambiguous,"+stuckSending+","+conflictedDeposit+
		" FROM payouts WHERE user_id=$1 AND state<>'sent' ORDER BY id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecipientPayout
	for rows.Next() {
		var order, kind, cur, address, state, payoutErr, network string
		var amt int64
		var ambiguous, stuck, conflicted bool
		if err = rows.Scan(&order, &kind, &cur, &amt, &address, &state, &payoutErr, &ambiguous, &stuck, &conflicted); err != nil {
			return nil, err
		}
		if p := a.provider(cur); p != nil {
			network = p.Network()
		}
		out = append(out, recipientPayout(order, kind, cur, network, amount(amt, currencyDecimals(cur)), address, state, payoutErr, ambiguous, stuck, conflicted))
	}
	return out, rows.Err()
}

// unsentPayoutsLoader counts the viewer's unsent payouts for the vendor desk, which links to the list.
func unsentPayoutsLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	return a.db.QueryRowContext(ctx, "SELECT count(*) FROM payouts WHERE user_id=$1 AND state<>'sent'", d.User.ID).Scan(&d.UnsentPayouts)
}

func paymentsAccountLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	v := &PayoutView{}
	if p := a.provider("BTC"); p != nil {
		v.BTCNetwork = p.Network()
	}
	if p := a.provider("XMR"); p != nil {
		v.XMRNetwork = p.Network()
	}
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

func paymentsAdminLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	status := map[string]ProviderStatus{}
	rows, err := a.db.QueryContext(ctx, "SELECT currency,network,COALESCE(to_char(last_poll,'YYYY-MM-DD HH24:MI:SS \"UTC\"'),''),last_error FROM payment_status")
	if err != nil {
		return err
	}
	for rows.Next() {
		var s ProviderStatus
		if err = rows.Scan(&s.Currency, &s.Network, &s.LastPoll, &s.Error); err != nil {
			rows.Close()
			return err
		}
		status[s.Currency] = s
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	env := map[string]string{"BTC": "BITCOIN_RPC_URL", "XMR": "MONERO_WALLET_RPC_URL"}
	for _, cur := range []string{"BTC", "XMR"} {
		s := ProviderStatus{Currency: cur, Status: "Disabled"}
		if p := a.provider(cur); p != nil {
			last := status[cur]
			s.Enabled, s.Status, s.Network, s.Confirmations = true, "Enabled", p.Network(), p.Confirmations()
			s.LastPoll, s.Error = last.LastPoll, last.Error
			if s.LastPoll == "" {
				s.LastPoll = "Not polled yet"
			}
			if v, ok := p.(versionReporter); ok {
				var unverified bool
				if s.Version, unverified = v.WalletVersion(); unverified {
					s.VersionWarning = "The rule that a failed " + cur + " send broadcast nothing was verified against monero-wallet-rpc " + moneroVerifiedWalletRelease + ", which reports " + moneroRPCVersion(moneroVerifiedWalletRPC, true) + ". Until the wallet reports that, every " + cur + " send failure is recorded as possibly broadcast, and requeueing one needs the wallet check. That refunds and overpayment returns take the network fee out of the amount sent (subtract_fee_from_outputs) is also unverified for this version. Set MONERO_WALLET_IMAGE and MONERO_IMAGE in .env to the " + moneroVerifiedWalletRelease + " tags, or follow the bump checklist before running another wallet version:"
				}
			}
		} else if msg, disabled := a.unavailableError(cur); msg != "" {
			// Configured but failing its checks in this process: payment requests are refused.
			s.Status, s.Error = "Unavailable — retried every poll", msg
			if disabled {
				s.Status = "Refused — not a test network"
				if a.live {
					s.Status = "Refused — not mainnet"
				}
			}
		} else {
			s.Error = "Not configured (" + env[cur] + " is blank)"
		}
		d.Providers = append(d.Providers, s)
	}
	// One statement, so now() (and so the attention flag) is the same for both halves: every payout needing
	// attention (oldest first; its resolve form is only on this page), then the most recent others up to
	// payoutHistoryLimit. OrderLink: the order page opens for this administrator (a party, or a disputed,
	// resolved or payment-flagged order read as a reviewer; see staffReviewer).
	uid := ""
	if d.User != nil {
		uid = d.User.ID
	}
	rows, err = a.db.QueryContext(ctx, `WITH v AS (SELECT p.id,p.order_id,p.kind,u.handle,p.currency,p.amount,p.address,p.state,p.txid,p.error,to_char(p.updated,'YYYY-MM-DD HH24:MI "UTC"') AS updated,
		(p.state IN ('blocked','held','failed') OR (p.state='sending' AND p.updated < now()-interval '5 minutes')) AS attention,p.send_ambiguous,
		(o.buyer_id=$1 OR pr.vendor_id=$1 OR o.state IN ('disputed','resolved') OR EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id AND pm.flagged)) AS order_link,
		CASE p.currency WHEN 'XMR' THEN u.payout_xmr ELSE u.payout_btc END AS current_address,
		COALESCE(to_char(CASE p.currency WHEN 'XMR' THEN u.payout_xmr_changed ELSE u.payout_btc_changed END,'YYYY-MM-DD HH24:MI "UTC"'),'') AS current_changed
		FROM payouts p JOIN users u ON u.id=p.user_id JOIN orders o ON o.id=p.order_id JOIN products pr ON pr.id=o.product_id)
		SELECT * FROM (SELECT * FROM v WHERE attention UNION ALL (SELECT * FROM v WHERE NOT attention ORDER BY id DESC LIMIT $2)) s
		ORDER BY attention DESC, CASE WHEN attention THEN id END, id DESC`, uid, payoutHistoryLimit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	others := 0
	for rows.Next() {
		var r PayoutRow
		var amt int64
		var sendAmbiguous bool
		if err = rows.Scan(&r.ID, &r.OrderID, &r.Kind, &r.Recipient, &r.Currency, &amt, &r.Address, &r.State, &r.TxID, &r.Error, &r.Updated, &r.Attention, &sendAmbiguous, &r.OrderLink, &r.CurrentAddress, &r.CurrentChanged); err != nil {
			return err
		}
		if r.Attention {
			d.PayoutsAttention++
		} else if others++; others > payoutHistoryLimit {
			d.PayoutHistoryLimit = payoutHistoryLimit
			continue
		}
		r.Amount, r.StateLabel, r.OrderShort = amount(amt, currencyDecimals(r.Currency)), payoutStateLabel(r.State), shortID(r.OrderID)
		r.Repoint = repointable(r.State, r.Error, sendAmbiguous) && r.Address != "" && r.CurrentAddress != "" && r.CurrentAddress != r.Address
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
		if heldForSuspension(r.State, r.Error) {
			r.StateLabel, r.AddressCheck = "Held: account suspended — check the payout address before releasing", true
			if r.Ambiguous {
				r.StateLabel = "Held after a restore from backup and for an account suspension — may already have been sent; check the wallet and the payout address before releasing"
			}
		}
		if heldLarge(r.State, r.Error) {
			r.LargeCheck = true
			if !r.AddressCheck && !r.Ambiguous {
				r.StateLabel = "Held: larger than the order price — review before releasing"
			}
		}
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
		r.Threshold = a.confirmationThreshold(r.Currency)
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
	}
	return nil
}
