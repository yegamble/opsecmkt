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
	disputedOrder := e.order(buyerID, disputed, "XMR", stateDisputed)
	addOpenDisputeFixture(t, e, disputedOrder)
	paidOrder := e.order(otherBuyerID, paid, "XMR", statePaid)

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
	e.mustExec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged) VALUES($1,'XMR','a164-flagged',0,'tb1qa164',1,1,true)`, paidOrder)
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

// A-264: an archived listing's photos show on its product page to those who may open that page (the order's
// parties and the staff reviewing it, orderReadable), served on the authenticated /desk/img route under the same
// rule. The public /img route still refuses them, and nobody else gets them on either route.
func TestArchivedListingPhotosThroughOrder(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("a264_vendor", "vendor")
	buyerID, buyer := e.user("a264_buyer", "buyer")
	otherBuyerID, otherBuyer := e.user("a264_buyer2", "buyer")
	_, stranger := e.user("a264_stranger", "buyer")
	otherVendorID, otherVendor := e.user("a264_vendor2", "vendor")
	_, moderator := e.user("a264_mod", "moderator")
	_, admin := e.user("a264_admin", "admin")
	disputed, paid := e.titledListing(vendorID, "A264 disputed listing"), e.titledListing(vendorID, "A264 paid listing")
	photo := e.image(vendorID, disputed, "photo")
	second := e.image(vendorID, disputed, "photo")
	e.mustExec("UPDATE images SET created=created+interval '1 second' WHERE id=$1", second.id)
	paidPhoto := e.image(vendorID, paid, "photo")
	avatar := e.image(vendorID, "", "avatar")
	disputedOrder := e.order(buyerID, disputed, "XMR", stateDisputed)
	addOpenDisputeFixture(t, e, disputedOrder)
	e.order(otherBuyerID, paid, "XMR", statePaid)

	// Live: public route, public URLs on the page; the desk route adds nothing for the order's buyer or staff.
	e.wantPublic(photo, "live listing")
	mustContain(t, e.body("GET", "/product?id="+disputed, buyer, nil, 200), `<img src="/img/`+photo.id+`-f"`)
	e.wantDeskHidden(buyer, photo, "buyer, live listing on the desk route")
	e.wantDeskHidden(moderator, photo, "moderator, live listing on the desk route")

	e.mustExec(`UPDATE products SET archived=true,archived_at=now() WHERE id=ANY($1::text[])`, "{"+disputed+","+paid+"}")

	// The public route still refuses the archived listing's photos, to everyone (it reads no session).
	e.wantHidden(photo, "archived, public route")
	e.wantHidden(paidPhoto, "archived paid listing, public route")

	// Those who may read the disputed order see its listing's photos on its product page, on /desk/img, and the
	// desk route serves them; the page links nothing on the public route and offers no staff remove form.
	for name, s := range map[string]string{"buyer": buyer, "vendor": vendor, "moderator": moderator, "admin": admin} {
		body := e.body("GET", "/product?id="+disputed, s, nil, 200)
		mustContain(t, body, archivedListingBadge, `<img src="/desk/img/`+photo.id+`-f" alt="Photo 1 of 2: A264 disputed listing"`,
			`<a href="/desk/img/`+second.id+`-f">`, `<img src="/desk/img/`+second.id+`-t"`, "Photos are supplied by the vendor")
		mustNotContain(t, body, `"/img/`, "ILLUSTRATION", `action="/images/remove"`)
		e.wantDesk(s, photo, name+", archived photo of a readable order")
		e.wantDesk(s, second, name+", second archived photo")
	}
	// Everyone else: the desk route answers as for an unknown id, as the public route does.
	for name, s := range map[string]string{"unrelated buyer": stranger, "other vendor": otherVendor, "buyer of the other order": otherBuyer} {
		e.wantDeskHidden(s, photo, name)
	}
	// The paid order is not open to staff (not disputed, no flagged payment): its listing's photos neither.
	e.wantDeskHidden(moderator, paidPhoto, "moderator, archived photo of an ordinary paid order")
	e.wantDeskHidden(buyer, paidPhoto, "buyer of another order")
	e.wantDesk(otherBuyer, paidPhoto, "buyer of the paid order")
	mustContain(t, e.body("GET", "/product?id="+paid, otherBuyer, nil, 200), `<img src="/desk/img/`+paidPhoto.id+`-f"`)
	// The rule covers listing photos only: the vendor's avatar stays refused to the order's buyer and staff.
	e.wantDeskHidden(buyer, avatar, "buyer, vendor avatar")
	e.wantDeskHidden(moderator, avatar, "moderator, vendor avatar")

	// A-296 (P2): only the listing vendor's own photos. A photo row another vendor owns that names the listing (a
	// forged or corrupted row) is not served to the order's readers and is not on the page.
	foreign := e.image(otherVendorID, disputed, "photo")
	for name, s := range map[string]string{"buyer": buyer, "moderator": moderator} {
		e.wantDeskHidden(s, foreign, name+", another vendor's photo row naming the listing")
		body := e.body("GET", "/product?id="+disputed, s, nil, 200)
		mustContain(t, body, `alt="Photo 1 of 2: A264 disputed listing"`)
		mustNotContain(t, body, foreign.id)
	}

	// A-296 (P7): a listing an administrator took down (A-163) is archived too; its order's readers still see its
	// photos on the desk route and its page.
	e.mustExec(`UPDATE products SET admin_archive_reason='A296 takedown reason text' WHERE id=$1`, disputed)
	for name, s := range map[string]string{"buyer": buyer, "moderator": moderator} {
		e.wantDesk(s, photo, name+", taken-down listing's photo")
		body := e.body("GET", "/product?id="+disputed, s, nil, 200)
		mustContain(t, body, `<img src="/desk/img/`+photo.id+`-f"`)
	}
	e.wantDeskHidden(stranger, photo, "unrelated buyer, taken-down listing")
}

