package market

import (
	"context"
	"strings"
	"testing"
)

// A-99: a payout whose counted sum is below the currency's minimum automatic payout (Bitcoin: every kind,
// because sendtoaddress subtracts the fee; Monero: refunds, because the wallet pays the fee on top) is never
// queued. Its deposits are flagged to staff once instead; an above-floor payout is queued as before.

// floorFlags counts the order's system flag notes for payouts below the minimum.
func (p *payEnv) floorFlags(order string) int {
	p.t.Helper()
	return p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND note LIKE '%below the minimum automatic%' AND note LIKE '%Moderator review required.'", order)
}

// reviewNotices counts "Payment review needed" notifications for order sent to user.
func (p *payEnv) reviewNotices(user, order string) int {
	p.t.Helper()
	return p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", user, "Payment review needed for order "+order[:8]+"%")
}

func TestDustBTCRefundIsFlaggedNotQueued(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	var adminID string
	if err := p.DB.QueryRow("SELECT user_id FROM sessions WHERE token_hash=$1", digest(p.adminSess)).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	txid := longTxid("d5")
	p.fake.Deposit(p.addr, txid, 0, 1000, 3)
	p.poll()
	p.move(p.order, stateAwaitingPayment, stateCancelled, p.buyer)
	p.poll()
	p.poll()

	if s := p.state(p.order); s != stateCancelled {
		t.Fatalf("order state %s", s)
	}
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 0 {
		t.Fatalf("dust refund queued: %d payout rows", n)
	}
	if s := p.fake.Sends(); len(s) != 0 {
		t.Fatalf("dust refund sent: %+v", s)
	}
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND flagged AND NOT credited", p.order); n != 1 {
		t.Fatalf("dust deposit flagged (uncredited): %d", n)
	}
	if n := p.floorFlags(p.order); n != 1 {
		t.Fatalf("flag notes after two passes: %d", n)
	}
	var note string
	if err := p.DB.QueryRow("SELECT note FROM order_events WHERE order_id=$1 AND note LIKE '%below the minimum automatic%'", p.order).Scan(&note); err != nil {
		t.Fatal(err)
	}
	mustContain(t, note, "TESTNET refund of 0.00001 BTC to the buyer", "minimum automatic payout of 0.0001 BTC", " "+truncate(txid, 20)+" ")
	for name, uid := range map[string]string{"moderator": p.mod.ID, "admin": adminID} {
		if n := p.reviewNotices(uid, p.order); n != 1 {
			t.Fatalf("%s review notifications: %d", name, n)
		}
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%being refunded%'", p.buyer.ID); n != 0 {
		t.Fatal("buyer told a dust refund is on its way")
	}
	for name, uid := range map[string]string{"buyer": p.buyer.ID, "vendor": p.vendor.ID} {
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%refund of 0.00001 BTC to the buyer is below the minimum automatic payout of 0.0001 BTC and was not sent.%'", uid); n != 1 {
			t.Fatalf("%s told the refund was not sent: %d", name, n)
		}
	}
	// Staff can read the order and find it open on the payment-review list with the reason.
	mustContain(t, p.page("/order?id="+p.order, p.adminSess), "Moderator review (read-only)", "below the minimum automatic payout")
	desk := p.page("/moderator", p.adminSess)
	mustContain(t, desk, "1 open flag", p.order, "Below the minimum automatic payout, not paid out")

	// A later deposit that lifts the sum over the floor is refunded with the dust, once, and closes the flag.
	p.fake.Deposit(p.addr, longTxid("d6"), 0, 20000, 3)
	p.poll()
	p.poll()
	if st, _, addr, amt := p.payout(p.order); st != "sent" || addr != "fake-testnet-buyer" || amt != 21000 {
		t.Fatalf("refund after the floor was reached: %s %s %d", st, addr, amt)
	}
	if s := p.fake.Sends(); len(s) != 1 || s[0].Amount != 21000 {
		t.Fatalf("sends %+v", s)
	}
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND credited AND NOT below_floor", p.order); n != 2 {
		t.Fatalf("paid deposits credited and unmarked: %d", n)
	}
	if n := p.floorFlags(p.order); n != 1 {
		t.Fatalf("flag notes after the payout: %d", n)
	}
	mustContain(t, p.page("/moderator", p.adminSess), "No flags are open.")
}

