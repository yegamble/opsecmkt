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

// A-440 (P4-R6) real-chain legs (scripts/test-escrow-regtest.sh; CI's escrow-regtest job): the market key protection
// on a Bitcoin Core regtest node.

// regtestOperator checks that every URL in urls reports the legs' chain (checkRealChain, before any wallet call) and
// returns a helper that calls Core as the operator user (full rights, standing in for the operator's own bitcoin-cli),
// and that user's client (for mustMine and matureFunds).
func regtestOperator(t *testing.T, opURL string, urls map[string]string) (func(wallet, method string, out any, params ...any), *rpcClient) {
	t.Helper()
	ctx := context.Background()
	op, err := newRPCClient("regtest operator", opURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	urls["operator"] = opURL
	for name, u := range urls {
		c, err := newRPCClient("regtest "+name, u, "1.0", false)
		if err != nil {
			t.Fatal(err)
		}
		checkRealChain(t, ctx, name, c)
	}
	return func(wallet, method string, out any, params ...any) {
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
	}, op
}

// TestEscrowRegtestPooled is A-440's leg (a): the pooled RPC user, restricted with the rpcwhitelist line the operator
// guide documents (pooledRPCMethods), is refused listdescriptors true on the escrow arbiter wallet on the same node,
// while the operator user is not (a count, never printed). With escrow and the pooled provider both configured
// against that node, escrow starts (the startup self-check got Core's real HTTP 403), and the pooled provider still
// works through every whitelisted method: the node check after an unloaded wallet (loadwallet), a deposit address, a
// deposit credited by a watcher pass, an address check and a payout sent by the watcher, confirmed on chain.
func TestEscrowRegtestPooled(t *testing.T) {
	appURL, opURL, pooledURL := os.Getenv("ESCROW_REGTEST_APP_RPC_URL"), os.Getenv("ESCROW_REGTEST_OPERATOR_RPC_URL"), os.Getenv("ESCROW_REGTEST_POOLED_RPC_URL")
	if appURL == "" || opURL == "" || pooledURL == "" {
		t.Skip("set ESCROW_REGTEST_APP_RPC_URL, ESCROW_REGTEST_OPERATOR_RPC_URL and ESCROW_REGTEST_POOLED_RPC_URL (scripts/test-escrow-regtest.sh) to run against bitcoind -regtest")
	}
	ctx := context.Background()
	call, op := regtestOperator(t, opURL, map[string]string{"app": appURL, "pooled": pooledURL})
	pooledRPC, err := newRPCClient("regtest pooled", pooledURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	s := randomToken()[:8]
	arbiter, watch, poolW, funder := "a440p-arbiter-"+s, "a440p-watch-"+s, "a440p-pooled-"+s, "a440p-funder-"+s
	call("", "createwallet", nil, arbiter)
	call("", "createwallet", nil, watch, true, true)
	call("", "createwallet", nil, poolW)
	call("", "createwallet", nil, funder)

	// The pooled user may not export, back up or even list the arbiter wallet's descriptors.
	for _, c := range []struct {
		method string
		params []any
	}{{"listdescriptors", []any{true}}, {"listdescriptors", []any{false}}, {"dumpwallet", []any{"/tmp/a440-dump-" + s}}, {"backupwallet", []any{"/tmp/a440-pooled-" + s}}} {
		err := classifyEscrowRPC(c.method, pooledRPC.call(ctx, "/wallet/"+arbiter, c.method, c.params, nil))
		var re *escrowRPCError
		if !errors.As(err, &re) || re.Class != "forbidden" {
			t.Fatalf("pooled user %s %v on the arbiter wallet: %v (want forbidden, HTTP 403)", c.method, c.params, err)
		}
		t.Logf("whitelist: pooled user %s %v on the arbiter wallet -> %s (HTTP 403)", c.method, c.params, re.Class)
	}
	var priv struct {
		Descriptors []struct {
			Desc string `json:"desc"`
		} `json:"descriptors"`
	}
	call(arbiter, "listdescriptors", &priv, true)
	n := 0
	for _, d := range priv.Descriptors {
		if strings.Contains(d.Desc, "tprv") {
			n++
		}
	}
	if n == 0 {
		t.Fatal("control: the operator user got no private descriptor from the arbiter wallet")
	}
	t.Logf("control: operator user (no rpcwhitelist line, rpcwhitelistdefault=0) listdescriptors true on the arbiter wallet -> %d private descriptors (not printed)", n)

	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", "")
	t.Setenv("ESCROW_SIGNER_SOCKET", "")
	t.Setenv("BITCOIN_RPC_URL", pooledURL)
	t.Setenv("BITCOIN_WALLET", poolW)
	t.Setenv("BITCOIN_CHAIN", realChainNet())
	t.Setenv("PAYMENT_CONFIRMATIONS_BTC", "1")
	t.Setenv("PAYMENT_EXPIRY", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	e := newTestApp(t)
	if e.A.escrow.Load() == nil || e.A.escrowPooledKey.Load() != nil {
		t.Fatal("escrow did not start with the pooled user whitelisted on the same node")
	}
	t.Log("escrow on with BITCOIN_RPC_URL on the same node: the self-check's listdescriptors (private=false) through the pooled client was refused with HTTP 403")
	p, ok := e.A.payments["BTC"].(*bitcoinProvider)
	if !ok {
		t.Fatalf("the pooled Bitcoin provider is not available: %v", e.A.unavailable["BTC"])
	}

	// loadwallet: the pooled wallet unloaded (a node restart without load_on_startup) is loaded again by the check.
	call("", "unloadwallet", nil, poolW)
	if _, err := p.Check(ctx); err != nil {
		t.Fatalf("pooled check after the wallet was unloaded: %v", err)
	}
	var loaded []string
	call("", "listwallets", &loaded)
	if !strings.Contains(" "+strings.Join(loaded, " ")+" ", " "+poolW+" ") {
		t.Fatalf("the pooled wallet was not loaded again: %v", loaded)
	}
	version, _ := p.WalletVersion()
	t.Logf("pooled check (getblockchaininfo, getwalletinfo, loadwallet, getnetworkinfo) after unloadwallet -> loaded again; %s", version)

	// A pooled order: its deposit address from the pooled wallet, paid by another wallet and credited by a watcher pass.
	suffix := randomToken()[:6]
	buyer, _ := e.user("a440_buyer_"+suffix, "buyer")
	vendor, _ := e.user("a440_vendor_"+suffix, "vendor")
	order := e.order(buyer, e.product(vendor, "physical"), "BTC", stateAwaitingPayment)
	addr, err := p.NewAddress(ctx, order)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'BTC',$2,$3)", order, addr, "btc-"+realChainNet()); err != nil {
		t.Fatal(err)
	}
	var mineTo string
	call(funder, "getnewaddress", &mineTo)
	matureFunds(t, ctx, op, mineTo)
	var deposit string
	call(funder, "sendtoaddress", &deposit, addr, json.Number("0.00100000"))
	mustMine(t, ctx, op, 1, mineTo)
	if err := e.A.pollOnce(ctx); err != nil {
		t.Fatalf("pooled watcher pass: %v", err)
	}
	var state string
	e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&state)
	if state != statePaid {
		t.Fatalf("pooled order after its deposit confirmed: %s", state)
	}
	t.Logf("pooled order %s: deposit address from getnewaddress, %s credited by a watcher pass (listreceivedbyaddress, gettransaction) -> paid", shortID(order), deposit)

	// Its payout, sent by the watcher from the pooled wallet as the whitelisted user, confirmed on chain.
	var payTo string
	call(funder, "getnewaddress", &payTo)
	if !p.ValidAddress(payTo) {
		t.Fatal("validateaddress through the pooled user refused a regtest address")
	}
	if _, err := e.DB.Exec("UPDATE orders SET state='completed' WHERE id=$1", order); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec("INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state) VALUES($1,'release',$2,'BTC',90000,$3,'pending')", order, vendor, payTo); err != nil {
		t.Fatal(err)
	}
	if err := e.A.pollOnce(ctx); err != nil {
		t.Fatalf("pooled watcher pass with a payout: %v", err)
	}
	var payState, txid string
	e.DB.QueryRow("SELECT state,COALESCE(txid,'') FROM payouts WHERE order_id=$1", order).Scan(&payState, &txid)
	if payState != "sent" || txid == "" {
		t.Fatalf("payout %s %q", payState, txid)
	}
	mustMine(t, ctx, op, 1, mineTo)
	var got struct {
		Confirmations int64       `json:"confirmations"`
		Amount        json.Number `json:"amount"`
	}
	call(funder, "gettransaction", &got, txid)
	sats, _ := parseAmount(strings.TrimPrefix(string(got.Amount), "-"), 8)
	if got.Confirmations < 1 || sats <= 0 || sats > 90000 {
		t.Fatalf("payout %s on chain: %+v", txid, got)
	}
	t.Logf("pooled payout of 0.0009 BTC sent by the watcher (validateaddress, sendtoaddress) as %s; received %d sats after the network fee, %d confirmation(s)", txid, sats, got.Confirmations)

	// Control: with an unrestricted user as BITCOIN_RPC_URL (the operator's, as before A-440's whitelist), the
	// self-check gets an answer and escrow stays disabled.
	t.Setenv("BITCOIN_RPC_URL", opURL)
	e.restart()
	if class := e.A.escrowPooledKey.Load(); e.A.escrow.Load() != nil || class == nil || *class != "answered" {
		t.Fatalf("escrow with an unrestricted pooled user: on %v, self-check %v", e.A.escrow.Load() != nil, class)
	}
	t.Log("control: BITCOIN_RPC_URL as the unrestricted operator user -> the self-check's listdescriptors answered; escrow stays disabled")
}

