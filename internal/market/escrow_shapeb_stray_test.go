package market

import (
	"bytes"
	"context"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-456b, QA5W3A-2 (rulings P4-R2, P4-R17): a deposit to a per-order escrow's sibling address (/0/k or /1/k of the order
// wallet) is returned by a stray refund that can be built, signed, co-signed and broadcast. The sibling's three keys are
// derived in Go from each one's per-order extended key (perOrderXpub; QA5W4A-5) and must pay the stored sibling address;
// the stray refund spends the deposits of one address at a time (the signer signs one 2-of-3 script per request), with
// that sibling's witness script; each party's copy names its own sibling key's path (/<i>/<branch>/<k>), and the signer
// co-signs at the escrow's market index, finding its key at /<m>/<branch>/<k>. Deposits to another address are returned
// by the next stray refund, and an address whose deposits cannot be built now never holds up another's. DB integration
// with the simulated node and the fake signer: not chain evidence (TestEscrowRegtestShapeBSiblingStray has the real Core
// leg).

// siblingKeysGo is Go's derivation of the three keys of order's sibling /branch/k: each party's account key's child
// <i>/branch/k and the signer's <m>/branch/k, by party.
func (x *escrowEnv) siblingKeysGo(order string, signer bip32Key, branch, k uint32) map[string][]byte {
	x.t.Helper()
	rows := x.escrowRows(order)
	out := map[string][]byte{}
	for party, expr := range map[string]string{"buyer": x.buyerKey, "vendor": x.vendorKey} {
		steps, _ := parseOriginPath(rows[party].suffix)
		child, _ := perOrderChild(x.t, accountKey(x.t, expr), steps[0])
		leaf := siblingKey(x.t, child, branch, k)
		out[party] = slices.Clone(leaf.PubKey[:])
	}
	child, _ := perOrderChild(x.t, signer, uint32(x.marketIndex(order)))
	leaf := siblingKey(x.t, child, branch, k)
	out["market"] = slices.Clone(leaf.PubKey[:])
	return out
}

// strayRefunds is order's stray refunds: id and state, oldest first.
func (x *escrowEnv) strayRefunds(order string) (ids []int64, states []string) {
	x.t.Helper()
	rows, err := x.DB.Query("SELECT id,state FROM settlements WHERE order_id=$1 AND kind='stray_refund' ORDER BY id", order)
	if err != nil {
		x.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var s string
		rows.Scan(&id, &s)
		ids, states = append(ids, id), append(states, s)
	}
	return ids, states
}

func TestEscrowShapeBSiblingStrayRefund(t *testing.T) {
	x, f := newPerOrderEnv(t)
	order := x.completed(regtestAddr("sibstray-refund"), 100000)
	sib := x.sibling(order, 0, 3)
	want := x.siblingKeysGo(order, f.key, 0, 3)
	x.core.deposit(sib, txidOf("sibstray"), 1, 30000, 6)
	x.poll() // recorded and the stray refund requested
	x.poll() // built
	ids, states := x.strayRefunds(order)
	if len(ids) != 1 || states[0] != "awaiting_signature" {
		var class string
		if len(ids) > 0 {
			x.DB.QueryRow("SELECT build_error FROM settlements WHERE id=$1", ids[0]).Scan(&class)
		}
		t.Fatalf("stray refunds %v %v (%s), want one awaiting a signature", ids, states, class)
	}
	id := ids[0]
	var stored string
	x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored)
	sp, _ := decodePSBT(stored)
	ws := sortedMultisigScript(want["buyer"], want["vendor"], want["market"])
	if len(sp.Inputs) != 1 || !bytes.Equal(sp.Inputs[0].WitnessScript, ws) || !bytes.Equal(sp.Inputs[0].WitnessUTXO.Script, p2wshScript(want["buyer"], want["vendor"], want["market"])) {
		t.Fatal("the stray refund's input does not carry the sibling's witness script")
	}
	bare := withChecksum("wsh(sortedmulti(2," + hex.EncodeToString(want["buyer"]) + "," + hex.EncodeToString(want["vendor"]) + "," + hex.EncodeToString(want["market"]) + "))")
	if x.core.countWith("utxoupdatepsbt", bare) != 1 {
		t.Fatal("utxoupdatepsbt was not given the sibling's bare-key descriptor")
	}
	// The buyer's copy names the buyer's own sibling key /<i>/0/3; the buyer's wallet signs it.
	sess, fp := x.partySess(roleBuyer)
	cp, _ := decodePSBT(x.partyCopy(order, sess, id))
	steps, _ := parseOriginPath(x.escrowRows(order)["buyer"].suffix)
	if d := cp.Inputs[0].Derivations; len(d) != 1 || !bytes.Equal(d[0].PubKey, want["buyer"]) || hex.EncodeToString(d[0].Fingerprint[:]) != fp ||
		!slices.Equal(d[0].Path[len(d[0].Path)-3:], []uint32{steps[0], 0, 3}) {
		t.Fatalf("the buyer's copy carries %+v", d)
	}
	x.signAs(order, roleBuyer, id)
	if got := agStrings(t, x.testEnv, "SELECT pubkey FROM settlement_signatures WHERE settlement_id=$1", id); len(got) != 1 || got[0] != hex.EncodeToString(want["buyer"]) {
		t.Fatalf("stored signatures %v", got)
	}
	n := len(f.signRequests())
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || r.Cosigned != "market" {
		t.Fatalf("after the pass: %+v (signer refusals %v)", r, f.refused)
	}
	if reqs := f.signRequests()[n:]; len(reqs) != 1 || reqs[0]["index"] != float64(x.marketIndex(order)) || reqs[0]["psbt"] != stored {
		t.Fatalf("sign requests %v", reqs)
	}
}

