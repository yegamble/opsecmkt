package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Escrow Phase 1, slice P1-c1: settlement building and party signing, driven through the HTTP forms and the escrow
// watcher against the fake escrow Core (simulated RPC, not chain evidence). The market never signs in this slice.

// txidOf is a 64-hex transaction ID for label, as Core prints one.
func txidOf(label string) string {
	h := sha256.Sum256([]byte("tx:" + label))
	return hex.EncodeToString(h[:])
}

// fund pays order's escrow with one output per amount, all at confs confirmations, and runs a watcher pass.
func (x *escrowEnv) fund(order, label string, confs int64, sats ...int64) string {
	x.t.Helper()
	var addr string
	if err := x.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
		x.t.Fatal(err)
	}
	for i, s := range sats {
		x.core.deposit(addr, txidOf(label), int64(i), s, confs)
	}
	x.poll()
	return addr
}

// completed is a new escrow order, funded with sats and completed on funding.
func (x *escrowEnv) completed(refund string, sats ...int64) string {
	x.t.Helper()
	order := x.draft()
	x.mustCheckout(order, refund)
	x.fund(order, order, 3, sats...)
	if x.state(order) != stateCompleted {
		x.t.Fatalf("order %s after funding", x.state(order))
	}
	return order
}

func (x *escrowEnv) claim(order, sess, addr string) (int, string) {
	w := x.do("POST", "/escrow/claim", sess, url.Values{"order_id": {order}, "address": {addr}, "password": {testPassword}})
	return w.Code, w.Body.String()
}

// settlement is order's release or refund row: id, kind, state, build_error.
func (x *escrowEnv) settlement(order string) (id int64, kind, state, buildErr string) {
	x.t.Helper()
	if err := x.DB.QueryRow("SELECT id,kind,state,build_error FROM settlements WHERE order_id=$1 AND kind IN ('release','refund')", order).Scan(&id, &kind, &state, &buildErr); err != nil {
		x.t.Fatalf("settlement of %s: %v", order, err)
	}
	return
}

// partyCopy downloads the party's copy of the settlement awaiting a signature.
func (x *escrowEnv) partyCopy(order, sess string, id int64) string {
	x.t.Helper()
	w := x.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10), sess, nil)
	x.check(w, 200)
	return strings.TrimSpace(w.Body.String())
}

// walletSign is a party's wallet in the fake: a signature (sighash sh) on every input from the key whose derivation
// the copy carries at fingerprint fp.
func walletSign(t *testing.T, copyB64, fp string, sh byte) string {
	t.Helper()
	p, err := decodePSBT(copyB64)
	if err != nil {
		t.Fatal(err)
	}
	raw := p.Raw
	for i, in := range p.Inputs {
		var pub []byte
		for _, d := range in.Derivations {
			if hex.EncodeToString(d.Fingerprint[:]) == fp {
				pub = d.PubKey
			}
		}
		if pub == nil {
			t.Fatalf("input %d carries no derivation at %s", i, fp)
		}
		raw = addField(t, raw, i, append([]byte{0x02}, pub...), fakeSig(pub, p.TxID(), sh))
	}
	return encodePSBT(raw)
}

