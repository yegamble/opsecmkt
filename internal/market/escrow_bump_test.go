package market

import (
	"context"
	"encoding/hex"
	"html"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Escrow Phase 2, slice P2-d: same-recipient fee bumps of a broadcast settlement (P2-R7, A-375), driven through the
// HTTP forms and the escrow watcher against the fake escrow Core (simulated RPC, not chain evidence:
// TestEscrowRegtestBump is the real-chain leg).

type bumpRow struct {
	ID                                                  int64
	N                                                   int
	State, Signer, TxID, Raw, Cosigned, Error, BuildErr string
	Fee, Rate                                           int64
}

func (x *escrowEnv) bumpByID(id int64) bumpRow {
	x.t.Helper()
	r := bumpRow{ID: id}
	if err := x.DB.QueryRow(`SELECT n,state,COALESCE(signer,''),COALESCE(txid,''),COALESCE(raw_tx,''),COALESCE(cosigned,''),send_error,build_error,COALESCE(network_fee,0),fee_rate
		FROM settlement_bumps WHERE id=$1`, id).Scan(&r.N, &r.State, &r.Signer, &r.TxID, &r.Raw, &r.Cosigned, &r.Error, &r.BuildErr, &r.Fee, &r.Rate); err != nil {
		x.t.Fatal(err)
	}
	return r
}

// lastBump is the latest fee bump of settlement sid.
func (x *escrowEnv) lastBump(sid int64) bumpRow {
	x.t.Helper()
	var id int64
	if err := x.DB.QueryRow("SELECT id FROM settlement_bumps WHERE settlement_id=$1 ORDER BY n DESC LIMIT 1", sid).Scan(&id); err != nil {
		x.t.Fatalf("no fee bump of settlement %d: %v", sid, err)
	}
	return x.bumpByID(id)
}

// parentRow is every column of a settlement a fee bump must never change (P2-R7), as one string.
func (x *escrowEnv) parentRow(id int64) string {
	x.t.Helper()
	var s string
	if err := x.DB.QueryRow(`SELECT concat_ws('|',kind,psbt,txid,raw_tx,cosigned,final_at,signer,signed_at,fee_rate,network_fee,input_total,destination,broadcast_at)
		FROM settlements WHERE id=$1`, id).Scan(&s); err != nil {
		x.t.Fatal(err)
	}
	return s + "|" + strings.Join(agStrings(x.t, x.testEnv, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id), ",")
}

// broadcastClaim is a vendor's claim of a completed escrow order funded with sats, signed by the vendor, co-signed and
// broadcast at the default fee rate (2 sat/vB): the order and the settlement.
func (x *escrowEnv) broadcastClaim(label string, sats ...int64) (string, int64) {
	x.t.Helper()
	order, id := x.built(regtestAddr("refund-"+label), regtestAddr("claim-"+label), sats...)
	x.signAs(order, roleVendor, id)
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" {
		x.t.Fatalf("%s: %+v", label, r)
	}
	return order, id
}

// broadcastRefund is the refund of an underpaid escrow order (sats) its buyer cancelled, signed by the buyer, co-signed
// and broadcast: the order and the settlement.
func (x *escrowEnv) broadcastRefund(label string, sats int64) (string, int64) {
	x.t.Helper()
	order := x.draft()
	x.mustCheckout(order, regtestAddr("refund-"+label))
	x.fund(order, order, 3, sats)
	w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, "amount_seen": {strconv.FormatInt(sats, 10)}, "payout_confirmed": {"confirmed"}})
	if w.Code != 303 || x.state(order) != stateCancelled {
		x.t.Fatalf("%s: cancel %d %s", label, w.Code, x.state(order))
	}
	x.poll()
	id, _, _, _ := x.settlement(order)
	x.signAs(order, roleBuyer, id)
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" {
		x.t.Fatalf("%s: %+v", label, r)
	}
	return order, id
}

func (x *escrowEnv) requestBump(order, sess string, id int64, rate string) (int, string) {
	w := x.do("POST", "/escrow/bump", sess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "fee_rate": {rate}, "password": {testPassword}})
	return w.Code, w.Body.String()
}

// mustBump requests a fee bump at rate as the claimant and runs a watcher pass, which builds it.
func (x *escrowEnv) mustBump(order, sess string, id int64, rate string) bumpRow {
	x.t.Helper()
	if code, body := x.requestBump(order, sess, id, rate); code != 303 {
		x.t.Fatalf("bump at %s: %d %s", rate, code, body)
	}
	x.poll()
	b := x.lastBump(id)
	if b.State != "awaiting_signature" {
		x.t.Fatalf("bump at %s after a pass: %+v", rate, b)
	}
	return b
}

func (x *escrowEnv) bumpCopy(order, sess string, sid, bid int64) (int, string) {
	w := x.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(sid, 10)+"&b="+strconv.FormatInt(bid, 10), sess, nil)
	return w.Code, strings.TrimSpace(w.Body.String())
}

