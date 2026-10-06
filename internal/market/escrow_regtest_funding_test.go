package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestEscrowRegtestFunding is escrow slice P1-b's real-chain leg (scripts/test-escrow-regtest.sh; not run in CI).
// Against a Bitcoin Core regtest node, with the App reaching Core only as the rpcwhitelist user: a buyer and a vendor
// register BIP48 keys from their own Core wallets and activate them by signing the practice transaction; the buyer
// creates an escrow through the real checkout form; Core confirms that the descriptor the order page shows has a
// valid checksum and derives the address shown, that the watch-only wallet imported exactly the bare-key descriptor
// of that address, and that its witness script holds the three expected keys (the buyer's and vendor's children at
// their origins, the market's from the arbiter wallet). A regtest wallet then funds the address, blocks are mined and
// one escrow watcher pass completes the order, with the content delivered and no payouts row.
func TestEscrowRegtestFunding(t *testing.T) {
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
	s := randomToken()[:8]
	arbiter, watch, funder := "p1b-arbiter-"+s, "p1b-watch-"+s, "p1b-funder-"+s
	call("", "createwallet", nil, arbiter)
	call("", "createwallet", nil, watch, true, true) // watch-only: private keys disabled, blank
	call("", "createwallet", nil, funder)
	// bip48Wallet makes a party's own Core wallet holding only a BIP48 account (m/48h/1h/0h/2h) and returns its key
	// expression, as TestEscrowRegtestPracticeSign does for the buyer.
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
	buyerWallet, vendorWallet := "p1b-buyer-"+s, "p1b-vendor-"+s
	buyerKey, vendorKey := bip48Wallet(buyerWallet), bip48Wallet(vendorWallet)

	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", "")
	t.Setenv("PAYMENT_CONFIRMATIONS_BTC", "3")
	t.Setenv("PAYMENT_EXPIRY", "")
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	e := newTestApp(t)
	if e.A.escrow.Load() == nil {
		t.Fatal("escrow did not start against the regtest node (arbiter or watch-only wallet unreadable?)")
	}
	t.Logf("escrow on: arbiter wallet %s, watch-only wallet %s (getwalletinfo: descriptors, private keys disabled)", arbiter, watch)

	// Both parties register and activate their keys with their own wallets (ES-9).
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
		t.Logf("%s: key [%s…] registered and activated by practice signing in wallet %s", handle, key[1:9], wallet)
		return uid, sess
	}
	buyer, buyerSess := activate("rt_buyer", "buyer", buyerKey, buyerWallet)
	vendor, vendorSess := activate("rt_vendor", "vendor", vendorKey, vendorWallet)
	product := e.product(vendor, "digital")
	if _, err := e.DB.Exec("UPDATE products SET delivery_content='regtest licence 0001' WHERE id=$1", product); err != nil {
		t.Fatal(err)
	}
	order := e.order(buyer, product, "BTC", stateDraft)
	var refund string
	call(funder, "getnewaddress", &refund)
	w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {refund}})
	if w.Code != 303 {
		t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
	}
	var addr string
	if err := e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
		t.Fatal(err)
	}
	t.Logf("escrow checkout -> 303; escrow address %s", addr)

	// The descriptor the order page shows each party: Core agrees with its checksum and derives the address shown.
	stored := map[string]string{}
	for _, r := range agStrings(t, e, "SELECT party||' '||pubkey||' '||origin_fingerprint||origin_path||derivation_suffix FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=$1", order) {
		f := strings.Fields(r)
		stored[f[0]] = f[1]
		t.Logf("escrow_pubkeys: %s %s… origin+suffix [%s]", f[0], f[1][:16], f[2])
	}
	for who, sess := range map[string]string{"buyer": buyerSess, "vendor": vendorSess} {
		dl := e.do("GET", "/escrow-descriptor?id="+order, sess, nil)
		desc := strings.TrimSpace(dl.Body.String())
		if dl.Code != 200 || !strings.Contains(e.do("GET", "/order?id="+order, sess, nil).Body.String(), desc) {
			t.Fatalf("%s: descriptor download %d, or not on the order page", who, dl.Code)
		}
		var info struct {
			Descriptor string `json:"descriptor"`
			Checksum   string `json:"checksum"`
		}
		body, sum, _ := strings.Cut(desc, "#")
		call("", "getdescriptorinfo", &info, body)
		var derived []string
		call("", "deriveaddresses", &derived, desc)
		if info.Checksum != sum || !slices.Equal(derived, []string{addr}) {
			t.Fatalf("%s: Core checksum %s (shown %s), address %v (shown %s)", who, info.Checksum, sum, derived, addr)
		}
		t.Logf("%s's descriptor: Core getdescriptorinfo checksum %s = shown; deriveaddresses = the address shown", who, sum)
	}
	// Each party's own wallet derives its escrow key at the origin shown.
	for who, c := range map[string]struct{ key, pub string }{"buyer": {buyerKey, stored["buyer"]}, "vendor": {vendorKey, stored["vendor"]}} {
		var a1, a2 []string
		call("", "deriveaddresses", &a1, withChecksum("wpkh("+c.key+"/0/1)"))
		call("", "deriveaddresses", &a2, withChecksum("wpkh("+c.pub+")"))
		if !slices.Equal(a1, a2) {
			t.Fatalf("%s's child key /0/1 is not the stored key", who)
		}
	}
	// The watch-only wallet holds exactly the bare-key descriptor of this address, and its witness script is the
	// 2-of-3 of the three stored keys.
	var listed struct {
		Descriptors []struct {
			Desc string `json:"desc"`
		} `json:"descriptors"`
	}
	call(watch, "listdescriptors", &listed)
	bare := withChecksum("wsh(sortedmulti(2," + stored["buyer"] + "," + stored["vendor"] + "," + stored["market"] + "))")
	if len(listed.Descriptors) != 1 || listed.Descriptors[0].Desc != bare || strings.Contains(listed.Descriptors[0].Desc, "pub") {
		t.Fatalf("watch-only wallet descriptors %+v, want only %s", listed.Descriptors, bare)
	}
	var ai struct {
		IsMine   bool     `json:"ismine"`
		Solvable bool     `json:"solvable"`
		Script   string   `json:"script"`
		Hex      string   `json:"hex"`
		PubKeys  []string `json:"pubkeys"`
		Sigs     int      `json:"sigsrequired"`
	}
	call(watch, "getaddressinfo", &ai, addr)
	keys := slices.Sorted(slices.Values(ai.PubKeys))
	want := slices.Sorted(slices.Values([]string{stored["buyer"], stored["vendor"], stored["market"]}))
	ws, _ := hex.DecodeString(ai.Hex)
	if !ai.IsMine || !ai.Solvable || ai.Script != "multisig" || ai.Sigs != 2 || !slices.Equal(keys, want) || len(ws) != 105 || ws[0] != 0x52 || ws[103] != 0x53 || ws[104] != 0xae {
		t.Fatalf("watch-only getaddressinfo %+v", ai)
	}
	t.Logf("watch-only wallet: one descriptor, the bare-key escrow; getaddressinfo ismine=%v solvable=%v script=%s sigsrequired=%d, 3 keys = the stored buyer, vendor and market keys", ai.IsMine, ai.Solvable, ai.Script, ai.Sigs)
	// The market key is the arbiter wallet's own child: the arbiter wallet knows it.
	var mk struct {
		IsMine bool `json:"ismine"`
	}
	var marketAddr []string
	call("", "deriveaddresses", &marketAddr, withChecksum("wpkh("+stored["market"]+")"))
	call(arbiter, "getaddressinfo", &mk, marketAddr[0])
	if !mk.IsMine {
		t.Fatal("the market key is not the arbiter wallet's")
	}
	t.Log("market key: the arbiter wallet's own child (getaddressinfo ismine=true on its wpkh address)")

	// Fund the escrow from a regtest wallet, mine, and let the escrow watcher complete the order.
	var mineTo string
	call(funder, "getnewaddress", &mineTo)
	matureFunds(t, ctx, op, mineTo)
	var txid string
	call(funder, "sendtoaddress", &txid, addr, json.Number("0.001"))
	if err := e.A.escrowPollOnce(ctx); err != nil {
		t.Fatalf("watcher pass: %v", err)
	}
	var st string
	e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&st)
	if st != stateAwaitingPayment || agInt(e, "SELECT count(*) FROM payments WHERE order_id=$1 AND txid=$2 AND confirmations=0", order, txid) != 1 {
		t.Fatalf("unconfirmed funding: %s", st)
	}
	t.Logf("funded %s with 0.001 BTC (txid %s); watcher pass at 0 confirmations -> order %s, 1 ledger row", addr, txid, st)
	mustMine(t, ctx, op, 3, mineTo)
	if err := e.A.escrowPollOnce(ctx); err != nil {
		t.Fatalf("watcher pass: %v", err)
	}
	e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&st)
	if st != stateCompleted {
		t.Fatalf("order %s after 3 confirmations", st)
	}
	for _, ev := range agStrings(t, e, "SELECT from_state||' -> '||to_state||' ('||coalesce(actor_id,'system')||'): '||note FROM order_events WHERE order_id=$1 ORDER BY id", order) {
		t.Logf("order event: %s", strings.ReplaceAll(ev, buyer, "buyer"))
	}
	if agInt(e, "SELECT count(*) FROM payouts WHERE order_id=$1", order) != 0 || agInt(e, "SELECT count(*) FROM deliveries WHERE order_id=$1", order) != 1 ||
		agInt(e, "SELECT count(*) FROM payments WHERE order_id=$1 AND credited AND confirmations=3 AND settlement_id IS NULL", order) != 1 {
		t.Fatal("completion: payouts, delivery or ledger")
	}
	t.Log("mined 3 blocks; watcher pass -> completed on funding; content delivered; payments row credited, settlement_id NULL; no payouts row")
}
