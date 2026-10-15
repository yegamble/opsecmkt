package market

import (
	"strings"
	"testing"
)

func TestSuspensionNotifiesActiveCounterpartiesOnceWithoutDrafts(t *testing.T) {
	w := newOrderWorld(t)
	_, admin := w.e.user("counterparty_admin", "admin")
	p := w.e.product(w.vendorID, "physical")
	paid := w.e.order(w.buyerID, p, "XMR", statePaid)
	w.e.order(w.buyerID, p, "XMR", stateAwaitingPayment)
	w.e.order(w.otherID, p, "XMR", stateDraft)
	handle := agStr(w.e, "SELECT handle FROM users WHERE id=$1", w.vendorID)
	for _, choice := range []string{"suspend", "restore"} {
		before := agInt(w.e, "SELECT count(*) FROM notifications WHERE user_id=$1", w.buyerID)
		otherBefore := agInt(w.e, "SELECT count(*) FROM notifications WHERE user_id=$1", w.otherID)
		w.e.check(w.e.do("POST", "/admin/suspend", admin, suspendForm(choice, handle)), 303)
		if got := agInt(w.e, "SELECT count(*) FROM notifications WHERE user_id=$1", w.buyerID); got != before+1 {
			t.Fatalf("%s sent %d notices for two active orders", choice, got-before)
		}
		if got := agInt(w.e, "SELECT count(*) FROM notifications WHERE user_id=$1", w.otherID); got != otherBefore {
			t.Fatal("suspension leaked a vendor notice to an unfunded draft's buyer")
		}
		page := w.page("/order?id="+paid, w.buyer, 200)
		if strings.Contains(page, "The vendor account is suspended") != (choice == "suspend") {
			t.Fatalf("%s produced incorrect suspension status", choice)
		}
		w.e.check(w.e.do("POST", "/admin/suspend", admin, suspendForm(choice, handle)), 409)
		if got := agInt(w.e, "SELECT count(*) FROM notifications WHERE user_id=$1", w.buyerID); got != before+1 {
			t.Fatal("refused repeat suspension added another notice")
		}
	}
}

func TestNewReviewNotifiesVendorOnceAndLinksOrder(t *testing.T) {
	w := newOrderWorld(t)
	p := w.e.product(w.vendorID, "physical")
	o := w.e.order(w.buyerID, p, "XMR", stateCompleted)
	f := form("order_id", o, "rating", "5", "body", "Everything arrived as described.")
	w.expect("/reviews", w.buyer, f, 303, "")
	w.expect("/reviews", w.buyer, f, 409, "already reviewed")
	if agInt(w.e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND order_id=$2", w.vendorID, o) != 1 {
		t.Fatal("new review did not send exactly one order-linked vendor notice")
	}
}