func (x *escrowEnv) signBump(order, sess string, bid int64, psbt string) (int, string) {
	w := x.do("POST", "/escrow/bump-sign", sess, url.Values{"order_id": {order}, "bump_id": {strconv.FormatInt(bid, 10)}, "psbt": {psbt}})
	return w.Code, refusalText(w.Body.String())
}

// signBumpAs pastes party's good signature of the fee bump (its wallet signing its own downloaded copy).
func (x *escrowEnv) signBumpAs(order, party string, sid, bid int64) {
	x.t.Helper()
	sess, fp := x.partySess(party)
	code, cp := x.bumpCopy(order, sess, sid, bid)
	if code != 200 {
		x.t.Fatalf("%s's copy of the fee bump: %d", party, code)
	}
	if code, body := x.signBump(order, sess, bid, walletSign(x.t, cp, fp, 1)); code != 303 {
		x.t.Fatalf("%s signs the fee bump: %d %s", party, code, body)
	}
}

// bumpPSBT is the stored PSBT of a fee bump.
func (x *escrowEnv) bumpPSBT(bid int64) string {
	x.t.Helper()
	var s string
	if err := x.DB.QueryRow("SELECT psbt FROM settlement_bumps WHERE id=$1", bid).Scan(&s); err != nil {
		x.t.Fatal(err)
	}
	return s
}

