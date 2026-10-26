package market

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A-652d (PAY87-4, PAY89-1; owner decision 2026-10-25 "refund + hold"): banning a vendor gives the buyer of each of its
// shipped Monero escrow orders with no dispute at least 72 hours from the ban to open one. The ban moves a sooner
// release deadline to 72 hours after the ban (never earlier; unban never shortens it) and tells each such buyer, for
// that order, that the vendor's account is closed, until when a dispute can be opened, and that the order otherwise
// completes with its payment held (A-652c). No money moves. DB integration tests against the simulated Monero wallet
// RPC, not chain evidence.

// banDeadlineText is how the ban's order note and buyer notice say what it did to a shipped order.
const banDeadlineText = "The vendor's account is closed"

// releaseAt is order's release deadline as the database holds it.
func (x *xmrEscrowEnv) releaseAt(order string) time.Time {
	x.t.Helper()
	var at time.Time
	if err := x.DB.QueryRow("SELECT release_at FROM escrows WHERE order_id=$1", order).Scan(&at); err != nil {
		x.t.Fatal(err)
	}
	return at
}

// dbNow is the database's clock.
func (x *xmrEscrowEnv) dbNow() time.Time {
	x.t.Helper()
	var now time.Time
	if err := x.DB.QueryRow("SELECT now()").Scan(&now); err != nil {
		x.t.Fatal(err)
	}
	return now
}

// banNotices lists the notifications of order's buyer, attached to order, that tell of the ban's deadline.
func (x *xmrEscrowEnv) banNotices(order string) []string {
	return agStrings(x.t, x.testEnv, "SELECT body FROM notifications WHERE user_id=$1 AND order_id=$2 AND strpos(body,$3)>0 ORDER BY created,id",
		x.partyOf(order, roleBuyer), order, banDeadlineText)
}

// banNotes lists order's history notes that tell of the ban's deadline.
func (x *xmrEscrowEnv) banNotes(order string) []string {
	return agStrings(x.t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1 AND from_state='shipped' AND to_state='shipped' AND actor_id IS NULL AND strpos(note,$2)>0 ORDER BY id",
		order, banDeadlineText)
}

// checkBanNotice fails unless order's buyer was told once, for that order, of the ban and of the deadline at, and the
// order history records it once.
func (x *xmrEscrowEnv) checkBanNotice(order string, at time.Time) {
	x.t.Helper()
	until := utcUntil(at)
	notices, notes := x.banNotices(order), x.banNotes(order)
	if len(notices) != 1 || !containsAll(notices[0], "Order "+shortID(order), until, "open a dispute", "held") {
		x.t.Fatalf("buyer's ban notices for %s (deadline %s): %q", shortID(order), until, notices)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], until) {
		x.t.Fatalf("ban notes of %s (deadline %s): %q", shortID(order), until, notes)
	}
}

// A ban moves a shipped order's passed or near release deadline to at least 72 hours after the ban, tells its buyer
// once for that order, and moves no money: the release requested at shipment stays requested and the watcher makes no
// spend call. The order stays shipped through the next passes, and the buyer's panel says the payment would be held.
func TestBanExtendsShippedDeadline(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("bandl_admin", "admin")
	phys := x.physical(x.vendor)
	passed, passedSess, _ := x.shippedXMR(phys)
	soon, _, _ := x.shippedXMR(phys)
	x.forceDeadline(passed, "release_at=now()-interval '1 minute'")
	x.forceDeadline(soon, "release_at=now()+interval '5 hours',release_recheck=true") // as if a restore had moved it
	calls := x.spendCalls()
	before := x.dbNow()

	x.banVendor(admin)
	after := x.dbNow()
	for _, order := range []string{passed, soon} {
		at := x.releaseAt(order)
		if at.Before(before.Add(72*time.Hour)) || at.After(after.Add(72*time.Hour)) {
			t.Fatalf("release deadline of %s after the ban: %s, want 72 hours after the ban (%s to %s)", shortID(order), at, before, after)
		}
		x.checkBanNotice(order, at)
		// Migration 079's flag is put back as it was: the order page says a restore moved the deadline only if one did.
		if got, want := agStr(x.testEnv, "SELECT release_recheck::text FROM escrows WHERE order_id=$1", order), strconv.FormatBool(order == soon); got != want {
			t.Fatalf("release_recheck of %s after the ban: %s, want %s", shortID(order), got, want)
		}
	}
	if p := x.panel(passed, passedSess); strings.Contains(p, "restored its records from a backup") {
		t.Fatalf("the ban's move reads as a restore's:\n%s", p)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE action LIKE 'Account banned by %' AND strpos(action,$1)>0", "2 shipped order(s)"); n != 1 {
		t.Fatalf("%d ban audit rows name the shipped orders", n)
	}
	for range 2 {
		x.mustPoll()
	}
	for _, order := range []string{passed, soon} {
		if x.state(order) != stateShipped {
			t.Fatalf("%s is %s after the ban and two passes", shortID(order), x.state(order))
		}
		if s := x.settlementOf(order, "release"); s.State != "requested" || x.liveOutcomes(order) != 1 {
			t.Fatalf("%s: release %+v, %d live outcomes", shortID(order), s, x.liveOutcomes(order))
		}
		// The ban's notice stands in for the 72-hour reminder, which would otherwise repeat it on the next pass.
		if n := x.notified(x.partyOf(order, roleBuyer), "Reminder"); n != 0 {
			t.Fatalf("%s: %d reminders after the ban's notice", shortID(order), n)
		}
	}
	if got := x.spendCalls(); got != calls {
		t.Fatalf("the ban moved money: %d spend calls, was %d", got, calls)
	}
	if p := x.panel(passed, passedSess); !containsAll(p, "open a dispute before "+utcUntil(x.releaseAt(passed)), "holds its payment", "no other address is ever paid") ||
		strings.Contains(p, "releases the coins in this escrow to the vendor") || strings.Contains(p, "the market releases the coins to the vendor") {
		t.Fatalf("the buyer's panel after the ban:\n%s", p)
	}
}

