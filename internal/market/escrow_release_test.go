package market

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// Escrow Phase 2, slice P2-b (WR-escrow A-247; P2-R1, P2-R2, P2-R4, P2-R6; A-371 to A-374, A-376): the release built
// when the vendor marks a physical escrow order shipped, the buyer and the vendor completing it with their two
// signatures (never the market before the outcome), disputes over a pre-built release and top-ups while the order is
// open. DB integration tests against the simulated Core of escrow_fake_test.go (simulated RPC, not chain evidence:
// TestEscrowRegtestPhysical is the real-chain leg).

// shipEscrow posts the escrow "Mark as shipped" form (a fresh claim address and the password) as sess.
func (x *escrowEnv) shipEscrow(order, sess, addr, password string) (int, string) {
	w := x.do("POST", "/escrow/ship", sess, url.Values{"order_id": {order}, "address": {addr}, "password": {password}, "note": {"Sent by tracked post."}})
	return w.Code, w.Body.String()
}

// paidPhysical is a funded physical escrow order (refund address refund, paid with sats), waiting to be shipped.
func (x *escrowEnv) paidPhysical(refund string, sats ...int64) string {
	x.t.Helper()
	order := x.physicalDraft(3)
	x.mustCheckout(order, refund)
	x.fund(order, order, 3, sats...)
	if x.state(order) != statePaid {
		x.t.Fatalf("funded physical escrow order is %s", x.state(order))
	}
	return order
}

// shippedPhysical is a funded physical escrow order the vendor marked shipped with claimTo, and its release built by
// one watcher pass: the order and the release.
func (x *escrowEnv) shippedPhysical(refund, claimTo string, sats ...int64) (string, int64) {
	x.t.Helper()
	order := x.paidPhysical(refund, sats...)
	if code, body := x.shipEscrow(order, x.vendorSess, claimTo, testPassword); code != 303 {
		x.t.Fatalf("ship: %d %s", code, body)
	}
	x.poll()
	id, kind, state, class := x.settlement(order)
	if kind != "release" || state != "awaiting_signature" {
		x.t.Fatalf("release after shipping: %s %s %s", kind, state, class)
	}
	return order, id
}

// marketCalls counts the RPCs only the market's own signature makes: the arbiter key check (deriveaddresses of the
// market child's wpkh address) and refill, and the arbiter wallet's walletprocesspsbt.
func (x *escrowEnv) marketCalls() int {
	return x.core.count("walletprocesspsbt") + x.core.count("keypoolrefill") + x.core.countWith("deriveaddresses", "wpkh(")
}

