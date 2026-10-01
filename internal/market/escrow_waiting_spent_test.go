package market

import (
	"context"
	"encoding/hex"
	"html"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// A-356: a settlement waiting for a party's signature, or still being built, whose escrow deposit another transaction
// spent can never complete. The watcher marks it external with the spending transaction when the watch-only wallet
// names it, flags it for staff and tells the parties, as markExternal does for a party-signed one; the order page stops
// asking for a signature. When the spending transaction is the settlement's own (the parties signed and sent it
// themselves), it is recorded as broadcast, as A-344 records a party-signed one. Fake escrow Core (simulated RPC, not
// chain evidence).

// sigsOf is party's good signature, as its own wallet would make it, on every input of settlement id.
func (x *escrowEnv) sigsOf(order string, id int64, party string) partySigs {
	x.t.Helper()
	var stored string
	if err := x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored); err != nil {
		x.t.Fatal(err)
	}
	sp, err := decodePSBT(stored)
	if err != nil {
		x.t.Fatal(err)
	}
	pub, _ := hex.DecodeString(x.keyHex(order, party))
	p := partySigs{Party: party}
	for range sp.Inputs {
		p.Sigs = append(p.Sigs, psbtSig{pub, fakeSig(pub, sp.TxID(), 1)})
	}
	return p
}