// addField adds one field to input i of a raw PSBT.
func addField(t *testing.T, raw []byte, i int, key, value []byte) []byte {
	t.Helper()
	p, err := parsePSBT(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.withInputField(i, key, value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (x *escrowEnv) sign(order, sess string, id int64, psbt string) (int, string) {
	w := x.do("POST", "/escrow/sign", sess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {psbt}})
	return w.Code, w.Body.String()
}

func (x *escrowEnv) keyHex(order, party string) string {
	x.t.Helper()
	var pub string
	if err := x.DB.QueryRow("SELECT p.pubkey FROM escrow_pubkeys p JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=$1 AND p.party=$2", order, party).Scan(&pub); err != nil {
		x.t.Fatal(err)
	}
	return pub
}

const (
	claimAddr  = "bcrt1qvendorclaim000000000000000000000001"
	refundAddr = "bcrt1qbuyerrefund00000000000000000000001"
)

// The vendor's claim end to end (ES-1, ES-2, ES-4, ES-5): requested with a fresh address and the vendor's password,
// built by the watcher from the escrow's confirmed output checked with gettxout, stored unsigned with its txid and
// frozen outputs, each party served a copy with only its own origin, signed by the vendor and pasted back: the
// settlement is party_signed, only the vendor's signature is kept, and nothing is sent.
func TestEscrowClaimBuildAndSign(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 100000)
	body := x.page("/order?id="+order, x.vendorSess)
	if !strings.Contains(body, `action="/escrow/claim"`) || !strings.Contains(body, `name="password"`) {
		t.Fatalf("completed order offers the vendor no claim form:\n%s", body)
	}
	if strings.Contains(x.page("/order?id="+order, x.buyerSess), `action="/escrow/claim"`) {
		t.Fatal("the buyer is offered the claim form")
	}
	if code, b := x.claim(order, x.vendorSess, claimAddr); code != 303 {
		t.Fatalf("claim: %d %s", code, b)
	}
	id, kind, state, _ := x.settlement(order)
	if kind != "release" || state != "building" {
		t.Fatalf("claim %s %s", kind, state)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE id=$1 AND destination=$2 AND requested_by=$3 AND psbt IS NULL", id, claimAddr, x.vendor); n != 1 {
		t.Fatal("claim row: destination, requester or an early PSBT")
	}
	calls := len(x.core.methods())
	x.poll()
	if got := slices.DeleteFunc(x.core.methods()[calls:], func(m string) bool { return m != "gettxout" && m != "createpsbt" && m != "utxoupdatepsbt" }); !slices.Equal(got, []string{"gettxout", "createpsbt", "utxoupdatepsbt"}) {
		t.Fatalf("build RPCs %v", got)
	}
	_, params := x.core.lastCall(t, "createpsbt")
	if want := `[[{"txid":"` + txidOf(order) + `","vout":0}],[{"` + claimAddr + `":0.00099682}],0,true]`; params != want {
		t.Fatalf("createpsbt %s\nwant %s", params, want)
	}
	_, params = x.core.lastCall(t, "gettxout")
	if params != `["`+txidOf(order)+`",0,true]` {
		t.Fatalf("gettxout %s", params)
	}
	_, params = x.core.lastCall(t, "utxoupdatepsbt")
	bare := withChecksum("wsh(sortedmulti(2," + x.keyHex(order, "buyer") + "," + x.keyHex(order, "vendor") + "," + x.keyHex(order, "market") + "))")
	if !strings.HasSuffix(params, `,["`+bare+`"]]`) || strings.Contains(params, "pub") {
		t.Fatalf("utxoupdatepsbt descriptors %s", params)
	}
	var stored, txid string
	var fee, total, rate int64
	if err := x.DB.QueryRow("SELECT state,psbt,txid,network_fee,input_total,fee_rate FROM settlements WHERE id=$1", id).Scan(&state, &stored, &txid, &fee, &total, &rate); err != nil {
		t.Fatal(err)
	}
	sp, err := decodePSBT(stored)
	if state != "awaiting_signature" || err != nil || sp.TxID() != txid || fee != 318 || total != 100000 || rate != 2 {
		t.Fatalf("built: %s txid %s fee %d total %d rate %d err %v", state, txid, fee, total, rate, err)
	}
	for _, in := range sp.Inputs {
		if len(in.Derivations) != 0 || len(in.PartialSigs) != 0 || in.WitnessUTXO == nil || len(in.WitnessScript) != 105 {
			t.Fatal("the stored copy is not the bare, unsigned, filled PSBT")
		}
	}
	if rows := agStrings(t, x.testEnv, "SELECT n||' '||role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id); !slices.Equal(rows, []string{"0 claimant " + claimAddr + " 99682"}) {
		t.Fatalf("outputs %q", rows)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id=$2", order, id) != 1 {
		t.Fatal("the spent output is not claimed by the settlement")
	}
	for _, uid := range []string{x.buyer, x.vendor} {
		if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%is ready to sign%' AND body LIKE '%Nothing has been sent%'", uid) != 1 {
			t.Fatal("a party was not told the settlement awaits a signature")
		}
	}
	// A second pass builds nothing again.
	calls = x.core.count("createpsbt")
	x.poll()
	if x.core.count("createpsbt") != calls {
		t.Fatal("a built settlement was built again")
	}

	// Each party's page shows the expected outputs and its own copy; each copy carries only that party's origin.
	vk, _ := parseEscrowKey(x.vendorKey)
	bk, _ := parseEscrowKey(x.buyerKey)
	for _, c := range []struct {
		sess, fp, pub string
	}{{x.buyerSess, bk.Fingerprint, x.keyHex(order, "buyer")}, {x.vendorSess, vk.Fingerprint, x.keyHex(order, "vendor")}} {
		page := x.page("/order?id="+order, c.sess)
		for _, want := range []string{"Expected outputs", claimAddr, "0.00099682 BTC", "0.00000318 BTC", `action="/escrow/sign"`, "/escrow-psbt?id=" + order, "Nothing has been sent"} {
			if !strings.Contains(page, want) {
				t.Fatalf("order page lacks %q", want)
			}
		}
		cp := x.partyCopy(order, c.sess, id)
		if !strings.Contains(html.UnescapeString(page), cp) {
			t.Fatal("the page's copy differs from the download")
		}
		p, err := decodePSBT(cp)
		if err != nil || p.TxID() != txid {
			t.Fatalf("copy: %v", err)
		}
		for _, in := range p.Inputs {
			if len(in.Derivations) != 1 || hex.EncodeToString(in.Derivations[0].PubKey) != c.pub || hex.EncodeToString(in.Derivations[0].Fingerprint[:]) != c.fp {
				t.Fatalf("copy derivations %+v", in.Derivations)
			}
		}
	}
	vcopy := x.partyCopy(order, x.vendorSess, id)
	p, _ := decodePSBT(vcopy)
	want := slices.Concat(vk.Path, []uint32{0, 1})
	if !slices.Equal(p.Inputs[0].Derivations[0].Path, want) {
		t.Fatalf("vendor origin path %v, want %v", p.Inputs[0].Derivations[0].Path, want)
	}

	// The vendor signs. A proprietary field carries a marker that must not be stored anywhere.
	signed := walletSign(t, vcopy, vk.Fingerprint, 0x01)
	sp2, _ := decodePSBT(signed)
	marker := "PASTED-BLOB-MARKER-" + randomToken()[:8]
	pasted := encodePSBT(addField(t, sp2.Raw, 0, append([]byte{0xfc}, []byte("opsec")...), []byte(marker)))
	notes := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1", x.buyer)
	if code, b := x.sign(order, x.vendorSess, id, pasted); code != 303 {
		t.Fatalf("sign: %d %s", code, b)
	}
	var signer string
	if err := x.DB.QueryRow("SELECT state,signer FROM settlements WHERE id=$1", id).Scan(&state, &signer); err != nil || state != "party_signed" || signer != "vendor" {
		t.Fatalf("after signing %s %s %v", state, signer, err)
	}
	sig := hex.EncodeToString(fakeSig(p.Inputs[0].Derivations[0].PubKey, txid, 1))
	if rows := agStrings(t, x.testEnv, "SELECT input_n||' '||party||' '||pubkey||' '||signature FROM settlement_signatures WHERE settlement_id=$1", id); !slices.Equal(rows, []string{"0 vendor " + x.keyHex(order, "vendor") + " " + sig}) {
		t.Fatalf("signatures %q", rows)
	}
	// The paste is kept nowhere: not whole, not its tail (the signed input map and the marker), not the marker.
	for _, needle := range []string{marker, hex.EncodeToString([]byte(marker)), pasted, strings.TrimRight(pasted, "=")[len(strings.TrimRight(pasted, "="))-80:]} {
		assertNowhereInDB(t, x.testEnv, needle)
	}
	var after string
	x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&after)
	if after != stored {
		t.Fatal("the stored copy changed after signing")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1", x.buyer) != notes+1 ||
		agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%the vendor signed its escrow settlement%Nothing has been sent%'", x.buyer) != 1 {
		t.Fatal("the buyer was not told the vendor signed")
	}
	// P1-c2 (A-340): the buyer, who has not signed, may sign too; the vendor, who has, is not offered it again.
	page := x.page("/order?id="+order, x.buyerSess)
	if !strings.Contains(page, "Signed by the vendor") || !strings.Contains(page, "Nothing has been sent") || !strings.Contains(page, `action="/escrow/sign"`) {
		t.Fatalf("signed page:\n%s", page)
	}
	if page := x.page("/order?id="+order, x.vendorSess); strings.Contains(page, `action="/escrow/sign"`) || strings.Contains(page, "/escrow-psbt?") {
		t.Fatalf("the signer's page offers signing again:\n%s", page)
	}
	if w := x.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10), x.vendorSess, nil); w.Code != 404 {
		t.Fatalf("copy served to the party who signed: %d", w.Code)
	}
	// Signing again changes nothing: the vendor is refused, and the vendor's paste is not the buyer's signature.
	if code, _ := x.sign(order, x.vendorSess, id, pasted); code != 409 {
		t.Fatalf("second signature: %d", code)
	}
	if code, _ := x.sign(order, x.buyerSess, id, pasted); code != 400 {
		t.Fatalf("the vendor's paste by the buyer: %d", code)
	}
	// The database refuses a rebuild of a signed settlement, a second outcome and a change of its outputs.
	if _, err := x.DB.Exec("UPDATE settlements SET psbt='cHNidP8x' WHERE id=$1", id); err == nil || !strings.Contains(err.Error(), "never rebuilt") {
		t.Fatalf("rebuild of a signed settlement: %v", err)
	}
	if _, err := x.DB.Exec("INSERT INTO settlements(order_id,kind,destination) VALUES($1,'refund',$2)", order, refundAddr); err == nil || !strings.Contains(err.Error(), "settlements_one_outcome") {
		t.Fatalf("second outcome: %v", err)
	}
	if _, err := x.DB.Exec("UPDATE settlement_outputs SET sats=sats+1 WHERE settlement_id=$1", id); err == nil {
		t.Fatal("an output of a built settlement changed")
	}
	if code, _ := x.claim(order, x.vendorSess, "bcrt1qvendorclaim000000000000000000000002"); code != 409 {
		t.Fatalf("second claim: %d", code)
	}
	x.noPayouts(order)
	if agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE action LIKE 'Signed the escrow settlement of order %' AND action NOT LIKE '%cHNidP8%'") != 1 {
		t.Fatal("audit row")
	}
	for _, m := range x.core.methods() {
		if m == "walletprocesspsbt" || m == "sendrawtransaction" || m == "combinepsbt" || m == "finalizepsbt" || m == "testmempoolaccept" {
			t.Fatalf("the market called %s in this slice", m)
		}
	}
}