// P2-R1: an escrow order is marked shipped only through the escrow form, with a fresh claim address (the claim
// action's checks) and the vendor's password; in one transaction the order is shipped, the release is requested to
// that address and release_at is the escrow's snapshot of release_days from now. The pooled form refuses an escrow
// order, and a historical pooled order is read-only.
func TestEscrowShipNeedsClaimAddress(t *testing.T) {
	x := newEscrowEnv(t)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('escrow_release_days','10')")
	refund := regtestAddr("ship-refund")
	order := x.paidPhysical(refund, 100000)
	page := x.page("/order?id="+order, x.vendorSess)
	if !strings.Contains(page, `action="/escrow/ship"`) || !strings.Contains(page, `name="address"`) || strings.Contains(page, `action="/orders/ship"`) {
		t.Fatalf("the vendor's page does not offer the escrow ship form:\n%s", page)
	}
	unchanged := func(name string) {
		t.Helper()
		if x.state(order) != statePaid || agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", order) != 0 ||
			agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND release_at IS NOT NULL", order) != 0 {
			t.Fatalf("%s changed the order", name)
		}
	}
	// The pooled form never ships an escrow order.
	if w := x.do("POST", "/orders/ship", x.vendorSess, url.Values{"order_id": {order}}); w.Code != 409 || !strings.Contains(w.Body.String(), "claim address") {
		t.Fatalf("pooled ship of an escrow order: %d %s", w.Code, w.Body.String())
	}
	unchanged("the pooled form")
	_, other := x.user("ship_other_"+randomToken()[:6], "vendor")
	for _, c := range []struct {
		name, sess, addr, password string
		code                       int
	}{
		{"no address", x.vendorSess, "", testPassword, 400},
		{"a malformed address", x.vendorSess, "not an address!", testPassword, 400},
		{"an address of another network", x.vendorSess, "tb1qnotregtest0000000000000000000000000", testPassword, 400},
		{"the buyer's refund address", x.vendorSess, refund, testPassword, 409},
		{"a wrong password", x.vendorSess, regtestAddr("ship-claim"), "wrong password 4321", 401},
		{"the buyer", x.buyerSess, regtestAddr("ship-claim"), testPassword, 403},
		{"another vendor", other, regtestAddr("ship-claim"), testPassword, 404},
	} {
		if code, body := x.shipEscrow(order, c.sess, c.addr, c.password); code != c.code {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
		unchanged(c.name)
	}
	before := time.Now()
	claimTo := regtestAddr("ship-claim")
	if code, body := x.shipEscrow(order, x.vendorSess, claimTo, testPassword); code != 303 {
		t.Fatalf("ship: %d %s", code, body)
	}
	if x.state(order) != stateShipped {
		t.Fatalf("after shipping: %s", x.state(order))
	}
	var kind, state, dest, by string
	if err := x.DB.QueryRow("SELECT kind,state,destination,requested_by FROM settlements WHERE order_id=$1", order).Scan(&kind, &state, &dest, &by); err != nil {
		t.Fatal(err)
	}
	if kind != "release" || state != "building" || dest != claimTo || by != x.vendor {
		t.Fatalf("release: %s %s %s %s", kind, state, dest, by)
	}
	var releaseAt time.Time
	if err := x.DB.QueryRow("SELECT release_at FROM escrows WHERE order_id=$1", order).Scan(&releaseAt); err != nil {
		t.Fatal(err)
	}
	if want := before.Add(10 * 24 * time.Hour); releaseAt.Before(want.Add(-time.Minute)) || releaseAt.After(want.Add(time.Minute)) {
		t.Fatalf("release_at %s, want about %s (the escrow's 10 days)", releaseAt, want)
	}
	if got := agStrings(t, x.testEnv, "SELECT action FROM audit_events WHERE user_id=$1 AND action LIKE 'Marked order%'", x.vendor); len(got) != 1 ||
		got[0] != "Marked order "+shortID(order)+" shipped (Bitcoin escrow, regtest) (confirmed with password)" {
		t.Fatalf("audit %q", got)
	}
	ev := strings.Join(x.events(order), "\n")
	if !strings.Contains(ev, "paid>shipped "+x.vendor+" Sent by tracked post.") || !strings.Contains(ev, "the market builds the release to the vendor") {
		t.Fatalf("events:\n%s", ev)
	}
	// Shipped once: a repeated form changes nothing, and the claim form is never offered beside the pre-built release.
	if code, _ := x.shipEscrow(order, x.vendorSess, regtestAddr("ship-claim-2"), testPassword); code != 409 {
		t.Fatalf("shipping twice: %d", code)
	}
	if code, _ := x.claim(order, x.vendorSess, regtestAddr("ship-claim-3")); code != 409 {
		t.Fatalf("a claim beside the pre-built release: %d", code)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", order) != 1 {
		t.Fatal("a second settlement was written")
	}
	// A historical non-escrow order cannot ship through either form.
	product := x.testEnv.product(x.vendor, "physical")
	historical := x.testEnv.order(x.buyer, product, "BTC", statePaid)
	if code, _ := x.shipEscrow(historical, x.vendorSess, regtestAddr("ship-historical"), testPassword); code != 404 {
		t.Fatalf("escrow form on a historical order: %d", code)
	}
	historyBefore := strings.Join(x.events(historical), "\n")
	if w := x.do("POST", "/orders/ship", x.vendorSess, url.Values{"order_id": {historical}}); w.Code != 409 || !strings.Contains(w.Body.String(), "Historical pooled orders are read-only") {
		t.Fatalf("historical ship was not refused read-only: %d %s", w.Code, w.Body.String())
	}
	if x.state(historical) != statePaid || strings.Join(x.events(historical), "\n") != historyBefore ||
		agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", historical) != 0 {
		t.Fatal("historical ship changed its recorded history")
	}

}

