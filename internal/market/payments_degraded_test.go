package market

import (
	"context"
	"strings"
	"testing"
)

// Provider availability without a database: initPayments at startup and refreshProviders at the start of
// every watcher pass, against the scripted Bitcoin Core and monero-wallet-rpc servers.

func newPaymentsApp() *App {
	return &App{payments: map[string]PaymentProvider{}, unavailable: map[string]*unavailableProvider{}}
}

func setPaymentEnv(t *testing.T, btcURL, btcChain, xmrWallet, xmrDaemon, xmrNetwork string) {
	t.Helper()
	for k, v := range map[string]string{
		"BITCOIN_RPC_URL": btcURL, "BITCOIN_CHAIN": btcChain, "BITCOIN_WALLET": "",
		"MONERO_WALLET_RPC_URL": xmrWallet, "MONERO_RPC_URL": xmrDaemon, "MONERO_NETWORK": xmrNetwork,
		"PAYMENT_CONFIRMATIONS_BTC": "", "PAYMENT_CONFIRMATIONS_XMR": "", "PAYMENT_POLL_INTERVAL": "", "PAYMENT_EXPIRY": "",
	} {
		t.Setenv(k, v)
	}
}

func (c *payCore) set(f func(c *payCore)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(c)
}

func (m *payMoneroWallet) set(f func(m *payMoneroWallet)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(m)
}

func TestUnreachableBitcoinNodeIsUnavailableNotFatal(t *testing.T) {
	ctx := context.Background()
	core, s := newPayCore(t, "signet")
	s.down.Store(true)
	setPaymentEnv(t, s.url(""), "", "", "", "") // blank BITCOIN_CHAIN accepts any test chain the node reports
	a := newPaymentsApp()
	if err := a.initPayments(ctx); err != nil {
		t.Fatalf("unreachable node stopped startup: %v", err)
	}
	if a.provider("BTC") != nil || !a.paymentsConfigured() {
		t.Fatal("unreachable provider registered as working, or forgotten")
	}
	msg, disabled := a.unavailableError("BTC")
	if !strings.Contains(msg, "bitcoin RPC unreachable") || disabled || strings.Contains(msg, "rpc-secret") {
		t.Fatalf("unavailable error %q disabled=%v", msg, disabled)
	}
	if skip, _, _ := a.refreshProviders(ctx); !strings.Contains(skip["BTC"], "unreachable") {
		t.Fatalf("pass not skipped while the node is down: %v", skip)
	}
	// The node comes back after a restart without the wallet loaded: the next pass loads it.
	s.down.Store(false)
	core.set(func(c *payCore) { c.walletLoaded = false })
	if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 {
		t.Fatalf("recovered node still skipped: %v", skip)
	}
	p := a.provider("BTC")
	if p == nil || p.Network() != "signet" || s.called("loadwallet") != 1 {
		t.Fatalf("provider not promoted: %v loadwallet=%d", p, s.called("loadwallet"))
	}
	if msg, _ := a.unavailableError("BTC"); msg != "" {
		t.Fatalf("stale unavailable error %q", msg)
	}
}

func TestMissingBitcoinWalletIsUnavailableUntilCreated(t *testing.T) {
	ctx := context.Background()
	core, s := newPayCore(t, "testnet4")
	core.set(func(c *payCore) { c.walletLoaded, c.walletGone = false, true })
	setPaymentEnv(t, s.url(""), "testnet4", "", "", "")
	a := newPaymentsApp()
	if err := a.initPayments(ctx); err != nil {
		t.Fatalf("missing wallet stopped startup: %v", err)
	}
	if msg, _ := a.unavailableError("BTC"); a.provider("BTC") != nil || !strings.Contains(msg, "createwallet") {
		t.Fatalf("missing wallet: %q", msg)
	}
	core.set(func(c *payCore) { c.walletGone = false })
	if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 || a.provider("BTC") == nil {
		t.Fatalf("created wallet not picked up: %v", skip)
	}
}

func TestNodeReportingMainnetAfterStartupStaysDisabled(t *testing.T) {
	ctx := context.Background()
	_, s := newPayCore(t, "main")
	s.down.Store(true)
	setPaymentEnv(t, s.url(""), "", "", "", "")
	a := newPaymentsApp()
	if err := a.initPayments(ctx); err != nil {
		t.Fatalf("unreachable node stopped startup: %v", err)
	}
	s.down.Store(false)
	logs := captureLog(t)
	skip, _, why := a.refreshProviders(ctx)
	msg, disabled := a.unavailableError("BTC")
	if a.provider("BTC") != nil || !disabled || !strings.Contains(msg, `bitcoin chain "main" is not a test network`) || skip["BTC"] != msg {
		t.Fatalf("mainnet node after startup: provider=%v disabled=%v msg=%q skip=%v", a.provider("BTC"), disabled, msg, skip)
	}
	// The log names the refusal's class, not the node's answer (A-170); the reason is on the admin page.
	if got := logs.lines("payments: BTC"); len(got) != 1 || got[0] != "payments: BTC provider disabled: "+refusedClass || why["BTC"] != refusedClass {
		t.Fatalf("disabled log %q why %q", got, why)
	}
	calls := s.called("getblockchaininfo")
	if skip, _, why = a.refreshProviders(ctx); skip["BTC"] != msg || why["BTC"] != refusedClass || s.called("getblockchaininfo") != calls {
		t.Fatal("a refused provider was checked again or its reason dropped")
	}
}

