package market

import (
	"context"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Escrow Phase 2, slice P2-c (WR-escrow A-247, A-377, A-378, A-381; P2-R3, P2-R5, P2-R9 and P2-R10's restore recheck):
// the release deadline of a physical escrow order, its 72-hour reminder, the buyer's no-ship cancellation, the refusal
// of a dispute filed after the deadline, the deadline recheck after a restore from backup, and the deadline on the
// order page, the /orders banner and the notices. DB integration tests against the simulated Core of
// escrow_fake_test.go (simulated RPC, not chain evidence: TestEscrowRegtestDeadline is the real-chain leg).

// forceDeadline writes set on order's escrow row past migration 079's freeze trigger, as a test moving the clock does
// (the trigger is disabled only inside this transaction).
func (x *escrowEnv) forceDeadline(order, set string) {
	x.t.Helper()
	tx, err := x.DB.Begin()
	if err != nil {
		x.t.Fatal(err)
	}
	defer tx.Rollback()
	for _, q := range []string{"ALTER TABLE escrows DISABLE TRIGGER escrows_deadlines_frozen", "UPDATE escrows SET " + set + " WHERE order_id=$1",
		"ALTER TABLE escrows ENABLE TRIGGER escrows_deadlines_frozen"} {
		var args []any
		if strings.Contains(q, "$1") {
			args = append(args, order)
		}
		if _, err = tx.Exec(q, args...); err != nil {
			x.t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		x.t.Fatal(err)
	}
}

// deadlineOf is order's escrow column (release_at or ship_by) as the pages and notices state it.
func (x *escrowEnv) deadlineOf(order, column string) string {
	x.t.Helper()
	var at time.Time
	if err := x.DB.QueryRow("SELECT "+column+" FROM escrows WHERE order_id=$1", order).Scan(&at); err != nil {
		x.t.Fatal(err)
	}
	if column == "ship_by" {
		return utcFrom(at)
	}
	return utcUntil(at)
}

// noticesOf lists uid's notifications that name order (its short ID), oldest first.
func (x *escrowEnv) noticesOf(uid, order string) []string {
	return agStrings(x.t, x.testEnv, "SELECT body FROM notifications WHERE user_id=$1 AND position($2 in body)>0 ORDER BY created,id", uid, shortID(order))
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func anyContains(list []string, parts ...string) bool {
	for _, s := range list {
		if containsAll(s, parts...) {
			return true
		}
	}
	return false
}

// countContains counts the entries of list holding every part.
func countContains(list []string, parts ...string) int {
	n := 0
	for _, s := range list {
		if containsAll(s, parts...) {
			n++
		}
	}
	return n
}

// waitForLockWaits waits until n backends of this database wait for a lock.
func (x *escrowEnv) waitForLockWaits(n int) {
	x.t.Helper()
	if !waitUntil(x.t, 5*time.Second, func() bool {
		return agInt(x.testEnv, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'") >= n
	}) {
		x.t.Fatalf("fewer than %d requests waited for a lock", n)
	}
}

// P2-R3: at release_at a shipped escrow order with no dispute is completed by the system ("auto-release deadline
// passed") and the co-sign step co-signs the vendor's stored signature in the same pass; before it nothing moves.
// Reaching the deadline exactly releases (release_at <= now()). A release nobody signed waits for the vendor's
// signature, and the page and the notices say so. A dispute row stops it even on an order still shipped. The shipped
// and release notices carry the order's short ID and the deadline in UTC, and the order page shows the deadline with a
// countdown computed when it loaded.
func TestEscrowDeadlineRelease(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.shippedPhysical(regtestAddr("dl-refund-1"), regtestAddr("dl-claim-1"), 100000)
	deadline := x.deadlineOf(order, "release_at")
	if got := x.noticesOf(x.buyer, order); !anyContains(got, "Order "+shortID(order), "Shipped", "Release deadline: "+deadline) {
		t.Fatalf("the buyer's shipped notice lacks the deadline %s: %q", deadline, got)
	}
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		page := html.UnescapeString(x.page("/order?id="+order, sess))
		if !containsAll(page, "Release deadline", deadline, "About 13 days", "computed when this page loaded", "Opening a dispute cancels the deadline") || strings.Contains(page, "paused") {
			t.Fatalf("the shipped order page lacks the deadline, its countdown or the dispute line:\n%s", page)
		}
	}
	if page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess)); !strings.Contains(page, "If you cannot sign, you need do nothing") {
		t.Fatalf("the buyer's who-acts-next line does not cover the deadline:\n%s", page)
	}
	x.signAs(order, roleVendor, id)
	x.poll()
	if x.state(order) != stateShipped || x.settleByID(id).State != "party_signed" || x.marketCalls() != 0 {
		t.Fatalf("before the deadline: order %s, release %s, market calls %d", x.state(order), x.settleByID(id).State, x.marketCalls())
	}
	// Past the deadline: completed by the system, then co-signed with the vendor's signature and broadcast, in one pass.
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	deadline = x.deadlineOf(order, "release_at")
	x.poll()
	if x.state(order) != stateCompleted {
		t.Fatalf("past the deadline: %s", x.state(order))
	}
	ev := x.events(order)
	if !anyContains(ev, "shipped>completed system auto-release deadline passed ("+deadline+")") {
		t.Fatalf("no system completion with the deadline note: %q", ev)
	}
	if r := x.settleByID(id); r.State != "broadcast" || r.Cosigned != "market" {
		t.Fatalf("the vendor's signature was not co-signed: %+v", r)
	}
	for _, uid := range []string{x.buyer, x.vendor} {
		if got := x.noticesOf(uid, order); !anyContains(got, "Order "+shortID(order), "Completed", "auto-release deadline passed ("+deadline+")", "co-signs the vendor's signature") {
			t.Fatalf("release notice to %s: %q", uid, got)
		}
	}

	// The boundary: a deadline equal to now() is due. One transaction sets release_at to its own now() and runs the check.
	order2, id2 := x.shippedPhysical(regtestAddr("dl-refund-2"), regtestAddr("dl-claim-2"), 100000)
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
	released, err := x.A.releaseIfDue(context.Background(), tx, order2)
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
	// Nobody signed: the release waits for the vendor's signature, and the page and the notices say so.
	calls := x.marketCalls()
	x.poll()
	if r := x.settleByID(id2); r.State != "awaiting_signature" || x.marketCalls() != calls {
		t.Fatalf("an unsigned release after the deadline: %+v, market calls %d", r, x.marketCalls())
	}
	for _, uid := range []string{x.buyer, x.vendor} {
		if got := x.noticesOf(uid, order2); !anyContains(got, "auto-release deadline passed", "waits for the vendor's signature") {
			t.Fatalf("notice to %s of an unsigned deadline release: %q", uid, got)
		}
	}
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		if page := html.UnescapeString(x.page("/order?id="+order2, sess)); !strings.Contains(page, "waits for the vendor's signature") || strings.Contains(page, "The vendor claims the escrowed coins") {
			t.Fatalf("the completed order page does not say the release waits for the vendor:\n%s", page)
		}
	}
	x.signAs(order2, roleVendor, id2)
	x.poll()
	if r := x.settleByID(id2); r.State != "broadcast" || r.Cosigned != "market" {
		t.Fatalf("the vendor's later signature was not co-signed: %+v", r)
	}

	// A dispute row stops the release even on an order still shipped (defence in depth: the dispute action moves the
	// order to disputed in the same transaction).
	order3, _ := x.shippedPhysical(regtestAddr("dl-refund-3"), regtestAddr("dl-claim-3"), 100000)
	x.forceDeadline(order3, "release_at=now()-interval '1 hour'")
	agExec(x.testEnv, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'A dispute row written beside a shipped order.')", randomToken(), order3)
	x.poll()
	if x.state(order3) != stateShipped || anyContains(x.events(order3), "auto-release") {
		t.Fatalf("an order with a dispute row was released: %s %q", x.state(order3), x.events(order3))
	}
	// The buyer signed and the vendor did not: the page no longer tells the buyer to do nothing, and at the deadline the
	// market co-signs the buyer's signature.
	order4, id4 := x.shippedPhysical(regtestAddr("dl-refund-4"), regtestAddr("dl-claim-4"), 100000)
	x.signAs(order4, roleBuyer, id4)
	if page := html.UnescapeString(x.page("/order?id="+order4, x.buyerSess)); strings.Contains(page, "you need do nothing") || !strings.Contains(page, "co-signs the buyer's signature") {
		t.Fatalf("the page of a release the buyer signed:\n%s", page)
	}
	x.forceDeadline(order4, "release_at=now()-interval '1 minute'")
	x.poll()
	if r := x.settleByID(id4); x.state(order4) != stateCompleted || r.State != "broadcast" || r.Cosigned != "market" || !anyContains(x.noticesOf(x.vendor, order4), "co-signs the buyer's signature") {
		t.Fatalf("the buyer-signed release at the deadline: order %s, %+v", x.state(order4), r)
	}
	// A pooled shipped order is never touched.
	pooled := x.testEnv.order(x.buyer, x.physicalListing(x.vendor, 3), "BTC", stateShipped)
	x.poll()
	if x.state(pooled) != stateShipped {
		t.Fatalf("a pooled shipped order moved: %s", x.state(pooled))
	}
}

// P2-R3, P2-R4: a dispute and the watcher's deadline release lock the order row, so exactly one takes effect. A
// dispute filed before the deadline that reaches the lock first wins even if the deadline passes while it waits; the
// release queued behind it then finds the order disputed. A release that reaches the lock first completes the order,
// and the dispute queued behind it is refused. Once disputed, a passed deadline never releases the order.
func TestEscrowDeadlineDisputeRace(t *testing.T) {
	x := newEscrowEnv(t)
	ctx := context.Background()
	reason := url.Values{"reason": {"The parcel never arrived at my address at all."}}
	dispute := func(order string) (int, string) {
		f := url.Values{"order_id": {order}}
		for k, v := range reason {
			f[k] = v
		}
		w := x.do("POST", "/disputes", x.buyerSess, f)
		return w.Code, w.Body.String()
	}
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
		order, _ := x.shippedPhysical(regtestAddr("race-dl-refund-1"), regtestAddr("race-dl-claim-1"), 100000)
		x.forceDeadline(order, "release_at=clock_timestamp()+interval '2 seconds'")
		tx := lockOrder(order)
		var code int
		var body string
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); code, body = dispute(order) }() // its transaction starts before the deadline
		x.waitForLockWaits(1)
		if !waitUntil(t, 5*time.Second, func() bool {
			return agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND release_at<clock_timestamp()", order) == 1
		}) {
			t.Fatal("the deadline did not pass")
		}
		var relErr error
		go func() { defer wg.Done(); relErr = x.A.releaseDue(ctx) }() // after the deadline, behind the dispute
		x.waitForLockWaits(2)
		tx.Commit()
		wg.Wait()
		if relErr != nil {
			t.Fatal(relErr)
		}
		if code != 303 || x.state(order) != stateDisputed || anyContains(x.events(order), "auto-release") || agInt(x.testEnv, "SELECT count(*) FROM disputes WHERE order_id=$1", order) != 1 {
			t.Fatalf("dispute %d %s; order %s; events %q", code, body, x.state(order), x.events(order))
		}
		// Disputed, a passed deadline never releases it, and the page says the deadline no longer applies.
		x.poll()
		if x.state(order) != stateDisputed {
			t.Fatalf("a disputed order moved at its deadline: %s", x.state(order))
		}
		if page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess)); strings.Contains(page, " h left") || strings.Contains(page, "paused") {
			t.Fatalf("the disputed order page still counts down or says paused:\n%s", page)
		}
	})
	t.Run("the release first", func(t *testing.T) {
		order, _ := x.shippedPhysical(regtestAddr("race-dl-refund-2"), regtestAddr("race-dl-claim-2"), 100000)
		x.forceDeadline(order, "release_at=now()-interval '1 second'")
		tx := lockOrder(order)
		var relErr error
		var code int
		var body string
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); relErr = x.A.releaseDue(ctx) }()
		x.waitForLockWaits(1)
		go func() { defer wg.Done(); code, body = dispute(order) }()
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

