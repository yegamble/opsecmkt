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
// conflicted, locked or unexpected deposits for moderators (a deposit first recorded after its order closed is
// flagged, never paid out), refunds deposits recorded before a cancellation that confirm after it, queues
// missing payouts and sends pending payouts once.

const paymentWatcherLock = 782494

// payoutSendTimeout bounds one wallet send (the RPC client applies it instead of its shorter rpcTimeout), and
// payoutRecordTimeout bounds recording its outcome. Both are detached from shutdown, so keep the container
// stop grace period (compose.yaml stop_grace_period, 60 s) above their sum plus the HTTP shutdown time (10 s).
// Variables only so tests can shorten them.
//
// watcherPassTimeout bounds one watcher pass (A-177): its queries, including the wait for a pool connection,
// and its wallet reads. A normal pass takes seconds; a pass cut short leaves its remaining orders and pending
// payouts for the next one. A payout send already under way is detached from it and keeps its own bound.
var (
	payoutSendTimeout   = 30 * time.Second
	payoutRecordTimeout = 10 * time.Second
	watcherPassTimeout  = 5 * time.Minute
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
		pass, cancel := context.WithTimeout(ctx, watcherPassTimeout)
		err := a.pollOnce(pass)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("payment watcher: %s", watcherCause(err))
		}
		t.Reset(interval)
	}
}

// watchError labels a watcher error. Error() keeps the wrapped text (payment_status, the admin pages), but the
// watcher's log line prints only the label and the class (watcherCause), never the wallet's or the driver's
// text, which can hold a payout address or the database user, name, host and port (A-170). A label holds
// only a currency, "order <short id>", "payout <id>" and fixed words; an empty label adds nothing to Error().
type watchError struct {
	label string
	class string // logged instead of watcherCause(err) when set
	err   error
}

func (e *watchError) Error() string {
	if e.label == "" {
		return e.err.Error()
	}
	return e.label + ": " + e.err.Error()
}

func (e *watchError) Unwrap() error { return e.err }

// watcherCause is the one-line log form of a watcher error: each joined error in turn ("; ") with all its
// labels, then the JSON-RPC code of a wallet error (rpc:<code>) or errorCause's class.
func watcherCause(err error) string { return strings.Join(watcherCauses(err), "; ") }

