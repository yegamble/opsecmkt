package market

import (
	"net/url"
	"testing"
)

// disputeOrderWorld layers order assertions over the in-memory escrow fixture.
type disputeOrderWorld struct {
	*orderWorld
	x       *escrowEnv
	keySalt uint8
}

func newDisputeOrderWorld(t *testing.T) *disputeOrderWorld {
	t.Helper()
	x := newEscrowEnv(t)
	w := &orderWorld{e: x.testEnv, buyerID: x.buyer, buyer: x.buyerSess, vendorID: x.vendor, vendor: x.vendorSess}
	w.otherID, w.other = x.user("dispute_other_"+randomToken()[:6], "buyer")
	w.modID, w.mod = x.user("dispute_mod_"+randomToken()[:6], "moderator")
	return &disputeOrderWorld{orderWorld: w, x: x, keySalt: 2}
}

func (w *disputeOrderWorld) uniqueEscrowKey(base string) string {
	w.e.t.Helper()
	salt := w.keySalt
	w.keySalt++
	return altEscrowKey(w.e.t, base, randomToken()[:8], salt)
}

func (w *disputeOrderWorld) ensureEscrowKeys(buyerID, vendorID string) {
	w.e.t.Helper()
	if buyerID != w.x.buyer {
		w.x.activeKey(buyerID, w.uniqueEscrowKey(w.x.buyerKey))
	}
	if vendorID != w.x.vendor {
		w.x.activeKey(vendorID, w.uniqueEscrowKey(w.x.buyerKey))
	}
}

func (w *disputeOrderWorld) fundedFor(buyerID, buyer, vendorID, vendor string) string {
	w.e.t.Helper()
	w.ensureEscrowKeys(buyerID, vendorID)
	product := w.x.autoListing(vendorID)
	order := w.e.order(buyerID, product, "BTC", stateDraft)
	checkout := w.e.do("POST", "/orders/escrow", buyer, url.Values{"order_id": {order}, "refund_address": {regtestAddr("dispute-refund-" + order[:8])}})
	if checkout.Code != 303 {
		w.e.t.Fatalf("escrow checkout: %d %s", checkout.Code, checkout.Body.String())
	}
	if _, err := w.e.DB.Exec("UPDATE products SET delivery_content='' WHERE id=$1", product); err != nil {
		w.e.t.Fatal(err)
	}
	w.x.fund(order, order, 3, 100000)
	if w.state(order) != statePaid {
		w.e.t.Fatalf("funded fixture state=%s", w.state(order))
	}
	return order
}

// fundedPhysicalFor creates a funded physical escrow with the dispatch snapshot
// and a usable vendor PGP key, for tests of fulfillment or shipping.
func (w *disputeOrderWorld) fundedPhysicalFor(buyerID, buyer, vendorID, vendor string) string {
	w.e.t.Helper()
	w.ensureEscrowKeys(buyerID, vendorID)
	_, public := testPGPKey(w.e.t, "dispute-physical-"+randomToken()[:6])
	w.x.setKey(vendorID, public)
	product := w.x.physicalListing(vendorID, 3)
	order := w.e.order(buyerID, product, "BTC", stateDraft)
	r := w.e.do("POST", "/orders/escrow", buyer, url.Values{"order_id": {order}, "refund_address": {regtestAddr("physical-dispute-refund-" + order[:8])}})
	if r.Code != 303 {
		w.e.t.Fatalf("physical escrow checkout: %d %s", r.Code, r.Body.String())
	}
	w.x.fund(order, order, 3, 100000)
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
