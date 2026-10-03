package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// The two clocks of a physical escrow order (WR-escrow Phase 2, slice P2-c; P2-R3, P2-R5, P2-R9 and P2-R10's restore
// recheck; owner decisions ES-11 and 2026-10-03). Behind ESCROW_BTC (off by default); regtest only.
//
//  1. Paid: escrows.ship_by is the paid time plus the escrow's dispatch window plus 48 hours (escrowTransitionHook).
//     From then on the buyer may cancel the paid order (paid -> cancelled, buyer, escrow only); before it the cancel is
//     refused (409) with the time stated (noShipCancel). The cancel and the vendor's "Mark shipped" both lock the order
//     row and compare-and-set its state, so exactly one takes effect; the other is told which one did and when. The
//     cancel requests the refund (escrowSettleTerminal), which the buyer or the vendor signs and the market co-signs.
//  2. Shipped: escrows.release_at (escrow_release.go). From 72 hours before it the buyer is reminded once (reminded_at
//     claimed by compare-and-set) and the vendor told the buyer's deadline (remindDeadlines). A dispute filed after it
//     is refused: the release is due (lateDispute).
//  3. At release_at a shipped order with no dispute is completed by the system ("auto-release deadline passed") under
//     the order row lock (releaseDue); the co-sign step then co-signs the release with the stored party signature (the
//     vendor's, or the buyer's), or the release waits for the vendor's signature. No RPC is made: it runs when the
//     node check fails, and not while the restore gate is set. A dispute stops the clock for good: a disputed order
//     only goes to resolved.
//  4. After a restore from backup (scripts/restore.sh, or UPGRADING.md section 6, sets escrow_release_recheck) the next
//     pass moves every passed deadline of a shipped order to now + 72 hours and tells the buyer
//     (recheckReleaseDeadlines). It is the only code that writes release_recheck: reset, then set in the statement that
//     moves release_at, as migration 079's trigger allows, so a second restore rechecks as the first did.
//
// The deadline is shown in UTC, to the minute, on the order page (with the time left when the page loaded), on /orders
// under 72 hours (deadlineBanners) and in every shipped, reminder, release and dispute notice.

func init() {
	registerLoader("orders", deadlineBanners)
}

const (
	// releaseRecheckKey is the setting a restore from backup writes to ask for the deadline recheck (step 4).
	releaseRecheckKey = "escrow_release_recheck"
	// escrowDeadlineNote opens the order history note of a release at the deadline (step 3).
	escrowDeadlineNote = "auto-release deadline passed"
	// releaseDueSQL: the release deadline of the escrow es has passed (step 3); the release, its recheck and a late
	// dispute share it.
	releaseDueSQL = "es.release_at <= now()"
	// releaseSoonSQL: the release deadline of the escrow es is less than 72 hours away (the reminder and the banner).
	releaseSoonSQL = "es.release_at > now() AND es.release_at <= now()+interval '72 hours'"
	// noDisputeSQL: the order o has no dispute row.
	noDisputeSQL = "NOT EXISTS (SELECT 1 FROM disputes d WHERE d.order_id=o.id)"
)

var errCancelNoShipBy = fail(409, "Only the vendor can cancel this paid order: a buyer may cancel a paid escrow order only when it is a physical order the vendor has not marked shipped within its dispatch window plus 48 hours. Nothing was changed.")

// utcUntil is a deadline as the market states it, in UTC to the minute, rounded down: the party acts before it.
func utcUntil(t time.Time) string {
	return t.UTC().Truncate(time.Minute).Format("2006-01-02 15:04 UTC")
}

// utcFrom is the time something becomes possible, in UTC to the minute, rounded up: it is possible from then.
func utcFrom(t time.Time) string {
	u := t.UTC()
	if m := u.Truncate(time.Minute); !m.Equal(u) {
		u = m.Add(time.Minute)
	}
	return u.Format("2006-01-02 15:04 UTC")
}

// timeLeft is the time left before a deadline, as computed when the page loaded.
func timeLeft(d time.Duration) string {
	days, hours := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour)
	switch {
	case d < time.Hour:
		return "Less than an hour left"
	case days == 0:
		return fmt.Sprintf("About %d h left", hours)
	case days == 1:
		return fmt.Sprintf("About 1 day %d h left", hours)
	}
	return fmt.Sprintf("About %d days %d h left", days, hours)
}