// A-373: on an escrow order the buyer confirms receipt only by signing the release; Complete is refused (409) and
// never offered, so it can never lead to a market co-signature. Historical pooled orders have no completion action.
func TestEscrowPooledCompleteRefusedOnEscrow(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.shippedPhysical(regtestAddr("complete-refund"), regtestAddr("complete-claim"), 100000)
	if page := x.page("/order?id="+order, x.buyerSess); strings.Contains(page, `action="/orders/complete"`) {
		t.Fatal("the buyer of an escrow order is offered the pooled Complete")
	}
	events := len(x.events(order))
	w := x.do("POST", "/orders/complete", x.buyerSess, url.Values{"order_id": {order}, "payout_confirmed": {"confirmed"}, "amount_seen": {"100000"}})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "signing the release") {
		t.Fatalf("pooled complete of an escrow order: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	if x.state(order) != stateShipped || len(x.events(order)) != events || x.settleByID(id).State != "awaiting_signature" || x.marketCalls() != 0 {
		t.Fatalf("the refused Complete changed something: %s, %d market calls", x.state(order), x.marketCalls())
	}
	product := x.testEnv.product(x.vendor, "physical")
	pooled := x.testEnv.order(x.buyer, product, "BTC", stateShipped)
	pooledEvents := strings.Join(x.events(pooled), "\n")
	if page := x.page("/order?id="+pooled, x.buyerSess); strings.Contains(page, `action="/orders/complete"`) {
		t.Fatal("the buyer of a historical pooled order is offered Complete")
	}
	if w := x.do("POST", "/orders/complete", x.buyerSess, url.Values{"order_id": {pooled}, "payout_confirmed": {"confirmed"}, "amount_seen": {"0"}}); w.Code != 409 || !strings.Contains(w.Body.String(), "Historical pooled orders are read-only") {
		t.Fatalf("historical pooled complete: %d %s", w.Code, w.Body.String())
	}
	if x.state(pooled) != stateShipped || strings.Join(x.events(pooled), "\n") != pooledEvents ||
		agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", pooled) != 0 {
		t.Fatal("the refused historical pooled Complete changed its recorded history")
	}
}

