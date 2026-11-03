package market

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
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
//     and tells the buyer and the vendor (xmrRecheckReleaseDeadlines; A-728: that pass runs before public access returns, so a deadline
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
//     more than xmrOutageGap since the last asks for the recheck, with its notices naming the outage, so downtime
//     consumes neither a buyer's deadline nor a vendor's time. The check also runs first in a pass (xmrNoteDeadlineStepEarly),
//     and until it has, a stored time past that gap holds a buyer's cancel and opens a late dispute (xmrStaleStep, A-763).
//     The vendor's grace of an outage this check found applies only to a ship_by from when the gap began (DA108-1,
//     xmrCancelWaitFromKey): an order overdue before it was not held up by the outage. A restore's grace holds them all.
//     A grace whose end falls after the gap began is the same outage's, ended or not, and the round keeps its start or none
//     (xmrSameGrace, PAY113-1); the hold before that round, on a stale step or with the request pending, goes by what the round
//     will keep (xmrCancelRecheck, PAY113-2).
//     The check sees only that this step did not run: public access down while the watcher runs is not visible here, and the
//     operator asks for the recheck with the host command -request-deadline-recheck (RequestDeadlineRecheck), which audits
//     itself (PM-3) and, with no gap to go by, leaves the grace whole. Each recheck
//     records itself in its own transaction: when, its cause and how many deadlines moved (xmrLastRecheckKey, shown on
//     Admin → Payments), one audit row with the cause and the count and one notice to each administrator naming the cause
//     and no order (PM-3), and after it commits logs one fixed line, since it moves deadlines market-wide and a clock jump
//     can ask for it falsely. The /admin overview flags Payments while one is pending.
//  5. A vendor's ban (admin_ban.go, A-652d) moves every sooner deadline of the vendor's shipped or delivered orders with
//     no dispute to 72 hours after the ban, in the ban's transaction, and tells each buyer (xmrBanDeadlines). While the vendor is
//     banned, steps 2 and 3 and the buyer's confirmation say the release is then held (xmrReleaseHeld, A-652c).
//
//     An outage's request also compares the clocks first: while the database's clock disagrees with this process's or with
//     the newest block of a daemon that says it is synced, it waits and stays pending, at most 72 hours on both clocks
//     (xmrClocksApart, xmrRecheckWaits, PAY108-1). A clock step finds the same gap as a real outage, and the ends written then
//     only move later.
//
// No step but that comparison makes an RPC, and it has its own short budget (xmrClockCheckBudget). None runs while the restore
// gate is set: the recheck then runs in the first pass after the gate is cleared, before the release. Step 5 runs with the ban
// whatever the gate: it only moves deadlines later.

func init() { registerLoader("admin", xmrDeadlineRecheckLoader) }

// digitalWindowNote opens the order history note of a digital order's completion at the end of its dispute window.
const digitalWindowNote = "dispute window ended"

// xmrReleaseRecheckKey is the setting that asks for the Monero deadline recheck (step 4): a restore from backup writes
// it with the value true, overwriting what the backup held, and an outage (xmrNoteDeadlineStep) with xmrRecheckOutage.
const xmrReleaseRecheckKey = "escrow_xmr_release_recheck"

// xmrRecheckOutage is xmrReleaseRecheckKey's value when an outage asked for the recheck, and xmrCancelWaitCauseKey's when
// one recorded the grace. Any other value of the request is a restore's.
const xmrRecheckOutage = "outage"

// xmrLastRecheckKey is the setting each recheck writes in the transaction that consumes xmrReleaseRecheckKey: when it ran,
// its cause and how many deadlines it moved, as JSON (xmrLastRecheck), with no order data. A later recheck replaces it.
const xmrLastRecheckKey = "escrow_xmr_last_recheck"

// xmrLastDeadlineStepKey is the setting that holds the database's time of the last deadline step (A-730), an RFC 3339
// time in UTC. It is absent before the first step after the upgrade.
const xmrLastDeadlineStepKey = "escrow_xmr_last_deadline_step"

// xmrOutageThreshold is the least gap between two deadline steps that counts as an outage (A-730; xmrOutageGap).
const xmrOutageThreshold = time.Hour

// xmrOutageGap is the gap between two deadline steps that counts as an outage: xmrOutageThreshold, or more when
// PAYMENT_POLL_INTERVAL (up to an hour) would make ordinary passes that far apart (PAY101-9): twice the interval, so one
// missed tick is no outage, and the pass's budget. An invalid interval is the 30 seconds the watcher then uses.
func xmrOutageGap() time.Duration {
	interval, err := pollInterval()
	if err != nil {
		interval = 30 * time.Second
	}
	return max(xmrOutageThreshold, 2*interval+xmrEscrowPassTimeout)
}

// xmrCancelWaitKey is the setting the recheck writes in the transaction that consumes xmrReleaseRecheckKey: the end of
// the vendor's grace after a restore or an outage (step 4, A-720), an RFC 3339 time in UTC. Expired, it is harmless; a
// later recheck replaces it.
const xmrCancelWaitKey = "escrow_xmr_cancel_wait_until"

// xmrCancelWaitCauseKey is the setting that holds xmrRecheckOutage while xmrCancelWaitKey's end was recorded after an
// outage (A-730); absent, a restore recorded it.
const xmrCancelWaitCauseKey = "escrow_xmr_cancel_wait_cause"

// xmrCancelWaitFromKey is the setting that holds the time the gap began while xmrCancelWaitKey's end was recorded after an
// outage the watcher detected (DA108-1): only a ship_by from then on gets the grace. Absent, every ship_by before the end
// does: a restore's, and a host request's, whose outage began at a time nothing recorded.
const xmrCancelWaitFromKey = "escrow_xmr_cancel_wait_from"

