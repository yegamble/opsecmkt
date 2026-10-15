package market

import (
	"net/url"

	"testing"
	"time"
)

func recoverNonceForm(order, nonce string) url.Values {
	return url.Values{"order_id": {order}, "password": {testPassword}, formNonceField: {nonce}}
}

func recoverNonceFixture(t *testing.T) *escrowEnv {
	t.Helper()
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, regtestAddr("nonce-recover-refund"))
	x.check(x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {x.order}, "from": {stateAwaitingPayment}}), 303)
	return x
}

func recoverRequestedAt(t *testing.T, x *escrowEnv) time.Time {
	t.Helper()
	var at time.Time
	if err := x.DB.QueryRow("SELECT recover_requested FROM escrows WHERE order_id=$1", x.order).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}
