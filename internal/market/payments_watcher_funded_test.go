package market

import "testing"

// A-7: funded non-terminal orders stay watched however long ago they last changed; a deposit first seen after
// funding is announced once to the order history, the buyer and the vendor, and the single release still pays
// the sum of settled deposits.
func TestWatcherAnnouncesDepositAfterFundingOnOldPaidOrder(t *testing.T) {
	p := newPayEnv(t)
	p.paidByWatcher(p.order, p.addr, "tx-fund")
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.DB.Exec("UPDATE orders SET updated=now()-interval '48 hours' WHERE id=$1", p.order)
	buyerBefore := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.buyer.ID)
	vendorBefore := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID)

	p.fake.Deposit(p.addr, "tx-extra", 1, 7000, 3)
	p.poll()
	p.poll()
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND txid='tx-extra' AND confirmations=3", p.order); n != 1 {
		t.Fatalf("extra deposit on a paid order updated 48 h ago not recorded: %d", n)
	}
	const note = "%tx-extra:1 of 0.00007 BTC received after this order was funded%"
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state='paid' AND to_state='paid' AND actor_id IS NULL AND note LIKE $2", p.order, note); n != 1 {
		t.Fatalf("post-funding deposit events: %d", n)
	}
	for _, u := range []struct {
		id     string
		before int
	}{{p.buyer.ID, buyerBefore}, {p.vendor.ID, vendorBefore}} {
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", u.id); n != u.before+1 {
			t.Fatalf("notifications for %s: %d, want %d", u.id[:8], n, u.before+1)
		}
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", u.id, note); n != 1 {
			t.Fatalf("post-funding deposit notification missing for %s", u.id[:8])
		}
	}

	// Single-payout semantics unchanged: the release pays the sum of settled deposits, once.
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	p.poll()
	if st, _, _, amt := p.payout(p.order); st != "sent" || amt != 107000 || len(p.fake.Sends()) != 1 {
		t.Fatalf("release %s amount=%d sends=%d", st, amt, len(p.fake.Sends()))
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%received after this order was funded%'", p.order); n != 1 {
		t.Fatalf("post-funding deposit events after completion: %d", n)
	}
}

// A deposit already seen while the order awaited payment is not announced as post-funding, and a locked transfer
// is left to the locked-transfer report.
func TestWatcherDoesNotAnnouncePreFundingOrLockedDeposits(t *testing.T) {
	p := newPayEnv(t)
	p.fake.Deposit(p.addr, "tx-early", 1, 5000, 1)
	p.paidByWatcher(p.order, p.addr, "tx-fund")
	p.fake.SetConfirmations("tx-early", 3)
	p.fake.outputs = append(p.fake.outputs, Incoming{Address: p.addr, TxID: "tx-locked", Amount: 5000, Confirmations: 3, Locked: true})
	p.poll()
	p.poll()
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%received after this order was funded%'", p.order); n != 0 {
		t.Fatalf("pre-funding or locked deposit announced as post-funding: %d", n)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%unlock time%'", p.order); n != 1 {
		t.Fatalf("locked transfer on a paid order not reported: %d", n)
	}
}

// A-7 criterion 3: the funding deposit of a shipped order last changed 48 h ago disappears from the wallet; the
// next poll marks it conflicted, flags the order and holds the eventual release.
func TestWatcherDetectsConflictOnOldShippedOrder(t *testing.T) {
	p := newPayEnv(t)
	p.paidByWatcher(p.order, p.addr, "tx-fund")
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.DB.Exec("UPDATE orders SET updated=now()-interval '48 hours' WHERE id=$1", p.order)
	p.fake.Drop("tx-fund")
	p.poll()
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND txid='tx-fund' AND confirmations=-1 AND flagged", p.order); n != 1 {
		t.Fatal("funding deposit missing from the wallet was not marked conflicted")
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND to_state='shipped' AND note LIKE '%conflicted or missing%'", p.order); n != 1 {
		t.Fatalf("conflict events: %d", n)
	}
	if p.state(p.order) != stateShipped {
		t.Fatal("order state changed on conflict")
	}
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	p.poll()
	if st, _, _, _ := p.payout(p.order); st != "held" || len(p.fake.Sends()) != 0 {
		t.Fatalf("release after conflict: %s sends=%d", st, len(p.fake.Sends()))
	}
}