// P2-R3: while the restore gate is set no deadline releases an order; once it is cleared the next pass does. The
// release needs no RPC: it runs when the node check fails (the node unreachable, or not on regtest).
func TestEscrowDeadlineGatePaused(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.shippedPhysical(regtestAddr("gate-dl-refund"), regtestAddr("gate-dl-claim"), 100000)
	x.signAs(order, roleVendor, id)
	x.forceDeadline(order, "release_at=now()-interval '1 hour'")
	x.setGate(true)
	x.poll()
	if x.state(order) != stateShipped || x.settleByID(id).State != "party_signed" {
		t.Fatalf("released while the restore gate is set: %s %s", x.state(order), x.settleByID(id).State)
	}
	if page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess)); !strings.Contains(page, "the release is due") {
		t.Fatalf("the page of a due release held by the gate:\n%s", page)
	}
	x.setGate(false)
	x.srv.down.Store(true)
	if err := x.A.escrowPollOnce(context.Background()); err == nil {
		t.Fatal("the pass reported no node failure")
	}
	if x.state(order) != stateCompleted || x.settleByID(id).State != "party_signed" {
		t.Fatalf("with the node unreachable: order %s, release %s", x.state(order), x.settleByID(id).State)
	}
	x.srv.down.Store(false)
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || r.Cosigned != "market" {
		t.Fatalf("once the node answers: %+v", r)
	}
	// A node that is not on regtest: the deadline is still applied.
	order2, _ := x.shippedPhysical(regtestAddr("gate-dl-refund-2"), regtestAddr("gate-dl-claim-2"), 100000)
	x.forceDeadline(order2, "release_at=now()-interval '1 hour'")
	x.core.mu.Lock()
	x.core.chain = "main"
	x.core.mu.Unlock()
	if err := x.A.escrowPollOnce(context.Background()); err == nil {
		t.Fatal("the pass reported no node failure")
	}
	x.core.mu.Lock()
	x.core.chain = "regtest"
	x.core.mu.Unlock()
	if x.state(order2) != stateCompleted {
		t.Fatalf("with the node not on regtest: %s", x.state(order2))
	}
}

