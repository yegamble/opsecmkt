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

// A-715 (SEC98-1): a buyer's saved draft above the current stock is refused at checkout in one sentence for every stock
// value below the saved quantity, so one draft at the top of a stock band is not a GET-only monitor of the exact stock
// and of every sale. The vendor and the staff still see the exact stock. DB integration.
func TestDraftAboveStockNamesNoCount(t *testing.T) {
	x := newXMREscrowEnv(t)
	product := x.physical(x.vendor)
	agExec(x.testEnv, "UPDATE products SET stock=5 WHERE id=$1", product)
	draft := x.order(x.buyer, product, "XMR", stateDraft)
	agExec(x.testEnv, "UPDATE orders SET quantity=5 WHERE id=$1", draft)
	_, modSess := x.user("sbd_mod_"+randomToken()[:6], "moderator")
	const refusal = "The saved quantity is more than this listing has available now. Review the listing to save a smaller quantity, or cancel this unfunded draft."
	tokens := regexp.MustCompile(`value="[0-9a-f]{16,}"`)
	draftPage := func(stock int) string {
		t.Helper()
		var got int
		if err := x.DB.QueryRow("SELECT stock FROM products WHERE id=$1", product).Scan(&got); err != nil || got != stock {
			t.Fatalf("stock %d (%v), want %d", got, err, stock)
		}
		page := x.orderPage(draft, x.buyerSess)
		mustContain(t, page, refusal)
		for _, leak := range []string{"Only " + strconv.Itoa(stock), strconv.Itoa(stock) + " units", strconv.Itoa(stock) + " unit "} {
			mustNotContain(t, page, leak)
		}
		return tokens.ReplaceAllString(page, `value="token"`)
	}
	// Exactly the stock: the draft can still check out.
	mustNotContain(t, x.orderPage(draft, x.buyerSess), refusal)
	x.mustPoll() // refills the free escrow accounts
	x.freshCheckoutOf(product, 2)
	three := draftPage(3)
	x.mustPoll() // refills the free escrow accounts
	x.freshCheckoutOf(product, 2)
	one := draftPage(1)
	if three != one {
		a, b := strings.Split(three, "\n"), strings.Split(one, "\n")
		for i := range min(len(a), len(b)) {
			if a[i] != b[i] {
				t.Errorf("a sale changed the buyer's draft page at line %d:\n%s\n%s", i, a[i], b[i])
				break
			}
		}
		t.Fatalf("a sale changed the buyer's draft page (%d and %d lines)", len(a), len(b))
	}
	agExec(x.testEnv, "UPDATE products SET stock=0 WHERE id=$1", product)
	draftPage(0)
	agExec(x.testEnv, "UPDATE products SET stock=1 WHERE id=$1", product)
	// The vendor and the staff keep the exact count on the listing, where they see it today.
	for _, sess := range []string{x.vendorSess, modSess} {
		mustContain(t, html.UnescapeString(x.page("/product?id="+product, sess)), "<dt>Available stock</dt>\n<dd>1</dd>")
	}
}