// Boundary and regression pin: exactly the floor is queued and sent as before; one satoshi less is flagged.
func TestBTCPayoutFloorBoundary(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.fake.Deposit(p.addr, "tx-floor", 0, minPayoutBTC, 3)
	p.poll()
	p.move(p.order, stateAwaitingPayment, stateCancelled, p.buyer)
	if st, _, addr, amt := p.payout(p.order); st != "pending" || addr != "fake-testnet-buyer" || amt != minPayoutBTC {
		t.Fatalf("refund at the floor: %s %s %d", st, addr, amt)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note='Queued TESTNET refund of 0.0001 BTC to the buyer (single attempt).'", p.order); n != 1 {
		t.Fatalf("queued note: %d", n)
	}
	p.poll()
	if s := p.fake.Sends(); len(s) != 1 || s[0].Amount != minPayoutBTC || s[0].To != "fake-testnet-buyer" {
		t.Fatalf("sends %+v", s)
	}
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND (flagged OR below_floor OR NOT credited)", p.order); n != 0 {
		t.Fatalf("floor refund flagged or uncredited: %d", n)
	}

	below, addr := p.newOrder(stateAwaitingPayment)
	p.fake.Deposit(addr, "tx-below", 0, minPayoutBTC-1, 3)
	p.poll()
	p.move(below, stateAwaitingPayment, stateCancelled, p.buyer)
	p.poll()
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", below); n != 0 {
		t.Fatalf("refund one satoshi below the floor queued: %d", n)
	}
	if n := p.floorFlags(below); n != 1 {
		t.Fatalf("flag notes: %d", n)
	}
}

// A release below the Bitcoin floor (a listing priced before the floor existed) completes the order without a
// payout; its credited deposit stays credited, is flagged once and the complete form says it will not be sent.
func TestDustBTCReleaseCompletesWithoutPayout(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	if _, err := p.DB.Exec("UPDATE orders SET amount=5000 WHERE id=$1", p.order); err != nil {
		t.Fatal(err)
	}
	p.fake.Deposit(p.addr, "tx-small", 0, 5000, 3)
	p.poll()
	if s := p.state(p.order); s != statePaid {
		t.Fatalf("state %s", s)
	}
	p.move(p.order, statePaid, stateShipped, p.vendor)
	mustContain(t, p.page("/order?id="+p.order, p.buyerSess), "below the minimum automatic payout of 0.0001 BTC")
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	p.poll()
	p.poll()
	if s := p.state(p.order); s != stateCompleted {
		t.Fatalf("state %s", s)
	}
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 0 || len(p.fake.Sends()) != 0 {
		t.Fatalf("dust release queued: %d rows, sends %+v", n, p.fake.Sends())
	}
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND credited AND flagged AND below_floor", p.order); n != 1 {
		t.Fatalf("credited dust deposit flagged: %d", n)
	}
	if n := p.floorFlags(p.order); n != 1 || p.reviewNotices(p.mod.ID, p.order) != 1 {
		t.Fatalf("flag notes %d, moderator notifications %d", n, p.reviewNotices(p.mod.ID, p.order))
	}
	mustContain(t, p.page("/moderator", p.adminSess), "1 open flag", "Below the minimum automatic payout, not paid out")
}

