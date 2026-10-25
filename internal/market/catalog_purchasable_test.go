package market

import (
	"html"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// productCard is the catalog, vendor-page or vendor-desk card of listing id in body ("" when absent).
func productCard(body, id string) string {
	for _, card := range strings.Split(body, `<article class="product-card`)[1:] {
		card, _, _ = strings.Cut(card, "</article>")
		if strings.Contains(card, "?id="+id+`"`) {
			return html.UnescapeString(card)
		}
	}
	return ""
}

// cardCount is how many listing cards body shows.
func cardCount(body string) int { return strings.Count(body, `<article class="product-card`) }

// unbuyableMark is what a card shows instead of its stock count when no buyer can order the listing (A-575).
const unbuyableMark = "Not available for purchase"

// TestCatalogMarksUnbuyableListings (A-575): every listing a new order is refused on is marked "Not available for
// purchase" instead of its stock band ("1–5 AVAILABLE", A-520) on the catalog (with search, category, region and paging), on its vendor's
// public page and, with the reason, on the vendor's own desk; the draft action refuses each one and accepts the
// control. Both sides read the one rule (purchasable.go), so a condition dropped from it fails here twice.
func TestCatalogMarksUnbuyableListings(t *testing.T) {
	e := newTestApp(t)
	suffix := randomToken()[:6]
	seed := "unbuyable" + suffix
	_, buyer := e.user("ub_buyer_"+suffix, "buyer")
	vendor, vendorSess := e.user("ub_vendor_"+suffix, "vendor")
	listing := func(owner, kind string) string {
		id := e.product(owner, kind)
		agExec(e, "UPDATE products SET title=$2 WHERE id=$1", id, seed+" "+kind)
		return id
	}
	control := listing(vendor, "physical")
	manual := listing(vendor, "digital")
	agExec(e, "UPDATE products SET digital_delivery_mode='manual' WHERE id=$1", manual)

	type unbuyable struct {
		name, id, session, reason string
		code                      int // POST /orders
	}
	var cases []unbuyable
	// Listing conditions, on the control's vendor.
	service := listing(vendor, "service")
	cases = append(cases, unbuyable{"legacy service listing", service, vendorSess, "legacy service listing", 409})
	noUnits := listing(vendor, "digital")
	agExec(e, "UPDATE products SET digital_delivery_mode='inventory' WHERE id=$1", noUnits)
	cases = append(cases, unbuyable{"digital inventory without a free unit", noUnits, vendorSess, "no distinct digital units left", 409})
	soldOut := listing(vendor, "physical")
	agExec(e, "UPDATE products SET stock=0 WHERE id=$1", soldOut)
	cases = append(cases, unbuyable{"sold out", soldOut, vendorSess, unavailableOutOfStock, 400})
	subFloor := listing(vendor, "physical")
	agExec(e, "UPDATE products SET xmr=$2 WHERE id=$1", subFloor, payoutFloor("XMR", "release")-1)
	cases = append(cases, unbuyable{"price below the settlement minimum", subFloor, vendorSess, "below the escrow settlement minimum", 400})
	// Seller conditions, each on a vendor of its own.
	for _, s := range []struct{ name, update, reason string }{
		{"suspended vendor", "UPDATE users SET suspended_at=now() WHERE id=$1", sellerSuspended},
		{"banned vendor", "UPDATE users SET suspended_at=now(),banned_at=now(),ban_reason='fixture ban reason' WHERE id=$1", sellerClosed},
		{"vendor on vacation", "UPDATE users SET vendor_vacation=true WHERE id=$1", sellerVacation},
		{"seller no longer a vendor", "UPDATE users SET role='buyer',vendor_approved=false WHERE id=$1", sellerNotVendor},
		{"vendor without a usable key", "UPDATE users SET pgp='',pgp_fingerprint='' WHERE id=$1", sellerPGPRequired},
	} {
		owner, session := e.user("ub_"+strings.ReplaceAll(s.name, " ", "")[:10]+suffix, "vendor")
		id := listing(owner, "physical")
		agExec(e, s.update, owner)
		code := 409
		if s.reason == sellerNotVendor {
			code = 400
		}
		cases = append(cases, unbuyable{s.name, id, session, s.reason, code})
	}
	// A deactivated storefront's listings stay off the catalog and out of its count, as before.
	deactivatedVendor, _ := e.user("ub_deact_"+suffix, "vendor")
	deactivated := listing(deactivatedVendor, "physical")
	agExec(e, "UPDATE users SET vendor_deactivated=true WHERE id=$1", deactivatedVendor)
	// Fillers take the search to two pages, so paging is checked against the marks.
	for len(cases)+2 <= catalogPageSize {
		cases = append(cases, unbuyable{name: "filler", id: listing(vendor, "physical")})
	}
	total := len(cases) + 2

	first := e.body("GET", "/?q="+seed, buyer, nil, 200)
	pages := first + e.body("GET", "/?q="+seed+"&page=2", buyer, nil, 200)
	mustContain(t, first, `<span class="accent">`+strconv.Itoa(total)+`</span> listings`)
	if got := cardCount(first); got != catalogPageSize {
		t.Fatalf("page 1 shows %d cards, want %d", got, catalogPageSize)
	}
	if got := cardCount(pages); got != total {
		t.Fatalf("the two pages show %d cards; the count says %d", got, total)
	}
	filtered := e.body("GET", "/?q="+seed+"&category=Hardware&region=Worldwide", buyer, nil, 200) +
		e.body("GET", "/?q="+seed+"&category=Hardware&region=Worldwide&page=2", buyer, nil, 200)
	if got := cardCount(filtered); got != total {
		t.Fatalf("category and region filters show %d cards, want %d", got, total)
	}
	for _, id := range []string{control, manual} {
		for _, body := range []string{pages, filtered} {
			card := productCard(body, id)
			if !strings.Contains(card, "1–5 AVAILABLE") || strings.Contains(card, unbuyableMark) { // A-520: a buyer sees the band
				t.Fatalf("a purchasable listing is not offered with its stock: %q", card)
			}
		}
	}
	e.check(e.do("POST", "/orders", buyer, url.Values{"product_id": {control}, "currency": {"XMR"}}), 303)
	if productCard(pages+filtered, deactivated) != "" {
		t.Fatal("a deactivated storefront's listing is on the catalog")
	}
	e.check(e.do("POST", "/orders", buyer, url.Values{"product_id": {deactivated}, "currency": {"XMR"}}), 409)

	desk := map[string]string{}
	for _, c := range cases {
		if c.name == "filler" {
			continue
		}
		storefront := "/vendor?id=" + agStr(e, "SELECT vendor_id FROM products WHERE id=$1", c.id)
		bodies := []string{pages, filtered}
		if c.reason == sellerNotVendor {
			e.check(e.do("GET", storefront, buyer, nil), 404) // a former vendor has no storefront
		} else {
			bodies = append(bodies, e.body("GET", storefront, buyer, nil, 200))
		}
		mark := unbuyableMark
		if c.reason == unavailableOutOfStock {
			mark = "OUT OF STOCK" // unchanged: a sold-out card already says so
		}
		for _, body := range bodies {
			card := productCard(body, c.id)
			if card == "" || !strings.Contains(card, mark) || strings.Contains(card, " AVAILABLE") {
				t.Fatalf("%s: card is not marked unavailable: %q", c.name, card)
			}
		}
		w := e.do("POST", "/orders", buyer, url.Values{"product_id": {c.id}, "currency": {"XMR"}})
		e.check(w, c.code)
		if c.reason == unavailableOutOfStock {
			mustContain(t, w.Body.String(), "Product unavailable")
		}
		if agInt(e, "SELECT count(*) FROM orders WHERE product_id=$1", c.id) != 0 {
			t.Fatalf("%s: the draft action accepted an order the catalog marks unavailable", c.name)
		}
		switch c.reason { // the product page gives these seller reasons verbatim (vacation has its own notice)
		case sellerSuspended, sellerClosed, sellerNotVendor, sellerPGPRequired:
			mustContain(t, html.UnescapeString(e.body("GET", "/product?id="+c.id, buyer, nil, 200)), "Unavailable. "+c.reason)
		}
		if c.reason == sellerSuspended || c.reason == sellerClosed {
			// A suspended or banned account's session no longer opens the desk.
			e.check(e.do("GET", "/vendor-dashboard", c.session, nil), 303)
			continue
		}
		if c.reason == sellerNotVendor {
			e.check(e.do("GET", "/vendor-dashboard", c.session, nil), 403) // no desk without the vendor role
			continue
		}
		if desk[c.session] == "" {
			desk[c.session] = html.UnescapeString(e.body("GET", "/vendor-dashboard", c.session, nil, 200))
		}
		card := productCard(desk[c.session], c.id)
		if !strings.Contains(card, unbuyableMark) || !strings.Contains(card, c.reason) {
			t.Fatalf("%s: the vendor's desk does not say why the listing cannot be bought: %q", c.name, card)
		}
	}

	// Escrow-only intake without Monero escrow enabled refuses every draft, so every listing is marked.
	agExec(e, "UPDATE settings SET value=$1 WHERE key=$2", intakeEscrow, paymentIntakeKey("XMR"))
	escrowOnly := e.body("GET", "/?q="+seed, buyer, nil, 200)
	if card := productCard(escrowOnly, manual); !strings.Contains(card, unbuyableMark) {
		t.Fatalf("escrow-only intake without escrow: the card is still offered: %q", card)
	}
	e.check(e.do("POST", "/orders", buyer, url.Values{"product_id": {manual}, "currency": {"XMR"}}), 409)
	mustContain(t, html.UnescapeString(e.body("GET", "/vendor-dashboard", vendorSess, nil, 200)), "Escrow for XMR is not enabled on this market.")
}

// TestCatalogMarksEscrowIneligibleListings (A-575): with Monero escrow on and intake escrow only, a listing escrow
// does not take (escrowEligible) is marked on the catalog and refused by the draft action; one it takes is offered.
// Manual digital fulfilment is offered as the current rule allows it (A-573 is the owner's call).
func TestCatalogMarksEscrowIneligibleListings(t *testing.T) {
	x := newXMREscrowEnv(t)
	seed := "escrowable" + randomToken()[:6]
	title := func(id string) string {
		agExec(x.testEnv, "UPDATE products SET title=$2 WHERE id=$1", id, seed)
		return id
	}
	physical := title(x.physical(x.vendor))
	reusable := title(x.product)
	manual := title(x.testEnv.product(x.vendor, "digital"))
	agExec(x.testEnv, "UPDATE products SET digital_delivery_mode='manual' WHERE id=$1", manual)
	stocked := title(x.testEnv.product(x.vendor, "digital"))
	agExec(x.testEnv, "UPDATE products SET digital_delivery_mode='inventory' WHERE id=$1", stocked)
	agExec(x.testEnv, "INSERT INTO digital_units(id,product_id,content,fingerprint) VALUES($1,$2,'unit-1',repeat('a',64))", randomToken(), stocked)
	noWindow := title(x.testEnv.product(x.vendor, "physical"))
	blank := title(x.testEnv.product(x.vendor, "digital"))

	if got := agStr(x.testEnv, "SELECT value FROM settings WHERE key=$1", paymentIntakeKey("XMR")); got != intakeEscrow {
		t.Fatalf("fixture intake is %q, want escrow only", got)
	}
	body := x.body("GET", "/?q="+seed, x.buyerSess, nil, 200)
	mustContain(t, body, `<span class="accent">6</span> listings`)
	if cardCount(body) != 6 {
		t.Fatalf("catalog shows %d cards, want 6", cardCount(body))
	}
	for _, id := range []string{physical, reusable, manual, stocked} {
		if card := productCard(body, id); strings.Contains(card, unbuyableMark) || !strings.Contains(card, " AVAILABLE") {
			t.Fatalf("an escrow-eligible listing is not offered: %q", card)
		}
		x.check(x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {id}, "currency": {"XMR"}}), 303)
	}
	desk := html.UnescapeString(x.body("GET", "/vendor-dashboard", x.vendorSess, nil, 200))
	for id, reason := range map[string]string{noWindow: "has no dispatch window", blank: "no deliverable automatic content"} {
		if card := productCard(body, id); !strings.Contains(card, unbuyableMark) {
			t.Fatalf("an escrow-ineligible listing is offered: %q", card)
		}
		x.check(x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {id}, "currency": {"XMR"}}), 409)
		if card := productCard(desk, id); !strings.Contains(card, unbuyableMark) || !strings.Contains(card, reason) {
			t.Fatalf("the vendor's desk does not say why: %q", card)
		}
	}
}
