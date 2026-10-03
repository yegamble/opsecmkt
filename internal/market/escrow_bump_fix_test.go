package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"html"
	"slices"
	"strings"
	"testing"
)

// Escrow Phase 2, slice P2-d-fix (A-382, A-383, A-384): a fee bump on the network without a stored final transaction,
// the fee cap at the smallest real size, the unlisted-conflict branch and the floor boundary. Fake escrow Core
// (simulated RPC, not chain evidence: TestEscrowRegtestBump has the real-chain legs).

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

// A-382: a fee bump the buyer and the vendor finalize and broadcast themselves (one party-signed on the market, one
// closed by a newer request and signed by neither there) is the market's own transaction: when the watch-only wallet
// holds it, it is checked and recorded as broadcast, and when it is mined the settlement is confirmed, with no flag and
// without the market signing anything.
func TestEscrowBumpSeenOnTheNetwork(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.broadcastClaim("bsn", 100000)
	parent := x.settleByID(id)
	b := x.mustBump(order, x.vendorSess, id, "5")
	x.signBumpAs(order, roleVendor, id, b.ID)
	raw := x.bumpFinal(order, b.ID, roleVendor, roleBuyer)
	if txid := x.sendRaw(raw); txid != b.TxID {
		t.Fatalf("sent %s, bump %s", txid, b.TxID)
	}
	signs := x.core.count("walletprocesspsbt")
	x.poll()
	r := x.bumpByID(b.ID)
	if r.State != "broadcast" || r.Raw != raw || r.Cosigned != "network" || x.core.count("walletprocesspsbt") != signs || x.settleByID(id).State != "broadcast" {
		t.Fatalf("the parties' own bump in the mempool: %+v (market signed %d times)", r, x.core.count("walletprocesspsbt")-signs)
	}
	if !slices.ContainsFunc(x.events(order), func(ev string) bool {
		return strings.Contains(ev, "fee bump") && strings.Contains(ev, "already on the regtest network") && strings.Contains(ev, "It carries the vendor's signature")
	}) {
		t.Fatalf("no honest note: %q", x.events(order))
	}
	x.core.setTxConfs(b.TxID, 3, "")
	x.core.setTxConfs(parent.TxID, -3, "")
	x.poll()
	if s := x.settleByID(id); s.State != "confirmed" || x.bumpByID(b.ID).State != "confirmed" || x.flaggedOrOutside(order, id) {
		t.Fatalf("mined: settlement %+v, bump %+v", s, x.bumpByID(b.ID))
	}

	// A bump closed by a newer request, which the parties still sign and broadcast themselves (nothing of it stored).
	order2, id2 := x.broadcastClaim("bsn2", 100000)
	p2 := x.settleByID(id2)
	old := x.mustBump(order2, x.vendorSess, id2, "4")
	x.mustBump(order2, x.vendorSess, id2, "6")
	if x.bumpByID(old.ID).State != "superseded" {
		t.Fatal("not superseded")
	}
	x.sendRaw(x.bumpFinal(order2, old.ID, roleVendor, roleBuyer))
	x.core.setTxConfs(old.TxID, 3, "")
	x.core.setTxConfs(p2.TxID, -3, "")
	x.poll()
	x.poll()
	if s := x.settleByID(id2); s.State != "confirmed" || x.bumpByID(old.ID).State != "confirmed" || x.bumpByID(old.ID).Cosigned != "network" || x.flaggedOrOutside(order2, id2) {
		t.Fatalf("a superseded bump mined: settlement %+v, bump %+v", s, x.bumpByID(old.ID))
	}
	if page := html.UnescapeString(x.page("/order?id="+order2, x.buyerSess)); !strings.Contains(page, "Confirmed on the regtest network: transaction "+old.TxID) {
		t.Fatalf("the confirmed page does not name the bump's transaction:\n%s", page)
	}
}

// A-382: a restore taken while a fee bump was party-signed: after the backup the market co-signed and broadcast it and
// it was mined, so the restored database has no final transaction for it. While the restore gate is set the watcher
// finds it mined, checks and records it and confirms the settlement, not external.
func TestEscrowBumpSeenAfterRestore(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.broadcastClaim("bsar", 100000)
	parent := x.settleByID(id)
	b := x.mustBump(order, x.vendorSess, id, "5")
	x.signBumpAs(order, roleVendor, id, b.ID) // the backup is taken now
	raw := x.bumpFinal(order, b.ID, roleVendor, "market")
	x.sendRaw(raw)
	x.core.setTxConfs(b.TxID, 3, "")
	x.core.setTxConfs(parent.TxID, -3, "")
	x.setGate(true)
	x.restoreHold(id)
	signs := x.core.count("walletprocesspsbt")
	x.poll()
	r := x.bumpByID(b.ID)
	if s := x.settleByID(id); s.State != "confirmed" || r.State != "confirmed" || r.Raw != raw || r.Cosigned != "network" || x.flaggedOrOutside(order, id) || x.core.count("walletprocesspsbt") != signs {
		t.Fatalf("after the restore: settlement %+v, bump %+v", s, r)
	}
}

