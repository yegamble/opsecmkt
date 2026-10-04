package market

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The clocks of a physical Monero escrow order (WR-escrow Phase 3, slice X-e; P3-R7 with the Phase 2 rules P2-R3, P2-R5,
// P2-R9 and P2-R10's restore recheck). Behind ESCROW_XMR (off by default); fakechain only. They are escrow_deadline.go's,
// run by the Monero escrow watcher for Monero escrow orders (xmrDeadlines, the last step of its pass), so they apply with
// ESCROW_BTC off; the Bitcoin escrow watcher's escrowDeadlines handles Bitcoin escrow orders only.
//
//  1. Paid: ship_by and the buyer's no-ship cancel are shared with Bitcoin escrow (escrowTransitionHook, noShipCancel,
//     shipConflict); the cancel requests the refund (escrowSettleTerminal).
//  2. Shipped: from 72 hours before release_at the buyer is reminded once (reminded_at claimed by compare-and-set) and
//     the vendor told the buyer's deadline (xmrRemindDeadlines). A dispute filed after it is refused (lateDispute).
//  3. At release_at a shipped order with no dispute is completed by the system ("auto-release deadline passed") under
//     the order row lock (xmrReleaseDue); the release requested at shipment is then buildable (settlementFits).
//  4. After a restore from backup (scripts/restore.sh, or UPGRADING.md section 6, sets escrow_xmr_release_recheck) the
//     pass moves every passed deadline of a shipped order to now + 72 hours and tells the buyer
//     (xmrRecheckReleaseDeadlines), as recheckReleaseDeadlines does for Bitcoin escrow with its own setting.
//
// No step makes an RPC, and none runs while the restore gate is set: the recheck then runs in the first pass after the
// gate is cleared, before the release.

// xmrReleaseRecheckKey is the setting a restore from backup writes to ask for the Monero deadline recheck (step 4).
const xmrReleaseRecheckKey = "escrow_xmr_release_recheck"

// releaseRecheckKeyOf is the setting that asks for the deadline recheck of coin's escrow orders.
func releaseRecheckKeyOf(coin string) string {
	if coin == "XMR" {
		return xmrReleaseRecheckKey
	}
	return releaseRecheckKey
}

// xmrDeadlines is the deadline part of a Monero escrow watcher pass, under its lock: the restore recheck first, so a
// restored deadline is moved before it could be released, then the release, then the reminders.
func (a *App) xmrDeadlines(ctx context.Context) error {
	gate, err := restoreGateSet(ctx, a.db)
	if err != nil || gate {
		return err
	}
	var errs []error
	if err = a.xmrRecheckReleaseDeadlines(ctx); err != nil {
		errs = append(errs, &watchError{label: "monero escrow deadlines", class: "restore recheck failed: " + errorClassOf(err), err: err})
	}
	if err = a.xmrReleaseDue(ctx); err != nil {
		errs = append(errs, err)
	}
	if err = a.xmrRemindDeadlines(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// xmrReleaseDue completes every shipped Monero escrow order whose release deadline has passed with no dispute, each in
// its own transaction (step 3 of the file comment).
func (a *App) xmrReleaseDue(ctx context.Context) error {
	ids, err := orderIDs(ctx, a.db, `SELECT o.id FROM orders o JOIN escrows es ON es.order_id=o.id WHERE es.coin='XMR' AND o.state='shipped' AND `+releaseDueSQL+` AND `+noDisputeSQL+` ORDER BY es.release_at`)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err = a.xmrReleaseOne(ctx, id); err != nil {
			errs = append(errs, &watchError{label: "monero escrow order " + shortID(id), class: "deadline release failed: " + errorClassOf(err), err: err})
		}
	}
	return errors.Join(errs...)
}

func (a *App) xmrReleaseOne(ctx context.Context, orderID string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = a.xmrReleaseIfDue(ctx, tx, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

// xmrReleaseIfDue completes order in tx if it is still shipped, has no dispute and its release deadline has passed: the
// order row is locked first and checked in a statement after the lock (releaseIfDue's rule). It reports whether the
// order was completed.
func (a *App) xmrReleaseIfDue(ctx context.Context, tx *sql.Tx, orderID string) (bool, error) {
	if _, err := tx.ExecContext(ctx, "SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE", orderID); err != nil {
		return false, err
	}
	var due bool
	var at time.Time
	err := tx.QueryRowContext(ctx, `SELECT o.state='shipped' AND `+noDisputeSQL+` AND `+releaseDueSQL+`,es.release_at
		FROM orders o JOIN escrows es ON es.order_id=o.id WHERE o.id=$1 AND es.coin='XMR' AND es.release_at IS NOT NULL`, orderID).Scan(&due, &at)
	if err == sql.ErrNoRows || err == nil && !due {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	note := escrowDeadlineNote + " (" + utcUntil(at) + "): no dispute was opened by then, so the order is completed (Monero escrow). " +
		"The market builds the release of the escrowed coins to the address the vendor gave when marking the order shipped, and sends it."
	if _, err = a.transition(ctx, tx, orderID, stateShipped, stateCompleted, nil, note); err != nil {
		return false, err
	}
	return true, nil
}

// xmrRemindDeadlines reminds the buyer of every shipped Monero escrow order with no dispute whose release deadline is
// less than 72 hours away, once, and tells the vendor the buyer's deadline (step 2 of the file comment).
func (a *App) xmrRemindDeadlines(ctx context.Context) error {
	ids, err := orderIDs(ctx, a.db, `SELECT o.id FROM orders o JOIN escrows es ON es.order_id=o.id WHERE es.coin='XMR' AND o.state='shipped' AND es.reminded_at IS NULL AND `+releaseSoonSQL+`
		AND `+noDisputeSQL+` ORDER BY es.release_at`)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err = a.xmrRemindOne(ctx, id); err != nil {
			errs = append(errs, &watchError{label: "monero escrow order " + shortID(id), class: "deadline reminder failed: " + errorClassOf(err), err: err})
		}
	}
	return errors.Join(errs...)
}

// xmrRemindOne claims order's reminder by compare-and-set on reminded_at and sends it; another pass that claimed it
// first leaves nothing to send.
func (a *App) xmrRemindOne(ctx context.Context, orderID string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var buyer, vendor string
	var at time.Time
	err = tx.QueryRowContext(ctx, `UPDATE escrows es SET reminded_at=now() FROM orders o JOIN products p ON p.id=o.product_id
		WHERE es.order_id=$1 AND es.coin='XMR' AND o.id=es.order_id AND es.reminded_at IS NULL AND o.state='shipped' AND `+noDisputeSQL+` RETURNING o.buyer_id,p.vendor_id,es.release_at`, orderID).Scan(&buyer, &vendor, &at)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	until := utcUntil(at)
	for _, n := range [][2]string{
		{buyer, "Order " + shortID(orderID) + ": Reminder — you have until " + until + " to use Confirm receipt on the order page if the goods have arrived, or to open a dispute if they have not. If you do neither, the order completes at that time and the market releases the coins in this Monero escrow to the vendor; a dispute can no longer be opened after it."},
		{vendor, "Order " + shortID(orderID) + ": the buyer has until " + until + " to confirm receipt or open a dispute. If the buyer does neither, the order completes at that time and the market releases the coins in this Monero escrow to you."},
	} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), n[0], n[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// xmrRecheckReleaseDeadlines moves, once after each restore from backup, every passed release deadline of a shipped
// Monero escrow order with no dispute to now + 72 hours, with an order note and a notice to the buyer, in one
// transaction that consumes the restore's request (step 4 of the file comment; recheckReleaseDeadlines' rules: only the
// rechecks reset and set release_recheck, so a second restore rechecks as the first did).
func (a *App) xmrRecheckReleaseDeadlines(ctx context.Context) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var asked bool
	err = tx.QueryRowContext(ctx, "SELECT true FROM settings WHERE key=$1 FOR UPDATE", xmrReleaseRecheckKey).Scan(&asked)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	ids, err := orderIDs(ctx, tx, `SELECT o.id FROM orders o JOIN escrows es ON es.order_id=o.id WHERE es.coin='XMR' AND o.state='shipped' AND `+releaseDueSQL+` ORDER BY o.id`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, "SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE", id); err != nil {
			return err
		}
		var due bool
		if err = tx.QueryRowContext(ctx, `SELECT o.state='shipped' AND `+releaseDueSQL+` AND `+noDisputeSQL+` FROM orders o JOIN escrows es ON es.order_id=o.id WHERE o.id=$1 AND es.coin='XMR'`, id).Scan(&due); err != nil {
			return err
		}
		if !due {
			continue
		}
		if _, err = tx.ExecContext(ctx, "UPDATE escrows SET release_recheck=false WHERE order_id=$1 AND coin='XMR' AND release_recheck", id); err != nil {
			return err
		}
		var at time.Time
		var buyer string
		if err = tx.QueryRowContext(ctx, `UPDATE escrows es SET release_at=now()+interval '72 hours',release_recheck=true,reminded_at=COALESCE(es.reminded_at,now())
			FROM orders o WHERE o.id=es.order_id AND es.order_id=$1 AND es.coin='XMR' RETURNING es.release_at,o.buyer_id`, id).Scan(&at, &buyer); err != nil {
			return err
		}
		until := utcUntil(at)
		note := "Release deadline moved to " + until + " after a restore from backup (Monero escrow): the deadline in the restored records had passed, so the buyer has 72 more hours to confirm receipt or open a dispute."
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", id, stateShipped, note); err != nil {
			return err
		}
		body := "Order " + shortID(id) + ": this market restored its records from a backup, and the release deadline of this order had passed, so it was moved to " + until +
			". Use Confirm receipt on the order page if the goods have arrived, or open a dispute before then if they have not. If you do neither, the order completes at that time and the market releases the coins in this Monero escrow to the vendor."
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), buyer, body); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", xmrReleaseRecheckKey); err != nil {
		return err
	}
	return tx.Commit()
}
