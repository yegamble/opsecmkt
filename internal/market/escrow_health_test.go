package market

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// A-451: the persisted health summary shows safe classes and allowlisted node fields, survives a disabled startup retry,
// and refreshes after the node recovers. The fake Core is simulated RPC, not chain evidence.
func TestEscrowHealthPersistsDisableRecoveryAndRendersNoNodeText(t *testing.T) {
	x := newEscrowEnv(t)
	x.poll()
	assertHealth := func(enabled bool, class, chain string, pinMatch *bool) {
		t.Helper()
		var gotEnabled bool
		var gotClass, gotChain string
		var gotPin sql.NullBool
		if err := x.DB.QueryRow(`SELECT enabled,disabled_class,chain,pin_match FROM bitcoin_escrow_health WHERE singleton`).
			Scan(&gotEnabled, &gotClass, &gotChain, &gotPin); err != nil {
			t.Fatal(err)
		}
		if gotEnabled != enabled || gotClass != class || gotChain != chain || (pinMatch == nil) != !gotPin.Valid || pinMatch != nil && gotPin.Bool != *pinMatch {
			t.Fatalf("health enabled=%v class=%q chain=%q pin=%v; want %v %q %q %v", gotEnabled, gotClass, gotChain, gotPin, enabled, class, chain, pinMatch)
		}
	}
	matched := true
	assertHealth(true, "not_checked", "regtest", &matched)

	// A node-supplied chain string is reduced to a class before it reaches durable state or the admin page.
	x.A.escrow.Store(nil)
	x.core.mu.Lock()
	x.core.chain = "chain-secret-rpc-reply"
	x.core.mu.Unlock()
	x.A.retryEscrow(context.Background(), &escrowRetry{last: escrowLogOn})
	assertHealth(false, "unsupported_chain", "unknown", nil)
	if n := agInt(x.testEnv, "SELECT count(*) FROM bitcoin_escrow_health WHERE disabled_class='chain-secret-rpc-reply'"); n != 0 {
		t.Fatal("node-supplied text was stored as a health class")
	}

	x.core.mu.Lock()
	x.core.chain = "regtest"
	x.core.mu.Unlock()
	x.A.retryEscrow(context.Background(), &escrowRetry{last: escrowLogRefused})
	x.poll()
	assertHealth(true, "unsupported_chain", "regtest", &matched)
	var lastPass bool
	var highWater sql.NullInt64
	var highest int64
	if err := x.DB.QueryRow(`SELECT last_pass IS NOT NULL,keypool_high_water,highest_market_index
		FROM bitcoin_escrow_health WHERE singleton`).Scan(&lastPass, &highWater, &highest); err != nil {
		t.Fatal(err)
	}
	if !lastPass || highWater.Valid && highWater.Int64 < 0 || highest < 0 {
		t.Fatalf("health pass/key indexes: lastPass=%v keypool=%v market=%d", lastPass, highWater, highest)
	}
	page := x.adminPage()
	for _, want := range []string{"id=\"escrow-health\"", "Bitcoin escrow node health:", "enabled", "last disabled class", "last successful watcher pass", "chain regtest", "chain pin matches", "highest allocated market index"} {
		if !strings.Contains(page, want) {
			t.Errorf("admin Payment providers lacks %q", want)
		}
	}
	if strings.Contains(page, "chain-secret-rpc-reply") || strings.Contains(page, "warroom-local-test") {
		t.Fatal("admin health summary exposed node reply text or database credentials")
	}
}

func TestEscrowHealthTracksArbiterKeypoolHighWater(t *testing.T) {
	x := newEscrowEnv(t)
	order, sid := x.built(regtestAddr("health-kp-refund"), regtestAddr("health-kp-claim"), 100000)
	x.signAs(order, roleVendor, sid)
	x.poll()
	var index, highWater, highest int64
	if err := x.DB.QueryRow(`SELECT es.market_index,h.keypool_high_water,h.highest_market_index
		FROM escrows es CROSS JOIN bitcoin_escrow_health h WHERE es.order_id=$1 AND h.singleton`, order).
		Scan(&index, &highWater, &highest); err != nil {
		t.Fatal(err)
	}
	if highWater < index || highest < index {
		t.Fatalf("keypool high-water %d and highest market index %d do not cover market index %d", highWater, highest, index)
	}
}