// P2-R3, P2-R9: 72 hours before the deadline the buyer is reminded once, with the deadline in UTC and the order's short
// ID, and the vendor told the buyer's deadline; reminded_at is claimed by compare-and-set, so two passes racing send one
// reminder. Never before the 72 hours, and never on a disputed order.
func TestEscrowReminderOnce(t *testing.T) {
	x := newEscrowEnv(t)
	ctx := context.Background()
	order, _ := x.shippedPhysical(regtestAddr("remind-refund-1"), regtestAddr("remind-claim-1"), 100000)
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND reminded_at IS NOT NULL", order) != 0 {
		t.Fatal("reminded 14 days before the deadline")
	}
	x.forceDeadline(order, "release_at=now()+interval '71 hours'")
	deadline := x.deadlineOf(order, "release_at")
	// Two reminder steps race on the escrow row: both list it, both wait for its lock, one claims it.
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
		go func() { defer wg.Done(); errs[i] = x.A.remindDeadlines(ctx) }()
	}
	x.waitForLockWaits(2)
	tx.Commit()
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	x.poll()
	x.poll()
	buyer, vendor := x.noticesOf(x.buyer, order), x.noticesOf(x.vendor, order)
	if n := countContains(buyer, "Reminder", deadline); n != 1 {
		t.Fatalf("%d reminders to the buyer, want 1 naming %s: %q", n, deadline, buyer)
	}
	if !anyContains(buyer, "Order "+shortID(order), "until "+deadline, "open a dispute") {
		t.Fatalf("the reminder does not name the order and the deadline: %q", buyer)
	}
	if n := countContains(vendor, "the buyer has until "+deadline); n != 1 {
		t.Fatalf("%d deadline notices to the vendor, want 1: %q", n, vendor)
	}
	// A disputed order is never reminded.
	order2, _ := x.shippedPhysical(regtestAddr("remind-refund-2"), regtestAddr("remind-claim-2"), 100000)
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order2}, "reason": {"The parcel never arrived at my address at all."}}); w.Code != 303 {
		t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
	x.forceDeadline(order2, "release_at=now()+interval '10 hours'")
	x.poll()
	if got := x.noticesOf(x.buyer, order2); anyContains(got, "Reminder") {
		t.Fatalf("a disputed order was reminded: %q", got)
	}
}

