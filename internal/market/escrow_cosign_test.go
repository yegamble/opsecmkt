package market

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Escrow Phase 1, slice P1-c2: the market's co-signature, the stored final transaction, its broadcast and
// confirmation, a party signature that does not verify (A-339) and both parties signing (A-340), against the fake
// escrow Core (simulated RPC, not chain evidence: scripts/test-escrow-regtest.sh has the real-chain legs).

// segwitAddress encodes a segwit address (BIP 173 for version 0, BIP 350 above it).
func segwitAddress(hrp string, version byte, program []byte) string {
	data := []byte{version}
	acc, bits := uint32(0), uint(0)
	for _, b := range program {
		acc, bits = acc<<8|uint32(b), bits+8
		for bits >= 5 {
			bits -= 5
			data = append(data, byte(acc>>bits&31))
		}
	}
	if bits > 0 {
		data = append(data, byte(acc<<(5-bits)&31))
	}
	constant := uint32(1)
	if version > 0 {
		constant = 0x2bc830a3
	}
	mod := bech32Polymod(append(append(bech32HRPExpand(hrp), data...), 0, 0, 0, 0, 0, 0)) ^ constant
	for i := range 6 {
		data = append(data, byte(mod>>(5*(5-i))&31))
	}
	var sb strings.Builder
	sb.WriteString(hrp + "1")
	for _, d := range data {
		sb.WriteByte(bech32Charset[d])
	}
	return sb.String()
}

// regtestAddr is a valid regtest P2WPKH address for label.
func regtestAddr(label string) string {
	h := sha256.Sum256([]byte("addr:" + label))
	return segwitAddress("bcrt", 0, h[:20])
}

type settleRow struct {
	ID                                              int64
	Kind, State, Signer, TxID, Raw, Cosigned, Error string
	SpentBy                                         string
}

// settleByID reads a settlement row.
func (x *escrowEnv) settleByID(id int64) settleRow {
	x.t.Helper()
	r := settleRow{ID: id}
	if err := x.DB.QueryRow(`SELECT kind,state,COALESCE(signer,''),COALESCE(txid,''),COALESCE(raw_tx,''),COALESCE(cosigned,''),send_error,COALESCE(spent_by,'')
		FROM settlements WHERE id=$1`, id).Scan(&r.Kind, &r.State, &r.Signer, &r.TxID, &r.Raw, &r.Cosigned, &r.Error, &r.SpentBy); err != nil {
		x.t.Fatal(err)
	}
	return r
}

// built is a completed escrow order (refund address refund, funded with sats) whose vendor claimed it to claimTo, and
// the claim built: the order and the settlement.
func (x *escrowEnv) built(refund, claimTo string, sats ...int64) (string, int64) {
	x.t.Helper()
	order := x.completed(refund, sats...)
	if code, body := x.claim(order, x.vendorSess, claimTo); code != 303 {
		x.t.Fatalf("claim: %d %s", code, body)
	}
	x.poll()
	id, _, state, class := x.settlement(order)
	if state != "awaiting_signature" {
		x.t.Fatalf("claim %s %s", state, class)
	}
	return order, id
}

// partySess is the session and key fingerprint of party.
func (x *escrowEnv) partySess(party string) (string, string) {
	key, sess := x.vendorKey, x.vendorSess
	if party == roleBuyer {
		key, sess = x.buyerKey, x.buyerSess
	}
	k, err := parseEscrowKey(key)
	if err != nil {
		x.t.Fatal(err)
	}
	return sess, k.Fingerprint
}

// signAs pastes party's good signature (its wallet signing its own copy) and requires it accepted.
func (x *escrowEnv) signAs(order, party string, id int64) {
	x.t.Helper()
	sess, fp := x.partySess(party)
	if code, body := x.sign(order, sess, id, walletSign(x.t, x.partyCopy(order, sess, id), fp, 1)); code != 303 {
		x.t.Fatalf("%s signs: %d %s", party, code, body)
	}
}

// Signatures that pass every paste check but do not verify (A-339): a signature over another amount (the party's
// wallet shown a different input value signs a different sighash) and r = s = 1, a well-formed DER that signs nothing.
var badSigs = map[string]func(pub []byte, txid string) []byte{
	"over the wrong amount": func(pub []byte, txid string) []byte { return fakeSig(pub, "amount-0.0009:"+txid, 1) },
	"r=s=1":                 func([]byte, string) []byte { return []byte{0x30, 0x06, 0x02, 0x01, 0x01, 0x02, 0x01, 0x01, 0x01} },
}

// signBad pastes party's copy carrying, on every input, a bad signature from the party's own key.
func (x *escrowEnv) signBad(order, party string, id int64, bad func(pub []byte, txid string) []byte) {
	x.t.Helper()
	sess, _ := x.partySess(party)
	cp := x.partyCopy(order, sess, id)
	p, err := decodePSBT(cp)
	if err != nil {
		x.t.Fatal(err)
	}
	pub, _ := hex.DecodeString(x.keyHex(order, party))
	raw := p.Raw
	for i := range p.Inputs {
		raw = addField(x.t, raw, i, append([]byte{0x02}, pub...), bad(pub, p.TxID()))
	}
	if code, body := x.sign(order, sess, id, encodePSBT(raw)); code != 303 {
		x.t.Fatalf("%s's bad signature was not accepted as well-formed: %d %s", party, code, body)
	}
}

// killAtSend makes the next broadcast end the pass as a process killed at that moment would: the pass's context is
// cancelled and the node never receives the transaction. sendNormally undoes it.
func (x *escrowEnv) killAtSend() context.Context {
	ctx, kill := context.WithCancel(context.Background())
	x.core.mu.Lock()
	x.core.beforeSend = func() {
		kill()
		x.core.mu.Lock()
		x.core.sendErr, x.core.beforeSend = -32603, nil
		x.core.mu.Unlock()
	}
	x.core.mu.Unlock()
	return ctx
}

