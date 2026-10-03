package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Escrow Phase 2, slice P2-b-fix (QA-P2b; A-385, A-386, A-387): a release the parties broadcast themselves before the
// order's outcome with fewer than two signatures stored, the buyer's signature and dispute serialised on the order row,
// and the resolve form's statement of deposits left out. Simulated RPC (escrow_fake_test.go), not chain evidence.

// offlineBuyer leaves a shipped order's release signed by the vendor through the market and by the buyer offline: both
// signatures are returned, and only the vendor's stays stored.
func (x *escrowEnv) offlineBuyer(order string, id int64) (vendor, buyer partySigs) {
	x.t.Helper()
	x.signAs(order, roleVendor, id)
	x.signAs(order, roleBuyer, id)
	vendor, buyer = x.storedSigs(id, roleVendor), x.storedSigs(id, roleBuyer)
	agExec(x.testEnv, "DELETE FROM settlement_signatures WHERE settlement_id=$1 AND party='buyer'", id) // the buyer never pasted
	return vendor, buyer
}

// disputeID is order's dispute.
func (x *escrowEnv) disputeID(order string) string {
	x.t.Helper()
	return physString(x.t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)
}

// A-385: the vendor signed the release through the market, the buyer signed offline and the parties broadcast it
// themselves while the order is shipped. The watcher finds the stored transaction on the network, records it as
// broadcast and completes the order; the market signs nothing, and the buyer can no longer dispute.
func TestEscrowShippedReleaseBroadcastByParties(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.shippedPhysical(regtestAddr("seenrel-refund"), regtestAddr("seenrel-claim"), 100000)
	vendor, buyer := x.offlineBuyer(order, id)
	txid, _ := x.partiesBroadcast(id, 1, vendor, buyer)
	x.poll()
	r := x.settleByID(id)
	if r.State != "broadcast" && r.State != "confirmed" || r.TxID != txid || r.Raw == "" || x.state(order) != stateCompleted {
		t.Fatalf("a party-signed release on the network while shipped: %+v, order %s", r, x.state(order))
	}
	if x.marketCalls() != 0 || x.core.count("finalizepsbt") != 0 {
		t.Fatalf("the market was asked to sign or finalize: %d market calls", x.marketCalls())
	}
	ev := strings.Join(x.events(order), "\n")
	if !strings.Contains(ev, "already on the regtest network when the market came to complete it") || !strings.Contains(ev, "shipped>completed system Completed: the release to the vendor is on the network") {
		t.Fatalf("events:\n%s", ev)
	}
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The goods never arrived at my address at all."}}); w.Code != 409 {
		t.Fatalf("a dispute after the release is on the network: %d", w.Code)
	}
}

// A-385: the same while the order is disputed (the vendor opened it): the release is recorded as broadcast, the order
// stays disputed, and a decision for the buyer is refused (409) and changes nothing.
func TestEscrowDisputedReleaseBroadcastByParties(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("seenrel_admin_"+randomToken()[:6], "admin")
	order, id := x.shippedPhysical(regtestAddr("seenrel-refund-2"), regtestAddr("seenrel-claim-2"), 100000)
	vendor, buyer := x.offlineBuyer(order, id)
	if w := x.do("POST", "/disputes", x.vendorSess, url.Values{"order_id": {order}, "reason": {"The buyer has not replied about the delivery."}}); w.Code != 303 {
		t.Fatalf("vendor's dispute: %d", w.Code)
	}
	x.partiesBroadcast(id, 1, vendor, buyer)
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || r.Raw == "" || x.state(order) != stateDisputed {
		t.Fatalf("a party-signed release on the network while disputed: %+v, order %s", r, x.state(order))
	}
	code, body := x.resolve(admin, x.disputeID(order), "refund", testPassword)
	if code != 409 || !strings.Contains(body, "already final") {
		t.Fatalf("a decision for the buyer over a release on the network: %d %s", code, body)
	}
	if x.state(order) != stateDisputed || agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", order) != 1 || x.marketCalls() != 0 {
		t.Fatal("the refused decision changed something, or the market signed")
	}
}

// A-385, A-376: a release waiting for any signature on a disputed order that the parties broadcast themselves is
// recorded as broadcast; the order stays disputed and a decision for the buyer is refused.
func TestEscrowDisputedAwaitingReleaseBroadcast(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("seenrel_admin_"+randomToken()[:6], "admin")
	order, id := x.shippedPhysical(regtestAddr("seenrel-refund-3"), regtestAddr("seenrel-claim-3"), 100000)
	x.signAs(order, roleVendor, id)
	x.signAs(order, roleBuyer, id)
	vendor, buyer := x.storedSigs(id, roleVendor), x.storedSigs(id, roleBuyer)
	agExec(x.testEnv, "DELETE FROM settlement_signatures WHERE settlement_id=$1", id) // signed offline only
	agExec(x.testEnv, "UPDATE settlements SET state='awaiting_signature',signer=NULL,signed_at=NULL WHERE id=$1", id)
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The goods never arrived at my address at all."}}); w.Code != 303 {
		t.Fatalf("dispute: %d", w.Code)
	}
	x.partiesBroadcast(id, 1, vendor, buyer)
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || x.state(order) != stateDisputed {
		t.Fatalf("after the pass: %+v, order %s", r, x.state(order))
	}
	if code, _ := x.resolve(admin, x.disputeID(order), "refund", testPassword); code != 409 {
		t.Fatalf("a decision for the buyer: %d", code)
	}
	if x.marketCalls() != 0 {
		t.Fatal("the market signed")
	}
}

