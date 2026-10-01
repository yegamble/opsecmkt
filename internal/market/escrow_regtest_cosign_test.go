package market

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestEscrowRegtestCosign is escrow slice P1-c2's real-chain leg (scripts/test-escrow-regtest.sh; not run in CI).
// Against a Bitcoin Core regtest node, with the App reaching Core only as the rpcwhitelist user, every settlement is
// signed by a party's own Core wallet and completed, stored, broadcast and confirmed by the App's escrow watcher:
//
//   - claim: the market's child index is set high (2501, past a fresh wallet's 1000 derived keys), so the watcher must
//     refill the arbiter's key range (keypoolrefill under rpcwhitelist) before it co-signs the vendor's claim; the
//     confirmed outputs are the planned ones (gettxout of each);
//   - refund: the buyer cancels an underpaid escrow and signs the refund; the watcher's co-sign step runs alone and
//     the App restarts before any broadcast; the next pass broadcasts the stored transaction, unchanged;
//   - both parties sign: the market does not sign; Core finalizes the stored PSBT with the two stored party
//     signatures alone to exactly the broadcast transaction;
//   - a bad signature (A-339): the vendor's wallet signs its copy with a changed input amount, and r = s = 1; each
//     reaches party_signed and is discarded when finalizing fails; the vendor's good signature then completes it;
//   - a stray refund: coins sent to a cancelled order's escrow return to the buyer's refund address;
//   - an administrator's dispute decision for the buyer: refunded from the escrow.
func TestEscrowRegtestCosign(t *testing.T) {
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
	arbiter, watch, funder := "p1c2-arbiter-"+s, "p1c2-watch-"+s, "p1c2-funder-"+s
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
	buyerWallet, vendorWallet := "p1c2-buyer-"+s, "p1c2-vendor-"+s
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
		t.Fatal("escrow did not start against the regtest node (arbiter or watch-only wallet unreadable, or the arbiter wallet not dedicated?)")
	}
	t.Logf("escrow on: arbiter wallet %s (dedicated: getwalletinfo txcount 0, unencrypted), watch-only wallet %s, market fee 1500 bps", arbiter, watch)
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
	buyer, buyerSess := activate("rc2_buyer", "buyer", buyerKey, buyerWallet)
	vendor, vendorSess := activate("rc2_vendor", "vendor", vendorKey, vendorWallet)
	_ = buyer
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
	// High market index: every escrow of this leg uses a market child key past the arbiter's initial key range.
	if _, err := e.DB.Exec("SELECT setval('escrow_market_index', 2500)"); err != nil {
		t.Fatal(err)
	}
	escrowOrder := func(refund, btc string, deliver bool) (order, addr, txid string) {
		t.Helper()
		product := e.product(vendor, "digital")
		if _, err := e.DB.Exec("UPDATE products SET delivery_content='regtest licence p1c2' WHERE id=$1", product); err != nil {
			t.Fatal(err)
		}
		order = e.order(buyer, product, "BTC", stateDraft)
		if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {refund}}); w.Code != 303 {
			t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
		}
		if !deliver { // the content can no longer be delivered automatically: the funded order stays paid
			if _, err := e.DB.Exec("UPDATE products SET delivery_content='' WHERE id=$1", product); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
			t.Fatal(err)
		}
		if btc != "" {
			call(funder, "sendtoaddress", &txid, addr, json.Number(btc))
			mine(3)
			poll()
		}
		return order, addr, txid
	}
	settlementOf := func(order, kind string) (id int64, state, txid, raw, cosigned string) {
		t.Helper()
		if err := e.DB.QueryRow(`SELECT id,state,COALESCE(txid,''),COALESCE(raw_tx,''),COALESCE(cosigned,'') FROM settlements WHERE order_id=$1 AND kind=$2 AND state<>'superseded'
			ORDER BY id DESC LIMIT 1`, order, kind).Scan(&id, &state, &txid, &raw, &cosigned); err != nil {
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
		if signed.Complete {
			t.Fatal("one party's signature completed the escrow spend")
		}
		return signed.PSBT
	}
	paste := func(order, sess string, id int64, psbt string) {
		t.Helper()
		w := e.do("POST", "/escrow/sign", sess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {psbt}})
		if w.Code != 303 {
			t.Fatalf("paste: %d %s", w.Code, w.Body.String())
		}
	}
	mempool := func() []string {
		t.Helper()
		var m []string
		call("", "getrawmempool", &m)
		return m
	}
	// confirmed mines 3 blocks, runs a pass and checks the settlement is confirmed and each planned output is an
	// unspent output of its txid on the chain, with the planned value and address.
	confirmed := func(id int64, txid string) {
		t.Helper()
		mine(3)
		poll()
		var state string
		e.DB.QueryRow("SELECT state FROM settlements WHERE id=$1", id).Scan(&state)
		if state != "confirmed" {
			t.Fatalf("settlement %d after 3 blocks: %s", id, state)
		}
		for _, r := range agStrings(t, e, "SELECT n||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id) {
			f := strings.Fields(r)
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
			if out.ScriptPubKey.Address != f[1] || strconv.FormatInt(sats, 10) != f[2] || out.Confirmations < 3 {
				t.Fatalf("output %d of %s on chain: %+v, planned %s", n, txid, out, r)
			}
		}
	}

	// Claim leg.
	orderA, addrA, fundA := escrowOrder(newAddr(), "0.0012", true)
	claimTo := newAddr()
	if w := e.do("POST", "/escrow/claim", vendorSess, url.Values{"order_id": {orderA}, "address": {claimTo}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	poll()
	idA, stA, txidA, _, _ := settlementOf(orderA, "release")
	if stA != "awaiting_signature" {
		t.Fatalf("claim %s", stA)
	}
	var indexA int64
	e.DB.QueryRow("SELECT market_index FROM escrows WHERE order_id=$1", orderA).Scan(&indexA)
	// The market child key's address is not the arbiter wallet's yet (its 1000 derived keys stop short of 2501).
	var childAddrs []string
	call("", "deriveaddresses", &childAddrs, withChecksum("wpkh("+agStrings(t, e, "SELECT p.pubkey FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=$1 AND p.party='market'", orderA)[0]+")"))
	arbiterOwns := func() bool {
		var info struct {
			IsMine bool `json:"ismine"`
		}
		call(arbiter, "getaddressinfo", &info, childAddrs[0])
		return info.IsMine
	}
	if arbiterOwns() {
		t.Fatal("the arbiter wallet already covers market index 2501: the leg would not exercise keypoolrefill")
	}
	paste(orderA, vendorSess, idA, walletSign(vendorWallet, partyCopy(orderA, vendorSess, idA)))
	poll()
	if !arbiterOwns() {
		t.Fatal("the arbiter key range was not refilled past the market index")
	}
	_, stA, _, rawA, cosA := settlementOf(orderA, "release")
	if stA != "broadcast" || cosA != "market" || !slices.Contains(mempool(), txidA) {
		t.Fatalf("claim after the vendor signed: %s %s, in mempool %v", stA, cosA, slices.Contains(mempool(), txidA))
	}
	var dec struct {
		TxID string `json:"txid"`
	}
	call("", "decoderawtransaction", &dec, rawA)
	if dec.TxID != txidA {
		t.Fatalf("stored raw transaction %s, settlement txid %s", dec.TxID, txidA)
	}
	t.Logf("claim leg: escrow %s funded (txid %s), market index %d; vendor's wallet signed; the watcher refilled the arbiter key range, co-signed, checked, stored and broadcast txid %s", addrA, fundA, indexA, txidA)
	confirmed(idA, txidA)
	t.Logf("claim leg: txid %s confirmed (3 blocks); outputs on chain = planned %q", txidA, agStrings(t, e, "SELECT role||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", idA))

	// Refund leg, with a restart between storing and broadcasting.
	refundTo := newAddr()
	orderB, _, fundB := escrowOrder(refundTo, "0.0004", true)
	if w := e.do("POST", "/orders/cancel", buyerSess, url.Values{"order_id": {orderB}, "from": {stateAwaitingPayment}, "amount_seen": {"40000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("buyer cancel: %d %s", w.Code, w.Body.String())
	}
	poll()
	idB, _, txidB, _, _ := settlementOf(orderB, "refund")
	paste(orderB, buyerSess, idB, walletSign(buyerWallet, partyCopy(orderB, buyerSess, idB)))
	if err := e.A.cosignSettlements(ctx, e.A.escrow.Load()); err != nil {
		t.Fatal(err)
	}
	_, stB, _, rawB, _ := settlementOf(orderB, "refund")
	if stB != "signed" || rawB == "" || slices.Contains(mempool(), txidB) {
		t.Fatalf("refund co-signed, not broadcast: %s", stB)
	}
	e.restart()
	poll()
	_, stB, _, rawB2, _ := settlementOf(orderB, "refund")
	if stB != "broadcast" || rawB2 != rawB || !slices.Contains(mempool(), txidB) {
		t.Fatalf("refund after the restart: %s (raw unchanged %v)", stB, rawB2 == rawB)
	}
	t.Logf("refund leg: escrow funded (txid %s), buyer cancelled and signed; stored txid %s, App restarted before any broadcast; the next pass broadcast the stored transaction unchanged", fundB, txidB)
	confirmed(idB, txidB)
	t.Logf("refund leg: txid %s confirmed, paying the pinned refund address", txidB)

	// Both parties sign: the market does not.
	orderC, _, _ := escrowOrder(newAddr(), "0.001", true)
	if w := e.do("POST", "/escrow/claim", vendorSess, url.Values{"order_id": {orderC}, "address": {newAddr()}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	poll()
	idC, _, txidC, _, _ := settlementOf(orderC, "release")
	paste(orderC, vendorSess, idC, walletSign(vendorWallet, partyCopy(orderC, vendorSess, idC)))
	paste(orderC, buyerSess, idC, walletSign(buyerWallet, partyCopy(orderC, buyerSess, idC)))
	poll()
	_, stC, _, rawC, cosC := settlementOf(orderC, "release")
	if stC != "broadcast" || cosC != "parties" {
		t.Fatalf("both signed: %s %s", stC, cosC)
	}
	var storedC string
	e.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", idC).Scan(&storedC)
	sp, _ := decodePSBT(storedC)
	raw := sp.Raw
	for _, r := range agStrings(t, e, "SELECT input_n||' '||pubkey||' '||signature FROM settlement_signatures WHERE settlement_id=$1 ORDER BY party,input_n", idC) {
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
	if !fin.Complete || fin.Hex != rawC {
		t.Fatal("the broadcast transaction is not the stored PSBT with the two party signatures alone")
	}
	t.Logf("both-signed leg: vendor and buyer signed in their wallets; cosigned=parties; Core finalizes the stored PSBT with the two stored party signatures alone to exactly the broadcast txid %s", txidC)
	confirmed(idC, txidC)

	// A-339: bad signatures are discarded.
	orderD, _, _ := escrowOrder(newAddr(), "0.001", true)
	if w := e.do("POST", "/escrow/claim", vendorSess, url.Values{"order_id": {orderD}, "address": {newAddr()}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	poll()
	idD, _, txidD, _, _ := settlementOf(orderD, "release")
	reverted := func(what string) {
		t.Helper()
		_, st, _, _, _ := settlementOf(orderD, "release")
		if st != "party_signed" {
			t.Fatalf("%s: %s before the pass", what, st)
		}
		poll()
		_, st, _, _, _ = settlementOf(orderD, "release")
		if st != "awaiting_signature" || agInt(e, "SELECT count(*) FROM settlement_signatures WHERE settlement_id=$1", idD) != 0 || slices.Contains(mempool(), txidD) {
			t.Fatalf("%s: %s after the pass", what, st)
		}
	}
	// The vendor's wallet signs a copy whose input amount was changed: a signature over the wrong amount.
	cp, _ := decodePSBT(partyCopy(orderD, vendorSess, idD))
	in := cp.Inputs[0].WitnessUTXO
	changed, _ := cp.withoutInputFields(func(_ int, k []byte) bool { return k[0] == 0x01 })
	wrong := append(append(binary.LittleEndian.AppendUint64(nil, uint64(in.Value-1000)), byte(len(in.Script))), in.Script...)
	cp2, _ := parsePSBT(changed)
	changed, _ = cp2.withInputField(0, []byte{0x01}, wrong)
	signedWrong, _ := decodePSBT(walletSign(vendorWallet, encodePSBT(changed)))
	if signedWrong.TxID() != txidD {
		t.Fatal("the changed amount changed the txid")
	}
	// Paste the wrong-amount signature on the true copy (the paste keeps only the signatures).
	vpub, _ := hex.DecodeString(agStrings(t, e, "SELECT p.pubkey FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=$1 AND p.party='vendor'", orderD)[0])
	trueCopy, _ := decodePSBT(partyCopy(orderD, vendorSess, idD))
	withSig := func(sig []byte) string {
		cur, _ := parsePSBT(trueCopy.Raw)
		out, err := cur.withInputField(0, append([]byte{0x02}, vpub...), sig)
		if err != nil {
			t.Fatal(err)
		}
		return encodePSBT(out)
	}
	paste(orderD, vendorSess, idD, withSig(signedWrong.Inputs[0].PartialSigs[0].Sig))
	reverted("a signature over the wrong amount")
	paste(orderD, vendorSess, idD, withSig([]byte{0x30, 0x06, 0x02, 0x01, 0x01, 0x02, 0x01, 0x01, 0x01}))
	reverted("r=s=1")
	if !strings.Contains(strings.Join(agStrings(t, e, "SELECT note FROM order_events WHERE order_id=$1", orderD), "\n"), "The vendor's signature on the escrow settlement (the vendor's claim) did not verify") {
		t.Fatal("A-339 note")
	}
	paste(orderD, vendorSess, idD, walletSign(vendorWallet, partyCopy(orderD, vendorSess, idD)))
	poll()
	if _, st, _, _, _ := settlementOf(orderD, "release"); st != "broadcast" {
		t.Fatalf("after the good signature: %s", st)
	}
	t.Logf("A-339 leg: a vendor-wallet signature over a changed input amount and an r=s=1 signature each reached party_signed and were discarded (finalizepsbt incomplete), nothing broadcast; the good signature then broadcast txid %s", txidD)
	confirmed(idD, txidD)

	// Stray refund.
	strayTo := newAddr()
	orderE, addrE, _ := escrowOrder(strayTo, "", true)
	if w := e.do("POST", "/orders/cancel", buyerSess, url.Values{"order_id": {orderE}, "from": {stateAwaitingPayment}}); w.Code != 303 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	var lateE string
	call(funder, "sendtoaddress", &lateE, addrE, json.Number("0.0003"))
	mine(3)
	poll()
	idE, stE, txidE, _, _ := settlementOf(orderE, "stray_refund")
	if stE != "awaiting_signature" || agInt(e, "SELECT count(*) FROM payments WHERE order_id=$1 AND after_close AND settlement_id=$2", orderE, idE) != 1 {
		t.Fatalf("stray refund: %s", stE)
	}
	paste(orderE, buyerSess, idE, walletSign(buyerWallet, partyCopy(orderE, buyerSess, idE)))
	poll()
	if _, st, _, _, _ := settlementOf(orderE, "stray_refund"); st != "broadcast" {
		t.Fatalf("stray refund after the buyer signed: %s", st)
	}
	t.Logf("stray leg: 0.0003 BTC sent to a cancelled order's escrow (txid %s) recorded after the close; stray refund signed by the buyer, co-signed and broadcast, txid %s", lateE, txidE)
	confirmed(idE, txidE)

	// An administrator's dispute decision for the buyer.
	orderF, _, _ := escrowOrder(newAddr(), "0.001", false)
	var stF string
	e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", orderF).Scan(&stF)
	if stF != statePaid {
		t.Fatalf("undelivered order %s", stF)
	}
	if w := e.do("POST", "/disputes", buyerSess, url.Values{"order_id": {orderF}, "reason": {"The licence was never delivered to me."}}); w.Code != 303 {
		t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
	_, admin := e.user("rc2_admin_"+s, "admin")
	var dispute string
	e.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", orderF).Scan(&dispute)
	if w := e.do("POST", "/resolve", admin, url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"Not delivered: refunded from the escrow."},
		"payout_confirmed": {"confirmed"}, "amount_seen": {"100000"}, "reply_seen": {"0"}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("resolve: %d %s", w.Code, w.Body.String())
	}
	poll()
	idF, _, txidF, _, _ := settlementOf(orderF, "refund")
	paste(orderF, buyerSess, idF, walletSign(buyerWallet, partyCopy(orderF, buyerSess, idF)))
	poll()
	if _, st, _, _, _ := settlementOf(orderF, "refund"); st != "broadcast" {
		t.Fatalf("dispute refund: %s", st)
	}
	t.Logf("dispute leg: an administrator decided for the buyer (password step-up); the refund was signed by the buyer, co-signed and broadcast, txid %s", txidF)
	confirmed(idF, txidF)
	if agInt(e, "SELECT count(*) FROM payouts") != 0 {
		t.Fatal("a payouts row for an escrow order")
	}
}