// xmrOutageGapFromKey is the setting xmrNoteDeadlineStep writes with its request: the time of the last step before the gap,
// for the recheck that consumes that request to carry into xmrCancelWaitFromKey.
const xmrOutageGapFromKey = "escrow_xmr_outage_gap_from"

// xmrRecheckRequestedAudit is the audit row of a new host request for the recheck (PM-3). A recheck the request asked for
// follows it; one the watcher asked for has none.
const xmrRecheckRequestedAudit = "Monero deadline recheck requested from the host"

// xmrRecheckWaitKey is the setting that holds, as JSON (xmrRecheckWait), why an outage's recheck is waiting for the clocks to
// agree and when the wait began on each clock (PAY108-1). The recheck deletes it in the transaction that consumes the request.
const xmrRecheckWaitKey = "escrow_xmr_recheck_wait"

// The clocks an outage's recheck waits on (PAY108-1): how far apart counts as disagreeing, and the most it waits. A clock set
// by NTP stays well within the tolerance, which is under xmrOutageThreshold, so a step large enough to look like an outage is
// always seen. The wait is 72 hours, the time this market already gives an operator's attention (RequestDeadlineRecheck).
const (
	xmrClockTolerance = 5 * time.Minute
	xmrRecheckWaitMax = 72 * time.Hour
)

// xmrClockCheckBudget bounds the read of the daemon's tip in xmrClocksApart. A variable so tests can shorten it.
var xmrClockCheckBudget = 5 * time.Second

// xmrClocksDatabase and xmrClocksDaemon are xmrRecheckWait.Reason: which comparison disagreed. Each has a fixed line, with no
// time or difference in it, for the log and the panel (xmrClocksWhy).
const (
	xmrClocksDatabase = "database"
	xmrClocksDaemon   = "daemon"
)

// xmrClocksWhy is the fixed line of a reason in xmrRecheckWait.
func xmrClocksWhy(reason string) string {
	if reason == xmrClocksDaemon {
		return "the database's clock and the Monero daemon's newest block differ by more than MONERO_MAX_TIP_AGE (check the daemon's peers and block time)"
	}
	return "the application's clock and the database's clock differ by more than 5 minutes"
}

// xmrRecheckWait is what xmrRecheckWaitKey holds: the reason and the time on the database's and the application's clock when
// the wait began, so that a jump of one clock alone does not end it.
type xmrRecheckWait struct {
	Reason string    `json:"reason"`
	DB     time.Time `json:"db"`
	App    time.Time `json:"app"`
}

// xmrOutageCauseSQL: what the pages say of the recheck pending, or else of the vendor's grace, for the escrow es is an
// outage rather than a restore (A-730).
const xmrOutageCauseSQL = `(es.coin='XMR' AND COALESCE((SELECT value='` + xmrRecheckOutage + `' FROM settings WHERE key='` + xmrReleaseRecheckKey + `'),
	(SELECT value='` + xmrRecheckOutage + `' FROM settings WHERE key='` + xmrCancelWaitCauseKey + `'),false))`

// outageWhy is why an outage moved a deadline or made a cancel wait, as a clause (A-730). It names no part of the market
// and no length, since it must be true of the market, its database or Monero escrow being down, and of the operator's
// request when public access failed while the watcher ran (PAY101-10, RequestDeadlineRecheck).
const outageWhy = "an outage made this market unavailable"

// xmrNoRecheckSQL: no restore recheck of Monero deadlines is pending (step 4). The release (step 3) waits while one is,
// as a late dispute does (disputeClosedSQL, lateDispute): a recheck that failed leaves the setting, so the same pass
// completes no order whose restored deadline has passed before the buyer was given 72 hours (A-393, A-714).
const xmrNoRecheckSQL = "NOT EXISTS (SELECT 1 FROM settings WHERE key='" + xmrReleaseRecheckKey + "')"

// xmrLastStepSQL is the stored time of the last deadline step for the Monero escrow es, and NULL for any other coin or with
// none stored, for xmrStaleStep. It is selected with now() in the statement after the order row lock that reads the recheck's
// request, so one snapshot shows both: a check that commits the request and a new time cannot fall between them. It writes
// nothing, and Go reads the value (A-769): a cast here would need a guard against a value it fails on.
const xmrLastStepSQL = `(SELECT value FROM settings WHERE es.coin='XMR' AND key='` + xmrLastDeadlineStepKey + `')`

// xmrStaleStep is whether step, from xmrLastStepSQL, is older than the gap at the database's time now, so the next check will
// ask for the recheck: the buyer's cancel and a late dispute treat it as asked for already (noShipCancel, lateDispute; A-763).
// It closes the time between a gap and that check. No stored step, one that cannot be read and no gap are false, as
// xmrNoteDeadlineStep asks for nothing on the first two.
func (a *App) xmrStaleStep(step sql.NullString, now time.Time) bool {
	gap, ok := a.xmrStaleStepGap()
	if !ok || !step.Valid {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, step.String)
	return err == nil && now.Sub(at) > gap
}

// xmrStaleStepGap is xmrOutageGap, or none with Monero escrow off, when no watcher runs to end the hold and a cancel and a
// dispute keep the rules UPGRADING.md gives for that time (DA104-3).
func (a *App) xmrStaleStepGap() (time.Duration, bool) {
	if a.escrowXMR == nil {
		return 0, false
	}
	return xmrOutageGap(), true
}

// xmrDisputeRecheck is lateDispute's rule for whether a recheck keeps a passed release deadline or dispute window open, and
// whether an outage is why: the request as selected (requested, outage), or a stale step, which the next check turns into the
// outage's request (xmrStaleStep, A-763). The order page goes by it too, so it offers the dispute the action takes (A-791).
func (a *App) xmrDisputeRecheck(step sql.NullString, now time.Time, requested, outage bool) (held, byOutage bool) {
	if !requested && a.xmrStaleStep(step, now) {
		return true, true
	}
	return requested, outage
}

