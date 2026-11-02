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

// A-709 (owner decision 2026-10-28 "Deadline + dispute window"): digital buyer protection for Monero escrow. A paid
// digital order gets a delivery deadline of 72 hours from payment, after which the buyer may cancel it for a refund
// (the physical no-ship cancel); every delivery, automatic on funding or by the vendor, leaves the order delivered for
// a 48-hour dispute window (escrows.release_at), in which the buyer may open a dispute or confirm receipt early, and at
// whose end the watcher completes it; the vendor claims only after completion. Once a deadline has passed the dispute
// form is not offered, for shipped physical orders too. DB integration tests against the simulated wallet-rpc
// (escrow_xmr_fake_test.go); TestXMREscrowRegtestFunding and the settle legs are the fakechain evidence.

// manualDigital is a digital listing of vendor's that the vendor delivers by hand (0.5 XMR).
func (x *xmrEscrowEnv) manualDigital(vendor string) string {
	p := x.testEnv.product(vendor, "digital")
	agExec(x.testEnv, "UPDATE products SET digital_delivery_mode='manual' WHERE id=$1", p)
	return p
}

// deliveredXMR is a digital Monero escrow order delivered automatically on funding, in its dispute window, and its
// buyer's session.
func (x *xmrEscrowEnv) deliveredXMR(product string) (string, string) {
	x.t.Helper()
	order, sess := x.fundedXMR(product)
	if x.state(order) != stateDelivered {
		x.t.Fatalf("funded digital Monero escrow order is %s, want delivered (dispute window)", x.state(order))
	}
	return order, sess
}

// completedXMR is a digital Monero escrow order delivered on funding whose dispute window ended with no dispute: the
// watcher's deadline step completed it (endWindows). It returns the order and its buyer's session.
func (x *xmrEscrowEnv) completedXMR(product string) (string, string) {
	x.t.Helper()
	order, sess := x.deliveredXMR(product)
	x.endWindows()
	if x.state(order) != stateCompleted {
		x.t.Fatalf("after its dispute window the digital order is %s", x.state(order))
	}
	return order, sess
}

// endDisputeWindow moves order's dispute window into the past and runs a pass: the watcher completes it.
func (x *xmrEscrowEnv) endDisputeWindow(order string) {
	x.t.Helper()
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	x.mustPoll()
	if x.state(order) != stateCompleted {
		x.t.Fatalf("after its dispute window the digital order is %s (events %q)", x.state(order), x.events(order))
	}
}

// endWindows is the fixture of tests of what follows a digital order's completion (settlement, retention, holds): it
// moves the dispute window of every delivered Monero escrow order into the past and runs only the watcher's deadline
// step (xmrReleaseDue, no RPC), which completes each with no dispute, as the watcher does 48 hours after delivery. Before
// A-709 such an order completed on funding.
func (x *xmrEscrowEnv) endWindows() {
	x.t.Helper()
	tx, err := x.DB.Begin()
	if err != nil {
		x.t.Fatal(err)
	}
	defer tx.Rollback()
	for _, q := range []string{"ALTER TABLE escrows DISABLE TRIGGER escrows_deadlines_frozen",
		"UPDATE escrows es SET release_at=now()-interval '1 minute' FROM orders o WHERE o.id=es.order_id AND es.coin='XMR' AND o.state='delivered' AND es.release_at>now()-interval '1 minute'",
		"ALTER TABLE escrows ENABLE TRIGGER escrows_deadlines_frozen"} {
		if _, err = tx.Exec(q); err != nil {
			x.t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		x.t.Fatal(err)
	}
	if err = x.A.xmrReleaseDue(context.Background()); err != nil {
		x.t.Fatalf("the deadline step: %v", err)
	}
}

func (x *xmrEscrowEnv) escrowTime(order, column string) time.Time {
	x.t.Helper()
	var at time.Time
	if err := x.DB.QueryRow("SELECT "+column+" FROM escrows WHERE order_id=$1", order).Scan(&at); err != nil {
		x.t.Fatalf("%s of %s: %v", column, shortID(order), err)
	}
	return at
}

func near(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if got.Before(want.Add(-time.Minute)) || got.After(want.Add(time.Minute)) {
		t.Fatalf("%s is %s, want about %s", what, got, want)
	}
}

// Criterion 1 and VEN-11: a paid manual-digital order gets ship_by = payment + 72 hours. Before it the buyer's cancel
// is refused with the time stated and the page says when it opens; from it the buyer cancels for a refund to the
// refund address, and the vendor's late delivery is refused with the cancel's time. A delivery before the deadline
// opens the dispute window, says "on delivery" in the history, and ends the buyer's cancel.
func TestXMREscrowDigitalManualDeliveryWindow(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	manual := x.manualDigital(x.vendor)
	// Criterion 4: the deadline and the window are stated before purchase, on the product and checkout pages and in
	// the draft's escrow checkout offer.
	for _, path := range []string{"/product?id=" + manual, "/checkout?id=" + manual, "/order?id=" + x.draft(manual)} {
		if page := html.UnescapeString(x.page(path, x.buyerSess)); !containsAll(page, "within 72 hours of payment", "48 hours", "dispute") {
			t.Fatalf("%s does not state the delivery deadline and the dispute window:\n%s", path, page)
		}
	}
	if page := html.UnescapeString(x.page("/product?id="+x.product, x.buyerSess)); !strings.Contains(page, "After delivery you have 48 hours") || strings.Contains(page, "within 72 hours") {
		t.Fatalf("an automatic listing's buyer protection:\n%s", page)
	}
	before := time.Now()
	order, buyerSess := x.fundedXMR(manual)
	if x.state(order) != statePaid {
		t.Fatalf("a funded manual-digital order is %s", x.state(order))
	}
	shipBy := x.escrowTime(order, "ship_by")
	near(t, "the delivery deadline", shipBy, before.Add(72*time.Hour))
	from := utcFrom(shipBy)
	page := html.UnescapeString(x.page("/order?id="+order, buyerSess))
	if !containsAll(page, "You can cancel from "+from+" if the vendor has not delivered", "Deliver by "+from) || strings.Contains(page, `action="/orders/cancel"`) {
		t.Fatalf("the buyer's page before the delivery deadline:\n%s", page)
	}
	if panel := x.panel(order, x.vendorSess); !containsAll(panel, "Deliver by "+from, "72 hours", "48 hours") {
		t.Fatalf("the vendor's panel does not state the delivery deadline and the dispute window:\n%s", panel)
	}
	if code, body := x.xmrBuyerCancel(order, buyerSess); code != 409 || !containsAll(body, from, "delivered") {
		t.Fatalf("a cancel before the delivery deadline: %d %s", code, body)
	}
	if x.state(order) != statePaid || len(x.xmrSettlements(order)) != 0 {
		t.Fatal("the refused cancel changed the order")
	}
	x.forceDeadline(order, "ship_by=now()-interval '1 minute'")
	page = html.UnescapeString(x.page("/order?id="+order, buyerSess))
	if !containsAll(page, `action="/orders/cancel"`, "Cancelling refunds the 0.5 fakechain XMR", `name="amount_seen" value="500000000000"`) {
		t.Fatalf("the buyer's page once the delivery deadline passed:\n%s", page)
	}
	for _, sess := range []string{buyerSess, x.vendorSess} {
		if panel := x.panel(order, sess); !strings.Contains(panel, "the buyer may now cancel it") {
			t.Fatalf("the panel once the delivery deadline passed:\n%s", panel)
		}
	}
	if code, body := x.xmrBuyerCancel(order, buyerSess); code != 303 {
		t.Fatalf("the cancel after the delivery deadline: %d %s", code, body)
	}
	if got := x.xmrSettlements(order); x.state(order) != stateCancelled || len(got) != 1 || got[0] != "refund requested "+testXMRAddr("refund "+order, true) {
		t.Fatalf("after the cancel: order %s, settlements %v", x.state(order), got)
	}
	deliver := func(order string) (int, string) {
		return x.xmrPost("/orders/deliver", x.vendorSess, url.Values{"order_id": {order}, "content": {"licence key MAN-0001"}})
	}
	if code, body := deliver(order); code != 409 || !containsAll(body, "The buyer cancelled this order at", "not delivered within 72 hours", "Do not send the content") {
		t.Fatalf("the vendor's delivery after the buyer's cancel: %d %s", code, body)
	}

	// A delivery before the deadline: the dispute window opens on delivery and the cancel is gone.
	on, onSess := x.fundedXMR(manual)
	before = time.Now()
	if code, body := deliver(on); code != 303 {
		t.Fatalf("the vendor's delivery: %d %s", code, body)
	}
	if x.state(on) != stateDelivered {
		t.Fatalf("a manual delivery left the order %s", x.state(on))
	}
	window := x.escrowTime(on, "release_at")
	near(t, "the dispute window", window, before.Add(48*time.Hour))
	ev := strings.Join(x.events(on), "\n")
	if !containsAll(ev, "paid>delivered "+x.vendor+" Digital content delivered by vendor", "Dispute window opened on delivery (Monero escrow)", utcUntil(window)) ||
		strings.Contains(ev, "on funding") || strings.Contains(ev, ">completed") {
		t.Fatalf("the history of a manual delivery:\n%s", ev)
	}
	noBitcoinWords(t, "the history of a manual delivery", ev)
	buyer := x.partyOf(on, roleBuyer)
	if got := x.noticesOf(buyer, on); !anyContains(got, "Order "+shortID(on), utcUntil(window), "open a dispute", "Confirm receipt") {
		t.Fatalf("the buyer's notices of the delivery: %q", got)
	}
	if got := x.noticesOf(x.vendor, on); !anyContains(got, "Order "+shortID(on), utcUntil(window), "claim") {
		t.Fatalf("the vendor's notices of the delivery: %q", got)
	}
	if code, _ := x.xmrBuyerCancel(on, onSess); code != 409 || x.state(on) != stateDelivered {
		t.Fatalf("a buyer cancel of a delivered order: %d; order %s", code, x.state(on))
	}
	if strings.Contains(x.page("/order?id="+on, onSess), `action="/orders/cancel"`) {
		t.Fatal("a delivered order offers the buyer a cancel form")
	}
}

// The vendor's delivery and the buyer's cancel past the delivery deadline race on the order row: exactly one takes
// effect, alternately, and the other is told which one did and when (TestXMREscrowShipNoShipCancelRaceOneOutcome's rule).
func TestXMREscrowDeliverCancelRaceOneOutcome(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	manual := x.manualDigital(x.vendor)
	wins := map[string]int{}
	for i := range 4 {
		order, buyerSess := x.fundedXMR(manual)
		x.forceDeadline(order, "ship_by=now()-interval '1 minute'")
		tx, err := x.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec("SELECT 1 FROM orders WHERE id=$1 FOR UPDATE", order); err != nil {
			t.Fatal(err)
		}
		var deliverCode, cancelCode int
		var deliverBody, cancelBody string
		var wg sync.WaitGroup
		deliver := func() {
			defer wg.Done()
			deliverCode, deliverBody = x.xmrPost("/orders/deliver", x.vendorSess, url.Values{"order_id": {order}, "content": {"licence key RACE-" + strconv.Itoa(i)}})
		}
		cancel := func() { defer wg.Done(); cancelCode, cancelBody = x.xmrBuyerCancel(order, buyerSess) }
		first, second := deliver, cancel
		if i%2 == 1 {
			first, second = cancel, deliver
		}
		wg.Add(2)
		go first()
		x.waitForLockWaits(1)
		go second()
		x.waitForLockWaits(2)
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		switch {
		case deliverCode == 303 && cancelCode == 409:
			wins["deliver"]++
			if x.state(order) != stateDelivered || !containsAll(cancelBody, "was delivered at", "can no longer be cancelled") || len(x.xmrSettlements(order)) != 0 {
				t.Fatalf("delivery won: order %s, the cancel was told %s", x.state(order), cancelBody)
			}
		case cancelCode == 303 && deliverCode == 409:
			wins["cancel"]++
			if x.state(order) != stateCancelled || !strings.Contains(deliverBody, "The buyer cancelled this order at") ||
				agInt(x.testEnv, "SELECT count(*) FROM deliveries WHERE order_id=$1", order) != 0 || len(x.xmrSettlements(order)) != 1 {
				t.Fatalf("cancel won: order %s, the delivery was told %s", x.state(order), deliverBody)
			}
		default:
			t.Fatalf("deliver %d %s; cancel %d %s; order %s", deliverCode, deliverBody, cancelCode, cancelBody, x.state(order))
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state='paid' AND to_state<>'paid'", order); n != 1 {
			t.Fatalf("%d moves out of paid, want 1", n)
		}
	}
	if wins["deliver"] == 0 || wins["cancel"] == 0 {
		t.Fatalf("each side should win in turn: %v", wins)
	}
}

// A-714 (PAY-2): ship_by is immutable (migration 079), so after a restore from backup a paid order's restored delivery
// deadline or ship-by time may have passed although the vendor delivered or shipped it after the backup was taken.
// While the restore's recheck is pending (lateDispute's exception), the buyer's no-ship cancel is refused 409 with the
// reason, the buyer's page offers no cancel form and says why, and the panel does not say the buyer may cancel now. The
// Bitcoin recheck's setting is not the Monero one; the vendor's cancel is unaffected; once the vendor's grace after the recheck has ended (A-720) the
// buyer cancels as before.
func TestXMREscrowCancelWaitsForRecheck(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	manual := x.manualDigital(x.vendor)
	digital, digitalSess := x.fundedXMR(manual)
	physical, physicalSess := x.paidXMRPhysical(x.physical(x.vendor))
	byVendor, _ := x.fundedXMR(manual)
	for _, o := range []string{digital, physical, byVendor} {
		x.forceDeadline(o, "ship_by=now()-interval '1 minute'")
	}
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", releaseRecheckKey)
	if page := x.page("/order?id="+digital, digitalSess); !strings.Contains(page, `action="/orders/cancel"`) {
		t.Fatal("a pending Bitcoin recheck hid a Monero order's cancel form")
	}
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", xmrReleaseRecheckKey)
	for _, c := range []struct{ order, sess string }{{digital, digitalSess}, {physical, physicalSess}} {
		page := html.UnescapeString(x.page("/order?id="+c.order, c.sess))
		if strings.Contains(page, `action="/orders/cancel"`) || !containsAll(page, "being reconciled after a restore from backup", "cancel") ||
			strings.Contains(page, "the buyer may now cancel it") {
			t.Fatalf("the buyer's page while the recheck is pending:\n%s", page)
		}
		if code, body := x.xmrBuyerCancel(c.order, c.sess); code != 409 || !containsAll(body, "records are being reconciled", "Nothing was changed") {
			t.Fatalf("a buyer cancel while the recheck is pending: %d %s", code, body)
		}
		if x.state(c.order) != statePaid || len(x.xmrSettlements(c.order)) != 0 {
			t.Fatal("the refused cancel changed the order")
		}
	}
	if code, body := x.xmrPost("/orders/cancel", x.vendorSess, url.Values{"order_id": {byVendor}, "from": {statePaid}, "payout_confirmed": {"confirmed"},
		"amount_seen": {strconv.FormatInt(xmrListingPrice, 10)}}); code != 303 || x.state(byVendor) != stateCancelled {
		t.Fatalf("the vendor's cancel while the recheck is pending: %d %s %s", code, x.state(byVendor), body)
	}
	// The recheck runs on the next pass and consumes the request: the buyer's cancel still waits, for the vendor's grace
	// (A-720, TestXMREscrowCancelGraceAfterRecheck), and then works as before.
	x.mustPoll()
	if agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key=$1", xmrReleaseRecheckKey) != 0 {
		t.Fatal("the recheck left its request")
	}
	if code, body := x.xmrBuyerCancel(digital, digitalSess); code != 409 || x.state(digital) != statePaid || !strings.Contains(body, "You can cancel this order from") {
		t.Fatalf("the buyer's cancel right after the recheck: %d %s %s", code, x.state(digital), body)
	}
	x.expireCancelWait()
	if page := x.page("/order?id="+digital, digitalSess); !strings.Contains(page, `action="/orders/cancel"`) {
		t.Fatal("no cancel form once the vendor's grace has ended")
	}
	if code, body := x.xmrBuyerCancel(digital, digitalSess); code != 303 || x.state(digital) != stateCancelled {
		t.Fatalf("the buyer's cancel once the vendor's grace has ended: %d %s %s", code, x.state(digital), body)
	}
}

