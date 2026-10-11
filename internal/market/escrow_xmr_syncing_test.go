package market

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A-449: a daemon still syncing reports the hard fork of the height it has reached (a stagenet node syncing from its
// genesis block reports version 1 for a long time, and the installer turns Monero escrow on beside a fresh one), so its
// hard fork is not refused: the check answers a fixed "daemon-syncing" class, which leaves Monero escrow disabled and
// retried, as an unreachable daemon does. A synced daemon below hard fork 16 is still refused. Unit test (no database).
func TestXMREscrowSyncingDaemonNotRefused(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		info    map[string]any
		refused bool
	}{
		{"stagenet syncing (target above height)", map[string]any{"nettype": "stagenet", "stagenet": true, "height": 5000, "target_height": 1_900_000, "synchronized": true}, false},
		{"stagenet not synchronized", map[string]any{"nettype": "stagenet", "stagenet": true, "height": 5000, "synchronized": false}, false},
		{"stagenet synced at hard fork 1 (an offline fork)", map[string]any{"nettype": "stagenet", "stagenet": true, "height": 5000, "synchronized": true}, true},
		{"fakechain synced at hard fork 1", map[string]any{"nettype": "fakechain", "height": 120, "synchronized": true}, true},
	} {
		_, daemon, r := happyXMRNodes(t)
		daemon.result("get_info", c.info)
		daemon.result("hard_fork_info", map[string]any{"version": 1})
		_, err := r.checkDaemon(ctx)
		var refusal *xmrRefusal
		var re *xmrRPCError
		switch {
		case c.refused && !errors.Is(err, errXMRHardFork):
			t.Errorf("%s: %v, want the hard fork refusal", c.name, err)
		case !c.refused && (errors.As(err, &refusal) || !errors.As(err, &re) || re.Class != "daemon-syncing"):
			t.Errorf("%s: %v, want the daemon-syncing class", c.name, err)
		}
	}
}

// End to end at startup: a market with ESCROW_XMR=on beside a stagenet daemon still syncing (hard fork 1) starts, with
// Monero escrow disabled and its state naming the class; once the daemon is synced at hard fork 16 the watcher's retry
// turns it on.
func TestXMREscrowStartsBesideSyncingDaemon(t *testing.T) {
	logs := captureLog(t)
	x := newXMREscrowEnvWith(t, func(f *fakeXMREscrow) { f.nettype, f.peers, f.targetAhead, f.hardFork = "stagenet", 8, 1_000_000, 1 })
	m := x.A.escrowXMR
	if m == nil || m.ready.Load() || m.refused.Load() || m.startState != xmrLogUnchecked {
		t.Fatalf("beside a syncing daemon: configured %v, state %q", m != nil, m.startState)
	}
	if n := len(logs.lines(xmrLogUnchecked + " (hard_fork_info daemon-syncing)")); n != 1 || strings.Contains(logs.String(), "refusing") {
		t.Fatalf("log:\n%s", logs.String())
	}
	// /admin names the unchecked state with its class, the readiness line is not a plain "on", and no network is named
	// before one was verified (QA4W4B-1).
	alert, readiness := xmrAdminLines(t, x.adminPage())
	unchecked := "the escrow daemon or wallet-rpc could not be checked (hard_fork_info daemon-syncing)"
	if !strings.Contains(alert, "Monero escrow not ready: "+unchecked) || !strings.Contains(alert, "ESCROW_XMR_DAEMON_URL") ||
		!strings.Contains(readiness, "XMR escrow: on (not ready: "+unchecked+")") {
		t.Fatalf("/admin beside a syncing daemon: alert %q, readiness %q", alert, readiness)
	}
	if strings.Contains(alert+readiness, "fakechain") || strings.Contains(alert+readiness, "stagenet") {
		t.Fatalf("admin status names an unverified network: alert %q, readiness %q", alert, readiness)
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.targetAhead, f.hardFork = 0, 16 })
	x.A.retryEscrowXMR(context.Background(), &escrowRetry{last: m.startState})
	if !m.ready.Load() {
		t.Fatal("Monero escrow did not start once the daemon was synced at hard fork 16")
	}
	alert, readiness = xmrAdminLines(t, x.adminPage())
	if alert != "" || !strings.HasPrefix(readiness, "<li>XMR escrow: on") || !strings.Contains(readiness, "intake: paused") {
		t.Fatalf("/admin once Monero escrow is ready: alert %q, readiness %q", alert, readiness)
	}
}
