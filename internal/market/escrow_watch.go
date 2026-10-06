package market

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"net/http"
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
// No payouts row is ever written for an escrow order (settleTerminal; migration 074 refuses one). What returns coins
// from an escrow is a settlement (escrow_settle.go): the watcher builds each requested claim or refund at the end of
// its pass (buildSettlements), then co-signs party-signed ones, stores and broadcasts them and follows them to their
// confirmation (escrow_cosign.go, slice P1-c2). A dispute on an escrow order is decided by an administrator and its
// decision is paid by a settlement (escrow_decision.go). An escrow order that has received nothing when its payment
// window ends is cancelled, as a pooled order is; one underpaid then is cancelled too and its refund requested. A confirmed deposit
// first recorded after an escrow order closed is flagged for payment review (flagLateDeposits); one that did not count
// when the order was cancelled and has confirmed since is left out for the stray refund (leaveOutUncounted, A-428).
//
// While escrow is switched on but not enabled (its node was down at startup, or on another chain than the one pinned for
// the database's escrows, say) the watcher retries the startup checks each interval (retryEscrow) and does nothing else
// but apply the release deadlines, which need no node (escrow_deadline.go): no deposit is read and no order expires
// until they pass. The same holds for each pass while the node is not synced (A-439, bitcoin_sync.go).

const escrowWatcherLock = 782495 // the pooled watcher's is 782494

// xmrEscrowWatcherLock is the Monero escrow watcher's session advisory lock (escrow_xmr_watch.go), apart from both, so
// a slow Monero wallet pass never holds up the Bitcoin escrow or the pooled watcher.
const xmrEscrowWatcherLock = 782496

// The escrow watcher's horizon for a closed order, in SQL interval text: the order changed within escrowClosedWatch
// or its escrow was created within escrowCreatedWatch (an open order is read whatever its age), or it has a
// settlement not yet signed (its build waits for the ledger's confirmations, P1-c1). escrowWatchedSQL is that test on
// the orders row o and escrows row es; the watcher's query and the order page's status share it, so the page never
// says "nothing was received" about an address that is no longer read (A-332).
const (
	escrowClosedWatch  = "24 hours"
	escrowCreatedWatch = "30 days"
	escrowWatchedSQL   = "(o.updated > now()-interval '" + escrowClosedWatch + "' OR es.created > now()-interval '" + escrowCreatedWatch +
		"' OR EXISTS (SELECT 1 FROM settlements s WHERE s.order_id=es.order_id AND s.state IN ('building','awaiting_signature','party_signed','signed','broadcast')))"
)

// escrowNetwork is the chain the escrow node reported at startup, for the network word in escrow notes ("regtest" in
// Phase 1; also before escrow is enabled, when no node has been read).
func (a *App) escrowNetwork() string {
	if e := a.escrow.Load(); e != nil && e.network != "" {
		return e.network
	}
	return "regtest"
}

func init() {
	registerBackground(escrowWatcher)
	registerTransitionHook(escrowTransitionHook)
	registerLoader("admin", escrowNodeLoader)
}

