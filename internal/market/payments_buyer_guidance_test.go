package market

import (
	"context"
	"strings"
	"testing"
)

func addGuidanceOrder(t *testing.T, p *payEnv, currency, state string) (string, *fakeProvider) {
	t.Helper()
	provider := p.fake
	if currency == "XMR" {
		provider = newFakeProvider("XMR", 10)
		p.A.payments["XMR"] = provider
	}
	order := p.testEnv.order(p.buyer.ID, p.productID, currency, state)
	address, err := provider.NewAddress(context.Background(), order)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,$2,$3,'fake')", order, currency, address); err != nil {
		t.Fatal(err)
	}
	return order, provider
}

func TestBuyerPayoutAddressPromptIsCurrencySpecificAndPrivate(t *testing.T) {
	p := newPayEnv(t)
	for _, currency := range []string{"BTC", "XMR"} {
		other := "payout_btc"
		column := "payout_xmr"
		if currency == "BTC" {
			other, column = "payout_xmr", "payout_btc"
		}
		if _, err := p.DB.Exec("UPDATE users SET "+other+"='saved-in-other-currency', "+column+"='' WHERE id=$1", p.buyer.ID); err != nil {
			t.Fatal(err)
		}
		want := "You have no " + currency + " payout address saved."
		for _, state := range []string{stateAwaitingPayment, statePaid, stateShipped, stateDelivered} {
			order, _ := addGuidanceOrder(t, p, currency, state)
			buyerPage := p.page("/order?id="+order, p.buyerSess)
			mustContain(t, buyerPage, want, "pooled refund or return", `href="/account#payout">your payout settings</a>`)
			mustNotContain(t, p.page("/order?id="+order, p.vendorSess), want)
			adminPage := p.body("GET", "/order?id="+order, p.adminSess, nil, 404)
			mustNotContain(t, adminPage, want)
		}
		// Issued orders still need a saved destination while their pooled wallet is unavailable.
		offline, provider := addGuidanceOrder(t, p, currency, statePaid)
		delete(p.A.payments, currency)
		mustContain(t, p.page("/order?id="+offline, p.buyerSess), want, "Payment is not monitored")
		mustNotContain(t, p.page("/order?id="+offline, p.vendorSess), want)
		mustNotContain(t, p.body("GET", "/order?id="+offline, p.adminSess, nil, 404), want)
		p.A.payments[currency] = provider
		// A saved address for this currency suppresses the prompt, regardless of the other currency.
		if _, err := p.DB.Exec("UPDATE users SET "+column+"='saved-for-this-currency' WHERE id=$1", p.buyer.ID); err != nil {
			t.Fatal(err)
		}
		order, _ := addGuidanceOrder(t, p, currency, stateAwaitingPayment)
		mustNotContain(t, p.page("/order?id="+order, p.buyerSess), want)
	}
	// Without an issued pooled address, a record does not get pooled-wallet payout guidance.
	unissued := p.testEnv.order(p.buyer.ID, p.productID, "BTC", stateAwaitingPayment)
	mustNotContain(t, p.page("/order?id="+unissued, p.buyerSess), "You have no BTC payout address saved.")
}

func TestAwaitingBuyerCopyExplainsReceivedAmountAndSubfloorExcess(t *testing.T) {
	p := newPayEnv(t)
	for _, currency := range []string{"BTC", "XMR"} {
		order, _ := addGuidanceOrder(t, p, currency, stateAwaitingPayment)
		floor := amount(payoutFloor(currency, "refund"), currencyDecimals(currency))
		want := "Make sure the full remaining amount reaches this address after any exchange withdrawal fee. If you pay more than the price, completing includes any excess below the minimum automatic payout of " + floor + " " + currency + " with the release to the vendor; it is not returned separately."
		buyerPage := p.page("/order?id="+order, p.buyerSess)
		mustContain(t, buyerPage, want)
		mustNotContain(t, p.page("/order?id="+order, p.vendorSess), want)
		adminPage := p.body("GET", "/order?id="+order, p.adminSess, nil, 404)
		mustNotContain(t, adminPage, want)
	}
	// A full but unconfirmed deposit leaves the order awaiting payment, but the buyer has nothing more to send.
	fullSeen, fullSeenAddress := p.newOrder(stateAwaitingPayment)
	p.fake.Deposit(fullSeenAddress, "tx-full-awaiting-confirmation", 0, 100000, 0)
	p.poll()
	if got := p.page("/order?id="+fullSeen, p.buyerSess); !strings.Contains(got, "The full required amount has been seen") || strings.Contains(got, "Make sure the full remaining amount reaches this address") {
		t.Fatal("fully seen awaiting order has stale received-amount guidance")
	}
	// The amount-received warning is also absent once the order is paid.
	paid, _ := p.newOrder(statePaid)
	if got := p.page("/order?id="+paid, p.buyerSess); strings.Contains(got, "Make sure the full remaining amount reaches this address") {
		t.Fatal("paid order still asks the buyer to send funds")
	}
}