func TestEscrowAwaitingSignatureSpentOutside(t *testing.T) {
	x := newEscrowEnv(t)
	_, mod := x.user("a356mod", "moderator")
	order, id := x.built(regtestAddr("refund-a356"), regtestAddr("claim-a356"), 100000)

	// Unspent: the check reads each input with gettxout (mempool included) and changes nothing.
	n := len(x.core.methods())
	x.poll()
	if r := x.settleByID(id); r.State != "awaiting_signature" {
		t.Fatalf("an unspent awaiting settlement moved: %+v", r)
	}
	var reads []string
	x.core.mu.Lock()
	for _, c := range x.core.calls[n:] {
		if c.Method == "gettxout" {
			reads = append(reads, c.Path+" "+string(c.Params))
		}
	}
	x.core.mu.Unlock()
	if !slices.Contains(reads, `/ ["`+txidOf(order)+`",0,true]`) {
		t.Fatalf("gettxout calls %q: the awaiting settlement's input was not read", reads)
	}

	// The parties co-spend the escrow directly while the claim waits for a signature.
	spender := x.coSpend(txidOf(order), 0, 0)
	x.poll()
	r := x.settleByID(id)
	if r.State != "external" || r.SpentBy != spender {
		t.Fatalf("an awaiting settlement whose input was co-spent: %+v (the wallet names %s)", r, spender)
	}
	if path, params := x.core.lastCall(t, "listtransactions"); path != fakeWatchWallet || params != `["*",100,0,true]` {
		t.Fatalf("listtransactions %s %s", path, params)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id=$2 AND flagged", order, id) != 1 {
		t.Fatal("the settlement's deposit was not flagged for staff")
	}
	ev := strings.Join(x.events(order), "\n")
	if !strings.Contains(ev, "before any party signed it") || !strings.Contains(ev, "transaction "+spender) || !strings.Contains(ev, "Moderator review required") {
		t.Fatalf("order notes:\n%s", ev)
	}
	for _, uid := range []string{x.buyer, x.vendor} {
		if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%were spent by a transaction this market did not make%'", uid) != 1 {
			t.Fatalf("party %s not told", uid)
		}
	}
	if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE body LIKE $1", "Payment review needed for order "+shortID(order)+"%") == 0 {
		t.Fatal("staff not told")
	}
	for _, sess := range []string{x.vendorSess, x.buyerSess, mod} {
		page := html.UnescapeString(x.page("/order?id="+order, sess))
		if !strings.Contains(page, "Spent outside the market") || !strings.Contains(page, spender) {
			t.Fatalf("order page:\n%s", page)
		}
		if strings.Contains(page, `name="psbt"`) || strings.Contains(page, "Waiting for a signature") {
			t.Fatalf("the order page still asks for a signature:\n%s", page)
		}
	}
	// A late paste is refused and changes nothing; repeated passes flag it once.
	sess, fp := x.partySess(roleVendor)
	if code, _ := x.sign(order, sess, id, walletSign(t, x.partyCopyAny(order, id, roleVendor), fp, 1)); code != 409 {
		t.Fatalf("a paste on an external settlement: %d", code)
	}
	x.poll()
	x.poll()
	if strings.Count(strings.Join(x.events(order), "\n"), "did not make") != 1 || x.core.count("walletprocesspsbt") != 0 || x.core.count("sendrawtransaction") != 0 {
		t.Fatal("flagged more than once, or signed or sent")
	}
	x.noPayouts(order)

	// A spend the wallet does not name (listtransactions refused): external without a transaction ID.
	order2, id2 := x.built(regtestAddr("refund-a356-2"), regtestAddr("claim-a356-2"), 100000)
	x.core.mu.Lock()
	x.core.listTxErr = -32601
	x.core.mu.Unlock()
	x.core.spend(txidOf(order2), 0)
	x.poll()
	if r := x.settleByID(id2); r.State != "external" || r.SpentBy != "" {
		t.Fatalf("an unnamed spend: %+v", r)
	}
	if ev := strings.Join(x.events(order2), "\n"); !strings.Contains(ev, "the market's watch-only wallet does not name it") {
		t.Fatalf("order notes:\n%s", ev)
	}
	x.core.mu.Lock()
	x.core.listTxErr = 0
	x.core.mu.Unlock()

	// A refund waiting for the buyer's signature is followed the same way.
	order3 := x.draft()
	x.mustCheckout(order3, regtestAddr("refund-a356-3"))
	x.fund(order3, order3, 3, 40000)
	if w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order3}, "from": {stateAwaitingPayment}, "amount_seen": {"40000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	id3, kind3, st3, _ := x.settlement(order3)
	if kind3 != "refund" || st3 != "awaiting_signature" {
		t.Fatalf("refund %s %s", kind3, st3)
	}
	spender3 := x.coSpend(txidOf(order3), 0, 2)
	x.poll()
	if r := x.settleByID(id3); r.State != "external" || r.SpentBy != spender3 {
		t.Fatalf("an awaiting refund whose input was co-spent: %+v", r)
	}

	// While the restore gate is set the check still runs: it only watches, and sends nothing.
	order4, id4 := x.built(regtestAddr("refund-a356-4"), regtestAddr("claim-a356-4"), 100000)
	x.setGate(true)
	spender4 := x.coSpend(txidOf(order4), 0, 0)
	x.poll()
	x.setGate(false)
	if r := x.settleByID(id4); r.State != "external" || r.SpentBy != spender4 {
		t.Fatalf("an awaiting settlement co-spent under the restore gate: %+v", r)
	}
}

// partyCopyAny is party's copy of settlement id as the stored PSBT gives it, whatever the settlement's state (the
// download serves only a settlement awaiting a signature).
func (x *escrowEnv) partyCopyAny(order string, id int64, party string) string {
	x.t.Helper()
	var stored string
	if err := x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored); err != nil {
		x.t.Fatal(err)
	}
	keys, err := escrowKeys(context.Background(), x.DB, order)
	if err != nil {
		x.t.Fatal(err)
	}
	cp, err := settlementCopy(stored, keys[party])
	if err != nil {
		x.t.Fatal(err)
	}
	return cp
}

