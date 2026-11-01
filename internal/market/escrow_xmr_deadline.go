package market

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// The clocks of a physical Monero escrow order (WR-escrow Phase 3, slice X-e; P3-R7 with the Phase 2 rules P2-R3, P2-R5,
// P2-R9 and P2-R10's restore recheck), and of a digital one (A-709: its delivery deadline and dispute window, steps 1
// and 3). Behind ESCROW_XMR (off by default); fakechain or stagenet (A-449). They are escrow_deadline.go's, run by
// the Monero escrow watcher for Monero escrow orders (xmrDeadlines, the last step of its pass), so they apply with
// ESCROW_BTC off; the Bitcoin escrow watcher's escrowDeadlines handles Bitcoin escrow orders only.
//
//  1. Paid: ship_by and the buyer's no-ship cancel are shared with Bitcoin escrow (escrowTransitionHook, noShipCancel,
//     shipConflict); the cancel requests the refund (escrowSettleTerminal).
//  2. Shipped: from 72 hours before release_at the buyer is reminded once (reminded_at claimed by compare-and-set) and
//     the vendor told the buyer's deadline (xmrRemindDeadlines). A dispute filed after it is refused (lateDispute).
//  3. At release_at a shipped order with no dispute is completed by the system ("auto-release deadline passed") under
//     the order row lock (xmrReleaseDue); the release requested at shipment is then buildable (settlementFits). A
//     delivered digital order's release_at is the end of its dispute window (A-709, escrowTransitionHook): it is
//     completed the same way ("dispute window ended"), and the vendor may then claim. A paid digital order's ship_by
//     is its delivery deadline: step 1's buyer cancel applies from it.
//  4. After a restore from backup (scripts/restore.sh, or UPGRADING.md section 6, sets escrow_xmr_release_recheck) the
//     pass moves every deadline of a shipped or delivered order that has passed or ends within 72 hours to now + 72 hours
//     and tells the buyer (xmrRecheckReleaseDeadlines; A-728: that pass runs before public access returns, so a deadline
//     just short of passing needs the grace as much as a passed one). No such order then completes for those 72 hours
//     unless its buyer confirms. recheckReleaseDeadlines is the Bitcoin escrow step, with its own setting. Until the
//     recheck has succeeded and consumed the setting, step 3 completes nothing, a dispute is not refused as late
//     (lateDispute) and the buyer's no-ship cancel waits (noShipCancel), since a restored ship_by cannot move (A-714).
//     The transaction that consumes the setting records the end of the vendor's grace, 72 hours on (xmrCancelWaitKey):
//     on the documented restore order that pass runs before public access returns, so the wait has to outlast it. Until
//     then the buyer's cancel of an order whose ship_by passes before that end still waits, and the vendor can ship or
//     deliver again what the restored records lost (xmrCancelWaitSQL, A-720). An order paid after the recheck is not
//     affected: its ship_by is at least 72 hours from payment.
//     An outage takes the same path (A-730, owner decision): each step records the database's time, and one that finds
//     more than xmrOutageThreshold since the last asks for the recheck, with its notices naming the outage, so downtime
//     consumes neither a buyer's deadline nor a vendor's time. It sees only that this step did not run: public access
//     down while the watcher runs is not visible here, and the operator handles that case by hand.
//  5. A vendor's ban (admin_ban.go, A-652d) moves every sooner deadline of the vendor's shipped or delivered orders with
//     no dispute to 72 hours after the ban, in the ban's transaction, and tells each buyer (xmrBanDeadlines). While the vendor is
//     banned, steps 2 and 3 and the buyer's confirmation say the release is then held (xmrReleaseHeld, A-652c).
//
// No step makes an RPC, and none runs while the restore gate is set: the recheck then runs in the first pass after the
// gate is cleared, before the release. Step 5 runs with the ban whatever the gate: it only moves deadlines later.

// digitalWindowNote opens the order history note of a digital order's completion at the end of its dispute window.
const digitalWindowNote = "dispute window ended"