func watcherCauses(err error) []string {
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		var parts []string
		for _, c := range e.Unwrap() {
			parts = append(parts, watcherCauses(c)...)
		}
		return parts
	case *watchError:
		parts := []string{e.class}
		if e.class == "" {
			parts = watcherCauses(e.err)
		}
		if e.label != "" {
			for i := range parts {
				parts[i] = e.label + ": " + parts[i]
			}
		}
		return parts
	}
	var re *rpcError
	if errors.As(err, &re) {
		return []string{fmt.Sprintf("rpc:%d", re.Code)}
	}
	return []string{errorCause(err)}
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
	skip, tips, why := a.refreshProviders(ctx)
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
			skip[cur], why[cur] = "Could not compare the node tip with the highest tip recorded; watcher pass skipped (no deposits read, no expiry, no payouts).", "tip comparison failed"
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
			class := "pass skipped"
			if why[cur] != "" {
				class += ": " + why[cur]
			}
			errs = append(errs, &watchError{label: cur, class: class, err: errors.New(reason)})
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
			errs = append(errs, &watchError{label: cur, err: err})
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
	// reorgs), plus recently changed terminal orders (late confirmations, refunds), plus every closed order whose
	// address was issued within 30 days (A-211: a deposit sent to it after the order closed is recorded, flagged
	// and shown to the buyer, never paid out automatically; see settleOrder). Whatever their age, orders with a
	// payout pending (sendPayouts sends only payouts of orders read and settled in this pass) or held are watched
	// too: the watcher's own hold resumes once the deposit re-confirms, and the ledger of a hold an administrator
	// must release (a watcher hold converted by a suspension or a restore from backup) stays current, so the
	// release is not refused on stale confirmations. Other older addresses of closed orders are not polled.
	//
	// Cost: the watch set is every open order plus every order whose address was issued in the last 30 days, so
	// about 30 days of orders (3,000 addresses at 100 orders a day). recordIncoming reads them in chunks of 200.
	// Bitcoin: one listreceivedbyaddress per chunk (it lists every address of the wallet that has received funds,
	// so its cost grows with the wallet as well) and one gettransaction per transaction received by the chunk's
	// addresses, every pass. Monero: one get_address_index per address the first time this process reads it (then
	// cached) and one get_transfers per chunk (subaddr_indices). Each watched order also takes one settleOrder
	// transaction per pass. The work per pass (every PAYMENT_POLL_INTERVAL, default 30 s) therefore grows with the
	// number of orders of the last 30 days and the deposits to them (not measured at volume); a pass is bounded
	// by watcherPassTimeout, and one cut short leaves the rest to the next.
	rows, err := a.db.QueryContext(ctx, `SELECT pa.address,pa.order_id FROM payment_addresses pa JOIN orders o ON o.id=pa.order_id
		WHERE pa.currency=$1 AND (o.state IN ('awaiting_payment',`+fundedOpenStates+`) OR (o.state<>'draft' AND o.updated > now()-interval '24 hours')
			OR (o.state IN ('cancelled','resolved','completed') AND pa.created > now()-interval '30 days')
			OR EXISTS (SELECT 1 FROM payouts po WHERE po.order_id=o.id AND po.state IN ('pending','held')))
		ORDER BY o.updated`, cur)
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
			errs = append(errs, &watchError{label: "order " + id[:min(8, len(id))], err: err})
		} else if !unread[id] {
			settled = append(settled, id)
		}
	}
	if err = a.sendPayouts(ctx, p, settled); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// recordIncoming upserts what the wallet reports for addresses (applyIncoming).
func (a *App) recordIncoming(ctx context.Context, p PaymentProvider, addresses []string, orderOf map[string]string) error {
	incoming, err := p.Incoming(ctx, addresses)
	if err != nil {
		return &watchError{class: "wallet read failed: " + watcherCause(err), err: err}
	}
	return a.applyIncoming(ctx, p.Currency(), incoming, addresses, orderOf)
}

// applyIncoming writes what a wallet reported for addresses to the payments ledger, one row per output; ledger rows
// the wallet no longer reports (replaced, conflicted or evicted transactions) are marked -1 confirmations. A new row
// on a funded, non-terminal order is marked late_notice for settleOrder to announce; one on a closed order is marked
// after_close: it is never counted toward the order's settlement (payoutBasis), and settleOrder flags it. The pooled
// watcher and the escrow watcher (escrow_watch.go) both write the ledger through it.
func (a *App) applyIncoming(ctx context.Context, cur string, incoming []Incoming, addresses []string, orderOf map[string]string) error {
	seen := map[ledgerKey]bool{}
	for _, in := range incoming {
		order, ok := orderOf[in.Address]
		if !ok || in.TxID == "" || len(in.TxID) > 128 || in.Amount <= 0 {
			continue
		}
		seen[ledgerKey{in.TxID, in.Index}] = true
		if _, err := a.upsertIncoming(ctx, cur, order, in); err != nil {
			return err
		}
	}
	return a.markMissing(ctx, cur, addresses, seen)
}

