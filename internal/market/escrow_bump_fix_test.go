package market

import (
	"context"

	"encoding/json"

	"slices"
	"strings"
	"testing"
)

// bumpFinal is the final transaction of fee bump bid signed by these parties' keys (and "market" for the market's),
// as the parties would finalize it themselves from their own wallets: the fake's finalizepsbt shape.
func (x *escrowEnv) bumpFinal(order string, bid int64, signers ...string) string {
	x.t.Helper()
	keys, err := escrowKeys(context.Background(), x.DB, order)
	if err != nil {
		x.t.Fatal(err)
	}
	p, err := decodePSBT(x.bumpPSBT(bid))
	if err != nil {
		x.t.Fatal(err)
	}
	raw := p.Raw
	for _, who := range signers {
		pub := keys[who].PubKey
		for i := range p.Inputs {
			raw = addField(x.t, raw, i, append([]byte{0x02}, pub...), fakeSig(pub, p.TxID(), 1))
		}
	}
	full, err := parsePSBT(raw)
	if err != nil {
		x.t.Fatal(err)
	}
	final, ok := fakeFinalTx(full)
	if !ok {
		x.t.Fatal("the fee bump does not finalize with these signatures")
	}
	return final
}

// sendRaw broadcasts raw through the fake node, as a party's own node would.
func (x *escrowEnv) sendRaw(raw string) string {
	x.t.Helper()
	params, _ := json.Marshal([]string{raw})
	got, code, msg := x.core.handle("", "sendrawtransaction", params)
	if code != 0 {
		x.t.Fatalf("the parties' broadcast was refused: %d %s", code, msg)
	}
	return got.(string)
}

// flaggedOrOutside reports whether the settlement was recorded as a spend outside the market in any way.
func (x *escrowEnv) flaggedOrOutside(order string, id int64) bool {
	return agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1 AND flagged", id) != 0 || x.settleByID(id).SpentBy != "" ||
		slices.ContainsFunc(x.events(order), func(ev string) bool { return strings.Contains(ev, "did not make") })
}

// A-384: the claimant's output may be exactly the floor after a bump (planBump and checkBump); one satoshi less is
// too small.
func TestEscrowBumpFloorBoundary(t *testing.T) {
	addr := regtestAddr("claim-floor")
	script, err := escrowAddressScript("regtest", addr)
	if err != nil {
		t.Fatal(err)
	}
	const claim, parentFee, fee = 100000, 2 * 159, 5 * 159
	outs := []settleOutput{{"claimant", addr, claim}}
	floor := int64(claim - (fee - parentFee))
	if next, got, class := planBump("regtest", outs, 1, parentFee, parentFee, 2, 5, floor); class != "" || got != fee || next[0].Sats != floor {
		t.Fatalf("planBump at the floor: %v %d %q", next, got, class)
	}
	if _, _, class := planBump("regtest", outs, 1, parentFee, parentFee, 2, 5, floor+1); class != settleTooSmall {
		t.Fatalf("planBump one below the floor: %q", class)
	}
	parent := psbtTx{Version: 2, In: []psbtTxIn{{Vout: 0, Sequence: 0xfffffffd}}, Out: []psbtTxOut{{Value: claim, Script: script}}}
	bump := parent
	bump.Out = []psbtTxOut{{Value: floor, Script: script}}
	if got, class := checkBump("regtest", parent, bump, outs, claim+parentFee, parentFee, floor); class != "" || got != fee {
		t.Fatalf("checkBump at the floor: %d %q", got, class)
	}
	if _, class := checkBump("regtest", parent, bump, outs, claim+parentFee, parentFee, floor+1); class != settleTooSmall {
		t.Fatalf("checkBump one below the floor: %q", class)
	}
}
