package market

import (
	"strings"
	"testing"
)

func TestSuspendedVendorRefusesBothEscrowCheckouts(t *testing.T) {
	t.Run("BTC", func(t *testing.T) {
		x := newEscrowEnv(t)
		order := x.draft()
		agExec(x.testEnv, "UPDATE users SET suspended_at=now() WHERE id=$1", x.vendor)
		page := x.page("/order?id="+order, x.buyerSess)
		if !strings.Contains(page, "<strong>Unavailable.</strong> "+sellerSuspended) || strings.Contains(page, `action="/orders/escrow"`) {
			t.Fatal("BTC draft page offered escrow after suspension")
		}
		before := len(x.core.calls)
		w := x.checkout(order, regtestAddr("suspended refund"))
		if w.Code != 409 || !strings.Contains(w.Body.String(), "Unavailable. "+sellerSuspended) {
			t.Fatalf("BTC checkout: %d %s", w.Code, w.Body.String())
		}
		x.nothingWritten(order)
		if len(x.core.calls) != before {
			t.Fatal("BTC escrow called Core after suspension")
		}
	})
	t.Run("XMR", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order := x.draft(x.digital(x.vendor))
		agExec(x.testEnv, "UPDATE users SET suspended_at=now() WHERE id=$1", x.vendor)
		page := x.page("/order?id="+order, x.buyerSess)
		if !strings.Contains(page, "<strong>Unavailable.</strong> "+sellerSuspended) || strings.Contains(page, `action="/orders/escrow-xmr"`) {
			t.Fatal("XMR draft page offered escrow after suspension")
		}
		before := x.fake.count()
		w := x.checkout(x.buyerSess, order, testXMRAddr("suspended refund", true))
		if w.Code != 409 || !strings.Contains(w.Body.String(), "Unavailable. "+sellerSuspended) {
			t.Fatalf("XMR checkout: %d %s", w.Code, w.Body.String())
		}
		if x.state(order) != stateDraft || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", order) != 0 || agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE state='claimed'") != 0 || x.fake.count() != before {
			t.Fatal("XMR escrow claimed a slot or made RPC after suspension")
		}
	})
}