// expireCancelWait makes the recorded grace after a restore's recheck (A-720) end a minute ago.
func (x *xmrEscrowEnv) expireCancelWait() {
	x.t.Helper()
	agExec(x.testEnv, "UPDATE settings SET value=to_char((now()-interval '1 minute') AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') WHERE key=$1", xmrCancelWaitKey)
}

// cancelWaitUntil is the recorded end of the grace after a restore's recheck (A-720).
func (x *xmrEscrowEnv) cancelWaitUntil() time.Time {
	x.t.Helper()
	var at time.Time
	if err := x.DB.QueryRow("SELECT value::timestamptz FROM settings WHERE key=$1", xmrCancelWaitKey).Scan(&at); err != nil {
		x.t.Fatal(err)
	}
	return at
}

// A-720 (PAY-3, PAY-4): on the documented restore order the first pass after the gate clears consumes the recheck before
// public access returns, so A-714's refusal alone protects nobody. That transaction records a grace of 72 hours: a
// buyer's cancel of an order whose ship-by time or delivery deadline passes before its end is refused 409 and not
// offered, on the buyer's page and the vendor's desk; the vendor's shipment or delivery in that time succeeds; after it
// the cancel works as before; an order paid after the recheck keeps its own deadline. A later restore's recheck extends
// the grace and never shortens it, whatever the restored row holds.
func TestXMREscrowCancelGraceAfterRecheck(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	manual := x.manualDigital(x.vendor)
	physicalProduct := x.physical(x.vendor)
	digital, digitalSess := x.fundedXMR(manual)
	physical, physicalSess := x.paidXMRPhysical(physicalProduct)
	delivered, deliveredSess := x.fundedXMR(manual)
	shipped, _ := x.paidXMRPhysical(physicalProduct)
	soon, soonSess := x.paidXMRPhysical(physicalProduct) // its restored ship-by time passes during the grace
	late, lateSess := x.paidXMRPhysical(physicalProduct) // its restored ship-by time is after the grace
	for _, o := range []string{digital, physical, delivered, shipped} {
		x.forceDeadline(o, "ship_by=now()-interval '1 minute'")
	}
	x.forceDeadline(soon, "ship_by=now()+interval '1 hour'")
	x.forceDeadline(late, "ship_by=now()+interval '100 hours'")

	// A restored row of an older restore is replaced: its end has passed.
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'2020-01-01T00:00:00.000000Z')", xmrCancelWaitKey)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", xmrReleaseRecheckKey)
	before := x.dbNow()
	x.mustPoll()
	if agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key=$1", xmrReleaseRecheckKey) != 0 {
		t.Fatal("the recheck left its request")
	}
	until := x.cancelWaitUntil()
	if until.Before(before.Add(72*time.Hour)) || until.After(x.dbNow().Add(72*time.Hour)) {
		t.Fatalf("the grace ends %s, want 72 hours after the recheck, near %s", until, before)
	}
	from := utcFrom(until)
	for _, c := range []struct{ order, sess, word string }{{digital, digitalSess, "delivered"}, {physical, physicalSess, "shipped"}, {soon, soonSess, "shipped"}} {
		if code, body := x.xmrBuyerCancel(c.order, c.sess); code != 409 || !containsAll(body, "You can cancel this order from "+from, "restored its records from a backup", "Nothing was changed") {
			t.Fatalf("a buyer's cancel of %s in the grace: %d %s", c.order, code, body)
		}
		if x.state(c.order) != statePaid || len(x.xmrSettlements(c.order)) != 0 {
			t.Fatal("the refused cancel changed the order")
		}
		page := html.UnescapeString(x.page("/order?id="+c.order, c.sess))
		if strings.Contains(page, `action="/orders/cancel"`) || !containsAll(page, "You can cancel from "+from, "has not "+c.word) ||
			strings.Contains(page, "the buyer may now cancel it") {
			t.Fatalf("the buyer's page in the grace:\n%s", page)
		}
		if vendorPage := html.UnescapeString(x.page("/order?id="+c.order, x.vendorSess)); !containsAll(vendorPage, "waits until "+from, "after that backup was taken") {
			t.Fatalf("the vendor's page in the grace:\n%s", vendorPage)
		}
	}
	// The vendor's desk says the cancel waits, not that the buyer may cancel.
	desk := html.UnescapeString(x.page("/vendor-dashboard", x.vendorSess))
	if !strings.Contains(desk, "the buyer can cancel from "+from) || strings.Contains(desk, "passed, so the buyer may cancel") {
		t.Fatalf("the vendor's desk in the grace:\n%s", desk)
	}
	// The vendor's shipment and delivery in the grace succeed, past the restored deadlines.
	if code, body := x.xmrShip(shipped, x.vendorSess, testXMRAddr("claim "+shipped, true), testPassword); code != 303 || x.state(shipped) != stateShipped {
		t.Fatalf("the vendor's shipment in the grace: %d %s %s", code, x.state(shipped), body)
	}
	if code, body := x.xmrPost("/orders/deliver", x.vendorSess, url.Values{"order_id": {delivered}, "content": {"licence key GRACE"}}); code != 303 || x.state(delivered) != stateDelivered {
		t.Fatalf("the vendor's delivery in the grace: %d %s %s", code, x.state(delivered), body)
	}
	if code, body := x.xmrBuyerCancel(delivered, deliveredSess); code != 409 || !strings.Contains(body, "was delivered at") {
		t.Fatalf("a buyer's cancel after the delivery in the grace: %d %s", code, body)
	}
	// A restored ship-by time after the grace keeps its own refusal; so does an order paid after the recheck.
	paidAfter, paidAfterSess := x.paidXMRPhysical(physicalProduct)
	digitalAfter, digitalAfterSess := x.fundedXMR(manual)
	for _, c := range []struct{ order, sess string }{{late, lateSess}, {paidAfter, paidAfterSess}, {digitalAfter, digitalAfterSess}} {
		var at time.Time
		if err := x.DB.QueryRow("SELECT ship_by FROM escrows WHERE order_id=$1", c.order).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if !at.After(until) {
			t.Fatalf("the deadline of order %s, %s, is not after the grace's end %s", c.order, at, until)
		}
		if code, body := x.xmrBuyerCancel(c.order, c.sess); code != 409 || !strings.Contains(body, "You can cancel this order from "+utcFrom(at)) || strings.Contains(body, "backup") {
			t.Fatalf("a buyer's cancel of an order with a deadline after the grace: %d %s", code, body)
		}
		if page := html.UnescapeString(x.page("/order?id="+c.order, c.sess)); !strings.Contains(page, "You can cancel from "+utcFrom(at)) || strings.Contains(page, "restored its records") {
			t.Fatalf("the buyer's page of an order with a deadline after the grace:\n%s", page)
		}
	}

	// A second restore while the grace runs: while its recheck is pending the cancel says so; a restored row that ends
	// later is not shortened, and one that ends sooner is extended.
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrReleaseRecheckKey)
	if code, body := x.xmrBuyerCancel(soon, soonSess); code != 409 || !strings.Contains(body, "records are being reconciled") {
		t.Fatalf("a buyer's cancel of an order in the grace with another recheck pending: %d %s", code, body)
	}
	agExec(x.testEnv, "UPDATE settings SET value='2099-01-01T00:00:00.000000Z' WHERE key=$1", xmrCancelWaitKey)
	x.mustPoll()
	if at := x.cancelWaitUntil(); !at.Equal(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("a recheck shortened the grace to %s", at)
	}
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrReleaseRecheckKey)
	agExec(x.testEnv, "UPDATE settings SET value=to_char((now()+interval '1 hour') AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') WHERE key=$1", xmrCancelWaitKey)
	before = x.dbNow()
	x.mustPoll()
	if at := x.cancelWaitUntil(); at.Before(before.Add(72 * time.Hour)) {
		t.Fatalf("a later recheck left the grace ending %s, want 72 hours after %s", at, before)
	}

	// After the grace the buyer cancels as before, and the row left behind changes nothing.
	x.expireCancelWait()
	x.forceDeadline(soon, "ship_by=now()-interval '1 minute'")
	for _, c := range []struct{ order, sess string }{{physical, physicalSess}, {soon, soonSess}, {digital, digitalSess}} {
		if page := x.page("/order?id="+c.order, c.sess); !strings.Contains(page, `action="/orders/cancel"`) {
			t.Fatalf("no cancel form once the grace has ended:\n%s", page)
		}
		if code, body := x.xmrBuyerCancel(c.order, c.sess); code != 303 || x.state(c.order) != stateCancelled || len(x.xmrSettlements(c.order)) != 1 {
			t.Fatalf("the buyer's cancel once the grace has ended: %d %s %s", code, x.state(c.order), body)
		}
	}
	if page := html.UnescapeString(x.page("/order?id="+late, x.vendorSess)); strings.Contains(page, "waits until") {
		t.Fatalf("the vendor's page says a cancel waits after the grace:\n%s", page)
	}
}

