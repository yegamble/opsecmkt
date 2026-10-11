package market

import (
	"net/url"
	"strings"
	"testing"
)

// Work queues (A-6): open disputes and paid orders are never hidden behind a cap of newer, closed history.

// bulkOrders inserts n orders by buyer on product in state, created within the last day (newer than
// the 30-day-old fixtures below), with ids prefix001..prefixNNN in creation order.
func (w *orderWorld) bulkOrders(prefix, buyerID, productID, state string, n int) {
	w.e.t.Helper()
	if _, err := w.e.DB.Exec(`INSERT INTO orders(id,buyer_id,product_id,currency,amount,state,created)
		SELECT $1||lpad(g::text,3,'0'),$2,$3,'BTC',100000,$4,now()-interval '1 day'+g*interval '1 second' FROM generate_series(1,$5) g`,
		prefix, buyerID, productID, state, n); err != nil {
		w.e.t.Fatal(err)
	}
}

func (w *orderWorld) aged(order string) {
	w.e.t.Helper()
	if _, err := w.e.DB.Exec("UPDATE orders SET created=now()-interval '30 days' WHERE id=$1", order); err != nil {
		w.e.t.Fatal(err)
	}
}

func TestModeratorDeskListsEveryOpenDisputeFirst(t *testing.T) {
	w := newDisputeOrderWorld(t)
	_, admin := w.e.user("queue_admin_"+randomToken()[:6], "admin")
	product := w.e.product(w.vendorID, "physical")
	oldOpen, _ := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "Oldest open dispute: parcel never arrived.")
	newOpen, _ := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "Newest open dispute: wrong item shipped.")
	w.aged(oldOpen)
	if _, err := w.e.DB.Exec("UPDATE disputes SET created=now()-interval '30 days' WHERE order_id=$1", oldOpen); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.DB.Exec("UPDATE disputes SET created=now()-interval '2 days' WHERE order_id=$1", newOpen); err != nil {
		t.Fatal(err)
	}
	// 101 newer resolved disputes: one more than the resolved history shown.
	w.bulkOrders("bulk-resolved-", w.buyerID, product, stateResolved, 101)
	if _, err := w.e.DB.Exec(`INSERT INTO disputes(id,order_id,reason,status,resolution,outcome,created)
		SELECT 'd-'||id,id,'Bulk resolved dispute reason.','Resolved — refund to buyer','Bulk decision text.','refund',created FROM orders WHERE id LIKE 'bulk-resolved-%'`); err != nil {
		t.Fatal(err)
	}

	for _, viewer := range []string{w.mod, w.buyer, w.vendor} {
		body := w.page("/moderator", w.mod, 200)
		if viewer != w.mod {
			body = w.page("/disputes", viewer, 200)
		}
		oldAt, newAt, resolvedAt := strings.Index(body, "Oldest open dispute"), strings.Index(body, "Newest open dispute"), strings.Index(body, "Resolved disputes</h2>")
		if oldAt < 0 || newAt < 0 || resolvedAt < 0 || !(oldAt < newAt && newAt < resolvedAt) {
			t.Fatalf("open disputes must be listed oldest first, before resolved history: old=%d new=%d resolved=%d", oldAt, newAt, resolvedAt)
		}
		if !strings.Contains(body, "Showing the 100 most recent resolved disputes") {
			t.Fatal("truncated resolved history is not announced")
		}
		if n := strings.Count(body, `<article class="message">`); n != 102 {
			t.Fatalf("want 2 open + 100 resolved disputes, got %d", n)
		}
		if strings.Contains(body, "Order bulk-resolved-001") || !strings.Contains(body, "Order bulk-resolved-101") || !strings.Contains(body, "Order bulk-resolved-002") {
			t.Fatal("resolved history is not the 100 most recent")
		}
	}
	// The open resolve form is still offered for the old dispute.
	if !strings.Contains(w.page("/moderator", admin, 200), `name="id" value="`+agStr(w.e, "SELECT id FROM disputes WHERE order_id=$1", oldOpen)+`"`) {
		t.Fatal("eligible administrator has no resolve form for the old escrow dispute")
	}
	if strings.Contains(w.page("/moderator", w.mod, 200), `action="/resolve"`) {
		t.Fatal("moderator can resolve an escrow dispute")
	}
	// Authorization unchanged: an unrelated buyer sees none of these disputes and no notice.
	if body := w.page("/disputes", w.other, 200); !strings.Contains(body, "No disputes to display") || strings.Contains(body, "Oldest open dispute") || strings.Contains(body, "most recent resolved") {
		t.Fatal("unrelated buyer sees other parties' disputes")
	}

	// Without truncation there is no notice, and with only resolved disputes the open queue says it is empty.
	if _, err := w.e.DB.Exec("DELETE FROM disputes WHERE order_id IN ($1,$2) OR id LIKE 'd-bulk-resolved-00%'", oldOpen, newOpen); err != nil {
		t.Fatal(err)
	}
	body := w.page("/moderator", w.mod, 200)
	if strings.Contains(body, "most recent resolved") || !strings.Contains(body, "No open disputes") || strings.Count(body, `<article class="message">`) != 92 {
		t.Fatal("untruncated resolved-only desk renders wrongly")
	}
}

