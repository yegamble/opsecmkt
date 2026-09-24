package market

import (
	"context"
	"database/sql"
	"net/url"
	"testing"
)

func TestReviewPaymentProviderDisabledDuringReservation(t *testing.T) {
	p := newPayEnv(t)
	order := p.testEnv.order(p.buyer.ID, p.productID, "BTC", stateDraft)
	// Simulate the watcher disabling the provider after transition's availability
	// check, while the reservation is still in progress. Tests are not parallel.
	original := transitionHooks
	transitionHooks = append(append([]transitionHook(nil), original...), func(_ context.Context, a *App, _ *sql.Tx, o *Order, _, to string) error {
		if o.ID == order && to == stateAwaitingPayment {
			a.payMu.Lock()
			delete(a.payments, "BTC")
			a.payMu.Unlock()
		}
		return nil
	})
	t.Cleanup(func() { transitionHooks = original })
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("provider disabled during reservation panicked: %v", r)
		}
	}()
	p.check(p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {order}}), 409)
	if p.state(order) != stateDraft || p.count("SELECT stock FROM products WHERE id=$1", p.productID) != 5 ||
		p.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", order) != 0 ||
		p.count("SELECT count(*) FROM order_events WHERE order_id=$1", order) != 0 {
		t.Fatal("unavailable payment request did not roll back completely")
	}
}

func TestReviewFlaggedCreditedDepositRecoveryAfterOneDay(t *testing.T) {
	for _, age := range []string{"2 days", "31 days"} {
		t.Run(age, func(t *testing.T) {
			p := newPayEnv(t)
			p.paidByWatcher(p.order, p.addr, "tx-recovery")
			p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
			p.complete(p.order)
			p.fake.Drop("tx-recovery")
			p.poll()
			if st, _, _, _ := p.payout(p.order); st != "held" {
				t.Fatalf("missing credited deposit: payout=%s", st)
			}
			agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '2 days' WHERE id=$1", p.order)
			agExec(p.testEnv, "UPDATE payment_addresses SET created=now()-$2::interval WHERE order_id=$1", p.order, age)
			p.fake.Deposit(p.addr, "tx-recovery", 0, 100000, 3)
			p.poll()
			st, _, _, _ := p.payout(p.order)
			// A-97: a payout the watcher held keeps its order watched, so it resumes whatever the address's age.
			p.poll()
			if st != "sent" || len(p.fake.Sends()) != 1 {
				t.Fatalf("reconfirmed deposit did not release exactly once: state=%s sends=%d", st, len(p.fake.Sends()))
			}
			if p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND credited AND flagged AND confirmations=3", p.order) != 1 ||
				p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%conflicted or missing%'", p.order) != 1 {
				t.Fatal("recovery lost the conflict history or repeated its notification")
			}
		})
	}
}
