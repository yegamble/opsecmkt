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

// A-449 (P4-R15): Monero escrow on stagenet. The address tags follow the network the escrow daemon was verified on
// (stagenet 24 and 36, fakechain 18 and 42), the daemon's network is pinned in settings (escrow_xmr_network) instead
// of the old height heuristic, and a stateless sync guard skips the Monero escrow pass and the pooled Monero pass while
// the daemon is not synced. DB integration tests against the simulated wallet-rpc and monerod (escrow_xmr_fake_test.go,
// payments_monero_test.go): simulated RPC, not chain evidence.

// onStagenet makes the fake a synced stagenet daemon with peers and a fresh tip.
func onStagenet(f *fakeXMREscrow) { f.nettype, f.peers = "stagenet", 8 }

func (x *xmrEscrowEnv) adminPage() string {
	x.t.Helper()
	_, as := x.user("xnetadmin_"+randomToken()[:6], "admin")
	return html.UnescapeString(x.page("/admin", as))
}

// xmrAdminLines reads, from an /admin page, the alert under Payment providers about the Monero escrow daemon ("" when
// there is none), the XMR line of the escrow-only readiness panel and the Payment providers paragraph (QA4W4B-1).
func xmrAdminLines(t *testing.T, page string) (alert, readiness, providers string) {
	t.Helper()
	cut := func(from, to string) string {
		i := strings.Index(page, from)
		if i < 0 {
			return ""
		}
		j := strings.Index(page[i:], to)
		if j < 0 {
			t.Fatalf("no %q after %q", to, from)
		}
		return page[i : i+j]
	}
	alert, readiness, providers = cut(`id="xmr-escrow-node"`, "</div>"), cut("<li>XMR escrow: ", "</li>"), cut("Deposits of pooled orders", "</p>")
	if readiness == "" || providers == "" {
		t.Fatalf("/admin lacks the readiness line or the Payment providers paragraph:\n%s", page)
	}
	return alert, readiness, providers
}

// adminOf is /admin, unescaped, as a new administrator of a's market.
func adminOf(t *testing.T, a *App) string {
	t.Helper()
	e := &testEnv{t: t, A: a, DB: a.db}
	_, sess := e.user("xwaitadmin_"+randomToken()[:6], "admin")
	w := e.do("GET", "/admin", sess, nil)
	if w.Code != 200 {
		t.Fatalf("GET /admin: %d", w.Code)
	}
	return html.UnescapeString(w.Body.String())
}

func (x *xmrEscrowEnv) networkPin() []string {
	return agStrings(x.t, x.testEnv, "SELECT value FROM settings WHERE key='escrow_xmr_network'")
}

