package market

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-462 (QA437-1): an administrator changes a dispute decision whose settlement was never built: still building
// (waiting for a deposit to confirm, or not yet tried) or failed. Migration 087 lets such a release or refund be
// superseded (before it, migration 078's settlements_built refused a superseded row with no PSBT and the change
// answered 500). A refund whose too-small failure left its deposits out for a stray refund (leaveOutTooFew) gives them
// back to the order, so the release spends them and no stray refund returns the vendor's coins. Simulated RPC.

// decisionRows is every release and refund of order, oldest first: kind, state, build error, the value it could spend
// (input_total, 0 when none) and whether it has a PSBT.
func (x *escrowEnv) decisionRows(order string) []string {
	return agStrings(x.t, x.testEnv, `SELECT kind||' '||state||' '||build_error||' '||COALESCE(input_total,0)||' '||(psbt IS NOT NULL) FROM settlements
		WHERE order_id=$1 AND kind IN ('release','refund') ORDER BY id`, order)
}

// decisionDeposits is every deposit of order by amount: amount, flagged, left out, whether a settlement spends it.
func (x *escrowEnv) decisionDeposits(order string) []string {
	return agStrings(x.t, x.testEnv, "SELECT amount||' '||flagged||' '||escrow_left_out||' '||(settlement_id IS NOT NULL) FROM payments WHERE order_id=$1 ORDER BY amount", order)
}

// countedAgain is the order note on n deposits worth sats that a changed decision counts for the order again (A-462).
func countedAgain(n int, sats int64) string {
	count, is, it := "1 deposit", "is", "it"
	if n > 1 {
		count, is, it = strconv.Itoa(n)+" deposits", "are", "them"
	}
	return count + " (" + amount(sats, 8) + " BTC, TESTNET regtest) to this order's escrow, left out for a stray refund when the refund of the earlier decision could not be built because the coins were too few, " +
		is + " counted for the order again: the release to the vendor includes " + it + ", and no stray refund returns " + it + "."
}

