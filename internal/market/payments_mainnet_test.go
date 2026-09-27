package market

import (
	"context"
	"strings"
	"testing"
)

// Mainnet is opt-in: BITCOIN_CHAIN=main and MONERO_NETWORK=mainnet accept a live node; a blank setting still
// accepts test networks only (payments_bitcoin_test.go, payments_monero_test.go). A market is all mainnet or
// all test networks, and a database that holds deposits from the other class of network is refused.

// liveFake is the fake wallet reporting mainnet, for a market configured for mainnet.
type liveFake struct{ *fakeProvider }

func (liveFake) Network() string         { return "mainnet" }
func (liveFake) MainnetConfigured() bool { return true }

func TestBitcoinMainnetOptIn(t *testing.T) {
	ctx := context.Background()
	core, s := newPayCore(t, "main")
	core.newAddr = "bc1qorderaddress000000000000000000000"
	p, err := newBitcoinProvider(ctx, s.url(""), "opsecmkt", "main", 3)
	if err != nil || p.Network() != "mainnet" || !isLiveNetwork(p.Network()) {
		t.Fatalf("BITCOIN_CHAIN=main with a mainnet node: %v network=%q", err, p.Network())
	}
	if addr, err := p.NewAddress(ctx, "order-1"); err != nil || addr != core.newAddr {
		t.Fatalf("mainnet address: %q %v", addr, err)
	}
	core.set(func(c *payCore) { c.newAddr = "tb1qorderaddress000000000000000000000" })
	if addr, err := p.NewAddress(ctx, "order-2"); err == nil {
		t.Fatalf("test-network address accepted on mainnet: %s", addr)
	}
	for addr, want := range map[string]bool{
		"bc1qpayoutaddress00000000000000000000":   true,
		"tb1qpayoutaddress00000000000000000000":   false,
		"bcrt1qpayoutaddress00000000000000000000": false,
		"BC1QPAYOUTADDRESS00000000000000000000":   false,
	} {
		if got := p.ValidAddress(addr); got != want {
			t.Fatalf("ValidAddress(%s) = %v on mainnet", addr, got)
		}
	}
	if _, err := p.Send(ctx, "tb1qpayoutaddress00000000000000000000", 1000); err == nil {
		t.Fatal("mainnet wallet sent to a test-network address")
	}
	if s.called("sendtoaddress") != 0 {
		t.Fatal("sendtoaddress called for a rejected address")
	}
	// The node later reporting a test chain is refused: the provider never changes network.
	core.set(func(c *payCore) { c.chain = "test" })
	if _, err := p.Check(ctx); err == nil || !isRefusal(err) {
		t.Fatalf("mainnet provider accepted a testnet node: %v", err)
	}
}

func TestMoneroMainnetOptIn(t *testing.T) {
	ctx := context.Background()
	mainSub := "8" + payStagenetSub[1:]
	w, s := newPayMonero(t, payMainnetPrimary, mainSub)
	w.nettype = "mainnet"
	w.valid[mainSub], w.valid[payStagenetSub] = "mainnet", "stagenet"
	p, err := newMoneroProvider(ctx, s.url(""), s.url(""), "mainnet", 10)
	if err != nil || p.Network() != "mainnet" {
		t.Fatalf("MONERO_NETWORK=mainnet with a mainnet wallet and daemon: %v", err)
	}
	if addr, err := p.NewAddress(ctx, "order-1"); err != nil || addr != mainSub {
		t.Fatalf("mainnet subaddress: %q %v", addr, err)
	}
	if !p.ValidAddress(mainSub) {
		t.Fatal("mainnet payout address rejected")
	}
	if p.ValidAddress(payStagenetSub) {
		t.Fatal("stagenet payout address accepted on mainnet")
	}
	w.set(func(m *payMoneroWallet) { m.nettype = "stagenet" })
	if _, err := p.Check(ctx); err == nil || !isRefusal(err) || !strings.Contains(err.Error(), "daemon is on stagenet but the wallet is on mainnet") {
		t.Fatalf("mainnet wallet accepted a stagenet daemon: %v", err)
	}
	// A stagenet wallet never satisfies MONERO_NETWORK=mainnet (payments_monero_test.go), and a mainnet wallet
	// without it is refused.
	if _, err := newMoneroProvider(ctx, s.url(""), "", "", 10); err == nil || !isRefusal(err) || !strings.Contains(err.Error(), "unless MONERO_NETWORK=mainnet") {
		t.Fatalf("mainnet wallet accepted without MONERO_NETWORK=mainnet: %v", err)
	}
}