// On stagenet the escrow accepts stagenet addresses (5…, 7…) and refuses every other network's, an integrated address
// included, for refund and claim addresses; its wallets' addresses are stagenet ones; the pin is stagenet; pages, notes
// and notices name stagenet, never the fakechain.
func TestXMREscrowStagenetTags(t *testing.T) {
	x := newXMREscrowEnvWith(t, onStagenet)
	if m := x.A.escrowXMR; m == nil || !m.ready.Load() {
		t.Fatal("Monero escrow is not ready on a stagenet daemon")
	}
	if got := x.networkPin(); len(got) != 1 || got[0] != "stagenet" {
		t.Fatalf("network pin %v", got)
	}
	x.mustPoll()
	if got := agStrings(t, x.testEnv, "SELECT substr(primary_address,1,1) FROM xmr_escrow_wallets"); len(got) != 1 || got[0] != "5" {
		t.Fatalf("wallet primary addresses %v", got)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE state='free' AND address NOT LIKE '7%'"); n != 0 {
		t.Fatalf("%d free slots with a non-stagenet address", n)
	}
	order := x.draft(x.product)
	key, view := testXMRKey("tags/s"), testXMRKey("tags/v")
	for name, addr := range map[string]string{
		"fakechain standard":   testXMRAddrOn("fakechain", "tags refund", false),
		"fakechain subaddress": testXMRAddrOn("fakechain", "tags refund", true),
		"testnet subaddress":   testXMRAddrOn("testnet", "tags refund", true),
		"stagenet integrated":  testXMRAddressWith(25, key, view, testXMRKey("pid")[:8], false),
	} {
		w := x.checkout(x.buyerSess, order, addr)
		body := html.UnescapeString(w.Body.String())
		if w.Code != 400 || x.state(order) != stateDraft {
			t.Fatalf("%s refund on stagenet: %d %s", name, w.Code, body)
		}
		if !strings.Contains(name, "integrated") && !strings.Contains(body, "start with 5 or 7") {
			t.Fatalf("%s refund: the refusal does not name stagenet's prefixes:\n%s", name, body)
		}
	}
	addr := x.mustCheckoutWith(order, testXMRAddrOn("stagenet", "tags refund", true))
	if addr[0] != '7' {
		t.Fatalf("escrow address %s…", addr[:8])
	}
	// The standard form is accepted too.
	buyer, sess := x.user("xb_"+randomToken()[:8], "buyer")
	other := x.order(buyer, x.digital(x.vendor), "XMR", stateDraft)
	if w := x.checkout(sess, other, testXMRAddrOn("stagenet", "tags refund standard", false)); w.Code != 303 {
		t.Fatalf("stagenet standard refund: %d %s", w.Code, w.Body.String())
	}
	page := html.UnescapeString(x.page(orderPage(order), x.buyerSess))
	if !strings.Contains(page, "STAGENET") || strings.Contains(strings.ToLower(page), "fakechain") {
		t.Fatalf("the order page does not name stagenet only:\n%s", page)
	}
	if home := html.UnescapeString(x.page("/", x.buyerSess)); !strings.Contains(home, "Monero escrow only (Monero stagenet)") || strings.Contains(strings.ToLower(home), "fakechain") {
		t.Fatalf("the site-wide notice does not name stagenet only:\n%s", home)
	}
	x.fake.payAddress(addr, hex64("tags deposit"), xmrListingPrice, 10)
	x.mustPoll()
	if x.state(order) != stateCompleted {
		t.Fatalf("funded order on stagenet: %s", x.state(order))
	}
	if notes := strings.Join(agStrings(t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1", order), "\n"); !strings.Contains(notes, "TESTNET stagenet escrow funded") || strings.Contains(notes, "fakechain") {
		t.Fatalf("order notes:\n%s", notes)
	}
	// Claim addresses follow the same table: a fakechain subaddress is refused, a stagenet one is built and relayed.
	if code, body := x.xmrClaim(order, x.vendorSess, testXMRAddrOn("fakechain", "tags claim", true), testPassword); code != 400 || !strings.Contains(html.UnescapeString(body), "start with 5 or 7") {
		t.Fatalf("fakechain claim address on stagenet: %d %s", code, body)
	}
	claim := testXMRAddrOn("stagenet", "tags claim", true)
	if code, body := x.xmrClaim(order, x.vendorSess, claim, testPassword); code != 303 {
		t.Fatalf("stagenet claim: %d %s", code, body)
	}
	x.mustPoll()
	if s := x.settlementOf(order, "release"); s.State != "relayed" || x.fake.countOf("transfer") != 1 {
		t.Fatalf("release on stagenet: %+v, %d transfers", s, x.fake.countOf("transfer"))
	}
	if x.notified(x.vendor, "was sent to the stagenet network") != 1 {
		t.Fatal("the vendor was not told the release was sent to the stagenet network")
	}
}

// mustCheckoutWith takes a Monero escrow for the buyer's draft order with refund and returns its address.
func (x *xmrEscrowEnv) mustCheckoutWith(order, refund string) string {
	x.t.Helper()
	if w := x.checkout(x.buyerSess, order, refund); w.Code != 303 {
		x.t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
	}
	return agStrings(x.t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1 AND coin='XMR'", order)[0]
}

// On the fakechain nothing changes: 4… and 8… are accepted, stagenet's 5… and 7… refused naming the fakechain's
// prefixes; the pin is fakechain.
func TestXMREscrowFakechainTags(t *testing.T) {
	x := newXMREscrowEnv(t)
	if got := x.networkPin(); len(got) != 1 || got[0] != "fakechain" {
		t.Fatalf("network pin %v", got)
	}
	x.mustPoll()
	order := x.draft(x.product)
	for _, sub := range []bool{false, true} {
		w := x.checkout(x.buyerSess, order, testXMRAddrOn("stagenet", "fc refund", sub))
		if body := html.UnescapeString(w.Body.String()); w.Code != 400 || !strings.Contains(body, "start with 4 or 8") {
			t.Fatalf("stagenet refund on the fakechain: %d %s", w.Code, body)
		}
	}
	if addr := x.mustCheckoutWith(order, testXMRAddrOn("fakechain", "fc refund", false)); addr[0] != '8' {
		t.Fatalf("escrow address %s…", addr[:8])
	}
}

// ESCROW_XMR_FEE_ADDRESS must be an address of the verified network: a fakechain fee address with a stagenet daemon,
// or a stagenet one with a fakechain daemon, stops startup with a fixed refusal that quotes nothing.
func TestXMREscrowFeeAddressFollowsNetwork(t *testing.T) {
	x := newXMREscrowEnv(t)
	t.Setenv("ESCROW_XMR_FEE_BPS", "250")
	for _, c := range []struct {
		daemon, fee string
		ok          bool
	}{{"fakechain", "stagenet", false}, {"stagenet", "fakechain", false}, {"stagenet", "stagenet", true}, {"fakechain", "fakechain", true}} {
		x.fake.edit(func(f *fakeXMREscrow) { f.nettype, f.peers = c.daemon, 8 })
		fee := testXMRAddrOn(c.fee, "market fee", false)
		t.Setenv("ESCROW_XMR_FEE_ADDRESS", fee)
		agExec(x.testEnv, "DELETE FROM settings WHERE key='escrow_xmr_network'")
		a, err := New(context.Background(), false)
		if a != nil {
			a.Close()
		}
		if c.ok != (err == nil) {
			t.Fatalf("%s daemon, %s fee address: %v", c.daemon, c.fee, err)
		}
		if err != nil && (!strings.Contains(err.Error(), "ESCROW_XMR_FEE_ADDRESS") || strings.Contains(err.Error(), fee[:12])) {
			t.Fatalf("%s daemon, %s fee address: refusal %v", c.daemon, c.fee, err)
		}
	}
}

// P4-R15: the escrow daemon's network is pinned (escrow_xmr_network). A database holding Monero escrow state refuses a
// daemon on another network: Monero escrow stays disabled with one fixed log line, the admin page names both values,
// and nothing is written; retries keep it disabled without another line; back on the pinned network it starts.
func TestXMREscrowNetworkPinMismatch(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.product)
	x.mustCheckout(order)
	if got := x.networkPin(); len(got) != 1 || got[0] != "fakechain" {
		t.Fatalf("pin %v", got)
	}
	settings := agStrings(t, x.testEnv, "SELECT key||'='||value FROM settings WHERE key<>'installed' ORDER BY key")
	state := func() string {
		return strings.Join(agStrings(t, x.testEnv, "SELECT order_id||' '||address FROM escrows ORDER BY order_id"), ",") + "|" +
			strings.Join(x.wallets(), ",") + "|" + strings.Join(agStrings(t, x.testEnv, "SELECT address||' '||state FROM xmr_escrow_slots ORDER BY id"), ",")
	}
	before := state()
	x.fake.edit(onStagenet)
	logs := captureLog(t)
	calls := x.fake.count()
	x.restart()
	const line = "escrow: Monero escrow disabled: the escrow daemon is not on the network this database's Monero escrows are on (escrow_xmr_network); the admin page names both; see docs/operator-guide.md, \"Monero escrow\""
	m := x.A.escrowXMR
	if m == nil || m.ready.Load() || m.startState != line {
		t.Fatalf("a stagenet daemon on a fakechain database: configured %v, state %q", m != nil, m.startState)
	}
	if n := strings.Count(logs.String(), line); n != 1 || strings.Contains(logs.String(), "Monero escrow on") {
		t.Fatalf("log:\n%s", logs.String())
	}
	if page := x.adminPage(); !strings.Contains(page, "Monero escrow disabled: the escrow daemon reports network stagenet, but this database's Monero escrows are on fakechain.") {
		t.Fatalf("admin page lacks the mismatch line:\n%s", page)
	}
	for range 2 {
		x.mustPoll()
	}
	retry := &escrowRetry{last: m.startState}
	x.A.retryEscrowXMR(context.Background(), retry)
	x.A.retryEscrowXMR(context.Background(), retry)
	if m.ready.Load() || strings.Count(logs.String(), line) != 1 {
		t.Fatalf("retries: ready %v\n%s", m.ready.Load(), logs.String())
	}
	// Nothing but the daemon is asked: no wallet of one network is refreshed against a daemon of the other.
	for _, c := range x.fake.callLog()[calls:] {
		if c.Method != "get_info" && c.Method != "hard_fork_info" {
			t.Fatalf("a refused start called %s", c.Method)
		}
	}
	after := agStrings(t, x.testEnv, "SELECT key||'='||value FROM settings WHERE key<>'installed' ORDER BY key")
	if strings.Join(settings, ",") != strings.Join(after, ",") || state() != before {
		t.Fatalf("a refused start changed the database: %v -> %v", settings, after)
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.nettype = "fakechain" })
	x.A.retryEscrowXMR(context.Background(), retry)
	if !m.ready.Load() {
		t.Fatal("Monero escrow did not start once the daemon was back on the pinned network")
	}
	if page := x.adminPage(); strings.Contains(page, "Monero escrow disabled: the escrow daemon reports network") {
		t.Fatal("the mismatch line outlived the mismatch")
	}
}

