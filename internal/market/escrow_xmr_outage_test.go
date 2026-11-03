package market

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// setLastDeadlineStep stores the time of the last deadline step, age before the database's now.
func (x *xmrEscrowEnv) setLastDeadlineStep(age string) {
	x.t.Helper()
	agExec(x.testEnv, `INSERT INTO settings(key,value) VALUES($1,to_char((now()-$2::interval) AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, xmrLastDeadlineStepKey, age)
}

// recheckKey is the value of the recheck's request, "" when there is none.
func (x *xmrEscrowEnv) recheckKey() string {
	x.t.Helper()
	return agStr(x.testEnv, "SELECT COALESCE((SELECT value FROM settings WHERE key=$1),'')", xmrReleaseRecheckKey)
}

// lastDeadlineStep is the stored time of the last deadline step, as stored.
func (x *xmrEscrowEnv) lastDeadlineStep() string {
	x.t.Helper()
	return agStr(x.testEnv, "SELECT COALESCE((SELECT value FROM settings WHERE key=$1),'')", xmrLastDeadlineStepKey)
}

// movedNotes lists order's notes of a moved deadline.
func (x *xmrEscrowEnv) movedNotes(order string) []string {
	x.t.Helper()
	return agStrings(x.t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1 AND strpos(note,'moved to')>0 ORDER BY id", order)
}

// movedNotices lists uid's notices for order that say the recheck moved its deadline.
func (x *xmrEscrowEnv) movedNotices(uid, order string) []string {
	x.t.Helper()
	var moved []string
	for _, n := range x.noticesOf(uid, order) {
		if strings.Contains(n, "so it was moved to") {
			moved = append(moved, n)
		}
	}
	return moved
}

// wantVendorMoved fails unless the vendor of order has exactly one notice that its deadline moved, naming the date now on
// the order, the cause and every outcome clause, with nothing of the buyer's in it (VEN103-1).
func (x *xmrEscrowEnv) wantVendorMoved(order, cause string, says ...string) {
	x.t.Helper()
	moved := x.movedNotices(x.partyOf(order, roleVendor), order)
	if len(moved) != 1 || !containsAll(moved[0], append(says, cause, "so it was moved to "+utcUntil(x.releaseAt(order)))...) {
		x.t.Fatalf("the vendor's recheck notices for %s, want one with %q and %q: %q", shortID(order), cause, says, moved)
	}
	buyer := agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", x.partyOf(order, roleBuyer))
	refund := agStr(x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)
	if strings.Contains(moved[0], buyer) || strings.Contains(moved[0], refund) {
		x.t.Fatalf("the vendor's notice names the buyer: %s", moved[0])
	}
}

// requestRecords lists the audit rows of a host command's recheck request as user:action (PM-3).
func (x *xmrEscrowEnv) requestRecords() []string {
	x.t.Helper()
	return agStrings(x.t, x.testEnv, "SELECT COALESCE(user_id,'-')||':'||action FROM audit_events WHERE action=$1 ORDER BY id", xmrRecheckRequestedAudit)
}

// recheckLogPrefix opens the log line of a deadline recheck.
const recheckLogPrefix = "monero escrow deadline recheck"

// recheckRecords lists the audit rows of the deadline recheck as user:action.
func (x *xmrEscrowEnv) recheckRecords() []string {
	x.t.Helper()
	return agStrings(x.t, x.testEnv, "SELECT COALESCE(user_id,'-')||':'||action FROM audit_events WHERE action LIKE 'Monero escrow deadline recheck%' ORDER BY id")
}

// wantRecheckRecorded fails unless exactly one recheck has logged and audited, with this cause and count and no order data.
func (x *xmrEscrowEnv) wantRecheckRecorded(logs *lockedBuffer, cause string, moved int, orders ...string) {
	x.t.Helper()
	line := recheckLogPrefix + ": cause=" + cause + " moved=" + strconv.Itoa(moved) + "\n"
	if text := logs.String(); strings.Count(text, recheckLogPrefix) != 1 || !strings.Contains(text, line) {
		x.t.Fatalf("the log, want one %q:\n%s", line, text)
	}
	audit := "-:Monero escrow deadline recheck (cause: " + cause + "): moved " + strconv.Itoa(moved) + " order deadline(s)"
	rows := x.recheckRecords()
	if len(rows) != 1 || rows[0] != audit {
		x.t.Fatalf("the audit rows, want one %q: %q", audit, rows)
	}
	for _, o := range orders {
		if strings.Contains(logs.String(), shortID(o)) {
			x.t.Fatalf("the recheck's log names order %s", shortID(o))
		}
	}
}

// outageMovedLine is what the panel says of a deadline an outage moved; restoreMovedLine of one a restore moved.
const (
	outageMovedLine  = "This deadline was moved because " + outageWhy + "."
	restoreMovedLine = "This deadline was moved after the market restored its records from a backup."
)

// A-730 (owner decision): the market being down consumes no buyer's deadline. A gap of over an hour since the last
// deadline step moves every deadline passed or within 72 hours as a restore's recheck does, before any of them could
// complete, and says an outage moved it, on the notice, the note and the order page. A later deadline is untouched. A
// vendor lost the time too, so the recheck's vendor grace applies, and says why.
func TestXMROutageMovesNearDeadlines(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	logs := captureLog(t)
	physical := x.physical(x.vendor)
	passed, passedSess, _ := x.shippedXMR(physical)
	near, _, _ := x.shippedXMR(physical)
	far, _, _ := x.shippedXMR(physical)
	window, windowSess := x.deliveredXMR(x.product)
	paid, paidSess := x.paidXMRPhysical(physical)
	remindedOrder, _, _ := x.shippedXMR(physical)
	x.forceDeadline(remindedOrder, "release_at=now()+interval '10 hours'")
	remindedWas := x.releaseAt(remindedOrder)
	x.mustPoll() // the reminder goes out, and the vendor is told this date
	x.forceDeadline(passed, "release_at=now()-interval '2 days'")
	x.forceDeadline(near, "release_at=now()+interval '24 hours'")
	x.forceDeadline(far, "release_at=now()+interval '5 days'")
	x.forceDeadline(window, "release_at=now()-interval '1 hour'")
	x.forceDeadline(paid, "ship_by=now()-interval '1 minute'")
	farAt := x.releaseAt(far)
	x.setLastDeadlineStep("2 hours")
	if strings.Contains(logs.String(), recheckLogPrefix) || len(x.recheckRecords()) != 0 {
		t.Fatalf("a recheck record before any recheck: %q, %q", logs.String(), x.recheckRecords())
	}

	x.mustPoll()
	// DA104-5: one fixed log line and one audit row with the cause and the count, and no order data.
	x.wantRecheckRecorded(logs, "outage", 4, passed, near, window, remindedOrder)
	if rows := x.requestRecords(); len(rows) != 0 { // PM-3: nobody on the host asked
		t.Fatalf("a recheck the watcher asked for has request rows: %q", rows)
	}
	for _, o := range []string{passed, near, window} {
		if d := x.releaseAt(o).Sub(x.dbNow()); d < 71*time.Hour || d > 72*time.Hour {
			t.Fatalf("the deadline of %s is %s away, want 72 hours", shortID(o), d)
		}
		notices := x.noticesOf(x.partyOf(o, roleBuyer), o)
		if countContaining(notices, "an outage") != 1 || anyContains(notices, "restore") || !anyContains(notices, utcUntil(x.releaseAt(o)), "Confirm receipt") {
			t.Fatalf("the buyer's notices for %s: %q", shortID(o), notices)
		}
		notes := x.movedNotes(o)
		if len(notes) != 1 || !strings.Contains(notes[0], "after an outage") || strings.Contains(notes[0], "restore") {
			t.Fatalf("the notes of %s: %q", shortID(o), notes)
		}
	}
	// VEN103-1: the vendor is told too, once, whether or not a reminder had gone out: a reminded order's vendor holds a date
	// that no longer applies, and an unreminded one's would hear nothing, as the recheck's move suppresses the reminder.
	if !anyContains(x.noticesOf(x.vendor, remindedOrder), "the buyer has until "+utcUntil(remindedWas)) {
		t.Fatalf("the reminder the vendor should have had first: %q", x.noticesOf(x.vendor, remindedOrder))
	}
	for _, o := range []string{passed, near, remindedOrder} {
		x.wantVendorMoved(o, outageWhy, "the order completes at that time and the market releases the coins in this Monero escrow to you")
	}
	x.wantVendorMoved(window, outageWhy, "dispute window", "the order completes at that time and you can claim the coins in this Monero escrow from the order page")
	if n := x.movedNotices(x.vendor, far); len(n) != 0 {
		t.Fatalf("the vendor of an order not moved was told: %q", n)
	}
	if n := x.movedNotices(x.vendor, paid); len(n) != 0 {
		t.Fatalf("the vendor of a paid order was told: %q", n)
	}
	if x.state(passed) != stateShipped || x.state(near) != stateShipped || x.state(window) != stateDelivered {
		t.Fatalf("the moved orders: %s %s %s", x.state(passed), x.state(near), x.state(window))
	}
	if !x.releaseAt(far).Equal(farAt) || len(x.movedNotes(far)) != 0 {
		t.Fatalf("an order further out than 72 hours was touched: %s, was %s", x.releaseAt(far), farAt)
	}
	if x.recheckKey() != "" {
		t.Fatalf("the recheck left its request: %q", x.recheckKey())
	}
	// The order page does not say a restore moved it.
	if p := x.panel(passed, passedSess); strings.Contains(p, "restored") || !containsAll(p, utcUntil(x.releaseAt(passed)), outageMovedLine) {
		t.Fatalf("the panel after an outage's recheck:\n%s", p)
	}
	if p := x.panel(window, windowSess); strings.Contains(p, "restored") || !strings.Contains(p, outageMovedLine) {
		t.Fatalf("the digital panel after an outage's recheck:\n%s", p)
	}
	// VEN103-1: the vendor's panel names the cause as well; an order not moved says nothing of one.
	for _, o := range []string{passed, window} {
		if p := x.panel(o, x.vendorSess); strings.Contains(p, "restored") || !strings.Contains(p, outageMovedLine) {
			t.Fatalf("the vendor's panel after an outage's recheck:\n%s", p)
		}
	}
	if p := x.panel(far, x.vendorSess); strings.Contains(p, outageMovedLine) || strings.Contains(p, restoreMovedLine) {
		t.Fatalf("the panel of an order the recheck did not move:\n%s", p)
	}

	// The vendor's grace: the same transaction moved it, and every line about it names the outage.
	until := x.cancelWaitUntil()
	if d := until.Sub(x.dbNow()); d < 71*time.Hour || d > 72*time.Hour {
		t.Fatalf("the vendor's grace ends %s away", d)
	}
	code, body := x.xmrBuyerCancel(paid, paidSess)
	if code != 409 || !containsAll(body, "You can cancel this order from "+utcFrom(until), "an outage", "Nothing was changed") || strings.Contains(body, "restore") {
		t.Fatalf("a buyer's cancel in an outage's grace: %d %s", code, body)
	}
	for name, page := range map[string]string{
		"buyer's order page":  x.page("/order?id="+paid, paidSess),
		"vendor's order page": x.page("/order?id="+paid, x.vendorSess),
		"vendor's desk":       x.page("/vendor-dashboard", x.vendorSess),
	} {
		page = html.UnescapeString(page)
		if !containsAll(page, utcFrom(until), "an outage") || strings.Contains(page, "this market's restore") || strings.Contains(page, "restored its records") {
			t.Fatalf("the %s in an outage's grace:\n%s", name, page)
		}
	}

	// Once: the next pass finds no gap, and no second notice, reminder, log line or audit row follows.
	x.mustPoll()
	x.wantRecheckRecorded(logs, "outage", 4, passed, near, window, remindedOrder)
	notices := x.noticesOf(x.partyOf(passed, roleBuyer), passed)
	if countContaining(notices, "an outage") != 1 || anyContains(notices, "Reminder") {
		t.Fatalf("after another pass: %q", notices)
	}
	for _, o := range []string{passed, near, window, remindedOrder} {
		if n := x.movedNotices(x.vendor, o); len(n) != 1 {
			t.Fatalf("the vendor's notices for %s after another pass: %q", shortID(o), n)
		}
	}
}

// VEN103-1: the line that names an outage follows the deadline it moved, from the system's own note. A party's note with
// the same words proves nothing, a later ban that moves the deadline again ends the claim, and a later recheck's cause
// replaces it, in either order.
func TestXMROutageCauseFollowsDeadline(t *testing.T) {
	setup := func(t *testing.T) (*xmrEscrowEnv, string, string) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		o, sess, _ := x.shippedXMR(x.physical(x.vendor))
		x.forceDeadline(o, "release_at=now()-interval '1 hour'")
		return x, o, sess
	}
	outage := func(x *xmrEscrowEnv) {
		x.t.Helper()
		x.setLastDeadlineStep("2 hours")
		x.mustPoll()
	}
	restore := func(x *xmrEscrowEnv) {
		x.t.Helper()
		agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrReleaseRecheckKey)
		x.mustPoll()
	}
	t.Run("a party's note", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		o, sess, _ := x.shippedXMR(x.physical(x.vendor))
		forged := "Release deadline moved to " + utcUntil(x.releaseAt(o)) + " after an outage (Monero escrow): this market was unavailable."
		agExec(x.testEnv, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,'shipped','shipped',$2,$3)", o, x.vendor, forged)
		for _, s := range []string{sess, x.vendorSess} {
			if p := x.panel(o, s); strings.Contains(p, outageMovedLine) {
				t.Fatalf("a party's note made the panel claim an outage:\n%s", p)
			}
		}
	})
	t.Run("a ban moves it later", func(t *testing.T) {
		x, o, sess := setup(t)
		outage(x)
		if p := x.panel(o, sess); !strings.Contains(p, outageMovedLine) {
			t.Fatalf("after the outage:\n%s", p)
		}
		// As if the outage's recheck were an hour old (a date is to the minute): the ban then moves the deadline to a new one.
		was := utcUntil(x.releaseAt(o))
		x.forceDeadline(o, "release_at=now()+interval '71 hours'")
		agExec(x.testEnv, "UPDATE order_events SET note=replace(note,$2,$3) WHERE order_id=$1 AND actor_id IS NULL", o, was, utcUntil(x.releaseAt(o)))
		if p := x.panel(o, sess); !strings.Contains(p, outageMovedLine) {
			t.Fatalf("an hour after the outage:\n%s", p)
		}
		_, admin := x.user("causeban_admin", "admin")
		x.banVendor(admin)
		if d := x.releaseAt(o).Sub(x.dbNow()); d < 71*time.Hour+30*time.Minute {
			t.Fatalf("the ban left the deadline %s away", d)
		}
		if p := x.panel(o, sess); strings.Contains(p, outageMovedLine) || strings.Contains(p, restoreMovedLine) {
			t.Fatalf("after a ban moved it:\n%s", p)
		}
	})
	t.Run("a restore after an outage", func(t *testing.T) {
		x, o, sess := setup(t)
		outage(x)
		x.forceDeadline(o, "release_at=now()+interval '1 hour'")
		restore(x)
		if p := x.panel(o, sess); strings.Contains(p, outageMovedLine) || !strings.Contains(p, restoreMovedLine) {
			t.Fatalf("after a restore's recheck moved it:\n%s", p)
		}
		x.wantLatestMoved(o, "restored its records from a backup")
	})
	t.Run("an outage after a restore", func(t *testing.T) {
		x, o, sess := setup(t)
		restore(x)
		if p := x.panel(o, x.vendorSess); !strings.Contains(p, restoreMovedLine) || strings.Contains(p, outageMovedLine) {
			t.Fatalf("after the restore, the vendor's panel:\n%s", p)
		}
		x.forceDeadline(o, "release_at=now()+interval '1 hour'")
		outage(x)
		for _, s := range []string{sess, x.vendorSess} {
			if p := x.panel(o, s); strings.Contains(p, restoreMovedLine) || !strings.Contains(p, outageMovedLine) {
				t.Fatalf("after an outage moved it:\n%s", p)
			}
		}
		x.wantLatestMoved(o, outageWhy)
	})
}

// wantLatestMoved fails unless the vendor has one notice from each of two rechecks of order, one naming cause, and one
// naming the date now on the order.
func (x *xmrEscrowEnv) wantLatestMoved(order, cause string) {
	x.t.Helper()
	moved := x.movedNotices(x.vendor, order)
	if len(moved) != 2 || countContaining(moved, cause) != 1 || countContaining(moved, utcUntil(x.releaseAt(order))) == 0 {
		x.t.Fatalf("the vendor's notices after two rechecks: %q", moved)
	}
}

// A-730: a pass whose daemon check fails does nothing else, and still reaches the deadline step: the gap it finds takes
// the same path. Monero escrow actually switched off is TestXMROutageEscrowOffPaths.
func TestXMROutageWhileDaemonNotReady(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.shippedXMR(x.physical(x.vendor))
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	x.setLastDeadlineStep("3 days")
	x.A.escrowXMR.ready.Store(false)
	x.mustPoll()
	x.A.escrowXMR.ready.Store(true)
	if x.state(order) != stateShipped || x.releaseAt(order).Before(x.dbNow().Add(71*time.Hour)) || x.recheckKey() != "" ||
		countContaining(x.noticesOf(x.partyOf(order, roleBuyer), order), "an outage") != 1 {
		t.Fatalf("order %s, deadline %s, request %q", x.state(order), x.releaseAt(order), x.recheckKey())
	}
}

// DA104-3: with ESCROW_XMR off no watcher runs. A dispute on a passed deadline is refused as late, a no-ship cancel past
// its time is accepted while no recheck is pending and no vendor's grace has time left, and is refused otherwise, and
// marking an order shipped answers 404. On again, an off period over the threshold rechecks and a shorter one does not.
func TestXMROutageEscrowOffPaths(t *testing.T) {
	t.Run("while off", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		physical := x.physical(x.vendor)
		late, lateSess, _ := x.shippedXMR(physical)
		lateAfter, lateAfterSess, _ := x.shippedXMR(physical)
		cancelled, cancelledSess := x.paidXMRPhysical(physical)
		pending, pendingSess := x.paidXMRPhysical(physical)
		more := x.physical(x.vendor) // a listing's stock is limited
		grace, graceSess := x.paidXMRPhysical(more)
		unshipped, _ := x.paidXMRPhysical(more)
		for _, o := range []string{late, lateAfter} {
			x.forceDeadline(o, "release_at=now()-interval '1 minute'")
		}
		for _, o := range []string{cancelled, pending, grace} {
			x.forceDeadline(o, "ship_by=now()-interval '1 minute'")
		}
		on := x.A.escrowXMR
		x.A.escrowXMR = nil

		// BE110-3: the banner tells the buyer the deadline still closes disputes, and the refusal says the same thing.
		if p := html.UnescapeString(x.page("/order?id="+late, lateSess)); !strings.Contains(p, "The deadline still closes disputes: after it, a dispute can no longer be opened") || strings.Contains(p, "applied only once it is on again") {
			t.Fatalf("the banner with escrow off:\n%s", p)
		}
		if code, body := x.xmrDispute(late, lateSess); code != 409 || !strings.Contains(html.UnescapeString(body), "The release deadline of this order passed at "+utcUntil(x.releaseAt(late))+", so the release to the vendor is due and a dispute can no longer be opened. Nothing was changed.") {
			t.Fatalf("a late dispute with escrow off: %d %s", code, body)
		}
		if code, body := x.xmrBuyerCancel(cancelled, cancelledSess); code != 303 || x.state(cancelled) != stateCancelled {
			t.Fatalf("a cancel with escrow off and nothing pending: %d %s, order %s", code, body, x.state(cancelled))
		}
		if code, body := x.xmrShip(unshipped, x.vendorSess, testXMRAddr("off ship", true), testPassword); code != 404 || x.state(unshipped) != statePaid {
			t.Fatalf("marking shipped with escrow off: %d %s, order %s", code, body, x.state(unshipped))
		}
		// The request stays pending with no watcher to consume it: the cancel waits, and a dispute is not refused as late.
		agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,$2)", xmrReleaseRecheckKey, xmrRecheckOutage)
		if code, body := x.xmrBuyerCancel(pending, pendingSess); code != 409 || x.state(pending) != statePaid || !strings.Contains(body, "an outage") {
			t.Fatalf("a cancel with a recheck pending: %d %s, order %s", code, body, x.state(pending))
		}
		if code, body := x.xmrDispute(lateAfter, lateAfterSess); code != 303 {
			t.Fatalf("a dispute with a recheck pending: %d %s", code, body)
		}
		// Back on, the recheck runs and records the vendor's grace; off again, the cancel waits for its end.
		x.A.escrowXMR = on
		x.mustPoll()
		if x.recheckKey() != "" {
			t.Fatalf("the request stayed: %q", x.recheckKey())
		}
		x.A.escrowXMR = nil
		if code, body := x.xmrBuyerCancel(grace, graceSess); code != 409 || x.state(grace) != statePaid || !strings.Contains(body, utcFrom(x.cancelWaitUntil())) {
			t.Fatalf("a cancel in the vendor's grace: %d %s, order %s", code, body, x.state(grace))
		}
		x.A.escrowXMR = on
	})
	for _, c := range []struct {
		name, off string
		recheck   bool
	}{{"off for longer than the threshold", "2 hours", true}, {"off for less than the threshold", "30 minutes", false}} {
		t.Run(c.name, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			order, _, _ := x.shippedXMR(x.physical(x.vendor))
			on := x.A.escrowXMR
			x.A.escrowXMR = nil
			x.forceDeadline(order, "release_at=now()-interval '1 minute'")
			x.setLastDeadlineStep(c.off)
			x.A.escrowXMR = on
			x.mustPoll() // the first pass with it on again
			told := anyContains(x.noticesOf(x.partyOf(order, roleBuyer), order), "an outage")
			if c.recheck && (x.state(order) != stateShipped || !told || x.releaseAt(order).Before(x.dbNow().Add(71*time.Hour))) {
				t.Fatalf("over the threshold: order %s, told %v, deadline %s", x.state(order), told, x.releaseAt(order))
			}
			if !c.recheck && (x.state(order) != stateCompleted || told) {
				t.Fatalf("under the threshold: order %s, told %v", x.state(order), told)
			}
		})
	}
}

// A-730: with no stored time (the first pass after the upgrade, or a fresh database) or a gap under the hour, the step
// records the time and asks for nothing: a due order completes as before, a near deadline stays and no notice is sent.
func TestXMROutageNoRecheckWithoutGap(t *testing.T) {
	for _, c := range []struct{ name, age string }{{"no stored time", ""}, {"under the threshold", "59 minutes"}, {"an unreadable time", "unreadable"}} {
		t.Run(c.name, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			physical := x.physical(x.vendor)
			due, _, _ := x.shippedXMR(physical)
			near, _, _ := x.shippedXMR(physical)
			x.forceDeadline(due, "release_at=now()-interval '1 minute'")
			x.forceDeadline(near, "release_at=now()+interval '24 hours'")
			nearAt := x.releaseAt(near)
			switch c.age {
			case "":
				agExec(x.testEnv, "DELETE FROM settings WHERE key=$1", xmrLastDeadlineStepKey)
			case "unreadable":
				agExec(x.testEnv, "UPDATE settings SET value='not a time' WHERE key=$1", xmrLastDeadlineStepKey)
			default:
				x.setLastDeadlineStep(c.age)
			}
			before := x.dbNow()
			if err := x.A.xmrDeadlines(context.Background()); err != nil {
				t.Fatal(err)
			}
			if x.recheckKey() != "" || x.state(due) != stateCompleted || !x.releaseAt(near).Equal(nearAt) {
				t.Fatalf("request %q, due order %s, near deadline %s (was %s)", x.recheckKey(), x.state(due), x.releaseAt(near), nearAt)
			}
			if anyContains(x.noticesOf(x.partyOf(near, roleBuyer), near), "an outage") || anyContains(x.noticesOf(x.partyOf(due, roleBuyer), due), "an outage") {
				t.Fatal("a notice of an outage without one")
			}
			var stored time.Time
			if err := x.DB.QueryRow("SELECT value::timestamptz FROM settings WHERE key=$1", xmrLastDeadlineStepKey).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored.Before(before) {
				t.Fatalf("the step recorded %s, before it ran at %s", stored, before)
			}
		})
	}
}

// A-730: the request and the new time commit together. A recheck that fails keeps the request and tells no buyer; the
// next pass, though it finds no gap, retries it, and each buyer is told once. A step that cannot write the time, as with
// a database failing during the outage, changes nothing and completes nothing, so the gap is still there on return.
//
// A-762: an order due within 72 hours and not yet reminded gets no reminder while the recheck is pending and failing, as
// it would name the date about to be replaced: each party gets exactly one notice in the end, the moved one.
func TestXMROutageRecheckRetries(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	order, sess, _ := x.shippedXMR(physical)
	soon, _, _ := x.shippedXMR(physical)
	window, windowSess := x.deliveredXMR(x.product)
	x.forceDeadline(order, "release_at=now()-interval '2 days'")
	x.forceDeadline(window, "release_at=now()-interval '2 days'")
	x.forceDeadline(soon, "release_at=now()+interval '10 hours',reminded_at=NULL")
	passedAt := x.releaseAt(order)
	soonAt := x.releaseAt(soon)
	soonBuyer, soonVendor := x.partyOf(soon, roleBuyer), x.partyOf(soon, roleVendor)
	buyerHad, vendorHad := len(x.noticesOf(soonBuyer, soon)), len(x.noticesOf(soonVendor, soon))
	x.setLastDeadlineStep("2 hours")
	stale := x.lastDeadlineStep()

	agExec(x.testEnv, `CREATE FUNCTION a730_fail_step() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected write failure'; END $$`)
	agExec(x.testEnv, "CREATE TRIGGER a730_fail_step BEFORE INSERT OR UPDATE ON settings FOR EACH ROW WHEN (NEW.key='"+xmrLastDeadlineStepKey+"') EXECUTE FUNCTION a730_fail_step()")
	if err := x.A.xmrDeadlines(context.Background()); err == nil {
		t.Fatal("a step that could not record its time reported no error")
	}
	if x.recheckKey() != "" || x.state(order) != stateShipped || x.lastDeadlineStep() != stale {
		t.Fatalf("the failed step: request %q, order %s, time %q", x.recheckKey(), x.state(order), x.lastDeadlineStep())
	}
	agExec(x.testEnv, "DROP TRIGGER a730_fail_step ON settings")

	agExec(x.testEnv, `CREATE FUNCTION a730_fail_recheck() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.note LIKE '%after an outage%' THEN RAISE EXCEPTION 'injected recheck failure'; END IF; RETURN NEW; END $$`)
	agExec(x.testEnv, "CREATE TRIGGER a730_fail_recheck BEFORE INSERT ON order_events FOR EACH ROW EXECUTE FUNCTION a730_fail_recheck()")
	for pass := range 2 {
		if err := x.A.xmrDeadlines(context.Background()); err == nil {
			t.Fatalf("pass %d: the failed recheck reported no error", pass)
		}
		if x.recheckKey() != xmrRecheckOutage || x.state(order) != stateShipped || !x.releaseAt(order).Equal(passedAt) || anyContains(x.noticesOf(x.partyOf(order, roleBuyer), order), "an outage") {
			t.Fatalf("pass %d: request %q, order %s, deadline %s, notices %q", pass, x.recheckKey(), x.state(order), x.releaseAt(order), x.noticesOf(x.partyOf(order, roleBuyer), order))
		}
		if x.lastDeadlineStep() == stale {
			t.Fatalf("pass %d: the time was not recorded", pass)
		}
		if b, v := x.noticesOf(soonBuyer, soon), x.noticesOf(soonVendor, soon); len(b) != buyerHad || len(v) != vendorHad || !x.releaseAt(soon).Equal(soonAt) {
			t.Fatalf("pass %d: a notice while the recheck is pending and failing: buyer %q, vendor %q, deadline %s", pass, b, v, x.releaseAt(soon))
		}
	}
	// While it is pending the page names the outage too.
	if p := x.panel(order, sess); strings.Contains(p, "restore") || !strings.Contains(p, "an outage") {
		t.Fatalf("the panel while an outage's recheck is pending:\n%s", p)
	}
	if p := x.panel(window, windowSess); strings.Contains(p, "restore") || !strings.Contains(p, "an outage") {
		t.Fatalf("the digital panel while an outage's recheck is pending:\n%s", p)
	}
	if page := html.UnescapeString(x.page("/order?id="+window, windowSess)); !strings.Contains(page, "The dispute window ended at") || strings.Contains(page, "restored records") {
		t.Fatalf("the digital order page while an outage's recheck is pending:\n%s", page)
	}
	agExec(x.testEnv, "DROP TRIGGER a730_fail_recheck ON order_events")
	for range 2 {
		if err := x.A.xmrDeadlines(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	notices := x.noticesOf(x.partyOf(order, roleBuyer), order)
	if x.recheckKey() != "" || countContaining(notices, "an outage") != 1 || x.releaseAt(order).Before(x.dbNow().Add(71*time.Hour)) || x.state(order) != stateShipped {
		t.Fatalf("after the retry: request %q, deadline %s, order %s, notices %q", x.recheckKey(), x.releaseAt(order), x.state(order), notices)
	}
	for party, had := range map[string]int{soonBuyer: buyerHad, soonVendor: vendorHad} {
		if n, moved := x.noticesOf(party, soon), x.movedNotices(party, soon); len(n) != had+1 || len(moved) != 1 || !strings.Contains(moved[0], utcUntil(x.releaseAt(soon))) || anyContains(n, "Reminder") {
			t.Fatalf("the notices after the retry, want one more, the moved one: %q", n)
		}
	}
	if x.releaseAt(soon).Before(x.dbNow().Add(71 * time.Hour)) {
		t.Fatalf("the deadline due soon: %s", x.releaseAt(soon))
	}
}

// A-763: between a gap and the next check nothing has asked for the recheck yet, so a stale stored step holds the cancel
// and opens the dispute by itself, with no pass: a refund cannot relay on a clock that has not been looked at. A fresh,
// missing or unreadable step, Monero escrow off, and a poll interval that allows the gap all behave as before, and a
// restore's request still says a restore.
func TestXMROutageStaleStepHoldsCancelBeforeFirstPass(t *testing.T) {
	setup := func(t *testing.T) (x *xmrEscrowEnv, paid, paidSess, shipped, shippedSess string) {
		x = newXMREscrowEnv(t)
		x.mustPoll()
		physical := x.physical(x.vendor)
		paid, paidSess = x.paidXMRPhysical(physical)
		shipped, shippedSess, _ = x.shippedXMR(physical)
		x.forceDeadline(paid, "ship_by=now()-interval '1 hour'")
		x.forceDeadline(shipped, "release_at=now()-interval '1 hour'")
		return x, paid, paidSess, shipped, shippedSess
	}
	// wantAsToday fails unless the past-time cancel is accepted, requesting the refund, and the late dispute refused.
	wantAsToday := func(t *testing.T, x *xmrEscrowEnv, paid, paidSess, shipped, shippedSess string) {
		t.Helper()
		if code, body := x.xmrDispute(shipped, shippedSess); code != 409 || !strings.Contains(body, "a dispute can no longer be opened") || x.state(shipped) != stateShipped {
			t.Fatalf("a late dispute: %d %s, order %s", code, body, x.state(shipped))
		}
		if code, body := x.xmrBuyerCancel(paid, paidSess); code != 303 || x.state(paid) != stateCancelled {
			t.Fatalf("a cancel past its time: %d %s, order %s", code, body, x.state(paid))
		}
		if x.A.escrowXMR != nil && len(x.xmrSettlements(paid)) != 1 {
			t.Fatalf("the cancel's refund: %q", x.xmrSettlements(paid))
		}
		if x.recheckKey() != "" {
			t.Fatalf("a request: %q", x.recheckKey())
		}
	}

	t.Run("stale", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		physical := x.physical(x.vendor)
		paid, paidSess := x.paidXMRPhysical(physical)
		notYet, notYetSess := x.paidXMRPhysical(physical)
		preGap, preGapSess := x.paidXMRPhysical(physical)
		shipped, shippedSess, _ := x.shippedXMR(physical)
		window, windowSess := x.deliveredXMR(x.product)
		x.forceDeadline(paid, "ship_by=now()-interval '1 hour'")
		x.forceDeadline(preGap, "ship_by=now()-interval '4 hours'")
		x.forceDeadline(shipped, "release_at=now()-interval '1 hour'")
		x.forceDeadline(window, "release_at=now()-interval '1 hour'")
		x.setLastDeadlineStep("2 hours")
		step := x.lastDeadlineStep()

		// PAY113-2: the page says what the action does on each side of the stored step, and the desk with it.
		if p := html.UnescapeString(x.page("/order?id="+paid, paidSess)); !containsAll(p, "You cannot cancel yet: "+outageWhy, "The buyer's cancel waits: "+outageWhy) || strings.Contains(p, `action="/orders/cancel"`) {
			t.Fatalf("the page of an order due in the gap, on a stale step:\n%s", p)
		}
		x.wantDeskLine(paid, "the buyer's cancel waits for this market's recheck after an outage")
		x.wantDeskLine(preGap, "passed, so the buyer may cancel")
		x.wantCancelAccepted(preGap, preGapSess)
		code, body := x.xmrBuyerCancel(paid, paidSess)
		if code != 409 || !containsAll(body, "cannot be cancelled yet", "an outage", "Nothing was changed") || strings.Contains(body, "restore") {
			t.Fatalf("a cancel past its time on a stale step: %d %s", code, body)
		}
		if x.state(paid) != statePaid || len(x.xmrSettlements(paid)) != 0 {
			t.Fatalf("after the refused cancel: order %s, settlements %q", x.state(paid), x.xmrSettlements(paid))
		}
		if code, body = x.xmrBuyerCancel(notYet, notYetSess); code != 409 || !strings.Contains(body, "if the vendor has not marked it shipped by then") {
			t.Fatalf("a cancel before its time on a stale step: %d %s", code, body)
		}
		if code, body = x.xmrDispute(shipped, shippedSess); code != 303 || x.state(shipped) != stateDisputed {
			t.Fatalf("a dispute on a stale step: %d %s, order %s", code, body, x.state(shipped))
		}
		if code, body = x.xmrDispute(window, windowSess); code != 303 || x.state(window) != stateDisputed {
			t.Fatalf("a dispute in a digital window on a stale step: %d %s, order %s", code, body, x.state(window))
		}
		// No pass ran: neither action asked for the recheck or recorded a time.
		if x.recheckKey() != "" || x.lastDeadlineStep() != step {
			t.Fatalf("an action wrote: request %q, step %q (was %q)", x.recheckKey(), x.lastDeadlineStep(), step)
		}
		// The next pass takes over, and the cancel still waits, now for the vendor's grace.
		x.mustPoll()
		if code, body = x.xmrBuyerCancel(paid, paidSess); code != 409 || !containsAll(body, "You can cancel this order from "+utcFrom(x.cancelWaitUntil()), "an outage") || x.state(paid) != statePaid {
			t.Fatalf("a cancel after the pass: %d %s, order %s", code, body, x.state(paid))
		}
	})
	t.Run("fresh", func(t *testing.T) {
		x, paid, paidSess, shipped, shippedSess := setup(t)
		x.setLastDeadlineStep("59 minutes")
		wantAsToday(t, x, paid, paidSess, shipped, shippedSess)
	})
	t.Run("missing", func(t *testing.T) {
		x, paid, paidSess, shipped, shippedSess := setup(t)
		agExec(x.testEnv, "DELETE FROM settings WHERE key=$1", xmrLastDeadlineStepKey)
		wantAsToday(t, x, paid, paidSess, shipped, shippedSess)
	})
	for name, value := range map[string]string{ // as xmrNoteDeadlineStep treats each: no hold, no error
		"unreadable":                "not a time",
		"unreadable impossible":     "2026-13-45T00:00:00Z", // the shape of a stored time, with no such date
		"unreadable not its format": "2020-01-01 00:00:00+00",
	} {
		t.Run(name, func(t *testing.T) {
			x, paid, paidSess, shipped, shippedSess := setup(t)
			agExec(x.testEnv, "UPDATE settings SET value=$2 WHERE key=$1", xmrLastDeadlineStepKey, value)
			wantAsToday(t, x, paid, paidSess, shipped, shippedSess)
		})
	}
	t.Run("escrow off", func(t *testing.T) { // DA104-3 stays true: no watcher runs, so no pass could end a hold
		x, paid, paidSess, shipped, shippedSess := setup(t)
		x.setLastDeadlineStep("2 hours")
		on := x.A.escrowXMR
		x.A.escrowXMR = nil
		defer func() { x.A.escrowXMR = on }()
		wantAsToday(t, x, paid, paidSess, shipped, shippedSess)
	})
	t.Run("the gap follows the poll interval", func(t *testing.T) { // the threshold is xmrOutageGap's, not a fixed hour
		t.Setenv("PAYMENT_POLL_INTERVAL", "1h")
		x, paid, paidSess, shipped, shippedSess := setup(t)
		x.setLastDeadlineStep("2 hours")
		wantAsToday(t, x, paid, paidSess, shipped, shippedSess)
	})
	t.Run("a restore's request", func(t *testing.T) { // a restored database carries an old time
		x, paid, paidSess, shipped, shippedSess := setup(t)
		agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrReleaseRecheckKey)
		x.setLastDeadlineStep("30 days")
		code, body := x.xmrBuyerCancel(paid, paidSess)
		if code != 409 || !strings.Contains(body, "after a restore from backup") || strings.Contains(body, "an outage") {
			t.Fatalf("a cancel with a restore's request: %d %s", code, body)
		}
		if code, body = x.xmrDispute(shipped, shippedSess); code != 303 {
			t.Fatalf("a dispute with a restore's request: %d %s", code, body)
		}
		// PAY113-2: a restore's request holds them all, also an order overdue before the stored step, also with a gap row its
		// overwrite left behind: restored records have no gap to go by.
		x.forceDeadline(paid, "ship_by=now()-interval '40 days'")
		x.wantCancelHeld(paid, paidSess, "after a restore from backup")
		agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,$2)", xmrOutageGapFromKey, x.lastDeadlineStep())
		x.wantCancelHeld(paid, paidSess, "after a restore from backup")
		x.wantNoHeldFromClause() // A-785: the panel and the host's reply say every one waits
	})
	// PAY113-2: a host's request has no start to go by, stale step or not: nothing recorded when ingress failed.
	t.Run("a host's request", func(t *testing.T) {
		x, paid, paidSess, _, _ := setup(t)
		x.forceDeadline(paid, "ship_by=now()-interval '3 hours'")
		if _, err := RequestDeadlineRecheck(context.Background(), x.DB); err != nil {
			t.Fatal(err)
		}
		x.setLastDeadlineStep("2 hours")
		x.wantCancelHeld(paid, paidSess, "cannot be cancelled yet", outageWhy)
		x.wantDeskLine(paid, "the buyer's cancel waits for this market's recheck after an outage")
		x.wantNoHeldFromClause() // A-785
	})
	// PAY113-2: once the check has stored the gap's start the request is the watcher's, and the same two orders read the same.
	t.Run("the request the check wrote", func(t *testing.T) {
		x, paid, paidSess, _, _ := setup(t)
		preGap, preGapSess := x.paidXMRPhysical(x.physical(x.vendor))
		x.forceDeadline(preGap, "ship_by=now()-interval '4 hours'")
		x.setLastDeadlineStep("2 hours")
		x.A.xmrNoteDeadlineStepEarly(context.Background())
		if x.recheckKey() != xmrRecheckOutage || len(x.settingsOf(xmrOutageGapFromKey)) != 1 {
			t.Fatalf("the watcher's request: %q, gap %q", x.recheckKey(), x.settingsOf(xmrOutageGapFromKey))
		}
		x.wantCancelHeld(paid, paidSess, "cannot be cancelled yet", outageWhy)
		x.wantDeskLine(preGap, "passed, so the buyer may cancel")
		x.wantCancelAccepted(preGap, preGapSess)
		// A-785: Admin → Payments and the host's reply name the earliest ship_by held, between the two orders above.
		gap := x.gapFromStored()
		if held := xmrGraceHeldFrom(gap); !x.shipBy(preGap).Before(held) || x.shipBy(paid).Before(held) {
			t.Fatalf("the orders are not on each side of %s: %s, %s", held, x.shipBy(preGap), x.shipBy(paid))
		}
		x.wantHeldFromClause(gap)
	})
	t.Run("only a Monero escrow", func(t *testing.T) { // noShipCancel and lateDispute also serve what remains of Bitcoin escrow
		x, _, _, _, _ := setup(t)
		x.setLastDeadlineStep("2 hours")
		for coin, want := range map[string]bool{"XMR": true, "BTC": false} {
			var step sql.NullString
			var now time.Time
			if err := x.DB.QueryRow(`SELECT `+xmrLastStepSQL+`,now() FROM (SELECT $1::text AS coin) es`, coin).Scan(&step, &now); err != nil {
				t.Fatal(err)
			}
			if stale := x.A.xmrStaleStep(step, now); stale != want {
				t.Fatalf("the stale step for %s: %v", coin, stale)
			}
		}
	})
}