// P2-R2, A-376: a shipped order's release is built, and the buyer and the vendor sign it through the sign action; the
// watcher finalizes the two party signatures alone (no arbiter call at all), stores the final transaction with cosigned
// 'parties' and completes the order (system, shipped -> completed) in the same transaction, then broadcasts it. Signing
// is confirming receipt: the buyer who signed cannot open a dispute. A dispute opened before the pass stops it.
func TestEscrowPhysicalBothSign(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.shippedPhysical(regtestAddr("both-refund"), regtestAddr("both-claim"), 100000)
	buyerPage := x.page("/order?id="+order, x.buyerSess)
	for _, want := range []string{"Settlement: Release to the vendor", `action="/escrow/sign"`, "confirms that you received the goods", "ends your right to open a dispute",
		"the vendor signs the release now"} {
		if !strings.Contains(buyerPage, want) {
			t.Fatalf("buyer's page lacks %q:\n%s", want, buyerPage)
		}
	}
	for _, page := range []string{buyerPage, x.page("/order?id="+order, x.vendorSess)} {
		for _, bad := range []string{"Either you or the other party can sign", "co-signs it on its next pass", "The vendor's claim"} {
			if strings.Contains(page, bad) {
				t.Fatalf("a shipped order's page says %q", bad)
			}
		}
	}
	x.signAs(order, roleVendor, id)
	x.poll()
	if r := x.settleByID(id); r.State != "party_signed" || r.Raw != "" || x.marketCalls() != 0 || x.core.count("finalizepsbt") != 0 || x.state(order) != stateShipped {
		t.Fatalf("the vendor's signature alone: %+v, %d market calls", r, x.marketCalls())
	}
	if page := x.page("/order?id="+order, x.buyerSess); !strings.Contains(page, "the buyer signs the release once") || !strings.Contains(page, `action="/escrow/sign"`) {
		t.Fatalf("after the vendor signed, the buyer's page:\n%s", page)
	}
	x.signAs(order, roleBuyer, id)
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The goods never arrived at my address at all."}}); w.Code != 409 || !strings.Contains(w.Body.String(), "confirms receipt") {
		t.Fatalf("a dispute by the buyer who signed: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	r := x.settleByID(id)
	if r.State != "broadcast" || r.Cosigned != "parties" || x.state(order) != stateCompleted {
		t.Fatalf("both signatures: %+v, order %s", r, x.state(order))
	}
	if x.marketCalls() != 0 || x.core.count("combinepsbt") != 0 {
		t.Fatalf("the market was asked to sign: walletprocesspsbt %d, market calls %d", x.core.count("walletprocesspsbt"), x.marketCalls())
	}
	ev := x.events(order)
	var completed bool
	for _, e := range ev {
		completed = completed || strings.HasPrefix(e, "shipped>completed system ") && strings.Contains(e, "signed the release")
	}
	if !completed || !strings.Contains(strings.Join(ev, "\n"), "so the market did not sign it") {
		t.Fatalf("events:\n%s", strings.Join(ev, "\n"))
	}
	if agInt(x.testEnv, `SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state='shipped' AND to_state='completed'
		AND created=(SELECT final_at FROM settlements WHERE id=$2)`, order, id) != 1 {
		t.Fatal("the order did not complete in the transaction that stored the final transaction")
	}
	if page := x.page("/order?id="+order, x.buyerSess); !strings.Contains(page, "Completed") || strings.Contains(page, `action="/escrow/sign"`) {
		t.Fatal("the completed order's page")
	}

	// Rechecked under the order row lock: both signed, then the vendor opens a dispute before the pass; nothing is
	// stored and the market signs nothing.
	order2, id2 := x.shippedPhysical(regtestAddr("both-refund-2"), regtestAddr("both-claim-2"), 100000)
	x.signAs(order2, roleVendor, id2)
	x.signAs(order2, roleBuyer, id2)
	if w := x.do("POST", "/disputes", x.vendorSess, url.Values{"order_id": {order2}, "reason": {"The buyer asked for a different delivery address now."}}); w.Code != 303 {
		t.Fatalf("vendor's dispute: %d %s", w.Code, w.Body.String())
	}
	x.poll()
	if r := x.settleByID(id2); r.State != "party_signed" || r.Raw != "" || x.state(order2) != stateDisputed || x.marketCalls() != 0 {
		t.Fatalf("disputed before the pass: %+v, order %s", r, x.state(order2))
	}

	// A-376: the parties broadcast the release themselves before the watcher's pass; it is recorded as broadcast and the
	// order completes.
	order3, id3 := x.shippedPhysical(regtestAddr("both-refund-3"), regtestAddr("both-claim-3"), 100000)
	x.signAs(order3, roleVendor, id3)
	x.signAs(order3, roleBuyer, id3)
	x.partiesBroadcast(id3, 0, x.storedSigs(id3, roleVendor), x.storedSigs(id3, roleBuyer))
	x.poll()
	if r := x.settleByID(id3); r.State != "broadcast" || r.Cosigned != "parties" || x.state(order3) != stateCompleted || x.marketCalls() != 0 {
		t.Fatalf("broadcast by the parties: %+v, order %s", r, x.state(order3))
	}
}

// A-372: a party signature that does not verify on a shipped order is discarded, and the market never signs: no
// arbiter call is made, whatever the signatures. Without the market's signature the market cannot tell which of the
// two failed, so both are discarded and the release waits for both again. One signature alone waits.
func TestEscrowShippedNeverMarketSigns(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.shippedPhysical(regtestAddr("never-refund"), regtestAddr("never-claim"), 100000)
	x.signAs(order, roleVendor, id)
	x.poll()
	x.poll()
	if r := x.settleByID(id); r.State != "party_signed" || x.marketCalls() != 0 || x.state(order) != stateShipped {
		t.Fatalf("one signature on a shipped order: %+v, %d market calls", r, x.marketCalls())
	}
	for _, c := range []struct{ name, bad string }{{"the buyer's signature is bad", roleBuyer}, {"the vendor's signature is bad", roleVendor}} {
		t.Run(c.name, func(t *testing.T) {
			order, id := x.shippedPhysical(regtestAddr("never-refund-"+c.bad), regtestAddr("never-claim-"+c.bad), 100000)
			first, second := roleVendor, roleBuyer
			for _, p := range []string{first, second} {
				if p == c.bad {
					x.signBad(order, p, id, badSigs["r=s=1"])
				} else {
					x.signAs(order, p, id)
				}
			}
			x.poll()
			r := x.settleByID(id)
			if r.State != "awaiting_signature" || r.Raw != "" || r.Signer != "" || x.marketCalls() != 0 || x.core.broadcast[r.TxID] != "" {
				t.Fatalf("after the pass: %+v, %d market calls", r, x.marketCalls())
			}
			if agInt(x.testEnv, "SELECT count(*) FROM settlement_signatures WHERE settlement_id=$1", id) != 0 || x.state(order) != stateShipped {
				t.Fatal("a signature was kept, or the order moved")
			}
			ev := strings.Join(x.events(order), "\n")
			if !strings.Contains(ev, "did not verify together") || !strings.Contains(ev, "the market never signs") {
				t.Fatalf("note:\n%s", ev)
			}
			for _, uid := range []string{x.buyer, x.vendor} {
				if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%'||$2||'%did not verify together%'", uid, shortID(order)) != 1 {
					t.Fatal("a party was not told")
				}
			}
			// Good signatures then complete it, still without the market.
			x.signAs(order, roleVendor, id)
			x.signAs(order, roleBuyer, id)
			x.poll()
			if r := x.settleByID(id); r.State != "broadcast" || r.Cosigned != "parties" || x.marketCalls() != 0 || x.state(order) != stateCompleted {
				t.Fatalf("good signatures: %+v", r)
			}
		})
	}
}

