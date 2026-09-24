package market

import (
	"net/url"
	"testing"
)

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

// setUnavailable replaces the working BTC provider with the same provider held as configured but unavailable in
// this process (as after a restart while the node is down), or disabled; unconfigured removes it entirely.
func (p *payEnv) setUnavailable(disabled, unconfigured bool) {
	p.A.payMu.Lock()
	defer p.A.payMu.Unlock()
	delete(p.A.payments, "BTC")
	if !unconfigured {
		p.A.unavailable["BTC"] = &unavailableProvider{p: p.fake, err: "bitcoin node check failed: connection refused", disabled: disabled}
	}
}

// A-54 (HUNT-E-3): the A-7 notice promises a post-funding deposit with the threshold at completion is included.
// When the configured provider is merely unavailable in this process, the release still counts it using the
// configured threshold against the confirmations last recorded by the market.
func TestAnnouncedDepositIncludedWhenCompletedWhileProviderUnavailable(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.paidByWatcher(p.order, p.addr, "tx-fund")
	p.fake.Deposit(p.addr, "tx-extra", 1, 7000, 3)
	p.poll()
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%tx-extra:1%If it has 3 confirmations, as last recorded by the market, when the order is completed%included%'", p.order); n != 1 {
		t.Fatalf("post-funding deposit not announced: %d", n)
	}
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.setUnavailable(false, false)
	if w := p.do("POST", "/orders/complete", p.buyerSess, url.Values{"order_id": {p.order}}); w.Code != 303 {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	if st, _, _, amt := p.payout(p.order); st != "pending" || amt != 107000 {
		t.Fatalf("release queued while the provider is unavailable: %s amount=%d, want pending 107000 (tx-fund 100000 + announced tx-extra 7000)", st, amt)
	}
	p.poll() // wallet back: provider promoted, payout sent
	if st, _, _, amt := p.payout(p.order); st != "sent" || amt != 107000 {
		t.Fatalf("release %s amount=%d, want sent 107000", st, amt)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%was not paid out%'", p.order); n != 0 {
		t.Fatalf("announced deposit flagged as not paid out: %d", n)
	}
}

// A-54: with the configured provider unavailable, a credited deposit last recorded below the configured threshold
// (re-confirming after a reorg) holds the payout, as it would with a working provider.
func TestPayoutHeldWhenCreditedDepositReconfirmingWhileProviderUnavailable(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.paidByWatcher(p.order, p.addr, "tx-fund")
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.DB.Exec("UPDATE payments SET confirmations=1 WHERE order_id=$1 AND txid='tx-fund'", p.order)
	p.setUnavailable(false, false)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	if st, _, _, amt := p.payout(p.order); st != "held" || amt != 100000 {
		t.Fatalf("release with a re-confirming credited deposit: %s amount=%d, want held 100000", st, amt)
	}
}

// A-54: a disabled or unconfigured provider has no threshold to trust, so only deposits already credited count;
// the announced deposit is left out (and flagged by settleOrder if its provider ever returns).
func TestDisabledOrUnconfiguredProviderCountsOnlyCreditedDeposits(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		disabled, unconfigured bool
	}{{"disabled", true, false}, {"unconfigured", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPayEnv(t)
			p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
			p.paidByWatcher(p.order, p.addr, "tx-fund")
			p.fake.Deposit(p.addr, "tx-extra", 1, 7000, 3)
			p.poll()
			p.move(p.order, statePaid, stateShipped, p.vendor)
			p.setUnavailable(tc.disabled, tc.unconfigured)
			p.move(p.order, stateShipped, stateCompleted, p.buyer)
			if st, _, _, amt := p.payout(p.order); st != "pending" || amt != 100000 {
				t.Fatalf("release %s amount=%d, want pending 100000 (credited tx-fund only)", st, amt)
			}
			if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND txid='tx-extra' AND credited", p.order); n != 0 {
				t.Fatal("uncredited deposit counted without a trusted threshold")
			}
		})
	}
}
