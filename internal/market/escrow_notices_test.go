package market

import (
	"strings"
	"testing"
)

// Escrow notices describe only active checkout options, including after a funded order outlives intake.
func TestEscrowSiteNotices(t *testing.T) {
	x := newEscrowEnv(t)
	for _, page := range []struct{ path, sess string }{
		{"/", x.buyerSess}, {"/product?id=" + x.product, x.buyerSess},
		{"/checkout?id=" + x.product, x.buyerSess}, {"/vendor-dashboard", x.vendorSess},
	} {
		body := x.page(page.path, page.sess)
		if strings.Contains(body, "pooled wallet") || strings.Contains(body, "Payments disabled") {
			t.Fatalf("%s describes retired payment intake: %s", page.path, body)
		}
	}
	if body := x.page("/", x.buyerSess); !strings.Contains(body, "Bitcoin escrow (regtest)") {
		t.Fatalf("catalog lacks Bitcoin escrow: %s", body)
	}
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	if body := x.page("/vendor-dashboard", x.vendorSess); !strings.Contains(body, "1 order(s) may hold coins") {
		t.Fatalf("vendor dashboard omits escrow order: %s", body)
	}
	t.Setenv("ESCROW_BTC", "off")
	x.restart()
	if body := x.page("/vendor-dashboard", x.vendorSess); !strings.Contains(body, "<strong>OFF</strong>") || !strings.Contains(body, "1 order(s) may hold coins") {
		t.Fatalf("historical escrow order vanished: %s", body)
	}
}

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

func TestEscrowStatementsSayTheMarketCosignsAndBroadcasts(t *testing.T) {
	x := newEscrowEnv(t)
	body := x.page("/", x.buyerSess)
	if !strings.Contains(body, "2-of-3 escrow") {
		t.Fatalf("catalog lacks escrow description: %s", body)
	}
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	x.fund(x.order, "partial", 3, 40000)
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		body = x.page("/order?id="+x.order, sess)
		if !strings.Contains(body, "the market then co-signs it and broadcasts it") {
			t.Fatalf("order lacks settlement explanation: %s", body)
		}
	}
}
