package market

import (
	"testing"
)

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
