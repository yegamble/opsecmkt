package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCatalogDesignFiltersCombineAndPersist(t *testing.T) {
	a := previewTestApp(t)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/?category=Digital&subcategory=Guides+%2F+Courses&kind=digital&region=Europe&sort=price-high", nil))
	if w.Code != 200 {
		t.Fatalf("catalog: %d", w.Code)
	}
	body := w.Body.String()
	if strings.Count(body, `<article class="product-card`) != 1 || !strings.Contains(body, "Practical Privacy — Field Guide") {
		t.Fatal("subcategory/type/region filters did not combine")
	}
	for _, field := range []string{`name="subcategory" value="Guides / Courses"`, `name="kind" value="digital"`, `name="sort" value="price-high"`} {
		if !strings.Contains(body, field) {
			t.Errorf("search dropped %s", field)
		}
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/?kind=physical&sort=price-high", nil))
	body = w.Body.String()
	if strings.Index(body, ">Encrypted USB Drive") > strings.Index(body, ">Faraday Laptop Bag") {
		t.Error("price high did not sort")
	}
}

func TestCatalogDesignListingTaxonomyValidation(t *testing.T) {
	f := inventoryForm("", map[string]string{"category": "Hardware", "subcategory": "Guides / Courses"})
	if _, err := validateListing(f); err == nil {
		t.Fatal("accepted a subcategory from another category")
	}
	f.Set("subcategory", "USB / Storage")
	f["shipping_regions"] = []string{"North America", "Europe"}
	if _, err := validateListing(f); err != nil {
		t.Fatal(err)
	}
	f["shipping_regions"] = []string{"unlisted destination"}
	if _, err := validateListing(f); err == nil {
		t.Fatal("accepted unknown additional shipping region")
	}
}

func TestCatalogDesignPagingPreservesFilters(t *testing.T) {
	d := &PageData{Query: "report", Category: "Digital", Subcategory: "Research / Reports", Region: "Europe", KindFilter: "digital", Sort: "price-high", Currency: "XMR"}
	paging := catalogPaging(d, 70, "2")
	for _, link := range []string{paging.PrevURL, paging.NextURL} {
		u, err := url.Parse(link)
		if err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{"q": "report", "category": "Digital", "subcategory": "Research / Reports", "region": "Europe", "kind": "digital", "sort": "price-high", "currency": "XMR"} {
			if u.Query().Get(key) != want {
				t.Errorf("%s lost %s", link, key)
			}
		}
	}
}

func TestCatalogDesignListingRoundTrip(t *testing.T) {
	e := newTestApp(t)
	_, session := e.user("taxonomy_vendor", "vendor")
	f := inventoryForm("", map[string]string{"category": "Hardware", "subcategory": "USB / Storage", "region": "Europe"})
	f["shipping_regions"] = []string{"North America", "Asia"}
	created := e.do("POST", "/listings", session, f)
	e.check(created, 303)
	location, err := url.Parse(created.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	id := location.Query().Get("id")
	if id == "" {
		t.Fatal("creation did not return listing id")
	}
	loaded := e.do("GET", "/product?id="+id, session, nil)
	e.check(loaded, 200)
	if !strings.Contains(loaded.Body.String(), "North America") || !strings.Contains(loaded.Body.String(), "USB / Storage") {
		t.Fatal("creation lost taxonomy/shipping")
	}
	f.Set("id", id)
	f.Set("title", "Updated classified listing")
	e.check(e.do("POST", "/listings/update", session, f), 303)
	for _, path := range []string{"/listing-edit?id=" + id, "/product?id=" + id, "/?region=Asia&subcategory=USB+%2F+Storage&kind=physical"} {
		w := e.do("GET", path, session, nil)
		e.check(w, 200)
		if !strings.Contains(w.Body.String(), "USB / Storage") || !strings.Contains(w.Body.String(), "North America") {
			t.Errorf("taxonomy missing on %s", path)
		}
	}
	w := e.do("GET", "/?region=Africa&category=Hardware", "", nil)
	e.check(w, 200)
	if len(cardTitles(w.Body.String())) != 0 {
		t.Fatal("shipping filter admitted unavailable destination")
	}
	w = e.do("GET", "/?region=United+States&kind=physical", "", nil)
	e.check(w, 200)
	if len(cardTitles(w.Body.String())) != 1 {
		t.Fatal("North America shipping excluded United States")
	}
}

func TestCatalogDesignRatingsAndSort(t *testing.T) {
	e := newTestApp(t)
	v, _ := e.user("rating_vendor", "vendor")
	b, _ := e.user("rating_buyer", "buyer")
	low, high := e.product(v, "physical"), e.product(v, "digital")
	if _, err := e.DB.Exec(`UPDATE products SET title=CASE WHEN id=$1 THEN 'Lower price' ELSE 'Higher price' END,xmr=CASE WHEN id=$1 THEN 10000000000 ELSE 90000000000 END WHERE id IN ($1,$2)`, low, high); err != nil {
		t.Fatal(err)
	}
	for _, review := range []struct {
		product, state string
		rating         int
		hidden         bool
	}{{low, stateCompleted, 4, false}, {low, stateCompleted, 1, true}, {high, stateDelivered, 5, false}} {
		o := e.order(b, review.product, "XMR", review.state)
		if _, err := e.DB.Exec(`INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body,hidden_at) VALUES($1,$2,$3,$4,$5,'Fixture',CASE WHEN $6 THEN now() END)`, randomToken(), o, review.product, b, review.rating, review.hidden); err != nil {
			t.Fatal(err)
		}
	}
	for sort, first := range map[string]string{"price-low": "Lower price", "price-high": "Higher price", "rating": "Lower price"} {
		w := e.do("GET", "/?"+url.Values{"sort": {sort}}.Encode(), "", nil)
		e.check(w, 200)
		got := cardTitles(w.Body.String())
		if len(got) != 2 || got[0] != first {
			t.Errorf("%s: %v", sort, got)
		}
		if !strings.Contains(w.Body.String(), "4.0") || !strings.Contains(w.Body.String(), "No reviews") {
			t.Errorf("%s: public review policy not reflected", sort)
		}
	}
}