func (x *escrowEnv) sendNormally() {
	x.core.mu.Lock()
	x.core.sendErr, x.core.beforeSend = 0, nil
	x.core.mu.Unlock()
}

// cosignCalls is the co-sign and broadcast RPCs called since the n-th call.
func (x *escrowEnv) cosignCalls(n int) []string {
	return slices.DeleteFunc(x.core.methods()[n:], func(m string) bool {
		return !slices.Contains([]string{"deriveaddresses", "getaddressinfo", "keypoolrefill", "walletprocesspsbt", "combinepsbt", "finalizepsbt", "testmempoolaccept", "sendrawtransaction"}, m)
	})
}

// The vendor's claim end to end (ES-2, ES-4): the vendor signs; the next watcher pass co-signs the pristine stored
// copy with the arbiter wallet (sighash ALL), combines it market copy first with the stored vendor signatures,
// finalizes, checks the transaction, runs testmempoolaccept at the rate cap, stores the raw transaction and only then
// broadcasts exactly that transaction. Neither the market-signed PSBT nor (before the broadcast) the raw transaction is
// shown anywhere; after the broadcast both parties see the transaction, and at the threshold it is confirmed.
func TestEscrowCosignBroadcastConfirm(t *testing.T) {
	x := newEscrowEnv(t)
	t.Setenv("ESCROW_BTC_FEE_BPS", "1500")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", regtestAddr("market-fee"))
	x.restart()
	refund, claimTo := regtestAddr("refund-1"), regtestAddr("claim-1")
	order, id := x.built(refund, claimTo, 70000, 50000)
	var stored, txid string
	x.DB.QueryRow("SELECT psbt,txid FROM settlements WHERE id=$1", id).Scan(&stored, &txid)
	calls := len(x.core.methods())
	x.signAs(order, roleVendor, id)
	if got := x.cosignCalls(calls); len(got) != 0 {
		t.Fatalf("the paste itself called %v: only the watcher co-signs", got)
	}
	var marketSigned string
	x.core.mu.Lock()
	x.core.marketSkew = func(s string) string { marketSigned = s; return s }
	x.core.mu.Unlock()
	calls = len(x.core.methods())
	x.poll()
	want := []string{"deriveaddresses", "getaddressinfo", "walletprocesspsbt", "combinepsbt", "finalizepsbt", "testmempoolaccept", "sendrawtransaction"}
	if got := x.cosignCalls(calls); !slices.Equal(got, want) {
		t.Fatalf("co-sign RPCs %v\nwant %v", got, want)
	}
	keys, err := escrowKeys(context.Background(), x.DB, order)
	if err != nil {
		t.Fatal(err)
	}
	marketCopy, _ := settlementCopy(stored, keys["market"])
	if path, params := x.core.lastCall(t, "walletprocesspsbt"); path != "/wallet/opsecmkt-arbiter" || params != `["`+marketCopy+`",true,"ALL",true,false]` {
		t.Fatalf("walletprocesspsbt %s %s", path, params)
	}
	sp, _ := decodePSBT(stored)
	var vsigs []psbtSig
	for _, r := range agStrings(t, x.testEnv, "SELECT pubkey||' '||signature FROM settlement_signatures WHERE settlement_id=$1 AND party='vendor' ORDER BY input_n", id) {
		f := strings.Fields(r)
		pub, _ := hex.DecodeString(f[0])
		sig, _ := hex.DecodeString(f[1])
		vsigs = append(vsigs, psbtSig{pub, sig})
	}
	partyCopy, _ := withPartySigs(sp, partySigs{roleVendor, vsigs})
	if _, params := x.core.lastCall(t, "combinepsbt"); params != `[["`+marketSigned+`","`+partyCopy+`"]]` {
		t.Fatalf("combinepsbt: not the market copy first and the stored copy with the vendor's signatures:\n%s", params)
	}
	r := x.settleByID(id)
	if r.State != "broadcast" || r.Cosigned != "market" || r.Raw == "" || r.Error != "" {
		t.Fatalf("after the pass: %+v", r)
	}
	if _, params := x.core.lastCall(t, "testmempoolaccept"); params != `[["`+r.Raw+`"],0.001]` {
		t.Fatalf("testmempoolaccept %s", params)
	}
	if _, params := x.core.lastCall(t, "sendrawtransaction"); params != `["`+r.Raw+`",0.001]` {
		t.Fatalf("sendrawtransaction is not the stored transaction: %s", params)
	}
	raw, _ := hex.DecodeString(r.Raw)
	final, err := parseSignedTx(raw)
	if err != nil || final.TxID() != txid {
		t.Fatalf("final txid %v, stored %s", err, txid)
	}
	outs := agStrings(t, x.testEnv, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
	if !slices.Equal(outs, []string{"claimant " + claimTo + " 84300", "market_fee " + regtestAddr("market-fee") + " 15000", "buyer_return " + refund + " 20000"}) {
		t.Fatalf("outputs %q", outs)
	}
	for i, o := range final.Out {
		script, _ := regtestAddressScript(strings.Fields(outs[i])[1])
		if !slices.Equal(o.Script, script) {
			t.Fatalf("output %d script", i)
		}
	}
	if marketSigned == "" || strings.Contains(x.page("/order?id="+order, x.vendorSess), marketSigned) {
		t.Fatal("the market-signed PSBT was not captured, or is shown")
	}
	assertNowhereInDB(t, x.testEnv, marketSigned)
	assertNowhereInDB(t, x.testEnv, strings.TrimRight(marketSigned, "=")[len(marketSigned)-120:])
	var after string
	x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&after)
	if after != stored {
		t.Fatal("the stored PSBT changed")
	}
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		page := html.UnescapeString(x.page("/order?id="+order, sess))
		if !strings.Contains(page, "Broadcast to the regtest network") || !strings.Contains(page, txid) || !strings.Contains(page, r.Raw) || strings.Contains(page, "co-signs it and broadcasts it on its next pass") {
			t.Fatalf("broadcast page:\n%s", page)
		}
	}
	for _, uid := range []string{x.buyer, x.vendor} {
		if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%co-signed the escrow settlement%'", uid) != 1 ||
			agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%was broadcast to the regtest network%'", uid) != 1 {
			t.Fatal("a party was not told of the co-signature and the broadcast")
		}
	}
	// Unconfirmed: the next pass sends the same stored transaction again (idempotent); it never signs again.
	signs, sends := x.core.count("walletprocesspsbt"), x.core.count("sendrawtransaction")
	x.poll()
	if x.core.count("walletprocesspsbt") != signs || x.core.count("sendrawtransaction") != sends+1 || x.settleByID(id).State != "broadcast" {
		t.Fatal("an unconfirmed broadcast was not sent again as stored, or was signed again")
	}
	if _, params := x.core.lastCall(t, "sendrawtransaction"); params != `["`+r.Raw+`",0.001]` {
		t.Fatal("rebroadcast of another transaction")
	}
	x.core.setTxConfs(txid, 3, "")
	x.poll()
	if r := x.settleByID(id); r.State != "confirmed" {
		t.Fatalf("at 3 confirmations: %s", r.State)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%is confirmed on the regtest network%'", x.buyer) != 1 {
		t.Fatal("confirmation notice")
	}
	if page := x.page("/order?id="+order, x.buyerSess); !strings.Contains(page, "Confirmed on the regtest network") {
		t.Fatalf("confirmed page:\n%s", page)
	}
	x.noPayouts(order)
}