// xmrReleaseRecheckKey is the setting that asks for the Monero deadline recheck (step 4): a restore from backup writes
// it with the value true, overwriting what the backup held, and an outage (xmrNoteDeadlineStep) with xmrRecheckOutage.
const xmrReleaseRecheckKey = "escrow_xmr_release_recheck"

// xmrRecheckOutage is xmrReleaseRecheckKey's value when an outage asked for the recheck, and xmrCancelWaitCauseKey's when
// one recorded the grace. Any other value of the request is a restore's.
const xmrRecheckOutage = "outage"

// xmrLastDeadlineStepKey is the setting that holds the database's time of the last deadline step (A-730), an RFC 3339
// time in UTC. It is absent before the first step after the upgrade.
const xmrLastDeadlineStepKey = "escrow_xmr_last_deadline_step"

// xmrOutageThreshold is the gap between two deadline steps that counts as an outage (A-730). A pass is bounded by
// xmrEscrowPassTimeout and xmrRecheckBudget, well under it, so a slow pass does not count.
const xmrOutageThreshold = time.Hour

// xmrCancelWaitKey is the setting the recheck writes in the transaction that consumes xmrReleaseRecheckKey: the end of
// the vendor's grace after a restore or an outage (step 4, A-720), an RFC 3339 time in UTC. Expired, it is harmless; a
// later recheck replaces it.
const xmrCancelWaitKey = "escrow_xmr_cancel_wait_until"

// xmrCancelWaitCauseKey is the setting that holds xmrRecheckOutage while xmrCancelWaitKey's end was recorded after an
// outage (A-730); absent, a restore recorded it.
const xmrCancelWaitCauseKey = "escrow_xmr_cancel_wait_cause"

// xmrOutageCauseSQL: what the pages say of the recheck pending, or else of the vendor's grace, for the escrow es is an
// outage rather than a restore (A-730).
const xmrOutageCauseSQL = `(es.coin='XMR' AND COALESCE((SELECT value='` + xmrRecheckOutage + `' FROM settings WHERE key='` + xmrReleaseRecheckKey + `'),
	(SELECT value='` + xmrRecheckOutage + `' FROM settings WHERE key='` + xmrCancelWaitCauseKey + `'),false))`

// outageWhy is why an outage moved a deadline or made a cancel wait, as a clause (A-730). The escrow service: the same
// gap follows the whole market being down, its database being down, and Monero escrow being switched off.
const outageWhy = "an outage stopped this market's escrow service for over an hour"

// xmrNoRecheckSQL: no restore recheck of Monero deadlines is pending (step 4). The release (step 3) waits while one is,
// as a late dispute does (disputeClosedSQL, lateDispute): a recheck that failed leaves the setting, so the same pass
// completes no order whose restored deadline has passed before the buyer was given 72 hours (A-393, A-714).
const xmrNoRecheckSQL = "NOT EXISTS (SELECT 1 FROM settings WHERE key='" + xmrReleaseRecheckKey + "')"

// xmrCancelWaitSQL is the end of the vendor's grace after a restore (step 4, A-720) for the escrow es, or NULL when none
// applies: not a Monero escrow, the grace has ended, or es.ship_by does not pass before it. A statement after the order
// row lock reads it for the buyer's cancel (noShipCancel). OFFSET 0 keeps the cast to the key's own row.
const xmrCancelWaitSQL = `(SELECT w.until FROM (SELECT value::timestamptz AS until FROM settings WHERE key='` + xmrCancelWaitKey + `' OFFSET 0) w
	WHERE es.coin='XMR' AND w.until>now() AND w.until>es.ship_by)`

// xmrOpenSQL is the order states whose release_at applies: a shipped physical order's release deadline and a delivered
// digital order's dispute window (A-709).
const xmrOpenSQL = "o.state IN ('shipped','delivered')"

// releaseGraceSQL is the soonest release deadline the restore recheck (step 4) and a vendor's ban (step 5) leave a
// shipped order: 72 hours from now, so its buyer has that long to confirm receipt or open a dispute.
const releaseGraceSQL = "now()+interval '72 hours'"