func TestEscrowDecisionChangeUnbuilt(t *testing.T) {
	x := newEscrowEnv(t)
	x.user("emod_"+randomToken()[:6], "moderator")
	_, admin := x.user("eadmin_"+randomToken()[:6], "admin")
	changed := func(leg, order, outcome string) {
		t.Helper()
		if page := x.page("/order?id="+order, admin); !strings.Contains(page, `action="/escrow/decision"`) {
			t.Fatalf("%s: the change is not offered:\n%s", leg, page)
		}
		if code, body := x.changeDecision(admin, order, outcome, testPassword); code != 303 {
			t.Fatalf("%s: POST /escrow/decision: %d %s", leg, code, strings.TrimSpace(body))
		}
		var got string
		x.DB.QueryRow("SELECT outcome FROM disputes WHERE order_id=$1", order).Scan(&got)
		if got != outcome {
			t.Fatalf("%s: the decision is %q", leg, got)
		}
	}
	// releaseBuilt has the vendor claim and checks the release the next pass builds: it spends n deposits worth sats.
	releaseBuilt := func(leg, order string, n int, sats int64) {
		t.Helper()
		if code, body := x.claim(order, x.vendorSess, regtestAddr("unbuilt claim "+leg)); code != 303 {
			t.Fatalf("%s: claim: %d %s", leg, code, strings.TrimSpace(body))
		}
		x.poll()
		id, kind, state := x.mainSettlement(order)
		if kind != "release" || state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", id) != n ||
			agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE id=$1 AND input_total=$2", id, sats) != 1 {
			t.Fatalf("%s: the release: %s %s; settlements %q, deposits %q; events %q", leg, kind, state, x.decisionRows(order), x.decisionDeposits(order), x.events(order))
		}
	}

	// A: decided for the buyer, the refund failed too-small and left its deposit out; a deposit Core reported as
	// conflicted then does not count. Changed to a release: the refund is superseded, the deposit counts again (no
	// stray refund returns it), and once the conflicted deposit is mined it counts for the release too (the coins are
	// the vendor's now), so the release spends both.
	refund := regtestAddr("unbuilt failed")
	order, dispute, addr, big := x.smallDispute(refund, 10000, 500000)
	x.decide(admin, dispute, "refund", 10000)
	x.poll()
	if got := x.decisionRows(order); !slices.Equal(got, []string{"refund failed " + settleTooSmall + " 10000 false"}) {
		t.Fatalf("A: before the change: %q", got)
	}
	if got := x.decisionDeposits(order); !slices.Equal(got, []string{"10000 true true false", "500000 false false false"}) {
		t.Fatalf("A: before the change: deposits %q", got)
	}
	changed("A", order, "release")
	if got := x.decisionRows(order); !slices.Equal(got, []string{"refund superseded " + settleTooSmall + " 10000 false"}) {
		t.Fatalf("A: after the change: %q", got)
	}
	if got := x.decisionDeposits(order); !slices.Equal(got, []string{"10000 false false false", "500000 false false false"}) {
		t.Fatalf("A: after the change: deposits %q", got)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2 AND actor_id IS NULL", order, countedAgain(1, 10000)) != 1 {
		t.Fatalf("A: no note on the deposit counted again: %q", x.events(order))
	}
	if page := x.page("/order?id="+order, x.buyerSess); strings.Contains(page, "Left out") || strings.Contains(page, decidedTooFew) {
		t.Fatalf("A: the buyer's page still shows the coins left out:\n%s", page)
	}
	events := len(x.events(order))
	for range 3 {
		x.poll()
	}
	if id, state := x.stray(order); id != 0 || len(x.events(order)) != events {
		t.Fatalf("A: a stray refund %d %s of the vendor's coins, or new notes; events %q", id, state, x.events(order))
	}
	// A deposit after the close is the buyer's: the superseded refund's failure holds nothing, so a stray refund of it
	// alone is requested (it tries 4000 sats, and fails too-small on its own).
	x.core.deposit(addr, txidOf("unbuilt after close "+order), 0, 4000, 3)
	x.poll()
	if got := agStrings(t, x.testEnv, "SELECT COALESCE(input_total,0)::text FROM settlements WHERE order_id=$1 AND kind='stray_refund'", order); !slices.Equal(got, []string{"4000"}) {
		t.Fatalf("A: a deposit after the close: stray refunds trying %q; events %q", got, x.events(order))
	}
	x.core.setConfs(big, 3)
	x.poll()
	if n, b, s := x.uncounted(order); n+b+s != 0 {
		t.Fatalf("A: the deposit mined since was left out for the buyer: notes %d, buyer %d, staff %d", n, b, s)
	}
	// The release tries both deposits; the vendor's output (the 10000-sat price less the fees; the rest goes back to
	// the buyer) is below the floor, so it fails too-small in turn, and both stay counted for the vendor.
	if code, body := x.claim(order, x.vendorSess, regtestAddr("unbuilt claim A")); code != 303 {
		t.Fatalf("A: claim: %d %s", code, strings.TrimSpace(body))
	}
	x.poll()
	if got := x.decisionRows(order); !slices.Equal(got, []string{"refund superseded " + settleTooSmall + " 10000 false", "release failed " + settleTooSmall + " 510000 false"}) {
		t.Fatalf("A: the release: %q", got)
	}
	if got := x.decisionDeposits(order); !slices.Equal(got, []string{"4000 true false false", "10000 false false false", "500000 false false false"}) {
		t.Fatalf("A: after the release: deposits %q", got)
	}

	// A3: the same at 100 sat/vB, where 25000 sats are too few for the refund; back at the default rate, the change
	// counts the deposit again and the vendor's release is built from it.
	e := x.A.escrow.Load()
	t.Cleanup(func() { e.feeRate = escrowDefaultFeeRate })
	e.feeRate = escrowMaxFeeRate
	order, dispute, _, _ = x.smallDispute(regtestAddr("unbuilt failed rate"), 25000, 0)
	x.decide(admin, dispute, "refund", 25000)
	x.poll()
	if got := x.decisionDeposits(order); !slices.Equal(got, []string{"25000 true true false"}) {
		t.Fatalf("A3: before the change: deposits %q; settlements %q", got, x.decisionRows(order))
	}
	e.feeRate = escrowDefaultFeeRate
	changed("A3", order, "release")
	if got := x.decisionDeposits(order); !slices.Equal(got, []string{"25000 false false false"}) {
		t.Fatalf("A3: after the change: deposits %q", got)
	}
	releaseBuilt("A3", order, 1, 25000)
	if id, state := x.stray(order); id != 0 {
		t.Fatalf("A3: a stray refund %d %s of the vendor's coins", id, state)
	}

	// A4: after the refund failed too-small, the deposit Core reported as conflicted when the dispute was decided is
	// mined and left out as one that did not count (A-428, flagged too); the restore gate keeps the stray refund it
	// requests from being built. The change counts again only the deposit the too-small failure left out; the other
	// stays for the stray refund, which returns it alone once the gate is lifted.
	order, dispute, _, big = x.smallDispute(regtestAddr("unbuilt uncounted"), 10000, 500000)
	x.decide(admin, dispute, "refund", 10000)
	x.poll()
	x.setGate(true)
	x.core.setConfs(big, 3)
	x.poll()
	if got := x.decisionDeposits(order); !slices.Equal(got, []string{"10000 true true false", "500000 true true false"}) {
		t.Fatalf("A4: before the change: deposits %q; events %q", got, x.events(order))
	}
	changed("A4", order, "release")
	if got := x.decisionDeposits(order); !slices.Equal(got, []string{"10000 false false false", "500000 true true false"}) {
		t.Fatalf("A4: after the change: deposits %q", got)
	}
	x.setGate(false)
	x.poll()
	sid, _ := x.stray(order)
	if sid == 0 || agStrings(t, x.testEnv, "SELECT COALESCE(string_agg(amount::text,','),'') FROM payments WHERE settlement_id=$1", sid)[0] != "500000" {
		t.Fatalf("A4: the deposit that did not count: stray refund %d; deposits %q; events %q", sid, x.decisionDeposits(order), x.events(order))
	}

	// A2: the refund waited for a deposit still confirming, built without it once the wait was over (A-342: that
	// deposit is left out, not flagged) and failed too-small on the rest. Changed to a release, only the deposit the
	// too-small failure left out counts again; the other stays left out for a stray refund.
	order, dispute, addr, _ = x.smallDispute(regtestAddr("unbuilt other"), 10000, 0)
	stuck := txidOf("unbuilt stuck " + order)
	x.core.deposit(addr, stuck, 0, 50000, 1)
	x.poll()
	x.decide(admin, dispute, "refund", 60000)
	x.poll()
	rid, _, state, class := x.settlement(order)
	if state != "building" || class != settleWaiting {
		t.Fatalf("A2: the refund waits: %s %s", state, class)
	}
	x.DB.Exec("UPDATE settlements SET created=now()-interval '25 hours' WHERE id=$1", rid)
	x.poll()
	if got := x.decisionDeposits(order); !slices.Equal(got, []string{"10000 true true false", "50000 false true false"}) {
		t.Fatalf("A2: before the change: deposits %q; settlements %q; events %q", got, x.decisionRows(order), x.events(order))
	}
	changed("A2", order, "release")
	if got := x.decisionDeposits(order); !slices.Equal(got, []string{"10000 false false false", "50000 false true false"}) {
		t.Fatalf("A2: after the change: deposits %q", got)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2", order, countedAgain(1, 10000)) != 1 {
		t.Fatalf("A2: the note: %q", x.events(order))
	}
	x.core.setConfs(stuck, 3)
	x.poll()
	sid, _ = x.stray(order)
	if sid == 0 || agStrings(t, x.testEnv, "SELECT COALESCE(string_agg(amount::text,','),'') FROM payments WHERE settlement_id=$1", sid)[0] != "50000" {
		t.Fatalf("A2: the deposit left out for another reason: stray refund %d; deposits %q", sid, x.decisionDeposits(order))
	}

	// U: a database from before A-437 (the too-small refund left its deposit counted): the next pass leaves it out with
	// the too-small note (leaveOutUncounted), naming the refund, so a change counts it again as well.
	order, dispute, _, _ = x.smallDispute(regtestAddr("unbuilt upgraded"), 10000, 0)
	x.decide(admin, dispute, "refund", 10000)
	x.poll()
	rid, _, _, _ = x.settlement(order)
	x.DB.Exec("UPDATE payments SET escrow_left_out=false,flagged=false,escrow_too_few_by=NULL WHERE order_id=$1", order)
	x.poll()
	if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out AND flagged AND escrow_too_few_by=$2", order, rid); n != 1 {
		t.Fatalf("U: the upgraded deposit left out naming the refund: %d; deposits %q", n, x.decisionDeposits(order))
	}
	changed("U", order, "release")
	if got := x.decisionDeposits(order); !slices.Equal(got, []string{"10000 false false false"}) {
		t.Fatalf("U: after the change: deposits %q", got)
	}

	// B: decided for the buyer, the refund is building and waits for a deposit still confirming. Changed to a release:
	// superseded with no PSBT; once that deposit confirms the release spends both.
	order, dispute, addr, _ = x.smallDispute(regtestAddr("unbuilt building"), 100000, 0)
	confirming := txidOf("unbuilt confirming " + order)
	x.core.deposit(addr, confirming, 0, 20000, 1)
	x.poll()
	x.decide(admin, dispute, "refund", 120000)
	x.poll()
	if _, _, state, class := x.settlement(order); state != "building" || class != settleWaiting {
		t.Fatalf("B: the refund waits: %s %s", state, class)
	}
	changed("B", order, "release")
	if got := x.decisionRows(order); !slices.Equal(got, []string{"refund superseded " + settleWaiting + " 0 false"}) {
		t.Fatalf("B: after the change: %q", got)
	}
	x.core.setConfs(confirming, 3)
	x.poll()
	releaseBuilt("B", order, 2, 120000)

	// C: decided for the buyer and changed at once, before the watcher's next pass builds the refund.
	order, dispute = x.disputedEscrow(regtestAddr("unbuilt at once"))
	x.decide(admin, dispute, "refund", 100000)
	changed("C", order, "release")
	if got := x.decisionRows(order); !slices.Equal(got, []string{"refund superseded  0 false"}) {
		t.Fatalf("C: after the change: %q", got)
	}
	x.poll()
	if got := x.decisionRows(order); !slices.Equal(got, []string{"refund superseded  0 false"}) {
		t.Fatalf("C: a superseded refund was built: %q", got)
	}
	releaseBuilt("C", order, 1, 100000)

	// R1, the reverse: decided for the vendor, the release is building and waits for a deposit still confirming.
	// Changed to a refund: the release is superseded and the refund requested; once that deposit confirms the refund
	// spends both, to the refund address pinned at checkout.
	refund = regtestAddr("unbuilt release building")
	order, dispute, addr, _ = x.smallDispute(refund, 100000, 0)
	confirming = txidOf("unbuilt release confirming " + order)
	x.core.deposit(addr, confirming, 0, 20000, 1)
	x.poll()
	x.decide(admin, dispute, "release", 120000)
	if code, body := x.claim(order, x.vendorSess, regtestAddr("unbuilt release claim")); code != 303 {
		t.Fatalf("R1: claim: %d %s", code, strings.TrimSpace(body))
	}
	x.poll()
	if _, kind, state := x.mainSettlement(order); kind != "release" || state != "building" {
		t.Fatalf("R1: the release: %s %s", kind, state)
	}
	changed("R1", order, "refund")
	if got := x.decisionRows(order); !slices.Equal(got, []string{"release superseded " + settleWaiting + " 0 false", "refund building  0 false"}) {
		t.Fatalf("R1: after the change: %q", got)
	}
	x.core.setConfs(confirming, 3)
	x.poll()
	id, kind, state := x.mainSettlement(order)
	if kind != "refund" || state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", id) != 2 ||
		agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE id=$1 AND destination=$2 AND input_total=120000", id, refund) != 1 {
		t.Fatalf("R1: the refund: %s %s; settlements %q", kind, state, x.decisionRows(order))
	}

	// R2, the reverse: decided for the vendor, the release failed too-small (its deposit stays counted). Changed to a
	// refund: the release is superseded and the refund requested; it fails too-small in turn and leaves the deposit
	// out for a stray refund (A-437).
	order, dispute = x.disputedEscrow(regtestAddr("unbuilt release failed"))
	x.decide(admin, dispute, "release", 100000)
	x.DB.Exec("UPDATE payments SET amount=5000 WHERE order_id=$1", order)
	if code, body := x.claim(order, x.vendorSess, regtestAddr("unbuilt release failed claim")); code != 303 {
		t.Fatalf("R2: claim: %d %s", code, strings.TrimSpace(body))
	}
	x.poll()
	if got := x.decisionRows(order); !slices.Equal(got, []string{"release failed " + settleTooSmall + " 5000 false"}) {
		t.Fatalf("R2: before the change: %q", got)
	}
	changed("R2", order, "refund")
	if got := x.decisionRows(order); !slices.Equal(got, []string{"release superseded " + settleTooSmall + " 5000 false", "refund building  0 false"}) {
		t.Fatalf("R2: after the change: %q", got)
	}
	x.poll()
	if got := x.decisionRows(order); !slices.Equal(got, []string{"release superseded " + settleTooSmall + " 5000 false", "refund failed " + settleTooSmall + " 5000 false"}) {
		t.Fatalf("R2: the refund: %q", got)
	}
	if got := x.decisionDeposits(order); !slices.Equal(got, []string{"5000 true true false"}) {
		t.Fatalf("R2: deposits %q", got)
	}

	// P: the same rule on the first decision: a physical order's release requested when it was shipped and not built
	// yet is superseded by a decision for the buyer (A-371's path; before migration 087 that decision failed too).
	order = x.paidPhysical(regtestAddr("unbuilt physical"), 100000)
	if code, body := x.shipEscrow(order, x.vendorSess, regtestAddr("unbuilt physical claim"), testPassword); code != 303 {
		t.Fatalf("P: ship: %d %s", code, strings.TrimSpace(body))
	}
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The parcel never arrived at my address."}}); w.Code != 303 {
		t.Fatalf("P: dispute: %d %s", w.Code, w.Body.String())
	}
	if code, body := x.resolve(admin, x.disputeID(order), "refund", testPassword); code != 303 {
		t.Fatalf("P: resolve for the buyer with the release still building: %d %s", code, strings.TrimSpace(body))
	}
	if got := x.decisionRows(order); !slices.Equal(got, []string{"release superseded  0 false", "refund building  0 false"}) {
		t.Fatalf("P: after the decision: %q", got)
	}
	x.poll()
	if _, kind, state := x.mainSettlement(order); kind != "refund" || state != "awaiting_signature" {
		t.Fatalf("P: the refund: %s %s", kind, state)
	}
}