// Outputs of a claim (ES-5): the market fee snapshotted at checkout, an overpayment returned to the buyer's refund
// address, and outputs below the floor folded into the vendor's with an order note. The network fee comes from the
// vendor's output alone.
func TestEscrowClaimOutputsFeeReturnAndFold(t *testing.T) {
	x := newEscrowEnv(t)
	t.Setenv("ESCROW_BTC_FEE_BPS", "1500")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", "bcrt1qmarketfee0000000000000000000000000")
	x.restart()
	// 0.0012 BTC in two outputs for a 0.001 BTC price: market fee 15000, return 20000; fee 2*(11+2*105+3*43)=700.
	order := x.completed(refundAddr, 70000, 50000)
	if code, b := x.claim(order, x.vendorSess, claimAddr); code != 303 {
		t.Fatalf("claim: %d %s", code, b)
	}
	x.poll()
	id, _, state, _ := x.settlement(order)
	rows := agStrings(t, x.testEnv, "SELECT n||' '||role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
	want := []string{"0 claimant " + claimAddr + " 84300", "1 market_fee bcrt1qmarketfee0000000000000000000000000 15000", "2 buyer_return " + refundAddr + " 20000"}
	if state != "awaiting_signature" || !slices.Equal(rows, want) {
		t.Fatalf("%s outputs\n%s\nwant\n%s", state, strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id=$2", order, id) != 2 {
		t.Fatal("both outputs claimed")
	}

	// Fee 500 bps (5000) and an overpayment of 5000 are both below the 10000 floor: folded into the vendor's output.
	t.Setenv("ESCROW_BTC_FEE_BPS", "50")
	x.restart()
	order2 := x.completed("bcrt1qbuyerrefund00000000000000000000002", 105000)
	if code, b := x.claim(order2, x.vendorSess, "bcrt1qvendorclaim000000000000000000000002"); code != 303 {
		t.Fatalf("claim: %d %s", code, b)
	}
	x.poll()
	id2, _, _, _ := x.settlement(order2)
	if rows := agStrings(t, x.testEnv, "SELECT n||' '||role||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id2); !slices.Equal(rows, []string{"0 claimant 104682"}) {
		t.Fatalf("folded outputs %q", rows)
	}
	ev := strings.Join(x.events(order2), "\n")
	for _, want := range []string{"market fee of 0.000005 BTC is below the minimum output of 0.0001 BTC, so it was added to the vendor's output", "overpayment of 0.00005 BTC is below the minimum output of 0.0001 BTC, so it was added to the vendor's output"} {
		if !strings.Contains(ev, want) {
			t.Fatalf("fold note %q missing:\n%s", want, ev)
		}
	}
}

// A-336: an output recorded before the order completed that confirms only afterwards joins the claim. The build waits
// until every counted output has the threshold, then spends them all; the amount above the price returns to the
// buyer's refund address. It is never flagged as a late deposit.
func TestEscrowClaimWaitsForLateConfirmingOutput(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.draft()
	addr := x.mustCheckout(order, refundAddr)
	x.core.deposit(addr, txidOf("full"), 0, 100000, 3)
	x.core.deposit(addr, txidOf("extra"), 0, 30000, 1)
	x.poll()
	if x.state(order) != stateCompleted || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND NOT after_close", order) != 2 {
		t.Fatalf("state %s", x.state(order))
	}
	x.claim(order, x.vendorSess, claimAddr)
	x.poll()
	if _, _, state, class := x.settlement(order); state != "building" || class != "waiting-confirmations" {
		t.Fatalf("with an output still confirming: %s %s", state, class)
	}
	if page := x.page("/order?id="+order, x.vendorSess); !strings.Contains(page, "Waiting until every deposit to this escrow has 3 confirmations") {
		t.Fatal("the page does not say what the build waits for")
	}
	x.core.setConfs(txidOf("extra"), 3)
	x.poll()
	id, _, state, _ := x.settlement(order)
	rows := agStrings(t, x.testEnv, "SELECT role||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
	if state != "awaiting_signature" || !slices.Equal(rows, []string{"claimant 99386", "buyer_return 30000"}) ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id=$2", order, id) != 2 {
		t.Fatalf("%s %q", state, rows)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", order) != 0 {
		t.Fatal("the late-confirming output was flagged")
	}
}

// The node's PSBT is checked before anything is stored: another transaction from createpsbt or utxoupdatepsbt, an
// input without the escrow's witness script, or a key origin with a path is refused (bad-reply), and the settlement
// stays building with nothing claimed. An output claimed by another settlement while the build ran rolls it back.
func TestEscrowBuildRefusesNodeReplies(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 60000, 40000)
	x.claim(order, x.vendorSess, claimAddr)
	id, _, _, _ := x.settlement(order)
	strip := func(key byte) func(string) string {
		return func(b64 string) string {
			p, _ := decodePSBT(b64)
			raw, _ := p.withoutInputFields(func(_ int, k []byte) bool { return k[0] == key })
			return encodePSBT(raw)
		}
	}
	for _, c := range []struct {
		name           string
		create, update func(string) string
	}{
		{"createpsbt answers another transaction", func(string) string { return testCorePSBTUnsigned }, nil},
		{"utxoupdatepsbt answers another transaction", nil, func(string) string { return testCorePSBTUnsigned }},
		{"no witness script", nil, strip(0x05)},
		{"no witness UTXO", nil, strip(0x01)},
		{"a key origin with a path", nil, func(b64 string) string {
			p, _ := decodePSBT(b64)
			raw, _ := p.withoutInputFields(func(_ int, k []byte) bool { return k[0] == 0x06 })
			vpub, _ := hex.DecodeString(x.keyHex(order, "vendor"))
			return encodePSBT(addField(t, raw, 0, append([]byte{0x06}, vpub...), []byte{0x5e, 0xed, 0x5e, 0xed, 0, 0, 0, 0}))
		}},
	} {
		x.core.mu.Lock()
		x.core.createSkew, x.core.updateSkew = c.create, c.update
		x.core.mu.Unlock()
		err := x.A.escrowPollOnce(context.Background())
		if err == nil || !strings.Contains(err.Error(), "bad-reply") {
			t.Fatalf("%s: pass %v", c.name, err)
		}
		if _, _, state, _ := x.settlement(order); state != "building" || agInt(x.testEnv, "SELECT count(*) FROM settlement_outputs")+agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id IS NOT NULL") != 0 {
			t.Fatalf("%s: stored something (%s)", c.name, state)
		}
	}
	x.core.mu.Lock()
	x.core.createSkew, x.core.updateSkew = nil, nil
	x.core.mu.Unlock()
	// While this build asks Core for the PSBT, one of its outputs is claimed by another settlement (as by hand).
	x.core.beforeCreate = func() {
		x.DB.Exec("INSERT INTO settlements(order_id,kind,destination) VALUES($1,'stray_refund',$2)", order, refundAddr)
		x.DB.Exec("UPDATE payments SET settlement_id=(SELECT id FROM settlements WHERE order_id=$1 AND kind='stray_refund') WHERE order_id=$1 AND idx=1", order)
	}
	x.poll()
	x.core.beforeCreate = nil
	if _, _, state, _ := x.settlement(order); state != "building" || agInt(x.testEnv, "SELECT count(*) FROM settlement_outputs WHERE settlement_id=$1", id) != 0 ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", id) != 0 {
		t.Fatalf("a build whose output was claimed meanwhile stored: %s", state)
	}
}