// The pin's rules: a database with Monero escrow state (an escrow or an escrow wallet) and no pin (one from before
// A-449) is pinned only when the daemon reports fakechain, the only network Monero escrow ran on before; on stagenet it
// is refused and nothing is written. With no such state the pin follows the daemon, and may be rewritten.
func TestXMREscrowNetworkPinBackfillAndRepin(t *testing.T) {
	// No state yet (the watcher has not run): the pin follows the daemon.
	x := newXMREscrowEnv(t)
	if got := x.networkPin(); len(got) != 1 || got[0] != "fakechain" {
		t.Fatalf("first start: %v", got)
	}
	x.fake.edit(onStagenet)
	x.restart()
	if got := x.networkPin(); !x.A.escrowXMR.ready.Load() || len(got) != 1 || got[0] != "stagenet" {
		t.Fatalf("re-pin with no Monero escrow state: ready %v, pin %v", x.A.escrowXMR.ready.Load(), got)
	}
	// A wallet is state: its slots' addresses are of the pinned network.
	x.mustPoll()
	x.fake.edit(func(f *fakeXMREscrow) { f.nettype = "fakechain" })
	x.restart()
	if got := x.networkPin(); x.A.escrowXMR.ready.Load() || len(got) != 1 || got[0] != "stagenet" {
		t.Fatalf("a stagenet wallet, fakechain daemon: ready %v, pin %v", x.A.escrowXMR.ready.Load(), got)
	}

	// Escrows and no pin: backfilled on the fakechain only.
	y := newXMREscrowEnv(t)
	y.mustPoll()
	y.mustCheckout(y.draft(y.product))
	agExec(y.testEnv, "DELETE FROM settings WHERE key='escrow_xmr_network'")
	y.fake.edit(onStagenet)
	y.restart()
	if got := y.networkPin(); y.A.escrowXMR.ready.Load() || len(got) != 0 {
		t.Fatalf("unpinned escrows, stagenet daemon: ready %v, pin %v", y.A.escrowXMR.ready.Load(), got)
	}
	if page := y.adminPage(); !strings.Contains(page, "Monero escrow disabled: the escrow daemon reports network stagenet, but this database's Monero escrows are on a network it did not record.") {
		t.Fatalf("admin page:\n%s", page)
	}
	y.fake.edit(func(f *fakeXMREscrow) { f.nettype = "fakechain" })
	y.restart()
	if got := y.networkPin(); !y.A.escrowXMR.ready.Load() || len(got) != 1 || got[0] != "fakechain" {
		t.Fatalf("unpinned escrows, fakechain daemon: ready %v, pin %v", y.A.escrowXMR.ready.Load(), got)
	}
}

