package market

import (
	"context"
	"database/sql"
	"fmt"
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

// A-451: the node height is recorded by each of the three writers (startup/retry attempt, watcher observation, completed
// pass) and rendered on /admin. A-507 pin: a writer that stored a constant height passed every earlier test.
func TestEscrowHealthStoresAndRendersNodeHeightFromEachWriter(t *testing.T) {
	x := newEscrowEnv(t)
	x.poll()
	setNode := func(chain string, blocks int64) {
		x.core.mu.Lock()
		x.core.chain, x.core.blocks = chain, blocks
		x.core.mu.Unlock()
	}
	assertHeight := func(writer string, want int64) {
		t.Helper()
		var got sql.NullInt64
		if err := x.DB.QueryRow(`SELECT height FROM bitcoin_escrow_health WHERE singleton`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if !got.Valid || got.Int64 != want {
			t.Fatalf("%s: stored height %v, want %d", writer, got, want)
		}
		if page := x.adminPage(); !strings.Contains(page, fmt.Sprintf("height %d; chain pin", want)) {
			t.Fatalf("%s: admin Payment providers does not render height %d", writer, want)
		}
	}

	// Completed watcher pass (saveEscrowHealthPass).
	setNode("regtest", 4321)
	x.poll()
	assertHeight("completed pass", 4321)

	// A retry that re-enables escrow (saveEscrowHealthAttempt from the observation startEscrow made).
	x.A.escrow.Store(nil)
	setNode("regtest", 4400)
	x.A.retryEscrow(context.Background(), &escrowRetry{last: escrowLogRefused})
	if x.A.escrow.Load() == nil {
		t.Fatal("retry did not re-enable escrow")
	}
	assertHeight("retry attempt", 4400)

	// A pass that finds the node on another chain stops before it completes: only the observation write
	// (saveEscrowHealthObservation) can have stored this height.
	setNode("signet", 5555)
	_ = x.A.escrowPollOnce(context.Background())
	assertHeight("pass on a changed chain", 5555)
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

func TestEscrowHealthPersistsConfiguredOffAcrossRestartWithoutRPC(t *testing.T) {
	x := newEscrowEnv(t)
	x.poll()
	var lastPass sql.NullTime
	var chain string
	var height sql.NullInt64
	var pin sql.NullBool
	var keypool sql.NullInt64
	if err := x.DB.QueryRow(`SELECT last_pass,chain,height,pin_match,keypool_high_water FROM bitcoin_escrow_health WHERE singleton`).
		Scan(&lastPass, &chain, &height, &pin, &keypool); err != nil {
		t.Fatal(err)
	}
	if !lastPass.Valid {
		t.Fatal("fixture did not record a successful watcher pass")
	}

	beforeRPC := len(x.core.methods())
	t.Setenv("ESCROW_BTC", "")
	x.restart()
	if x.A.escrowConfigured || x.A.escrow.Load() != nil || len(x.core.methods()) != beforeRPC {
		t.Fatalf("off startup called the node or enabled escrow: calls %v -> %v", beforeRPC, len(x.core.methods()))
	}
	var enabled bool
	var disabledClass string
	var afterPass sql.NullTime
	var afterChain string
	var afterHeight sql.NullInt64
	var afterPin sql.NullBool
	var afterKeypool sql.NullInt64
	if err := x.DB.QueryRow(`SELECT enabled,disabled_class,last_pass,chain,height,pin_match,keypool_high_water
		FROM bitcoin_escrow_health WHERE singleton`).Scan(&enabled, &disabledClass, &afterPass, &afterChain, &afterHeight, &afterPin, &afterKeypool); err != nil {
		t.Fatal(err)
	}
	if enabled || disabledClass != "off" || !afterPass.Valid || !afterPass.Time.Equal(lastPass.Time) ||
		afterChain != chain || afterHeight != height || afterPin != pin || afterKeypool != keypool {
		t.Fatalf("off startup lost retained health: enabled=%v class=%q pass=%v chain=%q height=%v pin=%v keypool=%v",
			enabled, disabledClass, afterPass, afterChain, afterHeight, afterPin, afterKeypool)
	}
	page := x.adminPage()
	for _, want := range []string{"id=\"escrow-health\"", "configuration off", "disabled", "<code>off</code>", "stale while <code>ESCROW_BTC=off</code>", "status updated"} {
		if !strings.Contains(page, want) {
			t.Errorf("admin Payment providers lacks %q when escrow is configured off", want)
		}
	}
}
