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
			mustNotContain(t, p.page("/order?id="+order, p.adminSess), want)
		}
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
		want := "Make sure the full remaining amount reaches this address after any exchange withdrawal fee; an overpayment below the minimum automatic payout of " + floor + " " + currency + " is not returned automatically and stays with the vendor."
		buyerPage := p.page("/order?id="+order, p.buyerSess)
		mustContain(t, buyerPage, want)
		mustNotContain(t, p.page("/order?id="+order, p.vendorSess), want)
		mustNotContain(t, p.page("/order?id="+order, p.adminSess), want)
	}
	// The amount-received warning is only useful while the pooled order still needs funds.
	paid, _ := p.newOrder(statePaid)
	if got := p.page("/order?id="+paid, p.buyerSess); strings.Contains(got, "Make sure the full remaining amount reaches this address") {
		t.Fatal("paid order still asks the buyer to send funds")
	}
}