// The settlement's own transaction found spending the escrow while it waits for a signature: the buyer and the vendor
// signed the copies they downloaded and sent it without the market. It is checked like any final transaction and
// recorded as broadcast (who signed it is not recorded: the market holds no signature), never external.
func TestEscrowAwaitingSignatureOwnTransactionSeen(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.built(regtestAddr("refund-a356-own"), regtestAddr("claim-a356-own"), 100000)
	txid, raw := x.partiesBroadcast(id, 0, x.sigsOf(order, id, roleBuyer), x.sigsOf(order, id, roleVendor))
	if txid != x.settleByID(id).TxID {
		t.Fatal("the parties' transaction is not the stored one")
	}
	x.poll()
	r := x.settleByID(id)
	if r.State != "broadcast" || r.Raw != raw || r.Cosigned != "network" || r.SpentBy != "" {
		t.Fatalf("the settlement's own transaction found while awaiting a signature: %+v", r)
	}
	if x.core.count("walletprocesspsbt") != 0 || x.sendsOf(raw) != 0 {
		t.Fatal("the market signed or sent it")
	}
	ev := strings.Join(x.events(order), "\n")
	if !strings.Contains(ev, "was already on the regtest network when the market came to complete it") || !strings.Contains(ev, "Who signed it is not recorded") || strings.Contains(ev, "did not make") {
		t.Fatalf("order notes:\n%s", ev)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", order) != 0 {
		t.Fatal("flagged as spent outside the market")
	}
	x.core.setTxConfs(txid, 3, "")
	x.poll()
	if r := x.settleByID(id); r.State != "confirmed" {
		t.Fatalf("at 3 confirmations: %+v", r)
	}
	x.noPayouts(order)
}

// A settlement being built whose input was spent: the build waits one pass on input-spent (P1-c1); still spent on the
// next pass, the deposit is marked spent outside the market with its spender and the settlement is external, never
// built.
func TestEscrowBuildingSpentOutside(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(regtestAddr("refund-a356-build"), 100000)
	if code, body := x.claim(order, x.vendorSess, regtestAddr("claim-a356-build")); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	spender := x.coSpend(txidOf(order), 0, 1)
	creates := x.core.count("createpsbt")
	x.poll()
	id, _, state, class := x.settlement(order)
	if state != "building" || class != settleSpent {
		t.Fatalf("first pass over a spent input: %s %s", state, class)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND spent_outside", order) != 0 {
		t.Fatal("marked before the wait")
	}
	x.poll()
	r := x.settleByID(id)
	if r.State != "external" || r.SpentBy != spender {
		t.Fatalf("a building settlement whose input stays spent: %+v (the wallet names %s)", r, spender)
	}
	if x.core.count("createpsbt") != creates {
		t.Fatal("a settlement was built over a spent input")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND spent_outside AND spent_by=$2 AND flagged AND settlement_id IS NULL", order, spender) != 1 {
		t.Fatal("the spent deposit was not marked with its spender and flagged")
	}
	ev := strings.Join(x.events(order), "\n")
	if !strings.Contains(ev, "so the market cannot build it") || !strings.Contains(ev, "transaction "+spender) || strings.Count(ev, "did not make") != 1 {
		t.Fatalf("order notes:\n%s", ev)
	}
	page := html.UnescapeString(x.page("/order?id="+order, x.vendorSess))
	if !strings.Contains(page, "Spent outside the market: an escrow deposit this settlement was to spend was spent by a transaction this market did not make ("+spender+"), so the market cannot build it.") || strings.Contains(page, "Being built") {
		t.Fatalf("order page:\n%s", page)
	}
	x.poll()
	if strings.Count(strings.Join(x.events(order), "\n"), "did not make") != 1 {
		t.Fatal("flagged more than once")
	}

	// A spend that is gone on the next pass (an evicted mempool spend) leaves the build to go ahead.
	order2 := x.completed(regtestAddr("refund-a356-build2"), 100000)
	if code, body := x.claim(order2, x.vendorSess, regtestAddr("claim-a356-build2")); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	x.core.spend(txidOf(order2), 0)
	x.poll()
	if _, _, st, cl := x.settlement(order2); st != "building" || cl != settleSpent {
		t.Fatalf("first pass: %s %s", st, cl)
	}
	x.core.mu.Lock()
	delete(x.core.spent, txidOf(order2)+":0")
	x.core.mu.Unlock()
	x.poll()
	if _, _, st, _ := x.settlement(order2); st != "awaiting_signature" {
		t.Fatalf("after the spend left the mempool: %s", st)
	}

	// A spend that leaves the mempool between the build's read and its second look: nothing is marked; the next pass
	// builds.
	order3 := x.completed(regtestAddr("refund-a356-build3"), 100000)
	if code, body := x.claim(order3, x.vendorSess, regtestAddr("claim-a356-build3")); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	x.core.spend(txidOf(order3), 0)
	x.poll()
	reads := 0
	x.core.mu.Lock()
	x.core.beforeUTXO = func(txid string, _ int64) {
		if txid != txidOf(order3) {
			return
		}
		if reads++; reads == 2 {
			x.core.mu.Lock()
			delete(x.core.spent, txidOf(order3)+":0")
			x.core.mu.Unlock()
		}
	}
	x.core.mu.Unlock()
	x.poll()
	x.core.mu.Lock()
	x.core.beforeUTXO = nil
	x.core.mu.Unlock()
	id3, _, st3, _ := x.settlement(order3)
	if st3 != "building" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND (spent_outside OR flagged)", order3) != 0 || x.settleByID(id3).SpentBy != "" {
		t.Fatalf("a spend gone before the second look: %s", st3)
	}
	x.poll()
	if _, _, st, _ := x.settlement(order3); st != "awaiting_signature" {
		t.Fatalf("the next pass: %s", st)
	}
	x.noPayouts(order)
}

