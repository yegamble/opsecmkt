package market

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A-15 is a separate, fixed wait for an observed deposit that never confirms.
// It does not change the ordinary unpaid or fully confirmed, underpaid window.
func TestConfirmationDeadlineFloorAndLongPaymentWindow(t *testing.T) {
	t.Setenv("PAYMENT_EXPIRY", "2h")
	issued := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first := issued.Add(time.Hour)
	if got, want := confirmationDeadline(issued, first), first.Add(14*24*time.Hour); !got.Equal(want) {
		t.Fatalf("14-day floor: %s, want %s", got, want)
	}
	t.Setenv("PAYMENT_EXPIRY", "400h")
	if got, want := confirmationDeadline(issued, first), first.Add(400*time.Hour); !got.Equal(want) {
		t.Fatalf("long payment window: %s, want %s", got, want)
	}
}

func TestPooledConfirmationWaitFixedAtFirstRecordedDeposit(t *testing.T) {
	t.Setenv("PAYMENT_EXPIRY", "2h")
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.fake.Deposit(p.addr, "a15-old", 0, 20000, 0)
	p.poll()
	if _, err := p.DB.Exec("UPDATE payment_addresses SET created=now()-interval '17 days' WHERE order_id=$1", p.order); err != nil {
		t.Fatal(err)
	}
	// A deposit read just inside the floor is still protected, regardless of the old address.
	if _, err := p.DB.Exec("UPDATE payments SET created=now()-interval '13 days' WHERE order_id=$1", p.order); err != nil {
		t.Fatal(err)
	}
	p.poll()
	if got := p.state(p.order); got != stateAwaitingPayment {
		t.Fatalf("before floor: %s", got)
	}
	// A recent top-up cannot restart the first deposit's clock. A negative row
	// is retained history even if its replacement never confirms.
	p.fake.SetConfirmations("a15-old", -1)
	p.fake.Deposit(p.addr, "a15-new", 0, 30000, 0)
	p.poll()
	// Outside/left-out are mutable disposition flags. They must not erase the
	// historical first-seen anchor while a replacement is still unconfirmed.
	if _, err := p.DB.Exec("UPDATE payments SET created=now()-interval '15 days',spent_outside=true,escrow_left_out=true WHERE order_id=$1 AND txid='a15-old'", p.order); err != nil {
		t.Fatal(err)
	}
	page := p.page("/order?id="+p.order, p.buyerSess)
	if !strings.Contains(page, "later top-ups do not extend this fixed confirmation wait") {
		t.Fatal("buyer page omitted the fixed confirmation wait")
	}
	stock := p.count("SELECT stock FROM products WHERE id=$1", p.productID)
	p.fake.SetWallet(nil, nil, true)
	p.poll()
	if got := p.state(p.order); got != stateAwaitingPayment {
		t.Fatalf("syncing wallet expired order: %s", got)
	}
	p.fake.SetWallet(nil, errors.New("unavailable"), false)
	if err := p.A.pollOnce(t.Context()); err == nil {
		t.Fatal("wallet failure was not reported")
	}
	if got := p.state(p.order); got != stateAwaitingPayment {
		t.Fatalf("failed wallet read expired order: %s", got)
	}
	p.fake.SetWallet(nil, nil, false)
	p.poll()
	if got := p.state(p.order); got != stateCancelled {
		t.Fatalf("past floor with recent replacement: %s", got)
	}
	if got := p.count("SELECT stock FROM products WHERE id=$1", p.productID); got != stock+1 {
		t.Fatalf("stock %d, want %d", got, stock+1)
	}
	if got := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); got != 0 {
		t.Fatalf("unconfirmed refund queued: %d", got)
	}
	p.fake.SetConfirmations("a15-new", 3)
	p.poll()
	if state, _, _, amount := p.payout(p.order); state != "sent" || amount != 30000 {
		t.Fatalf("later confirmation refund %s %d", state, amount)
	}
	p.poll()
	if got := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); got != 1 {
		t.Fatalf("repeat payout rows %d", got)
	}
}

func TestPooledConfirmationWaitMixedFundsOnlyConfirmedPart(t *testing.T) {
	t.Setenv("PAYMENT_EXPIRY", "2h")
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.fake.Deposit(p.addr, "a15-part-confirmed", 0, 40000, 3)
	p.fake.Deposit(p.addr, "a15-part-pending", 0, 30000, 0)
	p.poll()
	if got := p.state(p.order); got != stateAwaitingPayment {
		t.Fatalf("part-funded: %s", got)
	}
	if _, err := p.DB.Exec("UPDATE payment_addresses SET created=now()-interval '17 days' WHERE order_id=$1", p.order); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB.Exec("UPDATE payments SET created=now()-interval '15 days' WHERE order_id=$1 AND txid='a15-part-confirmed'", p.order); err != nil {
		t.Fatal(err)
	}
	p.poll()
	if got := p.state(p.order); got != stateCancelled {
		t.Fatalf("mixed confirmed/pending order: %s", got)
	}
	if state, _, _, amount := p.payout(p.order); state != "sent" || amount != 40000 {
		t.Fatalf("only confirmed part refunded: %s %d", state, amount)
	}
	if got := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND credited", p.order); got != 1 {
		t.Fatalf("credited deposits %d, want only confirmed part", got)
	}
	p.fake.SetConfirmations("a15-part-pending", 3)
	p.poll()
	if got := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); got != 1 {
		t.Fatalf("late deposit duplicated payout: %d", got)
	}
	if got := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND txid='a15-part-pending' AND flagged", p.order); got != 1 {
		t.Fatalf("late deposit needing staff review: %d", got)
	}
}