// The old height heuristic is gone: a fakechain or stagenet daemon and wallet past height 1,000,000 pass the network
// check (the pin, not the height, tells networks apart); the wallet must still be within 10 blocks of the daemon.
func TestXMREscrowNoHeightHeuristic(t *testing.T) {
	ctx := context.Background()
	for _, nettype := range []string{"fakechain", "stagenet"} {
		wallet, daemon, r := happyXMRNodes(t)
		daemon.result("get_info", map[string]any{"nettype": nettype, "stagenet": nettype == "stagenet", "height": 1_900_000})
		wallet.result("get_height", map[string]any{"height": 1_900_000})
		if _, _, err := testXMRCheckNetwork(ctx, r); err != nil {
			t.Fatalf("%s at height 1,900,000: %v", nettype, err)
		}
		wallet.result("get_height", map[string]any{"height": 1_899_989})
		if _, _, err := testXMRCheckNetwork(ctx, r); !errors.Is(err, errXMRWalletLag) {
			t.Fatalf("%s, wallet 11 blocks behind: %v", nettype, err)
		}
	}
}

// While the escrow daemon is not synced (synchronized false, or target_height above height; on stagenet also no peers or
// a tip older than MONERO_MAX_TIP_AGE) the Monero escrow pass is skipped: no wallet is opened, no deposit read, no order
// funded or expired, no settlement built or relayed; one log line names the reason class and the admin page says so;
// nothing is stored for the guard. The next synced pass does the work, even at a lower height than the unsynced daemon
// reported (no high-water mark, A-147).
func TestXMREscrowSkipsWhileNotSynced(t *testing.T) {
	for _, tc := range []struct {
		name, nettype, why string
		set, undo          func(f *fakeXMREscrow)
	}{
		{"unsynchronized", "fakechain", "daemon not synchronized", func(f *fakeXMREscrow) { f.unsynced = true }, func(f *fakeXMREscrow) { f.unsynced = false }},
		{"target above height", "fakechain", "target height above height", func(f *fakeXMREscrow) { f.targetAhead = 40 }, func(f *fakeXMREscrow) { f.targetAhead = 0 }},
		{"no peers on stagenet", "stagenet", "no peers", func(f *fakeXMREscrow) { f.peers = 0 }, func(f *fakeXMREscrow) { f.peers = 8 }},
		{"stale tip on stagenet", "stagenet", "tip older than MONERO_MAX_TIP_AGE", func(f *fakeXMREscrow) { f.tipAge = 2 * time.Hour }, func(f *fakeXMREscrow) { f.tipAge = 0 }},
		// A restricted RPC hides the connection counts (0): offline and the tip's age decide there.
		{"offline on a restricted stagenet RPC", "stagenet", "no peers", func(f *fakeXMREscrow) { f.restricted, f.offline = true, true }, func(f *fakeXMREscrow) { f.offline = false }},
		{"stale tip on a restricted stagenet RPC", "stagenet", "tip older than MONERO_MAX_TIP_AGE", func(f *fakeXMREscrow) { f.restricted, f.tipAge = true, 2*time.Hour }, func(f *fakeXMREscrow) { f.tipAge = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MONERO_MAX_TIP_AGE", "")
			x := newXMREscrowEnvWith(t, func(f *fakeXMREscrow) { f.nettype, f.peers = tc.nettype, 8 })
			x.mustPoll()
			// A completed order with a release requested (built by the watcher), an unpaid order past its window
			// (cancelled by the watcher) and an order paid at 10 confirmations (funded by the watcher).
			done, _, _ := x.completed()
			agExec(x.testEnv, "INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'release',$2)", done, testXMRAddrOn(tc.nettype, "sync claim", true))
			unpaid, _ := x.freshCheckoutOn(tc.nettype)
			agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", unpaid)
			paid, paidAddr := x.freshCheckoutOn(tc.nettype)
			settings := agStrings(t, x.testEnv, "SELECT key||'='||value FROM settings WHERE key<>'installed' ORDER BY key")
			logs := captureLog(t)
			x.fake.edit(func(f *fakeXMREscrow) {
				tc.set(f)
				f.height += 30 // the unsynced daemon reports a higher tip than the synced one below
			})
			x.fake.payAddress(paidAddr, hex64("sync paid "+paid), xmrListingPrice, 10)
			calls := x.fake.count()

			x.mustPoll()
			x.mustPoll()
			if s := x.state(unpaid); s != stateAwaitingPayment {
				t.Fatalf("an order expired while the daemon was not synced: %s", s)
			}
			if s := x.state(paid); s != stateAwaitingPayment {
				t.Fatalf("an order was funded while the daemon was not synced: %s", s)
			}
			if s := x.settlementOf(done, "release"); s.State != "requested" {
				t.Fatalf("a release was built while the daemon was not synced: %+v", s)
			}
			for _, c := range x.fake.callLog()[calls:] {
				if c.Method != "get_info" && c.Method != "get_last_block_header" {
					t.Fatalf("the pass called %s while the daemon was not synced", c.Method)
				}
			}
			line := "escrow: Monero escrow daemon not synced (" + tc.why + "); Monero escrow watcher passes skipped until it is"
			if got := logs.String(); strings.Count(got, line) != 1 || strings.Contains(got, "monero escrow watcher:") {
				t.Fatalf("log while not synced:\n%s", got)
			}
			if page := x.adminPage(); !strings.Contains(page, "Monero escrow daemon not synced: "+tc.why+".") {
				t.Fatalf("admin page lacks the not-synced line:\n%s", page)
			}
			if got := agStrings(t, x.testEnv, "SELECT key||'='||value FROM settings WHERE key<>'installed' ORDER BY key"); strings.Join(got, ",") != strings.Join(settings, ",") {
				t.Fatalf("settings changed by the guard: %v -> %v", settings, got)
			}

			x.fake.edit(func(f *fakeXMREscrow) {
				tc.undo(f)
				f.height -= 30
			})
			x.mustPoll()
			if s := x.state(unpaid); s != stateCancelled {
				t.Fatalf("the expired order after a synced pass: %s", s)
			}
			if s := x.state(paid); s != stateCompleted {
				t.Fatalf("the paid order after a synced pass: %s", s)
			}
			if s := x.settlementOf(done, "release"); s.State != "relayed" {
				t.Fatalf("the release after a synced pass: %+v", s)
			}
			if got := logs.String(); strings.Count(got, "escrow: Monero escrow daemon synced; Monero escrow watcher passes resumed") != 1 || strings.Count(got, line) != 1 {
				t.Fatalf("log after the synced pass:\n%s", got)
			}
			if page := x.adminPage(); strings.Contains(page, "Monero escrow daemon not synced") {
				t.Fatal("the admin page still says the daemon is not synced")
			}
		})
	}
}