// After a restore from backup the first pass moves a delivered digital order's dispute window, and a shipped order's
// release deadline, that has passed or ends within 72 hours to 72 hours from then, with one note and one notice to the
// buyer each, before the deadline step could complete it (A-728: the recheck runs before public access returns, so a
// deadline just short of passing needs the grace too). A later deadline and an order with a dispute are left alone, a
// second pass moves nothing, and a buyer can still confirm early.
func TestXMREscrowRestoreExtendsDigitalWindow(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	order, _ := x.deliveredXMR(x.product)
	closing, _ := x.deliveredXMR(x.product)
	shipped, shippedSess, _ := x.shippedXMR(physical)
	soon, _, _ := x.shippedXMR(physical)
	later, _ := x.deliveredXMR(x.product)
	laterShipped, _, _ := x.shippedXMR(physical)
	disputed, _, _ := x.shippedXMR(physical)
	// Set once every order exists: creating one polls, and a poll completes what is due while no recheck is pending.
	x.forceDeadline(order, "release_at=now()-interval '2 days'")
	x.forceDeadline(closing, "release_at=now()+interval '4 seconds'")
	x.forceDeadline(shipped, "release_at=now()+interval '4 seconds'")
	x.forceDeadline(soon, "release_at=now()+interval '71 hours'")
	x.forceDeadline(later, "release_at=now()+interval '73 hours'")
	x.forceDeadline(laterShipped, "release_at=now()+interval '5 days'")
	x.forceDeadline(disputed, "release_at=now()+interval '1 hour'")
	agExec(x.testEnv, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'A dispute row written beside a shipped order.')", randomToken(), disputed)
	kept := map[string]time.Time{}
	for _, o := range []string{later, laterShipped, disputed} {
		kept[o] = x.releaseAt(o)
	}
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", xmrReleaseRecheckKey)
	before := x.dbNow()
	x.mustPoll()

	moved := map[string]time.Time{}
	for _, o := range []string{order, closing, shipped, soon} {
		at := x.releaseAt(o)
		if at.Before(before.Add(72 * time.Hour)) {
			t.Fatalf("order %s's deadline after the recheck ends %s, want at least 72 hours after %s", shortID(o), at, before)
		}
		moved[o] = at
	}
	for _, o := range []string{order, closing} {
		if x.state(o) != stateDelivered {
			t.Fatalf("a delivered order after the recheck: %s", x.state(o))
		}
		notes := agStrings(t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1 AND from_state='delivered' AND to_state='delivered' AND strpos(note,'after a restore from backup')>0", o)
		notices := x.noticesOf(x.partyOf(o, roleBuyer), o)
		if len(notes) != 1 || !containsAll(notes[0], "Dispute window moved to "+utcUntil(moved[o]), "has passed or ends within 72 hours") ||
			countContaining(notices, "restored its records from a backup") != 1 ||
			!anyContains(notices, "dispute window of this order has passed or ends within 72 hours", utcUntil(moved[o]), "delivered content works") {
			t.Fatalf("the recheck's note and notice: %q %q", notes, notices)
		}
		// VEN103-1: its vendor is told the same date and cause, and what follows for them.
		x.wantVendorMoved(o, "restored its records from a backup", "dispute window", "the order completes at that time and you can claim the coins in this Monero escrow from the order page")
		if p := x.panel(o, x.vendorSess); !strings.Contains(p, restoreMovedLine) || strings.Contains(p, outageMovedLine) {
			t.Fatalf("the vendor's panel after a restore's recheck:\n%s", p)
		}
	}
	for _, o := range []string{shipped, soon} {
		if x.state(o) != stateShipped {
			t.Fatalf("a shipped order after the recheck: %s", x.state(o))
		}
		notes := agStrings(t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1 AND from_state='shipped' AND to_state='shipped' AND strpos(note,'after a restore from backup')>0", o)
		notices := x.noticesOf(x.partyOf(o, roleBuyer), o)
		if len(notes) != 1 || !containsAll(notes[0], "Release deadline moved to "+utcUntil(moved[o]), "has passed or ends within 72 hours") ||
			countContaining(notices, "restored its records from a backup") != 1 ||
			!anyContains(notices, "release deadline of this order has passed or ends within 72 hours", utcUntil(moved[o]), "goods have arrived") {
			t.Fatalf("the recheck's note and notice: %q %q", notes, notices)
		}
		x.wantVendorMoved(o, "restored its records from a backup", "release deadline", "the order completes at that time and the market releases the coins in this Monero escrow to you")
	}
	for o, at := range kept {
		if !x.releaseAt(o).Equal(at) || anyContains(x.events(o), "after a restore from backup") {
			t.Fatalf("the recheck touched order %s: %s, was %s; %q", shortID(o), x.releaseAt(o), at, x.events(o))
		}
		if n := x.movedNotices(x.vendor, o); len(n) != 0 {
			t.Fatalf("the vendor of an order not moved was told: %q", n)
		}
	}

	// A second pass moves nothing and repeats nothing, and the original few seconds passing completes nothing.
	time.Sleep(5 * time.Second)
	x.mustPoll()
	for o, at := range moved {
		if !x.releaseAt(o).Equal(at) || anyContains(x.events(o), ">completed") {
			t.Fatalf("the second pass changed order %s: %s, was %s; %q", shortID(o), x.releaseAt(o), at, x.events(o))
		}
		if n := countContaining(x.noticesOf(x.partyOf(o, roleBuyer), o), "restored its records from a backup"); n != 1 {
			t.Fatalf("%d restore notices for order %s after the second pass", n, shortID(o))
		}
		if n := x.movedNotices(x.vendor, o); len(n) != 1 {
			t.Fatalf("the vendor's notices for order %s after the second pass: %q", shortID(o), n)
		}
	}
	// Only the buyer completes it sooner.
	if code, body := x.xmrConfirm(shipped, shippedSess, testPassword); code != 303 || x.state(shipped) != stateCompleted {
		t.Fatalf("the buyer's confirmation after the recheck: %d %s %s", code, x.state(shipped), body)
	}
	// A later restore's recheck moves a deadline the last one moved, though release_recheck is already true on it (migration
	// 079's trigger needs it to go from false to true, so the recheck resets it first), and leaves a completed order alone.
	completedAt := x.releaseAt(shipped)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrReleaseRecheckKey)
	before = x.dbNow()
	x.mustPoll()
	if at := x.releaseAt(soon); at.Before(before.Add(72*time.Hour)) || !at.After(moved[soon]) || x.state(soon) != stateShipped {
		t.Fatalf("a later recheck left the deadline at %s (%s), want 72 hours after %s", at, x.state(soon), before)
	}
	if !x.releaseAt(shipped).Equal(completedAt) || x.state(shipped) != stateCompleted {
		t.Fatalf("a later recheck touched a completed order: %s %s", x.releaseAt(shipped), x.state(shipped))
	}
}

// countContaining counts the items of list that contain part.
func countContaining(list []string, part string) int {
	n := 0
	for _, s := range list {
		if strings.Contains(s, part) {
			n++
		}
	}
	return n
}

// BE103-1: a paid order whose only deposit the wallet no longer lists counts no coins, so its cancel form used to carry
// neither field the action requires, and its one button answered 400. The form now carries both, and says nothing is
// refunded; the action still refuses a forged post without the tick, a buyer before the ship-by time still gets no form,
// and a form shown at zero is refused once the deposit is back. A deposit back after the cancel goes by a stray refund.
func TestXMREscrowCancelFormAfterDepositVanished(t *testing.T) {
	const nothing = "No counted coins are in this escrow, so cancelling refunds nothing now"
	vanish := func(x *xmrEscrowEnv, order string) {
		x.t.Helper()
		x.dropTransfer(hex64("fund " + order))
		x.mustPoll()
		if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND confirmations=-1", order); n != 1 || x.state(order) != statePaid {
			x.t.Fatalf("the vanished deposit of a %s order: %v", x.state(order), x.creditedRows(order))
		}
	}
	cancelForm := func(x *xmrEscrowEnv, order, sess string) string {
		x.t.Helper()
		body := x.page("/order?id="+order, sess)
		i := strings.Index(body, `action="/orders/cancel"`)
		if i < 0 {
			return ""
		}
		form := body[i:]
		return html.UnescapeString(form[:strings.Index(form, "</form>")])
	}
	wantZeroForm := func(x *xmrEscrowEnv, order, sess string) {
		x.t.Helper()
		form := cancelForm(x, order, sess)
		if !containsAll(form, `name="amount_seen" value="0"`, `name="payout_confirmed" value="confirmed"`, nothing, "stray refund") {
			x.t.Fatalf("the cancel form with no counted coins:\n%s", form)
		}
		// A-770: the stock sentence stays, the coins sentence does not sit above a tick saying nothing is sent.
		if !containsAll(form, "of stock.", xmrListedAgainRefund, "a stray refund is") {
			x.t.Fatalf("the cancel form does not say what happens if the deposit is listed again:\n%s", form)
		}
		mustNotContain(x.t, form, "The coins in this escrow go back", "builds the refund transaction", "deposit returns")
	}
	post := func(x *xmrEscrowEnv, order, sess string, extra url.Values) (int, string) {
		x.t.Helper()
		form := url.Values{"order_id": {order}, "from": {statePaid}}
		for k, v := range extra {
			form[k] = v
		}
		return x.xmrPost("/orders/cancel", sess, form)
	}
	ticked := url.Values{"payout_confirmed": {"confirmed"}, "amount_seen": {"0"}}
	wantClosedNothingSent := func(x *xmrEscrowEnv, order string) {
		x.t.Helper()
		if x.state(order) != stateCancelled {
			x.t.Fatalf("the cancelled order is %s", x.state(order))
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order); n != 0 {
			x.t.Fatalf("%d settlements on a cancel of no counted coins", n)
		}
	}
	addressOf := func(x *xmrEscrowEnv, order string) string {
		return agStr(x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)
	}

	t.Run("vendor", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, _ := x.paidXMRPhysical(x.physical(x.vendor))
		txid := hex64("fund " + order)
		vanish(x, order)
		wantZeroForm(x, order, x.vendorSess)
		// A forged post without the tick stays refused, with or without the amount.
		for _, forged := range []url.Values{{}, {"amount_seen": {"0"}}} {
			if code, body := post(x, order, x.vendorSess, forged); code != 400 || x.state(order) != statePaid || !strings.Contains(body, "Tick the box") {
				t.Fatalf("a post without the tick: %d %s %s", code, x.state(order), body)
			}
		}
		// The deposit back before the form is posted: zero is no longer what the form must state.
		x.fake.payAddress(addressOf(x, order), txid, xmrListingPrice, 30)
		x.mustPoll()
		if code, body := post(x, order, x.vendorSess, ticked); code != 409 || x.state(order) != statePaid || !strings.Contains(body, "review again") {
			t.Fatalf("a form at zero once the deposit is back: %d %s %s", code, x.state(order), body)
		}
		if form := cancelForm(x, order, x.vendorSess); strings.Contains(form, nothing) || !containsAll(form, `name="amount_seen" value="`+strconv.FormatInt(xmrListingPrice, 10)+`"`, "The coins in this escrow go back to the buyer's refund address given at checkout") {
			t.Fatalf("the form with the deposit back:\n%s", form)
		}
		// Gone again, the cancel goes through and nothing is sent.
		vanish(x, order)
		wantZeroForm(x, order, x.vendorSess)
		if code, body := post(x, order, x.vendorSess, ticked); code != 303 {
			t.Fatalf("the form's own post: %d %s", code, body)
		}
		wantClosedNothingSent(x, order)
		// After the reload the panel neither says nothing was received nor promises the order's refund, also once the deposit
		// is back and still confirming (BE103V-2): no refund was requested, so a stray refund returns it once it has confirmed.
		const cancelledAtZero = "no deposit to this escrow counted when the order was cancelled, so nothing was refunded"
		wantPanel := func(when string) {
			t.Helper()
			if p := x.panel(order, x.vendorSess); !containsAll(p, cancelledAtZero, "by a stray refund once it has confirmed") ||
				strings.Contains(p, "nothing was received") || strings.Contains(p, "by a refund the market builds and sends") {
				t.Fatalf("the panel %s:\n%s", when, p)
			}
		}
		wantPanel("after the cancel")
		x.fake.payAddress(addressOf(x, order), txid, xmrListingPrice, 2)
		x.mustPoll()
		wantClosedNothingSent(x, order)
		if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND confirmations=2 AND NOT escrow_left_out", order); n != 1 {
			t.Fatalf("the deposit back at 2 confirmations: %v", x.creditedRows(order))
		}
		wantPanel("with the deposit back at 2 confirmations")
	})

	// A-740: a form that stated the price, posted after the deposit vanished, is refused with the form again at the amount
	// counted now, and nothing changes; that form then succeeds.
	t.Run("stale form", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, _ := x.paidXMRPhysical(x.physical(x.vendor))
		vanish(x, order)
		price := url.Values{"order_id": {order}, "from": {statePaid}, "payout_confirmed": {"confirmed"}, "amount_seen": {strconv.FormatInt(xmrListingPrice, 10)}}
		x.refused(order, "/orders/cancel", x.vendorSess, price, 409, "The amount changed to 0 XMR")
		w := x.do("POST", "/orders/cancel", x.vendorSess, price)
		body := html.UnescapeString(w.Body.String())
		if w.Code != 409 || !containsAll(body, `<div class="alert error" role="alert">The amount changed to 0 XMR`, `action="/orders/cancel"`, `name="amount_seen" value="0"`, nothing) {
			t.Fatalf("the re-rendered form: %d\n%s", w.Code, body)
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order); n != 0 || x.state(order) != statePaid {
			t.Fatalf("a refused cancel changed the order: %s, %d settlements", x.state(order), n)
		}
		if code, body := post(x, order, x.vendorSess, ticked); code != 303 {
			t.Fatalf("the re-rendered form's own post: %d %s", code, body)
		}
		wantClosedNothingSent(x, order)
	})

	// A-740: a staff decision follows the same rule: the form at zero says zero and can succeed by posting it, for either
	// outcome, and one that stated the price is refused with the dispute left open.
	// BE103V-1: the same on an order awaiting payment: a form that stated a partial deposit, posted after it vanished, is
	// refused with the form again and nothing changed, a tick is still required, and a form with no amount still cancels.
	t.Run("stale awaiting form", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, addr := x.freshCheckout(x.physical(x.vendor))
		txid := hex64("part " + order)
		x.fake.payAddress(addr, txid, xmrListingPrice/4, 12)
		x.mustPoll()
		x.dropTransfer(txid)
		x.mustPoll()
		if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND confirmations=-1", order); n != 1 || x.state(order) != stateAwaitingPayment {
			t.Fatalf("the vanished deposit of a %s order: %v", x.state(order), x.creditedRows(order))
		}
		sess := x.sessOf(order, roleBuyer)
		partial := strconv.FormatInt(xmrListingPrice/4, 10)
		stale := url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, "payout_confirmed": {"confirmed"}, "amount_seen": {partial}}
		x.refused(order, "/orders/cancel", sess, stale, 409, "The amount changed to 0 XMR")
		w := x.do("POST", "/orders/cancel", sess, stale)
		body := html.UnescapeString(w.Body.String())
		if w.Code != 409 || !containsAll(body, `<div class="alert error" role="alert">The amount changed to 0 XMR`, `action="/orders/cancel"`) || strings.Contains(body, `name="amount_seen" value="`+partial+`"`) {
			t.Fatalf("the re-rendered form: %d\n%s", w.Code, body)
		}
		x.refused(order, "/orders/cancel", sess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, "amount_seen": {partial}}, 400, "Tick the box")
		if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order); n != 0 || x.state(order) != stateAwaitingPayment {
			t.Fatalf("a refused cancel changed the order: %s, %d settlements", x.state(order), n)
		}
		if code, body := x.xmrPost("/orders/cancel", sess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}}); code != 303 {
			t.Fatalf("a form with no amount: %d %s", code, body)
		}
		wantClosedNothingSent(x, order)
	})

	t.Run("staff", func(t *testing.T) {
		for _, outcome := range []string{"refund", "release"} {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			order, sess := x.paidXMRPhysical(x.physical(x.vendor))
			if code, body := x.xmrDispute(order, sess); code != 303 {
				t.Fatalf("dispute: %d %s", code, body)
			}
			x.dropTransfer(hex64("fund " + order))
			x.mustPoll()
			if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND confirmations=-1", order); n != 1 || x.state(order) != stateDisputed {
				t.Fatalf("the vanished deposit of a %s order: %v", x.state(order), x.creditedRows(order))
			}
			dispute := agStr(x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)
			_, admin := x.user("zero_judge_"+outcome, "admin")
			page := html.UnescapeString(x.page("/moderator", admin))
			if !containsAll(page, `action="/resolve"`, `name="amount_seen" value="0"`, "no counted coins are in this escrow, so nothing is sent now", xmrListedAgainRefund, "The market sends nothing now") {
				t.Fatalf("the staff form at zero:\n%s", page)
			}
			// A-770: neither outcome nor the tick says these coins are sent.
			mustNotContain(t, page, "The market sends these coins", "the escrowed 0", "On its next pass after you resolve", "Resolving requests the settlement")
			resolve := func(seen string) url.Values {
				return url.Values{"id": {dispute}, "outcome": {outcome}, "resolution": {"Decided from the evidence both parties sent."},
					"payout_confirmed": {"confirmed"}, "amount_seen": {seen}, "reply_seen": {"0"}, "password": {testPassword}}
			}
			x.refused(order, "/resolve", admin, resolve(strconv.FormatInt(xmrListingPrice, 10)), 409, "The amount changed to 0 XMR")
			if status := agStr(x.testEnv, "SELECT status FROM disputes WHERE id=$1", dispute); status != "Open" || x.state(order) != stateDisputed {
				t.Fatalf("a refused decision: dispute %s, order %s", status, x.state(order))
			}
			if code, body := x.xmrPost("/resolve", admin, resolve("0")); code != 303 || x.state(order) != stateResolved {
				t.Fatalf("the form's own post at zero: %d %s %s", code, x.state(order), body)
			}
			if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order); n != 0 {
				t.Fatalf("%d settlements on a decision of no counted coins", n)
			}
			// A-770: the panel and the Resolved notice say nothing is sent, and what happens if the deposit is listed again.
			vsess := x.sessOf(order, roleVendor)
			for _, who := range []string{roleBuyer, roleVendor} {
				n := x.noticesOf(x.partyOf(order, who), order)
				if !anyContains(n, "is now Resolved", "No counted coins are in this escrow, so nothing is sent now", "if the network lists the deposit again") {
					t.Fatalf("the %s's notices for %s: %q", who, outcome, n)
				}
				p := x.panel(order, x.sessOf(order, who))
				mustContain(t, p, "no deposit to this escrow", "so nothing is sent now")
				mustNotContain(t, p, "the coins in this escrow go back", "by a refund the market builds and sends", "the vendor claims the escrowed coins", "Funded")
			}
			txid, addr := hex64("fund "+order), addressOf(x, order)
			if outcome == "refund" {
				mustContain(t, x.panel(order, sess), xmrListedAgainRefund)
				// What it said: the deposit listed again goes by a stray refund, and no ordinary refund appears.
				x.fake.payAddress(addr, txid, xmrListingPrice, 30)
				x.mustPoll()
				x.mustPoll()
				if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='stray_refund'", order); n != 1 || agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='refund'", order) != 0 {
					t.Fatalf("the deposit listed again: %v; events %q", x.xmrSettlements(order), x.events(order))
				}
				p := x.panel(order, sess)
				mustContain(t, p, "no deposit to this escrow counted when the dispute was decided", "by a stray refund")
				mustNotContain(t, p, "so nothing is sent now", xmrListedAgainRefund)
				continue
			}
			// PM-E1: a claim at zero is accepted, says it waits, stays requested and pays once the deposit is listed again.
			mustContain(t, x.panel(order, vsess), "The vendor can claim", "if the network lists the deposit again")
			form := x.panel(order, vsess)
			form = form[strings.Index(form, "Claim the escrowed coins"):]
			mustContain(t, form, "No deposit to this escrow counts now, so the market sends nothing now")
			w := x.do("POST", "/escrow-xmr/claim", vsess, url.Values{"order_id": {order}, "address": {testXMRAddr("claim "+order, true)}, "password": {testPassword}})
			if w.Code != 303 {
				t.Fatalf("a claim at zero: %d %s", w.Code, w.Body.String())
			}
			if n := html.UnescapeString(userPageNotice(t, x.testEnv, w, vsess, w.Header().Get("Location"))); !strings.Contains(n, notices["escrow-xmr-claim-waits"].text) {
				t.Fatalf("the claim's notice: %s", n)
			}
			for _, when := range []string{"before a pass", "after one pass", "after two passes"} {
				if s := x.settlementOf(order, "release"); s.State != "requested" {
					t.Fatalf("the release %s: %+v", when, s)
				}
				p := x.panel(order, vsess)
				mustContain(t, p, "no deposit to this escrow counts now, so nothing is sent now", "if the network lists the deposit again")
				mustNotContain(t, p, "requested when the order was shipped pays the vendor", "sends it on its next pass")
				x.mustPoll()
			}
			if ev := x.events(order); !anyContains(ev, "no deposit to this escrow counts now") || anyContains(ev, "and sends it. Nothing is sent yet") {
				t.Fatalf("the claim's event at zero: %q", ev)
			}
			x.fake.payAddress(addr, txid, xmrListingPrice, 30)
			x.mustPoll()
			x.mustPoll()
			if s := x.settlementOf(order, "release"); s.State != "relayed" {
				t.Fatalf("the release with the deposit listed again: %+v", s)
			}
			mustNotContain(t, x.panel(order, vsess), "so nothing is sent now", "no deposit to this escrow counts now")
		}
	})

	// A-770: a party's concession at zero states the same: nothing is sent now, and how a deposit listed again goes.
	t.Run("concede", func(t *testing.T) {
		for _, c := range []struct{ party, outcome, to, again string }{
			{roleBuyer, "release", stateCompleted, "it goes to the vendor"},
			{roleVendor, "refund", stateCancelled, xmrListedAgainRefund},
		} {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			order, buyer := x.paidXMRPhysical(x.physical(x.vendor))
			if code, body := x.xmrDispute(order, buyer); code != 303 {
				t.Fatalf("dispute: %d %s", code, body)
			}
			x.dropTransfer(hex64("fund " + order))
			x.mustPoll()
			dispute := agStr(x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)
			sess, other := x.sessOf(order, c.party), roleVendor
			if c.party == roleVendor {
				other = roleBuyer
			}
			raw := x.page("/order?id="+order, sess)
			page := html.UnescapeString(raw)
			mustContain(t, page, "no counted coins are in this escrow, so nothing is sent now", c.again)
			mustNotContain(t, page, "the escrowed 0", "Funded", "The decision is paid from this escrow")
			x.check(x.do("POST", "/disputes/concede", sess, concessionForm(dispute, c.outcome, renderedFormNonce(t, raw, "/disputes/concede"), 0)), 303)
			if x.state(order) != c.to {
				t.Fatalf("the conceded order is %s", x.state(order))
			}
			if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order); n != 0 {
				t.Fatalf("%d settlements on a concession of no counted coins", n)
			}
			if n := x.noticesOf(x.partyOf(order, other), order); !anyContains(n, "is now "+stateLabel(c.to), "No counted coins are in this escrow, so nothing is sent now", "if the network lists the deposit again") {
				t.Fatalf("the %s's notices: %q", other, n)
			}
			after := html.UnescapeString(x.page("/order?id="+order, sess))
			mustNotContain(t, after, "Funded and completed", "receives the refund", "receives the release", "The coins in this escrow go back", "the vendor claims the escrowed coins")
			mustContain(t, after, "if the network lists the deposit again", "No counted coins are in this escrow, so nothing is sent now")
		}
	})

	// QA-A770-1: the Mark shipped form, the shipped panel and the confirm at zero say nothing is sent, and what follows if
	// the deposit is listed again; so does the shipped notice.
	t.Run("ship", func(t *testing.T) {
		const zero = "No deposit to this escrow counts now"
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, sess := x.paidXMRPhysical(x.physical(x.vendor))
		txid := hex64("fund " + order)
		vanish(x, order)
		p := x.panel(order, x.vendorSess)
		form := p[strings.Index(p, `id="xmr-escrow-ship"`):]
		form = form[:strings.Index(form, "</section>")]
		mustContain(t, form, zero+", so nothing is sent now", "if the network lists the deposit again", "nothing is paid to you")
		mustNotContain(t, form, "requests the release to you of the coins in this escrow")
		if code, body := x.xmrShip(order, x.vendorSess, testXMRAddr("claim "+order, true), testPassword); code != 303 {
			t.Fatalf("ship at zero: %d %s", code, body)
		}
		if n := x.noticesOf(x.partyOf(order, roleBuyer), order); !anyContains(n, "is now Shipped", zero) {
			t.Fatalf("the buyer's shipped notice: %q", n)
		}
		for _, when := range []string{"before a pass", "after a pass"} {
			for _, viewer := range []string{sess, x.vendorSess} {
				p := x.panel(order, viewer)
				mustContain(t, p, "Shipped; no deposit to this escrow counts now", zero+", so nothing is sent now")
				mustNotContain(t, p, "Funded", "the market releases the coins in this escrow to the vendor")
			}
			if strings.Count(x.panel(order, sess), "so nothing is sent") < 2 {
				t.Fatalf("the settlement row %s:\n%s", when, x.panel(order, sess))
			}
			x.mustPoll()
		}
		w := x.do("POST", "/escrow-xmr/confirm", sess, url.Values{"order_id": {order}, "password": {testPassword}})
		x.check(w, 303)
		flash := html.UnescapeString(userPageNotice(t, x.testEnv, w, sess, w.Header().Get("Location")))
		mustContain(t, flash, notices["escrow-xmr-confirm-waits"].text)
		mustNotContain(t, flash, notices["escrow-xmr-confirmed"].text)
		n := x.noticesOf(x.partyOf(order, roleVendor), order)
		if !anyContains(n, "is now Completed", "so nothing is sent now", "if the network lists the deposit again") || anyContains(n, "and sends it.") {
			t.Fatalf("the vendor's completed notice: %q", n)
		}
		if ev := x.events(order); !anyContains(ev, "so nothing is sent now") || anyContains(ev, xmrConfirmedNote) {
			t.Fatalf("the confirm's event: %q", ev)
		}
		x.fake.payAddress(addressOf(x, order), txid, xmrListingPrice, 30)
		x.mustPoll()
		x.mustPoll()
		if s := x.settlementOf(order, "release"); s.State != "relayed" {
			t.Fatalf("the release once the deposit is listed again: %+v", s)
		}
	})

	// QA-A770-1: the confirm of a delivered digital order at zero, and its flash.
	t.Run("digital confirm", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, sess := x.deliveredXMR(x.digital(x.vendor))
		vanish0 := hex64("fund " + order)
		x.dropTransfer(vanish0)
		x.mustPoll()
		form := x.panel(order, sess)
		mustContain(t, form[strings.Index(form, `id="xmr-escrow-confirm"`):], "No deposit to this escrow counts now")
		mustNotContain(t, form, "the vendor can claim the coins in this escrow; nothing is paid")
		w := x.do("POST", "/escrow-xmr/confirm", sess, url.Values{"order_id": {order}, "password": {testPassword}})
		x.check(w, 303)
		flash := html.UnescapeString(userPageNotice(t, x.testEnv, w, sess, w.Header().Get("Location")))
		mustContain(t, flash, notices["escrow-xmr-receive-waits"].text)
		if n := x.noticesOf(x.partyOf(order, roleVendor), order); !anyContains(n, "is now Completed", "so nothing is sent now", "who claims it") {
			t.Fatalf("the vendor's notice: %q", n)
		}
	})

	// QA-A770-1: the watcher's completion at zero tells both parties nothing is sent, for a shipped and a delivered order.
	t.Run("auto completion", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		shipped, _, _ := x.shippedXMR(x.physical(x.vendor))
		delivered, _ := x.deliveredXMR(x.digital(x.vendor))
		x.dropTransfer(hex64("fund " + shipped))
		x.dropTransfer(hex64("fund " + delivered))
		x.mustPoll()
		x.forceDeadline(shipped, "release_at=now()-interval '1 minute'")
		x.forceDeadline(delivered, "release_at=now()-interval '1 minute'")
		if err := x.A.xmrReleaseDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		for order, again := range map[string]string{shipped: xmrListedAgainLive, delivered: xmrListedAgainClaim} {
			if x.state(order) != stateCompleted {
				t.Fatalf("the order is %s", x.state(order))
			}
			for _, who := range []string{roleBuyer, roleVendor} {
				n := x.noticesOf(x.partyOf(order, who), order)
				if !anyContains(n, "is now Completed", "so nothing is sent now", again) || anyContains(n, "and sends it.") || anyContains(n, xmrClaimCue) {
					t.Fatalf("the %s's completion notice: %q", who, n)
				}
			}
			mustContain(t, x.panel(order, x.sessOf(order, roleVendor)), "Completed; no deposit to this escrow counts now")
		}
	})

	t.Run("buyer", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, sess := x.paidXMRPhysical(x.physical(x.vendor))
		txid := hex64("fund " + order)
		vanish(x, order)
		if form := cancelForm(x, order, sess); form != "" {
			t.Fatalf("a buyer before the ship-by time got a form:\n%s", form)
		}
		if code, body := post(x, order, sess, ticked); code == 303 || x.state(order) != statePaid {
			t.Fatalf("a buyer's post before the ship-by time: %d %s %s", code, x.state(order), body)
		}
		x.forceDeadline(order, "ship_by=now()-interval '1 minute'")
		wantZeroForm(x, order, sess)
		if code, body := post(x, order, sess, url.Values{}); code != 400 || x.state(order) != statePaid {
			t.Fatalf("a post without the tick: %d %s %s", code, x.state(order), body)
		}
		if code, body := post(x, order, sess, ticked); code != 303 {
			t.Fatalf("the form's own post: %d %s", code, body)
		}
		wantClosedNothingSent(x, order)

		// The deposit returns after the cancel: what the form says happens. It is left out for a stray refund to the buyer's
		// refund address, the buyer is told, and no ordinary refund appears.
		x.fake.payAddress(addressOf(x, order), txid, xmrListingPrice, 30)
		x.mustPoll()
		x.mustPoll()
		if x.state(order) != stateCancelled {
			t.Fatalf("the deposit's return reopened the order: %s", x.state(order))
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out AND confirmations>=0", order); n != 1 {
			t.Fatalf("the returned deposit: %v", x.creditedRows(order))
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='refund'", order); n != 0 {
			t.Fatalf("%d ordinary refunds", n)
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='stray_refund'", order); n != 1 {
			t.Fatalf("%d stray refunds; events %q", n, x.events(order))
		}
		if n := x.noticesOf(x.partyOf(order, roleBuyer), order); !anyContains(n, "a stray refund returns them") {
			t.Fatalf("the buyer's notices: %q", n)
		}
	})

	t.Run("digital buyer", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, sess := x.undeliveredXMR()
		vanish(x, order)
		if form := cancelForm(x, order, sess); form != "" {
			t.Fatalf("a buyer before the delivery deadline got a form:\n%s", form)
		}
		x.forceDeadline(order, "ship_by=now()-interval '1 minute'")
		wantZeroForm(x, order, sess)
		if code, body := post(x, order, sess, ticked); code != 303 {
			t.Fatalf("the form's own post: %d %s", code, body)
		}
		wantClosedNothingSent(x, order)
	})
}