// P2-R7: only the claimant asks for a fee bump and signs it first (the vendor of a release, the buyer of a refund);
// then the market co-signs it with the claimant's signature, or the other party signs it too and the market does not.
// The other party's signature alone is refused before anything is stored, so a fee bump is never completed by the
// other party and the market; ES-2's paste checks apply to the bump's own transaction; the settlement's row never
// changes.
func TestEscrowBumpClaimantOnly(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.broadcastClaim("bco", 100000)
	parent := x.parentRow(id)
	if code, body := x.requestBump(order, x.buyerSess, id, "5"); code != 403 || !strings.Contains(body, "Only the vendor raises the network fee") {
		t.Fatalf("the buyer asked for a fee bump of the vendor's claim: %d %s", code, body)
	}
	if strings.Contains(x.page("/order?id="+order, x.buyerSess), `action="/escrow/bump"`) || !strings.Contains(x.page("/order?id="+order, x.vendorSess), `action="/escrow/bump"`) {
		t.Fatal("the fee bump form is offered to the wrong party")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM settlement_bumps") != 0 {
		t.Fatal("a refused request stored a fee bump")
	}
	b := x.mustBump(order, x.vendorSess, id, "5")
	if b.N != 1 || b.Rate != 5 || b.Fee != 5*(11+105+43) || b.Signer != "" || b.Raw != "" {
		t.Fatalf("built: %+v", b)
	}
	if outs := agStrings(t, x.testEnv, "SELECT role||' '||sats FROM settlement_outputs WHERE settlement_id=$1", id); !slices.Equal(outs, []string{"claimant 99682"}) {
		t.Fatalf("the settlement's outputs changed: %q", outs)
	}
	if page := html.UnescapeString(x.page("/order?id="+order, x.vendorSess)); !strings.Contains(page, "99205 sat") && !strings.Contains(page, "0.00099205 BTC") {
		t.Fatalf("the vendor's page does not show the bump's smaller claimant output:\n%s", page)
	}
	// The other party's copy is not served, and its signature is refused, while the claimant has not signed.
	if code, _ := x.bumpCopy(order, x.buyerSess, id, b.ID); code != 404 {
		t.Fatalf("the buyer was served a copy of a fee bump the vendor has not signed: %d", code)
	}
	keys, err := escrowKeys(context.Background(), x.DB, order)
	if err != nil {
		t.Fatal(err)
	}
	buyerCopy, err := settlementCopy(x.bumpPSBT(b.ID), keys[roleBuyer])
	if err != nil {
		t.Fatal(err)
	}
	_, bfp := x.partySess(roleBuyer)
	buyerSigned := walletSign(t, buyerCopy, bfp, 1)
	signs := x.core.count("walletprocesspsbt")
	if code, body := x.signBump(order, x.buyerSess, b.ID, buyerSigned); code != 409 || !strings.Contains(body, "The vendor signs a fee bump of the vendor's claim first") {
		t.Fatalf("the buyer signed the vendor's fee bump first: %d %s", code, body)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM settlement_bump_signatures") != 0 || x.bumpByID(b.ID).State != "awaiting_signature" {
		t.Fatal("a refused signature was stored")
	}
	x.poll()
	if x.core.count("walletprocesspsbt") != signs || x.bumpByID(b.ID).State != "awaiting_signature" {
		t.Fatal("the market signed a fee bump nobody (or only the other party) signed")
	}
	// ES-2's checks on the bump's own transaction, each refused before anything is stored.
	vsess, vfp := x.partySess(roleVendor)
	_, vcopy := x.bumpCopy(order, vsess, id, b.ID)
	vp, _ := decodePSBT(vcopy)
	vendorPub, _ := hex.DecodeString(x.keyHex(order, roleVendor))
	var parentPSBT string
	x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&parentPSBT)
	parentCopy, _ := settlementCopy(parentPSBT, keys[roleVendor])
	forged := walletSign(t, vcopy, vfp, 1)
	fp, _ := decodePSBT(forged)
	withMarket := encodePSBT(addField(t, fp.Raw, 0, append([]byte{0x02}, keys["market"].PubKey...), fakeSig(keys["market"].PubKey, vp.TxID(), 1)))
	for name, c := range map[string]struct{ paste, want string }{
		"sighash NONE":            {walletSign(t, vcopy, vfp, 2), "sighash type other than ALL"},
		"the buyer's signature":   {buyerSigned, "signature from a key other than your escrow key"},
		"the settlement's own tx": {walletSign(t, parentCopy, vfp, 1), "not the settlement transaction shown on this page"},
		"a market-key entry":      {withMarket, "signature under the market's key"},
		"unsigned":                {vcopy, "does not carry your signature on every input"},
	} {
		if code, body := x.signBump(order, vsess, b.ID, c.paste); code != 400 || !strings.Contains(html.UnescapeString(body), c.want) {
			t.Fatalf("%s: %d %s", name, code, body)
		}
	}
	if agInt(x.testEnv, "SELECT count(*) FROM settlement_bump_signatures") != 0 || x.bumpByID(b.ID).State != "awaiting_signature" {
		t.Fatal("a refused paste stored something")
	}
	good := walletSign(t, vcopy, vfp, 1)
	if code, body := x.signBump(order, vsess, b.ID, good); code != 303 {
		t.Fatalf("the vendor signs: %d %s", code, body)
	}
	if r := x.bumpByID(b.ID); r.State != "party_signed" || r.Signer != roleVendor {
		t.Fatalf("after the vendor's signature: %+v", r)
	}
	assertNowhereInDB(t, x.testEnv, good)
	if agInt(x.testEnv, "SELECT count(*) FROM settlement_bump_signatures WHERE bump_id=$1 AND party='vendor' AND pubkey=$2", b.ID, hex.EncodeToString(vendorPub)) != 1 {
		t.Fatal("the vendor's signature was not kept")
	}
	if code, _ := x.bumpCopy(order, x.buyerSess, id, b.ID); code != 200 {
		t.Fatalf("once the vendor signed, the buyer may sign too: copy %d", code)
	}
	// The market co-signs the bump's own market copy with the vendor's signature, checks, stores and broadcasts it.
	x.poll()
	r := x.bumpByID(b.ID)
	if r.State != "broadcast" || r.Cosigned != "market" || r.Raw == "" || x.core.count("walletprocesspsbt") != signs+1 {
		t.Fatalf("after the pass: %+v", r)
	}
	marketCopy, _ := settlementCopy(x.bumpPSBT(b.ID), keys["market"])
	if path, params := x.core.lastCall(t, "walletprocesspsbt"); path != "/wallet/opsecmkt-arbiter" || params != `["`+marketCopy+`",true,"ALL",true,false]` {
		t.Fatalf("walletprocesspsbt %s %s", path, params)
	}
	if x.parentRow(id) != parent || x.settleByID(id).State != "broadcast" {
		t.Fatal("the fee bump changed the settlement's row")
	}

	// A refund: the buyer is the claimant. The vendor may neither ask nor sign first; both signing completes it without
	// the market.
	orderR, idR := x.broadcastRefund("bcor", 40000)
	if code, body := x.requestBump(orderR, x.vendorSess, idR, "5"); code != 403 || !strings.Contains(body, "Only the buyer raises the network fee") {
		t.Fatalf("the vendor asked for a fee bump of the buyer's refund: %d %s", code, body)
	}
	bR := x.mustBump(orderR, x.buyerSess, idR, "5")
	vendorCopy, _ := settlementCopy(x.bumpPSBT(bR.ID), keys[roleVendor])
	if code, body := x.signBump(orderR, x.vendorSess, bR.ID, walletSign(t, vendorCopy, vfp, 1)); code != 409 || !strings.Contains(body, "The buyer signs a fee bump of a refund to the buyer first") {
		t.Fatalf("the vendor signed the buyer's fee bump first: %d %s", code, body)
	}
	x.signBumpAs(orderR, roleBuyer, idR, bR.ID)
	x.signBumpAs(orderR, roleVendor, idR, bR.ID)
	signs = x.core.count("walletprocesspsbt")
	x.poll()
	if r := x.bumpByID(bR.ID); r.State != "broadcast" || r.Cosigned != "parties" || x.core.count("walletprocesspsbt") != signs {
		t.Fatalf("both parties signed the fee bump: %+v, market signs %d", r, x.core.count("walletprocesspsbt")-signs)
	}

	// The claimant's signature does not verify and the other party's does: the market never completes it with the
	// other party's. Both are discarded and the bump waits for the claimant's signature again.
	order3, id3 := x.broadcastClaim("bco3", 100000)
	b3 := x.mustBump(order3, x.vendorSess, id3, "4")
	_, cp3 := x.bumpCopy(order3, vsess, id3, b3.ID)
	p3, _ := decodePSBT(cp3)
	pub3, _ := hex.DecodeString(x.keyHex(order3, roleVendor))
	bad := encodePSBT(addField(t, p3.Raw, 0, append([]byte{0x02}, pub3...), fakeSig(pub3, "amount-0.0009:"+p3.TxID(), 1)))
	if code, body := x.signBump(order3, vsess, b3.ID, bad); code != 303 {
		t.Fatalf("a well-formed bad signature: %d %s", code, body)
	}
	x.signBumpAs(order3, roleBuyer, id3, b3.ID)
	x.poll()
	if r := x.bumpByID(b3.ID); r.State != "awaiting_signature" || r.Raw != "" || r.Signer != "" ||
		agInt(x.testEnv, "SELECT count(*) FROM settlement_bump_signatures WHERE bump_id=$1", b3.ID) != 0 {
		t.Fatalf("a fee bump whose claimant signature did not verify: %+v", r)
	}
	if !slices.ContainsFunc(x.events(order3), func(ev string) bool {
		return strings.Contains(ev, "The vendor's signature on the fee bump") && strings.Contains(ev, "did not verify")
	}) {
		t.Fatalf("no note of the discarded signature: %q", x.events(order3))
	}
}

