package market

import (
	"context"
	"encoding/json"
	"html"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestEscrowRegtestPrivateBroadcast is A-441's real-chain leg (scripts/test-escrow-regtest.sh): broadcast honesty on a
// second Bitcoin Core regtest node started with -privatebroadcast -proxy=127.0.0.1:9 -listen=0 (a proxy nobody answers),
// with the same users and whitelists as the main node. With the App reaching Core only as the rpcwhitelist user, in
// APP_MODE=tor, and every party signature made by that party's own Core wallet:
//
//   - the Tor-mode gate on real getnetworkinfo answers: the private node (proxy set for IPv4 and IPv6, Core 31.1) lets
//     broadcasts through; the main node (no proxy) holds them, "no proxy for IPv4 or IPv6";
//   - a funded, completed escrow's claim is signed by the vendor's wallet; the watcher co-signs it and sends it:
//     sendrawtransaction answers its txid, but it is not in the node's mempool, the watch-only wallet does not know it and
//     getprivatebroadcastinfo lists it. The settlement is recorded broadcast, as Core said;
//   - five blocks later nothing is reported; at six (escrowStallBlocks) one staff notice and one order note say it is
//     queued or stuck and /admin lists it; more passes and blocks do not repeat it; the settlement stays broadcast;
//   - generateblock mines the stored transaction directly (standing in for the network delivering it): the wallet sees
//     it, the alert is cleared by one note and the /admin line goes; two blocks later it is confirmed.
func TestEscrowRegtestPrivateBroadcast(t *testing.T) {
	skipOnPrivateSignet(t, privateSignetSkipPrivate)
	mainApp := os.Getenv("ESCROW_REGTEST_APP_RPC_URL")
	appURL, opURL := os.Getenv("ESCROW_REGTEST_PRIVATE_APP_RPC_URL"), os.Getenv("ESCROW_REGTEST_PRIVATE_OPERATOR_RPC_URL")
	if mainApp == "" || appURL == "" || opURL == "" {
		t.Skip("set ESCROW_REGTEST_APP_RPC_URL, ESCROW_REGTEST_PRIVATE_APP_RPC_URL and ESCROW_REGTEST_PRIVATE_OPERATOR_RPC_URL (scripts/test-escrow-regtest.sh) to run against bitcoind -regtest")
	}
	ctx := context.Background()
	op, err := newRPCClient("regtest private operator", opURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newRPCClient("regtest private app", appURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	mainClient, err := newRPCClient("regtest app", mainApp, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*rpcClient{"private operator": op, "private app": app, "main app": mainClient} {
		var info struct {
			Chain string `json:"chain"`
		}
		if err := c.call(ctx, "", "getblockchaininfo", []any{}, &info); err != nil || info.Chain != "regtest" {
			t.Fatalf("refusing: the %s node URL is not on regtest (or is unreachable)", name)
		}
	}
	// The gate on real answers, read as the whitelisted app user.
	gateOf := func(c *rpcClient) (string, escrowNetInfo) {
		t.Helper()
		var info escrowNetInfo
		err := c.call(ctx, "", "getnetworkinfo", []any{}, &info)
		return torGate(info, err), info
	}
	privWhy, privInfo := gateOf(app)
	mainWhy, mainInfo := gateOf(mainClient)
	if privWhy != "" || mainWhy != torGateNoProxy {
		t.Fatalf("Tor-mode gate: private node %q (%+v), main node %q (%+v); want it to pass the private node and hold the main one (%q)", privWhy, privInfo, mainWhy, mainInfo, torGateNoProxy)
	}
	t.Logf("Tor-mode gate on real getnetworkinfo: private node (version %d, networks %+v) sends; main node (version %d, networks %+v) holds: %s",
		privInfo.Version, privInfo.Networks, mainInfo.Version, mainInfo.Networks, mainWhy)

	call := func(wallet, method string, out any, params ...any) {
		t.Helper()
		path := ""
		if wallet != "" {
			path = "/wallet/" + wallet
		}
		if params == nil {
			params = []any{}
		}
		if err := op.call(ctx, path, method, params, out); err != nil {
			t.Fatalf("operator %s: %v", method, err)
		}
	}
	s := randomToken()[:8]
	arbiter, watch, funder := "a441-arbiter-"+s, "a441-watch-"+s, "a441-funder-"+s
	call("", "createwallet", nil, arbiter)
	call("", "createwallet", nil, watch, true, true)
	call("", "createwallet", nil, funder)
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
	buyerWallet, vendorWallet := "a441-buyer-"+s, "a441-vendor-"+s
	buyerKey, vendorKey := bip48Wallet(buyerWallet), bip48Wallet(vendorWallet)
	newAddr := func() string {
		t.Helper()
		var a string
		call(funder, "getnewaddress", &a)
		return a
	}
	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", "")
	t.Setenv("ESCROW_BTC_FEE_RATE", "")
	t.Setenv("PAYMENT_CONFIRMATIONS_BTC", "3")
	t.Setenv("PAYMENT_EXPIRY", "")
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	e := newTestApp(t)
	if e.A.escrow.Load() == nil {
		t.Fatal("escrow did not start against the private regtest node")
	}
	e.A.mode = "tor"
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
	buyer, buyerSess := activate("ra441_buyer", "buyer", buyerKey, buyerWallet)
	vendor, vendorSess := activate("ra441_vendor", "vendor", vendorKey, vendorWallet)
	e.user("ra441_mod_"+s, "moderator")
	_, adminSess := e.user("ra441_admin_"+s, "admin")
	mineTo := newAddr()
	call("", "generatetoaddress", nil, 101, mineTo)
	mine := func(n int) { t.Helper(); call("", "generatetoaddress", nil, n, mineTo) }
	poll := func() {
		t.Helper()
		if err := e.A.escrowPollOnce(ctx); err != nil {
			t.Fatalf("watcher pass: %v", err)
		}
	}
	if why := e.A.broadcastHold(); why != torGateUnread {
		t.Fatalf("before any pass the Tor-mode gate reads %q, want %q", why, torGateUnread)
	}

	// A funded, completed escrow order (the wallet's own send reaches the mempool: wallet sends are never private).
	product := e.product(vendor, "digital")
	if _, err := e.DB.Exec("UPDATE products SET delivery_content='regtest licence a441' WHERE id=$1", product); err != nil {
		t.Fatal(err)
	}
	order := e.order(buyer, product, "BTC", stateDraft)
	if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {newAddr()}}); w.Code != 303 {
		t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
	}
	var addr string
	if err := e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
		t.Fatal(err)
	}
	var deposit string
	call(funder, "sendtoaddress", &deposit, addr, json.Number("0.001"))
	var pool []string
	call("", "getrawmempool", &pool)
	if !slices.Contains(pool, deposit) {
		t.Fatalf("the funding wallet's send %s is not in the private node's mempool", deposit)
	}
	mine(3)
	poll()
	var state string
	e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&state)
	if state != stateCompleted {
		t.Fatalf("funded order: %s", state)
	}
	if why := e.A.broadcastHold(); why != "" {
		t.Fatalf("the Tor-mode gate holds on the private node: %q", why)
	}

	// The vendor's claim, signed by the vendor's wallet; the watcher co-signs it and sends it into the private queue.
	if w := e.do("POST", "/escrow/claim", vendorSess, url.Values{"order_id": {order}, "address": {newAddr()}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	poll()
	var id int64
	var txid, raw string
	if err := e.DB.QueryRow("SELECT id,state FROM settlements WHERE order_id=$1 AND kind='release'", order).Scan(&id, &state); err != nil || state != "awaiting_signature" {
		t.Fatalf("claim: %s %v", state, err)
	}
	w := e.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10), vendorSess, nil)
	if w.Code != 200 {
		t.Fatalf("copy: %d", w.Code)
	}
	var vs struct {
		PSBT     string `json:"psbt"`
		Complete bool   `json:"complete"`
	}
	call(vendorWallet, "walletprocesspsbt", &vs, strings.TrimSpace(w.Body.String()), true, "ALL", true, false)
	if vs.Complete {
		t.Fatal("one party's signature completed the escrow spend")
	}
	if w := e.do("POST", "/escrow/sign", vendorSess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {vs.PSBT}}); w.Code != 303 {
		t.Fatalf("paste: %d %s", w.Code, w.Body.String())
	}
	poll()
	if err := e.DB.QueryRow("SELECT state,txid,COALESCE(raw_tx,'') FROM settlements WHERE id=$1", id).Scan(&state, &txid, &raw); err != nil || state != "broadcast" || raw == "" {
		t.Fatalf("after the co-sign pass: %s %v", state, err)
	}
	var height int64
	call("", "getblockcount", &height)
	queued := func() bool {
		t.Helper()
		var info struct {
			Transactions []struct {
				TxID  string `json:"txid"`
				Peers []any  `json:"peers"`
			} `json:"transactions"`
		}
		call("", "getprivatebroadcastinfo", &info)
		for _, tx := range info.Transactions {
			if tx.TxID == txid {
				return true
			}
		}
		return false
	}
	inWallet := func() bool {
		t.Helper()
		var tx any
		return op.call(ctx, "/wallet/"+watch, "gettransaction", []any{txid, true}, &tx) == nil
	}
	call("", "getrawmempool", &pool)
	if slices.Contains(pool, txid) || inWallet() || !queued() {
		t.Fatalf("sent %s: in the mempool %v, known to the watch-only wallet %v, in the private queue %v; want only queued", txid, slices.Contains(pool, txid), inWallet(), queued())
	}
	t.Logf("sent privately: sendrawtransaction answered %s at height %d; it is not in the node's mempool, the watch-only wallet does not know it, getprivatebroadcastinfo lists it; the settlement is recorded broadcast", txid, height)

	const marker = "Payment review needed for order "
	told := func() (staff, notes, seen int) {
		t.Helper()
		staff = agInt(e, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,$1)>0 AND strpos(n.body,$2)>0",
			marker+shortID(order), escrowStallMark)
		notes = agInt(e, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)=1", order, escrowStallMark)
		seen = agInt(e, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)=1", order, escrowSeenMark)
		return
	}
	adminSays := func() bool {
		t.Helper()
		w := e.do("GET", "/admin", adminSess, nil)
		if w.Code != 200 {
			t.Fatalf("admin page: %d", w.Code)
		}
		return strings.Contains(html.UnescapeString(w.Body.String()), "Bitcoin escrow settlements queued or stuck: not seen by the network.")
	}
	poll()
	poll()
	mine(escrowStallBlocks - 1)
	poll()
	if staff, notes, _ := told(); staff+notes != 0 || adminSays() {
		t.Fatalf("%d blocks after the broadcast: %d staff notices, %d notes, admin line %v", escrowStallBlocks-1, staff, notes, adminSays())
	}
	mine(1)
	poll()
	staff, notes, _ := told()
	if staff < 2 || notes != 1 || !adminSays() {
		t.Fatalf("%d blocks after the broadcast: %d staff notices, %d notes, admin line %v", escrowStallBlocks, staff, notes, adminSays())
	}
	poll()
	mine(2)
	poll()
	e.DB.QueryRow("SELECT state FROM settlements WHERE id=$1", id).Scan(&state)
	call("", "getrawmempool", &pool)
	if s2, n2, _ := told(); s2 != staff || n2 != 1 || state != "broadcast" || slices.Contains(pool, txid) || inWallet() || !queued() {
		t.Fatalf("stalled: %d staff notices, %d notes, settlement %s, in the mempool %v, in the wallet %v, queued %v", s2, n2, state, slices.Contains(pool, txid), inWallet(), queued())
	}
	t.Logf("stalled: at %d blocks unseen %d staff notices and %d order note (queued or stuck); two more blocks and passes told nobody again; the settlement stays broadcast, still queued, never in the mempool", escrowStallBlocks, staff, notes)

	// The transaction reaches a block (generateblock with the stored raw transaction, standing in for the network).
	var blk struct {
		Hash string `json:"hash"`
	}
	call("", "generateblock", &blk, mineTo, []string{raw})
	poll()
	if s3, n3, seen := told(); s3 != staff || n3 != 1 || seen != 1 || adminSays() || !inWallet() {
		t.Fatalf("seen: %d staff notices, %d stall notes, %d seen notes, admin line %v, in the wallet %v", s3, n3, seen, adminSays(), inWallet())
	}
	mine(2)
	poll()
	e.DB.QueryRow("SELECT state FROM settlements WHERE id=$1", id).Scan(&state)
	if state != "confirmed" {
		t.Fatalf("after 3 confirmations: %s", state)
	}
	t.Logf("seen: generateblock %s mined the stored transaction %s; the watch-only wallet has it, one note cleared the alert and the /admin line went; at 3 confirmations the settlement is confirmed", blk.Hash, txid)
	if agInt(e, "SELECT count(*) FROM payouts") != 0 {
		t.Fatal("a payouts row for an escrow order")
	}
}
