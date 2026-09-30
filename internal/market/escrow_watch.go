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

// The escrow watcher (WR-escrow Phase 1, slice P1-b). One per deployment (its own PostgreSQL advisory lock), apart
// from the pooled watcher: it reads what the watch-only wallet received at each watched escrow address and writes it
// to the payments ledger, one row per output (applyIncoming; payments.settlement_id stays NULL until a settlement
// claims the output, P1-c). An escrow order awaiting payment whose confirmed outputs reach the price moves to paid;
// autoDeliveryHook delivers the content and escrowTransitionHook completes the order on funding through the
// escrow-only system edge delivered -> completed. Each move goes through transition (row lock, compare-and-set,
// order_events).
//
// No payouts row is ever written for an escrow order (settleTerminal; migration 074 refuses one), and nothing here
// signs or sends. What returns coins from an escrow is a settlement, which P1-c builds: a refund (after expiry,
// underpayment or cancellation), a dispute decision, a stray refund. Until then an escrow order holding coins is
// never cancelled or disputed (escrowTransitionHook), an underpaid one stays open after its payment window with one
// notice, and one that has received nothing is cancelled when its window ends, as a pooled order is. A confirmed
// deposit first recorded after an escrow order closed is flagged for payment review (flagLateDeposits).
//
// While escrow is switched on but not enabled (its node was down at startup, say) the watcher retries the startup
// checks each interval (retryEscrow) and does nothing else: no deposit is read and no order expires until they pass.

const escrowWatcherLock = 782495 // the pooled watcher's is 782494

// The escrow watcher's horizon for a closed order, in SQL interval text: the order changed within escrowClosedWatch
// or its escrow was created within escrowCreatedWatch (an open order is read whatever its age). escrowWatchedSQL is
// that test on the orders row o and escrows row es; the watcher's query and the order page's status share it, so the
// page never says "nothing was received" about an address that is no longer read (A-332).
const (
	escrowClosedWatch  = "24 hours"
	escrowCreatedWatch = "30 days"
	escrowWatchedSQL   = "(o.updated > now()-interval '" + escrowClosedWatch + "' OR es.created > now()-interval '" + escrowCreatedWatch + "')"
)

func init() {
	registerBackground(escrowWatcher)
	registerTransitionHook(escrowTransitionHook)
}

// escrowCompletedNote is the order history note of the completion on funding.
const escrowCompletedNote = "Completed on funding (Bitcoin escrow): the content was delivered. The vendor claims the escrowed coins by signing a settlement; that step is not available in this build yet."

var errEscrowSettlementLater = fail(409, "Not available for an escrow order yet: refunds and dispute decisions are paid from the escrow by the settlement step, which this build does not have yet (Bitcoin escrow Phase 1 runs on regtest only). Nothing was changed.")

// escrowWatcher runs while ESCROW_BTC=on. Until escrow is enabled (its node was down at startup, say) each pass first
// retries its checks (retryEscrow, A-316), so escrow starts without a restart once the node answers.
func escrowWatcher(ctx context.Context, a *App) {
	if !a.escrowConfigured {
		return
	}
	interval, err := pollInterval()
	if err != nil {
		interval = 30 * time.Second
	}
	retry := &escrowRetry{last: a.escrowStartState}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if a.retryEscrow(ctx, retry); a.escrow.Load() == nil {
			t.Reset(interval)
			continue
		}
		pass, cancel := context.WithTimeout(ctx, watcherPassTimeout)
		err := a.escrowPollOnce(pass)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("escrow watcher: %s", watcherCause(err))
		}
		t.Reset(interval)
	}
}

