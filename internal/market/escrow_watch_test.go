package market

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
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
	for _, want := range []string{escrowDelivery, "Completed", "The vendor claims the escrowed coins from this page"} {
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

	// Overpayment: completes, and the note says the vendor's claim returns the excess to the buyer (P1-c1).
	order2 := x.draft()
	addr2 := x.mustCheckout(order2, "bcrt1qbuyerrefund00000000000000000000002")
	x.core.deposit(addr2, "o1", 0, 150000, 3)
	x.poll()
	if x.state(order2) != stateCompleted {
		t.Fatal("overpaid order not completed")
	}
	ev := strings.Join(x.events(order2), "\n")
	if !strings.Contains(ev, "0.0005 BTC more than the price") || !strings.Contains(ev, "the vendor's claim returns it to the buyer's refund address") {
		t.Fatalf("overpayment note missing: %s", ev)
	}
	x.noPayouts(order2)
}

// Expiry: an escrow order with nothing received is cancelled when the payment window ends (no coins, nothing to
// refund) and its unit returned. One underpaid past the window is cancelled too, and the cancellation requests the
// refund of its escrow (P1-c1); a top-up within the window still completes an order. Disputing an escrow order is
// refused until P1-c2.
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
	x.core.deposit(addr2, txidOf("short"), 0, 40000, 5)
	x.poll()
	// Within the window a top-up still completes an underpaid order.
	topped := x.draft()
	addrT := x.mustCheckout(topped, "bcrt1qbuyerrefund00000000000000000000004")
	x.core.deposit(addrT, txidOf("part"), 0, 40000, 5)
	x.poll()
	x.core.deposit(addrT, txidOf("topup"), 0, 60000, 3)
	x.poll()
	if x.state(topped) != stateCompleted {
		t.Fatalf("topped-up order: %s", x.state(topped))
	}
	x.noPayouts(topped)
	// Past the window it is cancelled, once, and its refund requested; the buyer is told once.
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", order2)
	x.DB.Exec("UPDATE payments SET created=now()-interval '25 hours' WHERE order_id=$1", order2)
	x.poll()
	x.poll()
	if x.state(order2) != stateCancelled || agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='refund'", order2) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE body LIKE '%payment window ended with 0.0004 of 0.001 BTC%so the order was cancelled%' AND user_id=$1", x.buyer) != 1 {
		t.Fatalf("underpaid past the window: %s", x.state(order2))
	}
	if ev := strings.Join(x.events(order2), "\n"); strings.Count(ev, "payment window expired underpaid: 0.0004 of 0.001 BTC received") != 1 || strings.Count(ev, "Escrow refund requested") != 1 {
		t.Fatalf("underpaid notes: %s", ev)
	}
	x.noPayouts(order2)

	// An escrow order whose content cannot be delivered automatically stays paid; the vendor delivers by hand and it
	// completes (or cancels it, which requests its refund: TestEscrowRefundRoutes). P1-c2: the buyer is offered a
	// dispute, which an administrator decides (TestEscrowDisputeDecision); a dispute on a funded order is refused only
	// once it is no longer paid, as for a pooled order.
	order3 := x.draft()
	addr3 := x.mustCheckout(order3, "bcrt1qbuyerrefund00000000000000000000003")
	x.DB.Exec("UPDATE products SET delivery_content='' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order3)
	x.core.deposit(addr3, txidOf("p3"), 0, 100000, 3)
	x.poll()
	if x.state(order3) != statePaid {
		t.Fatalf("undeliverable order: %s", x.state(order3))
	}
	body := x.page("/order?id="+order3, x.buyerSess)
	if !strings.Contains(body, `action="/disputes"`) {
		t.Fatalf("paid escrow order page offers no dispute: %s", body)
	}
	if w := x.do("POST", "/orders/deliver", x.vendorSess, url.Values{"order_id": {order3}, "content": {"by hand"}}); w.Code != 303 {
		t.Fatalf("deliver: %d %s", w.Code, w.Body.String())
	}
	if x.state(order3) != stateCompleted {
		t.Fatalf("delivered by hand: %s", x.state(order3))
	}
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order3}, "reason": {"The content was never delivered to me."}}); w.Code != 409 {
		t.Fatalf("dispute of a completed order: %d %s", w.Code, w.Body.String())
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

// A-315 (QA29-2): an escrow order always renders the escrow panel. With ESCROW_BTC switched off, a funded escrow
// order shows its escrow address and the unavailable copy, never the pooled "no funds are held for it"; the moves
// that need a settlement stay withheld, and the download (a route that does not exist with escrow off) is not offered.
func TestEscrowOrderPanelWithEscrowOff(t *testing.T) {
	x := newEscrowEnv(t)
	addr := x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	x.core.deposit(addr, "part", 0, 40000, 1)
	x.poll()
	if body := x.page("/order?id="+x.order, x.buyerSess); !strings.Contains(body, "later top-ups do not extend this fixed wait") {
		t.Fatal("no fixed confirmation wait on the open escrow order with escrow on")
	}
	done := x.draft()
	addr2 := x.mustCheckout(done, "bcrt1qbuyerrefund00000000000000000000002")
	x.core.deposit(addr2, "full", 0, 100000, 3)
	x.poll()
	if x.state(done) != stateCompleted {
		t.Fatalf("funded order %s", x.state(done))
	}
	t.Setenv("ESCROW_BTC", "off")
	x.restart()
	if x.A.escrowConfigured || x.A.escrow.Load() != nil {
		t.Fatal("escrow still on")
	}
	for _, c := range []struct{ order, addr string }{{x.order, addr}, {done, addr2}} {
		for _, sess := range []string{x.buyerSess, x.vendorSess} {
			body := x.page("/order?id="+c.order, sess)
			for _, want := range []string{"Bitcoin escrow", c.addr, "Bitcoin escrow is unavailable on this market right now", "Coins sent to it stay in the escrow"} {
				if !strings.Contains(body, want) {
					t.Fatalf("escrow order with escrow off lacks %q:\n%s", want, body)
				}
			}
			// Nor a payment window: with escrow off no escrow order expires.
			for _, bad := range []string{"No payment record", "no funds are held", "Payment is unavailable", "/escrow-descriptor", "If nothing has been received by", "saved BTC payout address", "/escrow-psbt"} {
				if strings.Contains(body, bad) {
					t.Fatalf("escrow order with escrow off shows %q:\n%s", bad, body)
				}
			}
		}
	}
	// The part-funded order can still be cancelled (a buyer is never trapped by an outage, A-322): its form states the
	// escrow refund, which waits in the database until escrow is back, and warns the buyer that escrow is unavailable.
	body := x.page("/order?id="+x.order, x.buyerSess)
	for _, want := range []string{`name="from" value="awaiting_payment"`, "Cancelling refunds the 0.0004 regtest BTC in this escrow", "Bitcoin escrow is unavailable right now."} {
		if !strings.Contains(body, want) {
			t.Fatalf("part-funded escrow order with escrow off lacks %q:\n%s", want, body)
		}
	}
	// Cancelling it requests the refund, which waits for escrow and says so; nothing claims to be built or sent.
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {x.order}, "from": {stateAwaitingPayment}, "amount_seen": {"40000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("cancel with escrow off: %d %s", w.Code, w.Body.String())
	}
	if body := x.page("/order?id="+x.order, x.buyerSess); !strings.Contains(body, "Waiting: Bitcoin escrow is unavailable on this market right now, so the market builds this settlement once it is available again.") ||
		strings.Contains(body, "Reload in a minute") {
		t.Fatalf("refund with escrow off:\n%s", body)
	}
	// A pooled draft is unchanged: no escrow panel, no checkout offer.
	if body := x.page("/order?id="+x.draft(), x.buyerSess); strings.Contains(body, "escrow") {
		t.Fatal("a pooled draft mentions escrow with escrow off")
	}
}

// N26 (A-317): an unconfirmed deposit at the end of the payment window keeps an escrow order open: it is neither
// cancelled as unpaid nor noted as underpaid, and completes once confirmed.
func TestEscrowUnconfirmedDepositAtWindowEnd(t *testing.T) {
	x := newEscrowEnv(t)
	addr := x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	x.core.deposit(addr, "slow", 0, 100000, 0)
	x.poll()
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", x.order)
	x.DB.Exec("UPDATE payments SET created=now()-interval '25 hours' WHERE order_id=$1", x.order)
	x.poll()
	if x.state(x.order) != stateAwaitingPayment || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND underpaid_noted IS NOT NULL", x.order) != 0 {
		t.Fatalf("unconfirmed deposit at window end: %s", x.state(x.order))
	}
	x.core.setConfs("slow", 3)
	x.poll()
	if x.state(x.order) != stateCompleted {
		t.Fatalf("confirmed: %s", x.state(x.order))
	}
	x.noPayouts(x.order)
}

