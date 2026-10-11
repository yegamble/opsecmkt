package market

import (
	"strings"
	"testing"
)

func TestEscrowFlaggedOrderReadableByIndependentStaff(t *testing.T) {
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	modID, mod := x.user("review_staff_"+randomToken()[:6], "moderator")
	addr := agStr(x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", x.order)
	agExec(x.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged,after_close) VALUES($1,'BTC',$2,0,$3,10000,3,true,true)", x.order, txidOf("flagged "+x.order), addr)
	if body := x.page("/order?id="+x.order, mod); !strings.Contains(body, "Administrator review (read-only)") && !strings.Contains(body, "Moderator review (read-only)") {
		t.Fatalf("staff cannot read escrow flag: %s", body)
	}
	if body := x.page("/moderator", mod); !strings.Contains(body, x.order) || !strings.Contains(body, "payment-review") {
		t.Fatalf("escrow flag missing from desk: %s", body)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1", modID); n != 0 {
		t.Fatalf("reader wrote %d notices", n)
	}
}

// A flag on an order without escrow (pooled history, removed by A-492) never reaches the review desk or its form.
func TestFlagWithoutEscrowIsNotAReviewWriter(t *testing.T) {
	p := newPayEnv(t)
	agExec(p.testEnv, "UPDATE orders SET state='completed' WHERE id=$1", p.order)
	addr := "historical-" + p.order[:12]
	agExec(p.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged,after_close) VALUES($1,'BTC','historical-review',0,$2,10000,3,true,true)", p.order, addr)
	_, mod := p.user("historical_reviewer_"+randomToken()[:6], "moderator")
	desk := p.page("/moderator", mod)
	mustNotContain(t, desk, `action="/moderator/payment-review"`, "historical-review")
}
