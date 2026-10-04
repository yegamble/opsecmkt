package market

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

// The stray refund (WR-escrow Phase 1, slice P1-c2; ES-8, A-322, A-327). Bitcoin escrows stay imported in the
// watch-only wallet with no age limit. Coins first recorded at an escrow after its order closed (payments.after_close)
// and outputs a bounded build left out (payments.escrow_left_out, A-342) are not the order's: once confirmed, the
// escrow watcher requests a stray_refund settlement returning them to the buyer's pinned refund address, which either
// party signs and the market co-signs (the same pipeline as a claim or refund). One is in progress per order at a
// time (migration 076); a later deposit gets the next one.
//
// The watcher reads closed escrows only within its horizon (escrowWatchedSQL), so a deposit to an older escrow, or to
// one cancelled while escrow was unavailable, is found by:
//   - the buyer's "Recover coins at this escrow" (/escrow/recover, password step-up): the next pass reads that escrow
//     and requests the stray refund of what it finds, or tells the buyer it found nothing;
//   - the daily check: once every escrowStrayCheck the pass reads every escrow, whatever its age. The first check runs
//     escrowStrayCheck after escrow first runs on a database (settings key escrow_btc_stray_check).

func init() {
	registerAction("/escrow/recover", actionSpec{OwnTx: true, OneAtATime: true, Off: escrowOff, Run: escrowRecoverAction})
}

const (
	escrowStrayCheck        = 24 * time.Hour
	escrowStrayCheckSetting = "escrow_btc_stray_check" // settings key: when the last daily check read every escrow
	// strayEligibleSQL selects the payments rows p a stray refund returns, for a query with the threshold as $2.
	strayEligibleSQL = "p.settlement_id IS NULL AND NOT p.locked AND NOT p.spent_outside AND p.confirmations>=$2 AND (p.after_close OR p.escrow_left_out)"
)

var (
	errRecoverOnlyBuyer = fail(403, "Only the buyer recovers coins sent to this escrow: they go back to the buyer's refund address.")
	errRecoverState     = fail(409, "Coins at this escrow are recovered once its order is closed (completed, cancelled or resolved). Nothing was changed.")
	errRecoverOpen      = fail(409, "A recovery of coins at this escrow is already in progress; it is shown on the order page. Nothing was changed.")
	errRecoverAsked     = fail(409, "You already asked; the market checks this escrow on its next pass. Reload the page in a minute. Nothing was changed.")
)

// strayCheckDue reports whether this pass is the daily check. On a database where escrow has not run before, it
// records now as the last check, so the first daily check runs escrowStrayCheck later.
func (a *App) strayCheckDue(ctx context.Context) (bool, error) {
	if _, err := a.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')) ON CONFLICT (key) DO NOTHING", escrowStrayCheckSetting); err != nil {
		return false, err
	}
	var due bool
	err := a.db.QueryRowContext(ctx, "SELECT value::timestamptz < now()-make_interval(secs => $2) FROM settings WHERE key=$1", escrowStrayCheckSetting, escrowStrayCheck.Seconds()).Scan(&due)
	return due, err
}

// strayChecked records a completed daily check.
func (a *App) strayChecked(ctx context.Context) error {
	_, err := a.db.ExecContext(ctx, "UPDATE settings SET value=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') WHERE key=$1", escrowStrayCheckSetting)
	return err
}