// escrowCompletedNote is the order history note of the completion on funding.
const escrowCompletedNote = "Completed on funding (Bitcoin escrow): the content was delivered. The vendor claims the escrowed coins from the order page."

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
		a.retryEscrow(ctx, retry)
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
// still report the chain escrow started on (regtest: startEscrow refuses any other) and be synced (bitcoinNode.notSynced,
// A-439); an order whose address could not be read in this pass never expires in it. While escrow is not enabled, the
// node check fails or the node is not synced, the pass applies only the release deadlines, which need no node
// (escrow_deadline.go): no deposit is read, no order expires, nothing is flagged and no settlement is built, co-signed
// or broadcast. A node that is not synced is not an error: its state is logged once per change (noteEscrowSync).
func (a *App) escrowPollOnce(ctx context.Context) error {
	e := a.escrow.Load()
	if a.db == nil || e == nil && !a.escrowConfigured {
		return nil
	}
	var nodeErr error
	notSynced := false
	if e != nil {
		node, err := e.blockchainInfo(ctx)
		if err == nil && node.Chain != "regtest" {
			node.Connections, err = e.connections(ctx)
		}
		switch {
		case err != nil:
			nodeErr = &watchError{label: "escrow", class: "node check failed: " + errorClassOf(err), err: err}
		case escrowChainID(node.Chain, node.SignetChallenge) != e.chainID:
			nodeErr = &watchError{label: "escrow", class: "node changed chain; pass skipped", err: errEscrowNotRegtest}
		default:
			why := node.notSynced(time.Now(), maxTipAge())
			a.noteEscrowSync(why)
			notSynced = why != ""
		}
	}
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return errors.Join(nodeErr, err)
	}
	defer conn.Close()
	var locked bool
	if err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", escrowWatcherLock).Scan(&locked); err != nil || !locked {
		return errors.Join(nodeErr, err)
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(uctx, "SELECT pg_advisory_unlock($1)", escrowWatcherLock); err != nil {
			conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if e == nil || nodeErr != nil || notSynced {
		return errors.Join(nodeErr, a.escrowDeadlines(ctx))
	}
	// Open escrow orders whatever their age, closed ones within escrowWatchedSQL's horizon (their ledger stays current
	// for the order page) and ones whose buyer asked to recover coins at them; on the daily check, every escrow
	// (escrow_stray.go).
	daily, err := a.strayCheckDue(ctx)
	if err != nil {
		return err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT es.address,es.order_id FROM escrows es JOIN orders o ON o.id=es.order_id
		WHERE es.coin='BTC' AND (o.state IN ('awaiting_payment',`+fundedOpenStates+`) OR `+escrowWatchedSQL+` OR es.recover_requested IS NOT NULL OR $1)
		ORDER BY o.updated`, daily)
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
	// Deposits with no settlement that another transaction spent are flagged before they could count (escrow_recovery.go).
	if err = a.findOutsideSpends(ctx, e, orders, unread); err != nil {
		errs = append(errs, err)
	}
	for _, id := range orders {
		if err = a.settleEscrow(ctx, id, !unread[id]); err != nil {
			errs = append(errs, &watchError{label: "escrow order " + shortID(id), err: err})
		}
	}
	// Coins at a closed escrow that are not the order's go back by a stray refund (escrow_stray.go).
	if err = a.requestStrays(ctx, e.confirmations, e.feeRate, orders, unread); err != nil {
		errs = append(errs, err)
	}
	// The daily check also lists the watch-only wallet's addresses with no escrow row and reserves their keys
	// (escrow_recovery.go); after a restore that orphan sweep runs on every pass until one completes (P2-R10, A-366).
	sweep, err := escrowSweepRequired(ctx, a.db)
	if err != nil {
		errs = append(errs, err)
	} else if daily || sweep {
		if err = a.reconcileOrphans(ctx, e); err != nil {
			errs = append(errs, err)
		}
	}
	if daily && len(unread) == 0 {
		if err = a.strayChecked(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	// Under the same lock, after the ledger is current: confirmed settlements are followed until they are deep enough
	// that a reorganisation is not expected (trackConfirmeds); settlements broadcast in an earlier pass are followed to
	// their confirmation; those waiting for a signature are checked for an input spent without the market (trackAwaiting,
	// A-356); those requested so far (a claim, or the refund a cancellation above may just have requested) are built;
	// party-signed ones are co-signed and stored; and stored ones are broadcast (escrow_cosign.go).
	// Then fee bumps of broadcast settlements are built, co-signed and sent (escrow_bump.go, P2-d).
	// While the restore gate is set (escrow_recovery.go) nothing is built or co-signed, and broadcastSettlements and
	// trackBroadcasts send only what an administrator confirmed; watching continues.
	// The release deadlines (escrow_deadline.go) come first, so an order completed at its deadline is co-signed below in
	// the same pass.
	if err = a.escrowDeadlines(ctx); err != nil {
		errs = append(errs, err)
	}
	gate, err := restoreGateSet(ctx, a.db)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	steps := []func(context.Context, *escrowBTC) error{a.trackConfirmeds, a.trackBroadcasts, a.trackAwaiting, a.buildSettlements, a.cosignSettlements, a.broadcastSettlements,
		a.buildBumps, a.cosignBumps, a.broadcastBumps}
	if gate {
		steps = []func(context.Context, *escrowBTC) error{a.trackConfirmeds, a.trackBroadcasts, a.trackAwaiting, a.broadcastSettlements}
	}
	for _, step := range steps {
		if ctx.Err() != nil {
			break
		}
		if err = step(ctx, e); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// noteEscrowSync records why (a bitcoinNode reason class, "" when synced) as the escrow node's sync state for the admin
// page and logs one fixed line when it changes. In memory only: nothing is stored for the guard (A-439).
func (a *App) noteEscrowSync(why string) {
	prev, _ := a.escrowSync.Swap(why).(string)
	switch {
	case prev == why:
	case why == "":
		log.Print("escrow: Bitcoin escrow node synced; escrow watcher passes resumed")
	default:
		log.Printf("escrow: Bitcoin escrow node not synced (%s); escrow watcher passes skipped until it is", why)
	}
}

// escrowNodeLoader shows on /admin, under Payment providers, why the Bitcoin escrow node is not used: the reason class
// it is not synced for (with syncRemedy's advice when there is one), or the pinned chain and the node's when
// startEscrow refused it.
func escrowNodeLoader(_ context.Context, a *App, _ *http.Request, d *PageData) error {
	if !a.escrowConfigured || d.User == nil || d.User.Role != "admin" {
		return nil
	}
	v := &EscrowNodeView{Mismatch: a.escrowMismatch.Load()}
	if e := a.escrow.Load(); e != nil {
		v.NotSynced, _ = a.escrowSync.Load().(string)
		v.Remedy = syncRemedy(e.network, v.NotSynced)
	}
	if v.NotSynced != "" || v.Mismatch != nil {
		d.EscrowNode = v
	}
	return nil
}

// settleEscrow applies the ledger to one escrow order in its own transaction: deposits its release or refund does not
// spend are left out (leaveOutTopUps; on a cancelled order also leaveOutUncounted) or, on a closed order, flagged
// (flagLateDeposits) for the stray refund; an order awaiting payment is funded, or cancelled at the end of its payment
// window (unpaid, or underpaid).
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
	if o.Escrow && o.State != stateDraft && o.State != stateAwaitingPayment {
		// P2-R6, A-374: a deposit no settlement spends, once the order's release or refund is built, is left out for the
		// stray refund (escrow_release.go); an open order's top-up is otherwise never paid or flagged.
		if err = a.leaveOutTopUps(ctx, tx, &o); err != nil {
			return err
		}
	}
	if o.Escrow && isTerminal(o.State) {
		// A-428: a deposit that did not count when the order was cancelled (conflicted, or no longer listed) and has
		// confirmed since is left out for the stray refund, unless the order's refund spends it.
		if err = a.leaveOutUncounted(ctx, tx, &o, threshold); err != nil {
			return err
		}
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
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(sum(amount),0),count(*) FROM payments WHERE order_id=$1 AND confirmations>=$2 AND NOT locked AND NOT spent_outside", orderID, threshold).Scan(&sum, &n); err != nil {
		return err
	}
	if n > 0 && sum >= total {
		if _, err = tx.ExecContext(ctx, "UPDATE payments SET credited=true WHERE order_id=$1 AND confirmations>=$2 AND NOT locked AND NOT spent_outside", orderID, threshold); err != nil {
			return err
		}
		note := fmt.Sprintf("%s %s escrow funded: %s BTC in %d output(s) with at least %d confirmations", a.netTag(), a.escrowNetwork(), amount(sum, 8), n, threshold)
		if _, err = a.transition(ctx, tx, orderID, stateAwaitingPayment, statePaid, nil, note); err != nil {
			return err
		}
		if sum > total {
			// What happens to the excess is decided by the settlement (P1-c); it is noted, never paid out here.
			excess := priceDifference(sum, total, "BTC") + " was paid into this escrow; the vendor's claim returns it to the buyer's refund address (or adds it to the vendor's output when it is below " + amount(payoutFloor("BTC", "release"), 8) + " BTC)."
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
	var unpaid, underpaid bool
	var received int64
	if err = tx.QueryRowContext(ctx, `WITH d AS (SELECT amount,confirmations,created FROM payments WHERE order_id=$1 AND confirmations>=0 AND NOT locked)
		SELECT es.created < now()-make_interval(secs => $2) AND NOT EXISTS (SELECT 1 FROM d),
			es.created < now()-make_interval(secs => $2) AND EXISTS (SELECT 1 FROM d) AND NOT EXISTS (SELECT 1 FROM d WHERE confirmations<$3)
				AND (SELECT max(created) FROM d) < now()-make_interval(secs => $2),
			(SELECT COALESCE(sum(amount),0) FROM d)
		FROM escrows es WHERE es.order_id=$1 AND es.coin='BTC'`, orderID, expiryWindow().Seconds(), threshold).Scan(&unpaid, &underpaid, &received); err != nil {
		return err
	}
	switch {
	case unpaid:
		// Nothing received: cancelled as a pooled order is; the stock hook returns the unit, and there is nothing to
		// refund. Coins sent after this are a stray payment (recovered by a stray refund, P1-c2).
		if _, err = a.transition(ctx, tx, orderID, stateAwaitingPayment, stateCancelled, nil, "payment window expired"); err != nil {
			return err
		}
	case underpaid:
		// Every deposit confirmed, short of the price, and no top-up for a whole window: cancelled as a pooled order
		// is, and the cancellation requests the refund of the escrow (settleTerminal), built by this watcher.
		what := fmt.Sprintf("%s of %s BTC received", amount(received, 8), amount(total, 8))
		if _, err = a.transition(ctx, tx, orderID, stateAwaitingPayment, stateCancelled, nil, "payment window expired underpaid: "+what); err != nil {
			return err
		}
		body := "Order " + shortID(orderID) + ": the payment window ended with " + what + " into its escrow (" + a.netTag() + " " + a.escrowNetwork() + "), so the order was cancelled. " +
			"The market builds a refund of the escrowed coins, less the network fee, to the refund address you gave at checkout; you or the vendor then sign it on the order page. " + a.settleNotSent()
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), o.BuyerID, body); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// escrowTransitionHook keeps escrow orders on the moves Phase 1 supports. Cancelling an escrow order that holds coins
// requests its refund, and resolving its dispute requests the settlement of the decision (settleTerminal); only an
// administrator resolves it (resolveAction). A delivered escrow order completes at once, by the system (the
// escrow-only edge; ES-4: digital orders complete on funding). A paid physical escrow order gets its ship-by time.
func escrowTransitionHook(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
	if !o.Escrow {
		return nil
	}
	switch {
	case to == statePaid && o.Kind == "physical":
		// P2-R5: the buyer may cancel from the paid time plus the escrow's dispatch window plus 48 hours (escrow_deadline.go).
		_, err := tx.ExecContext(ctx, "UPDATE escrows SET ship_by=now()+make_interval(days => dispatch_days)+interval '48 hours' WHERE order_id=$1 AND dispatch_days IS NOT NULL AND ship_by IS NULL", o.ID)
		return err
	case to == stateDelivered:
		note := escrowCompletedNote
		if o.Currency == "XMR" {
			note = xmrEscrowCompletedNote
		}
		next, err := a.transition(ctx, tx, o.ID, stateDelivered, stateCompleted, nil, note)
		if err != nil {
			return err
		}
		o.State, o.Status = next.State, next.Status
	}
	return nil
}
