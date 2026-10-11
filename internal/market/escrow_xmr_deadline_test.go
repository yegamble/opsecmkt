package market

import (
	"context"
	"html"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Escrow Phase 3, slice X-e (WR-escrow A-248, P3-R7 with the Phase 2 rules P2-R3 to P2-R5 and P2-R9): the clocks of a
// physical Monero escrow order, run by the Monero escrow watcher with ESCROW_BTC off; dispute decisions on Monero
// escrow orders. DB integration tests against the simulated wallet-rpc of escrow_xmr_fake_test.go (simulated RPC, not
// chain evidence).

// P3-R7: each watcher runs the deadline step for its own coin's orders, so with ESCROW_BTC off and ESCROW_XMR on the
// Monero escrow watcher completes a shipped order with no dispute at its release deadline (release_at <= now()) and
// makes no RPC to do it; the release requested at shipment then becomes buildable. Before the deadline, under the
// restore gate, with a dispute row, and on a pooled order nothing moves. The page shows the deadline with a countdown
// computed when it loaded, the notices name the coin, the order and the deadline in UTC.
func TestXMREscrowDeadlineWithBTCOff(t *testing.T) {
	x := newXMREscrowEnv(t)
	if x.A.escrow.Load() != nil || x.A.escrowConfigured || !x.A.xmrEscrowReady() {
		t.Fatal("this test needs ESCROW_BTC off and Monero escrow ready")
	}
	x.mustPoll()
	physical := x.physical(x.vendor)
	order, buyerSess, claim := x.shippedXMR(physical)
	deadline := x.deadlineOf(order)
	for _, sess := range []string{buyerSess, x.vendorSess} {
		panel := x.panel(order, sess)
		if !containsAll(panel, "Release deadline", deadline, "About 13 days", "computed when this page loaded", "Opening a dispute cancels the deadline") {
			t.Fatalf("the shipped order's panel lacks the deadline, its countdown or the dispute line:\n%s", panel)
		}
		noBitcoinWords(t, "the shipped order's panel", panel)
	}
	if page := html.UnescapeString(x.page("/order?id="+order, buyerSess)); !containsAll(page, "you need do nothing", "Opening a dispute cancels the deadline of "+deadline) {
		t.Fatalf("the buyer's page does not cover the deadline:\n%s", page)
	}
	x.mustPoll()
	if x.state(order) != stateShipped {
		t.Fatalf("before the deadline: %s", x.state(order))
	}
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	deadline = x.deadlineOf(order)
	if page := x.panel(order, buyerSess); !strings.Contains(page, "the release is due") {
		t.Fatalf("a passed deadline:\n%s", page)
	}
	// Under the restore gate nothing moves.
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	x.mustPoll()
	if x.state(order) != stateShipped {
		t.Fatalf("released under the restore gate: %s", x.state(order))
	}
	agExec(x.testEnv, "DELETE FROM settings WHERE key='payments_recovery_required'")
	calls := x.fake.count()
	if err := x.A.xmrDeadlines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if x.fake.count() != calls {
		t.Fatal("the deadline step made an RPC")
	}
	if x.state(order) != stateCompleted || !anyContains(x.events(order), "shipped>completed system auto-release deadline passed ("+deadline+")", "Monero escrow") {
		t.Fatalf("past the deadline: %s %q", x.state(order), x.events(order))
	}
	noBitcoinWords(t, "the order history", strings.Join(x.events(order), "\n"))
	for _, uid := range []string{x.partyOf(order, roleBuyer), x.vendor} {
		got := x.noticesOf(uid, order)
		if !anyContains(got, "Order "+shortID(order), "Completed", "auto-release deadline passed ("+deadline+")") {
			t.Fatalf("release notice to %s: %q", uid, got)
		}
		noBitcoinWords(t, "the notices", strings.Join(got, "\n"))
	}
	if got := x.xmrSettlements(order); len(got) != 1 || got[0] != "release requested "+claim || !settlementFits("release", x.state(order), "") {
		t.Fatalf("the release after the deadline: %v", got)
	}

	// The boundary: a deadline equal to now() is due. One transaction sets release_at to its own now() and runs the check.
	order2, _, _ := x.shippedXMR(physical)
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"ALTER TABLE escrows DISABLE TRIGGER escrows_deadlines_frozen", "UPDATE escrows SET release_at=now() WHERE order_id='" + order2 + "'",
		"ALTER TABLE escrows ENABLE TRIGGER escrows_deadlines_frozen"} {
		if _, err = tx.Exec(q); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	released, err := x.A.xmrReleaseIfDue(context.Background(), tx, order2)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !released || x.state(order2) != stateCompleted {
		t.Fatalf("a deadline at now() exactly: released %v, order %s", released, x.state(order2))
	}
	// A dispute row stops the release even on an order still shipped.
	order3, _, _ := x.shippedXMR(physical)
	x.forceDeadline(order3, "release_at=now()-interval '1 hour'")
	agExec(x.testEnv, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'A dispute row written beside a shipped order.')", randomToken(), order3)
	x.mustPoll()
	if x.state(order3) != stateShipped || anyContains(x.events(order3), "auto-release") {
		t.Fatalf("an order with a dispute row was released: %s", x.state(order3))
	}
	// A pooled shipped Monero order is never touched.
	pooled := x.testEnv.order(x.buyer, physical, "XMR", stateShipped)
	x.mustPoll()
	if x.state(pooled) != stateShipped {
		t.Fatalf("a pooled shipped order moved: %s", x.state(pooled))
	}
}

// QA-Xe F2 (A-416): with Monero escrow configured but not ready (its node unreachable, or refused until a restart), a
// pass touches no wallet but still applies the deadlines, which make no RPC, as Bitcoin escrow does when its node check
// fails: a shipped order past its deadline is completed, not left neither released nor disputable.
func TestXMREscrowDeadlineWhileNotReady(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.shippedXMR(x.physical(x.vendor))
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	x.A.escrowXMR.ready.Store(false)
	calls := x.fake.count()
	x.mustPoll()
	if x.state(order) != stateCompleted || !anyContains(x.events(order), "auto-release deadline passed") {
		t.Fatalf("a due order while Monero escrow is not ready: %s", x.state(order))
	}
	if x.fake.count() != calls {
		t.Fatal("a pass made an RPC while Monero escrow was not ready")
	}
}

// P2-R3, P2-R4 for Monero: a dispute and the Monero watcher's deadline release lock the order row, so exactly one takes
// effect. A dispute that reaches the lock first wins even if the deadline passes while it waits; a release that
// reaches it first completes the order, and the dispute queued behind it is refused.
func TestXMREscrowDeadlineDisputeRace(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	ctx := context.Background()
	lockOrder := func(order string) interface{ Commit() error } {
		tx, err := x.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec("SELECT 1 FROM orders WHERE id=$1 FOR UPDATE", order); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	t.Run("the dispute first", func(t *testing.T) {
		order, sess, _ := x.shippedXMR(physical)
		x.forceDeadline(order, "release_at=clock_timestamp()+interval '2 seconds'")
		tx := lockOrder(order)
		var code int
		var body string
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); code, body = x.xmrDispute(order, sess) }() // its transaction starts before the deadline
		x.waitForLockWaits(1)
		if !waitUntil(t, 5*time.Second, func() bool {
			return agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND release_at<clock_timestamp()", order) == 1
		}) {
			t.Fatal("the deadline did not pass")
		}
		var relErr error
		go func() { defer wg.Done(); relErr = x.A.xmrReleaseDue(ctx) }() // after the deadline, behind the dispute
		x.waitForLockWaits(2)
		tx.Commit()
		wg.Wait()
		if relErr != nil {
			t.Fatal(relErr)
		}
		if code != 303 || x.state(order) != stateDisputed || anyContains(x.events(order), "auto-release") {
			t.Fatalf("dispute %d %s; order %s", code, body, x.state(order))
		}
		x.mustPoll()
		if x.state(order) != stateDisputed {
			t.Fatalf("a disputed order moved at its deadline: %s", x.state(order))
		}
		if page := x.panel(order, sess); strings.Contains(page, " h left") || !strings.Contains(page, "no longer applies") {
			t.Fatalf("the disputed order's panel:\n%s", page)
		}
	})
	t.Run("the release first", func(t *testing.T) {
		order, sess, _ := x.shippedXMR(physical)
		x.forceDeadline(order, "release_at=now()-interval '1 second'")
		tx := lockOrder(order)
		var relErr error
		var code int
		var body string
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); relErr = x.A.xmrReleaseDue(ctx) }()
		x.waitForLockWaits(1)
		go func() { defer wg.Done(); code, body = x.xmrDispute(order, sess) }()
		x.waitForLockWaits(2)
		tx.Commit()
		wg.Wait()
		if relErr != nil {
			t.Fatal(relErr)
		}
		if code != 409 || x.state(order) != stateCompleted || agInt(x.testEnv, "SELECT count(*) FROM disputes WHERE order_id=$1", order) != 0 {
			t.Fatalf("dispute %d %s; order %s", code, body, x.state(order))
		}
	})
}

