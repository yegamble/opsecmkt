package market

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Escrow Phase 1, slice P1-b: the escrow watcher. Funding is recorded per output in payments (settlement_id NULL
// until P1-c); an escrow order completes on funding through the system edge delivered -> completed; no payouts row
// is ever written; settlements (claims and refunds) are P1-c. Simulated RPC, not chain evidence.

func (x *escrowEnv) poll() {
	x.t.Helper()
	if err := x.A.escrowPollOnce(context.Background()); err != nil {
		x.t.Fatalf("escrow watcher pass: %v", err)
	}
}

func (x *escrowEnv) events(order string) []string {
	return agStrings(x.t, x.testEnv, "SELECT from_state||'>'||to_state||' '||coalesce(actor_id,'system')||' '||note FROM order_events WHERE order_id=$1 ORDER BY id", order)
}

func (x *escrowEnv) noPayouts(order string) {
	x.t.Helper()
	if n := agInt(x.testEnv, "SELECT count(*) FROM payouts WHERE order_id=$1", order); n != 0 {
		x.t.Fatalf("%d payouts rows for an escrow order", n)
	}
}

// Exact funding in one output: recorded unconfirmed first, then at the threshold the order is paid, the content is
// delivered and the order completes, each move by the system through the transition table, and no payout exists.
func TestEscrowFundingCompletesOrder(t *testing.T) {
	x := newEscrowEnv(t)
	addr := x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	x.core.deposit(addr, "f1", 0, 100000, 1)
	x.poll()
	if x.state(x.order) != stateAwaitingPayment {
		t.Fatal("paid below the confirmation threshold")
	}
	rows := agStrings(t, x.testEnv, "SELECT txid||':'||idx||' '||amount||' '||confirmations||' '||credited||' '||coalesce(settlement_id::text,'null')||' '||address FROM payments WHERE order_id=$1", x.order)
	if !slices.Equal(rows, []string{"f1:0 100000 1 false null " + addr}) {
		t.Fatalf("ledger %q", rows)
	}
	if body := x.page("/order?id="+x.order, x.buyerSess); !strings.Contains(body, "Waiting for 3 confirmations") {
		t.Fatalf("unconfirmed page: %s", body)
	}
	x.core.setConfs("f1", 3)
	x.poll()
	if x.state(x.order) != stateCompleted {
		t.Fatalf("state %s", x.state(x.order))
	}
	ev := x.events(x.order)
	want := []string{
		"draft>awaiting_payment " + x.buyer + " Escrow address requested",
		"awaiting_payment>paid system TESTNET regtest escrow funded: 0.001 BTC in 1 output(s) with at least 3 confirmations",
		"paid>delivered system Automatic delivery after confirmed payment",
		"delivered>completed system " + escrowCompletedNote,
	}
	if !slices.Equal(ev, want) {
		t.Fatalf("events\n%s\nwant\n%s", strings.Join(ev, "\n"), strings.Join(want, "\n"))
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND credited AND settlement_id IS NULL", x.order) != 1 {
		t.Fatal("funding output not credited")
	}
	x.noPayouts(x.order)
	if agInt(x.testEnv, "SELECT count(*) FROM deliveries WHERE order_id=$1 AND content=$2", x.order, escrowDelivery) != 1 {
		t.Fatal("content not delivered")
	}
	body := x.page("/order?id="+x.order, x.buyerSess)
	for _, want := range []string{escrowDelivery, "Completed", "The vendor claims the escrowed coins by signing a settlement", "not available in this build yet"} {
		if !strings.Contains(body, want) {
			t.Fatalf("completed page lacks %q", want)
		}
	}
	// Further passes change nothing.
	x.poll()
	if len(x.events(x.order)) != 4 || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1", x.order) != 1 {
		t.Fatal("a second pass changed the order")
	}
}

