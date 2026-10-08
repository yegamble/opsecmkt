package market

import (
	"html"
	"net/url"
	"testing"
)

func TestOverpaidBelowReleaseFloorDoesNotPromiseSend(t *testing.T) {
	p := newPayEnv(t)
	for _, currency := range []string{"BTC", "XMR"} {
		floor := payoutFloor(currency, "release")
		for _, sum := range []int64{floor - 1, floor} {
			order, provider := addGuidanceOrder(t, p, currency, stateAwaitingPayment)
			if _, err := p.DB.Exec("UPDATE orders SET amount=$1 WHERE id=$2", floor/4, order); err != nil {
				t.Fatal(err)
			}
			addr := p.str("SELECT address FROM payment_addresses WHERE order_id=$1", order)
			provider.Deposit(addr, "overpaid-floor-"+order, 0, sum, int64(provider.Confirmations()))
			p.poll()
			if got := p.state(order); got != statePaid {
				t.Fatalf("%s payment state %s", currency, got)
			}
			for _, state := range []string{statePaid, stateShipped, stateDelivered} {
				if _, err := p.DB.Exec("UPDATE orders SET state=$1 WHERE id=$2", state, order); err != nil {
					t.Fatal(err)
				}
				for _, session := range []string{p.buyerSess, p.vendorSess} {
					page := html.UnescapeString(p.page("/order?id="+order, session))
					if sum < floor {
						mustContain(t, page, "Completing records the amount for staff review", belowFloor(currency, "release", sum), "nothing is sent automatically")
						mustNotContain(t, page, "Completing releases everything", "The network fee is deducted from each payout")
					} else {
						mustContain(t, page, "Completing releases everything", "An administrator reviews the release before it is sent")
						mustNotContain(t, page, "Completing records the amount for staff review")
					}
				}
			}
			if sum < floor {
				// The fixture is a physical order; complete from its supported shipped state.
				if _, err := p.DB.Exec("UPDATE orders SET state=$1 WHERE id=$2", stateShipped, order); err != nil {
					t.Fatal(err)
				}
				p.check(p.do("POST", "/orders/complete", p.buyerSess, payoutConfirmed(url.Values{"order_id": {order}}, sum)), 303)
				if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", order); n != 0 {
					t.Fatalf("below-floor completion queued %d payouts", n)
				}
				if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", order); n != 1 {
					t.Fatalf("below-floor completion flagged %d deposits", n)
				}
			}
		}
	}
}
