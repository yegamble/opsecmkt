package market

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestEscrowRegtestConflicted is A-428's real-chain leg (scripts/test-escrow-regtest.sh; not run in CI): a deposit that
// did not count when its order was cancelled and was mined afterwards. Against a Bitcoin Core regtest node, with the
// App reaching Core only as the rpcwhitelist user and the stray refund signed by the buyer's own Core wallet:
//
//   - a regtest wallet pays an escrow and the watcher records the deposit unconfirmed; the payer replaces it with a
//     transaction spending the same inputs to an address of its own, which is mined, so Core reports the deposit
//     conflicted (logged: gettransaction's confirmations on the watch-only wallet, and whether listreceivedbyaddress
//     still lists it) and the ledger row goes negative;
//   - the payment window ends (the clock moved in SQL): the order is cancelled unpaid and no refund is requested;
//   - invalidateblock takes the replacement's block out of the chain, the original deposit transaction is sent again
//     (prioritisetransaction lets it replace the replacement back in the mempool) and mined: at 1 confirmation
//     nothing happens; at 3 the deposit is left out and flagged once (an order note, the buyer and staff told), and a
//     stray refund to the refund address pinned at checkout is requested, signed by the buyer's wallet, co-signed by
//     the market, broadcast and confirmed, with its output on chain (gettxout) the plan's.
func TestEscrowRegtestConflicted(t *testing.T) {
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
	s := randomToken()[:8]
	arbiter, watch, funder := "a428-arbiter-"+s, "a428-watch-"+s, "a428-funder-"+s
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
	buyerWallet, vendorWallet := "a428-buyer-"+s, "a428-vendor-"+s
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
	buyer, buyerSess := activate("ra428_buyer", "buyer", buyerKey, buyerWallet)
	vendor, _ := activate("ra428_vendor", "vendor", vendorKey, vendorWallet)
	e.user("ra428_mod_"+s, "moderator")
	mineTo := newAddr()
	call("", "generatetoaddress", nil, 101, mineTo)
	mine := func(n int) { t.Helper(); call("", "generatetoaddress", nil, n, mineTo) }
	poll := func() {
		t.Helper()
		if err := e.A.escrowPollOnce(ctx); err != nil {
			t.Fatalf("watcher pass: %v", err)
		}
	}
	ledger := func(order string) string {
		t.Helper()
		got := agStrings(t, e, "SELECT confirmations||' '||credited||' '||flagged||' '||after_close||' '||escrow_left_out FROM payments WHERE order_id=$1", order)
		if len(got) != 1 {
			t.Fatalf("ledger rows of %s: %q", order, got)
		}
		return got[0]
	}
	const uncounted = "did not count for the order when it was cancelled"
	told := func(order string) (notes, buyerNotices, staff int) {
		t.Helper()
		notes = agInt(e, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, uncounted)
		buyerNotices = agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0 AND strpos(body,$3)>0", buyer, shortID(order), uncounted)
		staff = agInt(e, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,$1)>0 AND strpos(n.body,$2)>0",
			"Payment review needed for order "+shortID(order), uncounted)
		return
	}

	// Checkout, and a deposit recorded unconfirmed.
	refund := newAddr()
	product := e.product(vendor, "digital")
	if _, err := e.DB.Exec("UPDATE products SET delivery_content='regtest licence a428' WHERE id=$1", product); err != nil {
		t.Fatal(err)
	}
	order := e.order(buyer, product, "BTC", stateDraft)
	if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {refund}}); w.Code != 303 {
		t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
	}
	var addr string
	if err := e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
		t.Fatal(err)
	}
	var deposit string
	call(funder, "sendtoaddress", &deposit, addr, json.Number("0.001"))
	var dt struct {
		Hex     string      `json:"hex"`
		Fee     json.Number `json:"fee"`
		Decoded struct {
			Vin []struct {
				TxID string `json:"txid"`
				Vout int64  `json:"vout"`
			} `json:"vin"`
			Vout []struct {
				Value json.Number `json:"value"`
			} `json:"vout"`
		} `json:"decoded"`
	}
	call(funder, "gettransaction", &dt, deposit, false, true)
	poll()
	if got := ledger(order); got != "0 false false false false" {
		t.Fatalf("deposit in the mempool: %s", got)
	}

	// The payer replaces it with a transaction paying an address of its own, which is mined.
	fee, err := parseAmount(strings.TrimPrefix(string(dt.Fee), "-"), 8)
	if err != nil {
		t.Fatalf("deposit fee %q: %v", dt.Fee, err)
	}
	total := fee
	var inputs []map[string]any
	for _, in := range dt.Decoded.Vin {
		inputs = append(inputs, map[string]any{"txid": in.TxID, "vout": in.Vout})
	}
	for _, out := range dt.Decoded.Vout {
		v, err := parseAmount(string(out.Value), 8)
		if err != nil {
			t.Fatalf("deposit output %q: %v", out.Value, err)
		}
		total += v
	}
	var unsigned string
	call("", "createrawtransaction", &unsigned, inputs, []map[string]any{{newAddr(): json.Number(amount(total-50000, 8))}})
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	call(funder, "signrawtransactionwithwallet", &signed, unsigned)
	if !signed.Complete {
		t.Fatal("the replacement is not fully signed")
	}
	var replacing string
	call("", "sendrawtransaction", &replacing, signed.Hex)
	type watchTx struct {
		Confirmations   int64    `json:"confirmations"`
		BlockHash       string   `json:"blockhash"`
		WalletConflicts []string `json:"walletconflicts"`
		MempoolConflict []string `json:"mempoolconflicts"`
	}
	var inPool watchTx
	call(watch, "gettransaction", &inPool, deposit, true)
	poll()
	t.Logf("replaced in the mempool: deposit %s replaced by %s; watch-only gettransaction confirmations %d, mempoolconflicts %v; ledger %q (still confirming)",
		deposit, replacing, inPool.Confirmations, inPool.MempoolConflict, ledger(order))
	mine(1)
	var conflicted watchTx
	call(watch, "gettransaction", &conflicted, deposit, true)
	var received []struct {
		Address string   `json:"address"`
		TxIDs   []string `json:"txids"`
	}
	call(watch, "listreceivedbyaddress", &received, 0, false, true)
	listed := slices.ContainsFunc(received, func(r struct {
		Address string   `json:"address"`
		TxIDs   []string `json:"txids"`
	}) bool {
		return slices.Contains(r.TxIDs, deposit)
	})
	if conflicted.Confirmations >= 0 {
		t.Fatalf("Core reports the replaced deposit with %d confirmations once the replacement is mined", conflicted.Confirmations)
	}
	poll()
	row := ledger(order)
	if !strings.HasPrefix(row, "-") {
		t.Fatalf("the conflicted deposit's ledger row: %s", row)
	}
	t.Logf("conflicted: replacement %s mined; Core reports the deposit %s on the watch-only wallet with gettransaction confirmations %d (walletconflicts %v, mempoolconflicts %v); listreceivedbyaddress(minconf 0, include_empty false, include_watchonly true) lists it: %v; the ledger row is now %q (confirmations credited flagged after_close escrow_left_out)",
		replacing, deposit, conflicted.Confirmations, conflicted.WalletConflicts, conflicted.MempoolConflict, listed, row)

	// The payment window ends: cancelled unpaid, nothing to refund.
	e.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", order)
	e.DB.Exec("UPDATE payments SET created=now()-interval '25 hours' WHERE order_id=$1", order)
	poll()
	var state string
	e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&state)
	if n := agInt(e, "SELECT count(*) FROM settlements WHERE order_id=$1", order); state != stateCancelled || n != 0 ||
		agInt(e, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note='payment window expired'", order) != 1 {
		t.Fatalf("at the end of the payment window: %s, %d settlements", state, n)
	}
	t.Logf("window end: the clock moved 25 hours in SQL; the order was cancelled (payment window expired) with no refund requested")

	// The replacement's block leaves the chain and the original deposit is sent again and mined.
	var rt watchTx
	call(funder, "gettransaction", &rt, replacing)
	call("", "invalidateblock", nil, rt.BlockHash)
	call("", "prioritisetransaction", nil, deposit, 0, 100000)
	var again string
	call("", "sendrawtransaction", &again, dt.Hex)
	if again != deposit {
		t.Fatalf("sent again as %s, want %s", again, deposit)
	}
	mine(1)
	poll()
	if notes, b, staff := told(order); ledger(order) != "1 false false false false" || notes+b+staff != 0 {
		t.Fatalf("mined, 1 confirmation: %s, notes %d, buyer %d, staff %d", ledger(order), notes, b, staff)
	}
	t.Logf("mined again: invalidateblock %s took the replacement out of the chain; prioritisetransaction and sendrawtransaction sent the deposit %s again, which was mined; at 1 confirmation the ledger row is %q and nothing is noted", rt.BlockHash, again, ledger(order))
	mine(2)
	poll()
	notes, b, staff := told(order)
	if got := ledger(order); got != "3 false true false true" || notes != 1 || b != 1 || staff == 0 {
		t.Fatalf("at 3 confirmations: %s, notes %d, buyer %d, staff %d", got, notes, b, staff)
	}
	var sid int64
	var sstate, dest string
	if err := e.DB.QueryRow("SELECT id,state,destination FROM settlements WHERE order_id=$1 AND kind='stray_refund'", order).Scan(&sid, &sstate, &dest); err != nil {
		t.Fatalf("no stray refund: %v", err)
	}
	if sstate != "awaiting_signature" || dest != refund || agInt(e, "SELECT count(*) FROM payments WHERE settlement_id=$1 AND txid=$2", sid, deposit) != 1 {
		t.Fatalf("stray refund %d: %s to %s", sid, sstate, dest)
	}
	t.Logf("left out: at 3 confirmations the ledger row is %q; %d order note, %d buyer notice, %d staff notices (did not count when cancelled); stray refund %d requested to the refund address pinned at checkout", ledger(order), notes, b, staff, sid)

	// The buyer's own wallet signs the stray refund; the market co-signs, broadcasts and follows it.
	w := e.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(sid, 10), buyerSess, nil)
	if w.Code != 200 {
		t.Fatalf("copy: %d", w.Code)
	}
	var bs struct {
		PSBT     string `json:"psbt"`
		Complete bool   `json:"complete"`
	}
	call(buyerWallet, "walletprocesspsbt", &bs, strings.TrimSpace(w.Body.String()), true, "ALL", true, false)
	if bs.Complete {
		t.Fatal("one party's signature completed the escrow spend")
	}
	if w := e.do("POST", "/escrow/sign", buyerSess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(sid, 10)}, "psbt": {bs.PSBT}}); w.Code != 303 {
		t.Fatalf("paste: %d %s", w.Code, w.Body.String())
	}
	poll()
	var txid, cosigned string
	e.DB.QueryRow("SELECT state,COALESCE(txid,''),COALESCE(cosigned,'') FROM settlements WHERE id=$1", sid).Scan(&sstate, &txid, &cosigned)
	if sstate != "broadcast" || cosigned != "market" {
		t.Fatalf("stray refund after the buyer signed: %s cosigned %s", sstate, cosigned)
	}
	mine(3)
	poll()
	e.DB.QueryRow("SELECT state FROM settlements WHERE id=$1", sid).Scan(&sstate)
	if sstate != "confirmed" {
		t.Fatalf("stray refund after 3 blocks: %s", sstate)
	}
	outs := agStrings(t, e, "SELECT n||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", sid)
	if len(outs) != 1 {
		t.Fatalf("stray refund outputs %q", outs)
	}
	f := strings.Fields(outs[0])
	n, _ := strconv.Atoi(f[0])
	var out *struct {
		Confirmations int64       `json:"confirmations"`
		Value         json.Number `json:"value"`
		ScriptPubKey  struct {
			Address string `json:"address"`
		} `json:"scriptPubKey"`
	}
	call("", "gettxout", &out, txid, n, false)
	if out == nil {
		t.Fatalf("output %d of %s is not an unspent output on the chain", n, txid)
	}
	sats, _ := parseAmount(string(out.Value), 8)
	if out.ScriptPubKey.Address != refund || f[1] != refund || strconv.FormatInt(sats, 10) != f[2] || out.Confirmations < 3 || sats <= 0 || sats >= 100000 {
		t.Fatalf("output %d of %s on chain: %+v, planned %s", n, txid, out, outs[0])
	}
	poll()
	if notes, b, _ := told(order); notes != 1 || b != 1 || agInt(e, "SELECT count(*) FROM settlements WHERE order_id=$1", order) != 1 {
		t.Fatalf("told again (notes %d, buyer %d), or another settlement", notes, b)
	}
	t.Logf("stray refund: signed by the buyer's wallet, co-signed by the market and broadcast as %s; confirmed, output %d pays %s sats to the refund address %s (gettxout, %d confirmations); nobody told twice", txid, n, f[2], refund, out.Confirmations)
	if agInt(e, "SELECT count(*) FROM payouts") != 0 {
		t.Fatal("a payouts row for an escrow order")
	}
}
