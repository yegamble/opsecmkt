package market

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Pins for escrow slice P1-c1 (A-341): tests that kill the mutants QA34b found surviving in escrow_settle.go,
// escrow_order.go and escrow_watch.go. Each test names the mutants it kills. Fake-provider tests (simulated RPC,
// not chain evidence).

// pinsReshape rebuilds the PSBT b64 with its unsigned transaction and its input and output maps changed by mut (the
// maps stay in step with the transaction, so the result still parses). The global map keeps only the transaction.
func pinsReshape(t *testing.T, b64 string, mut func(tx *psbtTx, ins, outs *[][]byte)) string {
	t.Helper()
	p, err := decodePSBT(b64)
	if err != nil {
		t.Fatal(err)
	}
	r := &psbtReader{b: p.Raw, off: 5}
	if _, _, err = r.readMap(); err != nil {
		t.Fatal(err)
	}
	maps := func(n int) [][]byte {
		var out [][]byte
		for range n {
			start := r.off
			if _, _, err := r.readMap(); err != nil {
				t.Fatal(err)
			}
			out = append(out, slices.Clone(p.Raw[start:r.off]))
		}
		return out
	}
	ins, outs := maps(len(p.Tx.In)), maps(len(p.Tx.Out))
	tx := p.Tx
	tx.In, tx.Out = slices.Clone(tx.In), slices.Clone(tx.Out)
	mut(&tx, &ins, &outs)
	all := append([][]byte{psbtMap(kv{[]byte{0}, pinsSerializeTx(tx)})}, ins...)
	return encodePSBT(buildPSBT(append(all, outs...)...))
}

// pinsSerializeTx is the transaction without witnesses, as a PSBT's unsigned transaction field holds it.
func pinsSerializeTx(tx psbtTx) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(tx.Version))
	b = append(b, compactSize(len(tx.In))...)
	for _, in := range tx.In {
		b = append(b, in.TxID[:]...)
		b = binary.LittleEndian.AppendUint32(b, in.Vout)
		b = append(b, 0)
		b = binary.LittleEndian.AppendUint32(b, in.Sequence)
	}
	b = append(b, compactSize(len(tx.Out))...)
	for _, o := range tx.Out {
		b = binary.LittleEndian.AppendUint64(b, uint64(o.Value))
		b = append(append(b, compactSize(len(o.Script))...), o.Script...)
	}
	return binary.LittleEndian.AppendUint32(b, tx.LockTime)
}

// pinsSkew sets what the fake node lies about (nil clears it).
func (x *escrowEnv) pinsSkew(create, update func(string) string, utxo func(map[string]any)) {
	x.core.mu.Lock()
	defer x.core.mu.Unlock()
	x.core.createSkew, x.core.updateSkew, x.core.utxoSkew = create, update, utxo
}

// pinsBuild runs one build of settlement id, as the watcher does, and returns its error.
func (x *escrowEnv) pinsBuild(id int64) error {
	return x.A.buildSettlement(context.Background(), x.A.escrow.Load(), id)
}

// pinsNothingStored asserts that settlement id is still building with no PSBT, no outputs and no claimed payment.
func (x *escrowEnv) pinsNothingStored(what string, id int64) {
	x.t.Helper()
	if agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE id=$1 AND state='building' AND psbt IS NULL AND txid IS NULL", id) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM settlement_outputs") != 0 || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id IS NOT NULL") != 0 {
		x.t.Fatalf("%s: something was stored", what)
	}
}

// pinsRPCError asserts err is the node-reply refusal of method with class.
func pinsRPCError(t *testing.T, what string, err error, method, class string) {
	t.Helper()
	var re *escrowRPCError
	if !errors.As(err, &re) || re.Method != method || re.Class != class {
		t.Fatalf("%s: build returned %v, want %s %s", what, err, method, class)
	}
}

