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

// TestEscrowRegtestGate is escrow slice P1-d2's real-chain leg (scripts/test-escrow-regtest.sh; not run in CI): the
// Phase 1 gate's adversarial legs that no earlier slice's leg covers, against a Bitcoin Core regtest node with the App
// reaching Core only as the rpcwhitelist user and every party signature made by that party's own Core wallet:
//
//   - comma injection: a key field holding two keys is refused before any RPC, while Core itself would read the
//     descriptor built from it as a 2-of-4;
//   - a forged market slot: a vendor's Core-signed copy with a signature added under the market's key is refused and
//     changes nothing;
//   - A-356 while awaiting a signature: the parties co-spend the escrow from their own wallets while the vendor's claim
//     waits for a signature; the watcher records the claim external with the co-spend's txid and the order page stops
//     asking for a signature;
//   - a spent-outpoint build: the parties co-spend the escrow, then the vendor claims; the build refuses the spent
//     input (nothing built), waits one pass, and the claim is external with the co-spend's txid (A-356);
//   - a conflicting outcome: once an administrator's refund decision has a broadcast transaction, changing it to a
//     release is refused, the database refuses a second outcome row, and only the refund reaches the chain;
//   - the restore gate: a party-signed claim is not co-signed while the gate is set; a stored transaction held by a
//     restore is not broadcast until an administrator confirms, and then exactly once;
//   - a restored database never re-issues an escrow address (A-359, slice P1-d3): an escrow created after the backup
//     point is lost by a restore (simulated in SQL: its rows removed and the market counter set back); checkout answers
//     503 while the gate is set; after it, the same buyer and vendor get the next child keys and a fresh address, the
//     lost one skipped because the real watch-only wallet still holds it, its label kept, the deposits apart and the
//     lost escrow listed as an orphan on the daily check;
//   - two escrows never mixed: one funding transaction pays two escrows; each claim spends only its own output;
//   - the fee cap: ten deposits at 100 sat/vB need more than the 0.001 BTC cap, so nothing is built until the rate is
//     lowered, and the claim then confirms.
func TestEscrowRegtestGate(t *testing.T) {
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
	arbiter, watch, funder := "p1d2-arbiter-"+s, "p1d2-watch-"+s, "p1d2-funder-"+s
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
	buyerWallet, vendorWallet := "p1d2-buyer-"+s, "p1d2-vendor-"+s
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
	esc := e.A.escrow.Load()
	if esc == nil {
		t.Fatal("escrow did not start against the regtest node")
	}
	t.Logf("escrow on: arbiter wallet %s, watch-only wallet %s, fee rate %d sat/vB", arbiter, watch, esc.feeRate)

	// Comma injection: two keys in one field are refused before any RPC; Core would read the descriptor built from
	// them as a 2-of-4, which the buyer and the injected key's owner could spend without the vendor or the market.
	_, intruderSess := e.user("rg2_intruder_"+s, "buyer")
	if code, body := registerKey(e, intruderSess, buyerKey+","+vendorKey); code != 400 || !strings.Contains(body, "A comma means more than one key") {
		t.Fatalf("comma key: %d %s", code, body)
	}
	if agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=(SELECT id FROM users WHERE handle=$1)", "rg2_intruder_"+s) != 0 {
		t.Fatal("a comma key was stored")
	}
	var injected struct {
		Descriptor string `json:"descriptor"`
	}
	call("", "getdescriptorinfo", &injected, "wsh(sortedmulti(2,"+buyerKey+"/0/1,"+vendorKey+"/0/1,"+buyerKey+"/0/2,"+vendorKey+"/0/2))")
	t.Logf("comma leg: a key field holding two keys -> 400 %q, nothing stored; Core reads such a descriptor as a 2-of-%d", errKeyComma.msg[:40], strings.Count(injected.Descriptor, "tpub"))

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
	buyer, buyerSess := activate("rg2_buyer", "buyer", buyerKey, buyerWallet)
	vendor, vendorSess := activate("rg2_vendor", "vendor", vendorKey, vendorWallet)
	_, admin := e.user("rg2_admin_"+s, "admin")
	mineTo := newAddr()
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
	// escrowOrder is a new escrow order (a digital listing, delivered automatically unless deliver is false) and its
	// escrow address, not yet funded.
	escrowOrder := func(deliver bool) (order, addr string) {
		t.Helper()
		product := e.product(vendor, "digital")
		if _, err := e.DB.Exec("UPDATE products SET delivery_content='regtest licence p1d2' WHERE id=$1", product); err != nil {
			t.Fatal(err)
		}
		order = e.order(buyer, product, "BTC", stateDraft)
		if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {newAddr()}}); w.Code != 303 {
			t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
		}
		if !deliver {
			if _, err := e.DB.Exec("UPDATE products SET delivery_content='' WHERE id=$1", product); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
			t.Fatal(err)
		}
		return order, addr
	}
	orderState := func(order string) string {
		var st string
		e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&st)
		return st
	}
	completedOrder := func(btc string) (order, addr, fund string) {
		t.Helper()
		order, addr = escrowOrder(true)
		call(funder, "sendtoaddress", &fund, addr, json.Number(btc))
		mine(3)
		poll()
		if st := orderState(order); st != stateCompleted {
			t.Fatalf("funded order %s", st)
		}
		return order, addr, fund
	}
	settlementOf := func(order, kind string) (id int64, state, txid, spentBy, buildErr string) {
		t.Helper()
		if err := e.DB.QueryRow(`SELECT id,state,COALESCE(txid,''),COALESCE(spent_by,''),build_error FROM settlements WHERE order_id=$1 AND kind=$2 AND state<>'superseded'
			ORDER BY id DESC LIMIT 1`, order, kind).Scan(&id, &state, &txid, &spentBy, &buildErr); err != nil {
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
	pasteCode := func(order, sess string, id int64, psbt string) (int, string) {
		w := e.do("POST", "/escrow/sign", sess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {psbt}})
		return w.Code, w.Body.String()
	}
	paste := func(order, sess string, id int64, psbt string) {
		t.Helper()
		if code, body := pasteCode(order, sess, id, psbt); code != 303 {
			t.Fatalf("paste: %d %s", code, body)
		}
	}
	claim := func(order string) {
		t.Helper()
		if w := e.do("POST", "/escrow/claim", vendorSess, url.Values{"order_id": {order}, "address": {newAddr()}, "password": {testPassword}}); w.Code != 303 {
			t.Fatalf("claim: %d %s", w.Code, w.Body.String())
		}
	}
	// confirmed mines 3 blocks, runs a pass and requires the settlement confirmed.
	confirmed := func(order, kind string) string {
		t.Helper()
		mine(3)
		poll()
		_, st, txid, _, _ := settlementOf(order, kind)
		if st != "confirmed" {
			t.Fatalf("%s of %s after 3 blocks: %s", kind, shortID(order), st)
		}
		return txid
	}
	inputsOf := func(txid string) []string {
		t.Helper()
		var raw string
		call("", "getrawtransaction", &raw, txid)
		var dec struct {
			Vin []struct {
				TxID string `json:"txid"`
				Vout int64  `json:"vout"`
			} `json:"vin"`
		}
		call("", "decoderawtransaction", &dec, raw)
		var out []string
		for _, in := range dec.Vin {
			out = append(out, in.TxID+":"+strconv.FormatInt(in.Vout, 10))
		}
		return out
	}

	// A forged market slot: the vendor's own wallet signs its copy, then a signature under the market's key is added.
	orderF, _, fundF := completedOrder("0.001")
	claim(orderF)
	poll()
	idF, stF, txidF, _, _ := settlementOf(orderF, "release")
	if stF != "awaiting_signature" {
		t.Fatalf("claim %s", stF)
	}
	signedF := walletSign(vendorWallet, partyCopy(orderF, vendorSess, idF))
	pf, err := decodePSBT(signedF)
	if err != nil {
		t.Fatal(err)
	}
	market := e.escrowKeyOf(t, orderF, "market")
	forged := pf.Raw
	for i := range pf.Inputs {
		cur, _ := parsePSBT(forged)
		forged, err = cur.withInputField(i, append([]byte{0x02}, market...), pf.Inputs[i].PartialSigs[0].Sig)
		if err != nil {
			t.Fatal(err)
		}
	}
	if code, body := pasteCode(orderF, vendorSess, idF, encodePSBT(forged)); code != 400 || !strings.Contains(body, "carries an entry under the market") {
		t.Fatalf("forged market slot: %d %s", code, body)
	}
	if _, st, _, _, _ := settlementOf(orderF, "release"); st != "awaiting_signature" || agInt(e, "SELECT count(*) FROM settlement_signatures WHERE settlement_id=$1", idF) != 0 {
		t.Fatalf("the forged paste changed the settlement: %s", st)
	}
	paste(orderF, vendorSess, idF, signedF)
	poll()
	t.Logf("forged-slot leg: escrow funded by %s; the vendor's Core-signed copy of %s with a signature added under the market key -> 400, nothing stored; the unchanged copy -> party_signed, co-signed and broadcast", fundF, txidF)
	t.Logf("forged-slot leg: %s confirmed", confirmed(orderF, "release"))

	// A-356 while awaiting a signature: the parties co-spend the escrow from their own wallets.
	orderA, addrA, fundA := completedOrder("0.001")
	claim(orderA)
	poll()
	idA, stA, txidA, _, _ := settlementOf(orderA, "release")
	if stA != "awaiting_signature" {
		t.Fatalf("claim %s", stA)
	}
	var voutA int64
	e.DB.QueryRow("SELECT idx FROM payments WHERE order_id=$1", orderA).Scan(&voutA)
	var cospentA string
	call("", "sendrawtransaction", &cospentA, coSpendFromWallets(t, call, e, orderA, fundA, voutA, newAddr(), "0.00099", buyerWallet, vendorWallet))
	poll()
	_, stA, _, byA, _ := settlementOf(orderA, "release")
	if stA != "external" || byA != cospentA || agInt(e, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id=$2 AND flagged", orderA, idA) != 1 {
		t.Fatalf("A-356 awaiting: %s spent_by %s, want external %s", stA, byA, cospentA)
	}
	if page := e.do("GET", "/order?id="+orderA, vendorSess, nil).Body.String(); strings.Contains(page, `name="psbt"`) || !strings.Contains(page, cospentA) {
		t.Fatal("the order page still asks for a signature, or does not name the co-spend")
	}
	mine(1)
	poll()
	if _, st, _, _, _ := settlementOf(orderA, "release"); st != "external" {
		t.Fatalf("after the co-spend was mined: %s", st)
	}
	t.Logf("A-356 leg (awaiting a signature): escrow %s funded by %s; claim %s waiting for a signature; the parties co-spent it from their wallets in %s (mempool); the watcher recorded the claim external with spent_by %s, flagged for staff, and the order page no longer offers the paste form", addrA, fundA, txidA, cospentA, byA)

	// A spent-outpoint build: the parties co-spend the escrow, then the vendor claims it.
	orderB, _, fundB := completedOrder("0.001")
	var voutB int64
	e.DB.QueryRow("SELECT idx FROM payments WHERE order_id=$1", orderB).Scan(&voutB)
	var cospentB string
	call("", "sendrawtransaction", &cospentB, coSpendFromWallets(t, call, e, orderB, fundB, voutB, newAddr(), "0.00099", buyerWallet, vendorWallet))
	claim(orderB)
	poll()
	idB, stB, txidB, _, errB := settlementOf(orderB, "release")
	if stB != "building" || errB != settleSpent || txidB != "" {
		t.Fatalf("a build over a spent input: %s %q txid %q", stB, errB, txidB)
	}
	t.Logf("spent-outpoint leg: escrow funded by %s, co-spent by the parties in %s (mempool) before the claim; the build refused the spent input (gettxout null): nothing built, build_error %s", fundB, cospentB, errB)
	mine(1)
	poll()
	_, stB, _, byB, _ := settlementOf(orderB, "release")
	if stB != "external" || byB != cospentB || agInt(e, "SELECT count(*) FROM payments WHERE order_id=$1 AND spent_outside AND spent_by=$2 AND flagged", orderB, cospentB) != 1 {
		t.Fatalf("A-356 building: settlement %d %s spent_by %s, want external %s", idB, stB, byB, cospentB)
	}
	t.Logf("A-356 leg (being built): after the P1-c1 wait the claim is external with spent_by %s and the deposit is marked spent outside the market with that txid", byB)

	// A conflicting outcome: an administrator's refund decision, then an attempt to change it once it is broadcast.
	orderC, addrC := escrowOrder(false)
	var fundC string
	call(funder, "sendtoaddress", &fundC, addrC, json.Number("0.001"))
	mine(3)
	poll()
	if st := orderState(orderC); st != statePaid {
		t.Fatalf("undelivered order %s", st)
	}
	if w := e.do("POST", "/disputes", buyerSess, url.Values{"order_id": {orderC}, "reason": {"The licence was never delivered to me."}}); w.Code != 303 {
		t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
	var dispute string
	e.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", orderC).Scan(&dispute)
	if w := e.do("POST", "/resolve", admin, url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"Not delivered: refunded from the escrow."},
		"payout_confirmed": {"confirmed"}, "amount_seen": {"100000"}, "reply_seen": {"0"}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("resolve: %d %s", w.Code, w.Body.String())
	}
	poll()
	idC, _, _, _, _ := settlementOf(orderC, "refund")
	paste(orderC, buyerSess, idC, walletSign(buyerWallet, partyCopy(orderC, buyerSess, idC)))
	poll()
	_, stC, txidC, _, _ := settlementOf(orderC, "refund")
	if stC != "broadcast" || !slices.Contains(mempool(), txidC) {
		t.Fatalf("dispute refund: %s", stC)
	}
	w := e.do("POST", "/escrow/decision", admin, url.Values{"order_id": {orderC}, "outcome": {"release"}, "reason": {"Changing my mind after the refund was broadcast."}, "password": {testPassword}})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "The decision is fixed") {
		t.Fatalf("changing a broadcast decision: %d %s", w.Code, w.Body.String())
	}
	if _, err := e.DB.Exec("INSERT INTO settlements(order_id,kind,state,destination) VALUES($1,'release','building',$2)", orderC, newAddr()); err == nil || !strings.Contains(err.Error(), "settlements_one_outcome") {
		t.Fatal("the database accepted a second outcome for the order")
	}
	poll()
	if agInt(e, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind IN ('release','refund')", orderC) != 1 {
		t.Fatal("a second outcome was built")
	}
	t.Logf("conflicting-outcome leg: escrow funded by %s; administrator's refund decision, buyer-signed, co-signed and broadcast as %s; changing the decision to a release -> 409 (fixed), a second outcome row refused by the database, nothing else built", fundC, txidC)
	t.Logf("conflicting-outcome leg: %s confirmed", confirmed(orderC, "refund"))

	// The restore gate: nothing is co-signed while it is set; a restored stored transaction waits for an administrator.
	orderG, _, fundG := completedOrder("0.001")
	claim(orderG)
	poll()
	idG, _, txidG, _, _ := settlementOf(orderG, "release")
	paste(orderG, vendorSess, idG, walletSign(vendorWallet, partyCopy(orderG, vendorSess, idG)))
	gateOn := func(on bool) {
		t.Helper()
		q := "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'"
		if !on {
			q = "DELETE FROM settings WHERE key='payments_recovery_required'"
		}
		if _, err := e.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	gateOn(true)
	poll()
	poll()
	if _, st, _, _, _ := settlementOf(orderG, "release"); st != "party_signed" || slices.Contains(mempool(), txidG) {
		t.Fatalf("under the restore gate: %s", st)
	}
	t.Logf("restore-gate leg: escrow funded by %s; claim %s party-signed by the vendor; two passes with the gate set: not co-signed, nothing in the mempool", fundG, txidG)
	gateOn(false)
	if err := e.A.cosignSettlements(ctx, esc); err != nil { // the co-sign step alone: stored, not yet broadcast
		t.Fatal(err)
	}
	if _, st, _, _, _ := settlementOf(orderG, "release"); st != "signed" || slices.Contains(mempool(), txidG) {
		t.Fatalf("co-signed: %s", st)
	}
	// What scripts/restore.sh does to a restored database: the gate, and a hold on every stored transaction.
	gateOn(true)
	if _, err := e.DB.Exec("UPDATE settlements SET send_error=$1 WHERE raw_tx IS NOT NULL AND state IN ('signed','broadcast') AND id=$2", restoredSettlementHold, idG); err != nil {
		t.Fatal(err)
	}
	poll()
	gateOn(false)
	poll()
	if _, st, _, _, _ := settlementOf(orderG, "release"); st != "signed" || slices.Contains(mempool(), txidG) {
		t.Fatalf("held by a restore: %s", st)
	}
	if w := e.do("POST", "/admin/escrow-rebroadcast", admin, url.Values{"settlement_id": {strconv.FormatInt(idG, 10)}, "not_sent": {"confirmed"}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("rebroadcast confirmation: %d %s", w.Code, w.Body.String())
	}
	poll()
	if _, st, _, _, _ := settlementOf(orderG, "release"); st != "broadcast" || !slices.Contains(mempool(), txidG) {
		t.Fatalf("after the administrator's confirmation: %s", st)
	}
	var sendErrG string
	e.DB.QueryRow("SELECT send_error FROM settlements WHERE id=$1", idG).Scan(&sendErrG)
	if sendErrG != "" {
		t.Fatalf("the confirmation was not used up: %q", sendErrG)
	}
	t.Logf("restore-gate leg: co-signed and stored (not broadcast); held as scripts/restore.sh holds it; with the gate set and then cleared it stayed unsent; after the administrator's password-confirmed check it was broadcast once: %s in the mempool, the confirmation used up", txidG)
	t.Logf("restore-gate leg: %s confirmed", confirmed(orderG, "release"))

	// A-359: a restored database never hands out an escrow address again. The backup point is the market counter and
	// each party's highest child index on record; an escrow created after it is lost by the restore (its rows go and
	// the counter goes back, as a restore of that backup would leave them; done in SQL here), while the watch-only
	// wallet on the node keeps watching it. Checkout waits while the restore gate is set; once it is cleared, the next
	// escrow for the same buyer and vendor composes the lost escrow's address first, finds it watched (getaddressinfo
	// on the real watch-only wallet) and takes the next child keys and a fresh address.
	childOf := func(order, party string) int64 {
		t.Helper()
		var suffix string
		if err := e.DB.QueryRow("SELECT derivation_suffix FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=$1 AND p.party=$2", order, party).Scan(&suffix); err != nil {
			t.Fatal(err)
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(suffix, "/0/"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	marketOf := func(order string) int64 {
		var n int64
		e.DB.QueryRow("SELECT market_index FROM escrows WHERE order_id=$1", order).Scan(&n)
		return n
	}
	var backupLast int64
	var backupCalled bool
	if err := e.DB.QueryRow("SELECT last_value,is_called FROM escrow_market_index").Scan(&backupLast, &backupCalled); err != nil {
		t.Fatal(err)
	}
	orderL, addrL := escrowOrder(true) // created after the backup
	buyerL, vendorL, marketL := childOf(orderL, "buyer"), childOf(orderL, "vendor"), marketOf(orderL)
	for _, q := range []string{
		"DELETE FROM escrow_pubkeys WHERE escrow_id IN (SELECT id FROM escrows WHERE order_id=$1)",
		"DELETE FROM escrows WHERE order_id=$1",
		"UPDATE orders SET state='cancelled' WHERE id=$1", // set aside: the restored database has no such order
	} {
		if _, err := e.DB.Exec(q, orderL); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.DB.Exec("SELECT setval('escrow_market_index',$1,$2)", backupLast, backupCalled); err != nil {
		t.Fatal(err)
	}
	gateOn(true)
	draftR := e.order(buyer, e.product(vendor, "digital"), "BTC", stateDraft)
	if _, err := e.DB.Exec("UPDATE products SET delivery_content='regtest licence p1d3' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", draftR); err != nil {
		t.Fatal(err)
	}
	if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {draftR}, "refund_address": {newAddr()}}); w.Code != 503 || !strings.Contains(w.Body.String(), "Bitcoin escrow is paused while this market restores its records from a backup") || orderState(draftR) != stateDraft {
		t.Fatalf("escrow checkout under the restore gate: %d %s", w.Code, w.Body.String())
	}
	gateOn(false)
	orderR, addrR := escrowOrder(true)
	if addrR == addrL || childOf(orderR, "buyer") != buyerL+1 || childOf(orderR, "vendor") != vendorL+1 || marketOf(orderR) != marketL+1 {
		t.Fatalf("the escrow after the restore: %s (lost %s), buyer /0/%d vendor /0/%d market /0/%d (lost /0/%d, /0/%d, /0/%d)",
			addrR, addrL, childOf(orderR, "buyer"), childOf(orderR, "vendor"), marketOf(orderR), buyerL, vendorL, marketL)
	}
	labelOf := func(addr string) []string {
		var info struct {
			IsMine bool     `json:"ismine"`
			Labels []string `json:"labels"`
		}
		call(watch, "getaddressinfo", &info, addr)
		if !info.IsMine {
			t.Fatalf("the watch-only wallet does not hold %s", addr)
		}
		return info.Labels
	}
	if l := labelOf(addrL); !slices.Equal(l, []string{"escrow:" + orderL}) {
		t.Fatalf("the lost escrow's label: %v", l)
	}
	if l := labelOf(addrR); !slices.Equal(l, []string{"escrow:" + orderR}) {
		t.Fatalf("the new escrow's label: %v", l)
	}
	// Coins at each address stay apart: the new order is funded by its own deposit only; the lost escrow is an orphan.
	var fundL, fundR string
	call(funder, "sendtoaddress", &fundL, addrL, json.Number("0.0005"))
	call(funder, "sendtoaddress", &fundR, addrR, json.Number("0.001"))
	mine(3)
	poll()
	if st := orderState(orderR); st != stateCompleted || !slices.Equal(agStrings(t, e, "SELECT txid FROM payments WHERE order_id=$1", orderR), []string{fundR}) {
		t.Fatalf("the new escrow's order after funding: %s", st)
	}
	if _, err := e.DB.Exec("INSERT INTO settings(key,value) VALUES('escrow_btc_stray_check','2000-01-01T00:00:00Z') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value"); err != nil {
		t.Fatal(err)
	}
	poll() // the daily check
	if got := agStrings(t, e, "SELECT order_id||' '||received FROM escrow_orphans WHERE address=$1", addrL); !slices.Equal(got, []string{orderL + " 50000"}) {
		t.Fatalf("the lost escrow on the orphan list: %v", got)
	}
	t.Logf("restore-reissue leg (A-359): escrow %s (buyer /0/%d, vendor /0/%d, market /0/%d) created after the backup point and lost by the restore; under the gate checkout -> 503, still a draft; after it, the same buyer and vendor got %s (buyer /0/%d, vendor /0/%d, market /0/%d), the lost address skipped and its label kept", addrL, buyerL, vendorL, marketL, addrR, buyerL+1, vendorL+1, marketL+1)
	t.Logf("restore-reissue leg (A-359): %s funded the lost address (listed as an orphan with 0.0005 BTC on the daily check), %s funded the new escrow, whose order completed on that deposit alone", fundL, fundR)

	// Two escrows never mixed: one transaction funds two escrows; each claim spends only its own output.
	orderM1, addrM1 := escrowOrder(true)
	orderM2, addrM2 := escrowOrder(true)
	var fundM string
	call(funder, "sendmany", &fundM, "", map[string]json.Number{addrM1: "0.001", addrM2: "0.0011"})
	mine(3)
	poll()
	var txM []string
	for _, o := range []string{orderM1, orderM2} {
		if st := orderState(o); st != stateCompleted {
			t.Fatalf("order funded by the shared transaction: %s", st)
		}
		claim(o)
	}
	poll()
	for _, o := range []string{orderM1, orderM2} {
		id, _, _, _, _ := settlementOf(o, "release")
		paste(o, vendorSess, id, walletSign(vendorWallet, partyCopy(o, vendorSess, id)))
	}
	poll()
	for _, o := range []string{orderM1, orderM2} {
		_, st, txid, _, _ := settlementOf(o, "release")
		var vout int64
		e.DB.QueryRow("SELECT idx FROM payments WHERE order_id=$1", o).Scan(&vout)
		if st != "broadcast" || !slices.Equal(inputsOf(txid), []string{fundM + ":" + strconv.FormatInt(vout, 10)}) {
			t.Fatalf("claim of %s: %s, inputs %v, want only %s:%d", shortID(o), st, inputsOf(txid), fundM, vout)
		}
		txM = append(txM, txid)
	}
	mine(3)
	poll()
	for _, o := range []string{orderM1, orderM2} {
		if _, st, _, _, _ := settlementOf(o, "release"); st != "confirmed" {
			t.Fatalf("claim of %s: %s", shortID(o), st)
		}
	}
	t.Logf("two-escrows leg: one transaction %s funded escrows %s and %s; their claims %s and %s each spend only their own output of it, and both confirmed", fundM, addrM1, addrM2, txM[0], txM[1])

	// The fee cap: ten deposits at 100 sat/vB need 100*(11+10*105+43) = 110400 sat, above the 0.001 BTC cap.
	orderX, addrX := escrowOrder(true)
	var fundsX []string
	for range 10 {
		var f string
		call(funder, "sendtoaddress", &f, addrX, json.Number("0.0001"))
		fundsX = append(fundsX, f)
	}
	mine(3)
	poll()
	if st := orderState(orderX); st != stateCompleted {
		t.Fatalf("order funded by ten deposits: %s", st)
	}
	esc.feeRate = 100
	claim(orderX)
	poll()
	_, stX, txidX, _, errX := settlementOf(orderX, "release")
	if stX != "building" || errX != settleFeeCap || txidX != "" {
		t.Fatalf("fee cap: %s %q %q", stX, errX, txidX)
	}
	t.Logf("fee-cap leg: escrow %s funded by ten deposits (%s … %s); at 100 sat/vB the build waits with build_error %s, nothing built", addrX, fundsX[0], fundsX[9], errX)
	esc.feeRate = escrowDefaultFeeRate
	poll()
	idX, stX, txidX, _, _ := settlementOf(orderX, "release")
	if stX != "awaiting_signature" {
		t.Fatalf("fee cap lifted: %s", stX)
	}
	paste(orderX, vendorSess, idX, walletSign(vendorWallet, partyCopy(orderX, vendorSess, idX)))
	poll()
	if n := len(inputsOf(txidX)); n != 10 {
		t.Fatalf("the claim spends %d inputs, want 10", n)
	}
	t.Logf("fee-cap leg: at %d sat/vB the claim was built, signed by the vendor, co-signed and broadcast as %s (10 inputs)", escrowDefaultFeeRate, txidX)
	t.Logf("fee-cap leg: %s confirmed", confirmed(orderX, "release"))

	if agInt(e, "SELECT count(*) FROM payouts") != 0 {
		t.Fatal("a payouts row for an escrow order")
	}
}

// escrowKeyOf is the stored public key of party in order's escrow.
func (e *testEnv) escrowKeyOf(t *testing.T, order, party string) []byte {
	t.Helper()
	keys, err := escrowKeys(context.Background(), e.DB, order)
	if err != nil {
		t.Fatal(err)
	}
	return keys[party].PubKey
}