// A-319 (QA29-6): a funded escrow order on a listing taken down is not delivered automatically, and its history note
// says what is possible for an escrow order: delivery by hand, or a cancel that requests the refund of the escrow
// (P1-c1). A pooled order keeps its note.
func TestEscrowTakedownNote(t *testing.T) {
	x := newEscrowEnv(t)
	addr := x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	if _, err := x.DB.Exec("UPDATE products SET archived=true,archived_at=now(),admin_archive_reason='counterfeit goods listed here' WHERE id=$1", x.product); err != nil {
		t.Fatal(err)
	}
	x.core.deposit(addr, "td", 0, 100000, 3)
	x.poll()
	if x.state(x.order) != statePaid {
		t.Fatalf("state %s", x.state(x.order))
	}
	want := "Not delivered automatically: an administrator took this listing down. The vendor can deliver by hand, or cancel: the escrow is then refunded to the buyer by a settlement the buyer or the vendor signs."
	if ev := x.events(x.order); ev[len(ev)-1] != "paid>paid system "+want {
		t.Fatalf("note %q", ev[len(ev)-1])
	}
	if w := x.do("POST", "/orders/cancel", x.vendorSess, url.Values{"order_id": {x.order}, "from": {statePaid}, "amount_seen": {"100000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 ||
		agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='refund' AND state='building'", x.order) != 1 {
		t.Fatalf("vendor cancel: %d", w.Code)
	}
	x.noPayouts(x.order)
	// The pooled note is unchanged.
	pooled := x.testEnv.order(x.buyer, x.product, "BTC", stateAwaitingPayment)
	tx, _ := x.DB.Begin()
	defer tx.Rollback()
	if _, err := x.A.transition(context.Background(), tx, pooled, stateAwaitingPayment, statePaid, nil, "test payment"); err != nil {
		t.Fatal(err)
	}
	var note string
	tx.QueryRow("SELECT note FROM order_events WHERE order_id=$1 ORDER BY id DESC LIMIT 1", pooled).Scan(&note)
	if note != "Not delivered automatically: an administrator took this listing down. The vendor can deliver by hand or cancel and refund." {
		t.Fatalf("pooled note %q", note)
	}
}

// A-316 (QA29-3): escrow that could not start (its node down at startup) is retried by the escrow watcher on its
// interval, as pooled providers are: once the node answers, escrow is enabled without a restart, logged once, and a
// deposit is credited. While the node stays down the disabled line is not repeated on every pass.
func TestEscrowRetriedAfterStartupFailure(t *testing.T) {
	x := newEscrowEnv(t)
	logs := captureLog(t)
	t.Setenv("PAYMENT_POLL_INTERVAL", "1s")
	srvDown(x, true)
	x.restart()
	if x.A.escrow.Load() != nil || !x.A.escrowConfigured {
		t.Fatal("escrow started with its node down")
	}
	x.A.Start(context.Background())
	waitUntil(t, 3500*time.Millisecond, func() bool { return false }) // three or more watcher passes with the node down
	if x.A.escrow.Load() != nil {
		t.Fatal("escrow enabled while its node is down")
	}
	if n := strings.Count(logs.String(), escrowLogUnchecked); n != 1 {
		t.Fatalf("disabled line logged %d times:\n%s", n, logs.String())
	}
	srvDown(x, false)
	if !waitUntil(t, 10*time.Second, func() bool { return x.A.escrow.Load() != nil }) {
		t.Fatalf("escrow not enabled after its node came back:\n%s", logs.String())
	}
	if n := strings.Count(logs.String(), escrowLogOn); n != 1 {
		t.Fatalf("enabled line logged %d times:\n%s", n, logs.String())
	}
	addr := x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	x.core.deposit(addr, "late", 0, 100000, 3)
	if !waitUntil(t, 10*time.Second, func() bool { return x.state(x.order) == stateCompleted }) {
		t.Fatalf("deposit not credited by the watcher: %s", x.state(x.order))
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND credited", x.order) != 1 {
		t.Fatal("deposit not credited")
	}
	x.noPayouts(x.order)
	if n := strings.Count(logs.String(), escrowLogOn); n != 1 {
		t.Fatalf("enabled line logged %d times:\n%s", n, logs.String())
	}
}

// A-316: a retry keeps every startup guard. A node that comes back with another arbiter fingerprint, or a watch
// wallet holding private keys, leaves escrow disabled (each line logged once); one on another chain ends the retries
// until a restart, even if it later reports regtest.
func TestEscrowRetryKeepsGuards(t *testing.T) {
	x := newEscrowEnv(t)
	srvDown(x, true)
	x.restart()
	logs := captureLog(t)
	r := &escrowRetry{last: x.A.escrowStartState}
	set := func(f func(c *fakeEscrowCore)) {
		x.core.mu.Lock()
		defer x.core.mu.Unlock()
		f(x.core)
	}
	retry := func(times int) {
		for range times {
			x.A.retryEscrow(context.Background(), r)
		}
	}
	retry(2)
	if logs.String() != "" {
		t.Fatalf("unchanged state logged again: %s", logs.String())
	}
	srvDown(x, false)
	good := x.core.arbiterDesc
	set(func(c *fakeEscrowCore) {
		c.arbiterFP, c.arbiterDesc = "0badf00d", "wpkh([0badf00d/84h/1h/0h]"+testBIP84Key[20:]+"/1/*)#x"
	})
	retry(3)
	if x.A.escrow.Load() != nil || strings.Count(logs.String(), escrowLogFingerprint) != 1 || strings.Contains(logs.String(), "0badf00d") {
		t.Fatalf("other fingerprint: %s", logs.String())
	}
	set(func(c *fakeEscrowCore) {
		c.arbiterFP, c.arbiterDesc, c.watchInfo["private_keys_enabled"] = fakeArbiterFP, good, true
	})
	retry(3)
	if x.A.escrow.Load() != nil || strings.Count(logs.String(), escrowLogWatch) != 1 {
		t.Fatalf("watch wallet with private keys: %s", logs.String())
	}
	set(func(c *fakeEscrowCore) { c.watchInfo["private_keys_enabled"], c.chain = false, "main" })
	retry(3)
	set(func(c *fakeEscrowCore) { c.chain = "regtest" })
	retry(2)
	if x.A.escrow.Load() != nil || !r.refused || strings.Count(logs.String(), escrowLogRefused) != 1 || strings.Contains(logs.String(), escrowLogOn) {
		t.Fatalf("another chain: %s", logs.String())
	}
	// A restart against the good node enables escrow again.
	x.restart()
	if x.A.escrow.Load() == nil {
		t.Fatal("escrow not enabled after a restart")
	}
}

// srvDown makes the escrow env's fake node unreachable (down) or reachable again.
func srvDown(x *escrowEnv, down bool) { x.srv.down.Store(down) }

// waitUntil polls cond every 100 ms until it holds or d has passed, and reports whether it held.
func waitUntil(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
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

// QA31-1 (ES-10): a cancelled escrow order's status never says "nothing was received" while coins may be there. With
// escrow enabled and nothing recorded it does; a deposit recorded after the order closed gives the after-close status
// (not counted, held at the escrow address); with escrow unavailable or switched off the escrow cannot be checked, so
// the status says that instead. Past the watcher's horizon (created over 30 days ago and closed over 24 hours ago)
// nothing is read any more, so the status says the address is no longer watched, never "nothing was received" (A-332).
func TestEscrowCancelledStatus(t *testing.T) {
	const nothing = "Cancelled; nothing was received in this escrow"
	const afterClose = "Cancelled; a deposit arrived after the order closed. It is not counted and is held at the escrow address."
	const unchecked = "Cancelled. This escrow cannot be checked right now because Bitcoin escrow is unavailable, so a deposit to it may not be shown here."
	const unwatched = "Cancelled. This escrow address is no longer watched: deposits are watched for 30 days after checkout (and for 24 hours after the order closes), so a deposit to it may not be shown here."
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	late := x.draft()
	lateAddr := x.mustCheckout(late, "bcrt1qbuyerrefund00000000000000000000002")
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours'")
	x.poll()
	if x.state(x.order) != stateCancelled || x.state(late) != stateCancelled {
		t.Fatalf("not cancelled at window end: %s %s", x.state(x.order), x.state(late))
	}
	old := x.draft()
	oldAddr := x.mustCheckout(old, "bcrt1qbuyerrefund00000000000000000000003")
	recent := x.draft()
	recentAddr := x.mustCheckout(recent, "bcrt1qbuyerrefund00000000000000000000004")
	fresh := x.draft()
	x.mustCheckout(fresh, "bcrt1qbuyerrefund00000000000000000000005")
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id IN ($1,$2,$3)", old, recent, fresh)
	x.poll()
	if x.state(old) != stateCancelled || x.state(recent) != stateCancelled || x.state(fresh) != stateCancelled {
		t.Fatalf("not cancelled at window end: %s %s %s", x.state(old), x.state(recent), x.state(fresh))
	}
	edge := x.draft()
	x.mustCheckout(edge, "bcrt1qbuyerrefund00000000000000000000006")
	edgeFunded := x.draft()
	edgeFundedAddr := x.mustCheckout(edgeFunded, "bcrt1qbuyerrefund00000000000000000000007")
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id IN ($1,$2)", edge, edgeFunded)
	x.poll()
	if x.state(edge) != stateCancelled || x.state(edgeFunded) != stateCancelled {
		t.Fatalf("not cancelled at window end: %s %s", x.state(edge), x.state(edgeFunded))
	}
	x.core.deposit(lateAddr, "stray", 0, 40000, 1)
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND after_close", late) != 1 {
		t.Fatal("stray deposit not recorded after close")
	}
	status := func(order, sess string) string {
		body := x.page("/order?id="+order, sess)
		for _, s := range []string{nothing, afterClose, unchecked, unwatched} {
			if strings.Contains(body, s) {
				return s
			}
		}
		return ""
	}
	// Past the horizon: checked out 31 days ago, closed 25 hours ago. The other is checked out 31 days ago but closed
	// just now, so the watcher still reads it (a deposit to either address shows whether it was read).
	// Inside the horizon with nothing recorded: closed just now (fresh, checked out 31 days ago) or checked out 25 hours
	// ago and closed 25 hours ago (x.order): each still says nothing was received, for the watcher reads both.
	x.DB.Exec("UPDATE escrows SET created=now()-interval '31 days' WHERE order_id IN ($1,$2,$3)", old, recent, fresh)
	x.DB.Exec("UPDATE orders SET updated=now()-interval '25 hours' WHERE id IN ($1,$2)", old, x.order)
	x.core.deposit(oldAddr, "strayold", 0, 30000, 1)
	x.core.deposit(recentAddr, "strayrecent", 0, 30000, 1)
	// Just inside the watcher's 30 days (29 days 23 hours), closed 25 hours ago: still read, so a page horizon shorter
	// than the watcher's (A-337) would call the empty one "no longer watched". The funded one proves it is read.
	x.DB.Exec("UPDATE escrows SET created=now()-interval '29 days 23 hours' WHERE order_id IN ($1,$2)", edge, edgeFunded)
	x.DB.Exec("UPDATE orders SET updated=now()-interval '25 hours' WHERE id IN ($1,$2)", edge, edgeFunded)
	x.core.deposit(edgeFundedAddr, "strayedge", 0, 30000, 1)
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND after_close", edgeFunded) != 1 {
		t.Fatal("the watcher did not read an escrow created 29 days 23 hours ago")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1", old) != 0 || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND after_close", recent) != 1 {
		t.Fatal("the watcher's horizon is not what the status assumes")
	}
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		if got := status(x.order, sess); got != nothing {
			t.Fatalf("enabled, cancelled, empty: %q", got)
		}
		if got := status(old, sess); got != unwatched {
			t.Fatalf("enabled, cancelled, empty, past the watcher's horizon: %q", got)
		}
		if got := status(recent, sess); got != afterClose {
			t.Fatalf("closed inside the horizon with a deposit: %q", got)
		}
		for _, inside := range []string{fresh, x.order, edge} {
			if got := status(inside, sess); got != nothing {
				t.Fatalf("enabled, cancelled, empty, inside the watcher's horizon: %q", got)
			}
		}
		if got := status(edgeFunded, sess); got != afterClose {
			t.Fatalf("created 29 days 23 hours ago, closed 25 hours ago, with a deposit: %q", got)
		}
		if got := status(late, sess); got != afterClose {
			t.Fatalf("cancelled with an after-close deposit: %q", got)
		}
	}
	// Escrow unavailable (its node down at startup), then switched off: never "nothing was received".
	srvDown(x, true)
	x.restart()
	if x.A.escrow.Load() != nil || !x.A.escrowConfigured {
		t.Fatal("escrow enabled with its node down")
	}
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		if got := status(x.order, sess); got != unchecked {
			t.Fatalf("unavailable, cancelled: %q", got)
		}
		if got := status(old, sess); got != unchecked {
			t.Fatalf("unavailable, cancelled, past the horizon: %q", got)
		}
		if got := status(late, sess); got != afterClose {
			t.Fatalf("unavailable, cancelled with an after-close deposit: %q", got)
		}
	}
	srvDown(x, false)
	t.Setenv("ESCROW_BTC", "off")
	x.restart()
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		if got := status(x.order, sess); got != unchecked {
			t.Fatalf("escrow off, cancelled: %q", got)
		}
	}
}

