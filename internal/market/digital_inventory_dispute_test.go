package market

import (
	"database/sql"
	"net/url"
	"strconv"
	"sync"
	"testing"
)

// A-675 (QA93-2): a dispute on a distinct-inventory digital order that ends in a refund, by an administrator's decision
// for the buyer (disputed -> resolved) or the vendor's concession (disputed -> cancelled), returns the units the order
// reserved and never delivered: the listing's stock is recounted, and the vendor controls each returned unit again (it
// can be removed and the same content imported once more). Units an order delivered stay consumed on both legs, and their
// content cannot be imported again while it is retained. A physical order's stock is never returned after a dispute. A
// repeated or racing refund, or the hook replayed on the closed order, releases nothing twice (stock is a count of the
// listing's unassigned units, never an increment). Disputes have two outcomes only (release, refund; migration 030), so
// no partial-refund leg exists. DB integration against the simulated Monero wallet RPC; not chain evidence.
func TestDigitalInventoryDisputeRefundReturnsUndisclosedUnits(t *testing.T) {
	x := newXMREscrowEnv(t)
	p := digitalInventoryFixture(t, x, "a675 unit A", "a675 unit B", "a675 unit C", "a675 unit D", "a675 unit E", "a675 unit F")
	x.mustPoll()
	available := func() int {
		return agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE product_id=$1 AND order_id IS NULL", p)
	}
	stock := func() int { return agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", p) }
	want := func(leg string, n int) {
		t.Helper()
		if available() != n || stock() != n {
			t.Fatalf("%s: %d available, stock %d, want %d", leg, available(), stock(), n)
		}
	}
	unitOf := func(order string) (string, string) {
		t.Helper()
		var id, content string
		if err := x.DB.QueryRow("SELECT id,content FROM digital_units WHERE order_id=$1", order).Scan(&id, &content); err != nil {
			t.Fatal(err)
		}
		return id, content
	}
	// undelivered is a funded order of one unit, left paid and undelivered by a takedown during payment.
	undelivered := func() string {
		t.Helper()
		order, addr := x.freshCheckout(p)
		agExec(x.testEnv, "UPDATE products SET archived=true,archived_at=now(),admin_archive_reason='Taken down for the test' WHERE id=$1", p)
		x.fake.payAddress(addr, hex64("a675 undelivered "+order), xmrListingPrice, 10)
		x.mustPoll()
		agExec(x.testEnv, "UPDATE products SET archived=false,archived_at=NULL,admin_archive_reason=NULL WHERE id=$1", p)
		if x.state(order) != statePaid || agInt(x.testEnv, "SELECT count(*) FROM deliveries WHERE order_id=$1", order) != 0 {
			t.Fatalf("undelivered fixture %s: %s", order, x.state(order))
		}
		return order
	}
	// delivered is a funded order of one unit that the market delivered automatically.
	delivered := func() string {
		t.Helper()
		order, addr := x.freshCheckout(p)
		x.fake.payAddress(addr, hex64("a675 delivered "+order), xmrListingPrice, 10)
		x.mustPoll()
		x.mustPoll()
		if x.state(order) != stateDelivered || agInt(x.testEnv, "SELECT count(*) FROM deliveries WHERE order_id=$1", order) != 1 {
			t.Fatalf("delivered fixture %s: %s", order, x.state(order))
		}
		return order
	}
	dispute := func(order string) string {
		t.Helper()
		x.check(x.do("POST", "/disputes", x.sessOf(order, roleBuyer), url.Values{"order_id": {order}, "reason": {"The licence key in the delivery does not work."}}), 303)
		return agStrings(t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)[0]
	}
	concedeForm := func(id string) url.Values {
		return payoutConfirmed(url.Values{"id": {id}, "outcome": {"refund"}, "password": {testPassword}}, xmrListingPrice)
	}
	_, judge := x.user("a675_judge_"+randomToken()[:6], "admin")
	// replayHook runs stockHook again on the closed order, as a second transition out of disputed would.
	replayHook := func(order, to string) {
		t.Helper()
		o, err := scanOrder(x.DB.QueryRow(orderQuery+" WHERE o.id=$1", order))
		if err != nil {
			t.Fatal(err)
		}
		runInTx(t, x.DB, func(tx *sql.Tx) error { return stockHook(t.Context(), x.A, tx, &o, stateDisputed, to) })
	}
	// reimport removes the returned unit and imports its content again: under the bug the unit stayed assigned, so the
	// removal was 404 and the import 409 (UNIQUE product_id,fingerprint).
	reimport := func(leg, unit, content string, n int) {
		t.Helper()
		x.check(x.do("POST", "/listings/digital-units", x.vendorSess, url.Values{"id": {p}, "contents": {content}}), 409) // already in stock
		x.check(x.do("POST", "/listings/digital-units/remove", x.vendorSess, url.Values{"id": {p}, "unit_id": {unit}}), 303)
		want(leg+" removed", n-1)
		x.check(x.do("POST", "/listings/digital-units", x.vendorSess, url.Values{"id": {p}, "contents": {content}}), 303)
		want(leg+" re-imported", n)
	}

	want("fixture", 6)

	// Leg 1: an administrator decides the dispute for the buyer.
	refunded := undelivered()
	unit, content := unitOf(refunded)
	want("after checkout", 5)
	x.disputeAndResolve(refunded, "refund")
	if x.state(refunded) != stateResolved || agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1", refunded) != 0 {
		t.Fatalf("refund decision: %s", x.state(refunded))
	}
	want("refund decision", 6)
	replayHook(refunded, stateResolved)
	want("refund decision replayed", 6)
	reimport("refund decision", unit, content, 6)

	// Leg 2: the vendor concedes the dispute; repeating the concession changes nothing.
	conceded := undelivered()
	unit, content = unitOf(conceded)
	d := dispute(conceded)
	want("conceded dispute open", 5)
	x.check(x.do("POST", "/disputes/concede", x.vendorSess, concedeForm(d)), 303)
	if x.state(conceded) != stateCancelled || agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1", conceded) != 0 {
		t.Fatalf("vendor concession: %s", x.state(conceded))
	}
	want("vendor concession", 6)
	events := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", conceded)
	x.check(x.do("POST", "/disputes/concede", x.vendorSess, concedeForm(d)), 409)
	replayHook(conceded, stateCancelled)
	if x.state(conceded) != stateCancelled || agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", conceded) != events {
		t.Fatalf("repeated concession: order %s, history changed", x.state(conceded))
	}
	want("vendor concession repeated", 6)
	reimport("vendor concession", unit, content, 6)

	// The concession and a refund decision race on one dispute: one wins, and the unit returns once.
	raced := undelivered()
	d = dispute(raced)
	want("raced dispute open", 5)
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("SELECT 1 FROM disputes WHERE id=$1 FOR UPDATE", d); err != nil {
		t.Fatal(err)
	}
	var concedeCode, resolveCode int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		concedeCode = x.do("POST", "/disputes/concede", x.vendorSess, concedeForm(d)).Code
	}()
	x.waitForLockWaits(1)
	go func() {
		defer wg.Done()
		resolveCode = x.do("POST", "/resolve", judge, url.Values{"id": {d}, "outcome": {"refund"}, "resolution": {"Decided from the evidence both parties sent."},
			"payout_confirmed": {"confirmed"}, "amount_seen": {strconv.FormatInt(xmrListingPrice, 10)}, "reply_seen": {"0"}, "password": {testPassword}}).Code
	}()
	x.waitForLockWaits(2)
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if (concedeCode == 303) == (resolveCode == 303) || x.state(raced) == stateDisputed {
		t.Fatalf("race: concession %d, decision %d, order %s", concedeCode, resolveCode, x.state(raced))
	}
	want("raced refunds", 6)

	// Delivered control, both legs: the delivered unit stays with its order and its content cannot be imported again.
	for _, leg := range []string{"decision", "concession"} {
		order := delivered()
		unit, content := unitOf(order)
		want(leg+" delivered", 5)
		if leg == "decision" {
			x.disputeAndResolve(order, "refund")
		} else {
			x.check(x.do("POST", "/disputes/concede", x.vendorSess, concedeForm(dispute(order))), 303)
		}
		if x.state(order) != stateResolved && x.state(order) != stateCancelled ||
			agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE id=$1 AND order_id=$2 AND delivered_at IS NOT NULL", unit, order) != 1 {
			t.Fatalf("delivered %s: %s, unit not kept with the order", leg, x.state(order))
		}
		want(leg+" delivered refunded", 5)
		x.check(x.do("POST", "/listings/digital-units/remove", x.vendorSess, url.Values{"id": {p}, "unit_id": {unit}}), 404)
		x.check(x.do("POST", "/listings/digital-units", x.vendorSess, url.Values{"id": {p}, "contents": {content}}), 409)
		want(leg+" delivered re-import refused", 5)
		// Restore the count for the next leg with a fresh unit.
		x.check(x.do("POST", "/listings/digital-units", x.vendorSess, url.Values{"id": {p}, "contents": {"a675 replacement " + leg}}), 303)
		want(leg+" replacement", 6)
	}

	// Physical control: a refunded dispute, either way, returns no stock.
	ph := x.physical(x.vendor)
	physicalStock := func() int { return agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", ph) }
	initial := physicalStock()
	paidPhysical := func() string {
		t.Helper()
		order, addr := x.freshCheckout(ph)
		x.fake.payAddress(addr, hex64("a675 physical "+order), xmrListingPrice, 10)
		x.mustPoll()
		if x.state(order) != statePaid {
			t.Fatalf("physical fixture %s: %s", order, x.state(order))
		}
		return order
	}
	decided := paidPhysical()
	x.disputeAndResolve(decided, "refund")
	physicalConceded := paidPhysical()
	x.check(x.do("POST", "/disputes/concede", x.vendorSess, concedeForm(dispute(physicalConceded))), 303)
	if x.state(decided) != stateResolved || x.state(physicalConceded) != stateCancelled || physicalStock() != initial-2 {
		t.Fatalf("physical: %s, %s, stock %d from %d", x.state(decided), x.state(physicalConceded), physicalStock(), initial)
	}
	replayHook(decided, stateResolved)
	replayHook(physicalConceded, stateCancelled)
	if physicalStock() != initial-2 {
		t.Fatalf("physical replay: stock %d from %d", physicalStock(), initial)
	}
	want("physical legs", 6)
}