// A-265: the moderation desk's dispute entries and payment-review rows, the parties' /disputes entries and the
// vendor desk's incoming-order rows link each order's listing, and each link opens for that viewer even once the
// listing is archived (the order page's rule, A-164).
func TestDeskOrderRowsLinkTheirListing(t *testing.T) {
	w := newDisputeOrderWorld(t)
	e := w.e
	disputedOrder := w.fundedPhysicalFor(w.buyerID, w.buyer, w.vendorID, w.vendor)
	disputed := agStr(e, "SELECT product_id FROM orders WHERE id=$1", disputedOrder)
	e.mustExec("UPDATE products SET title='A265 disputed listing' WHERE id=$1", disputed)
	e.mustExec("UPDATE orders SET listing_title='A265 disputed listing' WHERE id=$1", disputedOrder)
	flagged := e.titledListing(w.vendorID, "A265 flagged listing")
	if code, body := w.x.xmrShip(disputedOrder, w.vendor, testXMRAddr("a265-claim", true), testPassword); code != 303 {
		t.Fatalf("ship disputed escrow: %d %s", code, body)
	}
	w.openDispute(disputedOrder, w.buyer, "The parcel never arrived at the agreed place.")
	flaggedOrder := e.order(w.otherID, flagged, "XMR", stateCancelled)
	// This direct fixture represents a legacy cancelled order whose purchase-time listing terms were never recorded.
	e.mustExec(`UPDATE orders SET listing_title=NULL,listing_description=NULL,listing_kind=NULL WHERE id=$1`, flaggedOrder)
	e.mustExec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged,after_close) VALUES($1,'XMR','a265-flagged',0,'tb1qa265',1,1,true,true)`, flaggedOrder)
	e.mustExec(`UPDATE products SET archived=true,archived_at=now() WHERE id=ANY($1::text[])`, "{"+disputed+","+flagged+"}")

	// segment returns the part of page from the last start before marker to the next end after it.
	segment := func(page, start, marker, end string) string {
		t.Helper()
		i := strings.Index(page, marker)
		if i < 0 {
			t.Fatalf("page lacks %q", marker)
		}
		from := strings.LastIndex(page[:i], start)
		to := strings.Index(page[i:], end)
		if from < 0 || to < 0 {
			t.Fatalf("no %q…%q around %q", start, end, marker)
		}
		return page[from : i+to]
	}
	opens := func(session, product string) {
		t.Helper()
		mustContain(t, e.body("GET", "/product?id="+product, session, nil, 200), archivedListingBadge)
	}
	disputeLink := `<a class="text-link" href="/product?id=` + disputed + `">A265 disputed listing</a>`

	desk := w.page("/moderator", w.mod, 200)
	mustContain(t, segment(desk, `<article class="message">`, "Order "+disputedOrder+"<", "</article>"), disputeLink)
	// A flag on an order without escrow stays readable on its order, but never enters the live review queue.
	mustNotContain(t, desk, `href="/order?id=`+flaggedOrder+`"`)
	opens(w.mod, disputed)
	opens(w.mod, flagged)

	for _, s := range []string{w.buyer, w.vendor} {
		mustContain(t, segment(w.page("/disputes", s, 200), `<article class="message">`, "Order "+disputedOrder+"<", "</article>"), disputeLink)
		opens(s, disputed)
	}

	vendorDesk := w.page("/vendor-dashboard", w.vendor, 200)
	for id, c := range map[string]struct{ order, title string }{disputed: {disputedOrder, "A265 disputed listing"}, flagged: {flaggedOrder, "Listing terms were not recorded for this order"}} {
		mustContain(t, segment(vendorDesk, "<tr>", `href="/order?id=`+c.order+`"`, "</tr>"),
			`<a class="text-link" href="/product?id=`+id+`">Listing<span class="sr-only">: `+c.title+`</span></a>`)
		opens(w.vendor, id)
	}
}

func addOpenDisputeFixture(t *testing.T, e *testEnv, order string) {
	t.Helper()
	e.mustExec(`INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The parcel never arrived; please review.')`, randomToken(), order)
}
