package market

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestEscrowRegtestPhysical is escrow slice P2-b's real-chain leg (scripts/test-escrow-regtest.sh; not run in CI): a
// physical escrow order's release (P2-R1, P2-R2) against a Bitcoin Core regtest node, with the App reaching Core only as
// the rpcwhitelist user, through a counting proxy that records every RPC the App makes, and every party signature made
// by that party's own Core wallet. The vendor marks the funded order shipped with a fresh claim address (the release is
// requested and built); the vendor signs the release in their wallet, and a watcher pass leaves it waiting for the
// buyer; the buyer signs it in their wallet, and the next pass finalizes the two signatures alone, stores the final
// transaction (cosigned 'parties'), completes the order and broadcasts it. The arbiter wallet is never asked to sign
// (walletprocesspsbt 0, keypoolrefill 0). Mined, the settlement is confirmed and each output on chain is the plan's.
func TestEscrowRegtestPhysical(t *testing.T) {
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
		var info struct {
			Chain string `json:"chain"`
		}
		if err := c.call(ctx, "", "getblockchaininfo", []any{}, &info); err != nil || info.Chain != "regtest" {
			t.Fatalf("refusing: the %s node URL is not on regtest (or is unreachable)", name)
		}
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
		if err := op.call(ctx, path, method, params, out); err != nil {
			t.Fatalf("operator %s: %v", method, err)
		}
	}
	// The App's RPCs pass through this proxy, which counts each method per wallet path (credentials pass untouched).
	target, err := url.Parse(appURL)
	if err != nil {
		t.Fatal(err)
	}
	creds := target.User
	target.User = nil
	var mu sync.Mutex
	counts := map[string]int{}
	forward := httputil.NewSingleHostReverseProxy(target)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req struct {
			Method string `json:"method"`
		}
		json.Unmarshal(body, &req)
		mu.Lock()
		counts[r.URL.Path+" "+req.Method]++
		mu.Unlock()
		forward.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	count := func(path, method string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[path+" "+method]
	}
	s := randomToken()[:8]
	arbiter, watch, funder := "p2b-arbiter-"+s, "p2b-watch-"+s, "p2b-funder-"+s
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
	buyerWallet, vendorWallet := "p2b-buyer-"+s, "p2b-vendor-"+s
	buyerKey, vendorKey := bip48Wallet(buyerWallet), bip48Wallet(vendorWallet)
	newAddr := func() string {
		t.Helper()
		var a string
		call(funder, "getnewaddress", &a)
		return a
	}
	feeAddr := newAddr()
	proxied := "http://" + creds.String() + "@" + strings.TrimPrefix(proxy.URL, "http://")
	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", proxied)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "1500")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", feeAddr)
	t.Setenv("ESCROW_BTC_FEE_RATE", "")
	t.Setenv("PAYMENT_CONFIRMATIONS_BTC", "3")
	t.Setenv("PAYMENT_EXPIRY", "")
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	e := newTestApp(t)
	if e.A.escrow.Load() == nil {
		t.Fatal("escrow did not start against the regtest node")
	}
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
	buyer, buyerSess := activate("rphys_buyer", "buyer", buyerKey, buyerWallet)
	vendor, vendorSess := activate("rphys_vendor", "vendor", vendorKey, vendorWallet)
	_, pgpPub := testPGPKey(t, "rphys_vendor_"+s) // A-83: the buyer can send the vendor an encrypted shipping address
	e.setKey(vendor, pgpPub)
	var mineTo string
	call(funder, "getnewaddress", &mineTo)
	call("", "generatetoaddress", nil, 101, mineTo)
	mine := func(n int) { t.Helper(); call("", "generatetoaddress", nil, n, mineTo) }
	poll := func() {
		t.Helper()
		if err := e.A.escrowPollOnce(ctx); err != nil {
			t.Fatalf("watcher pass: %v", err)
		}
	}
	mempool := func() []string {
		t.Helper()
		var m []string
		call("", "getrawmempool", &m)
		return m
	}
	walletSign := func(wallet, psbt string) string {
		t.Helper()
		var signed struct {
			PSBT     string `json:"psbt"`
			Complete bool   `json:"complete"`
		}
		call(wallet, "walletprocesspsbt", &signed, psbt, true, "ALL", true, false)
		if signed.Complete {
			t.Fatal("one party's signature completed the escrow spend")
		}
		return signed.PSBT
	}
	arbiterSigns := func() int {
		return count("/wallet/"+arbiter, "walletprocesspsbt") + count("/wallet/"+arbiter, "keypoolrefill")
	}

	// A physical listing with a dispatch window; the buyer pays its escrow above the price, and it is funded.
	product := e.product(vendor, "physical")
	if _, err := e.DB.Exec("UPDATE products SET dispatch_days=3 WHERE id=$1", product); err != nil {
		t.Fatal(err)
	}
	order := e.order(buyer, product, "BTC", stateDraft)
	refund := newAddr()
	if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {refund}}); w.Code != 303 {
		t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
	}
	var addr, fund string
	e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr)
	call(funder, "sendtoaddress", &fund, addr, json.Number("0.0012"))
	mine(3)
	poll()
	var state string
	e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&state)
	if state != statePaid {
		t.Fatalf("funded physical escrow order: %s", state)
	}
	t.Logf("physical escrow order %s funded by %s (0.0012 BTC, 3 blocks): paid", shortID(order), fund)

	// The vendor marks it shipped with a fresh claim address; the release is requested and built.
	claimTo := newAddr()
	if w := e.do("POST", "/escrow/ship", vendorSess, url.Values{"order_id": {order}, "address": {claimTo}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("ship: %d %s", w.Code, w.Body.String())
	}
	poll()
	var id int64
	var txid, stored string
	if err := e.DB.QueryRow("SELECT id,txid,psbt FROM settlements WHERE order_id=$1 AND kind='release' AND state='awaiting_signature'", order).Scan(&id, &txid, &stored); err != nil {
		t.Fatalf("release not built while shipped: %v", err)
	}
	planned := agStrings(t, e, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
	var dec struct {
		Tx struct {
			TxID string `json:"txid"`
		} `json:"tx"`
	}
	call("", "decodepsbt", &dec, stored)
	if dec.Tx.TxID != txid || len(planned) != 3 || !strings.Contains(planned[0], claimTo) {
		t.Fatalf("Core reads the release as %s (stored %s); planned %q", dec.Tx.TxID, txid, planned)
	}
	t.Logf("shipped: release %s built while the order is shipped (Core's decodepsbt agrees on its txid); planned outputs %q", txid, planned)
	sid := strconv.FormatInt(id, 10)
	copyOf := func(sess string) string {
		t.Helper()
		w := e.do("GET", "/escrow-psbt?id="+order+"&s="+sid, sess, nil)
		if w.Code != 200 {
			t.Fatalf("copy: %d", w.Code)
		}
		return strings.TrimSpace(w.Body.String())
	}

	// The vendor signs in their own wallet; one pass leaves it waiting for the buyer, with no arbiter call.
	if w := e.do("POST", "/escrow/sign", vendorSess, url.Values{"order_id": {order}, "settlement_id": {sid}, "psbt": {walletSign(vendorWallet, copyOf(vendorSess))}}); w.Code != 303 {
		t.Fatalf("vendor signs the release: %d %s", w.Code, w.Body.String())
	}
	poll()
	e.DB.QueryRow("SELECT state FROM settlements WHERE id=$1", id).Scan(&state)
	if state != "party_signed" || arbiterSigns() != 0 || slices.Contains(mempool(), txid) {
		t.Fatalf("the vendor's signature alone: %s, arbiter calls %d", state, arbiterSigns())
	}
	t.Log("the vendor signed in their own Core wallet; after a watcher pass the release waits for the buyer (party_signed), no arbiter call")

	// The buyer signs in their own wallet; the next pass finalizes the two signatures, completes the order, broadcasts.
	if w := e.do("POST", "/escrow/sign", buyerSess, url.Values{"order_id": {order}, "settlement_id": {sid}, "psbt": {walletSign(buyerWallet, copyOf(buyerSess))}}); w.Code != 303 {
		t.Fatalf("buyer signs the release: %d %s", w.Code, w.Body.String())
	}
	poll()
	var cosigned, orderState string
	e.DB.QueryRow("SELECT s.state,COALESCE(s.cosigned,''),o.state FROM settlements s JOIN orders o ON o.id=s.order_id WHERE s.id=$1", id).Scan(&state, &cosigned, &orderState)
	if state != "broadcast" || cosigned != "parties" || orderState != stateCompleted || !slices.Contains(mempool(), txid) {
		t.Fatalf("both signatures: settlement %s %s, order %s, in mempool %v", state, cosigned, orderState, slices.Contains(mempool(), txid))
	}
	if n := count("/wallet/"+arbiter, "walletprocesspsbt"); n != 0 || arbiterSigns() != 0 {
		t.Fatalf("the arbiter wallet was asked to sign: walletprocesspsbt %d", n)
	}
	// The proxy does see the App's traffic: the market child key came from the arbiter wallet at checkout, and the
	// release was finalized and sent through it.
	if count("/wallet/"+arbiter, "getrawchangeaddress") < 1 || count("", "finalizepsbt")+count("/", "finalizepsbt") < 1 ||
		count("", "sendrawtransaction")+count("/", "sendrawtransaction") < 1 {
		t.Fatalf("the counting proxy did not see the App's RPCs: %v", counts)
	}
	t.Logf("the buyer signed in their own Core wallet; one pass finalized the two signatures alone (cosigned parties), completed the order and broadcast %s; arbiter walletprocesspsbt calls: %d, keypoolrefill: %d",
		txid, count("/wallet/"+arbiter, "walletprocesspsbt"), count("/wallet/"+arbiter, "keypoolrefill"))

	mine(3)
	poll()
	e.DB.QueryRow("SELECT state FROM settlements WHERE id=$1", id).Scan(&state)
	if state != "confirmed" {
		t.Fatalf("after 3 blocks: %s", state)
	}
	for i, r := range planned {
		f := strings.Fields(r)
		want, _ := strconv.ParseInt(f[2], 10, 64)
		var out *struct {
			Value        json.Number `json:"value"`
			ScriptPubKey struct {
				Address string `json:"address"`
			} `json:"scriptPubKey"`
		}
		call("", "gettxout", &out, txid, i, false)
		if out == nil {
			t.Fatalf("output %d of the release is not an unspent output on the chain", i)
		}
		got, _ := parseAmount(string(out.Value), 8)
		if out.ScriptPubKey.Address != f[1] || got != want {
			t.Fatalf("output %d on chain: %s %d, planned %s %d", i, out.ScriptPubKey.Address, got, f[1], want)
		}
	}
	if n := count("/wallet/"+arbiter, "walletprocesspsbt"); n != 0 {
		t.Fatalf("arbiter walletprocesspsbt calls at the end: %d", n)
	}
	t.Logf("release %s confirmed (3 blocks); each output on chain (gettxout) is the plan's; arbiter walletprocesspsbt calls over the whole leg: 0", txid)
}