// P2-R4, A-371: a dispute over a shipped order decided for the buyer supersedes the pre-built release (no final
// transaction), frees its deposits and requests the refund, all in the resolve transaction; the refund is then built
// from those deposits. A release that already has a final transaction refuses the decision for the buyer (409, saying
// why) and changes nothing.
func TestEscrowDisputeSupersedesRelease(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("rel_admin_"+randomToken()[:6], "admin")
	refund := regtestAddr("sup-refund")
	order, id := x.shippedPhysical(refund, regtestAddr("sup-claim"), 100000)
	x.signAs(order, roleVendor, id)
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The goods never arrived at my address at all."}}); w.Code != 303 {
		t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
	var dispute string
	x.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order).Scan(&dispute)
	if desk := x.page("/moderator", admin); !strings.Contains(desk, "the release to the vendor built when the order was shipped is cancelled") {
		t.Fatalf("the resolve form does not state that the release is cancelled:\n%s", desk)
	}
	if code, body := x.resolve(admin, dispute, "refund", testPassword); code != 303 {
		t.Fatalf("resolve for the buyer: %d %s", code, body)
	}
	if r := x.settleByID(id); r.State != "superseded" {
		t.Fatalf("the release: %+v", r)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id IS NOT NULL", order) != 0 {
		t.Fatal("the release's deposits were not freed")
	}
	sid, kind, state := x.mainSettlement(order)
	if kind != "refund" || state != "building" || x.state(order) != stateResolved {
		t.Fatalf("after resolving: %s %s, order %s", kind, state, x.state(order))
	}
	ev := strings.Join(x.events(order), "\n")
	if !strings.Contains(ev, "The release to the vendor built when the order was shipped is cancelled") || !strings.Contains(ev, "Dispute decided for the buyer. Escrow refund requested") {
		t.Fatalf("events:\n%s", ev)
	}
	x.poll()
	if _, _, state := x.mainSettlement(order); state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id=$2", order, sid) != 1 {
		t.Fatalf("the refund was not built from the freed deposit: %s", state)
	}
	if out := agStrings(t, x.testEnv, "SELECT address FROM settlement_outputs WHERE settlement_id=$1", sid); len(out) != 1 || out[0] != refund {
		t.Fatalf("refund outputs %v", out)
	}
	if x.marketCalls() != 0 {
		t.Fatal("the market signed")
	}

	// A release with a final transaction (here both parties' signatures stored while the order was disputed) is the
	// outcome: deciding for the buyer is refused and changes nothing.
	order2, id2 := x.shippedPhysical(regtestAddr("sup-refund-2"), regtestAddr("sup-claim-2"), 100000)
	order3, id3 := x.shippedPhysical(regtestAddr("sup-refund-3"), regtestAddr("sup-claim-3"), 100000) // before the hand-written final transaction below, which is never sent
	x.signAs(order2, roleVendor, id2)
	if w := x.do("POST", "/disputes", x.vendorSess, url.Values{"order_id": {order2}, "reason": {"The buyer has not replied about the delivery."}}); w.Code != 303 {
		t.Fatalf("dispute: %d", w.Code)
	}
	agExec(x.testEnv, "UPDATE settlements SET state='signed',raw_tx='00',cosigned='parties',final_at=now() WHERE id=$1", id2)
	var dispute2 string
	x.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order2).Scan(&dispute2)
	code, body := x.resolve(admin, dispute2, "refund", testPassword)
	if code != 409 || !strings.Contains(body, "already final") {
		t.Fatalf("resolve for the buyer over a final release: %d %s", code, body)
	}
	if x.state(order2) != stateDisputed || x.settleByID(id2).State != "signed" || agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", order2) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM disputes WHERE order_id=$1 AND status='Open' AND outcome=''", order2) != 1 {
		t.Fatal("the refused decision changed something")
	}

	// Never silent: with no deposit counted any more (its transaction conflicted), the release still ends and the order
	// notes it; there is nothing to refund.
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order3}, "reason": {"The goods never arrived at my address at all."}}); w.Code != 303 {
		t.Fatalf("dispute: %d", w.Code)
	}
	agExec(x.testEnv, "UPDATE payments SET confirmations=-1 WHERE order_id=$1", order3)
	var dispute3 string
	x.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order3).Scan(&dispute3)
	if code, body := x.resolve(admin, dispute3, "refund", testPassword); code != 303 {
		t.Fatalf("resolve for the buyer with nothing counted: %d %s", code, body)
	}
	if x.settleByID(id3).State != "superseded" || agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND state<>'superseded'", order3) != 0 ||
		!strings.Contains(strings.Join(x.events(order3), "\n"), "is cancelled: it was not final") {
		t.Fatal("the release was left live on an order decided for the buyer")
	}
}

