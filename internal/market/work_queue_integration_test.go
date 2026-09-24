package market

import (
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
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	oldOpen := w.e.order(w.buyerID, product, "BTC", stateDisputed)
	newOpen := w.e.order(w.buyerID, product, "XMR", stateDisputed)
	w.aged(oldOpen)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO disputes(id,order_id,reason,created) VALUES('old-open-dispute',$1,'Oldest open dispute: parcel never arrived.',now()-interval '30 days')", []any{oldOpen}},
		{"INSERT INTO disputes(id,order_id,reason,created) VALUES('new-open-dispute',$1,'Newest open dispute: wrong item shipped.',now()-interval '2 days')", []any{newOpen}},
	} {
		if _, err := w.e.DB.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
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
	if !strings.Contains(w.page("/moderator", w.mod, 200), `name="id" value="old-open-dispute"`) {
		t.Fatal("old open dispute has no resolve form")
	}
	// Authorization unchanged: an unrelated buyer sees none of these disputes and no notice.
	if body := w.page("/disputes", w.other, 200); !strings.Contains(body, "No disputes to display") || strings.Contains(body, "Oldest open dispute") || strings.Contains(body, "most recent resolved") {
		t.Fatal("unrelated buyer sees other parties' disputes")
	}

	// Without truncation there is no notice, and with only resolved disputes the open queue says it is empty.
	if _, err := w.e.DB.Exec("DELETE FROM disputes WHERE id IN ('old-open-dispute','new-open-dispute') OR id LIKE 'd-bulk-resolved-00%'"); err != nil {
		t.Fatal(err)
	}
	body := w.page("/moderator", w.mod, 200)
	if strings.Contains(body, "most recent resolved") || !strings.Contains(body, "No open disputes") || strings.Count(body, `<article class="message">`) != 92 {
		t.Fatal("untruncated resolved-only desk renders wrongly")
	}
}

func TestVendorDeskCountsAndListsEveryPaidOrder(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	paidOld := w.e.order(w.buyerID, product, "BTC", statePaid)
	paidOlder := w.e.order(w.otherID, product, "BTC", statePaid)
	shippedOld := w.e.order(w.buyerID, product, "XMR", stateShipped)
	for _, id := range []string{paidOld, paidOlder, shippedOld} {
		w.aged(id)
	}
	if _, err := w.e.DB.Exec("UPDATE orders SET created=created-interval '1 day' WHERE id=$1", paidOlder); err != nil {
		t.Fatal(err)
	}
	// The vendor's own paid purchase from someone else is not their work.
	elsewhere := w.e.order(w.vendorID, w.e.product(w.otherID, "physical"), "BTC", statePaid)
	w.bulkOrders("bulk-done-", w.buyerID, product, stateCompleted, 101)

	body := w.page("/vendor-dashboard", w.vendor, 200)
	if !strings.Contains(body, "To fulfil</div>\n<strong>2</strong>") {
		t.Fatal("NeedsAction is not the true number of paid orders on the vendor's listings")
	}
	olderAt, oldAt, bulkAt := strings.Index(body, paidOlder), strings.Index(body, paidOld), strings.Index(body, "bulk-done-101")
	if olderAt < 0 || oldAt < 0 || bulkAt < 0 || !(olderAt < oldAt && oldAt < bulkAt) {
		t.Fatalf("paid orders must be listed first, oldest first: older=%d old=%d bulk=%d", olderAt, oldAt, bulkAt)
	}
	if strings.Contains(body, elsewhere) || strings.Contains(body, "bulk-done-001") || strings.Contains(body, shippedOld) {
		t.Fatal("incoming orders include foreign orders or more than the capped history")
	}
	if !strings.Contains(body, "Showing every paid order and the 100 most recent other orders") {
		t.Fatal("truncated order history is not announced")
	}

	// The buyer can still open a dispute on an old shipped order behind 100 newer orders.
	if !strings.Contains(w.page("/disputes", w.buyer, 200), `<option value="`+shippedOld+`">`) {
		t.Fatal("disputes page hides an old disputable order")
	}
	if strings.Contains(w.page("/disputes", w.other, 200), `<option value="`+shippedOld+`">`) {
		t.Fatal("disputes page offers another buyer's order")
	}
}
