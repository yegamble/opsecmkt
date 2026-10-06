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

// TestEscrowRegtestRecovery is escrow slice P1-d1's real-chain leg (scripts/test-escrow-regtest.sh; not run in CI).
// Against a Bitcoin Core regtest node, with the App reaching Core only as the rpcwhitelist user and every party
// signature made by that party's own Core wallet:
//
//   - A-344, in the mempool and mined: both parties sign a claim on the order page and one of them broadcasts the
//     transaction the two signatures finalize to before the watcher's pass; the watcher records it as broadcast
//     (cosigned=parties), never external, and follows it to confirmed;
//   - a reorganisation: invalidateblock takes a confirmed settlement's block out of the chain, the watcher returns it
//     to broadcast and sends the stored transaction again; reconsiderblock brings the block back and the watcher
//     confirms it again;
//   - a reorganisation and a double-spend: a confirmed settlement's block is invalidated and the parties replace its
//     transaction with one paying elsewhere, which is mined; the watcher records it external with the conflicting txid;
//   - a direct co-spend with no settlement: the parties spend a completed order's escrow from their own wallets; the
//     watcher flags the deposit with the spending txid the watch-only wallet names;
//   - an orphan: a labelled descriptor imported into the watch-only wallet with no escrow row (a rolled-back
//     checkout) is listed on the daily check.
func TestEscrowRegtestRecovery(t *testing.T) {
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
	s := randomToken()[:8]
	arbiter, watch, funder := "p1d1-arbiter-"+s, "p1d1-watch-"+s, "p1d1-funder-"+s
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
	buyerWallet, vendorWallet := "p1d1-buyer-"+s, "p1d1-vendor-"+s
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
	buyer, buyerSess := activate("rd1_buyer", "buyer", buyerKey, buyerWallet)
	vendor, vendorSess := activate("rd1_vendor", "vendor", vendorKey, vendorWallet)
	mineTo := newAddr()
	call("", "generatetoaddress", nil, 101, mineTo)
	mine := func(n int) { t.Helper(); call("", "generatetoaddress", nil, n, mineTo) }
	poll := func() {
		t.Helper()
		if err := e.A.escrowPollOnce(ctx); err != nil {
			t.Fatalf("watcher pass: %v", err)
		}
	}
	completedOrder := func(btc string) (order, addr, fund string) {
		t.Helper()
		product := e.product(vendor, "digital")
		if _, err := e.DB.Exec("UPDATE products SET delivery_content='regtest licence p1d1' WHERE id=$1", product); err != nil {
			t.Fatal(err)
		}
		order = e.order(buyer, product, "BTC", stateDraft)
		if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {newAddr()}}); w.Code != 303 {
			t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
		}
		if err := e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
			t.Fatal(err)
		}
		call(funder, "sendtoaddress", &fund, addr, json.Number(btc))
		mine(3)
		poll()
		var state string
		e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&state)
		if state != stateCompleted {
			t.Fatalf("funded order %s", state)
		}
		return order, addr, fund
	}
	settlementOf := func(order string) (id int64, state, txid, raw, cosigned, spentBy string) {
		t.Helper()
		if err := e.DB.QueryRow(`SELECT id,state,COALESCE(txid,''),COALESCE(raw_tx,''),COALESCE(cosigned,''),COALESCE(spent_by,'') FROM settlements WHERE order_id=$1 AND kind='release'`, order).Scan(&id, &state, &txid, &raw, &cosigned, &spentBy); err != nil {
			t.Fatalf("settlement of %s: %v", order, err)
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
	walletSign := func(wallet, psbt string) string {
		t.Helper()
		var signed struct {
			PSBT     string `json:"psbt"`
			Complete bool   `json:"complete"`
		}
		call(wallet, "walletprocesspsbt", &signed, psbt, true, "ALL", true, false)
		return signed.PSBT
	}
	paste := func(order, sess string, id int64, psbt string) {
		t.Helper()
		w := e.do("POST", "/escrow/sign", sess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {psbt}})
		if w.Code != 303 {
			t.Fatalf("paste: %d %s", w.Code, w.Body.String())
		}
	}
	claimed := func(order string) int64 {
		t.Helper()
		if w := e.do("POST", "/escrow/claim", vendorSess, url.Values{"order_id": {order}, "address": {newAddr()}, "password": {testPassword}}); w.Code != 303 {
			t.Fatalf("claim: %d %s", w.Code, w.Body.String())
		}
		poll()
		id, state, _, _, _, _ := settlementOf(order)
		if state != "awaiting_signature" {
			t.Fatalf("claim %s", state)
		}
		return id
	}
	// partiesFinal is the transaction the two stored party signatures finalize the stored PSBT to: what either party
	// can broadcast from their own node once both have signed.
	partiesFinal := func(id int64) string {
		t.Helper()
		var stored string
		e.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored)
		sp, _ := decodePSBT(stored)
		raw := sp.Raw
		for _, r := range agStrings(t, e, "SELECT input_n||' '||pubkey||' '||signature FROM settlement_signatures WHERE settlement_id=$1 ORDER BY party,input_n", id) {
			f := strings.Fields(r)
			n, _ := strconv.Atoi(f[0])
			pub, _ := hex.DecodeString(f[1])
			sig, _ := hex.DecodeString(f[2])
			cur, _ := parsePSBT(raw)
			raw, _ = cur.withInputField(n, append([]byte{0x02}, pub...), sig)
		}
		var fin struct {
			Hex      string `json:"hex"`
			Complete bool   `json:"complete"`
		}
		call("", "finalizepsbt", &fin, encodePSBT(raw), true)
		if !fin.Complete {
			t.Fatal("the two party signatures do not finalize")
		}
		return fin.Hex
	}
	blockOf := func(txid string) string {
		t.Helper()
		var tx struct {
			BlockHash string `json:"blockhash"`
		}
		call(watch, "gettransaction", &tx, txid, true)
		if tx.BlockHash == "" {
			t.Fatalf("%s is in no block", txid)
		}
		return tx.BlockHash
	}
	stateOf := func(id int64) string {
		var st string
		e.DB.QueryRow("SELECT state FROM settlements WHERE id=$1", id).Scan(&st)
		return st
	}

	// A-344: the parties broadcast the both-signed transaction before the watcher's pass, in the mempool and mined.
	var confirmedID int64
	var confirmedTx, confirmedOrder string
	for _, mined := range []bool{false, true} {
		order, _, fund := completedOrder("0.001")
		id := claimed(order)
		paste(order, vendorSess, id, walletSign(vendorWallet, partyCopy(order, vendorSess, id)))
		paste(order, buyerSess, id, walletSign(buyerWallet, partyCopy(order, buyerSess, id)))
		if st := stateOf(id); st != "party_signed" {
			t.Fatalf("both signed: %s", st)
		}
		raw := partiesFinal(id)
		var sent string
		call("", "sendrawtransaction", &sent, raw)
		where := "in the mempool"
		if mined {
			mine(1)
			where = "mined (1 confirmation)"
		}
		poll()
		_, st, txid, stored, cos, _ := settlementOf(order)
		if st != "broadcast" || txid != sent || stored != raw || cos != "parties" || agInt(e, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", order) != 0 {
			t.Fatalf("A-344 %s: %s %s cosigned %s (raw equal %v)", where, st, txid, cos, stored == raw)
		}
		t.Logf("A-344 leg (%s): escrow funded by %s; both parties signed in their wallets; the parties broadcast %s before the watcher's pass; recorded as broadcast, cosigned=parties, not external", where, fund, sent)
		mine(3)
		poll()
		if st := stateOf(id); st != "confirmed" {
			t.Fatalf("A-344 %s after 3 more blocks: %s", where, st)
		}
		t.Logf("A-344 leg (%s): %s confirmed", where, sent)
		confirmedID, confirmedTx, confirmedOrder = id, sent, order
	}

	// A reorganisation: the confirmed settlement's block leaves the chain, then returns.
	block := blockOf(confirmedTx)
	call("", "invalidateblock", nil, block)
	poll()
	var inMempool []string
	call("", "getrawmempool", &inMempool)
	if st := stateOf(confirmedID); st != "broadcast" || !slices.Contains(inMempool, confirmedTx) {
		t.Fatalf("after invalidateblock: %s, in mempool %v", st, slices.Contains(inMempool, confirmedTx))
	}
	if !strings.Contains(strings.Join(agStrings(t, e, "SELECT note FROM order_events WHERE order_id=$1", confirmedOrder), "\n"), "a reorganisation of the chain took it out of the block that confirmed it") {
		t.Fatal("no reorganisation note")
	}
	t.Logf("reorg leg: invalidateblock %s took %s out of the chain; the watcher returned the settlement to broadcast and sent the stored transaction again (in the mempool)", block, confirmedTx)
	call("", "reconsiderblock", nil, block)
	poll()
	if st := stateOf(confirmedID); st != "confirmed" {
		t.Fatalf("after reconsiderblock: %s", st)
	}
	t.Logf("reorg leg: reconsiderblock %s; %s confirmed again in block %s", block, confirmedTx, blockOf(confirmedTx))

	// A reorganisation and a double-spend: the parties replace the transaction while its block is out of the chain.
	order, _, _ := completedOrder("0.001")
	id := claimed(order)
	paste(order, vendorSess, id, walletSign(vendorWallet, partyCopy(order, vendorSess, id)))
	poll()
	_, st, txid, _, _, _ := settlementOf(order)
	if st != "broadcast" {
		t.Fatalf("claim: %s", st)
	}
	mine(3)
	poll()
	if stateOf(id) != "confirmed" {
		t.Fatalf("claim not confirmed: %s", stateOf(id))
	}
	blockDS := blockOf(txid)
	call("", "invalidateblock", nil, blockDS)
	var fundTxid string
	var fundVout int64
	e.DB.QueryRow("SELECT txid,idx FROM payments WHERE order_id=$1", order).Scan(&fundTxid, &fundVout)
	double := coSpendFromWallets(t, call, e, order, fundTxid, fundVout, newAddr(), "0.00095", buyerWallet, vendorWallet)
	var replacing string
	call("", "sendrawtransaction", &replacing, double)
	mine(1)
	poll()
	_, st, _, _, _, spentBy := settlementOf(order)
	if st != "external" || spentBy != replacing {
		t.Fatalf("reorganisation and double-spend: %s spent_by %s, want %s", st, spentBy, replacing)
	}
	t.Logf("reorg+double-spend leg: confirmed %s, invalidateblock %s, the parties' replacement %s mined instead; the watcher recorded the settlement external with spent_by %s, flagged for staff", txid, blockDS, replacing, spentBy)

	// A direct co-spend with no settlement.
	orderC, addrC, fundC := completedOrder("0.001")
	var voutC int64
	e.DB.QueryRow("SELECT idx FROM payments WHERE order_id=$1", orderC).Scan(&voutC)
	cospend := coSpendFromWallets(t, call, e, orderC, fundC, voutC, newAddr(), "0.00099", buyerWallet, vendorWallet)
	var cospent string
	call("", "sendrawtransaction", &cospent, cospend)
	mine(1)
	poll()
	if agInt(e, "SELECT count(*) FROM payments WHERE order_id=$1 AND spent_outside AND flagged AND spent_by=$2", orderC, cospent) != 1 {
		t.Fatalf("the co-spend %s of %s was not flagged with its txid", cospent, addrC)
	}
	t.Logf("co-spend leg: the parties spent escrow %s (deposit %s) directly from their wallets in %s; the watcher flagged the deposit with spent_by %s", addrC, fundC, cospent, cospent)

	// An orphan: a labelled descriptor in the watch-only wallet with no escrow row (as a rolled-back checkout leaves).
	var keys []string
	for range 3 {
		var info struct {
			PubKey string `json:"pubkey"`
		}
		call(funder, "getaddressinfo", &info, newAddr())
		keys = append(keys, info.PubKey)
	}
	desc := withChecksum("wsh(sortedmulti(2," + strings.Join(keys, ",") + "))")
	var orphanAddrs []string
	call("", "deriveaddresses", &orphanAddrs, desc)
	draft := e.order(buyer, e.product(vendor, "digital"), "BTC", stateDraft)
	call(watch, "importdescriptors", nil, []map[string]any{{"desc": desc, "timestamp": "now", "active": false, "label": "escrow:" + draft}})
	if _, err := e.DB.Exec("UPDATE settings SET value='2000-01-01T00:00:00Z' WHERE key='escrow_btc_stray_check'"); err != nil {
		t.Fatal(err)
	}
	poll()
	if got := agStrings(t, e, "SELECT address||' '||order_id FROM escrow_orphans"); !slices.Equal(got, []string{orphanAddrs[0] + " " + draft}) {
		t.Fatalf("orphans %q, want %s", got, orphanAddrs[0])
	}
	t.Logf("orphan leg: %s imported with label escrow:%s and no escrow row is listed in escrow_orphans after the daily check (listreceivedbyaddress include_empty; no listdescriptors)", orphanAddrs[0], shortID(draft))
	if agInt(e, "SELECT count(*) FROM payouts") != 0 {
		t.Fatal("a payouts row for an escrow order")
	}
}

