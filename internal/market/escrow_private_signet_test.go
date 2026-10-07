package market

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// A-443: the private-signet mode of the real-chain legs (scripts/test-escrow-regtest.sh --private-signet): the miner
// node's preparation, the isolated-node leg, and the mining helper's refusal of an empty generatetoaddress answer.
// Everything on the private signet is private signet, not public.

// mineBlocks never takes generatetoaddress's silent [] (Core's answer on signet when maxtries runs out) for a block, and
// on signet it passes maxtries 2000000000 and asks again for the blocks still missing. No database; a scripted
// JSON-RPC server.
func TestEscrowRealChainMineRefusesEmpty(t *testing.T) {
	type call struct {
		Params []any `json:"params"`
	}
	serve := func(answers ...string) (*rpcClient, *[]call) {
		var calls []call
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var c call
			json.Unmarshal(body, &c)
			calls = append(calls, c)
			if len(calls) > 5 { // a helper that keeps asking after an empty answer: stop it, the call count tells
				io.WriteString(w, `{"result":null,"error":{"code":-1,"message":"too many calls"},"id":"x"}`)
				return
			}
			result := "[]"
			if len(calls) <= len(answers) {
				result = answers[len(calls)-1]
			}
			io.WriteString(w, `{"result":`+result+`,"error":null,"id":"x"}`)
		}))
		t.Cleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		u.User = url.UserPassword("u", "p")
		c, err := newRPCClient("mine test", u.String(), "1.0", false)
		if err != nil {
			t.Fatal(err)
		}
		return c, &calls
	}
	h := func(b byte) string { return `"` + strings.Repeat(string(b), 64) + `"` }
	ctx := context.Background()
	for _, chain := range []string{"regtest", "signet"} {
		c, calls := serve("[]")
		if mined, err := mineBlocks(ctx, c, chain, 3, "addr"); err == nil || len(mined) != 0 {
			t.Errorf("%s: an empty answer was taken for blocks: %v %v", chain, mined, err)
		}
		if len(*calls) != 1 {
			t.Errorf("%s: %d calls after an empty answer", chain, len(*calls))
		}
	}
	// Signet: 2 of 3 blocks, then the missing one, each call with maxtries 2000000000.
	c, calls := serve("["+h('a')+","+h('b')+"]", "["+h('c')+"]")
	mined, err := mineBlocks(ctx, c, "signet", 3, "addr")
	if err != nil || len(mined) != 3 || len(*calls) != 2 {
		t.Fatalf("signet in two calls: %v %v (%d calls)", mined, err, len(*calls))
	}
	for i, want := range []float64{3, 1} {
		p := (*calls)[i].Params
		if len(p) != 3 || p[0] != want || p[1] != "addr" || p[2] != float64(signetMaxTries) {
			t.Errorf("signet call %d params %v", i, p)
		}
	}
	// Signet: a call that mines blocks then one that mines none fails, keeping what was mined.
	c, _ = serve("["+h('a')+"]", "[]")
	if mined, err = mineBlocks(ctx, c, "signet", 2, "addr"); err == nil || len(mined) != 1 {
		t.Fatalf("signet, then empty: %v %v", mined, err)
	}
	// Regtest: one call with no maxtries; fewer blocks than asked for fails, more than asked for fails.
	c, calls = serve("[" + h('a') + "," + h('b') + "]")
	if mined, err = mineBlocks(ctx, c, "regtest", 2, "addr"); err != nil || len(mined) != 2 || len((*calls)[0].Params) != 2 {
		t.Fatalf("regtest: %v %v %v", mined, err, *calls)
	}
	c, _ = serve("[" + h('a') + "]")
	if _, err = mineBlocks(ctx, c, "regtest", 2, "addr"); err == nil {
		t.Fatal("regtest: 1 of 2 blocks accepted")
	}
	c, _ = serve("[" + h('a') + "," + h('b') + "]")
	if _, err = mineBlocks(ctx, c, "signet", 1, "addr"); err == nil {
		t.Fatal("signet: 2 blocks for 1 accepted")
	}
}

