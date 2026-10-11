package market

import (
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-494: a cancelled order stays on its vendor's /orders list, in the list's paging total and in the vendor desk's
// incoming-order history once checkout took an escrow for it (Bitcoin or Monero) or a payment to it was recorded; a
// draft cancelled before checkout is not customer work and stays hidden from the vendor. The buyer's list is unchanged.

// activityOrders runs the /orders loader for uid: the listed order ids and the paging total.
func activityOrders(t *testing.T, e *testEnv, uid string) ([]string, int) {
	t.Helper()
	d := &PageData{Page: "orders", User: e.loadUser(uid), Settings: map[string]string{}}
	if err := e.A.load(httptest.NewRequest("GET", "/orders", nil), d); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(d.Orders))
	for _, o := range d.Orders {
		ids = append(ids, o.ID)
	}
	if d.ActivityPaging == nil {
		t.Fatal("no activity paging on /orders")
	}
	return ids, d.ActivityPaging.Total
}

// incomingOrders runs the vendor desk's incoming-order loader for uid.
func incomingOrders(t *testing.T, e *testEnv, uid string) []string {
	t.Helper()
	d := &PageData{Page: "vendor-dashboard", User: e.loadUser(uid), Settings: map[string]string{}}
	if err := loadIncomingOrders(t.Context(), e.A, httptest.NewRequest("GET", "/vendor", nil), d); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(d.IncomingOrders))
	for _, o := range d.IncomingOrders {
		ids = append(ids, o.ID)
	}
	return ids
}

// sameOrders reports whether got and want hold the same order ids, in any order.
func sameOrders(got, want []string) bool {
	g, w := slices.Clone(got), slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	return slices.Equal(g, w)
}

func TestVendorOrdersListCancelledEscrowOrders(t *testing.T) {
	x := newEscrowEnv(t)
	// A funded physical Bitcoin escrow order the vendor cancels: the cancellation requests the escrow's refund.
	paid := x.paidPhysical(regtestAddr("a494-refund-1"), 100000)
	if w := x.do("POST", "/orders/cancel", x.vendorSess, url.Values{"order_id": {paid}, "from": {statePaid}, "amount_seen": {"100000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 ||
		x.state(paid) != stateCancelled || agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='refund'", paid) != 1 {
		t.Fatalf("vendor cancel of the funded escrow order: %d %s", w.Code, x.state(paid))
	}
	// An escrow order whose payment window ended without a deposit (cancelled by the watcher).
	expired := x.draft()
	x.mustCheckout(expired, regtestAddr("a494-refund-2"))
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", expired)
	x.poll()
	if x.state(expired) != stateCancelled {
		t.Fatalf("expired escrow order is %s", x.state(expired))
	}
	// A draft the buyer cancelled before checkout: no escrow, no payment.
	abandoned := x.draft()
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {abandoned}, "from": {stateDraft}}); w.Code != 303 || x.state(abandoned) != stateCancelled {
		t.Fatalf("buyer cancel of a draft: %d %s", w.Code, x.state(abandoned))
	}
	if n := agInt(x.testEnv, "SELECT (SELECT count(*) FROM escrows WHERE order_id=$1)+(SELECT count(*) FROM payments WHERE order_id=$1)", abandoned); n != 0 {
		t.Fatalf("the cancelled draft has %d escrow or payment rows", n)
	}

	// The vendor sees both escrow cancellations, counted in the paging total, and not the draft (x.order is another).
	ids, total := activityOrders(t, x.testEnv, x.vendor)
	if want := []string{paid, expired}; !sameOrders(ids, want) || total != len(want) {
		t.Fatalf("vendor /orders lists %v (total %d), want %v", ids, total, want)
	}
	page := x.page("/orders", x.vendorSess)
	if !strings.Contains(page, "/order?id="+paid) || !strings.Contains(page, "/order?id="+expired) || strings.Contains(page, "/order?id="+abandoned) {
		t.Fatal("the vendor's rendered /orders does not list exactly the cancelled escrow orders")
	}
	if got := incomingOrders(t, x.testEnv, x.vendor); !sameOrders(got, []string{paid, expired}) {
		t.Fatalf("vendor desk incoming orders %v, want %v", got, []string{paid, expired})
	}

	// The buyer's own list is unchanged: every order of theirs, the cancelled draft included.
	buyerAll := agStrings(t, x.testEnv, "SELECT id FROM orders WHERE buyer_id=$1", x.buyer)
	ids, total = activityOrders(t, x.testEnv, x.buyer)
	if !sameOrders(ids, buyerAll) || total != len(buyerAll) || !slices.Contains(ids, abandoned) || !slices.Contains(ids, x.order) {
		t.Fatalf("buyer /orders lists %v (total %d), want %v", ids, total, buyerAll)
	}
}

func TestVendorOrdersListCancelledXMREscrowOrders(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	paid, _ := x.paidXMRPhysical(x.physical(x.vendor))
	if code, body := x.xmrPost("/orders/cancel", x.vendorSess, url.Values{"order_id": {paid}, "from": {statePaid}, "payout_confirmed": {"confirmed"},
		"amount_seen": {strconv.FormatInt(xmrListingPrice, 10)}}); code != 303 || x.state(paid) != stateCancelled {
		t.Fatalf("vendor cancel of the funded Monero escrow order: %d %s %s", code, x.state(paid), body)
	}
	abandoned := x.draft(x.product)
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {abandoned}, "from": {stateDraft}}); w.Code != 303 || x.state(abandoned) != stateCancelled {
		t.Fatalf("buyer cancel of a Monero draft: %d %s", w.Code, x.state(abandoned))
	}
	ids, total := activityOrders(t, x.testEnv, x.vendor)
	if !sameOrders(ids, []string{paid}) || total != 1 {
		t.Fatalf("vendor /orders lists %v (total %d), want only %s", ids, total, paid)
	}
	if got := incomingOrders(t, x.testEnv, x.vendor); !sameOrders(got, []string{paid}) {
		t.Fatalf("vendor desk incoming orders %v, want only %s", got, paid)
	}
	ids, total = activityOrders(t, x.testEnv, x.buyer)
	if !sameOrders(ids, []string{abandoned}) || total != 1 {
		t.Fatalf("buyer /orders lists %v (total %d), want only %s", ids, total, abandoned)
	}
}