// A build checks every input with gettxout first: unconfirmed, spent or not what the ledger recorded leaves the
// settlement building (tried again on the next pass) with no PSBT asked for; a fee above the cap does too; once the
// node agrees, it is built.
func TestEscrowBuildRefusesBadInputsAndFeeCap(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 100000)
	x.claim(order, x.vendorSess, claimAddr)
	for _, c := range []struct {
		name, class string
		skew        func(map[string]any)
		spend       bool
	}{
		{"unconfirmed", "input-unconfirmed", func(m map[string]any) { m["confirmations"] = 1 }, false},
		{"value", "input-mismatch", func(m map[string]any) { m["value"] = 0.0009 }, false},
		{"address", "input-mismatch", func(m map[string]any) {
			m["scriptPubKey"] = map[string]any{"address": "bcrt1qsomewhereelse0000000000000000000"}
		}, false},
		{"spent", "input-spent", nil, true},
	} {
		x.core.mu.Lock()
		x.core.utxoSkew, x.core.spent = c.skew, nil
		x.core.mu.Unlock()
		if c.spend {
			x.core.spend(txidOf(order), 0)
		}
		creates := x.core.count("createpsbt")
		x.poll()
		_, _, state, class := x.settlement(order)
		if state != "building" || class != c.class || x.core.count("createpsbt") != creates {
			t.Fatalf("%s: %s %s, createpsbt called %d times", c.name, state, class, x.core.count("createpsbt")-creates)
		}
		if page := x.page("/order?id="+order, x.vendorSess); !strings.Contains(page, "not unspent as recorded") {
			t.Fatalf("%s: page does not say why", c.name)
		}
	}
	x.core.mu.Lock()
	x.core.utxoSkew, x.core.spent = nil, nil
	x.core.mu.Unlock()
	x.poll()
	if _, _, state, class := x.settlement(order); state != "awaiting_signature" || class != "" {
		t.Fatalf("after the node agrees: %s %q", state, class)
	}

	// Fee cap: 10 outputs at 100 sat/vB is 100*(11+1050+43) = 110400 sat, above the 100000 cap.
	t.Setenv("ESCROW_BTC_FEE_RATE", "100")
	x.restart()
	amounts := slices.Repeat([]int64{10000}, 10)
	order2 := x.completed("bcrt1qbuyerrefund00000000000000000000002", amounts...)
	x.claim(order2, x.vendorSess, "bcrt1qvendorclaim000000000000000000000002")
	// (The claim of order, waiting for a signature, is read with gettxout on every pass, A-356; order2's outputs are not.)
	creates, gets := x.core.count("createpsbt"), x.core.countWith("gettxout", txidOf(order2))
	x.poll()
	if _, _, state, class := x.settlement(order2); state != "building" || class != "fee-cap" || x.core.count("createpsbt") != creates || x.core.countWith("gettxout", txidOf(order2)) != gets {
		t.Fatalf("fee cap: %s %s", state, class)
	}
	if page := x.page("/order?id="+order2, x.buyerSess); !strings.Contains(page, "exceed the market") {
		t.Fatal("fee cap not shown")
	}
	// A refused rate never reaches a build: startup refuses ESCROW_BTC_FEE_RATE above the cap.
	if _, err := escrowConfigFromEnv(func(k string) string {
		return map[string]string{"ESCROW_BTC": "on", "ESCROW_BTC_RPC_URL": "http://u:p@127.0.0.1:1", "ESCROW_BTC_FEE_RATE": "101"}[k]
	}); err == nil {
		t.Fatal("fee rate above the cap accepted")
	}
}