// xmrSameGrace is whether a round whose gap began at gap belongs to the grace ending at end (PAY113-1): that grace's end fell
// in the gap or after it, so the vendor had no use of the time the outage took, and the round keeps the start it has, or none.
// A gap read from a clock that had stepped back says little of the grace, and the step that saw it dropped the starts this
// keeps (xmrNoteDeadlineStep, PAY113R-1), so what is kept then is none.
func xmrSameGrace(gap time.Time, end sql.NullTime) bool {
	return end.Valid && end.Time.After(gap)
}

// xmrCancelHoldSQL is what xmrCancelRecheck reads for the escrow es, in the order of xmrCancelHold.dest: selected after the
// order row lock in the statement that reads the request, so one snapshot shows them all.
const xmrCancelHoldSQL = `(SELECT value FROM settings WHERE es.coin='XMR' AND key='` + xmrOutageGapFromKey + `'),` + xmrLastStepSQL + `,
	(SELECT value::timestamptz FROM settings WHERE es.coin='XMR' AND key='` + xmrCancelWaitKey + `'),
	(SELECT value::timestamptz FROM settings WHERE es.coin='XMR' AND key='` + xmrCancelWaitFromKey + `'),now()`

// xmrCancelHold is what xmrCancelHoldSQL selects: the pending request's gap, the last step, the grace's end and start, and the
// database's time.
type xmrCancelHold struct {
	gapFrom, step       sql.NullString
	graceEnd, graceFrom sql.NullTime
	now                 time.Time
}

func (h *xmrCancelHold) dest() []any {
	return []any{&h.gapFrom, &h.step, &h.graceEnd, &h.graceFrom, &h.now}
}

// xmrCancelRecheck reads the request for the buyer's no-ship cancel of an order with shipBy, for noShipCancel and the pages:
// whether a recheck holds this order and whether an outage is why. It takes the request as selected (requested, outage). A
// stale step counts as the outage's request the next check will write, with that step as its gap (A-763). A watcher-found gap
// then holds only what its round will: an order overdue before the start that round leaves was not held up by the outage
// (PAY113-2), and is cancelled now as it would be after the round. A host's request and a restore's have no gap to go by, and
// hold them all. The caller's check that ship_by has passed still applies to the rest.
//
// The stored start is only as true as the clock that wrote it, so the comparison holds from xmrOutageGap() before it
// (xmrGraceHeldFrom, PAY113R-2) and a step that sees the clock behind its stored time drops the starts (PAY113R-1). Those
// cover a forward jump shorter than the gap, which makes no request yet stores a step ahead of true time, and a step back,
// with an order paid under the behind clock. These clock cases remain unsafe. While the database's clock is ahead, every
// deadline falls due early by true time: besides a refund, an order can complete and its release go to the vendor (A-784):
//   - A forward jump shorter than the gap itself makes a ship_by or release_at inside the jump look passed, with no gap to hold
//     it, and such jumps add up. Nothing sees this one (it was so before A-779).
//   - A forward jump longer than the gap whose request has run with the clocks still apart (past the wait's bound, or both
//     clocks together with no synced daemon) stores a step further ahead than the margin, and needs no outage: its moved
//     deadlines fall due 72 hours on, with the clock still ahead. An outage that outlasts the jump with no correcting pass
//     between also treats an order due in the jump as overdue before the gap.
//   - A step back and its correction between two steps is not seen. No deposit is credited without a pass, which starts with
//     a step, so this needs a pass whose step failed or a step the restore gate withheld (xmrDeadlines); not exercised.
//
// These are reasoning from the code on a simulated database clock (simDBClock), not a stepped host.
func (a *App) xmrCancelRecheck(h xmrCancelHold, shipBy sql.NullTime, requested, outage bool) (holds, byOutage bool) {
	gapFrom := h.gapFrom
	if !requested && a.xmrStaleStep(h.step, h.now) {
		requested, outage, gapFrom = true, true, h.step
	}
	if !requested || !outage || !shipBy.Valid {
		return requested, outage
	}
	start := xmrRoundStart(gapFrom, h.graceEnd, h.graceFrom)
	return !start.Valid || !shipBy.Time.Before(xmrGraceHeldFrom(start.Time)), true
}

// xmrRoundStart is the start the round for a pending outage request whose gap began at gapFrom will leave (PAY113-2): the
// grace's start graceFrom, or none, when the gap belongs to the grace ending at graceEnd (xmrSameGrace, PAY113-1), and otherwise
// the gap. No gap, or one that cannot be read, is none: the round holds every order. xmrCancelRecheck, the panel and the host
// command go by it, so what they say and what the cancel does cannot drift apart (A-785).
func xmrRoundStart(gapFrom sql.NullString, graceEnd, graceFrom sql.NullTime) sql.NullTime {
	gap, err := time.Parse(time.RFC3339Nano, gapFrom.String)
	if !gapFrom.Valid || err != nil {
		return sql.NullTime{}
	}
	if xmrSameGrace(gap, graceEnd) {
		return graceFrom
	}
	return sql.NullTime{Time: gap, Valid: true}
}

