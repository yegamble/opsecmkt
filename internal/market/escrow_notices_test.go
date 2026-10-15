package market

import (
	"strings"
	"testing"
)

func TestPausedIntakeNotice(t *testing.T) {
	t.Setenv("ESCROW_BTC", "off")
	e := newTestApp(t)
	vendor, vendorSess := e.user("nvendor", "vendor")
	_, buyerSess := e.user("nbuyer", "buyer")
	product := e.product(vendor, "digital")
	for _, c := range []struct{ path, sess string }{{"/", buyerSess}, {"/product?id=" + product, buyerSess}, {"/checkout?id=" + product, buyerSess}, {"/vendor-dashboard", vendorSess}} {
		w := e.do("GET", c.path, c.sess, nil)
		e.check(w, 200)
		if !strings.Contains(w.Body.String(), "New payments paused") {
			t.Fatalf("%s lacks paused notice: %s", c.path, w.Body.String())
		}
	}
}