// Underpayment, several outputs and overpayment.
func TestEscrowFundingUnderOverAndSplit(t *testing.T) {
	x := newEscrowEnv(t)
	addr := x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	x.core.deposit(addr, "u1", 0, 40000, 6)
	x.poll()
	if x.state(x.order) != stateAwaitingPayment {
		t.Fatal("underpaid order moved")
	}
	if body := x.page("/order?id="+x.order, x.buyerSess); !strings.Contains(body, "0.0006") || !strings.Contains(body, "Confirmed deposits are below the price") {
		t.Fatalf("underpaid page: %s", body)
	}
	// Two more outputs of one transaction complete it.
	x.core.deposit(addr, "u2", 0, 30000, 4)
	x.core.deposit(addr, "u2", 1, 30000, 4)
	x.poll()
	if x.state(x.order) != stateCompleted || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND credited", x.order) != 3 {
		t.Fatalf("split funding: %s", x.state(x.order))
	}
	x.noPayouts(x.order)

	// Overpayment: completes, and the excess is noted as staying in the escrow until the settlement (P1-c).
	order2 := x.draft()
	addr2 := x.mustCheckout(order2, "bcrt1qbuyerrefund00000000000000000000002")
	x.core.deposit(addr2, "o1", 0, 150000, 3)
	x.poll()
	if x.state(order2) != stateCompleted {
		t.Fatal("overpaid order not completed")
	}
	ev := strings.Join(x.events(order2), "\n")
	if !strings.Contains(ev, "0.0005 BTC more than the price") || !strings.Contains(ev, "stays in the escrow") {
		t.Fatalf("overpayment note missing: %s", ev)
	}
	x.noPayouts(order2)
}

// Expiry: an escrow order with nothing received is cancelled when the payment window ends (no coins, nothing to
// refund) and its unit returned. One underpaid past the window is not cancelled: refunding an escrow is a settlement,
// which P1-c builds; the parties are told once, and a top-up to the price still completes it. Cancelling an escrow
// order that holds coins, or disputing one, is refused until P1-c.
func TestEscrowExpiryAndRefusedSettlements(t *testing.T) {
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", x.order)
	x.poll()
	if x.state(x.order) != stateCancelled || agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", x.product) != 5 {
		t.Fatalf("unpaid escrow not cancelled: %s", x.state(x.order))
	}
	x.noPayouts(x.order)

	order2 := x.draft()
	addr2 := x.mustCheckout(order2, "bcrt1qbuyerrefund00000000000000000000002")
	x.core.deposit(addr2, "short", 0, 40000, 5)
	x.poll()
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", order2)
	x.DB.Exec("UPDATE payments SET created=now()-interval '25 hours' WHERE order_id=$1", order2)
	notes := func() int {
		return agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE body LIKE '%payment window ended with 0.0004 of 0.001 BTC%' AND user_id IN ($1,$2)", x.buyer, x.vendor)
	}
	x.poll()
	x.poll()
	if x.state(order2) != stateAwaitingPayment || notes() != 2 || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND underpaid_noted IS NOT NULL", order2) != 1 {
		t.Fatalf("underpaid past the window: %s, %d notices", x.state(order2), notes())
	}
	if ev := strings.Join(x.events(order2), "\n"); strings.Count(ev, "handled by the escrow settlement step") != 1 {
		t.Fatalf("underpaid note: %s", ev)
	}
	body := x.page("/order?id="+order2, x.buyerSess)
	if !strings.Contains(body, "Refunding an escrow is part of the settlement step") || strings.Contains(body, `<input type="hidden" name="from" value="awaiting_payment">`) {
		t.Fatalf("underpaid page offers a cancel: %s", body)
	}
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		w := x.do("POST", "/orders/cancel", sess, url.Values{"order_id": {order2}, "from": {stateAwaitingPayment}, "amount_seen": {"40000"}, "payout_confirmed": {"confirmed"}})
		if w.Code != 409 || !strings.Contains(w.Body.String(), "settlement step") {
			t.Fatalf("cancel with coins in the escrow: %d %s", w.Code, w.Body.String())
		}
	}
	// A top-up completes it.
	x.core.deposit(addr2, "topup", 0, 60000, 3)
	x.poll()
	if x.state(order2) != stateCompleted {
		t.Fatalf("topped-up order: %s", x.state(order2))
	}
	x.noPayouts(order2)

	// An escrow order whose content cannot be delivered automatically stays paid: no dispute and no cancel (P1-c);
	// the vendor delivers by hand and it completes.
	order3 := x.draft()
	addr3 := x.mustCheckout(order3, "bcrt1qbuyerrefund00000000000000000000003")
	x.DB.Exec("UPDATE products SET delivery_content='' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order3)
	x.core.deposit(addr3, "p3", 0, 100000, 3)
	x.poll()
	if x.state(order3) != statePaid {
		t.Fatalf("undeliverable order: %s", x.state(order3))
	}
	body = x.page("/order?id="+order3, x.buyerSess)
	if strings.Contains(body, `action="/disputes"`) || !strings.Contains(body, "settlement step") {
		t.Fatalf("paid escrow order page offers a dispute: %s", body)
	}
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order3}, "reason": {"The content was never delivered to me."}}); w.Code != 409 {
		t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
	if w := x.do("POST", "/orders/cancel", x.vendorSess, url.Values{"order_id": {order3}, "from": {statePaid}, "amount_seen": {"100000"}, "payout_confirmed": {"confirmed"}}); w.Code != 409 {
		t.Fatalf("vendor cancel of a paid escrow order: %d %s", w.Code, w.Body.String())
	}
	if w := x.do("POST", "/orders/deliver", x.vendorSess, url.Values{"order_id": {order3}, "content": {"by hand"}}); w.Code != 303 {
		t.Fatalf("deliver: %d %s", w.Code, w.Body.String())
	}
	if x.state(order3) != stateCompleted {
		t.Fatalf("delivered by hand: %s", x.state(order3))
	}
	x.noPayouts(order3)
}

