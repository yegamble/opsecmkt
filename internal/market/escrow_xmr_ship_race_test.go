package market

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A-564 (QA66-5), P2-R5 for Monero: from the ship-by time on, the vendor's "Mark shipped" and the buyer's no-ship
// cancel of a paid physical Monero escrow order race through the HTTP handlers on the order row (transition's locked
// state check and compare-and-set; the cancel also takes partyOrderLocked). Exactly one wins: one state, one move out
// of paid, one settlement (the release requested at shipment, or the refund), one stock effect (kept for the
// shipment, returned once by the cancel), and the watcher sends at most the refund. The loser is refused 409 and told
// which happened, with no Bitcoin wording. Each race holds the order row from the test until both requests wait for
// it. DB integration against the simulated Monero wallet-rpc: simulated RPC, not chain evidence.
func TestXMREscrowShipNoShipCancelRaceOneOutcome(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	shippedAt := func(order string) string {
		return physString(t, x.testEnv, `SELECT to_char(max(created) AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"') FROM order_events WHERE order_id=$1 AND to_state='shipped' AND from_state='paid'`, order)
	}
	cancelledAt := func(order string) string {
		return physString(t, x.testEnv, `SELECT to_char(max(created) AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"') FROM order_events WHERE order_id=$1 AND to_state='cancelled'`, order)
	}
	count := func(order, kind string) int {
		return agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind=$2", order, kind)
	}
	stock := func() int { return agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", physical) }
	wins := map[string]int{}
	for i := range 4 {
		order, buyerSess := x.paidXMRPhysical(physical)
		x.forceDeadline(order, "ship_by=now()-interval '1 minute'")
		held := stock() // the paid order's unit is already reserved
		escrowAddr := agStr(x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)
		tx, err := x.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec("SELECT 1 FROM orders WHERE id=$1 FOR UPDATE", order); err != nil {
			t.Fatal(err)
		}
		var shipCode, cancelCode int
		var shipBody, cancelBody string
		var wg sync.WaitGroup
		ship := func() {
			defer wg.Done()
			shipCode, shipBody = x.xmrShip(order, x.vendorSess, testXMRAddr("race claim "+strconv.Itoa(i)+order, true), testPassword)
		}
		cancel := func() { defer wg.Done(); cancelCode, cancelBody = x.xmrBuyerCancel(order, buyerSess) }
		first, second := ship, cancel // alternate which request queues for the row first, so each wins in turn
		if i%2 == 1 {
			first, second = cancel, ship
		}
		wg.Add(2)
		go first()
		x.waitForLockWaits(1)
		go second()
		x.waitForLockWaits(2)
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		var state string
		var releases, refunds, units int
		switch {
		case shipCode == 303 && cancelCode == 409:
			wins["ship"]++
			state, releases, units = stateShipped, 1, held
			if !containsAll(cancelBody, "marked this order shipped at "+shippedAt(order), "Confirm receipt") {
				t.Fatalf("ship won; the cancel was told %s", cancelBody)
			}
			noBitcoinWords(t, "the refused cancel", cancelBody)
		case cancelCode == 303 && shipCode == 409:
			wins["cancel"]++
			state, refunds, units = stateCancelled, 1, held+1
			if !strings.Contains(shipBody, "buyer cancelled this order at "+cancelledAt(order)) {
				t.Fatalf("cancel won; the ship was told %s", shipBody)
			}
			noBitcoinWords(t, "the refused ship", shipBody)
		default:
			t.Fatalf("ship %d %s; cancel %d %s; order %s", shipCode, shipBody, cancelCode, cancelBody, x.state(order))
		}
		if got := x.state(order); got != state {
			t.Fatalf("order %s, want %s", got, state)
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state='paid' AND to_state<>'paid'", order); n != 1 {
			t.Fatalf("%d moves out of paid, want 1", n)
		}
		if count(order, "release") != releases || count(order, "refund") != refunds {
			t.Fatalf("settlements %v, want %d release and %d refund", x.xmrSettlements(order), releases, refunds)
		}
		if got := stock(); got != units {
			t.Fatalf("stock %d, want %d (held %d)", got, units, held)
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND release_at IS NOT NULL", order); n != releases {
			t.Fatalf("release_at set on %d escrows, want %d", n, releases)
		}
		x.mustPoll()
		if got := spentFrom(x.fake, escrowAddr); got != refunds {
			t.Fatalf("the watcher relayed %d transactions from the escrow, want %d: %v", got, refunds, x.xmrSettlements(order))
		}
		if got := x.state(order); got != state || stock() != units {
			t.Fatalf("after the watcher's pass: order %s stock %d, want %s and %d", got, stock(), state, units)
		}
	}
	if wins["ship"] == 0 || wins["cancel"] == 0 {
		t.Fatalf("each side should win the races it queued first in: %v", wins)
	}
}
