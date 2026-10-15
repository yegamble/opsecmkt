package market

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// A-127: /admin has no order list. Staff see an order only while it is disputed or resolved or a payment on it is
// flagged for review (A-24, /canary#records), so the admin page must not read every order either.
func TestAdminPageReadsNoOrders(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("orders_admin", "admin")
	vendorID, _ := e.user("orders_vendor", "vendor")
	buyerID, _ := e.user("orders_buyer", "buyer")
	id := e.order(buyerID, e.product(vendorID, "physical"), "XMR", "shipped")
	d := &PageData{Page: "admin", User: e.loadUser(adminID), Settings: map[string]string{}}
	if err := e.A.load(httptest.NewRequest("GET", "/admin", nil), d); err != nil {
		t.Fatal(err)
	}
	if len(d.Orders) != 0 {
		t.Fatalf("the admin page loaded %d orders", len(d.Orders))
	}
	page := e.do("GET", "/admin", admin, nil)
	e.check(page, 200)
	if strings.Contains(page.Body.String(), id) {
		t.Fatal("the admin page names an order it has no business showing")
	}
}
