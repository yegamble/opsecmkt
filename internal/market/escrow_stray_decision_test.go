package market

import (
	"net/url"

	"strconv"
)

// smallDispute is a disputed escrow order priced and funded at sats (one deposit at 3 confirmations), with, when
// conflicted > 0, a deposit of that many sats Core reports as conflicted when it is funded, so it does not count: the
// order, its dispute, its escrow address and the conflicted deposit's transaction.
func (x *escrowEnv) smallDispute(refund string, sats, conflicted int64) (order, dispute, addr, big string) {
	x.t.Helper()
	order = x.draft()
	x.DB.Exec("UPDATE products SET btc=$2 WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order, sats)
	addr = x.mustCheckout(order, refund)
	x.DB.Exec("UPDATE products SET delivery_content='' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order)
	x.core.deposit(addr, txidOf("small dispute tiny "+order), 0, sats, 3)
	if conflicted > 0 {
		big = txidOf("small dispute big " + order)
		x.core.deposit(addr, big, 0, conflicted, -2)
	}
	x.poll()
	if x.state(order) != statePaid {
		x.t.Fatalf("funded order %s", x.state(order))
	}
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The content was never delivered to me at all."}}); w.Code != 303 {
		x.t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
	if err := x.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order).Scan(&dispute); err != nil {
		x.t.Fatal(err)
	}
	return order, dispute, addr, big
}

// decide has the administrator decide dispute (of an order whose counted deposits are worth seen sats) for outcome.
func (x *escrowEnv) decide(sess, dispute, outcome string, seen int64) {
	x.t.Helper()
	w := x.do("POST", "/resolve", sess, url.Values{"id": {dispute}, "outcome": {outcome}, "resolution": {"Decided from the evidence both parties sent."},
		"payout_confirmed": {"confirmed"}, "amount_seen": {strconv.FormatInt(seen, 10)}, "reply_seen": {"0"}, "password": {testPassword}})
	if w.Code != 303 {
		x.t.Fatalf("resolve %s: %d %s", outcome, w.Code, w.Body.String())
	}
}

const decidedTooFew = "The dispute was decided for the buyer, but the market could not build the refund: the coins in this escrow were too few to pay the network fee and an output of at least 0.0001 BTC. A stray refund returns them, together with any later deposit, to the buyer's refund address once together they can pay the network fee; each deposit below says where it stands."