// A settlement of an escrow whose coins cannot pay the network fee and an output above the floor is never built:
// it fails with a note, and the coins stay in the escrow.
func TestEscrowBuildTooSmallFails(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.draft()
	x.mustCheckout(order, refundAddr)
	x.fund(order, order, 3, 10100)
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, "amount_seen": {"10100"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	if _, _, state, class := x.settlement(order); state != "failed" || class != "too-small" {
		t.Fatalf("%s %s", state, class)
	}
	if !strings.Contains(strings.Join(x.events(order), "\n"), "too few to pay the network fee") || x.core.count("createpsbt") != 1 { // the checkout's
		t.Fatal("too-small note, or a PSBT was asked for")
	}
}

// Two builds of one settlement at once store one result; a paste while it is building is refused; two parties
// signing at once give one party_signed and one refusal.
func TestEscrowSettlementRaces(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 60000, 40000)
	x.claim(order, x.vendorSess, claimAddr)
	id, _, _, _ := x.settlement(order)
	// A paste arrives each time a build asks Core for the PSBT. The first comes before any build has stored (both ask
	// before storing), so it meets a settlement still building: refused with 409, nothing changed.
	var pasteMu sync.Mutex
	var pasteCodes []int
	x.core.beforeCreate = func() {
		code, _ := x.sign(order, x.vendorSess, id, "cHNidP8BAAoCAAAAAAAAAAAAAAAA")
		pasteMu.Lock()
		pasteCodes = append(pasteCodes, code)
		pasteMu.Unlock()
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	e := x.A.escrow.Load()
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = x.A.buildSettlement(context.Background(), e, id)
		}()
	}
	wg.Wait()
	x.core.beforeCreate = nil
	if errs[0] != nil || errs[1] != nil || !slices.Contains(pasteCodes, 409) || slices.ContainsFunc(pasteCodes, func(c int) bool { return c != 409 && c != 400 }) ||
		agInt(x.testEnv, "SELECT count(*) FROM settlement_signatures") != 0 {
		t.Fatalf("builds %v %v; pastes during the builds %v", errs[0], errs[1], pasteCodes)
	}
	if _, _, state, _ := x.settlement(order); state != "awaiting_signature" ||
		agInt(x.testEnv, "SELECT count(*) FROM settlement_outputs WHERE settlement_id=$1", id) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id=$2", order, id) != 2 {
		t.Fatalf("after two builds: %s", state)
	}
	vk, _ := parseEscrowKey(x.vendorKey)
	bk, _ := parseEscrowKey(x.buyerKey)
	vs := walletSign(t, x.partyCopy(order, x.vendorSess, id), vk.Fingerprint, 1)
	bs := walletSign(t, x.partyCopy(order, x.buyerSess, id), bk.Fingerprint, 1)
	codes := make([]int, 2)
	for i, c := range []struct{ sess, psbt string }{{x.vendorSess, vs}, {x.buyerSess, bs}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = x.sign(order, c.sess, id, c.psbt)
		}()
	}
	wg.Wait()
	// P1-c2 (A-340): the second signature is accepted too (the row lock orders them); both are kept, one signer.
	if !slices.Equal(codes, []int{303, 303}) {
		t.Fatalf("concurrent signatures: %v", codes)
	}
	if n := agInt(x.testEnv, "SELECT count(DISTINCT party) FROM settlement_signatures WHERE settlement_id=$1", id); n != 2 ||
		agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE id=$1 AND state='party_signed' AND signer IS NOT NULL", id) != 1 {
		t.Fatalf("%d parties' signatures kept", n)
	}
	// A second claim of the same outputs by another settlement is refused by the ledger's compare-and-set.
	if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id IS NULL AND NOT after_close", order); n != 0 {
		t.Fatalf("%d unclaimed outputs", n)
	}
}

