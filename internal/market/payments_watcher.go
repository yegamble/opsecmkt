package market

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"
)

// One background watcher per deployment (PostgreSQL advisory lock): it records deposits in the idempotent
// payments ledger, moves fully confirmed orders awaiting_payment -> paid through transition(), cancels orders
// left unpaid past PAYMENT_EXPIRY, flags conflicted, locked or unexpected deposits for moderators, refunds
// deposits that confirm after a cancellation, queues missing payouts and sends pending payouts once.

const paymentWatcherLock = 782494

// payoutSendTimeout bounds one wallet send (the RPC client applies it instead of its shorter rpcTimeout), and
// payoutRecordTimeout bounds recording its outcome. Both are detached from shutdown, so keep the container
// stop grace period (compose.yaml stop_grace_period, 60 s) above their sum plus the HTTP shutdown time (10 s).
// Variables only so tests can shorten them.
var (
	payoutSendTimeout   = 30 * time.Second
	payoutRecordTimeout = 10 * time.Second
)

func init() { registerBackground(paymentWatcher) }

func paymentWatcher(ctx context.Context, a *App) {
	if !a.paymentsConfigured() {
		return
	}
	interval, err := pollInterval()
	if err != nil {
		interval = 30 * time.Second // validated at startup by the provider factories
	}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := a.pollOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("payment watcher: %v", err)
		}
		t.Reset(interval)
	}
}

func currencyDecimals(currency string) int {
	if currency == "XMR" {
		return 12
	}
	return 8
}

