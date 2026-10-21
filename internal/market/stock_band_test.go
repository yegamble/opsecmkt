package market

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestStockBandEdges(t *testing.T) {
	for n, want := range map[int]string{-1: "0", 0: "0", 1: "1–5", 5: "1–5", 6: "6–20", 20: "6–20", 21: "21+", 1000000: "21+"} {
		if got := stockBand(n); got != want {
			t.Errorf("stockBand(%d) = %q, want %q", n, got, want)
		}
	}
}

// A-520 (PRIV60-6): a listing's exact stock is shown only to its vendor and to staff; buyers and signed-out visitors
// see its band on the catalog, the vendor's page, the listing page and the checkout page, the checkout's quantity
// field does not carry the count as its maximum, and a refused quantity names no count. A sold-out or unbuyable card
// still says so (A-575). DB integration.
func TestPublicStockShownInBands(t *testing.T) {
	p := newPayEnv(t)
	seed := "stockband" + randomToken()[:6]
	_, modSess := p.user("sb_mod_"+randomToken()[:6], "moderator")
	listings := map[int]string{} // exact stock -> listing
	for _, n := range []int{3, 13, 47} {
		id := p.product(p.vendor.ID, "physical")
		agExec(p.testEnv, "UPDATE products SET title=$2,stock=$3 WHERE id=$1", id, seed+" "+strconv.Itoa(n), n)
		listings[n] = id
	}
	band := map[int]string{3: "1–5", 13: "6–20", 47: "21+"}
	storefront := "/vendor?id=" + p.vendor.ID
	catalog := "/?q=" + seed
	listingStock := func(page, label string) string {
		page = html.UnescapeString(page)
		_, after, ok := strings.Cut(page, "<dt>"+label+"</dt>\n<dd>")
		if !ok {
			return ""
		}
		v, _, _ := strings.Cut(after, "</dd>")
		return v
	}
	for _, viewer := range []struct {
		name, sess string
		exact      bool
	}{
		{"signed out", "", false},
		{"buyer", p.buyerSess, false},
		{"vendor", p.vendorSess, true},
		{"moderator", modSess, true},
		{"administrator", p.adminSess, true},
	} {
		lists := map[string]string{"catalog": p.page(catalog, viewer.sess), "vendor page": p.page(storefront, viewer.sess)}
		for n, id := range listings {
			want, other := band[n], strconv.Itoa(n)
			if viewer.exact {
				want, other = other, band[n]
			}
			for where, body := range lists {
				card := html.UnescapeString(productCard(body, id))
				if !strings.Contains(card, `<span class="stock">`+want+` AVAILABLE</span>`) || strings.Contains(card, `>`+other+` AVAILABLE`) {
					t.Errorf("%s, %s, stock %d: card %q, want %q AVAILABLE", viewer.name, where, n, card, want)
				}
			}
			if got := listingStock(p.page("/product?id="+id, viewer.sess), "Available stock"); got != want {
				t.Errorf("%s, listing page, stock %d: %q, want %q", viewer.name, n, got, want)
			}
			if viewer.sess != "" && viewer.name != "vendor" {
				checkout := p.page("/checkout?id="+id, viewer.sess)
				if got := listingStock(checkout, "Available units"); got != want {
					t.Errorf("%s, checkout, stock %d: %q, want %q", viewer.name, n, got, want)
				}
				if !viewer.exact && strings.Contains(checkout, `max="`+strconv.Itoa(n)+`"`) {
					t.Errorf("%s, checkout, stock %d: the quantity field's maximum is the exact count", viewer.name, n)
				}
			}
		}
	}

	// The buyer's checkout form: no exact maximum, a band in its help, and an over-large quantity refused without the
	// count (the refusal reads the same for every quantity above it).
	checkout := html.UnescapeString(p.page("/checkout?id="+listings[13], p.buyerSess))
	mustContain(t, checkout, `name="quantity"`, `max="1000000"`, "This listing has 6–20 units available")
	mustNotContain(t, checkout, "Order up to 13")
	countWord := regexp.MustCompile(`(^|[^0-9A-Za-z])13([^0-9A-Za-z]|$)`)
	for _, q := range []string{"14", "20", "999"} {
		body := html.UnescapeString(p.body("POST", "/orders", p.buyerSess, url.Values{"product_id": {listings[13]}, "currency": {"XMR"}, "quantity": {q}}, 400))
		if !strings.Contains(body, "The requested quantity exceeds the available stock. Nothing was changed.") || countWord.MatchString(body) {
			t.Fatalf("the refusal of quantity %s: %s", q, body)
		}
	}
	p.check(p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {listings[13]}, "currency": {"XMR"}, "quantity": {"13"}}), 303)

	// Staff keep the exact count in the checkout help.
	staff := html.UnescapeString(p.page("/checkout?id="+listings[13], modSess))
	mustNotContain(t, staff, "This listing has 6–20 units available")
	if strings.Contains(staff, `name="quantity"`) {
		mustContain(t, staff, `max="13"`, "Order up to 13 available units")
	}

	// Sold out and unbuyable listings still say so to everyone (A-575), never as a band.
	agExec(p.testEnv, "UPDATE products SET stock=0 WHERE id=$1", listings[3])
	if card := productCard(p.page(catalog, p.buyerSess), listings[3]); !strings.Contains(card, "OUT OF STOCK") || strings.Contains(card, " AVAILABLE") {
		t.Fatalf("a sold-out card: %q", card)
	}
	agExec(p.testEnv, "UPDATE users SET vendor_vacation=true WHERE id=$1", p.vendor.ID)
	if card := productCard(p.page(catalog, p.buyerSess), listings[47]); !strings.Contains(card, unbuyableMark) || strings.Contains(card, " AVAILABLE") {
		t.Fatalf("an unbuyable card: %q", card)
	}
	// The catalog's in-stock filter still hides only the sold-out listing.
	inStock := p.page(catalog+"&in_stock=1", p.buyerSess)
	if productCard(inStock, listings[3]) != "" || productCard(inStock, listings[13]) == "" {
		t.Fatal("the in-stock filter does not follow the stock")
	}
}