// TestEscrowPrivateSignetPrepare readies node A of the private signet (ESCROW_REGTEST_MINER_RPC_URL; run by
// scripts/test-escrow-regtest.sh --private-signet before the legs, never in CI): it loads or creates the miner wallet,
// mines to it until at least 150 BTC are mature (the first time: about 110 blocks, each checked by mineBlocks), and
// mines one more block so the tip is fresh (a cached datadir's tip is old: the sync guard's tip age and Core's initial
// block download).
func TestEscrowPrivateSignetPrepare(t *testing.T) {
	if realChainNet() != "signet" || os.Getenv("ESCROW_REGTEST_MINER_RPC_URL") == "" {
		t.Skip("set ESCROW_REGTEST_CHAIN=signet and ESCROW_REGTEST_MINER_RPC_URL (scripts/test-escrow-regtest.sh --private-signet)")
	}
	ctx := context.Background()
	miner := privateSignetMiner(t, ctx)
	var version struct {
		Subversion string `json:"subversion"`
	}
	if err := miner.call(ctx, "", "getnetworkinfo", []any{}, &version); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: node A %s, signet_challenge %s", privateSignetLabel, version.Subversion, privateSignetChallenge)
	wallet := "/wallet/" + privateSignetMinerWallet
	if err := miner.call(ctx, "", "loadwallet", []any{privateSignetMinerWallet, true}, nil); err != nil {
		if err := miner.call(ctx, wallet, "getwalletinfo", []any{}, nil); err != nil {
			if err := miner.call(ctx, "", "createwallet", []any{privateSignetMinerWallet, false, false, "", false, true, true}, nil); err != nil {
				t.Fatalf("createwallet: %v", err)
			}
		}
	}
	var addr string
	if err := miner.call(ctx, wallet, "getnewaddress", []any{}, &addr); err != nil {
		t.Fatal(err)
	}
	balance := func() float64 {
		var b struct {
			Mine struct {
				Trusted float64 `json:"trusted"`
			} `json:"mine"`
		}
		if err := miner.call(ctx, wallet, "getbalances", []any{}, &b); err != nil {
			t.Fatal(err)
		}
		return b.Mine.Trusted
	}
	start := time.Now()
	mined := 0
	for b := balance(); b < 150; b = balance() {
		if mined >= 300 {
			t.Fatalf("%s: %d blocks mined and only %.8f BTC mature", privateSignetLabel, mined, b)
		}
		mustMine(t, ctx, miner, 10, addr)
		mined += 10
		t.Logf("%s: mined %d blocks so far (%s), %.8f BTC mature", privateSignetLabel, mined, time.Since(start).Round(time.Second), balance())
	}
	mustMine(t, ctx, miner, 1, addr)
	var info struct {
		Blocks int64 `json:"blocks"`
		IBD    bool  `json:"initialblockdownload"`
	}
	if err := miner.call(ctx, "", "getblockchaininfo", []any{}, &info); err != nil || info.IBD {
		t.Fatalf("node A after a fresh block: %+v %v", info, err)
	}
	t.Logf("%s: node A at height %d, initial block download over, miner wallet %.8f BTC mature (%d blocks mined to mature it in %s, plus one fresh block)",
		privateSignetLabel, info.Blocks, balance(), mined, time.Since(start).Round(time.Second))
}

