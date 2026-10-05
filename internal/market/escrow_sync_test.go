package market

import (
	"context"
	"html"
	"strings"
	"testing"
	"time"
)

// A-439 (P4-R3, P4-R4): the escrow watcher's sync guard and the escrow chain pin, against the fake escrow Core
// (simulated RPC, not chain evidence; scripts/test-escrow-regtest.sh has the real regtest legs).

// onSignet makes the fake node a synced signet node (signet_challenge 51, 8 peers, a fresh tip) and the enabled escrow
// one started on it, as A-443 will allow: startEscrow still refuses every chain but regtest in this slice.
func (x *escrowEnv) onSignet() {
	x.t.Helper()
	x.core.mu.Lock()
	x.core.chain, x.core.challenge, x.core.peers, x.core.tipTime = "signet", "51", 8, time.Now().Unix()
	x.core.mu.Unlock()
	e := x.A.escrow.Load()
	e.network, e.chainID = "signet", escrowChainID("signet", "51")
}

func (x *escrowEnv) adminPage() string {
	x.t.Helper()
	_, as := x.user("syncadmin_"+randomToken()[:6], "admin")
	return html.UnescapeString(x.page("/admin", as))
}

// While the escrow node is not synced the escrow Bitcoin pass is skipped: no deposit is read, no order expires, no
// deposit is flagged spent outside the market and no settlement is built; one log line names the reason class and the
// admin page says so. Nothing is persisted for the guard. The next synced pass does the work, even when the node's
// height is now below what the unsynced node reported (no high-water mark, A-147).
func TestEscrowWatcherSkipsWhileNotSynced(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signet bool
		set    func(c *fakeEscrowCore)
		why    string
	}{
		{"ibd", false, func(c *fakeEscrowCore) { c.ibd = true }, notSyncedIBD},
		{"headers ahead", false, func(c *fakeEscrowCore) { c.headers = 305 }, notSyncedHeaders},
		{"no peers on signet", true, func(c *fakeEscrowCore) { c.peers = 0 }, notSyncedPeers},
		{"stale tip on signet", true, func(c *fakeEscrowCore) { c.tipTime = time.Now().Add(-3 * time.Hour).Unix() }, notSyncedTip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newEscrowEnv(t)
			// A claim requested (built by the watcher), a completed order whose deposit the parties co-spend (flagged
			// spent outside by the watcher) and an unpaid order past its payment window (cancelled by the watcher).
			claimed := x.completed(regtestAddr("sync-claim-refund"), 100000)
			cospent := x.completed(regtestAddr("sync-cospend-refund"), 100000)
			x.mustCheckout(x.order, regtestAddr("sync-expired-refund"))
			x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", x.order)
			spender := x.coSpend(txidOf(cospent), 0, 1)
			if code, body := x.claim(claimed, x.vendorSess, regtestAddr("sync-claim-to")); code != 303 {
				t.Fatalf("claim: %d %s", code, body)
			}
			if tc.signet {
				x.onSignet()
			}
			settings := agStrings(t, x.testEnv, "SELECT key||'='||value FROM settings ORDER BY key")
			creates, reads := x.core.count("createpsbt"), x.core.count("listreceivedbyaddress")
			logs := captureLog(t)
			x.core.mu.Lock()
			tc.set(x.core)
			x.core.blocks = 300 // the unsynced node reports a higher tip than the synced one below
			x.core.mu.Unlock()

			x.poll()
			x.poll()
			if s := x.state(x.order); s != stateAwaitingPayment {
				t.Fatalf("an order expired while the node was not synced: %s", s)
			}
			if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE spent_outside"); n != 0 {
				t.Fatalf("%d deposits flagged spent outside while the node was not synced", n)
			}
			if x.core.count("createpsbt") != creates || x.core.count("listreceivedbyaddress") != reads {
				t.Fatal("a settlement was built or deposits read while the node was not synced")
			}
			if _, _, state, _ := x.settlement(claimed); state != "building" {
				t.Fatalf("claim %s while the node was not synced", state)
			}
			line := "escrow: Bitcoin escrow node not synced (" + tc.why + "); escrow watcher passes skipped until it is"
			if got := logs.String(); strings.Count(got, line) != 1 || strings.Contains(got, "escrow watcher:") {
				t.Fatalf("log while not synced:\n%s", got)
			}
			if page := x.adminPage(); !strings.Contains(page, "Bitcoin escrow node not synced: "+tc.why+".") {
				t.Fatalf("admin page lacks the not-synced line:\n%s", page)
			}
			if got := agStrings(t, x.testEnv, "SELECT key||'='||value FROM settings WHERE key NOT LIKE 'installed' ORDER BY key"); strings.Join(got, ",") != strings.Join(withoutInstalled(settings), ",") {
				t.Fatalf("settings changed by the guard: %v -> %v", settings, got)
			}

			x.core.mu.Lock()
			x.core.ibd, x.core.headers, x.core.blocks = false, 0, 0
			if tc.signet {
				x.core.peers, x.core.tipTime = 8, time.Now().Unix()
			}
			x.core.mu.Unlock()
			x.poll()
			if s := x.state(x.order); s != stateCancelled {
				t.Fatalf("the expired order after a synced pass: %s", s)
			}
			if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND spent_outside AND spent_by=$2", cospent, spender) != 1 {
				t.Fatal("the co-spent deposit was not flagged on the synced pass")
			}
			if x.core.count("createpsbt") == creates {
				t.Fatal("the claim was not built on the synced pass")
			}
			if _, _, state, class := x.settlement(claimed); state != "awaiting_signature" {
				t.Fatalf("claim after the synced pass: %s %s", state, class)
			}
			if got := logs.String(); strings.Count(got, "escrow: Bitcoin escrow node synced; escrow watcher passes resumed") != 1 || strings.Count(got, line) != 1 {
				t.Fatalf("log after the synced pass:\n%s", got)
			}
			if page := x.adminPage(); strings.Contains(page, "Bitcoin escrow node not synced") {
				t.Fatal("the admin page still says the node is not synced")
			}
		})
	}
}