// waitForLockWait waits until a backend of this database waits for a row lock (a request queued behind tx).
func (x *escrowEnv) waitForLockWait() {
	x.t.Helper()
	if !waitUntil(x.t, 2*time.Second, func() bool {
		return agInt(x.testEnv, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'") > 0
	}) {
		x.t.Fatal("the request did not wait for the order row lock")
	}
}

// A-386: the buyer's signature of the release and the buyer's dispute both lock the order row first, so of the two
// exactly one takes effect: a dispute queued behind an in-flight signature is refused once it commits, and a signature
// queued behind an in-flight dispute is refused once the order is disputed.
func TestEscrowSignDisputeSerialised(t *testing.T) {
	x := newEscrowEnv(t)
	t.Run("the signature first", func(t *testing.T) {
		order, id := x.shippedPhysical(regtestAddr("race-refund-1"), regtestAddr("race-claim-1"), 100000)
		x.signAs(order, roleVendor, id)
		tx, err := x.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		// The sign action as it runs: the order row, then the settlement row, then the buyer's signatures.
		for _, q := range []string{"SELECT 1 FROM orders WHERE id=$1 FOR UPDATE", "SELECT 1 FROM settlements WHERE order_id=$1 FOR UPDATE"} {
			if _, err = tx.Exec(q, order); err != nil {
				t.Fatal(err)
			}
		}
		for i := range x.storedSigs(id, roleVendor).Sigs {
			if _, err = tx.Exec("INSERT INTO settlement_signatures(settlement_id,input_n,party,pubkey,signature) VALUES($1,$2,'buyer',$3,'300602010102010101')", id, i, x.keyHex(order, roleBuyer)); err != nil {
				t.Fatal(err)
			}
		}
		done := make(chan *httptest.ResponseRecorder)
		go func() {
			done <- x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The goods never arrived at my address at all."}})
		}()
		x.waitForLockWait()
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if w := <-done; w.Code != 409 || x.state(order) != stateShipped {
			t.Fatalf("a dispute queued behind the buyer's signature: %d, order %s", w.Code, x.state(order))
		}
	})
	t.Run("the dispute first", func(t *testing.T) {
		order, id := x.shippedPhysical(regtestAddr("race-refund-2"), regtestAddr("race-claim-2"), 100000)
		x.signAs(order, roleVendor, id)
		sess, fp := x.partySess(roleBuyer)
		paste := walletSign(t, x.partyCopy(order, sess, id), fp, 1)
		tx, err := x.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		// The dispute action as it runs: the order row, the move to disputed and the dispute.
		for _, q := range []string{"SELECT 1 FROM orders WHERE id=$1 FOR UPDATE", "UPDATE orders SET state='disputed' WHERE id=$1",
			"INSERT INTO disputes(id,order_id,reason) VALUES(md5(random()::text),$1,'The goods never arrived at my address at all.')"} {
			if _, err = tx.Exec(q, order); err != nil {
				t.Fatal(err)
			}
		}
		done := make(chan int)
		go func() {
			code, _ := x.sign(order, sess, id, paste)
			done <- code
		}()
		x.waitForLockWait()
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if code := <-done; code != 409 || agInt(x.testEnv, "SELECT count(*) FROM settlement_signatures WHERE settlement_id=$1 AND party='buyer'", id) != 0 {
			t.Fatalf("the buyer's signature queued behind a dispute: %d", code)
		}
		if page := x.page("/order?id="+order, x.buyerSess); strings.Contains(page, `action="/escrow/sign"`) {
			t.Fatal("the buyer of a disputed order is offered to sign the release")
		}
	})
}

// A-387: when deposits were left out of the release, the resolve form names the amount the settlement pays and says the
// left-out deposits come back by a stray refund.
func TestEscrowResolveFormNamesLeftOut(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("seenrel_admin_"+randomToken()[:6], "admin")
	order, _ := x.shippedPhysical(regtestAddr("leftform-refund"), regtestAddr("leftform-claim"), 100000)
	x.core.deposit(physString(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order), txidOf("leftform-topup"), 0, 30000, 3)
	x.poll()
	if w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The goods never arrived at my address at all."}}); w.Code != 303 {
		t.Fatalf("dispute: %d", w.Code)
	}
	desk := x.page("/moderator", admin)
	for _, want := range []string{"pays 0.001 regtest BTC", "goes back 0.001 regtest BTC", "0.0003 regtest BTC left out of the release", "by a stray refund"} {
		if !strings.Contains(desk, want) {
			t.Fatalf("the resolve form lacks %q:\n%s", want, desk)
		}
	}
}
