package market

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
)

func init() {
	registerAction("/account/payout", actionSpec{OwnTx: true, Run: payoutAddressAction})
	registerLoader("*", paymentsGlobalLoader)
	registerLoader("order", paymentsOrderLoader)
	registerLoader("account", paymentsAccountLoader)
	registerLoader("admin", paymentsAdminLoader)
}

// payoutAddressAction saves (or clears) the user's payout address for one currency. Saving requires a live
// provider and an address valid for its test network; blocked payouts for that currency are then released.
// Every change needs the current password (and a TOTP code when enrolled), checked after the cheap validation.
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
			return actionResult{}, fail(400, "Enter a valid "+cur+" address for the "+p.Network()+" test network. Mainnet addresses are rejected.")
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
	if _, err := c.Tx.ExecContext(ctx, "UPDATE users SET "+column+"=$1 WHERE id=$2", addr, c.User.ID); err != nil {
		return actionResult{}, err
	}
	audit := "Removed " + cur + " payout address"
	if addr != "" {
		// Blocked payouts become pending; held payouts that had no address keep their hold but gain the address.
		res, err := c.Tx.ExecContext(ctx, `UPDATE payouts SET address=$1,state=CASE WHEN state='blocked' THEN 'pending' ELSE state END,
			error=CASE WHEN state='blocked' THEN '' ELSE error END,updated=now()
			WHERE user_id=$2 AND currency=$3 AND (state='blocked' OR (state='held' AND address=''))`, addr, c.User.ID, cur)
		if err != nil {
			return actionResult{}, err
		}
		n, _ := res.RowsAffected()
		audit = "Saved " + cur + " payout address (TESTNET " + p.Network() + ")"
		if n > 0 {
			audit += "; " + strconv.FormatInt(n, 10) + " waiting payout(s) now use it"
		}
	}
	return actionResult{Redirect: "/account?saved=1", Audit: audit}, nil
}

func paymentsGlobalLoader(_ context.Context, a *App, _ *http.Request, d *PageData) error {
	var nets []string
	ready := a.providers()
	for _, cur := range sortedCurrencies(ready) {
		nets = append(nets, cur+" "+ready[cur].Network())
	}
	d.PaymentsEnabled = len(nets) > 0
	d.PaymentNetworks = strings.Join(nets, ", ")
	return nil
}

// paymentsOrderLoader builds PaymentView only from payment_addresses and the payments ledger.
// load.go has already restricted the order to its buyer and vendor.
func paymentsOrderLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.Order == nil {
		return nil
	}
	o := d.Order
	p := a.provider(o.Currency)
	pv := &PaymentView{Currency: o.Currency, Required: o.Amount, Testnet: true, Available: p != nil}
	if p == nil {
		msg, _ := a.unavailableError(o.Currency)
		pv.Degraded = msg != ""
	}
	d.Payment = pv
	if p != nil {
		pv.Network, pv.Threshold = p.Network(), p.Confirmations()
	}
	var addr string
	err := a.db.QueryRowContext(ctx, "SELECT address FROM payment_addresses WHERE order_id=$1", o.ID).Scan(&addr)
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
			pv.Status = "Not monitored right now: the " + o.Currency + " test-network wallet is unavailable."
		}
		return nil
	}
	pv.Open = o.State == stateAwaitingPayment
	if pv.Open {
		pv.Address = addr
	}
	dec := currencyDecimals(o.Currency)
	rows, err := a.db.QueryContext(ctx, "SELECT txid,idx,amount,confirmations,credited,locked FROM payments WHERE order_id=$1 ORDER BY created,id", o.ID)
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
	conflicted := false
	for rows.Next() {
		var dep PaymentDeposit
		var amt int64
		var credited, locked bool
		if err = rows.Scan(&dep.TxID, &dep.Index, &amt, &dep.Confirmations, &credited, &locked); err != nil {
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
		case dep.Confirmations >= int64(pv.Threshold):
			dep.State = "Confirmed"
			received += amt
		default:
			dep.State = "Waiting for confirmations"
			pending += amt
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
	case conflicted:
		pv.Status = "A credited deposit is conflicted; moderators have been notified"
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
	var row PayoutRow
	var amt int64
	err = a.db.QueryRowContext(ctx, "SELECT kind,state,txid,amount,currency FROM payouts WHERE order_id=$1", o.ID).Scan(&row.Kind, &row.State, &row.TxID, &amt, &row.Currency)
	if err == nil {
		row.Amount, row.StateLabel = amount(amt, dec), payoutStateLabel(row.State)
		pv.Payout = &row
	} else if err != sql.ErrNoRows {
		return err
	}
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
	d.Payout = v
	return nil
}

func paymentsAdminLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	status := map[string]ProviderStatus{}
	rows, err := a.db.QueryContext(ctx, "SELECT currency,network,COALESCE(to_char(last_poll,'YYYY-MM-DD HH24:MI:SS'),''),last_error FROM payment_status")
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
		} else if msg, disabled := a.unavailableError(cur); msg != "" {
			// Configured but failing its checks in this process: payment requests are refused.
			s.Status, s.Error = "Unavailable — retried every poll", msg
			if disabled {
				s.Status = "Refused — not a test network"
			}
		} else {
			s.Error = "Not configured (" + env[cur] + " is blank)"
		}
		d.Providers = append(d.Providers, s)
	}
	rows, err = a.db.QueryContext(ctx, `SELECT p.id,p.order_id,p.kind,u.handle,p.currency,p.amount,p.address,p.state,p.txid,p.error,to_char(p.updated,'YYYY-MM-DD HH24:MI'),
		(p.state IN ('blocked','held','failed') OR (p.state='sending' AND p.updated < now()-interval '5 minutes')),p.send_ambiguous
		FROM payouts p JOIN users u ON u.id=p.user_id ORDER BY p.id DESC LIMIT 50`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r PayoutRow
		var amt int64
		var sendAmbiguous bool
		if err = rows.Scan(&r.ID, &r.OrderID, &r.Kind, &r.Recipient, &r.Currency, &amt, &r.Address, &r.State, &r.TxID, &r.Error, &r.Updated, &r.Attention, &sendAmbiguous); err != nil {
			return err
		}
		r.Amount, r.StateLabel = amount(amt, currencyDecimals(r.Currency)), payoutStateLabel(r.State)
		switch {
		case r.State == "sending" && r.Attention:
			r.StateLabel, r.Ambiguous = "Stuck in sending — never retried; may have been broadcast, check the wallet", true
		case r.State == "failed" && sendAmbiguous:
			r.StateLabel, r.Ambiguous = r.StateLabel+"; outcome unknown, may have been broadcast", true
		case r.State == "failed":
			r.StateLabel += "; rejected by the wallet, nothing broadcast"
		}
		d.Payouts = append(d.Payouts, r)
	}
	return rows.Err()
}
