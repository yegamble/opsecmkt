package market

import (
	"context"
	"database/sql"
	"encoding/hex"
	"html"
	"strconv"
	"strings"
	"testing"
)

// QA5W5A-1 (chair's ruling): checkout stores the market's order wallet key (the escrow signer's account key's child <m>,
// public data) beside the buyer's and the vendor's, so nothing but co-signing needs the escrow signer: while it is down a
// sibling deposit's stray refund is still shown, downloaded and signed, and only its co-signature waits (signer-waiting,
// nothing rebuilt). A per-order key that cannot be had (a row with no stored key, or stored data that does not give the
// address) never turns the order page, the download or the sign form into a 500: the page says why and offers no copy to
// sign, the watcher records signer-waiting, and the log names a fixed class. DB integration with the simulated node and the
// fake signer: not chain evidence (TestEscrowRegtestShapeBSiblingStraySignerDown is the real Core leg).

// keysUnavailableText is what the order page and the sign form say about a settlement whose keys cannot be had.
const keysUnavailableText = "cannot be signed right now"

// siblingStray is a completed per-order escrow order with a deposit to its sibling /0/<k>, and that deposit's stray
// refund, built and awaiting a signature.
func (x *escrowEnv) siblingStray(label string, k int) (string, int64) {
	x.t.Helper()
	order := x.completed(regtestAddr(label+"-refund"), 100000)
	x.core.deposit(x.sibling(order, 0, k), txidOf(label), 0, 30000, 6)
	x.poll() // recorded and the stray refund requested
	x.poll() // built
	ids, states := x.strayRefunds(order)
	if len(ids) != 1 || states[0] != "awaiting_signature" {
		x.t.Fatalf("stray refunds %v %v, want one awaiting a signature", ids, states)
	}
	return order, ids[0]
}

// hasSignForm reports whether page offers the settlement sign form.
func hasSignForm(page string) bool { return strings.Contains(page, `action="/escrow/sign"`) }

func TestEscrowShapeBSiblingStraySignerDown(t *testing.T) {
	logs := captureLog(t)
	x, f := newPerOrderEnv(t)
	order, id := x.siblingStray("sibdown", 2)
	var psbt string
	x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&psbt)
	f.stop()
	// Both parties' order pages, with the sign form; the copy; the paste.
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		if page := x.page("/order?id="+order, sess); !hasSignForm(page) {
			t.Fatalf("signer down: no sign form on the order page:\n%s", page)
		}
	}
	sess, fp := x.partySess(roleBuyer)
	signed := walletSign(t, x.partyCopy(order, sess, id), fp, 1)
	if code, body := x.sign(order, sess, id, signed); code != 303 {
		t.Fatalf("signer down: the buyer signs: %d %s", code, body)
	}
	if r := x.settleByID(id); r.State != "party_signed" || r.Signer != roleBuyer {
		t.Fatalf("after the paste: %+v", r)
	}
	// The watcher records signer-waiting and rebuilds nothing.
	x.poll()
	var now string
	x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&now)
	if r := x.settleByID(id); r.State != "party_signed" || r.Error != sendSignerWait || r.Raw != "" || now != psbt {
		t.Fatalf("signer down after the pass: %+v, want party_signed with %s and nothing rebuilt", r, sendSignerWait)
	}
	if page := html.UnescapeString(x.page("/order?id="+order, x.vendorSess)); !strings.Contains(page, "The market's escrow signer could not co-sign it yet.") {
		t.Fatal("the order page does not say the co-signature waits for the signer")
	}
	if strings.Contains(logs.String(), "*errors.") {
		t.Fatalf("a Go type name logged as a class:\n%s", logs.String())
	}
	// The signer back: co-signed, broadcast, confirmed.
	f.start()
	x.poll()
	r := x.settleByID(id)
	if r.State != "broadcast" || r.Cosigned != "market" || r.Error != "" {
		t.Fatalf("once the signer is back: %+v (refusals %v)", r, f.refused)
	}
	x.core.setTxConfs(r.TxID, 6, "")
	x.poll()
	if r = x.settleByID(id); r.State != "confirmed" {
		t.Fatalf("after its confirmations: %+v", r)
	}
}