// Deposits to two addresses of one per-order escrow after its order closed (one to the escrow address, one to a sibling)
// are returned by two stray refunds, one script each; while the sibling's deposit cannot be built (the node reports it
// other than recorded), the escrow address's is built and returned, and the sibling's after it.
func TestEscrowShapeBStrayOneScriptEach(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	order := x.completed(regtestAddr("sibtwo-refund"), 100000)
	var addr string
	x.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr)
	sib := x.sibling(order, 1, 4)
	x.core.deposit(sib, txidOf("sibtwo-sib"), 0, 40000, 6)
	x.core.deposit(addr, txidOf("sibtwo-main"), 0, 50000, 6)
	// The sibling's deposit is reported at another address for a while: the stray refund must not wait on it.
	x.core.mu.Lock()
	x.core.utxoSkew = func(out map[string]any) {
		if sp, _ := out["scriptPubKey"].(map[string]any); sp["address"] == sib {
			sp["address"] = regtestAddr("elsewhere")
		}
	}
	x.core.mu.Unlock()
	x.poll()
	x.poll()
	ids, states := x.strayRefunds(order)
	if len(ids) != 1 || states[0] != "awaiting_signature" {
		t.Fatalf("stray refunds %v %v", ids, states)
	}
	if got := agStrings(t, x.testEnv, "SELECT address FROM payments WHERE settlement_id=$1", ids[0]); !slices.Equal(got, []string{addr}) {
		t.Fatalf("the first stray refund spends %v, want only the escrow address's deposit", got)
	}
	x.signAs(order, roleVendor, ids[0])
	x.poll()
	x.core.mu.Lock()
	x.core.utxoSkew = nil
	x.core.mu.Unlock()
	for _, txid := range agStrings(t, x.testEnv, "SELECT txid FROM settlements WHERE id=$1", ids[0]) {
		x.core.setTxConfs(txid, 6, "")
	}
	x.poll()
	x.poll()
	x.poll()
	ids, states = x.strayRefunds(order)
	if len(ids) != 2 || states[0] != "confirmed" || states[1] != "awaiting_signature" {
		t.Fatalf("stray refunds %v %v, want the first confirmed and the sibling's built", ids, states)
	}
	if got := agStrings(t, x.testEnv, "SELECT address FROM payments WHERE settlement_id=$1", ids[1]); !slices.Equal(got, []string{sib}) {
		t.Fatalf("the second stray refund spends %v, want the sibling's deposit", got)
	}
	x.signAs(order, roleBuyer, ids[1])
	x.poll()
	if r := x.settleByID(ids[1]); r.State != "broadcast" {
		t.Fatalf("the sibling's stray refund: %+v", r)
	}
}

// settlementKeys refuses a settlement whose claimed deposits are at two addresses: no build makes one.
func TestEscrowSettlementKeysOneScript(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	order := x.completed(regtestAddr("sibmix-refund"), 100000)
	x.core.deposit(x.sibling(order, 0, 2), txidOf("sibmix"), 0, 20000, 6)
	x.poll()
	var sid int64
	if err := x.DB.QueryRow("SELECT id FROM settlements WHERE order_id=$1 AND kind='stray_refund'", order).Scan(&sid); err != nil { // the sibling deposit's stray refund
		t.Fatal(err)
	}
	agExec(x.testEnv, "UPDATE payments SET settlement_id=$1 WHERE order_id=$2", sid, order)
	if _, err := x.A.settlementKeys(context.Background(), x.DB, order, sid); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("a settlement spending two scripts: %v", err)
	}
	agExec(x.testEnv, "UPDATE payments SET settlement_id=NULL WHERE order_id=$1 AND address<>(SELECT address FROM escrows WHERE order_id=$1)", order)
	keys, err := x.A.settlementKeys(context.Background(), x.DB, order, sid)
	if err != nil || keys["market"].Suffix != "/"+strconv.FormatInt(x.marketIndex(order), 10)+"/0/0" {
		t.Fatalf("the escrow address's keys: %v %+v", err, keys["market"])
	}
	agExec(x.testEnv, "UPDATE payments SET settlement_id=CASE WHEN address=(SELECT address FROM escrows WHERE order_id=$2) THEN NULL ELSE $1::bigint END WHERE order_id=$2", sid, order)
	if keys, err = x.A.settlementKeys(context.Background(), x.DB, order, sid); err != nil || keys["market"].Suffix != "/"+strconv.FormatInt(x.marketIndex(order), 10)+"/0/2" {
		t.Fatalf("the sibling's keys: %v %+v", err, keys["market"])
	}
}
