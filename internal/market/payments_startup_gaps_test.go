package market

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The production startup path: environment -> New() -> initPayments -> registered providers -> Start ->
// background watcher pass, against scripted Bitcoin Core and monero-wallet-rpc/monerod nodes. Plus the
// generic startup guards (duplicate currency, non-test network) that no adapter test reaches.

// withProviderFactory registers an extra provider factory for one test (after the Bitcoin and Monero ones).
func withProviderFactory(t *testing.T, f providerFactory) {
	saved := providerFactories
	providerFactories = append(slices.Clip(saved), f)
	t.Cleanup(func() { providerFactories = saved })
}

// newAppFromEnv runs New() exactly as cmd/server does, against a fresh schema.
func newAppFromEnv(t *testing.T) (*App, error) {
	t.Setenv("DATABASE_URL", testSchemaDSN(t))
	t.Setenv("SETUP_TOKEN", testSetupToken)
	t.Setenv("COOKIE_SECURE", "false")
	a, err := New(context.Background(), false)
	if err == nil {
		t.Cleanup(a.Close)
	}
	return a, err
}

// gapNetProvider is a fake wallet reporting an arbitrary network name (it keeps the fake's Check).
type gapNetProvider struct {
	*fakeProvider
	net string
}

func (g *gapNetProvider) Network() string { return g.net }

// gapNoCheck hides Check: only the generic network guard stands between it and the site.
type gapNoCheck struct{ PaymentProvider }