// buyerCancel posts the buyer's cancellation of a paid order with the refund confirmation the form carries.
func (x *escrowEnv) buyerCancel(order string) (int, string) {
	w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {statePaid}, "payout_confirmed": {"confirmed"}, "amount_seen": {"100000"}})
	return w.Code, w.Body.String()
}

// P2-R5: a paid physical escrow order gets ship_by = paid time + its dispatch window + 48 h. Before it the buyer's
// cancellation is refused (409) with the time stated and the page says when it opens; from it the page offers the
// cancel form with the refund confirmation, and the cancellation requests the refund, which the buyer signs and the
// market co-signs. A pooled order's buyer still cannot cancel a paid order, nor can a digital escrow order's.
func TestEscrowNoShipCancel(t *testing.T) {
	x := newEscrowEnv(t)
	before := time.Now()
	order := x.paidPhysical(regtestAddr("noship-refund-1"), 100000)
	var shipBy time.Time
	if err := x.DB.QueryRow("SELECT ship_by FROM escrows WHERE order_id=$1", order).Scan(&shipBy); err != nil {
		t.Fatalf("no ship_by after payment: %v", err)
	}
	if want := before.Add(3*24*time.Hour + 48*time.Hour); shipBy.Before(want.Add(-time.Minute)) || shipBy.After(want.Add(time.Minute)) {
		t.Fatalf("ship_by %s, want about %s (paid + 3 days + 48 h)", shipBy, want)
	}
	from := x.deadlineOf(order, "ship_by")
	page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
	if !strings.Contains(page, "You can cancel from "+from+" if the vendor has not shipped") || strings.Contains(page, `action="/orders/cancel"`) {
		t.Fatalf("the buyer's page before the window opens:\n%s", page)
	}
	if page := html.UnescapeString(x.page("/order?id="+order, x.vendorSess)); !strings.Contains(page, "Ship by "+from) {
		t.Fatalf("the vendor's page does not state the ship-by time:\n%s", page)
	}
	code, body := x.buyerCancel(order)
	if code != 409 || !strings.Contains(html.UnescapeString(body), from) {
		t.Fatalf("a cancel before ship_by: %d %s", code, body)
	}
	if x.state(order) != statePaid || agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", order) != 0 {
		t.Fatal("the refused cancel changed the order")
	}
	// The window opens: the cancel form with the refund confirmation, and the refund.
	x.forceDeadline(order, "ship_by=now()-interval '1 minute'")
	page = html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
	if !containsAll(page, `action="/orders/cancel"`, "Cancelling refunds the 0.001 regtest BTC", `name="amount_seen" value="100000"`) {
		t.Fatalf("the buyer's page once the window opened:\n%s", page)
	}
	if code, body := x.buyerCancel(order); code != 303 {
		t.Fatalf("the cancel after ship_by: %d %s", code, body)
	}
	id, kind, state, _ := x.settlement(order)
	if x.state(order) != stateCancelled || kind != "refund" || state != "building" {
		t.Fatalf("after the cancel: order %s, settlement %s %s", x.state(order), kind, state)
	}
	x.poll()
	x.signAs(order, roleBuyer, id)
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || r.Cosigned != "market" {
		t.Fatalf("the refund signed by the buyer: %+v", r)
	}
	// A pooled paid order: the buyer has no such edge.
	pooled := x.testEnv.order(x.buyer, x.physicalListing(x.vendor, 3), "BTC", statePaid)
	if code, body := x.buyerCancel(pooled); code != 403 {
		t.Fatalf("a buyer cancel of a pooled paid order: %d %s", code, body)
	}
	if page := x.page("/order?id="+pooled, x.buyerSess); strings.Contains(page, `action="/orders/cancel"`) {
		t.Fatal("a pooled paid order offers the buyer a cancel form")
	}
	// A digital escrow order left paid (its content could not be delivered automatically): no ship_by, no buyer cancel.
	digital := x.draft()
	x.mustCheckout(digital, regtestAddr("noship-refund-2"))
	agExec(x.testEnv, "UPDATE products SET delivery_content='' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", digital)
	x.fund(digital, digital, 3, 100000)
	if x.state(digital) != statePaid || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND ship_by IS NULL", digital) != 1 {
		t.Fatalf("digital escrow order %s", x.state(digital))
	}
	if code, body := x.buyerCancel(digital); code != 409 {
		t.Fatalf("a buyer cancel of a paid digital escrow order: %d %s", code, body)
	}
	if page := x.page("/order?id="+digital, x.buyerSess); strings.Contains(page, `action="/orders/cancel"`) {
		t.Fatal("a paid digital escrow order offers the buyer a cancel form")
	}
}

