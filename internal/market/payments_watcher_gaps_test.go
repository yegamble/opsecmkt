package market

import (
	"context"
	"encoding/json"
	"html"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Watcher and payout behaviour through the real Bitcoin and Monero adapters (scripted nodes) against
// PostgreSQL: wallet send failures, RPC errors in the middle of a pass, the admin page, concurrency of the
// single-send guarantee and payout-address validation.

const gapVendorBTC = "tb1qvendorpayout000000000000000000000"

// gapBTC installs a real Bitcoin adapter (testnet4, confs) in the payEnv in place of the fake BTC wallet.
func gapBTC(t *testing.T, p *payEnv, confs int) (*payCore, *rpcOverride, *payRPCServer, *bitcoinProvider) {
	core, ov, s := newGapCore(t, "testnet4")
	bp, err := newBitcoinProvider(context.Background(), s.url(""), "opsecmkt", "testnet4", confs)
	if err != nil {
		t.Fatal(err)
	}
	p.A.payments["BTC"] = bp
	return core, ov, s, bp
}

// gapXMR installs a real Monero adapter (stagenet) whose wallet resolves the subaddresses registered in index.
type gapXMRWallet struct {
	w   *payMoneroWallet
	ov  *rpcOverride
	s   *payRPCServer
	mu  sync.Mutex
	idx map[string]int64
	in  []map[string]any
}

func gapXMR(t *testing.T, p *payEnv, confs int) (*gapXMRWallet, *moneroProvider) {
	w, ov, s := newGapMonero(t)
	g := &gapXMRWallet{w: w, ov: ov, s: s, idx: map[string]int64{}}
	g.reset()
	return g, g.provider(t, p, confs)
}

// reset clears every override and answers get_address_index from the registered subaddresses
// (WRONG_ADDRESS, -2, for any other address, as monero-wallet-rpc does).
func (g *gapXMRWallet) reset() {
	g.ov.clear()
	g.ov.answer("get_address_index", func(_, _ string, params json.RawMessage) (any, int, string) {
		var q struct{ Address string }
		json.Unmarshal(params, &q)
		g.mu.Lock()
		defer g.mu.Unlock()
		if i, ok := g.idx[q.Address]; ok {
			return map[string]any{"index": map[string]any{"major": 0, "minor": i}}, 0, ""
		}
		return nil, -2, "Address doesn't belong to the wallet"
	})
}

func (g *gapXMRWallet) subaddress(addr string, minor int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.idx[addr] = minor
}

// provider builds a fresh adapter (empty subaddress cache, as after an app restart) and installs it.
func (g *gapXMRWallet) provider(t *testing.T, p *payEnv, confs int) *moneroProvider {
	xp, err := newMoneroProvider(context.Background(), g.s.url(""), "", "stagenet", confs)
	if err != nil {
		t.Fatal(err)
	}
	p.A.payments["XMR"] = xp
	return xp
}

func (g *gapXMRWallet) deposit(addr, txid string, piconero int64, confs int) {
	g.mu.Lock()
	minor := g.idx[addr]
	g.in = append(g.in, map[string]any{"txid": txid, "amount": piconero, "confirmations": confs, "subaddr_index": map[string]any{"major": 0, "minor": minor}})
	in := append([]map[string]any(nil), g.in...)
	g.mu.Unlock()
	g.w.set(func(m *payMoneroWallet) { m.transfers = map[string]any{"in": in} })
}

// gapOrder inserts an order awaiting payment in currency with deposit address addr (as /orders/pay does).
func (p *payEnv) gapOrder(currency, addr string) string {
	p.t.Helper()
	id := p.testEnv.order(p.buyer.ID, p.productID, currency, stateAwaitingPayment)
	if _, err := p.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,$2,$3,'gap')", id, currency, addr); err != nil {
		p.t.Fatal(err)
	}
	return id
}

func (p *payEnv) complete(order string) {
	p.t.Helper()
	if s := p.state(order); s != statePaid {
		p.t.Fatalf("order %s is %s, not paid", order[:8], s)
	}
	p.move(order, statePaid, stateShipped, p.vendor)
	p.move(order, stateShipped, stateCompleted, p.buyer)
}