func TestInitPaymentsLiveMarket(t *testing.T) {
	ctx := context.Background()
	_, btc := newPayCore(t, "main")
	setPaymentEnv(t, btc.url(""), "main", "", "", "")
	a := newPaymentsApp()
	if err := a.initPayments(ctx); err != nil || !a.live || a.provider("BTC") == nil || a.netTag() != "MAINNET" {
		t.Fatalf("mainnet market: %v live=%v", err, a.live)
	}
	// An unreachable mainnet node leaves the market live, with the currency unavailable.
	setPaymentEnv(t, "http://u:p@127.0.0.1:1", "main", "", "", "")
	a = newPaymentsApp()
	if err := a.initPayments(ctx); err != nil || !a.live || a.provider("BTC") != nil {
		t.Fatalf("unreachable mainnet node: %v live=%v", err, a.live)
	}
	// Mainnet and a test network together are refused, whichever currency is live.
	_, xmr := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	setPaymentEnv(t, btc.url(""), "main", xmr.url(""), "", "")
	if err := newPaymentsApp().initPayments(ctx); err == nil || !strings.Contains(err.Error(), "one payment currency is configured for mainnet") {
		t.Fatalf("mixed mainnet and stagenet accepted: %v", err)
	}
	_, signet := newPayCore(t, "signet")
	setPaymentEnv(t, signet.url(""), "", "http://127.0.0.1:1", "", "mainnet")
	if err := newPaymentsApp().initPayments(ctx); err == nil || !strings.Contains(err.Error(), "one payment currency is configured for mainnet") {
		t.Fatalf("mixed signet and mainnet accepted: %v", err)
	}
	// A blank BITCOIN_CHAIN never implies mainnet.
	setPaymentEnv(t, btc.url(""), "", "", "", "")
	if err := newPaymentsApp().initPayments(ctx); err == nil || !strings.Contains(err.Error(), "set BITCOIN_CHAIN=main") {
		t.Fatalf("mainnet node accepted without BITCOIN_CHAIN=main: %v", err)
	}
}

func TestCheckProviderNetworkClass(t *testing.T) {
	ctx := context.Background()
	f := newFakeProvider("BTC", 3)
	if _, err := checkProvider(ctx, liveFake{f}, false); err == nil || !isRefusal(err) || !strings.Contains(err.Error(), `network "mainnet" is not a test network`) {
		t.Fatalf("mainnet provider accepted on a test-network market: %v", err)
	}
	if _, err := checkProvider(ctx, f, true); err == nil || !isRefusal(err) || !strings.Contains(err.Error(), "not mainnet") {
		t.Fatalf("test-network provider accepted on a mainnet market: %v", err)
	}
	if _, err := checkProvider(ctx, liveFake{f}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := checkProvider(ctx, f, false); err != nil {
		t.Fatal(err)
	}
}

// A mainnet market labels pages and order notes MAINNET, and startup refuses a database that holds deposits
// from the other class of network.
func TestLiveMarketLabelsAndHistory(t *testing.T) {
	p := newPayEnv(t)
	ctx := context.Background()
	// newPayEnv issued a test-network deposit address: a mainnet wallet must not take this database over.
	p.A.live = true
	if err := p.A.checkNetworkHistory(ctx); err == nil || !strings.Contains(err.Error(), "holds BTC payments made on a test network") {
		t.Fatalf("mainnet market started on a test-network database: %v", err)
	}
	p.A.live = false
	if err := p.A.checkNetworkHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB.Exec("UPDATE payment_addresses SET provider='btc-mainnet'"); err != nil {
		t.Fatal(err)
	}
	if err := p.A.checkNetworkHistory(ctx); err == nil || !strings.Contains(err.Error(), "holds BTC payments made on mainnet") {
		t.Fatalf("test-network market started on a mainnet database: %v", err)
	}

	// A watcher status row from a test network (a testnet3 tip is far above mainnet's) is reset, not kept.
	if _, err := p.DB.Exec("INSERT INTO payment_status(currency,network,last_error,tip_height) VALUES('BTC','testnet3','',4500000)"); err != nil {
		t.Fatal(err)
	}
	p.A.live = true
	p.A.payments["BTC"] = liveFake{p.fake}
	if err := p.A.checkNetworkHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if n := p.count("SELECT count(*) FROM payment_status WHERE currency='BTC'"); n != 0 {
		t.Fatalf("test-network watcher status kept on a mainnet market: %d row(s)", n)
	}
	catalog := p.page("/", p.buyerSess)
	if !strings.Contains(catalog, "MAINNET payments (BTC mainnet) — real funds") || strings.Contains(catalog, "TESTNET") || strings.Contains(catalog, "no real funds") {
		t.Fatal("catalog does not say payments are mainnet with real funds")
	}
	order := p.page("/order?id="+p.order, p.buyerSess)
	if !strings.Contains(order, `<span class="badge live">MAINNET deposit address</span>`) || !strings.Contains(order, "Mainnet coins only") ||
		strings.Contains(order, "TESTNET") || strings.Contains(order, "Test coins only") {
		t.Fatal("order page does not label the deposit MAINNET")
	}
	// Every page with a network label renders on the mainnet path and never says TESTNET.
	for _, pg := range []struct{ path, session string }{
		{"/product?id=" + p.productID, p.buyerSess}, {"/checkout?id=" + p.productID, p.buyerSess},
		{"/account", p.vendorSess}, {"/vendor-dashboard", p.vendorSess}, {"/admin", p.adminSess},
		{"/moderator", p.adminSess}, {"/canary", ""},
	} {
		if body := p.page(pg.path, pg.session); strings.Contains(body, "TESTNET") || strings.Contains(body, "no real funds") {
			t.Fatalf("%s says TESTNET on a mainnet market", pg.path)
		}
	}
	p.paidByWatcher(p.order, p.addr, "tx-live-1")
	note := p.str("SELECT note FROM order_events WHERE order_id=$1 AND note LIKE '%payment confirmed%'", p.order)
	if !strings.HasPrefix(note, "MAINNET mainnet payment confirmed") {
		t.Fatalf("payment note: %q", note)
	}
}
