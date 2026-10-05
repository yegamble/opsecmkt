package market

import (
	"context"
	"html"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A-439 (P4-R3): the pooled Bitcoin check applies the escrow watcher's not-synced rule. Core's initialblockdownload
// latches false, so a node on a public chain that lost its peers, or whose tip stopped moving, reported "synced" and
// the pooled watcher expired orders whose deposits it could not see. Simulated RPC, not chain evidence.
func TestBitcoinPooledGuardPeersAndTipAge(t *testing.T) {
	ctx := context.Background()
	core, s := newPayCore(t, "testnet4")
	setPaymentEnv(t, s.url(""), "", "", "", "")
	t.Setenv("BITCOIN_MAX_TIP_AGE", "")
	a := newPaymentsApp()
	if err := a.initPayments(ctx); err != nil || a.provider("BTC") == nil {
		t.Fatalf("startup: %v", err)
	}
	logs := captureLog(t)
	if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 {
		t.Fatalf("a synced testnet4 node skipped: %v", skip)
	}
	for _, tc := range []struct {
		set  func(c *payCore)
		why  string
		undo func(c *payCore)
	}{
		{func(c *payCore) { c.peers = 0 }, notSyncedPeers, func(c *payCore) { c.peers = 8 }},
		{func(c *payCore) { c.tipAge = 3 * time.Hour }, notSyncedTip, func(c *payCore) { c.tipAge = 0 }},
		{func(c *payCore) { c.headersAhead = 2 }, notSyncedHeaders, func(c *payCore) { c.headersAhead = 0 }},
	} {
		core.set(tc.set)
		want := syncSkipReason(tc.why)
		if skip, _, _ := a.refreshProviders(ctx); skip["BTC"] != want || a.provider("BTC") == nil {
			t.Fatalf("%s: skip %q, want %q", tc.why, skip["BTC"], want)
		}
		a.refreshProviders(ctx)
		if line := "payments: BTC node not synced (" + tc.why + "); watcher passes skipped until it is"; strings.Count(logs.String(), line) != 1 {
			t.Fatalf("%s: log\n%s", tc.why, logs.String())
		}
		core.set(tc.undo)
		if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 {
			t.Fatalf("%s: synced again but skipped: %v", tc.why, skip)
		}
	}
	if n := strings.Count(logs.String(), "payments: BTC node synced; watcher passes resumed"); n != 3 {
		t.Fatalf("%d resumed lines:\n%s", n, logs.String())
	}
	// BITCOIN_MAX_TIP_AGE moves the bound.
	t.Setenv("BITCOIN_MAX_TIP_AGE", "4h")
	core.set(func(c *payCore) { c.tipAge = 3 * time.Hour })
	if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 {
		t.Fatalf("a 3h-old tip under BITCOIN_MAX_TIP_AGE=4h: %v", skip)
	}
	// IBD keeps today's reason text.
	core.set(func(c *payCore) { c.tipAge, c.ibd = 0, true })
	if skip, _, _ := a.refreshProviders(ctx); skip["BTC"] != syncingReason {
		t.Fatalf("ibd: %v", skip)
	}
}

// A failed getnetworkinfo fails no pooled check (the version is only shown), but on a public chain the peer count is
// then unknown and the node counts as not synced; on regtest nothing changes.
func TestBitcoinPooledGuardPeerCountUnread(t *testing.T) {
	ctx := context.Background()
	for chain, want := range map[string]string{"testnet4": notSyncedPeersUnread, "regtest": ""} {
		_, ov, s := newGapCore(t, chain)
		p, err := buildBitcoinProvider(s.url(""), "opsecmkt", "", 1)
		if err != nil {
			t.Fatal(err)
		}
		ov.fail("getnetworkinfo", -32601, "Method not found")
		st, err := p.Check(ctx)
		if err != nil || st.Why != want || st.Syncing != (want != "") {
			t.Fatalf("%s: %+v %v", chain, st, err)
		}
	}
}

// Regtest keeps today's pooled guard: a lone regtest node with an old tip is synced.
func TestBitcoinPooledGuardRegtestExempt(t *testing.T) {
	ctx := context.Background()
	core, s := newPayCore(t, "regtest")
	core.newAddr = "bcrt1qorderaddress0000000000000000000"
	setPaymentEnv(t, s.url(""), "regtest", "", "", "")
	a := newPaymentsApp()
	if err := a.initPayments(ctx); err != nil {
		t.Fatal(err)
	}
	core.set(func(c *payCore) { c.peers, c.tipAge = 0, 72*time.Hour })
	if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 {
		t.Fatalf("a lone regtest node skipped: %v", skip)
	}
}

// The pooled pass end to end on a public chain: with no peers an unpaid order past its window is not cancelled, the
// admin page names the reason under Payment providers and the pass is not reported as an error; once the node has
// peers again the next pass cancels it.
func TestBitcoinPooledNoPeersHoldsExpiry(t *testing.T) {
	core, node := newPayCore(t, "testnet4")
	setPaymentEnv(t, node.url(""), "", "", "", "")
	t.Setenv("BITCOIN_MAX_TIP_AGE", "")
	e := newTestApp(t)
	b, bs := e.user("syncbuyer", "buyer")
	v, _ := e.user("syncvendor", "vendor")
	_, as := e.user("syncadmin", "admin")
	order := e.order(b, e.product(v, "digital"), "BTC", stateDraft)
	if w := e.do("POST", "/orders/pay", bs, url.Values{"order_id": {order}}); w.Code != 303 {
		t.Fatalf("pay: %d %s", w.Code, w.Body.String())
	}
	if _, err := e.DB.Exec("UPDATE payment_addresses SET created=now()-interval '48 hours' WHERE order_id=$1", order); err != nil {
		t.Fatal(err)
	}
	core.set(func(c *payCore) { c.peers = 0 })
	if err := e.A.pollOnce(context.Background()); err != nil {
		t.Fatalf("a not-synced pass is not an error: %v", err)
	}
	state := func() string {
		var s string
		e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&s)
		return s
	}
	if s := state(); s != stateAwaitingPayment {
		t.Fatalf("an order expired while the node had no peers: %s", s)
	}
	w := e.do("GET", "/admin", as, nil)
	e.check(w, 200)
	if page := html.UnescapeString(w.Body.String()); !strings.Contains(page, syncSkipReason(notSyncedPeers)) {
		t.Fatalf("admin page lacks the reason:\n%s", page)
	}
	core.set(func(c *payCore) { c.peers = 8 })
	if err := e.A.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := state(); s != stateCancelled {
		t.Fatalf("expired order after a synced pass: %s", s)
	}
}