// P2-R3, P2-R9 for Monero: 72 hours before the deadline the buyer is reminded once, with the coin, the deadline in UTC
// and the order's short ID, and the vendor told the buyer's deadline; reminded_at is claimed by compare-and-set, so two
// reminder steps racing send one reminder. Never before the 72 hours, and never on a disputed order.
func TestXMREscrowReminderOnce(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	ctx := context.Background()
	order, _, _ := x.shippedXMR(physical)
	buyer := x.partyOf(order, roleBuyer)
	x.mustPoll()
	if agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND reminded_at IS NOT NULL", order) != 0 {
		t.Fatal("reminded 14 days before the deadline")
	}
	x.forceDeadline(order, "release_at=now()+interval '71 hours'")
	deadline := x.deadlineOf(order)
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("SELECT 1 FROM escrows WHERE order_id=$1 FOR UPDATE", order); err != nil {
		t.Fatal(err)
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = x.A.xmrRemindDeadlines(ctx) }()
	}
	x.waitForLockWaits(2)
	tx.Commit()
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	x.mustPoll()
	x.mustPoll()
	got, vendor := x.noticesOf(buyer, order), x.noticesOf(x.vendor, order)
	if n := countContains(got, "Reminder", deadline); n != 1 {
		t.Fatalf("%d reminders to the buyer, want 1 naming %s: %q", n, deadline, got)
	}
	if !anyContains(got, "Order "+shortID(order), "until "+deadline, "Confirm receipt", "open a dispute", "Monero escrow") {
		t.Fatalf("the reminder does not name the order, the coin and the deadline: %q", got)
	}
	if n := countContains(vendor, "the buyer has until "+deadline); n != 1 {
		t.Fatalf("%d deadline notices to the vendor, want 1: %q", n, vendor)
	}
	noBitcoinWords(t, "the reminders", strings.Join(append(got, vendor...), "\n"))
	order2, sess2, _ := x.shippedXMR(physical)
	if code, body := x.xmrDispute(order2, sess2); code != 303 {
		t.Fatalf("dispute: %d %s", code, body)
	}
	x.forceDeadline(order2, "release_at=now()+interval '10 hours'")
	x.mustPoll()
	if anyContains(x.noticesOf(x.partyOf(order2, roleBuyer), order2), "Reminder") {
		t.Fatal("a disputed order was reminded")
	}
}