// TestEscrowRegtestIsolated is A-443's isolated-node leg, on the private signet only (P4-R3's sync guard against the
// fact the council found: Core's initial block download latches false, so a node that lost its peers looks synced while
// deposits are invisible). The App's escrow node is node B; the buyer's coins come from node A:
//   - an escrow order is created and a watcher pass on the connected node B is synced;
//   - node B leaves the network (setnetworkactive false): no peers;
//   - node A pays the escrow and mines it, which node B cannot see; the order's payment window is moved into the past
//     (SQL);
//   - a watcher pass does nothing: the sync guard reports no peers, initial block download is still false, and the
//     order is not cancelled;
//   - node B reconnects and catches up with node A: the next pass credits the deposit and the order completes.
func TestEscrowRegtestIsolated(t *testing.T) {
	appURL, opURL := os.Getenv("ESCROW_REGTEST_APP_RPC_URL"), os.Getenv("ESCROW_REGTEST_OPERATOR_RPC_URL")
	if realChainNet() != "signet" || appURL == "" || opURL == "" || os.Getenv("ESCROW_REGTEST_MINER_RPC_URL") == "" {
		t.Skip("the isolated-node leg runs on the private signet only (scripts/test-escrow-regtest.sh --private-signet): it needs a second node")
	}
	ctx := context.Background()
	call, op := regtestOperator(t, opURL, map[string]string{"app": appURL})
	miner := privateSignetMiner(t, ctx)
	s := randomToken()[:8]
	arbiter, watch, refundW := "iso-arbiter-"+s, "iso-watch-"+s, "iso-refund-"+s
	call("", "createwallet", nil, arbiter)
	call("", "createwallet", nil, watch, true, true)
	call("", "createwallet", nil, refundW)
	bip48Wallet := func(name string) string {
		t.Helper()
		seed := name + "-seed"
		call("", "createwallet", nil, seed)
		call("", "createwallet", nil, name, false, true)
		var priv struct {
			Descriptors []struct {
				Desc     string `json:"desc"`
				Internal bool   `json:"internal"`
			} `json:"descriptors"`
		}
		call(seed, "listdescriptors", &priv, true)
		var root string
		for _, d := range priv.Descriptors {
			if strings.HasPrefix(d.Desc, "wpkh(tprv") && !d.Internal {
				root = strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/", 2)[0]
			}
		}
		var imports []map[string]any
		for chain, internal := range map[string]bool{"0": false, "1": true} {
			var info struct {
				Checksum string `json:"checksum"`
			}
			d := "wpkh(" + root + "/48h/1h/0h/2h/" + chain + "/*)"
			call("", "getdescriptorinfo", &info, d)
			imports = append(imports, map[string]any{"desc": d + "#" + info.Checksum, "active": true, "internal": internal, "range": []int{0, 1000}, "timestamp": "now"})
		}
		call(name, "importdescriptors", nil, imports)
		var pub struct {
			Descriptors []struct {
				Desc     string `json:"desc"`
				Internal bool   `json:"internal"`
			} `json:"descriptors"`
		}
		call(name, "listdescriptors", &pub)
		for _, d := range pub.Descriptors {
			if !d.Internal {
				return strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/0/*", 2)[0]
			}
		}
		t.Fatalf("no BIP48 account in %s", name)
		return ""
	}
	buyerWallet, vendorWallet := "iso-buyer-"+s, "iso-vendor-"+s
	buyerKey, vendorKey := bip48Wallet(buyerWallet), bip48Wallet(vendorWallet)
	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", "")
	t.Setenv("PAYMENT_CONFIRMATIONS_BTC", "1")
	t.Setenv("PAYMENT_EXPIRY", "")
	t.Setenv("BITCOIN_MAX_TIP_AGE", "")
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	e := newTestApp(t)
	if e.A.escrow.Load() == nil || e.A.escrowNetwork() != "signet" {
		t.Fatal("escrow did not start on node B of the private signet")
	}
	t.Logf("%s: escrow on, node B, chain %s", privateSignetLabel, e.A.escrow.Load().chainID)
	activate := func(handle, role, key, wallet string) (string, string) {
		t.Helper()
		uid, sess := e.user(handle+"_"+s, role)
		if code, body := registerKey(e, sess, key); code != 303 {
			t.Fatalf("%s register: %d %s", handle, code, body)
		}
		var signed struct {
			PSBT string `json:"psbt"`
		}
		call(wallet, "walletprocesspsbt", &signed, practicePSBT(t, e, sess), true, "ALL", true, false)
		if code, body := submitPractice(e, sess, signed.PSBT); code != 303 {
			t.Fatalf("%s practice: %d %s", handle, code, body)
		}
		return uid, sess
	}
	buyer, buyerSess := activate("iso_buyer", "buyer", buyerKey, buyerWallet)
	vendor, _ := activate("iso_vendor", "vendor", vendorKey, vendorWallet)
	product := e.product(vendor, "digital")
	if _, err := e.DB.Exec("UPDATE products SET delivery_content='private signet licence' WHERE id=$1", product); err != nil {
		t.Fatal(err)
	}
	order := e.order(buyer, product, "BTC", stateDraft)
	var refund string
	call(refundW, "getnewaddress", &refund)
	if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {refund}}); w.Code != 303 {
		t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
	}
	var addr string
	if err := e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
		t.Fatal(err)
	}
	state := func() string {
		var st string
		e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&st)
		return st
	}
	poll := func() {
		t.Helper()
		if err := e.A.escrowPollOnce(ctx); err != nil {
			t.Fatalf("watcher pass: %v", err)
		}
	}
	type nodeView struct {
		Blocks int64 `json:"blocks"`
		IBD    bool  `json:"initialblockdownload"`
	}
	view := func(c *rpcClient) (v nodeView, peers int64) {
		t.Helper()
		var n struct {
			Connections int64 `json:"connections"`
		}
		if err := c.call(ctx, "", "getblockchaininfo", []any{}, &v); err != nil {
			t.Fatal(err)
		}
		if err := c.call(ctx, "", "getnetworkinfo", []any{}, &n); err != nil {
			t.Fatal(err)
		}
		return v, n.Connections
	}
	syncReason := func() string { r, _ := e.A.escrowSync.Load().(string); return r }
	poll()
	if v, peers := view(op); peers == 0 || state() != stateAwaitingPayment || syncReason() != "" {
		t.Fatalf("connected: %+v peers %d, order %s, not synced %q", v, peers, state(), syncReason())
	}
	t.Logf("%s: escrow %s for order %s; node B connected, synced, order awaiting payment", privateSignetLabel, addr, shortID(order))

	// Node B leaves the network.
	call("", "setnetworkactive", nil, false)
	t.Cleanup(func() { op.call(context.Background(), "", "setnetworkactive", []any{true}, nil) })
	for i := 0; ; i++ {
		if _, peers := view(op); peers == 0 {
			break
		}
		if i == 30 {
			t.Fatal("node B still has peers 30 seconds after setnetworkactive false")
		}
		time.Sleep(time.Second)
	}
	// Node A pays the escrow and mines it; node B cannot see either.
	var minerAddr, txid string
	wallet := "/wallet/" + privateSignetMinerWallet
	if err := miner.call(ctx, wallet, "getnewaddress", []any{}, &minerAddr); err != nil {
		t.Fatal(err)
	}
	if err := miner.callWithin(ctx, regtestOperatorTimeout, wallet, "sendtoaddress", []any{addr, json.Number("0.001")}, &txid); err != nil {
		t.Fatal(err)
	}
	mustMine(t, ctx, miner, 1, minerAddr)
	a, _ := view(miner)
	if _, err := e.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", order); err != nil {
		t.Fatal(err)
	}
	poll()
	b, peers := view(op)
	if state() != stateAwaitingPayment || syncReason() != notSyncedPeers || b.IBD || b.Blocks >= a.Blocks ||
		agInt(e, "SELECT count(*) FROM payments WHERE order_id=$1", order) != 0 {
		t.Fatalf("isolated: order %s, not synced %q, node B %+v peers %d, node A %+v", state(), syncReason(), b, peers, a)
	}
	t.Logf("%s: node B isolated (setnetworkactive false: %d peers, initialblockdownload %v, height %d); node A paid the escrow (%s) and mined it at height %d; payment window moved 25 hours back; watcher pass: not synced (%s), order still %s, no deposit read",
		privateSignetLabel, peers, b.IBD, b.Blocks, txid, a.Blocks, syncReason(), state())

	// Node B reconnects and catches up with node A.
	call("", "setnetworkactive", nil, true)
	for i := 0; ; i++ {
		if b, peers = view(op); peers > 0 && b.Blocks >= a.Blocks {
			break
		}
		if i == 120 {
			t.Fatalf("node B did not reconnect and catch up within 120 seconds: %+v peers %d, node A height %d", b, peers, a.Blocks)
		}
		time.Sleep(time.Second)
	}
	poll()
	if state() != stateCompleted || syncReason() != "" || agInt(e, "SELECT count(*) FROM payments WHERE order_id=$1 AND txid=$2 AND credited", order, txid) != 1 {
		t.Fatalf("reconnected: order %s, not synced %q", state(), syncReason())
	}
	t.Logf("%s: node B reconnected (%d peers, height %d); watcher pass: synced, the deposit %s credited, order %s (not cancelled)", privateSignetLabel, peers, b.Blocks, txid, state())
}