// xmrPendingHeldFrom is, for a pending outage request, the earliest ship_by its round holds as the pages name it
// (xmrRoundStart, xmrGraceHeldFrom), or "" when it holds every order. It takes the three settings as stored: a time that
// cannot be read is none, as on the panel's other lines.
func xmrPendingHeldFrom(gapFrom, graceEnd, graceFrom sql.NullString) string {
	readTime := func(s sql.NullString) sql.NullTime {
		t, err := time.Parse(time.RFC3339Nano, s.String)
		return sql.NullTime{Time: t, Valid: s.Valid && err == nil}
	}
	if start := xmrRoundStart(gapFrom, readTime(graceEnd), readTime(graceFrom)); start.Valid {
		return utcUntil(xmrGraceHeldFrom(start.Time))
	}
	return ""
}

// xmrGraceHeldFrom is the earliest ship_by a found gap's start holds (PAY113R-2): the start less xmrOutageGap(), the most a
// jump that makes no request can have stored a step ahead of true time. An order due in that margin waits though it may be
// overdue before the gap: this errs towards holding. Every comparison with a found gap's start goes by it: xmrCancelRecheck,
// xmrCancelWaitSQL and the panel.
func xmrGraceHeldFrom(start time.Time) time.Time {
	return start.Add(-xmrOutageGap())
}

// xmrCancelWaitSQL is the end of the vendor's grace after a restore (step 4, A-720) for the escrow es, or NULL when none
// applies: not a Monero escrow, the grace has ended, es.ship_by does not pass before it, or es.ship_by passed before the
// outage that recorded it began, less the margin (xmrCancelWaitFromKey, DA108-1, xmrGraceHeldFrom). A statement after the order
// row lock reads it for the buyer's cancel (noShipCancel). OFFSET 0 keeps each cast to the key's own row. A function for the
// margin, which follows PAYMENT_POLL_INTERVAL: only that whole number of seconds goes into the statement.
func xmrCancelWaitSQL() string {
	return `(SELECT w.until FROM (SELECT value::timestamptz AS until FROM settings WHERE key='` + xmrCancelWaitKey + `' OFFSET 0) w
	WHERE es.coin='XMR' AND w.until>now() AND w.until>es.ship_by
	AND COALESCE(es.ship_by>=(SELECT f.since FROM (SELECT value::timestamptz AS since FROM settings WHERE key='` + xmrCancelWaitFromKey + `' OFFSET 0) f)-interval '` + strconv.FormatInt(int64(xmrOutageGap()/time.Second), 10) + ` seconds',true))`
}

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

// xmrNoteDeadlineStepEarly is xmrNoteDeadlineStep at the start of a pass, before any node or wallet call (A-730): the
// deadline step runs last, and a first pass after an outage can spend minutes on its wallets, in which a dispute would be
// refused as late and a no-ship cancel accepted. The server accepts requests from just after the watcher starts
// (cmd/server's app.Start, then serveAll), and until a check succeeds the stale time itself holds those actions
// (xmrStaleStep, A-763). A failure does not touch the pass: the check in xmrDeadlines runs anyway and reports it. Under
// the restore gate it writes nothing, as there. It needs neither the pass's connection nor the watcher's lock (PAY101-11): another process's check waits on the row, then reads
// the new time, so two at once ask once.
func (a *App) xmrNoteDeadlineStepEarly(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, xmrDeadlineBudget)
	defer cancel()
	if gate, err := restoreGateSet(ctx, a.db); err == nil && !gate {
		_ = a.xmrNoteDeadlineStep(ctx)
	}
}