// P2-R5: the vendor's "Mark shipped" and the buyer's no-ship cancellation lock the order row and compare-and-set its
// state, so exactly one takes effect; the other is refused with a message naming what happened and when, not the
// generic 409.
func TestEscrowShipCancelRace(t *testing.T) {
	x := newEscrowEnv(t)
	ready := func(label string) string {
		order := x.paidPhysical(regtestAddr(label+"-refund"), 100000)
		x.forceDeadline(order, "ship_by=now()-interval '1 minute'")
		return order
	}
	shippedAt := func(order string) string {
		return physString(t, x.testEnv, `SELECT to_char(max(created) AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"') FROM order_events WHERE order_id=$1 AND to_state='shipped' AND from_state='paid'`, order)
	}
	cancelledAt := func(order string) string {
		return physString(t, x.testEnv, `SELECT to_char(max(created) AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"') FROM order_events WHERE order_id=$1 AND to_state='cancelled'`, order)
	}
	t.Run("shipped, then the cancel", func(t *testing.T) {
		order := ready("sc-1")
		if code, body := x.shipEscrow(order, x.vendorSess, regtestAddr("sc-1-claim"), testPassword); code != 303 {
			t.Fatalf("ship: %d %s", code, body)
		}
		code, body := x.buyerCancel(order)
		if body = html.UnescapeString(body); code != 409 || !containsAll(body, "marked this order shipped at "+shippedAt(order)) || strings.Contains(body, errConflict.Error()) {
			t.Fatalf("the cancel after the shipment: %d %s", code, body)
		}
	})
	t.Run("cancelled, then the shipment", func(t *testing.T) {
		order := ready("sc-2")
		if code, body := x.buyerCancel(order); code != 303 {
			t.Fatalf("cancel: %d %s", code, body)
		}
		code, body := x.shipEscrow(order, x.vendorSess, regtestAddr("sc-2-claim"), testPassword)
		if body = html.UnescapeString(body); code != 409 || !containsAll(body, "buyer cancelled this order at "+cancelledAt(order)) {
			t.Fatalf("the shipment after the cancel: %d %s", code, body)
		}
		if agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='release'", order) != 0 {
			t.Fatal("a release was requested on a cancelled order")
		}
	})
	t.Run("concurrent", func(t *testing.T) {
		for i := range 3 {
			order := ready("sc-3-" + string(rune('a'+i)))
			tx, err := x.DB.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec("SELECT 1 FROM orders WHERE id=$1 FOR UPDATE", order); err != nil {
				t.Fatal(err)
			}
			var shipCode, cancelCode int
			var shipBody, cancelBody string
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				shipCode, shipBody = x.shipEscrow(order, x.vendorSess, regtestAddr("sc-3-claim-"+string(rune('a'+i))), testPassword)
			}()
			go func() { defer wg.Done(); cancelCode, cancelBody = x.buyerCancel(order) }()
			x.waitForLockWaits(2)
			tx.Commit()
			wg.Wait()
			releases := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='release'", order)
			refunds := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='refund'", order)
			switch {
			case shipCode == 303 && cancelCode == 409:
				if x.state(order) != stateShipped || releases != 1 || refunds != 0 || !strings.Contains(html.UnescapeString(cancelBody), "marked this order shipped at "+shippedAt(order)) {
					t.Fatalf("ship won: order %s, releases %d, refunds %d, cancel said %s", x.state(order), releases, refunds, cancelBody)
				}
			case cancelCode == 303 && shipCode == 409:
				if x.state(order) != stateCancelled || releases != 0 || refunds != 1 || !strings.Contains(html.UnescapeString(shipBody), "buyer cancelled this order at "+cancelledAt(order)) {
					t.Fatalf("cancel won: order %s, releases %d, refunds %d, ship said %s", x.state(order), releases, refunds, shipBody)
				}
			default:
				t.Fatalf("ship %d %s; cancel %d %s", shipCode, shipBody, cancelCode, cancelBody)
			}
		}
	})
}