func withoutInstalled(s []string) []string {
	var out []string
	for _, v := range s {
		if !strings.HasPrefix(v, "installed=") {
			out = append(out, v)
		}
	}
	return out
}

// Regtest keeps today's behaviour: a lone regtest node (no peers, a tip two days old) is synced, and getnetworkinfo is
// never called, so an rpcwhitelist written before A-439 still works.
func TestEscrowRegtestExemptFromPeerAndTipChecks(t *testing.T) {
	x := newEscrowEnv(t)
	x.core.mu.Lock()
	x.core.peers, x.core.tipTime = 0, time.Now().Add(-48*time.Hour).Unix()
	x.core.mu.Unlock()
	x.mustCheckout(x.order, regtestAddr("exempt-refund"))
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", x.order)
	logs := captureLog(t)
	x.poll()
	if s := x.state(x.order); s != stateCancelled {
		t.Fatalf("a lone regtest node held the pass: %s", s)
	}
	if n := x.core.count("getnetworkinfo"); n != 0 {
		t.Fatalf("getnetworkinfo called %d times on regtest", n)
	}
	if strings.Contains(logs.String(), "not synced") {
		t.Fatalf("log: %s", logs.String())
	}
}

// P4-R4: the escrow node's chain is pinned in settings (escrow_btc_chain). A database holding an escrow refuses a
// node on another chain: escrow stays disabled with one fixed log line, the admin page names both values, and nothing
// is written. Retries keep it disabled without another log line.
func TestEscrowChainPinMismatchDisables(t *testing.T) {
	x := newEscrowEnv(t)
	if got := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", escrowChainPin); len(got) != 1 || got[0] != "regtest" {
		t.Fatalf("pin after the first start: %v", got)
	}
	x.mustCheckout(x.order, regtestAddr("pin-refund"))
	x.DB.Exec("UPDATE settings SET value='testnet4' WHERE key=$1", escrowChainPin)
	before := agStrings(t, x.testEnv, "SELECT key||'='||value FROM settings ORDER BY key")
	escrows := agStrings(t, x.testEnv, "SELECT order_id||' '||address FROM escrows ORDER BY order_id")
	logs := captureLog(t)
	x.restart()
	if x.A.escrow.Load() != nil || !x.A.escrowConfigured {
		t.Fatal("escrow enabled against a database pinned to another chain")
	}
	if strings.Count(logs.String(), escrowLogChain) != 1 || strings.Contains(logs.String(), escrowLogOn) {
		t.Fatalf("log:\n%s", logs.String())
	}
	page := x.adminPage()
	if !strings.Contains(page, "Bitcoin escrow disabled: the escrow node reports chain regtest, but this database's escrows are on testnet4.") {
		t.Fatalf("admin page:\n%s", page)
	}
	retry := &escrowRetry{last: x.A.escrowStartState}
	x.A.retryEscrow(context.Background(), retry)
	x.A.retryEscrow(context.Background(), retry)
	if x.A.escrow.Load() != nil || strings.Count(logs.String(), escrowLogChain) != 1 {
		t.Fatalf("retries: %v\n%s", x.A.escrow.Load() != nil, logs.String())
	}
	after := agStrings(t, x.testEnv, "SELECT key||'='||value FROM settings WHERE key<>'installed' ORDER BY key")
	if strings.Join(withoutInstalled(before), ",") != strings.Join(after, ",") ||
		strings.Join(escrows, ",") != strings.Join(agStrings(t, x.testEnv, "SELECT order_id||' '||address FROM escrows ORDER BY order_id"), ",") {
		t.Fatalf("a refused start changed the database: %v -> %v", before, after)
	}
	if s := x.state(x.order); s != stateAwaitingPayment {
		t.Fatalf("order %s", s)
	}
	// Back on the pinned chain's terms (the operator restores the pin, or points at the right node), escrow starts.
	x.DB.Exec("UPDATE settings SET value='regtest' WHERE key=$1", escrowChainPin)
	x.A.retryEscrow(context.Background(), retry)
	if x.A.escrow.Load() == nil {
		t.Fatal("escrow did not start once the chain matched")
	}
	if page := x.adminPage(); strings.Contains(page, "Bitcoin escrow disabled: the escrow node reports chain") {
		t.Fatal("the mismatch line outlived the mismatch")
	}
}