// escrowPollOnce runs one escrow watcher pass unless another process holds the escrow watcher lock. The node must
// still report regtest; an order whose address could not be read in this pass never expires in it.
func (a *App) escrowPollOnce(ctx context.Context) error {
	e := a.escrow.Load()
	if e == nil || a.db == nil {
		return nil
	}
	chain, err := e.chain(ctx)
	if err != nil {
		return &watchError{label: "escrow", class: "node check failed: " + errorClassOf(err), err: err}
	}
	if chain != "regtest" {
		return &watchError{label: "escrow", class: "node is not on regtest; pass skipped", err: errEscrowNotRegtest}
	}
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var locked bool
	if err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", escrowWatcherLock).Scan(&locked); err != nil || !locked {
		return err
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(uctx, "SELECT pg_advisory_unlock($1)", escrowWatcherLock); err != nil {
			conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	// Open escrow orders whatever their age, and closed ones within escrowWatchedSQL's horizon (their ledger stays
	// current for the order page and P1-c's stray-refund check).
	rows, err := a.db.QueryContext(ctx, `SELECT es.address,es.order_id FROM escrows es JOIN orders o ON o.id=es.order_id
		WHERE o.state IN ('awaiting_payment',`+fundedOpenStates+`) OR `+escrowWatchedSQL+`
		ORDER BY o.updated`)
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
	unread := map[string]bool{}
	for chunk := range slices.Chunk(addresses, 200) {
		incoming, err := e.incoming(ctx, chunk)
		if err == nil {
			err = a.applyIncoming(ctx, "BTC", incoming, chunk, orderOf)
		}
		if err != nil {
			errs = append(errs, &watchError{label: "escrow", class: "wallet read failed: " + errorClassOf(err), err: err})
			for _, addr := range chunk {
				unread[orderOf[addr]] = true
			}
		}
	}
	for _, id := range orders {
		if err = a.settleEscrow(ctx, id, !unread[id]); err != nil {
			errs = append(errs, &watchError{label: "escrow order " + shortID(id), err: err})
		}
	}
	return errors.Join(errs...)
}

// settleEscrow applies the ledger to one escrow order in its own transaction: funding, then the payment window.
func (a *App) settleEscrow(ctx context.Context, orderID string, walletRead bool) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", orderID))
	if err != nil {
		return err
	}
	threshold := a.escrow.Load().confirmations
	if o.Escrow && isTerminal(o.State) {
		// A deposit first recorded after the order closed is never counted; it is flagged for payment review, the
		// buyer and the staff told once per episode, as a pooled one is (A-211, QA31-3). It stays in the escrow.
		if err = a.flagLateDeposits(ctx, tx, &o, threshold, false); err != nil {
			return err
		}
		return tx.Commit()
	}
	if !o.Escrow || o.State != stateAwaitingPayment {
		return tx.Commit()
	}
	var total int64
	if err = tx.QueryRowContext(ctx, "SELECT amount FROM orders WHERE id=$1", orderID).Scan(&total); err != nil {
		return err
	}
	var sum int64
	var n int
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(sum(amount),0),count(*) FROM payments WHERE order_id=$1 AND confirmations>=$2 AND NOT locked", orderID, threshold).Scan(&sum, &n); err != nil {
		return err
	}
	if n > 0 && sum >= total {
		if _, err = tx.ExecContext(ctx, "UPDATE payments SET credited=true WHERE order_id=$1 AND confirmations>=$2 AND NOT locked", orderID, threshold); err != nil {
			return err
		}
		note := fmt.Sprintf("%s regtest escrow funded: %s BTC in %d output(s) with at least %d confirmations", a.netTag(), amount(sum, 8), n, threshold)
		if _, err = a.transition(ctx, tx, orderID, stateAwaitingPayment, statePaid, nil, note); err != nil {
			return err
		}
		if sum > total {
			// What happens to the excess is decided by the settlement (P1-c); it is noted, never paid out here.
			excess := priceDifference(sum, total, "BTC") + " was paid into this escrow; it stays in the escrow until its settlement, which this build does not have yet."
			if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) SELECT id,state,state,NULL,$2 FROM orders WHERE id=$1", orderID, excess); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	if !walletRead {
		return tx.Commit()
	}
	// The payment window, from the same inputs as a pooled order's (settleOrder): the escrow's creation and the
	// latest deposit that counts.
	var unpaid, underpaid, noted bool
	var received int64
	if err = tx.QueryRowContext(ctx, `WITH d AS (SELECT amount,confirmations,created FROM payments WHERE order_id=$1 AND confirmations>=0 AND NOT locked)
		SELECT es.created < now()-make_interval(secs => $2) AND NOT EXISTS (SELECT 1 FROM d),
			es.created < now()-make_interval(secs => $2) AND EXISTS (SELECT 1 FROM d) AND NOT EXISTS (SELECT 1 FROM d WHERE confirmations<$3)
				AND (SELECT max(created) FROM d) < now()-make_interval(secs => $2),
			(SELECT COALESCE(sum(amount),0) FROM d), es.underpaid_noted IS NOT NULL
		FROM escrows es WHERE es.order_id=$1`, orderID, expiryWindow().Seconds(), threshold).Scan(&unpaid, &underpaid, &received, &noted); err != nil {
		return err
	}
	switch {
	case unpaid:
		// Nothing received: cancelled as a pooled order is; the stock hook returns the unit, and there is nothing to
		// refund. Coins sent after this are a stray payment (recovered by a stray refund, P1-c).
		if _, err = a.transition(ctx, tx, orderID, stateAwaitingPayment, stateCancelled, nil, "payment window expired"); err != nil {
			return err
		}
	case underpaid && !noted:
		// Coins are in the escrow: the order stays open (a top-up to the price still completes it) and the parties
		// are told once; its refund is a settlement (P1-c).
		what := fmt.Sprintf("the payment window ended with %s of %s BTC received", amount(received, 8), amount(total, 8))
		note := "Escrow underpaid: " + what + ". The order stays open; a top-up to the full price completes it. Its refund is handled by the escrow settlement step, which this build does not have yet."
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", orderID, o.State, note); err != nil {
			return err
		}
		body := "Order " + shortID(orderID) + ": " + what + " into its escrow (" + a.netTag() + " regtest). The order stays open: a top-up to the full price completes it. Refunding an escrow is part of the settlement step, which this build of the market does not have yet."
		for _, uid := range []string{o.BuyerID, o.VendorID} {
			if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, body); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE escrows SET underpaid_noted=now() WHERE order_id=$1", orderID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// escrowTransitionHook keeps escrow orders on the moves Phase 1 slice P1-b supports. A move that would return or
// award the escrowed coins needs a settlement (P1-c), so it is refused while the escrow holds coins: a dispute, a
// cancellation of a paid order, and one of an order awaiting payment that has received a deposit. A delivered escrow
// order completes at once, by the system (the escrow-only edge; ES-4: digital orders complete on funding).
func escrowTransitionHook(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
	if !o.Escrow {
		return nil
	}
	switch {
	case to == stateDisputed, to == stateCancelled && from == statePaid:
		return errEscrowSettlementLater
	case to == stateCancelled && from == stateAwaitingPayment:
		if funded, err := hasDeposits(ctx, tx, o.ID); err != nil || funded {
			if err == nil {
				err = errEscrowSettlementLater
			}
			return err
		}
	case to == stateDelivered:
		next, err := a.transition(ctx, tx, o.ID, stateDelivered, stateCompleted, nil, escrowCompletedNote)
		if err != nil {
			return err
		}
		o.State, o.Status = next.State, next.Status
	}
	return nil
}