// P2-R3 (owner decision 2026-10-03): a dispute filed on a shipped escrow order after its release deadline is refused
// (409, the release is due) by the buyer and the vendor alike, and changes nothing. Before the deadline it is opened,
// and the other party's notice says the deadline no longer applies, with the deadline in UTC.
func TestEscrowLateDisputeRefused(t *testing.T) {
	x := newEscrowEnv(t)
	order, _ := x.shippedPhysical(regtestAddr("late-refund-1"), regtestAddr("late-claim-1"), 100000)
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	deadline := x.deadlineOf(order, "release_at")
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		w := x.do("POST", "/disputes", sess, url.Values{"order_id": {order}, "reason": {"The parcel never arrived at my address at all."}})
		if body := html.UnescapeString(w.Body.String()); w.Code != 409 || !containsAll(body, "release to the vendor is due", deadline) {
			t.Fatalf("a dispute after the deadline: %d %s", w.Code, body)
		}
	}
	if x.state(order) != stateShipped || agInt(x.testEnv, "SELECT count(*) FROM disputes WHERE order_id=$1", order) != 0 {
		t.Fatal("a refused dispute changed the order")
	}
	// Before the deadline: opened, and the vendor told the deadline no longer applies.
	order2, _ := x.shippedPhysical(regtestAddr("late-refund-2"), regtestAddr("late-claim-2"), 100000)
	deadline2 := x.deadlineOf(order2, "release_at")
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order2}, "reason": {"The parcel never arrived at my address at all."}}); w.Code != 303 {
		t.Fatalf("a dispute before the deadline: %d %s", w.Code, w.Body.String())
	}
	if got := x.noticesOf(x.vendor, order2); !anyContains(got, "Order "+shortID(order2), "Disputed", "release deadline of "+deadline2+" no longer applies") {
		t.Fatalf("the vendor's dispute notice: %q", got)
	}
}