// A per-order escrow whose market row has no stored order wallet key (none exists outside tests; the ruling asks to
// degrade): the escrow address's keys are the stored ones, but a sibling's cannot be derived. The order page (both
// parties) is served with the reason and no copy to sign, the copy is refused 503 (never 500), the sign form 503 with
// nothing stored, a party-signed one waits with signer-waiting and nothing rebuilt, the setup page says the market's key
// cannot be read from the market's records, and no Go type name is logged.
func TestEscrowShapeBPerOrderKeyUnavailable(t *testing.T) {
	logs := captureLog(t)
	x, f := newPerOrderEnv(t)
	order, id := x.siblingStray("nokey", 2)
	var psbt string
	x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&psbt)
	x.signAs(order, roleBuyer, id)
	agExec(x.testEnv, "UPDATE escrow_pubkeys SET per_order_xpub=NULL WHERE party='market' AND escrow_id=(SELECT id FROM escrows WHERE order_id=$1)", order)
	n := len(f.signRequests())
	x.poll()
	var now string
	x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&now)
	if r := x.settleByID(id); r.State != "party_signed" || r.Error != sendSignerWait || r.Raw != "" || now != psbt {
		t.Fatalf("party-signed with the market's key missing: %+v, want signer-waiting and nothing rebuilt", r)
	}
	if got := len(f.signRequests()) - n; got != 0 {
		t.Fatalf("%d sign requests for a settlement whose keys cannot be had", got)
	}
	// Both pages say the co-signature waits; the vendor's, which would offer it a copy to sign too, says why it does not.
	for sess, unsigned := range map[string]bool{x.buyerSess: false, x.vendorSess: true} {
		page := html.UnescapeString(x.page("/order?id="+order, sess))
		if hasSignForm(page) || strings.Contains(page, keysUnavailableText) != unsigned || !strings.Contains(page, "The market's escrow signer could not co-sign it yet.") {
			t.Fatalf("order page with the market's key missing:\n%s", page)
		}
	}
	// QA5W6-2 (mutant M10): the fixed refusal, not the generic 503 page.
	w := x.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10), x.vendorSess, nil)
	if w.Code != 503 || strings.TrimSpace(w.Body.String()) != settleKeysUnavailable {
		t.Fatalf("the vendor's copy with the market's key missing: %d %q", w.Code, w.Body.String())
	}
	// The keys are read before the paste is: whatever is pasted, the refusal is the same.
	code, body := x.sign(order, x.vendorSess, id, psbt)
	if code != 503 || !strings.Contains(html.UnescapeString(body), keysUnavailableText) {
		t.Fatalf("the vendor signs with the market's key missing: %d %s", code, body)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlement_signatures WHERE settlement_id=$1 AND party='vendor'", id); n != 0 {
		t.Fatalf("%d vendor signatures stored", n)
	}
	if page := html.UnescapeString(x.page("/escrow-setup?id="+order, x.buyerSess)); !strings.Contains(page, "The market's cosigner key of this escrow cannot be read from the market's records") {
		t.Fatalf("setup page with the market's key missing:\n%s", page)
	}
	if strings.Contains(logs.String(), "*errors.") {
		t.Fatalf("a Go type name logged as a class:\n%s", logs.String())
	}
}