// P2-R9 for Monero: /orders shows a banner, to the buyer and the vendor, for a shipped Monero escrow order with no
// dispute whose release deadline is less than 72 hours away, with the short ID and the deadline in UTC; none further away.
func TestXMREscrowOrdersBannerUnder72h(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	soon, buyerSess, _ := x.shippedXMR(physical)
	later, laterSess, _ := x.shippedXMR(physical)
	x.forceDeadline(soon, "release_at=now()+interval '71 hours'")
	deadline := x.deadlineOf(soon)
	banner := func(sess string) string {
		body := html.UnescapeString(x.page("/orders", sess))
		i := strings.Index(body, `class="alert deadline-banner"`)
		if i < 0 {
			return ""
		}
		return body[i : i+strings.Index(body[i:], "</p>")]
	}
	for _, sess := range []string{buyerSess, x.vendorSess} {
		b := banner(sess)
		if !containsAll(b, "Order "+shortID(soon), deadline, "confirm receipt") {
			t.Fatalf("the banner:\n%s", b)
		}
		noBitcoinWords(t, "the banner", b)
	}
	if b := banner(laterSess); b != "" || strings.Contains(x.page("/orders", x.vendorSess), shortID(later)+"</a>:") {
		t.Fatalf("a banner for a deadline 14 days away: %s", b)
	}
}

