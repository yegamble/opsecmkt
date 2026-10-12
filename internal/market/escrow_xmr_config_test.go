package market

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func xmrEnv(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

// xmrOnEnv is a complete ESCROW_XMR=on configuration with extra overriding it.
func xmrOnEnv(seedKeyPath string, extra ...string) []string {
	base := []string{
		"ESCROW_XMR", "on",
		"ESCROW_XMR_WALLET_RPC_URL", "http://escrowuser:s3cret-escrow-pass@127.0.0.1:28088",
		"ESCROW_XMR_DAEMON_URL", "http://daemonuser:s3cret-daemon-pass@127.0.0.1:28081",
		"MONERO_WALLET_SECRET", testXMRSecret,
		"ESCROW_XMR_SEED_KEY", seedKeyPath,
		"SETUP_TOKEN", testSetupToken,
	}
	return append(base, extra...)
}

// TestXMREscrowConfigFromEnv (P3-R13): every setting, its default and its bounds; no refusal quotes a value.
func TestXMREscrowConfigFromEnv(t *testing.T) {
	// Off reads ESCROW_XMR and nothing else, however the other settings are set.
	junk := []string{"ESCROW_XMR_WALLET_RPC_URL", "junk", "ESCROW_XMR_DAEMON_URL", "junk", "ESCROW_XMR_FEE_BPS", "-5", "ESCROW_XMR_SLOTS", "0",
		"ESCROW_XMR_MAX_FEE", "x", "MONERO_WALLET_SECRET", "short", "ESCROW_XMR_SEED_KEY", "/nonexistent", "MONERO_NETWORK", "mainnet", "BITCOIN_CHAIN", "main"}
	for _, v := range []string{"", "off", " ", " off "} {
		var read []string
		env := xmrEnv(append([]string{"ESCROW_XMR", v}, junk...)...)
		c, err := escrowXMRConfigFromEnv(func(k string) string { read = append(read, k); return env(k) })
		if err != nil || c.on || !slices.Equal(read, []string{"ESCROW_XMR"}) {
			t.Fatalf("ESCROW_XMR=%q: %+v %v, read %v", v, c, err, read)
		}
	}
	c, err := escrowXMRConfigFromEnv(xmrEnv(xmrOnEnv("/keys/seed.asc")...))
	if err != nil || !c.on || c.walletURL != "http://escrowuser:s3cret-escrow-pass@127.0.0.1:28088" || c.daemonURL != "http://daemonuser:s3cret-daemon-pass@127.0.0.1:28081" ||
		c.feeBps != 0 || c.feeAddress != "" || c.slots != 20 || c.maxFee != 10_000_000_000 || c.secret != testXMRSecret || c.seedKeyPath != "/keys/seed.asc" {
		t.Fatalf("on: %+v %v", c, err)
	}
	feeAddr := testXMRAddr("market fee", false)
	c, err = escrowXMRConfigFromEnv(xmrEnv(xmrOnEnv("/k", "ESCROW_XMR_FEE_BPS", "250", "ESCROW_XMR_FEE_ADDRESS", " "+feeAddr+" ",
		"ESCROW_XMR_SLOTS", "200", "ESCROW_XMR_MAX_FEE", "100000000000", "MONERO_WALLET_RPC_URL", "http://u:p@127.0.0.1:38088", "BITCOIN_CHAIN", "regtest", "MONERO_NETWORK", "stagenet")...))
	if err != nil || c.feeBps != 250 || c.feeAddress != feeAddr || c.slots != 200 || c.maxFee != 100_000_000_000 {
		t.Fatalf("fee, slots and max fee: %+v %v", c, err)
	}
	for _, ok := range [][]string{{"ESCROW_XMR_SLOTS", "5"}, {"ESCROW_XMR_MAX_FEE", "1"}, {"ESCROW_XMR_FEE_BPS", "10000", "ESCROW_XMR_FEE_ADDRESS", testXMRAddr("f", true)},
		{"ESCROW_XMR_FEE_BPS", "0", "ESCROW_XMR_FEE_ADDRESS", "ignored when the fee is 0"}} {
		if _, err := escrowXMRConfigFromEnv(xmrEnv(xmrOnEnv("/k", ok...)...)); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	refused := map[string][]string{
		"bad value":                       {"ESCROW_XMR", "yes"},
		"upper case":                      {"ESCROW_XMR", "ON"},
		"no wallet URL":                   {"ESCROW_XMR_WALLET_RPC_URL", ""},
		"no daemon URL":                   {"ESCROW_XMR_DAEMON_URL", " "},
		"mainnet Bitcoin market":          {"BITCOIN_CHAIN", "main"}, // A-460: Monero needs its own opt-in (TestXMREscrowConfigMainnetOptIn)
		"mixed network classes":           {"MONERO_NETWORK", "mainnet", "BITCOIN_CHAIN", "regtest"},
		"negative fee":                    {"ESCROW_XMR_FEE_BPS", "-1"},
		"fee above 100%":                  {"ESCROW_XMR_FEE_BPS", "10001"},
		"fractional fee":                  {"ESCROW_XMR_FEE_BPS", "2.5"},
		"fee without address":             {"ESCROW_XMR_FEE_BPS", "250"},
		"testnet fee address":             {"ESCROW_XMR_FEE_BPS", "250", "ESCROW_XMR_FEE_ADDRESS", testXMRAddressWith(53, testXMRKey("s"), testXMRKey("v"), nil, false)},
		"stagenet integrated fee address": {"ESCROW_XMR_FEE_BPS", "250", "ESCROW_XMR_FEE_ADDRESS", testXMRAddressWith(25, testXMRKey("s"), testXMRKey("v"), nil, false)}, // a stagenet fee address is checked against the daemon at startup (A-449)
		"integrated fee address":          {"ESCROW_XMR_FEE_BPS", "250", "ESCROW_XMR_FEE_ADDRESS", testXMRAddressWith(19, testXMRKey("s"), testXMRKey("v"), nil, false)},
		"fee address with a bad checksum": {"ESCROW_XMR_FEE_BPS", "250", "ESCROW_XMR_FEE_ADDRESS", testXMRAddressWith(18, testXMRKey("s"), testXMRKey("v"), nil, true)},
		"fee address, off-curve key":      {"ESCROW_XMR_FEE_BPS", "250", "ESCROW_XMR_FEE_ADDRESS", testXMRAddressWith(18, testXMROffCurveKey(), testXMRKey("v"), nil, false)},
		"fee address, small-order key":    {"ESCROW_XMR_FEE_BPS", "250", "ESCROW_XMR_FEE_ADDRESS", testXMRAddressWith(42, testXMRKey("s"), testXMRSmallOrderKey(), nil, false)},
		"Bitcoin fee address":             {"ESCROW_XMR_FEE_BPS", "250", "ESCROW_XMR_FEE_ADDRESS", "bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080"},
		"4 slots":                         {"ESCROW_XMR_SLOTS", "4"},
		"201 slots":                       {"ESCROW_XMR_SLOTS", "201"},
		"slots not a number":              {"ESCROW_XMR_SLOTS", "twenty"},
		"max fee 0":                       {"ESCROW_XMR_MAX_FEE", "0"},
		"negative max fee":                {"ESCROW_XMR_MAX_FEE", "-1"},
		"max fee above 0.1 XMR":           {"ESCROW_XMR_MAX_FEE", "100000000001"},
		"max fee in XMR":                  {"ESCROW_XMR_MAX_FEE", "0.01"},
		"max fee in exponent form":        {"ESCROW_XMR_MAX_FEE", "1e10"},
		"no wallet secret":                {"MONERO_WALLET_SECRET", ""},
		"short wallet secret":             {"MONERO_WALLET_SECRET", testXMRSecret[:31]},
		"wallet secret is SETUP_TOKEN":    {"MONERO_WALLET_SECRET", testSetupToken},
		"wallet secret is APP_SECRET_KEY": {"APP_SECRET_KEY", testXMRSecret},
		"no seed key":                     {"ESCROW_XMR_SEED_KEY", ""},
	}
	for name, extra := range refused {
		_, err := escrowXMRConfigFromEnv(xmrEnv(xmrOnEnv("/k", extra...)...))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		for _, leak := range []string{"s3cret", "127.0.0.1", testXMRSecret, testSetupToken, "escrowuser"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: refusal quotes %q: %v", name, leak, err)
			}
		}
	}
	if _, err := escrowXMRConfigFromEnv(xmrEnv(xmrOnEnv("/k", "MONERO_WALLET_SECRET", testSetupToken)...)); !errors.Is(err, errXMRSecretIsSetupToken) {
		t.Fatalf("secret equal to SETUP_TOKEN: %v", err)
	}
}

// A-460: a market configured for mainnet (a.live) runs Monero escrow only with Monero's own opt-in,
// MONERO_NETWORK=mainnet; a mainnet market from BITCOIN_CHAIN=main alone refuses ESCROW_XMR=on before anything is read
// or contacted, naming the setting.
func TestXMREscrowRefusedOnLiveMarket(t *testing.T) {
	env := xmrOnEnv("/nonexistent/seed.asc", "BITCOIN_CHAIN", "main", "MONERO_NETWORK", "")
	for i := 0; i < len(env); i += 2 {
		t.Setenv(env[i], env[i+1])
	}
	a := &App{live: true}
	if err := a.initEscrowXMR(context.Background()); !errors.Is(err, errXMRMainnetOptIn) || a.escrowXMR != nil || !strings.Contains(err.Error(), "MONERO_NETWORK=mainnet") {
		t.Fatalf("live market without MONERO_NETWORK=mainnet: %v %v", err, a.escrowXMR)
	}
	// The same opt-in on an App whose class says mainnet while the environment does not (no MONERO_NETWORK at all).
	t.Setenv("BITCOIN_CHAIN", "")
	if err := a.initEscrowXMR(context.Background()); !errors.Is(err, errXMRMainnetOptIn) || a.escrowXMR != nil {
		t.Fatalf("live App, no opt-in: %v", err)
	}
}

// A-460: MONERO_NETWORK=mainnet is Monero escrow's mainnet opt-in. Configuration reads it with the market's class
// rule (errMixedNetworks), and checks ESCROW_XMR_FEE_ADDRESS by class before any node is contacted: on a mainnet
// market only a mainnet address (standard 4… or subaddress 8…, valid keys); on a test-network market a fakechain or
// stagenet one as before (a 4…/8… address is a fakechain's there: the formats are the same).
func TestXMREscrowConfigMainnetOptIn(t *testing.T) {
	cfg := func(extra ...string) (escrowXMRConfig, error) {
		return escrowXMRConfigFromEnv(xmrEnv(xmrOnEnv("/k", extra...)...))
	}
	for _, env := range [][]string{{"MONERO_NETWORK", "mainnet"}, {"MONERO_NETWORK", " mainnet ", "BITCOIN_CHAIN", "main"}} {
		if c, err := cfg(env...); err != nil || !c.mainnet {
			t.Fatalf("%v: %+v %v", env, c, err)
		}
	}
	for _, env := range [][]string{{}, {"MONERO_NETWORK", "stagenet"}, {"BITCOIN_CHAIN", "regtest"}} {
		if c, err := cfg(env...); err != nil || c.mainnet {
			t.Fatalf("%v: mainnet %v, %v", env, c.mainnet, err)
		}
	}
	if _, err := cfg("BITCOIN_CHAIN", "main"); !errors.Is(err, errXMRMainnetOptIn) || !strings.Contains(err.Error(), "MONERO_NETWORK=mainnet") {
		t.Fatalf("BITCOIN_CHAIN=main alone: %v", err)
	}
	if _, err := cfg("BITCOIN_CHAIN", "main", "MONERO_NETWORK", "stagenet"); !errors.Is(err, errMixedNetworks) {
		t.Fatalf("mixed classes: %v", err)
	}
	fee := func(addr string, live bool) error {
		extra := []string{"ESCROW_XMR_FEE_BPS", "250", "ESCROW_XMR_FEE_ADDRESS", addr}
		if live {
			extra = append(extra, "MONERO_NETWORK", "mainnet")
		}
		_, err := cfg(extra...)
		return err
	}
	for _, c := range []struct {
		name string
		addr string
		live bool
		ok   bool
	}{
		{"mainnet standard on mainnet", testXMRAddrOn("mainnet", "fee", false), true, true},
		{"mainnet subaddress on mainnet", testXMRAddrOn("mainnet", "fee", true), true, true},
		{"stagenet standard on mainnet", testXMRAddrOn("stagenet", "fee", false), true, false},
		{"stagenet subaddress on mainnet", testXMRAddrOn("stagenet", "fee", true), true, false},
		{"testnet on mainnet", testXMRAddrOn("testnet", "fee", false), true, false},
		{"mainnet integrated on mainnet", testXMRAddressWith(19, testXMRKey("s"), testXMRKey("v"), nil, false), true, false},
		{"4… on a test-network market (a fakechain's)", testXMRAddrOn("mainnet", "fee", false), false, true},
		{"stagenet on a test-network market", testXMRAddrOn("stagenet", "fee", true), false, true},
	} {
		err := fee(c.addr, c.live)
		if c.ok != (err == nil) {
			t.Errorf("%s: %v", c.name, err)
		}
		if err != nil && (!strings.Contains(err.Error(), "ESCROW_XMR_FEE_ADDRESS") || strings.Contains(err.Error(), c.addr[:12])) {
			t.Errorf("%s: refusal %v", c.name, err)
		}
		if err != nil && c.live && !strings.Contains(err.Error(), "mainnet") {
			t.Errorf("%s: the mainnet refusal does not name mainnet: %v", c.name, err)
		}
	}
	if err := fee(testXMRAddressWith(xmrTagStandard, testXMROffCurveKey(), testXMRKey("v"), nil, false), true); !errors.Is(err, errXMRFeeAddressKeys) {
		t.Fatalf("off-curve mainnet fee address: %v", err)
	}
}

// A-417: a fee address whose keys are not points of the prime-order subgroup (its checksum valid) stops startup
// before anything is read or contacted, with its own fixed refusal naming the variable and not quoting the address.
func TestXMREscrowFeeAddressKeysRefuseStartup(t *testing.T) {
	for _, fee := range []string{testXMRAddressWith(xmrTagStandard, testXMROffCurveKey(), testXMRKey("v"), nil, false),
		testXMRAddressWith(xmrTagSubaddress, testXMRKey("s"), testXMRSmallOrderKey(), nil, false)} {
		env := xmrOnEnv("/nonexistent/seed.asc", "ESCROW_XMR_FEE_BPS", "250", "ESCROW_XMR_FEE_ADDRESS", fee)
		for i := 0; i < len(env); i += 2 {
			t.Setenv(env[i], env[i+1])
		}
		a := &App{}
		err := a.initEscrowXMR(context.Background())
		if !errors.Is(err, errXMRFeeAddressKeys) || a.escrowXMR != nil || !strings.Contains(err.Error(), "ESCROW_XMR_FEE_ADDRESS") || strings.Contains(err.Error(), fee[:12]) {
			t.Fatalf("fee address %s…: %v %v", fee[:8], err, a.escrowXMR)
		}
	}
}

// setXMREnv sets the ESCROW_XMR=on settings for New, pointing at the fake nodes.
func setXMREnv(t *testing.T, wallet, daemon *fakeXMRNode, seedKeyPath string) {
	t.Helper()
	for k, v := range map[string]string{"ESCROW_XMR": "on", "ESCROW_XMR_WALLET_RPC_URL": wallet.url(), "ESCROW_XMR_DAEMON_URL": daemon.url(),
		"MONERO_WALLET_SECRET": testXMRSecret, "ESCROW_XMR_SEED_KEY": seedKeyPath, "ESCROW_XMR_FEE_BPS": "", "ESCROW_XMR_FEE_ADDRESS": "",
		"ESCROW_XMR_SLOTS": "", "ESCROW_XMR_MAX_FEE": ""} {
		t.Setenv(k, v)
	}
}

// TestXMREscrowOffStartsAsToday: with ESCROW_XMR off the market starts as before: nothing about Monero escrow is
// read, contacted, written or logged, even with every other setting broken. DB integration.
func TestXMREscrowOffStartsAsToday(t *testing.T) {
	wallet, daemon := newFakeXMRNode(t), newFakeXMRNode(t)
	logs := captureLog(t)
	for _, v := range []string{"", "off"} {
		t.Setenv("ESCROW_XMR", v)
		for k, junk := range map[string]string{"ESCROW_XMR_WALLET_RPC_URL": wallet.url(), "ESCROW_XMR_DAEMON_URL": daemon.url(),
			"ESCROW_XMR_FEE_BPS": "-5", "ESCROW_XMR_SLOTS": "0", "ESCROW_XMR_MAX_FEE": "x", "MONERO_WALLET_SECRET": "short", "ESCROW_XMR_SEED_KEY": "/nonexistent"} {
			t.Setenv(k, junk)
		}
		x := newTestApp(t)
		if x.A.escrowXMR != nil {
			t.Fatalf("ESCROW_XMR=%q configured Monero escrow", v)
		}
		var n int
		if err := x.DB.QueryRow("SELECT count(*) FROM settings WHERE key LIKE 'escrow_xmr%'").Scan(&n); err != nil || n != 0 {
			t.Fatalf("ESCROW_XMR=%q wrote %d settings (%v)", v, n, err)
		}
	}
	if wallet.count()+daemon.count() != 0 {
		t.Fatalf("ESCROW_XMR off contacted the escrow nodes %d times", wallet.count()+daemon.count())
	}
	if lines := logs.lines("Monero escrow"); len(lines) != 0 {
		t.Fatalf("ESCROW_XMR off logged %q", lines)
	}
}

// TestXMREscrowStartupStates (P3-R2, P3-R4): ESCROW_XMR=on pins the secret verifier and the seed key on the first
// start; a changed secret disables Monero escrow without contacting the nodes; a changed seed key stops wallet
// creation only; a refused or unreachable node leaves escrow configured but unavailable and the market running.
// Every state is one fixed log line with no URL, credential or secret. DB integration.
func TestXMREscrowStartupStates(t *testing.T) {
	logs := captureLog(t)
	wallet, daemon, _ := happyXMRNodes(t)
	_, pub := testSeedKey(t, nil)
	keyPath := writeSeedKey(t, pub)
	setXMREnv(t, wallet, daemon, keyPath)
	x := newTestApp(t)
	m := x.A.escrowXMR
	if m == nil || !m.ready.Load() || !m.canCreate.Load() || m.startState != xmrLogOn || m.slots != 20 || m.maxFee != 10_000_000_000 {
		t.Fatalf("first start: configured %v", m != nil)
	}
	pinned := func(key string) string {
		var v string
		if err := x.DB.QueryRow("SELECT value FROM settings WHERE key=$1", key).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if pinned(xmrVerifierSetting) != xmrSecretVerifier(testXMRSecret) || pinned(xmrSeedKeySetting) != m.seedKey.fpr {
		t.Fatal("first start did not pin the verifier and the seed key")
	}
	if xmrWalletPassword(m.secret, "escrow-202610-01") != "e9c16b299aef3cfe95dcb7fd03131dcce1a4236db3c40ec7fe8ff44fd10eac13" {
		t.Fatal("the wallet password does not derive from MONERO_WALLET_SECRET")
	}

	// A different secret: disabled with one fixed line, and neither node is contacted.
	t.Setenv("MONERO_WALLET_SECRET", "another-wallet-secret-9876543210fedcbaZYXW")
	before := wallet.count() + daemon.count()
	x.restart()
	if m = x.A.escrowXMR; m == nil {
		t.Fatal("changed secret: Monero escrow not configured")
	}
	if m.ready.Load() || m.startState != xmrLogSecret || wallet.count()+daemon.count() != before {
		t.Fatalf("changed secret: state %q, %d requests", m.startState, wallet.count()+daemon.count()-before)
	}
	if pinned(xmrVerifierSetting) != xmrSecretVerifier(testXMRSecret) {
		t.Fatal("a changed secret re-pinned the verifier")
	}
	t.Setenv("MONERO_WALLET_SECRET", testXMRSecret)

	// A different seed key: escrow runs, wallet creation stops.
	_, other := testSeedKey(t, nil)
	t.Setenv("ESCROW_XMR_SEED_KEY", writeSeedKey(t, other))
	x.restart()
	if m = x.A.escrowXMR; !m.ready.Load() || m.canCreate.Load() || len(logs.lines(xmrLogSeedKey)) != 1 {
		t.Fatalf("changed seed key: ready %v create %v, lines %q", m.ready.Load(), m.canCreate.Load(), logs.lines("seed key"))
	}
	t.Setenv("ESCROW_XMR_SEED_KEY", keyPath)

	// A node that answers but fails the network check stops startup with a fixed refusal (errEscrowChain's rule):
	// the daemon's line for the daemon checks, the wallet-rpc's line for the wallet height checks.
	refusals := []struct {
		name         string
		info         map[string]any
		hardFork     int64
		walletHeight int64
		want, reason error
	}{
		// A-449: stagenet is accepted (TestXMREscrowStagenetTags), and heights no longer tell networks apart
		// (TestXMREscrowNoHeightHeuristic); mainnet, testnet and inconsistent flags are refused.
		{"testnet daemon", map[string]any{"nettype": "testnet", "testnet": true, "height": 120}, 16, 120, errXMRStartDaemon, errXMRDaemonNetwork},
		{"mainnet daemon", map[string]any{"nettype": "mainnet", "mainnet": true, "height": 120}, 16, 120, errXMRStartDaemon, errXMRDaemonNetwork},
		{"fakechain with the mainnet flag", map[string]any{"nettype": "fakechain", "mainnet": true, "height": 120}, 16, 120, errXMRStartDaemon, errXMRDaemonNetwork},
		{"fakechain with the stagenet flag", map[string]any{"nettype": "fakechain", "stagenet": true, "height": 120}, 16, 120, errXMRStartDaemon, errXMRDaemonNetwork},
		{"stagenet without the stagenet flag", map[string]any{"nettype": "stagenet", "height": 120}, 16, 120, errXMRStartDaemon, errXMRDaemonNetwork},
		{"stagenet with the mainnet flag", map[string]any{"nettype": "stagenet", "stagenet": true, "mainnet": true, "height": 120}, 16, 120, errXMRStartDaemon, errXMRDaemonNetwork},
		{"no nettype", map[string]any{"height": 120}, 16, 120, errXMRStartDaemon, errXMRDaemonNetwork},
		{"hard fork 15", map[string]any{"nettype": "fakechain", "height": 120, "synchronized": true}, 15, 120, errXMRStartDaemon, errXMRHardFork},
		{"synced stagenet at hard fork 1", map[string]any{"nettype": "stagenet", "stagenet": true, "height": 120, "synchronized": true}, 1, 120, errXMRStartDaemon, errXMRHardFork},
		{"wallet 11 blocks behind", map[string]any{"nettype": "fakechain", "height": 120}, 16, 109, errXMRStartWallet, errXMRWalletLag},
	}
	for _, c := range refusals {
		daemon.result("get_info", c.info)
		daemon.result("hard_fork_info", map[string]any{"version": c.hardFork})
		wallet.result("get_height", map[string]any{"height": c.walletHeight})
		a, err := New(context.Background(), false)
		if err == nil {
			a.Close()
			t.Fatalf("%s: the market started", c.name)
		}
		if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.reason.Error()) {
			t.Fatalf("%s: %v", c.name, err)
		}
		for _, leak := range []string{"127.0.0.1", "s3cret", "escrowuser"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("%s: the refusal carries %q", c.name, leak)
			}
		}
	}
	daemon.result("get_info", map[string]any{"nettype": "fakechain", "height": 120})
	daemon.result("hard_fork_info", map[string]any{"version": 16})
	wallet.result("get_height", map[string]any{"height": 120})

	// No wallet open (-13): only the daemon is checked, and escrow is on.
	wallet.set("refresh", func(json.RawMessage) (any, int, string) { return nil, -13, "No wallet file" })
	x.restart()
	if m = x.A.escrowXMR; !m.ready.Load() || m.startState != xmrLogOn {
		t.Fatalf("no wallet open: %q", m.startState)
	}
	wallet.result("refresh", map[string]any{"blocks_fetched": 0})

	// An unreachable wallet-rpc, an unreachable daemon, a daemon that times out: configured, unavailable, the class
	// logged, and the market starts.
	down := newFakeXMRNode(t)
	downURL := down.url()
	down.Close()
	t.Setenv("ESCROW_XMR_WALLET_RPC_URL", downURL)
	x.restart()
	if m = x.A.escrowXMR; m.ready.Load() || m.refused.Load() || m.startState != xmrLogUnchecked || len(logs.lines(xmrLogUnchecked+" (refresh unreachable)")) != 1 {
		t.Fatalf("unreachable wallet: %q, lines %q", m.startState, logs.lines("could not be checked"))
	}
	t.Setenv("ESCROW_XMR_WALLET_RPC_URL", wallet.url())
	t.Setenv("ESCROW_XMR_DAEMON_URL", downURL)
	x.restart()
	if m = x.A.escrowXMR; m.ready.Load() || m.startState != xmrLogUnchecked || len(logs.lines(xmrLogUnchecked+" (get_info unreachable)")) != 1 {
		t.Fatalf("unreachable daemon: %q, lines %q", m.startState, logs.lines("could not be checked"))
	}
	t.Setenv("ESCROW_XMR_DAEMON_URL", daemon.url())
	saved := paymentStartupTimeout
	paymentStartupTimeout = 300 * time.Millisecond
	daemon.setDelay("get_info", time.Second)
	x.restart()
	paymentStartupTimeout = saved
	daemon.setDelay("get_info", 0)
	if m = x.A.escrowXMR; m.ready.Load() || m.startState != xmrLogUnchecked || len(logs.lines(xmrLogUnchecked+" (get_info timeout)")) != 1 {
		t.Fatalf("daemon timeout: %q, lines %q", m.startState, logs.lines("could not be checked"))
	}

	for _, leak := range []string{"127.0.0.1", "s3cret", "escrowuser", testXMRSecret, keyPath} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("the log carries %q:\n%s", leak, logs.String())
		}
	}
	for want, n := range map[string]int{xmrLogOn: 3, xmrLogSecret: 1, xmrLogRefused: 0} { // on: first start, changed seed key, no wallet open
		if len(logs.lines(want)) != n {
			t.Errorf("want %d lines %q, got %q", n, want, logs.lines(want))
		}
	}
}