// QA5W6-1: a sibling stray refund nobody signed yet whose keys cannot be had (the market row with no stored order wallet
// key) records nothing (no signer-waiting: no party signed, so nothing waits for the signer), the log names the fixed
// class per-order-key-unavailable, and its deposit is still followed from the stored PSBT (gettxout needs no keys): spent
// outside the market, the settlement is external as usual, with no send_error. No sign request is made.
func TestEscrowShapeBAwaitingKeysUnavailableTracked(t *testing.T) {
	logs := captureLog(t)
	x, f := newPerOrderEnv(t)
	order, id := x.siblingStray("awnokey", 2)
	agExec(x.testEnv, "UPDATE escrow_pubkeys SET per_order_xpub=NULL WHERE party='market' AND escrow_id=(SELECT id FROM escrows WHERE order_id=$1)", order)
	n := len(f.signRequests())
	x.poll()
	if r := x.settleByID(id); r.State != "awaiting_signature" || r.Error != "" {
		t.Fatalf("awaiting a signature with the market's key missing, after a pass: %+v, want awaiting_signature with no send_error", r)
	}
	if !strings.Contains(logs.String(), "per-order-key-unavailable") {
		t.Fatalf("the watcher did not log the fixed class per-order-key-unavailable:\n%s", logs.String())
	}
	x.core.spend(txidOf("awnokey"), 0)
	x.poll()
	r := x.settleByID(id)
	if r.State != "external" || r.Error != "" {
		t.Fatalf("deposit spent outside the market with the market's key missing: %+v, want external with no send_error", r)
	}
	if got := len(f.signRequests()) - n; got != 0 {
		t.Fatalf("%d sign requests for a settlement nobody signed", got)
	}
	if strings.Contains(logs.String(), "*errors.") || strings.Contains(logs.String(), sendSignerWait) {
		t.Fatalf("a Go type name or signer-waiting logged:\n%s", logs.String())
	}
}

// QA5W6-2 (mutants M7, M8 and M10): a sibling stray refund's fee bump whose keys cannot be had (the market row with no
// stored order wallet key, after the bump was built): the buyer's order page is served with the reason and no bump sign
// form (never a 500), the bump copy is refused 503 with the fixed text, the bump sign form 503 with that text and nothing
// stored, and another order's claim keeps moving in the same watcher pass.
func TestEscrowShapeBBumpKeysUnavailable(t *testing.T) {
	logs := captureLog(t)
	x, _ := newPerOrderEnv(t)
	order, id := x.siblingStray("bumpnokey", 2)
	x.signAs(order, roleBuyer, id)
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" {
		t.Fatalf("the stray refund with the signer up: %+v, want broadcast", r)
	}
	b := x.mustBump(order, x.buyerSess, id, "5")
	agExec(x.testEnv, "UPDATE escrow_pubkeys SET per_order_xpub=NULL WHERE party='market' AND escrow_id=(SELECT id FROM escrows WHERE order_id=$1)", order)
	w := x.do("GET", "/order?id="+order, x.buyerSess, nil)
	page := html.UnescapeString(w.Body.String())
	if w.Code != 200 || strings.Contains(page, `action="/escrow/bump-sign"`) || !strings.Contains(page, keysUnavailableText) {
		t.Fatalf("order page with a fee bump whose keys cannot be had: %d\n%s", w.Code, page)
	}
	if code, body := x.bumpCopy(order, x.buyerSess, id, b.ID); code != 503 || body != settleKeysUnavailable {
		t.Fatalf("the bump copy with the market's key missing: %d %q", code, body)
	}
	if code, body := x.signBump(order, x.buyerSess, b.ID, "cHNidP8="); code != 503 || !strings.Contains(html.UnescapeString(body), keysUnavailableText) {
		t.Fatalf("the bump sign form with the market's key missing: %d %s", code, body)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlement_bump_signatures WHERE bump_id=$1", b.ID); n != 0 {
		t.Fatalf("%d bump signatures stored", n)
	}
	other, oid := x.built(regtestAddr("bumpnokey-o-refund"), regtestAddr("bumpnokey-o-claim"), 100000)
	x.signAs(other, roleVendor, oid)
	x.poll()
	if r := x.settleByID(oid); r.State != "broadcast" {
		t.Fatalf("another order's claim in the same pass: %+v, want broadcast", r)
	}
	if strings.Contains(logs.String(), "*errors.") {
		t.Fatalf("a Go type name logged as a class:\n%s", logs.String())
	}
}

