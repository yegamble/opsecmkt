package market

import (
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Work queues (A-6): open disputes and paid orders are never hidden behind a cap of newer, closed history.

// bulkOrders inserts n orders by buyer on product in state, created within the last day (newer than
// the 30-day-old fixtures below), with ids prefix001..prefixNNN in creation order.
func (w *orderWorld) bulkOrders(prefix, buyerID, productID, state string, n int) {
	w.e.t.Helper()
	if _, err := w.e.DB.Exec(`INSERT INTO orders(id,buyer_id,product_id,currency,amount,state,created)
		SELECT $1||lpad(g::text,3,'0'),$2,$3,'XMR',100000,$4,now()-interval '1 day'+g*interval '1 second' FROM generate_series(1,$5) g`,
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
	paidOld := w.e.order(w.buyerID, product, "XMR", statePaid)
	paidOlder := w.e.order(w.otherID, product, "XMR", statePaid)
	shippedNew := w.e.order(w.buyerID, product, "XMR", stateShipped)
	shippedOld := w.e.order(w.buyerID, product, "XMR", stateShipped)
	oldActive := w.e.order(w.otherID, product, "XMR", stateDelivered)
	for _, item := range []struct{ id, age string }{{paidOld, "2 hours"}, {paidOlder, "90 minutes"}, {shippedNew, "1 minute"}, {oldActive, "3 hours"}, {shippedOld, "4 hours"}} {
		if _, err := w.e.DB.Exec("UPDATE orders SET updated=now()-$2::interval WHERE id=$1", item.id, item.age); err != nil {
			t.Fatal(err)
		}
	}
	// The vendor's own purchase from someone else is not their work.
	elsewhere := w.e.order(w.vendorID, w.e.product(w.otherID, "physical"), "XMR", statePaid)
	openDispute := w.e.order(w.buyerID, product, "XMR", stateDisputed)
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
	table := body[strings.Index(body, `id="vendor-orders"`):] // the incoming-orders table, after "Waiting for you" (A-710)
	shippedNewAt, disputeAt, paidNewAt, paidOldAt, oldAt, shippedOldAt := strings.Index(table, shippedNew), strings.Index(table, openDispute), strings.Index(table, paidOlder), strings.Index(table, paidOld), strings.Index(table, oldActive), strings.Index(table, shippedOld)
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

// A-494 / A-507: the vendor desk's history cap turns on the number of terminal orders of every kind together
// (completed, resolved and cancelled with an escrow or a payment), counted exactly against historyLimit and without
// the buyer-side abandoned drafts. No single kind exceeds the limit here, so a count that left one kind out or
// admitted the drafts, or that compared with >= , gives a different answer at 100 or 101.
func TestVendorDeskHistoryCapCountsEveryTerminalKindAtTheLimit(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	// Terminal order g is completed (g%3=1), resolved (2) or cancelled with a payment (0); a higher g changed later.
	terminals := func(from, to int) {
		t.Helper()
		agExec(w.e, `INSERT INTO orders(id,buyer_id,product_id,currency,amount,state,created,updated)
			SELECT 'term-'||lpad(g::text,3,'0'),$1,$2,'XMR',100000,
			  CASE g%3 WHEN 1 THEN 'completed' WHEN 2 THEN 'resolved' ELSE 'cancelled' END,
			  now()-interval '1 day', now()-interval '1 day'+g*interval '1 second' FROM generate_series($3::int,$4::int) g`,
			w.buyerID, product, from, to)
		agExec(w.e, `INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations)
			SELECT id,'XMR',encode(sha256(convert_to(id,'UTF8')),'hex'),0,'bcrt1qterm'||right(id,3),100000,3
			FROM orders WHERE id LIKE 'term-%' AND state='cancelled' AND id NOT IN (SELECT order_id FROM payments)`)
	}
	listed := func() (ids []string, limit int) {
		t.Helper()
		d := &PageData{Page: "vendor-dashboard", User: w.e.loadUser(w.vendorID), Settings: map[string]string{}}
		if err := loadIncomingOrders(t.Context(), w.e.A, httptest.NewRequest("GET", "/vendor", nil), d); err != nil {
			t.Fatal(err)
		}
		for _, o := range d.IncomingOrders {
			ids = append(ids, o.ID)
		}
		return ids, d.HistoryLimit
	}
	const note = "Showing the 100 most recent completed, resolved or funded-cancelled orders as history"
	active := []string{
		w.e.order(w.buyerID, product, "XMR", statePaid),
		w.e.order(w.buyerID, product, "XMR", stateShipped),
		w.e.order(w.buyerID, product, "XMR", stateAwaitingPayment),
	}
	// Abandoned drafts (cancelled, no escrow, no payment) are not customer work: they neither show nor count.
	var abandoned []string
	for range 5 {
		abandoned = append(abandoned, w.e.order(w.buyerID, product, "XMR", stateCancelled))
	}

	// Exactly historyLimit terminal orders of three kinds: everything is listed, nothing is announced as cut.
	terminals(1, historyLimit)
	ids, limit := listed()
	if limit != 0 || len(ids) != historyLimit+len(active) || !sameOrders(slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return !strings.HasPrefix(id, "term-") }), agStrings(t, w.e, "SELECT id FROM orders WHERE id LIKE 'term-%'")) {
		t.Fatalf("at the limit: HistoryLimit=%d, %d orders listed, want 0 and %d", limit, len(ids), historyLimit+len(active))
	}
	for _, id := range append(slices.Clone(active), "term-001", "term-100") {
		if !slices.Contains(ids, id) {
			t.Fatalf("at the limit the desk lost %s", id)
		}
	}
	for _, id := range abandoned {
		if slices.Contains(ids, id) {
			t.Fatalf("an abandoned draft %s is listed on the vendor desk", id)
		}
	}
	if body := w.page("/vendor-dashboard", w.vendor, 200); strings.Contains(body, note) || !strings.Contains(body, "term-001") {
		t.Fatal("the desk announced a history cut at exactly the limit, or lost the oldest order")
	}

	// One more terminal order tips it over: the oldest of the 101 is dropped, active work is untouched and the cut is announced.
	terminals(historyLimit+1, historyLimit+1)
	ids, limit = listed()
	if limit != historyLimit || len(ids) != historyLimit+len(active) || slices.Contains(ids, "term-001") || !slices.Contains(ids, "term-101") {
		t.Fatalf("over the limit: HistoryLimit=%d, %d orders listed, term-001 listed=%v, term-101 listed=%v; want %d, %d, false, true",
			limit, len(ids), slices.Contains(ids, "term-001"), slices.Contains(ids, "term-101"), historyLimit, historyLimit+len(active))
	}
	for _, id := range active {
		if !slices.Contains(ids, id) {
			t.Fatalf("the history cap dropped active order %s", id)
		}
	}
	if body := w.page("/vendor-dashboard", w.vendor, 200); !strings.Contains(body, note) || strings.Contains(body, "term-001") || !strings.Contains(body, "term-101") {
		t.Fatal("the over-limit desk does not announce the cut or keeps the wrong end of the history")
	}
}

func TestDraftCancelDoesNotNotifyVendor(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	draft := w.e.order(w.buyerID, product, "XMR", stateDraft)
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
	draft := w.e.order(w.buyerID, product, "XMR", stateDraft)
	cancelledDraft := w.e.order(w.buyerID, product, "XMR", stateCancelled)
	fundedCancelled := w.e.order(w.otherID, product, "XMR", stateCancelled)
	// A cancellation that received a payment is customer work its vendor keeps seeing (A-494, cancelledCustomerWork).
	if _, err := w.e.DB.Exec("INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'XMR',$2,0,'bcrt1qtestfundedcancelled',100000,3)", fundedCancelled, txidOf("funded-cancelled")); err != nil {
		t.Fatal(err)
	}
	w.bulkOrders("bulk-funded-cancel-", w.otherID, product, stateCancelled, 51)
	if _, err := w.e.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations)
		SELECT id,'XMR',encode(sha256(convert_to(id,'UTF8')),'hex'),0,'bcrt1qaddress'||right(id,3),100000,3 FROM orders WHERE id LIKE 'bulk-funded-cancel-%'`); err != nil {
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
		t.Fatal("vendor terminal history lost a cancellation with a recorded payment")
	}
}
