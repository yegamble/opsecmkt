package market

import (
	"strings"
	"testing"
)

func TestListingRefusesBTCPriceBelowPayoutFloor(t *testing.T) {
	e := newTestApp(t)
	owner, s := e.user("inv_floor", "vendor")
	p := e.product(owner, "physical")
	for _, price := range []string{"0.00000001", "0.00009999"} {
		w := e.do("POST", "/listings", s, inventoryForm("", map[string]string{"price_btc": price}))
		e.check(w, 400)
		if !strings.Contains(w.Body.String(), "at least 0.0001 BTC") {
			t.Fatalf("create at %s: reason missing", price)
		}
		e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"price_btc": price})), 400)
	}
	var btc int64
	if err := e.DB.QueryRow("SELECT btc FROM products WHERE id=$1", p).Scan(&btc); err != nil || btc != 100000 {
		t.Fatalf("price changed by a refused edit: %d %v", btc, err)
	}
	e.check(e.do("POST", "/listings", s, inventoryForm("", map[string]string{"price_btc": "0.0001"})), 303)
	e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"price_btc": "0.0001"})), 303)
	// Monero prices have their own floor: TestListingRefusesSubFloorXMRPrice.
}