// Every refusal of a pasted PSBT (ES-2) happens before anything is stored: the settlement stays awaiting a
// signature, no signature row and no notice is written.
func TestEscrowPasteRefusals(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 60000, 40000) // two inputs
	x.claim(order, x.vendorSess, claimAddr)
	x.poll()
	id, _, _, _ := x.settlement(order)
	other := x.completed("bcrt1qbuyerrefund00000000000000000000002", 100000)
	x.claim(other, x.vendorSess, "bcrt1qvendorclaim000000000000000000000002")
	x.poll()
	otherID, _, _, _ := x.settlement(other)

	vk, _ := parseEscrowKey(x.vendorKey)
	bk, _ := parseEscrowKey(x.buyerKey)
	vcopy := x.partyCopy(order, x.vendorSess, id)
	cp, _ := decodePSBT(vcopy)
	txid := cp.TxID()
	vpub, _ := hex.DecodeString(x.keyHex(order, "vendor"))
	bpub, _ := hex.DecodeString(x.keyHex(order, "buyer"))
	mpub, _ := hex.DecodeString(x.keyHex(order, "market"))
	good := walletSign(t, vcopy, vk.Fingerprint, 1)
	gp, _ := decodePSBT(good)
	sigOn := func(raw []byte, i int, pub []byte, sh byte) []byte {
		return addField(t, raw, i, append([]byte{0x02}, pub...), fakeSig(pub, txid, sh))
	}
	oneInput := sigOn(cp.Raw, 0, vpub, 1)
	var market byte
	market = 0x06
	cases := []struct {
		name, sess, psbt, want string
	}{
		{"another settlement's transaction", x.vendorSess, walletSign(t, x.partyCopy(other, x.vendorSess, otherID), vk.Fingerprint, 1), "not the settlement transaction shown"},
		{"a signature missing on one input", x.vendorSess, encodePSBT(oneInput), "does not carry your signature on every input"},
		{"no signature at all", x.vendorSess, vcopy, "does not carry your signature on every input"},
		{"the buyer's signature pasted by the vendor", x.vendorSess, walletSign(t, x.partyCopy(order, x.buyerSess, id), bk.Fingerprint, 1), "a key other than your escrow key"},
		{"the vendor's signature pasted by the buyer", x.buyerSess, good, "a key other than your escrow key"},
		{"a second signer's signature added", x.vendorSess, encodePSBT(sigOn(gp.Raw, 1, bpub, 1)), "a key other than your escrow key"},
		{"a market-key signature", x.vendorSess, encodePSBT(sigOn(gp.Raw, 0, mpub, 1)), "under the market's key"},
		{"a market-key derivation (bare, as Core writes one)", x.vendorSess, encodePSBT(addField(t, gp.Raw, 1, append([]byte{market}, mpub...), mpub[1:5])), "under the market's key"},
		{"a derivation from the arbiter fingerprint", x.vendorSess, encodePSBT(addField(t, gp.Raw, 0, append([]byte{market}, bpub...), append([]byte{0xa1, 0xb2, 0xc3, 0xd4}, 1, 0, 0, 0))), "under the market's key"},
		{"SIGHASH_NONE", x.vendorSess, walletSign(t, vcopy, vk.Fingerprint, 0x02), "sighash type other than ALL"},
		{"SIGHASH_ALL|ANYONECANPAY", x.vendorSess, walletSign(t, vcopy, vk.Fingerprint, 0x81), "sighash type other than ALL"},
		{"one input SIGHASH_SINGLE", x.vendorSess, encodePSBT(sigOn(sigOn(cp.Raw, 0, vpub, 1), 1, vpub, 0x03)), "sighash type other than ALL"},
		{"a sighash-type field of NONE", x.vendorSess, encodePSBT(addField(t, gp.Raw, 1, []byte{0x03}, []byte{2, 0, 0, 0})), "sighash type other than ALL"},
		{"finalized", x.vendorSess, encodePSBT(addField(t, gp.Raw, 0, []byte{0x08}, []byte{0})), "already finalized"},
		{"garbage", x.vendorSess, "this is not a psbt at all", "could not be read"},
		{"not a PSBT", x.vendorSess, "aGVsbG8gd29ybGQ=", "could not be read"},
		{"oversize", x.vendorSess, strings.Repeat("A", 70000), "oversized form"},
		{"empty", x.vendorSess, "", "could not be read"},
	}
	notes := agInt(x.testEnv, "SELECT count(*) FROM notifications")
	for _, c := range cases {
		code, body := x.sign(order, c.sess, id, c.psbt)
		if code != 400 || !strings.Contains(body, c.want) || c.name != "oversize" && !strings.Contains(body, "Nothing was changed") {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
		if _, _, state, _ := x.settlement(order); state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM settlement_signatures") != 0 || agInt(x.testEnv, "SELECT count(*) FROM notifications") != notes {
			t.Fatalf("%s: something was stored (%s)", c.name, state)
		}
	}
	// A settlement of another order, or one that does not exist, is not found; a stranger cannot sign.
	if code, _ := x.sign(order, x.vendorSess, otherID, good); code != 404 {
		t.Fatalf("another order's settlement: %d", code)
	}
	_, stranger := x.user("stranger_"+randomToken()[:6], "buyer")
	if code, _ := x.sign(order, stranger, id, good); code != 404 {
		t.Fatalf("stranger: %d", code)
	}
	if w := x.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10), stranger, nil); w.Code != 404 {
		t.Fatalf("stranger download: %d", w.Code)
	}
	if code, body := x.sign(order, x.vendorSess, id, good); code != 303 {
		t.Fatalf("the good paste after the refusals: %d %s", code, body)
	}
}