// A-770: a paid order whose credited deposit is no longer listed stays paid, and its panel says so in hedged words: no
// Funded, no refund of "the escrow", and the deposit row says what to check; with two deposits and one gone too. Once the
// deposit is listed again the panel is exactly what it was. Physical and digital, before and after the deadline, so the
// sentences of the cancel the buyer may use are read too.
func TestXMREscrowPaidOrderDepositVanishedTellsBuyer(t *testing.T) {
	hedged := func(t *testing.T, p string) {
		t.Helper()
		mustContain(t, p, "may have been dropped or replaced on the network", "whatever is counted then is refunded",
			"Check this transaction in your own wallet", "If the network lists it again, it counts again")
		mustNotContain(t, p, "Funded", "the escrow is refunded", "the escrow is then refunded", "claim the coins")
	}
	for _, kind := range []string{"physical", "digital"} {
		t.Run(kind, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			var order, sess string
			if kind == "physical" {
				order, sess = x.paidXMRPhysical(x.physical(x.vendor))
			} else {
				order, sess = x.undeliveredXMR()
			}
			before := x.panel(order, sess)
			mustContain(t, before, "Funded", "the escrow is refunded")
			mustNotContain(t, before, "may have been dropped", "whatever is counted")
			txid, addr := hex64("fund "+order), agStr(x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)
			x.dropTransfer(txid)
			x.mustPoll()
			x.mustPoll()
			if x.state(order) != statePaid {
				t.Fatalf("the order is %s", x.state(order))
			}
			hedged(t, x.panel(order, sess))
			if vendor := x.panel(order, x.sessOf(order, roleVendor)); !containsAll(vendor, "may have been dropped", "The buyer can check this transaction in their own wallet", "If the network lists it again, it counts again") {
				t.Fatalf("the vendor's panel:\n%s", vendor)
			}
			x.fake.payAddress(addr, txid, xmrListingPrice, 10)
			x.mustPoll()
			if after := x.panel(order, sess); after != before {
				t.Fatalf("the panel with the deposit listed again differs:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			// Past the deadline, gone again: the same words, and the cancel offered to the buyer says what it refunds.
			x.dropTransfer(txid)
			x.mustPoll()
			x.forceDeadline(order, "ship_by=now()-interval '1 minute'")
			p := x.panel(order, sess)
			hedged(t, p)
			mustContain(t, p, "whatever is counted then is refunded to the buyer's refund address")
		})
	}
	t.Run("two deposits, one gone", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, addr := x.freshCheckout(x.physical(x.vendor))
		first, second := hex64("first "+order), hex64("second "+order)
		x.fake.payAddress(addr, first, xmrListingPrice/2, 10)
		x.fake.payAddress(addr, second, xmrListingPrice-xmrListingPrice/2, 10)
		x.mustPoll()
		if x.state(order) != statePaid {
			t.Fatalf("funded in two deposits: %s", x.state(order))
		}
		sess := x.sessOf(order, roleBuyer)
		x.dropTransfer(second)
		x.mustPoll()
		x.mustPoll()
		p := x.panel(order, sess)
		hedged(t, p)
		mustContain(t, p, "0.25 XMR")
		if n := strings.Count(p, "Check this transaction in your own wallet"); n != 1 {
			t.Fatalf("%d deposit rows say to check; only the missing one should:\n%s", n, p)
		}
		// The vendor's cancel form has coins to refund, so it states them.
		form := html.UnescapeString(x.page("/order?id="+order, x.vendorSess))
		mustContain(t, form, "Cancelling refunds the 0.25", "The coins in this escrow go back to the buyer's refund address given at checkout")
	})
	// QA-A770-3: past paid too, with no transaction yet: no status says Funded, and the confirm form is hedged. Listed again,
	// the release is built and the status is the funded one.
	t.Run("shipped, one of two gone", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, addr := x.freshCheckout(x.physical(x.vendor))
		first, second := hex64("first "+order), hex64("second "+order)
		x.fake.payAddress(addr, first, xmrListingPrice/2, 10)
		x.fake.payAddress(addr, second, xmrListingPrice-xmrListingPrice/2, 10)
		x.mustPoll()
		sess := x.sessOf(order, roleBuyer)
		if code, body := x.xmrShip(order, x.vendorSess, testXMRAddr("claim "+order, true), testPassword); code != 303 {
			t.Fatalf("ship: %d %s", code, body)
		}
		funded := x.panel(order, sess)
		mustContain(t, funded, "Funded and shipped")
		mustNotContain(t, funded, "may have been dropped", "Check this transaction")
		x.dropTransfer(second)
		x.mustPoll()
		wantHedged := func(state string) {
			t.Helper()
			p := x.panel(order, sess)
			mustContain(t, p, state+", but a deposit to this escrow may have been dropped or replaced on the network", "Check this transaction in your own wallet", "If the network lists it again, it counts again")
			mustNotContain(t, p, "Funded")
		}
		wantHedged("Shipped")
		p := x.panel(order, sess)
		mustContain(t, p[strings.Index(p, `id="xmr-escrow-confirm"`):], "may have been dropped or replaced on the network")
		// Completed: confirmed while it is gone; the release waits for the deposit, so no transaction exists yet.
		if code, body := x.xmrConfirm(order, sess, testPassword); code != 303 {
			t.Fatalf("confirm: %d %s", code, body)
		}
		wantHedged("Completed")
		x.mustPoll()
		if s := x.settlementOf(order, "release"); s.TxHash != "" {
			t.Fatalf("the release is built with a deposit missing: %+v", s)
		}
		wantHedged("Completed")
		x.fake.payAddress(addr, second, xmrListingPrice-xmrListingPrice/2, 10)
		x.mustPoll()
		x.mustPoll()
		if s := x.settlementOf(order, "release"); s.TxHash == "" {
			t.Fatalf("the release with the deposit listed again: %+v", s)
		}
		p = x.panel(order, sess)
		mustContain(t, p, "Funded and completed")
		mustNotContain(t, p, "may have been dropped", "Check this transaction")
	})
	// QA-A770-5: past the wait the release is built from the deposit that remains. It does not read Funded, says it was
	// paid from less than the price, and the row says what becomes of the deposit (xmrMissingFate), which no longer counts
	// again. Listed again it stays with staff, and the release it missed is still short.
	t.Run("built short past the wait", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, addr := x.freshCheckout(x.physical(x.vendor))
		first, second := hex64("first "+order), hex64("second "+order)
		x.fake.payAddress(addr, first, xmrListingPrice/2, 10)
		x.fake.payAddress(addr, second, xmrListingPrice-xmrListingPrice/2, 10)
		x.mustPoll()
		sess := x.sessOf(order, roleBuyer)
		if code, body := x.xmrShip(order, x.vendorSess, testXMRAddr("claim "+order, true), testPassword); code != 303 {
			t.Fatalf("ship: %d %s", code, body)
		}
		x.dropTransfer(second)
		x.mustPoll()
		if code, body := x.xmrConfirm(order, sess, testPassword); code != 303 {
			t.Fatalf("confirm: %d %s", code, body)
		}
		x.fake.mine(int(2 * x.A.escrowXMR.confirmations))
		x.mustPoll()
		x.mustPoll()
		if s := x.settlementOf(order, "release"); s.State != "relayed" || s.InputTotal != xmrListingPrice/2 {
			t.Fatalf("the release past the wait: %+v", s)
		}
		const short = "Completed, but the release is paid from less than the price: it spends 0.25 fakechain XMR of the 0.5 fakechain XMR price, and was built without a deposit that counted when the order was funded."
		fate := "The release was built without it. " + xmrMissingFate("release")
		p := x.panel(order, sess)
		mustContain(t, p, short, xmrDepositMissing+". Check this transaction in your own wallet. "+fate)
		mustNotContain(t, p, "Funded", "may have been dropped", "it counts again")
		vendor := x.panel(order, x.vendorSess)
		mustContain(t, vendor, short, xmrDepositMissing+". The buyer can check this transaction in their own wallet. "+fate)
		mustNotContain(t, vendor, "Funded", "may have been dropped", "it counts again")
		x.fake.payAddress(addr, second, xmrListingPrice-xmrListingPrice/2, 30)
		x.mustPoll()
		p = x.panel(order, sess)
		mustContain(t, p, short)
		mustNotContain(t, p, "Funded", xmrDepositMissing)
	})
	t.Run("decided for the vendor, built short past the wait", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, addr := x.freshCheckout(x.physical(x.vendor))
		first, second := hex64("first "+order), hex64("second "+order)
		x.fake.payAddress(addr, first, xmrListingPrice/2, 10)
		x.fake.payAddress(addr, second, xmrListingPrice-xmrListingPrice/2, 10)
		x.mustPoll()
		sess := x.sessOf(order, roleBuyer)
		if code, body := x.xmrShip(order, x.vendorSess, testXMRAddr("claim "+order, true), testPassword); code != 303 {
			t.Fatalf("ship: %d %s", code, body)
		}
		x.dropTransfer(second)
		x.mustPoll()
		x.disputeAndResolve(order, "release")
		x.fake.mine(int(2 * x.A.escrowXMR.confirmations))
		x.mustPoll()
		x.mustPoll()
		if s := x.settlementOf(order, "release"); s.State != "relayed" || s.InputTotal != xmrListingPrice/2 {
			t.Fatalf("the release past the wait: %+v", s)
		}
		p := x.panel(order, sess)
		mustContain(t, p, "Resolved, but the release is paid from less than the price", "The release was built without it. "+xmrMissingFate("release"))
		mustNotContain(t, p, "Funded", "it counts again")
	})
	// A release built in full whose deposit is gone afterwards spent the price: it reads as funded, with no hedge, because a
	// transaction exists (ShortCounted is cleared).
	t.Run("gone after a full release", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, addr := x.freshCheckout(x.physical(x.vendor))
		first, second := hex64("first "+order), hex64("second "+order)
		x.fake.payAddress(addr, first, xmrListingPrice/2, 10)
		x.fake.payAddress(addr, second, xmrListingPrice-xmrListingPrice/2, 10)
		x.mustPoll()
		sess := x.sessOf(order, roleBuyer)
		if code, body := x.xmrShip(order, x.vendorSess, testXMRAddr("claim "+order, true), testPassword); code != 303 {
			t.Fatalf("ship: %d %s", code, body)
		}
		if code, body := x.xmrConfirm(order, sess, testPassword); code != 303 {
			t.Fatalf("confirm: %d %s", code, body)
		}
		x.mustPoll()
		x.mustPoll()
		if s := x.settlementOf(order, "release"); s.State != "relayed" || s.InputTotal != xmrListingPrice {
			t.Fatalf("the full release: %+v", s)
		}
		mustContain(t, x.panel(order, sess), "Funded and completed")
		x.dropTransfer(second)
		x.mustPoll()
		if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND confirmations<0", order); n != 1 {
			t.Fatalf("%d deposits read missing", n)
		}
		p := x.panel(order, sess)
		mustContain(t, p, "Funded and completed", "<td>"+xmrDepositMissing+"</td>")
		mustNotContain(t, p, "less than the price", "may have been dropped", "Check this transaction", "it counts again")
	})
	// One of two deposits to a delivered order gone: the delivered status and the confirm form are hedged.
	t.Run("delivered, one of two gone", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, addr := x.freshCheckout(x.digital(x.vendor))
		first, second := hex64("first "+order), hex64("second "+order)
		x.fake.payAddress(addr, first, xmrListingPrice/2, 10)
		x.fake.payAddress(addr, second, xmrListingPrice-xmrListingPrice/2, 10)
		x.mustPoll()
		if x.state(order) != stateDelivered {
			t.Fatalf("funded in two deposits: %s", x.state(order))
		}
		sess := x.sessOf(order, roleBuyer)
		funded := x.panel(order, sess)
		mustContain(t, funded, "Funded and delivered")
		mustNotContain(t, funded, "may have been dropped", "Check this transaction")
		x.dropTransfer(second)
		x.mustPoll()
		p := x.panel(order, sess)
		mustContain(t, p, "Delivered, but a deposit to this escrow may have been dropped or replaced on the network", "Check this transaction in your own wallet", "If the network lists it again, it counts again")
		mustContain(t, p[strings.Index(p, `id="xmr-escrow-confirm"`):], "the release pays what is counted when it is built")
		mustNotContain(t, p, "Funded")
	})
	// One of two deposits to a paid physical order gone: Mark shipped tells the vendor to check before sending the goods.
	t.Run("Mark shipped, one of two gone", func(t *testing.T) {
		const check = "A deposit to this escrow may have been dropped or replaced on the network, so less than the price counts now: check the deposit before you send the goods."
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, addr := x.freshCheckout(x.physical(x.vendor))
		first, second := hex64("first "+order), hex64("second "+order)
		x.fake.payAddress(addr, first, xmrListingPrice/2, 10)
		x.fake.payAddress(addr, second, xmrListingPrice-xmrListingPrice/2, 10)
		x.mustPoll()
		form := func() string {
			t.Helper()
			p := x.panel(order, x.vendorSess)
			i := strings.Index(p, `id="xmr-escrow-ship"`)
			if i < 0 {
				t.Fatalf("no Mark shipped form:\n%s", p)
			}
			return p[i:]
		}
		mustNotContain(t, form(), check)
		x.dropTransfer(second)
		x.mustPoll()
		mustContain(t, form(), check)
	})
	// A vanished extra deposit on an order the rest still pays for in full is not a short order: it still reads Funded.
	t.Run("an extra deposit gone", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, addr := x.freshCheckout(x.physical(x.vendor))
		extra := hex64("extra " + order)
		x.fake.payAddress(addr, hex64("fund "+order), xmrListingPrice, 10)
		x.fake.payAddress(addr, extra, xmrListingPrice/4, 10)
		x.mustPoll()
		sess := x.sessOf(order, roleBuyer)
		x.dropTransfer(extra)
		x.mustPoll()
		p := x.panel(order, sess)
		mustContain(t, p, "Funded", "the escrow is refunded", "Conflicted or missing — not counted")
		mustNotContain(t, p, "may have been dropped", "whatever is counted", "Check this transaction")
	})
}