// A broadcast settlement is confirmed at PAYMENT_CONFIRMATIONS_BTC (3 by default) and not before (A-347): at 1 and 2
// confirmations it stays broadcast and nobody is told it is confirmed.
func TestEscrowConfirmationThreshold(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.built(regtestAddr("refund-th"), regtestAddr("claim-th"), 100000)
	x.signAs(order, roleVendor, id)
	x.poll()
	txid := x.settleByID(id).TxID
	for _, confs := range []int64{1, 2} {
		x.core.setTxConfs(txid, confs, "")
		x.poll()
		if r := x.settleByID(id); r.State != "broadcast" {
			t.Fatalf("at %d confirmations: %+v", confs, r)
		}
		if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE body LIKE '%is confirmed on the regtest network%'") != 0 {
			t.Fatalf("at %d confirmations a party was told it is confirmed", confs)
		}
	}
	x.core.setTxConfs(txid, 3, "")
	x.poll()
	if r := x.settleByID(id); r.State != "confirmed" {
		t.Fatalf("at 3 confirmations: %+v", r)
	}
}

// A signed settlement the node refuses (-26) because its inputs are spent is not external when the watch-only wallet
// knows the stored transaction: the node has it, so the settlement is broadcast (A-347). A wallet that does not know
// it leaves it external.
func TestEscrowBroadcastRefusedButOwnTransactionSpent(t *testing.T) {
	x := newEscrowEnv(t)
	for i, c := range []struct {
		name  string
		known bool
		state string
	}{{"the wallet knows the stored transaction", true, "broadcast"}, {"the wallet does not know it", false, "external"}} {
		order, id := x.built(regtestAddr("refund-os"+strconv.Itoa(i)), regtestAddr("claim-os"+strconv.Itoa(i)), 100000)
		x.signAs(order, roleVendor, id)
		x.A.escrowPollOnce(x.killAtSend())
		x.sendNormally()
		r := x.settleByID(id)
		if r.State != "signed" || r.Raw == "" {
			t.Fatalf("%s: before the pass: %+v", c.name, r)
		}
		x.core.spend(txidOf(order), 0) // its inputs are spent
		if c.known {
			x.core.mu.Lock()
			if x.core.broadcast == nil {
				x.core.broadcast, x.core.txConfs = map[string]string{}, map[string]int64{}
			}
			x.core.broadcast[r.TxID], x.core.txConfs[r.TxID] = r.Raw, 1 // by this very transaction: the node and the wallet have it
			x.core.mu.Unlock()
		}
		x.core.mu.Lock()
		x.core.sendErr = -26
		x.core.mu.Unlock()
		x.poll()
		x.sendNormally()
		if got := x.settleByID(id); got.State != c.state {
			t.Fatalf("%s: %+v, want %s", c.name, got, c.state)
		}
	}
}

// The market co-signs only a party-signed settlement whose kind fits the order's terminal state (ES-2): nothing is
// signed while the settlement awaits a signature, nor for an order not in a terminal state, nor for a release on an
// order resolved for the buyer.
func TestEscrowCosignOnlyTerminalAndPartySigned(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.built(regtestAddr("refund-2"), regtestAddr("claim-2"), 100000)
	x.poll()
	if x.core.count("walletprocesspsbt") != 0 || x.settleByID(id).State != "awaiting_signature" {
		t.Fatal("a settlement awaiting a signature was co-signed")
	}
	x.signAs(order, roleVendor, id)
	for _, c := range []struct{ name, sql string }{
		{"an order not in a terminal state", "UPDATE orders SET state='delivered' WHERE id=$1"},
		{"a release of an order resolved for the buyer", "UPDATE orders SET state='resolved' WHERE id=$1"},
	} {
		if _, err := x.DB.Exec(c.sql, order); err != nil {
			t.Fatal(err)
		}
		if c.name == "a release of an order resolved for the buyer" {
			x.DB.Exec("INSERT INTO disputes(id,order_id,reason,outcome,status) VALUES($1,$2,'The content was never delivered to me at all.','refund','Resolved — refund to buyer')", randomToken(), order)
		}
		x.poll()
		if x.core.count("walletprocesspsbt") != 0 || x.core.count("finalizepsbt") != 0 || x.settleByID(id).State != "party_signed" {
			t.Fatalf("%s: co-signed", c.name)
		}
	}
	x.DB.Exec("DELETE FROM disputes WHERE order_id=$1", order)
	x.DB.Exec("UPDATE orders SET state='completed' WHERE id=$1", order)
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || x.core.count("walletprocesspsbt") != 1 {
		t.Fatalf("back to completed: %+v", r)
	}
}

