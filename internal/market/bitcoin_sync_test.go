package market

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// A-439 (P4-R3): the stateless not-synced rule shared by the escrow watcher and the pooled Bitcoin check. Unit tests,
// no node and no database.
func TestBitcoinNodeNotSynced(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fresh, stale := now.Add(-10*time.Minute).Unix(), now.Add(-3*time.Hour).Unix()
	for _, tc := range []struct {
		name string
		n    bitcoinNode
		want string
	}{
		{"public synced", bitcoinNode{Chain: "signet", Headers: 9, Blocks: 9, Time: fresh, Connections: 8}, ""},
		{"ibd", bitcoinNode{Chain: "testnet4", IBD: true, Headers: 9, Blocks: 9, Time: fresh, Connections: 8}, notSyncedIBD},
		{"headers ahead", bitcoinNode{Chain: "testnet4", Headers: 10, Blocks: 9, Time: fresh, Connections: 8}, notSyncedHeaders},
		{"no peers", bitcoinNode{Chain: "signet", Headers: 9, Blocks: 9, Time: fresh}, notSyncedPeers},
		{"peer count unread", bitcoinNode{Chain: "testnet4", Headers: 9, Blocks: 9, Time: fresh, Connections: -1}, notSyncedPeersUnread},
		{"regtest peer count unread", bitcoinNode{Chain: "regtest", Connections: -1}, ""},
		{"stale tip", bitcoinNode{Chain: "test", Headers: 9, Blocks: 9, Time: stale, Connections: 3}, notSyncedTip},
		{"no tip time", bitcoinNode{Chain: "main", Headers: 9, Blocks: 9, Connections: 3}, notSyncedTip},
		{"regtest ibd", bitcoinNode{Chain: "regtest", IBD: true}, notSyncedIBD},
		{"regtest headers ahead", bitcoinNode{Chain: "regtest", Headers: 3, Blocks: 2}, notSyncedHeaders},
		// Regtest is exempt from the peer and tip-age checks: a lone regtest node has no peers and an old tip.
		{"regtest lone and old", bitcoinNode{Chain: "regtest", Headers: 9, Blocks: 9, Time: stale}, ""},
	} {
		if got := tc.n.notSynced(now, 2*time.Hour); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	// The bound is the configured age, not a constant.
	n := bitcoinNode{Chain: "signet", Headers: 9, Blocks: 9, Time: stale, Connections: 1}
	if got := n.notSynced(now, 4*time.Hour); got != "" {
		t.Fatalf("a 3-hour-old tip under a 4h bound: %q", got)
	}
}

// BITCOIN_MAX_TIP_AGE: a Go duration from 30m to 24h, 2h when blank, parsed like PAYMENT_EXPIRY.
func TestBitcoinMaxTipAgeSetting(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 2 * time.Hour, " 30m ": 30 * time.Minute, "6h": 6 * time.Hour, "24h": 24 * time.Hour} {
		if got, err := parseMaxTipAge(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"29m", "25h", "2", "two hours", "-1h"} {
		if _, err := parseMaxTipAge(bad); err == nil || !strings.Contains(err.Error(), "BITCOIN_MAX_TIP_AGE must be a duration from 30m to 24h") {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	t.Setenv("BITCOIN_MAX_TIP_AGE", "45m")
	if maxTipAge() != 45*time.Minute {
		t.Fatal("maxTipAge ignores the setting")
	}
	t.Setenv("BITCOIN_MAX_TIP_AGE", "bogus") // refused at startup; the watcher falls back to the default
	if maxTipAge() != 2*time.Hour {
		t.Fatal("maxTipAge fallback")
	}
	if _, err := escrowConfigFromEnv(func(k string) string {
		return map[string]string{"ESCROW_BTC": "on", "ESCROW_BTC_RPC_URL": "http://u:p@127.0.0.1:1", "BITCOIN_MAX_TIP_AGE": "10m"}[k]
	}); err == nil || !strings.Contains(err.Error(), "BITCOIN_MAX_TIP_AGE") {
		t.Fatalf("escrow startup accepted a 10m tip age: %v", err)
	}
	t.Setenv("BITCOIN_RPC_URL", "http://u:p@127.0.0.1:1")
	t.Setenv("BITCOIN_MAX_TIP_AGE", "10m")
	if _, err := bitcoinFromEnv(t.Context()); err == nil || !strings.Contains(err.Error(), "BITCOIN_MAX_TIP_AGE") {
		t.Fatalf("pooled startup accepted a 10m tip age: %v", err)
	}
}

// A-439 (P4-R5): one chain table. regtest pays bcrt, signet and testnet4 pay tb; main and testnet3 ("test") are not
// escrow chains. Base58 test-network P2PKH (0x6f) and P2SH (0xc4) are the same on all three.
func TestEscrowAddressTable(t *testing.T) {
	prog20, prog32 := make([]byte, 20), make([]byte, 32)
	prog20[0], prog32[0] = 7, 9
	p2wpkh, p2wsh := []byte{0, 20}, []byte{0, 32}
	cases := []struct {
		chain, addr string
		want        []byte // nil = refused
	}{
		{"regtest", segwitAddress("bcrt", 0, prog20), append(p2wpkh, prog20...)},
		{"regtest", segwitAddress("bcrt", 0, prog32), append(p2wsh, prog32...)},
		{"regtest", segwitAddress("bcrt", 1, prog32), append([]byte{0x51, 32}, prog32...)},
		{"signet", segwitAddress("tb", 0, prog20), append(p2wpkh, prog20...)},
		{"signet", segwitAddress("tb", 0, prog32), append(p2wsh, prog32...)},
		{"testnet4", segwitAddress("tb", 0, prog20), append(p2wpkh, prog20...)},
		{"testnet4", segwitAddress("tb", 0, prog32), append(p2wsh, prog32...)},
		{"signet", base58CheckEncode(append([]byte{0x6f}, prog20...)), slices.Concat([]byte{0x76, 0xa9, 20}, prog20, []byte{0x88, 0xac})},
		{"testnet4", base58CheckEncode(append([]byte{0xc4}, prog20...)), slices.Concat([]byte{0xa9, 20}, prog20, []byte{0x87})},
		// Wrong human-readable part for the chain.
		{"regtest", segwitAddress("tb", 0, prog20), nil},
		{"signet", segwitAddress("bcrt", 0, prog20), nil},
		{"testnet4", segwitAddress("bcrt", 0, prog32), nil},
		// Mainnet addresses, on every chain.
		{"regtest", segwitAddress("bc", 0, prog20), nil},
		{"signet", segwitAddress("bc", 0, prog20), nil},
		{"testnet4", segwitAddress("bc", 0, prog32), nil},
		{"signet", base58CheckEncode(append([]byte{0x00}, prog20...)), nil},
		{"testnet4", base58CheckEncode(append([]byte{0x05}, prog20...)), nil},
		// Not escrow chains, even for an address of their own form.
		{"main", segwitAddress("bc", 0, prog20), nil},
		{"test", segwitAddress("tb", 0, prog20), nil},
		{"", segwitAddress("bcrt", 0, prog20), nil},
	}
	for _, c := range cases {
		got, err := escrowAddressScript(c.chain, c.addr)
		switch {
		case c.want == nil && err == nil:
			t.Errorf("%s %s accepted as %x", c.chain, c.addr, got)
		case c.want != nil && (err != nil || !slices.Equal(got, c.want)):
			t.Errorf("%s %s: %x %v, want %x", c.chain, c.addr, got, err, c.want)
		}
	}
	if escrowChains["signet"] != "tb" || escrowChains["testnet4"] != "tb" || escrowChains["regtest"] != "bcrt" || len(escrowChains) != 3 {
		t.Fatalf("chain table %v", escrowChains)
	}
}
