package market

import (
	"net/url"
)

// shipEscrow posts the escrow "Mark as shipped" form (a fresh claim address and the password) as sess.
func (x *escrowEnv) shipEscrow(order, sess, addr, password string) (int, string) {
	w := x.do("POST", "/escrow/ship", sess, url.Values{"order_id": {order}, "address": {addr}, "password": {password}, "note": {"Sent by tracked post."}})
	return w.Code, w.Body.String()
}

// paidPhysical is a funded physical escrow order (refund address refund, paid with sats), waiting to be shipped.
func (x *escrowEnv) paidPhysical(refund string, sats ...int64) string {
	x.t.Helper()
	order := x.physicalDraft(3)
	x.mustCheckout(order, refund)
	x.fund(order, order, 3, sats...)
	if x.state(order) != statePaid {
		x.t.Fatalf("funded physical escrow order is %s", x.state(order))
	}
	return order
}

// shippedPhysical is a funded physical escrow order the vendor marked shipped with claimTo, and its release built by
// one watcher pass: the order and the release.
func (x *escrowEnv) shippedPhysical(refund, claimTo string, sats ...int64) (string, int64) {
	x.t.Helper()
	order := x.paidPhysical(refund, sats...)
	if code, body := x.shipEscrow(order, x.vendorSess, claimTo, testPassword); code != 303 {
		x.t.Fatalf("ship: %d %s", code, body)
	}
	x.poll()
	id, kind, state, class := x.settlement(order)
	if kind != "release" || state != "awaiting_signature" {
		x.t.Fatalf("release after shipping: %s %s %s", kind, state, class)
	}
	return order, id
}

// marketCalls counts the RPCs only the market's own signature makes: the arbiter key check (deriveaddresses of the
// market child's wpkh address) and refill, and the arbiter wallet's walletprocesspsbt.
func (x *escrowEnv) marketCalls() int {
	return x.core.count("walletprocesspsbt") + x.core.count("keypoolrefill") + x.core.countWith("deriveaddresses", "wpkh(")
}