// A dispute filed after the ban within the extended window is accepted, though the original deadline had passed; a
// decision for the buyer supersedes the release requested at shipment (A-371) and pays the one refund, to the refund
// address the buyer gave at checkout. Nothing pays the vendor's claim address.
func TestBanShippedDisputeRefundsAfterBan(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("bandisp_admin", "admin")
	order, _, claim := x.shippedXMR(x.physical(x.vendor))
	release := x.settlementOf(order, "release").ID
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	x.banVendor(admin)
	x.mustPoll()
	x.mustPoll()
	if x.state(order) != stateShipped {
		t.Fatalf("the banned vendor's shipped order is %s after two passes", x.state(order))
	}
	x.disputeAndResolve(order, "refund")
	if x.state(order) != stateResolved {
		t.Fatalf("decided for the buyer: %s", x.state(order))
	}
	x.mustPoll()
	x.mustPoll()
	if s := x.settlement(release); s.State != "superseded" {
		t.Fatalf("the release requested at shipment: %+v", s)
	}
	refund := x.settlementOf(order, "refund")
	if want := []string{"claimant:" + x.refundAddressOf(order) + ":" + strconv.FormatInt(xmrListingPrice-refund.Fee, 10)}; refund.State != "relayed" ||
		!slices.Equal(x.outputsOf(refund.ID), want) {
		t.Fatalf("refund %+v outputs %v, want %v", refund, x.outputsOf(refund.ID), want)
	}
	if n := x.liveOutcomes(order); n != 1 {
		t.Fatalf("%d live outcomes", n)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND state<>'superseded' AND destination=$2", order, claim); n != 0 {
		t.Fatalf("%d live settlements pay the vendor's claim address", n)
	}
}

// A release deadline already more than 72 hours away is kept exactly; the buyer is still told, with that deadline.
func TestBanDoesNotShortenDeadline(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("bankeep_admin", "admin")
	order, _, _ := x.shippedXMR(x.physical(x.vendor))
	x.forceDeadline(order, "release_at=now()+interval '10 days'")
	at := x.releaseAt(order)
	x.banVendor(admin)
	if got := x.releaseAt(order); !got.Equal(at) {
		t.Fatalf("the ban moved a deadline 10 days away from %s to %s", at, got)
	}
	x.checkBanNotice(order, at)
}