// QA31-3a (A-211's rule for escrow): a confirmed deposit recorded after an escrow order closed is flagged for payment
// review; the buyer (the amount, no transaction ID) and each staff member are notified once per episode, the vendor
// is not, and nothing is paid. Staff can open the order read-only.
func TestEscrowAfterCloseDepositFlagged(t *testing.T) {
	x := newEscrowEnv(t)
	_, modSess := x.user("emod_"+randomToken()[:6], "moderator")
	mod := agStrings(t, x.testEnv, "SELECT id FROM users WHERE role='moderator' AND suspended_at IS NULL")
	addr := x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", x.order)
	x.poll()
	if x.state(x.order) != stateCancelled {
		t.Fatalf("state %s", x.state(x.order))
	}
	notices := func(uid, like string) int {
		return agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", uid, like)
	}
	buyerLike := "%" + shortID(x.order) + "%after the order closed%"
	staffLike := "Payment review needed for order " + shortID(x.order) + "%"
	x.core.deposit(addr, txidOf("stray1"), 0, 40000, 1)
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", x.order) != 0 || notices(x.buyer, buyerLike) != 0 {
		t.Fatal("an unconfirmed after-close deposit was flagged")
	}
	x.core.setConfs(txidOf("stray1"), 3)
	x.poll()
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged AND after_close AND NOT credited", x.order) != 1 {
		t.Fatal("after-close escrow deposit not flagged")
	}
	body := agStrings(t, x.testEnv, "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE $2", x.buyer, buyerLike)
	if len(body) != 1 || !strings.Contains(body[0], "0.0004 BTC") || strings.Contains(body[0], txidOf("stray1")[:10]) || !strings.Contains(body[0], "escrow address") {
		t.Fatalf("buyer notice %q", body)
	}
	if n := notices(x.vendor, "%"+shortID(x.order)+"%after%closed%"); n != 0 {
		t.Fatalf("vendor told %d times", n)
	}
	for _, m := range mod {
		if n := notices(m, staffLike); n != 1 {
			t.Fatalf("staff %s told %d times", m, n)
		}
	}
	// A second deposit in the same episode: flagged and noted, nobody told again.
	x.core.deposit(addr, txidOf("stray2"), 0, 20000, 3)
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", x.order) != 2 || notices(x.buyer, buyerLike) != 1 || notices(mod[0], staffLike) != 1 {
		t.Fatal("second deposit of the episode announced again")
	}
	if ev := strings.Join(x.events(x.order), "\n"); strings.Count(ev, "Moderator review required.") != 2 || !strings.Contains(ev, "escrow address") {
		t.Fatalf("order notes:\n%s", ev)
	}
	x.noPayouts(x.order)
	page := x.page("/order?id="+x.order, modSess)
	if !strings.Contains(page, "Arrived after the order closed") || !strings.Contains(page, addr) {
		t.Fatalf("staff view of the flagged escrow order:\n%s", page)
	}
	// The same on a completed escrow order.
	done := x.draft()
	addr2 := x.mustCheckout(done, "bcrt1qbuyerrefund00000000000000000000002")
	x.core.deposit(addr2, "full", 0, 100000, 3)
	x.poll()
	x.core.deposit(addr2, txidOf("extra-done"), 0, 30000, 3)
	x.poll()
	if x.state(done) != stateCompleted || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged AND after_close", done) != 1 ||
		notices(x.buyer, "%"+shortID(done)+"%after the order closed%") != 1 {
		t.Fatal("after-close deposit on a completed escrow order not flagged once")
	}
	x.noPayouts(done)
}