// PAY113-2: xmrCancelRecheck's rule, with no database. The hold while a round is to come is the one that round will leave.
func TestXMRCancelRecheckReads(t *testing.T) {
	t.Setenv("PAYMENT_POLL_INTERVAL", "")
	a := &App{escrowXMR: &escrowXMR{}}
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) sql.NullTime { return sql.NullTime{Time: now.Add(-ago), Valid: true} }
	text := func(ago time.Duration) sql.NullString {
		return sql.NullString{String: now.Add(-ago).Format(time.RFC3339Nano), Valid: true}
	}
	none := sql.NullTime{}
	margin := xmrOutageGap() // PAY113R-2
	for _, c := range []struct {
		name               string
		gap, step          sql.NullString
		end, start, shipBy sql.NullTime
		requested, outage  bool
		holds, byOutage    bool
	}{
		{name: "nothing asked, fresh step", step: text(time.Minute), shipBy: at(time.Hour)},
		{name: "a restore holds them all", gap: text(2 * time.Hour), shipBy: at(50 * time.Hour), requested: true, holds: true},
		{name: "a restore on a stale step", step: text(50 * time.Hour), shipBy: at(60 * time.Hour), requested: true, holds: true},
		{name: "a host's request holds them all", shipBy: at(50 * time.Hour), requested: true, outage: true, holds: true, byOutage: true},
		{name: "a host's request on a stale step", step: text(2 * time.Hour), shipBy: at(50 * time.Hour), requested: true, outage: true, holds: true, byOutage: true},
		{name: "before the gap and its margin", gap: text(2 * time.Hour), shipBy: at(2*time.Hour + margin + time.Second), requested: true, outage: true, byOutage: true},
		{name: "at the margin", gap: text(2 * time.Hour), shipBy: at(2*time.Hour + margin), requested: true, outage: true, holds: true, byOutage: true},
		{name: "inside the margin", gap: text(2 * time.Hour), shipBy: at(2*time.Hour + margin - time.Second), requested: true, outage: true, holds: true, byOutage: true},
		{name: "at the gap's start", gap: text(2 * time.Hour), shipBy: at(2 * time.Hour), requested: true, outage: true, holds: true, byOutage: true},
		{name: "after the gap's start", gap: text(2 * time.Hour), shipBy: at(time.Hour), requested: true, outage: true, holds: true, byOutage: true},
		{name: "an unreadable gap is no start", gap: sql.NullString{String: "not a time", Valid: true}, shipBy: at(3 * time.Hour), requested: true, outage: true, holds: true, byOutage: true},
		{name: "stale, before the step and its margin", step: text(2 * time.Hour), shipBy: at(2*time.Hour + margin + time.Second), byOutage: true},
		{name: "stale, inside the margin", step: text(2 * time.Hour), shipBy: at(2*time.Hour + margin - time.Second), holds: true, byOutage: true},
		{name: "stale, after the step", step: text(2 * time.Hour), shipBy: at(time.Hour), holds: true, byOutage: true},
		{name: "a grace that ended before the gap narrows", gap: text(2 * time.Hour), end: at(3 * time.Hour), start: at(9 * time.Hour), shipBy: at(5 * time.Hour), requested: true, outage: true, byOutage: true},
		{name: "a kept start's margin", gap: text(2 * time.Hour), end: at(time.Hour), start: at(9 * time.Hour), shipBy: at(9*time.Hour + margin - time.Second), requested: true, outage: true, holds: true, byOutage: true},
		{name: "a grace that ended as the gap began narrows", gap: text(2 * time.Hour), end: at(2 * time.Hour), start: at(9 * time.Hour), shipBy: at(5 * time.Hour), requested: true, outage: true, byOutage: true},
		{name: "a grace that ended in the gap keeps its start", gap: text(2 * time.Hour), end: at(time.Hour), start: at(9 * time.Hour), shipBy: at(5 * time.Hour), requested: true, outage: true, holds: true, byOutage: true},
		{name: "before that kept start and its margin", gap: text(2 * time.Hour), end: at(time.Hour), start: at(9 * time.Hour), shipBy: at(9*time.Hour + margin + time.Second), requested: true, outage: true, byOutage: true},
		{name: "a grace with no start keeps none", gap: text(2 * time.Hour), end: at(time.Hour), start: none, shipBy: at(50 * time.Hour), requested: true, outage: true, holds: true, byOutage: true},
		{name: "a running grace, stale", step: text(2 * time.Hour), end: at(-time.Hour), start: none, shipBy: at(50 * time.Hour), holds: true, byOutage: true},
		{name: "no ship-by", gap: text(2 * time.Hour), requested: true, outage: true, holds: true, byOutage: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := xmrCancelHold{gapFrom: c.gap, step: c.step, graceEnd: c.end, graceFrom: c.start, now: now}
			if holds, byOutage := a.xmrCancelRecheck(h, c.shipBy, c.requested, c.outage); holds != c.holds || byOutage != c.byOutage {
				t.Fatalf("holds %v, by outage %v; want %v, %v", holds, byOutage, c.holds, c.byOutage)
			}
		})
	}
}