// Kills TM1 (input vout unchecked), TM2 (input txid unchecked), TM3b (output values unchecked), TM4 (an extra output
// allowed), and pins the rest of settlementTxMatches: a createpsbt reply that is not exactly the asked-for inputs in
// order and the planned outputs is refused as a bad node reply before utxoupdatepsbt is asked anything, with nothing
// stored.
func TestPinsBuildRefusesSkewedCreatepsbtReplies(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 60000, 40000) // two inputs, one output
	if code, b := x.claim(order, x.vendorSess, claimAddr); code != 303 {
		t.Fatalf("claim: %d %s", code, b)
	}
	id, _, _, _ := x.settlement(order)
	reshape := func(mut func(tx *psbtTx, ins, outs *[][]byte)) func(string) string {
		return func(b64 string) string { return pinsReshape(t, b64, mut) }
	}
	for _, c := range []struct {
		name string
		skew func(string) string
	}{
		{"an input's vout one higher", reshape(func(tx *psbtTx, _, _ *[][]byte) { tx.In[1].Vout++ })},
		{"the first input's vout one higher", reshape(func(tx *psbtTx, _, _ *[][]byte) { tx.In[0].Vout++ })},
		{"one input's txid changed", reshape(func(tx *psbtTx, _, _ *[][]byte) { tx.In[0].TxID[0] ^= 1 })},
		{"the second input's txid changed", reshape(func(tx *psbtTx, _, _ *[][]byte) { tx.In[1].TxID[31] ^= 1 })},
		{"an extra output", reshape(func(tx *psbtTx, _, outs *[][]byte) {
			tx.Out = append(tx.Out, psbtTxOut{Value: 1000, Script: tx.Out[0].Script})
			*outs = append(*outs, psbtMap())
		})},
		{"a dropped input", reshape(func(tx *psbtTx, ins, _ *[][]byte) { tx.In, *ins = tx.In[:1], (*ins)[:1] })},
		{"an extra input", reshape(func(tx *psbtTx, ins, _ *[][]byte) {
			extra := tx.In[0]
			extra.Vout += 7
			tx.In, *ins = append(tx.In, extra), append(*ins, psbtMap())
		})},
		{"the inputs in the other order", reshape(func(tx *psbtTx, ins, _ *[][]byte) {
			tx.In[0], tx.In[1] = tx.In[1], tx.In[0]
			(*ins)[0], (*ins)[1] = (*ins)[1], (*ins)[0]
		})},
		{"an output worth one satoshi less", reshape(func(tx *psbtTx, _, _ *[][]byte) { tx.Out[0].Value-- })},
		{"an output worth one satoshi more", reshape(func(tx *psbtTx, _, _ *[][]byte) { tx.Out[0].Value++ })},
	} {
		x.pinsSkew(c.skew, nil, nil)
		updates := x.core.count("utxoupdatepsbt")
		pinsRPCError(t, c.name, x.pinsBuild(id), "createpsbt", "bad-reply")
		if x.core.count("utxoupdatepsbt") != updates {
			t.Fatalf("%s: the build went on to utxoupdatepsbt", c.name)
		}
		x.pinsNothingStored(c.name, id)
	}
	// Control: the unskewed reply builds, so each refusal above was the skew's doing.
	x.pinsSkew(nil, nil, nil)
	if err := x.pinsBuild(id); err != nil {
		t.Fatal(err)
	}
	if _, _, state, _ := x.settlement(order); state != "awaiting_signature" {
		t.Fatalf("control: %s", state)
	}
}

// Kills IF1b (witness UTXO value unchecked), IF2b (witness UTXO script unchecked), IF3 (a partial signature
// allowed), IF4 (a finalized input allowed) and IF6 (a derivation for a key outside the escrow script allowed): a
// utxoupdatepsbt reply whose first input carries any of these is refused as a bad node reply, with nothing stored.
func TestPinsBuildRefusesSkewedUtxoupdatepsbtReplies(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 60000, 40000)
	if code, b := x.claim(order, x.vendorSess, claimAddr); code != 303 {
		t.Fatalf("claim: %d %s", code, b)
	}
	id, _, _, _ := x.settlement(order)
	vpub, _ := hex.DecodeString(x.keyHex(order, "vendor"))
	// withUTXO replaces input 0's witness UTXO by value and script.
	withUTXO := func(edit func(u *psbtTxOut)) func(string) string {
		return func(b64 string) string {
			p, err := decodePSBT(b64)
			if err != nil {
				t.Fatal(err)
			}
			u := *p.Inputs[0].WitnessUTXO
			u.Script = slices.Clone(u.Script)
			edit(&u)
			raw, err := p.withoutInputFields(func(i int, k []byte) bool { return i == 0 && len(k) == 1 && k[0] == 0x01 })
			if err != nil {
				t.Fatal(err)
			}
			value := append(binary.LittleEndian.AppendUint64(nil, uint64(u.Value)), compactSize(len(u.Script))...)
			return encodePSBT(addField(t, raw, 0, []byte{0x01}, append(value, u.Script...)))
		}
	}
	plus := func(key byte, pub, value []byte) func(string) string {
		return func(b64 string) string {
			p, err := decodePSBT(b64)
			if err != nil {
				t.Fatal(err)
			}
			return encodePSBT(addField(t, p.Raw, 0, append([]byte{key}, pub...), value))
		}
	}
	signed := func(b64 string) string {
		p, err := decodePSBT(b64)
		if err != nil {
			t.Fatal(err)
		}
		return plus(0x02, vpub, fakeSig(vpub, p.TxID(), 1))(b64)
	}
	for _, c := range []struct {
		name string
		skew func(string) string
	}{
		{"a witness UTXO worth one satoshi less", withUTXO(func(u *psbtTxOut) { u.Value-- })},
		{"a witness UTXO worth one satoshi more", withUTXO(func(u *psbtTxOut) { u.Value++ })},
		{"a witness UTXO paying another script", withUTXO(func(u *psbtTxOut) { u.Script[len(u.Script)-1] ^= 1 })},
		{"a partial signature already on the input", signed},
		{"a finalized input", plus(0x08, nil, []byte{0})},
		{"a derivation of a key outside the escrow script", plus(0x06, testPub(9), []byte{1, 2, 3, 4})},
	} {
		x.pinsSkew(nil, c.skew, nil)
		pinsRPCError(t, c.name, x.pinsBuild(id), "utxoupdatepsbt", "bad-reply")
		x.pinsNothingStored(c.name, id)
	}
	x.pinsSkew(nil, nil, nil)
	if err := x.pinsBuild(id); err != nil {
		t.Fatal(err)
	}
	if _, _, state, _ := x.settlement(order); state != "awaiting_signature" {
		t.Fatalf("control: %s", state)
	}
}