// A-329: the vendor can also cancel an escrow order awaiting payment, so while escrow is not enabled its cancel form
// warns, before its button, that the buyer may already have sent coins that are not seen yet. It is absent with escrow
// enabled, never shows the buyer's wording, and the cancel stays allowed.
func TestEscrowVendorCancelFormWarnsWhileUnavailable(t *testing.T) {
	const warn = "The buyer may already have sent coins to this order's escrow address that are not seen until Bitcoin escrow is available again."
	const buyerWarn = "Coins you already sent to this order's escrow address stay there"
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	vendorForm := func() (string, string) {
		body := x.page("/order?id="+x.order, x.vendorSess)
		i := strings.Index(body, `<input type="hidden" name="from" value="awaiting_payment">`)
		if i < 0 {
			t.Fatal("no cancel form on the vendor's awaiting escrow order")
		}
		j := strings.Index(body[i:], "</form>")
		return body[i : i+j], body
	}
	check := func(want bool, label string) {
		t.Helper()
		form, body := vendorForm()
		w := strings.Index(form, warn)
		if (w >= 0) != want || strings.Contains(body, warn) != want {
			t.Fatalf("%s: warning in the vendor's cancel form %v, on the page %v, want %v", label, w >= 0, strings.Contains(body, warn), want)
		}
		if want && w > strings.Index(form, `<button`) {
			t.Fatalf("%s: the warning follows the cancel button", label)
		}
		if strings.Contains(body, buyerWarn) || strings.Contains(x.page("/order?id="+x.order, x.buyerSess), warn) {
			t.Fatalf("%s: one party's warning on the other's page", label)
		}
	}
	check(false, "escrow enabled")
	srvDown(x, true)
	x.restart()
	check(true, "escrow unavailable")
	srvDown(x, false)
	t.Setenv("ESCROW_BTC", "off")
	x.restart()
	check(true, "escrow off")
	if w := x.do("POST", "/orders/cancel", x.vendorSess, url.Values{"order_id": {x.order}, "from": {stateAwaitingPayment}}); w.Code != 303 || x.state(x.order) != stateCancelled {
		t.Fatalf("vendor cancel with escrow off: %d %s", w.Code, x.state(x.order))
	}
	if strings.Contains(x.page("/order?id="+x.order, x.vendorSess), warn) {
		t.Fatal("warning on a cancelled order")
	}
}

