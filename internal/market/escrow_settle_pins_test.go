package market

import (
	"encoding/hex"
	"fmt"

	"slices"

	"testing"
)

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