// freshCheckoutOn is freshCheckout with a refund address of nettype.
func (x *xmrEscrowEnv) freshCheckoutOn(nettype string) (string, string) {
	x.t.Helper()
	buyer, sess := x.user("xb_"+randomToken()[:8], "buyer")
	order := x.order(buyer, x.digital(x.vendor), "XMR", stateDraft)
	if w := x.checkout(sess, order, testXMRAddrOn(nettype, "refund "+order, true)); w.Code != 303 {
		x.t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
	}
	return order, agStrings(x.t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1 AND coin='XMR'", order)[0]
}

// The fakechain is exempt from the peer and tip-age checks: a lone fakechain daemon with no peers and a tip two days
// old is synced, and get_last_block_header is never called on it.
func TestXMREscrowFakechainExemptFromPeerAndTipChecks(t *testing.T) {
	x := newXMREscrowEnvWith(t, func(f *fakeXMREscrow) { f.peers, f.tipAge = 0, 48*time.Hour })
	x.mustPoll()
	unpaid := x.draft(x.product)
	x.mustCheckout(unpaid)
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", unpaid)
	logs := captureLog(t)
	x.mustPoll()
	if s := x.state(unpaid); s != stateCancelled {
		t.Fatalf("a lone fakechain daemon held the pass: %s", s)
	}
	if n := x.fake.countOf("get_last_block_header"); n != 0 {
		t.Fatalf("get_last_block_header called %d times on the fakechain", n)
	}
	if strings.Contains(logs.String(), "not synced") {
		t.Fatalf("log: %s", logs.String())
	}
}

// QA4W3-3: on stagenet a top block get_last_block_header cannot read is not taken as fresh: the Monero escrow pass is
// skipped as for a daemon that cannot be checked (an error naming the daemon check, no wallet call, nothing funded or
// expired), and the next pass that reads the tip does the work.
func TestXMREscrowSkipsWhileTipUnreadable(t *testing.T) {
	t.Setenv("MONERO_MAX_TIP_AGE", "")
	x := newXMREscrowEnvWith(t, onStagenet)
	x.mustPoll()
	unpaid, _ := x.freshCheckoutOn("stagenet")
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", unpaid)
	paid, paidAddr := x.freshCheckoutOn("stagenet")
	x.fake.edit(func(f *fakeXMREscrow) { f.tipErr = true })
	x.fake.payAddress(paidAddr, hex64("tip unreadable "+paid), xmrListingPrice, 10)
	from := x.fake.count()
	if err := x.poll(); err == nil || !strings.Contains(watcherCause(err), "monero escrow: daemon check failed: ") || !strings.Contains(watcherCause(err), "get_last_block_header") {
		t.Fatalf("a pass with the tip unreadable: %v (logged as %q)", err, errorCauseOrNone(err))
	}
	if x.state(unpaid) != stateAwaitingPayment || x.state(paid) != stateAwaitingPayment {
		t.Fatalf("orders moved while the tip was unreadable: %s %s", x.state(unpaid), x.state(paid))
	}
	for _, c := range x.fake.callLog()[from:] {
		if c.Method != "get_info" && c.Method != "get_last_block_header" {
			t.Fatalf("the pass called %s while the tip was unreadable", c.Method)
		}
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.tipErr = false })
	x.mustPoll()
	if x.state(unpaid) != stateCancelled || x.state(paid) != stateCompleted {
		t.Fatalf("after a pass that read the tip: %s %s", x.state(unpaid), x.state(paid))
	}
}