// A-331: the late-deposit note and the buyer's notice name the escrow node's chain (read at startup), as the pooled
// notes name their provider's network, not a fixed "regtest".
func TestEscrowLateDepositNamesTheNodesChain(t *testing.T) {
	x := newEscrowEnv(t)
	if e := x.A.escrow.Load(); e == nil || e.network != "regtest" {
		t.Fatalf("the adapter did not record the node's chain at startup: %+v", e)
	}
	addr := x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", x.order)
	x.poll()
	if x.state(x.order) != stateCancelled {
		t.Fatalf("state %s", x.state(x.order))
	}
	x.A.escrow.Load().network = "signet"
	x.core.deposit(addr, txidOf("strayn"), 0, 40000, 3)
	x.poll()
	x.poll()
	body := agStrings(t, x.testEnv, "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE $2", x.buyer, "%"+shortID(x.order)+"%after the order closed%")
	if len(body) != 1 || !strings.Contains(body[0], "(TESTNET signet)") || strings.Contains(body[0], "regtest") {
		t.Fatalf("buyer notice %q", body)
	}
	ev := strings.Join(x.events(x.order), "\n")
	if !strings.Contains(ev, "TESTNET signet deposit ") || !strings.Contains(ev, "escrow address") || strings.Contains(ev, "regtest") {
		t.Fatalf("order notes:\n%s", ev)
	}
}