// settlementTxMatches on a packet whose counts disagree (which parsePSBT never produces): more transaction inputs
// than asked for is refused on the transaction's own count. Kills TM5 (input count compared with <), which no
// parsed PSBT can reach because the input maps always follow the transaction.
func TestPinsSettlementTxMatchesCounts(t *testing.T) {
	ins := []settleInput{{TxID: txidOf("m"), Vout: 0}, {TxID: txidOf("m"), Vout: 1}}
	plan := settlePlan{Outputs: []settleOutput{{Sats: 1000}}}
	raw, _ := hex.DecodeString(txidOf("m"))
	slices.Reverse(raw)
	var id [32]byte
	copy(id[:], raw)
	packet := func(nTxIn, nMaps, nOut int) *psbtPacket {
		p := &psbtPacket{Inputs: make([]psbtInput, nMaps)}
		for i := range nTxIn {
			p.Tx.In = append(p.Tx.In, psbtTxIn{TxID: id, Vout: uint32(i)})
		}
		for range nOut {
			p.Tx.Out = append(p.Tx.Out, psbtTxOut{Value: 1000})
		}
		return p
	}
	for _, c := range []struct {
		name string
		p    *psbtPacket
		want bool
	}{
		{"as asked", packet(2, 2, 1), true},
		{"a third transaction input", packet(3, 2, 1), false},
		{"a third input and a third map", packet(3, 3, 1), false},
		{"one transaction input", packet(1, 2, 1), false},
		{"one input map too few", packet(2, 1, 1), false},
		{"an extra output", packet(2, 2, 2), false},
	} {
		if got := settlementTxMatches(c.p, ins, plan); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	bad := packet(2, 2, 1)
	bad.Tx.Out[0].Value--
	if settlementTxMatches(bad, ins, plan) {
		t.Error("an output one satoshi short matched")
	}
	bad = packet(2, 2, 1)
	bad.Tx.In[1].Vout++
	if settlementTxMatches(bad, ins, plan) {
		t.Error("a vout one higher matched")
	}
	bad = packet(2, 2, 1)
	bad.Tx.In[0].TxID[0] ^= 1
	if settlementTxMatches(bad, ins, plan) {
		t.Error("another txid matched")
	}
}

// Kills QB1 (gettxout asked of the first input only) and BS3 (a gettxout error skipped): each way the node can
// disagree about the SECOND input of a two-input escrow leaves the settlement building with its class and asks for no
// PSBT; a gettxout that fails returns the error and stores nothing.
func TestPinsBuildChecksTheSecondInput(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 60000, 40000)
	if code, b := x.claim(order, x.vendorSess, claimAddr); code != 303 {
		t.Fatalf("claim: %d %s", code, b)
	}
	id, _, _, _ := x.settlement(order)
	second := func(edit func(map[string]any)) func(map[string]any) {
		return func(m map[string]any) {
			if fmt.Sprint(m["value"]) == "0.0004" { // the 40000-sat deposit, the second input
				edit(m)
			}
		}
	}
	for _, c := range []struct {
		name, class string
		skew        func(map[string]any)
		spend       bool
	}{
		{"second input unconfirmed", "input-unconfirmed", second(func(m map[string]any) { m["confirmations"] = 1 }), false},
		{"second input of another value", "input-mismatch", second(func(m map[string]any) { m["value"] = 0.0003 }), false},
		{"second input at another address", "input-mismatch", second(func(m map[string]any) {
			m["scriptPubKey"] = map[string]any{"address": "bcrt1qsomewhereelse0000000000000000000"}
		}), false},
		{"second input spent", "input-spent", nil, true},
	} {
		x.pinsSkew(nil, nil, c.skew)
		x.core.mu.Lock()
		x.core.spent = nil
		x.core.mu.Unlock()
		if c.spend {
			x.core.spend(txidOf(order), 1)
		}
		creates := x.core.count("createpsbt")
		if err := x.pinsBuild(id); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if _, _, state, class := x.settlement(order); state != "building" || class != c.class || x.core.count("createpsbt") != creates {
			t.Fatalf("%s: %s %q, createpsbt called %d times", c.name, state, class, x.core.count("createpsbt")-creates)
		}
		x.pinsNothingStored(c.name, id)
	}
	// A gettxout that fails (an unreadable reply) on the second input, then on the first: the build returns the error
	// and stores nothing; it does not skip the input.
	x.core.mu.Lock()
	x.core.spent = nil
	x.core.mu.Unlock()
	for _, c := range []struct {
		name string
		skew func(map[string]any)
	}{
		{"second input's value unreadable", second(func(m map[string]any) { m["value"] = "lots" })},
		{"second input's confirmations negative", second(func(m map[string]any) { m["confirmations"] = -1 })},
		{"first input's value unreadable", func(m map[string]any) {
			if fmt.Sprint(m["value"]) == "0.0006" {
				m["value"] = "lots"
			}
		}},
	} {
		x.pinsSkew(nil, nil, c.skew)
		creates := x.core.count("createpsbt")
		pinsRPCError(t, c.name, x.pinsBuild(id), "gettxout", "bad-reply")
		if x.core.count("createpsbt") != creates {
			t.Fatalf("%s: a PSBT was asked for", c.name)
		}
		x.pinsNothingStored(c.name, id)
	}
	x.pinsSkew(nil, nil, nil)
	if err := x.pinsBuild(id); err != nil {
		t.Fatal(err)
	}
	if _, _, state, class := x.settlement(order); state != "awaiting_signature" || class != "" {
		t.Fatalf("control: %s %q", state, class)
	}
}