// xmrNoteDeadlineStep records the database's time of this deadline step and, when the last was more than
// xmrOutageGap ago, asks for the recheck, in one transaction (A-730): a request already there stays as it is, so a
// restored database, which carries an old time, stays a restore; one that fails keeps the request for the next pass. A
// request it writes itself comes with the time of that last step, where the gap began (xmrOutageGapFromKey, DA108-1).
// No stored time asks for nothing, nor does one that cannot be read, which only a hand could have written. It runs after
// the restore gate's check, so the time of a gated step stays old, and it is the restore's request that the first pass
// after the gate finds. This is the database's clock, since it is what every deadline is compared with.
//
// A clock behind the stored time by more than xmrClockTolerance has stepped back, and an order paid under it gets a ship_by no
// start stored earlier says anything about, once the clock is corrected. So that step drops both starts: a grace or a request
// then holds every order, in the same transaction, with one fixed log line (PAY113R-1). The step's new time ends the condition.
func (a *App) xmrNoteDeadlineStep(ctx context.Context) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var last string
	var now time.Time
	steppedBack := false
	err = tx.QueryRowContext(ctx, "SELECT value,now() FROM settings WHERE key=$1 FOR UPDATE", xmrLastDeadlineStepKey).Scan(&last, &now)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		at, parseErr := time.Parse(time.RFC3339Nano, last)
		if parseErr == nil && now.Sub(at) > xmrOutageGap() {
			asked, err := tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO NOTHING", xmrReleaseRecheckKey, xmrRecheckOutage)
			if err != nil {
				return err
			}
			if n, err := asked.RowsAffected(); err != nil {
				return err
			} else if n > 0 {
				if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value", xmrOutageGapFromKey, last); err != nil {
					return err
				}
			}
		}
		if parseErr == nil && at.Sub(now) > xmrClockTolerance {
			steppedBack = true
			if _, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key IN ($1,$2)", xmrCancelWaitFromKey, xmrOutageGapFromKey); err != nil {
				return err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, xmrLastDeadlineStepKey); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if steppedBack {
		log.Printf("monero escrow deadline step: the database's clock is behind the stored step; any recorded start of a cancel hold was dropped, so a hold covers every order")
	}
	return nil
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
	zero, live, err := xmrCountedZero(ctx, tx, orderID)
	if err != nil {
		return false, err
	}
	if banned, err := orderVendorBanned(ctx, tx, orderID); err != nil {
		return false, err
	} else if banned {
		days, err := holdRefundDaysShown(ctx, tx)
		if err != nil {
			return false, err
		}
		note += xmrHeldNote + " " + holdRefundLater(days) // PAY90-3, A-652f
	} else if zero { // A-770
		note += xmrZeroNotice("release", live)
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
// less than 72 hours away, once, and tells the vendor the buyer's deadline (step 2 of the file comment); nothing while a
// recheck is pending (xmrNoRecheckSQL, A-762), since it would name a date about to be replaced: the recheck's own notice
// counts as the reminder.
func (a *App) xmrRemindDeadlines(ctx context.Context) error {
	ids, err := orderIDs(ctx, a.db, `SELECT o.id FROM orders o JOIN escrows es ON es.order_id=o.id WHERE es.coin='XMR' AND o.state='shipped' AND es.reminded_at IS NULL AND `+releaseSoonSQL+`
		AND `+noDisputeSQL+` AND `+xmrNoRecheckSQL+` ORDER BY es.release_at`)
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
	if _, err = tx.ExecContext(ctx, "SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE", orderID); err != nil { // PAY113-4: what is counted, below
		return err
	}
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
	} else if zero, live, err := xmrCountedZero(ctx, tx, orderID); err != nil {
		return err
	} else if zero { // PAY113-4, as the completion says (xmrReleaseIfDue)
		for i := range notes {
			notes[i][1] += " " + xmrZeroNotice("release", live)
		}
	}
	for _, n := range notes {
		if _, err = insertNotification(ctx, tx, n[0], n[1], orderID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// xmrDeadlineWhat names what release_at is for an order in state: a delivered digital order's dispute window (A-709), or
// a shipped physical order's release deadline.
func xmrDeadlineWhat(state string) string {
	if state == stateDelivered {
		return "dispute window"
	}
	return "release deadline"
}

// xmrOutageMovedNote opens the order note of a deadline an outage's recheck moved to until (a time as utcUntil writes it).
// It is the one record of that move per order, since the recheck resets release_recheck and the cause setting is
// global, so the order page reads it (xmrMovedByOutage).
func xmrOutageMovedNote(state, until string) string {
	what := xmrDeadlineWhat(state)
	return strings.ToUpper(what[:1]) + what[1:] + " moved to " + until + " after an outage (Monero escrow)"
}

// xmrMovedByOutage reports whether an outage's recheck moved the deadline of orderID to until, where it still is (VEN103-1).
// Only the system's own note counts (actor_id IS NULL), as a party's order note can say anything, and it must name
// until, so a later move by a ban or a restore ends the claim.
func xmrMovedByOutage(ctx context.Context, q rowQuerier, orderID, state, until string) (bool, error) {
	var moved bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND from_state=$2 AND to_state=$2 AND starts_with(note,$3))`,
		orderID, state, xmrOutageMovedNote(state, until)).Scan(&moved)
	return moved, err
}

// xmrRecheckReleaseDeadlines moves, once after each restore from backup or outage (A-730), every release deadline of a
// shipped Monero escrow order, and every dispute window of a delivered one (A-709), that has passed or ends within 72
// hours (A-728), with no dispute, to now + 72 hours, with an order note and a notice to the buyer and to the vendor
// (VEN103-1: the move counts as the reminder, so it is the vendor's only word of the date, and replaces any earlier), in one
// transaction that consumes the request (step 4 of the file comment; recheckReleaseDeadlines' rules: only the
// rechecks reset and set release_recheck, so a second restore rechecks as the first did). The same transaction records
// the end of the vendor's grace (xmrCancelWaitKey, A-720), with no gap between the two a buyer's cancel could use. A row
// the restored database carries is replaced only by a later end, so a restore never shortens a grace.
//
// After an outage the order page must not say a restore moved the deadline: release_recheck is what it reads, so the
// flag is reset again (the ban's way round migration 079's trigger, xmrBanDeadlines); the order's history says why, and
// the page reads that note (xmrOutageMovedNote).
//
// An outage's request first compares the clocks (xmrClocksApart, PAY108-1): a forward step of the database's clock finds the
// same gap as a month's outage, and a recheck on it writes ends that only move later. While they disagree it waits, the
// request stays pending and every hold stays (xmrRecheckWaits): deferred, never treated as no outage. The comparison, with
// its daemon read, runs before the transaction, so no RPC waits with the request's row locked. A restore's request runs as
// it always did: a restored database's deadlines must not wait for a clock.
func (a *App) xmrRecheckReleaseDeadlines(ctx context.Context) error {
	var seen string
	var dbNow time.Time
	err := a.db.QueryRowContext(ctx, "SELECT value,now() FROM settings WHERE key=$1", xmrReleaseRecheckKey).Scan(&seen, &dbNow)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	apart := ""
	if seen == xmrRecheckOutage {
		apart = a.xmrClocksApart(ctx, dbNow)
	}
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
	ranApart := false
	if outage && apart != "" {
		waiting, err := xmrRecheckWaits(ctx, tx, apart)
		if err != nil || waiting {
			return err
		}
		ranApart = true // the wait is over and they still disagree: it runs, and says so
	}
	cause := "restore"
	if outage {
		cause = xmrRecheckOutage
	}
	count := 0
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
		var buyer, vendor string
		if err = tx.QueryRowContext(ctx, `UPDATE escrows es SET release_at=GREATEST(es.release_at,`+releaseGraceSQL+`),release_recheck=true,reminded_at=COALESCE(es.reminded_at,now())
			FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=es.order_id AND es.order_id=$1 AND es.coin='XMR' RETURNING es.release_at,o.buyer_id,p.vendor_id`, id).Scan(&at, &buyer, &vendor); err != nil {
			return err
		}
		until, what, check, outcome, vendorOutcome := utcUntil(at), xmrDeadlineWhat(state), "if the goods have arrived, or open a dispute before then if they have not", "the market releases the coins in this Monero escrow to the vendor", "the market releases the coins in this Monero escrow to you"
		if state == stateDelivered { // A-709: a digital order's dispute window
			check, outcome, vendorOutcome = "if the delivered content works, or open a dispute before then if it does not", "the vendor can claim the coins in this Monero escrow", "you can claim the coins in this Monero escrow from the order page"
		}
		closed, vendorClosed, nothingSent := "", "", ""
		if banned, err := orderVendorBanned(ctx, tx, id); err != nil { // PAY101-6: after the row lock, as a ban locks the order first
			return err
		} else if banned {
			closed, outcome = banDeadlineNote+". ", xmrBanHeldPayment
			vendorClosed, vendorOutcome = "Your account is closed. ", xmrBanHeldPayment
		} else if zero, live, err := xmrCountedZero(ctx, tx, id); err != nil {
			return err
		} else if zero { // PAY113-4, as the completion says (xmrReleaseIfDue)
			nothingSent = " " + xmrZeroNotice("release", live)
		}
		note, why := strings.ToUpper(what[:1])+what[1:]+" moved to "+until+" after a restore from backup (Monero escrow): the deadline in the restored records has passed or ends within 72 hours, so the buyer has 72 hours from this recheck to confirm receipt or open a dispute.", "this market restored its records from a backup"
		if outage {
			if _, err = tx.ExecContext(ctx, "UPDATE escrows SET release_recheck=false WHERE order_id=$1 AND coin='XMR'", id); err != nil {
				return err
			}
			note, why = xmrOutageMovedNote(state, until)+": this market was unavailable, and this deadline has passed or ends within 72 hours, so the buyer has 72 hours from this recheck to confirm receipt or open a dispute.", outageWhy
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", id, state, note); err != nil {
			return err
		}
		moved := "Order " + shortID(id) + ": " + why + ", and the " + what + " of this order has passed or ends within 72 hours, so it was moved to " + until + ". "
		for _, n := range [][2]string{
			{buyer, moved + closed + "Use Confirm receipt on the order page " + check + ". If you do neither, the order completes at that time and " + outcome + "." + nothingSent},
			{vendor, moved + vendorClosed + "The buyer has until then to confirm receipt or open a dispute. If the buyer does neither, the order completes at that time and " + vendorOutcome + "." + nothingSent},
		} {
			if _, err = insertNotification(ctx, tx, n[0], n[1], id); err != nil {
				return err
			}
		}
		count++
	}
	// The start of the grace (DA108-1). A round within a running grace is the same outage, so it keeps what that grace has,
	// its start or none, and so is one whose grace ended after the watcher's gap began (xmrSameGrace, PAY113-1); otherwise it
	// takes the gap's start if the watcher asked. A restore's grace holds every order.
	var gapFrom sql.NullString
	var graceEnd sql.NullTime
	var txNow time.Time
	if err = tx.QueryRowContext(ctx, "SELECT (SELECT value FROM settings WHERE key=$1),(SELECT value::timestamptz FROM settings WHERE key=$2),now()", xmrOutageGapFromKey, xmrCancelWaitKey).Scan(&gapFrom, &graceEnd, &txNow); err != nil {
		return err
	}
	running := graceEnd.Valid && graceEnd.Time.After(txNow)
	if gapFrom.Valid {
		if gap, parseErr := time.Parse(time.RFC3339Nano, gapFrom.String); parseErr != nil {
			gapFrom.Valid = false // one that cannot be read, which only a hand could have written, is no start: as a host's request
		} else {
			running = xmrSameGrace(gap, graceEnd)
		}
	}
	switch {
	case outage && running:
	case outage && gapFrom.Valid:
		_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value", xmrCancelWaitFromKey, gapFrom.String)
	default:
		_, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", xmrCancelWaitFromKey)
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", xmrOutageGapFromKey); err != nil {
		return err
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
	if _, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key IN ($1,$2)", xmrReleaseRecheckKey, xmrRecheckWaitKey); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,json_build_object('at',now(),'cause',$2::text,'moved',$3::int,'clocks_apart',$4::boolean)::text)
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, xmrLastRecheckKey, cause, count, ranApart); err != nil {
		return err
	}
	action := "Monero escrow deadline recheck (cause: " + cause + "): moved " + strconv.Itoa(count) + " order deadline(s)"
	if ranApart {
		action += ", with the clocks still apart after the wait"
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES(NULL,$1)", action); err != nil {
		return err
	}
	after := "a restore from backup"
	if outage {
		after = "an outage"
	}
	if ranApart { // QA112-4: past the wait's bound, so a clock left wrong may have moved its dates too far
		after += ", with the clocks still apart"
	}
	// PM-3: in the transaction that consumes the request, so one notice per recheck however often a pass retries. It
	// names no order and no count: the panel it points to has the count.
	if err = notifyAdmins(ctx, tx, "A Monero escrow deadline recheck ran after "+after+". Admin > Payments shows how many deadlines it moved and any hold on buyers' no-ship cancels."); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if ranApart {
		log.Printf("monero escrow deadline recheck: cause=%s moved=%d clocks_apart=true", cause, count)
	} else {
		log.Printf("monero escrow deadline recheck: cause=%s moved=%d", cause, count)
	}
	return nil
}

// xmrClocksApart compares the database's time dbNow with this process's clock and with the daemon's newest block, and returns
// the reason they disagree, or "" (PAY108-1). The application's clock is time.Now, not the watcher's clock, which tests fix.
// A check that cannot run proves nothing and stops nothing: a daemon that is off, unreachable or hung (xmrClockCheckBudget),
// or that does not itself say it is synced (still catching up after the same outage, or an unrestricted RPC with no peers), or
// the fakechain, whose tip is old by design, leaves only the two clocks that could be read. A restricted RPC cannot show its
// peers (notSynced does not read the count), so a synced one with an old tip does disagree, and waits like any other (QA112-1).
// Run with those two already apart, it asks no daemon.
func (a *App) xmrClocksApart(ctx context.Context, dbNow time.Time) string {
	if dbNow.Sub(time.Now()).Abs() > xmrClockTolerance {
		return xmrClocksDatabase
	}
	x := a.escrowXMR
	if x == nil || !x.ready.Load() {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, xmrClockCheckBudget)
	defer cancel()
	node, err := x.rpc.node(ctx)
	if err != nil || node.NetType == "fakechain" || node.NetType != x.rpc.nettype() || node.notSynced(dbNow, math.MaxInt64) != "" {
		return ""
	}
	if dbNow.Sub(time.Unix(node.TipTime, 0)).Abs() > moneroMaxTipAge() {
		return xmrClocksDaemon
	}
	return ""
}

// xmrRecheckWaits decides, in the recheck's transaction, whether its request keeps waiting on clocks that disagree for
// reason, and if so commits the record and reports true: no deadline has moved, and the setting that holds the request keeps
// every hold in force. It logs and writes when the reason changes, so a pass adds nothing. The wait ends, with the clocks still
// apart, once xmrRecheckWaitMax has passed on both clocks: it must not hold a market for good, and one clock's jump must not
// end it. A clock that went back since the wait began says nothing of how long it has run, so the count starts again.
func xmrRecheckWaits(ctx context.Context, tx *sql.Tx, reason string) (bool, error) {
	var stored sql.NullString
	var dbNow time.Time
	if err := tx.QueryRowContext(ctx, "SELECT (SELECT value FROM settings WHERE key=$1),now()", xmrRecheckWaitKey).Scan(&stored, &dbNow); err != nil {
		return false, err
	}
	appNow := time.Now().UTC()
	var wait xmrRecheckWait
	fresh := !stored.Valid || json.Unmarshal([]byte(stored.String), &wait) != nil || wait.DB.IsZero() || wait.App.IsZero() || dbNow.Before(wait.DB) || appNow.Before(wait.App)
	switch {
	case fresh:
		wait.DB, wait.App = dbNow, appNow
	case dbNow.Sub(wait.DB) >= xmrRecheckWaitMax && appNow.Sub(wait.App) >= xmrRecheckWaitMax:
		return false, nil
	case wait.Reason == reason:
		return true, nil
	}
	wait.Reason = reason
	raw, err := json.Marshal(wait)
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value", xmrRecheckWaitKey, string(raw)); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	log.Printf("monero escrow recheck deferred: %s; its request stays pending and every hold stays in force", xmrClocksWhy(reason))
	return true, nil
}

// xmrLastRecheck is what xmrLastRecheckKey holds.
type xmrLastRecheck struct {
	At          time.Time `json:"at"`
	Cause       string    `json:"cause"`
	Moved       int       `json:"moved"`
	ClocksApart bool      `json:"clocks_apart,omitempty"`
}

// RequestDeadlineRecheck is the host command -request-deadline-recheck: public access failed while the watcher runs, which
// the watcher cannot see, so the operator asks for the outage's recheck (step 4 of the file comment). It may run while the
// application serves the database. In one transaction it writes one setting, xmrReleaseRecheckKey, with the value
// xmrRecheckOutage, and one audit row with no actor (xmrRecheckRequestedAudit, PM-3), and only if no request is there and the
// restore gate is not set: the statement checks both, so a restore's request is never replaced and a restore's first pass
// still says a restore. A pending request is reported as it stands, and the gate is refused, each changing nothing. It
// returns what the operator is told.
func RequestDeadlineRecheck(ctx context.Context, db *sql.DB) (string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", recheckRequestFailed(err)
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, "INSERT INTO settings(key,value) SELECT $1,$2 WHERE NOT "+restoreGateSQL+" ON CONFLICT (key) DO NOTHING", xmrReleaseRecheckKey, xmrRecheckOutage)
	if err != nil {
		return "", recheckRequestFailed(err)
	}
	written, err := r.RowsAffected()
	if err != nil {
		return "", recheckRequestFailed(err)
	}
	if written > 0 {
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES(NULL,$1)", xmrRecheckRequestedAudit); err != nil {
			return "", recheckRequestFailed(err)
		}
	}
	if err = tx.Commit(); err != nil {
		return "", recheckRequestFailed(err)
	}
	const next = " With Monero escrow on, the watcher's next pass moves every shipped or delivered order's deadline that has passed or ends within 72 hours to 72 hours later and tells each buyer and vendor. With ESCROW_XMR off the request stays pending until the first pass after it is on again, and a buyer's no-ship cancel past its time waits until then"
	const hold = ". Each recheck also holds a buyer's no-ship cancel for 72 hours, even if it moves no deadline: Admin > Payments states the hold and its end. "
	const limits = "It cannot undo a completion. Repeat this within every 72 hours until public access returns, and run this once more when public access returns, so a buyer does not come back with minutes left. Each repeat notifies every buyer and vendor it moves again, so repeat no more often than that needs. If this market's clocks disagree, the recheck waits for them, 72 hours at the most, and Admin > Payments says why: correct the clock. Confirm on Admin > Payments that it shows pending or has just run, or by the operator guide's check if that panel shares the failed ingress: this command writes only to the database its URL names."
	if written > 0 {
		return "Deadline recheck now pending (outage)." + next + hold + limits, nil
	}
	var gate bool
	var value, gapFrom, graceEnd, graceFrom sql.NullString
	if err = db.QueryRowContext(ctx, "SELECT "+restoreGateSQL+",(SELECT value FROM settings WHERE key=$1),(SELECT value FROM settings WHERE key=$2),(SELECT value FROM settings WHERE key=$3),(SELECT value FROM settings WHERE key=$4)",
		xmrReleaseRecheckKey, xmrOutageGapFromKey, xmrCancelWaitKey, xmrCancelWaitFromKey).Scan(&gate, &value, &gapFrom, &graceEnd, &graceFrom); err != nil {
		return "", recheckRequestFailed(err)
	}
	switch {
	case gate:
		return "", errors.New("the restore gate is set: no watcher pass completes an order or runs a recheck until it is cleared, and the first pass after that runs the restore's recheck. Follow docs/testnet-runbook.md; nothing was changed")
	case !value.Valid: // a pass consumed the request between the two statements
		return "", errors.New("a recheck ran just now; run this again if public access is still down; nothing was changed")
	case value.String == xmrRecheckOutage:
		// A-785: the watcher's request, with the gap's start it stored, holds only what its round will (xmrCancelRecheck).
		// QA114-N1: and the grace its round leaves does not hold those orders either (the panel's CancelHeldFrom line).
		if from := xmrPendingHeldFrom(gapFrom, graceEnd, graceFrom); from != "" {
			return "Deadline recheck already pending (outage); nothing was changed." + next + ", except an order overdue since before " + from + ", which can still be cancelled" +
				". Each recheck also holds a buyer's no-ship cancel for 72 hours, even if it moves no deadline, this one except an order overdue since before that time: Admin > Payments states the hold and its end. " + limits, nil
		}
		return "Deadline recheck already pending (outage); nothing was changed." + next + hold + limits, nil
	}
	return "A restore's deadline recheck is already pending; nothing was changed. Its notices say a restore, as they must." + next + hold + limits, nil
}

// recheckRequestFailed is RequestDeadlineRecheck's failed statement, by class only: pgconn's own text names the URL's user,
// database and host. It does not say nothing changed, since a connection lost at the commit cannot tell; a repeat is safe.
func recheckRequestFailed(err error) error {
	return errors.New("database statement failed (" + errorCause(err) + "); check DATABASE_URL and service health, then run this again")
}

// XMRDeadlineRecheckView is Admin → Payments' read-only line on the recheck (step 4 of the file comment): whether one is
// pending and who asked, and the last one's record. It names no order.
type XMRDeadlineRecheckView struct {
	On, Pending, PendingOutage bool
	Ran, RanOutage             bool
	RanAt                      string
	Moved                      int
	RanClocksApart             bool   // the last one ran past the wait's bound with the clocks apart (PAY108-1)
	WaitingWhy                 string // why an outage's pending recheck waits for the clocks (xmrClocksWhy, PAY108-1), fixed text
	CancelHeldUntil            string // the end of the vendor's grace, while it runs (xmrCancelWaitKey, DA108-2)
	CancelHeldFrom             string // when the grace has a gap's start, the earliest ship_by it holds (xmrCancelWaitFromKey, xmrGraceHeldFrom)
	PendingHeldFrom            string // when the pending outage request has a start, the earliest ship_by it holds (xmrPendingHeldFrom, A-785)
}

// xmrDeadlineRecheckLoader fills XMRDeadlineRecheckView. A record that cannot be read, which only a hand could have
// written, shows as none, and so does a hold on cancels. With Monero escrow off the section still shows: a request then
// stays pending, and a hold still applies.
func xmrDeadlineRecheckLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" {
		return nil
	}
	var pending, last, waitUntil, waitFrom, waiting, gapFrom sql.NullString
	var now time.Time
	if err := a.db.QueryRowContext(ctx, "SELECT (SELECT value FROM settings WHERE key=$1),(SELECT value FROM settings WHERE key=$2),(SELECT value FROM settings WHERE key=$3),(SELECT value FROM settings WHERE key=$4),(SELECT value FROM settings WHERE key=$5),(SELECT value FROM settings WHERE key=$6),now()",
		xmrReleaseRecheckKey, xmrLastRecheckKey, xmrCancelWaitKey, xmrCancelWaitFromKey, xmrRecheckWaitKey, xmrOutageGapFromKey).Scan(&pending, &last, &waitUntil, &waitFrom, &waiting, &gapFrom, &now); err != nil {
		return err
	}
	v := &XMRDeadlineRecheckView{On: a.escrowXMR != nil, Pending: pending.Valid, PendingOutage: pending.String == xmrRecheckOutage}
	if v.PendingOutage {
		v.PendingHeldFrom = xmrPendingHeldFrom(gapFrom, waitUntil, waitFrom)
	}
	var rec xmrLastRecheck
	if last.Valid && json.Unmarshal([]byte(last.String), &rec) == nil && !rec.At.IsZero() {
		v.Ran, v.RanOutage, v.RanAt, v.Moved, v.RanClocksApart = true, rec.Cause == xmrRecheckOutage, utcUntil(rec.At), rec.Moved, rec.ClocksApart
	}
	var wait xmrRecheckWait
	if v.PendingOutage && waiting.Valid && json.Unmarshal([]byte(waiting.String), &wait) == nil {
		v.WaitingWhy = xmrClocksWhy(wait.Reason)
	}
	if until, err := time.Parse(time.RFC3339Nano, waitUntil.String); err == nil && until.After(now) {
		v.CancelHeldUntil = utcFrom(until) // as the buyer's refusal names it
		if from, err := time.Parse(time.RFC3339Nano, waitFrom.String); err == nil {
			v.CancelHeldFrom = utcUntil(xmrGraceHeldFrom(from))
		}
	}
	d.XMRDeadlineRecheck = v
	return nil
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
