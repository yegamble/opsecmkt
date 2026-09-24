package market

import (
	"net/url"
	"strings"
	"testing"
)

// HTTP actions and the real PostgreSQL ledger/state machine, with scripted
// test-network wallet observations. This does not claim a live-chain broadcast.
func TestReviewPartialPaymentTopUpThroughFulfillmentAndPayout(t *testing.T) {
	for _, currency := range []string{"BTC", "XMR"} {
		for _, kind := range []string{"physical", "digital"} {
			t.Run(currency+"/"+kind, func(t *testing.T) {
				p := newPayEnv(t)
				wallet := newFakeProvider(currency, 3)
				p.A.payments[currency] = wallet
				product := p.product(p.vendor.ID, kind)
				total := int64(100000)
				if currency == "XMR" {
					total = 500000000000
				}
				p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{
					"currency": {currency}, "address": {"fake-testnet-vendor"}, "password": {testPassword},
				}), 303)
				draft := p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {product}, "currency": {currency}})
				p.check(draft, 303)
				location, err := url.Parse(draft.Header().Get("Location"))
				if err != nil {
					t.Fatal(err)
				}
				order := location.Query().Get("id")
				p.check(p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {order}}), 303)
				addr := p.str("SELECT address FROM payment_addresses WHERE order_id=$1", order)
				if p.count("SELECT stock FROM products WHERE id=$1", product) != 4 {
					t.Fatal("payment request did not reserve exactly one item")
				}
				wallet.Deposit(addr, "tx-partial", 0, total*3/5, 3)
				p.poll()
				p.poll()
				if p.state(order) != stateAwaitingPayment || !strings.Contains(p.page("/order?id="+order, p.buyerSess), "Confirmed deposits are below the required amount") {
					t.Fatal("confirmed partial payment must remain awaiting payment with a helpful status")
				}
				partialPage := p.page("/order?id="+order, p.buyerSess)
				if !strings.Contains(partialPage, "Send <strong>"+amount(total-total*3/5, currencyDecimals(currency))+" fake test "+currency) || !strings.Contains(partialPage, "using the same address") {
					t.Fatal("partial-payment panel does not request only the remaining top-up")
				}
				fulfill := "/orders/ship"
				fulfillment := url.Values{"order_id": {order}}
				if kind == "digital" {
					fulfill = "/orders/deliver"
					fulfillment.Set("content", "Purchased delivery: top-up regression fixture")
				}
				p.check(p.do("POST", fulfill, p.vendorSess, fulfillment), 409)
				wallet.Deposit(addr, "tx-topup", 1, total-total*3/5, 0)
				p.poll()
				if p.state(order) != stateAwaitingPayment || !strings.Contains(p.page("/order?id="+order, p.buyerSess), "waiting for 3 confirmations") {
					t.Fatal("unconfirmed top-up must not unlock fulfillment")
				}
				if !strings.Contains(p.page("/order?id="+order, p.buyerSess), "Do not send another payment; wait for confirmation.") {
					t.Fatal("fully received but unconfirmed payment still asks the buyer for more")
				}
				wallet.SetConfirmations("tx-topup", 3)
				p.poll()
				p.poll()
				if p.state(order) != statePaid || p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND credited", order) != 2 ||
					p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND to_state='paid'", order) != 1 {
					t.Fatal("confirmed top-up did not credit both deposits and fund exactly once")
				}
				p.check(p.do("POST", fulfill, p.vendorSess, fulfillment), 303)
				if kind == "digital" && !strings.Contains(p.page("/order?id="+order, p.buyerSess), "Purchased delivery: top-up regression fixture") {
					t.Fatal("buyer cannot read purchased delivery")
				}
				p.check(p.do("POST", "/orders/complete", p.buyerSess, payoutConfirmed(url.Values{"order_id": {order}}, total)), 303)
				p.check(p.do("POST", "/orders/complete", p.buyerSess, payoutConfirmed(url.Values{"order_id": {order}}, total)), 409)
				p.poll()
				p.poll()
				state, _, to, paid := p.payout(order)
				if state != "sent" || to != "fake-testnet-vendor" || paid != total || len(wallet.Sends()) != 1 || wallet.Sends()[0].Amount != total {
					t.Fatalf("partial/top-up payout: state=%s to=%s amount=%d sends=%+v", state, to, paid, wallet.Sends())
				}
				if p.count("SELECT stock FROM products WHERE id=$1", product) != 4 {
					t.Fatal("fulfillment changed the single stock reservation")
				}
			})
		}
	}
}

func TestReviewRemainingPaymentExcludesLockedAndConflictedDeposits(t *testing.T) {
	p := newPayEnv(t)
	p.fake.Deposit(p.addr, "tx-confirmed", 0, 60000, 3)
	p.fake.Deposit(p.addr, "tx-unconfirmed", 1, 20000, 0)
	p.fake.Deposit(p.addr, "tx-conflicted", 2, 100000, -1)
	p.fake.mu.Lock()
	p.fake.outputs = append(p.fake.outputs, Incoming{Address: p.addr, TxID: "tx-locked", Index: 3, Amount: 100000, Confirmations: 3, Locked: true})
	p.fake.mu.Unlock()
	p.poll()
	if p.state(p.order) != stateAwaitingPayment || !strings.Contains(p.page("/order?id="+p.order, p.buyerSess), "Send <strong>0.0002 fake test BTC") {
		t.Fatal("remaining amount must subtract usable confirmed and unconfirmed deposits only")
	}
	p.fake.Deposit(p.addr, "tx-extra-pending", 4, 50000, 0)
	p.poll()
	if !strings.Contains(p.page("/order?id="+p.order, p.buyerSess), "Do not send another payment; wait for confirmation.") {
		t.Fatal("overpayment seen must clamp the remaining amount to zero")
	}
}