// Kills QB7 (an output first seen after the order closed counted as a build input): neither a claim nor a refund
// spends it, and it stays unclaimed.
func TestPinsAfterCloseOutputIsNeverAnInput(t *testing.T) {
	x := newEscrowEnv(t)
	// A release: the late output arrives after completion, before the claim is built.
	order := x.completed(refundAddr, 100000)
	x.fund(order, "late-release", 3, 30000)
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND after_close", order) != 1 {
		t.Fatal("the late output was not recorded as after the close")
	}
	if code, b := x.claim(order, x.vendorSess, claimAddr); code != 303 {
		t.Fatalf("claim: %d %s", code, b)
	}
	x.poll()
	id, _, state, _ := x.settlement(order)
	if rows := agStrings(t, x.testEnv, "SELECT role||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id); state != "awaiting_signature" || !slices.Equal(rows, []string{"claimant 99682"}) {
		t.Fatalf("release %s %q", state, rows)
	}
	if _, params := x.core.lastCall(t, "createpsbt"); strings.Contains(params, txidOf("late-release")) {
		t.Fatalf("createpsbt spends the late output: %s", params)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND after_close AND settlement_id IS NULL", order) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id=$2", order, id) != 1 {
		t.Fatal("the late output was claimed by the settlement, or the escrow output was not")
	}
	// A refund: cancelled with a deposit, a second deposit after the cancel, then the refund is built.
	refund := x.draft()
	x.mustCheckout(refund, "bcrt1qbuyerrefund00000000000000000000002")
	x.fund(refund, refund, 3, 40000)
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {refund}, "from": {stateAwaitingPayment}, "amount_seen": {"40000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	x.fund(refund, "late-refund", 3, 25000)
	rid, _, state, _ := x.settlement(refund)
	if rows := agStrings(t, x.testEnv, "SELECT role||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", rid); state != "awaiting_signature" || !slices.Equal(rows, []string{"claimant 39682"}) {
		t.Fatalf("refund %s %q", state, rows)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND after_close AND settlement_id IS NULL", refund) != 1 {
		t.Fatal("the late output of the refunded order was claimed")
	}
}

