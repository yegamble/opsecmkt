package market

import (
	"strings"
	"testing"
)

func TestCompletedDigitalDeliverySurvivesListingKindChange(t *testing.T) {
	e := newTestApp(t)
	vendor, vendorSession := e.user("delivery_vendor", "vendor")
	buyer, buyerSession := e.user("delivery_buyer", "buyer")
	_, stranger := e.user("delivery_stranger", "buyer")
	product := e.product(vendor, "digital")
	order := e.order(buyer, product, "BTC", statePaid)
	e.check(e.do("POST", "/orders/deliver", vendorSession, form("order_id", order, "content", "persistent-licence-123")), 303)
	e.check(e.do("POST", "/orders/complete", buyerSession, payoutConfirmed(form("order_id", order), 0)), 303)
	e.check(e.do("POST", "/listings/update", vendorSession, inventoryForm(product, map[string]string{"kind": "physical"})), 303)
	for _, session := range []string{buyerSession, vendorSession} {
		page := e.do("GET", "/order?id="+order, session, nil)
		e.check(page, 200)
		if !strings.Contains(page.Body.String(), "persistent-licence-123") {
			t.Fatal("completed order lost its delivered content after listing edit")
		}
	}
	e.check(e.do("GET", "/order?id="+order, stranger, nil), 404)
}