// releaseBeforeGraceSQL: the release deadline of the escrow es has passed or ends before that grace does, so the restore
// recheck (step 4) moves it. now() is the recheck transaction's, so a second check after the row lock agrees with the first.
const releaseBeforeGraceSQL = "es.release_at<" + releaseGraceSQL

// xmrHeldNote is what the order history says of the release when an order of a banned vendor completes (A-652c, A-652d).
const xmrHeldNote = "The vendor's account is closed, so the market holds the release: it builds and sends nothing while the account is banned, and the coins stay in this order's escrow account. No other address is ever paid."

// xmrBanHeldPayment says, after "the order completes at that time and", what happens to a banned vendor's payment.
const xmrBanHeldPayment = "its payment is held: the market sends nothing to the vendor while the account is closed, and no other address is ever paid"

// banDeadlineNote opens the order note and the buyer's notice of a vendor's ban on a shipped order (xmrBanDeadlines).
const banDeadlineNote = "The vendor's account is closed"

// orderVendorBanned reports whether the vendor of order's listing is banned (A-546).
func orderVendorBanned(ctx context.Context, q rowQuerier, orderID string) (bool, error) {
	var banned bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id JOIN users v ON v.id=p.vendor_id
		WHERE o.id=$1 AND v.banned_at IS NOT NULL)`, orderID).Scan(&banned)
	return banned, err
}

// releaseRecheckKeyOf is the setting that asks for the deadline recheck of coin's escrow orders.
func releaseRecheckKeyOf(coin string) string {
	if coin == "XMR" {
		return xmrReleaseRecheckKey
	}
	return releaseRecheckKey
}

// xmrDeadlineBudget bounds the deadline part of a pass but for the recheck, apart from the pass's own budget
// (xmrDeadlines). It makes no RPC. A variable so tests can shorten it.
var xmrDeadlineBudget = 30 * time.Second

// xmrRecheckBudget bounds the recheck (A-732): one transaction, for a frozen now() and the vendor's grace written with
// it, so it cannot be batched, and a pass whose recheck never fits would complete nothing for good. It is spent apart from
// xmrDeadlineBudget. A variable so tests can shorten it.
var xmrRecheckBudget = xmrEscrowPassTimeout

// xmrShutdownKey holds the context whose cancel is a shutdown (withXMRShutdown).
type xmrShutdownKey struct{}

// withXMRShutdown names ctx as the context whose cancel is a shutdown, for the pass derived from it: once the pass's
// deadline has passed, its own context no longer reports a cancel (xmrDeadlines).
func withXMRShutdown(ctx context.Context) context.Context {
	return context.WithValue(ctx, xmrShutdownKey{}, ctx)
}

// xmrDeadlines is the deadline part of a Monero escrow watcher pass, under its lock: the check for an outage, then the
// recheck, so a deadline that passed in a restored database or in an outage is moved before it could be released, then
// the release, then the reminders. A recheck that fails keeps its setting, so the release then completes nothing until a
// later pass's recheck succeeds (xmrNoRecheckSQL). A step that cannot record its time does nothing else (the gap is
// still there on the next pass, and a due order must not complete before it is seen).
//
// It runs on its own budgets (A-728, A-732): a hung wallet-rpc can use up the pass's, and a due order must still complete,
// since a dispute on it is already refused as late. Only the pass's deadline is ignored: a shutdown stops it midway, at
// any time, and it does not start after one.
func (a *App) xmrDeadlines(pass context.Context) error {
	shutdown := pass
	if s, ok := pass.Value(xmrShutdownKey{}).(context.Context); ok {
		shutdown = s
	}
	stopped := func() bool { return errors.Is(shutdown.Err(), context.Canceled) }
	if stopped() {
		return shutdown.Err()
	}
	budget := func(d time.Duration) (context.Context, func()) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(pass), d)
		stop := context.AfterFunc(shutdown, func() {
			if stopped() {
				cancel()
			}
		})
		return ctx, func() { stop(); cancel() }
	}
	ctx, done := budget(xmrDeadlineBudget)
	defer done()
	gate, err := restoreGateSet(ctx, a.db)
	if err != nil || gate {
		return err
	}
	if err = a.xmrNoteDeadlineStep(ctx); err != nil {
		return &watchError{label: "monero escrow deadlines", class: "outage check failed: " + errorClassOf(err), err: err}
	}
	var errs []error
	recheck, recheckDone := budget(xmrRecheckBudget)
	defer recheckDone()
	if err = a.xmrRecheckReleaseDeadlines(recheck); err != nil {
		errs = append(errs, &watchError{label: "monero escrow deadlines", class: "recheck failed: " + errorClassOf(err), err: err})
	}
	if stopped() {
		return errors.Join(append(errs, shutdown.Err())...)
	}
	due, dueDone := budget(xmrDeadlineBudget) // the recheck's time is not taken from the completions'
	defer dueDone()
	if err = a.xmrReleaseDue(due); err != nil {
		errs = append(errs, err)
	}
	if err = a.xmrRemindDeadlines(due); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// xmrNoteDeadlineStep records the database's time of this deadline step and, when the last was more than
// xmrOutageThreshold ago, asks for the recheck, in one transaction (A-730): a request already there stays as it is, so a
// restored database, which carries an old time, stays a restore; one that fails keeps the request for the next pass.
// No stored time asks for nothing, nor does one that cannot be read, which only a hand could have written. It runs after
// the restore gate's check, so the time of a gated step stays old, and it is the restore's request that the first pass
// after the gate finds. This is the database's clock, since it is what every deadline is compared with.
func (a *App) xmrNoteDeadlineStep(ctx context.Context) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var last string
	var now time.Time
	err = tx.QueryRowContext(ctx, "SELECT value,now() FROM settings WHERE key=$1 FOR UPDATE", xmrLastDeadlineStepKey).Scan(&last, &now)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if at, parseErr := time.Parse(time.RFC3339Nano, last); parseErr == nil && now.Sub(at) > xmrOutageThreshold {
			if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO NOTHING", xmrReleaseRecheckKey, xmrRecheckOutage); err != nil {
				return err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, xmrLastDeadlineStepKey); err != nil {
		return err
	}
	return tx.Commit()
}

// xmrReleaseDue completes every shipped or delivered Monero escrow order whose release deadline or dispute window has
// passed with no dispute, each in its own transaction (step 3 of the file comment); nothing while a restore recheck is
// pending (xmrNoRecheckSQL).
func (a *App) xmrReleaseDue(ctx context.Context) error {
	ids, err := orderIDs(ctx, a.db, `SELECT o.id FROM orders o JOIN escrows es ON es.order_id=o.id WHERE es.coin='XMR' AND `+xmrOpenSQL+` AND `+releaseDueSQL+` AND `+noDisputeSQL+` AND `+xmrNoRecheckSQL+` ORDER BY es.release_at`)
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

// xmrReleaseIfDue completes order in tx if it is still shipped or delivered, has no dispute and its release deadline or
// dispute window has passed with no restore recheck pending: the order row is locked first and checked in a statement
// after the lock (releaseIfDue's rule). It reports whether the order was completed.
func (a *App) xmrReleaseIfDue(ctx context.Context, tx *sql.Tx, orderID string) (bool, error) {
	if _, err := tx.ExecContext(ctx, "SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE", orderID); err != nil {
		return false, err
	}
	var due bool
	var state string
	var at time.Time
	err := tx.QueryRowContext(ctx, `SELECT `+xmrOpenSQL+` AND `+noDisputeSQL+` AND `+releaseDueSQL+` AND `+xmrNoRecheckSQL+`,o.state,es.release_at
		FROM orders o JOIN escrows es ON es.order_id=o.id WHERE o.id=$1 AND es.coin='XMR' AND es.release_at IS NOT NULL`, orderID).Scan(&due, &state, &at)
	if err == sql.ErrNoRows || err == nil && !due {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	lead := escrowDeadlineNote
	if state == stateDelivered {
		lead = digitalWindowNote
	}
	note := lead + " (" + utcUntil(at) + "): no dispute was opened by then, so the order is completed (Monero escrow). "
	if banned, err := orderVendorBanned(ctx, tx, orderID); err != nil {
		return false, err
	} else if banned {
		days, err := holdRefundDaysShown(ctx, tx)
		if err != nil {
			return false, err
		}
		note += xmrHeldNote + " " + holdRefundLater(days) // PAY90-3, A-652f
	} else if state == stateDelivered {
		note += "The vendor claims the escrowed coins from the order page."
		ctx = withNoticeDetail(ctx, orderID, stateCompleted, xmrClaimCue) // A-710's cue: the vendor has to claim
	} else {
		note += "The market builds the release of the escrowed coins to the address the vendor gave when marking the order shipped, and sends it."
	}
	if _, err = a.transition(ctx, tx, orderID, state, stateCompleted, nil, note); err != nil {
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
	var banned bool
	err = tx.QueryRowContext(ctx, `UPDATE escrows es SET reminded_at=now() FROM orders o JOIN products p ON p.id=o.product_id JOIN users v ON v.id=p.vendor_id
		WHERE es.order_id=$1 AND es.coin='XMR' AND o.id=es.order_id AND es.reminded_at IS NULL AND o.state='shipped' AND `+noDisputeSQL+` RETURNING o.buyer_id,p.vendor_id,es.release_at,v.banned_at IS NOT NULL`, orderID).Scan(&buyer, &vendor, &at, &banned)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	until, short := utcUntil(at), "Order "+shortID(orderID)+": "
	notes := [][2]string{
		{buyer, short + "Reminder — you have until " + until + " to use Confirm receipt on the order page if the goods have arrived, or to open a dispute if they have not. If you do neither, the order completes at that time and the market releases the coins in this Monero escrow to the vendor; a dispute can no longer be opened after it."},
		{vendor, short + "the buyer has until " + until + " to confirm receipt or open a dispute. If the buyer does neither, the order completes at that time and the market releases the coins in this Monero escrow to you."},
	}
	if banned { // A-652d: the release would be held (xmrReleaseHeld)
		notes = [][2]string{
			{buyer, short + "Reminder — the vendor's account is closed. You have until " + until + " to open a dispute if the goods have not arrived, or to use Confirm receipt on the order page if they have. If you do neither, the order completes at that time and " + xmrBanHeldPayment + "; a dispute can no longer be opened after it."},
			{vendor, short + "the buyer has until " + until + " to confirm receipt or open a dispute. If the buyer does neither, the order completes at that time, and its release is held while your account is closed."},
		}
	}
	for _, n := range notes {
		if _, err = insertNotification(ctx, tx, n[0], n[1], orderID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// xmrRecheckReleaseDeadlines moves, once after each restore from backup or outage (A-730), every release deadline of a
// shipped Monero escrow order, and every dispute window of a delivered one (A-709), that has passed or ends within 72
// hours (A-728), with no dispute, to now + 72 hours, with an order note and a notice to the buyer, in one
// transaction that consumes the request (step 4 of the file comment; recheckReleaseDeadlines' rules: only the
// rechecks reset and set release_recheck, so a second restore rechecks as the first did). The same transaction records
// the end of the vendor's grace (xmrCancelWaitKey, A-720), with no gap between the two a buyer's cancel could use. A row
// the restored database carries is replaced only by a later end, so a restore never shortens a grace.
//
// After an outage the order page must not say a restore moved the deadline: release_recheck is what it reads, so the
// flag is reset again (the ban's way round migration 079's trigger, xmrBanDeadlines); the order's history says why.
func (a *App) xmrRecheckReleaseDeadlines(ctx context.Context) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var requested string
	err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1 FOR UPDATE", xmrReleaseRecheckKey).Scan(&requested)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	outage := requested == xmrRecheckOutage
	ids, err := orderIDs(ctx, tx, `SELECT o.id FROM orders o JOIN escrows es ON es.order_id=o.id WHERE es.coin='XMR' AND `+xmrOpenSQL+` AND `+releaseBeforeGraceSQL+` ORDER BY o.id`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, "SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE", id); err != nil {
			return err
		}
		var due bool
		var state string
		if err = tx.QueryRowContext(ctx, `SELECT `+xmrOpenSQL+` AND `+releaseBeforeGraceSQL+` AND `+noDisputeSQL+`,o.state FROM orders o JOIN escrows es ON es.order_id=o.id WHERE o.id=$1 AND es.coin='XMR'`, id).Scan(&due, &state); err != nil {
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
		if err = tx.QueryRowContext(ctx, `UPDATE escrows es SET release_at=GREATEST(es.release_at,`+releaseGraceSQL+`),release_recheck=true,reminded_at=COALESCE(es.reminded_at,now())
			FROM orders o WHERE o.id=es.order_id AND es.order_id=$1 AND es.coin='XMR' RETURNING es.release_at,o.buyer_id`, id).Scan(&at, &buyer); err != nil {
			return err
		}
		until, what, check, outcome := utcUntil(at), "release deadline", "if the goods have arrived, or open a dispute before then if they have not", "the market releases the coins in this Monero escrow to the vendor"
		if state == stateDelivered { // A-709: a digital order's dispute window
			what, check, outcome = "dispute window", "if the delivered content works, or open a dispute before then if it does not", "the vendor can claim the coins in this Monero escrow"
		}
		closed := ""
		if banned, err := orderVendorBanned(ctx, tx, id); err != nil { // PAY101-6: after the row lock, as a ban locks the order first
			return err
		} else if banned {
			closed, outcome = banDeadlineNote+". ", xmrBanHeldPayment
		}
		note, why := strings.ToUpper(what[:1])+what[1:]+" moved to "+until+" after a restore from backup (Monero escrow): the deadline in the restored records has passed or ends within 72 hours, so the buyer has 72 hours from this recheck to confirm receipt or open a dispute.", "this market restored its records from a backup"
		if outage {
			if _, err = tx.ExecContext(ctx, "UPDATE escrows SET release_recheck=false WHERE order_id=$1 AND coin='XMR'", id); err != nil {
				return err
			}
			note, why = strings.ToUpper(what[:1])+what[1:]+" moved to "+until+" after an outage (Monero escrow): this market's escrow service did not run for over an hour, and this deadline has passed or ends within 72 hours, so the buyer has 72 hours from this recheck to confirm receipt or open a dispute.", outageWhy
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", id, state, note); err != nil {
			return err
		}
		body := "Order " + shortID(id) + ": " + why + ", and the " + what + " of this order has passed or ends within 72 hours, so it was moved to " + until +
			". " + closed + "Use Confirm receipt on the order page " + check + ". If you do neither, the order completes at that time and " + outcome + "."
		if _, err = insertNotification(ctx, tx, buyer, body, id); err != nil {
			return err
		}
	}
	grace, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,to_char((`+releaseGraceSQL+`) AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value WHERE settings.value::timestamptz<EXCLUDED.value::timestamptz`, xmrCancelWaitKey)
	if err != nil {
		return err
	}
	// The reason follows the end it explains: a row left in place keeps its own.
	if n, err := grace.RowsAffected(); err != nil {
		return err
	} else if n > 0 && outage {
		if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value", xmrCancelWaitCauseKey, xmrRecheckOutage); err != nil {
			return err
		}
	} else if n > 0 {
		if _, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", xmrCancelWaitCauseKey); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", xmrReleaseRecheckKey); err != nil {
		return err
	}
	return tx.Commit()
}

