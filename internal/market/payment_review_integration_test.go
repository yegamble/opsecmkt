package market

import (
	"testing"
)

// A flag on an order without escrow (pooled history, removed by A-492) never reaches the review desk or its form.
func TestFlagWithoutEscrowIsNotAReviewWriter(t *testing.T) {
	p := newPayEnv(t)
	agExec(p.testEnv, "UPDATE orders SET state='completed' WHERE id=$1", p.order)
	addr := "historical-" + p.order[:12]
	agExec(p.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged,after_close) VALUES($1,'XMR','historical-review',0,$2,10000,3,true,true)", p.order, addr)
	_, mod := p.user("historical_reviewer_"+randomToken()[:6], "moderator")
	desk := p.page("/moderator", mod)
	mustNotContain(t, desk, `action="/moderator/payment-review"`, "historical-review")
}
