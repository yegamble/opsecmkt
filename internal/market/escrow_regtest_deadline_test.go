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

// TestEscrowRegtestDeadline is escrow slice P2-c's real-chain leg (scripts/test-escrow-regtest.sh; not run in CI): a
// physical escrow order released at its deadline (P2-R3) against a Bitcoin Core regtest node, the App reaching Core
// only as the rpcwhitelist user through a counting proxy, every party signature made by that party's own Core wallet.
// The funded order gets its ship-by time; the vendor marks it shipped and signs the release in their wallet, and a
// watcher pass leaves it waiting for the buyer with no arbiter call. The release deadline is then moved into the past
// through SQL (the clock), and one watcher pass completes the order ("auto-release deadline passed"), co-signs the
// vendor's signature with the arbiter wallet and broadcasts it. Mined, the settlement is confirmed and each output on
// chain is the plan's.
func TestEscrowRegtestDeadline(t *testing.T) {
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
		if err := op.callWithin(ctx, regtestOperatorTimeout, path, method, params, out); err != nil {
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
	arbiter, watch, funder := "p2c-arbiter-"+s, "p2c-watch-"+s, "p2c-funder-"+s
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
	buyerWallet, vendorWallet := "p2c-buyer-"+s, "p2c-vendor-"+s
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
	buyer, buyerSess := activate("rdead_buyer", "buyer", buyerKey, buyerWallet)
	vendor, vendorSess := activate("rdead_vendor", "vendor", vendorKey, vendorWallet)
	_, pgpPub := testPGPKey(t, "rdead_vendor_"+s) // A-83: the buyer can send the vendor an encrypted shipping address
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
	arbiterSigns := func() int { return count("/wallet/"+arbiter, "walletprocesspsbt") }

	// A physical listing with a dispatch window; the buyer pays its escrow above the price, and it is funded: the
	// order is paid and gets its ship-by time.
	product := e.product(vendor, "physical")
	if _, err := e.DB.Exec("UPDATE products SET dispatch_days=3 WHERE id=$1", product); err != nil {
		t.Fatal(err)
	}
	order := e.order(buyer, product, "BTC", stateDraft)
	if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {newAddr()}}); w.Code != 303 {
		t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
	}
	var addr, fund, state string
	e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr)
	call(funder, "sendtoaddress", &fund, addr, json.Number("0.0012"))
	mine(3)
	poll()
	var shipBy bool
	e.DB.QueryRow("SELECT o.state,es.ship_by IS NOT NULL FROM orders o JOIN escrows es ON es.order_id=o.id WHERE o.id=$1", order).Scan(&state, &shipBy)
	if state != statePaid || !shipBy {
		t.Fatalf("funded physical escrow order: %s, ship_by set %v", state, shipBy)
	}
	t.Logf("physical escrow order %s funded by %s (0.0012 BTC, 3 blocks): paid, ship-by time set", shortID(order), fund)

	// The vendor marks it shipped; the release is built; the vendor signs it in their own wallet.
	claimTo := newAddr()
	if w := e.do("POST", "/escrow/ship", vendorSess, url.Values{"order_id": {order}, "address": {claimTo}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("ship: %d %s", w.Code, w.Body.String())
	}
	poll()
	var id int64
	var txid string
	if err := e.DB.QueryRow("SELECT id,txid FROM settlements WHERE order_id=$1 AND kind='release' AND state='awaiting_signature'", order).Scan(&id, &txid); err != nil {
		t.Fatalf("release not built while shipped: %v", err)
	}
	planned := agStrings(t, e, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
	sid := strconv.FormatInt(id, 10)
	w := e.do("GET", "/escrow-psbt?id="+order+"&s="+sid, vendorSess, nil)
	if w.Code != 200 {
		t.Fatalf("vendor's copy: %d", w.Code)
	}
	var signed struct {
		PSBT     string `json:"psbt"`
		Complete bool   `json:"complete"`
	}
	call(vendorWallet, "walletprocesspsbt", &signed, strings.TrimSpace(w.Body.String()), true, "ALL", true, false)
	if signed.Complete {
		t.Fatal("the vendor's signature alone completed the escrow spend")
	}
	if w := e.do("POST", "/escrow/sign", vendorSess, url.Values{"order_id": {order}, "settlement_id": {sid}, "psbt": {signed.PSBT}}); w.Code != 303 {
		t.Fatalf("vendor signs the release: %d %s", w.Code, w.Body.String())
	}
	poll()
	var orderState string
	e.DB.QueryRow("SELECT s.state,o.state FROM settlements s JOIN orders o ON o.id=s.order_id WHERE s.id=$1", id).Scan(&state, &orderState)
	if state != "party_signed" || orderState != stateShipped || arbiterSigns() != 0 || slices.Contains(mempool(), txid) {
		t.Fatalf("before the deadline: release %s, order %s, arbiter walletprocesspsbt %d", state, orderState, arbiterSigns())
	}
	t.Logf("shipped: release %s signed by the vendor in their own Core wallet waits for the buyer before the deadline; arbiter walletprocesspsbt calls: 0", txid)

	// The clock: the release deadline moved into the past through SQL (past migration 079's freeze trigger).
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"ALTER TABLE escrows DISABLE TRIGGER escrows_deadlines_frozen", "UPDATE escrows SET release_at=now()-interval '1 minute' WHERE order_id='" + order + "'",
		"ALTER TABLE escrows ENABLE TRIGGER escrows_deadlines_frozen"} {
		if _, err = tx.Exec(q); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	poll()
	var cosigned, note string
	e.DB.QueryRow("SELECT s.state,COALESCE(s.cosigned,''),o.state FROM settlements s JOIN orders o ON o.id=s.order_id WHERE s.id=$1", id).Scan(&state, &cosigned, &orderState)
	e.DB.QueryRow("SELECT note FROM order_events WHERE order_id=$1 AND from_state='shipped' AND to_state='completed' AND actor_id IS NULL", order).Scan(&note)
	if orderState != stateCompleted || !strings.HasPrefix(note, escrowDeadlineNote) || state != "broadcast" || cosigned != "market" || !slices.Contains(mempool(), txid) {
		t.Fatalf("past the deadline: order %s (%q), release %s %s, in mempool %v", orderState, note, state, cosigned, slices.Contains(mempool(), txid))
	}
	if n := arbiterSigns(); n != 1 {
		t.Fatalf("arbiter walletprocesspsbt calls after the deadline: %d, want 1", n)
	}
	t.Logf("past the deadline one watcher pass completed the order (%q), co-signed the vendor's signature with the arbiter wallet (walletprocesspsbt 1) and broadcast %s", note, txid)

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
	t.Logf("release %s confirmed (3 blocks); each output on chain (gettxout) is the plan's %q", txid, planned)
}
