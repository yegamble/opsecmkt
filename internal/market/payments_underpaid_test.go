package market

import (
	"strings"
	"testing"
)

// An underpaid order (deposits confirmed but short of the price) is not left waiting forever: once its address
// is older than PAYMENT_EXPIRY and no deposit has arrived for PAYMENT_EXPIRY, the watcher cancels it and the
// confirmed deposits are refunded to the buyer. A cancel by a party states the refund it queues.

// ageDeposits moves an order's deposits and address back past the default 24h payment window.
func (p *payEnv) ageDeposits(order string) {
	p.t.Helper()
	p.expireAddress(order)
	if _, err := p.DB.Exec("UPDATE payments SET created=now()-interval '48 hours' WHERE order_id=$1", order); err != nil {
		p.t.Fatal(err)
	}
}

func TestUnderpaidOrderExpiresAndIsRefunded(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.fake.Deposit(p.addr, "tx-under", 0, 40000, 3)
	p.poll()
	if s := p.state(p.order); s != stateAwaitingPayment {
		t.Fatalf("underpaid order moved to %s", s)
	}
	// A recent top-up keeps the order open even when the address is old.
	p.expireAddress(p.order)
	p.poll()
	if s := p.state(p.order); s != stateAwaitingPayment {
		t.Fatalf("underpaid order cancelled while its last deposit is recent: %s", s)
	}
	stock := p.count("SELECT stock FROM products WHERE id=$1", p.productID)
	p.ageDeposits(p.order)
	p.poll()
	if s := p.state(p.order); s != stateCancelled {
		t.Fatalf("underpaid order past the window: %s", s)
	}
	// The same pass queues the refund and sends it.
	if st, _, addr, amt := p.payout(p.order); st != "sent" || addr != "fake-testnet-buyer" || amt != 40000 || p.sendsTo("fake-testnet-buyer") != 1 {
		t.Fatalf("underpayment refund %s %s %d", st, addr, amt)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND note='payment window expired underpaid: 0.0004 of 0.001 BTC received'", p.order); n != 1 {
		t.Fatalf("expiry note rows %d", n)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%payment window ended with 0.0004 of 0.001 BTC received%refund of 0.0004 BTC to your saved BTC payout address is queued%not refunded automatically.'", p.buyer.ID); n != 1 {
		t.Fatalf("buyer underpayment notices %d", n)
	}
	if got := p.count("SELECT stock FROM products WHERE id=$1", p.productID); got != stock+1 {
		t.Fatalf("stock %d after cancel, was %d", got, stock)
	}
	// A deposit that confirms after the cancellation is refunded later, not lost; repeated passes queue nothing more.
	p.poll()
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 1 {
		t.Fatalf("payout rows %d", n)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%confirmed after this order was cancelled%'", p.buyer.ID); n != 0 {
		t.Fatal("expiry refund announced as a late deposit")
	}
}

func TestUnderpaidOrderWithDepositConfirmingStaysOpen(t *testing.T) {
	p := newPayEnv(t)
	p.fake.Deposit(p.addr, "tx-confirmed", 0, 40000, 3)
	p.fake.Deposit(p.addr, "tx-confirming", 0, 60000, 1)
	p.poll()
	p.ageDeposits(p.order)
	p.poll()
	if s := p.state(p.order); s != stateAwaitingPayment {
		t.Fatalf("order with a deposit still confirming: %s", s)
	}
	// Once it confirms the order is paid, not cancelled.
	p.fake.SetConfirmations("tx-confirming", 3)
	p.poll()
	if s := p.state(p.order); s != statePaid {
		t.Fatalf("topped-up order: %s", s)
	}
}

// Without a payout address the refund waits (blocked) and the buyer is told to save one.
func TestUnderpaidExpiryWithoutPayoutAddressBlocksRefund(t *testing.T) {
	p := newPayEnv(t)
	p.fake.Deposit(p.addr, "tx-under", 0, 40000, 3)
	p.poll()
	p.ageDeposits(p.order)
	p.poll()
	if st, _, _, amt := p.payout(p.order); p.state(p.order) != stateCancelled || st != "blocked" || amt != 40000 {
		t.Fatalf("state %s payout %s %d", p.state(p.order), st, amt)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%payment window ended with%refund of 0.0004 BTC waits until you save a BTC payout address%not refunded automatically.'", p.buyer.ID); n != 1 {
		t.Fatalf("buyer notices naming the blocked refund %d", n)
	}
}