// P2-R4: a dispute decided for the vendor reuses the release built at shipment (no new claim, no new address); its
// note says so; the market then co-signs the stored vendor signature, the outcome being reached.
func TestEscrowDisputeVendorReusesRelease(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("rel_admin_"+randomToken()[:6], "admin")
	order, id := x.shippedPhysical(regtestAddr("reuse-refund"), regtestAddr("reuse-claim"), 100000)
	x.signAs(order, roleVendor, id)
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The goods arrived damaged and unusable."}}); w.Code != 303 {
		t.Fatalf("dispute: %d", w.Code)
	}
	var dispute string
	x.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order).Scan(&dispute)
	if code, body := x.resolve(admin, dispute, "release", testPassword); code != 303 {
		t.Fatalf("resolve for the vendor: %d %s", code, body)
	}
	if sid, kind, state := x.mainSettlement(order); sid != id || kind != "release" || state != "party_signed" ||
		agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", order) != 1 {
		t.Fatalf("after resolving: %d %s %s", sid, kind, state)
	}
	ev := strings.Join(x.events(order), "\n")
	if !strings.Contains(ev, "Dispute decided for the vendor: the release to the vendor built when the order was shipped pays") || strings.Contains(ev, "with a fresh address") {
		t.Fatalf("events:\n%s", ev)
	}
	page := x.page("/order?id="+order, x.vendorSess)
	if strings.Contains(page, `action="/escrow/claim"`) || strings.Contains(page, "the vendor claims the escrowed coins") {
		t.Fatalf("the vendor is asked for a new claim:\n%s", page)
	}
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || r.Cosigned != "market" || r.Signer != roleVendor {
		t.Fatalf("after the pass: %+v", r)
	}
}