// Kills BS11 (a conflicted output, confirmations -1, counted as an input) and BS9 (an output another settlement
// already claimed counted as an input): the build spends only the confirmed, unclaimed output.
func TestPinsBuildSkipsConflictedAndClaimedOutputs(t *testing.T) {
	x := newEscrowEnv(t)
	conflicted := x.completed(refundAddr, 60000, 40000)
	claimed := x.completed("bcrt1qbuyerrefund00000000000000000000002", 60000, 40000)
	// Both set up after the last watcher pass (a pass rewrites an output's confirmations from the node).
	x.DB.Exec("UPDATE payments SET confirmations=-1 WHERE order_id=$1 AND idx=1", conflicted)
	if _, err := x.DB.Exec("INSERT INTO settlements(order_id,kind,destination) VALUES($1,'stray_refund',$2)", claimed, refundAddr); err != nil {
		t.Fatal(err)
	}
	x.DB.Exec("UPDATE payments SET settlement_id=(SELECT id FROM settlements WHERE order_id=$1 AND kind='stray_refund') WHERE order_id=$1 AND idx=1", claimed)
	for _, c := range []struct {
		name, order, addr string
	}{{"conflicted", conflicted, claimAddr}, {"claimed elsewhere", claimed, "bcrt1qvendorclaim000000000000000000000002"}} {
		if code, b := x.claim(c.order, x.vendorSess, c.addr); code != 303 {
			t.Fatalf("%s: claim %d %s", c.name, code, b)
		}
		id, _, _, _ := x.settlement(c.order)
		if err := x.pinsBuild(id); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		_, _, state, class := x.settlement(c.order)
		rows := agStrings(t, x.testEnv, "SELECT role||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
		if state != "awaiting_signature" || class != "" || !slices.Equal(rows, []string{"claimant 59682"}) {
			t.Fatalf("%s: %s %q %q", c.name, state, class, rows)
		}
		if _, params := x.core.lastCall(t, "createpsbt"); params != `[[{"txid":"`+txidOf(c.order)+`","vout":0}],[{"`+c.addr+`":0.00059682}],0,true]` {
			t.Fatalf("%s: createpsbt %s", c.name, params)
		}
		if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id=$2", c.order, id) != 1 ||
			agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND idx=1 AND (settlement_id IS NULL OR settlement_id<>$2)", c.order, id) != 1 {
			t.Fatalf("%s: the wrong outputs were claimed", c.name)
		}
	}
}

// Kills BS5 (nothing to settle waits instead of failing): a settlement with no output left that counts ends failed,
// with its order note, a notice to both parties and its words on the page; no PSBT is asked for.
func TestPinsNothingToSettleFails(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 100000)
	if code, b := x.claim(order, x.vendorSess, claimAddr); code != 303 {
		t.Fatalf("claim: %d %s", code, b)
	}
	id, _, _, _ := x.settlement(order)
	x.DB.Exec("UPDATE payments SET confirmations=-1 WHERE order_id=$1", order) // every output conflicted
	creates := x.core.count("createpsbt")
	if err := x.pinsBuild(id); err != nil {
		t.Fatal(err)
	}
	if _, _, state, class := x.settlement(order); state != "failed" || class != "nothing-to-settle" || x.core.count("createpsbt") != creates {
		t.Fatalf("%s %q, createpsbt called %d times", state, class, x.core.count("createpsbt")-creates)
	}
	const text = "No confirmed deposit of this escrow is left to spend, so there is nothing to settle."
	if ev := strings.Join(x.events(order), "\n"); !strings.Contains(ev, "Escrow settlement not built: "+text) {
		t.Fatalf("no note:\n%s", ev)
	}
	for _, uid := range []string{x.buyer, x.vendor} {
		if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%its escrow settlement could not be built.%nothing to settle.'", uid) != 1 {
			t.Fatal("a party was not told")
		}
	}
	if page := x.page("/order?id="+order, x.buyerSess); !strings.Contains(page, "Not built. "+text) {
		t.Fatalf("page:\n%s", page)
	}
	if err := x.pinsBuild(id); err != nil || agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'Escrow settlement not built%'", order) != 1 {
		t.Fatalf("a failed settlement was noted again: %v", err)
	}
}

