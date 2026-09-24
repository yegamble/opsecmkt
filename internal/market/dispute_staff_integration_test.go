package market

import (
	"strings"
	"testing"
)

// A-8: opening a dispute tells the staff who can resolve it; a dispute nobody independent can resolve is
// still accepted, and administrators are told it needs a moderator who is not a party.

func (w *orderWorld) resolverNotices(uid string) int {
	return w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Dispute opened on order%'", uid)
}

func TestDisputeNotifiesIndependentStaff(t *testing.T) {
	w := newOrderWorld(t)
	adminID, _ := w.e.user("admin_"+randomToken()[:6], "admin")
	order := w.e.order(w.buyerID, w.e.product(w.vendorID, "physical"), "BTC", stateShipped)
	w.expect("/disputes", w.buyer, form("order_id", order, "reason", "The parcel arrived empty and the seal was broken."), 303, "")

	want := "Dispute opened on order " + shortID(order) + ". Review it on the moderator desk."
	for _, uid := range []string{w.modID, adminID} {
		if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", uid, want); n != 1 {
			t.Fatalf("staff %s got %d resolver notices, want 1", uid, n)
		}
	}
	for _, uid := range []string{w.buyerID, w.vendorID, w.otherID} {
		if n := w.resolverNotices(uid); n != 0 {
			t.Fatalf("non-resolver %s got %d resolver notices", uid, n)
		}
	}
	// The vendor keeps today's single transition notice; the buyer (actor) gets none.
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1", w.vendorID); n != 1 {
		t.Fatalf("vendor notifications=%d, want 1", n)
	}
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1", w.buyerID); n != 0 {
		t.Fatalf("buyer notifications=%d, want 0", n)
	}
	if n := w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'No independent resolver%'", order); n != 0 {
		t.Fatal("no-resolver note written although a resolver exists")
	}
	mustNotContain(t, w.page("/moderator", w.mod, 200), "No eligible resolver")
}

func TestDisputePartyModeratorIsNotAResolver(t *testing.T) {
	w := newOrderWorld(t)
	adminID, _ := w.e.user("admin_"+randomToken()[:6], "admin")
	// The moderator buys from the vendor, so they are a party and cannot resolve this dispute.
	order := w.e.order(w.modID, w.e.product(w.vendorID, "physical"), "BTC", stateShipped)
	w.expect("/disputes", w.vendor, form("order_id", order, "reason", "The buyer claims non-delivery but tracking shows it arrived."), 303, "")

	if n := w.resolverNotices(w.modID); n != 0 {
		t.Fatalf("party moderator got %d resolver notices", n)
	}
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1", w.modID); n != 1 {
		t.Fatalf("party moderator notifications=%d, want only the transition notice", n)
	}
	if n := w.resolverNotices(adminID); n != 1 {
		t.Fatalf("independent admin got %d resolver notices, want 1", n)
	}
	if n := w.count("SELECT count(*) FROM notifications WHERE body LIKE '%needs a moderator who is not a party%'"); n != 0 {
		t.Fatal("no-resolver notice sent although an independent administrator exists")
	}
}

func TestDisputeWithNoEligibleResolver(t *testing.T) {
	e := newTestApp(t)
	suffix := randomToken()[:6]
	buyerID, buyer := e.user("buyer_"+suffix, "buyer")
	adminID, admin := e.user("admin_"+suffix, "admin")
	w := &orderWorld{e: e}
	// The only administrator is the vendor and no moderator exists.
	order := e.order(buyerID, e.product(adminID, "physical"), "BTC", stateShipped)
	w.expect("/disputes", buyer, form("order_id", order, "reason", "The parcel never arrived at the agreed drop."), 303, "")

	if w.state(order) != stateDisputed || w.count("SELECT count(*) FROM disputes WHERE order_id=$1 AND status='Open'", order) != 1 {
		t.Fatal("dispute refused although no independent resolver exists")
	}
	want := "Dispute opened on order " + shortID(order) + " needs a moderator who is not a party to the order. Every moderator and administrator is its buyer or vendor, so nobody can resolve it yet."
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", adminID, want); n != 1 {
		t.Fatalf("admin no-resolver notices=%d, want 1", n)
	}
	if n := w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND from_state='disputed' AND to_state='disputed' AND note LIKE 'No independent resolver was available%'", order); n != 1 {
		t.Fatalf("history notes=%d, want 1", n)
	}
	if !strings.Contains(w.page("/order?id="+order, buyer, 200), "No independent resolver was available") {
		t.Fatal("order history does not show the no-resolver note")
	}

	const note = "No eligible resolver."
	mustContain(t, w.page("/moderator", admin, 200), note, "You are a party to this order")
	mustNotContain(t, w.page("/disputes", buyer, 200), note)

	// Once an independent moderator exists the desk note disappears; the history note stays.
	_, mod := e.user("mod_"+suffix, "moderator")
	mustNotContain(t, w.page("/moderator", admin, 200), note)
	mustContain(t, w.page("/moderator", mod, 200), `name="outcome" value="refund"`)
}