// The pooled Monero check applies the same rule (A-439 found pooled Monero read only target_height): on stagenet no
// peers, a stale tip, a tip get_last_block_header cannot read, or synchronized false count as not synced, each logged once and named on the admin page; target
// above height keeps the "still syncing" reason; MONERO_MAX_TIP_AGE moves the bound.
func TestMoneroPooledGuardPeersAndTipAge(t *testing.T) {
	ctx := context.Background()
	w, s := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	setPaymentEnv(t, "", "", s.url(""), s.url(""), "stagenet")
	t.Setenv("MONERO_MAX_TIP_AGE", "")
	a := newPaymentsApp()
	if err := a.initPayments(ctx); err != nil || a.provider("XMR") == nil {
		t.Fatalf("startup: %v", err)
	}
	logs := captureLog(t)
	if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 {
		t.Fatalf("a synced stagenet daemon skipped: %v", skip)
	}
	for _, tc := range []struct {
		set, undo func(m *payMoneroWallet)
		why       string
	}{
		{func(m *payMoneroWallet) { m.peers = 0 }, func(m *payMoneroWallet) { m.peers = 8 }, "no peers"},
		{func(m *payMoneroWallet) { m.tipAge = 2 * time.Hour }, func(m *payMoneroWallet) { m.tipAge = 0 }, "tip older than MONERO_MAX_TIP_AGE"},
		{func(m *payMoneroWallet) { m.unsynced = true }, func(m *payMoneroWallet) { m.unsynced = false }, "daemon not synchronized"},
		// QA4W3-3: a tip that cannot be read is not taken as fresh.
		{func(m *payMoneroWallet) { m.tipErr = true }, func(m *payMoneroWallet) { m.tipErr = false }, "tip time unreadable"},
	} {
		w.set(tc.set)
		want := syncSkipReason(tc.why, "")
		if skip, _, _ := a.refreshProviders(ctx); skip["XMR"] != want || a.provider("XMR") == nil || !strings.Contains(want, tc.why) {
			t.Fatalf("%s: skip %q, want %q", tc.why, skip["XMR"], want)
		}
		a.refreshProviders(ctx)
		if line := "payments: XMR node not synced (" + tc.why + "); watcher passes skipped until it is"; strings.Count(logs.String(), line) != 1 {
			t.Fatalf("%s: log\n%s", tc.why, logs.String())
		}
		w.set(tc.undo)
		if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 {
			t.Fatalf("%s: synced again but skipped: %v", tc.why, skip)
		}
	}
	if n := strings.Count(logs.String(), "payments: XMR node synced; watcher passes resumed"); n != 4 {
		t.Fatalf("%d resumed lines:\n%s", n, logs.String())
	}
	// A restricted RPC (the bundled monero service) reports 0 connections whatever its peers: with a fresh tip it is
	// synced; offline or a stale tip still count.
	w.set(func(m *payMoneroWallet) { m.restricted = true })
	if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 {
		t.Fatalf("a restricted RPC with a fresh tip: %v", skip)
	}
	w.set(func(m *payMoneroWallet) { m.offline = true })
	if skip, _, _ := a.refreshProviders(ctx); skip["XMR"] != syncSkipReason("no peers", "") {
		t.Fatalf("an offline restricted RPC: %v", skip)
	}
	w.set(func(m *payMoneroWallet) { m.offline, m.tipAge = false, 2*time.Hour })
	if skip, _, _ := a.refreshProviders(ctx); skip["XMR"] != syncSkipReason("tip older than MONERO_MAX_TIP_AGE", "") {
		t.Fatalf("a restricted RPC with a stale tip: %v", skip)
	}
	w.set(func(m *payMoneroWallet) { m.restricted, m.tipAge = false, 0 })
	a.refreshProviders(ctx)
	t.Setenv("MONERO_MAX_TIP_AGE", "3h")
	w.set(func(m *payMoneroWallet) { m.tipAge = 2 * time.Hour })
	if skip, _, _ := a.refreshProviders(ctx); len(skip) != 0 {
		t.Fatalf("a 2h-old tip under MONERO_MAX_TIP_AGE=3h: %v", skip)
	}
	w.set(func(m *payMoneroWallet) { m.tipAge, m.height, m.target = 0, 100, 500 })
	if skip, _, _ := a.refreshProviders(ctx); skip["XMR"] != syncingReason {
		t.Fatalf("target above height: %v", skip)
	}
}

