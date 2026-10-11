package market

import (
	"strings"
	"testing"
)

func TestCompletedDigitalDeliverySurvivesListingKindChange(t *testing.T) {
	x := newEscrowEnv(t)
	e := x.testEnv
	_, stranger := e.user("delivery_stranger", "buyer")
	x.mustCheckout(x.order, regtestAddr("delivery-snapshot"))
	x.fund(x.order, x.order, 3, 100000)
	if x.state(x.order) != stateCompleted {
		t.Fatalf("funded digital escrow state=%s", x.state(x.order))
	}
	e.check(e.do("POST", "/listings/update", x.vendorSess, inventoryForm(x.product, map[string]string{"kind": "physical"})), 303)
	for _, session := range []string{x.buyerSess, x.vendorSess} {
		page := e.do("GET", "/order?id="+x.order, session, nil)
		e.check(page, 200)
		if !strings.Contains(page.Body.String(), escrowDelivery) {
			t.Fatal("completed order lost its delivered content after listing edit")
		}
	}
	e.check(e.do("GET", "/order?id="+x.order, stranger, nil), 404)
}