// The pin's rules, for the chains startEscrow does not yet accept (A-443 lifts the regtest-only refusal): a signet pin
// includes signet_challenge, so a signet with another challenge is a mismatch; a database holding escrows and no pin is
// pinned only when the node reports regtest; with no escrow the pin is written and may be rewritten.
func TestEscrowChainPinRules(t *testing.T) {
	x := newEscrowEnv(t)
	ctx := context.Background()
	pin := func() []string {
		return agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", escrowChainPin)
	}
	// No escrow: pins and re-pins.
	for _, id := range []string{escrowChainID("signet", "51"), escrowChainID("testnet4", ""), escrowChainID("regtest", "")} {
		if pinned, ok, err := pinEscrowChain(ctx, x.DB, id); err != nil || !ok || pinned != id || strings.Join(pin(), ",") != id {
			t.Fatalf("no escrow, %s: %q %v %v (stored %v)", id, pinned, ok, err, pin())
		}
	}
	if escrowChainID("signet", "51") != "signet:51" || escrowChainID("regtest", "51") != "regtest" {
		t.Fatalf("chain IDs %q %q", escrowChainID("signet", "51"), escrowChainID("regtest", "51"))
	}
	x.mustCheckout(x.order, regtestAddr("rules-refund"))
	// Escrows exist, pinned to a signet: the same challenge matches, another one does not, and nothing is written.
	x.DB.Exec("UPDATE settings SET value='signet:51' WHERE key=$1", escrowChainPin)
	if pinned, ok, err := pinEscrowChain(ctx, x.DB, "signet:51"); err != nil || !ok || pinned != "signet:51" {
		t.Fatalf("same signet: %q %v %v", pinned, ok, err)
	}
	for _, id := range []string{escrowChainID("signet", "5121deadbeef51ae"), "regtest", "testnet4", "signet"} {
		if pinned, ok, err := pinEscrowChain(ctx, x.DB, id); err != nil || ok || pinned != "signet:51" || strings.Join(pin(), ",") != "signet:51" {
			t.Fatalf("escrows on signet:51, node %s: %q %v %v (stored %v)", id, pinned, ok, err, pin())
		}
	}
	// Escrows exist and no pin (a database from before A-439): a signet node is refused and nothing is written; a
	// regtest node is pinned.
	x.DB.Exec("DELETE FROM settings WHERE key=$1", escrowChainPin)
	if pinned, ok, err := pinEscrowChain(ctx, x.DB, "signet:51"); err != nil || ok || pinned != "" || len(pin()) != 0 {
		t.Fatalf("unpinned escrows, signet node: %q %v %v (stored %v)", pinned, ok, err, pin())
	}
	if pinned, ok, err := pinEscrowChain(ctx, x.DB, "regtest"); err != nil || !ok || pinned != "regtest" || strings.Join(pin(), ",") != "regtest" {
		t.Fatalf("unpinned escrows, regtest node: %q %v %v (stored %v)", pinned, ok, err, pin())
	}
	// The startup backfill end to end: a restart with no pin and escrows on regtest pins it and enables escrow.
	x.DB.Exec("DELETE FROM settings WHERE key=$1", escrowChainPin)
	x.restart()
	if x.A.escrow.Load() == nil || strings.Join(pin(), ",") != "regtest" {
		t.Fatalf("backfill at startup: %v %v", x.A.escrow.Load() != nil, pin())
	}
}