func sortedCurrencies(m map[string]PaymentProvider) []string {
	var out []string
	for c := range m {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// pollOnce checks every configured provider (in every process), then runs one watcher pass unless another
// process holds the watcher lock. A currency whose check failed, whose node is syncing or whose provider is
// disabled is not polled: no deposits are read, nothing expires and no payout is sent. Safe to call
// concurrently.
func (a *App) pollOnce(ctx context.Context) error {
	if a.db == nil || !a.paymentsConfigured() {
		return nil
	}
	skip := a.refreshProviders(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var locked bool
	if err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", paymentWatcherLock).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(uctx, "SELECT pg_advisory_unlock($1)", paymentWatcherLock); err != nil {
			conn.Raw(func(any) error { return driver.ErrBadConn }) // discard the session so the lock is released
		}
	}()
	var errs []error
	ready := a.providers()
	for cur, reason := range skip {
		network := ""
		if p := ready[cur]; p != nil {
			network = p.Network()
		}
		if _, err := a.db.ExecContext(ctx, `INSERT INTO payment_status(currency,network,last_poll,last_error) VALUES($1,$2,NULL,$3)
			ON CONFLICT(currency) DO UPDATE SET last_error=excluded.last_error`, cur, network, reason); err != nil {
			errs = append(errs, err)
		}
		if reason != syncingReason {
			errs = append(errs, fmt.Errorf("%s: %s", cur, reason))
		}
	}
	for _, cur := range sortedCurrencies(ready) {
		if _, skipped := skip[cur]; skipped {
			continue
		}
		p := ready[cur]
		err := a.pollCurrency(ctx, p)
		if ctx.Err() == nil {
			msg := ""
			if err != nil {
				msg = truncate(err.Error(), 300)
			}
			if _, serr := a.db.ExecContext(ctx, `INSERT INTO payment_status(currency,network,last_poll,last_error) VALUES($1,$2,now(),$3)
				ON CONFLICT(currency) DO UPDATE SET network=excluded.network,last_poll=now(),last_error=excluded.last_error`, cur, p.Network(), msg); serr != nil && err == nil {
				err = serr
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", cur, err))
		}
	}
	return errors.Join(errs...)
}

type ledgerKey struct {
	txid string
	idx  int64
}

func (a *App) pollCurrency(ctx context.Context, p PaymentProvider) error {
	cur := p.Currency()
	// Orders awaiting payment, plus recently changed funded/terminal orders (late confirmations, conflicts, refunds),
	// plus closed orders whose address was issued within 30 days and that still have a deposit below the threshold
	// or a confirmed deposit neither credited (paid out / counted) nor flagged. Credited deposits below
	// threshold remain watched even after a conflict was flagged, so held payouts can recover. Older
	// addresses are not polled by this recovery path.
	rows, err := a.db.QueryContext(ctx, `SELECT pa.address,pa.order_id FROM payment_addresses pa JOIN orders o ON o.id=pa.order_id
		WHERE pa.currency=$1 AND (o.state='awaiting_payment' OR (o.state<>'draft' AND o.updated > now()-interval '24 hours')
			OR (o.state IN ('cancelled','resolved','completed') AND pa.created > now()-interval '30 days' AND EXISTS (SELECT 1 FROM payments pm
				WHERE pm.order_id=o.id AND ((pm.credited AND pm.confirmations<$2)
					OR (NOT pm.flagged AND (pm.confirmations BETWEEN 0 AND $2-1 OR (pm.confirmations>=$2 AND NOT pm.credited)))))))
		ORDER BY o.updated`, cur, int64(p.Confirmations()))
	if err != nil {
		return err
	}
	orderOf := map[string]string{}
	var addresses, orders []string
	for rows.Next() {
		var addr, order string
		if err = rows.Scan(&addr, &order); err != nil {
			rows.Close()
			return err
		}
		orderOf[addr] = order
		addresses = append(addresses, addr)
		orders = append(orders, order)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var errs []error
	// Orders whose addresses could not be read from the wallet this pass never expire in this pass.
	unread := map[string]bool{}
	for chunk := range slices.Chunk(addresses, 200) {
		if err = a.recordIncoming(ctx, p, chunk, orderOf); err != nil {
			errs = append(errs, err)
			for _, addr := range chunk {
				unread[orderOf[addr]] = true
			}
		}
	}
	for _, id := range orders {
		if err = a.settleOrder(ctx, p, id, !unread[id]); err != nil {
			errs = append(errs, fmt.Errorf("order %s: %w", id[:min(8, len(id))], err))
		}
	}
	if err = a.sendPayouts(ctx, p); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// recordIncoming upserts what the wallet reports for addresses; ledger rows the wallet no longer reports
// (replaced, conflicted or evicted transactions) are marked -1 confirmations.
func (a *App) recordIncoming(ctx context.Context, p PaymentProvider, addresses []string, orderOf map[string]string) error {
	incoming, err := p.Incoming(ctx, addresses)
	if err != nil {
		return err
	}
	cur := p.Currency()
	seen := map[ledgerKey]bool{}
	for _, in := range incoming {
		order, ok := orderOf[in.Address]
		if !ok || in.TxID == "" || len(in.TxID) > 128 || in.Amount <= 0 {
			continue
		}
		seen[ledgerKey{in.TxID, in.Index}] = true
		if _, err = a.db.ExecContext(ctx, `INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,locked) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (currency,txid,idx) DO UPDATE SET confirmations=excluded.confirmations, locked=excluded.locked, updated=now()
			WHERE (payments.confirmations,payments.locked) IS DISTINCT FROM (excluded.confirmations,excluded.locked)`, order, cur, in.TxID, in.Index, in.Address, in.Amount, in.Confirmations, in.Locked); err != nil {
			return err
		}
	}
	rows, err := a.db.QueryContext(ctx, "SELECT id,txid,idx FROM payments WHERE currency=$1 AND address=ANY($2) AND confirmations>=0", cur, addresses)
	if err != nil {
		return err
	}
	var missing []int64
	for rows.Next() {
		var id, idx int64
		var txid string
		if err = rows.Scan(&id, &txid, &idx); err != nil {
			rows.Close()
			return err
		}
		if !seen[ledgerKey{txid, idx}] {
			missing = append(missing, id)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, id := range missing {
		if _, err = a.db.ExecContext(ctx, "UPDATE payments SET confirmations=-1, updated=now() WHERE id=$1", id); err != nil {
			return err
		}
	}
	return nil
}

func isTerminal(state string) bool {
	return state == stateCompleted || state == stateResolved || state == stateCancelled
}

// settleOrder applies the ledger to one order in its own transaction. walletRead reports whether the wallet
// was queried successfully for this order's address in the same pass; without it the order never expires.
func (a *App) settleOrder(ctx context.Context, p PaymentProvider, orderID string, walletRead bool) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", orderID))
	if err != nil {
		return err
	}
	var total int64
	if err = tx.QueryRowContext(ctx, "SELECT amount FROM orders WHERE id=$1", orderID).Scan(&total); err != nil {
		return err
	}
	threshold := int64(p.Confirmations())
	dec := currencyDecimals(o.Currency)
	if o.State == stateAwaitingPayment {
		var sum int64
		var n int
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE(sum(amount),0),count(*) FROM payments WHERE order_id=$1 AND confirmations>=$2 AND NOT locked", orderID, threshold).Scan(&sum, &n); err != nil {
			return err
		}
		if n > 0 && sum >= total {
			if _, err = tx.ExecContext(ctx, "UPDATE payments SET credited=true WHERE order_id=$1 AND confirmations>=$2 AND NOT locked", orderID, threshold); err != nil {
				return err
			}
			note := fmt.Sprintf("TESTNET %s payment confirmed: %s %s in %d output(s) with at least %d confirmations", p.Network(), amount(sum, dec), o.Currency, n, threshold)
			next, err := a.transition(ctx, tx, orderID, stateAwaitingPayment, statePaid, nil, note)
			if err != nil {
				return err
			}
			o = *next
		}
	}
	// Unpaid past PAYMENT_EXPIRY with no deposit seen (a conflicted or locked one does not count): cancel as the
	// system actor; the stock hook returns the reserved unit. A deposit still confirming keeps the order open.
	if o.State == stateAwaitingPayment && walletRead {
		expiry, err := paymentExpiry()
		if err != nil {
			expiry = 24 * time.Hour // validated at startup by the provider factories
		}
		var expired bool
		if err = tx.QueryRowContext(ctx, `SELECT created < now()-make_interval(secs => $2) AND NOT EXISTS (SELECT 1 FROM payments WHERE order_id=$1 AND confirmations>=0 AND NOT locked)
			FROM payment_addresses WHERE order_id=$1`, orderID, expiry.Seconds()).Scan(&expired); err != nil {
			return err
		}
		if expired {
			next, err := a.transition(ctx, tx, orderID, stateAwaitingPayment, stateCancelled, nil, "payment window expired")
			if err != nil {
				return err
			}
			o = *next
		}
	}
	// A Monero transfer with an unlock time is never credited: report it once to moderators and the buyer.
	locked, err := collectStrings(ctx, tx, "SELECT txid FROM payments WHERE order_id=$1 AND locked AND NOT flagged", orderID)
	if err != nil {
		return err
	}
	for _, txid := range locked {
		if _, err = tx.ExecContext(ctx, "UPDATE payments SET flagged=true WHERE order_id=$1 AND txid=$2 AND locked", orderID, txid); err != nil {
			return err
		}
		note := fmt.Sprintf("TESTNET deposit %s has an unlock time; locked transfer ignored: it is not counted toward payment and is never paid out automatically. Moderator review required.", truncate(txid, 20))
		if err = a.flagOrder(ctx, tx, &o, note); err != nil {
			return err
		}
		body := "Order " + orderID[:min(8, len(orderID))] + ": locked transfer ignored. Deposit " + truncate(txid, 20) + " has an unlock time and does not count toward payment; send an ordinary transfer (TESTNET)."
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), o.BuyerID, body); err != nil {
			return err
		}
	}
	// A credited deposit that is now conflicted or gone: report once, hold unsent payouts, never revert state.
	conflicted, err := collectStrings(ctx, tx, "SELECT txid FROM payments WHERE order_id=$1 AND credited AND confirmations<0 AND NOT flagged", orderID)
	if err != nil {
		return err
	}
	for _, txid := range conflicted {
		if _, err = tx.ExecContext(ctx, "UPDATE payments SET flagged=true WHERE order_id=$1 AND txid=$2 AND credited AND confirmations<0", orderID, txid); err != nil {
			return err
		}
		note := fmt.Sprintf("Credited TESTNET deposit %s is now conflicted or missing from the wallet. Order state was not changed; unsent payouts are held. Moderator review required.", truncate(txid, 20))
		if err = a.flagOrder(ctx, tx, &o, note); err != nil {
			return err
		}
	}
	// Unsent payouts wait while any credited deposit is conflicted or back below the threshold (reorg), and
	// resume automatically once every credited deposit is confirmed again.
	var held bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payments WHERE order_id=$1 AND credited AND confirmations<$2)", orderID, threshold).Scan(&held); err != nil {
		return err
	}
	if held {
		_, err = tx.ExecContext(ctx, "UPDATE payouts SET state='held',error=$2,updated=now() WHERE order_id=$1 AND state IN ('pending','blocked')", orderID, heldReason)
	} else {
		// Only the watcher's own hold is lifted automatically; other holds (restored from a backup, for
		// example) wait for an administrator.
		_, err = tx.ExecContext(ctx, "UPDATE payouts SET state=CASE WHEN address='' THEN 'blocked' ELSE 'pending' END,error='',updated=now() WHERE order_id=$1 AND state='held' AND error=$2", orderID, heldReason)
	}
	if err != nil {
		return err
	}
	if isTerminal(o.State) {
		var queued, paidOut bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payouts WHERE order_id=$1)", orderID).Scan(&queued); err != nil {
			return err
		}
		if err = a.enqueuePayout(ctx, tx, &o); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payouts WHERE order_id=$1)", orderID).Scan(&paidOut); err != nil {
			return err
		}
		if !queued && paidOut && o.State == stateCancelled {
			// The cancellation found nothing to refund, so these funds confirmed afterwards.
			body := "Order " + orderID[:min(8, len(orderID))] + ": a TESTNET deposit confirmed after this order was cancelled and is being refunded to you (blocked until you save a " + o.Currency + " payout address, if you have none)."
			if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), o.BuyerID, body); err != nil {
				return err
			}
		}
		if paidOut {
			late, err := collectStrings(ctx, tx, "SELECT txid FROM payments WHERE order_id=$1 AND NOT credited AND NOT flagged AND confirmations>=$2", orderID, threshold)
			if err != nil {
				return err
			}
			for _, txid := range late {
				if _, err = tx.ExecContext(ctx, "UPDATE payments SET flagged=true WHERE order_id=$1 AND txid=$2 AND NOT credited", orderID, txid); err != nil {
					return err
				}
				note := fmt.Sprintf("TESTNET deposit %s arrived after this order was settled and was not paid out. Moderator review required.", truncate(txid, 20))
				if err = a.flagOrder(ctx, tx, &o, note); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

func collectStrings(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// flagOrder records a note in order_events (state unchanged) and notifies every moderator and administrator.
func (a *App) flagOrder(ctx context.Context, tx *sql.Tx, o *Order, note string) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, o.State, note); err != nil {
		return err
	}
	staff, err := collectStrings(ctx, tx, "SELECT id FROM users WHERE role IN ('moderator','admin')")
	if err != nil {
		return err
	}
	body := "Payment review needed for order " + o.ID[:min(8, len(o.ID))] + ": " + note
	for _, uid := range staff {
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, body); err != nil {
			return err
		}
	}
	return nil
}

