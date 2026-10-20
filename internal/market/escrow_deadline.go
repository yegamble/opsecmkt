package market

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

// The two clocks of a physical escrow order (WR-escrow Phase 2, slice P2-c; P2-R3, P2-R5, P2-R9 and P2-R10's restore
// recheck; owner decisions ES-11 and 2026-10-03). Behind ESCROW_BTC (off by default); test networks only.
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
	// buyerUnsignedSQL: the buyer of the order o has not signed its release (a signature confirms receipt and ends the
	// right to dispute, so the reminders and the banner that say "sign or dispute" do not apply).
	buyerUnsignedSQL = `NOT EXISTS (SELECT 1 FROM settlement_signatures g JOIN settlements s ON s.id=g.settlement_id
		WHERE s.order_id=o.id AND s.kind='release' AND s.state<>'superseded' AND g.party='buyer')`
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
		next := "Sign the release once the goods have arrived"
		if o.xmrEscrow() {
			next = "Use Confirm receipt on the order page once the goods have arrived"
		}
		return fail(409, "The vendor marked this order shipped at "+utcUntil(at)+", so it can no longer be cancelled. "+next+", or open a dispute if they do not arrive. Nothing was changed.")
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
// other party's notice carries. While a restore's recheck is pending (escrow off after the restore, or before the first
// pass after startup) the passed deadline is not yet due: the recheck will give the buyer 72 hours (A-393).
func lateDispute(c *actionCtx, o *Order) (string, error) {
	if !o.Escrow || o.State != stateShipped {
		return "", nil
	}
	var at sql.NullTime
	var due bool
	err := c.Tx.QueryRowContext(c.Ctx(), "SELECT es.release_at,COALESCE("+releaseDueSQL+" AND NOT EXISTS (SELECT 1 FROM settings WHERE key=$2),false) FROM escrows es WHERE es.order_id=$1", o.ID, releaseRecheckKeyOf(o.Currency)).Scan(&at, &due)
	if err != nil || !at.Valid {
		return "", err
	}
	if due {
		return "", fail(409, "The release deadline of this order passed at "+utcUntil(at.Time)+", so the release to the vendor is due and a dispute can no longer be opened. Nothing was changed.")
	}
	return "The release deadline of " + utcUntil(at.Time) + " no longer applies: opening a dispute cancels it for good, and an administrator of this market decides the order.", nil
}

// deadlineBanners fills /orders' banners: the viewer's shipped escrow orders, with no dispute, whose release deadline is
// less than 72 hours away (P2-R9, A-377), unless the buyer has signed the release (A-392): the banner would tell the
// buyer to sign or dispute, and the vendor that the buyer may still do either.
func deadlineBanners(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.User == nil || a.db == nil {
		return nil
	}
	// A Monero escrow order (P3-R7) has no signatures, so buyerUnsignedSQL always holds for it; its banner says Confirm
	// receipt.
	rows, err := a.db.QueryContext(ctx, `SELECT o.id,o.buyer_id=$1,es.coin='XMR',es.release_at,extract(epoch FROM es.release_at-now()) FROM orders o JOIN products p ON p.id=o.product_id JOIN escrows es ON es.order_id=o.id
		WHERE (o.buyer_id=$1 OR p.vendor_id=$1) AND o.state='shipped' AND `+releaseSoonSQL+` AND `+noDisputeSQL+` AND `+buyerUnsignedSQL+` ORDER BY es.release_at`, d.User.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var b DeadlineBanner
		var at time.Time
		var left float64
		if err = rows.Scan(&b.OrderID, &b.Buyer, &b.XMR, &at, &left); err != nil {
			return err
		}
		b.Short, b.At, b.Left = shortID(b.OrderID), utcUntil(at), timeLeft(time.Duration(left*float64(time.Second)))
		d.DeadlineBanners = append(d.DeadlineBanners, b)
	}
	return rows.Err()
}
