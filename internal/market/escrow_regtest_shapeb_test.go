package market

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestEscrowRegtestPerOrderEscrow is A-456a's real-chain leg (scripts/test-escrow-regtest.sh; not run in CI): a shape-(b)
// escrow created through the real checkout form against Bitcoin Core on regtest, with the per-order switch turned on
// for this test only (no user can do this until A-456b). Two Core wallets' seeds stand in for the parties' BIP39 seeds:
// their BIP48 account keys are stored as active per-order wallet keys; a simulated escrow signer serves a rooted account
// key (signing is not exercised). Checkout must pass the app's own Go-against-Core cross-check; then the operator's Core
// derives the escrow address from the parties' private roots at <i>/0/0 and the signer key at <m>/0/0, which must be the
// address stored, and the address Go computes from its own derivation. The watch-only wallet must hold the order
// wallet's sibling addresses (/0/1 and /1/9) and, after the operator pays /0/1 and /1/3 and 3 blocks are mined, one
// escrow watcher pass must record both deposits for the order, left out and not credited, the order still awaiting
// payment. The App reaches Core only as the rpcwhitelist user; no private key is logged.
func TestEscrowRegtestPerOrderEscrow(t *testing.T) {
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
	arbiter, watch, funder := "a456a-arbiter-"+s, "a456a-watch-"+s, "a456a-funder-"+s
	call("", "createwallet", nil, arbiter)
	call("", "createwallet", nil, watch, true, true)
	call("", "createwallet", nil, funder)
	// A party's seed wallet: its root key (kept in memory, never logged) and its BIP48 account key with origin, as
	// Core's getdescriptorinfo writes the public form.
	party := func(name string) (root, key string) {
		t.Helper()
		call("", "createwallet", nil, name)
		var priv struct {
			Descriptors []struct {
				Desc     string `json:"desc"`
				Internal bool   `json:"internal"`
			} `json:"descriptors"`
		}
		call(name, "listdescriptors", &priv, true)
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
		for _, d := range pub.Descriptors {
			if strings.Contains(d.Desc, "/48h/1h/0h/2h]") {
				key = strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/0/*", 2)[0]
			}
		}
		if k, err := parseEscrowKey(key); err != nil || !k.BIP48 {
			t.Fatalf("%s key %q: %v", name, key, err)
		}
		return root, key
	}
	buyerRoot, buyerKey := party("a456a-buyer-" + s)
	vendorRoot, vendorKey := party("a456a-vendor-" + s)
	t.Logf("buyer account %s…, vendor account %s… (BIP48, from two Core wallets' seeds)", buyerKey[:36], vendorKey[:36])

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
		t.Fatalf("escrow did not start against the regtest node (signer %q)", e.A.escrowSignerState())
	}
	buyer, buyerSess := e.user("regtest_b_buyer_"+s, "buyer")
	vendor, _ := e.user("regtest_b_vendor_"+s, "vendor")
	for uid, key := range map[string]string{buyer: buyerKey, vendor: vendorKey} {
		k := mustParseKey(t, key)
		sealed, err := e.A.seal("escrow-xpub", k.XPub)
		if err != nil {
			t.Fatal(err)
		}
		agExec(e, `INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,active_at,layout) VALUES($1,$2,$3,$4,$5,now(),'per_order')`,
			uid, sealed, e.A.escrowKeyHMAC(k), k.Fingerprint, k.PathText)
	}
	product := e.product(vendor, "digital")
	agExec(e, "UPDATE products SET delivery_content='licence key A456A-REGTEST' WHERE id=$1", product)
	order := e.order(buyer, product, "BTC", stateDraft)
	var refund string
	call(funder, "getnewaddress", &refund)
	w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {refund}})
	if w.Code != 303 {
		t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
	}
	var addr string
	var m int64
	if err := e.DB.QueryRow("SELECT address,market_index FROM escrows WHERE order_id=$1", order).Scan(&addr, &m); err != nil {
		t.Fatal(err)
	}
	suffixes := agStrings(t, e, "SELECT p.party||' '||p.origin_fingerprint||p.origin_path||p.derivation_suffix FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=$1 ORDER BY p.party", order)
	t.Logf("checkout (the app's Go-against-Core cross-check passed): escrow %s, keys %v", addr, suffixes)

	// Core, from the parties' private roots and the signer's account key: the same address.
	signerTpub := f.key.Rooted(versionTpub)
	var fromRoots []string
	call("", "deriveaddresses", &fromRoots, withSum("wsh(sortedmulti(2,"+buyerRoot+"/48h/1h/0h/2h/2/0/0,"+vendorRoot+"/48h/1h/0h/2h/2/0/0,"+signerTpub+"/"+strconv.FormatInt(m, 10)+"/0/0))"))
	if len(fromRoots) != 1 || fromRoots[0] != addr {
		t.Fatalf("Core derives %v from the parties' private roots at 2/0/0 and the signer at %d/0/0; the escrow is %s", fromRoots, m, addr)
	}
	// Go, from its own derivation of the three account keys.
	var leaves [][]byte
	for _, k := range []struct {
		key bip32Key
		i   uint32
	}{{accountKey(t, buyerKey), 2}, {accountKey(t, vendorKey), 2}, {f.key, uint32(m)}} {
		_, leaf := perOrderChild(t, k.key, k.i)
		leaves = append(leaves, leaf.PubKey[:])
	}
	if spk, err := escrowAddressScript(realChainNet(), addr); err != nil || !bytes.Equal(spk, p2wshScript(leaves...)) {
		t.Fatalf("Go's derivation does not give %s (%v)", addr, err)
	}
	t.Logf("Go and Core agree: %s = wsh(sortedmulti(2, buyer/48h/1h/0h/2h/2/0/0, vendor/48h/1h/0h/2h/2/0/0, signer/%d/0/0))", addr, m)

	sibling := func(branch, k int) string {
		t.Helper()
		got := agStrings(t, e, "SELECT s.address FROM escrow_siblings s JOIN escrows es ON es.id=s.escrow_id WHERE es.order_id=$1 AND s.branch=$2 AND s.idx=$3", order, branch, k)
		if len(got) != 1 {
			t.Fatalf("sibling /%d/%d: %v", branch, k, got)
		}
		return got[0]
	}
	s01, s13, s19 := sibling(0, 1), sibling(1, 3), sibling(1, 9)
	for _, a := range []string{s01, s19} {
		var info struct {
			IsMine bool `json:"ismine"`
		}
		call(watch, "getaddressinfo", &info, a)
		if !info.IsMine {
			t.Fatalf("the watch-only wallet does not hold sibling %s", a)
		}
	}
	var fromRoots01 []string
	call("", "deriveaddresses", &fromRoots01, withSum("wsh(sortedmulti(2,"+buyerRoot+"/48h/1h/0h/2h/2/0/1,"+vendorRoot+"/48h/1h/0h/2h/2/0/1,"+signerTpub+"/"+strconv.FormatInt(m, 10)+"/0/1))"))
	if len(fromRoots01) != 1 || fromRoots01[0] != s01 {
		t.Fatalf("Core derives %v at /0/1 from the private roots; the stored sibling is %s", fromRoots01, s01)
	}
	t.Logf("watch-only wallet holds siblings /0/1 %s and /1/9 %s (getaddressinfo ismine); Core derives /0/1 from the private roots", s01, s19)

	var mineTo string
	call(funder, "getnewaddress", &mineTo)
	matureFunds(t, ctx, op, mineTo)
	var tx1, tx2 string
	call(funder, "sendtoaddress", &tx1, s01, json.Number("0.001"))
	call(funder, "sendtoaddress", &tx2, s13, json.Number("0.0005"))
	mustMine(t, ctx, op, 3, mineTo)
	if err := e.A.escrowPollOnce(ctx); err != nil {
		t.Fatalf("watcher pass: %v", err)
	}
	rows := agStrings(t, e, "SELECT address||' '||amount||' left_out='||escrow_left_out||' credited='||credited FROM payments WHERE order_id=$1 ORDER BY amount", order)
	want := []string{s13 + " 50000 left_out=true credited=false", s01 + " 100000 left_out=true credited=false"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Fatalf("sibling deposits recorded %v, want %v", rows, want)
	}
	var st string
	if err := e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&st); err != nil || st != stateAwaitingPayment {
		t.Fatalf("order state %q %v", st, err)
	}
	t.Logf("sibling deposits %s (/0/1) and %s (/1/3), 3 confirmations: seen by the watch-only wallet and recorded for the order, left out, not credited; order still %s", tx1, tx2, st)
}