// QA5W5A-4 (mutant M4): siblingKeys checks the keys it derives against the stored sibling address. An escrow_siblings row
// whose idx no longer names its address (the /0/3 row removed and the /0/2 row's idx made 3) gives keys that do not pay
// it: errPerOrderXpub, so the order page is served without a copy to sign and says why.
func TestEscrowShapeBSiblingRowMismatch(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	order, id := x.siblingStray("sibrow", 2)
	agExec(x.testEnv, "DELETE FROM escrow_siblings WHERE escrow_id=(SELECT id FROM escrows WHERE order_id=$1) AND branch=0 AND idx=3", order)
	agExec(x.testEnv, "UPDATE escrow_siblings SET idx=3 WHERE escrow_id=(SELECT id FROM escrows WHERE order_id=$1) AND branch=0 AND idx=2", order)
	page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
	if hasSignForm(page) || !strings.Contains(page, keysUnavailableText) {
		t.Fatalf("order page with a sibling row that does not give its address:\n%s", page)
	}
	if code, body := x.sign(order, x.buyerSess, id, "cHNidP8="); code != 503 || !strings.Contains(html.UnescapeString(body), keysUnavailableText) {
		t.Fatalf("sign with a sibling row that does not give its address: %d %s", code, body)
	}
}

// QA5W5A-3 (mutant M12): a stray refund tries each address's deposits in the order of their first deposit, and one whose
// deposits are too few to pay their fee never fails the refund of another: dust recorded first at sibling /0/1, a real
// deposit at /0/2; the stray refund spends /0/2's deposit and is awaiting a signature, not failed too-small.
func TestEscrowShapeBStrayDustSiblingDoesNotHold(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	order := x.completed(regtestAddr("sibdust-refund"), 100000)
	dust, real := x.sibling(order, 0, 1), x.sibling(order, 0, 2)
	x.core.deposit(dust, txidOf("sibdust-dust"), 0, 400, 6)
	x.core.deposit(real, txidOf("sibdust-real"), 0, 40000, 6)
	x.poll()
	x.poll()
	if got := agStrings(t, x.testEnv, "SELECT address FROM payments WHERE order_id=$1 AND address IN ($2,$3) ORDER BY id", order, dust, real); len(got) != 2 || got[0] != dust {
		t.Fatalf("deposits recorded %v, want the dust first", got)
	}
	ids, states := x.strayRefunds(order)
	if len(ids) != 1 || states[0] != "awaiting_signature" {
		t.Fatalf("stray refunds %v %v, want one awaiting a signature", ids, states)
	}
	if got := agStrings(t, x.testEnv, "SELECT address FROM payments WHERE settlement_id=$1", ids[0]); len(got) != 1 || got[0] != real {
		t.Fatalf("the stray refund spends %v, want only /0/2's deposit", got)
	}
	// QA5W5A-6: the order note and the buyer's notice name where they arrived: not the escrow address.
	events := strings.Join(x.events(order), "\n")
	notice := strings.Join(agStrings(t, x.testEnv, "SELECT body FROM notifications WHERE user_id=$1", x.buyer), "\n")
	for what, text := range map[string]string{"order note": events, "buyer's notice": notice} {
		if !strings.Contains(text, "arrived at an address of this order's wallet other than its escrow address") || strings.Contains(text, "arrived at this order's escrow address") {
			t.Fatalf("%s for sibling deposits:\n%s", what, text)
		}
	}
}

// QA5W5A-3 (mutant M11), adapted to QA5W5A-1: with the escrow signer down a sibling's keys still come from what checkout
// stored, so only a sibling whose keys cannot be had (here: a market row with no stored order wallet key) has deposits
// that wait; they never hold up the escrow address's. Its deposit recorded, the stray refund spends the escrow address's
// deposit alone, is built and signed while the signer is down, and waits only for the co-signature.
func TestEscrowShapeBStraySignerDownEscrowAddressBuilds(t *testing.T) {
	x, f := newPerOrderEnv(t)
	order := x.completed(regtestAddr("sibhold-refund"), 100000)
	var addr string
	x.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr)
	sib := x.sibling(order, 0, 1)
	f.stop()
	agExec(x.testEnv, "UPDATE escrow_pubkeys SET per_order_xpub=NULL WHERE party='market' AND escrow_id=(SELECT id FROM escrows WHERE order_id=$1)", order)
	x.core.deposit(sib, txidOf("sibhold-sib"), 0, 40000, 6)
	x.core.deposit(addr, txidOf("sibhold-main"), 0, 50000, 6)
	x.poll()
	x.poll()
	ids, states := x.strayRefunds(order)
	if len(ids) != 1 || states[0] != "awaiting_signature" {
		t.Fatalf("stray refunds %v %v, want one awaiting a signature", ids, states)
	}
	if got := agStrings(t, x.testEnv, "SELECT address FROM payments WHERE settlement_id=$1", ids[0]); len(got) != 1 || got[0] != addr {
		t.Fatalf("the stray refund spends %v, want only the escrow address's deposit", got)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE address=$1 AND settlement_id IS NULL", sib); n != 1 {
		t.Fatalf("the sibling's deposit: %d unclaimed, want 1 waiting", n)
	}
	x.signAs(order, roleVendor, ids[0])
	x.poll()
	if r := x.settleByID(ids[0]); r.State != "party_signed" || r.Error != sendSignerWait {
		t.Fatalf("signed while the signer is down: %+v", r)
	}
}

