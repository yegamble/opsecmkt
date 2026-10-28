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

// After a restore from backup the first pass moves a delivered digital order's passed dispute window to 72 hours from
// then, with a delivered -> delivered note and a notice to the buyer, before the deadline step could complete it.
func TestXMREscrowRestoreExtendsDigitalWindow(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _ := x.deliveredXMR(x.product)
	x.forceDeadline(order, "release_at=now()-interval '2 days'")
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", xmrReleaseRecheckKey)
	before := x.dbNow()
	x.mustPoll()
	at := x.releaseAt(order)
	if x.state(order) != stateDelivered || at.Before(before.Add(72*time.Hour-time.Minute)) {
		t.Fatalf("after the recheck: %s, window ends %s", x.state(order), at)
	}
	if !anyContains(x.events(order), "delivered>delivered system Dispute window moved to "+utcUntil(at)) ||
		!anyContains(x.noticesOf(x.partyOf(order, roleBuyer), order), "dispute window of this order had passed", utcUntil(at), "delivered content works") {
		t.Fatalf("the recheck's note and notice: %q %q", x.events(order), x.noticesOf(x.partyOf(order, roleBuyer), order))
	}
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
}
