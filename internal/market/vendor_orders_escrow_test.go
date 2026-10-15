package market

import (
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"

	"testing"
)

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