// P2-R5 for Monero: a paid physical Monero escrow order gets ship_by = paid time + its dispatch window + 48 h. Before it
// the buyer's cancel is refused with the time stated, and the page says when it opens; from it the buyer cancels, and
// the cancel requests the refund to the refund address (escrowSettleTerminal). A paid digital order offers no buyer
// cancel. The panel states the window before and after it opens.
func TestXMREscrowNoShipCancel(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	before := time.Now()
	order, buyerSess := x.paidXMRPhysical(x.physical(x.vendor))
	var shipBy time.Time
	if err := x.DB.QueryRow("SELECT ship_by FROM escrows WHERE order_id=$1", order).Scan(&shipBy); err != nil {
		t.Fatalf("no ship_by after payment: %v", err)
	}
	if want := before.Add(3*24*time.Hour + 48*time.Hour); shipBy.Before(want.Add(-time.Minute)) || shipBy.After(want.Add(time.Minute)) {
		t.Fatalf("ship_by %s, want about %s", shipBy, want)
	}
	from := utcFrom(shipBy)
	page := html.UnescapeString(x.page("/order?id="+order, buyerSess))
	if !strings.Contains(page, "You can cancel from "+from+" if the vendor has not shipped") || strings.Contains(page, `action="/orders/cancel"`) {
		t.Fatalf("the buyer's page before the window opens:\n%s", page)
	}
	if panel := x.panel(order, x.vendorSess); !strings.Contains(panel, "Ship by "+from) {
		t.Fatalf("the vendor's panel does not state the ship-by time:\n%s", panel)
	}
	if code, body := x.xmrBuyerCancel(order, buyerSess); code != 409 || !strings.Contains(body, from) {
		t.Fatalf("a cancel before ship_by: %d %s", code, body)
	}
	if x.state(order) != statePaid || len(x.xmrSettlements(order)) != 0 {
		t.Fatal("the refused cancel changed the order")
	}
	x.forceDeadline(order, "ship_by=now()-interval '1 minute'")
	page = html.UnescapeString(x.page("/order?id="+order, buyerSess))
	if !containsAll(page, `action="/orders/cancel"`, "Cancelling refunds the 0.5 fakechain XMR", `name="amount_seen" value="500000000000"`) {
		t.Fatalf("the buyer's page once the window opened:\n%s", page)
	}
	for _, sess := range []string{buyerSess, x.vendorSess} {
		if panel := x.panel(order, sess); !strings.Contains(panel, "the buyer may now cancel it") {
			t.Fatalf("the panel once the window opened:\n%s", panel)
		}
	}
	if code, body := x.xmrBuyerCancel(order, buyerSess); code != 303 {
		t.Fatalf("the cancel after ship_by: %d %s", code, body)
	}
	if got := x.xmrSettlements(order); x.state(order) != stateCancelled || len(got) != 1 || got[0] != "refund requested "+testXMRAddr("refund "+order, true) {
		t.Fatalf("after the cancel: order %s, settlements %v", x.state(order), got)
	}
	// A digital order left paid: no ship_by, no buyer cancel.
	digital, digitalSess := x.undeliveredXMR()
	if code, _ := x.xmrBuyerCancel(digital, digitalSess); code != 409 {
		t.Fatalf("a buyer cancel of a paid digital Monero escrow order: %d", code)
	}
	if strings.Contains(x.page("/order?id="+digital, digitalSess), `action="/orders/cancel"`) {
		t.Fatal("a paid digital order offers the buyer a cancel form")
	}
}

