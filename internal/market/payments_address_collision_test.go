package market

import (
	"net/url"
	"strings"
	"testing"
)

// Historical issued addresses still reserve their identity against new Bitcoin escrow refunds.
func TestHistoricalPaymentAddressCannotBeReusedForEscrow(t *testing.T) {
	x := newEscrowEnv(t)
	historical := x.testEnv.order(x.buyer, x.product, "BTC", stateCompleted)
	address := "bcrt1qpooleddeposit000000000000000000000"
	agExec(x.testEnv, "INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'BTC',$2,'btc-regtest')", historical, address)
	stock := agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", x.product)
	for attempt := 0; attempt < 2; attempt++ {
		w := x.do("POST", "/orders/escrow", x.buyerSess, url.Values{"order_id": {x.order}, "refund_address": {address}})
		if w.Code != 409 || !strings.Contains(w.Body.String(), "already used on this market") {
			t.Fatalf("attempt %d: %d %s", attempt, w.Code, w.Body)
		}
		if x.state(x.order) != stateDraft || agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", x.product) != stock || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", x.order) != 0 {
			t.Fatal("refused reuse changed draft, stock or escrow")
		}
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payment_addresses WHERE order_id=$1", historical) != 1 {
		t.Fatal("historical address disappeared")
	}
}
