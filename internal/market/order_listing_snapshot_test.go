package market

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"
)

func TestPaidOrderKeepsListingTermsAfterEditForPartiesAndDisputeDesk(t *testing.T) {
	w := newOrderWorld(t)
	listing := w.e.product(w.vendorID, "digital")
	order := w.e.order(w.buyerID, listing, "BTC", stateDraft)
	w.e.A.payments["BTC"] = &stubProvider{currency: "BTC"}
	w.e.mustExec("UPDATE products SET title='Purchased name',description='Purchased description: private order context' WHERE id=$1", listing)
	mustContain(t, w.page("/order?id="+order, w.buyer, 200), "Purchased name", "Purchased description: private order context")
	w.expect("/orders/pay", w.buyer, url.Values{"order_id": {order}}, 303, "")
	var title, description, kind string
	if err := w.e.DB.QueryRow("SELECT listing_title,listing_description,listing_kind FROM orders WHERE id=$1", order).Scan(&title, &description, &kind); err != nil || title != "Purchased name" || description != "Purchased description: private order context" || kind != "digital" {
		t.Fatalf("snapshot %q %q %q: %v", title, description, kind, err)
	}
	w.e.mustExec("UPDATE products SET title='Edited live name',description='Edited live description',kind='physical' WHERE id=$1", listing)
	for _, session := range []string{w.buyer, w.vendor} {
		body := w.page("/order?id="+order, session, 200)
		mustContain(t, body, "Purchased name", "Purchased description: private order context", "digital", "Listing as it is now; it may have changed since purchase")
		mustNotContain(t, body, "Edited live name", "Edited live description")
	}
	for _, session := range []string{w.other, w.mod} {
		if page := w.page("/order?id="+order, session, 404); strings.Contains(page, "Purchased description: private order context") {
			t.Fatal("unauthorized viewer read a purchased description")
		}
	}
	// A paid order may later be disputed: the staff desk and the parties' dispute list
	// must continue naming what was purchased, not what the vendor edits afterwards.
	w.e.mustExec("UPDATE orders SET state='disputed' WHERE id=$1", order)
	w.e.mustExec("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Terms changed later')", randomToken(), order)
	for _, session := range []string{w.buyer, w.vendor, w.mod} {
		body := w.page("/disputes", session, 200)
		mustContain(t, body, "Purchased name", "digital")
		mustNotContain(t, body, "Edited live name")
	}
	staffOrder := w.page("/order?id="+order, w.mod, 200)
	mustContain(t, staffOrder, "Purchased description: private order context")
	w.e.mustExec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged)
        VALUES($1,'BTC','snapshot-review',0,'stub-review',100000,1,true)`, order)
	moderatorDesk := w.page("/moderator", w.mod, 200)
	mustContain(t, moderatorDesk, "Purchased name", "View current listing")
	mustNotContain(t, moderatorDesk, "Edited live name")
}

func TestEarlierOrderWithoutSnapshotStatesDetailsUnavailable(t *testing.T) {
	w := newOrderWorld(t)
	listing := w.e.product(w.vendorID, "physical")
	old := randomToken()
	w.e.mustExec("INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES($1,$2,$3,'BTC',100000,'paid')", old, w.buyerID, listing)
	w.e.mustExec("UPDATE products SET title='Current name only',description='Current description only' WHERE id=$1", listing)
	body := w.page("/order?id="+old, w.buyer, 200)
	mustContain(t, body, "Listing terms were not recorded for this order", "Not recorded", "Listing as it is now; it may have changed since purchase")
	mustNotContain(t, body, "Current name only", "Current description only")
	loaded, err := scanOrder(w.e.DB.QueryRow(orderQuery+" WHERE o.id=$1", old))
	if err != nil || loaded.Kind != "physical" || loaded.ListingRecorded || loaded.KindDisplay != "Not recorded" {
		t.Fatalf("legacy order lost operational kind or invented snapshot: %+v %v", loaded, err)
	}
	// A draft cancelled before payment has no purchase-time terms either; the label
	// must not imply it was purchased in an earlier version of the application.
	cancelled := randomToken()
	w.e.mustExec("INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES($1,$2,$3,'BTC',100000,'cancelled')", cancelled, w.buyerID, listing)
	mustContain(t, w.page("/order?id="+cancelled, w.buyer, 200), "Listing terms were not recorded for this order", "Not recorded")
}

func TestRefusedPaymentRequestRollsBackListingSnapshot(t *testing.T) {
	w := newOrderWorld(t)
	listing := w.e.product(w.vendorID, "digital")
	order := w.e.order(w.buyerID, listing, "BTC", stateDraft)
	provider := &stubProvider{currency: "BTC", broken: true}
	w.e.A.payments["BTC"] = provider
	w.expect("/orders/pay", w.buyer, url.Values{"order_id": {order}}, 503, "did not return a usable address")
	var title, description, kind sql.NullString
	if err := w.e.DB.QueryRow("SELECT listing_title,listing_description,listing_kind FROM orders WHERE id=$1", order).Scan(&title, &description, &kind); err != nil {
		t.Fatal(err)
	}
	if title.Valid || description.Valid || kind.Valid || w.state(order) != stateDraft || w.stock(listing) != 5 || len(provider.created) != 0 {
		t.Fatal("refused payment request retained a snapshot, stock reservation or address")
	}
	provider.broken = false
	w.expect("/orders/pay", w.buyer, url.Values{"order_id": {order}}, 303, "")
	if err := w.e.DB.QueryRow("SELECT listing_title FROM orders WHERE id=$1", order).Scan(&title); err != nil || !title.Valid || title.String != "Test listing" {
		t.Fatalf("successful retry did not save snapshot: %+v %v", title, err)
	}
}

// Repricing precedes the stock row lock. If an edit commits while checkout waits
// for that lock, the row-locked terms must not be saved beside the old amount.
func TestPaymentPriceEditWhileStockLockWaitsRollsBackSnapshot(t *testing.T) {
	w := newOrderWorld(t)
	listing := w.e.product(w.vendorID, "digital")
	order := w.e.order(w.buyerID, listing, "BTC", stateDraft)
	provider := &stubProvider{currency: "BTC"}
	w.e.A.payments["BTC"] = provider
	blocker, err := w.e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var blockerPID int
	if err := blocker.QueryRow("UPDATE products SET btc=200000,title='New priced terms' WHERE id=$1 RETURNING pg_backend_pid()", listing).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	result := make(chan int, 1)
	go func() { result <- w.post("/orders/pay", w.buyer, url.Values{"order_id": {order}}).Code }()
	waitFor(t, func() bool {
		return w.count(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()
			AND query LIKE 'UPDATE products SET stock=stock-1%'
			AND $1=ANY(pg_blocking_pids(pid))`, blockerPID) == 1
	})
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-result; code != 409 {
		t.Fatalf("price edit racing checkout returned %d, want 409", code)
	}
	var amount int64
	var title, description, kind sql.NullString
	if err := w.e.DB.QueryRow("SELECT amount,listing_title,listing_description,listing_kind FROM orders WHERE id=$1", order).Scan(&amount, &title, &description, &kind); err != nil {
		t.Fatal(err)
	}
	if amount != 100000 || title.Valid || description.Valid || kind.Valid || w.state(order) != stateDraft || w.stock(listing) != 5 || len(provider.created) != 0 ||
		w.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", order) != 0 ||
		w.count("SELECT count(*) FROM order_events WHERE order_id=$1", order) != 0 ||
		w.count("SELECT count(*) FROM notifications WHERE body LIKE '%'||$1||'%'", order[:8]) != 0 {
		t.Fatal("racing price edit left an order, stock, address, event, or notice side effect")
	}
	w.expect("/orders/pay", w.buyer, url.Values{"order_id": {order}}, 303, "")
	if err := w.e.DB.QueryRow("SELECT amount,listing_title FROM orders WHERE id=$1", order).Scan(&amount, &title); err != nil || amount != 200000 || !title.Valid || title.String != "New priced terms" {
		t.Fatalf("retry did not atomically use revised price and terms: amount=%d title=%+v err=%v", amount, title, err)
	}
}