// P2-R3 for Monero (owner decision 2026-10-03): a dispute filed after the release deadline is refused (the release is
// due) by either party and changes nothing; before it the dispute is opened and the other party told the deadline no
// longer applies. While a restore's recheck of Monero deadlines is pending, a passed deadline is not yet due (A-393).
func TestXMREscrowLateDisputeRefused(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	order, buyerSess, _ := x.shippedXMR(physical)
	order2, sess2, _ := x.shippedXMR(physical)
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	deadline := x.deadlineOf(order)
	for _, sess := range []string{buyerSess, x.vendorSess} {
		if code, body := x.xmrDispute(order, sess); code != 409 || !containsAll(body, "release to the vendor is due", deadline) {
			t.Fatalf("a dispute after the deadline: %d %s", code, body)
		}
	}
	if x.state(order) != stateShipped || agInt(x.testEnv, "SELECT count(*) FROM disputes WHERE order_id=$1", order) != 0 {
		t.Fatal("a refused dispute changed the order")
	}
	deadline2 := x.deadlineOf(order2)
	if code, body := x.xmrDispute(order2, sess2); code != 303 {
		t.Fatalf("a dispute before the deadline: %d %s", code, body)
	}
	if got := x.noticesOf(x.vendor, order2); !anyContains(got, "Order "+shortID(order2), "Disputed", "release deadline of "+deadline2+" no longer applies") {
		t.Fatalf("the vendor's dispute notice: %q", got)
	}
	// A pending Monero recheck: not yet due. The Bitcoin recheck's setting is not the Monero one.
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", releaseRecheckKey)
	if code, _ := x.xmrDispute(order, buyerSess); code != 409 {
		t.Fatalf("a pending Bitcoin recheck let a late Monero dispute through: %d", code)
	}
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", xmrReleaseRecheckKey)
	if code, body := x.xmrDispute(order, buyerSess); code != 303 {
		t.Fatalf("a dispute while the Monero recheck is pending: %d %s", code, body)
	}
}

// P2-R10's restore recheck for Monero: after a restore from backup (scripts/restore.sh sets escrow_xmr_release_recheck
// with the restore gate), nothing moves while the gate is set; the first pass after it is cleared moves every passed
// deadline of a shipped Monero escrow order with no dispute to now + 72 h, tells the buyer, consumes the request, and
// only then releases what is due. A deadline still ahead is left alone.
func TestXMREscrowRestoreExtendsDeadline(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	passed, _, _ := x.shippedXMR(physical)
	ahead, _, _ := x.shippedXMR(physical)
	x.forceDeadline(passed, "release_at=now()-interval '2 days'")
	x.forceDeadline(ahead, "release_at=now()+interval '5 days'")
	at := func(order string) time.Time {
		var v time.Time
		x.DB.QueryRow("SELECT release_at FROM escrows WHERE order_id=$1", order).Scan(&v)
		return v
	}
	aheadAt, passedAt := at(ahead), at(passed)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", xmrReleaseRecheckKey)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	x.mustPoll()
	if !at(passed).Equal(passedAt) || x.state(passed) != stateShipped || agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key=$1", xmrReleaseRecheckKey) != 1 {
		t.Fatal("the Monero deadline step ran under the restore gate")
	}
	agExec(x.testEnv, "DELETE FROM settings WHERE key='payments_recovery_required'")
	start := time.Now()
	x.mustPoll()
	got := at(passed)
	if got.Before(start.Add(72*time.Hour-time.Minute)) || got.After(time.Now().Add(72*time.Hour+time.Minute)) || x.state(passed) != stateShipped {
		t.Fatalf("release_at %s, order %s; want now + 72 h, shipped", got, x.state(passed))
	}
	if agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND release_recheck", passed) != 1 || !at(ahead).Equal(aheadAt) ||
		agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key=$1", xmrReleaseRecheckKey) != 0 {
		t.Fatal("the recheck did not set release_recheck, moved a deadline ahead, or was not consumed")
	}
	notices := x.noticesOf(x.partyOf(passed, roleBuyer), passed)
	if !anyContains(notices, "restored its records from a backup", utcUntil(got), "Confirm receipt", "Monero escrow") {
		t.Fatalf("the buyer's restore notice: %q", notices)
	}
	noBitcoinWords(t, "the restore notice", strings.Join(notices, "\n"))
	// The moved deadline is 72 hours away, inside the reminder window: the restore notice counts as the reminder, so no
	// second notice follows it (reminded_at is set by the recheck).
	x.mustPoll()
	if notices = x.noticesOf(x.partyOf(passed, roleBuyer), passed); anyContains(notices, "Reminder") ||
		agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND reminded_at IS NOT NULL", passed) != 1 {
		t.Fatalf("a reminder straight after the restore notice: %q", notices)
	}
	if panel := x.panel(passed, x.vendorSess); !containsAll(panel, utcUntil(got), "moved after the market restored its records from a backup") {
		t.Fatalf("the panel after the recheck:\n%s", panel)
	}
}