func TestGapStartupFromEnvironmentRunsTheWatcher(t *testing.T) {
	core, _, btc := newGapCore(t, "test")                 // testnet3, as Bitcoin Core names it
	core.set(func(c *payCore) { c.walletLoaded = false }) // bitcoind restarted without load_on_startup: New() loads it
	w, _, xmr := newGapMonero(t)
	w.set(func(m *payMoneroWallet) {
		m.open = false // monero-wallet-rpc started with --wallet-dir: New() opens the conventional wallet
		m.valid[payStagenetSub] = "stagenet"
	})
	setPaymentEnv(t, btc.url(""), "", xmr.url(""), xmr.url(""), "stagenet")
	t.Setenv("PAYMENT_CONFIRMATIONS_BTC", "2")
	t.Setenv("PAYMENT_CONFIRMATIONS_XMR", "3")
	t.Setenv("PAYMENT_POLL_INTERVAL", "1s")
	e := newTestApp(t)

	bp, ok := e.A.provider("BTC").(*bitcoinProvider)
	if !ok || bp.Network() != "testnet3" || bp.Confirmations() != 2 || btc.called("loadwallet") != 1 {
		t.Fatalf("BTC provider from the environment: %T %v", e.A.provider("BTC"), e.A.provider("BTC"))
	}
	xp, ok := e.A.provider("XMR").(*moneroProvider)
	if !ok || xp.Network() != "stagenet" || xp.Confirmations() != 3 || xmr.called("open_wallet") != 1 || xmr.called("get_info") != 1 {
		t.Fatalf("XMR provider from the environment: %T", e.A.provider("XMR"))
	}
	suffix := randomToken()[:6]
	vendorID, _ := e.user("svendor_"+suffix, "vendor")
	_, buyer := e.user("sbuyer_"+suffix, "buyer")
	_, admin := e.user("sadmin_"+suffix, "admin")
	product := e.product(vendorID, "physical")
	get := func(path, sess string) string {
		t.Helper()
		w := e.do("GET", path, sess, nil)
		e.check(w, 200)
		return w.Body.String()
	}
	if body := get("/", buyer); !strings.Contains(body, "TESTNET payments (BTC testnet3, XMR stagenet) — no real funds") {
		t.Fatal("catalog does not show the configured test networks")
	}
	pay := func(currency string) (order, addr, provider string) {
		t.Helper()
		w := e.do("POST", "/orders", buyer, url.Values{"product_id": {product}, "currency": {currency}})
		e.check(w, 303)
		order = strings.TrimPrefix(w.Header().Get("Location"), "/order?id=")
		e.check(e.do("POST", "/orders/pay", buyer, url.Values{"order_id": {order}}), 303)
		if err := e.DB.QueryRow("SELECT address,provider FROM payment_addresses WHERE order_id=$1", order).Scan(&addr, &provider); err != nil {
			t.Fatal(err)
		}
		return
	}
	btcOrder, btcAddr, btcProv := pay("BTC")
	xmrOrder, xmrAddr, xmrProv := pay("XMR")
	if btcAddr != core.newAddr || btcProv != "btc-testnet3" || xmrAddr != payStagenetSub || xmrProv != "xmr-stagenet" {
		t.Fatalf("issued addresses: %s %s / %s %s", btcAddr, btcProv, xmrAddr, xmrProv)
	}
	if got := string(btc.lastParams("getnewaddress")); got != `["order:`+btcOrder+`","bech32"]` {
		t.Fatalf("getnewaddress params %s", got)
	}
	if body := get("/order?id="+btcOrder, buyer); !strings.Contains(body, "TESTNET testnet3 deposit address") || !strings.Contains(body, btcAddr) {
		t.Fatal("BTC order page does not show the testnet3 deposit address")
	}
	// Deposits at exactly each threshold, then the background watcher (Start) does the rest.
	core.gapReceive(btcAddr, "tx-startup-btc", 1, 100000, 2)
	w.set(func(m *payMoneroWallet) {
		m.transfers = map[string]any{"in": []map[string]any{{"txid": "tx-startup-xmr", "amount": 500000000000, "confirmations": 3, "subaddr_index": map[string]any{"major": 0, "minor": 1}}}}
	})
	e.A.Start(context.Background())
	deadline := time.Now().Add(15 * time.Second)
	state := func(id string) string {
		var s string
		if err := e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for state(btcOrder) != statePaid || state(xmrOrder) != statePaid {
		if time.Now().After(deadline) {
			t.Fatalf("background watcher did not settle: BTC %s XMR %s", state(btcOrder), state(xmrOrder))
		}
		time.Sleep(100 * time.Millisecond)
	}
	for order, want := range map[string]string{
		btcOrder: "TESTNET testnet3 payment confirmed: 0.001 BTC in 1 output(s) with at least 2 confirmations",
		xmrOrder: "TESTNET stagenet payment confirmed: 0.5 XMR in 1 output(s) with at least 3 confirmations",
	} {
		var note string
		var actor sql.NullString
		if err := e.DB.QueryRow("SELECT note,actor_id FROM order_events WHERE order_id=$1 AND to_state='paid'", order).Scan(&note, &actor); err != nil || note != want || actor.Valid {
			t.Fatalf("paid event %q actor=%v err=%v", note, actor, err)
		}
	}
	var n int
	if err := e.DB.QueryRow("SELECT count(*) FROM payments WHERE (order_id=$1 AND txid='tx-startup-btc' AND idx=1 AND amount=100000) OR (order_id=$2 AND txid='tx-startup-xmr' AND idx=1 AND amount=500000000000)", btcOrder, xmrOrder).Scan(&n); err != nil || n != 2 {
		t.Fatalf("ledger rows %d %v", n, err)
	}
	status := map[string]string{}
	rows, err := e.DB.Query("SELECT currency,network,last_error FROM payment_status WHERE last_poll IS NOT NULL")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cur, network, last string
		rows.Scan(&cur, &network, &last)
		status[cur] = network + "|" + last
	}
	rows.Close()
	if status["BTC"] != "testnet3|" || status["XMR"] != "stagenet|" {
		t.Fatalf("payment_status %v", status)
	}
	if body := get("/admin", admin); !strings.Contains(body, "TESTNET testnet3") || !strings.Contains(body, "TESTNET stagenet") || strings.Contains(body, "Not polled yet") {
		t.Fatal("admin page does not show both providers as polled")
	}
	// Every pass re-checks the node (the startup check plus at least one pass).
	if btc.called("getblockchaininfo") < 2 || xmr.called("get_info") < 2 {
		t.Fatalf("watcher did not re-check the nodes: getblockchaininfo=%d get_info=%d", btc.called("getblockchaininfo"), xmr.called("get_info"))
	}
}