// Migration 078: a settlement may become external straight from building, without a PSBT, and then never gets one; a
// settlement waiting for a signature may store a final transaction only by moving to signed. Everything else
// migrations 075-077 refuse stays refused.
func TestEscrowMigration078WaitingSpent(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(regtestAddr("refund-m78"), 100000)
	if code, body := x.claim(order, x.vendorSess, regtestAddr("claim-m78")); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	id, _, state, _ := x.settlement(order)
	if state != "building" {
		t.Fatalf("claim %s", state)
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='awaiting_signature' WHERE id=$1", id); err == nil {
		t.Fatal("the database allowed a settlement with no PSBT to wait for a signature")
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='external' WHERE id=$1", id); err != nil {
		t.Fatalf("building -> external without a PSBT refused: %v", err)
	}
	for _, q := range []string{
		"UPDATE settlements SET psbt='cHNidP8=' WHERE id=$1",
		"UPDATE settlements SET txid='" + txidOf("m78") + "' WHERE id=$1",
		"UPDATE settlements SET state='building' WHERE id=$1",
	} {
		if _, err := x.DB.Exec(q, id); err == nil {
			t.Fatalf("the database allowed on a never-built external settlement: %s", q)
		}
	}

	// Waiting for a signature: a final transaction is stored only with the move to signed.
	_, id2 := x.built(regtestAddr("refund-m78b"), regtestAddr("claim-m78b"), 100000)
	if _, err := x.DB.Exec("UPDATE settlements SET raw_tx='00',cosigned='network',final_at=now() WHERE id=$1", id2); err == nil {
		t.Fatal("the database stored a final transaction on a settlement left waiting for a signature")
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='broadcast',raw_tx='00',cosigned='network',final_at=now(),broadcast_at=now() WHERE id=$1", id2); err == nil {
		t.Fatal("the database stored a final transaction with a move other than to signed")
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='signed',raw_tx='00',cosigned='network',final_at=now() WHERE id=$1", id2); err != nil {
		t.Fatalf("awaiting_signature -> signed with its final transaction refused: %v", err)
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='awaiting_signature' WHERE id=$1", id2); err == nil {
		t.Fatal("the database allowed a final transaction back to awaiting_signature")
	}
}
