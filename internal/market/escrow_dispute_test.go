package market

import (
	"net/url"
)

// disputedEscrow is a funded escrow order whose content was not delivered automatically, disputed by its buyer: the
// order and the dispute's id.
func (x *escrowEnv) disputedEscrow(refund string) (string, string) {
	x.t.Helper()
	order := x.draft()
	x.mustCheckout(order, refund)
	x.DB.Exec("UPDATE products SET delivery_content='' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order)
	x.fund(order, order, 3, 100000)
	if x.state(order) != statePaid {
		x.t.Fatalf("funded order %s", x.state(order))
	}
	w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The content was never delivered to me at all."}})
	if w.Code != 303 || x.state(order) != stateDisputed {
		x.t.Fatalf("dispute: %d %s (%s)", w.Code, w.Body.String(), x.state(order))
	}
	var id string
	if err := x.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order).Scan(&id); err != nil {
		x.t.Fatal(err)
	}
	return order, id
}

func (x *escrowEnv) resolve(sess, dispute, outcome, password string) (int, string) {
	w := x.do("POST", "/resolve", sess, url.Values{"id": {dispute}, "outcome": {outcome}, "resolution": {"Decided from the evidence both parties sent."},
		"payout_confirmed": {"confirmed"}, "amount_seen": {"100000"}, "reply_seen": {"0"}, "password": {password}})
	return w.Code, w.Body.String()
}

func (x *escrowEnv) changeDecision(sess, order, outcome, password string) (int, string) {
	w := x.do("POST", "/escrow/decision", sess, url.Values{"order_id": {order}, "outcome": {outcome}, "reason": {"New evidence showed the content was delivered."}, "password": {password}})
	return w.Code, w.Body.String()
}

// mainSettlement is order's release or refund that is not superseded.
func (x *escrowEnv) mainSettlement(order string) (int64, string, string) {
	x.t.Helper()
	var id int64
	var kind, state string
	if err := x.DB.QueryRow("SELECT id,kind,state FROM settlements WHERE order_id=$1 AND kind IN ('release','refund') AND state<>'superseded'", order).Scan(&id, &kind, &state); err != nil {
		x.t.Fatalf("settlement of %s: %v", order, err)
	}
	return id, kind, state
}