// Migration 153 gives a digital Monero escrow order paid before A-709, and still paid, a delivery deadline 72 hours from
// the upgrade, so its buyer can cancel once it passes; a deadline already set, a completed order and a physical order
// are left as they are, and running it again changes nothing.
func TestXMREscrowDigitalDeadlineBackfill(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	paid, paidSess := x.undeliveredXMR()
	completed, _ := x.completedXMR(x.product)
	physical, _ := x.paidXMRPhysical(x.physical(x.vendor))
	kept, _ := x.undeliveredXMR()
	keptAt := x.escrowTime(kept, "ship_by")
	for _, order := range []string{paid, completed, physical} {
		x.forceDeadline(order, "ship_by=NULL") // as before A-709 (a physical order is never without one; here a control)
	}
	migration, err := migrationFiles.ReadFile("migrations/153_digital_delivery_deadline.sql")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	for range 2 {
		if _, err = x.DB.Exec(string(migration)); err != nil {
			t.Fatalf("migration 153: %v", err)
		}
	}
	near(t, "the backfilled delivery deadline", x.escrowTime(paid, "ship_by"), before.Add(72*time.Hour))
	if !x.escrowTime(kept, "ship_by").Equal(keptAt) {
		t.Fatal("migration 153 moved a delivery deadline already set")
	}
	for _, order := range []string{completed, physical} {
		if n := agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND ship_by IS NOT NULL", order); n != 0 {
			t.Fatalf("migration 153 set the ship-by time of a %s order", x.state(order))
		}
	}
	if code, body := x.xmrBuyerCancel(paid, paidSess); code != 409 || !strings.Contains(body, utcFrom(x.escrowTime(paid, "ship_by"))) {
		t.Fatalf("a cancel before the backfilled deadline: %d %s", code, body)
	}
}