// xmrBanDeadlines is step 5 of the file comment (A-652d; owner decision 2026-10-25 "refund + hold": shipped orders get at
// least 72 hours from the ban for the buyer to dispute, with a notice to each buyer for their order; PAY87-4, PAY89-1).
// In the ban's transaction tx, after the vendor's account row lock (adminBanAction), it locks each shipped or delivered
// Monero escrow order of vendorID in id order, without waiting, and checks it again after the lock (the recheck's
// rule): the ban already holds those listed before that lock (lockBanShippedOrders), so only an order shipped or
// delivered in between can be held by another transaction, which refuses the whole ban with the retryable database-busy
// 503, nothing changed. One still shipped or delivered with no dispute gets release_at = GREATEST(release_at,
// releaseGraceSQL), never earlier (a delivered order's dispute window, A-709), and its buyer an order note and a
// notice for that order: the vendor's account is closed, a dispute can be opened until the deadline, and
// otherwise the order completes and its payment is held (xmrReleaseHeld). Nothing else changes and no money moves;
// lifting the ban never touches a deadline. A moved deadline counts as reminded: the ban's notice names it.
//
// Migration 079's trigger lets release_at move only later, in the statement that sets release_recheck from false to
// true: the flag is reset first, as the recheck does, and then put back as it was, so the order page does not say that
// a restore from backup moved the deadline. It returns how many deadlines moved and how many buyers were told.
func xmrBanDeadlines(ctx context.Context, tx *sql.Tx, vendorID string) (moved, told int, err error) {
	ids, err := orderIDs(ctx, tx, banShippedSQL, vendorID)
	if err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		// NOWAIT: an order message locks its order and then share-locks the partner's account (orderMessageAction), so
		// waiting here while the ban holds the vendor's account row could deadlock. Those the ban locked before that row
		// are its own already; contention on one shipped since is the retryable 503.
		if _, err = tx.ExecContext(ctx, "SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE NOWAIT", id); err != nil {
			return 0, 0, err
		}
		var open, soon, prior bool
		var buyer, state string
		var at time.Time
		err = tx.QueryRowContext(ctx, `SELECT `+xmrOpenSQL+` AND `+noDisputeSQL+`,es.release_at<`+releaseGraceSQL+`,es.release_recheck,o.buyer_id,es.release_at,o.state
			FROM orders o JOIN escrows es ON es.order_id=o.id WHERE o.id=$1 AND es.coin='XMR' AND es.release_at IS NOT NULL`, id).Scan(&open, &soon, &prior, &buyer, &at, &state)
		if err == sql.ErrNoRows || err == nil && !open {
			continue
		}
		if err != nil {
			return 0, 0, err
		}
		what, problem := "release deadline", "if the goods have not arrived"
		if state == stateDelivered { // A-709: a digital order's dispute window
			what, problem = "dispute window", "if the delivered content does not work"
		}
		note := banDeadlineNote + " (Monero escrow); the " + what + " stays at " + utcUntil(at) + ", at least 72 hours after the ban, for the buyer to open a dispute."
		if soon {
			if _, err = tx.ExecContext(ctx, "UPDATE escrows SET release_recheck=false WHERE order_id=$1 AND coin='XMR' AND release_recheck", id); err != nil {
				return 0, 0, err
			}
			if err = tx.QueryRowContext(ctx, `UPDATE escrows SET release_at=GREATEST(release_at,`+releaseGraceSQL+`),release_recheck=true,reminded_at=COALESCE(reminded_at,now())
				WHERE order_id=$1 AND coin='XMR' RETURNING release_at`, id).Scan(&at); err != nil {
				return 0, 0, err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE escrows SET release_recheck=$2 WHERE order_id=$1 AND coin='XMR'", id, prior); err != nil {
				return 0, 0, err
			}
			moved++
			note = banDeadlineNote + " (Monero escrow), so the " + what + " moved to " + utcUntil(at) + ": the buyer has at least 72 hours from the ban to open a dispute."
		}
		note += " If the buyer neither opens a dispute nor confirms receipt by then, the order completes at that time and " + xmrBanHeldPayment + "."
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", id, state, note); err != nil {
			return 0, 0, err
		}
		body := "Order " + shortID(id) + ": " + banDeadlineNote + ". You may open a dispute on the order page until " + utcUntil(at) +
			" " + problem + ". If you neither open a dispute nor confirm receipt by then, the order completes at that time and " + xmrBanHeldPayment + "."
		if _, err = insertNotification(ctx, tx, buyer, body, id); err != nil {
			return 0, 0, err
		}
		told++
	}
	return moved, told, nil
}