// coSpendFromWallets is a transaction the buyer's and the vendor's own wallets sign together, spending the escrow
// output txid:vout of order to dest: each party's copy carries its own key origin (its descriptor), the two copies are
// combined and finalized. It is not a settlement of the market.
func coSpendFromWallets(t *testing.T, call func(string, string, any, ...any), e *testEnv, order, txid string, vout int64, dest, btc, buyerWallet, vendorWallet string) string {
	t.Helper()
	var unsigned string
	call("", "createpsbt", &unsigned, []map[string]any{{"txid": txid, "vout": vout}}, []map[string]any{{dest: json.Number(btc)}}, 0, true)
	var signed []string
	for party, wallet := range map[string]string{roleBuyer: buyerWallet, roleVendor: vendorWallet} {
		desc, err := escrowDescriptorFor(context.Background(), e.DB, order, party)
		if err != nil {
			t.Fatal(err)
		}
		var updated string
		call("", "utxoupdatepsbt", &updated, unsigned, []string{desc})
		var out struct {
			PSBT string `json:"psbt"`
		}
		call(wallet, "walletprocesspsbt", &out, updated, true, "ALL", true, false)
		signed = append(signed, out.PSBT)
	}
	var combined string
	call("", "combinepsbt", &combined, signed)
	var fin struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	call("", "finalizepsbt", &fin, combined, true)
	if !fin.Complete {
		t.Fatal("the parties' co-spend does not finalize")
	}
	return fin.Hex
}
