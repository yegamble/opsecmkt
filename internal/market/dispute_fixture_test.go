package market

import (
	"net/url"
	"testing"
)

// disputeOrderWorld layers order assertions over the simulated Monero fixture.
type disputeOrderWorld struct {
	*orderWorld
	x *xmrEscrowEnv
}

func newDisputeOrderWorld(t *testing.T) *disputeOrderWorld {
	t.Helper()
	x := newXMREscrowEnv(t)
	x.mustPoll()
	// These staff-permission tests construct their own exact staff population.
	// The payment fixture's unrelated administrator must not become a resolver.
	agExec(x.testEnv, "UPDATE users SET role='buyer' WHERE id=$1", x.admin)
	w := &orderWorld{e: x.testEnv, buyerID: x.buyer, buyer: x.buyerSess, vendorID: x.vendor, vendor: x.vendorSess}
	w.otherID, w.other = x.user("dispute_other_"+randomToken()[:6], "buyer")
	w.modID, w.mod = x.user("dispute_mod_"+randomToken()[:6], "moderator")
	return &disputeOrderWorld{orderWorld: w, x: x}
}

func (w *disputeOrderWorld) fundedFor(buyerID, buyer, vendorID, vendor string) string {
	w.e.t.Helper()
	product := w.x.digital(vendorID)
	order := w.e.order(buyerID, product, "XMR", stateDraft)
	checkout := w.e.do("POST", "/orders/escrow-xmr", buyer, url.Values{"order_id": {order}, "refund_address": {testXMRAddr("dispute-refund-"+order, true)}})
	if checkout.Code != 303 {
		w.e.t.Fatalf("escrow checkout: %d %s", checkout.Code, checkout.Body.String())
	}
	if _, err := w.e.DB.Exec("UPDATE products SET delivery_content='' WHERE id=$1", product); err != nil {
		w.e.t.Fatal(err)
	}
	address := agStr(w.e, "SELECT address FROM escrows WHERE order_id=$1", order)
	w.x.fake.payAddress(address, hex64("dispute-fund-"+order), xmrListingPrice, 10)
	w.x.mustPoll()
	if w.state(order) != statePaid {
		w.e.t.Fatalf("funded fixture state=%s", w.state(order))
	}
	return order
}

// fundedPhysicalFor creates a funded physical Monero escrow with its dispatch
// snapshot and a usable vendor PGP key for fulfillment and shipping tests.
func (w *disputeOrderWorld) fundedPhysicalFor(buyerID, buyer, vendorID, vendor string) string {
	w.e.t.Helper()
	product := w.x.physical(vendorID)
	order := w.e.order(buyerID, product, "XMR", stateDraft)
	r := w.e.do("POST", "/orders/escrow-xmr", buyer, url.Values{"order_id": {order}, "refund_address": {testXMRAddr("physical-dispute-refund-"+order, true)}})
	if r.Code != 303 {
		w.e.t.Fatalf("physical escrow checkout: %d %s", r.Code, r.Body.String())
	}
	address := agStr(w.e, "SELECT address FROM escrows WHERE order_id=$1", order)
	w.x.fake.payAddress(address, hex64("physical-dispute-fund-"+order), xmrListingPrice, 10)
	w.x.mustPoll()
	if w.state(order) != statePaid {
		w.e.t.Fatalf("funded physical fixture state=%s", w.state(order))
	}
	return order
}

func (w *disputeOrderWorld) openDispute(order, opener, reason string) string {
	w.e.t.Helper()
	r := w.e.do("POST", "/disputes", opener, url.Values{"order_id": {order}, "reason": {reason}})
	if r.Code != 303 || w.state(order) != stateDisputed {
		w.e.t.Fatalf("open escrow dispute: %d %s (%s)", r.Code, r.Body.String(), w.state(order))
	}
	var id string
	if err := w.e.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order).Scan(&id); err != nil {
		w.e.t.Fatal(err)
	}
	return id
}

func (w *disputeOrderWorld) disputedFor(buyerID, buyer, vendorID, vendor, opener, reason string) (string, string) {
	order := w.fundedFor(buyerID, buyer, vendorID, vendor)
	return order, w.openDispute(order, opener, reason)
}
