package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestPublishedZeroStockRemainsVisibleWithOptionalFilter(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("stock_filter_vendor", "vendor")
	_, buyer := e.user("stock_filter_buyer", "buyer")
	empty, available := e.product(vendor, "physical"), e.product(vendor, "physical")
	agExec(e, "UPDATE products SET stock=0 WHERE id=$1", empty)
	all := e.body("GET", "/", buyer, nil, 200)
	mustContain(t, all, "/product?id="+empty, "/product?id="+available, "OUT OF STOCK")
	filtered := e.body("GET", "/?in_stock=1&sort=price-low", buyer, nil, 200)
	mustContain(t, filtered, "/product?id="+available, `name="in_stock" value="1"`, "Hide out of stock")
	mustNotContain(t, filtered, "/product?id="+empty)
	if !strings.Contains(filtered, `in_stock=1`) || !strings.Contains(filtered, `name="in_stock" value="1" checked`) {
		t.Fatal("stock filter was not kept in navigation and selected form state")
	}
	if agInt(e, "SELECT count(*) FROM products WHERE id=$1 AND NOT archived AND stock=0", empty) != 1 {
		t.Fatal("browsing archived or changed the empty listing")
	}
	e.check(e.do("POST", "/orders", buyer, url.Values{"product_id": {empty}, "currency": {"XMR"}}), 400)
	if agInt(e, "SELECT count(*) FROM orders WHERE product_id=$1", empty) != 0 {
		t.Fatal("an empty listing accepted a purchase")
	}
}

func TestCatalogStockFilterSurvivesPaginationAndCategories(t *testing.T) {
	d := PageData{InStockOnly: true, Query: "fixture", Sort: "price-high"}
	for _, path := range []string{catalogPaging(&d, 30, "1").NextURL, d.CatalogCategoryURL("Digital", "Software / Tools")} {
		u, err := url.Parse(path)
		if err != nil || u.Query().Get("in_stock") != "1" || u.Query().Get("q") != "fixture" || u.Query().Get("sort") != "price-high" {
			t.Fatalf("navigation dropped combined stock filters: %q (%v)", path, err)
		}
	}
}