func TestBitcoinEscrowConfirmationWaitCancelsWithoutSpending(t *testing.T) {
	t.Setenv("PAYMENT_EXPIRY", "2h")
	x := newEscrowEnv(t)
	addr := x.mustCheckout(x.order, regtestAddr("a15-buyer-refund"))
	old, recent := hex64("a15-escrow-old"), hex64("a15-escrow-new")
	x.core.deposit(addr, old, 0, 40000, 0)
	x.poll()
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '17 days' WHERE order_id=$1", x.order)
	agExec(x.testEnv, "UPDATE payments SET created=now()-interval '13 days' WHERE order_id=$1", x.order)
	x.poll()
	if got := x.state(x.order); got != stateAwaitingPayment {
		t.Fatalf("before floor: %s", got)
	}
	x.core.deposit(addr, recent, 0, 30000, 0)
	x.poll()
	agExec(x.testEnv, "UPDATE payments SET created=now()-interval '15 days' WHERE order_id=$1 AND txid=$2", x.order, old)
	if page := x.page("/order?id="+x.order, x.buyerSess); !strings.Contains(page, "later top-ups do not extend this fixed wait") {
		t.Fatal("escrow page omitted fixed confirmation wait")
	}
	stock := agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", x.product)
	x.core.ibd = true
	x.poll()
	if got := x.state(x.order); got != stateAwaitingPayment {
		t.Fatalf("unsynced Bitcoin node expired order: %s", got)
	}
	x.core.ibd = false
	x.poll()
	if got := x.state(x.order); got != stateCancelled {
		t.Fatalf("past floor: %s", got)
	}
	if got := agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", x.product); got != stock+1 {
		t.Fatalf("stock %d, want %d", got, stock+1)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND credited", x.order); got != 0 {
		t.Fatalf("unconfirmed credited outputs %d", got)
	}
	if sent := x.core.count("sendrawtransaction"); sent != 0 {
		t.Fatalf("unconfirmed escrow broadcast count %d", sent)
	}
	x.noPayouts(x.order)
	x.core.setConfs(old, 3)
	x.core.setConfs(recent, 3)
	x.poll()
	if got := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind IN ('refund','stray_refund')", x.order); got == 0 {
		t.Fatal("confirmed closed-order escrow lacks refund recovery")
	}
}

func TestMoneroEscrowConfirmationWaitCancelsWithoutSending(t *testing.T) {
	t.Setenv("PAYMENT_EXPIRY", "2h")
	x := newXMREscrowEnv(t)
	x.mustPoll()
	product := x.digital(x.vendor)
	order, addr := x.freshCheckout(product)
	old := hex64("a15-monero-old" + order)
	newTx := hex64("a15-monero-new" + order)
	x.fake.payAddress(addr, old, xmrListingPrice/4, 0)
	x.mustPoll()
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '17 days' WHERE order_id=$1", order)
	agExec(x.testEnv, "UPDATE payments SET created=now()-interval '13 days' WHERE order_id=$1", order)
	x.mustPoll()
	if got := x.state(order); got != stateAwaitingPayment {
		t.Fatalf("before floor: %s", got)
	}
	x.fake.payAddress(addr, newTx, xmrListingPrice/4, 0)
	x.mustPoll()
	agExec(x.testEnv, "UPDATE payments SET created=now()-interval '15 days' WHERE order_id=$1 AND txid=$2", order, old)
	if page := x.page("/order?id="+order, x.sessOf(order, roleBuyer)); !strings.Contains(page, "later top-ups do not extend this fixed wait") {
		t.Fatal("Monero escrow page omitted fixed confirmation wait")
	}
	stock := agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", product)
	x.fake.edit(func(f *fakeXMREscrow) { f.unsynced = true })
	x.mustPoll()
	if got := x.state(order); got != stateAwaitingPayment {
		t.Fatalf("unsynced Monero wallet expired order: %s", got)
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.unsynced = false })
	x.mustPoll()
	if got := x.state(order); got != stateCancelled {
		t.Fatalf("past floor: %s", got)
	}
	if got := agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", product); got != stock+1 {
		t.Fatalf("stock %d, want %d", got, stock+1)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND credited", order); got != 0 {
		t.Fatalf("unconfirmed credited outputs %d", got)
	}
	if sent := x.fake.countOf("transfer"); sent != 0 {
		t.Fatalf("unconfirmed escrow transfer count %d", sent)
	}
	if got := x.xmrSettlements(order); len(got) > 1 {
		t.Fatalf("unexpected settlements %v", got)
	}
	x.fake.setConfs(old, 10)
	x.fake.setConfs(newTx, 10)
	x.mustPoll()
	if got := x.xmrSettlements(order); len(got) == 0 {
		t.Fatal("confirmed closed-order Monero escrow lacks refund recovery")
	}
}