// QA5W5A-4 (mutant M16): the escrow signer's answer is checked as the arbiter's is (checkMarketSigned): an answer with an
// extra signature, one signed with SIGHASH_NONE and one for another transaction each leave the settlement party-signed
// with node-bad-reply, nothing stored or broadcast; the next pass with a proper answer co-signs it.
func TestEscrowShapeBSignerAnswerChecked(t *testing.T) {
	x, f := newPerOrderEnv(t)
	order, id := x.built(regtestAddr("answer-refund"), regtestAddr("answer-claim"), 100000)
	x.signAs(order, roleVendor, id)
	for _, mode := range []string{"extra-sig", "none", "other-tx"} {
		f.set(func(f *fakeSigner) { f.tamper = mode })
		n := len(f.signRequests())
		x.poll()
		if got := len(f.signRequests()) - n; got != 1 {
			t.Fatalf("%s: %d sign requests", mode, got)
		}
		if r := x.settleByID(id); r.State != "party_signed" || r.Error != sendBadReply || r.Raw != "" {
			t.Fatalf("%s: %+v, want party_signed with %s and nothing stored", mode, r, sendBadReply)
		}
		if n := x.core.count("sendrawtransaction"); n != 0 {
			t.Fatalf("%s: %d broadcasts", mode, n)
		}
	}
	f.set(func(f *fakeSigner) { f.tamper = "" })
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || r.Cosigned != "market" {
		t.Fatalf("with a proper answer: %+v", r)
	}
}

// QA5W5A-4 (mutant M2b): the escrow signer's fingerprint must be the one the escrow's market key was built with. A market
// row whose origin fingerprint differs (changed in the database) leaves the settlement party-signed with signer-waiting:
// the signer is not asked to sign, nothing is rebuilt.
func TestEscrowShapeBMarketFingerprintDiffers(t *testing.T) {
	x, f := newPerOrderEnv(t)
	order, id := x.built(regtestAddr("fp-refund"), regtestAddr("fp-claim"), 100000)
	x.signAs(order, roleVendor, id)
	agExec(x.testEnv, "UPDATE escrow_pubkeys SET origin_fingerprint='0badf00d' WHERE party='market' AND escrow_id=(SELECT id FROM escrows WHERE order_id=$1)", order)
	n := len(f.signRequests())
	x.poll()
	if r := x.settleByID(id); r.State != "party_signed" || r.Error != sendSignerWait || r.Raw != "" {
		t.Fatalf("a market key built under another fingerprint: %+v, want party_signed with %s", r, sendSignerWait)
	}
	if got := len(f.signRequests()) - n; got != 0 {
		t.Fatalf("%d sign requests for a market key built under another fingerprint", got)
	}
}

// QA5W5A-4 (mutant M21): an escrow signer whose account key differs from the pins makes per-order escrows unavailable: the
// order page of a buyer and a vendor with per-order keys says so and offers no escrow, and under escrow-only BTC intake the
// listing of a per-order vendor is not available for purchase in BTC, saying why.
func TestEscrowShapeBSignerMismatchUnavailable(t *testing.T) {
	x, f := newPerOrderEnv(t)
	f.set(func(f *fakeSigner) { f.key = signerKey(t, 8) })
	x.A.checkEscrowSigner(context.Background())
	if x.A.escrowSignerState() != signerMismatch {
		t.Fatalf("signer state %q", x.A.escrowSignerState())
	}
	page := html.UnescapeString(x.page("/order?id="+x.order, x.buyerSess))
	if !strings.Contains(page, errPerOrderUnavailable) || strings.Contains(page, "Pay into an escrow</button>") {
		t.Fatalf("order page with a mismatched signer:\n%s", page)
	}
	agExec(x.testEnv, "UPDATE settings SET value='escrow' WHERE key='payment_intake_BTC'")
	listing := html.UnescapeString(x.page("/product?id="+x.product, x.buyerSess))
	if !strings.Contains(listing, "Not available for purchase in BTC") || !strings.Contains(listing, errPerOrderUnavailable) {
		t.Fatalf("listing with a mismatched signer:\n%s", listing)
	}
}