// TestXMREscrowNotReadyNamesEachState: while Monero escrow is configured but not ready, notReady names each fixed state
// of its startup checks in words, with its class, and what to do; ready, it says nothing (QA4W4B-1). The refusal is the
// one refuseEscrowXMR records. Unit test (no database).
func TestXMREscrowNotReadyNamesEachState(t *testing.T) {
	x := &escrowXMR{}
	if s, r := x.notReady(); s != "its startup checks have not passed yet" || !strings.Contains(r, "server log") {
		t.Fatalf("no state recorded: %q, %q", s, r)
	}
	for _, c := range []struct{ line, class, state, remedy string }{
		{xmrLogUnchecked, "get_info unreachable", "the escrow daemon or wallet-rpc could not be checked at startup (get_info unreachable)", "ESCROW_XMR_WALLET_RPC_URL"},
		{xmrLogNetwork, "", "the escrow daemon is not on the network this database's Monero escrows are on", "a new network needs a new database"},
		{xmrLogSecret, "", "MONERO_WALLET_SECRET is not the secret this database was first started with", "Restore the original secret and restart the app"},
		{xmrLogUnrecorded, "db-connection", "its secret verifier or seed key could not be checked in the database (db-connection)", "Check the database"},
		{xmrLogOn, "stagenet", "its startup checks have not passed yet", "The server log names the state"},
	} {
		x.state.Store(&xmrState{c.line, c.class})
		if s, r := x.notReady(); s != c.state || !strings.Contains(r, c.remedy) {
			t.Errorf("%s: %q, %q", c.line, s, r)
		}
	}
	x.state.Store(&xmrState{xmrLogUnchecked, "refresh unreachable"})
	x.ready.Store(true)
	if s, r := x.notReady(); s != "" || r != "" {
		t.Fatalf("ready: %q, %q", s, r)
	}
	logs := captureLog(t)
	x.refuseEscrowXMR(errXMRNetworkMoved)
	if s, r := x.notReady(); s != "refused until the app is restarted" || !strings.Contains(r, "Reason: "+errXMRNetworkMoved.msg+".") || !strings.Contains(r, "restart the app") ||
		strings.Count(logs.String(), xmrLogRefused) != 1 {
		t.Fatalf("refused: %q, %q", s, r)
	}
}