// Every path that answered 409 "settlement later" before P1-c1 now requests a refund of the escrow (the A-246
// carry-over): the buyer's or the vendor's cancel of an order awaiting payment with a deposit, the vendor's cancel of
// a paid one, and an underpaid order at the end of its payment window. Each refund pays the pinned refund address,
// less the network fee, and the buyer signs it. A dispute is still refused (P1-c2).
func TestEscrowRefundRoutes(t *testing.T) {
	x := newEscrowEnv(t)
	type route struct {
		name   string
		refund string
		setup  func(order string)
		cancel func(order string) int
		sats   int64
	}
	cancelAs := func(sess, from string, seen int64) func(string) int {
		return func(order string) int {
			w := x.do("POST", "/orders/cancel", sess, url.Values{"order_id": {order}, "from": {from}, "amount_seen": {strconv.FormatInt(seen, 10)}, "payout_confirmed": {"confirmed"}})
			return w.Code
		}
	}
	routes := []route{
		{"buyer cancels after funding", "bcrt1qbuyerrefund00000000000000000000011", func(o string) { x.fund(o, o, 3, 40000) }, cancelAs(x.buyerSess, stateAwaitingPayment, 40000), 40000},
		{"vendor cancels after funding", "bcrt1qbuyerrefund00000000000000000000012", func(o string) { x.fund(o, o, 3, 40000) }, cancelAs(x.vendorSess, stateAwaitingPayment, 40000), 40000},
		{"vendor cancels a paid order", "bcrt1qbuyerrefund00000000000000000000013", func(o string) {
			x.DB.Exec("UPDATE products SET delivery_content='' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", o)
			x.fund(o, o, 3, 100000)
		}, cancelAs(x.vendorSess, statePaid, 100000), 100000},
		{"expired underpaid", "bcrt1qbuyerrefund00000000000000000000014", func(o string) {
			x.fund(o, o, 5, 40000)
			x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", o)
			x.DB.Exec("UPDATE payments SET created=now()-interval '25 hours' WHERE order_id=$1", o)
		}, func(o string) int { x.poll(); return 303 }, 40000},
	}
	bk, _ := parseEscrowKey(x.buyerKey)
	for _, r := range routes {
		order := x.draft()
		x.mustCheckout(order, r.refund)
		r.setup(order)
		if code := r.cancel(order); code != 303 || x.state(order) != stateCancelled {
			t.Fatalf("%s: cancel %d, state %s", r.name, code, x.state(order))
		}
		id, kind, state, _ := x.settlement(order)
		if kind != "refund" || agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE id=$1 AND destination=$2", id, r.refund) != 1 {
			t.Fatalf("%s: %s %s", r.name, kind, state)
		}
		x.poll()
		_, _, state, class := x.settlement(order)
		fee := int64(2 * (11 + 105 + 43))
		rows := agStrings(t, x.testEnv, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1", id)
		if state != "awaiting_signature" || !slices.Equal(rows, []string{"claimant " + r.refund + " " + strconv.FormatInt(r.sats-fee, 10)}) {
			t.Fatalf("%s: %s %q %q", r.name, state, class, rows)
		}
		if code, body := x.sign(order, x.buyerSess, id, walletSign(t, x.partyCopy(order, x.buyerSess, id), bk.Fingerprint, 1)); code != 303 {
			t.Fatalf("%s: buyer signs: %d %s", r.name, code, body)
		}
		if _, _, state, _ := x.settlement(order); state != "party_signed" {
			t.Fatalf("%s: %s", r.name, state)
		}
		x.noPayouts(order)
		if r.name == "expired underpaid" {
			if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%so the order was cancelled%builds a refund%'", x.buyer) != 1 {
				t.Fatal("underpaid expiry notice")
			}
		}
	}
	// A cancel form whose stated amount is stale is refused and changes nothing.
	order := x.draft()
	x.mustCheckout(order, "bcrt1qbuyerrefund00000000000000000000015")
	x.fund(order, order, 3, 40000)
	if code := cancelAs(x.buyerSess, stateAwaitingPayment, 30000)(order); code != 409 || x.state(order) != stateAwaitingPayment {
		t.Fatalf("stale amount: %d %s", code, x.state(order))
	}
	body := x.page("/order?id="+order, x.buyerSess)
	if !strings.Contains(body, `name="amount_seen" value="40000"`) || !strings.Contains(body, "Cancelling refunds the 0.0004 regtest BTC in this escrow") {
		t.Fatalf("cancel form of a funded escrow order:\n%s", body)
	}
	// P1-c2: a dispute opens (an administrator decides it, escrow_dispute_test.go); nothing is settled by opening it.
	paid := x.draft()
	x.mustCheckout(paid, "bcrt1qbuyerrefund00000000000000000000016")
	x.DB.Exec("UPDATE products SET delivery_content='' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", paid)
	x.fund(paid, paid, 3, 100000)
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {paid}, "reason": {"The content was never delivered to me."}}); w.Code != 303 || x.state(paid) != stateDisputed ||
		agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", paid) != 0 {
		t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
}