func TestCancelUnderpaidOrderStatesRefund(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.fake.Deposit(p.addr, "tx-under", 0, 40000, 3)
	p.poll()
	body := p.page("/order?id="+p.order, p.buyerSess)
	for _, want := range []string{"Refund 0.0004 BTC (test network) to " + p.buyer.Handle, `name="amount_seen" value="40000"`, "does not cover the price"} {
		if !strings.Contains(body, want) {
			t.Fatalf("cancel form missing %q", want)
		}
	}
	// A-215: the help addresses the buyer as "you" and never says a refund left to staff is sent automatically.
	mustContain(t, body, "queues one refund of every confirmed deposit to your saved BTC payout address (it waits until you save one)",
		"The payment watcher sends the refund automatically unless the statement below says it is below the minimum automatic amount: then it is not sent automatically, and market staff are asked to review it.")
	mustNotContain(t, body, "the buyer's saved", "until the buyer saves", "staff review it. The payment watcher sends it automatically")
	vendorPage := p.page("/order?id="+p.order, p.vendorSess)
	mustContain(t, vendorPage, "queues one refund of every confirmed deposit to the buyer's saved BTC payout address (it waits until the buyer saves one)")
	mustNotContain(t, vendorPage, "deposit to your saved", "until you save")
	cancel := form("order_id", p.order, "from", stateAwaitingPayment)
	// A form shown before the deposit arrived has no amount: refused, nothing changes.
	w := p.do("POST", "/orders/cancel", p.buyerSess, cancel)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "A deposit arrived for this order after the page was shown") {
		t.Fatalf("cancel without the refund review: %d", w.Code)
	}
	unticked := form("order_id", p.order, "from", stateAwaitingPayment, "amount_seen", "40000")
	if w = p.do("POST", "/orders/cancel", p.buyerSess, unticked); w.Code != 400 {
		t.Fatalf("cancel without the confirmation: %d", w.Code)
	}
	if w = p.do("POST", "/orders/cancel", p.buyerSess, payoutConfirmed(form("order_id", p.order, "from", stateAwaitingPayment), 30000)); w.Code != 409 || !strings.Contains(w.Body.String(), "The amount changed to 0.0004 BTC") {
		t.Fatalf("cancel with a stale amount: %d", w.Code)
	}
	if s := p.state(p.order); s != stateAwaitingPayment || p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order) != 0 {
		t.Fatalf("refused cancel changed the order: %s", s)
	}
	p.check(p.do("POST", "/orders/cancel", p.buyerSess, payoutConfirmed(form("order_id", p.order, "from", stateAwaitingPayment), 40000)), 303)
	if st, _, addr, amt := p.payout(p.order); p.state(p.order) != stateCancelled || st != "pending" || addr != "fake-testnet-buyer" || amt != 40000 {
		t.Fatalf("cancel refund %s %s %d", st, addr, amt)
	}
}

// A deposit still confirming counts as nothing yet: the form says so and it is refunded when it confirms.
func TestCancelOrderWithConfirmingDepositStatesLaterRefund(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.fake.Deposit(p.addr, "tx-confirming", 0, 40000, 1)
	p.poll()
	if body := p.page("/order?id="+p.order, p.vendorSess); !strings.Contains(body, "nothing counted yet; deposits confirming later go to the buyer") {
		t.Fatal("vendor's cancel form does not state the later refund")
	}
	p.check(p.do("POST", "/orders/cancel", p.vendorSess, payoutConfirmed(form("order_id", p.order, "from", stateAwaitingPayment), 0)), 303)
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 0 {
		t.Fatalf("refund queued before the deposit confirmed: %d", n)
	}
	p.fake.SetConfirmations("tx-confirming", 3)
	p.poll()
	if st, _, _, amt := p.payout(p.order); st != "sent" || amt != 40000 || p.sendsTo("fake-testnet-buyer") != 1 {
		t.Fatalf("late refund %s %d", st, amt)
	}
}

// An order with no deposit keeps the plain cancel form: no amount, no confirmation needed.
func TestCancelOrderWithoutDepositNeedsNoConfirmation(t *testing.T) {
	p := newPayEnv(t)
	if body := p.page("/order?id="+p.order, p.buyerSess); strings.Contains(body, `name="amount_seen"`) {
		t.Fatal("cancel form asks to confirm a refund with no deposit")
	}
	p.check(p.do("POST", "/orders/cancel", p.buyerSess, form("order_id", p.order, "from", stateAwaitingPayment)), 303)
	if s := p.state(p.order); s != stateCancelled {
		t.Fatalf("state %s", s)
	}
}

// Below the minimum automatic refund, the deadline still cancels the order, but nothing is sent: the deposit
// is flagged for staff once, as for any refund below the floor.
func TestUnderpaidExpiryBelowRefundFloorIsFlagged(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.fake.Deposit(p.addr, "tx-dust", 0, minPayoutBTC-1, 3)
	p.poll()
	p.ageDeposits(p.order)
	p.poll()
	p.poll()
	if s := p.state(p.order); s != stateCancelled {
		t.Fatalf("dust-underpaid order: %s", s)
	}
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 0 || len(p.fake.Sends()) != 0 {
		t.Fatalf("dust refund queued or sent: %d payout rows", n)
	}
	if n := p.floorFlags(p.order); n != 1 {
		t.Fatalf("floor flags %d", n)
	}
	// The notification says which case applies, and the order page states it (A-211).
	var notice string
	if err := p.DB.QueryRow("SELECT body FROM notifications WHERE user_id=$1 AND body LIKE '%payment window ended with%'", p.buyer.ID).Scan(&notice); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"0.00009999 BTC", "below the minimum automatic refund of 0.0001 BTC", "not refunded", "market staff were notified", "Anything sent to this order's address after the cancellation is not refunded automatically"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice %q lacks %q", notice, want)
		}
	}
	if strings.Contains(notice, "The order page shows the refund") || strings.Contains(notice, "is refunded to your saved") {
		t.Fatalf("notice promises a refund: %q", notice)
	}
	mustContain(t, p.page("/order?id="+p.order, p.buyerSess), "0.00009999 BTC received, below the minimum automatic refund of 0.0001 BTC; not refunded; market staff were notified")
}