// upsertIncoming writes one output to the payments ledger for order (applyIncoming's row) and reports whether the row
// was inserted or changed. The Monero escrow watcher calls it row by row, so a row the database refuses is caught for
// that row alone (escrow_xmr_watch.go).
func (a *App) upsertIncoming(ctx context.Context, cur, order string, in Incoming) (bool, error) {
	res, err := a.db.ExecContext(ctx, `INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,locked,late_notice,after_close)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,EXISTS(SELECT 1 FROM orders WHERE id=$1 AND state IN (`+fundedOpenStates+`)),
			EXISTS(SELECT 1 FROM orders WHERE id=$1 AND state IN ('cancelled','resolved','completed')))
		ON CONFLICT (currency,txid,idx) DO UPDATE SET confirmations=excluded.confirmations, locked=excluded.locked, updated=now()
		WHERE (payments.confirmations,payments.locked) IS DISTINCT FROM (excluded.confirmations,excluded.locked)`, order, cur, in.TxID, in.Index, in.Address, in.Amount, in.Confirmations, in.Locked)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// markMissing marks -1 confirmations on the ledger rows at addresses that the wallet no longer reports (not in seen).
func (a *App) markMissing(ctx context.Context, cur string, addresses []string, seen map[ledgerKey]bool) error {
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
			note := fmt.Sprintf("%s %s payment confirmed: %s %s in %d output(s) with at least %d confirmations", a.netTag(), p.Network(), amount(sum, dec), o.Currency, n, threshold)
			next, err := a.transition(ctx, tx, orderID, stateAwaitingPayment, statePaid, nil, note)
			if err != nil {
				return err
			}
			o = *next
		}
	}
	// Unpaid past PAYMENT_EXPIRY with no deposit seen (a conflicted or locked one does not count): cancel as the
	// system actor; the stock hook returns the reserved unit. A deposit still confirming keeps the order open.
	// Underpaid: every deposit confirmed but short of the price, the address older than PAYMENT_EXPIRY and no
	// deposit (top-up) seen for PAYMENT_EXPIRY either: cancel the same way, and the payout hook refunds the
	// confirmed deposits to the buyer. Without this an underpaid order and its deposits would wait forever.
	if o.State == stateAwaitingPayment && walletRead {
		// The buyer's order page states the resulting time from the same inputs (paymentsOrderLoader, A-123).
		expiry := expiryWindow()
		var unpaid, underpaid bool
		var received int64
		if err = tx.QueryRowContext(ctx, `WITH d AS (SELECT amount,confirmations,created FROM payments WHERE order_id=$1 AND confirmations>=0 AND NOT locked)
			SELECT pa.created < now()-make_interval(secs => $2) AND NOT EXISTS (SELECT 1 FROM d),
				pa.created < now()-make_interval(secs => $2) AND EXISTS (SELECT 1 FROM d) AND NOT EXISTS (SELECT 1 FROM d WHERE confirmations<$3)
					AND (SELECT max(created) FROM d) < now()-make_interval(secs => $2),
				(SELECT COALESCE(sum(amount),0) FROM d)
			FROM payment_addresses pa WHERE pa.order_id=$1`, orderID, expiry.Seconds(), threshold).Scan(&unpaid, &underpaid, &received); err != nil {
			return err
		}
		if unpaid || underpaid {
			note := "payment window expired"
			if underpaid {
				note = fmt.Sprintf("payment window expired underpaid: %s of %s %s received", amount(received, dec), amount(total, dec), o.Currency)
			}
			next, err := a.transition(ctx, tx, orderID, stateAwaitingPayment, stateCancelled, nil, note)
			if err != nil {
				return err
			}
			o = *next
			if underpaid {
				// The transition queued the refund (payout hook) or, below the floor, flagged the deposits instead:
				// the notification says which (A-211).
				var refundState string
				var refund int64
				err = tx.QueryRowContext(ctx, "SELECT state,amount FROM payouts WHERE order_id=$1 AND kind='refund'", orderID).Scan(&refundState, &refund)
				if err != nil && err != sql.ErrNoRows {
					return err
				}
				what := fmt.Sprintf("A refund of %s %s to your saved %s payout address is queued; the network fee is taken from it. The order page shows its status.", amount(refund, dec), o.Currency, o.Currency)
				switch {
				case err == sql.ErrNoRows:
					what = fmt.Sprintf("%s %s is below the minimum automatic payout of %s %s, so it is not refunded; market staff were notified.", amount(received, dec), o.Currency, amount(payoutFloor(o.Currency, "refund"), dec), o.Currency)
				case refundState == "blocked":
					what = fmt.Sprintf("The refund of %s %s waits until you save a %s payout address on your account page; the network fee is taken from it.", amount(refund, dec), o.Currency, o.Currency)
				}
				body := fmt.Sprintf("Order %s: the payment window ended with %s of %s %s received, so the order was cancelled. %s Anything sent to this order's address after the cancellation is not refunded automatically.",
					orderID[:min(8, len(orderID))], amount(received, dec), amount(total, dec), o.Currency, what)
				if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), o.BuyerID, body); err != nil {
					return err
				}
			}
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
		note := fmt.Sprintf("%s deposit %s has an unlock time; locked transfer ignored: it is not counted toward payment and is never paid out automatically. Moderator review required.", a.netTag(), truncate(txid, 20))
		if err = a.flagOrder(ctx, tx, &o, note, ""); err != nil {
			return err
		}
		body := "Order " + orderID[:min(8, len(orderID))] + ": locked transfer ignored. Deposit " + truncate(txid, 20) + " has an unlock time and does not count toward payment; send an ordinary transfer (" + a.netTag() + ")."
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), o.BuyerID, body); err != nil {
			return err
		}
	}
	// A deposit first recorded after funding is announced once to the order history, the buyer and the vendor. It
	// counts toward the order's settlement (its release and any return, or its refund) only if it has reached the threshold by then
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
			notes = append(notes, fmt.Sprintf("%s %s deposit %s:%d of %s %s received after this order was funded. If it has %d confirmations, as last recorded by the market, when the order is completed, cancelled or resolved, it is included in the order's settlement (a release to the vendor with any excess returned to the buyer, or a refund); otherwise it is not paid out automatically.",
				a.netTag(), p.Network(), truncate(txid, 20), idx, amount(amt, dec), o.Currency, threshold))
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
	// announced once per episode while the order is open or any of its payouts is unsent (A-213: a release sent
	// with its return still unsent counts as unsent); unsent payouts are held below and the order state is never
	// reverted. Reaching the threshold again ends the episode (regress_notice), so a later regression is announced
	// again, as a new flag with no disposition (A-31; the order history keeps the earlier one). payments.flagged
	// stays set: staff keep read access and the desk lists it.
	if _, err = tx.ExecContext(ctx, "UPDATE payments SET regress_notice=false WHERE order_id=$1 AND regress_notice AND confirmations>=$2", orderID, threshold); err != nil {
		return err
	}
	var allSent bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payouts WHERE order_id=$1) AND NOT EXISTS(SELECT 1 FROM payouts WHERE order_id=$1 AND state<>'sent')", orderID).Scan(&allSent); err != nil {
		return err
	}
	if !isTerminal(o.State) || !allSent {
		rows, err := tx.QueryContext(ctx, "UPDATE payments SET flagged=true,regress_notice=true,disposition='',disposition_note='',disposed_at=NULL WHERE order_id=$1 AND credited AND confirmations<$2 AND NOT regress_notice RETURNING txid,confirmations", orderID, threshold)
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
			note := fmt.Sprintf("Credited %s deposit %s %s. Order state was not changed; unsent payouts are held until it confirms again. Moderator review required.", a.netTag(), truncate(r.txid, 20), what)
			party := fmt.Sprintf("Order %s: credited %s deposit %s %s. The order was not changed and any unsent payout waits until the deposit confirms again; until then this payment is not final. The market staff have been notified.", orderID[:min(8, len(orderID))], a.netTag(), truncate(r.txid, 20), what)
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
		if err = a.settleTerminal(ctx, tx, &o); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payouts WHERE order_id=$1)", orderID).Scan(&paidOut); err != nil {
			return err
		}
		if !queued && paidOut && o.State == stateCancelled {
			// The cancellation found nothing to refund, so these funds confirmed afterwards.
			body := "Order " + orderID[:min(8, len(orderID))] + ": a " + a.netTag() + " deposit confirmed after this order was cancelled and is being refunded to you (blocked until you save a " + o.Currency + " payout address, if you have none)."
			if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), o.BuyerID, body); err != nil {
				return err
			}
		}
		if err = a.flagLateDeposits(ctx, tx, &o, threshold, paidOut); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// flagLateDeposits flags, on a closed order, the confirmed deposits that are not paid out: those that confirmed