// Before anything leaves, the final transaction is re-checked; each failed check refuses with nothing stored or
// broadcast, the settlement stays party-signed with a fixed class, and the parties are not told it was signed. The
// market's own signature must be the only one on its copy, on every input, with SIGHASH_ALL.
func TestEscrowCosignRechecksRefuse(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.built(regtestAddr("refund-3"), regtestAddr("claim-3"), 60000, 40000)
	x.signAs(order, roleVendor, id)
	var stored string
	x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored)
	sp, _ := decodePSBT(stored)
	rewrite := func(f func(t *signedTx, raw []byte) []byte) func(string) string {
		return func(h string) string {
			raw, _ := hex.DecodeString(h)
			tx, err := parseSignedTx(raw)
			if err != nil {
				t.Fatal(err)
			}
			return hex.EncodeToString(f(tx, slices.Clone(raw)))
		}
	}
	// lastSigByte is the offset of the last byte of input 0's first witness signature.
	lastSigByte := func(raw []byte) int {
		off := 4 + 2 + len(sp.UnsignedTx) - 8 // version, marker and flag, the unsigned body without version and lock time
		return off + 1 + 1 + 1 + 71 - 1       // item count, empty item, signature length, its 71 bytes (fakeSig)
	}
	sid := strconv.FormatInt(id, 10)
	cases := []struct {
		name, class string
		market      func(string) string
		final       func(string) string
		db          string
	}{
		{"another transaction (lock time)", finalOtherTx, nil, rewrite(func(_ *signedTx, raw []byte) []byte { raw[len(raw)-1] = 1; return raw }), ""},
		{"a witness signature that is not SIGHASH_ALL", finalWitness, nil, rewrite(func(_ *signedTx, raw []byte) []byte { raw[lastSigByte(raw)] = 0x02; return raw }), ""},
		{"an output that is not the frozen one", finalOutputs, nil, nil, "UPDATE settlement_outputs SET address='" + regtestAddr("elsewhere") + "' WHERE settlement_id=$1"},
		{"a fee that is not the stored one", finalFee, nil, nil, "UPDATE settlements SET network_fee=network_fee+1 WHERE id=$1"},
		{"a fee above the absolute cap", finalFee, nil, nil, "UPDATE settlements SET network_fee=network_fee+200000,input_total=input_total+200000 WHERE id=$1"},
		{"a market signature with SIGHASH_NONE", sendBadReply, func(b64 string) string {
			p, _ := decodePSBT(b64)
			raw, _ := p.withoutInputFields(func(_ int, k []byte) bool { return k[0] == 0x02 })
			mpub, _ := hex.DecodeString(x.keyHex(order, "market"))
			for i := range p.Inputs {
				raw = addField(t, raw, i, append([]byte{0x02}, mpub...), fakeSig(mpub, p.TxID(), 2))
			}
			return encodePSBT(raw)
		}, nil, ""},
		{"a market copy carrying another signature", sendBadReply, func(b64 string) string {
			p, _ := decodePSBT(b64)
			bpub, _ := hex.DecodeString(x.keyHex(order, "buyer"))
			return encodePSBT(addField(t, p.Raw, 1, append([]byte{0x02}, bpub...), fakeSig(bpub, p.TxID(), 1)))
		}, nil, ""},
		{"a market copy whose only signature is under another key", sendBadReply, func(b64 string) string {
			p, _ := decodePSBT(b64)
			raw, _ := p.withoutInputFields(func(_ int, k []byte) bool { return k[0] == 0x02 })
			bpub, _ := hex.DecodeString(x.keyHex(order, "buyer"))
			for i := range p.Inputs {
				raw = addField(t, raw, i, append([]byte{0x02}, bpub...), fakeSig(bpub, p.TxID(), 1))
			}
			return encodePSBT(raw)
		}, nil, ""},
		{"a market copy the arbiter did not sign", sendMarketKey, func(b64 string) string {
			p, _ := decodePSBT(b64)
			raw, _ := p.withoutInputFields(func(_ int, k []byte) bool { return k[0] == 0x02 })
			return encodePSBT(raw)
		}, nil, ""},
	}
	for _, c := range cases {
		x.core.mu.Lock()
		x.core.marketSkew, x.core.finalSkew = c.market, c.final
		x.core.mu.Unlock()
		if c.db != "" {
			x.DB.Exec("ALTER TABLE settlements DISABLE TRIGGER settlements_frozen")
			x.DB.Exec("ALTER TABLE settlement_outputs DISABLE TRIGGER settlement_outputs_frozen")
			x.DB.Exec("CREATE TABLE so_backup AS SELECT * FROM settlement_outputs WHERE settlement_id=" + sid)
			x.DB.Exec("CREATE TABLE s_backup AS SELECT network_fee,input_total FROM settlements WHERE id=" + sid)
			if _, err := x.DB.Exec(c.db, id); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
		}
		sends := x.core.count("sendrawtransaction")
		x.poll()
		r := x.settleByID(id)
		if r.State != "party_signed" || r.Raw != "" || r.Error != c.class || x.core.count("sendrawtransaction") != sends {
			t.Fatalf("%s: %+v (sent %d)", c.name, r, x.core.count("sendrawtransaction")-sends)
		}
		if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE body LIKE '%The market co-signed%' OR body LIKE '%was broadcast to the regtest network%'") != 0 {
			t.Fatalf("%s: a party was told", c.name)
		}
		if c.db != "" {
			x.DB.Exec("DELETE FROM settlement_outputs WHERE settlement_id=$1", id)
			x.DB.Exec("INSERT INTO settlement_outputs SELECT * FROM so_backup")
			x.DB.Exec("UPDATE settlements SET network_fee=b.network_fee,input_total=b.input_total FROM s_backup b WHERE id=$1", id)
			x.DB.Exec("DROP TABLE so_backup")
			x.DB.Exec("DROP TABLE s_backup")
			x.DB.Exec("ALTER TABLE settlements ENABLE TRIGGER settlements_frozen")
			x.DB.Exec("ALTER TABLE settlement_outputs ENABLE TRIGGER settlement_outputs_frozen")
		}
	}
	if page := x.page("/order?id="+order, x.buyerSess); strings.Contains(page, "Broadcast") {
		t.Fatal("a refused settlement reads as broadcast")
	}
	x.core.mu.Lock()
	x.core.marketSkew, x.core.finalSkew = nil, nil
	x.core.mu.Unlock()
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || r.Error != "" {
		t.Fatalf("once the checks pass: %+v", r)
	}
}