func (p *payEnv) str(query string, args ...any) string {
	p.t.Helper()
	var s string
	if err := p.DB.QueryRow(query, args...).Scan(&s); err != nil {
		p.t.Fatalf("%s: %v", query, err)
	}
	return s
}

// failedPayout asserts the order's payout was recorded failed with wallet error msg, never sent, and shown,
// and whether the failure was recorded as ambiguous (may have been broadcast) or a definite rejection.
func (p *payEnv) failedPayout(order, msg string, ambiguous bool) {
	p.t.Helper()
	st, txid, _, _ := p.payout(order)
	errText := p.str("SELECT error FROM payouts WHERE order_id=$1", order)
	recorded := p.str("SELECT send_ambiguous::text FROM payouts WHERE order_id=$1", order) == "true"
	wording := "The wallet reported a pre-broadcast error; nothing was broadcast."
	if ambiguous {
		wording = "may or may not have been broadcast"
	}
	if st != "failed" || txid != "" || !strings.Contains(errText, msg) || !strings.Contains(errText, wording) || recorded != ambiguous {
		p.t.Fatalf("payout for %s: state=%s txid=%q error=%q ambiguous=%v", order[:8], st, txid, errText, recorded)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%failed and will not be retried%'", order); n != 1 {
		p.t.Fatalf("failure events: %d", n)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%sent:%'", order); n != 0 {
		p.t.Fatal("failed payout recorded as sent")
	}
	if admin := html.UnescapeString(p.page("/admin", p.adminSess)); !strings.Contains(admin, msg) || !strings.Contains(admin, "Failed — not retried") {
		p.t.Fatalf("admin page does not show the wallet error %q", msg)
	}
}

func TestGapWalletSendFailuresRecordFailedPayouts(t *testing.T) {
	p := newPayEnv(t)
	ctx := context.Background()
	core, bov, bnode, _ := gapBTC(t, p, 1)
	p.setPayoutAddress(p.vendor, gapVendorBTC)
	for i, f := range []rpcFailure{{-6, "Insufficient funds"}, {-13, "Error: Please enter the wallet passphrase with walletpassphrase first."}} {
		addr := "tb1qgapsendfailure" + strings.Repeat(string(rune('a'+i)), 20)
		order := p.gapOrder("BTC", addr)
		core.gapReceive(addr, "tx-send-"+addr[len(addr)-4:], 0, 100000, 1)
		p.poll()
		p.complete(order)
		if st, _, to, amt := p.payout(order); st != "pending" || to != gapVendorBTC || amt != 100000 {
			t.Fatalf("queued payout %s %s %d", st, to, amt)
		}
		before := bnode.called("sendtoaddress")
		bov.fail("sendtoaddress", f.code, f.msg)
		err := p.A.pollOnce(ctx)
		if err == nil || !strings.Contains(err.Error(), f.msg) {
			t.Fatalf("send failure %d not reported: %v", f.code, err)
		}
		if bnode.called("sendtoaddress") != before+1 {
			t.Fatalf("sendtoaddress calls %d, want exactly one more than %d", bnode.called("sendtoaddress"), before)
		}
		p.failedPayout(order, f.msg, false) // a JSON-RPC error: nothing was broadcast
		// payment_status carries the pass error and the admin provider table shows it.
		if last := p.str("SELECT last_error FROM payment_status WHERE currency='BTC'"); !strings.Contains(last, f.msg) {
			t.Fatalf("payment_status.last_error %q", last)
		}
		// The wallet recovers: the failed payout is still never retried.
		bov.clear()
		p.poll()
		if st, _, _, _ := p.payout(order); st != "failed" || bnode.called("sendtoaddress") != before+1 {
			t.Fatalf("failed payout retried: %s calls=%d", st, bnode.called("sendtoaddress"))
		}
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%payout of 0.001 BTC failed and will not be retried%'", p.vendor.ID); n != i+1 {
			t.Fatalf("vendor failure notifications: %d", n)
		}
	}
	if last := p.str("SELECT last_error FROM payment_status WHERE currency='BTC'"); last != "" {
		t.Fatalf("last_error not cleared by a clean pass: %q", last)
	}

	// Monero: not enough money, not enough unlocked money, and a transfer that returns no tx_hash.
	g, _ := gapXMR(t, p, 1)
	if _, err := p.DB.Exec("UPDATE users SET payout_xmr=$1 WHERE id=$2", payStagenetSub, p.vendor.ID); err != nil {
		t.Fatal(err)
	}
	for i, f := range []rpcFailure{{-17, "not enough money"}, {-37, "not enough unlocked money"}, {0, "monero transfer returned no transaction hash"}} {
		addr := "7gapxmrsend" + strings.Repeat(string(rune('a'+i)), 84)
		g.subaddress(addr, int64(20+i))
		order := p.gapOrder("XMR", addr)
		g.deposit(addr, "xmr-send-"+string(rune('a'+i)), 100000, 1)
		p.poll()
		p.complete(order)
		before := g.s.called("transfer")
		if f.code != 0 {
			g.ov.fail("transfer", f.code, f.msg)
		} else {
			g.ov.answer("transfer", func(string, string, json.RawMessage) (any, int, string) { return map[string]any{"tx_hash": ""}, 0, "" })
		}
		if err := p.A.pollOnce(ctx); err == nil || !strings.Contains(err.Error(), f.msg) {
			t.Fatalf("monero send failure %q not reported: %v", f.msg, err)
		}
		if g.s.called("transfer") != before+1 {
			t.Fatalf("transfer calls %d, want %d", g.s.called("transfer"), before+1)
		}
		// A JSON-RPC error is a definite rejection; a success reply without a transaction hash is ambiguous.
		p.failedPayout(order, f.msg, f.code == 0)
		if last := p.str("SELECT last_error FROM payment_status WHERE currency='XMR'"); !strings.Contains(last, f.msg) {
			t.Fatalf("XMR payment_status.last_error %q", last)
		}
		g.reset()
		p.poll()
		if st, _, _, _ := p.payout(order); st != "failed" || g.s.called("transfer") != before+1 {
			t.Fatalf("failed XMR payout retried: %s", st)
		}
	}
}