// Migration 087: a release or refund that was never built (building or failed, no PSBT) may be superseded, and keeps
// what it had (a failed one keeps its build error, the value it could spend and its fee rate); a superseded row never
// gains or loses a PSBT, and one with a PSBT keeps all of it. Everything else migrations 075-078 refuse stays refused.
func TestEscrowMigration087SupersedeUnbuilt(t *testing.T) {
	x := newEscrowEnv(t)
	refused := func(what, q string, args ...any) {
		t.Helper()
		if _, err := x.DB.Exec(q, args...); err == nil {
			t.Errorf("the database allowed %s: %s", what, q)
		}
	}
	// Building, never tried: superseded without a PSBT.
	order := x.completed(regtestAddr("m87 building"), 100000)
	if code, body := x.claim(order, x.vendorSess, regtestAddr("m87 claim")); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	id, _, state, _ := x.settlement(order)
	if state != "building" {
		t.Fatalf("claim %s", state)
	}
	refused("superseding a settlement while giving it a PSBT", "UPDATE settlements SET state='superseded',psbt='cHNidP8=',txid=$2,fee_rate=1,network_fee=1,input_total=1 WHERE id=$1", id, txidOf("m87 built"))
	if _, err := x.DB.Exec("UPDATE settlements SET state='superseded' WHERE id=$1", id); err != nil {
		t.Fatalf("building -> superseded without a PSBT refused: %v", err)
	}
	for what, q := range map[string]string{
		"a PSBT on a superseded settlement":         "UPDATE settlements SET psbt='cHNidP8=' WHERE id=$1",
		"a transaction ID on a superseded one":      "UPDATE settlements SET txid='" + txidOf("m87") + "' WHERE id=$1",
		"a signer on a never-built superseded one":  "UPDATE settlements SET signer='buyer' WHERE id=$1",
		"a superseded settlement back to building":  "UPDATE settlements SET state='building' WHERE id=$1",
		"a superseded settlement back to failed":    "UPDATE settlements SET state='failed' WHERE id=$1",
		"a superseded settlement to awaiting":       "UPDATE settlements SET state='awaiting_signature' WHERE id=$1",
		"a superseded settlement to external":       "UPDATE settlements SET state='external' WHERE id=$1",
		"a superseded settlement's network fee set": "UPDATE settlements SET network_fee=1 WHERE id=$1",
	} {
		refused(what, q, id)
	}

	// Failed: superseded, keeping its build error, value and fee rate.
	order2 := x.completed(regtestAddr("m87 failed"), 100000)
	if code, body := x.claim(order2, x.vendorSess, regtestAddr("m87 claim failed")); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	fid, _, _, _ := x.settlement(order2)
	agExec(x.testEnv, "UPDATE settlements SET state='failed',build_error=$2,input_total=5000,fee_rate=2 WHERE id=$1", fid, settleTooSmall)
	refused("superseding a failed settlement while dropping its value", "UPDATE settlements SET state='superseded',input_total=NULL WHERE id=$1", fid)
	if _, err := x.DB.Exec("UPDATE settlements SET state='superseded' WHERE id=$1", fid); err != nil {
		t.Fatalf("failed -> superseded refused: %v", err)
	}
	if got := agStrings(t, x.testEnv, "SELECT state||' '||build_error||' '||input_total||' '||fee_rate FROM settlements WHERE id=$1", fid); !slices.Equal(got, []string{"superseded " + settleTooSmall + " 5000 2"}) {
		t.Fatalf("the superseded failed settlement: %q", got)
	}
	// A superseded row with no PSBT may not carry a fee or a transaction ID: an insert is refused.
	refused("a superseded row with a transaction ID and no PSBT", "INSERT INTO settlements(order_id,kind,state,destination,txid) VALUES($1,'refund','superseded',$2,$3)", order2, regtestAddr("m87 insert"), txidOf("m87 insert"))
	refused("a superseded row with a network fee and no PSBT", "INSERT INTO settlements(order_id,kind,state,destination,network_fee) VALUES($1,'refund','superseded',$2,1)", order2, regtestAddr("m87 insert fee"))
	// A stray refund is never superseded, built or not (migration 076).
	agExec(x.testEnv, "INSERT INTO settlements(order_id,kind,destination) VALUES($1,'stray_refund',$2)", order2, regtestAddr("m87 stray"))
	refused("a stray refund superseded", "UPDATE settlements SET state='superseded' WHERE order_id=$1 AND kind='stray_refund'", order2)

	// Built, waiting for a signature: superseded keeps its PSBT and everything with it, and never loses it.
	order3, bid := x.built(regtestAddr("m87 built"), regtestAddr("m87 claim built"), 100000)
	before := agStrings(t, x.testEnv, "SELECT md5(psbt)||' '||txid||' '||fee_rate||' '||network_fee||' '||input_total||' '||destination FROM settlements WHERE id=$1", bid)
	for what, q := range map[string]string{
		"superseding a built settlement while dropping its PSBT":           "UPDATE settlements SET state='superseded',psbt=NULL WHERE id=$1",
		"superseding a built settlement while dropping its whole build":    "UPDATE settlements SET state='superseded',psbt=NULL,txid=NULL,fee_rate=NULL,network_fee=NULL,input_total=NULL WHERE id=$1",
		"superseding a built settlement while changing its PSBT":           "UPDATE settlements SET state='superseded',psbt='cHNidP8=' WHERE id=$1",
		"superseding a built settlement while changing its value":          "UPDATE settlements SET state='superseded',input_total=input_total+1 WHERE id=$1",
		"superseding a built settlement while changing its destination":    "UPDATE settlements SET state='superseded',destination='" + regtestAddr("m87 other") + "' WHERE id=$1",
		"superseding a built settlement while dropping its transaction ID": "UPDATE settlements SET state='superseded',txid=NULL WHERE id=$1",
	} {
		refused(what, q, bid)
	}
	if _, err := x.DB.Exec("UPDATE settlements SET state='superseded' WHERE id=$1", bid); err != nil {
		t.Fatalf("awaiting_signature -> superseded refused: %v", err)
	}
	for what, q := range map[string]string{
		"a superseded built settlement losing its PSBT":           "UPDATE settlements SET psbt=NULL,txid=NULL,fee_rate=NULL,network_fee=NULL,input_total=NULL WHERE id=$1",
		"a superseded built settlement losing its transaction ID": "UPDATE settlements SET txid=NULL WHERE id=$1",
		"a superseded built settlement back to awaiting":          "UPDATE settlements SET state='awaiting_signature' WHERE id=$1",
	} {
		refused(what, q, bid)
	}
	if after := agStrings(t, x.testEnv, "SELECT md5(psbt)||' '||txid||' '||fee_rate||' '||network_fee||' '||input_total||' '||destination FROM settlements WHERE id=$1", bid); !slices.Equal(after, before) {
		t.Fatalf("the superseded built settlement changed: %q -> %q", before, after)
	}
	// One live release or refund per order still (settlements_one_outcome): with the failed release superseded a
	// refund may be requested, and a second live row beside it is refused.
	agExec(x.testEnv, "INSERT INTO settlements(order_id,kind,destination) VALUES($1,'refund',$2)", order2, regtestAddr("m87 refund"))
	refused("a second live release or refund", "INSERT INTO settlements(order_id,kind,destination) VALUES($1,'release',$2)", order2, regtestAddr("m87 second"))
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind IN ('release','refund')", order3); n != 1 {
		t.Fatalf("order3: %d releases or refunds", n)
	}
}