func TestVendorDeskListsEveryActiveOrderByLastChangeAndCapsOnlyTerminalHistory(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	paidOld := w.e.order(w.buyerID, product, "BTC", statePaid)
	paidOlder := w.e.order(w.otherID, product, "BTC", statePaid)
	shippedNew := w.e.order(w.buyerID, product, "XMR", stateShipped)
	shippedOld := w.e.order(w.buyerID, product, "BTC", stateShipped)
	oldActive := w.e.order(w.otherID, product, "BTC", stateDelivered)
	for _, item := range []struct{ id, age string }{{paidOld, "2 hours"}, {paidOlder, "90 minutes"}, {shippedNew, "1 minute"}, {oldActive, "3 hours"}, {shippedOld, "4 hours"}} {
		if _, err := w.e.DB.Exec("UPDATE orders SET updated=now()-$2::interval WHERE id=$1", item.id, item.age); err != nil {
			t.Fatal(err)
		}
	}
	// The vendor's own purchase from someone else is not their work.
	elsewhere := w.e.order(w.vendorID, w.e.product(w.otherID, "physical"), "BTC", statePaid)
	openDispute := w.e.order(w.buyerID, product, "BTC", stateDisputed)
	if _, err := w.e.DB.Exec("INSERT INTO disputes(id,order_id,reason) VALUES('vendor-open-dispute',$1,'The parcel did not arrive.')", openDispute); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.DB.Exec("UPDATE orders SET updated=now()-interval '30 seconds' WHERE id=$1", openDispute); err != nil {
		t.Fatal(err)
	}
	w.bulkOrders("bulk-active-", w.buyerID, product, stateAwaitingPayment, 101)
	w.bulkOrders("bulk-done-", w.buyerID, product, stateCompleted, 101)

	body := w.page("/vendor-dashboard", w.vendor, 200)
	if !strings.Contains(body, "To fulfil</div>\n<strong>2</strong>") || !strings.Contains(body, "Open disputes</div>\n<strong>1</strong>") {
		t.Fatal("vendor paid-order or open-dispute counts are incorrect")
	}
	shippedNewAt, disputeAt, paidNewAt, paidOldAt, oldAt, shippedOldAt := strings.Index(body, shippedNew), strings.Index(body, openDispute), strings.Index(body, paidOlder), strings.Index(body, paidOld), strings.Index(body, oldActive), strings.Index(body, shippedOld)
	if shippedNewAt < 0 || disputeAt < 0 || paidNewAt < 0 || paidOldAt < 0 || oldAt < 0 || shippedOldAt < 0 || !(disputeAt < shippedNewAt && shippedNewAt < paidNewAt && paidNewAt < paidOldAt && paidOldAt < oldAt && oldAt < shippedOldAt) {
		t.Fatalf("active orders should be uncapped and sorted by most recent change: dispute=%d shipped=%d paid-new=%d paid-old=%d old=%d shipped-old=%d", disputeAt, shippedNewAt, paidNewAt, paidOldAt, oldAt, shippedOldAt)
	}
	if strings.Contains(body, elsewhere) || strings.Contains(body, "bulk-done-001") || !strings.Contains(body, "bulk-done-101") || !strings.Contains(body, "bulk-done-002") || !strings.Contains(body, "bulk-active-001") || !strings.Contains(body, "bulk-active-101") {
		t.Fatal("vendor desk leaked a foreign order or did not retain exactly the newest terminal history")
	}
	if !strings.Contains(body, "Showing the 100 most recent completed, resolved or funded-cancelled orders as history") {
		t.Fatal("truncated order history is not announced")
	}
	for _, session := range []string{w.other, w.vendor} {
		if !strings.Contains(w.page("/order?id="+oldActive, session, 200), oldActive) {
			t.Fatal("old active order became inaccessible")
		}
	}
	if !strings.Contains(w.page("/disputes", w.buyer, 200), `<option value="`+shippedOld+`">`) {
		t.Fatal("old shipped order disappeared from the buyer's dispute choices")
	}
	if strings.Contains(w.page("/disputes", w.other, 200), `<option value="`+shippedOld+`">`) {
		t.Fatal("unrelated buyer sees another buyer's dispute choice")
	}
}