// Kills QB6 (51 inputs allowed) and BS7 (too many inputs fails instead of waiting): 50 outputs build; 51 leave the
// settlement building as too-many-inputs, claim nothing, ask for no PSBT and say so on the page.
func TestPinsMaxInputs(t *testing.T) {
	x := newEscrowEnv(t)
	fifty := x.completed(refundAddr, slices.Repeat([]int64{2000}, 50)...)
	if code, b := x.claim(fifty, x.vendorSess, claimAddr); code != 303 {
		t.Fatalf("claim: %d %s", code, b)
	}
	x.poll()
	id, _, state, class := x.settlement(fifty)
	// 50 inputs, one output: 2 * (11 + 50*105 + 43) = 10608.
	if rows := agStrings(t, x.testEnv, "SELECT role||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id); state != "awaiting_signature" || class != "" || !slices.Equal(rows, []string{"claimant 89392"}) ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id=$2", fifty, id) != 50 {
		t.Fatalf("50 outputs: %s %q %q", state, class, rows)
	}
	if p, err := decodePSBT(x.partyCopy(fifty, x.vendorSess, id)); err != nil || len(p.Inputs) != 50 {
		t.Fatalf("the copy of 50 inputs: %v", err)
	}

	many := x.completed("bcrt1qbuyerrefund00000000000000000000002", slices.Repeat([]int64{2000}, 51)...)
	if code, b := x.claim(many, x.vendorSess, "bcrt1qvendorclaim000000000000000000000002"); code != 303 {
		t.Fatalf("claim: %d %s", code, b)
	}
	creates, gets := x.core.count("createpsbt"), x.core.count("gettxout")
	x.poll()
	id, _, state, class = x.settlement(many)
	if state != "building" || class != "too-many-inputs" || x.core.count("createpsbt") != creates || x.core.count("gettxout") != gets {
		t.Fatalf("51 outputs: %s %q", state, class)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id IS NOT NULL", many) != 0 || agInt(x.testEnv, "SELECT count(*) FROM settlement_outputs WHERE settlement_id=$1", id) != 0 {
		t.Fatal("51 outputs: something was stored")
	}
	if page := x.page("/order?id="+many, x.vendorSess); !strings.Contains(page, "more deposits than one settlement can spend") {
		t.Fatalf("page does not say why:\n%s", page)
	}
	// It is tried again on the next pass (still building, not failed).
	x.poll()
	if _, _, state, class = x.settlement(many); state != "building" || class != "too-many-inputs" {
		t.Fatalf("second pass: %s %q", state, class)
	}
}

// Kills QB12 (an overpayment exactly at the floor folded) with a market fee and an overpayment each exactly at the
// floor (kept as outputs) and one satoshi below it (folded, with a note).
func TestPinsPlanSettlementFloorBoundary(t *testing.T) {
	const floor = 10000
	plan := func(total, price, bps int64) ([]string, int) {
		p, refused := planSettlement("release", 1, total, price, bps, "fee", "claim", "refund", 2, floor)
		if refused != "" {
			t.Fatalf("total %d refused: %s", total, refused)
		}
		var got []string
		for _, o := range p.Outputs {
			got = append(got, fmt.Sprintf("%s %d", o.Role, o.Sats))
		}
		return got, len(p.Folded)
	}
	// Overpayment of exactly 10000, then 9999 (fee 404 for one input and two outputs, one for one output).
	if got, folded := plan(110000, 100000, 0); !slices.Equal(got, []string{"claimant 99596", "buyer_return 10000"}) || folded != 0 {
		t.Errorf("overpayment at the floor: %q folded %d", got, folded)
	}
	if got, folded := plan(109999, 100000, 0); !slices.Equal(got, []string{"claimant 109681"}) || folded != 1 {
		t.Errorf("overpayment below the floor: %q folded %d", got, folded)
	}
	// Market fee of exactly 10000 (price 100000 at 1000 bps), then 9999 (price 99990).
	if got, folded := plan(100000, 100000, 1000); !slices.Equal(got, []string{"claimant 89596", "market_fee 10000"}) || folded != 0 {
		t.Errorf("fee at the floor: %q folded %d", got, folded)
	}
	if got, folded := plan(99990, 99990, 1000); !slices.Equal(got, []string{"claimant 99672"}) || folded != 1 {
		t.Errorf("fee below the floor: %q folded %d", got, folded)
	}
}