// P2-R6, A-374: a deposit that arrives once the release is built (past building) is left out of it: marked
// escrow_left_out, announced once (an order note, a notice to the buyer and to staff), and returned by the stray refund
// once the order is closed. A top-up while the order is paid, before the release exists, is spent by the release.
func TestEscrowTopUpWhileShipped(t *testing.T) {
	x := newEscrowEnv(t)
	modID, modSess := x.user("rel_mod_"+randomToken()[:6], "moderator")
	refund := regtestAddr("topup-refund")
	order := x.paidPhysical(refund, 100000)
	addr := physString(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)
	x.core.deposit(addr, txidOf("topup-paid"), 0, 20000, 3)
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out", order) != 0 {
		t.Fatal("a top-up while paid, before any release, was left out")
	}
	if code, body := x.shipEscrow(order, x.vendorSess, regtestAddr("topup-claim"), testPassword); code != 303 {
		t.Fatalf("ship: %d %s", code, body)
	}
	x.poll()
	id, _, state, _ := x.settlement(order)
	if state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id=$2", order, id) != 2 {
		t.Fatalf("the release does not spend both deposits: %s", state)
	}
	// A top-up while shipped.
	x.core.deposit(addr, txidOf("topup-shipped"), 0, 30000, 3)
	x.poll()
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out AND txid=$2", order, txidOf("topup-shipped")) != 1 {
		t.Fatal("the top-up while shipped was not left out")
	}
	// QA462E-2: the note names the top-up, so the desk dates its open flag; the buyer's notice asks nothing of staff.
	if row := deskRow(x.page("/moderator", modSess), txidOf("topup-shipped")); !flaggedAt.MatchString(row) || !strings.Contains(row, "<strong>Open:</strong> Escrow deposit left out of its settlement") {
		t.Fatalf("the left-out top-up on the desk: %s\nevents %q", row, x.events(order))
	}
	if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,'Moderator review required')>0", x.buyer) != 0 {
		t.Fatal("the buyer's notice of the top-up asks for a moderator review")
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%after its release to the vendor was built%'", order); n != 1 {
		t.Fatalf("%d notes", n)
	}
	for who, uid := range map[string]string{"buyer": x.buyer, "staff": modID} {
		if n := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%after its release to the vendor was built%'", uid); n != 1 {
			t.Fatalf("%s told %d times", who, n)
		}
	}
	if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%after its release to the vendor was built%'", x.vendor) != 0 {
		t.Fatal("the vendor was told about the buyer's refund")
	}
	// Flagged like a deposit after the order closed, so the staff member told of it can read the order.
	if page := x.page("/order?id="+order, modSess); !strings.Contains(page, txidOf("topup-shipped")) {
		t.Fatal("the moderator told of the top-up cannot read the order")
	}
	// The order is open, so its page says the stray refund comes once it is closed (QA428-5).
	if page := x.page("/order?id="+order, x.buyerSess); !strings.Contains(page, "by a stray refund once the order is closed and it is confirmed") {
		t.Fatalf("buyer page of the open order with a top-up:\n%s", page)
	}
	// The order completes by both signatures; the stray refund then returns the top-up to the refund address.
	x.signAs(order, roleVendor, id)
	x.signAs(order, roleBuyer, id)
	x.poll()
	if x.state(order) != stateCompleted {
		t.Fatalf("order %s", x.state(order))
	}
	x.poll()
	stray, sstate := x.stray(order)
	if stray == 0 || sstate != "awaiting_signature" {
		t.Fatalf("stray refund %d %s", stray, sstate)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND settlement_id=$2 AND txid=$3", order, stray, txidOf("topup-shipped")) != 1 {
		t.Fatal("the stray refund does not spend the top-up")
	}
	if out := agStrings(t, x.testEnv, "SELECT address FROM settlement_outputs WHERE settlement_id=$1", stray); len(out) != 1 || out[0] != refund {
		t.Fatalf("stray refund outputs %v", out)
	}
}