// A vendor's ban moves a delivered digital order's dispute window to at least 72 hours after the ban (A-652d's rule
// for a release deadline), tells its buyer once with a delivered -> delivered note, and moves no money; when the window
// ends the order completes and its payment is held (A-652c), with no claim possible for the banned vendor.
func TestXMRBanExtendsDigitalWindow(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("banwin_admin", "admin")
	order, sess := x.deliveredXMR(x.product)
	before := x.dbNow()
	x.banVendor(admin)
	at := x.releaseAt(order)
	if at.Before(before.Add(72 * time.Hour)) {
		t.Fatalf("the dispute window after the ban ends %s, want at least 72 hours after %s", at, before)
	}
	notices := x.banNotices(order)
	notes := agStrings(t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1 AND from_state='delivered' AND to_state='delivered' AND strpos(note,$2)>0", order, banDeadlineText)
	if len(notices) != 1 || !containsAll(notices[0], utcUntil(at), "delivered content does not work", "held") || len(notes) != 1 || !strings.Contains(notes[0], "dispute window moved to "+utcUntil(at)) {
		t.Fatalf("the ban's notice %q and notes %q", notices, notes)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE action LIKE 'Account banned by %' AND strpos(action,$1)>0", "1 shipped or delivered order(s)"); n != 1 {
		t.Fatalf("%d ban audit rows name the delivered order", n)
	}
	if p := x.panel(order, sess); !strings.Contains(p, "the market holds its payment") {
		t.Fatalf("the buyer's panel after the ban:\n%s", p)
	}
	x.endDisputeWindow(order)
	if !anyContains(x.events(order), "delivered>completed system "+digitalWindowNote, xmrHeldNote) || len(x.xmrSettlements(order)) != 0 {
		t.Fatalf("after the window: %q %v", x.events(order), x.xmrSettlements(order))
	}
}

// Criterion 2: an automatic delivery on funding leaves the order delivered for 48 hours. The buyer is offered a
// dispute and Confirm receipt; the vendor is offered no claim, and a claim is refused. A dispute in the window goes to
// an administrator's decision; Confirm receipt completes it early; with neither, the watcher completes it when the
// window ends ("dispute window ended"), and the vendor then claims.
func TestXMREscrowDigitalDisputeWindow(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("xadm_"+randomToken()[:6], "admin")
	before := time.Now()
	order, buyerSess := x.deliveredXMR(x.product)
	window := x.escrowTime(order, "release_at")
	near(t, "the dispute window", window, before.Add(48*time.Hour))
	until := utcUntil(window)
	if n := agInt(x.testEnv, "SELECT count(*) FROM deliveries WHERE order_id=$1", order); n != 1 {
		t.Fatalf("the automatic delivery wrote %d deliveries", n)
	}
	ev := strings.Join(x.events(order), "\n")
	if !containsAll(ev, "Automatic delivery after confirmed payment", "Dispute window opened on funding (Monero escrow)", until) || strings.Contains(ev, ">completed") {
		t.Fatalf("the history of an automatic delivery:\n%s", ev)
	}
	buyerPage := html.UnescapeString(x.page("/order?id="+order, buyerSess))
	if !containsAll(buyerPage, `action="/escrow-xmr/confirm"`, `action="/disputes"`, "Dispute window", until) || strings.Contains(buyerPage, `action="/escrow-xmr/claim"`) {
		t.Fatalf("the buyer's page in the dispute window:\n%s", buyerPage)
	}
	noBitcoinWords(t, "the buyer's Monero escrow panel", x.panel(order, buyerSess))
	for _, sess := range []string{buyerSess, x.vendorSess} {
		if page := html.UnescapeString(x.page("/orders", sess)); !containsAll(page, "Dispute window open.", "Order "+shortID(order), until) {
			t.Fatalf("/orders has no dispute-window banner:\n%s", page)
		}
	}
	vendorPage := html.UnescapeString(x.page("/order?id="+order, x.vendorSess))
	if strings.Contains(vendorPage, `action="/escrow-xmr/claim"`) || !containsAll(vendorPage, until, "claim") {
		t.Fatalf("the vendor's page in the dispute window:\n%s", vendorPage)
	}
	claim := testXMRAddr("claim "+order, true)
	if code, body := x.xmrClaim(order, x.vendorSess, claim, testPassword); code != 409 || !strings.Contains(body, errClaimState.Error()) {
		t.Fatalf("a claim in the dispute window: %d %s", code, body)
	}
	if got := x.xmrSettlements(order); len(got) != 0 {
		t.Fatalf("a refused claim wrote %v", got)
	}
	// A pass before the window ends changes nothing.
	x.mustPoll()
	if x.state(order) != stateDelivered {
		t.Fatalf("a pass in the window left the order %s", x.state(order))
	}

	// No dispute: the watcher's pass completes it at the window's end, also while Monero escrow is not ready (a node
	// outage: the pass then runs only the deadline step, which makes no RPC), and the vendor claims.
	x.A.escrowXMR.ready.Store(false)
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	calls := x.fake.count()
	_ = x.poll()
	x.A.escrowXMR.ready.Store(true)
	if x.state(order) != stateCompleted || x.fake.count() != calls {
		t.Fatalf("the window's end while Monero escrow was not ready: order %s, %d RPC calls", x.state(order), x.fake.count()-calls)
	}
	if !anyContains(x.events(order), "delivered>completed system", digitalWindowNote) {
		t.Fatalf("the completion note: %q", x.events(order))
	}
	if got := x.noticesOf(x.vendor, order); !anyContains(got, "Completed", digitalWindowNote, xmrClaimCue) {
		t.Fatalf("the vendor's completion notice does not cue the claim: %q", got)
	}
	if code, body := x.xmrClaim(order, x.vendorSess, claim, testPassword); code != 303 {
		t.Fatalf("the claim after completion: %d %s", code, body)
	}
	if got := x.xmrSettlements(order); len(got) != 1 || got[0] != "release requested "+claim {
		t.Fatalf("after the claim: %v", got)
	}

	// A dispute in the window: an administrator decides it, for the buyer here.
	disputed, disputedSess := x.deliveredXMR(x.product)
	if code, body := x.xmrDispute(disputed, disputedSess); code != 303 {
		t.Fatalf("a dispute in the dispute window: %d %s", code, body)
	}
	if got := x.noticesOf(x.vendor, disputed); !anyContains(got, "Disputed", "no longer applies") {
		t.Fatalf("the vendor's dispute notice: %q", got)
	}
	x.forceDeadline(disputed, "release_at=now()-interval '1 minute'")
	x.mustPoll()
	if x.state(disputed) != stateDisputed {
		t.Fatalf("the watcher completed a disputed order: %s", x.state(disputed))
	}
	dispute := agStrings(t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", disputed)[0]
	if code, body := x.xmrPost("/resolve", admin, url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"The delivered key was refused by the issuer."},
		"payout_confirmed": {"confirmed"}, "amount_seen": {strconv.FormatInt(xmrListingPrice, 10)}, "reply_seen": {"0"}, "password": {testPassword}}); code != 303 {
		t.Fatalf("the administrator's decision: %d %s", code, body)
	}
	if got := x.xmrSettlements(disputed); x.state(disputed) != stateResolved || len(got) != 1 || got[0] != "refund requested "+testXMRAddr("refund "+disputed, true) {
		t.Fatalf("after deciding for the buyer: order %s, settlements %v", x.state(disputed), got)
	}

	// Confirm receipt early: completed by the buyer, and the vendor may claim.
	early, earlySess := x.deliveredXMR(x.product)
	if code, body := x.xmrConfirm(early, x.vendorSess, testPassword); code != 403 || x.state(early) != stateDelivered {
		t.Fatalf("the vendor's Confirm receipt: %d %s", code, body)
	}
	if code, body := x.xmrConfirm(early, earlySess, testPassword); code != 303 {
		t.Fatalf("Confirm receipt in the dispute window: %d %s", code, body)
	}
	if x.state(early) != stateCompleted || !anyContains(x.events(early), "delivered>completed "+x.partyOf(early, roleBuyer)+" Receipt confirmed by the buyer (Monero escrow)") {
		t.Fatalf("after Confirm receipt: %s %q", x.state(early), x.events(early))
	}
	noBitcoinWords(t, "the digital Confirm receipt notice", notices["escrow-xmr-received"].text)
	if code, _ := x.xmrDispute(early, earlySess); code != 409 {
		t.Fatalf("a dispute after Confirm receipt: %d", code)
	}
	if page := html.UnescapeString(x.page("/order?id="+early, x.vendorSess)); !strings.Contains(page, `action="/escrow-xmr/claim"`) {
		t.Fatalf("the vendor is not offered the claim after Confirm receipt:\n%s", page)
	}
}