func TestGapRPCErrorsMidPassNeverExpireOrChangeState(t *testing.T) {
	p := newPayEnv(t)
	ctx := context.Background()
	core, bov, _, _ := gapBTC(t, p, 2)
	expired := p.gapOrder("BTC", "tb1qgapexpiredorder000000000000000000")
	p.expireAddress(expired)
	funded := p.gapOrder("BTC", "tb1qgapfundedorder0000000000000000000")
	core.gapReceive("tb1qgapfundedorder0000000000000000000", "tx-funded", 0, 100000, 2)

	unchanged := func(stage string) {
		t.Helper()
		if s1, s2 := p.state(expired), p.state(funded); s1 != stateAwaitingPayment || s2 != stateAwaitingPayment {
			t.Fatalf("%s: state changed without a wallet read: expired=%s funded=%s", stage, s1, s2)
		}
		if n := p.count("SELECT count(*) FROM payments WHERE order_id IN ($1,$2)", expired, funded); n != 0 {
			t.Fatalf("%s: %d ledger rows written from a failed read", stage, n)
		}
	}
	pass := func(stage, want string) {
		t.Helper()
		err := p.A.pollOnce(ctx)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: pass error %v, want %q", stage, err, want)
		}
		unchanged(stage)
		var last, network string
		var polled bool
		if err := p.DB.QueryRow("SELECT last_error,network,last_poll IS NOT NULL FROM payment_status WHERE currency='BTC'").Scan(&last, &network, &polled); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(last, want) || network != "testnet4" || !polled {
			t.Fatalf("%s: payment_status last_error=%q network=%q polled=%v", stage, last, network, polled)
		}
		if admin := html.UnescapeString(p.page("/admin", p.adminSess)); !strings.Contains(admin, want) {
			t.Fatalf("%s: admin page does not show %q", stage, want)
		}
	}

	bov.fail("listreceivedbyaddress", -4, "Wallet is currently rescanning. Abort existing rescan or wait.")
	pass("listreceivedbyaddress", "Wallet is currently rescanning")
	bov.clear()
	bov.fail("gettransaction", -5, "Invalid or non-wallet transaction id")
	pass("gettransaction", "error -5: Invalid or non-wallet transaction id")
	bov.clear()
	// A node that accepts the request and never answers: the client timeout ends the read.
	bov.stall("listreceivedbyaddress", true)
	defer func(d time.Duration) { rpcTimeout = d }(rpcTimeout)
	rpcTimeout = 300 * time.Millisecond
	pass("timeout", "bitcoin RPC unreachable")
	bov.stall("listreceivedbyaddress", false)
	rpcTimeout = 10 * time.Second

	p.poll()
	if s := p.state(expired); s != stateCancelled {
		t.Fatalf("expired order after a successful read: %s", s)
	}
	if s := p.state(funded); s != statePaid {
		t.Fatalf("funded order after a successful read: %s", s)
	}
	if last := p.str("SELECT last_error FROM payment_status WHERE currency='BTC'"); last != "" {
		t.Fatalf("stale last_error %q", last)
	}
	if admin := p.page("/admin", p.adminSess); strings.Contains(admin, "Wallet is currently rescanning") || strings.Contains(admin, "bitcoin RPC unreachable") {
		t.Fatal("admin page still shows a cleared error")
	}
}