// A-375: the watcher's own transactions are the settlement's and every stored fee bump of it. Once a fee bump is
// broadcast, only it (the highest fee) is sent again, never the settlement's original transaction; a conflict between
// the market's own transactions is not a spend outside the market; when the bump confirms, the settlement is confirmed
// with the bump's transaction recorded on the bump, and a reorganisation follows the bump's transaction. A transaction
// outside that set still makes the settlement external.
func TestEscrowBumpConfirmsNotExternal(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.broadcastClaim("bne", 100000)
	parent := x.settleByID(id)
	before := x.parentRow(id)
	b := x.mustBump(order, x.vendorSess, id, "5")
	x.signBumpAs(order, roleVendor, id, b.ID)
	x.poll() // the original is sent again early in this pass, before the bump is co-signed and broadcast at its end
	b = x.bumpByID(b.ID)
	if b.State != "broadcast" || x.sendsOf(b.Raw) != 1 {
		t.Fatalf("the fee bump was not broadcast: %+v", b)
	}
	sentParent := x.sendsOf(parent.Raw)
	// Unconfirmed: only the highest-fee stored transaction is sent again, never the original.
	x.poll()
	x.poll()
	if x.sendsOf(b.Raw) != 3 || x.sendsOf(parent.Raw) != sentParent {
		t.Fatalf("after the bump: bump sent %d times, original %d more times", x.sendsOf(b.Raw), x.sendsOf(parent.Raw)-sentParent)
	}
	// The original conflicted by the market's own bump (the wallet saw the block before the bump's confirmations): it
	// waits; it is not external.
	x.core.setTxConfs(parent.TxID, -1, "")
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1 AND flagged", id) != 0 {
		t.Fatalf("a conflict with the market's own fee bump: %+v", r)
	}
	x.core.setTxConfs(b.TxID, 3, "")
	x.core.setTxConfs(parent.TxID, -3, "")
	x.poll()
	r := x.settleByID(id)
	if r.State != "confirmed" || x.bumpByID(b.ID).State != "confirmed" || r.SpentBy != "" {
		t.Fatalf("the bump at 3 confirmations: settlement %+v, bump %+v", r, x.bumpByID(b.ID))
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1 AND flagged", id) != 0 ||
		slices.ContainsFunc(x.events(order), func(ev string) bool { return strings.Contains(ev, "did not make") }) {
		t.Fatal("the confirmed fee bump was recorded as a spend outside the market")
	}
	if x.parentRow(id) != before {
		t.Fatal("the settlement's frozen columns changed")
	}
	page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
	if !strings.Contains(page, "Confirmed on the regtest network: transaction "+b.TxID) || !strings.Contains(page, b.Raw) {
		t.Fatalf("the confirmed page does not name the bump's transaction:\n%s", page)
	}
	// A reorganisation takes the bump out of the chain: back to broadcast, and the bump (not the original) is sent again.
	x.core.setTxConfs(b.TxID, 0, "")
	x.core.setTxConfs(parent.TxID, 0, "")
	sentParent = x.sendsOf(parent.Raw)
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || x.bumpByID(b.ID).State != "broadcast" {
		t.Fatalf("after the reorganisation: %+v %+v", r, x.bumpByID(b.ID))
	}
	x.poll()
	if x.sendsOf(parent.Raw) != sentParent {
		t.Fatal("the original was sent again after the reorganisation")
	}
	x.core.setTxConfs(b.TxID, 3, "")
	x.poll()
	if r := x.settleByID(id); r.State != "confirmed" {
		t.Fatalf("confirmed again: %+v", r)
	}

	// A transaction that is neither the original nor a stored bump still makes the settlement external.
	order2, id2 := x.broadcastClaim("bne2", 100000)
	p2 := x.settleByID(id2)
	b2 := x.mustBump(order2, x.vendorSess, id2, "5")
	x.signBumpAs(order2, roleVendor, id2, b2.ID)
	x.poll()
	b2 = x.bumpByID(b2.ID)
	outside := txidOf("outside-" + order2)
	x.core.setTxConfs(p2.TxID, -2, outside)
	x.core.setTxConfs(b2.TxID, -2, outside)
	x.poll()
	if r := x.settleByID(id2); r.State != "external" || r.SpentBy != outside {
		t.Fatalf("an outside transaction mined in place of both: %+v", r)
	}
}