// requestStrays requests a stray refund for each closed escrow order of this pass (read without error) that has coins
// to return or a buyer's request to answer.
func (a *App) requestStrays(ctx context.Context, threshold int64, orders []string, unread map[string]bool) error {
	var read []string
	for _, id := range orders {
		if !unread[id] {
			read = append(read, id)
		}
	}
	if len(read) == 0 {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT es.order_id FROM escrows es JOIN orders o ON o.id=es.order_id
		WHERE es.order_id=ANY($1) AND es.coin='BTC' AND o.state IN ('completed','cancelled','resolved')
		AND (es.recover_requested IS NOT NULL OR EXISTS (SELECT 1 FROM payments p WHERE p.order_id=es.order_id AND `+strayEligibleSQL+`)) ORDER BY es.id`, read, threshold)
	if err != nil {
		return err
	}
	var due []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		due = append(due, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, id := range due {
		if err = a.requestStray(ctx, threshold, id); err != nil {
			return &watchError{label: "escrow order " + shortID(id), class: "stray refund not requested: " + errorCause(err), err: err}
		}
	}
	return nil
}

// requestStray requests one order's stray refund (state building, to the pinned refund address), or answers the
// buyer's request when there is nothing to return, in one transaction under the escrow row's lock.
func (a *App) requestStray(ctx context.Context, threshold int64, orderID string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var requested, open bool
	var buyer, state string
	var sum, n, pending int64
	if err = tx.QueryRowContext(ctx, `SELECT es.recover_requested IS NOT NULL,o.buyer_id,o.state,
			EXISTS(SELECT 1 FROM settlements s WHERE s.order_id=es.order_id AND s.kind='stray_refund' AND s.state IN ('building','awaiting_signature','party_signed','signed','broadcast')),
			(SELECT COALESCE(sum(p.amount),0) FROM payments p WHERE p.order_id=es.order_id AND `+strayEligibleSQL+`),
			(SELECT count(*) FROM payments p WHERE p.order_id=es.order_id AND `+strayEligibleSQL+`),
			(SELECT count(*) FROM payments p WHERE p.order_id=es.order_id AND p.settlement_id IS NULL AND NOT p.locked AND p.confirmations BETWEEN 0 AND $2-1 AND (p.after_close OR p.escrow_left_out))
		FROM escrows es JOIN orders o ON o.id=es.order_id WHERE es.order_id=$1 AND es.coin='BTC' FOR UPDATE OF es`, orderID, threshold).Scan(&requested, &buyer, &state, &open, &sum, &n, &pending); err != nil {
		return err
	}
	if !isTerminal(state) {
		return nil
	}
	note, body := "", ""
	switch {
	case n > 0 && !open:
		var by any
		if requested {
			by = buyer
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO settlements(order_id,kind,destination,requested_by) SELECT order_id,'stray_refund',refund_address,$2 FROM escrows WHERE order_id=$1`, orderID, by); err != nil {
			return err
		}
		note = "Stray refund requested: " + amount(sum, 8) + " BTC in " + strconv.FormatInt(n, 10) + " deposit(s) at this escrow that do not count for the order (sent once it had closed, or left out of its settlement) go back, less the network fee, to the buyer's refund address given at checkout. The market builds the transaction; the buyer or the vendor signs it on the order page and the market co-signs it. Nothing is sent yet."
		body = note
	case requested && !open:
		note = "The market checked this escrow at the buyer's request and found no coins to recover."
		if pending > 0 {
			note += " A deposit to it is still confirming; it is recovered once it has " + strconv.FormatInt(threshold, 10) + " confirmations."
		}
		body = note
	}
	if requested {
		if _, err = tx.ExecContext(ctx, "UPDATE escrows SET recover_requested=NULL WHERE order_id=$1 AND coin='BTC'", orderID); err != nil {
			return err
		}
	}
	if note != "" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", orderID, state, note); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), buyer, "Order "+shortID(orderID)+": "+body); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// escrowRecoverAction is the buyer's "Recover coins at this escrow" (see the file comment): confirmed with the buyer's
// password (and authenticator code when enrolled), on a closed escrow order with no stray refund in progress.
func escrowRecoverAction(c *actionCtx) (actionResult, error) {
	if c.A.escrow.Load() == nil {
		return actionResult{}, errEscrowDown
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		o, err := partyOrder(c, c.Form.Get("order_id"))
		switch {
		case err != nil:
			return actionResult{}, err
		case !o.btcEscrow():
			return actionResult{}, errNoOrder
		case o.BuyerID != c.User.ID:
			return actionResult{}, errRecoverOnlyBuyer
		case !isTerminal(o.State):
			return actionResult{}, errRecoverState
		}
		var open bool
		if err = c.Tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM settlements WHERE order_id=$1 AND kind='stray_refund'
			AND state IN ('building','awaiting_signature','party_signed','signed','broadcast'))`, o.ID).Scan(&open); err != nil {
			return actionResult{}, err
		}
		if open {
			return actionResult{}, errRecoverOpen
		}
		res, err := c.Tx.ExecContext(ctx, "UPDATE escrows SET recover_requested=now() WHERE order_id=$1 AND coin='BTC' AND recover_requested IS NULL", o.ID)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return actionResult{}, errRecoverAsked
		}
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", o.ID, o.State, c.User.ID,
			"The buyer asked the market to check this escrow for coins sent to it after the order closed. The market reads it on its next pass and returns what it finds to the buyer's refund address by a stray refund."); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: orderPage(o.ID) + "#escrow-recover", Notice: "escrow-recover", Audit: "Asked to recover coins at the escrow of order " + shortID(o.ID) + " (" + c.A.escrowNetwork() + ")"}, nil
	})
}

// escrowStrayView adds the order's stray refunds (newest first) to the escrow panel and, for the buyer of a closed
// order, the recovery form (or the note that the next pass checks the escrow).
func (a *App) escrowStrayView(ctx context.Context, d *PageData, v *EscrowOrderView) error {
	o := d.Order
	rows, err := a.db.QueryContext(ctx, "SELECT id FROM settlements WHERE order_id=$1 AND kind='stray_refund' ORDER BY id DESC LIMIT 20", o.ID)
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
	open := false
	for _, id := range ids {
		s, err := a.settlementView(ctx, d, v, id)
		if err != nil {
			return err
		}
		s.Stray = true
		switch s.State {
		case "building", "awaiting_signature", "party_signed", "signed", "broadcast":
			open = true
		}
		v.Settlements = append(v.Settlements, s)
	}
	if d.OrderViewer != roleBuyer || !isTerminal(o.State) || open {
		return nil
	}
	var asked sql.NullTime
	if err = a.db.QueryRowContext(ctx, "SELECT recover_requested FROM escrows WHERE order_id=$1 AND coin='BTC'", o.ID).Scan(&asked); err != nil {
		return err
	}
	switch {
	case asked.Valid:
		v.RecoverAsked = true
	case v.Available:
		v.Recover = true
		if d.Security == nil {
			d.Security, _, _, err = a.securityView(ctx, d.User.ID)
		}
	}
	return err
}