// QA5W5A-1: checkout stores the market's order wallet key only after the checks the parties' keys get and two of its own:
// its /0/0 child is the market key the escrow stores, its origin fingerprint is the pinned signer fingerprint, and it is
// the child <m> of the signer key this checkout read and checked against the pins. insertEscrow, given a built escrow
// whose market order wallet key breaks one of them, refuses it (errPerOrderMismatch) with nothing written; the escrow as
// built is written with the market's key stored.
func TestEscrowShapeBMarketKeyCrossChecked(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	ctx := context.Background()
	other := signerKey(t, 9)
	for _, c := range []struct {
		name  string
		spoil func(b *builtEscrow, tx *sql.Tx)
	}{
		{"another key's child", func(b *builtEscrow, _ *sql.Tx) {
			order, leaf, _ := perOrderKeys(escrowKey{ChainCode: other.ChainCode, PubKey: other.PubKey}, uint32(b.Parties[2].Index))
			b.Parties[2].Order, b.Parties[2].Leaf, b.Keys["market"] = order, leaf, leaf.PubKey[:]
		}},
		{"another fingerprint", func(b *builtEscrow, _ *sql.Tx) { b.Parties[2].Key.Fingerprint = "0badf00d" }},
		{"no pins", func(_ *builtEscrow, tx *sql.Tx) {
			if _, err := tx.Exec("DELETE FROM settings WHERE key IN ($1,$2)", escrowSignerFingerprintPin, escrowSignerXPubPin); err != nil {
				t.Fatal(err)
			}
		}},
		{"as built", func(*builtEscrow, *sql.Tx) {}},
	} {
		tx, err := x.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1", x.order))
		if err != nil {
			t.Fatal(err)
		}
		buyer, vendor, err := x.A.escrowParties(ctx, tx, &o)
		if err != nil {
			t.Fatal(err)
		}
		signer, err := x.A.signerAccount(ctx, tx)
		if err != nil {
			t.Fatal(err)
		}
		market, err := nextMarketParty(ctx, tx, x.A.escrow.Load(), &signer)
		if err != nil {
			t.Fatal(err)
		}
		b := &builtEscrow{Address: regtestAddr("xcheck-" + strings.Fields(c.name)[0]), MarketIndex: market.Index, Parties: []escrowParty{buyer, vendor, market},
			Keys: map[string][]byte{"buyer": buyer.Leaf.PubKey[:], "vendor": vendor.Leaf.PubKey[:], "market": market.Leaf.PubKey[:]}, Net: keyNetTest}
		c.spoil(b, tx)
		err = insertEscrow(ctx, tx, x.order, regtestAddr("xcheck-refund-"+strings.Fields(c.name)[0]), 0, "", 0, 0, b)
		switch {
		case c.name == "as built" && err != nil:
			t.Fatalf("%s: %v", c.name, err)
		case c.name == "as built":
			var stored string
			if err := tx.QueryRow("SELECT per_order_xpub FROM escrow_pubkeys WHERE party='market' AND pubkey=$1", hex.EncodeToString(b.Keys["market"])).Scan(&stored); err != nil || stored != market.Order.Serialize(versionTpub) {
				t.Fatalf("%s: the market's order wallet key stored %q, %v", c.name, stored, err)
			}
		case err != errPerOrderMismatch:
			t.Fatalf("%s: %v, want errPerOrderMismatch", c.name, err)
		}
		tx.Rollback()
	}
	x.nothingWritten(x.order)
}