func TestDraftCancelDoesNotNotifyVendor(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	draft := w.e.order(w.buyerID, product, "BTC", stateDraft)
	page := w.page("/order?id="+draft, w.buyer, 200)
	nonce := renderedFormNonce(t, page, "/orders/cancel")
	before := w.count("SELECT count(*) FROM notifications WHERE user_id=$1", w.vendorID)
	resp := w.post("/orders/cancel", w.buyer, url.Values{"order_id": {draft}, "from": {stateDraft}, formNonceField: {nonce}})
	if resp.Code != 303 || w.state(draft) != stateCancelled {
		t.Fatalf("draft cancellation failed: status=%d state=%s body=%q", resp.Code, w.state(draft), resp.Body.String())
	}
	if got := w.count("SELECT count(*) FROM notifications WHERE user_id=$1", w.vendorID); got != before {
		t.Fatalf("draft cancellation notified vendor: before=%d after=%d", before, got)
	}
}

func TestVendorRowsExcludeUnfundedDraftsButKeepBuyerHistory(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	draft := w.e.order(w.buyerID, product, "BTC", stateDraft)
	cancelledDraft := w.e.order(w.buyerID, product, "XMR", stateCancelled)
	fundedCancelled := w.e.order(w.otherID, product, "BTC", stateCancelled)
	if _, err := w.e.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'BTC','test-funded-cancelled','test')", fundedCancelled); err != nil {
		t.Fatal(err)
	}
	w.bulkOrders("bulk-funded-cancel-", w.otherID, product, stateCancelled, 51)
	if _, err := w.e.DB.Exec(`INSERT INTO payment_addresses(order_id,currency,address,provider)
		SELECT id,'BTC','address-'||id,'test' FROM orders WHERE id LIKE 'bulk-funded-cancel-%'`); err != nil {
		t.Fatal(err)
	}
	w.bulkOrders("bulk-unfunded-cancel-", w.otherID, product, stateCancelled, 51)
	vendorOrders := w.page("/orders", w.vendor, 200)
	if strings.Contains(vendorOrders, draft) || strings.Contains(vendorOrders, cancelledDraft) || strings.Contains(vendorOrders, "bulk-unfunded-cancel-") || !strings.Contains(vendorOrders, "Showing 1–50 of 52 orders") {
		t.Fatal("vendor order history should hide drafts/unfunded cancellations and count all 52 funded orders")
	}
	buyerOrders := w.page("/orders", w.buyer, 200)
	if !strings.Contains(buyerOrders, draft) || !strings.Contains(buyerOrders, cancelledDraft) || strings.Contains(buyerOrders, fundedCancelled) {
		t.Fatal("buyer order history lost its own draft or cancelled draft")
	}
	if !strings.Contains(w.page("/orders?page=2", w.vendor, 200), fundedCancelled) {
		t.Fatal("vendor order history did not page through funded cancelled orders")
	}
	if !strings.Contains(w.page("/vendor-dashboard", w.vendor, 200), fundedCancelled) {
		t.Fatal("vendor terminal history lost a cancellation with an issued payment address")
	}
}