// TestEscrowRegtestArbiterRestore is A-440's leg (b): the arbiter wallet backed up as the operator user right after it
// is created (backupwallet, the block height recorded), the private descriptors exported by the operator for the
// fallback (never printed); an escrow is created, funded and completed, the vendor's claim is built and signed by the
// vendor's own Core wallet. The arbiter wallet is then wiped from the node (unloadwallet; the harness cannot delete
// files inside the node, so it restores under a new name, as onto a fresh node) and a watcher pass co-signs nothing.
// restorewallet brings it back: after a restart the fingerprint pin is unchanged and the watcher co-signs, broadcasts
// and confirms the claim. Then the importdescriptors fallback (a blank wallet, the exported descriptors with timestamp
// "now", as on a pruned node) co-signs a second claim with the pin unchanged.
func TestEscrowRegtestArbiterRestore(t *testing.T) {
	appURL, opURL := os.Getenv("ESCROW_REGTEST_APP_RPC_URL"), os.Getenv("ESCROW_REGTEST_OPERATOR_RPC_URL")
	if appURL == "" || opURL == "" {
		t.Skip("set ESCROW_REGTEST_APP_RPC_URL and ESCROW_REGTEST_OPERATOR_RPC_URL (scripts/test-escrow-regtest.sh) to run against bitcoind -regtest")
	}
	ctx := context.Background()
	call, op := regtestOperator(t, opURL, map[string]string{"app": appURL})
	s := randomToken()[:8]
	arbiter, watch, funder := "a440r-arbiter-"+s, "a440r-watch-"+s, "a440r-funder-"+s
	call("", "createwallet", nil, arbiter)
	call("", "createwallet", nil, watch, true, true)
	call("", "createwallet", nil, funder)

	// The backup, as the runbook's operator steps take it (encryption to the operator's key happens off the node).
	var height int64
	call("", "getblockcount", &height)
	backup := "/tmp/a440r-arbiter-" + s + ".bak"
	call(arbiter, "backupwallet", nil, backup)
	var exported struct {
		Descriptors []struct {
			Desc     string `json:"desc"`
			Active   bool   `json:"active"`
			Internal bool   `json:"internal"`
			Range    []int  `json:"range"`
			Next     int    `json:"next_index"`
		} `json:"descriptors"`
	}
	call(arbiter, "listdescriptors", &exported, true)
	t.Logf("arbiter wallet backed up by the operator user (backupwallet) at height %d; %d private descriptors exported for the fallback (not printed)", height, len(exported.Descriptors))

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
	buyerWallet, vendorWallet := "a440r-buyer-"+s, "a440r-vendor-"+s
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
	t.Setenv("ESCROW_SIGNER_SOCKET", "")
	t.Setenv("PAYMENT_CONFIRMATIONS_BTC", "3")
	t.Setenv("PAYMENT_EXPIRY", "")
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	e := newTestApp(t)
	if e.A.escrow.Load() == nil {
		t.Fatal("escrow did not start against the regtest node")
	}
	pin := func() string {
		t.Helper()
		got := agStrings(t, e, "SELECT value FROM settings WHERE key=$1", escrowFingerprintPin)
		if len(got) != 1 {
			t.Fatalf("fingerprint pin: %v", got)
		}
		return got[0]
	}
	pinned := pin()
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
	buyer, buyerSess := activate("a440r_buyer", "buyer", buyerKey, buyerWallet)
	vendor, vendorSess := activate("a440r_vendor", "vendor", vendorKey, vendorWallet)
	var mineTo string
	call(funder, "getnewaddress", &mineTo)
	matureFunds(t, ctx, op, mineTo)
	mine := func(n int) { t.Helper(); mustMine(t, ctx, op, n, mineTo) }
	poll := func() {
		t.Helper()
		if err := e.A.escrowPollOnce(ctx); err != nil {
			t.Fatalf("watcher pass: %v", err)
		}
	}
	settlement := func(order string) (id int64, state, txid, cosigned string) {
		t.Helper()
		if err := e.DB.QueryRow(`SELECT id,state,COALESCE(txid,''),COALESCE(cosigned,'') FROM settlements WHERE order_id=$1 AND kind='release' AND state<>'superseded' ORDER BY id DESC LIMIT 1`,
			order).Scan(&id, &state, &txid, &cosigned); err != nil {
			t.Fatalf("settlement of %s: %v", order, err)
		}
		return
	}
	// signedClaim: an escrow on a digital listing with automatic delivery, funded and completed; the vendor's claim is
	// built and signed by the vendor's own Core wallet, waiting for the market's co-signature.
	signedClaim := func() (order string, id int64) {
		t.Helper()
		product := e.product(vendor, "digital")
		if _, err := e.DB.Exec("UPDATE products SET delivery_content='regtest licence a440' WHERE id=$1", product); err != nil {
			t.Fatal(err)
		}
		order = e.order(buyer, product, "BTC", stateDraft)
		if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {newAddr()}}); w.Code != 303 {
			t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
		}
		var addr, fund, state string
		e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr)
		call(funder, "sendtoaddress", &fund, addr, json.Number("0.0012"))
		mine(3)
		poll()
		e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&state)
		if state != stateCompleted {
			t.Fatalf("funded escrow order: %s", state)
		}
		if w := e.do("POST", "/escrow/claim", vendorSess, url.Values{"order_id": {order}, "address": {newAddr()}, "password": {testPassword}}); w.Code != 303 {
			t.Fatalf("claim: %d %s", w.Code, w.Body.String())
		}
		poll()
		id, state, _, _ = settlement(order)
		if state != "awaiting_signature" {
			t.Fatalf("claim %s", state)
		}
		w := e.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10), vendorSess, nil)
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
		if w := e.do("POST", "/escrow/sign", vendorSess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {signed.PSBT}}); w.Code != 303 {
			t.Fatalf("vendor signs: %d %s", w.Code, w.Body.String())
		}
		t.Logf("escrow order %s funded (%s) and completed; claim %d built and signed by the vendor's own Core wallet", shortID(order), fund, id)
		return order, id
	}
	// cosigned: after a restart against wallet, escrow is on with the fingerprint pin unchanged, and one watcher pass
	// co-signs and broadcasts the claim, confirmed after 3 blocks.
	cosigned := func(order, wallet, how string) {
		t.Helper()
		t.Setenv("ESCROW_BTC_ARBITER_WALLET", wallet)
		e.restart()
		if e.A.escrow.Load() == nil {
			t.Fatalf("escrow did not start against the %s arbiter wallet", how)
		}
		if got := pin(); got != pinned || e.A.escrow.Load().fingerprint != pinned {
			t.Fatalf("fingerprint pin %s, wallet %s, want %s unchanged", got, e.A.escrow.Load().fingerprint, pinned)
		}
		poll()
		id, state, txid, by := settlement(order)
		if state != "broadcast" || by != "market" {
			t.Fatalf("claim %d after the %s: %s cosigned %q", id, how, state, by)
		}
		mine(3)
		poll()
		if _, state, _, _ = settlement(order); state != "confirmed" {
			t.Fatalf("claim %d after 3 blocks: %s", id, state)
		}
		t.Logf("%s arbiter wallet: escrow on, fingerprint pin %s unchanged; the watcher co-signed claim %d (cosigned=market), broadcast %s, confirmed after 3 blocks", how, pinned, id, txid)
	}

	order, id := signedClaim()
	// Wipe: the arbiter wallet is gone from the node; a watcher pass co-signs nothing.
	call("", "unloadwallet", nil, arbiter)
	err := e.A.escrowPollOnce(ctx)
	if _, state, _, _ := settlement(order); state != "party_signed" {
		t.Fatalf("claim %d with the arbiter wallet gone: %s", id, state)
	}
	t.Logf("arbiter wallet unloaded (wiped from the node): a watcher pass leaves claim %d party_signed, nothing broadcast (pass: %s)", id, errorCauseOrNone(err))
	restored := arbiter + "-restored"
	call("", "restorewallet", nil, restored, backup)
	cosigned(order, restored, "restored (restorewallet)")

	// The importdescriptors fallback: a blank wallet with private keys and the operator-exported descriptors.
	order2, _ := signedClaim()
	call("", "unloadwallet", nil, restored)
	fallback := arbiter + "-fallback"
	call("", "createwallet", nil, fallback, false, true)
	var imports []map[string]any
	for _, d := range exported.Descriptors {
		r := map[string]any{"desc": d.Desc, "active": d.Active, "timestamp": "now"}
		if d.Active {
			r["internal"] = d.Internal
		}
		if len(d.Range) == 2 {
			r["range"], r["next_index"] = d.Range, d.Next
		}
		imports = append(imports, r)
	}
	var results []struct {
		Success bool `json:"success"`
	}
	call(fallback, "importdescriptors", &results, imports)
	for i, r := range results {
		if !r.Success {
			t.Fatalf("importdescriptors: descriptor %d not imported", i)
		}
	}
	t.Logf("fallback: %d descriptors imported into blank wallet %s with timestamp now", len(results), fallback)
	cosigned(order2, fallback, "fallback (importdescriptors)")
}

func errorCauseOrNone(err error) string {
	if err == nil {
		return "no error"
	}
	return watcherCause(err)
}