// P2-R7's shape and fee rules, at the request (fee rates, the caps, the floor, a settlement not broadcast), in the
// build and finalize check (checkBump: inputs, recipients, the market fee and the buyer's return unchanged, the BIP125
// increment, the caps, the floor), and at finalize (the settlement must still be unconfirmed).
func TestEscrowBumpShapeRefusals(t *testing.T) {
	x := newEscrowEnv(t)
	t.Setenv("ESCROW_BTC_FEE_BPS", "1500")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", regtestAddr("market-fee-bsr"))
	x.restart()
	order, id := x.broadcastClaim("bsr", 70000, 50000)
	for rate, want := range map[string]string{
		"2":   "higher than the 2 sat/vB",
		"1":   "higher than the 2 sat/vB",
		"0":   "whole number of sat/vB, at least 1",
		"x":   "whole number of sat/vB, at least 1",
		"101": "whole number of sat/vB, at least 1",
	} {
		if code, body := x.requestBump(order, x.vendorSess, id, rate); code != 400 || !strings.Contains(html.UnescapeString(body), want) {
			t.Fatalf("rate %s: %d %s", rate, code, body)
		}
	}
	if agInt(x.testEnv, "SELECT count(*) FROM settlement_bumps") != 0 {
		t.Fatal("a refused request stored a fee bump")
	}
	// The pure check, on the settlement's real transaction: a valid bump at 3 sat/vB, then each way a bump may not differ.
	var stored string
	x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored)
	sp, _ := decodePSBT(stored)
	var outs []settleOutput
	for _, r := range agStrings(t, x.testEnv, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id) {
		f := strings.Fields(r)
		n, _ := strconv.ParseInt(f[2], 10, 64)
		outs = append(outs, settleOutput{f[0], f[1], n})
	}
	if len(outs) != 3 || outs[0].Role != "claimant" || outs[1].Role != "market_fee" || outs[2].Role != "buyer_return" {
		t.Fatalf("outputs %+v", outs)
	}
	const vsize, parentFee, total = 11 + 2*105 + 3*43, 2 * (11 + 2*105 + 3*43), 120000
	mut := func(f func(tx *psbtTx)) psbtTx {
		c := sp.Tx
		c.In = slices.Clone(c.In)
		c.Out = slices.Clone(c.Out)
		for i := range c.Out {
			c.Out[i].Script = slices.Clone(c.Out[i].Script)
		}
		c.Out[0].Value -= vsize // 3 sat/vB
		f(&c)
		return c
	}
	if fee, class := checkBump("regtest", sp.Tx, mut(func(*psbtTx) {}), outs, total, parentFee, 10000); class != "" || fee != 3*vsize {
		t.Fatalf("a valid bump: %d %q", fee, class)
	}
	other, _ := escrowAddressScript("regtest", regtestAddr("elsewhere"))
	for name, c := range map[string]struct {
		f    func(*psbtTx)
		want string
	}{
		"claimant paid elsewhere":     {func(tx *psbtTx) { tx.Out[0].Script = other }, bumpRecipients},
		"market fee paid elsewhere":   {func(tx *psbtTx) { tx.Out[1].Script = other }, bumpRecipients},
		"buyer return paid elsewhere": {func(tx *psbtTx) { tx.Out[2].Script = other }, bumpRecipients},
		"outputs reordered":           {func(tx *psbtTx) { tx.Out[1], tx.Out[2] = tx.Out[2], tx.Out[1] }, bumpRecipients},
		"an output added":             {func(tx *psbtTx) { tx.Out = append(tx.Out, psbtTxOut{Value: 10000, Script: other}) }, bumpRecipients},
		"an output dropped":           {func(tx *psbtTx) { tx.Out = tx.Out[:2] }, bumpRecipients},
		"market fee shrunk":           {func(tx *psbtTx) { tx.Out[1].Value -= 1000 }, bumpOutputs},
		"buyer return shrunk":         {func(tx *psbtTx) { tx.Out[2].Value -= 1000 }, bumpOutputs},
		"market fee grown":            {func(tx *psbtTx) { tx.Out[1].Value += 100; tx.Out[0].Value -= 100 }, bumpOutputs},
		"claimant not smaller":        {func(tx *psbtTx) { tx.Out[0].Value += vsize }, bumpOutputs},
		"fee below the increment":     {func(tx *psbtTx) { tx.Out[0].Value += vsize - 100 }, bumpFeeLow},
		"fee above the rate cap":      {func(tx *psbtTx) { tx.Out[0].Value -= escrowMaxFeeRate * vsize }, settleFeeCap},
		"an input changed":            {func(tx *psbtTx) { tx.In[1].Vout = 7 }, bumpInputs},
		"inputs reordered":            {func(tx *psbtTx) { tx.In[0], tx.In[1] = tx.In[1], tx.In[0] }, bumpInputs},
		"an input dropped":            {func(tx *psbtTx) { tx.In = tx.In[:1] }, bumpInputs},
		"a sequence changed":          {func(tx *psbtTx) { tx.In[0].Sequence = 0xffffffff }, bumpInputs},
		"lock time changed":           {func(tx *psbtTx) { tx.LockTime = 1 }, bumpInputs},
		"version changed":             {func(tx *psbtTx) { tx.Version = 1 }, bumpInputs},
	} {
		if _, class := checkBump("regtest", sp.Tx, mut(c.f), outs, total, parentFee, 10000); class != c.want {
			t.Errorf("%s: %q, want %q", name, class, c.want)
		}
	}
	if _, class := checkBump("regtest", sp.Tx, mut(func(*psbtTx) {}), outs, total, 3*vsize, 10000); class != bumpFeeLow {
		t.Errorf("not above an earlier bump at the same rate: %q", class)
	}
	if _, class := checkBump("regtest", sp.Tx, mut(func(*psbtTx) {}), outs, total, parentFee, outs[0].Sats); class != settleTooSmall {
		t.Errorf("claimant below the floor: %q", class)
	}
	// The absolute cap: ten inputs at 88 sat/vB pay more than escrowMaxFee.
	orderC, idC := x.broadcastClaim("bsrc", 12000, 12000, 12000, 12000, 12000, 12000, 12000, 12000, 12000, 12000)
	if code, body := x.requestBump(orderC, x.vendorSess, idC, "88"); code != 400 || !strings.Contains(html.UnescapeString(body), "fee cap") {
		t.Fatalf("absolute fee cap: %d %s", code, body)
	}
	// The floor: a small refund cannot pay 70 sat/vB and keep the buyer's output at 0.0001 BTC.
	orderS, idS := x.broadcastRefund("bsrs", 20000)
	if code, body := x.requestBump(orderS, x.buyerSess, idS, "70"); code != 400 || !strings.Contains(html.UnescapeString(body), "below the minimum output") {
		t.Fatalf("floor: %d %s", code, body)
	}
	if code, body := x.requestBump(orderS, x.buyerSess, idS, "50"); code != 303 {
		t.Fatalf("a bump that keeps the floor: %d %s", code, body)
	}
	// Only a broadcast settlement is bumped: one waiting for a signature, or confirmed, is refused.
	orderW, idW := x.built(regtestAddr("refund-bsrw"), regtestAddr("claim-bsrw"), 100000)
	if code, body := x.requestBump(orderW, x.vendorSess, idW, "5"); code != 409 || !strings.Contains(body, "only while its transaction is broadcast and unconfirmed") {
		t.Fatalf("a settlement waiting for a signature: %d %s", code, body)
	}
	orderK, idK := x.confirmedSettlement("bsrk")
	if code, _ := x.requestBump(orderK, x.vendorSess, idK, "5"); code != 409 {
		t.Fatalf("a confirmed settlement: %d", code)
	}
	// A second request replaces an open one (one open bump per settlement); the next must beat the highest stored.
	b1 := x.mustBump(order, x.vendorSess, id, "4")
	b2 := x.mustBump(order, x.vendorSess, id, "6")
	if r := x.bumpByID(b1.ID); r.State != "superseded" || b2.N != 2 {
		t.Fatalf("the open bump was not replaced: %+v %+v", r, b2)
	}
	x.signBumpAs(order, roleVendor, id, b2.ID)
	x.poll()
	if r := x.bumpByID(b2.ID); r.State != "broadcast" {
		t.Fatalf("bump at 6: %+v", r)
	}
	if code, body := x.requestBump(order, x.vendorSess, id, "6"); code != 400 || !strings.Contains(html.UnescapeString(body), "higher than the 6 sat/vB") {
		t.Fatalf("not above the broadcast bump: %d %s", code, body)
	}
	// At finalize: once a stored transaction is in a block, a party-signed bump is not co-signed; when the settlement
	// confirms, the open bump is closed.
	b3 := x.mustBump(order, x.vendorSess, id, "8")
	x.signBumpAs(order, roleVendor, id, b3.ID)
	x.core.setTxConfs(x.bumpByID(b2.ID).TxID, 1, "")
	signs := x.core.count("walletprocesspsbt")
	x.poll()
	if r := x.bumpByID(b3.ID); r.State != "party_signed" || x.core.count("walletprocesspsbt") != signs {
		t.Fatalf("a bump of a settlement whose transaction is in a block was co-signed: %+v", r)
	}
	x.core.setTxConfs(x.bumpByID(b2.ID).TxID, 3, "")
	x.poll()
	if r := x.bumpByID(b3.ID); r.State != "superseded" || x.settleByID(id).State != "confirmed" || x.bumpByID(b2.ID).State != "confirmed" {
		t.Fatalf("after the settlement confirmed: %+v", r)
	}
}