// after the order's settlement was queued (paidOut) and those first recorded after the order closed
// (after_close), which are never counted. They are never credited or refunded automatically (A-211). Each pass
// that flags some writes one order note naming them (the desk dates each flag from it). The buyer (the amount, no
// transaction ID; the vendor is not told) and the staff are notified once per episode: an episode starts when
// the order has no late flag open and lasts while one is, so many small deposits give one notice each, and a deposit
// after staff disposed of every earlier late flag is announced again. A late flag is one open by paymentFlagOpenRule
// (the rule the moderation desk and the review handler read, so staff can always record a flag that holds an episode,
// A-31) of a deposit not credited, not below the floor and not locked: a counted escrow deposit's flag is never open,
// so it never holds an episode (QA462C-1), and an open locked, below-floor, regressed or credited spent-outside flag
// does not silence the notice of a new deposit after the close. Only this function flags a deposit after the close of
// an escrow order before it confirms or is announced (xmrSettleWaits leaves such a deposit to it), so each is
// announced once.
func (a *App) flagLateDeposits(ctx context.Context, tx *sql.Tx, o *Order, threshold int64, paidOut bool) error {
	var episode bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payments p WHERE p.order_id=$1 AND "+paymentFlagOpen("p")+" AND NOT p.credited AND NOT p.below_floor AND NOT p.locked)", o.ID).Scan(&episode); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE payments SET flagged=true WHERE order_id=$1 AND NOT credited AND NOT flagged AND NOT locked
		AND confirmations>=$2 AND ($3 OR after_close) RETURNING txid,amount`, o.ID, threshold, paidOut)
	if err != nil {
		return err
	}
	dec := currencyDecimals(o.Currency)
	var deposits []string
	var sum int64
	for rows.Next() {
		var txid string
		var amt int64
		if err = rows.Scan(&txid, &amt); err != nil {
			rows.Close()
			return err
		}
		deposits = append(deposits, "deposit "+truncate(txid, 20)+" ("+amount(amt, dec)+" "+o.Currency+")")
		sum += amt
	}
	rows.Close()
	if err = rows.Err(); err != nil || len(deposits) == 0 {
		return err
	}
	slices.Sort(deposits)
	closed, verb := "closed", "was"
	if paidOut {
		closed = "settled"
	}
	if len(deposits) > 1 {
		verb = "were"
	}
	note := a.netTag() + " " + strings.Join(deposits, ", ") + " arrived after this order was " + closed + " and " + verb + " not paid out. Moderator review required."
	body := "Order " + o.ID[:min(8, len(o.ID))] + ": " + amount(sum, dec) + " " + o.Currency + " (" + a.netTag() + ") arrived at this order's deposit address after the order closed and was not paid out; it is not refunded automatically. Market staff were notified and review it."
	if o.Escrow {
		// An escrow deposit is at the order's escrow address, never in a wallet staff could refund from (QA31-3).
		net, how := a.escrowNetwork(), "the market builds a stray refund returning it to your refund address, which you or the vendor sign on the order page"
		if o.Currency == "XMR" { // a Monero escrow is custodial: the market alone builds and sends its stray refund (P3-R8)
			net, how = xmrEscrowNetwork, "the market returns it, less the network fee, to your refund address by a stray refund from the escrow"
		}
		note = a.netTag() + " " + net + " " + strings.Join(deposits, ", ") + " arrived at this order's escrow address after the order closed; not counted, it is returned to the buyer's refund address by a stray refund from the escrow. Moderator review required."
		body = "Order " + o.ID[:min(8, len(o.ID))] + ": " + amount(sum, dec) + " " + o.Currency + " (" + a.netTag() + " " + net + ") arrived at this order's escrow address after the order closed. It is not counted; " + how + ". Market staff were notified."
	}
	if episode {
		_, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, o.State, note)
		return err
	}
	if err = a.flagOrder(ctx, tx, o, note, ""); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), o.BuyerID, body)
	return err
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
		// The claim never takes a payout of an escrow order (ES-1): such an order is settled from its escrow, and the
		// database refuses to write one (migration 074), so only a row older than its order's escrow could match.
		err = a.db.QueryRowContext(ctx, `UPDATE payouts SET state='sending',updated=now() WHERE id=$1 AND state='pending' AND address<>''
			AND NOT EXISTS (SELECT 1 FROM escrows es WHERE es.order_id=payouts.order_id)
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
		// Every payout pays its own network fee (feeFromAmountSender, A-232): the recipient receives amt less the fee.
		send, feeTaken := p.Send, ""
		if f, ok := p.(feeFromAmountSender); ok {
			send, feeTaken = f.SendFeeFromAmount, "; the network fee was taken from it"
		}
		txid, serr := send(sctx, to, amt)
		scancel()
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), payoutRecordTimeout)
		label := amount(amt, currencyDecimals(cur)) + " " + cur
		if serr != nil {
			ambiguous := !walletRejected(cur, serr)
			msg := "The wallet reported a pre-broadcast error; nothing was broadcast. "
			if ambiguous {
				msg = "Wallet call failed without a definite answer; the transaction may or may not have been broadcast. Check the wallet before requeueing or paying manually. "
			}
			note := a.netTag() + " payout of " + label + " failed and will not be retried automatically."
			err = a.recordPayout(rctx, id, order, recipient, "failed", "", msg+truncate(serr.Error(), 200), ambiguous, note, note)
			errs = append(errs, &watchError{label: fmt.Sprintf("payout %d", id), class: payoutErrorClass(cur, serr), err: serr})
		} else {
			// Only the recipient's notification names the txid: on Bitcoin it reveals their payout address (A-214).
			// The recipient received the amount less the network fee (A-250).
			note := a.netTag() + " payout of " + label + " sent" + feeTaken + "."
			err = a.recordPayout(rctx, id, order, recipient, "sent", txid, "", false, note, note+" Transaction ID: "+txid)
		}
		cancel()
		// Logged here, not only in the returned error: the watcher drops a pass's error once shutdown has begun,
		// and a send that could not be recorded leaves its txid nowhere else (A-141, A-107).
		recorded := "yes"
		if err != nil {
			recorded = "no cause=" + errorCause(err)
			errs = append(errs, &watchError{label: fmt.Sprintf("payout %d recorded as sending only", id), err: err})
		}
		outcome := "sent txid=" + txid
		if serr != nil {
			outcome = "failed error=" + payoutErrorClass(cur, serr)
		}
		log.Printf("payout id=%d order=%s currency=%s amount=%s outcome=%s recorded=%s", id, order[:min(8, len(order))], cur, amount(amt, currencyDecimals(cur)), outcome, recorded)
	}
	return errors.Join(errs...)
}

