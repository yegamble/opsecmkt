package market

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
)

// One background watcher per deployment (PostgreSQL advisory lock): it records deposits in the idempotent
// payments ledger, moves fully confirmed orders awaiting_payment -> paid through transition(), cancels orders
// left unpaid past PAYMENT_EXPIRY, announces deposits received after funding to the buyer and vendor, flags
// conflicted, locked or unexpected deposits for moderators, refunds deposits that confirm after a
// cancellation, queues missing payouts and sends pending payouts once.

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
	skip, tips := a.refreshProviders(ctx)
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
	// A node whose tip is below the highest tip recorded for its currency (restarted from an older state, or a
	// wallet rescanning after a restore) is treated as syncing: read now, deposits it has not caught up with would
	// be recorded as missing or back below the threshold and announced as regressions.
	for _, cur := range sortedCurrencies(ready) {
		tip, reported := tips[cur]
		if _, skipped := skip[cur]; skipped || !reported {
			continue
		}
		var highest int64
		if err := a.db.QueryRowContext(ctx, `INSERT INTO payment_status(currency,network,last_poll,last_error,tip_height) VALUES($1,$2,NULL,'',$3)
			ON CONFLICT(currency) DO UPDATE SET tip_height=GREATEST(payment_status.tip_height,excluded.tip_height) RETURNING tip_height`, cur, ready[cur].Network(), tip).Scan(&highest); err != nil {
			errs = append(errs, err)
			skip[cur] = "Could not compare the node tip with the highest tip recorded; watcher pass skipped (no deposits read, no expiry, no payouts)."
		} else if tip < highest {
			skip[cur] = fmt.Sprintf(tipBehindPrefix+" (node tip %d is below the highest tip %d recorded by the market: restarted or restored from an older state); watcher pass skipped (no deposits read, no expiry, no payouts) until it catches up.", tip, highest)
		}
	}
	for cur, reason := range skip {
		network := ""
		if p := ready[cur]; p != nil {
			network = p.Network()
		}
		if _, err := a.db.ExecContext(ctx, `INSERT INTO payment_status(currency,network,last_poll,last_error) VALUES($1,$2,NULL,$3)
			ON CONFLICT(currency) DO UPDATE SET last_error=excluded.last_error`, cur, network, reason); err != nil {
			errs = append(errs, err)
		}
		if reason != syncingReason && !strings.HasPrefix(reason, tipBehindPrefix) {
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

// fundedOpenStates lists, for SQL, the funded states that are not terminal: an order in one of them keeps its
// deposit address watched, and a deposit first recorded there is announced once (payments.late_notice).
const fundedOpenStates = "'paid','shipped','delivered','disputed'"

type ledgerKey struct {
	txid string
	idx  int64
}

func (a *App) pollCurrency(ctx context.Context, p PaymentProvider) error {
	cur := p.Currency()
	// Orders awaiting payment and every funded non-terminal order whatever its age (extra deposits, conflicts,
	// reorgs), plus recently changed terminal orders (late confirmations, refunds), plus closed orders whose
	// address was issued within 30 days and that still have a deposit below the threshold or a confirmed deposit
	// neither credited (paid out / counted) nor flagged. Credited deposits below threshold remain watched even
	// after a conflict was flagged. Whatever their age, orders with a payout pending (sendPayouts sends only
	// payouts of orders read and settled in this pass) or held are watched too: the watcher's own hold resumes
	// once the deposit re-confirms, and the ledger of a hold an administrator must release (a watcher hold
	// converted by a suspension or a restore from backup) stays current, so the release is not refused on stale
	// confirmations. Other older addresses of closed orders are not polled.
	rows, err := a.db.QueryContext(ctx, `SELECT pa.address,pa.order_id FROM payment_addresses pa JOIN orders o ON o.id=pa.order_id
		WHERE pa.currency=$1 AND (o.state IN ('awaiting_payment',`+fundedOpenStates+`) OR (o.state<>'draft' AND o.updated > now()-interval '24 hours')
			OR (o.state IN ('cancelled','resolved','completed') AND pa.created > now()-interval '30 days' AND EXISTS (SELECT 1 FROM payments pm
				WHERE pm.order_id=o.id AND ((pm.credited AND pm.confirmations<$2)
					OR (NOT pm.flagged AND (pm.confirmations BETWEEN 0 AND $2-1 OR (pm.confirmations>=$2 AND NOT pm.credited))))))
			OR EXISTS (SELECT 1 FROM payouts po WHERE po.order_id=o.id AND po.state IN ('pending','held')))
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
	// Only payouts of orders whose address was read and whose ledger was applied in this pass are sent. A payout
	// that becomes pending while the pass runs (address saved, administrator release or requeue) for an order not
	// read in it waits for the next pass, which watches it.
	var settled []string
	for _, id := range orders {
		if err = a.settleOrder(ctx, p, id, !unread[id]); err != nil {
			errs = append(errs, fmt.Errorf("order %s: %w", id[:min(8, len(id))], err))
		} else if !unread[id] {
			settled = append(settled, id)
		}
	}
	if err = a.sendPayouts(ctx, p, settled); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// recordIncoming upserts what the wallet reports for addresses; ledger rows the wallet no longer reports
// (replaced, conflicted or evicted transactions) are marked -1 confirmations. A new row on a funded,
// non-terminal order is marked late_notice for settleOrder to announce.
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
		if _, err = a.db.ExecContext(ctx, `INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,locked,late_notice)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,EXISTS(SELECT 1 FROM orders WHERE id=$1 AND state IN (`+fundedOpenStates+`)))
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
		if err = a.flagOrder(ctx, tx, &o, note, ""); err != nil {
			return err
		}
		body := "Order " + orderID[:min(8, len(orderID))] + ": locked transfer ignored. Deposit " + truncate(txid, 20) + " has an unlock time and does not count toward payment; send an ordinary transfer (TESTNET)."
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), o.BuyerID, body); err != nil {
			return err
		}
	}
	// A deposit first recorded after funding is announced once to the order history, the buyer and the vendor. It
	// counts toward the order's single release or refund only if it has reached the threshold by then
	// (enqueuePayout); one that confirms after the payout is queued is flagged to moderators below, while the
	// order is still watched.
	if o.State != stateAwaitingPayment && !isTerminal(o.State) {
		rows, err := tx.QueryContext(ctx, "UPDATE payments SET late_notice=false WHERE order_id=$1 AND late_notice AND NOT locked AND confirmations>=0 RETURNING txid,idx,amount", orderID)
		if err != nil {
			return err
		}
		var notes []string
		for rows.Next() {
			var txid string
			var idx, amt int64
			if err = rows.Scan(&txid, &idx, &amt); err != nil {
				rows.Close()
				return err
			}
			notes = append(notes, fmt.Sprintf("TESTNET %s deposit %s:%d of %s %s received after this order was funded. If it has %d confirmations, as last recorded by the market, when the order is completed, cancelled or resolved, it is included in the order's single release or refund; otherwise it is not paid out automatically.",
				p.Network(), truncate(txid, 20), idx, amount(amt, dec), o.Currency, threshold))
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		for _, note := range notes {
			if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", orderID, o.State, note); err != nil {
				return err
			}
			for _, uid := range []string{o.BuyerID, o.VendorID} {
				if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, "Order "+orderID[:min(8, len(orderID))]+": "+note); err != nil {
					return err
				}
			}
		}
	}
	// A credited deposit back below the threshold (conflicted or missing, or at a lower depth after a reorg) is
	// announced once per episode while the order is open or its payout unsent; unsent payouts are held below and
	// the order state is never reverted. Reaching the threshold again ends the episode (regress_notice), so a
	// later regression is announced again. payments.flagged stays set: staff keep read access and the desk lists it.
	if _, err = tx.ExecContext(ctx, "UPDATE payments SET regress_notice=false WHERE order_id=$1 AND regress_notice AND confirmations>=$2", orderID, threshold); err != nil {
		return err
	}
	var sent bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payouts WHERE order_id=$1 AND state='sent')", orderID).Scan(&sent); err != nil {
		return err
	}
	if !isTerminal(o.State) || !sent {
		rows, err := tx.QueryContext(ctx, "UPDATE payments SET flagged=true,regress_notice=true WHERE order_id=$1 AND credited AND confirmations<$2 AND NOT regress_notice RETURNING txid,confirmations", orderID, threshold)
		if err != nil {
			return err
		}
		type regression struct {
			txid  string
			confs int64
		}
		var found []regression
		for rows.Next() {
			var r regression
			if err = rows.Scan(&r.txid, &r.confs); err != nil {
				rows.Close()
				return err
			}
			found = append(found, r)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		for _, r := range found {
			what := "is now conflicted or missing from the wallet"
			if r.confs >= 0 {
				what = fmt.Sprintf("is back below the confirmation threshold (%d of %d confirmations), as after a chain reorganisation", r.confs, threshold)
			}
			note := fmt.Sprintf("Credited TESTNET deposit %s %s. Order state was not changed; unsent payouts are held until it confirms again. Moderator review required.", truncate(r.txid, 20), what)
			party := fmt.Sprintf("Order %s: credited TESTNET deposit %s %s. The order was not changed and any unsent payout waits until the deposit confirms again; until then this payment is not final. The market staff have been notified.", orderID[:min(8, len(orderID))], truncate(r.txid, 20), what)
			if err = a.flagOrder(ctx, tx, &o, note, party); err != nil {
				return err
			}
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
				if err = a.flagOrder(ctx, tx, &o, note, ""); err != nil {
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

// flagOrder records a note in order_events (state unchanged) and notifies every moderator and administrator
// who is not suspended. With a party message, the buyer and the vendor are sent it instead of the staff
// notification.
func (a *App) flagOrder(ctx context.Context, tx *sql.Tx, o *Order, note, party string) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, o.State, note); err != nil {
		return err
	}
	parties := []string{}
	if party != "" {
		parties = []string{o.BuyerID, o.VendorID}
		for _, uid := range parties {
			if _, err := tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, party); err != nil {
				return err
			}
		}
	}
	staff, err := collectStrings(ctx, tx, "SELECT id FROM users WHERE role IN ('moderator','admin') AND suspended_at IS NULL AND id<>ALL($1)", parties)
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

// sendPayouts claims each pending payout of the given orders (committed pending -> sending) before a single
// wallet call, then records sent+txid or failed+error. Payouts left in sending (crash, database error) are
// never retried. orders are those whose deposit address was read and whose ledger was applied in this pass.
func (a *App) sendPayouts(ctx context.Context, p PaymentProvider, orders []string) error {
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
	rows, err := a.db.QueryContext(ctx, "SELECT id FROM payouts WHERE state='pending' AND currency=$1 AND order_id=ANY($2) ORDER BY id LIMIT 50", cur, orders)
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
			ambiguous := !walletRejected(cur, serr)
			msg := "The wallet reported a pre-broadcast error; nothing was broadcast. "
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

// walletRejected reports a definite refusal: the wallet answered the send with a JSON-RPC error raised before
// anything was broadcast. Any other failure (timeout, reset connection, unreadable or incomplete reply after
// the request was sent), and any error from a wallet not classified here, leaves the outcome unknown.
//
// Bitcoin: every sendtoaddress JSON-RPC error is definite. Bitcoin Core (v31.1 src/wallet/rpc/spend.cpp:185-190,
// src/wallet/wallet.cpp:2314-2351) builds the transaction, stores it in the wallet and only then submits it to
// the mempool; a failed submission is logged, not returned, so the RPC answers with the txid.
//
// Monero: monero-wallet-rpc's transfer submits through wallet2::commit_tx, whose /sendrawtransaction call can
// fail after the daemon relayed the transaction (-38 on a timeout, -3/-4 from the daemon's reply, -1 when
// saving the tx info after commit), so only the codes in moneroPreSubmitCodes are definite.
func walletRejected(cur string, err error) bool {
	var re *rpcError
	if !errors.As(err, &re) {
		return false
	}
	return cur == "BTC" || (cur == "XMR" && moneroPreSubmitCodes[re.Code])
}

// moneroPreSubmitCodes are the monero-wallet-rpc transfer error codes raised only before wallet2::commit_tx
// submits anything (monero v0.18.5.1: codes from src/wallet/wallet_rpc_server_error_codes.h; raised by
// validate_transfer, on_transfer before fill_response and by handle_rpc_exception for exceptions that
// create_transactions_2 throws; commit_tx, src/wallet/wallet2.cpp:7560-7644, throws none of them).
var moneroPreSubmitCodes = map[int64]bool{
	-2:  true, // WRONG_ADDRESS: validate_transfer, wallet_rpc_server.cpp:1064
	-16: true, // TX_NOT_POSSIBLE: no transaction created (:1270) or tx_not_possible (:3809)
	-17: true, // NOT_ENOUGH_MONEY: not_enough_money (:3799)
	-18: true, // TX_TOO_LARGE: more than one transaction needed (:1278)
	-19: true, // NOT_ENOUGH_OUTS_TO_MIX: not_enough_outs_to_mix (:3819)
	-20: true, // ZERO_DESTINATION: validate_transfer (:1099) or zero_destination (:3794)
	-37: true, // NOT_ENOUGH_UNLOCKED_MONEY: not_enough_unlocked_money (:3804)
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