// A-769: xmrStaleStep's comparison, with no database: what the SQL's interval comparison and null handling did.
func TestXMRStaleStepReads(t *testing.T) {
	gap := xmrOutageGap()
	now := time.Date(2026, 6, 15, 12, 0, 0, 500_000_000, time.UTC)
	stored := func(at time.Time) sql.NullString {
		return sql.NullString{String: at.UTC().Format("2006-01-02T15:04:05.000000Z"), Valid: true}
	}
	on, off := &App{escrowXMR: &escrowXMR{}}, &App{}
	for name, c := range map[string]struct {
		a    *App
		step sql.NullString
		want bool
	}{
		"past the gap":        {on, stored(now.Add(-gap - time.Microsecond)), true},
		"exactly the gap":     {on, stored(now.Add(-gap)), false},
		"inside the gap":      {on, stored(now.Add(-gap + time.Microsecond)), false},
		"from the future":     {on, stored(now.Add(time.Hour)), false},
		"another offset":      {on, sql.NullString{String: now.Add(-2 * gap).In(time.FixedZone("", 5*3600)).Format(time.RFC3339Nano), Valid: true}, true},
		"far past":            {on, sql.NullString{String: "0001-01-01T00:00:00Z", Valid: true}, true},
		"none stored":         {on, sql.NullString{}, false},
		"empty":               {on, sql.NullString{Valid: true}, false},
		"not a time":          {on, sql.NullString{String: "not a time", Valid: true}, false},
		"an impossible date":  {on, sql.NullString{String: "2026-13-45T00:00:00Z", Valid: true}, false},
		"no such day":         {on, sql.NullString{String: "2026-02-30T00:00:00Z", Valid: true}, false},
		"Monero escrow off":   {off, stored(now.Add(-2 * gap)), false},
		"off and none stored": {off, sql.NullString{}, false},
	} {
		if got := c.a.xmrStaleStep(c.step, now); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// A-769: pg_input_is_valid needs PostgreSQL 16, and one call in a statement fails that statement at parse on an older
// server, whatever its data; no minimum is stated or enforced. TestMain runs the tests from the repository root.
func TestXMRShippedSQLHasNoPgInputFunctions(t *testing.T) {
	checked := 0
	err := filepath.WalkDir("internal/market", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".sql") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		if bytes.Contains(bytes.ToLower(b), []byte("pg_input_")) {
			t.Errorf("%s calls a pg_input_ function", path)
		}
		return nil
	})
	if err != nil || checked < 100 {
		t.Fatalf("checked %d files: %v", checked, err)
	}
}