// The raw transaction is stored before the first broadcast, and only it is ever broadcast (ES-2): a process killed
// between storing and broadcasting broadcasts the stored transaction after a restart, without building or signing
// again; an answer of -27 (already confirmed) counts as sent; the database refuses any rebuild of a settlement with a
// final transaction.
func TestEscrowRawStoredBeforeBroadcast(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.built(regtestAddr("refund-4"), regtestAddr("claim-4"), 100000)
	x.signAs(order, roleVendor, id)
	if err := x.A.escrowPollOnce(x.killAtSend()); err == nil {
		t.Fatal("the killed pass reported no error")
	}
	r := x.settleByID(id)
	if r.State != "signed" || r.Raw == "" || len(x.core.broadcast) != 0 {
		t.Fatalf("after the kill: %+v, broadcast %d", r, len(x.core.broadcast))
	}
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		if page := x.page("/order?id="+order, sess); strings.Contains(page, r.Raw) || !strings.Contains(page, "broadcasts it on its next pass") {
			t.Fatalf("a signed, unbroadcast settlement's page:\n%s", page)
		}
	}
	x.sendNormally()
	x.restart()
	counts := map[string]int{}
	for _, m := range []string{"createpsbt", "utxoupdatepsbt", "walletprocesspsbt", "combinepsbt", "finalizepsbt", "testmempoolaccept"} {
		counts[m] = x.core.count(m)
	}
	x.poll()
	for m, n := range counts {
		if x.core.count(m) != n {
			t.Fatalf("after the restart the market called %s again", m)
		}
	}
	if _, params := x.core.lastCall(t, "sendrawtransaction"); params != `["`+r.Raw+`",0.001]` {
		t.Fatalf("after the restart: %s", params)
	}
	if got := x.settleByID(id); got.State != "broadcast" || got.Raw != r.Raw {
		t.Fatalf("after the restart: %+v", got)
	}
	// The database refuses a rebuild, a new final transaction, a step back, and removing the signatures it used.
	for _, q := range []string{
		"UPDATE settlements SET psbt='cHNidP8x' WHERE id=$1",
		"UPDATE settlements SET raw_tx='00' WHERE id=$1",
		"UPDATE settlements SET state='party_signed' WHERE id=$1",
		"UPDATE settlements SET state='awaiting_signature',signer=NULL WHERE id=$1",
		"UPDATE settlements SET state='superseded' WHERE id=$1",
		"DELETE FROM settlement_signatures WHERE settlement_id=$1",
	} {
		if _, err := x.DB.Exec(q, id); err == nil {
			t.Fatalf("the database allowed %s", q)
		}
	}

	// -27: the node already has it in a block.
	order2, id2 := x.built(regtestAddr("refund-5"), regtestAddr("claim-5"), 100000)
	x.signAs(order2, roleVendor, id2)
	x.core.mu.Lock()
	x.core.sendErr = -27
	x.core.mu.Unlock()
	x.poll()
	x.core.mu.Lock()
	x.core.sendErr = 0
	x.core.mu.Unlock()
	if r := x.settleByID(id2); r.State != "broadcast" {
		t.Fatalf("-27: %+v", r)
	}
	// Any other refusal leaves it signed, to be sent again as stored.
	order3, id3 := x.built(regtestAddr("refund-6"), regtestAddr("claim-6"), 100000)
	x.signAs(order3, roleVendor, id3)
	x.core.mu.Lock()
	x.core.sendErr = -26
	x.core.mu.Unlock()
	x.poll()
	if r := x.settleByID(id3); r.State != "signed" || r.Error != sendRejected {
		t.Fatalf("-26: %+v", r)
	}
	x.core.mu.Lock()
	x.core.sendErr = 0
	x.core.mu.Unlock()
	x.poll()
	if r := x.settleByID(id3); r.State != "broadcast" {
		t.Fatalf("after -26: %+v", r)
	}
}

// An escrow output spent by another transaction is reconciled as external and flagged for staff: a broadcast
// settlement the wallet reports conflicted (with the conflicting txid), a party-signed one whose input is gone before
// the co-signature, and a signed one whose input is gone before the broadcast.
func TestEscrowExternalSpend(t *testing.T) {
	x := newEscrowEnv(t)
	_, mod := x.user("emod_"+randomToken()[:6], "moderator")
	order, id := x.built(regtestAddr("refund-7"), regtestAddr("claim-7"), 100000)
	x.signAs(order, roleVendor, id)
	x.poll()
	r := x.settleByID(id)
	other := txidOf("someone else's spend")
	x.core.setTxConfs(r.TxID, -2, other)
	x.poll()
	if r := x.settleByID(id); r.State != "external" || r.SpentBy != other {
		t.Fatalf("conflicted broadcast: %+v", r)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1 AND flagged", id) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE body LIKE 'Payment review needed%did not make%'") == 0 {
		t.Fatal("not flagged for staff")
	}
	if page := x.page("/order?id="+order, mod); !strings.Contains(page, "Spent outside the market") {
		t.Fatalf("staff page:\n%s", page)
	}

	// A wallet that does not know the market's own broadcast transaction (its outputs spent by it): the node still has
	// it (its txid, or -27 once in a block), so it is not external.
	orderW, idW := x.built(regtestAddr("refund-w"), regtestAddr("claim-w"), 100000)
	x.signAs(orderW, roleVendor, idW)
	x.poll()
	x.core.mu.Lock()
	x.core.walletForgets = true
	x.core.mu.Unlock()
	x.poll()
	x.core.setTxConfs(x.settleByID(idW).TxID, 2, "")
	x.poll()
	if r := x.settleByID(idW); r.State != "broadcast" {
		t.Fatalf("own transaction unknown to the wallet: %+v", r)
	}
	x.core.mu.Lock()
	x.core.walletForgets = false
	x.core.mu.Unlock()
	x.core.setTxConfs(x.settleByID(idW).TxID, 3, "")
	x.poll()
	if r := x.settleByID(idW); r.State != "confirmed" {
		t.Fatalf("own transaction, known again: %+v", r)
	}
	sendsBefore := x.core.count("sendrawtransaction")

	order2, id2 := x.built(regtestAddr("refund-8"), regtestAddr("claim-8"), 100000)
	x.signAs(order2, roleVendor, id2)
	x.core.spend(txidOf(order2), 0)
	x.poll()
	if r := x.settleByID(id2); r.State != "external" || x.core.count("sendrawtransaction") != sendsBefore {
		t.Fatalf("party-signed, input gone: %+v", r)
	}

	order3, id3 := x.built(regtestAddr("refund-9"), regtestAddr("claim-9"), 100000)
	x.signAs(order3, roleVendor, id3)
	x.A.escrowPollOnce(x.killAtSend())
	x.sendNormally()
	if r := x.settleByID(id3); r.State != "signed" {
		t.Fatalf("before the spend: %+v", r)
	}
	x.core.spend(txidOf(order3), 0)
	x.poll()
	if r := x.settleByID(id3); r.State != "external" {
		t.Fatalf("signed, input gone: %+v", r)
	}
}