// payoutErrorClass names a failed send for the log without the wallet's error text: definite (refused before
// broadcasting, walletRejected) or ambiguous, then the JSON-RPC error code or errorCause's class.
func payoutErrorClass(cur string, err error) string {
	class := "ambiguous/"
	if walletRejected(cur, err) {
		class = "definite/"
	}
	var re *rpcError
	if errors.As(err, &re) {
		return class + fmt.Sprintf("rpc:%d", re.Code)
	}
	return class + errorCause(err)
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
// saving the tx info after commit), so only the codes in moneroPreSubmitCodes are definite, and only from the
// wallet they were verified against: an error from any other wallet version is unverifiedWalletError (A-174).
func walletRejected(cur string, err error) bool {
	var re *rpcError
	var uv *unverifiedWalletError
	if !errors.As(err, &re) || errors.As(err, &uv) {
		return false
	}
	return cur == "BTC" || (cur == "XMR" && moneroPreSubmitCodes[re.Code])
}

// moneroVerifiedWalletRelease is the monero-wallet-rpc release moneroPreSubmitCodes was verified against, the
// tag compose.nodes.yaml, .env.example and scripts/install.sh pin (tests/test_installer.py fails if they
// differ from it). monero-wallet-rpc does not report its release: get_version answers WALLET_RPC_VERSION, the
// version of its RPC interface as (major<<16)|minor, and "release", whether it is a release build
// (v0.18.5.1 src/wallet/wallet_rpc_server.cpp:4842-4847). v0.18.5.1 reports 1.31, a release build
// (src/wallet/wallet_rpc_server_commands_defs.h:49-52); v0.18.5.0 reports 1.30, v0.18.4.1-v0.18.4.5 1.29,
// and the release-v0.18 branch after v0.18.5.1 1.32. The minor is bumped with every change to the RPC, not
// with every release, so a later release that leaves the RPC unchanged would also report 1.31: the check tells
// apart every wallet whose RPC changed, not every build. Only moneroVerifiedWalletRPC with release true is
// verified; any other answer, none, or a check that failed leaves every transfer error ambiguous (fail safe).
const (
	moneroVerifiedWalletRelease = "v0.18.5.1"
	moneroVerifiedWalletRPC     = 1<<16 | 31
)

// unverifiedWalletError wraps a JSON-RPC error from a monero-wallet-rpc whose get_version is not the verified
// one: walletRejected treats it as ambiguous, whatever its code (errors.As still finds the rpcError).
type unverifiedWalletError struct {
	version string // the wallet's get_version as the last check recorded it ("RPC version 1.30, release build")
	err     error
}

func (e *unverifiedWalletError) Error() string {
	// The wallet's error first: the recorded payout error keeps only its first 200 bytes.
	return fmt.Sprintf("%v; monero-wallet-rpc %s is not verified (%s reports %s), so this error may follow a broadcast", e.err, e.version, moneroVerifiedWalletRelease, moneroRPCVersion(moneroVerifiedWalletRPC, true))
}

func (e *unverifiedWalletError) Unwrap() error { return e.err }

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
// unknown: requeueing it then needs the administrator's explicit confirmation (payments_admin.go). note goes into
// the order history, which both parties read; notice is the recipient's notification.
func (a *App) recordPayout(ctx context.Context, id int64, orderID, recipient, state, txid, msg string, ambiguous bool, note, notice string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A send whose outcome is not recorded leaves its txid only in the log, so recording waits for row locks
	// as long as its own bound (payoutRecordTimeout) allows, not the application pool's 3 s/8 s (A-177).
	if _, err = tx.ExecContext(ctx, "SELECT set_config('lock_timeout','0',true), set_config('statement_timeout','0',true)"); err != nil {
		return err
	}
	var orderState string
	if err = tx.QueryRowContext(ctx, "UPDATE payouts p SET state=$2,txid=$3,error=$4,send_ambiguous=$5,updated=now() FROM orders o WHERE p.id=$1 AND p.state='sending' AND o.id=p.order_id RETURNING o.state", id, state, txid, msg, ambiguous).Scan(&orderState); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", orderID, orderState, note); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), recipient, "Order "+orderID[:min(8, len(orderID))]+": "+notice); err != nil {
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