// The restore gate and a restore's hold (P2-R7, ES-6): scripts/restore.sh and UPGRADING.md hold every restored fee bump
// with a stored transaction as they hold settlements; while the gate is set the watcher builds, co-signs and sends no
// fee bump; an administrator's confirmation of the settlement's rebroadcast releases its bumps' holds and the market
// then sends the highest-fee stored transaction once.
func TestEscrowBumpRestoreHold(t *testing.T) {
	sqlText := strings.ReplaceAll(restoredSettlementHold, "'", "''")
	for _, f := range []string{"scripts/restore.sh", "UPGRADING.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "UPDATE settlement_bumps SET send_error='"+sqlText+"'") ||
			!strings.Contains(string(b), "WHERE raw_tx IS NOT NULL AND state IN ('signed','broadcast')") {
			t.Errorf("%s does not hold restored fee bumps", f)
		}
	}
	x := newEscrowEnv(t)
	order, id := x.broadcastClaim("brh", 100000)
	order2, id2 := x.broadcastClaim("brh2", 100000) // before the restore: escrow checkout is refused while the gate is set
	parent := x.settleByID(id)
	b := x.mustBump(order, x.vendorSess, id, "5")
	x.signBumpAs(order, roleVendor, id, b.ID)
	x.poll()
	b = x.bumpByID(b.ID)
	if b.State != "broadcast" {
		t.Fatalf("%+v", b)
	}
	// The restore: the gate, and the holds exactly as restore.sh writes them.
	x.setGate(true)
	x.restoreHold(id)
	if _, err := x.DB.Exec("UPDATE settlement_bumps SET send_error=$1 WHERE raw_tx IS NOT NULL AND state IN ('signed','broadcast')", restoredSettlementHold); err != nil {
		t.Fatal(err)
	}
	sentB, sentP := x.sendsOf(b.Raw), x.sendsOf(parent.Raw)
	x.poll()
	if x.sendsOf(b.Raw) != sentB || x.sendsOf(parent.Raw) != sentP {
		t.Fatal("a held settlement or fee bump was sent while the gate was set")
	}
	// Bumps pause under the gate: a request is stored, but nothing is built, co-signed or sent.
	if code, body := x.requestBump(order2, x.vendorSess, id2, "5"); code != 303 {
		t.Fatalf("request under the gate: %d %s", code, body)
	}
	x.poll()
	if r := x.lastBump(id2); r.State != "building" {
		t.Fatalf("a fee bump was built while the gate was set: %+v", r)
	}
	if page := html.UnescapeString(x.page("/order?id="+order2, x.vendorSess)); !strings.Contains(page, "Paused: the market restored its records from a backup") {
		t.Fatalf("the paused bump is not explained:\n%s", page)
	}
	x.setGate(false)
	x.poll()
	b2 := x.lastBump(id2)
	if b2.State != "awaiting_signature" {
		t.Fatalf("after the gate: %+v", b2)
	}
	x.signBumpAs(order2, roleVendor, id2, b2.ID)
	x.setGate(true)
	signs := x.core.count("walletprocesspsbt")
	x.poll()
	if x.core.count("walletprocesspsbt") != signs || x.bumpByID(b2.ID).State != "party_signed" {
		t.Fatal("a fee bump was co-signed while the gate was set")
	}
	// The administrator confirms the held settlement's rebroadcast: its bumps' holds end, and the highest-fee stored
	// transaction (the bump) is sent once, not the original.
	_, admin := x.user("bradmin", "admin")
	page := x.page("/admin", admin)
	if !strings.Contains(page, `name="settlement_id" value="`+strconv.FormatInt(id, 10)+`"`) {
		t.Fatalf("the held settlement is not offered for confirmation on /admin:\n%s", page)
	}
	if code, body := x.rebroadcast(admin, id, url.Values{"not_sent": {"confirmed"}, "password": {testPassword}}); code != 303 {
		t.Fatalf("confirm: %d %s", code, body)
	}
	if r := x.bumpByID(b.ID); r.Error != "" {
		t.Fatalf("the bump is still held: %+v", r)
	}
	x.poll()
	x.poll()
	if x.sendsOf(b.Raw) != sentB+1 || x.sendsOf(parent.Raw) != sentP {
		t.Fatalf("after the confirmation: bump sent %d more, original %d more", x.sendsOf(b.Raw)-sentB, x.sendsOf(parent.Raw)-sentP)
	}
}

