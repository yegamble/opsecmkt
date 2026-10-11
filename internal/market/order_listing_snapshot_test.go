package market

import (
	"database/sql"
	"strings"
	"testing"
)

func TestHistoricalPaidOrderKeepsListingTermsAfterEdit(t *testing.T) {
	w := newOrderWorld(t)
	listing := w.e.product(w.vendorID, "digital")
	order := w.e.order(w.buyerID, listing, "BTC", statePaid)
	w.e.mustExec("UPDATE orders SET listing_title='Purchased name',listing_description='Purchased description: private order context',listing_kind='digital' WHERE id=$1", order)
	w.e.mustExec("UPDATE products SET title='Edited live name',description='Edited live description',kind='physical' WHERE id=$1", listing)
	for _, session := range []string{w.buyer, w.vendor} {
		body := w.page("/order?id="+order, session, 200)
		mustContain(t, body, "Purchased name", "Purchased description: private order context", "Listing as it is now; it may have changed since purchase")
		mustNotContain(t, body, "Edited live name", "Edited live description")
	}
	for _, session := range []string{w.other, w.mod} {
		if body := w.page("/order?id="+order, session, 404); strings.Contains(body, "Purchased description: private order context") {
			t.Fatal("unauthorized viewer read purchased terms")
		}
	}
	w.e.mustExec("UPDATE orders SET state='disputed' WHERE id=$1", order)
	w.e.mustExec("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Terms changed later')", randomToken(), order)
	for _, session := range []string{w.buyer, w.vendor, w.mod} {
		body := w.page("/disputes", session, 200)
		mustContain(t, body, "Purchased name", "digital")
		mustNotContain(t, body, "Edited live name")
	}
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

func TestRefusedEscrowCheckoutRollsBackListingSnapshot(t *testing.T) {
	x := newEscrowEnv(t)
	x.testEnv.mustExec("UPDATE products SET title='Checkout terms' WHERE id=$1", x.product)
	w := x.checkout(x.order, "invalid-address")
	if w.Code != 400 {
		t.Fatalf("bad refund address: %d %s", w.Code, w.Body)
	}
	var title, description, kind sql.NullString
	if err := x.DB.QueryRow("SELECT listing_title,listing_description,listing_kind FROM orders WHERE id=$1", x.order).Scan(&title, &description, &kind); err != nil {
		t.Fatal(err)
	}
	if title.Valid || description.Valid || kind.Valid || x.state(x.order) != stateDraft {
		t.Fatal("refused escrow checkout stored a snapshot or changed draft")
	}
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	if err := x.DB.QueryRow("SELECT listing_title FROM orders WHERE id=$1", x.order).Scan(&title); err != nil || title.String != "Checkout terms" {
		t.Fatalf("successful checkout snapshot %+v %v", title, err)
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