// A-730: a pending outage's request says so on every line a paid order's parties read, as a restore's does.
func TestXMROutagePendingCancelWording(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	paid, sess := x.paidXMRPhysical(x.physical(x.vendor))
	x.forceDeadline(paid, "ship_by=now()-interval '1 minute'")
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,$2)", xmrReleaseRecheckKey, xmrRecheckOutage)
	code, body := x.xmrBuyerCancel(paid, sess)
	if code != 409 || !containsAll(body, "cannot be cancelled yet", "an outage", "Nothing was changed") || strings.Contains(body, "restore") {
		t.Fatalf("a buyer's cancel while an outage's recheck is pending: %d %s", code, body)
	}
	for name, page := range map[string]string{
		"buyer's order page":  x.page("/order?id="+paid, sess),
		"vendor's order page": x.page("/order?id="+paid, x.vendorSess),
		"vendor's desk":       x.page("/vendor-dashboard", x.vendorSess),
	} {
		page = html.UnescapeString(page)
		if !strings.Contains(page, "an outage") || strings.Contains(page, "restore recheck") || strings.Contains(page, "restore from backup") {
			t.Fatalf("the %s while an outage's recheck is pending:\n%s", name, page)
		}
	}
	// This is also the operator's request by hand (docs/operator-guide.md), with no gap: the next pass consumes it.
	x.mustPoll()
	if x.recheckKey() != "" || agStr(x.testEnv, "SELECT value FROM settings WHERE key=$1", xmrCancelWaitCauseKey) != xmrRecheckOutage {
		t.Fatalf("the request by hand: %q", x.recheckKey())
	}
}

// A-730: a restored database carries an old time, and the restore's request. It stays a restore: the step leaves the
// request alone, and the notice, the note and the page say what they say today, also where an outage's grace came first.
func TestXMROutageRestoreStaysRestore(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	order, sess, _ := x.shippedXMR(physical)
	paid, paidSess := x.paidXMRPhysical(physical)
	x.forceDeadline(order, "release_at=now()-interval '2 days'")
	x.forceDeadline(paid, "ship_by=now()-interval '1 minute'")

	// First an outage: its grace has its reason.
	x.setLastDeadlineStep("2 hours")
	x.mustPoll()
	if got := agStr(x.testEnv, "SELECT value FROM settings WHERE key=$1", xmrCancelWaitCauseKey); got != xmrRecheckOutage {
		t.Fatalf("the grace's reason after an outage: %q", got)
	}
	// Then a restore, whose request is already there when the step sees the old time.
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", xmrReleaseRecheckKey)
	x.setLastDeadlineStep("30 days")
	x.mustPoll()
	if x.recheckKey() != "" || x.state(order) != stateShipped {
		t.Fatalf("after the restore: request %q, order %s", x.recheckKey(), x.state(order))
	}
	notices := x.noticesOf(x.partyOf(order, roleBuyer), order)
	if countContaining(notices, "restored its records from a backup") != 1 || countContaining(notices, "an outage") != 1 {
		t.Fatalf("the notices: %q", notices)
	}
	if notes := x.movedNotes(order); len(notes) != 2 || !strings.Contains(notes[1], "after a restore from backup") {
		t.Fatalf("the notes: %q", notes)
	}
	if p := x.panel(order, sess); !strings.Contains(p, "moved after the market restored its records from a backup") {
		t.Fatalf("the panel after the restore's recheck:\n%s", p)
	}
	if code, body := x.xmrBuyerCancel(paid, paidSess); code != 409 || !strings.Contains(body, "restored its records from a backup") || strings.Contains(body, "an outage") {
		t.Fatalf("a buyer's cancel after the restore: %d %s", code, body)
	}
}

// A-730: under the restore gate the step neither records a time nor asks for anything: a restore's own request is what
// the first pass after the gate finds, and the old time cannot make it an outage.
func TestXMROutageNotUnderRestoreGate(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	x.setLastDeadlineStep("2 hours")
	stale := x.lastDeadlineStep()
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	x.mustPoll()
	if x.recheckKey() != "" || x.lastDeadlineStep() != stale {
		t.Fatalf("under the gate: request %q, time %q (was %q)", x.recheckKey(), x.lastDeadlineStep(), stale)
	}
}

// PAY101-6: the recheck's notice said the market releases the coins or the vendor can claim them, false while the
// vendor's ban holds the payment. A banned vendor's buyer is told what the order page says, whatever moved the deadline.
func TestXMRRecheckNoticeBannedVendor(t *testing.T) {
	for _, c := range []struct{ name, request, says string }{
		{"outage", "", "an outage"},
		{"restore", "true", "restored its records from a backup"},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			shipped, _, _ := x.shippedXMR(x.physical(x.vendor))
			delivered, _ := x.deliveredXMR(x.product)
			_, admin := x.user("bannotice_admin", "admin")
			x.banVendor(admin)
			for _, o := range []string{shipped, delivered} {
				x.forceDeadline(o, "release_at=now()-interval '1 hour'")
			}
			x.setLastDeadlineStep("2 hours")
			if c.request != "" {
				agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,$2)", xmrReleaseRecheckKey, c.request)
			}
			x.mustPoll()
			for _, o := range []string{shipped, delivered} {
				var moved []string
				for _, n := range x.noticesOf(x.partyOf(o, roleBuyer), o) {
					if strings.Contains(n, "so it was moved to") { // the shipment's and delivery's own notices predate the ban
						moved = append(moved, n)
					}
				}
				if len(moved) != 1 || !containsAll(moved[0], c.says, banDeadlineNote, "the order completes at that time and "+xmrBanHeldPayment) ||
					strings.Contains(moved[0], "can claim") || strings.Contains(moved[0], "releases the coins") {
					t.Fatalf("the recheck's notices for %s: %q", shortID(o), moved)
				}
				// VEN103-1: its vendor is told the payment is held, and no claim or release is promised.
				x.wantVendorMoved(o, c.says, "Your account is closed.", "the order completes at that time and "+xmrBanHeldPayment)
				if n := x.movedNotices(x.vendor, o)[0]; strings.Contains(n, "can claim") || strings.Contains(n, "releases the coins") {
					t.Fatalf("a banned vendor's notice promises the coins: %s", n)
				}
			}
		})
	}
	// A vendor who is not banned keeps the wording the buyer had.
	x := newXMREscrowEnv(t)
	x.mustPoll()
	shipped, _, _ := x.shippedXMR(x.physical(x.vendor))
	delivered, _ := x.deliveredXMR(x.product)
	for _, o := range []string{shipped, delivered} {
		x.forceDeadline(o, "release_at=now()-interval '1 hour'")
	}
	x.setLastDeadlineStep("2 hours")
	x.mustPoll()
	if n := x.noticesOf(x.partyOf(shipped, roleBuyer), shipped); !anyContains(n, "the market releases the coins in this Monero escrow to the vendor") || anyContains(n, "held") {
		t.Fatalf("the shipped order's notices: %q", n)
	}
	if n := x.noticesOf(x.partyOf(delivered, roleBuyer), delivered); !anyContains(n, "the vendor can claim the coins in this Monero escrow") || anyContains(n, "held") {
		t.Fatalf("the delivered order's notices: %q", n)
	}
}

// A-730: the first pass after an outage can spend minutes on its wallets before its deadline step, so the check runs
// first: before the first node or wallet call the request is set, and while that call hangs a dispute on a deadline that
// passed in the outage is accepted and a no-ship cancel past its time refused. The hang's end still reaches the recheck.
func TestXMROutageCheckedBeforeWalletCalls(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	disputed, disputedSess, _ := x.shippedXMR(physical)
	undisputed, _, _ := x.shippedXMR(physical)
	paid, paidSess := x.paidXMRPhysical(physical)
	for _, o := range []string{disputed, undisputed} {
		x.forceDeadline(o, "release_at=now()-interval '1 hour'")
	}
	x.forceDeadline(paid, "ship_by=now()-interval '1 hour'")
	x.setLastDeadlineStep("2 hours")

	// seen records the request at the first call of each method, from the fake's goroutine.
	var mu sync.Mutex
	seen := map[string]string{}
	record := func(method string) {
		var request string
		x.DB.QueryRow("SELECT COALESCE((SELECT value FROM settings WHERE key=$1),'none')", xmrReleaseRecheckKey).Scan(&request)
		mu.Lock()
		defer mu.Unlock()
		if _, ok := seen[method]; !ok {
			seen[method] = request
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hanging := make(chan struct{})
	var once sync.Once
	x.fake.hook("get_info", func() { record("get_info") })
	x.fake.hook("open_wallet", func() {
		record("open_wallet")
		once.Do(func() { close(hanging) })
		<-ctx.Done()
	})
	done := make(chan error, 1)
	go func() { done <- x.A.xmrEscrowPollOnce(ctx) }()
	select {
	case <-hanging:
	case err := <-done:
		t.Fatalf("the pass ended before its wallet call: %v", err)
	}
	if code, body := x.xmrDispute(disputed, disputedSess); code != 303 {
		t.Fatalf("a dispute on a deadline that passed in the outage, during the hang: %d %s", code, body)
	}
	if code, body := x.xmrBuyerCancel(paid, paidSess); code != 409 || !containsAll(body, "cannot be cancelled yet", "an outage") {
		t.Fatalf("a no-ship cancel during the hang: %d %s", code, body)
	}
	if ctx.Err() != nil {
		t.Fatal("the hang ended before the dispute and the cancel were tried")
	}
	if err := <-done; err == nil || ctx.Err() == nil {
		t.Fatalf("the pass did not outlast its budget: ctx %v, pass %v", ctx.Err(), err)
	}
	x.fake.hook("get_info", nil)
	x.fake.hook("open_wallet", nil)
	mu.Lock()
	defer mu.Unlock()
	if seen["get_info"] != xmrRecheckOutage || seen["open_wallet"] != xmrRecheckOutage {
		t.Fatalf("the request at the first calls: %v", seen)
	}
	if x.state(disputed) != stateDisputed || x.state(paid) != statePaid || x.state(undisputed) != stateShipped || x.recheckKey() != "" ||
		countContaining(x.noticesOf(x.partyOf(undisputed, roleBuyer), undisputed), "an outage") != 1 || x.releaseAt(undisputed).Before(x.dbNow().Add(71*time.Hour)) {
		t.Fatalf("after the pass: %s %s %s, request %q, deadline %s", x.state(disputed), x.state(paid), x.state(undisputed), x.recheckKey(), x.releaseAt(undisputed))
	}
}

// A-730: a watcher not ready retries its startup checks, up to watcherPassTimeout of node calls, before its pass, so the
// check comes before them too.
func TestXMROutageCheckedBeforeStartupRetry(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	x.setLastDeadlineStep("2 hours")
	x.A.escrowXMR.ready.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calling := make(chan string, 1)
	var once sync.Once
	x.fake.hook("get_info", func() {
		once.Do(func() {
			var request string
			x.DB.QueryRow("SELECT COALESCE((SELECT value FROM settings WHERE key=$1),'none')", xmrReleaseRecheckKey).Scan(&request)
			calling <- request
		})
		<-ctx.Done()
	})
	stopped := make(chan struct{})
	go func() { defer close(stopped); xmrEscrowWatcher(ctx, x.A) }()
	select {
	case request := <-calling:
		if request != xmrRecheckOutage {
			t.Errorf("the request at the retry's first node call: %q", request)
		}
	case <-time.After(30 * time.Second):
		t.Error("the startup retry made no node call")
	}
	cancel()
	<-stopped
	x.fake.hook("get_info", nil)
}

// A-730: a check that fails at the pass's start neither fails nor skips the pass: the wallets are still read, the later
// check reports the failure, and the gap stays for the next pass. Under the restore gate the early check writes nothing
// (TestXMROutageNotUnderRestoreGate).
func TestXMROutageEarlyCheckFailureKeepsPass(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.shippedXMR(x.physical(x.vendor))
	x.forceDeadline(order, "release_at=now()-interval '1 hour'")
	x.setLastDeadlineStep("2 hours")
	stale := x.lastDeadlineStep()
	agExec(x.testEnv, `CREATE FUNCTION a730_fail_early() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected write failure'; END $$`)
	agExec(x.testEnv, "CREATE TRIGGER a730_fail_early BEFORE INSERT OR UPDATE ON settings FOR EACH ROW WHEN (NEW.key='"+xmrLastDeadlineStepKey+"') EXECUTE FUNCTION a730_fail_early()")
	opened := make(chan struct{}, 64)
	x.fake.hook("open_wallet", func() {
		select {
		case opened <- struct{}{}:
		default:
		}
	})
	err := x.A.xmrEscrowPollOnce(context.Background())
	x.fake.hook("open_wallet", nil)
	if err == nil || !strings.Contains(err.Error(), "injected write failure") {
		t.Fatalf("the pass with a failing check: %v", err)
	}
	if len(opened) == 0 {
		t.Fatal("a failing check skipped the wallet work")
	}
	if x.recheckKey() != "" || x.lastDeadlineStep() != stale || x.state(order) != stateShipped {
		t.Fatalf("after the failed check: request %q, time %q, order %s", x.recheckKey(), x.lastDeadlineStep(), x.state(order))
	}
	agExec(x.testEnv, "DROP TRIGGER a730_fail_early ON settings")
	x.mustPoll()
	if x.state(order) != stateShipped || x.releaseAt(order).Before(x.dbNow().Add(71*time.Hour)) {
		t.Fatalf("the next pass: order %s, deadline %s", x.state(order), x.releaseAt(order))
	}
}

// PAY101-9: at the longest PAYMENT_POLL_INTERVAL every step is over an hour after the last, which is no outage: repeated
// passes ask for nothing, tell nobody and renew no grace, and a due order completes. A gap longer than the interval's
// allowance still counts.
func TestXMROutageNotFromPollInterval(t *testing.T) {
	for interval, want := range map[string]time.Duration{"": time.Hour, "1s": time.Hour, "25m": time.Hour, "26m": 62 * time.Minute, "1h": 130 * time.Minute, "nonsense": time.Hour} {
		t.Setenv("PAYMENT_POLL_INTERVAL", interval)
		if got := xmrOutageGap(); got != want {
			t.Fatalf("the gap at an interval of %q: %s, want %s", interval, got, want)
		}
	}
	t.Setenv("PAYMENT_POLL_INTERVAL", "1h")
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	near, _, _ := x.shippedXMR(physical)
	x.forceDeadline(near, "release_at=now()+interval '24 hours'")
	nearAt := x.releaseAt(near)
	for cycle := range 3 {
		due, _, _ := x.shippedXMR(physical)
		x.forceDeadline(due, "release_at=now()-interval '1 minute'")
		x.setLastDeadlineStep("1 hour 5 minutes")
		x.mustPoll()
		if x.recheckKey() != "" || x.state(due) != stateCompleted || !x.releaseAt(near).Equal(nearAt) {
			t.Fatalf("cycle %d: request %q, due order %s, near deadline %s (was %s)", cycle, x.recheckKey(), x.state(due), x.releaseAt(near), nearAt)
		}
		if anyContains(x.noticesOf(x.partyOf(near, roleBuyer), near), "an outage") || agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key=$1", xmrCancelWaitKey) != 0 {
			t.Fatalf("cycle %d: a false outage: %q", cycle, x.noticesOf(x.partyOf(near, roleBuyer), near))
		}
	}
	x.setLastDeadlineStep("3 hours")
	x.mustPoll()
	if x.releaseAt(near).Before(x.dbNow().Add(71*time.Hour)) || countContaining(x.noticesOf(x.partyOf(near, roleBuyer), near), "an outage") != 1 {
		t.Fatalf("a real gap at this interval: deadline %s, notices %q", x.releaseAt(near), x.noticesOf(x.partyOf(near, roleBuyer), near))
	}
}

// PAY101-10: the request by hand cannot undo a completion. With public access down and the watcher running, a deadline
// that passes completes on that pass, and its release goes ahead; a later request moves nothing and tells nobody of it.
// It does move a deadline still ahead, and that notice is true of an outage the watcher did not see.
func TestXMROutageManualRequestAfterCompletion(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	shipped, _, _ := x.shippedXMR(physical)
	delivered, _ := x.deliveredXMR(x.product)
	near, _, _ := x.shippedXMR(physical)
	for _, o := range []string{shipped, delivered} {
		x.forceDeadline(o, "release_at=now()-interval '1 minute'")
	}
	x.forceDeadline(near, "release_at=now()+interval '24 hours'")
	x.mustPoll()
	if x.state(shipped) != stateCompleted || x.state(delivered) != stateCompleted {
		t.Fatalf("before the request: %s %s", x.state(shipped), x.state(delivered))
	}
	shippedAt, deliveredAt := x.releaseAt(shipped), x.releaseAt(delivered)
	releases := len(x.xmrSettlements(shipped))
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO NOTHING", xmrReleaseRecheckKey, xmrRecheckOutage)
	x.mustPoll()
	if x.recheckKey() != "" || x.state(shipped) != stateCompleted || x.state(delivered) != stateCompleted ||
		!x.releaseAt(shipped).Equal(shippedAt) || !x.releaseAt(delivered).Equal(deliveredAt) || len(x.xmrSettlements(shipped)) != releases {
		t.Fatalf("after the request: %s %s, deadlines %s %s, settlements %q", x.state(shipped), x.state(delivered), x.releaseAt(shipped), x.releaseAt(delivered), x.xmrSettlements(shipped))
	}
	for _, o := range []string{shipped, delivered} {
		if anyContains(x.noticesOf(x.partyOf(o, roleBuyer), o), "an outage") || len(x.movedNotes(o)) != 0 {
			t.Fatalf("a completed order was told of an outage: %q %q", x.noticesOf(x.partyOf(o, roleBuyer), o), x.movedNotes(o))
		}
	}
	notices := x.noticesOf(x.partyOf(near, roleBuyer), near)
	if x.releaseAt(near).Before(x.dbNow().Add(71*time.Hour)) || !anyContains(notices, "an outage", "unavailable") || anyContains(notices, "escrow service") {
		t.Fatalf("the order still open: deadline %s, notices %q", x.releaseAt(near), notices)
	}
}

// A-761: the last request before public access returns can leave a buyer minutes. One more on return restores the grace.
func TestXMROutageRecheckOnReturnRestoresFullGrace(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	order, _, _ := x.shippedXMR(physical)
	buyer := x.partyOf(order, roleBuyer)
	restored, _, _ := x.shippedXMR(physical)
	request := func() {
		t.Helper()
		out, err := RequestDeadlineRecheck(context.Background(), x.DB)
		if err != nil || !strings.HasPrefix(out, "Deadline recheck now pending (outage).") {
			t.Fatalf("request: %q, %v", out, err)
		}
	}
	// A restore's recheck moved this one, so its flag stays set: no later recheck may skip it for that.
	x.forceDeadline(restored, "release_at=now()+interval '1 hour'")
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrReleaseRecheckKey)
	x.mustPoll()
	// The earlier request of the outage, then the return, with the deadline nearly used up: DA110-5.
	x.forceDeadline(order, "release_at=now()+interval '1 hour'")
	request()
	x.mustPoll()
	if d := x.releaseAt(order).Sub(x.dbNow()); d < 71*time.Hour || x.recheckKey() != "" {
		t.Fatalf("after the earlier request: %s left, request %q", d, x.recheckKey())
	}
	x.forceDeadline(order, "release_at=now()+interval '1 hour'")
	x.forceDeadline(restored, "release_at=now()+interval '1 hour'")
	request()
	x.mustPoll()
	for name, o := range map[string]string{"a repeated request's": order, "a restore's": restored} {
		if d := x.releaseAt(o).Sub(x.dbNow()); d < 71*time.Hour || x.state(o) != stateShipped || x.recheckKey() != "" {
			t.Fatalf("after the request on return, %s order: %s left, order %s, request %q", name, d, x.state(o), x.recheckKey())
		}
	}
	// QA108-2: each repeat tells every party again, as the guides warn.
	if n := len(x.movedNotices(buyer, order)); n != 2 {
		t.Fatalf("the buyer's notices after two rounds: %d", n)
	}
}