// sendPayouts claims each pending payout (committed pending -> sending) before a single wallet call, then
// records sent+txid or failed+error. Payouts left in sending (crash, database error) are never retried.
func (a *App) sendPayouts(ctx context.Context, p PaymentProvider) error {
	// Read the restore gate directly, rather than cached page settings. Monitoring
	// may continue during recovery, but no payout may leave the restored wallet.
	var recoveryRequired bool
	if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key='payments_recovery_required' AND value='true')").Scan(&recoveryRequired); err != nil {
		return err
	}
	if recoveryRequired {
		return nil
	}
	cur := p.Currency()
	rows, err := a.db.QueryContext(ctx, "SELECT id FROM payouts WHERE state='pending' AND currency=$1 ORDER BY id LIMIT 50", cur)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		var to, order, recipient string
		var amt int64
		err = a.db.QueryRowContext(ctx, `UPDATE payouts SET state='sending',updated=now() WHERE id=$1 AND state='pending' AND address<>''
			AND NOT EXISTS (SELECT 1 FROM settings WHERE key='payments_recovery_required' AND value='true')
			AND NOT EXISTS (SELECT 1 FROM payments pm WHERE pm.order_id=payouts.order_id AND pm.credited AND pm.confirmations<$2)
			RETURNING address,amount,order_id,user_id`, id, int64(p.Confirmations())).Scan(&to, &amt, &order, &recipient)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		// The wallet call is detached from shutdown (a redeploy must not abort a broadcast mid-flight) but
		// bounded; Close waits for it. The outcome is recorded on a fresh bounded context for the same reason.
		sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), payoutSendTimeout)
		txid, serr := p.Send(sctx, to, amt)
		scancel()
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), payoutRecordTimeout)
		label := amount(amt, currencyDecimals(cur)) + " " + cur
		if serr != nil {
			ambiguous := !walletRejected(serr)
			msg := "The wallet rejected the send; nothing was broadcast. "
			if ambiguous {
				msg = "Wallet call failed without a definite answer; the transaction may or may not have been broadcast. Check the wallet before requeueing or paying manually. "
			}
			err = a.recordPayout(rctx, id, order, recipient, "failed", "", msg+truncate(serr.Error(), 200), ambiguous, "TESTNET payout of "+label+" failed and will not be retried automatically.")
			errs = append(errs, fmt.Errorf("payout %d: %w", id, serr))
		} else {
			err = a.recordPayout(rctx, id, order, recipient, "sent", txid, "", false, "TESTNET payout of "+label+" sent: "+txid)
		}
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("payout %d recorded as sending only: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// walletRejected reports a definite refusal: the wallet answered the send with a JSON-RPC error, so nothing
// was broadcast. Any other failure (timeout, reset connection, unreadable or incomplete reply after the
// request was sent) leaves the outcome unknown.
func walletRejected(err error) bool {
	var re *rpcError
	return errors.As(err, &re)
}

// recordPayout moves a claimed payout from sending to state. ambiguous marks a failure whose broadcast is
// unknown: requeueing it then needs the administrator's explicit confirmation (payments_admin.go).
func (a *App) recordPayout(ctx context.Context, id int64, orderID, recipient, state, txid, msg string, ambiguous bool, note string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var orderState string
	if err = tx.QueryRowContext(ctx, "UPDATE payouts p SET state=$2,txid=$3,error=$4,send_ambiguous=$5,updated=now() FROM orders o WHERE p.id=$1 AND p.state='sending' AND o.id=p.order_id RETURNING o.state", id, state, txid, msg, ambiguous).Scan(&orderState); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", orderID, orderState, note); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), recipient, "Order "+orderID[:min(8, len(orderID))]+": "+note); err != nil {
		return err
	}
	if state == "failed" {
		staff, err := collectStrings(ctx, tx, "SELECT id FROM users WHERE role='admin'")
		if err != nil {
			return err
		}
		for _, uid := range staff {
			if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, "Payout failed for order "+orderID[:min(8, len(orderID))]+"; see the admin payouts panel."); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
