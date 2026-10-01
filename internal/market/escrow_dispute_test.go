package market

import (
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Escrow Phase 1, slice P1-c2: dispute decisions on escrow orders (ES-4, plan P1-c). A dispute is no longer refused;
// only an administrator decides it, with their password (and authenticator code when enrolled); the decision builds
// the settlement of that kind (a refund at once; a release once the vendor gives the claim address); an
// administrator may change it until its settlement has a final transaction, never after. Simulated RPC.

// disputedEscrow is a funded escrow order whose content was not delivered automatically, disputed by its buyer: the
// order and the dispute's id.
func (x *escrowEnv) disputedEscrow(refund string) (string, string) {
	x.t.Helper()
	order := x.draft()
	x.mustCheckout(order, refund)
	x.DB.Exec("UPDATE products SET delivery_content='' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order)
	x.fund(order, order, 3, 100000)
	if x.state(order) != statePaid {
		x.t.Fatalf("funded order %s", x.state(order))
	}
	w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The content was never delivered to me at all."}})
	if w.Code != 303 || x.state(order) != stateDisputed {
		x.t.Fatalf("dispute: %d %s (%s)", w.Code, w.Body.String(), x.state(order))
	}
	var id string
	if err := x.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order).Scan(&id); err != nil {
		x.t.Fatal(err)
	}
	return order, id
}

func (x *escrowEnv) resolve(sess, dispute, outcome, password string) (int, string) {
	w := x.do("POST", "/resolve", sess, url.Values{"id": {dispute}, "outcome": {outcome}, "resolution": {"Decided from the evidence both parties sent."},
		"payout_confirmed": {"confirmed"}, "amount_seen": {"100000"}, "reply_seen": {"0"}, "password": {password}})
	return w.Code, w.Body.String()
}

func (x *escrowEnv) changeDecision(sess, order, outcome, password string) (int, string) {
	w := x.do("POST", "/escrow/decision", sess, url.Values{"order_id": {order}, "outcome": {outcome}, "reason": {"New evidence showed the content was delivered."}, "password": {password}})
	return w.Code, w.Body.String()
}

// mainSettlement is order's release or refund that is not superseded.
func (x *escrowEnv) mainSettlement(order string) (int64, string, string) {
	x.t.Helper()
	var id int64
	var kind, state string
	if err := x.DB.QueryRow("SELECT id,kind,state FROM settlements WHERE order_id=$1 AND kind IN ('release','refund') AND state<>'superseded'", order).Scan(&id, &kind, &state); err != nil {
		x.t.Fatalf("settlement of %s: %v", order, err)
	}
	return id, kind, state
}

func TestEscrowDisputeDecision(t *testing.T) {
	x := newEscrowEnv(t)
	adminID, admin := x.user("eadmin_"+randomToken()[:6], "admin")
	_, mod := x.user("emod_"+randomToken()[:6], "moderator")
	refund := regtestAddr("dispute-refund-1")
	order, dispute := x.disputedEscrow(refund)

	// The desk offers the decision to an administrator only, with the escrow settlement stated.
	if desk := x.page("/moderator", mod); strings.Contains(desk, `action="/resolve"`) || !strings.Contains(desk, "Only an administrator decides a dispute on a Bitcoin escrow order") {
		t.Fatalf("moderator desk:\n%s", desk)
	}
	desk := x.page("/moderator", admin)
	if !strings.Contains(desk, `action="/resolve"`) || !strings.Contains(desk, `name="amount_seen" value="100000"`) || !strings.Contains(desk, "by a settlement from the escrow") {
		t.Fatalf("admin desk:\n%s", desk)
	}
	// Refusals change nothing: a moderator, an administrator without or with a wrong password.
	for _, c := range []struct {
		name, sess, password string
		code                 int
	}{{"a moderator", mod, testPassword, 403}, {"no password", admin, "", 400}, {"a wrong password", admin, "wrong password 123", 401}} {
		if code, body := x.resolve(c.sess, dispute, "refund", c.password); code != c.code {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		if x.state(order) != stateDisputed || agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", order)+agInt(x.testEnv, "SELECT count(*) FROM disputes WHERE order_id=$1 AND outcome<>''", order) != 0 {
			t.Fatalf("%s: something changed", c.name)
		}
	}
	if code, body := x.resolve(admin, dispute, "refund", testPassword); code != 303 {
		t.Fatalf("admin resolves: %d %s", code, body)
	}
	id, kind, state := x.mainSettlement(order)
	if x.state(order) != stateResolved || kind != "refund" || state != "building" || agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE id=$1 AND destination=$2", id, refund) != 1 {
		t.Fatalf("after the decision: %s %s %s", x.state(order), kind, state)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Resolved dispute on order % (outcome: refund)%(confirmed with password)%'", adminID) != 1 {
		t.Fatal("audit row")
	}
	x.noPayouts(order)

	// Before any final transaction the administrator may change it: the refund is superseded, its outputs freed, and
	// the vendor claims instead.
	x.poll()
	x.signAs(order, roleBuyer, id) // party-signed, not yet co-signed: no market signature exists
	for _, c := range []struct {
		name, sess, password string
		code                 int
	}{{"a moderator", mod, testPassword, 403}, {"a wrong password", admin, "wrong password 456", 401}, {"the same decision", admin, testPassword, 409}} {
		outcome := "release"
		if c.name == "the same decision" {
			outcome = "refund"
		}
		if code, body := x.changeDecision(c.sess, order, outcome, c.password); code != c.code {
			t.Fatalf("change by %s: %d %s", c.name, code, body)
		}
	}
	if _, _, state := x.mainSettlement(order); state != "party_signed" {
		t.Fatalf("a refused change changed the settlement: %s", state)
	}
	page := x.page("/order?id="+order, admin)
	if !strings.Contains(page, `action="/escrow/decision"`) || !strings.Contains(page, "Refund to the buyer") {
		t.Fatalf("admin order page:\n%s", page)
	}
	if strings.Contains(x.page("/order?id="+order, x.buyerSess), `action="/escrow/decision"`) {
		t.Fatal("a party is offered the decision change")
	}
	if code, body := x.changeDecision(admin, order, "release", testPassword); code != 303 {
		t.Fatalf("change: %d %s", code, body)
	}
	if r := x.settleByID(id); r.State != "superseded" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", id) != 0 {
		t.Fatalf("the replaced refund: %+v", r)
	}
	var outcome string
	x.DB.QueryRow("SELECT outcome FROM disputes WHERE id=$1", dispute).Scan(&outcome)
	if outcome != "release" || !strings.Contains(strings.Join(x.events(order), "\n"), "changed the dispute decision from a refund to the buyer to a release to the vendor") {
		t.Fatalf("decision %s:\n%s", outcome, strings.Join(x.events(order), "\n"))
	}
	for _, uid := range []string{x.buyer, x.vendor} {
		if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%dispute decision was changed%'", uid) != 1 {
			t.Fatal("a party was not told of the change")
		}
	}
	if agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Changed the escrow dispute decision of order % (refund to release)%(confirmed with password)%'", adminID) != 1 {
		t.Fatal("audit row of the change")
	}
	x.poll()
	if r := x.settleByID(id); r.State != "superseded" || x.core.count("walletprocesspsbt") != 0 {
		t.Fatalf("a superseded settlement was co-signed: %+v", r)
	}
	if page := x.page("/order?id="+order, x.vendorSess); !strings.Contains(page, `action="/escrow/claim"`) {
		t.Fatalf("the vendor is not offered the claim after a release decision:\n%s", page)
	}
	if code, body := x.claim(order, x.vendorSess, regtestAddr("dispute-claim-1")); code != 303 {
		t.Fatalf("claim after the decision: %d %s", code, body)
	}
	x.poll()
	rid, kind, state := x.mainSettlement(order)
	if kind != "release" || state != "awaiting_signature" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1", rid) != 1 {
		t.Fatalf("the release: %s %s", kind, state)
	}
	x.signAs(order, roleVendor, rid)
	x.poll()
	if r := x.settleByID(rid); r.State != "broadcast" {
		t.Fatalf("the release after the decision: %+v", r)
	}
	// Fixed once the settlement has a final transaction.
	if code, body := x.changeDecision(admin, order, "refund", testPassword); code != 409 || !strings.Contains(body, "fixed") {
		t.Fatalf("change after the broadcast: %d %s", code, body)
	}
	if strings.Contains(x.page("/order?id="+order, admin), `action="/escrow/decision"`) {
		t.Fatal("the change is offered after the broadcast")
	}

	x.core.setTxConfs(x.settleByID(rid).TxID, 3, "")
	x.poll()
	if r := x.settleByID(rid); r.State != "confirmed" {
		t.Fatalf("the release confirmed: %+v", r)
	}

	// Also fixed while a co-signed transaction is stored but not yet broadcast.
	order2, dispute2 := x.disputedEscrow(regtestAddr("dispute-refund-2"))
	if code, body := x.resolve(admin, dispute2, "refund", testPassword); code != 303 {
		t.Fatalf("resolve: %d %s", code, body)
	}
	x.poll()
	id2, _, _ := x.mainSettlement(order2)
	x.signAs(order2, roleBuyer, id2)
	x.A.escrowPollOnce(x.killAtSend())
	x.sendNormally()
	if r := x.settleByID(id2); r.State != "signed" {
		t.Fatalf("stored, not broadcast: %+v", r)
	}
	if code, _ := x.changeDecision(admin, order2, "release", testPassword); code != 409 {
		t.Fatalf("change of a co-signed settlement: %d", code)
	}
	if r := x.settleByID(id2); r.State != "signed" {
		t.Fatalf("after the refused change: %+v", r)
	}
	x.noPayouts(order)
	x.noPayouts(order2)
	if got := agStrings(t, x.testEnv, "SELECT kind||' '||state FROM settlements WHERE order_id=$1 ORDER BY id", order); !slices.Equal(got, []string{"refund superseded", "release confirmed"}) {
		t.Fatalf("settlements of the order: %q", got)
	}
}
