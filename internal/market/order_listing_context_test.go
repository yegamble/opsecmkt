package market

import (
	"strings"
	"testing"
)

// A-164: the order page links to the listing ("as it is now"), and an archived listing's product page is served
// to the parties of an order on it and to the staff allowed to read that order (orderReadable), with an Archived
// badge, no checkout and no delivery content. Everyone else, and the catalog, vendor page and checkout, still
// treat it as gone.

const archivedListingBadge = `<span class="badge neutral">Archived</span>`

func TestArchivedListingVisibleThroughOrder(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("a164_vendor", "vendor")
	buyerID, buyer := e.user("a164_buyer", "buyer")
	otherBuyerID, otherBuyer := e.user("a164_buyer2", "buyer")
	_, stranger := e.user("a164_stranger", "buyer")
	_, otherVendor := e.user("a164_vendor2", "vendor")
	_, moderator := e.user("a164_mod", "moderator")
	_, admin := e.user("a164_admin", "admin")

	// disputed: a digital listing with delivery content and a disputed order (staff may read the order).
	// paid: a listing with an ordinary paid order (staff may not read it) and, later, a flagged payment.
	disputed, paid := e.titledListing(vendorID, "A164 disputed listing"), e.titledListing(vendorID, "A164 paid listing")
	e.mustExec(`UPDATE products SET kind='digital',description='A164 full description of what was sold',delivery_content='A164-SECRET-DELIVERY' WHERE id=$1`, disputed)
	disputedOrder := e.order(buyerID, disputed, "BTC", stateDisputed)
	paidOrder := e.order(otherBuyerID, paid, "BTC", statePaid)

	// The order page names the listing and links to it as it is now, for each viewer allowed to read the order.
	for _, s := range []string{buyer, vendor, moderator, admin} {
		body := e.body("GET", "/order?id="+disputedOrder, s, nil, 200)
		mustContain(t, body, `href="/product?id=`+disputed+`"`, "Listing as it is now; it may have changed since purchase")
	}

	// Live listing: public, with checkout and no Archived badge.
	for _, s := range []string{"", stranger, buyer, moderator} {
		body := e.body("GET", "/product?id="+disputed, s, nil, 200)
		mustContain(t, body, "A164 full description of what was sold", `href="/checkout?id=`+disputed+`"`)
		mustNotContain(t, body, archivedListingBadge, "A164-SECRET-DELIVERY")
	}

	e.mustExec(`UPDATE products SET archived=true,archived_at=now() WHERE id=ANY($1::text[])`, "{"+disputed+","+paid+"}")

	// Archived: the order's buyer and vendor, and non-party staff reading the disputed order, see it with the badge,
	// no checkout link and no delivery content.
	for name, s := range map[string]string{"buyer": buyer, "vendor": vendor, "moderator": moderator, "admin": admin} {
		w := e.do("GET", "/product?id="+disputed, s, nil)
		if w.Code != 200 {
			t.Fatalf("%s: archived listing of a readable order: status %d, want 200", name, w.Code)
		}
		body := w.Body.String()
		mustContain(t, body, archivedListingBadge, "A164 full description of what was sold", "accepts no new orders")
		mustNotContain(t, body, `href="/checkout?id=`, "Review order draft", "A164-SECRET-DELIVERY")
	}

	// The vendor is told who can still open it.
	mustContain(t, e.body("GET", "/listing-edit?id="+disputed, vendor, nil, 200), "Existing orders continue unchanged; their buyers, and staff reviewing them, can still open this listing's page from the order.")

	// Everyone else still gets 404: anonymous, an unrelated buyer, another vendor, and staff whose order view does
	// not cover the paid order (not disputed, no flagged payment).
	for name, c := range map[string]struct{ session, id string }{
		"anonymous":             {"", disputed},
		"unrelated buyer":       {stranger, disputed},
		"other vendor":          {otherVendor, disputed},
		"buyer of other order":  {otherBuyer, disputed},
		"moderator, paid order": {moderator, paid},
		"admin, paid order":     {admin, paid},
		"disputed buyer, paid":  {buyer, paid},
	} {
		if w := e.do("GET", "/product?id="+c.id, c.session, nil); w.Code != 404 {
			t.Fatalf("%s: archived listing: status %d, want 404", name, w.Code)
		}
	}
	// The paid order's own parties do see it.
	for _, s := range []string{otherBuyer, vendor} {
		mustContain(t, e.body("GET", "/product?id="+paid, s, nil, 200), archivedListingBadge)
	}

	// A flagged payment opens the order, and so the listing, to staff (the order page's rule).
	e.mustExec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged) VALUES($1,'BTC','a164-flagged',0,'tb1qa164',1,1,true)`, paidOrder)
	for _, s := range []string{moderator, admin} {
		e.body("GET", "/order?id="+paidOrder, s, nil, 200)
		mustContain(t, e.body("GET", "/product?id="+paid, s, nil, 200), archivedListingBadge)
	}
	if w := e.do("GET", "/product?id="+paid, stranger, nil); w.Code != 404 {
		t.Fatalf("unrelated buyer, flagged order: status %d, want 404", w.Code)
	}

	// Checkout, the catalog and the vendor page still treat archived listings as gone, for parties and staff too.
	for _, s := range []string{buyer, vendor, admin} {
		if w := e.do("GET", "/checkout?id="+disputed, s, nil); w.Code != 404 {
			t.Fatalf("checkout of an archived listing: status %d, want 404", w.Code)
		}
		for _, page := range []string{"/", "/?q=A164", "/vendor?id=" + vendorID} {
			if body := e.body("GET", page, s, nil, 200); strings.Contains(body, "A164 disputed listing") || strings.Contains(body, "A164 paid listing") {
				t.Fatalf("%s lists an archived listing", page)
			}
		}
	}

	// A listing taken down by an administrator (A-163) is archived too: the order's buyer sees the listing, never
	// the takedown reason; strangers get 404.
	e.mustExec(`UPDATE products SET admin_archive_reason='A164 takedown reason text' WHERE id=$1`, disputed)
	body := e.body("GET", "/product?id="+disputed, buyer, nil, 200)
	mustContain(t, body, archivedListingBadge)
	mustNotContain(t, body, "A164 takedown reason text")
	if w := e.do("GET", "/product?id="+disputed, stranger, nil); w.Code != 404 {
		t.Fatalf("unrelated buyer, taken-down listing: status %d, want 404", w.Code)
	}
}