// The pooled Monero pass end to end: with no peers an unpaid order past its window is not cancelled, the admin page
// names the reason and the pass is no error; once the daemon has peers again the next pass cancels it.
func TestMoneroPooledNoPeersHoldsExpiry(t *testing.T) {
	w, s := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	w.valid[payStagenetSub] = "stagenet"
	setPaymentEnv(t, "", "", s.url(""), s.url(""), "stagenet")
	t.Setenv("MONERO_MAX_TIP_AGE", "")
	e := newTestApp(t)
	b, bs := e.user("xsyncbuyer", "buyer")
	v, _ := e.user("xsyncvendor", "vendor")
	_, as := e.user("xsyncadmin", "admin")
	order := e.order(b, e.product(v, "digital"), "XMR", stateDraft)
	if r := e.do("POST", "/orders/pay", bs, url.Values{"order_id": {order}}); r.Code != 303 {
		t.Fatalf("pay: %d %s", r.Code, r.Body.String())
	}
	if _, err := e.DB.Exec("UPDATE payment_addresses SET created=now()-interval '48 hours' WHERE order_id=$1", order); err != nil {
		t.Fatal(err)
	}
	w.set(func(m *payMoneroWallet) { m.peers = 0 })
	if err := e.A.pollOnce(context.Background()); err != nil {
		t.Fatalf("a not-synced pass is not an error: %v", err)
	}
	state := func() string {
		var st string
		e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&st)
		return st
	}
	if st := state(); st != stateAwaitingPayment {
		t.Fatalf("an order expired while the daemon had no peers: %s", st)
	}
	r := e.do("GET", "/admin", as, nil)
	e.check(r, 200)
	if page := html.UnescapeString(r.Body.String()); !strings.Contains(page, syncSkipReason("no peers", "")) {
		t.Fatalf("admin page lacks the reason:\n%s", page)
	}
	w.set(func(m *payMoneroWallet) { m.peers = 8 })
	if err := e.A.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := state(); st != stateCancelled {
		t.Fatalf("expired order after a synced pass: %s", st)
	}
}

// While the guard holds an expired order open (its payment window has passed, no deposit waits for confirmations, the
// daemon is not synced), its page says the order stays open while the market waits for its Monero node, never that it
// will be cancelled at a time already past (the Bitcoin page's QA4W1-5, for Monero).
func TestXMREscrowWindowPassedWhileNotSynced(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.product)
	x.mustCheckout(order)
	before := html.UnescapeString(x.page(orderPage(order), x.buyerSess))
	if !strings.Contains(before, "If nothing has been received by ") || strings.Contains(before, "The payment window ended at") {
		t.Fatalf("an order inside its window:\n%s", before)
	}
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", order)
	x.fake.edit(func(f *fakeXMREscrow) { f.unsynced = true })
	x.mustPoll()
	if s := x.state(order); s != stateAwaitingPayment {
		t.Fatalf("the guard did not hold the order: %s", s)
	}
	page := html.UnescapeString(x.page(orderPage(order), x.buyerSess))
	want := "but the order stays open for now: the market is waiting for its Monero node before it checks this escrow again and acts on the payment window."
	if !strings.Contains(page, "The payment window ended at ") || !strings.Contains(page, want) || strings.Contains(page, "If nothing has been received by ") {
		t.Fatalf("an expired order held open:\n%s", page)
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.unsynced = false })
	x.mustPoll()
	if s := x.state(order); s != stateCancelled {
		t.Fatalf("after a synced pass: %s", s)
	}
}

// QA4W5-5: the watcher's retry of Monero escrow's startup checks records the state it reached, so /admin follows it:
// started with the escrow daemon unchecked (get_info past startup's budget) and the pooled wallet-rpc down, then
// retried once the daemon answers, the alert says Monero escrow waits for the pooled wallet's first check, no longer
// that the daemon could not be checked.
func TestXMREscrowRetryRecordsState(t *testing.T) {
	ctx := context.Background()
	fake := newFakeXMREscrow(t)
	fake.edit(onStagenet)
	fake.hook("get_info", func() { time.Sleep(time.Second) })
	_, pub := testSeedKey(t, nil)
	fake.setEnv(t, writeSeedKey(t, pub))
	_, pooled := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	t.Setenv("MONERO_WALLET_RPC_URL", pooled.url(""))
	t.Setenv("MONERO_RPC_URL", pooled.url(""))
	t.Setenv("MONERO_NETWORK", "stagenet")
	t.Setenv("MONERO_MAX_TIP_AGE", "")
	pooled.down.Store(true)
	saved := paymentStartupTimeout
	paymentStartupTimeout = 300 * time.Millisecond
	a, _, err := startApp(t, "")
	paymentStartupTimeout = saved
	if err != nil || a.escrowXMR == nil || a.escrowXMR.ready.Load() || a.escrowXMR.startState != xmrLogUnchecked {
		t.Fatalf("startup with the escrow daemon unchecked: %v %q", err, a.escrowXMR.startState)
	}
	t.Cleanup(a.Close)
	unchecked := "the escrow daemon or wallet-rpc could not be checked"
	if alert, _, _ := xmrAdminLines(t, adminOf(t, a)); !strings.Contains(alert, "Monero escrow not ready: "+unchecked) {
		t.Fatalf("/admin with the daemon unchecked: alert %q", alert)
	}
	fake.hook("get_info", nil)
	retry := &escrowRetry{last: a.escrowXMR.startState}
	a.retryEscrowXMR(ctx, retry)
	if a.escrowXMR.ready.Load() || retry.last != xmrLogPooledWait {
		t.Fatalf("retry with the daemon answering and the pooled wallet-rpc down: ready %v, state %q", a.escrowXMR.ready.Load(), retry.last)
	}
	waiting := "waiting for the pooled Monero wallet's first check"
	if alert, readiness, _ := xmrAdminLines(t, adminOf(t, a)); !strings.Contains(alert, "Monero escrow not ready: "+waiting) || strings.Contains(alert, unchecked) ||
		!strings.Contains(readiness, "(not ready: "+waiting+")") {
		t.Fatalf("/admin after the retry: alert %q, readiness %q", alert, readiness)
	}
}