// QA31-3b (amends A-322): while escrow is not enabled, the buyer's cancel form on an escrow order awaiting payment says,
// before its button, that coins already sent stay at the escrow address and are not seen until escrow is available
// again, and not to cancel after sending. The cancel stays allowed. The copy appears in no other state or view.
func TestEscrowCancelFormWarnsWhileUnavailable(t *testing.T) {
	const warn = "Coins you already sent to this order's escrow address stay there and are not seen until Bitcoin escrow is available again. If you already sent coins, do not cancel."
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	cancelForm := func(body string) (form string, ok bool) {
		i := strings.Index(body, `<input type="hidden" name="from" value="awaiting_payment">`)
		if i < 0 {
			return "", false
		}
		j := strings.Index(body[i:], "</form>")
		return body[i : i+j], true
	}
	has := func(sess string) (bool, bool) {
		form, ok := cancelForm(x.page("/order?id="+x.order, sess))
		if !ok {
			t.Fatal("no cancel form on an awaiting escrow order")
		}
		w := strings.Index(form, warn)
		if w >= 0 && w > strings.Index(form, `<button`) {
			t.Fatal("the warning follows the cancel button")
		}
		return w >= 0, strings.Contains(x.page("/order?id="+x.order, sess), warn)
	}
	if inForm, onPage := has(x.buyerSess); inForm || onPage {
		t.Fatal("warning shown with escrow enabled")
	}
	srvDown(x, true)
	x.restart()
	if inForm, _ := has(x.buyerSess); !inForm {
		t.Fatal("no warning on the buyer's cancel form with escrow unavailable")
	}
	if _, onPage := has(x.vendorSess); onPage {
		t.Fatal("warning on the vendor's page")
	}
	srvDown(x, false)
	t.Setenv("ESCROW_BTC", "off")
	x.restart()
	if inForm, _ := has(x.buyerSess); !inForm {
		t.Fatal("no warning on the buyer's cancel form with escrow off")
	}
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {x.order}, "from": {stateAwaitingPayment}}); w.Code != 303 || x.state(x.order) != stateCancelled {
		t.Fatalf("cancel with escrow off: %d %s", w.Code, x.state(x.order))
	}
	if strings.Contains(x.page("/order?id="+x.order, x.buyerSess), warn) {
		t.Fatal("warning on a cancelled order")
	}
	x.noPayouts(x.order)
}

