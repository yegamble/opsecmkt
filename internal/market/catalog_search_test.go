package market

import (
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// A-181 (with A-16 catalog paging and A-60 PA-8): a lighter catalog search.

func TestSearchTermCapsAndCleans(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"  drive  ", "drive"},
		{strings.Repeat("a", 64), strings.Repeat("a", 64)},
		{strings.Repeat("a", 65), strings.Repeat("a", 64)},
		{strings.Repeat("a", 63) + "é", strings.Repeat("a", 63)}, // é is 2 bytes: never cut inside a rune
		{strings.Repeat("ж", 40), strings.Repeat("ж", 32)},
		{"a\x00b\xffc", "abc"},
	} {
		got := searchTerm(tc.in)
		if got != tc.want || len(got) > searchMaxBytes || !utf8.ValidString(got) {
			t.Errorf("searchTerm(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := filterValue(strings.Repeat("r", 200) + "\x00\xfe"); len(got) != filterMaxBytes || !utf8.ValidString(got) {
		t.Errorf("filterValue: %d bytes %q", len(got), got)
	}
}

func TestLikePatternEscapesWildcards(t *testing.T) {
	for in, want := range map[string]string{
		"drive": `%drive%`,
		"%":     `%\%%`,
		"_":     `%\_%`,
		`\`:     `%\\%`,
		`a%_\b`: `%a\%\_\\b%`,
	} {
		if got := likePattern(in); got != want {
			t.Errorf("likePattern(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExcerptBoundsCatalogText(t *testing.T) {
	short := Product{Description: "Short description."}
	if short.Excerpt() != short.Description {
		t.Errorf("short excerpt changed: %q", short.Excerpt())
	}
	exact := Product{Description: strings.Repeat("ж", excerptRunes)}
	if exact.Excerpt() != exact.Description {
		t.Error("a description of exactly the limit must not be cut")
	}
	for _, d := range []string{strings.Repeat("x", 10000), strings.Repeat("ж", 10000), strings.Repeat("😀", 400), strings.Repeat("a ", 5000)} {
		e := Product{Description: d}.Excerpt()
		if n := utf8.RuneCountInString(e); n > excerptRunes || n < excerptRunes-10 {
			t.Errorf("excerpt of %d-byte description has %d characters", len(d), n)
		}
		if !utf8.ValidString(e) || !strings.HasSuffix(e, "…") {
			t.Errorf("excerpt not valid UTF-8 or unmarked: %q", e[len(e)-8:])
		}
	}
}

func TestCatalogPagingBoundaries(t *testing.T) {
	d := &PageData{Query: "usb drive", Category: "Hardware", Region: "Europe", Currency: "XMR"}
	for _, tc := range []struct {
		total                int
		raw                  string
		page, pages, offset  int
		prev, next, from, to bool
	}{
		{0, "", 1, 1, 0, false, false, false, false},
		{0, "7", 1, 1, 0, false, false, false, false},
		{25, "2", 1, 1, 0, false, false, true, true},
		{26, "2", 2, 2, 25, true, false, true, true},
		{100, "4", 4, 4, 75, true, false, true, true},
		{100, "5", 4, 4, 75, true, false, true, true},
		{100, "99999999999999999999", 4, 4, 75, true, false, true, true},
		{100, "2", 2, 4, 25, true, true, true, true},
		{100, "0", 1, 4, 0, false, true, true, true},
		{100, "-3", 1, 4, 0, false, true, true, true},
		{100, "abc", 1, 4, 0, false, true, true, true},
		{100, "2.5", 1, 4, 0, false, true, true, true},
		{100, " 3", 1, 4, 0, false, true, true, true},
	} {
		p := catalogPaging(d, tc.total, tc.raw)
		if p.Page != tc.page || p.Pages != tc.pages || p.Offset() != tc.offset || (p.PrevURL != "") != tc.prev || (p.NextURL != "") != tc.next {
			t.Errorf("total=%d page=%q: got %+v", tc.total, tc.raw, p)
		}
		if tc.total > 0 && (p.First != tc.offset+1 || p.Last != min(tc.offset+catalogPageSize, tc.total)) {
			t.Errorf("total=%d page=%q: shows %d-%d", tc.total, tc.raw, p.First, p.Last)
		}
	}
	p := catalogPaging(d, 100, "2")
	for _, link := range []string{p.PrevURL, p.NextURL} {
		u, err := url.Parse(link)
		if err != nil || u.Path != "/" {
			t.Fatalf("paging link %q", link)
		}
		q := u.Query()
		if q.Get("q") != "usb drive" || q.Get("category") != "Hardware" || q.Get("region") != "Europe" || q.Get("currency") != "XMR" {
			t.Errorf("paging link drops filters: %q", link)
		}
	}
	if p.PrevURL != "/?category=Hardware&currency=XMR&q=usb+drive&region=Europe" || !strings.HasSuffix(p.NextURL, "&page=3&q=usb+drive&region=Europe") {
		t.Errorf("paging links: %q %q", p.PrevURL, p.NextURL)
	}
	if bare := catalogPaging(&PageData{Currency: "BTC"}, 60, "2"); bare.PrevURL != "/" || bare.NextURL != "/?page=3" {
		t.Errorf("unfiltered paging links: %q %q", bare.PrevURL, bare.NextURL)
	}
}

func previewTestApp(t *testing.T) *App {
	t.Helper()
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	a := testHTTPApp(true)
	a.templates = tmpl
	return a
}

func TestPreviewCatalogPagingAndCap(t *testing.T) {
	a := previewTestApp(t)
	get := func(path string) string {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("GET %s: %d", path, w.Code)
		}
		return w.Body.String()
	}
	for _, path := range []string{"/", "/?page=9", "/?page=-1", "/?page=x"} {
		body := get(path)
		if n := strings.Count(body, `<article class="product-card`); n != 6 {
			t.Errorf("%s: %d cards, want the six samples", path, n)
		}
		if strings.Contains(body, `class="pagination"`) {
			t.Errorf("%s: one page needs no paging links", path)
		}
	}
	long := strings.Repeat("e", 70)
	body := get("/?q=" + long)
	if !strings.Contains(body, `value="`+strings.Repeat("e", 64)+`"`) || strings.Contains(body, strings.Repeat("e", 65)) {
		t.Error("search box does not echo the 64-byte capped term")
	}
	if !strings.Contains(body, `maxlength="64"`) {
		t.Error("search inputs lack maxlength=64")
	}
	if body := get("/?q=%25"); !strings.Contains(body, "No listings found") {
		t.Error("preview: % is not a wildcard")
	}
}

