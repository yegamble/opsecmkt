package market

import (
	"context"
	"errors"
	"html"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// A-443 (P4-R5): Bitcoin escrow runs on regtest, signet and testnet4 (the chain table, escrowChains); mainnet and
// testnet3 stay refused. DB integration with the simulated Core (escrow_fake_test.go: simulated RPC, not chain
// evidence; scripts/test-escrow-regtest.sh runs the real legs on regtest and on a private signet).

// signetEscrowEnv is newEscrowEnv on a fake signet node (signet_challenge 51, 8 peers, a fresh tip), started the way
// escrow starts on one: the market is restarted against it while it holds no escrow, so the chain pin moves to signet.
func signetEscrowEnv(t *testing.T, env ...string) *escrowEnv {
	t.Helper()
	x := newEscrowEnv(t, env...)
	x.core.mu.Lock()
	x.core.chain, x.core.challenge, x.core.peers, x.core.tipTime = "signet", "51", 8, time.Now().Unix()
	x.core.mu.Unlock()
	x.restart()
	if e := x.A.escrow.Load(); e == nil || e.network != "signet" || e.chainID != "signet:51" {
		t.Fatal("escrow did not start on the fake signet node")
	}
	return x
}

// With ESCROW_BTC=on a node on mainnet or testnet3 stops startup with the fixed message, before any wallet call. A node
// on signet or testnet4 starts escrow like a regtest node: its chain is pinned (with signet_challenge on signet), the
// notes name it, and the watcher's pass runs with the sync guard's public-chain checks (getnetworkinfo is read).
func TestEscrowStartupChains(t *testing.T) {
	for _, chain := range []string{"main", "test"} {
		t.Run(chain, func(t *testing.T) {
			core, srv := newFakeEscrowCore(t)
			core.chain = chain
			setEscrowEnv(t, srv.url(""))
			_, _, err := startApp(t, "")
			if !errors.Is(err, errEscrowChain) {
				t.Fatalf("got %v", err)
			}
			if m := core.methods(); !slices.Equal(m, []string{"getblockchaininfo"}) {
				t.Fatalf("calls before the refusal: %v", m)
			}
		})
	}
	for _, tc := range []struct{ chain, challenge, pin string }{{"signet", "51", "signet:51"}, {"testnet4", "", "testnet4"}} {
		t.Run(tc.chain, func(t *testing.T) {
			core, srv := newFakeEscrowCore(t)
			core.chain, core.challenge, core.peers, core.tipTime = tc.chain, tc.challenge, 8, time.Now().Unix()
			setEscrowEnv(t, srv.url(""))
			logs := captureLog(t)
			a, dsn, err := startApp(t, "")
			if err != nil || a.escrow.Load() == nil {
				t.Fatalf("escrow on %s: %v\n%s", tc.chain, err, logs.String())
			}
			if a.escrowNetwork() != tc.chain || !strings.Contains(logs.String(), escrowLogOn) {
				t.Fatalf("network %q, log:\n%s", a.escrowNetwork(), logs.String())
			}
			var pin string
			if err := a.db.QueryRow("SELECT value FROM settings WHERE key=$1", escrowChainPin).Scan(&pin); err != nil || pin != tc.pin {
				t.Fatalf("pin %q %v", pin, err)
			}
			if err := a.escrowPollOnce(context.Background()); err != nil {
				t.Fatalf("watcher pass: %v", err)
			}
			if core.count("getnetworkinfo") == 0 {
				t.Fatal("the pass did not read getnetworkinfo on a public chain")
			}
			// Restarted while the node does not answer: escrow is disabled and its notes still name the pinned chain,
			// not regtest.
			srv.down.Store(true)
			a.Close()
			b, _, err := startApp(t, dsn)
			if err != nil || b.escrow.Load() != nil || b.escrowNetwork() != tc.chain {
				t.Fatalf("restarted with the node down: %v, network %q", err, b.escrowNetwork())
			}
			// Restarted with ESCROW_BTC off (escrow orders' pages are still shown): the pinned chain too.
			b.Close()
			t.Setenv("ESCROW_BTC", "off")
			c, _, err := startApp(t, dsn)
			if err != nil || c.escrowConfigured || c.escrowNetwork() != tc.chain {
				t.Fatalf("restarted with ESCROW_BTC off: %v, network %q", err, c.escrowNetwork())
			}
		})
	}
}

// BITCOIN_CHAIN, when set beside ESCROW_BTC=on, may name an escrow chain only: mainnet and testnet3 are refused before
// anything is contacted.
func TestEscrowConfigChains(t *testing.T) {
	for chain, ok := range map[string]bool{"": true, "regtest": true, "signet": true, "testnet4": true, "main": false, "test": false} {
		_, err := escrowConfigFromEnv(func(k string) string {
			return map[string]string{"ESCROW_BTC": "on", "ESCROW_BTC_RPC_URL": "http://a:b@h:1", "BITCOIN_CHAIN": chain}[k]
		})
		if (err == nil) != ok {
			t.Errorf("BITCOIN_CHAIN=%q: %v", chain, err)
		}
	}
}

// QA4W1-4: an address in capitals is lowered by the human-readable part of the escrow node's chain (the chain table),
// so a TB1 address on signet or testnet4 is stored in the one spelling the market compares and pays, as a BCRT1 one is
// on regtest. Another chain's prefix and a base58 address are left as given (the node then refuses or checks them).
func TestEscrowAddressFormChains(t *testing.T) {
	tb := segwitAddress("tb", 0, make([]byte, 20))
	bcrt := segwitAddress("bcrt", 0, make([]byte, 20))
	for _, tc := range []struct{ chain, in, want string }{
		{"regtest", strings.ToUpper(bcrt), bcrt},
		{"signet", strings.ToUpper(tb), tb},
		{"testnet4", strings.ToUpper(tb), tb},
		{"signet", " " + strings.ToUpper(tb) + " ", tb},
		{"regtest", strings.ToUpper(tb), strings.ToUpper(tb)},
		{"signet", strings.ToUpper(bcrt), strings.ToUpper(bcrt)},
		{"signet", "mipcBbFg9gMiCh81Kj8tqqdgoZub1ZJRfn", "mipcBbFg9gMiCh81Kj8tqqdgoZub1ZJRfn"},
	} {
		if got, ok := escrowAddressForm(tc.chain, tc.in); !ok || got != tc.want {
			t.Errorf("%s %q: %q %v, want %q", tc.chain, tc.in, got, ok, tc.want)
		}
	}
	for _, bad := range []string{"", "tb1q", strings.ToUpper(tb) + ")", "TB1Q" + strings.Repeat("Q", 90)} {
		if _, ok := escrowAddressForm("signet", bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

// QA4W1-4 end to end on signet: a buyer's refund address and a vendor's claim address in capitals (TB1…) are stored in
// lower case; the claim is built, signed, co-signed, broadcast and confirmed, and pays the lower-case address; a refund
// to the capitalised refund address is built for it too. The fee address an administrator gives in capitals, and
// ESCROW_BTC_FEE_ADDRESS in capitals at startup, are kept in lower case.
func TestEscrowUppercaseAddressOnSignet(t *testing.T) {
	x := signetEscrowEnv(t)
	refund, claimTo := x.netAddr("upper-refund"), x.netAddr("upper-claim")
	if !strings.HasPrefix(refund, "tb1") {
		t.Fatalf("not a signet address: %s", refund)
	}
	order, id := x.built(strings.ToUpper(refund), strings.ToUpper(claimTo), 100000)
	if got := agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order); strings.Join(got, ",") != refund {
		t.Fatalf("stored refund address %v, want %s", got, refund)
	}
	if got := agStrings(t, x.testEnv, "SELECT destination FROM settlements WHERE id=$1", id); strings.Join(got, ",") != claimTo {
		t.Fatalf("stored claim address %v, want %s", got, claimTo)
	}
	x.signAs(order, roleVendor, id)
	x.poll()
	s := x.settleByID(id)
	if s.State != "broadcast" || s.TxID == "" {
		t.Fatalf("claim after signing: %+v", s)
	}
	x.core.setTxConfs(s.TxID, 3, "")
	x.poll()
	if s = x.settleByID(id); s.State != "confirmed" {
		t.Fatalf("claim after 3 confirmations: %+v", s)
	}
	if got := agStrings(t, x.testEnv, "SELECT address FROM settlement_outputs WHERE settlement_id=$1 AND role='claimant'", id); strings.Join(got, ",") != claimTo {
		t.Fatalf("claim output %v, want %s", got, claimTo)
	}

	// A refund to a capitalised refund address: the buyer cancels a funded order awaiting payment.
	refund2 := x.netAddr("upper-refund-2")
	under := x.draft()
	x.mustCheckout(under, strings.ToUpper(refund2))
	x.fund(under, under, 3, 40000)
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {under}, "from": {stateAwaitingPayment}, "amount_seen": {"40000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	rid, kind, state, class := x.settlement(under)
	if kind != "refund" || state != "awaiting_signature" {
		t.Fatalf("refund %s %s %s", kind, state, class)
	}
	if got := agStrings(t, x.testEnv, "SELECT address FROM settlement_outputs WHERE settlement_id=$1", rid); strings.Join(got, ",") != refund2 {
		t.Fatalf("refund output %v, want %s", got, refund2)
	}

	// The administrator's fee address in capitals.
	_, admin := x.user("upperadmin", "admin")
	fee := x.netAddr("upper-fee")
	if w := x.do("POST", "/admin/escrow-fee", admin, url.Values{"fee_bps": {"100"}, "fee_address": {strings.ToUpper(fee)}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("fee: %d %s", w.Code, w.Body.String())
	}
	if got := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", escrowFeeAddrSetting); strings.Join(got, ",") != fee {
		t.Fatalf("stored fee address %v, want %s", got, fee)
	}

	// ESCROW_BTC_FEE_ADDRESS in capitals at startup.
	envFee := x.netAddr("upper-env-fee")
	t.Setenv("ESCROW_BTC_FEE_BPS", "100")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", strings.ToUpper(envFee))
	x.restart()
	if e := x.A.escrow.Load(); e == nil || e.feeAddress != envFee {
		t.Fatalf("configured fee address after a restart: %v", e)
	}
}

// A-443: on signet every page that describes Bitcoin escrow names signet, never regtest: the order panel's headline
// and badge, the checkout offer, the escrow key page, the transparency page, the vendor desk tile, the footer and the
// administrator's page.
func TestEscrowPagesNameSignet(t *testing.T) {
	x := signetEscrowEnv(t)
	order, _ := x.built(x.netAddr("pages-refund"), x.netAddr("pages-claim"), 100000)
	_, admin := x.user("pagesadmin", "admin")
	for _, p := range []struct{ name, path, sess, want string }{
		{"order panel", "/order?id=" + order, x.buyerSess, `<span class="badge testnet">SIGNET</span>`},
		{"order panel headline", "/order?id=" + order, x.buyerSess, "<strong>Signet only.</strong> Bitcoin escrow on this market runs on signet, a test network, and has not been verified on any public network"},
		{"checkout offer", "/order?id=" + x.order, x.buyerSess, "Or pay into a Bitcoin escrow (signet only)"},
		{"escrow key", "/escrow-key", x.buyerSess, "Bitcoin escrow · signet only"},
		{"transparency", "/canary", "", "Bitcoin escrow (signet only)"},
		{"transparency chain", "/canary", "", "It runs on signet, a test network with no real funds"},
		{"vendor desk", "/vendor-dashboard", x.vendorSess, "<strong>SIGNET</strong>"},
		{"administration", "/admin", admin, "Bitcoin escrow orders (signet only)"},
	} {
		page := html.UnescapeString(x.page(p.path, p.sess))
		if !strings.Contains(page, p.want) {
			t.Errorf("%s lacks %q:\n%s", p.name, p.want, page)
		}
		if i := strings.Index(strings.ToLower(page), "regtest"); i >= 0 {
			t.Errorf("%s names regtest: …%s…", p.name, page[max(0, i-200):min(len(page), i+100)])
		}
	}
}