// The claim form's refusals change nothing: only the vendor, only on a completed escrow order, only a fresh valid
// address, and only with the vendor's password.
func TestEscrowClaimRefusals(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 100000)
	open := x.draft()
	x.mustCheckout(open, "bcrt1qbuyerrefund00000000000000000000002")
	cases := []struct {
		name, order, sess, addr, password string
		code                              int
	}{
		{"the buyer", order, x.buyerSess, claimAddr, testPassword, 403},
		{"an open order", open, x.vendorSess, claimAddr, testPassword, 409},
		{"the escrow's refund address", order, x.vendorSess, refundAddr, testPassword, 409},
		{"the escrow address", order, x.vendorSess, "", testPassword, 409},
		{"an address of another network", order, x.vendorSess, "tb1qnotregtest0000000000000000000000000", testPassword, 400},
		{"no address", order, x.vendorSess, " ", testPassword, 400},
		{"a wrong password", order, x.vendorSess, claimAddr, "wrong password 123", 401},
	}
	var escrowAddr string
	x.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&escrowAddr)
	for _, c := range cases {
		addr := c.addr
		if addr == "" {
			addr = escrowAddr
		}
		w := x.do("POST", "/escrow/claim", c.sess, url.Values{"order_id": {c.order}, "address": {addr}, "password": {c.password}})
		if w.Code != c.code {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlements"); n != 0 {
		t.Fatalf("%d settlements after refusals", n)
	}
	// A claim address is single-use across the market: another escrow's checkout refuses it as a refund address.
	if code, _ := x.claim(order, x.vendorSess, claimAddr); code != 303 {
		t.Fatal("claim")
	}
	if w := x.checkout(x.draft(), claimAddr); w.Code != 409 {
		t.Fatalf("a claim address reused as a refund address: %d", w.Code)
	}
	order2 := x.completed("bcrt1qbuyerrefund00000000000000000000003", 100000)
	if code, _ := x.claim(order2, x.vendorSess, claimAddr); code != 409 {
		t.Fatalf("a claim address reused: %d", code)
	}
	// With ESCROW_BTC off the routes do not exist.
	t.Setenv("ESCROW_BTC", "off")
	x.restart()
	if code, _ := x.claim(order2, x.vendorSess, "bcrt1qvendorclaim000000000000000000000009"); code != 404 {
		t.Fatalf("claim with escrow off: %d", code)
	}
	if code, _ := x.sign(order, x.vendorSess, 1, "cHNidP8="); code != 404 {
		t.Fatalf("sign with escrow off: %d", code)
	}
}

// Staff reviewing an escrow order (a flagged payment) see its settlement's state and outputs read-only: no copy, no
// paste form, no download.
func TestEscrowSettlementStaffReadOnly(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 100000)
	x.claim(order, x.vendorSess, claimAddr)
	x.poll()
	x.DB.Exec("UPDATE payments SET flagged=true WHERE order_id=$1", order)
	_, mod := x.user("emod_"+randomToken()[:6], "moderator")
	body := x.page("/order?id="+order, mod)
	if !strings.Contains(body, "Settlement: The vendor&#39;s claim") || !strings.Contains(body, "Waiting for a signature") || !strings.Contains(body, claimAddr) {
		t.Fatalf("staff view lacks the settlement:\n%s", body)
	}
	for _, bad := range []string{`action="/escrow/sign"`, "/escrow-psbt?", "cHNidP8", `action="/escrow/claim"`} {
		if strings.Contains(body, bad) {
			t.Fatalf("staff view offers %q", bad)
		}
	}
}

// A-338: a cancelled escrow order whose address is no longer watched, or while escrow is unavailable, does not show
// "0 BTC" for its confirmed and unconfirmed figures.
func TestEscrowUnwatchedFiguresNotZero(t *testing.T) {
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, refundAddr)
	x.DB.Exec("UPDATE escrows SET created=now()-interval '31 days' WHERE order_id=$1", x.order)
	x.poll()
	x.DB.Exec("UPDATE orders SET updated=now()-interval '2 days' WHERE id=$1", x.order)
	if x.state(x.order) != stateCancelled {
		t.Fatalf("state %s", x.state(x.order))
	}
	body := x.page("/order?id="+x.order, x.buyerSess)
	if !strings.Contains(body, "no longer watched") || strings.Count(body, "Not watched any more") != 2 || strings.Contains(body, "<dd>0 BTC</dd>") {
		t.Fatalf("unwatched cancelled order figures:\n%s", body)
	}
	// A watched cancelled order with nothing received still says 0.
	order2 := x.draft()
	x.mustCheckout(order2, "bcrt1qbuyerrefund00000000000000000000002")
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", order2)
	x.poll()
	if body := x.page("/order?id="+order2, x.buyerSess); strings.Count(body, "<dd>0 BTC</dd>") != 2 || strings.Contains(body, "Not watched") {
		t.Fatal("a watched cancelled order's figures")
	}
}
