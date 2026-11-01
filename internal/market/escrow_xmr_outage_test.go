package market

import (
	"context"
	"html"
	"strings"
	"testing"
	"time"
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

// A-730 (owner decision): the market being down consumes no buyer's deadline. A gap of over an hour since the last
// deadline step moves every deadline passed or within 72 hours as a restore's recheck does, before any of them could
// complete, and says an outage moved it, on the notice, the note and the order page. A later deadline is untouched. A
// vendor lost the time too, so the recheck's vendor grace applies, and says why.
func TestXMROutageMovesNearDeadlines(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	passed, passedSess, _ := x.shippedXMR(physical)
	near, _, _ := x.shippedXMR(physical)
	far, _, _ := x.shippedXMR(physical)
	window, windowSess := x.deliveredXMR(x.product)
	paid, paidSess := x.paidXMRPhysical(physical)
	x.forceDeadline(passed, "release_at=now()-interval '2 days'")
	x.forceDeadline(near, "release_at=now()+interval '24 hours'")
	x.forceDeadline(far, "release_at=now()+interval '5 days'")
	x.forceDeadline(window, "release_at=now()-interval '1 hour'")
	x.forceDeadline(paid, "ship_by=now()-interval '1 minute'")
	farAt := x.releaseAt(far)
	x.setLastDeadlineStep("2 hours")

	x.mustPoll()
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
	if p := x.panel(passed, passedSess); strings.Contains(p, "restored") || !strings.Contains(p, utcUntil(x.releaseAt(passed))) {
		t.Fatalf("the panel after an outage's recheck:\n%s", p)
	}
	if p := x.panel(window, windowSess); strings.Contains(p, "restored") {
		t.Fatalf("the digital panel after an outage's recheck:\n%s", p)
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

	// Once: the next pass finds no gap, and no second notice or reminder follows.
	x.mustPoll()
	notices := x.noticesOf(x.partyOf(passed, roleBuyer), passed)
	if countContaining(notices, "an outage") != 1 || anyContains(notices, "Reminder") {
		t.Fatalf("after another pass: %q", notices)
	}
}

// A-730: escrow switched off for over an hour leaves the same gap: the first pass after it is switched on again, however
// it reaches the deadline step, takes the same path. Here the daemon check fails, so that pass does nothing else.
func TestXMROutageAfterEscrowOff(t *testing.T) {
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
func TestXMROutageRecheckRetries(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, sess, _ := x.shippedXMR(x.physical(x.vendor))
	window, windowSess := x.deliveredXMR(x.product)
	x.forceDeadline(order, "release_at=now()-interval '2 days'")
	x.forceDeadline(window, "release_at=now()-interval '2 days'")
	passedAt := x.releaseAt(order)
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
