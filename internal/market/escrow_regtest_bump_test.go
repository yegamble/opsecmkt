package market

import (
	"context"
	"encoding/json"
	"html"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestEscrowRegtestBump is escrow slice P2-d's real-chain leg (scripts/test-escrow-regtest.sh; not run in CI): a fee
// bump (P2-R7, A-375) against a Bitcoin Core regtest node, with the App reaching Core only as the rpcwhitelist user and
// every party signature made by that party's own Core wallet. A vendor's claim (with a market fee and a return of the
// overpayment to the buyer) is broadcast at 1 sat/vB and left unmined; the vendor asks for a fee bump at 5 sat/vB; the
// buyer's own signature of it, made first, is refused; the vendor signs it in their wallet; the watcher co-signs it,
// testmempoolaccept accepts it as a replacement, and it replaces the original in the mempool. Mined, the settlement is
// confirmed (not external), the original is conflicted in the watch-only wallet by the bump, and the confirmed outputs
// are the planned ones with only the vendor's output smaller.
func TestEscrowRegtestBump(t *testing.T) {
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
	arbiter, watch, funder := "p2d-arbiter-"+s, "p2d-watch-"+s, "p2d-funder-"+s
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
	buyerWallet, vendorWallet := "p2d-buyer-"+s, "p2d-vendor-"+s
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
	t.Setenv("ESCROW_BTC_FEE_RATE", "1") // the original is sent at 1 sat/vB
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
	buyer, buyerSess := activate("rbump_buyer", "buyer", buyerKey, buyerWallet)
	vendor, vendorSess := activate("rbump_vendor", "vendor", vendorKey, vendorWallet)
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
	feeRateOf := func(txid string) float64 {
		t.Helper()
		var entry struct {
			VSize int64 `json:"vsize"`
			Fees  struct {
				Base json.Number `json:"base"`
			} `json:"fees"`
		}
		call("", "getmempoolentry", &entry, txid)
		fee, err := parseAmount(string(entry.Fees.Base), 8)
		if err != nil || entry.VSize == 0 {
			t.Fatalf("mempool entry of %s: %+v", txid, entry)
		}
		return float64(fee) / float64(entry.VSize)
	}

	// A completed escrow order paid above its price, claimed by the vendor, signed in the vendor's wallet and broadcast
	// at 1 sat/vB. Nothing is mined after this until the bump replaced it.
	product := e.product(vendor, "digital")
	if _, err := e.DB.Exec("UPDATE products SET delivery_content='regtest licence p2d' WHERE id=$1", product); err != nil {
		t.Fatal(err)
	}
	order := e.order(buyer, product, "BTC", stateDraft)
	if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {newAddr()}}); w.Code != 303 {
		t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
	}
	var addr, fund string
	e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr)
	call(funder, "sendtoaddress", &fund, addr, json.Number("0.0012"))
	mine(3)
	poll()
	if w := e.do("POST", "/escrow/claim", vendorSess, url.Values{"order_id": {order}, "address": {newAddr()}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	poll()
	var id int64
	var parentTxID string
	if err := e.DB.QueryRow("SELECT id,txid FROM settlements WHERE order_id=$1 AND kind='release' AND state='awaiting_signature'", order).Scan(&id, &parentTxID); err != nil {
		t.Fatalf("claim not built: %v", err)
	}
	sid := strconv.FormatInt(id, 10)
	copyOf := func(sess, query string) string {
		t.Helper()
		w := e.do("GET", "/escrow-psbt?id="+order+"&s="+sid+query, sess, nil)
		if w.Code != 200 {
			t.Fatalf("copy %s: %d", query, w.Code)
		}
		return strings.TrimSpace(w.Body.String())
	}
	if w := e.do("POST", "/escrow/sign", vendorSess, url.Values{"order_id": {order}, "settlement_id": {sid}, "psbt": {walletSign(vendorWallet, copyOf(vendorSess, ""))}}); w.Code != 303 {
		t.Fatalf("vendor signs the claim: %d %s", w.Code, w.Body.String())
	}
	poll()
	var state string
	var parentFee int64
	e.DB.QueryRow("SELECT state,network_fee FROM settlements WHERE id=$1", id).Scan(&state, &parentFee)
	if state != "broadcast" || !slices.Contains(mempool(), parentTxID) {
		t.Fatalf("claim at 1 sat/vB: %s, in mempool %v", state, slices.Contains(mempool(), parentTxID))
	}
	planned := agStrings(t, e, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
	t.Logf("original: txid %s broadcast at %.2f sat/vB (network fee %d sats), unmined; planned outputs %q", parentTxID, feeRateOf(parentTxID), parentFee, planned)
	parentRow := func() string {
		var r string
		e.DB.QueryRow("SELECT concat_ws('|',psbt,txid,raw_tx,cosigned,final_at,signer,fee_rate,network_fee,input_total) FROM settlements WHERE id=$1", id).Scan(&r)
		return r
	}
	before := parentRow()

	// Only the vendor (the claimant) asks; the bump is built from the stored settlement.
	if w := e.do("POST", "/escrow/bump", buyerSess, url.Values{"order_id": {order}, "settlement_id": {sid}, "fee_rate": {"5"}, "password": {testPassword}}); w.Code != 403 {
		t.Fatalf("the buyer asked for a fee bump of the vendor's claim: %d", w.Code)
	}
	if w := e.do("POST", "/escrow/bump", vendorSess, url.Values{"order_id": {order}, "settlement_id": {sid}, "fee_rate": {"5"}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("bump: %d %s", w.Code, w.Body.String())
	}
	poll()
	var bid, bumpFee int64
	var bumpTxID, bumpPSBTText string
	if err := e.DB.QueryRow("SELECT id,txid,network_fee,psbt FROM settlement_bumps WHERE settlement_id=$1 AND state='awaiting_signature'", id).Scan(&bid, &bumpTxID, &bumpFee, &bumpPSBTText); err != nil {
		t.Fatalf("bump not built: %v", err)
	}
	bq := "&b=" + strconv.FormatInt(bid, 10)
	vendorCopy := copyOf(vendorSess, bq)
	// What Core reads in the vendor's copy: the bump's txid, the original's inputs, the same addresses, the planned fee.
	var dec, decParent struct {
		Tx struct {
			TxID string `json:"txid"`
			Vin  []struct {
				TxID string `json:"txid"`
				Vout int    `json:"vout"`
			} `json:"vin"`
			Vout []struct {
				Value        json.Number `json:"value"`
				ScriptPubKey struct {
					Address string `json:"address"`
				} `json:"scriptPubKey"`
			} `json:"vout"`
		} `json:"tx"`
		Fee json.Number `json:"fee"`
	}
	call("", "decodepsbt", &dec, vendorCopy)
	var parentPSBT string
	e.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&parentPSBT)
	call("", "decodepsbt", &decParent, parentPSBT)
	fee, _ := parseAmount(string(dec.Fee), 8)
	if dec.Tx.TxID != bumpTxID || fee != bumpFee || !slices.Equal(dec.Tx.Vin, decParent.Tx.Vin) || len(dec.Tx.Vout) != len(decParent.Tx.Vout) {
		t.Fatalf("Core reads the bump as txid %s fee %d (stored %s, %d)", dec.Tx.TxID, fee, bumpTxID, bumpFee)
	}
	for i, o := range dec.Tx.Vout {
		got, _ := parseAmount(string(o.Value), 8)
		was, _ := parseAmount(string(decParent.Tx.Vout[i].Value), 8)
		want := was
		if i == 0 {
			want = was - (bumpFee - parentFee)
		}
		if o.ScriptPubKey.Address != decParent.Tx.Vout[i].ScriptPubKey.Address || got != want {
			t.Fatalf("bump output %d: %s %d, original %s %d", i, o.ScriptPubKey.Address, got, decParent.Tx.Vout[i].ScriptPubKey.Address, was)
		}
	}
	t.Logf("fee bump built: txid %s, network fee %d sats at 5 sat/vB (Core's decodepsbt agrees); same inputs and addresses, only the vendor's output smaller by %d sats", bumpTxID, bumpFee, bumpFee-parentFee)
	// The buyer's own wallet signature, made before the vendor's, is refused and nothing is stored.
	keys, err := escrowKeys(ctx, e.DB, order)
	if err != nil {
		t.Fatal(err)
	}
	buyerCopy, err := settlementCopy(bumpPSBTText, keys[roleBuyer])
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do("POST", "/escrow/bump-sign", buyerSess, url.Values{"order_id": {order}, "bump_id": {strconv.FormatInt(bid, 10)}, "psbt": {walletSign(buyerWallet, buyerCopy)}}); w.Code != 409 ||
		agInt(e, "SELECT count(*) FROM settlement_bump_signatures WHERE bump_id=$1", bid) != 0 {
		t.Fatalf("the buyer's signature before the vendor's: %d %s", w.Code, w.Body.String())
	}
	t.Log("the buyer's Core-signed copy of the bump, pasted before the vendor's signature -> 409, nothing stored")
	if w := e.do("POST", "/escrow/bump-sign", vendorSess, url.Values{"order_id": {order}, "bump_id": {strconv.FormatInt(bid, 10)}, "psbt": {walletSign(vendorWallet, vendorCopy)}}); w.Code != 303 {
		t.Fatalf("vendor signs the bump: %d %s", w.Code, w.Body.String())
	}
	poll()
	var bstate, cosigned string
	e.DB.QueryRow("SELECT state,COALESCE(cosigned,'') FROM settlement_bumps WHERE id=$1", bid).Scan(&bstate, &cosigned)
	m := mempool()
	if bstate != "broadcast" || cosigned != "market" || !slices.Contains(m, bumpTxID) || slices.Contains(m, parentTxID) {
		t.Fatalf("bump after the vendor signed: %s %s; bump in mempool %v, original %v", bstate, cosigned, slices.Contains(m, bumpTxID), slices.Contains(m, parentTxID))
	}
	t.Logf("the watcher co-signed, checked (testmempoolaccept as a replacement), stored and broadcast the bump: txid %s at %.2f sat/vB replaced %s in the mempool", bumpTxID, feeRateOf(bumpTxID), parentTxID)
	poll() // unconfirmed: only the bump is sent again; the original stays out of the mempool
	if m := mempool(); !slices.Contains(m, bumpTxID) || slices.Contains(m, parentTxID) {
		t.Fatal("after another pass the original is back in the mempool, or the bump is gone")
	}
	mine(3)
	poll()
	var sstate string
	var spentBy *string
	e.DB.QueryRow("SELECT state,spent_by FROM settlements WHERE id=$1", id).Scan(&sstate, &spentBy)
	e.DB.QueryRow("SELECT state FROM settlement_bumps WHERE id=$1", bid).Scan(&bstate)
	if sstate != "confirmed" || bstate != "confirmed" || spentBy != nil || agInt(e, "SELECT count(*) FROM payments WHERE settlement_id=$1 AND flagged", id) != 0 {
		t.Fatalf("after 3 blocks: settlement %s, bump %s", sstate, bstate)
	}
	var wtx struct {
		Confirmations int64    `json:"confirmations"`
		Conflicts     []string `json:"walletconflicts"`
	}
	call(watch, "gettransaction", &wtx, parentTxID, true)
	if wtx.Confirmations >= 0 || !slices.Contains(wtx.Conflicts, bumpTxID) {
		t.Fatalf("the original in the watch-only wallet: %+v", wtx)
	}
	t.Logf("original %s in the watch-only wallet: confirmations %d, walletconflicts %v (conflicted by the bump); settlement confirmed, not external", parentTxID, wtx.Confirmations, wtx.Conflicts)
	for i, r := range planned {
		f := strings.Fields(r)
		want, _ := strconv.ParseInt(f[2], 10, 64)
		if i == 0 {
			want -= bumpFee - parentFee
		}
		var out *struct {
			Value        json.Number `json:"value"`
			ScriptPubKey struct {
				Address string `json:"address"`
			} `json:"scriptPubKey"`
		}
		call("", "gettxout", &out, bumpTxID, i, false)
		if out == nil {
			t.Fatalf("output %d of the bump is not an unspent output on the chain", i)
		}
		got, _ := parseAmount(string(out.Value), 8)
		if out.ScriptPubKey.Address != f[1] || got != want {
			t.Fatalf("output %d on chain: %s %d, planned %s %d", i, out.ScriptPubKey.Address, got, f[1], want)
		}
	}
	if parentRow() != before {
		t.Fatal("the settlement's frozen columns changed")
	}
	t.Logf("bump %s confirmed (3 blocks); its outputs on chain = the planned outputs with the vendor's smaller by %d sats; the settlement row is unchanged", bumpTxID, bumpFee-parentFee)

	// P2-d-fix legs, each on a fresh claim broadcast at 1 sat/vB and left unmined.
	claimBroadcast := func(label string) (string, string) {
		t.Helper()
		product := e.product(vendor, "digital")
		e.DB.Exec("UPDATE products SET delivery_content='regtest licence p2d "+label+"' WHERE id=$1", product)
		order := e.order(buyer, product, "BTC", stateDraft)
		if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {newAddr()}}); w.Code != 303 {
			t.Fatalf("%s checkout: %d", label, w.Code)
		}
		var addr, fund string
		e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr)
		call(funder, "sendtoaddress", &fund, addr, json.Number("0.0012"))
		mine(3)
		poll()
		if w := e.do("POST", "/escrow/claim", vendorSess, url.Values{"order_id": {order}, "address": {newAddr()}, "password": {testPassword}}); w.Code != 303 {
			t.Fatalf("%s claim: %d", label, w.Code)
		}
		poll()
		var sid string
		e.DB.QueryRow("SELECT id::text FROM settlements WHERE order_id=$1 AND kind='release' AND state='awaiting_signature'", order).Scan(&sid)
		w := e.do("GET", "/escrow-psbt?id="+order+"&s="+sid, vendorSess, nil)
		if w := e.do("POST", "/escrow/sign", vendorSess, url.Values{"order_id": {order}, "settlement_id": {sid}, "psbt": {walletSign(vendorWallet, strings.TrimSpace(w.Body.String()))}}); w.Code != 303 {
			t.Fatalf("%s: vendor signs: %d", label, w.Code)
		}
		poll()
		var state string
		e.DB.QueryRow("SELECT state FROM settlements WHERE id=$1::bigint", sid).Scan(&state)
		if state != "broadcast" {
			t.Fatalf("%s: claim %s", label, state)
		}
		return order, sid
	}
	bumpRequest := func(order, sid, rate string) (int, string) {
		w := e.do("POST", "/escrow/bump", vendorSess, url.Values{"order_id": {order}, "settlement_id": {sid}, "fee_rate": {rate}, "password": {testPassword}})
		return w.Code, w.Body.String()
	}
	builtBump := func(sid string) (string, string) {
		t.Helper()
		var bid, txid string
		if err := e.DB.QueryRow("SELECT id::text,txid FROM settlement_bumps WHERE settlement_id=$1::bigint AND state='awaiting_signature'", sid).Scan(&bid, &txid); err != nil {
			t.Fatalf("bump not built: %v", err)
		}
		return bid, txid
	}
	bumpCopy := func(order, sid, bid, sess string) string {
		t.Helper()
		w := e.do("GET", "/escrow-psbt?id="+order+"&s="+sid+"&b="+bid, sess, nil)
		if w.Code != 200 {
			t.Fatalf("bump copy: %d", w.Code)
		}
		return strings.TrimSpace(w.Body.String())
	}
	settledClean := func(order, sid, bid string) (string, string) {
		t.Helper()
		var sstate, bstate, cosigned string
		var spent *string
		e.DB.QueryRow("SELECT state,spent_by FROM settlements WHERE id=$1::bigint", sid).Scan(&sstate, &spent)
		e.DB.QueryRow("SELECT state,COALESCE(cosigned,'') FROM settlement_bumps WHERE id=$1::bigint", bid).Scan(&bstate, &cosigned)
		if sstate != "confirmed" || bstate != "confirmed" || spent != nil || agInt(e, "SELECT count(*) FROM payments WHERE settlement_id=$1::bigint AND flagged", sid) != 0 ||
			agInt(e, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%did not make%'", order) != 0 {
			t.Fatalf("settlement %s %v, bump %s", sstate, spent, bstate)
		}
		return bstate, cosigned
	}

	// A-383: a 100 sat/vB request is refused with the fee-cap message (it could exceed 100 sat/vB at the transaction's
	// real size); the highest rate the form offers is broadcast and confirmed, never stuck at final-fee.
	orderM, sidM := claimBroadcast("rate cap")
	code, body := bumpRequest(orderM, sidM, "100")
	if code != 400 || !strings.Contains(html.UnescapeString(body), "fee cap") {
		t.Fatalf("100 sat/vB: %d %s", code, body)
	}
	page := e.do("GET", "/order?id="+orderM, vendorSess, nil).Body.String()
	maxAttr := regexp.MustCompile(`name="fee_rate" type="number" required min="\d+" max="(\d+)"`).FindStringSubmatch(page)
	if maxAttr == nil {
		t.Fatal("no fee bump form")
	}
	if code, body := bumpRequest(orderM, sidM, maxAttr[1]); code != 303 {
		t.Fatalf("%s sat/vB: %d %s", maxAttr[1], code, body)
	}
	poll()
	bidM, txidM := builtBump(sidM)
	if w := e.do("POST", "/escrow/bump-sign", vendorSess, url.Values{"order_id": {orderM}, "bump_id": {bidM}, "psbt": {walletSign(vendorWallet, bumpCopy(orderM, sidM, bidM, vendorSess))}}); w.Code != 303 {
		t.Fatalf("vendor signs the bump at the highest rate: %d", w.Code)
	}
	poll()
	var stateM, sendErrM string
	e.DB.QueryRow("SELECT state,send_error FROM settlement_bumps WHERE id=$1::bigint", bidM).Scan(&stateM, &sendErrM)
	if stateM != "broadcast" || sendErrM != "" || !slices.Contains(mempool(), txidM) {
		t.Fatalf("the bump at %s sat/vB: %s %q", maxAttr[1], stateM, sendErrM)
	}
	t.Logf("A-383: 100 sat/vB refused with the fee-cap message; the form's highest rate, %s sat/vB, was co-signed and broadcast: txid %s at %.2f sat/vB real", maxAttr[1], txidM, feeRateOf(txidM))
	mine(3)
	poll()
	settledClean(orderM, sidM, bidM)

	// A-382: the vendor signs a bump on the market, the buyer signs its own copy, and the two combine, finalize and
	// broadcast it from their own node before the market's next pass; once mined, the settlement is confirmed, with the
	// bump recorded as seen on the network and nothing flagged.
	orderP, sidP := claimBroadcast("parties")
	if code, body := bumpRequest(orderP, sidP, "5"); code != 303 {
		t.Fatalf("bump: %d %s", code, body)
	}
	poll()
	bidP, txidP := builtBump(sidP)
	vendorSigned := walletSign(vendorWallet, bumpCopy(orderP, sidP, bidP, vendorSess))
	if w := e.do("POST", "/escrow/bump-sign", vendorSess, url.Values{"order_id": {orderP}, "bump_id": {bidP}, "psbt": {vendorSigned}}); w.Code != 303 {
		t.Fatalf("vendor signs: %d", w.Code)
	}
	buyerSigned := walletSign(buyerWallet, bumpCopy(orderP, sidP, bidP, buyerSess))
	var combined string
	call("", "combinepsbt", &combined, []string{vendorSigned, buyerSigned})
	var fin struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	call("", "finalizepsbt", &fin, combined)
	var sentP string
	call("", "sendrawtransaction", &sentP, fin.Hex)
	if !fin.Complete || sentP != txidP {
		t.Fatalf("the parties' own bump: complete %v, sent %s, bump %s", fin.Complete, sentP, txidP)
	}
	mine(3)
	poll()
	_, cosignedP := settledClean(orderP, sidP, bidP)
	t.Logf("A-382: the parties combined, finalized and broadcast bump %s themselves; mined, the settlement is confirmed, the bump recorded with cosigned %q, nothing flagged", txidP, cosignedP)
}