// Kills ST6 (the "settlement built" order note dropped): each built settlement leaves its note in the order history,
// naming the kind, the total, the input count, the network fee and whose output pays it.
func TestPinsSettlementBuiltNote(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 100000)
	x.claim(order, x.vendorSess, claimAddr)
	x.poll()
	want := "Escrow settlement built (the vendor's claim): spends 0.001 BTC from the escrow in 1 output(s); network fee 0.00000318 BTC, taken from the vendor's output. It waits for a signature from the buyer or the vendor. Nothing is sent yet."
	if ev := strings.Join(x.events(order), "\n"); strings.Count(ev, want) != 1 {
		t.Fatalf("release note missing:\n%s", ev)
	}
	refund := x.draft()
	x.mustCheckout(refund, "bcrt1qbuyerrefund00000000000000000000002")
	x.fund(refund, refund, 3, 40000)
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {refund}, "from": {stateAwaitingPayment}, "amount_seen": {"40000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	want = "Escrow settlement built (a refund to the buyer): spends 0.0004 BTC from the escrow in 1 output(s); network fee 0.00000318 BTC, taken from the buyer's refund output. It waits for a signature from the buyer or the vendor. Nothing is sent yet."
	if ev := strings.Join(x.events(refund), "\n"); strings.Count(ev, want) != 1 {
		t.Fatalf("refund note missing:\n%s", ev)
	}
}

// Kills CL4 (the configured fee address accepted as a claim address), CL5 (a pooled deposit address accepted) and CL6
// (another escrow's snapshotted fee destination accepted): each is refused as already used, nothing is requested,
// and a fresh address is then accepted.
func TestPinsClaimRefusesMarketAddresses(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 100000)
	other := x.draft()
	x.mustCheckout(other, "bcrt1qbuyerrefund00000000000000000000002")
	x.DB.Exec("UPDATE escrows SET fee_bps=100,fee_destination='bcrt1qoldfee000000000000000000000000000' WHERE order_id=$1", other)
	x.A.escrow.Load().feeAddress = "bcrt1qnowfee000000000000000000000000000"
	x.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'BTC','bcrt1qpooleddeposit000000000000000000000','btc-regtest')",
		x.testEnv.order(x.buyer, x.testEnv.product(x.vendor, "physical"), "BTC", stateAwaitingPayment))
	for _, c := range []struct{ name, addr string }{
		{"the configured fee address", "bcrt1qnowfee000000000000000000000000000"},
		{"a pooled deposit address", "bcrt1qpooleddeposit000000000000000000000"},
		{"another escrow's fee destination", "bcrt1qoldfee000000000000000000000000000"},
	} {
		code, body := x.claim(order, x.vendorSess, c.addr)
		if code != 409 || !strings.Contains(body, "already used on this market") {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM settlements"); n != 0 {
			t.Fatalf("%s: %d settlements after the refusal", c.name, n)
		}
	}
	if code, body := x.claim(order, x.vendorSess, claimAddr); code != 303 {
		t.Fatalf("a fresh address: %d %s", code, body)
	}
}

// Kills QP5 (a market-key signature refused as the market's only on the first input): on the second input of two it
// is refused with the market-key message, not the other-key one, and nothing is stored.
func TestPinsPasteMarketSignatureOnSecondInput(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 60000, 40000)
	x.claim(order, x.vendorSess, claimAddr)
	x.poll()
	id, _, _, _ := x.settlement(order)
	vk, _ := parseEscrowKey(x.vendorKey)
	vcopy := x.partyCopy(order, x.vendorSess, id)
	cp, _ := decodePSBT(vcopy)
	vpub, _ := hex.DecodeString(x.keyHex(order, "vendor"))
	mpub, _ := hex.DecodeString(x.keyHex(order, "market"))
	good := walletSign(t, vcopy, vk.Fingerprint, 1)
	gp, _ := decodePSBT(good)
	sigOn := func(raw []byte, i int, pub []byte) []byte {
		return addField(t, raw, i, append([]byte{0x02}, pub...), fakeSig(pub, cp.TxID(), 1))
	}
	notes := agInt(x.testEnv, "SELECT count(*) FROM notifications")
	for _, c := range []struct {
		name string
		psbt []byte
	}{
		{"the vendor signed both inputs, the market key signed the second", sigOn(gp.Raw, 1, mpub)},
		{"the vendor signed the first input, the market key the second", sigOn(sigOn(cp.Raw, 0, vpub), 1, mpub)},
	} {
		code, body := x.sign(order, x.vendorSess, id, encodePSBT(c.psbt))
		if code != 400 || !strings.Contains(body, "under the market's key") || strings.Contains(body, "a key other than your escrow key") {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
		if _, _, state, _ := x.settlement(order); state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM settlement_signatures") != 0 || agInt(x.testEnv, "SELECT count(*) FROM notifications") != notes {
			t.Fatalf("%s: something was stored (%s)", c.name, state)
		}
	}
}

