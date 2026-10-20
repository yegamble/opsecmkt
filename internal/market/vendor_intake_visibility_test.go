package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestAwayVendorRefusesDraftAndExistingDraftCheckout(t *testing.T) {
	for _, c := range []struct {
		column, reason string
	}{
		{"vendor_vacation", sellerVacation},
		{"vendor_deactivated", sellerDeactivated},
	} {
		t.Run(c.column, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			product := x.digital(x.vendor)
			order := x.testEnv.order(x.buyer, product, "XMR", stateDraft)
			agExec(x.testEnv, "UPDATE users SET "+c.column+"=true WHERE id=$1", x.vendor)

			w := x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {product}, "currency": {"XMR"}})
			if w.Code != 409 || !strings.Contains(w.Body.String(), c.reason) {
				t.Fatalf("new draft: %d %s", w.Code, w.Body.String())
			}
			page := x.page("/order?id="+order, x.buyerSess)
			if !strings.Contains(page, c.reason) || strings.Contains(page, `action="/orders/escrow-xmr"`) {
				t.Fatal("stale draft offered escrow while the vendor was away")
			}
			calls := x.fake.count()
			w = x.checkout(x.buyerSess, order, testXMRAddr("away refund "+c.column, true))
			if w.Code != 409 || !strings.Contains(w.Body.String(), c.reason) {
				t.Fatalf("stale draft checkout: %d %s", w.Code, w.Body.String())
			}
			if x.state(order) != stateDraft || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", order) != 0 ||
				agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE state='claimed'") != 0 || x.fake.count() != calls {
				t.Fatal("refused checkout changed the draft, claimed a slot or made a wallet call")
			}
			if agInt(x.testEnv, "SELECT count(*) FROM orders") != 1 {
				t.Fatal("the refused draft added an order")
			}
		})
	}
}

func TestDeactivatedVendorImagesRemainOnlyForExistingOrderReaders(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("away_images_vendor", "vendor")
	buyerID, buyer := e.user("away_images_buyer", "buyer")
	_, stranger := e.user("away_images_stranger", "buyer")
	product := e.product(vendorID, "digital")
	photo := e.image(vendorID, product, "photo")
	avatar := e.image(vendorID, "", "avatar")
	e.order(buyerID, product, "XMR", statePaid)
	e.wantPublic(photo, "active vendor photo")
	e.wantPublic(avatar, "active vendor avatar")

	agExec(e, "UPDATE users SET vendor_deactivated=true WHERE id=$1", vendorID)
	e.wantHidden(photo, "deactivated vendor photo")
	e.wantHidden(avatar, "deactivated vendor avatar")
	e.wantDesk(buyer, photo, "existing order buyer's listing photo")
	e.wantDeskHidden(stranger, photo, "unrelated buyer's listing photo")
	e.wantDeskHidden(buyer, avatar, "buyer's access does not publish the hidden vendor avatar")
	e.wantDesk(vendor, avatar, "owner's avatar")
	page := e.body("GET", "/product?id="+product, buyer, nil, 200)
	mustContain(t, page, deskImageURL(photo.id, true))
	mustNotContain(t, page, `src="`+publicImageURL(photo.id, true)+`"`)
	mustNotContain(t, page, `href="`+publicImageURL(photo.id, true)+`"`)
	e.body("GET", "/product?id="+product, stranger, nil, 404)

	// A later staff promotion does not republish photos of a hidden storefront.
	// Staff-avatar publication remains an independent opt-in.
	agExec(e, "UPDATE users SET role='admin',staff_hidden=false WHERE id=$1", vendorID)
	agExec(e, "INSERT INTO settings(key,value) VALUES('staff_list_public','true') ON CONFLICT(key) DO UPDATE SET value=excluded.value")
	e.wantHidden(photo, "deactivated storefront photo after staff promotion")
	e.wantPublic(avatar, "publicly listed staff avatar after promotion")
	agExec(e, "UPDATE users SET role='vendor',vendor_deactivated=false,vendor_vacation=true WHERE id=$1", vendorID)
	e.wantPublic(photo, "vacation vendor photo")
	e.wantPublic(avatar, "vacation vendor avatar")
}

func TestLegacyVendorWithoutPGPRefusesEveryListingKind(t *testing.T) {
	for _, kind := range []string{"digital", "physical"} {
		t.Run(kind, func(t *testing.T) {
			e := newTestApp(t)
			vendorID, _ := e.user("keyless_vendor_"+kind, "vendor")
			_, buyer := e.user("keyless_buyer_"+kind, "buyer")
			product := e.product(vendorID, kind)
			agExec(e, "UPDATE users SET pgp='' WHERE id=$1", vendorID)
			w := e.do("POST", "/orders", buyer, url.Values{"product_id": {product}, "currency": {"XMR"}})
			if w.Code != 409 || !strings.Contains(w.Body.String(), sellerPGPRequired) {
				t.Fatalf("keyless vendor draft: %d %s", w.Code, w.Body.String())
			}
			if agInt(e, "SELECT count(*) FROM orders") != 0 {
				t.Fatal("a keyless vendor received an order")
			}
			page := e.body("GET", "/product?id="+product, buyer, nil, 200)
			mustContain(t, page, sellerPGPRequired)
			mustNotContain(t, page, `action="/orders"`)
		})
	}
}

func TestDraftCheckoutFormsRequireCurrentBrowserPGPAuthentication(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.testEnv.order(x.buyer, x.product, "XMR", stateDraft)
	before := x.page("/order?id="+order, x.buyerSess)
	mustContain(t, before, `action="/orders/escrow-xmr"`)

	// A saved account key and an earlier login do not authorize a browser whose
	// session no longer has proof for the current key.
	agExec(x.testEnv, "UPDATE sessions SET pgp_access_key='' WHERE user_id=$1", x.buyer)
	page := x.page("/order?id="+order, x.buyerSess)
	mustContain(t, page, pgpAccessRequiredMessage)
	mustNotContain(t, page, `action="/orders/escrow-xmr"`)
	mustContain(t, page, `action="/orders/cancel"`)
}
