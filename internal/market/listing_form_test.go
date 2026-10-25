package market

import (
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// A-59: every listing condition has its own message, on its own field.
func TestListingProblemsOnePerCondition(t *testing.T) {
	for _, c := range []struct {
		set        map[string]string
		field, msg string
	}{
		{map[string]string{"title": "   "}, "title", "Listing title: enter at least 3 characters."},
		{map[string]string{"title": "ab"}, "title", "Listing title: use at least 3 characters."},
		{map[string]string{"title": strings.Repeat("界", 141)}, "title", "Listing title: use at most 140 characters."},
		{map[string]string{"title": "bad\xff title"}, "title", "Listing title: remove the characters that are not valid text."},
		{map[string]string{"description": strings.Repeat("d", 10001)}, "description", "Description: use at most 10000 characters."},
		{map[string]string{"description": "nul\x00"}, "description", "Description: remove the characters that are not valid text."},
		{map[string]string{"category": "Weapons"}, "category", "Category: choose Hardware, Digital or Services."},
		{map[string]string{"category": "Hardware", "subcategory": "Guides / Courses"}, "subcategory", "Subcategory: choose an option within the selected category, or leave it unspecified."},
		{map[string]string{"region": ""}, "region", "Region: choose a region."},
		{map[string]string{"region": "E"}, "region", "Region: use at least 2 characters."},
		{map[string]string{"region": strings.Repeat("r", 81)}, "region", "Region: use at most 80 characters."},
		{map[string]string{"kind": "service"}, "kind", "Fulfillment type: choose physical or digital."},
		{map[string]string{"stock": "x"}, "stock", "Stock: enter a whole number, such as 1."},
		{map[string]string{"stock": "-1"}, "stock", "Stock: use a whole number from 0 to 1000000."},
		{map[string]string{"stock": "1000001"}, "stock", "Stock: use a whole number from 0 to 1000000."},
		{map[string]string{"dispatch_days": "31"}, "dispatch_days", "Dispatch within: use a whole number of days from 1 to 30."},
		{map[string]string{"dispatch_days": "two"}, "dispatch_days", "Dispatch within: use a whole number of days from 1 to 30."},
		{map[string]string{"price_xmr": "abc"}, "price_xmr", "Monero price: use a positive amount with at most 12 decimal places."},
		{map[string]string{"price_xmr": "0.1234567890123"}, "price_xmr", "Monero price: use a positive amount with at most 12 decimal places."},
		{map[string]string{"price_xmr": "0.0009"}, "price_xmr", "Monero price: use at least 0.001 XMR. The network fee is deducted from every payout and would take too large a share of a smaller one."},
		{map[string]string{"delivery_content": "secret-link"}, "delivery_content", "Reusable download content: only a digital listing can have it. Clear it, or choose Digital as the fulfillment type."},
		{map[string]string{"kind": "digital", "delivery_content": strings.Repeat("c", 32001)}, "delivery_content", "Reusable download content: use at most 32000 characters."},
		{map[string]string{"kind": "digital", "delivery_content": "bad\xff"}, "delivery_content", "Reusable download content: remove the characters that are not valid text."},
	} {
		_, err := validateListing(inventoryForm("", c.set))
		var p listingProblems
		if !errors.As(err, &p) || len(p) != 1 || p[0].field != c.field || p[0].msg != c.msg {
			t.Errorf("%v: got %#v, want one problem %s %q", c.set, err, c.field, c.msg)
		}
	}
	f := inventoryForm("", nil)
	f["shipping_regions"] = []string{"Europe", "unlisted destination"}
	if _, err := validateListing(f); err == nil || err.Error() != "Additional shipping regions: choose only listed regions." {
		t.Errorf("unlisted shipping region: %v", err)
	}
	// Every field in error is named at once, in form order.
	_, err := validateListing(inventoryForm("", map[string]string{"price_xmr": "x", "stock": "x", "title": ""}))
	if err == nil || err.Error() != "Listing title: enter at least 3 characters. Stock: enter a whole number, such as 1. Monero price: use a positive amount with at most 12 decimal places." {
		t.Errorf("several problems: %v", err)
	}
}

var listingFormNonce = regexp.MustCompile(`name="form_nonce" value="([^"]+)"`)

// listingFormSection is body from the opening tag of the form posting to action through its end.
func listingFormSection(t *testing.T, body, action string) string {
	t.Helper()
	i := strings.Index(body, `action="`+action+`"`)
	if i < 0 {
		t.Fatalf("no %s form:\n%s", action, body)
	}
	j := strings.Index(body[i:], "</form>")
	return body[i : i+j]
}

func checkFieldError(t *testing.T, form, field, msg string) {
	t.Helper()
	mustContain(t, form, `<p class="field-error" id="lf-`+field+`-error">`+template.HTMLEscapeString(msg)+`</p>`)
	if field != "shipping_regions" && !regexp.MustCompile(`name="`+field+`"[^>]*aria-invalid="true"|aria-invalid="true"[^>]*name="`+field+`"|name="`+field+`"[^>]*lf-`+field+`-error`).MatchString(form) {
		t.Fatalf("%s is not marked invalid:\n%s", field, form)
	}
}

// A-59 (PA-6): a refused new listing is the vendor desk's publish form again, open, with everything typed kept (escaped),
// each problem next to its field, a fresh form nonce and the page's CSRF token, and never the reusable download
// content. Corrected and sent again, it publishes.
func TestRefusedListingRerendersFormWithValues(t *testing.T) {
	p := newPayEnv(t)
	before := p.count("SELECT count(*) FROM products WHERE vendor_id=$1", p.vendor.ID)
	const secret = "delivery-secret-marker-7f3a"
	sent := form("title", `My <b>own</b> listing "quoted"`, "description", "A long description I typed & do not want to lose.\nSecond line.",
		"category", "Services", "subcategory", "", "region", "Europe", "kind", "physical", "stock", "x", "price_xmr", "0.0009",
		"digital_delivery_mode", "reusable", "delivery_content", secret)
	sent["shipping_regions"] = []string{"North America", "Asia"}
	nonce := formNonceFor("/listings")
	sent.Set(formNonceField, nonce)
	w := refusalPost(p.testEnv, "POST", "/listings", p.vendorSess, sent, "http://example.com/vendor-dashboard")
	b := w.Body.String()
	if w.Code != 400 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("refused listing: %d %q\n%s", w.Code, w.Header().Get("Content-Type"), b)
	}
	mustContain(t, b, "<title>Error: ", `<details class="panel" id="publish-listing" open>`,
		`role="alert">The listing was not published; nothing was saved. Stock: enter a whole number, such as 1. Monero price: use at least 0.001 XMR.`)
	f := listingFormSection(t, b, "/listings")
	mustContain(t, f,
		`name="title" required minlength="3" maxlength="140" value="My &lt;b&gt;own&lt;/b&gt; listing &#34;quoted&#34;"`,
		"\nA long description I typed &amp; do not want to lose.\nSecond line.</textarea>",
		`<option selected>Services</option>`, `<option selected>Europe</option>`, `<option value="physical" selected>`,
		`name="stock" required min="0" max="1000000" value="x"`, `value="0.0009"`, `<option value="reusable" selected>`,
		`<details class="additional-shipping" open>`, `value="North America" checked>`, `value="Asia" checked>`,
		`name="csrf" value="`+p.A.csrf(p.vendorSess)+`"`,
		"Reusable download content is never filled in again after a refused form. Enter it again.")
	mustNotContain(t, b, secret, "<b>own")
	checkFieldError(t, f, "stock", "Stock: enter a whole number, such as 1.")
	checkFieldError(t, f, "price_xmr", "Monero price: use at least 0.001 XMR. The network fee is deducted from every payout and would take too large a share of a smaller one.")
	checkFieldError(t, f, "delivery_content", "Reusable download content: only a digital listing can have it. Clear it, or choose Digital as the fulfillment type.")
	mustNotContain(t, f, "lf-title-error", "lf-description-error", `value="Europe" checked`)
	m := listingFormNonce.FindStringSubmatch(f)
	if m == nil || m[1] == nonce {
		t.Fatalf("re-rendered form nonce: %v (sent %s)", m, nonce)
	}
	if n := p.count("SELECT count(*) FROM products WHERE vendor_id=$1", p.vendor.ID); n != before {
		t.Fatalf("a refused listing wrote %d products", n-before)
	}
	// Corrected, with a wrong CSRF token the re-rendered form is still refused; with the page's own it publishes.
	fixed := cloneValues(sent)
	fixed.Set("stock", "4")
	fixed.Set("price_xmr", "0.5")
	fixed.Del("delivery_content")
	fixed.Set(formNonceField, m[1])
	forged := cloneValues(fixed)
	forged.Set("csrf", "wrong-token")
	req := httptest.NewRequest("POST", "/listings", strings.NewReader(forged.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "session", Value: p.vendorSess})
	rw := httptest.NewRecorder()
	p.A.ServeHTTP(rw, req)
	if rw.Code != 403 || p.count("SELECT count(*) FROM products WHERE vendor_id=$1", p.vendor.ID) != before {
		t.Fatalf("re-rendered form without its CSRF token: %d", rw.Code)
	}
	r := refusalPost(p.testEnv, "POST", "/listings", p.vendorSess, fixed, "")
	if r.Code != 303 || !strings.HasPrefix(r.Header().Get("Location"), "/listing-edit?id=") {
		t.Fatalf("corrected listing: %d %s", r.Code, r.Body.String())
	}
	if n := p.count("SELECT count(*) FROM products WHERE vendor_id=$1 AND title=$2 AND stock=4", p.vendor.ID, `My <b>own</b> listing "quoted"`); n != 1 {
		t.Fatalf("corrected listing stored %d times", n)
	}
	// A digital delivery method that is not listed is named on its own field.
	bad := form("title", "Digital thing", "description", "", "category", "Digital", "region", "Digital", "kind", "digital", "stock", "1",
		"price_xmr", "0.5", "digital_delivery_mode", "teleport")
	w = refusalPost(p.testEnv, "POST", "/listings", p.vendorSess, bad, "")
	if w.Code != 400 {
		t.Fatalf("unknown delivery method: %d", w.Code)
	}
	f = listingFormSection(t, w.Body.String(), "/listings")
	checkFieldError(t, f, "digital_delivery_mode", "Digital delivery method: choose one of the listed methods.")
	mustContain(t, f, `value="Digital thing"`, `<option value="digital" selected>`)
}