func TestGapMoneroRPCErrorsMidPassNeverExpireOrFlag(t *testing.T) {
	p := newPayEnv(t)
	ctx := context.Background()
	g, _ := gapXMR(t, p, 1)
	subC, subD := "7gapxmrexpired"+strings.Repeat("c", 81), "7gapxmrpaid"+strings.Repeat("d", 84)
	g.subaddress(subC, 3)
	g.subaddress(subD, 4)
	expired := p.gapOrder("XMR", subC)
	paid := p.gapOrder("XMR", subD)
	g.deposit(subD, "xmr-paid", 100000, 1)
	p.poll()
	if s := p.state(paid); s != statePaid {
		t.Fatalf("paid order: %s", s)
	}
	p.expireAddress(expired)
	check := func(stage, want string) {
		t.Helper()
		if err := p.A.pollOnce(ctx); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: pass error %v, want %q", stage, err, want)
		}
		if s := p.state(expired); s != stateAwaitingPayment {
			t.Fatalf("%s: order expired although the wallet was not read: %s", stage, s)
		}
		if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND credited AND confirmations=1 AND NOT flagged", paid); n != 1 {
			t.Fatalf("%s: credited deposit marked missing or flagged after a failed read", stage)
		}
		if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%conflicted or missing%'", paid); n != 0 {
			t.Fatalf("%s: false conflict reported to moderators", stage)
		}
		if last := p.str("SELECT last_error FROM payment_status WHERE currency='XMR'"); !strings.Contains(last, want) {
			t.Fatalf("%s: XMR last_error %q", stage, last)
		}
		if admin := html.UnescapeString(p.page("/admin", p.adminSess)); !strings.Contains(admin, want) {
			t.Fatalf("%s: admin page does not show %q", stage, want)
		}
	}
	g.ov.fail("get_transfers", -1, "Failed to get transfers")
	check("get_transfers", "get_transfers: error -1: Failed to get transfers")
	g.reset()
	// The app restarts (empty subaddress index cache) while the wallet service has no wallet open.
	g.provider(t, p, 1)
	g.ov.fail("get_address_index", -13, "No wallet file")
	check("get_address_index", "get_address_index: error -13: No wallet file")
	g.reset()
	p.poll()
	if s := p.state(expired); s != stateCancelled {
		t.Fatalf("expired order after a successful read: %s", s)
	}
	if s := p.state(paid); s != statePaid || p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", paid) != 0 {
		t.Fatalf("paid order after recovery: %s", s)
	}
}