func TestGapStartupRefusesDuplicateCurrency(t *testing.T) {
	_, _, btc := newGapCore(t, "testnet4")
	fake := func(cur string) providerFactory {
		return func(context.Context) (PaymentProvider, error) { return newFakeProvider(cur, 1), nil }
	}
	t.Run("beside a working node", func(t *testing.T) {
		setPaymentEnv(t, btc.url(""), "", "", "", "")
		withProviderFactory(t, fake("BTC"))
		if _, err := newAppFromEnv(t); err == nil || err.Error() != "two payment providers configured for BTC" {
			t.Fatalf("second BTC provider accepted: %v", err)
		}
	})
	t.Run("beside an unavailable node", func(t *testing.T) {
		setPaymentEnv(t, btc.url(""), "", "", "", "")
		btc.down.Store(true)
		defer btc.down.Store(false)
		withProviderFactory(t, fake("BTC"))
		if _, err := newAppFromEnv(t); err == nil || err.Error() != "two payment providers configured for BTC" {
			t.Fatalf("second BTC provider accepted beside an unavailable one: %v", err)
		}
	})
	t.Run("two registered providers", func(t *testing.T) {
		setPaymentEnv(t, "", "", "", "", "")
		withProviderFactory(t, fake("XMR"))
		withProviderFactory(t, fake("XMR"))
		if _, err := newAppFromEnv(t); err == nil || err.Error() != "two payment providers configured for XMR" {
			t.Fatalf("second XMR provider accepted: %v", err)
		}
	})
	t.Run("one of each currency", func(t *testing.T) {
		setPaymentEnv(t, btc.url(""), "", "", "", "")
		withProviderFactory(t, fake("XMR"))
		a, err := newAppFromEnv(t)
		if err != nil || a.provider("BTC") == nil || a.provider("XMR") == nil {
			t.Fatalf("BTC node plus XMR provider: %v", err)
		}
	})
	t.Run("factory error", func(t *testing.T) {
		setPaymentEnv(t, "", "", "", "", "")
		withProviderFactory(t, func(context.Context) (PaymentProvider, error) { return nil, errors.New("XMR configuration invalid") })
		if _, err := newAppFromEnv(t); err == nil || err.Error() != "XMR configuration invalid" {
			t.Fatalf("factory error ignored: %v", err)
		}
	})
}

func TestGapStartupGenericNetworkGuard(t *testing.T) {
	setPaymentEnv(t, "", "", "", "", "")
	for _, net := range []string{"main", "mainnet", "MainNet", ""} {
		for _, hide := range []bool{false, true} {
			t.Run(strconv.Quote(net)+map[bool]string{true: " without Check"}[hide], func(t *testing.T) {
				var p PaymentProvider = &gapNetProvider{fakeProvider: newFakeProvider("XMR", 1), net: net}
				if hide {
					p = gapNoCheck{p}
				}
				withProviderFactory(t, func(context.Context) (PaymentProvider, error) { return p, nil })
				want := `refusing to start: XMR provider network "` + net + `" is not a test network`
				if _, err := newAppFromEnv(t); err == nil || err.Error() != want {
					t.Fatalf("got %v, want %s", err, want)
				}
			})
		}
	}
	// A provider that passes at startup and later reports mainnet is disabled by the running site.
	fake := newFakeProvider("XMR", 1)
	p := &gapNetProvider{fakeProvider: fake, net: "stagenet"}
	withProviderFactory(t, func(context.Context) (PaymentProvider, error) { return p, nil })
	t.Setenv("DATABASE_URL", testSchemaDSN(t))
	t.Setenv("SETUP_TOKEN", testSetupToken)
	t.Setenv("COOKIE_SECURE", "false")
	e := &testEnv{t: t}
	e.restart()
	t.Cleanup(func() { e.A.Close() })
	if e.A.provider("XMR") != p {
		t.Fatal("stagenet provider not registered")
	}
	b, bs := e.user("gbuyer", "buyer")
	v, _ := e.user("gvendor", "vendor")
	_, as := e.user("gadmin", "admin")
	draft := e.order(b, e.product(v, "physical"), "XMR", stateDraft)
	if err := e.A.pollOnce(context.Background()); err != nil {
		t.Fatalf("pass on stagenet: %v", err)
	}
	p.net = "mainnet"
	if err := e.A.pollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), `XMR provider network "mainnet" is not a test network`) {
		t.Fatalf("mainnet after startup: %v", err)
	}
	msg, disabled := e.A.unavailableError("XMR")
	if e.A.provider("XMR") != nil || !disabled || !strings.Contains(msg, "Disabled until the node is fixed") {
		t.Fatalf("provider not disabled: %q %v", msg, disabled)
	}
	checks := fake.checks
	if err := e.A.pollOnce(context.Background()); err == nil || fake.checks != checks {
		t.Fatalf("disabled provider checked again (%d -> %d) or its reason dropped: %v", checks, fake.checks, err)
	}
	w := e.do("GET", "/admin", as, nil)
	e.check(w, 200)
	if body := w.Body.String(); !strings.Contains(body, "Refused — not a test network") || !strings.Contains(body, `XMR provider network &#34;mainnet&#34; is not a test network`) {
		t.Fatal("admin page does not show the refusal")
	}
	w = e.do("POST", "/orders/pay", bs, url.Values{"order_id": {draft}})
	e.check(w, 409)
	if !strings.Contains(w.Body.String(), "Payment unavailable for XMR") {
		t.Fatalf("pay after refusal: %s", w.Body.String())
	}
	var state string
	if err := e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", draft).Scan(&state); err != nil || state != stateDraft {
		t.Fatalf("refused pay changed the order: %s %v", state, err)
	}
}