// A-383: a fee bump's rate is capped so that its fee stays within escrowMaxFeeRate at the smallest size the signed
// transaction can have: the form offers no more, a request above it is refused with the fee-cap message, and a bump at
// the highest rate is broadcast (never stuck at final-fee).
func TestEscrowBumpRateCapAtRealSize(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.broadcastClaim("brc", 100000)
	// One input and one P2WPKH output: smallest real size (4+4+1+1+41+31)*4 + 2+1+1+2*10+1+105 = 458 weight, 114 vB;
	// the fee is the rate on the 159 vB bound, so 100*114/159 = 71 sat/vB is the highest.
	outs := []settleOutput{{"claimant", regtestAddr("claim-brc"), 99682}}
	if v, max := settleVSizeMin(1, outs), bumpMaxRate(1, outs); v != 114 || max != 71 {
		t.Fatalf("settleVSizeMin %d, bumpMaxRate %d", v, max)
	}
	if page := x.page("/order?id="+order, x.vendorSess); !strings.Contains(page, `min="3" max="71"`) {
		t.Fatalf("the fee bump form does not stop at 71 sat/vB:\n%s", page)
	}
	for _, rate := range []string{"72", "85", "100"} {
		if code, body := x.requestBump(order, x.vendorSess, id, rate); code != 400 || !strings.Contains(html.UnescapeString(body), "fee cap") ||
			!strings.Contains(html.UnescapeString(body), "The highest fee rate this settlement can take is 71 sat/vB") {
			t.Fatalf("rate %s: %d %s", rate, code, body)
		}
	}
	b := x.mustBump(order, x.vendorSess, id, "71")
	x.signBumpAs(order, roleVendor, id, b.ID)
	x.poll()
	r := x.bumpByID(b.ID)
	rawB, _ := hex.DecodeString(r.Raw)
	final, err := parseSignedTx(rawB)
	if r.State != "broadcast" || r.Error != "" || err != nil || r.Fee > escrowMaxFeeRate*final.VSize {
		t.Fatalf("the bump at the highest rate: %+v (%v)", r, err)
	}
	// checkBump refuses at build and finalize a fee within the cap on the size bound but not at the smallest real size.
	var stored string
	x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored)
	sp, _ := decodePSBT(stored)
	bump := sp.Tx
	bump.Out = slices.Clone(bump.Out)
	bump.Out[0].Value -= 72*159 - 318
	if _, class := checkBump(sp.Tx, bump, outs, 100000, 318, 10000); class != settleFeeCap {
		t.Fatalf("72 sat/vB on the bound (under 100 sat/vB on it): %q", class)
	}
}

// A-384: a broadcast settlement whose transaction the wallet reports conflicted, with no conflicting transaction listed,
// is external (the market's own transactions cannot be told apart from it, so it is not waited for).
func TestEscrowBumpConflictUnlistedExternal(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.broadcastClaim("bcu", 100000)
	x.core.setTxConfs(x.settleByID(id).TxID, -1, "")
	x.poll()
	if s := x.settleByID(id); s.State != "external" || s.SpentBy != "" || !x.flaggedOrOutside(order, id) {
		t.Fatalf("conflicted with nothing listed: %+v", s)
	}
}

// A-384: the claimant's output may be exactly the floor after a bump (planBump and checkBump); one satoshi less is
// too small.
func TestEscrowBumpFloorBoundary(t *testing.T) {
	addr := regtestAddr("claim-floor")
	script, err := regtestAddressScript(addr)
	if err != nil {
		t.Fatal(err)
	}
	const claim, parentFee, fee = 100000, 2 * 159, 5 * 159
	outs := []settleOutput{{"claimant", addr, claim}}
	floor := int64(claim - (fee - parentFee))
	if next, got, class := planBump(outs, 1, parentFee, parentFee, 2, 5, floor); class != "" || got != fee || next[0].Sats != floor {
		t.Fatalf("planBump at the floor: %v %d %q", next, got, class)
	}
	if _, _, class := planBump(outs, 1, parentFee, parentFee, 2, 5, floor+1); class != settleTooSmall {
		t.Fatalf("planBump one below the floor: %q", class)
	}
	parent := psbtTx{Version: 2, In: []psbtTxIn{{Vout: 0, Sequence: 0xfffffffd}}, Out: []psbtTxOut{{Value: claim, Script: script}}}
	bump := parent
	bump.Out = []psbtTxOut{{Value: floor, Script: script}}
	if got, class := checkBump(parent, bump, outs, claim+parentFee, parentFee, floor); class != "" || got != fee {
		t.Fatalf("checkBump at the floor: %d %q", got, class)
	}
	if _, class := checkBump(parent, bump, outs, claim+parentFee, parentFee, floor+1); class != settleTooSmall {
		t.Fatalf("checkBump one below the floor: %q", class)
	}
}