// A-339: a well-formed party signature that does not verify reaches party_signed (the app cannot check it without the
// market's co-signature), then the watcher's finalize fails: the settlement returns to awaiting a signature, the bad
// signature is discarded, a fixed note is written and both parties are told; nothing is broadcast. The same holds for
// a script testmempoolaccept refuses. A good signature then completes it.
func TestEscrowBadSignatureReverts(t *testing.T) {
	for name, bad := range badSigs {
		t.Run(name, func(t *testing.T) {
			x := newEscrowEnv(t)
			order, id := x.built(regtestAddr("refund-a"), regtestAddr("claim-a"), 60000, 40000)
			x.signBad(order, roleVendor, id, bad)
			if r := x.settleByID(id); r.State != "party_signed" {
				t.Fatalf("bad signature: %+v", r)
			}
			x.poll()
			r := x.settleByID(id)
			if r.State != "awaiting_signature" || r.Signer != "" || r.Raw != "" || x.core.count("sendrawtransaction") != 0 ||
				agInt(x.testEnv, "SELECT count(*) FROM settlement_signatures WHERE settlement_id=$1", id) != 0 {
				t.Fatalf("after the pass: %+v", r)
			}
			ev := strings.Join(x.events(order), "\n")
			if !strings.Contains(ev, "The vendor's signature on the escrow settlement (the vendor's claim) did not verify") || !strings.Contains(ev, "waits for a signature again") {
				t.Fatalf("note:\n%s", ev)
			}
			for _, uid := range []string{x.buyer, x.vendor} {
				if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%did not verify%waits for a signature again%'", uid) != 1 {
					t.Fatal("a party was not told")
				}
			}
			if page := x.page("/order?id="+order, x.buyerSess); !strings.Contains(page, `action="/escrow/sign"`) {
				t.Fatal("the reverted settlement offers no paste form")
			}
			x.signAs(order, roleVendor, id)
			x.poll()
			if r := x.settleByID(id); r.State != "broadcast" || r.Cosigned != "market" {
				t.Fatalf("the good signature: %+v", r)
			}
		})
	}
	t.Run("a script testmempoolaccept refuses", func(t *testing.T) {
		x := newEscrowEnv(t)
		order, id := x.built(regtestAddr("refund-b"), regtestAddr("claim-b"), 100000)
		x.signAs(order, roleBuyer, id)
		x.core.mu.Lock()
		x.core.mempoolReject = "mandatory-script-verify-flag-failed (Signature must be zero for failed CHECK(MULTI)SIG operation)"
		x.core.mu.Unlock()
		x.poll()
		if r := x.settleByID(id); r.State != "awaiting_signature" || x.core.count("sendrawtransaction") != 0 {
			t.Fatalf("%+v", r)
		}
		if !strings.Contains(strings.Join(x.events(order), "\n"), "The buyer's signature") {
			t.Fatal("note")
		}
		// Another refusal is not the party's fault: the settlement stays party-signed and waits.
		x.signAs(order, roleBuyer, id)
		x.core.mu.Lock()
		x.core.mempoolReject = "min relay fee not met"
		x.core.mu.Unlock()
		x.poll()
		if r := x.settleByID(id); r.State != "party_signed" || r.Error != sendRejected {
			t.Fatalf("%+v", r)
		}
	})
}

