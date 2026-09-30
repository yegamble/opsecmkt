package market

import (
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The frozen plan of a settlement (ES-5), without a database: who each output pays, the network fee from the
// claimant's output only, the market fee never reduced, outputs below the floor folded into the claimant's, and the
// fee caps.
func TestPlanSettlement(t *testing.T) {
	const floor = 10000
	cases := []struct {
		name         string
		kind         string
		nIn          int
		total, price int64
		bps          int64
		feeDest      string
		rate         int64
		want         []string
		fee          int64
		refused      string
		folded       int
	}{
		{"refund, one input", "refund", 1, 40000, 100000, 250, "fee", 2, []string{"claimant refund 39682"}, 318, "", 0},
		{"refund takes no market fee", "refund", 2, 150000, 100000, 250, "fee", 2, []string{"claimant refund 149472"}, 528, "", 0},
		{"release, exact, no fee", "release", 1, 100000, 100000, 0, "", 2, []string{"claimant claim 99682"}, 318, "", 0},
		{"release with fee and return", "release", 2, 120000, 100000, 1500, "fee", 2, []string{"claimant claim 84300", "market_fee fee 15000", "buyer_return refund 20000"}, 700, "", 0},
		{"fee below floor folds", "release", 1, 100000, 100000, 50, "fee", 2, []string{"claimant claim 99682"}, 318, "", 1},
		{"return below floor folds", "release", 1, 109999, 100000, 0, "", 2, []string{"claimant claim 109681"}, 318, "", 1},
		{"fee at the floor stays", "release", 1, 100000, 100000, 1000, "fee", 2, []string{"claimant claim 89596", "market_fee fee 10000"}, 404, "", 0},
		{"fee without a destination is not charged", "release", 1, 100000, 100000, 250, "", 2, []string{"claimant claim 99682"}, 318, "", 0},
		{"short escrow still pays the full fee", "release", 1, 90000, 100000, 1500, "fee", 2, []string{"claimant claim 74596", "market_fee fee 15000"}, 404, "", 0},
		{"absolute cap", "release", 10, 1000000, 1000000, 0, "", 100, nil, 110400, "fee-cap", 0},
		{"rate cap", "refund", 1, 1000000, 1000000, 0, "", 101, nil, 101 * 159, "fee-cap", 0},
		{"zero rate", "refund", 1, 1000000, 1000000, 0, "", 0, nil, 0, "fee-cap", 0},
		{"too small after the fee", "refund", 1, 10317, 100000, 0, "", 2, nil, 318, "too-small", 0},
		{"exactly the floor after the fee", "refund", 1, 10318, 100000, 0, "", 2, []string{"claimant refund 10000"}, 318, "", 0},
		{"too small after the market fee", "release", 1, 20000, 20000, 5000, "fee", 2, nil, 404, "too-small", 0},
	}
	for _, c := range cases {
		claimant := "claim"
		if c.kind == "refund" {
			claimant = "refund"
		}
		p, refused := planSettlement(c.kind, c.nIn, c.total, c.price, c.bps, c.feeDest, claimant, "refund", c.rate, floor)
		var got []string
		var sum int64
		for _, o := range p.Outputs {
			got = append(got, o.Role+" "+o.Address+" "+strconv.FormatInt(o.Sats, 10))
			sum += o.Sats
		}
		if refused != c.refused || !slices.Equal(got, c.want) || p.NetworkFee != c.fee || len(p.Folded) != c.folded {
			t.Errorf("%s: %q %q fee %d folded %d", c.name, refused, got, p.NetworkFee, len(p.Folded))
			continue
		}
		if refused == "" && sum+p.NetworkFee != c.total {
			t.Errorf("%s: outputs %d + fee %d != escrow %d", c.name, sum, p.NetworkFee, c.total)
		}
	}
	if marketFeeSats(2_100_000_000_000_000, 10000) != 2_100_000_000_000_000 || marketFeeSats(12345, 250) != 308 {
		t.Fatal("marketFeeSats")
	}
}

// A party's copy (settlementCopy) is the stored PSBT plus that party's origin on every input, nothing else: the same
// transaction, and every other field unchanged.
func TestSettlementCopy(t *testing.T) {
	stored := testCorePSBTUnsigned
	sp, err := decodePSBT(stored)
	if err != nil {
		t.Fatal(err)
	}
	pub := testPub(9)
	k := escrowKeyRow{Party: "vendor", Fingerprint: "5eed5eed", Origin: "/48h/1h/0h/2h", Suffix: "/0/7", PubKey: pub}
	cp, err := settlementCopy(stored, k)
	if err != nil {
		t.Fatal(err)
	}
	p, err := decodePSBT(cp)
	if err != nil || p.TxID() != sp.TxID() || len(p.Inputs) != len(sp.Inputs) {
		t.Fatalf("copy %v", err)
	}
	want := []uint32{48 | 1<<31, 1 | 1<<31, 1 << 31, 2 | 1<<31, 0, 7}
	for i, in := range p.Inputs {
		if len(in.Derivations) != len(sp.Inputs[i].Derivations)+1 {
			t.Fatalf("input %d: %d derivations", i, len(in.Derivations))
		}
		var d *psbtDerivation
		for j := range in.Derivations {
			if hex.EncodeToString(in.Derivations[j].PubKey) == hex.EncodeToString(pub) {
				d = &in.Derivations[j]
			}
		}
		if d == nil || hex.EncodeToString(d.Fingerprint[:]) != "5eed5eed" || !slices.Equal(d.Path, want) {
			t.Fatalf("input %d derivation %+v", i, d)
		}
	}
	if len(p.Raw) != len(sp.Raw)+len(p.Inputs)*(1+34+1+4+4*len(want)) {
		t.Fatalf("copy is %d bytes, stored %d: something else was added", len(p.Raw), len(sp.Raw))
	}
}

// A PSBT larger than any form post is refused by checkSettlementPaste itself, before its transaction is compared.
func TestCheckSettlementPasteOversize(t *testing.T) {
	if _, err := checkSettlementPaste(strings.Repeat("A", psbtMaxBase64+4), strings.Repeat("0", 64), escrowKeyRow{}, escrowKeyRow{}); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversize: %v", err)
	}
}