// --- PostgreSQL-backed ---

// seedCatalog inserts n listings by one vendor, the i-th created i seconds ago (newest first), with the
// given title and description expressions (SQL over i).
func seedCatalog(e *testEnv, vendorID string, n int, title, description string) {
	e.t.Helper()
	if _, err := e.DB.Exec(`INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,created)
		SELECT md5($1||i::text),$1,`+title+`,`+description+`,'Hardware','Europe','physical',100000,500000000000,5,now()-i*interval '1 second'
		FROM generate_series(1,$2::int) i`, vendorID, n); err != nil {
		e.t.Fatal(err)
	}
}

var cardTitle = regexp.MustCompile(`<h2>\s*<a href="/product\?id=[0-9a-f]+">([^<]*)</a>`)

func cardTitles(body string) []string {
	var out []string
	for _, m := range cardTitle.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestCatalogSearchEscapesWildcards(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("wild_vendor", "vendor")
	for _, title := range []string{"Cotton 100% organic", "Plain widget", "under_score kit", `back\slash tool`, "Percent-free item"} {
		if _, err := e.DB.Exec(`INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES($1,$2,$3,'Fixture','Hardware','Worldwide','physical',1,1,1)`, randomToken(), vendor, title); err != nil {
			t.Fatal(err)
		}
	}
	for q, want := range map[string][]string{
		"%":      {"Cotton 100% organic"},
		"_":      {"under_score kit"},
		`\`:      {`back\slash tool`},
		"%%":     nil,
		"0%":     {"Cotton 100% organic"},
		"r_s":    {"under_score kit"}, // literal: "under_score" contains "r_s"
		"widg%":  nil,
		"WIDGET": {"Plain widget"},
	} {
		w := e.do("GET", "/?"+url.Values{"q": {q}}.Encode(), "", nil)
		e.check(w, 200)
		got := cardTitles(w.Body.String())
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("q=%q: got %q want %q", q, got, want)
		}
	}
	// The ESCAPE literal must not depend on the server's string-literal setting (external databases).
	// Set per connection through the DSN (pgx runtime parameter), not for the role: other test packages
	// share the database.
	t.Setenv("DATABASE_URL", os.Getenv("DATABASE_URL")+"&standard_conforming_strings=off")
	e.restart()
	var scs string
	if err := e.DB.QueryRow("SHOW standard_conforming_strings").Scan(&scs); err != nil || scs != "off" {
		t.Fatalf("standard_conforming_strings=%q %v", scs, err)
	}
	w := e.do("GET", "/?q=%25", "", nil)
	e.check(w, 200)
	if got := cardTitles(w.Body.String()); len(got) != 1 || got[0] != "Cotton 100% organic" {
		t.Errorf("standard_conforming_strings=off: q=%% got %q", got)
	}
}

func TestCatalogQueryCappedAt64Bytes(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("cap_vendor", "vendor")
	term := strings.Repeat("k", 64)
	if _, err := e.DB.Exec(`INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES($1,$2,'Capped','prefix `+term+` suffix','Hardware','Worldwide','physical',1,1,1)`, randomToken(), vendor); err != nil {
		t.Fatal(err)
	}
	// The 65th byte onward is ignored: a longer term that the listing does not contain still finds it.
	w := e.do("GET", "/?q="+term+"ZZZZ", "", nil)
	e.check(w, 200)
	body := w.Body.String()
	if got := cardTitles(body); len(got) != 1 {
		t.Fatalf("capped search: %q", got)
	}
	if !strings.Contains(body, `value="`+term+`"`) || strings.Contains(body, "ZZZZ") {
		t.Error("search box does not echo the capped term")
	}
	// Invalid UTF-8 and NUL bytes are dropped, not sent to PostgreSQL (which would refuse them with a 500).
	w = e.do("GET", "/?q=Cap%00%FFped&category=Hard%FFware&region=%00", "", nil)
	e.check(w, 200)
	if got := cardTitles(w.Body.String()); len(got) != 1 {
		t.Errorf("cleaned search: %q", got)
	}
}

func TestCatalogRendersExcerptProductPageFull(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("excerpt_vendor", "vendor")
	desc := strings.Repeat("abcdefghij", 999) + "END-MARKER"
	id := randomToken()
	if _, err := e.DB.Exec(`INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES($1,$2,'Long one',$3,'Hardware','Worldwide','physical',1,1,1)`, id, vendor, desc); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/vendor?id=" + vendor} {
		w := e.do("GET", path, "", nil)
		e.check(w, 200)
		m := regexp.MustCompile(`<p class="product-description">([^<]*)</p>`).FindStringSubmatch(w.Body.String())
		if m == nil {
			t.Fatalf("%s: no description paragraph", path)
		}
		if n := utf8.RuneCountInString(m[1]); n > excerptRunes || !strings.HasPrefix(m[1], desc[:200]) || strings.Contains(w.Body.String(), "END-MARKER") {
			t.Errorf("%s: excerpt has %d characters", path, n)
		}
	}
	w := e.do("GET", "/product?id="+id, "", nil)
	e.check(w, 200)
	if !strings.Contains(w.Body.String(), desc) {
		t.Error("product page no longer shows the full description")
	}
}

func TestCatalogPagesPreserveFiltersAndOrder(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("page_vendor", "vendor")
	seedCatalog(e, vendor, 60, `'Paged item '||lpad(i::text,3,'0')`, `'Paged fixture'`)
	// Same created timestamp for five more: the order must still be total (no row on two pages).
	if _, err := e.DB.Exec(`INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,created)
		SELECT md5('tie'||i::text),$1,'Tied item '||i,'Paged fixture','Hardware','Europe','physical',1,1,1,now()-interval '30.5 seconds' FROM generate_series(1,5) i`, vendor); err != nil {
		t.Fatal(err)
	}
	// Different category: filtered out.
	if _, err := e.DB.Exec(`INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES($1,$2,'Digital paged','Paged fixture','Digital','Europe','digital',1,1,1)`, randomToken(), vendor); err != nil {
		t.Fatal(err)
	}
	base := url.Values{"q": {"paged fixture"}, "category": {"Hardware"}, "region": {"Europe"}, "currency": {"XMR"}}
	get := func(page string) string {
		v := url.Values{}
		for k, x := range base {
			v[k] = x
		}
		if page != "" {
			v.Set("page", page)
		}
		w := e.do("GET", "/?"+v.Encode(), "", nil)
		e.check(w, 200)
		return w.Body.String()
	}
	seen := map[string]bool{}
	for i, want := range []int{25, 25, 15} {
		body := get(fmt.Sprint(i + 1))
		titles := cardTitles(body)
		if len(titles) != want {
			t.Fatalf("page %d: %d cards, want %d", i+1, len(titles), want)
		}
		for _, x := range titles {
			if seen[x] || x == "Digital paged" {
				t.Errorf("page %d: %q repeated or unfiltered", i+1, x)
			}
			seen[x] = true
		}
		if !strings.Contains(body, fmt.Sprintf(`aria-current="page">Page %d of 3</span>`, i+1)) || !strings.Contains(body, ">65</span> listings") {
			t.Errorf("page %d: no current-page marker or total", i+1)
		}
		next := `href="/?category=Hardware&amp;currency=XMR&amp;page=` + fmt.Sprint(i+2) + `&amp;q=paged&#43;fixture&amp;region=Europe"`
		if hasNext := strings.Contains(body, next); hasNext != (i < 2) {
			t.Errorf("page %d: next link present=%v\n%s", i+1, hasNext, body[strings.Index(body, "pagination"):][:600])
		}
		if hasPrev := strings.Contains(body, `rel="prev"`); hasPrev != (i > 0) {
			t.Errorf("page %d: prev link present=%v", i+1, hasPrev)
		}
	}
	if len(seen) != 65 {
		t.Errorf("paging covered %d listings, want 65", len(seen))
	}
	// Newest first: page 1 starts with item 001.
	if first := cardTitles(get(""))[0]; first != "Paged item 001" {
		t.Errorf("first card %q", first)
	}
	// Out of range: past the end shows the last page, nonsense shows the first.
	if body := get("40"); !strings.Contains(body, "Page 3 of 3") || len(cardTitles(body)) != 15 {
		t.Error("page past the end is not the last page")
	}
	for _, p := range []string{"0", "-2", "abc", "1e3", "99999999999999999999999"} {
		body := get(p)
		if !strings.Contains(body, "Page 1 of 3") && !(p == "99999999999999999999999" && strings.Contains(body, "Page 3 of 3")) {
			t.Errorf("page=%q: not handled", p)
		}
	}
	// No matches: the search empty state, no paging.
	w := e.do("GET", "/?q=nothing-matches-this&page=3", "", nil)
	e.check(w, 200)
	if body := w.Body.String(); !strings.Contains(body, "No listings found") || strings.Contains(body, `class="pagination"`) {
		t.Error("empty search state")
	}
}

func TestCatalogEmptyMarketState(t *testing.T) {
	e := newTestApp(t)
	e.user("lonely_buyer", "buyer")
	w := e.do("GET", "/", "", nil)
	e.check(w, 200)
	if body := w.Body.String(); !strings.Contains(body, "No listings yet") || strings.Contains(body, `class="pagination"`) {
		t.Error("an empty market does not say so")
	}
}

// A catalog page stays small even when every listing is the largest a vendor can publish (140-byte title,
// 10 000-byte description): Tor clients fetch it at a few hundred KB/s.
func TestCatalogPageUnder100KBWithMaximumListings(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("big_vendor", "vendor")
	seedCatalog(e, vendor, 100, `rpad('Maximum listing '||i||' ',140,'W')`, `left(repeat('Lorem ipsum dolor sit amet, <b>&"quoted"</b> ',250),10000)`)
	var n, maxDesc, maxTitle int
	if err := e.DB.QueryRow(`SELECT count(*),max(length(description)),max(length(title)) FROM products`).Scan(&n, &maxDesc, &maxTitle); err != nil || n != 100 || maxDesc < 10000 || maxTitle != 140 {
		t.Fatalf("seed: %d listings, description %d, title %d, %v", n, maxDesc, maxTitle, err)
	}
	for _, path := range []string{"/", "/?page=2", "/?page=4", "/?q=lorem", "/?q=lorem&page=3&currency=XMR"} {
		w := e.do("GET", path, "", nil)
		e.check(w, 200)
		size := w.Body.Len()
		if cards := len(cardTitles(w.Body.String())); cards != catalogPageSize {
			t.Errorf("%s: %d cards", path, cards)
		}
		if size >= 100*1000 {
			t.Errorf("%s: %d bytes, want < 100 KB", path, size)
		}
		t.Logf("%s: %d bytes", path, size)
	}
}

func TestProductsCatalogIndexMigrated(t *testing.T) {
	e := newTestApp(t)
	var def string
	if err := e.DB.QueryRow(`SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema() AND tablename='products' AND indexname='products_archived_created'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "(archived, created)") {
		t.Errorf("index definition %q", def)
	}
	var version int
	if err := e.DB.QueryRow(`SELECT version FROM schema_migrations WHERE name='products_catalog_index'`).Scan(&version); err != nil || version != 62 {
		t.Errorf("migration 062 not recorded: %d %v", version, err)
	}
}