// P3-R7: the Bitcoin escrow watcher's deadline step (escrowDeadlines) handles Bitcoin escrow orders only, and the Monero
// escrow watcher's (xmrDeadlines) Monero ones only: the release, the reminder and the restore recheck, each with its own
// coin's recheck request.
func TestXMREscrowDeadlinesSplitByCoin(t *testing.T) {
	x := newEscrowEnv(t)
	ctx := context.Background()
	w := xmrWallet(t, x.testEnv, 1, "active")
	physical := x.physicalListing(x.vendor, 3)
	xmrShipped := func(account int64, set string) string {
		order, _, _ := xmrEscrow(t, x.testEnv, w, account, x.buyer, physical, stateShipped)
		x.forceDeadline(order, set)
		return order
	}
	btcShipped := func(label, set string) string {
		order, _ := x.shippedPhysical(regtestAddr(label+"-refund"), regtestAddr(label+"-claim"), 100000)
		x.forceDeadline(order, set)
		return order
	}
	due, soon := "release_at=now()-interval '1 minute'", "release_at=now()+interval '10 hours'"
	btcDue, btcSoon := btcShipped("split-due", due), btcShipped("split-soon", soon)
	xmrDue, xmrSoon := xmrShipped(1, due), xmrShipped(2, soon)
	reminded := func(order string) bool {
		return agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND reminded_at IS NOT NULL", order) == 1
	}
	if err := x.A.escrowDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	if x.state(btcDue) != stateCompleted || !reminded(btcSoon) || x.state(xmrDue) != stateShipped || reminded(xmrSoon) {
		t.Fatalf("the Bitcoin step: Bitcoin %s reminded %v, Monero %s reminded %v", x.state(btcDue), reminded(btcSoon), x.state(xmrDue), reminded(xmrSoon))
	}
	btcDue2 := btcShipped("split-due-2", due)
	if err := x.A.xmrDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	if x.state(xmrDue) != stateCompleted || !reminded(xmrSoon) || x.state(btcDue2) != stateShipped {
		t.Fatalf("the Monero step: Monero %s reminded %v, Bitcoin %s", x.state(xmrDue), reminded(xmrSoon), x.state(btcDue2))
	}
	// The restore recheck: each step consumes its own coin's request and moves its own coin's deadlines.
	xmrPassed := xmrShipped(3, "release_at=now()-interval '2 days'")
	for _, key := range []string{releaseRecheckKey, xmrReleaseRecheckKey} {
		agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", key)
	}
	rechecked := func(order string) bool {
		return agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND release_recheck", order) == 1
	}
	pending := func(key string) bool { return agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key=$1", key) == 1 }
	if err := x.A.escrowDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	if !rechecked(btcDue2) || rechecked(xmrPassed) || pending(releaseRecheckKey) || !pending(xmrReleaseRecheckKey) {
		t.Fatalf("the Bitcoin recheck: Bitcoin %v, Monero %v", rechecked(btcDue2), rechecked(xmrPassed))
	}
	if err := x.A.xmrDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	if !rechecked(xmrPassed) || pending(xmrReleaseRecheckKey) || x.state(xmrPassed) != stateShipped || x.state(btcDue2) != stateShipped {
		t.Fatalf("the Monero recheck: Monero %v %s", rechecked(xmrPassed), x.state(xmrPassed))
	}
}

// P3-R7 (P2-R4 for Monero): a decision for the buyer on a shipped Monero escrow order supersedes the release requested
// at shipment in the resolve transaction and requests the refund to the refund address; a decision for the vendor
// reuses that release (no new claim). A release whose transaction is stored is the outcome: deciding for the buyer is
// then refused (409, saying why) and changes nothing. The dispute desk states the coin, the amount in XMR and the
// custody sentence, with no Bitcoin wording.
func TestXMREscrowDisputeSupersedes(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("xadm_"+randomToken()[:6], "admin")
	physical := x.physical(x.vendor)
	resolve := func(order, outcome string) (int, string) {
		dispute := agStrings(t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)[0]
		return x.xmrPost("/resolve", admin, url.Values{"id": {dispute}, "outcome": {outcome}, "resolution": {"Decided from the evidence both parties sent."},
			"payout_confirmed": {"confirmed"}, "amount_seen": {strconv.FormatInt(xmrListingPrice, 10)}, "reply_seen": {"0"}, "password": {testPassword}})
	}
	disputedShipped := func() (string, string, string) {
		order, sess, claim := x.shippedXMR(physical)
		if code, body := x.xmrDispute(order, sess); code != 303 {
			t.Fatalf("dispute: %d %s", code, body)
		}
		return order, sess, claim
	}

	order, buyerSess, claim := disputedShipped()
	desk := html.UnescapeString(x.page("/moderator", admin))
	i := strings.Index(desk, "Order "+order)
	if i < 0 {
		t.Fatalf("the desk does not list the order:\n%s", desk)
	}
	article := desk[i : i+strings.Index(desk[i:], "</article>")]
	if !containsAll(article, "Monero escrow order", "0.5 fakechain XMR", xmrCustody, "the release to the vendor requested when the order was shipped is cancelled",
		"On its next pass after you resolve, the market sends the coins to the vendor or refunds the buyer", "cannot be undone", xmrResolveConfirm) {
		t.Fatalf("the desk's view of a Monero escrow dispute:\n%s", article)
	}
	noBitcoinWords(t, "the desk's view of a Monero escrow dispute", article)
	if code, body := resolve(order, "refund"); code != 303 {
		t.Fatalf("resolve for the buyer: %d %s", code, body)
	}
	if got := x.xmrSettlements(order); x.state(order) != stateResolved || len(got) != 2 || got[0] != "release superseded "+claim ||
		got[1] != "refund requested "+testXMRAddr("refund "+order, true) {
		t.Fatalf("after deciding for the buyer: order %s, settlements %v", x.state(order), got)
	}
	if panel := x.panel(order, buyerSess); !strings.Contains(panel, "decided for the buyer") {
		t.Fatalf("the panel of an order decided for the buyer:\n%s", panel)
	}

	vendorOrder, _, vendorClaim := disputedShipped()
	if code, body := resolve(vendorOrder, "release"); code != 303 {
		t.Fatalf("resolve for the vendor: %d %s", code, body)
	}
	if got := x.xmrSettlements(vendorOrder); x.state(vendorOrder) != stateResolved || len(got) != 1 || got[0] != "release requested "+vendorClaim ||
		!settlementFits("release", stateResolved, "release") {
		t.Fatalf("after deciding for the vendor: order %s, settlements %v", x.state(vendorOrder), got)
	}
	vendorPage := x.panel(vendorOrder, x.vendorSess)
	if strings.Contains(x.page("/order?id="+vendorOrder, x.vendorSess), `action="/escrow-xmr/claim"`) || !strings.Contains(vendorPage, "requested when the order was shipped pays the vendor") {
		t.Fatalf("the vendor of an order decided for the vendor:\n%s", vendorPage)
	}

	// A release with its transaction stored (written here past the order state the database requires for it, as only a
	// completed order's release can be built) is the outcome: deciding for the buyer is refused and changes nothing.
	final, _, _ := disputedShipped()
	agExec(x.testEnv, "UPDATE orders SET state='completed' WHERE id=$1", final)
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='built',tx_hash=$2,tx_metadata='ab01',tx_blob='cd02',fee=30000000,input_total=500000000000,key_images=ARRAY[$2] WHERE order_id=$1",
		final, txidOf("final "+final))
	agExec(x.testEnv, "UPDATE orders SET state='disputed' WHERE id=$1", final)
	code, body := resolve(final, "refund")
	if code != 409 || !strings.Contains(body, errXMRReleaseFinal.Error()) {
		t.Fatalf("resolve for the buyer over a built release: %d %s", code, body)
	}
	noBitcoinWords(t, "the refusal", errXMRReleaseFinal.Error())
	if got := x.xmrSettlements(final); x.state(final) != stateDisputed || len(got) != 1 || !strings.HasPrefix(got[0], "release built ") ||
		agInt(x.testEnv, "SELECT count(*) FROM disputes WHERE order_id=$1 AND status='Open' AND outcome=''", final) != 1 {
		t.Fatalf("the refused decision changed something: %s %v", x.state(final), got)
	}
}