// A-340 (ES-4): the second party may also sign a party-signed settlement; with both signatures the market does not
// sign: the watcher finalizes the two party signatures alone. When the two do not finalize together, the market
// co-signs with whichever verifies and discards the other (A-339).
func TestEscrowBothPartiesSign(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.built(regtestAddr("refund-c"), regtestAddr("claim-c"), 60000, 40000)
	vsess, vfp := x.partySess(roleVendor)
	vpaste := walletSign(t, x.partyCopy(order, vsess, id), vfp, 1)
	if code, body := x.sign(order, vsess, id, vpaste); code != 303 {
		t.Fatalf("vendor signs: %d %s", code, body)
	}
	page := x.page("/order?id="+order, x.buyerSess)
	if !strings.Contains(page, `action="/escrow/sign"`) || !strings.Contains(page, "the market does not sign") {
		t.Fatalf("the buyer is not offered to sign too:\n%s", page)
	}
	if strings.Contains(x.page("/order?id="+order, x.vendorSess), `action="/escrow/sign"`) {
		t.Fatal("the vendor, who signed, is offered the form again")
	}
	if w := x.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10), vsess, nil); w.Code != 404 {
		t.Fatalf("the vendor, who signed, is served a copy: %d", w.Code)
	}
	if code, _ := x.sign(order, vsess, id, vpaste); code != 409 {
		t.Fatalf("the vendor signing twice: %d", code)
	}
	bsess, bfp := x.partySess(roleBuyer)
	bpaste := walletSign(t, x.partyCopy(order, bsess, id), bfp, 1)
	if code, body := x.sign(order, bsess, id, bpaste); code != 303 {
		t.Fatalf("buyer signs too: %d %s", code, body)
	}
	for who, c := range map[string]struct{ sess, paste string }{"vendor": {vsess, vpaste}, "buyer": {bsess, bpaste}} {
		if code, _ := x.sign(order, c.sess, id, c.paste); code != 409 {
			t.Fatalf("the %s signing again after both signed: %d", who, code)
		}
	}
	r := x.settleByID(id)
	if r.State != "party_signed" || r.Signer != roleVendor || agInt(x.testEnv, "SELECT count(DISTINCT party) FROM settlement_signatures WHERE settlement_id=$1", id) != 2 {
		t.Fatalf("both signed: %+v", r)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%the buyer also signed%'", x.vendor) != 1 {
		t.Fatal("the vendor was not told the buyer signed too")
	}
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || r.Cosigned != "parties" || x.core.count("walletprocesspsbt") != 0 || x.core.count("combinepsbt") != 0 {
		t.Fatalf("both parties' signatures: %+v, walletprocesspsbt %d", r, x.core.count("walletprocesspsbt"))
	}
	if !strings.Contains(strings.Join(x.events(order), "\n"), "so the market did not sign it") {
		t.Fatal("note")
	}

	for _, c := range []struct{ name, first, good string }{{"the second signature is bad", roleVendor, roleVendor}, {"the first signature is bad", roleVendor, roleBuyer}} {
		order, id := x.built(regtestAddr("refund-"+c.name), regtestAddr("claim-"+c.name), 100000)
		if c.good == c.first {
			x.signAs(order, c.first, id)
			x.signBad(order, otherParty(c.first), id, badSigs["r=s=1"])
		} else {
			x.signBad(order, c.first, id, badSigs["over the wrong amount"])
			x.signAs(order, c.good, id)
		}
		x.poll()
		r := x.settleByID(id)
		if r.State != "broadcast" || r.Cosigned != "market" || r.Signer != c.good {
			t.Fatalf("%s: %+v", c.name, r)
		}
		bad := otherParty(c.good)
		if agInt(x.testEnv, "SELECT count(*) FROM settlement_signatures WHERE settlement_id=$1 AND party=$2", id, bad) != 0 ||
			!strings.Contains(strings.Join(x.events(order), "\n"), "The "+bad+"'s signature on the escrow settlement") {
			t.Fatalf("%s: the bad signature was not discarded with a note", c.name)
		}
	}
}

