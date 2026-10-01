package market

import (
	"testing"
)

// A-213: a return is an unsent payout of the same settlement, so a credited deposit that regresses after the
// release was sent but while the return is unsent is announced like any A-98 regression. Once every payout of the
// order is sent, a regression is not announced (unchanged).

// allReviewNotices counts "Payment review needed" notifications for order, to anyone.
func (p *payEnv) allReviewNotices(order string) int {
	p.t.Helper()
	return p.count("SELECT count(*) FROM notifications WHERE body LIKE $1", "Payment review needed for order "+order[:8]+"%")
}

// regressionNotes counts the watcher's regression notes on order.
func (p *payEnv) regressionNotes(order string) int {
	p.t.Helper()
	return p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND note LIKE 'Credited %deposit%Moderator review required.'", order)
}

func TestRegressionAfterReleaseSentFlagsUnsentReturn(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	// The buyer has no payout address, so the return of the 0.0005 BTC paid over the price stays blocked.
	p.paidByWatcherAmount(150000)
	p.complete(p.order)
	p.poll()
	if st, _, _, _ := p.payout(p.order); st != "sent" {
		t.Fatalf("release %s", st)
	}
	if st, _, _ := p.returnPayout(p.order); st != "blocked" {
		t.Fatalf("return %s", st)
	}
	p.fake.Drop("tx-paid-150000")
	p.poll()
	p.poll()
	if n := p.regressionNotes(p.order); n != 1 {
		t.Fatalf("regression notes %d", n)
	}
	for _, u := range []*User{p.buyer, p.vendor} {
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", u.ID, "Order "+p.order[:8]+": credited %deposit%market staff have been notified."); n != 1 {
			t.Fatalf("%s regression notices %d", u.Handle, n)
		}
	}
	if n := p.allReviewNotices(p.order); n != 2 {
		t.Fatalf("staff notices %d", n)
	}
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND flagged AND regress_notice", p.order); n != 1 {
		t.Fatalf("flagged deposits %d", n)
	}
	if st, errText, _ := p.returnPayout(p.order); st != "held" || errText != heldReason {
		t.Fatalf("return %s %q", st, errText)
	}
	body := p.page("/order?id="+p.order, p.buyerSess)
	mustContain(t, body, "market staff review it")
	mustNotContain(t, body, "by itself")
	mustNotContain(t, p.page("/account", p.buyerSess), "by itself")
	mustNotContain(t, p.page("/order?id="+p.order, p.vendorSess), "released automatically")
	if len(p.fake.Sends()) != 1 {
		t.Fatalf("sends %d", len(p.fake.Sends()))
	}
}

func TestRegressionAfterEveryPayoutSentIsNotAnnounced(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.paidByWatcherAmount(150000)
	p.complete(p.order)
	p.poll()
	if st, _, _ := p.returnPayout(p.order); st != "sent" || len(p.fake.Sends()) != 2 {
		t.Fatalf("return %s sends %d", st, len(p.fake.Sends()))
	}
	p.fake.Drop("tx-paid-150000")
	p.poll()
	if n := p.regressionNotes(p.order); n != 0 {
		t.Fatalf("regression notes %d", n)
	}
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", p.order); n != 0 {
		t.Fatalf("flagged deposits %d", n)
	}
	if n := p.allReviewNotices(p.order); n != 0 {
		t.Fatalf("staff notices %d", n)
	}
}