// Kills DL2 (the download served with ESCROW_BTC off): with escrow switched off /escrow-psbt answers as an unknown
// page does, for the vendor, the buyer and a visitor alike, although the settlement awaits a signature.
func TestPinsDownloadIsAbsentWithEscrowOff(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 100000)
	x.claim(order, x.vendorSess, claimAddr)
	x.poll()
	id, _, state, _ := x.settlement(order)
	if state != "awaiting_signature" {
		t.Fatalf("state %s", state)
	}
	x.partyCopy(order, x.vendorSess, id) // served while escrow is on
	t.Setenv("ESCROW_BTC", "off")
	x.restart()
	for name, sess := range map[string]string{"vendor": x.vendorSess, "buyer": x.buyerSess, "visitor": randomToken()} {
		unknown := x.do("GET", "/no-such-page", sess, nil)
		w := x.do("GET", "/escrow-psbt?id="+order+"&s="+fmt.Sprint(id), sess, nil)
		if w.Code != 404 || w.Body.String() != unknown.Body.String() || strings.Contains(w.Header().Get("Content-Disposition"), "psbt") {
			t.Errorf("%s: %d %q", name, w.Code, w.Header())
		}
	}
}

// Kills QV2 (the "Not read" figures not set while escrow is unavailable) and QV3 (the claim form offered while escrow
// is unavailable): with the escrow node down a cancelled, empty escrow does not show "0 BTC" for what was never
// read, and a completed order offers the vendor no claim form until escrow is back.
func TestPinsEscrowUnavailablePanel(t *testing.T) {
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, refundAddr)
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", x.order)
	x.poll()
	if x.state(x.order) != stateCancelled {
		t.Fatalf("state %s", x.state(x.order))
	}
	done := x.completed("bcrt1qbuyerrefund00000000000000000000002", 100000)
	up := x.A.escrow.Load()
	x.A.escrow.Store(nil)
	body := x.page("/order?id="+x.order, x.buyerSess)
	if strings.Count(body, "Not read while Bitcoin escrow is unavailable") != 2 || strings.Contains(body, "<dd>0 BTC</dd>") ||
		!strings.Contains(body, "This escrow cannot be checked right now") {
		t.Fatalf("figures with escrow unavailable:\n%s", body)
	}
	if page := x.page("/order?id="+done, x.vendorSess); strings.Contains(page, `action="/escrow/claim"`) {
		t.Fatal("the claim form is offered while escrow is unavailable")
	}
	x.A.escrow.Store(up)
	if page := x.page("/order?id="+done, x.vendorSess); !strings.Contains(page, `action="/escrow/claim"`) {
		t.Fatal("the claim form is not offered once escrow is back")
	}
	if body := x.page("/order?id="+x.order, x.buyerSess); strings.Contains(body, "Not read while") || strings.Count(body, "<dd>0 BTC</dd>") != 2 {
		t.Fatalf("figures with escrow available:\n%s", body)
	}
}

// Kills QW3 (the watch horizon ignoring a settlement still being built): an escrow whose order closed long ago is
// still read while its settlement is building, so a late-confirming output reaches the ledger and the build goes on.
func TestPinsWatcherReadsWhileSettlementBuilds(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.draft()
	addr := x.mustCheckout(order, refundAddr)
	x.core.deposit(addr, txidOf("full"), 0, 100000, 3)
	x.core.deposit(addr, txidOf("extra"), 0, 30000, 1)
	x.poll()
	x.claim(order, x.vendorSess, claimAddr)
	x.poll()
	if _, _, state, class := x.settlement(order); state != "building" || class != "waiting-confirmations" {
		t.Fatalf("with an output still confirming: %s %s", state, class)
	}
	// Past both horizons (closed over 24 hours ago, escrow over 30 days old).
	x.DB.Exec("UPDATE escrows SET created=now()-interval '31 days' WHERE order_id=$1", order)
	x.DB.Exec("UPDATE orders SET updated=now()-interval '2 days' WHERE id=$1", order)
	x.core.setConfs(txidOf("extra"), 3)
	x.poll()
	id, _, state, class := x.settlement(order)
	if state != "awaiting_signature" || class != "" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id=$2", order, id) != 2 {
		t.Fatalf("the aged order with a building settlement was not read: %s %q", state, class)
	}
}