// Concurrent /orders/complete with a live provider queues one payout, and while its wallet send is in
// flight neither another watcher pass nor another sender sends it again.
func TestGapConcurrentCompletionWithProviderSendsOnce(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.paidByWatcher(p.order, p.addr, "tx-cc")
	p.move(p.order, statePaid, stateShipped, p.vendor)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- p.do("POST", "/orders/complete", p.buyerSess, url.Values{"order_id": {p.order}}).Code
		}()
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[303] != 1 || got[409] != 7 || p.state(p.order) != stateCompleted {
		t.Fatalf("status counts %v state %s", got, p.state(p.order))
	}
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 1 {
		t.Fatalf("payouts queued: %d", n)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'Queued TESTNET release of 0.001 BTC%'", p.order); n != 1 {
		t.Fatalf("queue events: %d", n)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var hooked atomic.Int32
	p.fake.sendHook = func() { // the first wallet call blocks until released; later calls pass through
		if hooked.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	first := make(chan error, 1)
	go func() { first <- p.A.pollOnce(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first watcher pass never reached the wallet send")
	}
	if st, _, _, _ := p.payout(p.order); st != "sending" {
		t.Fatalf("payout during the wallet call: %s (the claim must be committed first)", st)
	}
	// Another process's watcher pass (advisory lock held) and a direct second sender both find nothing to send.
	if err := p.A.pollOnce(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if err := p.A.sendPayouts(context.Background(), p.fake); err != nil {
		t.Fatalf("second sender: %v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first pass: %v", err)
	}
	p.fake.sendHook = nil
	p.poll()
	sends := p.fake.Sends()
	if len(sends) != 1 || sends[0].To != "fake-testnet-vendor" || sends[0].Amount != 100000 || len(p.fake.sendCtxErrs) != 1 {
		t.Fatalf("sends %+v (wallet calls %d)", sends, len(p.fake.sendCtxErrs))
	}
	if st, txid, _, _ := p.payout(p.order); st != "sent" || txid != sends[0].TxID {
		t.Fatalf("payout %s %s", st, txid)
	}
}

// The claim-lost branch: a sender that listed a payout as pending finds it already claimed by another sender
// and skips it instead of sending it a second time.
func TestGapPayoutClaimLostToAnotherSender(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	// Both payouts are queued before any watcher pass could send the first.
	first, addrA := p.newOrder(stateAwaitingPayment)
	second, addrB := p.newOrder(stateAwaitingPayment)
	p.fake.Deposit(addrA, "tx-claim-a", 0, 100000, 3)
	p.fake.Deposit(addrB, "tx-claim-b", 0, 100000, 3)
	p.poll()
	p.complete(first)
	p.complete(second)
	idA, idB := p.count("SELECT id FROM payouts WHERE order_id=$1", first), p.count("SELECT id FROM payouts WHERE order_id=$1", second)
	if idA >= idB || p.count("SELECT count(*) FROM payouts WHERE state='pending'") != 2 {
		t.Fatalf("payout ids %d %d, or not both pending", idA, idB)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var hooked atomic.Int32
	p.fake.sendHook = func() { // the first wallet call blocks until released; later calls pass through
		if hooked.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	done := make(chan error, 1)
	// Sender 1 lists [A, B], claims A and blocks in the wallet call for A.
	go func() { done <- p.A.sendPayouts(context.Background(), p.fake) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("sender 1 never reached the wallet")
	}
	// Sender 2 lists [B] (A is claimed), claims and sends B.
	if err := p.A.sendPayouts(context.Background(), p.fake); err != nil {
		t.Fatal(err)
	}
	if st, _, _, _ := p.payout(second); st != "sent" {
		t.Fatalf("B after sender 2: %s", st)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("sender 1: %v", err)
	}
	p.fake.sendHook = nil
	if calls := len(p.fake.sendCtxErrs); calls != 2 {
		t.Fatalf("wallet calls %d: sender 1 sent B again after losing the claim", calls)
	}
	stA, txA, _, _ := p.payout(first)
	stB, txB, _, _ := p.payout(second)
	sends := p.fake.Sends()
	if stA != "sent" || stB != "sent" || txA == txB || len(sends) != 2 || sends[0].TxID != txB || sends[1].TxID != txA {
		t.Fatalf("payouts A=%s/%s B=%s/%s sends=%+v", stA, txA, stB, txB, sends)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%sent:%'", second); n != 1 {
		t.Fatalf("B sent events: %d", n)
	}
}

// Payout-address rejection is decided by the provider's ValidAddress: every rejected request carries the
// correct password, and the stored address never changes.
func TestGapPayoutAddressRejectedByValidAddress(t *testing.T) {
	p := newPayEnv(t)
	save := func(currency, addr string) (int, string) {
		w := p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {currency}, "address": {addr}, "password": {testPassword}})
		return w.Code, w.Body.String()
	}
	stored := func(column string) string {
		return p.str("SELECT "+column+" FROM users WHERE id=$1", p.vendor.ID)
	}
	audits := func() int { return p.count("SELECT count(*) FROM audit_events WHERE user_id=$1", p.vendor.ID) }
	reject := func(currency, column, addr, network, keep string) {
		t.Helper()
		before := audits()
		code, body := save(currency, addr)
		if code != 400 || !strings.Contains(body, "Enter a valid "+currency+" address for the "+network+" test network") {
			t.Fatalf("%s %q with the correct password: %d %s", currency, addr, code, body)
		}
		if got := stored(column); got != keep || audits() != before {
			t.Fatalf("%s address changed to %q by a rejected request (audits %d -> %d)", currency, got, before, audits())
		}
	}
	accept := func(currency, column, addr string) {
		t.Helper()
		if code, body := save(currency, addr); code != 303 || stored(column) != addr {
			t.Fatalf("%s %q: %d %s stored=%q", currency, addr, code, body, stored(column))
		}
	}
	// Fake BTC wallet: the password is accepted (a valid address saves), so each 400 is ValidAddress's.
	accept("BTC", "payout_btc", "fake-testnet-original")
	for _, bad := range []string{"bc1qmainnetaddress0000000000000000000", "1BoatSLRHtKNngkdXEeobR76b53LETtpyT", "tb1qtestnetbutnotthiswallet0000000000", "fake-testnet-" + strings.Repeat("x", 60)} {
		reject("BTC", "payout_btc", bad, "fake", "fake-testnet-original")
	}

	// Real Bitcoin adapter: mainnet formats are refused offline, a checksum the node rejects after one RPC.
	core, _, bnode, _ := gapBTC(t, p, 1)
	calls := bnode.called("validateaddress")
	for _, bad := range []string{"bc1qmainnetaddress0000000000000000000", "bcrt1qregtestaddress000000000000000000", "TB1QUPPERCASE0000000000000000000000"} {
		reject("BTC", "payout_btc", bad, "testnet4", "fake-testnet-original")
	}
	if bnode.called("validateaddress") != calls {
		t.Fatal("format-invalid addresses reached the node")
	}
	core.set(func(c *payCore) { c.invalid["tb1qbadchecksum00000000000000000000000"] = true })
	reject("BTC", "payout_btc", "tb1qbadchecksum00000000000000000000000", "testnet4", "fake-testnet-original")
	if bnode.called("validateaddress") != calls+1 {
		t.Fatal("node-side validation not consulted")
	}
	accept("BTC", "payout_btc", gapVendorBTC)

	// Real Monero adapter: mainnet and wrong-network addresses are refused; a stagenet subaddress saves.
	g, _ := gapXMR(t, p, 1)
	g.w.set(func(m *payMoneroWallet) {
		m.valid[payStagenetSub] = "stagenet"
		m.valid[payTestnetPrimary] = "testnet"
		m.valid[payMainnetPrimary] = "mainnet"
	})
	for _, bad := range []string{payMainnetPrimary, payTestnetPrimary, "7short", payStagenetSub[:len(payStagenetSub)-1] + "Z"} {
		reject("XMR", "payout_xmr", bad, "stagenet", "")
	}
	accept("XMR", "payout_xmr", payStagenetSub)
	reject("XMR", "payout_xmr", payMainnetPrimary, "stagenet", payStagenetSub)
	if got := stored("payout_btc"); got != gapVendorBTC {
		t.Fatalf("BTC address changed by XMR requests: %q", got)
	}
}

// A-52: enqueuePayout runs inside a transition (HTTP cancel/complete/resolve) while the watcher's
// recordIncoming writes the ledger without the order lock. A deposit that reaches the threshold after the
// payout amount is fixed must not be marked credited (it is then flagged as not paid out); a counted deposit
// cannot turn conflicted before it is credited. A test-only trigger (waiting on an advisory lock the test
// holds) pauses the transition at the payout INSERT; it does not change what enqueuePayout does.
func TestEnqueuePayoutCreditsOnlyWhatItPays(t *testing.T) {
	p := newPayEnv(t)
	p.paidByWatcher(p.order, p.addr, "tx-fund")    // 100000 credited, order paid
	p.fake.Deposit(p.addr, "tx-extra", 1, 7000, 2) // extra deposit, one confirmation short
	p.poll()
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")

	const key = 5252052
	for _, q := range []string{
		`CREATE FUNCTION a52_pause() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(5252052); RETURN NEW; END $$`,
		`CREATE TRIGGER a52_pause BEFORE INSERT ON payouts FOR EACH ROW EXECUTE FUNCTION a52_pause()`,
	} {
		if _, err := p.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	hold, err := p.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	if _, err = hold.ExecContext(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		t.Fatal(err)
	}
	unlocked := false
	defer func() {
		if !unlocked {
			hold.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", key)
		}
	}()

	// The vendor cancels the paid order: transition -> enqueuePayout (refund) fixes the amount, then pauses.
	cancelled := make(chan int, 1)
	go func() {
		cancelled <- p.do("POST", "/orders/cancel", p.vendorSess, url.Values{"order_id": {p.order}, "from": {statePaid}}).Code
	}()
	waitFor(t, func() bool {
		return p.count("SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'INSERT INTO payouts%' AND wait_event_type='Lock'") == 1
	})

	// The watcher records the wallet's report: the extra deposit has reached the threshold. (A wallet lists
	// outputs in any order; with tx-extra first, its upsert lands while the refund is being queued, and the
	// tx-fund upsert then waits for the refund's row lock.)
	p.fake.SetConfirmations("tx-extra", 3)
	report := newFakeProvider("BTC", 3)
	report.Deposit(p.addr, "tx-extra", 1, 7000, 3)
	report.Deposit(p.addr, "tx-fund", 0, 100000, 3)
	recorded := make(chan error, 1)
	go func() {
		recorded <- p.A.recordIncoming(ctx, report, []string{p.addr}, map[string]string{p.addr: p.order})
	}()
	waitFor(t, func() bool {
		return p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND txid='tx-extra' AND confirmations=3", p.order) == 1
	})
	if _, err = hold.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
		t.Fatal(err)
	}
	unlocked = true
	if code := <-cancelled; code != 303 {
		t.Fatalf("cancel: %d", code)
	}
	if err = <-recorded; err != nil {
		t.Fatal(err)
	}
	p.poll()

	_, _, _, amt := p.payout(p.order)
	var credited, flagged bool
	if err = p.DB.QueryRow("SELECT credited,flagged FROM payments WHERE order_id=$1 AND txid='tx-extra'", p.order).Scan(&credited, &flagged); err != nil {
		t.Fatal(err)
	}
	creditedSum := p.count("SELECT COALESCE(sum(amount),0) FROM payments WHERE order_id=$1 AND credited", p.order)
	if int64(creditedSum) != amt {
		t.Fatalf("refund amount=%d but credited deposits sum to %d (tx-extra credited=%v flagged=%v)", amt, creditedSum, credited, flagged)
	}
	if amt != 107000 && !flagged {
		t.Fatalf("confirmed deposit tx-extra lost: refund amount=%d, tx-extra credited=%v flagged=%v", amt, credited, flagged)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%tx-extra arrived after this order was settled and was not paid out%'", p.order); amt != 107000 && n != 1 {
		t.Fatalf("not-paid-out flag events for tx-extra: %d", n)
	}
}

// raceEnqueueAgainstUpsert runs n transitions paid -> cancelled (enqueuePayout in the transition's
// transaction) each racing the exact autocommit upsert recordIncoming runs for a second deposit, sweeping the
// upsert's start offset across one transition. No trigger or pause: the natural window only. check inspects
// each order afterwards.
func raceEnqueueAgainstUpsert(t *testing.T, p *payEnv, n int, extraConfs, upsertConfs int64, check func(order, txExtra string)) {
	t.Helper()
	upsert := `INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,locked,late_notice)
		VALUES($1,'BTC',$2,1,$3,7000,$4,false,false)
		ON CONFLICT (currency,txid,idx) DO UPDATE SET confirmations=excluded.confirmations, locked=excluded.locked, updated=now()
		WHERE (payments.confirmations,payments.locked) IS DISTINCT FROM (excluded.confirmations,excluded.locked)`
	ctx := context.Background()
	const calibrate, sweep = 20, 50 // mean transition time from the first 20 runs; 50 start offsets across it
	step := 10 * time.Microsecond
	var moveTotal time.Duration
	for i := 0; i < n; i++ {
		order := p.testEnv.order(p.buyer.ID, p.productID, "BTC", statePaid)
		addr := "addr-" + order[:12]
		txExtra := "tx-extra-" + order[:12]
		for _, q := range []struct {
			s    string
			args []any
		}{
			{"INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'BTC',$2,'fake')", []any{order, addr}},
			{"INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,credited) VALUES($1,'BTC',$2,0,$3,100000,3,true)", []any{order, "tx-fund-" + order[:12], addr}},
			{"INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'BTC',$2,1,$3,7000,$4)", []any{order, txExtra, addr, extraConfs}},
		} {
			if _, err := p.DB.Exec(q.s, q.args...); err != nil {
				t.Fatal(err)
			}
		}
		p.fake.Deposit(addr, "tx-fund-"+order[:12], 0, 100000, 3)
		delay := time.Duration(i%sweep) * step
		done := make(chan error, 1)
		start := time.Now()
		go func() {
			for time.Since(start) < delay {
			}
			_, err := p.DB.ExecContext(ctx, upsert, order, txExtra, addr, upsertConfs)
			done <- err
		}()
		m0 := time.Now()
		p.move(order, statePaid, stateCancelled, p.vendor)
		if i < calibrate {
			moveTotal += time.Since(m0)
			if i == calibrate-1 {
				step = moveTotal / calibrate / sweep
			}
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		check(order, txExtra)
	}
}

// A-52, direction 1: a deposit reaching the threshold while the refund is being queued is either in the
// refund and credited, or neither (and later flagged as not paid out) — never credited but unpaid.
func TestEnqueuePayoutNaturalRaceNeverCreditsUnpaidDeposit(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	lost := 0
	raceEnqueueAgainstUpsert(t, p, 100, 2, 3, func(order, txExtra string) {
		_, _, _, amt := p.payout(order)
		var credited bool
		if err := p.DB.QueryRow("SELECT credited FROM payments WHERE txid=$1", txExtra).Scan(&credited); err != nil {
			t.Fatal(err)
		}
		if amt == 100000 && credited {
			lost++
		}
	})
	if lost != 0 {
		t.Fatalf("%d of 100 refunds left a credited deposit out of the payout (never flagged)", lost)
	}
}

// A-52, direction 2: a counted deposit that turns conflicted while the refund is being queued is either left
// out of the refund, or paid and credited (so the payout is held) — never paid out uncredited.
func TestEnqueuePayoutNaturalRaceNeverPaysUncreditedConflictedDeposit(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	lost := 0
	raceEnqueueAgainstUpsert(t, p, 100, 3, -1, func(order, txExtra string) {
		_, _, _, amt := p.payout(order)
		var credited bool
		if err := p.DB.QueryRow("SELECT credited FROM payments WHERE txid=$1", txExtra).Scan(&credited); err != nil {
			t.Fatal(err)
		}
		if amt == 107000 && !credited {
			lost++
		}
	})
	if lost != 0 {
		t.Fatalf("%d of 100 refunds paid out a conflicted deposit the ledger does not mark credited", lost)
	}
	p.poll()
	p.poll()
	for _, s := range p.fake.Sends() {
		if s.Amount == 107000 {
			t.Fatalf("wallet sent a refund including a conflicted deposit: %+v", s)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
