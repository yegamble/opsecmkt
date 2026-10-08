package market

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// A-445: the read-only drain panel on /admin (administrators only): every kind's count, never capped; the first
// readinessListLimit of each kind, each linked to where it is resolved; the pooled balances as advice, unavailable
// rather than an error page when a wallet does not answer; and the sweep note.

// drainPanel returns /admin's drain panel as session, from its heading to the next h3.
func drainPanel(t *testing.T, p *payEnv, session string) string {
	t.Helper()
	page := p.page("/admin", session)
	_, panel, ok := strings.Cut(page, `<h3 id="pooled-drain">`)
	if !ok {
		t.Fatal("no drain panel on /admin")
	}
	panel, _, _ = strings.Cut(panel, "<h3 ")
	return panel
}

// drainCount returns the count the panel shows for kind.
func drainCount(t *testing.T, panel, kind string) int {
	t.Helper()
	_, after, ok := strings.Cut(panel, `<li id="pooled-drain-`+kind+`">`)
	if !ok {
		t.Fatalf("kind %s missing from the panel", kind)
	}
	_, after, _ = strings.Cut(after, `<span class="pooled-drain-count">`)
	n, err := strconv.Atoi(after[:strings.Index(after, "<")])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPooledDrainPanel(t *testing.T) {
	p := newPayEnv(t)
	p.fake.balance, p.fake.pendingBalance = 150000000, 2000
	panel := drainPanel(t, p, p.adminSess)
	// payEnv's order awaits payment: one open pooled order, which this administrator may not read (no link).
	mustContain(t, panel, "<strong>1</strong> item still needs the pooled wallet.", "Open pooled orders: <span class=\"pooled-drain-count\">1</span>",
		`Order <span class="mono">`+p.order[:8]+`</span> (BTC, awaiting_payment)`,
		"The list decides, not the balance.", "advice only",
		"BTC pooled wallet: 1.5 BTC confirmed, 0.00002 BTC pending: unconfirmed, immature or locked (TESTNET fake; advice only).",
		"XMR pooled wallet: no pooled wallet is configured.",
		"The market never sweeps the pooled wallet. Once this list is empty, move any balance left in it yourself")
	mustNotContain(t, panel, `href="/order?id=`+p.order, "<form")
	for _, k := range drainKinds {
		want := 0
		if k.kind == "open_order" {
			want = 1
		}
		if n := drainCount(t, panel, k.kind); n != want {
			t.Fatalf("%s: %d, want %d", k.kind, n, want)
		}
	}

	// One of every other kind, each linked to where it is resolved.
	_, blockedID := p.blockedRelease("panel-paid") // its order is completed: its address is watched too
	paid := p.str("SELECT order_id FROM payouts WHERE id=$1::bigint", blockedID)
	queued := p.order // a pending payout has no resolve form: it links to the payouts table
	agExec(p.testEnv, "UPDATE orders SET state='completed' WHERE id=$1", queued)
	pendingID := p.str("INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state) VALUES($1,'refund',$2,'BTC',100000,'fake-testnet-buyer','pending') RETURNING id::text", queued, p.buyer.ID)
	p.fake.Deposit(p.addr, "panel-late", 0, 546, 3)
	agExec(p.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true')") // the watcher sends nothing now
	p.poll()
	flagged := p.str("SELECT id::text FROM payments WHERE txid='panel-late' AND flagged")
	p.A.recordAddressCollision(context.Background(), "BTC", "fake")
	main := p.order2("BTC", stateCompleted)
	agExec(p.testEnv, "UPDATE payment_addresses SET provider='btc-main',created=now()-interval '40 days' WHERE order_id=$1", main)
	agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '40 days' WHERE id=$1", main)
	panel = drainPanel(t, p, p.adminSess)
	mustContain(t, panel,
		`<a class="text-link" href="/admin#resolve-payout-`+blockedID+`">Payout `+blockedID+`</a>: release blocked (BTC), order <span class="mono">`+paid[:8]+`</span>`,
		`<a class="text-link" href="/admin#payouts">Payout `+pendingID+`</a>: refund pending (BTC)`,
		`<a class="text-link" href="/moderator#payment-review-`+flagged+`">Payment `+flagged+`</a>: deposit <span class="mono">panel-late:0</span>`,
		`<a class="text-link" href="/admin#payment-providers">Outbound payments paused</a>`,
		`<a class="text-link" href="/admin#payment-providers">BTC warning</a>: 1 payment request refused`,
		"BTC: 1 mainnet deposit address",
		// The flagged order is readable by staff: its watched address links to it.
		`<span class="mono">`+p.addr+`</span> (BTC), order <a class="text-link mono" href="/order?id=`+queued+`">`+queued[:8]+`</a>, watched until `)
	for kind, want := range map[string]int{"open_order": 0, "unsent_payout": 2, "flagged_payment": 1, "watched_address": 2, "recovery_required": 1, "address_collision": 1, "mainnet_history": 1} {
		if n := drainCount(t, panel, kind); n != want {
			t.Fatalf("%s: %d, want %d", kind, n, want)
		}
	}
	// The resolve form each payout link names is on the page.
	mustContain(t, p.page("/admin", p.adminSess), `id="resolve-payout-`+blockedID+`"`, `id="payouts"`, `id="payment-providers"`)

	// Counts are never capped; each list is.
	agExec(p.testEnv, `INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) SELECT 'drain-open-'||lpad(g::text,4,'0'),$1,$2,'XMR',100000,'paid' FROM generate_series(1,$3::int) g`,
		p.buyer.ID, p.productID, readinessListLimit+2)
	panel = drainPanel(t, p, p.adminSess)
	if n := drainCount(t, panel, "open_order"); n != readinessListLimit+2 {
		t.Fatalf("open orders %d", n)
	}
	_, orders, _ := strings.Cut(panel, `<li id="pooled-drain-open_order">`)
	orders, _, _ = strings.Cut(orders, `<li id="pooled-drain-unsent_payout">`)
	if n := strings.Count(orders, "<li>Order "); n != readinessListLimit {
		t.Fatalf("listed %d open orders, want %d", n, readinessListLimit)
	}
	mustContain(t, orders, "Showing the first "+strconv.Itoa(readinessListLimit)+".")
	total := readinessListLimit + 2 + 2 + 1 + 2 + 1 + 1 + 1
	mustContain(t, panel, "<strong>"+strconv.Itoa(total)+"</strong> items still need the pooled wallet.")
}

// order2 inserts another order of the payEnv's buyer and listing in state, with a pooled address.
func (p *payEnv) order2(cur, state string) string {
	p.t.Helper()
	id := p.testEnv.order(p.buyer.ID, p.productID, cur, state)
	agExec(p.testEnv, "INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,$2,$3,'fake')", id, cur, "fake-testnet-"+id[:12])
	return id
}

// A wallet that does not answer leaves the balance unavailable; the page is served. A provider configured but
// unavailable at startup reads the same.
func TestPooledDrainPanelBalanceUnavailable(t *testing.T) {
	p := newPayEnv(t)
	p.fake.balanceErr = errors.New("connection refused")
	panel := drainPanel(t, p, p.adminSess)
	mustContain(t, panel, "BTC pooled wallet: balance unavailable (its wallet did not answer the balance request).", "The list decides, not the balance.")
	mustNotContain(t, panel, "BTC confirmed")

	// A real Bitcoin adapter whose node is gone.
	core, srv := newPayCore(t, "regtest")
	core.newAddr = "bcrt1qorderaddress000000000000000000000"
	btc, err := newBitcoinProvider(context.Background(), srv.url(""), "opsecmkt", "regtest", 1)
	if err != nil {
		t.Fatal(err)
	}
	p.A.payMu.Lock()
	p.A.payments["BTC"] = btc
	p.A.payMu.Unlock()
	// A pooled user whose rpcwhitelist predates getbalances (HTTP 403), then a node that is down.
	srv.mu.Lock()
	srv.forbid = func(method string) bool { return method == "getbalances" }
	srv.mu.Unlock()
	mustContain(t, drainPanel(t, p, p.adminSess), "BTC pooled wallet: balance unavailable (its wallet did not answer the balance request).")
	srv.down.Store(true)
	mustContain(t, drainPanel(t, p, p.adminSess), "BTC pooled wallet: balance unavailable (its wallet did not answer the balance request).")
}

// The adapters read the balances as the wallets report them (Core 31.1's getbalances shape, recorded on regtest in
// artifacts/a445/core31-getbalances.json; monero-wallet-rpc get_balance in atomic units) and refuse a malformed one.
func TestPooledWalletBalanceAdapters(t *testing.T) {
	ctx := context.Background()
	// untrusted_pending (unconfirmed receipts) and immature (coinbase) both count as pending (QA445 Q17).
	answer := map[string]any{"mine": map[string]any{"trusted": json.Number("50.00000000"), "untrusted_pending": json.Number("0.00012345"), "immature": json.Number("5000.00000000")}}
	srv := newPayRPCServer(t, "pooled", "pooled-pass", false, func(path, method string, params json.RawMessage) (any, int, string) {
		switch method {
		case "getblockchaininfo":
			return map[string]any{"chain": "regtest", "blocks": 200, "headers": 200}, 0, ""
		case "getwalletinfo":
			return map[string]any{"walletname": "opsecmkt"}, 0, ""
		case "getnetworkinfo":
			return map[string]any{"version": 310100, "connections": 0}, 0, ""
		case "getbalances":
			if path != "/wallet/opsecmkt" {
				return nil, -18, "Requested wallet does not exist or is not loaded"
			}
			return answer, 0, ""
		}
		return nil, -32601, "Method not found"
	})
	btc, err := newBitcoinProvider(ctx, srv.url(""), "opsecmkt", "regtest", 1)
	if err != nil {
		t.Fatal(err)
	}
	if c, pend, err := btc.Balance(ctx); err != nil || c != 5000000000 || pend != 500000012345 {
		t.Fatalf("bitcoin balance %d %d %v", c, pend, err)
	}
	answer["mine"].(map[string]any)["trusted"] = json.Number("-1")
	if _, _, err := btc.Balance(ctx); err == nil {
		t.Fatal("a negative balance was read")
	}
	for in, want := range map[string]int64{"0": 0, "0.00000000": 0, "0.": 0, "00.0": 0, "1.5": 150000000} {
		if got, err := walletBalance(in, 8); err != nil || got != want {
			t.Errorf("walletBalance(%q) = %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"", ".", "0.0.0", "-0", "1e3", "0.000000001"} {
		if _, err := walletBalance(bad, 8); err == nil {
			t.Errorf("walletBalance(%q) accepted", bad)
		}
	}

	w, ms := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	xmr, err := newMoneroProvider(ctx, ms.url(""), "", "stagenet", 10)
	if err != nil {
		t.Fatal(err)
	}
	w.balance, w.unlocked = json.Number("18446744073709551"), json.Number("18446744073709000")
	if c, pend, err := xmr.Balance(ctx); err != nil || c != 18446744073709000 || pend != 551 {
		t.Fatalf("monero balance %d %d %v", c, pend, err)
	}
	if got := string(ms.lastParams("get_balance")); got != `{"account_index":0}` {
		t.Fatalf("get_balance params %s", got)
	}
	w.unlocked = json.Number("18446744073709552")
	if _, _, err := xmr.Balance(ctx); err == nil {
		t.Fatal("an unlocked balance above the balance was read")
	}
}

// Only an administrator sees the panel: other roles are refused /admin as before.
func TestPooledDrainPanelAdministratorOnly(t *testing.T) {
	p := newPayEnv(t)
	_, modSess := p.user("dmod_"+randomToken()[:6], "moderator")
	for _, s := range []string{p.buyerSess, p.vendorSess, modSess, ""} {
		w := p.do("GET", "/admin", s, nil)
		if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "pooled-drain") {
			t.Fatalf("session %q: %d", s, w.Code)
		}
	}
	// The loader itself never fills the panel for anyone else.
	d := &PageData{User: p.vendor, Settings: map[string]string{}}
	if err := pooledDrainLoader(context.Background(), p.A, httptest.NewRequest("GET", "/admin", nil), d); err != nil || d.PooledDrain != nil {
		t.Fatalf("vendor's page data: %v %+v", err, d.PooledDrain)
	}
}