// restoreRecheck is what scripts/restore.sh (and UPGRADING.md section 6's manual protection) writes for the release
// deadline recheck, with the restore gate.
func (x *escrowEnv) restoreRecheck() {
	x.t.Helper()
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('escrow_release_recheck','true') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value")
	x.setGate(true)
}

// P2-R10, A-381 (owner decision 2026-10-03): after a restore from backup, the watcher's next pass moves every passed
// release deadline of a shipped escrow order to now + 72 h, sets release_recheck in that statement and tells the
// buyer. A deadline still ahead, and a disputed order's, are left alone. A second restore works as the first (only the
// recheck resets release_recheck), and without a restore a passed deadline is never moved.
func TestEscrowRestoreExtendsDeadline(t *testing.T) {
	x := newEscrowEnv(t)
	passed, _ := x.shippedPhysical(regtestAddr("rr-refund-1"), regtestAddr("rr-claim-1"), 100000)
	ahead, _ := x.shippedPhysical(regtestAddr("rr-refund-2"), regtestAddr("rr-claim-2"), 100000)
	disputed, _ := x.shippedPhysical(regtestAddr("rr-refund-3"), regtestAddr("rr-claim-3"), 100000)
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {disputed}, "reason": {"The parcel never arrived at my address at all."}}); w.Code != 303 {
		t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
	x.forceDeadline(passed, "release_at=now()-interval '2 days'")
	x.forceDeadline(ahead, "release_at=now()+interval '5 days'")
	x.forceDeadline(disputed, "release_at=now()-interval '2 days'")
	at := func(order string) time.Time {
		var v time.Time
		if err := x.DB.QueryRow("SELECT release_at FROM escrows WHERE order_id=$1", order).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	aheadAt, disputedAt := at(ahead), at(disputed)
	for round := 1; round <= 2; round++ {
		x.restoreRecheck()
		start := time.Now()
		x.poll()
		got := at(passed)
		if got.Before(start.Add(72*time.Hour-time.Minute)) || got.After(time.Now().Add(72*time.Hour+time.Minute)) {
			t.Fatalf("restore %d: release_at %s, want now + 72 h", round, got)
		}
		if agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND release_recheck", passed) != 1 || x.state(passed) != stateShipped {
			t.Fatalf("restore %d: release_recheck not set, or the order moved (%s)", round, x.state(passed))
		}
		if !at(ahead).Equal(aheadAt) || !at(disputed).Equal(disputedAt) || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id IN ($1,$2) AND release_recheck", ahead, disputed) != 0 {
			t.Fatalf("restore %d moved a deadline still ahead or a disputed order's", round)
		}
		if agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key='escrow_release_recheck'") != 0 {
			t.Fatalf("restore %d: the recheck flag was not consumed", round)
		}
		deadline := utcUntil(got)
		notices := x.noticesOf(x.buyer, passed)
		if n := countContains(notices, "restored its records from a backup"); n != round || !containsAll(notices[len(notices)-1], "restored its records from a backup", deadline) {
			t.Fatalf("restore %d: %d restore notices to the buyer, the last not naming %s: %q", round, n, deadline, notices)
		}
		if page := html.UnescapeString(x.page("/order?id="+passed, x.buyerSess)); !containsAll(page, deadline, "moved after the market restored its records from a backup") {
			t.Fatalf("restore %d: the order page:\n%s", round, page)
		}
		// Another pass without a restore moves nothing.
		x.poll()
		if !at(passed).Equal(got) {
			t.Fatalf("restore %d: a pass without a restore moved the deadline again", round)
		}
		x.setGate(false)
		// The backup of the second restore was taken after this recheck, once the moved deadline had passed again.
		x.forceDeadline(passed, "release_at=now()-interval '1 hour'")
		x.setGate(true)
	}
	// Without a restore a passed deadline releases the order once the gate is cleared; it is not moved.
	before := at(passed)
	x.setGate(false)
	x.poll()
	if x.state(passed) != stateCompleted || !at(passed).Equal(before) {
		t.Fatalf("without a restore: order %s, release_at moved %v", x.state(passed), !at(passed).Equal(before))
	}
}