// TestEscrowFinalTxChecks pins checkFinalTx on a transaction built by hand: the stored one passes; another lock time,
// another output value, a fee above the rate cap, a witness with three items or a signature not ending in 0x01 fail
// with their classes.
func TestEscrowFinalTxChecks(t *testing.T) {
	ws := sortedMultisigScript(fakeChild("a"), fakeChild("b"), fakeChild("c"))
	sum := sha256.Sum256(ws)
	dest := regtestAddr("final-check")
	destScript, _ := regtestAddressScript(dest)
	in := kv{[]byte{0x01}, append(append([]byte{0xa0, 0x86, 1, 0, 0, 0, 0, 0}, 34, 0, 32), sum[:]...)}
	unsigned := []byte{2, 0, 0, 0, 1}
	unsigned = append(unsigned, make([]byte, 32)...)
	unsigned = append(unsigned, 0, 0, 0, 0, 0, 0xfd, 0xff, 0xff, 0xff, 1)
	unsigned = append(append(append(unsigned, 0x1e, 0x85, 1, 0, 0, 0, 0, 0), byte(len(destScript))), destScript...) // 99614 sat
	unsigned = append(unsigned, 0, 0, 0, 0)
	stored, err := parsePSBT(buildPSBT(psbtMap(kv{[]byte{0}, unsigned}), psbtMap(in, kv{[]byte{0x05}, ws}), psbtMap()))
	if err != nil {
		t.Fatal(err)
	}
	sig := fakeSig(fakeChild("a"), stored.TxID(), 1)
	final := func(u []byte, items ...[]byte) string {
		w := compactSize(len(items))
		for _, it := range items {
			w = append(append(w, compactSize(len(it))...), it...)
		}
		return hex.EncodeToString(slices.Concat(u[:4], []byte{0, 1}, u[4:len(u)-4], w, u[len(u)-4:]))
	}
	outs := []settleOutput{{"claimant", dest, 99614}}
	good := final(unsigned, nil, sig, sig, ws)
	if c := checkFinalTx(good, stored, stored.TxID(), outs, 100000, 386); c != "" {
		t.Fatalf("the stored transaction: %s", c)
	}
	other := slices.Clone(unsigned)
	other[len(other)-1] = 1
	sigNone := slices.Clone(sig)
	sigNone[len(sigNone)-1] = 2
	sigBadDER := slices.Clone(sig)
	sigBadDER[4] |= 0x80 // r negative: the right length and the 0x01 at the end, but not strict DER
	if !bytes.HasSuffix(sigBadDER, []byte{1}) || strictDER(sigBadDER[:len(sigBadDER)-1]) {
		t.Fatal("the test's signature is not a non-strict DER signature ending in 0x01")
	}
	for _, c := range []struct {
		name, raw, want string
		outs            []settleOutput
		total, fee      int64
	}{
		{"unreadable", "zz", finalUnreadable, outs, 100000, 386},
		{"no witness", hex.EncodeToString(unsigned), finalUnreadable, outs, 100000, 386},
		{"another lock time", final(other, nil, sig, sig, ws), finalOtherTx, outs, 100000, 386},
		{"another output value", good, finalOutputs, []settleOutput{{"claimant", dest, 99615}}, 100000, 386},
		{"another output address", good, finalOutputs, []settleOutput{{"claimant", regtestAddr("other"), 99614}}, 100000, 386},
		{"an output address that is not a regtest one", good, finalOutputs, []settleOutput{{"claimant", "tb1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3q0sl5k7", 99614}}, 100000, 386},
		{"another fee", good, finalFee, outs, 100001, 386},
		{"a fee above the rate cap", good, finalFee, outs, 99614 + 30000, 30000},
		{"a witness of three items", final(unsigned, nil, sig, ws), finalWitness, outs, 100000, 386},
		{"another witness script", final(unsigned, nil, sig, sig, ws[:104]), finalWitness, outs, 100000, 386},
		{"SIGHASH_NONE", final(unsigned, nil, sig, sigNone, ws), finalWitness, outs, 100000, 386},
		{"a witness of five items", final(unsigned, nil, sig, sig, ws, []byte{1}), finalWitness, outs, 100000, 386},
		{"a witness with a non-empty dummy", final(unsigned, []byte{1}, sig, sig, ws), finalWitness, outs, 100000, 386},
		{"a signature ending in 0x01 that is not strict DER", final(unsigned, nil, sig, sigBadDER, ws), finalWitness, outs, 100000, 386},
	} {
		if got := checkFinalTx(c.raw, stored, stored.TxID(), c.outs, c.total, c.fee); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// The absolute cap binds where the rate cap does not: 12 inputs make about 1,270 vB, so a fee of 110,000 sat is
	// under 100 sat/vB but above 0.001 BTC; 90,000 sat passes both.
	big := []byte{2, 0, 0, 0, 12}
	maps := [][]byte{nil}
	var witnesses [][]byte
	for i := range 12 {
		big = append(append(big, bytes32(byte(i))...), 0, 0, 0, 0, 0, 0xfd, 0xff, 0xff, 0xff)
		maps = append(maps, psbtMap(in, kv{[]byte{0x05}, ws}))
	}
	big = append(append(append(big, 1, 0x1e, 0x85, 1, 0, 0, 0, 0, 0), byte(len(destScript))), destScript...)
	big = append(big, 0, 0, 0, 0)
	maps[0] = psbtMap(kv{[]byte{0}, big})
	bigStored, err := parsePSBT(buildPSBT(append(maps, psbtMap())...))
	if err != nil {
		t.Fatal(err)
	}
	for range 12 {
		w := []byte{4, 0}
		for _, it := range [][]byte{sig, sig, ws} {
			w = append(append(w, compactSize(len(it))...), it...)
		}
		witnesses = append(witnesses, w)
	}
	bigRaw := hex.EncodeToString(slices.Concat(big[:4], []byte{0, 1}, big[4:len(big)-4], slices.Concat(witnesses...), big[len(big)-4:]))
	if c := checkFinalTx(bigRaw, bigStored, bigStored.TxID(), outs, 99614+110000, 110000); c != finalFee {
		t.Fatalf("a fee above the absolute cap under the rate cap: %q", c)
	}
	if c := checkFinalTx(bigRaw, bigStored, bigStored.TxID(), outs, 99614+90000, 90000); c != "" {
		t.Fatalf("a fee under both caps: %q", c)
	}
}

// bytes32 is 32 bytes of b, a made-up outpoint txid.
func bytes32(b byte) []byte { return slices.Repeat([]byte{b}, 32) }

// TestRegtestAddressScript decodes BIP 173 and BIP 350 vectors (with their own prefixes) and regtest addresses, and
// refuses bad checksums, mixed versions and other networks.
func TestRegtestAddressScript(t *testing.T) {
	for _, c := range []struct{ hrp, addr, script string }{
		{"tb", "tb1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3q0sl5k7", "00201863143c14c5166804bd19203356da136c985678cd4d27a1b8c6329604903262"},
		{"tb", "tb1pqqqqp399et2xygdj5xreqhjjvcmzhxw4aywxecjdzew6hylgvsesf3hn0c", "5120000000c4a5cad46221b2a187905e5266362b99d5e91c6ce24d165dab93e86433"},
		{"bc", "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", "0014751e76e8199196d454941c45d1b3a323f1433bd6"},
	} {
		v, prog, err := decodeSegwitAddress(c.hrp, c.addr)
		if err != nil {
			t.Fatalf("%s: %v", c.addr, err)
		}
		op := byte(0)
		if v > 0 {
			op = 0x50 + v
		}
		if got := hex.EncodeToString(append([]byte{op, byte(len(prog))}, prog...)); got != c.script {
			t.Errorf("%s: %s", c.addr, got)
		}
	}
	for _, bad := range []string{
		"tb1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3q0sl5k8", // checksum
		"tb1pw508d6qejxtdg4y5r3zarqfsj6c3",                               // bech32 checksum on version 1
		"tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kemeawh",                     // bech32m checksum on version 0
		"tb1q0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vq24jc47", // padding
	} {
		if _, _, err := decodeSegwitAddress("tb", bad); err == nil {
			t.Errorf("%s decoded", bad)
		}
	}
	addr := regtestAddr("x")
	if s, err := regtestAddressScript(addr); err != nil || len(s) != 22 || s[0] != 0 {
		t.Fatalf("%s: %x %v", addr, s, err)
	}
	for _, other := range []string{"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx", "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"} {
		if _, err := regtestAddressScript(other); err == nil {
			t.Errorf("%s accepted as a regtest address", other)
		}
	}
	if s, err := regtestAddressScript(base58CheckEncode(append([]byte{0xc4}, make([]byte, 20)...))); err != nil || s[0] != 0xa9 {
		t.Fatalf("P2SH: %x %v", s, err)
	}
	if s, err := regtestAddressScript(base58CheckEncode(append([]byte{0x6f}, make([]byte, 20)...))); err != nil || s[0] != 0x76 {
		t.Fatalf("P2PKH: %x %v", s, err)
	}
}
