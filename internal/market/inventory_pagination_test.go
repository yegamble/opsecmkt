package market

import (
	"strings"
	"testing"
)

func TestVendorDashboardCountsAndPaginatesOwnedListings(t *testing.T) {
	e := newTestApp(t)
	ownerID, owner := e.user("dashboard_owner", "vendor")
	otherID, other := e.user("dashboard_other", "vendor")

	// Equal creation times make the id tie-breaker observable. Half of these 202 listings are archived.
	agExec(e, `INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,archived,created)
		SELECT 'dashboard-listing-'||lpad(g::text,3,'0'),$1,'Dashboard listing '||g,'Fixture','Hardware','Worldwide','physical',100000,500000000000,5,g%2=0,TIMESTAMPTZ '2026-01-01 00:00:00+00'
		FROM generate_series(1,202) g`, ownerID)
	privateListing := e.product(otherID, "physical")
	agExec(e, "UPDATE products SET title='Other vendor private listing',created=TIMESTAMPTZ '2027-01-01 00:00:00+00' WHERE id=$1", privateListing)

	page := e.body("GET", "/vendor-dashboard?currency=XMR", owner, nil, 200)
	assertDashboardCards := func(name, body string, count int) {
		t.Helper()
		if got := strings.Count(body, `class="product-card`); got != count {
			t.Fatalf("%s card count=%d, want %d", name, got, count)
		}
	}
	assertDashboardCards("all page one", page, inventoryPageSize)
	mustContain(t, page, "All (202)", "Active (101)", "Archived (101)", "Showing 1–50 of 202 listings.", `aria-label="Filter your listings"`)
	mustNotContain(t, page, "Other vendor private listing")
	if first, last := strings.Index(page, "dashboard-listing-202"), strings.Index(page, "dashboard-listing-153"); first < 0 || last < 0 || first >= last {
		t.Fatalf("equal-created listings are not ordered by id descending: first=%d last=%d", first, last)
	}
	mustContain(t, page, `href="/vendor-dashboard?currency=XMR&amp;inventory=all&amp;page=2"`)

	active := e.body("GET", "/vendor-dashboard?currency=XMR&inventory=active", owner, nil, 200)
	assertDashboardCards("active page one", active, inventoryPageSize)
	mustContain(t, active, "Active (101)", `aria-current="page"`, `href="/vendor-dashboard?currency=XMR&amp;inventory=active&amp;page=2"`)
	mustNotContain(t, active, "Archived</span>", "Other vendor private listing")
	activeSecond := e.body("GET", "/vendor-dashboard?inventory=active&page=2", owner, nil, 200)
	assertDashboardCards("active page two", activeSecond, inventoryPageSize)
	mustContain(t, activeSecond, "dashboard-listing-101", "dashboard-listing-003", "Showing 51–100 of 101 listings.")
	mustNotContain(t, activeSecond, "dashboard-listing-201", "dashboard-listing-001")
	activeLast := e.body("GET", "/vendor-dashboard?inventory=active&page=999", owner, nil, 200)
	assertDashboardCards("active last page", activeLast, 1)
	mustContain(t, activeLast, "Dashboard listing 1", "Showing 101–101 of 101 listings.")
	mustNotContain(t, activeLast, "Dashboard listing 2", "Dashboard listing 3")

	archivedFirst := e.body("GET", "/vendor-dashboard?inventory=archived", owner, nil, 200)
	assertDashboardCards("archived page one", archivedFirst, inventoryPageSize)
	mustContain(t, archivedFirst, "dashboard-listing-202", "dashboard-listing-104", "Showing 1–50 of 101 listings.")
	archivedSecond := e.body("GET", "/vendor-dashboard?inventory=archived&page=2", owner, nil, 200)
	assertDashboardCards("archived page two", archivedSecond, inventoryPageSize)
	mustContain(t, archivedSecond, "dashboard-listing-102", "dashboard-listing-004", "Showing 51–100 of 101 listings.")
	archived := e.body("GET", "/vendor-dashboard?inventory=archived&page=3", owner, nil, 200)
	assertDashboardCards("archived last page", archived, 1)
	mustContain(t, archived, "dashboard-listing-002", "Showing 101–101 of 101 listings.", "Archived (101)")
	mustNotContain(t, archived, "dashboard-listing-001", "Other vendor private listing")

	// An unknown filter safely falls back to the complete owned inventory; another vendor sees only their own.
	invalid := e.body("GET", "/vendor-dashboard?inventory=unknown", owner, nil, 200)
	mustContain(t, invalid, "All (202)", "Dashboard listing 202")
	mustNotContain(t, invalid, "Other vendor private listing")
	otherPage := e.body("GET", "/vendor-dashboard", other, nil, 200)
	mustContain(t, otherPage, "Other vendor private listing")
	mustNotContain(t, otherPage, "Dashboard listing ")
	emptyArchiveFilter := e.body("GET", "/vendor-dashboard?inventory=archived", other, nil, 200)
	mustContain(t, emptyArchiveFilter, "Archived (0)", "You have no archived listings.")
	mustNotContain(t, emptyArchiveFilter, "Other vendor private listing", "Dashboard listing ")

	// Sanity-check that the generated fixture really crosses the old hard cap and both states were included.
	if want := 202; agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1", ownerID) != want {
		t.Fatalf("fixture should contain %d owned listings", want)
	}
}
