package market

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestEscrowRegtestPerOrderRepair is the real-chain leg of the watch-only wallet repair of per-order (shape (b)) escrows
// after QA5W4A (scripts/test-escrow-regtest.sh; not run in CI), from QA5W4A's probe: a shape-(b) escrow created through
// the checkout form against Bitcoin Core, funded at its address and at two
// sibling addresses, then the app restarted onto empty watch-only wallets standing in for ones restored from a backup
// older than the escrow. Signer up: one watcher pass imports everything again (the app asks getwalletinfo first and
// makes one import), every address is held, labelled and solvable, and the deposits made meanwhile are found. Signer down
// (QA5W5A-1, where QA5W4A-1 made the repair wait): the market's order wallet key is stored at checkout with the parties',
// so one pass imports everything just the same, the siblings solvable. Vendor's key re-registered (QA5W4A-5): the
// siblings are still solvable, rebuilt from the order wallet keys stored at checkout. Finally Core's own behaviour behind QA5W4A-1 and
// -2: an address an addr(…) import held first stays unsolvable after its descriptor (bare or ranged) is imported, and the
// app's orphan-sweep read (watchKeys) reports it unsolvable. The App reaches Core only as the rpcwhitelist user; no
// private key is logged.
func TestEscrowRegtestPerOrderRepair(t *testing.T) {
	appURL, opURL := os.Getenv("ESCROW_REGTEST_APP_RPC_URL"), os.Getenv("ESCROW_REGTEST_OPERATOR_RPC_URL")
	if appURL == "" || opURL == "" {
		t.Skip("set ESCROW_REGTEST_APP_RPC_URL and ESCROW_REGTEST_OPERATOR_RPC_URL (scripts/test-escrow-regtest.sh) to run against bitcoind -regtest")
	}
	ctx := context.Background()
	op, err := newRPCClient("regtest operator", opURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newRPCClient("regtest app", appURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*rpcClient{"operator": op, "app": app} {
		checkRealChain(t, ctx, name, c)
	}
	call := func(wallet, method string, out any, params ...any) {
		t.Helper()
		path := ""
		if wallet != "" {
			path = "/wallet/" + wallet
		}
		if params == nil {
			params = []any{}
		}
		if err := op.callWithin(ctx, regtestOperatorTimeout, path, method, params, out); err != nil {
			t.Fatalf("operator %s: %v", method, err)
		}
	}
	withSum := func(d string) string {
		t.Helper()
		var info struct {
			Checksum string `json:"checksum"`
		}
		call("", "getdescriptorinfo", &info, d)
		return d + "#" + info.Checksum
	}
	s := randomToken()[:8]
	arbiter, watch, funder := "po-repair-arbiter-"+s, "po-repair-watch-"+s, "po-repair-funder-"+s
	restored := map[string]string{}
	for _, w := range []string{"up", "down", "rekey", "addr"} {
		restored[w] = "po-repair-" + w + "-" + s
		call("", "createwallet", nil, restored[w], true, true)
	}
	call("", "createwallet", nil, arbiter)
	call("", "createwallet", nil, watch, true, true)
	call("", "createwallet", nil, funder)
	// A party's seed wallet's BIP48 account key with origin, as Core's getdescriptorinfo writes the public form (its
	// root key stays in the node).
	party := func(name string) string {
		t.Helper()
		call("", "createwallet", nil, name)
		var priv struct {
			Descriptors []struct {
				Desc     string `json:"desc"`
				Internal bool   `json:"internal"`
			} `json:"descriptors"`
		}
		call(name, "listdescriptors", &priv, true)
		root := ""
		for _, d := range priv.Descriptors {
			if strings.HasPrefix(d.Desc, "wpkh(tprv") && !d.Internal {
				root = strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/", 2)[0]
			}
		}
		if root == "" {
			t.Fatalf("%s: no wpkh root key", name)
		}
		var imported []struct {
			Success bool `json:"success"`
		}
		call(name, "importdescriptors", &imported, []map[string]any{{"desc": withSum("wpkh(" + root + "/48h/1h/0h/2h/0/*)"), "active": false, "range": []int{0, 10}, "timestamp": "now"}})
		if len(imported) != 1 || !imported[0].Success {
			t.Fatalf("%s importdescriptors: %+v", name, imported)
		}
		var pub struct {
			Descriptors []struct {
				Desc string `json:"desc"`
			} `json:"descriptors"`
		}
		call(name, "listdescriptors", &pub)
		key := ""
		for _, d := range pub.Descriptors {
			if strings.Contains(d.Desc, "/48h/1h/0h/2h]") {
				key = strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/0/*", 2)[0]
			}
		}
		if k, err := parseEscrowKey(key, keyNetTest); err != nil || !k.BIP48 {
			t.Fatalf("%s key %q: %v", name, key, err)
		}
		return key
	}
	buyerKey, vendorKey := party("po-repair-buyer-"+s), party("po-repair-vendor-"+s)

	f := newFakeSigner(t, signerKey(t, 7))
	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("ESCROW_SIGNER_SOCKET", f.path)
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	e := newTestApp(t)
	if e.A.escrow.Load() == nil || e.A.escrowSignerState() != signerReady {
		t.Fatalf("escrow did not start against the node (signer %q)", e.A.escrowSignerState())
	}
	buyer, buyerSess := e.user("po_repair_b_"+s, "buyer")
	vendor, _ := e.user("po_repair_v_"+s, "vendor")
	storeKey := func(uid, key, sql string) {
		t.Helper()
		k := mustParseKey(t, key)
		sealed, err := e.A.seal("escrow-xpub", k.XPub)
		if err != nil {
			t.Fatal(err)
		}
		agExec(e, sql, uid, sealed, e.A.escrowKeyHMAC(k), k.Fingerprint, k.PathText)
	}
	for uid, key := range map[string]string{buyer: buyerKey, vendor: vendorKey} {
		storeKey(uid, key, `INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,active_at,layout) VALUES($1,$2,$3,$4,$5,now(),'per_order')`)
	}
	product := e.product(vendor, "digital")
	agExec(e, "UPDATE products SET delivery_content='licence key PO-REPAIR' WHERE id=$1", product)
	order := e.order(buyer, product, "BTC", stateDraft)
	var refund string
	call(funder, "getnewaddress", &refund)
	if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {refund}}); w.Code != 303 {
		t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
	}
	var addr string
	if err := e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
		t.Fatal(err)
	}
	xpubs := agStrings(t, e, "SELECT p.party||' '||coalesce(p.per_order_xpub,'-') FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=$1 ORDER BY p.party", order)
	marketIndex := agInt64(t, e, "SELECT market_index FROM escrows WHERE order_id=$1", order)
	for _, r := range xpubs {
		party, xpub, _ := strings.Cut(r, " ")
		if !strings.HasPrefix(xpub, "tpub") {
			t.Fatalf("per_order_xpub of the %s: %q", party, xpub)
		}
		// Core derives the escrow's stored key from the stored order wallet key (the market's, QA5W5A-1, from the
		// signer's rooted account key at <m>/0/0).
		key, path := map[string]string{"buyer": buyerKey, "vendor": vendorKey}[party], "/2/0/0"
		if party == "market" {
			key, path = f.key.Rooted(versionTpub), "/"+strconv.FormatInt(marketIndex, 10)+"/0/0"
		}
		var got, want []string
		call("", "deriveaddresses", &got, withSum("wpkh("+xpub+"/0/0)"))
		call("", "deriveaddresses", &want, withSum("wpkh("+key+path+")"))
		if len(got) != 1 || len(want) != 1 || got[0] != want[0] {
			t.Fatalf("Core derives %v from the %s's stored order wallet key at /0/0 and %v from the account key at %s", got, party, want, path)
		}
	}
	t.Logf("checkout: escrow %s; per_order_xpub stored for the buyer, the vendor and the market (Core derives /0/0 of each as the account key's /<i>/0/0)", addr)
	sibling := func(branch, k int) string {
		t.Helper()
		got := agStrings(t, e, "SELECT s.address FROM escrow_siblings s JOIN escrows es ON es.id=s.escrow_id WHERE es.order_id=$1 AND s.branch=$2 AND s.idx=$3", order, branch, k)
		if len(got) != 1 {
			t.Fatalf("sibling /%d/%d: %v", branch, k, got)
		}
		return got[0]
	}
	s01, s13, s19 := sibling(0, 1), sibling(1, 3), sibling(1, 9)
	var mineTo string
	call(funder, "getnewaddress", &mineTo)
	matureFunds(t, ctx, op, mineTo)
	var price int64
	if err := e.DB.QueryRow("SELECT amount FROM orders WHERE id=$1", order).Scan(&price); err != nil {
		t.Fatal(err)
	}
	var txid string
	call(funder, "sendtoaddress", &txid, addr, json.Number(strconv.FormatFloat(float64(price)/1e8, 'f', 8, 64)))
	call(funder, "sendtoaddress", &txid, s01, json.Number("0.001"))
	call(funder, "sendtoaddress", &txid, s13, json.Number("0.0005"))
	mustMine(t, ctx, op, 3, mineTo)

	type addrInfo struct {
		IsMine   bool     `json:"ismine"`
		Solvable bool     `json:"solvable"`
		Labels   []string `json:"labels"`
		PubKeys  []string `json:"pubkeys"`
	}
	info := func(wallet, a string) addrInfo {
		t.Helper()
		var i addrInfo
		call(wallet, "getaddressinfo", &i, a)
		return i
	}
	held := func(wallet, name, a string) {
		t.Helper()
		i := info(wallet, a)
		t.Logf("%s, %s %s: ismine %v solvable %v labels %v keys %d", wallet[:len(wallet)-len(s)-1], name, a, i.IsMine, i.Solvable, i.Labels, len(i.PubKeys))
		if !i.IsMine || !i.Solvable || len(i.PubKeys) != 3 || len(i.Labels) != 1 || i.Labels[0] != "escrow:"+order {
			t.Errorf("%s is not held, solvable with its three keys and labelled in %s", name, wallet)
		}
	}
	onto := func(wallet string) {
		t.Helper()
		t.Setenv("ESCROW_BTC_WATCH_WALLET", wallet)
		e.restart()
		if e.A.escrow.Load() == nil {
			t.Fatalf("escrow off after the restart onto %s", wallet)
		}
	}

	// Signer up: one pass repairs everything; the deposits made meanwhile are found.
	onto(restored["up"])
	if err := e.A.escrowPollOnce(ctx); err != nil {
		t.Fatalf("watcher pass after the restore: %v", err)
	}
	if !e.A.escrowSiblingsChecked.Load() {
		t.Fatal("the repair did not finish in one pass with the signer up")
	}
	for name, a := range map[string]string{"escrow": addr, "/0/1": s01, "/1/3": s13, "/1/9": s19} {
		held(restored["up"], name, a)
	}
	rows := agStrings(t, e, "SELECT (address=$2)::text||' '||amount||' left_out='||escrow_left_out||' credited='||credited FROM payments WHERE order_id=$1 ORDER BY amount", order, addr)
	t.Logf("payments after the repair: %v", rows)
	if len(rows) != 3 {
		t.Errorf("payments %v, want the escrow deposit and two sibling deposits", rows)
	}

	// Signer down (QA5W5A-1): the market's order wallet key is the stored one, so one pass repairs everything as above.
	signerDown(f, true)
	onto(restored["down"])
	if err := e.A.escrowPollOnce(ctx); err != nil || !e.A.escrowSiblingsChecked.Load() {
		t.Fatalf("pass with the signer down: %v", err)
	}
	for name, a := range map[string]string{"escrow": addr, "/0/1": s01, "/1/3": s13, "/1/9": s19} {
		held(restored["down"], name, a)
	}
	t.Logf("signer down: one pass repaired the escrow address and every sibling, solvable")
	signerDown(f, false)

	// The vendor registers another key (QA5W4A-5): the order wallet is rebuilt from what checkout stored.
	storeKey(vendor, altEscrowKey(t, vendorKey, "5eed5eed", 3), "UPDATE user_escrow_keys SET xpub_sealed=$2,xpub_hmac=$3,origin_fingerprint=$4,origin_path=$5 WHERE user_id=$1")
	onto(restored["rekey"])
	if err := e.A.escrowPollOnce(ctx); err != nil {
		t.Fatalf("pass after the vendor's key changed: %v", err)
	}
	for name, a := range map[string]string{"escrow": addr, "/0/1": s01, "/1/9": s19} {
		held(restored["rekey"], name, a)
	}

	// Core's behaviour behind QA5W4A-1 and -2: addr(…) first, then the escrow's bare descriptor and the order wallet's
	// ranged descriptor (as checkout imported them): both addresses stay unsolvable, and watchKeys says so.
	descs := agStrings(t, e, "SELECT party||'='||pubkey FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=$1 ORDER BY party='market',party='vendor'", order)
	var keys []string
	for _, d := range descs {
		_, k, _ := strings.Cut(d, "=")
		keys = append(keys, k)
	}
	var importReqs []map[string]any
	for _, a := range []string{addr, s01} {
		importReqs = append(importReqs, map[string]any{"desc": withSum("addr(" + a + ")"), "timestamp": 0, "label": "escrow:" + order, "active": false})
	}
	var imported []struct {
		Success bool `json:"success"`
	}
	call(restored["addr"], "importdescriptors", &imported, importReqs)
	var xb, xv string
	for _, r := range xpubs {
		party, xpub, _ := strings.Cut(r, " ")
		switch party {
		case "buyer":
			xb = xpub
		case "vendor":
			xv = xpub
		}
	}
	market, err := e.A.perOrderXpub(ctx, agInt64(t, e, "SELECT id FROM escrows WHERE order_id=$1", order), "market")
	if err != nil {
		t.Fatal(err)
	}
	ranged := withSum("wsh(sortedmulti(2," + xb + "/0/*," + xv + "/0/*," + market.Rooted(versionTpub) + "/0/*))")
	call(restored["addr"], "importdescriptors", &imported, []map[string]any{
		{"desc": withSum("wsh(sortedmulti(2," + strings.Join(keys, ",") + "))"), "timestamp": 0, "label": "escrow:" + order, "active": false},
		{"desc": ranged, "timestamp": 0, "range": []int{0, 19}, "active": false},
	})
	for _, r := range imported {
		if !r.Success {
			t.Fatalf("operator importdescriptors after addr(…): %+v", imported)
		}
	}
	for name, a := range map[string]string{"escrow (bare descriptor after addr(…))": addr, "/0/1 (ranged descriptor after addr(…))": s01} {
		i := info(restored["addr"], a)
		t.Logf("addr(…) first, then the descriptor: %s: ismine %v solvable %v keys %d", name, i.IsMine, i.Solvable, len(i.PubKeys))
		if !i.IsMine || i.Solvable || len(i.PubKeys) != 0 {
			t.Errorf("%s: Core no longer keeps an address addr(…) held first unsolvable: %+v", name, i)
		}
	}
	onto(restored["addr"])
	esc := e.A.escrow.Load()
	for _, a := range []string{addr, s01} {
		if keys, err := esc.watchKeys(ctx, a); keys != nil || !errors.Is(err, errWatchUnsolvable) {
			t.Errorf("watchKeys of %s held by addr(…) first: %d keys, %v", a, len(keys), err)
		}
	}
	t.Logf("watchKeys of both: %v", errWatchUnsolvable)
}