// banShippedSQL lists vendor $1's shipped and delivered Monero escrow orders in id order: those a ban may move
// (xmrBanDeadlines; a delivered one is a digital order in its dispute window, A-709).
const banShippedSQL = `SELECT o.id FROM orders o JOIN products p ON p.id=o.product_id JOIN escrows es ON es.order_id=o.id
	WHERE p.vendor_id=$1 AND es.coin='XMR' AND ` + xmrOpenSQL + ` ORDER BY o.id`

// lockBanShippedOrders locks, in the ban's transaction tx and before the vendor's account row (adminBanAction,
// PAY90-2), each shipped or delivered Monero escrow order of vendorID in id order, waiting for one another transaction
// holds (the escrow watcher locks each order whose deposits it records on every pass). That is an order message's order too (its
// order, then the partner's account, orderMessageAction), so the wait cannot close a cycle with it, and the ban no
// longer refuses with the busy 503 while the watcher runs. The ban's lock wait bounds it (waitForWalletLocks).
func lockBanShippedOrders(ctx context.Context, tx *sql.Tx, vendorID string) error {
	ids, err := orderIDs(ctx, tx, banShippedSQL, vendorID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, "SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE", id); err != nil {
			return err
		}
	}
	return nil
}

// openDisputeWindow sets the dispute window of a digital Monero escrow order just delivered (escrowTransitionHook; step
// 3 of the file comment), in the delivery's transaction, and records it: an order note that says whether it opened on
// funding (the automatic delivery, by the system) or on delivery (the vendor's), and a notice to the buyer and to the
// vendor stating its end.
func openDisputeWindow(ctx context.Context, tx *sql.Tx, o *Order) error {
	var until time.Time
	err := tx.QueryRowContext(ctx, "UPDATE escrows SET release_at=now()+make_interval(hours => $2) WHERE order_id=$1 AND coin='XMR' AND release_at IS NULL RETURNING release_at", o.ID, disputeWindowHours).Scan(&until)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var automatic bool
	if err = tx.QueryRowContext(ctx, "SELECT actor_id IS NULL FROM order_events WHERE order_id=$1 AND to_state='delivered' ORDER BY id DESC LIMIT 1", o.ID).Scan(&automatic); err != nil {
		return err
	}
	at := utcUntil(until)
	note := "Dispute window opened on delivery (Monero escrow): the vendor delivered the content."
	if automatic {
		note = "Dispute window opened on funding (Monero escrow): the content was delivered automatically."
	}
	note += " The buyer has until " + at + " (" + strconv.Itoa(disputeWindowHours) + " hours) to check it and open a dispute, or to confirm receipt. If the buyer does neither, the order completes at that time and the vendor claims the escrowed coins from the order page; nothing is paid to the vendor before then."
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, stateDelivered, note); err != nil {
		return err
	}
	short := "Order " + shortID(o.ID) + ": "
	for _, n := range [][2]string{
		{o.BuyerID, short + "check the delivered content now. You have until " + at + " to use Confirm receipt on the order page once it works, or to open a dispute if it does not. If you do neither, the order completes at that time and the vendor can claim the coins in this Monero escrow; a dispute can no longer be opened after it."},
		{o.VendorID, short + "the buyer has until " + at + " to confirm receipt or open a dispute. If the buyer does neither, the order completes at that time and you can claim the coins in this Monero escrow from the order page; no claim is possible before the order is completed."},
	} {
		if _, err = insertNotification(ctx, tx, n[0], n[1], o.ID); err != nil {
			return err
		}
	}
	return nil
}