// Criterion 3 (and PM-4): once the dispute window of a delivered digital order, or the release deadline of a shipped
// physical one, has passed, the order page offers no dispute form and a dispute is refused, though the watcher has not
// completed the order yet. While a restore's recheck is pending the passed deadline is not yet due (A-393): the form
// is offered again.
func TestXMRDisputeFormHiddenAfterDeadline(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	digital, digitalSess := x.deliveredXMR(x.product)
	shipped, shippedSess, _ := x.shippedXMR(x.physical(x.vendor))
	for _, c := range []struct{ order, sess string }{{digital, digitalSess}, {shipped, shippedSess}} {
		if page := x.page("/order?id="+c.order, c.sess); !strings.Contains(page, `action="/disputes"`) {
			t.Fatalf("no dispute form before the deadline of %s:\n%s", x.state(c.order), page)
		}
		if page := x.page("/disputes", c.sess); !strings.Contains(page, `<option value="`+c.order+`"`) {
			t.Fatalf("/disputes does not offer a %s order before its deadline:\n%s", x.state(c.order), page)
		}
		x.forceDeadline(c.order, "release_at=now()-interval '1 minute'")
		for _, sess := range []string{c.sess, x.vendorSess} {
			if page := x.page("/order?id="+c.order, sess); strings.Contains(page, `action="/disputes"`) {
				t.Fatalf("a dispute form after the deadline of a %s order:\n%s", x.state(c.order), page)
			}
		}
		if code, body := x.xmrDispute(c.order, c.sess); code != 409 || !strings.Contains(body, "can no longer be opened") {
			t.Fatalf("a dispute after the deadline of a %s order: %d %s", x.state(c.order), code, body)
		}
		if page := x.page("/disputes", c.sess); strings.Contains(page, `<option value="`+c.order+`"`) {
			t.Fatalf("/disputes offers a %s order past its deadline:\n%s", x.state(c.order), page)
		}
	}
	if panel := x.panel(digital, digitalSess); !containsAll(panel, "dispute window", "ended", "next pass") {
		t.Fatalf("the panel of a digital order past its dispute window:\n%s", panel)
	}
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", xmrReleaseRecheckKey)
	if page := x.page("/order?id="+digital, digitalSess); !strings.Contains(page, `action="/disputes"`) {
		t.Fatalf("no dispute form while the restore recheck is pending:\n%s", page)
	}
	// A-720 (PAY-5): the panel says what happens to the deadline, not that the release is due or the order completes next.
	for _, c := range []struct{ order, sess string }{{digital, digitalSess}, {shipped, shippedSess}, {shipped, x.vendorSess}} {
		panel := x.panel(c.order, c.sess)
		if !containsAll(panel, "moves to 72 hours after", "a dispute can still be opened") ||
			strings.Contains(panel, "next pass") || strings.Contains(panel, "the release is due") || strings.Contains(panel, "can no longer be opened") {
			t.Fatalf("the panel of a %s order with the recheck pending:\n%s", x.state(c.order), panel)
		}
	}
}