func TestMainnetAtStartupIsStillFatal(t *testing.T) {
	ctx := context.Background()
	_, s := newPayCore(t, "main")
	setPaymentEnv(t, s.url(""), "", "", "", "")
	if err := newPaymentsApp().initPayments(ctx); err == nil || !strings.Contains(err.Error(), `refusing to start: bitcoin chain "main"`) {
		t.Fatalf("mainnet bitcoin node accepted at startup: %v", err)
	}
	_, w := newPayMonero(t, payMainnetPrimary, payStagenetSub)
	setPaymentEnv(t, "", "", w.url(""), "", "")
	if err := newPaymentsApp().initPayments(ctx); err == nil || !strings.Contains(err.Error(), "refusing to start: monero wallet primary address is a mainnet address") {
		t.Fatalf("mainnet monero wallet accepted at startup: %v", err)
	}
	setPaymentEnv(t, "", "", "", "", "mainnet")
	t.Setenv("MONERO_WALLET_RPC_URL", "http://127.0.0.1:1")
	if err := newPaymentsApp().initPayments(ctx); err == nil || !strings.Contains(err.Error(), "MONERO_NETWORK") {
		t.Fatalf("MONERO_NETWORK=mainnet accepted: %v", err)
	}
}

func TestRunningBitcoinProviderRecoversWalletAndSkipsWhileSyncing(t *testing.T) {
	ctx := context.Background()
	core, s := newPayCore(t, "testnet4")
	setPaymentEnv(t, s.url(""), "", "", "", "")
	a := newPaymentsApp()
	if err := a.initPayments(ctx); err != nil || a.provider("BTC") == nil {
		t.Fatalf("startup: %v", err)
	}
	core.set(func(c *payCore) { c.walletLoaded = false }) // bitcoind restarted without load_on_startup
	if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 || s.called("loadwallet") != 1 {
		t.Fatalf("wallet not reloaded: %v loadwallet=%d", skip, s.called("loadwallet"))
	}
	core.set(func(c *payCore) { c.ibd = true })
	if skip, _, _ := a.refreshProviders(ctx); skip["BTC"] != syncingReason || a.provider("BTC") == nil {
		t.Fatalf("initial block download not skipped: %v", skip)
	}
	core.set(func(c *payCore) { c.ibd = false })
	s.down.Store(true)
	if skip, _, _ := a.refreshProviders(ctx); !strings.Contains(skip["BTC"], "Wallet check failed") || a.provider("BTC") == nil {
		t.Fatalf("outage must skip the pass but keep the provider: %v", skip)
	}
	s.down.Store(false)
	core.set(func(c *payCore) { c.chain = "main" })
	skip, _, _ := a.refreshProviders(ctx)
	if msg, disabled := a.unavailableError("BTC"); a.provider("BTC") != nil || !disabled || skip["BTC"] != msg || !strings.Contains(msg, "not a test network") {
		t.Fatalf("mainnet after startup kept the provider: %v %q", skip, msg)
	}
}

func TestMoneroWalletReopenedAndDaemonSyncState(t *testing.T) {
	ctx := context.Background()
	w, s := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	w.set(func(m *payMoneroWallet) { m.open = false })
	s.down.Store(true)
	setPaymentEnv(t, "", "", s.url(""), s.url(""), "")
	a := newPaymentsApp()
	if err := a.initPayments(ctx); err != nil {
		t.Fatalf("unreachable wallet stopped startup: %v", err)
	}
	if msg, _ := a.unavailableError("XMR"); a.provider("XMR") != nil || !strings.Contains(msg, "monero wallet RPC unreachable") {
		t.Fatalf("unavailable wallet: %q", msg)
	}
	s.down.Store(false)
	if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 || a.provider("XMR") == nil || a.provider("XMR").Network() != "stagenet" || s.called("open_wallet") != 1 {
		t.Fatalf("wallet not opened and promoted: %v", skip)
	}
	w.set(func(m *payMoneroWallet) { m.open = false }) // monero-wallet-rpc restarted with no wallet open
	if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 || s.called("open_wallet") != 2 {
		t.Fatalf("wallet not reopened: %v open_wallet=%d", skip, s.called("open_wallet"))
	}
	w.set(func(m *payMoneroWallet) { m.height, m.target = 100, 500 })
	if skip, _, _ := a.refreshProviders(ctx); skip["XMR"] != syncingReason {
		t.Fatalf("syncing daemon not skipped: %v", skip)
	}
	w.set(func(m *payMoneroWallet) { m.target = 0 }) // monerod reports 0 once synchronized
	if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 {
		t.Fatalf("synced daemon skipped: %v", skip)
	}
	w.set(func(m *payMoneroWallet) { m.nettype = "mainnet" })
	a.refreshProviders(ctx)
	if msg, disabled := a.unavailableError("XMR"); a.provider("XMR") != nil || !disabled || !strings.Contains(msg, `monero daemon network "mainnet"`) {
		t.Fatalf("mainnet daemon after startup: %q", msg)
	}
}