// wantCancelHeld fails unless the buyer's page offers no cancel for order and the action refuses it (409) with says, changing
// nothing.
func (x *xmrEscrowEnv) wantCancelHeld(order, sess string, says ...string) {
	x.t.Helper()
	if p := x.page("/order?id="+order, sess); strings.Contains(p, `action="/orders/cancel"`) {
		x.t.Fatalf("the page offers a cancel the action refuses:\n%s", html.UnescapeString(p))
	}
	code, body := x.xmrBuyerCancel(order, sess)
	if code != 409 || !containsAll(html.UnescapeString(body), says...) || x.state(order) != statePaid || len(x.xmrSettlements(order)) != 0 {
		x.t.Fatalf("a held cancel: %d %s, order %s, settlements %q", code, body, x.state(order), x.xmrSettlements(order))
	}
}

// wantCancelAccepted fails unless the buyer's page offers the cancel and states no hold, and the action takes it: 303, the
// order cancelled and one refund requested.
func (x *xmrEscrowEnv) wantCancelAccepted(order, sess string) {
	x.t.Helper()
	p := html.UnescapeString(x.page("/order?id="+order, sess))
	if !strings.Contains(p, `action="/orders/cancel"`) || strings.Contains(p, "The buyer's cancel waits") || strings.Contains(p, outageWhy) {
		x.t.Fatalf("the page of an order with no hold:\n%s", p)
	}
	code, body := x.xmrBuyerCancel(order, sess)
	if code != 303 || x.state(order) != stateCancelled || len(x.xmrSettlements(order)) != 1 {
		x.t.Fatalf("a cancel with no hold: %d %s, order %s, settlements %q", code, body, x.state(order), x.xmrSettlements(order))
	}
}

// wantDeskLine fails unless the vendor desk's row of order says says.
func (x *xmrEscrowEnv) wantDeskLine(order, says string) {
	x.t.Helper()
	page := html.UnescapeString(x.page("/vendor-dashboard", x.vendorSess))
	_, after, found := strings.Cut(page, "/order?id="+order)
	if !found {
		x.t.Fatalf("no desk row of %s:\n%s", order, page)
	}
	row, _, _ := strings.Cut(after, "</tr>")
	if !strings.Contains(row, says) {
		x.t.Fatalf("the desk row of %s does not say %q:\n%s", order, says, row)
	}
}