// Banning the buyer of a shipped order changes no deadline and sends no deadline notice.
func TestBuyerBanLeavesDeadlines(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("banbuyer_admin", "admin")
	order, _, _ := x.shippedXMR(x.physical(x.vendor))
	x.forceDeadline(order, "release_at=now()+interval '5 hours'")
	at := x.releaseAt(order)
	agExpect(x.testEnv, "/admin/ban", admin, banForm("ban", agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", x.partyOf(order, roleBuyer)), testBanReason), 303, "")
	if got := x.releaseAt(order); !got.Equal(at) {
		t.Fatalf("banning the buyer moved the deadline from %s to %s", at, got)
	}
	if n, m := len(x.banNotices(order)), len(x.banNotes(order)); n != 0 || m != 0 {
		t.Fatalf("banning the buyer: %d deadline notices, %d notes", n, m)
	}
}

// A disputed order is decided by an administrator, so a ban neither moves its (no longer applying) deadline nor tells
// its buyer of one.
func TestBanLeavesDisputedShipped(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("bandisputed_admin", "admin")
	order, bsess, _ := x.shippedXMR(x.physical(x.vendor))
	x.forceDeadline(order, "release_at=now()+interval '5 hours'")
	if code, body := x.xmrDispute(order, bsess); code != 303 {
		t.Fatalf("dispute: %d %s", code, body)
	}
	at := x.releaseAt(order)
	x.banVendor(admin)
	if got := x.releaseAt(order); !got.Equal(at) || x.state(order) != stateDisputed {
		t.Fatalf("the ban moved a disputed order's deadline from %s to %s (%s)", at, got, x.state(order))
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE order_id=$1 AND strpos(body,$2)>0", order, banDeadlineText); n != 0 || len(x.banNotes(order)) != 0 {
		t.Fatalf("a disputed order: %d deadline notices, notes %q", n, x.banNotes(order))
	}
}

// Lifting the ban keeps the extended deadline: the order stays shipped, and the panel reads as for any vendor again.
func TestUnbanKeepsExtendedDeadline(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("banunban_admin", "admin")
	order, bsess, _ := x.shippedXMR(x.physical(x.vendor))
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	x.banVendor(admin)
	at := x.releaseAt(order)
	if !at.After(x.dbNow().Add(71 * time.Hour)) {
		t.Fatalf("the ban left the deadline at %s", at)
	}
	x.unbanVendor(admin)
	if got := x.releaseAt(order); !got.Equal(at) {
		t.Fatalf("lifting the ban moved the deadline from %s to %s", at, got)
	}
	x.mustPoll()
	if x.state(order) != stateShipped {
		t.Fatalf("after the ban was lifted the order is %s", x.state(order))
	}
	if p := x.panel(order, bsess); !strings.Contains(p, "the market releases the coins to the vendor") || strings.Contains(p, "holds its payment") {
		t.Fatalf("the buyer's panel after the ban was lifted:\n%s", p)
	}
}

// While the vendor is banned, the order notes of completion by the deadline and by the buyer's confirmation, the
// vendor's notice of the confirmation and the buyer's reminder say the release is held, not that the market sends it.
func TestBanCompletionNotesSayHeld(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("bannotes_admin", "admin")
	phys := x.physical(x.vendor)
	byDeadline, _, _ := x.shippedXMR(phys)
	byConfirm, confirmSess, _ := x.shippedXMR(phys)
	reminded, _, _ := x.shippedXMR(phys)
	x.forceDeadline(reminded, "release_at=now()+interval '10 days'")
	x.banVendor(admin)
	x.forceDeadline(byDeadline, "release_at=now()-interval '1 minute'")
	x.forceDeadline(reminded, "release_at=now()+interval '5 hours'")
	if code, body := x.xmrConfirm(byConfirm, confirmSess, testPassword); code != 303 {
		t.Fatalf("confirm receipt: %d %s", code, body)
	}
	x.mustPoll()
	sends := "and sends it"
	for _, c := range []struct{ order, part string }{{byDeadline, escrowDeadlineNote}, {byConfirm, "Receipt confirmed by the buyer"}} {
		notes := agStrings(t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1 AND from_state='shipped' AND to_state='completed'", c.order)
		if len(notes) != 1 || !containsAll(notes[0], c.part, "the market holds the release", "No other address is ever paid") || strings.Contains(notes[0], sends) {
			t.Fatalf("completion note of %s: %q", shortID(c.order), notes)
		}
	}
	vendorNotice := x.noticesOf(x.vendor, byConfirm)
	if n := len(vendorNotice); n == 0 || !strings.Contains(vendorNotice[n-1], "holds the release") || strings.Contains(vendorNotice[n-1], sends) {
		t.Fatalf("the vendor's notice of the confirmation: %q", vendorNotice)
	}
	for _, uid := range []string{x.partyOf(reminded, roleBuyer), x.vendor} {
		got := agStrings(t, x.testEnv, "SELECT body FROM notifications WHERE user_id=$1 AND order_id=$2 AND strpos(body,'Reminder')+strpos(body,'has until')>0", uid, reminded)
		if len(got) != 1 || !strings.Contains(got[0], "held") || strings.Contains(got[0], "releases the coins") {
			t.Fatalf("reminder to %s: %q", uid, got)
		}
	}
}

// PAY90-2: the ban locks the vendor's shipped orders, in id order, before the vendor's account row, as an order message
// does (its order, then the partner's account). A shipped order held by another transaction makes the ban wait for it
// instead of refusing; meanwhile that transaction can still take the vendor's account row, as an order message does,
// with no deadlock, and the ban then goes through.
func TestBanWaitsForHeldShippedOrder(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("banbusy_admin", "admin")
	order, _, _ := x.shippedXMR(x.physical(x.vendor))
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	handle := agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", x.vendor)
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SET LOCAL lock_timeout='5s'"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE", order); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- x.do("POST", "/admin/ban", admin, banForm("ban", handle, testBanReason)).Code }()
	x.waitForLockWaits(1)
	// An order message's second lock: the ban does not hold the vendor's account row while it waits for the order.
	if _, err = tx.Exec("SELECT 1 FROM users WHERE id=$1 FOR SHARE", x.vendor); err != nil {
		t.Fatalf("the vendor's account row while the ban waits for the order: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 303 {
		t.Fatalf("the ban that waited for the held order: %d", code)
	}
	x.checkBanNotice(order, x.releaseAt(order))
}

// PAY90-2: a busy watcher (passes back to back; each locks every escrow order whose deposits it records) beside 20 bans
// and unbans of a vendor with 30 shipped orders: every ban and unban goes through (no busy 503, no deadlock), and after
// each ban every order is still shipped, its release deadline at least 72 hours after the ban, and its buyer has exactly
// one notice of each ban so far.
func TestBanUnderWatcherNoBusy(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("banwatch_admin", "admin")
	phys := x.physical(x.vendor)
	agExec(x.testEnv, "UPDATE products SET stock=100 WHERE id=$1", phys)
	orders, addrs := make([]string, 30), make([]string, 30)
	for i := range orders {
		orders[i], addrs[i] = x.freshCheckout(phys)
		x.fake.payAddress(addrs[i], hex64("fund "+orders[i]), xmrListingPrice, 10)
		if i%5 == 4 { // the pass funds these and refills ESCROW_XMR_SLOTS (5 in the fake)
			x.mustPoll()
		}
	}
	for _, o := range orders {
		if code, body := x.xmrShip(o, x.vendorSess, testXMRAddr("claim "+o, true), testPassword); code != 303 {
			t.Fatalf("ship %s (%s): %d %s", shortID(o), x.state(o), code, body)
		}
		x.forceDeadline(o, "release_at=now()+interval '5 hours'")
	}
	handle := agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", x.vendor)
	ctx, stop := context.WithCancel(context.Background())
	var passes atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			_ = x.A.xmrEscrowPollOnce(ctx) // a pass cut short when the test ends is not a finding
			passes.Add(1)
		}
	}()
	defer wg.Wait()
	defer stop()
	refused := map[string]int{}
	bans := 0
	for i := 1; i <= 20; i++ {
		before := x.dbNow()
		if w := x.do("POST", "/admin/ban", admin, banForm("ban", handle, testBanReason)); w.Code != 303 {
			refused["ban "+strconv.Itoa(w.Code)]++
			continue
		}
		bans++
		if n := agInt(x.testEnv, `SELECT count(*) FROM orders o JOIN escrows es ON es.order_id=o.id WHERE o.id=ANY($1)
			AND (o.state<>'shipped' OR es.release_at<$2::timestamptz+interval '72 hours')`, orders, before); n != 0 {
			t.Fatalf("ban %d: %d order(s) not shipped or with a release deadline sooner than 72 hours after the ban", i, n)
		}
		if n := agInt(x.testEnv, `SELECT count(*) FROM orders o WHERE o.id=ANY($1) AND (SELECT count(*) FROM notifications n
			WHERE n.user_id=o.buyer_id AND n.order_id=o.id AND strpos(n.body,$2)>0)<>$3`, orders, banDeadlineText, bans); n != 0 {
			t.Fatalf("ban %d: the buyers of %d order(s) do not have exactly %d ban notice(s)", i, n, bans)
		}
		if w := x.do("POST", "/admin/ban", admin, banForm("unban", handle, testLiftReason)); w.Code != 303 {
			refused["unban "+strconv.Itoa(w.Code)]++
			break
		}
	}
	stop()
	wg.Wait()
	if len(refused) != 0 {
		t.Fatalf("refused beside a busy watcher (%d passes): %v", passes.Load(), refused)
	}
	if passes.Load() < 2 {
		t.Fatalf("the watcher ran %d passes beside 20 bans: it was not busy", passes.Load())
	}
}