// Monero: refunds below the minimum automatic refund are flagged, not queued; releases have no floor.
func TestDustXMRRefundIsFlaggedNotQueued(t *testing.T) {
	p := newPayEnv(t)
	xf := newFakeProvider("XMR", 10)
	p.A.payments["XMR"] = xf
	if _, err := p.DB.Exec("UPDATE users SET payout_xmr=$1 WHERE id=$2", "fake-testnet-xbuyer", p.buyer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB.Exec("UPDATE users SET payout_xmr=$1 WHERE id=$2", "fake-testnet-xvendor", p.vendor.ID); err != nil {
		t.Fatal(err)
	}
	xmrOrder := func(amt int64) (string, string) {
		id := p.testEnv.order(p.buyer.ID, p.productID, "XMR", stateAwaitingPayment)
		addr, err := xf.NewAddress(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = p.DB.Exec("UPDATE orders SET amount=$2 WHERE id=$1", id, amt); err != nil {
			t.Fatal(err)
		}
		if _, err = p.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'XMR',$2,'fake')", id, addr); err != nil {
			t.Fatal(err)
		}
		return id, addr
	}

	refund, addr := xmrOrder(500000000000)
	xf.Deposit(addr, "xmr-dust", 0, minAutoRefundXMR-1, 10)
	p.poll()
	p.move(refund, stateAwaitingPayment, stateCancelled, p.buyer)
	p.poll()
	p.poll()
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", refund); n != 0 {
		t.Fatalf("dust XMR refund queued: %d", n)
	}
	if n := p.floorFlags(refund); n != 1 || p.reviewNotices(p.mod.ID, refund) != 1 {
		t.Fatalf("flag notes %d, moderator notifications %d", n, p.reviewNotices(p.mod.ID, refund))
	}
	var note string
	if err := p.DB.QueryRow("SELECT note FROM order_events WHERE order_id=$1 AND note LIKE '%below the minimum automatic%'", refund).Scan(&note); err != nil {
		t.Fatal(err)
	}
	mustContain(t, note, "TESTNET refund of 0.000999999999 XMR to the buyer", "minimum automatic refund of 0.001 XMR")

	// At the refund floor the refund is queued and sent.
	atFloor, addr2 := xmrOrder(500000000000)
	xf.Deposit(addr2, "xmr-floor", 0, minAutoRefundXMR, 10)
	p.poll()
	p.move(atFloor, stateAwaitingPayment, stateCancelled, p.buyer)
	p.poll()
	if st, _, to, amt := p.payout(atFloor); st != "sent" || to != "fake-testnet-xbuyer" || amt != minAutoRefundXMR {
		t.Fatalf("XMR refund at the floor: %s %s %d", st, to, amt)
	}

	// A small XMR release is paid as before (the wallet pays the fee on top).
	release, addr3 := xmrOrder(1000)
	xf.Deposit(addr3, "xmr-small", 0, 1000, 10)
	p.poll()
	p.move(release, statePaid, stateShipped, p.vendor)
	p.move(release, stateShipped, stateCompleted, p.buyer)
	p.poll()
	if st, _, to, amt := p.payout(release); st != "sent" || to != "fake-testnet-xvendor" || amt != 1000 {
		t.Fatalf("small XMR release: %s %s %d", st, to, amt)
	}
	if n := p.floorFlags(release); n != 0 {
		t.Fatalf("XMR release flagged: %d", n)
	}
	if s := xf.Sends(); len(s) != 2 {
		t.Fatalf("XMR sends %+v", s)
	}
}

func TestListingRefusesBTCPriceBelowPayoutFloor(t *testing.T) {
	e := newTestApp(t)
	owner, s := e.user("inv_floor", "vendor")
	p := e.product(owner, "physical")
	for _, price := range []string{"0.00000001", "0.00009999"} {
		w := e.do("POST", "/listings", s, inventoryForm("", map[string]string{"price_btc": price}))
		e.check(w, 400)
		if !strings.Contains(w.Body.String(), "at least 0.0001 BTC") {
			t.Fatalf("create at %s: reason missing", price)
		}
		e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"price_btc": price})), 400)
	}
	var btc int64
	if err := e.DB.QueryRow("SELECT btc FROM products WHERE id=$1", p).Scan(&btc); err != nil || btc != 100000 {
		t.Fatalf("price changed by a refused edit: %d %v", btc, err)
	}
	e.check(e.do("POST", "/listings", s, inventoryForm("", map[string]string{"price_btc": "0.0001"})), 303)
	e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"price_btc": "0.0001"})), 303)
	// Monero prices have no floor: the wallet pays its fee on top of a release.
	e.check(e.do("POST", "/listings/update", s, inventoryForm(p, map[string]string{"price_xmr": "0.000000000001"})), 303)
}
