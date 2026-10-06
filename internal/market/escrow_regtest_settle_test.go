package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestEscrowRegtestSettlement is escrow slice P1-c1's real-chain leg (scripts/test-escrow-regtest.sh; not run in
// CI). Against a Bitcoin Core regtest node, with the App reaching Core only as the rpcwhitelist user:
//
//   - claim: an escrow is funded above its price and completes; the vendor requests the claim; one escrow watcher
//     pass builds it (gettxout, createpsbt, utxoupdatepsbt); the vendor signs its own copy in its own Core wallet
//     (walletprocesspsbt) and pastes it: the settlement is party_signed. Adversarial pastes from real Core first:
//     SIGHASH_NONE, and the buyer's wallet's signature pasted by the vendor, are refused.
//   - refund: an underpaid escrow is cancelled by its buyer; the refund is built and signed in the buyer's own wallet.
//
// For each, before the App's watcher runs again, Core checks the market's half by hand: the stored PSBT with the
// market's signature (the arbiter wallet's walletprocesspsbt, called by this test) combined with the stored party
// signatures finalizes, has the stored txid and outputs, and testmempoolaccept allows it; the test broadcasts nothing.
// (Since P1-c2 a later watcher pass co-signs and broadcasts such a settlement itself: TestEscrowRegtestCosign.)
func TestEscrowRegtestSettlement(t *testing.T) {
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
	arbiter, watch, funder := "p1c-arbiter-"+s, "p1c-watch-"+s, "p1c-funder-"+s
	call("", "createwallet", nil, arbiter)
	call("", "createwallet", nil, watch, true, true)
	call("", "createwallet", nil, funder)
	// A party's own Core wallet holding only a BIP48 account (m/48h/1h/0h/2h), as the P1-a and P1-b legs make it.
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
	buyerWallet, vendorWallet := "p1c-buyer-"+s, "p1c-vendor-"+s
	buyerKey, vendorKey := bip48Wallet(buyerWallet), bip48Wallet(vendorWallet)
	newAddr := func() string {
		t.Helper()
		var a string
		call(funder, "getnewaddress", &a)
		return a
	}
	feeAddr := newAddr()

	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
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
		t.Fatal("escrow did not start against the regtest node (arbiter or watch-only wallet unreadable?)")
	}
	t.Logf("escrow on: arbiter wallet %s, watch-only wallet %s, market fee 1500 bps to a funder-wallet address, fee rate %d sat/vB", arbiter, watch, e.A.escrow.Load().feeRate)
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
	buyer, buyerSess := activate("rc_buyer", "buyer", buyerKey, buyerWallet)
	vendor, vendorSess := activate("rc_vendor", "vendor", vendorKey, vendorWallet)
	t.Log("buyer and vendor keys registered from their own Core wallets and activated by practice signing")
	var mineTo string
	call(funder, "getnewaddress", &mineTo)
	matureFunds(t, ctx, op, mineTo)

	// escrowOrder checks out a new escrow order paying refund and funds it with btc from the funder wallet.
	escrowOrder := func(refund, btc string) (order, addr, txid string) {
		t.Helper()
		product := e.product(vendor, "digital")
		if _, err := e.DB.Exec("UPDATE products SET delivery_content='regtest licence 0002' WHERE id=$1", product); err != nil {
			t.Fatal(err)
		}
		order = e.order(buyer, product, "BTC", stateDraft)
		if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {refund}}); w.Code != 303 {
			t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
		}
		if err := e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
			t.Fatal(err)
		}
		call(funder, "sendtoaddress", &txid, addr, json.Number(btc))
		mustMine(t, ctx, op, 3, mineTo)
		if err := e.A.escrowPollOnce(ctx); err != nil {
			t.Fatalf("watcher pass: %v", err)
		}
		return order, addr, txid
	}
	state := func(order string) (id int64, st, stored, txid string) {
		t.Helper()
		if err := e.DB.QueryRow("SELECT id,state,COALESCE(psbt,''),COALESCE(txid,'') FROM settlements WHERE order_id=$1 AND kind IN ('release','refund')", order).Scan(&id, &st, &stored, &txid); err != nil {
			t.Fatal(err)
		}
		return
	}
	partyCopy := func(order, sess string, id int64) string {
		t.Helper()
		w := e.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10), sess, nil)
		if w.Code != 200 {
			t.Fatalf("copy: %d", w.Code)
		}
		return strings.TrimSpace(w.Body.String())
	}
	walletSign := func(wallet, psbt, sighash string) (string, bool) {
		t.Helper()
		var signed struct {
			PSBT     string `json:"psbt"`
			Complete bool   `json:"complete"`
		}
		call(wallet, "walletprocesspsbt", &signed, psbt, true, sighash, true, false)
		return signed.PSBT, signed.Complete
	}
	paste := func(order, sess string, id int64, psbt string) (int, string) {
		w := e.do("POST", "/escrow/sign", sess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {psbt}})
		return w.Code, w.Body.String()
	}
	// wouldFinalize is what slice P1-c2 will do, done by this test with the operator's RPC user: the stored PSBT with
	// the market key's origin, signed by the arbiter wallet with SIGHASH_ALL, combined (market copy first) with the
	// stored copy carrying the stored party signatures, finalized and checked by testmempoolaccept. Never broadcast.
	wouldFinalize := func(order string, id int64, stored, txid string) {
		t.Helper()
		keys, err := escrowKeys(ctx, e.DB, order)
		if err != nil {
			t.Fatal(err)
		}
		marketCopy, err := settlementCopy(stored, keys["market"])
		if err != nil {
			t.Fatal(err)
		}
		marketSigned, complete := walletSign(arbiter, marketCopy, "ALL")
		if complete {
			t.Fatal("the market's signature alone completed the escrow spend")
		}
		sp, err := decodePSBT(stored)
		if err != nil {
			t.Fatal(err)
		}
		raw := sp.Raw
		for _, r := range agStrings(t, e, "SELECT input_n||' '||pubkey||' '||signature FROM settlement_signatures WHERE settlement_id=$1 ORDER BY input_n", id) {
			f := strings.Fields(r)
			n, _ := strconv.Atoi(f[0])
			pub, _ := hex.DecodeString(f[1])
			sig, _ := hex.DecodeString(f[2])
			cur, err := parsePSBT(raw)
			if err != nil {
				t.Fatal(err)
			}
			if raw, err = cur.withInputField(n, append([]byte{0x02}, pub...), sig); err != nil {
				t.Fatal(err)
			}
		}
		var combined string
		call("", "combinepsbt", &combined, []string{marketSigned, encodePSBT(raw)})
		var fin struct {
			Hex      string `json:"hex"`
			Complete bool   `json:"complete"`
		}
		call("", "finalizepsbt", &fin, combined, true)
		if !fin.Complete {
			t.Fatal("the stored PSBT with the party's and the market's signatures does not finalize")
		}
		var tx struct {
			TxID string `json:"txid"`
			Vout []struct {
				Value        json.Number `json:"value"`
				ScriptPubKey struct {
					Address string `json:"address"`
				} `json:"scriptPubKey"`
			} `json:"vout"`
		}
		call("", "decoderawtransaction", &tx, fin.Hex)
		var got []string
		for _, o := range tx.Vout {
			sats, _ := parseAmount(string(o.Value), 8)
			got = append(got, o.ScriptPubKey.Address+" "+strconv.FormatInt(sats, 10))
		}
		want := agStrings(t, e, "SELECT address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
		if tx.TxID != txid || !slices.Equal(got, want) {
			t.Fatalf("final tx %s outputs %q; stored txid %s outputs %q", tx.TxID, got, txid, want)
		}
		var accept []struct {
			Allowed bool   `json:"allowed"`
			Reject  string `json:"reject-reason"`
		}
		call("", "testmempoolaccept", &accept, []string{fin.Hex}, feeRateParam(escrowMaxFeeRate))
		if len(accept) != 1 || !accept[0].Allowed {
			t.Fatalf("testmempoolaccept %+v", accept)
		}
		var mempool []string
		call("", "getrawmempool", &mempool)
		if slices.Contains(mempool, txid) {
			t.Fatal("the settlement reached the mempool: nothing may broadcast it in this slice")
		}
		t.Logf("Core: stored PSBT + arbiter walletprocesspsbt (test only) + stored party signatures -> finalizepsbt complete, txid %s = stored, outputs = stored %q, testmempoolaccept allowed; not broadcast (not in the mempool)", txid, want)
	}

	// Claim leg: 0.0012 BTC for a 0.001 BTC price.
	orderA, addrA, fundA := escrowOrder(newAddr(), "0.0012")
	var st string
	e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", orderA).Scan(&st)
	if st != stateCompleted {
		t.Fatalf("claim order %s after funding", st)
	}
	t.Logf("claim leg: escrow %s funded with 0.0012 BTC (txid %s), 3 blocks, watcher pass -> order completed", addrA, fundA)
	claimTo := newAddr()
	if w := e.do("POST", "/escrow/claim", vendorSess, url.Values{"order_id": {orderA}, "address": {claimTo}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	if err := e.A.escrowPollOnce(ctx); err != nil {
		t.Fatalf("watcher pass: %v", err)
	}
	idA, stA, storedA, txidA := state(orderA)
	if stA != "awaiting_signature" {
		t.Fatalf("claim %s", stA)
	}
	var dec struct {
		Fee json.Number `json:"fee"`
		Tx  struct {
			TxID string `json:"txid"`
		} `json:"tx"`
	}
	call("", "decodepsbt", &dec, storedA)
	var fee int64
	e.DB.QueryRow("SELECT network_fee FROM settlements WHERE id=$1", idA).Scan(&fee)
	coreFee, _ := parseAmount(string(dec.Fee), 8)
	if dec.Tx.TxID != txidA || coreFee != fee {
		t.Fatalf("Core decodepsbt: txid %s fee %d; stored %s fee %d", dec.Tx.TxID, coreFee, txidA, fee)
	}
	t.Logf("claim built: settlement %d awaiting_signature, unsigned txid %s; Core decodepsbt agrees on the txid and the fee (%d sat); outputs %q", idA, txidA, fee,
		agStrings(t, e, "SELECT role||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", idA))
	vendorCopy := partyCopy(orderA, vendorSess, idA)
	// Adversarial pastes signed by real Core wallets are refused and change nothing.
	none, _ := walletSign(vendorWallet, vendorCopy, "NONE")
	if code, body := paste(orderA, vendorSess, idA, none); code != 400 || !strings.Contains(body, "sighash type other than ALL") {
		t.Fatalf("SIGHASH_NONE paste: %d %s", code, body)
	}
	buyerSigned, _ := walletSign(buyerWallet, partyCopy(orderA, buyerSess, idA), "ALL")
	if code, body := paste(orderA, vendorSess, idA, buyerSigned); code != 400 || !strings.Contains(body, "a key other than your escrow key") {
		t.Fatalf("the buyer's signature pasted by the vendor: %d %s", code, body)
	}
	if _, st, _, _ := state(orderA); st != "awaiting_signature" || agInt(e, "SELECT count(*) FROM settlement_signatures WHERE settlement_id=$1", idA) != 0 {
		t.Fatal("a refused paste changed the settlement")
	}
	t.Log("refused: the vendor's SIGHASH_NONE signature from Core, and the buyer wallet's signature pasted by the vendor")
	vendorSigned, complete := walletSign(vendorWallet, vendorCopy, "ALL")
	if complete {
		t.Fatal("the vendor's signature alone completed the escrow spend")
	}
	if code, body := paste(orderA, vendorSess, idA, vendorSigned); code != 303 {
		t.Fatalf("vendor paste: %d %s", code, body)
	}
	if _, st, after, _ := state(orderA); st != "party_signed" || after != storedA {
		t.Fatalf("after the vendor's paste: %s (stored copy changed: %v)", st, after != storedA)
	}
	t.Logf("vendor's own Core wallet signed its copy (walletprocesspsbt ALL, complete=false); pasted -> 303; settlement party_signed by the vendor")
	wouldFinalize(orderA, idA, storedA, txidA)

	// Refund leg: 0.0004 BTC of 0.001 BTC; the buyer cancels.
	refundTo := newAddr()
	orderB, addrB, fundB := escrowOrder(refundTo, "0.0004")
	if w := e.do("POST", "/orders/cancel", buyerSess, url.Values{"order_id": {orderB}, "from": {stateAwaitingPayment}, "amount_seen": {"40000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("buyer cancel: %d %s", w.Code, w.Body.String())
	}
	if err := e.A.escrowPollOnce(ctx); err != nil {
		t.Fatalf("watcher pass: %v", err)
	}
	idB, stB, storedB, txidB := state(orderB)
	if stB != "awaiting_signature" || agInt(e, "SELECT count(*) FROM settlement_outputs WHERE settlement_id=$1 AND role='claimant' AND address=$2", idB, refundTo) != 1 {
		t.Fatalf("refund %s", stB)
	}
	t.Logf("refund leg: escrow %s funded with 0.0004 BTC (txid %s); buyer cancelled -> refund requested; watcher pass -> settlement %d awaiting_signature, txid %s, paying the pinned refund address", addrB, fundB, idB, txidB)
	buyerRefund, _ := walletSign(buyerWallet, partyCopy(orderB, buyerSess, idB), "ALL")
	if code, body := paste(orderB, buyerSess, idB, buyerRefund); code != 303 {
		t.Fatalf("buyer paste: %d %s", code, body)
	}
	if _, st, _, _ := state(orderB); st != "party_signed" {
		t.Fatalf("refund after the buyer's paste: %s", st)
	}
	t.Log("buyer's own Core wallet signed the refund; pasted -> 303; settlement party_signed by the buyer")
	wouldFinalize(orderB, idB, storedB, txidB)
	if agInt(e, "SELECT count(*) FROM payouts WHERE order_id IN ($1,$2)", orderA, orderB) != 0 {
		t.Fatal("a payouts row for an escrow order")
	}
}