// orderIDs lists the order IDs a query selects.
func orderIDs(ctx context.Context, q rowsQuerier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// escrowDeadlines is the deadline part of an escrow watcher pass, under the escrow watcher lock: the restore recheck
// first, so a restored deadline is moved before it could be released, then the release (not while the restore gate is
// set), then the reminders. None makes an RPC, so it also runs when the node check fails (escrowPollOnce).
func (a *App) escrowDeadlines(ctx context.Context) error {
	var errs []error
	if err := a.recheckReleaseDeadlines(ctx); err != nil {
		errs = append(errs, &watchError{label: "escrow deadlines", class: "restore recheck failed: " + errorClassOf(err), err: err})
	}
	gate, err := restoreGateSet(ctx, a.db)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if !gate {
		if err = a.releaseDue(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if err = a.remindDeadlines(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// releaseDue completes every shipped escrow order whose release deadline has passed with no dispute, each in its own
// transaction (step 3 of the file comment).
func (a *App) releaseDue(ctx context.Context) error {
	ids, err := orderIDs(ctx, a.db, `SELECT o.id FROM orders o JOIN escrows es ON es.order_id=o.id WHERE o.state='shipped' AND `+releaseDueSQL+` AND `+noDisputeSQL+` ORDER BY es.release_at`)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err = a.releaseOne(ctx, id); err != nil {
			errs = append(errs, &watchError{label: "escrow order " + shortID(id), class: "deadline release failed: " + errorClassOf(err), err: err})
		}
	}
	return errors.Join(errs...)
}

func (a *App) releaseOne(ctx context.Context, orderID string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = a.releaseIfDue(ctx, tx, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

// releaseIfDue completes order in tx if it is still shipped, has no dispute and its release deadline has passed: the
// order row is locked first (the lock order of every order action), and checked in a statement after the lock, so a
// dispute committed while this waited is seen. It reports whether the order was completed.
func (a *App) releaseIfDue(ctx context.Context, tx *sql.Tx, orderID string) (bool, error) {
	if _, err := tx.ExecContext(ctx, "SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE", orderID); err != nil {
		return false, err
	}
	var due bool
	var at time.Time
	err := tx.QueryRowContext(ctx, `SELECT o.state='shipped' AND `+noDisputeSQL+` AND `+releaseDueSQL+`,es.release_at
		FROM orders o JOIN escrows es ON es.order_id=o.id WHERE o.id=$1 AND es.release_at IS NOT NULL`, orderID).Scan(&due, &at)
	if err == sql.ErrNoRows || err == nil && !due {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var vendorSigned, buyerSigned bool
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(bool_or(g.party='vendor'),false),COALESCE(bool_or(g.party='buyer'),false) FROM settlement_signatures g
		JOIN settlements s ON s.id=g.settlement_id WHERE s.order_id=$1 AND s.kind='release' AND s.state<>'superseded'`, orderID).Scan(&vendorSigned, &buyerSigned); err != nil {
		return false, err
	}
	next := "The release to the vendor waits for the vendor's signature on the order page; once the vendor has signed it, the market co-signs it and broadcasts it."
	switch {
	case vendorSigned:
		next = "The market co-signs the vendor's signature on the release to the vendor and broadcasts it on its next pass."
	case buyerSigned:
		next = "The market co-signs the buyer's signature on the release to the vendor and broadcasts it on its next pass."
	}
	note := escrowDeadlineNote + " (" + utcUntil(at) + "): no dispute was opened by then, so the order is completed (Bitcoin escrow). " + next
	if _, err = a.transition(ctx, tx, orderID, stateShipped, stateCompleted, nil, note); err != nil {
		return false, err
	}
	return true, nil
}

// remindDeadlines reminds the buyer of every shipped escrow order whose release deadline is less than 72 hours away,
// once, and tells the vendor the buyer's deadline (step 2 of the file comment). A buyer who signed the release, and a
// disputed order, are not reminded.
func (a *App) remindDeadlines(ctx context.Context) error {
	ids, err := orderIDs(ctx, a.db, `SELECT o.id FROM orders o JOIN escrows es ON es.order_id=o.id WHERE o.state='shipped' AND es.reminded_at IS NULL AND `+releaseSoonSQL+`
		AND `+noDisputeSQL+` AND NOT EXISTS (SELECT 1 FROM settlement_signatures g JOIN settlements s ON s.id=g.settlement_id
			WHERE s.order_id=o.id AND s.kind='release' AND s.state<>'superseded' AND g.party='buyer') ORDER BY es.release_at`)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err = a.remindOne(ctx, id); err != nil {
			errs = append(errs, &watchError{label: "escrow order " + shortID(id), class: "deadline reminder failed: " + errorClassOf(err), err: err})
		}
	}
	return errors.Join(errs...)
}

// remindOne claims order's reminder by compare-and-set on reminded_at and sends it; another pass that claimed it first
// leaves nothing to send.
func (a *App) remindOne(ctx context.Context, orderID string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var buyer, vendor string
	var at time.Time
	err = tx.QueryRowContext(ctx, `UPDATE escrows es SET reminded_at=now() FROM orders o JOIN products p ON p.id=o.product_id
		WHERE es.order_id=$1 AND o.id=es.order_id AND es.reminded_at IS NULL AND o.state='shipped' RETURNING o.buyer_id,p.vendor_id,es.release_at`, orderID).Scan(&buyer, &vendor, &at)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	until := utcUntil(at)
	for _, n := range [][2]string{
		{buyer, "Order " + shortID(orderID) + ": Reminder — you have until " + until + " to sign the release to the vendor on the order page if the goods have arrived, or to open a dispute if they have not. If you do neither, the order completes at that time and the market co-signs the vendor's signature on the release; a dispute can no longer be opened after it."},
		{vendor, "Order " + shortID(orderID) + ": the buyer has until " + until + " to sign the release or open a dispute. If the buyer does neither, the order completes at that time and the market co-signs your signature on the release (sign it on the order page if you have not)."},
	} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), n[0], n[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// recheckReleaseDeadlines moves, once after each restore from backup, every passed release deadline of a shipped escrow
// order with no dispute to now + 72 hours, with an order note and a notice to the buyer, in one transaction that
// consumes the restore's request (step 4 of the file comment). The moved deadline counts as reminded: the notice
// states it.
func (a *App) recheckReleaseDeadlines(ctx context.Context) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var asked bool
	err = tx.QueryRowContext(ctx, "SELECT true FROM settings WHERE key=$1 FOR UPDATE", releaseRecheckKey).Scan(&asked)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	ids, err := orderIDs(ctx, tx, `SELECT o.id FROM orders o JOIN escrows es ON es.order_id=o.id WHERE o.state='shipped' AND `+releaseDueSQL+` ORDER BY o.id`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		// The order row first, then checked in a statement after the lock (as releaseIfDue does).
		if _, err = tx.ExecContext(ctx, "SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE", id); err != nil {
			return err
		}
		var due bool
		if err = tx.QueryRowContext(ctx, `SELECT o.state='shipped' AND `+releaseDueSQL+` AND `+noDisputeSQL+` FROM orders o JOIN escrows es ON es.order_id=o.id WHERE o.id=$1`, id).Scan(&due); err != nil {
			return err
		}
		if !due {
			continue
		}
		// Migration 079's trigger moves release_at only later, and only in the statement that sets release_recheck from
		// false to true: an escrow rechecked after an earlier restore is reset first.
		if _, err = tx.ExecContext(ctx, "UPDATE escrows SET release_recheck=false WHERE order_id=$1 AND release_recheck", id); err != nil {
			return err
		}
		var at time.Time
		var buyer string
		if err = tx.QueryRowContext(ctx, `UPDATE escrows es SET release_at=now()+interval '72 hours',release_recheck=true,reminded_at=COALESCE(es.reminded_at,now())
			FROM orders o WHERE o.id=es.order_id AND es.order_id=$1 RETURNING es.release_at,o.buyer_id`, id).Scan(&at, &buyer); err != nil {
			return err
		}
		until := utcUntil(at)
		note := "Release deadline moved to " + until + " after a restore from backup: the deadline in the restored records had passed, so the buyer has 72 more hours to sign the release or open a dispute."
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", id, stateShipped, note); err != nil {
			return err
		}
		body := "Order " + shortID(id) + ": this market restored its records from a backup, and the release deadline of this order had passed, so it was moved to " + until +
			". Sign the release to the vendor on the order page if the goods have arrived, or open a dispute before then if they have not. If you do neither, the order completes at that time and the market co-signs the vendor's signature on the release."
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), buyer, body); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", releaseRecheckKey); err != nil {
		return err
	}
	return tx.Commit()
}

// noShipCancel checks the buyer's cancellation of a paid escrow order, in tx after the order row lock (step 1 of the
// file comment): refused once the order was marked shipped (naming when), for an order with no ship-by time (a digital
// one), and before its ship-by time (naming it).
func noShipCancel(c *actionCtx, o *Order) error {
	ctx := c.Ctx()
	if o.State == stateShipped {
		var at time.Time
		if err := c.Tx.QueryRowContext(ctx, "SELECT max(created) FROM order_events WHERE order_id=$1 AND from_state='paid' AND to_state='shipped'", o.ID).Scan(&at); err != nil {
			return err
		}
		return fail(409, "The vendor marked this order shipped at "+utcUntil(at)+", so it can no longer be cancelled. Sign the release once the goods have arrived, or open a dispute if they do not arrive. Nothing was changed.")
	}
	if o.State != statePaid {
		return errConflict
	}
	var shipBy sql.NullTime
	var open bool
	var days sql.NullInt64
	if err := c.Tx.QueryRowContext(ctx, "SELECT ship_by,COALESCE(ship_by<=now(),false),dispatch_days FROM escrows WHERE order_id=$1", o.ID).Scan(&shipBy, &open, &days); err != nil {
		return err
	}
	switch {
	case !shipBy.Valid:
		return errCancelNoShipBy
	case !open:
		return fail(409, fmt.Sprintf("You can cancel this order from %s if the vendor has not marked it shipped by then: the vendor's dispatch window of %d days plus 48 hours, counted from payment. Nothing was changed.", utcFrom(shipBy.Time), days.Int64))
	}
	return nil
}

// shipConflict is the vendor's refusal when the order is no longer paid: a buyer's cancellation names when it was made.
func shipConflict(c *actionCtx, orderID string) error {
	var at sql.NullTime
	if err := c.Tx.QueryRowContext(c.Ctx(), `SELECT max(e.created) FROM order_events e JOIN orders o ON o.id=e.order_id
		WHERE e.order_id=$1 AND e.to_state='cancelled' AND e.from_state='paid' AND e.actor_id=o.buyer_id AND o.state='cancelled'`, orderID).Scan(&at); err != nil {
		return err
	}
	if !at.Valid {
		return errConflict
	}
	return fail(409, "The buyer cancelled this order at "+utcUntil(at.Time)+" because it was not marked shipped within its dispatch window plus 48 hours, so it cannot be marked shipped: the escrow is refunded to the buyer. Do not send the goods. Nothing was changed.")
}

// lateDispute refuses (409) a dispute on a shipped escrow order whose release deadline has passed (owner decision
// 2026-10-03: the release is due), in tx after the order row lock; before the deadline it returns the sentence the
// other party's notice carries.
func lateDispute(c *actionCtx, o *Order) (string, error) {
	if !o.Escrow || o.State != stateShipped {
		return "", nil
	}
	var at sql.NullTime
	var due bool
	err := c.Tx.QueryRowContext(c.Ctx(), "SELECT es.release_at,COALESCE("+releaseDueSQL+",false) FROM escrows es WHERE es.order_id=$1", o.ID).Scan(&at, &due)
	if err != nil || !at.Valid {
		return "", err
	}
	if due {
		return "", fail(409, "The release deadline of this order passed at "+utcUntil(at.Time)+", so the release to the vendor is due and a dispute can no longer be opened. Nothing was changed.")
	}
	return "The release deadline of " + utcUntil(at.Time) + " no longer applies: opening a dispute cancels it for good, and an administrator of this market decides the order.", nil
}

// deadlineNextStep is the part of a shipped order's "who acts next" line about its release deadline, for viewer.
func deadlineNextStep(viewer string, dl *EscrowDeadlineView, paused bool) string {
	switch {
	case dl.Due && paused:
		return "The release deadline (" + dl.At + ") has passed: the release is due, and the order completes once the market has finished reconciling its restore from backup."
	case dl.Due:
		return "The release deadline (" + dl.At + ") has passed: the release is due, and the order completes on the market's next pass."
	case viewer == roleBuyer:
		return "If you cannot sign, you need do nothing: if you neither sign nor open a dispute by " + dl.At + ", the order completes at that time and the market co-signs the vendor's signature on the release. Open a dispute before then if the goods did not arrive."
	case viewer == roleVendor:
		return "The buyer has until " + dl.At + ": if the buyer neither signs nor opens a dispute by then, the order completes and the market co-signs your signature on the release (it waits for your signature if you have not signed)."
	}
	return "If the buyer neither signs nor opens a dispute by " + dl.At + ", the order completes at that time and the market co-signs the vendor's signature on the release."
}

// deadlineBanners fills /orders' banners: the viewer's shipped escrow orders, with no dispute, whose release deadline is
// less than 72 hours away (P2-R9, A-377).
func deadlineBanners(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.User == nil || a.db == nil {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT o.id,o.buyer_id=$1,es.release_at,extract(epoch FROM es.release_at-now()) FROM orders o JOIN products p ON p.id=o.product_id JOIN escrows es ON es.order_id=o.id
		WHERE (o.buyer_id=$1 OR p.vendor_id=$1) AND o.state='shipped' AND `+releaseSoonSQL+` AND `+noDisputeSQL+` ORDER BY es.release_at`, d.User.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var b DeadlineBanner
		var at time.Time
		var left float64
		if err = rows.Scan(&b.OrderID, &b.Buyer, &at, &left); err != nil {
			return err
		}
		b.Short, b.At, b.Left = shortID(b.OrderID), utcUntil(at), timeLeft(time.Duration(left*float64(time.Second)))
		d.DeadlineBanners = append(d.DeadlineBanners, b)
	}
	return rows.Err()
}