// QA31-4 (A-316): App.escrow is set once and never cleared. After escrow is enabled, watcher passes that fail (the
// watch wallet unreadable, then the node unreachable) leave it enabled throughout and never rerun the startup checks.
func TestEscrowNeverClearedOnceSet(t *testing.T) {
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001") // an escrow for the watcher to read
	t.Setenv("PAYMENT_POLL_INTERVAL", "1s")
	x.restart()
	if x.A.escrow.Load() == nil {
		t.Fatal("escrow not enabled")
	}
	count := func(methods ...string) int {
		n := 0
		for _, m := range x.core.methods() {
			if slices.Contains(methods, m) {
				n++
			}
		}
		return n
	}
	startup := func() int { return count("getrawchangeaddress", "getaddressinfo", "getwalletinfo") }
	before := startup()
	x.core.mu.Lock()
	x.core.txErr = -4
	x.core.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x.A.Start(ctx)
	held := func(d time.Duration) {
		t.Helper()
		if waitUntil(t, d, func() bool { return x.A.escrow.Load() == nil }) {
			t.Fatal("escrow cleared after a failed watcher pass")
		}
	}
	held(3500 * time.Millisecond) // three or more failed wallet reads
	if n := count("listreceivedbyaddress"); n < 3 {
		t.Fatalf("%d wallet reads, want 3 or more failed passes", n)
	}
	if n := startup(); n != before {
		t.Fatalf("startup checks ran again on failed passes: %d calls, %d before", n, before)
	}
	srvDown(x, true)
	held(3500 * time.Millisecond) // three or more passes with the node down
	srvDown(x, false)
	if n := startup(); n != before {
		t.Fatalf("startup checks ran again with the node down: %d calls, %d before", n, before)
	}
}