// BE110-2: a deposit with an unlock time never goes back by a stray refund (xmrLeaveOutUncounted filters NOT locked), so
// a cancelled order whose only deposit is locked does not promise one.
func TestXMREscrowCancelledOnlyDepositLockedPromisesNoRefund(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, addr := x.freshCheckout(x.digital(x.vendor))
	txid := hex64("locked " + order)
	x.fake.payAddress(addr, txid, xmrListingPrice, 50)
	x.fake.setTransfer(txid, func(t *fakeXMRTransfer) { t.UnlockTime = 3000 })
	x.mustPoll()
	if n := x.notified(x.admin, "unlock time"); n == 0 || x.state(order) != stateAwaitingPayment {
		t.Fatalf("the locked deposit: order %s, %d staff notices", x.state(order), n)
	}
	sess := x.sessOf(order, roleBuyer)
	if code, body := x.xmrPost("/orders/cancel", sess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}}); code != 303 {
		t.Fatalf("cancel: %d %s", code, body)
	}
	for range 2 {
		p := x.panel(order, sess)
		mustContain(t, p, "Cancelled; no deposit to this escrow counted: a deposit with an unlock time is not counted and is not refunded automatically. Market staff were notified.")
		mustNotContain(t, p, "stray refund", "goes back", "nothing was refunded")
		x.mustPoll()
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order); n != 0 {
		t.Fatalf("%d settlements on the cancelled order", n)
	}
}
