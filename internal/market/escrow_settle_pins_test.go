package market

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"slices"

	"testing"
)

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