// QA4W3-2: the wallet open in the escrow wallet-rpc is compared with the pooled Monero wallet's primary address
// (pooledXMRWalletOpen), which backs up sameRPCEndpoint now that both wallet-rpcs may follow one stagenet daemon. When
// the escrow wallet-rpc answers get_address with the pooled primary, startup is refused (errXMRStartWallet). When the
// pooled provider has not passed a check yet (its wallet-rpc down at startup), the comparison has nothing to compare
// with, so Monero escrow waits, making no escrow wallet call, instead of starting unchecked; once the pooled check
// passes, the next retry compares and refuses, and later retries stay refused (until a restart) whatever wallet the
// escrow wallet-rpc then has open.
func TestXMREscrowRefusesPooledWalletOpen(t *testing.T) {
	ctx := context.Background()
	fake := newFakeXMREscrow(t)
	pooledFile := &fakeXMRWalletFile{name: "opsecmkt", accounts: []string{payStagenetPrimary}, nettype: "stagenet"}
	fake.edit(func(f *fakeXMREscrow) {
		onStagenet(f)
		f.files[pooledFile.name], f.open = pooledFile, pooledFile
	})
	_, pub := testSeedKey(t, nil)
	fake.setEnv(t, writeSeedKey(t, pub))
	_, pooled := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	t.Setenv("MONERO_WALLET_RPC_URL", pooled.url(""))
	t.Setenv("MONERO_RPC_URL", pooled.url(""))
	t.Setenv("MONERO_NETWORK", "stagenet")
	t.Setenv("MONERO_MAX_TIP_AGE", "")
	a, dsn, err := startApp(t, "")
	if !errors.Is(err, errXMRStartWallet) || !strings.Contains(err.Error(), errXMRPooledWallet.msg) {
		t.Fatalf("startup with the pooled wallet open in the escrow wallet-rpc: %v", err)
	}
	if a != nil {
		a.Close()
	}

	// The pooled wallet-rpc down at startup: no primary address to compare with yet.
	pooled.down.Store(true)
	logs := captureLog(t)
	from := fake.count()
	a, _, err = startApp(t, dsn)
	if err != nil || a.escrowXMR == nil || a.escrowXMR.ready.Load() || a.escrowXMR.startState != xmrLogPooledWait || !strings.Contains(logs.String(), xmrLogPooledWait) {
		t.Fatalf("startup with the pooled provider unchecked: %v\n%s", err, logs.String())
	}
	for _, c := range fake.callLog()[from:] {
		if !slices.Contains([]string{"get_info", "get_last_block_header", "hard_fork_info"}, c.Method) {
			t.Fatalf("Monero escrow called %s while waiting for the pooled check", c.Method)
		}
	}
	// The administrator reads the waiting state, not a plain "on" (QA4W4B-1).
	alert, readiness, _ := xmrAdminLines(t, adminOf(t, a))
	waiting := "waiting for the pooled Monero wallet's first check"
	if !strings.Contains(alert, "Monero escrow not ready: "+waiting) || !strings.Contains(alert, "MONERO_WALLET_RPC_URL") ||
		!strings.Contains(readiness, "XMR escrow: on (not ready: "+waiting+")") {
		t.Fatalf("/admin while waiting for the pooled check: alert %q, readiness %q", alert, readiness)
	}
	pooled.down.Store(false)
	if a.refreshProviders(ctx); a.provider("XMR") == nil {
		t.Fatal("the pooled Monero provider did not come up")
	}
	retry := &escrowRetry{last: a.escrowXMR.startState}
	a.retryEscrowXMR(ctx, retry)
	refusedLine := xmrLogRefused + errXMRPooledWallet.msg
	if !retry.refused || a.escrowXMR.ready.Load() || strings.Count(logs.String(), refusedLine) != 1 {
		t.Fatalf("retry once the pooled check passed: refused %v, ready %v\n%s", retry.refused, a.escrowXMR.ready.Load(), logs.String())
	}
	alert, readiness, _ = xmrAdminLines(t, adminOf(t, a))
	if !strings.Contains(alert, "Monero escrow not ready: refused until the app is restarted") || !strings.Contains(alert, errXMRPooledWallet.msg) ||
		strings.Contains(alert, waiting) || !strings.Contains(readiness, "XMR escrow: on (not ready: refused until the app is restarted)") {
		t.Fatalf("/admin after the refusal: alert %q, readiness %q", alert, readiness)
	}
	// It sticks: with an escrow wallet open instead, a retry checks nothing and stays refused.
	fake.edit(func(f *fakeXMREscrow) {
		other := &fakeXMRWalletFile{name: "escrow-other", accounts: []string{testXMRAddrOn("stagenet", "escrow other", false)}, nettype: "stagenet"}
		f.files[other.name], f.open = other, other
	})
	from = fake.count()
	a.retryEscrowXMR(ctx, &escrowRetry{last: xmrLogUnchecked})
	if _, _, err := a.startEscrowXMR(ctx); !errors.Is(err, errXMRRefusedUntilRestart) || a.escrowXMR.ready.Load() || fake.count() != from || strings.Count(logs.String(), refusedLine) != 1 {
		t.Fatalf("after the refusal: %v, ready %v, %d calls\n%s", err, a.escrowXMR.ready.Load(), fake.count()-from, logs.String())
	}
}
