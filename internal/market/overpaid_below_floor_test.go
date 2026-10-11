package market

import "testing"

// Historical overpayments remain records and cannot promise a new automatic payout.
func TestHistoricalOverpaidOrderDoesNotPromiseSend(t *testing.T) {
	p := newPayEnv(t)
	agExec(p.testEnv, "UPDATE orders SET state='paid',amount=5000 WHERE id=$1", p.order)
	agExec(p.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,credited) VALUES($1,'BTC','historical-overpayment',0,'legacy-overpaid',100000,3,true)", p.order)
	for _, session := range []string{p.buyerSess, p.vendorSess} {
		body := p.page("/order?id="+p.order, session)
		mustContain(t, body, "Historical pooled order", "payment and payout actions are retired")
		mustNotContain(t, body, "Completing releases", "payment watcher sends", "save a payout address")
	}
}