// A deposit seen, the watcher restarted, then confirmed: the order completes once, with one ledger row.
func TestEscrowWatcherRestartMidFunding(t *testing.T) {
	x := newEscrowEnv(t)
	addr := x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	x.core.deposit(addr, "r1", 0, 100000, 1)
	x.poll()
	x.restart()
	x.core.setConfs("r1", 2)
	x.poll()
	if x.state(x.order) != stateAwaitingPayment {
		t.Fatal("paid early")
	}
	x.restart()
	x.core.setConfs("r1", 3)
	x.poll()
	x.poll()
	if x.state(x.order) != stateCompleted || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1", x.order) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND to_state='completed'", x.order) != 1 {
		t.Fatal("restart mid-funding")
	}
	x.noPayouts(x.order)
	// A wallet read failure leaves everything as it was and never expires the order.
	order2 := x.draft()
	x.mustCheckout(order2, "bcrt1qbuyerrefund00000000000000000000002")
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", order2)
	x.core.txErr = -4
	if err := x.A.escrowPollOnce(context.Background()); err == nil {
		t.Fatal("a failed wallet read reported success")
	}
	if x.state(order2) != stateAwaitingPayment {
		t.Fatal("expired while the wallet could not be read")
	}
}

// No payouts row for an escrow order, and the send claim never claims one (A-244's carry-over, ES-1): a pending
// payout that existed before its order became an escrow order (as by hand) is never sent.
func TestEscrowSendClaimExcludesEscrowOrders(t *testing.T) {
	x := newEscrowEnv(t)
	fake := newFakeProvider("BTC", 3)
	x.A.payments["BTC"] = fake
	addr, _ := fake.NewAddress(context.Background(), x.order)
	x.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'BTC',$2,'fake')", x.order, addr)
	if _, err := x.DB.Exec("INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state) VALUES($1,'release',$2,'BTC',100000,'fake-testnet-vendor','pending')", x.order, x.vendor); err != nil {
		t.Fatal(err)
	}
	if _, err := x.DB.Exec("INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,'bcrt1qhand',nextval('escrow_market_index'),'bcrt1qhandrefund',0,'')", x.order); err != nil {
		t.Fatal(err)
	}
	x.DB.Exec("UPDATE orders SET state='completed' WHERE id=$1", x.order)
	if err := x.A.sendPayouts(context.Background(), fake, []string{x.order}); err != nil {
		t.Fatal(err)
	}
	if len(fake.Sends()) != 0 || agInt(x.testEnv, "SELECT count(*) FROM payouts WHERE order_id=$1 AND state='pending'", x.order) != 1 {
		t.Fatalf("escrow order payout claimed: %d sends", len(fake.Sends()))
	}
	// A pooled order's payout in the same pass is still sent.
	pooled := x.testEnv.order(x.buyer, x.testEnv.product(x.vendor, "physical"), "BTC", stateCompleted)
	x.DB.Exec("INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state) VALUES($1,'release',$2,'BTC',100000,'fake-testnet-vendor','pending')", pooled, x.vendor)
	if err := x.A.sendPayouts(context.Background(), fake, []string{x.order, pooled}); err != nil {
		t.Fatal(err)
	}
	if len(fake.Sends()) != 1 {
		t.Fatalf("%d sends, want the pooled one", len(fake.Sends()))
	}
}

// settleTerminal: an escrow order in a terminal state never gets a payouts row and the transition succeeds.
func TestEscrowTerminalWritesNoPayout(t *testing.T) {
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {x.order}, "from": {stateAwaitingPayment}}); w.Code != 303 {
		t.Fatalf("cancel with nothing received: %d %s", w.Code, w.Body.String())
	}
	x.noPayouts(x.order)
	if agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", x.product) != 5 {
		t.Fatal("stock")
	}
}
