package market

import (
	"bytes"
	"encoding/hex"
	"html"
	"strings"
	"testing"
)

// A-456b (rulings P4-R2, P4-R17): the market co-signs a per-order (shape (b)) escrow's settlements and fee bumps through
// the escrow signer, never the Core arbiter wallet, which keeps shape (a). Each request is the stored PSBT as built (the
// witness UTXO and witness script of every input, no derivation, no sighash field) with the escrow's per-order market
// index, exactly what the signer's refusals allow. A signer that is down, that refuses, or whose key no longer matches
// the pins leaves the settlement party-signed with a reason the order page shows, and the next pass tries again with
// nothing rebuilt; a shape (a) settlement is co-signed by the arbiter all the while. DB integration with the simulated
// node (escrow_fake_test.go) and a fake signer that makes cmd/escrow-signer's checks (escrow_signer_test.go): simulated,
// not chain evidence (scripts/test-escrow-regtest.sh runs the real signer, TestEscrowRegtestShapeBRelease).

// signRequests is the sign requests the fake signer read.
func (f *fakeSigner) signRequests() []map[string]any {
	var out []map[string]any
	for _, r := range f.reqs() {
		if r["op"] == "sign" {
			out = append(out, r)
		}
	}
	return out
}

func TestEscrowShapeBCosignedBySigner(t *testing.T) {
	x, f := newPerOrderEnv(t)
	order, id := x.built(regtestAddr("cosign-refund"), regtestAddr("cosign-claim"), 60000, 40000)
	var stored string
	if err := x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	x.signAs(order, roleVendor, id)
	x.poll()
	r := x.settleByID(id)
	if r.State != "broadcast" || r.Cosigned != "market" || r.Signer != roleVendor || r.Error != "" {
		t.Fatalf("after the pass: %+v (signer refusals %v)", r, f.refused)
	}
	if n := x.core.count("walletprocesspsbt") + x.core.count("keypoolrefill"); n != 0 {
		t.Fatalf("the Core arbiter wallet was asked %d times for a per-order escrow", n)
	}
	reqs := f.signRequests()
	if len(reqs) != 1 || reqs[0]["psbt"] != stored || reqs[0]["index"] != float64(x.marketIndex(order)) || reqs[0]["version"] != float64(1) {
		t.Fatalf("sign requests %v, want one: the stored PSBT at market index %d", reqs, x.marketIndex(order))
	}
	sp, _ := decodePSBT(stored)
	for i, in := range sp.Inputs {
		if len(in.Derivations) != 0 || in.HasSighashType || in.WitnessUTXO == nil || in.WitnessScript == nil || len(in.PartialSigs) != 0 {
			t.Fatalf("input %d of the request is not the bare stored input", i)
		}
	}
	if len(f.refused) != 0 {
		t.Fatalf("the signer refused %v", f.refused)
	}
	// The broadcast transaction carries the market's signature from the signer's key and the vendor's.
	mpub, _ := hex.DecodeString(x.keyHex(order, "market"))
	msig := fakeSig(mpub, r.TxID, 1)
	raw, _ := hex.DecodeString(r.Raw)
	st, err := parseSignedTx(raw)
	if err != nil || len(st.Witness) != 2 {
		t.Fatalf("final transaction: %v", err)
	}
	for i, w := range st.Witness {
		if len(w) != 4 || !bytes.Equal(w[1], msig) && !bytes.Equal(w[2], msig) {
			t.Fatalf("input %d's witness lacks the signer's signature", i)
		}
	}
}

// A signer that is down, a refusal and a key that no longer matches the pins each leave the per-order settlement
// party-signed, with nothing rebuilt and a reason on the order page; a shape (a) settlement is co-signed by the arbiter
// on the same pass (a mismatch disables shape (b) only, P4-R17). Once the signer answers again it is co-signed.
func TestEscrowShapeBSignerWaits(t *testing.T) {
	x, f := newPerOrderEnv(t)
	// A shape (a) escrow made while both keys were Bitcoin Core keys.
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='core' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	coreOrder, coreID := x.built(regtestAddr("wait-core-refund"), regtestAddr("wait-core-claim"), 100000)
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='per_order' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	order, id := x.built(regtestAddr("wait-refund"), regtestAddr("wait-claim"), 100000)
	var psbt string
	x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&psbt)
	x.signAs(order, roleVendor, id)
	waits := func(what, class string) {
		t.Helper()
		x.poll()
		r := x.settleByID(id)
		var now string
		x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&now)
		if r.State != "party_signed" || r.Error != class || r.Raw != "" || now != psbt {
			t.Fatalf("%s: %+v, want party_signed with %s and nothing rebuilt", what, r, class)
		}
		page := html.UnescapeString(x.page("/order?id="+order, x.vendorSess))
		if !strings.Contains(page, "The market's escrow signer could not co-sign it yet.") {
			t.Fatalf("%s: the order page does not say why it waits", what)
		}
	}
	f.stop()
	waits("signer down", sendSignerWait)
	f.start()
	f.set(func(f *fakeSigner) { f.refuse = "key-not-found" })
	waits("signer refuses", sendSignerRefused)
	f.set(func(f *fakeSigner) { f.refuse, f.key = "", signerKey(t, 8) })
	x.signAs(coreOrder, roleVendor, coreID)
	waits("signer key changed", sendSignerWait)
	if r := x.settleByID(coreID); r.State != "broadcast" || r.Cosigned != "market" {
		t.Fatalf("the shape (a) settlement on the same pass: %+v", r)
	}
	f.set(func(f *fakeSigner) { f.key = signerKey(t, 7) })
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || r.Cosigned != "market" || r.Error != "" {
		t.Fatalf("once the signer answers again: %+v", r)
	}
}

// A fee bump of a per-order escrow's broadcast settlement is co-signed through the signer too: the bump's stored PSBT at
// the escrow's market index.
func TestEscrowShapeBBumpCosignedBySigner(t *testing.T) {
	x, f := newPerOrderEnv(t)
	order, id := x.broadcastClaim("bump-b", 100000)
	b := x.mustBump(order, x.vendorSess, id, "5")
	x.signBumpAs(order, roleVendor, id, b.ID)
	n := len(f.signRequests())
	x.poll()
	if got := x.bumpByID(b.ID); got.State != "broadcast" || got.Cosigned != "market" {
		t.Fatalf("fee bump after the pass: %+v (signer refusals %v)", got, f.refused)
	}
	reqs := f.signRequests()[n:]
	if len(reqs) != 1 || reqs[0]["psbt"] != x.bumpPSBT(b.ID) || reqs[0]["index"] != float64(x.marketIndex(order)) {
		t.Fatalf("sign requests for the bump %v", reqs)
	}
	if x.core.count("walletprocesspsbt") != 0 {
		t.Fatal("the arbiter wallet signed a per-order escrow's fee bump")
	}
}