// Migration 080: fee bumps are child rows of a settlement with their own freeze and forward-only rules; the
// settlement's row never changes because of them.
func TestEscrowMigration080Bumps(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.broadcastClaim("m80", 100000)
	before := x.parentRow(id)
	exec := func(q string, args ...any) error { _, err := x.DB.Exec(q, args...); return err }
	// Requested only for a broadcast settlement, only as building and unbuilt, by a known user.
	_, idW := x.built(regtestAddr("refund-m80w"), regtestAddr("claim-m80w"), 100000)
	if exec("INSERT INTO settlement_bumps(settlement_id,n,requested_by,fee_rate) VALUES($1,1,$2,5)", idW, x.vendor) == nil {
		t.Fatal("a fee bump of a settlement waiting for a signature was stored")
	}
	if exec("INSERT INTO settlement_bumps(settlement_id,n,requested_by,fee_rate,psbt,txid,network_fee,state) VALUES($1,1,$2,5,'cHNidP8',$3,795,'awaiting_signature')", id, x.vendor, txidOf("m80")) == nil {
		t.Fatal("a fee bump was inserted already built")
	}
	b := x.mustBump(order, x.vendorSess, id, "5")
	if exec("INSERT INTO settlement_bumps(settlement_id,n,requested_by,fee_rate) VALUES($1,2,$2,6)", id, x.vendor) == nil {
		t.Fatal("a second open fee bump of one settlement was stored")
	}
	// Never rebuilt once built.
	for _, q := range []string{
		"UPDATE settlement_bumps SET psbt='cHNidP9' WHERE id=$1",
		"UPDATE settlement_bumps SET txid=repeat('ab',32) WHERE id=$1",
		"UPDATE settlement_bumps SET network_fee=1000 WHERE id=$1",
		"UPDATE settlement_bumps SET fee_rate=9 WHERE id=$1",
		"UPDATE settlement_bumps SET settlement_id=settlement_id+1 WHERE id=$1",
		"UPDATE settlement_bumps SET n=5 WHERE id=$1",
		"DELETE FROM settlement_bumps WHERE id=$1",
	} {
		if exec(q, b.ID) == nil {
			t.Fatalf("the database allowed %s", q)
		}
	}
	// The signer is the claimant (the vendor of a release); the other party's signature waits for the claimant's.
	if exec("UPDATE settlement_bumps SET state='party_signed',signer='buyer',signed_at=now() WHERE id=$1", b.ID) == nil {
		t.Fatal("the buyer was recorded as the signer of a fee bump of the vendor's claim")
	}
	buyerPub := x.keyHex(order, roleBuyer)
	sig := hex.EncodeToString(fakeSig([]byte{2}, b.TxID, 1))
	if exec("INSERT INTO settlement_bump_signatures(bump_id,input_n,party,pubkey,signature) VALUES($1,0,'buyer',$2,$3)", b.ID, buyerPub, sig) == nil {
		t.Fatal("the other party's signature was stored before the claimant's")
	}
	if exec("UPDATE settlement_bumps SET raw_tx='00',cosigned='market',final_at=now(),state='signed' WHERE id=$1", b.ID) == nil {
		t.Fatal("a final transaction was stored on a bump nobody signed")
	}
	x.signBumpAs(order, roleVendor, id, b.ID)
	if exec("UPDATE settlement_bump_signatures SET signature=$2 WHERE bump_id=$1", b.ID, sig) == nil {
		t.Fatal("a kept signature was changed")
	}
	x.poll()
	b = x.bumpByID(b.ID)
	if b.State != "broadcast" {
		t.Fatalf("%+v", b)
	}
	for _, q := range []string{
		"UPDATE settlement_bumps SET raw_tx='00' WHERE id=$1",
		"UPDATE settlement_bumps SET cosigned='parties' WHERE id=$1",
		"UPDATE settlement_bumps SET signer=NULL WHERE id=$1",
		"UPDATE settlement_bumps SET state='signed' WHERE id=$1",
		"UPDATE settlement_bumps SET state='superseded' WHERE id=$1",
		"UPDATE settlement_bumps SET state='awaiting_signature' WHERE id=$1",
		"DELETE FROM settlement_bump_signatures WHERE bump_id=$1",
	} {
		if exec(q, b.ID) == nil {
			t.Fatalf("the database allowed %s on a broadcast fee bump", q)
		}
	}
	if err := exec("UPDATE settlement_bumps SET state='confirmed',confirmed_at=now() WHERE id=$1", b.ID); err != nil {
		t.Fatalf("broadcast -> confirmed: %v", err)
	}
	if err := exec("UPDATE settlement_bumps SET state='broadcast',confirmed_at=NULL WHERE id=$1", b.ID); err != nil {
		t.Fatalf("confirmed -> broadcast (a reorganisation): %v", err)
	}
	if x.parentRow(id) != before {
		t.Fatal("the settlement's row changed")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM pg_indexes WHERE indexname IN ('settlement_bumps_one_open','settlement_bumps_one_confirmed')") != 2 {
		t.Fatal("the one-open or one-confirmed index is missing")
	}
}
