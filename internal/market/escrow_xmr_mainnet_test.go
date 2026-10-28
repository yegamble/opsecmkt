package market

import (
	"context"
	"errors"
	"html"
	"strings"
	"testing"
)

// A-460: Monero escrow on mainnet, the operator's opt-in (MONERO_NETWORK=mainnet). DB integration tests against the
// simulated wallet-rpc and monerod (escrow_xmr_fake_test.go): simulated RPC only, never a real mainnet node or funds.

// onMainnet makes the fake a synced mainnet daemon with peers and a fresh tip; setEnv then sets MONERO_NETWORK=mainnet.
func onMainnet(f *fakeXMREscrow) { f.nettype, f.peers = "mainnet", 8 }

// On a mainnet market the escrow runs against a mainnet daemon: the pin is mainnet, the wallets' addresses are mainnet
// ones, refund and claim addresses must be mainnet standard addresses or subaddresses (stagenet, testnet and integrated
// refused, naming mainnet's prefixes), and pages, notes and notices say mainnet, never a test network.
func TestXMREscrowMainnetTags(t *testing.T) {
	x := newXMREscrowEnvWith(t, onMainnet)
	if !x.A.live {
		t.Fatal("MONERO_NETWORK=mainnet did not make a mainnet market")
	}
	if m := x.A.escrowXMR; m == nil || !m.ready.Load() || m.rpc.nettype() != "mainnet" {
		t.Fatal("Monero escrow is not ready on a mainnet daemon with MONERO_NETWORK=mainnet")
	}
	if got := x.networkPin(); len(got) != 1 || got[0] != "mainnet" {
		t.Fatalf("network pin %v", got)
	}
	if x.A.xmrEscrowNetwork() != "mainnet" || x.A.xmrEscrowNetShort() != "Monero mainnet" {
		t.Fatalf("network %q, short %q", x.A.xmrEscrowNetwork(), x.A.xmrEscrowNetShort())
	}
	x.mustPoll()
	if got := agStrings(t, x.testEnv, "SELECT substr(primary_address,1,1) FROM xmr_escrow_wallets"); len(got) != 1 || got[0] != "4" {
		t.Fatalf("wallet primary addresses %v", got)
	}
	order := x.draft(x.product)
	if page := html.UnescapeString(x.page(orderPage(order), x.buyerSess)); !strings.Contains(page, "Or pay into a Monero escrow (mainnet, real funds)") ||
		!strings.Contains(page, "starting with 4 or 8 on Monero mainnet") || strings.Contains(strings.ToLower(page), "fakechain") || strings.Contains(page, "test chain") {
		t.Fatalf("the checkout offer does not name mainnet only:\n%s", page)
	}
	for name, addr := range map[string]string{
		"stagenet standard":   testXMRAddrOn("stagenet", "mn refund", false),
		"stagenet subaddress": testXMRAddrOn("stagenet", "mn refund", true),
		"testnet subaddress":  testXMRAddrOn("testnet", "mn refund", true),
		"mainnet integrated":  testXMRAddressWith(19, testXMRKey("mn/s"), testXMRKey("mn/v"), testXMRKey("pid")[:8], false),
	} {
		w := x.checkout(x.buyerSess, order, addr)
		body := html.UnescapeString(w.Body.String())
		if w.Code != 400 || x.state(order) != stateDraft {
			t.Fatalf("%s refund on mainnet: %d %s", name, w.Code, body)
		}
		if !strings.Contains(name, "integrated") && !strings.Contains(body, "Monero mainnet, whose addresses start with 4 or 8") {
			t.Fatalf("%s refund: the refusal does not name mainnet's prefixes:\n%s", name, body)
		}
	}
	addr := x.mustCheckoutWith(order, testXMRAddrOn("mainnet", "mn refund", true))
	if addr[0] != '8' {
		t.Fatalf("escrow address %s…", addr[:8])
	}
	page := html.UnescapeString(x.page(orderPage(order), x.buyerSess))
	if !strings.Contains(page, `<span class="badge live">MAINNET</span>`) || !strings.Contains(page, "Monero escrow on this market runs on Monero mainnet") ||
		strings.Contains(strings.ToLower(page), "fakechain") || strings.Contains(page, "STAGENET") || strings.Contains(page, "no value") {
		t.Fatalf("the order page does not name mainnet only:\n%s", page)
	}
	if home := html.UnescapeString(x.page("/", x.buyerSess)); !strings.Contains(home, "Monero escrow (Monero mainnet)") || strings.Contains(strings.ToLower(home), "fakechain") {
		t.Fatalf("the site-wide notice does not name mainnet only:\n%s", home)
	}
	x.fake.payAddress(addr, hex64("mn deposit"), xmrListingPrice, 10)
	x.mustPoll()
	x.endWindows()
	if x.state(order) != stateCompleted {
		t.Fatalf("funded order on mainnet: %s", x.state(order))
	}
	if notes := strings.Join(agStrings(t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1", order), "\n"); !strings.Contains(notes, "MAINNET mainnet escrow funded") || strings.Contains(notes, "fakechain") || strings.Contains(notes, "TESTNET") {
		t.Fatalf("order notes:\n%s", notes)
	}
	if code, body := x.xmrClaim(order, x.vendorSess, testXMRAddrOn("stagenet", "mn claim", true), testPassword); code != 400 || !strings.Contains(html.UnescapeString(body), "start with 4 or 8") {
		t.Fatalf("stagenet claim address on mainnet: %d %s", code, body)
	}
	if code, body := x.xmrClaim(order, x.vendorSess, testXMRAddrOn("mainnet", "mn claim", true), testPassword); code != 303 {
		t.Fatalf("mainnet claim: %d %s", code, body)
	}
	x.mustPoll()
	if s := x.settlementOf(order, "release"); s.State != "relayed" || x.fake.countOf("transfer") != 1 {
		t.Fatalf("release on mainnet: %+v, %d transfers", s, x.fake.countOf("transfer"))
	}
	if x.notified(x.vendor, "was sent to the mainnet network") != 1 {
		t.Fatal("the vendor was not told the release was sent to the mainnet network")
	}
}

// A-460: which daemon a market's class accepts, at startup. With MONERO_NETWORK=mainnet a test-network daemon is
// refused with the mainnet market's fixed refusal; on a test-network market a mainnet daemon is refused as before; a
// retired Bitcoin configuration is refused before any node call; and a database
// pinned to mainnet never starts as a test-network market. The fee address must be a mainnet one.
func TestXMREscrowMainnetStartup(t *testing.T) {
	x := newXMREscrowEnvWith(t, onMainnet)
	start := func() error {
		a, err := New(context.Background(), false)
		if a != nil {
			a.Close()
		}
		return err
	}
	for _, net := range []string{"stagenet", "fakechain", "testnet"} {
		x.fake.edit(func(f *fakeXMREscrow) { f.nettype = net })
		if err := start(); !errors.Is(err, errXMRStartDaemon) || !strings.Contains(err.Error(), errXMRDaemonNotMainnet.Error()) {
			t.Fatalf("%s daemon on a mainnet market: %v", net, err)
		}
	}
	x.fake.edit(onMainnet)
	t.Setenv("ESCROW_XMR_FEE_BPS", "250")
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", testXMRAddrOn("stagenet", "mn fee", false))
	if err := start(); err == nil || !strings.Contains(err.Error(), "ESCROW_XMR_FEE_ADDRESS") {
		t.Fatalf("stagenet fee address on a mainnet market: %v", err)
	}
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", testXMRAddrOn("mainnet", "mn fee", true))
	if err := start(); err != nil {
		t.Fatalf("mainnet fee address on a mainnet market: %v", err)
	}
	t.Setenv("ESCROW_XMR_FEE_BPS", "")
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", "")

	// BITCOIN_CHAIN can no longer choose a market's network class.
	t.Setenv("MONERO_NETWORK", "")
	t.Setenv("BITCOIN_CHAIN", "main")
	bitcoinCalls := x.fake.count()
	if err := start(); err == nil || !strings.Contains(err.Error(), "Bitcoin is removed") || x.fake.count() != bitcoinCalls {
		t.Fatalf("obsolete Bitcoin network setting was not refused before RPC: %v (%d calls)", err, x.fake.count()-bitcoinCalls)
	}
	// A test-network market: a database holding Monero escrow state pinned to mainnet is refused before any node is
	// asked (checkNetworkClass; a pin alone, with no escrow state, may still be rewritten, so a wallet row is held).
	t.Setenv("BITCOIN_CHAIN", "")
	agExec(x.testEnv, "INSERT INTO xmr_escrow_wallets(name,month,state) VALUES('escrow-202610-01','2026-10-01','creating')")
	calls := x.fake.count()
	if err := start(); err == nil || !strings.Contains(err.Error(), "holds mainnet escrows") || x.fake.count() != calls {
		t.Fatalf("a mainnet-pinned database on a test-network market: %v (%d calls)", err, x.fake.count()-calls)
	}

	// A fresh test-network market never accepts a mainnet daemon.
	y := newXMREscrowEnv(t)
	y.fake.edit(onMainnet)
	t.Setenv("MONERO_NETWORK", "")
	if err := start(); !errors.Is(err, errXMRStartDaemon) || !strings.Contains(err.Error(), errXMRDaemonNetwork.Error()) {
		t.Fatalf("mainnet daemon on a test-network market: %v", err)
	}
	if got := y.networkPin(); len(got) != 1 || got[0] != "fakechain" {
		t.Fatalf("a refused mainnet daemon changed the pin: %v", got)
	}
}

// A-460: the pin's rules hold for mainnet. A database with Monero escrow state pinned to a test network refuses a
// mainnet daemon and writes nothing; one pinned to mainnet refuses a test network the same way; with no state the pin
// follows the daemon.
func TestXMREscrowNetworkPinMainnet(t *testing.T) {
	x := newXMREscrowEnv(t)
	ctx := context.Background()
	// No state yet (the watcher has not run): the pin follows the daemon, mainnet included, and back.
	for _, net := range []string{"mainnet", "fakechain"} {
		if pinned, ok, err := pinXMRNetwork(ctx, x.DB, net); err != nil || !ok || pinned != net {
			t.Fatalf("no state, %s daemon: %q %v %v", net, pinned, ok, err)
		}
	}
	x.mustPoll() // a fakechain wallet: Monero escrow state
	if pinned, ok, err := pinXMRNetwork(ctx, x.DB, "mainnet"); err != nil || ok || pinned != "fakechain" {
		t.Fatalf("fakechain state, mainnet daemon: %q %v %v", pinned, ok, err)
	}
	agExec(x.testEnv, "UPDATE settings SET value='stagenet' WHERE key='escrow_xmr_network'")
	if pinned, ok, err := pinXMRNetwork(ctx, x.DB, "mainnet"); err != nil || ok || pinned != "stagenet" {
		t.Fatalf("stagenet state, mainnet daemon: %q %v %v", pinned, ok, err)
	}
	// Unpinned state (from before A-449) is backfilled on the fakechain only, never on mainnet.
	agExec(x.testEnv, "DELETE FROM settings WHERE key='escrow_xmr_network'")
	if pinned, ok, err := pinXMRNetwork(ctx, x.DB, "mainnet"); err != nil || ok || pinned != "" {
		t.Fatalf("unpinned state, mainnet daemon: %q %v %v", pinned, ok, err)
	}
	if got := x.networkPin(); len(got) != 0 {
		t.Fatalf("a refusal wrote the pin: %v", got)
	}
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('escrow_xmr_network','mainnet')")
	for _, net := range []string{"fakechain", "stagenet"} {
		if pinned, ok, err := pinXMRNetwork(ctx, x.DB, net); err != nil || ok || pinned != "mainnet" {
			t.Fatalf("mainnet state, %s daemon: %q %v %v", net, pinned, ok, err)
		}
	}
	if pinned, ok, err := pinXMRNetwork(ctx, x.DB, "mainnet"); err != nil || !ok || pinned != "mainnet" {
		t.Fatalf("mainnet state, mainnet daemon: %q %v %v", pinned, ok, err)
	}
}
