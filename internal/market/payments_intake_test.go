package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPaymentIntakePolicyPreservesExistingSettlement(t *testing.T) {
	for _, cur := range []string{"BTC", "XMR"} {
		t.Run(cur, func(t *testing.T) {
			p := newPayEnv(t)
			wallet := newFakeProvider(cur, 3)
			p.A.payments[cur] = wallet
			change := func(session, enabled, password string) int {
				return p.do("POST", "/admin/payment-intake", session, url.Values{"currency": {cur}, "enabled": {enabled}, "password": {password}}).Code
			}
			if change(p.buyerSess, "false", testPassword) != 403 || change(p.adminSess, "false", "wrong-password") != 401 {
				t.Fatal("intake changes did not require administrator authorization and password")
			}
			draft := p.testEnv.order(p.buyer.ID, p.productID, cur, stateDraft)
			p.check(p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {draft}}), 303)
			addr := p.str("SELECT address FROM payment_addresses WHERE order_id=$1", draft)
			if change(p.adminSess, "false", testPassword) != 303 {
				t.Fatal("administrator could not pause intake")
			}
			next := p.testEnv.order(p.buyer.ID, p.productID, cur, stateDraft)
			p.check(p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {next}}), 409)
			if p.state(next) != stateDraft || p.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", next) != 0 ||
				p.count("SELECT stock FROM products WHERE id=$1", p.productID) != 4 {
				t.Fatal("paused request changed stock, order or address")
			}
			page := p.page("/order?id="+next, p.buyerSess)
			if !strings.Contains(page, "New payments paused") || strings.Contains(page, `action="/orders/pay"`) || !strings.Contains(page, "TESTNET payments") {
				t.Fatal("paused draft does not explain intake policy separately from wallet operation")
			}
			if !strings.Contains(p.page("/admin", p.adminSess), "Enable new "+cur+" payments") {
				t.Fatal("administrator cannot re-enable currency")
			}
			other := "BTC"
			if cur == "BTC" {
				other = "XMR"
			}
			if p.str("SELECT value FROM settings WHERE key=$1", paymentIntakeKey(other)) != "true" {
				t.Fatal("pausing one currency affected the other")
			}
			// Saving a payout address and settling a previously issued address remain available.
			p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {cur}, "address": {"fake-testnet-vendor"}, "password": {testPassword}}), 303)
			total := int64(100000)
			if cur == "XMR" {
				total = 500000000000
			}
			wallet.Deposit(addr, "tx-intake-paused", 0, total, 3)
			p.poll()
			p.complete(draft)
			p.poll()
			if state, _, _, _ := p.payout(draft); state != "sent" || len(wallet.Sends()) != 1 {
				t.Fatal("pausing intake stopped existing payment confirmation or payout")
			}
			if change(p.adminSess, "true", testPassword) != 303 {
				t.Fatal("administrator could not re-enable intake")
			}
			p.check(p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {next}}), 303)
			if p.count("SELECT count(*) FROM audit_events WHERE action LIKE '%new ' || $1 || ' payment%'", cur) != 2 {
				t.Fatal("intake changes were not audited exactly once each")
			}
		})
	}
}

type intakeBlockingProvider struct {
	*fakeProvider
	entered, release chan struct{}
}

func (p *intakeBlockingProvider) NewAddress(ctx context.Context, order string) (string, error) {
	close(p.entered)
	select {
	case <-p.release:
		return p.fakeProvider.NewAddress(ctx, order)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestPaymentIntakePauseSerializesWithAddressIssuance(t *testing.T) {
	p := newPayEnv(t)
	wallet := &intakeBlockingProvider{fakeProvider: p.fake, entered: make(chan struct{}), release: make(chan struct{})}
	p.A.payments["BTC"] = wallet
	order := p.testEnv.order(p.buyer.ID, p.productID, "BTC", stateDraft)
	paid, paused := make(chan int, 1), make(chan int, 1)
	go func() { paid <- p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {order}}).Code }()
	select {
	case <-wallet.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("payment request never reached wallet")
	}
	go func() {
		paused <- p.do("POST", "/admin/payment-intake", p.adminSess, url.Values{"currency": {"BTC"}, "enabled": {"false"}, "password": {testPassword}}).Code
	}()
	// Observe the UPDATE actually waiting for the payment transaction's shared lock.
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		if p.count("SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'UPDATE settings SET value=$2 WHERE key=$1%' AND wait_event_type='Lock'") > 0 {
			blocked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(wallet.release)
	if !blocked || <-paid != 303 || <-paused != 303 {
		t.Fatal("pause did not serialize after the already accepted payment request")
	}
	next := p.testEnv.order(p.buyer.ID, p.productID, "BTC", stateDraft)
	p.check(p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {next}}), 409)
}

func TestPaymentIntakeCannotConfigureMissingWallet(t *testing.T) {
	p := newPayEnv(t)
	p.check(p.do("POST", "/admin/payment-intake", p.adminSess, url.Values{"currency": {"XMR"}, "enabled": {"true"}, "password": {testPassword}}), 409)
	p.check(p.do("POST", "/admin/payment-intake", p.adminSess, url.Values{"currency": {"USD"}, "enabled": {"true"}, "password": {testPassword}}), 400)
}