func TestEscrowCheckoutsUseSameListingSnapshot(t *testing.T) {
	t.Run("Bitcoin", func(t *testing.T) {
		x := newEscrowEnv(t)
		x.testEnv.mustExec("UPDATE products SET title='BTC escrow purchase',description='BTC terms' WHERE id=$1", x.product)
		x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
		var title, description, kind string
		if err := x.DB.QueryRow("SELECT listing_title,listing_description,listing_kind FROM orders WHERE id=$1", x.order).Scan(&title, &description, &kind); err != nil || title != "BTC escrow purchase" || description != "BTC terms" || kind != "digital" {
			t.Fatalf("Bitcoin checkout snapshot %q %q %q: %v", title, description, kind, err)
		}
	})
	t.Run("Monero", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order := x.draft(x.product)
		x.testEnv.mustExec("UPDATE products SET title='XMR escrow purchase',description='XMR terms' WHERE id=$1", x.product)
		x.mustCheckout(order)
		var title, description, kind string
		if err := x.DB.QueryRow("SELECT listing_title,listing_description,listing_kind FROM orders WHERE id=$1", order).Scan(&title, &description, &kind); err != nil || title != "XMR escrow purchase" || description != "XMR terms" || kind != "digital" {
			t.Fatalf("Monero checkout snapshot %q %q %q: %v", title, description, kind, err)
		}
	})
}

func TestListingSnapshotRejectsPartialColumns(t *testing.T) {
	w := newOrderWorld(t)
	listing := w.e.product(w.vendorID, "digital")
	order := w.e.order(w.buyerID, listing, "BTC", stateDraft)
	for _, query := range []string{
		"UPDATE orders SET listing_title='title',listing_description='description' WHERE id=$1",
		"UPDATE orders SET listing_kind='digital' WHERE id=$1",
	} {
		if _, err := w.e.DB.Exec(query, order); err == nil {
			t.Fatal("partial listing snapshot passed schema constraint")
		}
	}
	var title, description, kind sql.NullString
	if err := w.e.DB.QueryRow("SELECT listing_title,listing_description,listing_kind FROM orders WHERE id=$1", order).Scan(&title, &description, &kind); err != nil || title.Valid || description.Valid || kind.Valid {
		t.Fatalf("partial-snapshot refusal changed draft: %+v %+v %+v %v", title, description, kind, err)
	}
}