// settingAgo sets a stored time to age before now, in the format the application writes.
func (x *xmrEscrowEnv) settingAgo(key, age string) {
	x.t.Helper()
	agExec(x.testEnv, `INSERT INTO settings(key,value) VALUES($1,to_char((now()-$2::interval) AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, key, age)
}

// PAY113-1: a grace whose end falls after a second gap began belongs to that outage: the market was down when it ended, so the
// vendor had no use of its end, and the round keeps what the grace has, its start or none. One that ended before the gap began
// narrows as today. The hold while the round is still to come, on a stale step and with its request pending, goes by what the
// round will do (PAY113-2), so a buyer's cancel does not refund an order the round is about to hold.
func TestXMROutageGraceEndedInsideSecondGap(t *testing.T) {
	for _, origin := range []string{"outage", "restore"} {
		for _, c := range []struct {
			name, ended string
			kept        bool
		}{{"ended in the gap", "12 hours", true}, {"ended before the gap", "73 hours", false}} {
			t.Run(origin+" grace "+c.name, func(t *testing.T) {
				x := newXMREscrowEnv(t)
				x.mustPoll()
				type paid struct{ order, sess string }
				newPaid := func(ago string) paid {
					order, sess := x.paidXMRPhysical(x.physical(x.vendor))
					x.forceDeadline(order, "ship_by=now()-interval '"+ago+"'")
					return paid{order, sess}
				}
				// One order per stage where the cancel may go through: on the stale step, with the request pending, after the round.
				inFirst := []paid{newPaid("86 hours"), newPaid("86 hours"), newPaid("86 hours")} // in the first outage's gap
				before := []paid{newPaid("100 hours"), newPaid("100 hours"), newPaid("100 hours")}
				inSecond := newPaid("1 hour")

				// The first round, 84 hours ago as the settings below read.
				if origin == "outage" {
					x.setLastDeadlineStep("2 hours")
				} else {
					agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrReleaseRecheckKey)
				}
				x.mustPoll()
				if got := len(x.settingsOf(xmrCancelWaitFromKey)); got != map[string]int{"outage": 1, "restore": 0}[origin] {
					t.Fatalf("the first grace's start: %d rows", got)
				}
				x.settingAgo(xmrCancelWaitKey, c.ended)
				if origin == "outage" {
					x.settingAgo(xmrCancelWaitFromKey, "88 hours")
				}
				firstFrom := x.settingsOf(xmrCancelWaitFromKey)
				x.setLastDeadlineStep("72 hours") // down from 12 hours after the first round
				gap := x.lastDeadlineStep()

				// inFirst's ship_by is after the first start and before this gap; before's is before both.
				heldBefore := c.kept && origin == "restore"
				stage := func(i int, says ...string) {
					t.Helper()
					if c.kept {
						x.wantCancelHeld(inFirst[i].order, inFirst[i].sess, says...)
					} else {
						x.wantCancelAccepted(inFirst[i].order, inFirst[i].sess)
					}
					if heldBefore {
						x.wantCancelHeld(before[i].order, before[i].sess, says...)
					} else {
						x.wantCancelAccepted(before[i].order, before[i].sess)
					}
					x.wantCancelHeld(inSecond.order, inSecond.sess, says...)
				}
				stage(0, "cannot be cancelled yet", outageWhy) // on the stale step
				x.A.xmrNoteDeadlineStepEarly(context.Background())
				if x.recheckKey() != xmrRecheckOutage || !slices.Equal(x.settingsOf(xmrOutageGapFromKey), []string{gap}) {
					t.Fatalf("the second request: %q, gap %q, want %q", x.recheckKey(), x.settingsOf(xmrOutageGapFromKey), gap)
				}
				stage(1, "cannot be cancelled yet", outageWhy) // with the request pending

				x.mustPoll() // the second round
				from := x.settingsOf(xmrCancelWaitFromKey)
				switch {
				case c.kept && !slices.Equal(from, firstFrom):
					t.Fatalf("the start after the round: %q, want the grace's own %q", from, firstFrom)
				case !c.kept && !slices.Equal(from, []string{gap}):
					t.Fatalf("the start after the round: %q, want the gap's %q", from, gap)
				}
				if x.recheckKey() != "" || len(x.settingsOf(xmrOutageGapFromKey)) != 0 {
					t.Fatalf("after the round: request %q, gap %q", x.recheckKey(), x.settingsOf(xmrOutageGapFromKey))
				}
				until := x.cancelWaitUntil()
				if d := until.Sub(x.dbNow()); d < 71*time.Hour || d > 72*time.Hour {
					t.Fatalf("the new end is %s away", d)
				}
				stage(2, "You can cancel this order from "+utcFrom(until), outageWhy)
			})
		}
	}
}

// PAY113R-1: a start stored before the database's clock stepped back says nothing of an order paid under the behind clock, whose
// ship_by is far behind true time once the clock is corrected. The step that sees the clock behind its stored time drops both
// starts, so the holds that would read them hold every order: on the stale step, with the request pending and after the round.
// A step back within the tolerance is ordinary clock correction and removes nothing. Simulated database clock, not a stepped host.
func TestXMROutageBackStepDropsGraceStart(t *testing.T) {
	const line = "monero escrow deadline step: the database's clock is behind the stored step"
	for _, c := range []struct {
		name         string
		round, ended bool
	}{{"running grace", true, false}, {"ended grace", true, true}, {"no earlier start", false, false}} {
		t.Run(c.name, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			jump := x.simDBClock()
			logs := captureLog(t)
			order, addr := x.freshCheckout(x.physical(x.vendor)) // a checkout under the behind clock is refused: this one is older
			sess := x.sessOf(order, roleBuyer)
			if c.round {
				x.setLastDeadlineStep("2 hours")
				x.mustPoll()
				if len(x.settingsOf(xmrCancelWaitFromKey)) != 1 {
					t.Fatalf("the round wrote no start")
				}
			}
			if c.ended {
				x.settingAgo(xmrCancelWaitKey, "10 days")
				x.settingAgo(xmrCancelWaitFromKey, "13 days")
			}
			jump("-30 days")
			x.mustPoll()
			if v := x.settingsOf(xmrCancelWaitFromKey); len(v) != 0 {
				t.Fatalf("the start after the clock stepped back: %q", v)
			}
			if n := strings.Count(logs.String(), line); n != 1 {
				t.Fatalf("%d log lines for one step back:\n%s", n, logs.String())
			}
			x.mustPoll()
			if n := strings.Count(logs.String(), line); n != 1 {
				t.Fatalf("%d log lines after a second pass on the same clock", n)
			}
			x.fake.payAddress(addr, hex64("fund "+order), xmrListingPrice, 10)
			x.mustPoll()
			if x.state(order) != statePaid {
				t.Fatalf("the order under the behind clock is %s", x.state(order))
			}
			jump("0")
			if behind := x.dbNow().Sub(x.escrowTime(order, "ship_by")); behind < 20*24*time.Hour {
				t.Fatalf("ship_by is only %s behind true time", behind)
			}
			x.wantCancelHeld(order, sess, "cannot be cancelled yet", outageWhy)
			x.wantDeskLine(order, "the buyer's cancel waits for this market's recheck after an outage")
			x.A.xmrNoteDeadlineStepEarly(context.Background())
			x.wantCancelHeld(order, sess, "cannot be cancelled yet", outageWhy)
			x.mustPoll()
			if x.recheckKey() != "" {
				t.Fatalf("the round did not run: %q", x.recheckKey())
			}
			x.wantCancelHeld(order, sess, "You can cancel this order from "+utcFrom(x.cancelWaitUntil()), outageWhy)
		})
	}
	t.Run("the tolerance", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		jump := x.simDBClock()
		logs := captureLog(t)
		x.setLastDeadlineStep("2 hours")
		x.mustPoll()
		from := x.settingsOf(xmrCancelWaitFromKey)
		if len(from) != 1 {
			t.Fatalf("the round's start: %q", from)
		}
		jump("10 minutes") // forward only: as today
		x.mustPoll()
		jump("6 minutes") // back by 4 minutes, within the tolerance
		x.mustPoll()
		if v := x.settingsOf(xmrCancelWaitFromKey); !slices.Equal(v, from) || strings.Contains(logs.String(), "is behind the stored step") {
			t.Fatalf("a step within the tolerance: start %q, was %q, log:\n%s", v, from, logs.String())
		}
		jump("0") // back by 6 minutes from the step stored at 6 minutes ahead: 1 minute past the tolerance's 5
		x.mustPoll()
		if v := x.settingsOf(xmrCancelWaitFromKey); len(v) != 0 || !strings.Contains(logs.String(), "is behind the stored step") {
			t.Fatalf("a step past the tolerance: start %q, log:\n%s", v, logs.String())
		}
	})
	t.Run("a pending request's gap", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		jump := x.simDBClock()
		order, sess := x.paidXMRPhysical(x.physical(x.vendor))
		x.forceDeadline(order, "ship_by=now()-interval '10 hours'")
		x.setLastDeadlineStep("2 hours")
		x.A.xmrNoteDeadlineStepEarly(context.Background())
		if len(x.settingsOf(xmrOutageGapFromKey)) != 1 {
			t.Fatalf("no gap row: %q", x.settingsOf(xmrOutageGapFromKey))
		}
		jump("-30 days")
		x.A.xmrNoteDeadlineStepEarly(context.Background())
		if v := x.settingsOf(xmrOutageGapFromKey); len(v) != 0 || x.recheckKey() != xmrRecheckOutage {
			t.Fatalf("after the step back: gap %q, request %q", v, x.recheckKey())
		}
		jump("0")
		x.wantCancelHeld(order, sess, "cannot be cancelled yet", outageWhy) // a request with no start holds them all
	})
}

// PAY113R-2: a jump shorter than the outage gap makes no request, and its pass stores a step ahead of true time. An order due
// within the jump then looks overdue before a real gap that follows, by that stored step. So every comparison with a found gap's
// start holds from xmrOutageGap() before it, the most such a jump can be, and errs towards holding. An order overdue by more than
// that margin still cancels, on the stale step, with the request pending and after the round. Simulated database clock and rows.
func TestXMROutageForwardStepThenOutage(t *testing.T) {
	t.Run("the jump", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		jump := x.simDBClock()
		order, sess := x.paidXMRPhysical(x.physical(x.vendor))
		x.forceDeadline(order, "ship_by=now()+interval '30 minutes'")
		jump("50 minutes")
		x.mustPoll()
		if x.recheckKey() != "" {
			t.Fatalf("a jump under the gap asked for a recheck: %q", x.recheckKey())
		}
		jump("3 hours") // true time moved on, with the step stored 50 minutes ahead of it
		x.wantCancelHeld(order, sess, "cannot be cancelled yet", outageWhy)
		x.wantDeskLine(order, "the buyer's cancel waits for this market's recheck after an outage")
		x.A.xmrNoteDeadlineStepEarly(context.Background())
		if x.recheckKey() != xmrRecheckOutage {
			t.Fatalf("the request: %q", x.recheckKey())
		}
		x.wantCancelHeld(order, sess, "cannot be cancelled yet", outageWhy)
	})
	t.Run("by rows", func(t *testing.T) { // the round's clocks agree, so it runs
		x := newXMREscrowEnv(t)
		x.mustPoll()
		type paid struct{ order, sess string }
		newPaid := func(ago string) paid {
			order, sess := x.paidXMRPhysical(x.physical(x.vendor))
			x.forceDeadline(order, "ship_by=now()-interval '"+ago+"'")
			return paid{order, sess}
		}
		inside := newPaid("2 hours 30 minutes") // before the stored step, inside the margin
		outside := []paid{newPaid("3 hours 30 minutes"), newPaid("3 hours 30 minutes"), newPaid("3 hours 30 minutes")}
		x.setLastDeadlineStep("2 hours 10 minutes")
		x.wantCancelHeld(inside.order, inside.sess, "cannot be cancelled yet", outageWhy)
		x.wantDeskLine(outside[0].order, "passed, so the buyer may cancel")
		x.wantCancelAccepted(outside[0].order, outside[0].sess)
		x.A.xmrNoteDeadlineStepEarly(context.Background())
		x.wantCancelHeld(inside.order, inside.sess, "cannot be cancelled yet", outageWhy)
		x.wantCancelAccepted(outside[1].order, outside[1].sess)
		x.mustPoll()
		until := x.cancelWaitUntil()
		x.wantCancelHeld(inside.order, inside.sess, "You can cancel this order from "+utcFrom(until), outageWhy)
		x.wantDeskLine(inside.order, "the buyer can cancel from "+utcFrom(until))
		x.wantDeskLine(outside[2].order, "passed, so the buyer may cancel")
		x.wantCancelAccepted(outside[2].order, outside[2].sess)
		// The panel names the time the hold actually starts from.
		_, admin := x.user("xmargin_"+randomToken()[:6], "admin")
		var from time.Time
		if err := x.DB.QueryRow("SELECT value::timestamptz FROM settings WHERE key=$1", xmrCancelWaitFromKey).Scan(&from); err != nil {
			t.Fatal(err)
		}
		if s := x.recheckPanel(admin); !strings.Contains(s, utcUntil(from.Add(-xmrOutageGap()))) {
			t.Fatalf("the panel does not name the margin's time %s:\n%s", utcUntil(from.Add(-xmrOutageGap())), s)
		}
	})
}

// DA108-1 (the ruled default, which the owner may overturn): an outage the watcher detected holds the buyer's no-ship cancel
// only for an order whose ship-by time fell after the gap began. One overdue before it has no such reason to wait. Further
// rounds in the same grace keep that start, a host request with no gap has no start to go by and holds them all, and a
// restore holds them all as it always did.
func TestXMROutageGraceOnlyAfterGapBegan(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	logs := captureLog(t)
	paid := func(n int) (orders, sessions []string) {
		for range n {
			o, sess := x.paidXMRPhysical(x.physical(x.vendor))
			orders, sessions = append(orders, o), append(sessions, sess)
		}
		return orders, sessions
	}
	os, ss := paid(8)
	before, inGap, soon := os[0], os[1], os[2]
	x.forceDeadline(before, "ship_by=now()-interval '4 hours'")
	x.forceDeadline(inGap, "ship_by=now()-interval '30 minutes'")
	x.forceDeadline(soon, "ship_by=now()+interval '1 hour'")
	x.forceDeadline(os[3], "ship_by=now()-interval '4 hours'") // the second round's
	x.forceDeadline(os[4], "ship_by=now()-interval '4 hours'") // the host request's
	x.forceDeadline(os[5], "ship_by=now()-interval '4 hours'") // the restore's
	x.forceDeadline(os[6], "ship_by=now()-interval '4 hours'") // the watcher's request a restore overwrites
	x.forceDeadline(os[7], "ship_by=now()-interval '4 hours'") // the host's request a found gap meets
	x.setLastDeadlineStep("2 hours")
	x.mustPoll()
	x.wantRecheckRecorded(logs, "outage", 0)
	var from string
	if err := x.DB.QueryRow("SELECT value FROM settings WHERE key=$1", xmrCancelWaitFromKey).Scan(&from); err != nil {
		t.Fatalf("the grace's start: %v", err)
	}
	if v := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", xmrOutageGapFromKey); len(v) != 0 {
		t.Fatalf("the request's gap stayed: %q", v)
	}
	// Before the gap: the cancel goes through. Its order page does not claim a wait either.
	if p := x.page("/order?id="+before, ss[0]); strings.Contains(p, "an outage made this market unavailable") {
		t.Fatalf("the order page of an order overdue before the gap claims the outage:\n%s", p)
	}
	if code, body := x.xmrBuyerCancel(before, ss[0]); code != 303 || x.state(before) != stateCancelled {
		t.Fatalf("overdue before the gap: %d %s, order %s", code, body, x.state(before))
	}
	// After the gap began, passed or still to come within the grace: the cancel waits, and says an outage is why.
	until := utcFrom(x.cancelWaitUntil())
	for i, o := range []string{inGap, soon} {
		if code, body := x.xmrBuyerCancel(o, ss[i+1]); code != 409 || x.state(o) != statePaid || !containsAll(html.UnescapeString(body), "You can cancel this order from "+until, outageWhy) {
			t.Fatalf("ship_by after the gap began (%d): %d %s, order %s", i, code, body, x.state(o))
		}
	}

	// A second gap in the same grace keeps the first start: this order overdue before it still has nothing to wait for.
	x.setLastDeadlineStep("2 hours")
	x.mustPoll()
	// A host request in the same grace keeps it too.
	if _, err := RequestDeadlineRecheck(context.Background(), x.DB); err != nil {
		t.Fatal(err)
	}
	x.mustPoll()
	var fromNow string
	if err := x.DB.QueryRow("SELECT value FROM settings WHERE key=$1", xmrCancelWaitFromKey).Scan(&fromNow); err != nil || fromNow != from {
		t.Fatalf("the grace's start after more rounds: %q, was %q, %v", fromNow, from, err)
	}
	if code, body := x.xmrBuyerCancel(os[3], ss[3]); code != 303 || x.state(os[3]) != stateCancelled {
		t.Fatalf("overdue before the gap, later rounds: %d %s, order %s", code, body, x.state(os[3]))
	}
	if code, body := x.xmrBuyerCancel(inGap, ss[1]); code != 409 || x.state(inGap) != statePaid {
		t.Fatalf("ship_by after the gap began, later rounds: %d %s, order %s", code, body, x.state(inGap))
	}

	// The grace over, a host request alone knows no start: everything overdue waits, and the old start is gone.
	agExec(x.testEnv, "UPDATE settings SET value=to_char((now()-interval '1 minute') AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') WHERE key=$1", xmrCancelWaitKey)
	if _, err := RequestDeadlineRecheck(context.Background(), x.DB); err != nil {
		t.Fatal(err)
	}
	x.mustPoll()
	if v := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", xmrCancelWaitFromKey); len(v) != 0 {
		t.Fatalf("a host request's grace has a start: %q", v)
	}
	if code, body := x.xmrBuyerCancel(os[4], ss[4]); code != 409 || x.state(os[4]) != statePaid || !strings.Contains(html.UnescapeString(body), outageWhy) {
		t.Fatalf("a host request's grace: %d %s, order %s", code, body, x.state(os[4]))
	}

	// A watcher's gap in that grace keeps it whole: a request with no start stays one.
	x.setLastDeadlineStep("2 hours")
	x.mustPoll()
	if v := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", xmrCancelWaitFromKey); len(v) != 0 {
		t.Fatalf("a gap narrowed a running grace: %q", v)
	}

	// A grace with a start that has ended, then a restore: as today, every order overdue waits, with a restore's reason.
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,$2)", xmrCancelWaitFromKey, from)
	agExec(x.testEnv, "UPDATE settings SET value=to_char((now()-interval '1 minute') AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') WHERE key=$1", xmrCancelWaitKey)
	x.setLastDeadlineStep("2 hours") // the backup's old time: the gap is found, and a restore's request stays one
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrReleaseRecheckKey)
	x.mustPoll()
	if v := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key IN ($1,$2)", xmrCancelWaitFromKey, xmrOutageGapFromKey); len(v) != 0 {
		t.Fatalf("a restore's grace has a start: %q", v)
	}
	if code, body := x.xmrBuyerCancel(os[5], ss[5]); code != 409 || x.state(os[5]) != statePaid || !strings.Contains(html.UnescapeString(body), "restored its records from a backup") {
		t.Fatalf("a restore's grace: %d %s, order %s", code, body, x.state(os[5]))
	}
	// A restore that leaves a later end in place still holds every order: it is not narrowed by the row the backup carried.
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,$2)", xmrCancelWaitFromKey, from)
	agExec(x.testEnv, "UPDATE settings SET value=to_char((now()+interval '100 hours') AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') WHERE key=$1", xmrCancelWaitKey)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrReleaseRecheckKey)
	x.mustPoll()
	if v := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", xmrCancelWaitFromKey); len(v) != 0 {
		t.Fatalf("a restore kept the start of a later grace: %q", v)
	}

	// QA111-1: a restore's overwrite of a watcher's request does not narrow its grace by the gap row that request left: every
	// order overdue waits, or a buyer's cancel opens during a restore's hold. Dropping "outage &&" from the gap's case fails here.
	endGrace := func() {
		t.Helper()
		agExec(x.testEnv, "UPDATE settings SET value=to_char((now()-interval '1 minute') AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') WHERE key=$1", xmrCancelWaitKey)
	}
	endGrace()
	x.setLastDeadlineStep("2 hours")
	x.A.xmrNoteDeadlineStepEarly(context.Background())
	if v := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", xmrOutageGapFromKey); len(v) != 1 || x.recheckKey() != xmrRecheckOutage {
		t.Fatalf("the watcher's request: gap row %q, request %q", v, x.recheckKey())
	}
	agExec(x.testEnv, "UPDATE settings SET value='true' WHERE key=$1", xmrReleaseRecheckKey) // restore.sh's overwrite
	x.mustPoll()
	if v := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key IN ($1,$2)", xmrCancelWaitFromKey, xmrOutageGapFromKey); len(v) != 0 {
		t.Fatalf("a restore's overwrite kept the gap's start: %q", v)
	}
	if code, body := x.xmrBuyerCancel(os[6], ss[6]); code != 409 || x.state(os[6]) != statePaid || !strings.Contains(html.UnescapeString(body), "restored its records from a backup") {
		t.Fatalf("a restore's overwrite of a watcher's request: %d %s, order %s", code, body, x.state(os[6]))
	}

	// QA111-1: a host's pending request keeps no start when the watcher then finds a gap: the gap did not write that request, so
	// its time is no start for it, and the order overdue before the gap still waits. n>=0 for n>0 in the check fails here.
	endGrace()
	if _, err := RequestDeadlineRecheck(context.Background(), x.DB); err != nil {
		t.Fatal(err)
	}
	x.setLastDeadlineStep("2 hours")
	x.A.xmrNoteDeadlineStepEarly(context.Background())
	if v := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", xmrOutageGapFromKey); len(v) != 0 {
		t.Fatalf("a gap found under a host's request wrote a start: %q", v)
	}
	x.mustPoll()
	if v := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key IN ($1,$2)", xmrCancelWaitFromKey, xmrOutageGapFromKey); len(v) != 0 {
		t.Fatalf("a host's request, then a found gap, has a start: %q", v)
	}
	if code, body := x.xmrBuyerCancel(os[7], ss[7]); code != 409 || x.state(os[7]) != statePaid || !strings.Contains(html.UnescapeString(body), outageWhy) {
		t.Fatalf("a host's request, then a found gap: %d %s, order %s", code, body, x.state(os[7]))
	}
}

// PAY101-11: the check does not wait for the watcher's lock. With another process holding it the pass does no work, yet
// the request is set, so a dispute is accepted and a cancel refused. Processes checking at once ask once: the second
// waits on the row and then reads the first's new time.
func TestXMROutageCheckedWithLockHeld(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	order, sess, _ := x.shippedXMR(physical)
	paid, paidSess := x.paidXMRPhysical(physical)
	x.forceDeadline(order, "release_at=now()-interval '1 hour'")
	x.forceDeadline(paid, "ship_by=now()-interval '1 hour'")
	x.setLastDeadlineStep("2 hours")

	conn, err := x.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var locked bool
	if err = conn.QueryRowContext(context.Background(), "SELECT pg_try_advisory_lock($1)", xmrEscrowWatcherLock).Scan(&locked); err != nil || !locked {
		t.Fatalf("the lock: %v %v", locked, err)
	}
	calls := x.fake.count()
	if err = x.A.xmrEscrowPollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if x.fake.count() != calls || x.recheckKey() != xmrRecheckOutage {
		t.Fatalf("with the lock held: %d calls, request %q", x.fake.count()-calls, x.recheckKey())
	}
	if code, body := x.xmrBuyerCancel(paid, paidSess); code != 409 || !strings.Contains(body, "an outage") {
		t.Fatalf("a no-ship cancel with the lock held: %d %s", code, body)
	}
	if code, body := x.xmrDispute(order, sess); code != 303 {
		t.Fatalf("a dispute with the lock held: %d %s", code, body)
	}

	// Many at once: one request.
	agExec(x.testEnv, "DELETE FROM settings WHERE key=$1", xmrReleaseRecheckKey)
	x.setLastDeadlineStep("2 hours")
	agExec(x.testEnv, "CREATE TABLE a730_asks(n int)")
	agExec(x.testEnv, `CREATE FUNCTION a730_count_ask() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO a730_asks VALUES(1); RETURN NEW; END $$`)
	agExec(x.testEnv, "CREATE TRIGGER a730_count_ask BEFORE INSERT ON settings FOR EACH ROW WHEN (NEW.key='"+xmrReleaseRecheckKey+"') EXECUTE FUNCTION a730_count_ask()")
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() { defer wg.Done(); x.A.xmrEscrowPollOnce(context.Background()) }()
	}
	wg.Wait()
	if asks := agInt(x.testEnv, "SELECT count(*) FROM a730_asks"); asks != 1 || x.recheckKey() != xmrRecheckOutage {
		t.Fatalf("%d asks, request %q", asks, x.recheckKey())
	}
}

// OPS104-1: Admin → Payments shows, read-only and to an administrator only, a recheck pending and its cause, and when the
// last one ran, its cause and how many orders it moved; each recheck records them in its own transaction. With escrow off
// a pending request stays pending, and the page says so.
func TestAdminPaymentsDeadlineRecheck(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	logs := captureLog(t)
	_, admin := x.user("xrecheck_"+randomToken()[:6], "admin")
	_, moderator := x.user("xrecheckm_"+randomToken()[:6], "moderator")
	section := func() string {
		t.Helper()
		page := html.UnescapeString(x.page("/admin/payments", admin))
		_, after, found := strings.Cut(page, `id="xmr-deadline-recheck"`)
		if !found {
			t.Fatalf("no recheck section:\n%s", page)
		}
		block, _, _ := strings.Cut(after, "<h3")
		return block
	}
	if s := section(); !containsAll(s, "No deadline recheck is pending.", "No deadline recheck has been recorded.", "-request-deadline-recheck", "and run it once more when access returns") || strings.Contains(s, "<form") {
		t.Fatalf("nothing pending or recorded:\n%s", s)
	}
	order, _, _ := x.shippedXMR(x.physical(x.vendor))
	x.forceDeadline(order, "release_at=now()+interval '24 hours'")

	// An outage's request, pending: the command's own write.
	out, err := RequestDeadlineRecheck(context.Background(), x.DB)
	if err != nil || !strings.HasPrefix(out, "Deadline recheck now pending (outage).") {
		t.Fatalf("request: %q, %v", out, err)
	}
	if s := section(); !strings.Contains(s, "Deadline recheck pending: an outage asked for it.") {
		t.Fatalf("an outage's pending:\n%s", s)
	}
	// Other roles and signed-out visitors do not see it.
	for name, sess := range map[string]string{"buyer": x.sessOf(order, roleBuyer), "vendor": x.vendorSess, "moderator": moderator, "signed out": ""} {
		w := x.do("GET", "/admin/payments", sess, nil)
		if w.Code == 200 || strings.Contains(w.Body.String(), "xmr-deadline-recheck") || strings.Contains(w.Body.String(), "Deadline recheck") {
			t.Fatalf("%s reads the section: %d", name, w.Code)
		}
	}
	// Escrow off leaves it pending.
	on := x.A.escrowXMR
	x.A.escrowXMR = nil
	if s := section(); !containsAll(s, "Deadline recheck pending: an outage asked for it.", "Monero escrow is off, so it stays pending") {
		t.Fatalf("pending with escrow off:\n%s", s)
	}
	x.A.escrowXMR = on

	// It ran: when, the cause and the count, with no order data.
	x.mustPoll()
	x.wantRecheckRecorded(logs, "outage", 1, order)
	if rows := x.requestRecords(); len(rows) != 1 || rows[0] != "-:"+xmrRecheckRequestedAudit { // PM-3: who asked
		t.Fatalf("the request's audit rows: %q", rows)
	}
	at := x.lastRecheckAt()
	if s := section(); !containsAll(s, "No deadline recheck is pending.", "Last deadline recheck: "+utcUntil(at)+", after an outage; it moved 1 order deadline(s).") || strings.Contains(s, shortID(order)) {
		t.Fatalf("after the recheck:\n%s", s)
	}
	// A restore's request reads as one, and its recheck replaces the record, here moving nothing.
	x.forceDeadline(order, "release_at=now()+interval '5 days'")
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrReleaseRecheckKey)
	if s := section(); !strings.Contains(s, "Deadline recheck pending: a restore asked for it.") {
		t.Fatalf("a restore's pending:\n%s", s)
	}
	x.mustPoll()
	if s := section(); !strings.Contains(s, "after a restore from backup; it moved 0 order deadline(s).") {
		t.Fatalf("after a restore's recheck:\n%s", s)
	}
	if !strings.Contains(logs.String(), recheckLogPrefix+": cause=restore moved=0\n") {
		t.Fatalf("the log after a restore's recheck:\n%s", logs.String())
	}
	if rows := x.recheckRecords(); len(rows) != 2 || rows[1] != "-:Monero escrow deadline recheck (cause: restore): moved 0 order deadline(s)" {
		t.Fatalf("the audit rows: %q", rows)
	}
	// DA108-2: moved 0 is not "nothing happened": the panel states the hold on cancels and its end.
	waitUntil := utcFrom(x.cancelWaitUntil())
	if s := section(); !containsAll(s, "Buyers' no-ship cancels are held until "+waitUntil, "even when the count above is 0") || strings.Contains(s, "overdue since before") {
		t.Fatalf("a restore's hold on cancels:\n%s", s)
	}
	// An outage the watcher detected names the start of the gap, and an order overdue before it is not held.
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,to_char((now()-interval '2 hours') AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'))", xmrCancelWaitFromKey)
	var from time.Time
	if err := x.DB.QueryRow("SELECT value::timestamptz FROM settings WHERE key=$1", xmrCancelWaitFromKey).Scan(&from); err != nil {
		t.Fatal(err)
	}
	if s := section(); !containsAll(s, "Buyers' no-ship cancels are held until "+waitUntil, "An order overdue since before "+utcUntil(from.Add(-xmrOutageGap()))+", the outage threshold before the gap began, is not held") {
		t.Fatalf("the hold with a gap's start:\n%s", s)
	}
	// Over, or spoiled by a hand, it says nothing.
	agExec(x.testEnv, "UPDATE settings SET value='2000-01-01T00:00:00Z' WHERE key=$1", xmrCancelWaitKey)
	if s := section(); strings.Contains(s, "no-ship cancels are held") {
		t.Fatalf("an ended hold is stated:\n%s", s)
	}
	// A record only a hand could have spoiled shows as none, and does not break the page.
	agExec(x.testEnv, "UPDATE settings SET value='not a record' WHERE key=$1", xmrLastRecheckKey)
	if s := section(); !strings.Contains(s, "No deadline recheck has been recorded.") {
		t.Fatalf("an unreadable record:\n%s", s)
	}
}

// lastRecheckAt is the time the last recheck recorded.
func (x *xmrEscrowEnv) lastRecheckAt() time.Time {
	x.t.Helper()
	var at time.Time
	if err := x.DB.QueryRow("SELECT (value::jsonb->>'at')::timestamptz FROM settings WHERE key=$1", xmrLastRecheckKey).Scan(&at); err != nil {
		x.t.Fatal(err)
	}
	return at
}

// OPS104-1: the read-only preview renders the section without a database: nothing pending, nothing recorded.
func TestAdminPaymentsDeadlineRecheckPreview(t *testing.T) {
	a, err := New(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/admin/payments", nil))
	if w.Code != 200 || !containsAll(w.Body.String(), `id="xmr-deadline-recheck"`, "No deadline recheck is pending.", "No deadline recheck has been recorded.") {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
}

// simDBClock puts every statement the application sends under a simulated database clock: now() in the application's SQL
// resolves to a function that adds a settable interval to the real one, by the search path of a reconnected App, and the
// returned function sets it ("30 days" ahead, "0" back). Clock_timestamp() and column defaults keep the real time. It stands
// for a stepped host clock, which a test must not do; it is not evidence of one.
func (x *xmrEscrowEnv) simDBClock() (jump func(ahead string)) {
	x.t.Helper()
	schema := "clock_" + randomToken()[:12]
	agExec(x.testEnv, "CREATE SCHEMA "+schema)
	agExec(x.testEnv, "CREATE TABLE "+schema+".shift AS SELECT interval '0' AS ahead")
	agExec(x.testEnv, "CREATE FUNCTION "+schema+".now() RETURNS timestamptz LANGUAGE sql STABLE AS $$SELECT pg_catalog.now()+ahead FROM "+schema+".shift$$")
	x.t.Cleanup(func() { x.DB.Exec("DROP SCHEMA " + schema + " CASCADE") })
	u, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		x.t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", q.Get("search_path")+","+schema+",pg_catalog")
	u.RawQuery = q.Encode()
	x.t.Setenv("DATABASE_URL", u.String())
	x.restart()
	x.mustPoll() // a restarted watcher makes its escrow accounts ready again
	return func(ahead string) {
		x.t.Helper()
		agExec(x.testEnv, "UPDATE "+schema+".shift SET ahead=$1::interval", ahead)
		agExec(x.testEnv, "UPDATE sessions SET expires=now()+interval '30 days'") // a step of that size signs everyone out; the pages are what is tested
	}
}

// recheckPanel is the section of Admin → Payments on the recheck, as an administrator reads it.
func (x *xmrEscrowEnv) recheckPanel(admin string) string {
	x.t.Helper()
	page := html.UnescapeString(x.page("/admin/payments", admin))
	_, after, found := strings.Cut(page, `id="xmr-deadline-recheck"`)
	if !found {
		x.t.Fatalf("no recheck section:\n%s", page)
	}
	block, _, _ := strings.Cut(after, "<h3")
	return block
}

// heldFromClause is what Admin → Payments and the host's reply add, while an outage's request the watcher wrote is pending,
// when its round holds only an order due from heldFrom (A-785).
func heldFromClause(heldFrom string) string {
	return "except an order overdue since before " + heldFrom + ", which can still be cancelled."
}

// wantHeldFromClause fails unless the panel and the host command's reply both say that the pending outage request holds
// only an order due from xmrGraceHeldFrom(start), and that cancels past their time otherwise wait (A-785).
func (x *xmrEscrowEnv) wantHeldFromClause(start time.Time) {
	x.t.Helper()
	_, admin := x.user("xheld_"+randomToken()[:6], "admin")
	clause := heldFromClause(utcUntil(xmrGraceHeldFrom(start)))
	if s := x.recheckPanel(admin); !containsAll(s, "Deadline recheck pending: an outage asked for it.", "a buyer's no-ship cancel past its time waits, "+clause) {
		x.t.Fatalf("the panel lacks %q:\n%s", clause, s)
	}
	out, err := RequestDeadlineRecheck(context.Background(), x.DB)
	if err != nil || !strings.HasPrefix(out, "Deadline recheck already pending (outage); nothing was changed.") || !strings.Contains(out, "a buyer's no-ship cancel past its time waits until then, "+clause) {
		x.t.Fatalf("the host's reply lacks %q: %q, %v", clause, out, err)
	}
}

// wantNoHeldFromClause fails unless the panel and the host command's reply say, of a pending request, that every no-ship
// cancel past its time waits (A-785).
func (x *xmrEscrowEnv) wantNoHeldFromClause() {
	x.t.Helper()
	_, admin := x.user("xheld_"+randomToken()[:6], "admin")
	if s := x.recheckPanel(admin); !strings.Contains(s, "a buyer's no-ship cancel past its time waits. ") || strings.Contains(s, "except an order overdue") {
		x.t.Fatalf("the panel of a request that holds every order:\n%s", s)
	}
	out, err := RequestDeadlineRecheck(context.Background(), x.DB)
	if err != nil || !strings.Contains(out, "already pending") || !strings.Contains(out, "a buyer's no-ship cancel past its time waits until then. ") || strings.Contains(out, "except an order overdue") {
		x.t.Fatalf("the host's reply to a request that holds every order: %q, %v", out, err)
	}
}

// gapFromStored is the start of the gap the watcher stored with its pending request (xmrOutageGapFromKey).
func (x *xmrEscrowEnv) gapFromStored() time.Time {
	x.t.Helper()
	v := x.settingsOf(xmrOutageGapFromKey)
	if len(v) != 1 {
		x.t.Fatalf("the gap's start: %q", v)
	}
	gap, err := time.Parse(time.RFC3339Nano, v[0])
	if err != nil {
		x.t.Fatal(err)
	}
	return gap
}

// shipBy is order's ship-by time as the database holds it.
func (x *xmrEscrowEnv) shipBy(order string) time.Time {
	x.t.Helper()
	var at time.Time
	if err := x.DB.QueryRow("SELECT ship_by FROM escrows WHERE order_id=$1", order).Scan(&at); err != nil {
		x.t.Fatal(err)
	}
	return at
}

// settingsOf lists the values of a setting (none or one).
func (x *xmrEscrowEnv) settingsOf(key string) []string {
	x.t.Helper()
	return agStrings(x.t, x.testEnv, "SELECT value FROM settings WHERE key=$1", key)
}

const (
	clocksApartLine  = "the application's clock and the database's clock differ by more than 5 minutes"
	daemonApartLine  = "the database's clock and the Monero daemon's newest block differ by more than MONERO_MAX_TIP_AGE (check the daemon's peers and block time)"
	deferredLogStart = "monero escrow recheck deferred: "
)

// PAY108-1: a forward step of the database's clock looks like a long outage, and a recheck on it writes a far-future end of
// the vendor's grace that no later recheck shortens. Compared with the application's clock it waits: no deadline moves,
// the request stays pending, holds stay in force, one log line and Admin → Payments say why, and once they agree the next
// pass runs it, at the true time.
func TestXMROutageRecheckWaitsForClocks(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	jump := x.simDBClock()
	adminID, admin := x.user("xclock_"+randomToken()[:6], "admin")
	physical := x.physical(x.vendor)
	order, _, _ := x.shippedXMR(physical)
	paid, paidSess := x.paidXMRPhysical(physical)
	preGap, preGapSess := x.paidXMRPhysical(physical)
	x.forceDeadline(order, "release_at=now()+interval '1 hour'")
	x.forceDeadline(paid, "ship_by=now()+interval '1 minute'") // not yet due on true time: only the jump makes it look overdue
	x.forceDeadline(preGap, "ship_by=now()-interval '10 days'")
	was := x.releaseAt(order)
	logs := captureLog(t)
	adminNotices := func() int {
		t.Helper()
		return agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE body LIKE 'A Monero escrow deadline recheck%' AND user_id=$1", adminID)
	}

	jump("30 days")
	for range 3 {
		x.mustPoll()
		if x.recheckKey() != xmrRecheckOutage {
			t.Fatalf("the request while the clocks disagree: %q", x.recheckKey())
		}
		if !x.releaseAt(order).Equal(was) || x.state(order) != stateShipped || len(x.movedNotes(order)) != 0 {
			t.Fatalf("a deadline moved while the clocks disagree: %s, was %s, order %s, notes %q", x.releaseAt(order), was, x.state(order), x.movedNotes(order))
		}
		if v := x.settingsOf(xmrCancelWaitKey); len(v) != 0 {
			t.Fatalf("a hold's end written while the clocks disagree: %q", v)
		}
		if rows := x.recheckRecords(); len(rows) != 0 {
			t.Fatalf("a recheck recorded while the clocks disagree: %q", rows)
		}
		// QA112-2: a deferred pass is no recheck, so it tells no administrator; the panel says why it waits.
		if n := adminNotices(); n != 0 {
			t.Fatalf("%d administrator notices while the clocks disagree", n)
		}
	}
	// One line for the change, whatever the passes, with the reason and nothing of the database's or the wallet's.
	text := logs.String()
	if strings.Count(text, deferredLogStart) != 1 || !strings.Contains(text, deferredLogStart+clocksApartLine) || strings.Contains(text, recheckLogPrefix) {
		t.Fatalf("the log while the clocks disagree:\n%s", text)
	}
	s := x.recheckPanel(admin)
	if !containsAll(s, "Deadline recheck pending: an outage asked for it.", clocksApartLine, "No deadline moves", "at the latest", "72 hours") || strings.Contains(s, "watcher's next pass runs it") {
		t.Fatalf("the panel while the clocks disagree:\n%s", s)
	}
	for _, private := range []string{"postgres", "127.0.0.1", "wallet", "30 days", "720"} {
		if strings.Contains(s, private) {
			t.Fatalf("the panel names %q:\n%s", private, s)
		}
	}
	// Holds stay in force: nothing completes on a passed deadline, and the buyer's no-ship cancel waits, page and action alike.
	x.wantCancelHeld(paid, paidSess, "cannot be cancelled yet", outageWhy)
	// PAY113-2: an order overdue before the gap was not held up by it. The gap's start and its ship_by were both stored before the
	// jump, so the comparison does not read the jumped clock: the page offers the cancel and the action requests the refund.
	x.wantCancelAccepted(preGap, preGapSess)
	// A-785: the panel and the host's reply say so, naming the earliest ship_by held, from the stored start.
	x.wantHeldFromClause(x.gapFromStored())

	jump("0")
	x.mustPoll()
	if x.recheckKey() != "" || len(x.settingsOf(xmrRecheckWaitKey)) != 0 {
		t.Fatalf("after agreement: request %q, wait %q", x.recheckKey(), x.settingsOf(xmrRecheckWaitKey))
	}
	if d := x.releaseAt(order).Sub(x.dbNow()); d < 71*time.Hour || d > 72*time.Hour {
		t.Fatalf("after agreement the deadline is %s away, want 72 hours", d)
	}
	if d := x.cancelWaitUntil().Sub(x.dbNow()); d < 71*time.Hour || d > 72*time.Hour {
		t.Fatalf("after agreement the vendor's grace ends %s away: a forward step must not write a far-future end", d)
	}
	if s := x.recheckPanel(admin); !strings.Contains(s, "No deadline recheck is pending.") || strings.Contains(s, clocksApartLine) || strings.Contains(s, "still apart") {
		t.Fatalf("the panel after agreement:\n%s", s)
	}
	x.wantRecheckRecorded(logs, "outage", 1, order)
	// QA112-2, QA112-4: exactly one notice after agreement, reading as any recheck's, with nothing of the clocks.
	if n := adminNotices(); n != 1 {
		t.Fatalf("%d administrator notices after agreement, want 1", n)
	}
	if body := agStr(x.testEnv, "SELECT body FROM notifications WHERE body LIKE 'A Monero escrow deadline recheck%' AND user_id=$1", adminID); body != "A Monero escrow deadline recheck ran after an outage. Admin > Payments shows how many deadlines it moved and any hold on buyers' no-ship cancels." {
		t.Fatalf("the notice of a recheck with the clocks agreeing: %q", body)
	}

	// The stated tolerance, either way, on an operator's request as on a found gap: past 5 minutes it waits, within them it runs.
	for _, c := range []struct {
		ahead string
		waits bool
	}{{"6 minutes", true}, {"-6 minutes", true}, {"-4 minutes", false}, {"4 minutes", false}} {
		jump("0")
		if _, err := RequestDeadlineRecheck(context.Background(), x.DB); err != nil {
			t.Fatal(err)
		}
		jump(c.ahead)
		x.mustPoll()
		if pending := x.recheckKey() == xmrRecheckOutage; pending != c.waits {
			t.Fatalf("the database's clock %s from the application's: pending %v, want %v", c.ahead, pending, c.waits)
		}
		jump("0")
		x.mustPoll()
		if x.recheckKey() != "" {
			t.Fatalf("after %s, agreement left the request %q", c.ahead, x.recheckKey())
		}
	}

	// A restore's recheck is unchanged: with the clocks apart it runs at once, and it says a restore.
	x.mustPoll()
	jump("30 days")
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", xmrReleaseRecheckKey)
	x.mustPoll()
	if x.recheckKey() != "" || !strings.Contains(x.recheckPanel(admin), "after a restore from backup") {
		t.Fatalf("a restore's recheck with the clocks apart: request %q\n%s", x.recheckKey(), x.recheckPanel(admin))
	}
}

// PAY108-1: a genuine long outage has the same gap as a step of the clock, and with the clocks agreeing it moves deadlines
// as it always did: a disagreement defers, and an agreement never reads as no outage.
func TestXMROutageMonthLongWithClocksAgreeing(t *testing.T) {
	x := newXMREscrowEnvWith(t, func(f *fakeXMREscrow) { f.nettype, f.peers = "stagenet", 8 })
	x.mustPoll()
	logs := captureLog(t)
	order, _ := x.deliveredXMR(x.product)
	x.forceDeadline(order, "release_at=now()-interval '20 days'")
	x.setLastDeadlineStep("30 days")
	x.mustPoll()
	if d := x.releaseAt(order).Sub(x.dbNow()); d < 71*time.Hour || d > 72*time.Hour || x.recheckKey() != "" || x.state(order) != stateDelivered {
		t.Fatalf("a month's outage: %s away, request %q, order %s", d, x.recheckKey(), x.state(order))
	}
	x.wantRecheckRecorded(logs, "outage", 1, order)
	if strings.Contains(logs.String(), deferredLogStart) {
		t.Fatalf("a deferral with the clocks agreeing:\n%s", logs.String())
	}
}

// PAY108-1: the daemon's newest block is the third clock, for a step of the host that moves the application and the database
// together. A daemon that says it is synced and has an old tip defers, and the tip's catching up ends that. A daemon that
// cannot say, whether unreachable, not synced or hung, cannot stop the recheck: only the two clocks that could be read count.
func TestXMROutageRecheckDaemonTip(t *testing.T) {
	x := newXMREscrowEnvWith(t, func(f *fakeXMREscrow) { f.nettype, f.peers = "stagenet", 8 })
	x.mustPoll()
	_, admin := x.user("xclockd_"+randomToken()[:6], "admin")
	order, _ := x.deliveredXMR(x.product)
	near := func() {
		t.Helper()
		x.forceDeadline(order, "release_at=now()+interval '1 hour'")
		x.setLastDeadlineStep("2 hours")
	}
	recheck := func() {
		t.Helper()
		x.A.xmrNoteDeadlineStepEarly(context.Background())
		if err := x.A.xmrRecheckReleaseDeadlines(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	moved := func(name string) {
		t.Helper()
		if d := x.releaseAt(order).Sub(x.dbNow()); d < 71*time.Hour || x.recheckKey() != "" {
			t.Fatalf("%s: the deadline is %s away, request %q", name, d, x.recheckKey())
		}
	}

	t.Run("a synced daemon's old tip waits, and its catching up ends the wait", func(t *testing.T) {
		logs := captureLog(t)
		near()
		was := x.releaseAt(order)
		x.fake.edit(func(f *fakeXMREscrow) { f.tipAge = 30 * 24 * time.Hour })
		recheck()
		if !x.releaseAt(order).Equal(was) || x.recheckKey() != xmrRecheckOutage {
			t.Fatalf("with an old tip: %s, was %s, request %q", x.releaseAt(order), was, x.recheckKey())
		}
		if text := logs.String(); !strings.Contains(text, deferredLogStart+daemonApartLine) {
			t.Fatalf("the log with an old tip:\n%s", text)
		}
		if s := x.recheckPanel(admin); !strings.Contains(s, daemonApartLine) {
			t.Fatalf("the panel with an old tip:\n%s", s)
		}
		x.fake.edit(func(f *fakeXMREscrow) { f.tipAge = 0 })
		x.mustPoll()
		moved("the tip caught up")
	})
	for _, c := range []struct {
		name       string
		set, unset func(f *fakeXMREscrow)
	}{
		{"not synced", func(f *fakeXMREscrow) { f.unsynced, f.tipAge = true, 30*24*time.Hour }, func(f *fakeXMREscrow) { f.unsynced, f.tipAge = false, 0 }},
		{"tip unreadable", func(f *fakeXMREscrow) { f.tipErr = true }, func(f *fakeXMREscrow) { f.tipErr = false }},
		{"unrestricted, no peers", func(f *fakeXMREscrow) { f.peers, f.tipAge = 0, 30*24*time.Hour }, func(f *fakeXMREscrow) { f.peers, f.tipAge = 8, 0 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			near()
			x.fake.edit(c.set)
			defer x.fake.edit(c.unset)
			recheck()
			moved(c.name)
		})
	}
	// QA112-1: a restricted RPC, the documented production form, cannot show its peers (notSynced does not read the count), so a
	// synced one with an old tip is the disagreement above, unlike an unrestricted daemon with no peers: it waits, bounded by
	// TestXMROutageRecheckWaitBound, and a fresh tip ends the wait.
	t.Run("restricted, no peers, old tip waits", func(t *testing.T) {
		near()
		was := x.releaseAt(order)
		x.fake.edit(func(f *fakeXMREscrow) { f.restricted, f.peers, f.tipAge = true, 0, 30*24*time.Hour })
		defer x.fake.edit(func(f *fakeXMREscrow) { f.restricted, f.peers, f.tipAge = false, 8, 0 })
		recheck()
		if !x.releaseAt(order).Equal(was) || x.recheckKey() != xmrRecheckOutage {
			t.Fatalf("restricted with an old tip: %s, was %s, request %q", x.releaseAt(order), was, x.recheckKey())
		}
		if s := x.recheckPanel(admin); !strings.Contains(s, daemonApartLine) {
			t.Fatalf("the panel, restricted with an old tip:\n%s", s)
		}
		x.fake.edit(func(f *fakeXMREscrow) { f.tipAge = 0 })
		recheck()
		moved("restricted with a fresh tip")
	})
	t.Run("unreachable", func(t *testing.T) {
		near()
		x.fake.fault("get_info", fakeXMRDrop)
		recheck()
		moved("unreachable")
	})
	t.Run("not ready", func(t *testing.T) {
		near()
		x.A.escrowXMR.ready.Store(false)
		defer x.A.escrowXMR.ready.Store(true)
		x.mustPoll()
		moved("not ready")
	})
	t.Run("hung", func(t *testing.T) {
		was := xmrClockCheckBudget
		xmrClockCheckBudget = 200 * time.Millisecond
		defer func() { xmrClockCheckBudget = was }()
		release := make(chan struct{})
		x.fake.hook("get_info", func() { <-release })
		defer func() { x.fake.hook("get_info", nil); close(release) }()
		near()
		started := time.Now()
		recheck()
		if took := time.Since(started); took > 10*time.Second {
			t.Fatalf("the recheck waited %s on a hung daemon", took)
		}
		moved("hung")
	})
	t.Run("a restore's overwrite during the comparison", func(t *testing.T) {
		near()
		x.fake.edit(func(f *fakeXMREscrow) { f.tipAge = 30 * 24 * time.Hour })
		defer x.fake.edit(func(f *fakeXMREscrow) { f.tipAge = 0 })
		x.fake.hook("get_info", func() {
			agExec(x.testEnv, "UPDATE settings SET value='true' WHERE key=$1", xmrReleaseRecheckKey)
		})
		defer x.fake.hook("get_info", nil)
		recheck()
		moved("a restore's overwrite")
		if s := x.recheckPanel(admin); !strings.Contains(s, "after a restore from backup") {
			t.Fatalf("the panel after a restore's overwrite:\n%s", s)
		}
	})
}

// PAY108-1: the wait is bounded. It ends at the latest once 72 hours have passed on both clocks, so a market whose clocks stay
// apart is not held for good: the recheck then runs as it would have, says it ran with them apart, and clears the wait.
// One clock's jump alone does not end it, and a clock stepped back starts the count again.
func TestXMROutageRecheckWaitBound(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	jump := x.simDBClock()
	adminID, admin := x.user("xclockb_"+randomToken()[:6], "admin")
	order, _, _ := x.shippedXMR(x.physical(x.vendor))
	x.forceDeadline(order, "release_at=now()+interval '1 hour'")
	was := x.releaseAt(order)
	logs := captureLog(t)
	jump("30 days")
	x.mustPoll()
	if x.recheckKey() != xmrRecheckOutage || !x.releaseAt(order).Equal(was) {
		t.Fatalf("the first wait: request %q, deadline %s", x.recheckKey(), x.releaseAt(order))
	}
	set := func(db, app time.Time) {
		t.Helper()
		raw, err := json.Marshal(xmrRecheckWait{Reason: xmrClocksDatabase, DB: db, App: app})
		if err != nil {
			t.Fatal(err)
		}
		agExec(x.testEnv, "UPDATE settings SET value=$2 WHERE key=$1", xmrRecheckWaitKey, string(raw))
	}
	dbNow, appNow := x.dbNow(), time.Now()
	past := xmrRecheckWaitMax + time.Hour
	for name, c := range map[string][2]time.Time{
		"only the database's clock passed the bound":    {dbNow.Add(-past), appNow},
		"only the application's clock passed the bound": {dbNow, appNow.Add(-past)},
	} {
		set(c[0], c[1])
		x.mustPoll()
		if x.recheckKey() != xmrRecheckOutage || !x.releaseAt(order).Equal(was) {
			t.Fatalf("%s: request %q, deadline %s", name, x.recheckKey(), x.releaseAt(order))
		}
	}
	// A clock stepped back: its elapsed time means nothing, so the count starts again rather than never ending.
	set(dbNow.Add(time.Hour), appNow.Add(-past))
	x.mustPoll()
	var again xmrRecheckWait
	if v := x.settingsOf(xmrRecheckWaitKey); len(v) != 1 || json.Unmarshal([]byte(v[0]), &again) != nil || again.DB.After(x.dbNow()) || time.Since(again.App) > time.Minute {
		t.Fatalf("the count after a clock stepped back: %q", v)
	}

	set(dbNow.Add(-past), appNow.Add(-past))
	x.mustPoll()
	if x.recheckKey() != "" || len(x.settingsOf(xmrRecheckWaitKey)) != 0 || x.releaseAt(order).Sub(x.dbNow()) < 71*time.Hour {
		t.Fatalf("past the bound: request %q, wait %q, deadline %s", x.recheckKey(), x.settingsOf(xmrRecheckWaitKey), x.releaseAt(order))
	}
	if !strings.Contains(logs.String(), recheckLogPrefix+": cause=outage moved=1 clocks_apart=true\n") {
		t.Fatalf("the log past the bound:\n%s", logs.String())
	}
	if rows := x.recheckRecords(); len(rows) != 1 || !strings.Contains(rows[0], "clocks still apart") {
		t.Fatalf("the audit row past the bound: %q", rows)
	}
	// UI114-2: both dangers of a clock left ahead are pinned, an early fall due and an early payment.
	if s := x.recheckPanel(admin); !containsAll(s, "No deadline recheck is pending.", "It ran with the clocks still apart", "While the database's clock stays ahead, deadlines also fall due early by true time", "and a release or refund can go early: correct the clock now.") {
		t.Fatalf("the panel past the bound:\n%s", s)
	}
	// QA112-4: the administrators' notice says so too, one fixed clause, with no difference or date.
	if body := agStr(x.testEnv, "SELECT body FROM notifications WHERE body LIKE 'A Monero escrow deadline recheck%' AND user_id=$1", adminID); body != "A Monero escrow deadline recheck ran after an outage, with the clocks still apart. Admin > Payments shows how many deadlines it moved and any hold on buyers' no-ship cancels." {
		t.Fatalf("the notice past the bound: %q", body)
	}
}

// OPS105-N1: a connection lost after the command's first check is reported by class: pgconn's own text names the URL's user
// and database.
func TestRestoreGateRecheckRequestErrorIsClassOnly(t *testing.T) {
	err := recheckRequestFailed(fmt.Errorf("exec: %w", &pgconn.ConnectError{Config: &pgconn.Config{User: "dsn_user_name", Database: "dsn_db_name", Host: "dsn.host.name"}}))
	if !strings.Contains(err.Error(), "database statement failed (") {
		t.Fatalf("connect error: %v", err)
	}
	for _, private := range []string{"dsn_user_name", "dsn_db_name", "dsn.host.name"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("the error names %q: %v", private, err)
		}
	}
}