// Migration 081: the database refuses a final transaction co-signed by the market unless the order's state fits the
// settlement's kind; one completed by both parties is stored on a shipped order. The same holds for a fee bump.
func TestEscrowMigration081Guard(t *testing.T) {
	x := newEscrowEnv(t)
	// Every order first (each takes watcher passes); the hand-written final transactions below are never sent.
	order, id := x.shippedPhysical(regtestAddr("g81-refund"), regtestAddr("g81-claim"), 100000)
	order2, id2 := x.shippedPhysical(regtestAddr("g81-refund-2"), regtestAddr("g81-claim-2"), 100000)
	refundOrder := x.paidPhysical(regtestAddr("g81-refund-3"), 100000)
	x.signAs(order, roleVendor, id)
	x.signAs(order2, roleVendor, id2)
	store := func(cosigned string) error {
		_, err := x.DB.Exec("UPDATE settlements SET state='signed',raw_tx='00',cosigned=$2,final_at=now() WHERE id=$1", id, cosigned)
		return err
	}
	if err := store("market"); err == nil || !strings.Contains(err.Error(), "the market co-signs") {
		t.Fatalf("a market final transaction on a shipped order: %v", err)
	}
	agExec(x.testEnv, "UPDATE orders SET state='disputed' WHERE id=$1", order)
	agExec(x.testEnv, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The goods never arrived at my address.')", randomToken(), order)
	if err := store("market"); err == nil {
		t.Fatal("a market final transaction on a disputed order was stored")
	}
	agExec(x.testEnv, "UPDATE disputes SET outcome='refund',status='Resolved — refund to buyer' WHERE order_id=$1", order)
	agExec(x.testEnv, "UPDATE orders SET state='resolved' WHERE id=$1", order)
	if err := store("market"); err == nil {
		t.Fatal("a market release on an order resolved for the buyer was stored")
	}
	agExec(x.testEnv, "UPDATE disputes SET outcome='release',status='Resolved — release to vendor' WHERE order_id=$1", order)
	if err := store("market"); err != nil {
		t.Fatalf("a market release on an order resolved for the vendor: %v", err)
	}
	// On a shipped order both parties' final transaction is stored.
	if _, err := x.DB.Exec("UPDATE settlements SET state='signed',raw_tx='00',cosigned='parties',final_at=now() WHERE id=$1", id2); err != nil {
		t.Fatalf("the parties' final transaction on a shipped order: %v", err)
	}
	// A refund co-signed by the market needs a cancelled order or one decided for the buyer.
	agExec(x.testEnv, "INSERT INTO settlements(order_id,kind,destination,state,psbt,txid,fee_rate,network_fee,input_total,signer,signed_at) VALUES($1,'refund',$2,'party_signed','cHNidP8=',$3,2,500,100000,'buyer',now())",
		refundOrder, regtestAddr("g81-refund-3"), txidOf("g81-refund"))
	rid := int64(agInt(x.testEnv, "SELECT id FROM settlements WHERE order_id=$1", refundOrder))
	if _, err := x.DB.Exec("UPDATE settlements SET state='signed',raw_tx='00',cosigned='market',final_at=now() WHERE id=$1", rid); err == nil {
		t.Fatal("a market refund on a paid order was stored")
	}
	agExec(x.testEnv, "UPDATE orders SET state='cancelled' WHERE id=$1", refundOrder)
	if _, err := x.DB.Exec("UPDATE settlements SET state='signed',raw_tx='00',cosigned='market',final_at=now() WHERE id=$1", rid); err != nil {
		t.Fatalf("a market refund on a cancelled order: %v", err)
	}
	// A fee bump of the release broadcast by both parties on a shipped order: never co-signed by the market.
	agExec(x.testEnv, "UPDATE settlements SET state='broadcast',broadcast_at=now() WHERE id=$1", id2)
	agExec(x.testEnv, "INSERT INTO settlement_bumps(settlement_id,n,requested_by,fee_rate) VALUES($1,1,$2,5)", id2, x.vendor)
	agExec(x.testEnv, "UPDATE settlement_bumps SET state='awaiting_signature',psbt='cHNidP8=',txid=$2,network_fee=900 WHERE settlement_id=$1", id2, txidOf("g81-bump"))
	agExec(x.testEnv, "UPDATE settlement_bumps SET state='party_signed',signer='vendor',signed_at=now() WHERE settlement_id=$1", id2)
	if _, err := x.DB.Exec("UPDATE settlement_bumps SET state='signed',raw_tx='00',cosigned='market',final_at=now() WHERE settlement_id=$1", id2); err == nil || !strings.Contains(err.Error(), "the market co-signs fee bump") {
		t.Fatalf("a market fee bump on a shipped order: %v", err)
	}
	if _, err := x.DB.Exec("UPDATE settlement_bumps SET state='signed',raw_tx='00',cosigned='parties',final_at=now() WHERE settlement_id=$1", id2); err != nil {
		t.Fatalf("the parties' fee bump on a shipped order: %v", err)
	}
}