// A-381: only the release recheck (escrow_deadline.go) writes release_recheck, and release_at is written only there and
// by the shipment (escrow_release.go, from NULL); scripts/restore.sh only asks for the recheck.
func TestEscrowReleaseAtWriters(t *testing.T) {
	writes := regexp.MustCompile(`(?i)(release_at|release_recheck)\s*=[^=]`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scripts, _ := filepath.Glob("../../scripts/*.sh")
	allowed := map[string]string{"escrow_release.go": "release_at", "escrow_deadline.go": "both"}
	for _, f := range append(files, scripts...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range writes.FindAllStringSubmatch(string(b), -1) {
			col := strings.ToLower(m[1])
			switch allowed[filepath.Base(f)] {
			case "both":
			case col:
			default:
				t.Errorf("%s writes %s (%q): only the release recheck moves release_at and resets release_recheck", f, col, m[0])
			}
		}
	}
}

// P2-R9, A-377: /orders shows a banner for each escrow order whose release deadline is less than 72 hours away (to the
// buyer and the vendor), with the order's short ID and the deadline in UTC; none for a deadline further away, a
// disputed or completed order, or a pooled one.
func TestEscrowOrdersBannerUnder72h(t *testing.T) {
	x := newEscrowEnv(t)
	soon, _ := x.shippedPhysical(regtestAddr("banner-refund-1"), regtestAddr("banner-claim-1"), 100000)
	later, _ := x.shippedPhysical(regtestAddr("banner-refund-2"), regtestAddr("banner-claim-2"), 100000)
	disputed, _ := x.shippedPhysical(regtestAddr("banner-refund-3"), regtestAddr("banner-claim-3"), 100000)
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {disputed}, "reason": {"The parcel never arrived at my address at all."}}); w.Code != 303 {
		t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
	x.forceDeadline(soon, "release_at=now()+interval '71 hours'")
	x.forceDeadline(later, "release_at=now()+interval '73 hours'")
	x.forceDeadline(disputed, "release_at=now()+interval '10 hours'")
	pooled := x.testEnv.order(x.buyer, x.physicalListing(x.vendor, 3), "BTC", stateShipped)
	deadline := x.deadlineOf(soon, "release_at")
	for _, c := range []struct{ sess, want string }{{x.buyerSess, "open a dispute before then"}, {x.vendorSess, "The buyer has until " + deadline}} {
		page := html.UnescapeString(x.page("/orders", c.sess))
		banners := regexp.MustCompile(`(?s)<p class="alert deadline-banner".*?</p>`).FindAllString(page, -1)
		if len(banners) != 1 || !containsAll(banners[0], "under 72 hours", shortID(soon), deadline, "About 2 days 22 h left", c.want) {
			t.Fatalf("banners %q in\n%s", banners, page)
		}
		for _, other := range []string{later, disputed, pooled} {
			if strings.Contains(banners[0], shortID(other)) {
				t.Fatalf("a banner names %s", other)
			}
		}
	}
}