// TestXMREscrowRecheckIsSticky: after startup, the watcher's re-check (recheckEscrowXMRNetwork) disables Monero
// escrow until a restart when a node answers but fails the network check, logs one fixed line, and contacts neither
// node again; an unreachable node changes nothing. Unit test (no database).
func TestXMREscrowRecheckIsSticky(t *testing.T) {
	logs := captureLog(t)
	ctx := context.Background()
	wallet, daemon, r := happyXMRNodes(t)
	a := &App{escrowXMR: &escrowXMR{rpc: r}}
	a.escrowXMR.ready.Store(true)
	if err := a.recheckEscrowXMRNetwork(ctx); err != nil {
		t.Fatalf("healthy nodes: %v", err)
	}

	// Unreachable: returned, not sticky.
	down := newFakeXMRNode(t)
	gone, err := newXMREscrowRPC(wallet.url(), down.url())
	if err != nil {
		t.Fatal(err)
	}
	down.Close()
	a.escrowXMR.rpc = gone
	var re *xmrRPCError
	if err := a.recheckEscrowXMRNetwork(ctx); !errors.As(err, &re) || re.Class != "unreachable" || a.escrowXMR.refused.Load() || !a.escrowXMR.ready.Load() {
		t.Fatalf("unreachable daemon: %v, refused %v", err, a.escrowXMR.refused.Load())
	}
	a.escrowXMR.rpc = r

	// The wallet-rpc drifts 11 blocks from the daemon: refused, sticky, one line.
	wallet.result("get_height", map[string]any{"height": 131})
	if err := a.recheckEscrowXMRNetwork(ctx); !errors.Is(err, errXMRWalletLag) || !a.escrowXMR.refused.Load() || a.escrowXMR.ready.Load() {
		t.Fatalf("wallet lag: %v", err)
	}
	wallet.result("get_height", map[string]any{"height": 120})
	before := wallet.count() + daemon.count()
	for range 3 {
		if err := a.recheckEscrowXMRNetwork(ctx); !errors.Is(err, errXMRRefusedUntilRestart) {
			t.Fatalf("after a refusal: %v", err)
		}
	}
	if n := wallet.count() + daemon.count() - before; n != 0 || a.escrowXMR.ready.Load() {
		t.Fatalf("a refused escrow made %d requests", n)
	}
	if lines := logs.lines(xmrLogRefused); len(lines) != 1 || lines[0] != xmrLogRefused+errXMRWalletLag.Error() {
		t.Fatalf("refusal lines %q", lines)
	}

	// A daemon refusal is sticky the same way; startup's refusals are never used at runtime.
	b := &App{escrowXMR: &escrowXMR{rpc: r}}
	daemon.result("get_info", map[string]any{"nettype": "mainnet", "mainnet": true, "height": 120})
	if err := b.recheckEscrowXMRNetwork(ctx); !errors.Is(err, errXMRDaemonNetwork) || errors.Is(err, errXMRStartDaemon) || !b.escrowXMR.refused.Load() {
		t.Fatalf("mainnet daemon: %v", err)
	}
	// A-449: a daemon that moved to the other escrow network (stagenet, escrow started on the fakechain) is refused the
	// same way: the pin decides on the next start.
	c := &App{escrowXMR: &escrowXMR{rpc: r}}
	daemon.result("get_info", map[string]any{"nettype": "stagenet", "stagenet": true, "height": 120})
	if err := c.recheckEscrowXMRNetwork(ctx); !errors.Is(err, errXMRNetworkMoved) || !c.escrowXMR.refused.Load() {
		t.Fatalf("stagenet daemon after a fakechain start: %v", err)
	}
	// The watcher's retry of the startup checks cannot undo a refusal either (it returns before the database).
	if _, _, err := b.startEscrowXMR(ctx); !errors.Is(err, errXMRRefusedUntilRestart) || b.escrowXMR.ready.Load() {
		t.Fatalf("retry after a refusal: %v", err)
	}
	if err := (&App{}).recheckEscrowXMRNetwork(ctx); !errors.Is(err, errXMRRefusedUntilRestart) {
		t.Fatalf("escrow off: %v", err)
	}
}