// A-59: a refused edit is the edit form again with the values sent, the stock it was loaded with (so the
// concurrent-stock check still applies) and the saved reusable download content, never the content sent; the
// listing is unchanged. An administrator's refused edit of another vendor's listing has no delivery content or note;
// another vendor's id is refused without the listing.
func TestRefusedListingEditRerendersFormWithValues(t *testing.T) {
	p := newPayEnv(t)
	agExec(p.testEnv, "UPDATE products SET kind='digital',category='Digital',region='Digital',delivery_content='saved-content-1' WHERE id=$1", p.productID)
	edit := inventoryForm(p.productID, map[string]string{"title": "Edited <title>", "description": "Typed & kept", "category": "Digital",
		"region": "Digital", "kind": "digital", "stock": "9", "stock_seen": "4", "price_xmr": "0.1234567890123", "delivery_content": "new-secret-marker-1b2c"})
	w := refusalPost(p.testEnv, "POST", "/listings/update", p.vendorSess, edit, "")
	b := w.Body.String()
	if w.Code != 400 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("refused edit: %d\n%s", w.Code, b)
	}
	mustContain(t, b, `role="alert">The changes were not saved. Monero price: use a positive amount with at most 12 decimal places.`)
	f := listingFormSection(t, b, "/listings/update")
	mustContain(t, f, `name="id" value="`+p.productID+`"`, `name="stock_seen" value="4"`, `value="Edited &lt;title&gt;"`,
		"\nTyped &amp; kept</textarea>", `name="stock" required min="0" max="1000000" value="9"`, `value="0.1234567890123"`,
		"\nsaved-content-1</textarea>", "The saved content is shown; enter your change again.")
	mustNotContain(t, b, "new-secret-marker-1b2c")
	checkFieldError(t, f, "price_xmr", "Monero price: use a positive amount with at most 12 decimal places.")
	if got := p.str("SELECT title||'|'||stock||'|'||delivery_content FROM products WHERE id=$1", p.productID); got != "Test listing|5|saved-content-1" {
		t.Fatalf("refused edit changed the listing: %s", got)
	}
	// Unchanged delivery content gets no note.
	edit.Set("delivery_content", "saved-content-1")
	b = refusalPost(p.testEnv, "POST", "/listings/update", p.vendorSess, edit, "").Body.String()
	mustNotContain(t, b, "never filled in again")
	// An administrator editing another vendor's listing never sees its delivery content.
	adminEdit := cloneValues(edit)
	adminEdit.Del("delivery_content")
	w = refusalPost(p.testEnv, "POST", "/listings/update", p.adminSess, adminEdit, "")
	b = w.Body.String()
	if w.Code != 400 {
		t.Fatalf("admin refused edit: %d\n%s", w.Code, b)
	}
	mustContain(t, listingFormSection(t, b, "/listings/update"), `value="Edited &lt;title&gt;"`)
	mustNotContain(t, b, "saved-content-1", "never filled in again")
	// Another vendor's id: the shared refusal page, which neither confirms nor shows the listing.
	_, other := p.user("other_vendor_"+randomToken()[:6], "vendor")
	w = refusalPost(p.testEnv, "POST", "/listings/update", other, adminEdit, "")
	b = w.Body.String()
	if w.Code != 400 || strings.Contains(b, `action="/listings/update"`) || strings.Contains(b, "Test listing") || !strings.Contains(b, "Monero price: use a positive amount") {
		t.Fatalf("another vendor's refused edit: %d\n%s", w.Code, b)
	}
}
