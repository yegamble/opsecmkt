package market

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A-440 (P4-R6) real-chain legs (scripts/test-escrow-regtest.sh; CI's escrow-regtest job): the market key protection
// on a Bitcoin Core regtest node.

// regtestOperatorTimeout bounds one call the regtest legs make as the operator (their call helpers). QA4W5-7: under
// rpcTimeout (10 s) a generatetoaddress, or a getwalletinfo just after blocks of thousands of dust outputs, failed legs
// on a loaded host; the operator's own bitcoin-cli waits as long as Core takes.
const regtestOperatorTimeout = 120 * time.Second

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
		if err := op.callWithin(ctx, regtestOperatorTimeout, path, method, params, out); err != nil {
			t.Fatalf("operator %s: %v", method, err)
		}
	}, op
}

// TestEscrowRegtestPooled is A-440's leg (a): the pooled RPC user, restricted with the rpcwhitelist line the operator
// guide documents (pooledRPCMethods), is refused listdescriptors true and gethdkeys {"private":true} (QA4W3-1) on the
// escrow arbiter wallet on the same node, while the operator user is not (a count, never printed); likewise
// walletprocesspsbt, which the operator user is allowed (an empty PSBT then fails decoding, -22). With escrow and the
// pooled provider both configured
// against that node, escrow starts (the startup self-check got Core's real HTTP 403), and the pooled provider still
// works through every whitelisted method: the node check after an unloaded wallet (loadwallet), a deposit address, a
// deposit credited by a watcher pass, the wallet's balance (getbalances, A-445), an address check and a payout sent by
// the watcher, confirmed on chain.
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
	}{{"listdescriptors", []any{true}}, {"listdescriptors", []any{false}}, {"gethdkeys", []any{map[string]any{"private": true}}}, {"gethdkeys", []any{map[string]any{"private": false}}},
		{"walletprocesspsbt", []any{"", false}}, {"dumpwallet", []any{"/tmp/a440-dump-" + s}}, {"backupwallet", []any{"/tmp/a440-pooled-" + s}}} {
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
	// QA4W3-1: the other key methods the self-check probes, as the unrestricted operator user: gethdkeys
	// {"private":true} exports the master key too, and walletprocesspsbt with an empty PSBT is not refused (403 or -18)
	// but fails decoding it (-22), so the self-check's call can sign nothing.
	var hd []struct {
		Xprv string `json:"xprv"`
	}
	call(arbiter, "gethdkeys", &hd, map[string]any{"private": true})
	if len(hd) == 0 || !strings.HasPrefix(hd[0].Xprv, "tprv") {
		t.Fatal("control: the operator user got no private key from gethdkeys on the arbiter wallet")
	}
	opRPC, err := newRPCClient("regtest operator", opURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	if class := errorClassOf(classifyEscrowRPC("walletprocesspsbt", opRPC.call(ctx, "/wallet/"+arbiter, "walletprocesspsbt", []any{"", false}, nil))); class != "walletprocesspsbt rpc:-22" {
		t.Fatalf("control: the operator user's walletprocesspsbt of an empty PSBT: %s, want rpc:-22", class)
	}
	t.Logf("control: operator user gethdkeys {\"private\":true} on the arbiter wallet -> %d private master key(s) (not printed); walletprocesspsbt of an empty PSBT -> rpc:-22 (decoded after the whitelist and the wallet)", len(hd))

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
	t.Log("escrow on with BITCOIN_RPC_URL on the same node: the self-check's listdescriptors (private=false), gethdkeys (private=false) and walletprocesspsbt (an empty PSBT) through the pooled client were each refused with HTTP 403")
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
	// A-445: the drain panel's advisory balance, read with getbalances through the whitelisted pooled user.
	confirmed, pending, err := p.Balance(ctx)
	if err != nil || confirmed+pending < 100000 {
		t.Fatalf("pooled getbalances: %d confirmed, %d pending, %v", confirmed, pending, err)
	}
	t.Logf("pooled wallet balance through the pooled user (getbalances): %d sats confirmed, %d pending", confirmed, pending)

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
	if class := e.A.escrowPooledKey.Load(); e.A.escrow.Load() != nil || class == nil || *class != "listdescriptors answered" {
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

// TestEscrowRegtestArbiterDeposit is A-471's leg: the descriptor on an escrow order's page shows the market's child
// key, and its wpkh address belongs to the arbiter wallet (getaddressinfo ismine), so any escrow party can pay it.
// Before A-471 the start check refused an arbiter wallet with any transaction (getwalletinfo txcount), so such a
// payment disabled escrow from the next restart on. Here a regtest wallet pays that address and it is mined (the arbiter
// wallet's txcount is then 1, its one listtransactions entry a "receive"); a restart starts escrow, and so does a
// watcher retry's check. QA4W4A-1: three transactions of 2000 outputs each then pay the same address (6000 entries); a
// restart reads only what is newer than the last passing check's block (the checkpoint, now the tip) and escrow stays
// on, and so does another restart. QA4W6-1: the funder pays the same address replaceably and bumps the fee 1100 times;
// once the arbiter wallet lists every replacement (about 82 MB from the checkpoint), a restart keeps escrow on; once the
// last one is mined (one entry of about 74 KB, its walletconflicts naming the others), a restart keeps it on and moves
// the checkpoint to the tip, and so does the next. Control, behind the dust: the operator spends the first payment from the arbiter
// wallet to the wallet's own change address (createrawtransaction and signrawtransactionwithwallet as the operator
// user; listtransactions lists no send for it, listsinceblock with include_change does), and a restart leaves escrow
// disabled as not dedicated. What "never spent" can and cannot see: the check reads the wallet's own entries, so a
// spend is any transaction spending its coins, change-only ones included; but rpcwhitelist is per method, not per
// wallet, so where the pooled payments share this node a pooled user allowed sendtoaddress can spend coins paid to the
// arbiter wallet (by naming its path) and so turn escrow off at the next check: the operator guide recommends a node of
// escrow's own.
func TestEscrowRegtestArbiterDeposit(t *testing.T) {
	appURL, opURL := os.Getenv("ESCROW_REGTEST_APP_RPC_URL"), os.Getenv("ESCROW_REGTEST_OPERATOR_RPC_URL")
	if appURL == "" || opURL == "" {
		t.Skip("set ESCROW_REGTEST_APP_RPC_URL and ESCROW_REGTEST_OPERATOR_RPC_URL (scripts/test-escrow-regtest.sh) to run against bitcoind -regtest")
	}
	ctx := context.Background()
	call, op := regtestOperator(t, opURL, map[string]string{"app": appURL})
	s := randomToken()[:8]
	arbiter, watch, funder := "a471-arbiter-"+s, "a471-watch-"+s, "a471-funder-"+s
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
	buyerWallet, vendorWallet := "a471-buyer-"+s, "a471-vendor-"+s
	buyerKey, vendorKey := bip48Wallet(buyerWallet), bip48Wallet(vendorWallet)
	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", "")
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
	buyer, buyerSess := activate("a471_buyer", "buyer", buyerKey, buyerWallet)
	vendor, _ := activate("a471_vendor", "vendor", vendorKey, vendorWallet)
	product := e.product(vendor, "digital")
	if _, err := e.DB.Exec("UPDATE products SET delivery_content='regtest licence a471' WHERE id=$1", product); err != nil {
		t.Fatal(err)
	}
	order := e.order(buyer, product, "BTC", stateDraft)
	var refund, mineTo string
	call(funder, "getnewaddress", &refund)
	call(funder, "getnewaddress", &mineTo)
	if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {refund}}); w.Code != 303 {
		t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
	}
	// The market key, as the buyer's order page shows it, and the arbiter wallet's own address for it.
	market := agStrings(t, e, "SELECT p.pubkey FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=$1 AND p.party='market'", order)
	if len(market) != 1 || !strings.Contains(e.do("GET", "/escrow-descriptor?id="+order, buyerSess, nil).Body.String(), market[0]) {
		t.Fatalf("the market key %v is not in the buyer's descriptor", market)
	}
	var marketAddr []string
	call("", "deriveaddresses", &marketAddr, withChecksum("wpkh("+market[0]+")"))
	var mine struct {
		IsMine bool `json:"ismine"`
	}
	call(arbiter, "getaddressinfo", &mine, marketAddr[0])
	if !mine.IsMine {
		t.Fatal("the market child's wpkh address is not the arbiter wallet's")
	}
	arbiterTxs := func() (int64, []string) {
		t.Helper()
		var info struct {
			TxCount int64 `json:"txcount"`
		}
		call(arbiter, "getwalletinfo", &info)
		var list []struct {
			Category string `json:"category"`
		}
		call(arbiter, "listtransactions", &list, "*", 1000, 0, true)
		var cats []string
		for _, l := range list {
			cats = append(cats, l.Category)
		}
		return info.TxCount, cats
	}

	// The payment: any party could make it from the descriptor on its order page.
	matureFunds(t, ctx, op, mineTo)
	var paid string
	call(funder, "sendtoaddress", &paid, marketAddr[0], json.Number("0.0001"))
	mustMine(t, ctx, op, 1, mineTo)
	n, cats := arbiterTxs()
	if n != 1 || strings.Join(cats, ",") != "receive" {
		t.Fatalf("arbiter wallet after the payment: txcount %d, listtransactions %v", n, cats)
	}
	t.Logf("paid 0.0001 BTC to %s, the wpkh address of order %s's market child key (in the buyer's descriptor; arbiter getaddressinfo ismine=true): txid %s, mined; arbiter wallet txcount %d, listtransactions %v",
		marketAddr[0], shortID(order), paid, n, cats)
	e.restart()
	if e.A.escrow.Load() == nil || e.A.escrowStartState != escrowLogOn {
		t.Fatalf("escrow after a restart with a payment to the arbiter wallet: %q", e.A.escrowStartState)
	}
	e.A.escrow.Store(nil)
	e.A.retryEscrow(ctx, &escrowRetry{last: escrowLogUnchecked})
	if e.A.escrow.Load() == nil {
		t.Fatal("a watcher retry's check refused the arbiter wallet that only received")
	}
	t.Log("restart: escrow on (the start check refuses only a wallet that has spent); a watcher retry's check: escrow on")

	// QA4W4A-1: mass dust. Three transactions of 2000 outputs each (1000 sat) pay the market child's address, 6000 more
	// arbiter entries; each check reads only what is newer than the last passing one's block, so restarts keep escrow on.
	var before arbiterCheckpoint
	if v := agStrings(t, e, "SELECT value FROM settings WHERE key=$1", escrowArbiterChecked); len(v) != 1 || json.Unmarshal([]byte(v[0]), &before) != nil || before.Wallet != arbiter {
		t.Fatalf("no checkpoint after the passing checks: %v", v)
	}
	var addrInfo struct {
		ScriptPubKey string `json:"scriptPubKey"`
	}
	call(arbiter, "getaddressinfo", &addrInfo, marketAddr[0])
	script, err := hex.DecodeString(addrInfo.ScriptPubKey)
	if err != nil || len(script) != 22 {
		t.Fatalf("market child scriptPubKey %q", addrInfo.ScriptPubKey)
	}
	const dustOuts, dustTxs = 2000, 3
	coins := map[string]any{}
	for range dustTxs {
		var a string
		call(funder, "getnewaddress", &a)
		coins[a] = json.Number("0.0213") // 2000 x 1000 sat and a 130000 sat fee (about 2 sat/vB): no change output
	}
	var fundTx string
	call(funder, "sendmany", &fundTx, "", coins)
	mustMine(t, ctx, op, 1, mineTo)
	type coin struct {
		TxID   string      `json:"txid"`
		Vout   uint32      `json:"vout"`
		Amount json.Number `json:"amount"`
	}
	var fundOuts []coin
	call(funder, "listunspent", &fundOuts, 1)
	var dustIDs []string
	for _, u := range fundOuts {
		if u.TxID != fundTx || u.Amount.String() != "0.02130000" {
			continue
		}
		prev, _ := hex.DecodeString(u.TxID)
		slices.Reverse(prev)
		raw := binary.LittleEndian.AppendUint32(nil, 2)
		raw = append(append(raw, 1), prev...)
		raw = binary.LittleEndian.AppendUint32(raw, u.Vout)
		raw = append(raw, 0, 0xfd, 0xff, 0xff, 0xff) // no scriptSig; sequence 0xfffffffd
		raw = binary.LittleEndian.AppendUint16(append(raw, 0xfd), dustOuts)
		for range dustOuts {
			raw = append(binary.LittleEndian.AppendUint64(raw, 1000), byte(len(script)))
			raw = append(raw, script...)
		}
		raw = binary.LittleEndian.AppendUint32(raw, 0)
		var signed struct {
			Hex      string `json:"hex"`
			Complete bool   `json:"complete"`
		}
		call(funder, "signrawtransactionwithwallet", &signed, hex.EncodeToString(raw))
		if !signed.Complete {
			t.Fatal("the funder could not sign a dust transaction")
		}
		var id string
		call("", "sendrawtransaction", &id, signed.Hex)
		dustIDs = append(dustIDs, id)
	}
	if len(dustIDs) != dustTxs {
		t.Fatalf("%d dust transactions built, want %d", len(dustIDs), dustTxs)
	}
	mustMine(t, ctx, op, 1, mineTo)
	var tip string
	call("", "getbestblockhash", &tip)
	n, _ = arbiterTxs()
	restarted := time.Now()
	e.restart()
	took := time.Since(restarted)
	var checked arbiterCheckpoint
	if e.A.escrow.Load() == nil || e.A.escrowStartState != escrowLogOn {
		t.Fatalf("escrow after a restart behind %d dust entries: %q", dustOuts*dustTxs, e.A.escrowStartState)
	}
	if v := agStrings(t, e, "SELECT value FROM settings WHERE key=$1", escrowArbiterChecked); len(v) != 1 || json.Unmarshal([]byte(v[0]), &checked) != nil || checked.Block != tip || checked.Wallet != arbiter {
		t.Fatalf("checkpoint after the dust %v, want the tip %s", v, tip)
	}
	restarted = time.Now()
	e.restart()
	if e.A.escrow.Load() == nil {
		t.Fatalf("escrow after a second restart: %q", e.A.escrowStartState)
	}
	t.Logf("dust: %d transactions of %d outputs (1000 sat) to %s, mined (txids %v; arbiter txcount %d); a restart read them from the checkpoint block %s and kept escrow on in %s (checkpoint now the tip %s, birthtime %d); another restart (nothing newer) in %s",
		dustTxs, dustOuts, marketAddr[0], dustIDs, n, before.Block, took.Round(time.Millisecond), tip, checked.BirthTime, time.Since(restarted).Round(time.Millisecond))

	// QA4W6-1: a payer's fee bumps. The funder pays the market child's address again, replaceable, and bumps the fee
	// arbiterBumps times (bumpfee in its own wallet; the arbiter wallet takes no part). While they are unconfirmed, Core
	// 31.1 lists every replacement as an arbiter entry whose walletconflicts names all the others (each entry about 67
	// bytes per bump, the answer about bumps² × 67 bytes, 82 MB here), from any checkpoint. Once the last replacement is
	// mined, the replaced ones are listed by no read, full or from a checkpoint, before or after a node restart (measured
	// on Core 31.1), and the mined one keeps all the others in its walletconflicts: one entry past 64 KiB, which before
	// QA4W6-1 was refused (bad-reply) on every read, so escrow stayed off for good. Here: once the arbiter wallet lists
	// every replacement, a restart keeps escrow on (or waiting on the read, then on), never "has spent"; once the last one
	// is mined, a restart keeps it on and moves the checkpoint to the tip, and so does the next.
	const arbiterBumps = 1100
	answer := func(from string) (int64, time.Duration) { // the size of the arbiter wallet's listsinceblock answer, as the operator
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": "a471", "method": "listsinceblock", "params": []any{from, 1, true, false, true}})
		start := time.Now()
		resp, err := http.Post(strings.TrimSuffix(opURL, "/")+"/wallet/"+arbiter, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("listsinceblock as the operator: %v", err)
		}
		defer resp.Body.Close()
		n, err := io.Copy(io.Discard, resp.Body)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("listsinceblock as the operator: HTTP %d, %v", resp.StatusCode, err)
		}
		return n, time.Since(start)
	}
	checkpoint := func() string {
		t.Helper()
		var c arbiterCheckpoint
		if v := agStrings(t, e, "SELECT value FROM settings WHERE key=$1", escrowArbiterChecked); len(v) != 1 || json.Unmarshal([]byte(v[0]), &c) != nil {
			t.Fatalf("checkpoint %v", v)
		}
		return c.Block
	}
	// restartOn restarts the App and requires escrow on: a check whose read outlives startup is "waiting" and the watcher's
	// retry takes the read's answer; "has spent" (not dedicated), a read failure or any other state fails the leg.
	restartOn := func(what string) (string, time.Duration) {
		t.Helper()
		start := time.Now()
		e.restart()
		state := e.A.escrowStartState
		if e.A.escrow.Load() == nil && state == escrowLogArbiterWait {
			e.A.retryEscrow(ctx, &escrowRetry{last: state})
		}
		if e.A.escrow.Load() == nil {
			t.Fatalf("escrow after a restart %s: %q (arbiter read %v)", what, state, e.A.escrowArbiterRead.Load())
		}
		return state, time.Since(start)
	}
	txcount := func() int64 { // getwalletinfo only: listtransactions over the bloated entries passes rpcMaxBody
		t.Helper()
		var info struct {
			TxCount int64 `json:"txcount"`
		}
		call(arbiter, "getwalletinfo", &info)
		return info.TxCount
	}
	n0 := txcount()
	var rbf struct {
		TxID string `json:"txid"`
	}
	call(funder, "send", &rbf, map[string]any{marketAddr[0]: json.Number("0.0001")}, nil, "unset", 1, map[string]any{"replaceable": true})
	bumped, bumpStart := rbf.TxID, time.Now()
	for range arbiterBumps {
		var b struct {
			TxID string `json:"txid"`
		}
		call(funder, "bumpfee", &b, bumped)
		bumped = b.TxID
	}
	bumpTook := time.Since(bumpStart)
	// The arbiter wallet takes the mempool's notifications behind the bumps: wait until it lists every replacement.
	var n1 int64
	for end := time.Now().Add(3 * time.Minute); ; time.Sleep(time.Second) {
		if n1 = txcount(); n1 >= n0+arbiterBumps+1 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the arbiter wallet lists %d transactions, want %d", n1, n0+arbiterBumps+1)
		}
	}
	from := checkpoint()
	unconfirmed, read := answer(from)
	state, took := restartOn(fmt.Sprintf("behind %d unconfirmed replacements", arbiterBumps))
	t.Logf("QA4W6-1 bumps: the funder paid %s replaceably (txid %s) and bumped it %d times in %s (last %s); arbiter txcount %d; unconfirmed, the arbiter wallet's listsinceblock from the checkpoint %s answers %d bytes in %s; a restart: %q, escrow on in %s",
		marketAddr[0], rbf.TxID, arbiterBumps, bumpTook.Round(time.Second), bumped, n1, from, unconfirmed, read.Round(time.Millisecond), state, took.Round(time.Millisecond))
	mustMine(t, ctx, op, 1, mineTo)
	var tx struct {
		Confirmations int64 `json:"confirmations"`
	}
	call(arbiter, "gettransaction", &tx, bumped, true)
	if tx.Confirmations != 1 {
		t.Fatalf("the last replacement %s: %d confirmations", bumped, tx.Confirmations)
	}
	from = checkpoint()
	mined, read := answer(from)
	state, took = restartOn("once the last replacement is mined")
	call("", "getbestblockhash", &tip)
	if c := checkpoint(); c != tip {
		t.Fatalf("checkpoint after the bumps were mined %s, want the tip %s", c, tip)
	}
	after, readAfter := answer(tip)
	state2, took2 := restartOn("with the checkpoint past the mined replacement")
	t.Logf("QA4W6-1 bumps mined (1 confirmation): listsinceblock from the checkpoint %s answers %d bytes in %s (the mined replacement alone, its walletconflicts naming the %d others); a restart: %q, escrow on in %s, checkpoint now the tip %s; from it %d bytes in %s, and the next restart: %q, escrow on in %s",
		from, mined, read.Round(time.Millisecond), arbiterBumps, state, took.Round(time.Millisecond), tip, after, readAfter.Round(time.Millisecond), state2, took2.Round(time.Millisecond))
	if mined <= rpcStreamMaxValue || mined >= unconfirmed/100 || after >= 1024 {
		t.Fatalf("answers of %d bytes unconfirmed, %d once mined (want one entry past 64 KiB, the replaced ones gone), %d from the tip", unconfirmed, mined, after)
	}

	// Control, behind the dust: a spend from the arbiter wallet, which only its keys (the operator) can make, still
	// disables escrow, even one whose only output goes to the wallet's own change address (listtransactions lists no
	// send for it; listsinceblock with include_change does).
	var arbCoins []coin
	call(arbiter, "listunspent", &arbCoins, 1)
	i := slices.IndexFunc(arbCoins, func(c coin) bool { return c.TxID == paid })
	if i < 0 {
		t.Fatal("the 0.0001 BTC payment is not among the arbiter wallet's coins")
	}
	var own, unsigned string
	call(arbiter, "getrawchangeaddress", &own)
	call("", "createrawtransaction", &unsigned, []any{map[string]any{"txid": paid, "vout": arbCoins[i].Vout}}, []any{map[string]any{own: json.Number("0.00009")}})
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	call(arbiter, "signrawtransactionwithwallet", &signed, unsigned)
	var spent string
	call("", "sendrawtransaction", &spent, signed.Hex)
	mustMine(t, ctx, op, 1, mineTo)
	type entry struct {
		TxID     string `json:"txid"`
		Category string `json:"category"`
	}
	var listed []entry
	call(arbiter, "listtransactions", &listed, "*", 10, 0, true)
	if slices.ContainsFunc(listed, func(l entry) bool { return l.TxID == spent && l.Category == "send" }) {
		t.Fatalf("listtransactions lists a send for the change-only spend %s", spent)
	}
	e.restart()
	if e.A.escrow.Load() != nil || e.A.escrowStartState != escrowLogArbiterUse {
		t.Fatalf("escrow after the arbiter wallet spent: on %v, %q", e.A.escrow.Load() != nil, e.A.escrowStartState)
	}
	t.Logf("control: the operator spent the 0.0001 BTC payment from the arbiter wallet to its own change address %s (txid %s; listtransactions lists no send for it); behind %d dust entries, a restart leaves escrow disabled (%q, listsinceblock not-dedicated)", own, spent, dustOuts*dustTxs, escrowLogArbiterUse)
}

func errorCauseOrNone(err error) string {
	if err == nil {
		return "no error"
	}
	return watcherCause(err)
}
