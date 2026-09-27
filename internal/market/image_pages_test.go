package market

import (
	"bytes"
	"context"
	"image/jpeg"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A-189 (war room 2026-09-27, owner decision 1): stored photos and avatars on pages. Pages link images only
// by id on the image routes (image_routes.go); which images a page links is decided by the same predicates
// those routes apply, so a page never shows a broken image.

// titledListing inserts a published listing titled title for vendorID and returns its id.
func (e *testEnv) titledListing(vendorID, title string) string {
	e.t.Helper()
	id := randomToken()
	if _, err := e.DB.Exec(`INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES($1,$2,$3,'Fixture','Hardware','Worldwide','physical',100000,500000000000,5)`, id, vendorID, title); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// setImageDescription sets an image's description.
func (e *testEnv) setImageDescription(im testImage, description string) {
	e.t.Helper()
	if _, err := e.DB.Exec("UPDATE images SET description=$2 WHERE id=$1", im.id, description); err != nil {
		e.t.Fatal(err)
	}
}

// mustExec runs a statement or fails the test.
func (e *testEnv) mustExec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.DB.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

// cardFor returns the listing card whose title is title in a page body ("" when there is none).
func cardFor(body, title string) string {
	for _, c := range strings.Split(body, `<article class="product-card`)[1:] {
		c, _, _ = strings.Cut(c, "</article>")
		if strings.Contains(c, ">"+title+"</a>") {
			return c
		}
	}
	return ""
}

var pageImageURL = regexp.MustCompile(`/(desk/)?img/[0-9a-f]{32}-[tf]`)

// Catalog and vendor-page cards: a listing with a public photo shows its first photo's thumbnail, decorative
// (alt="") at 320×240, with no illustration label; a listing without one keeps the CSS illustration and its
// "ILLUSTRATION" label. A photo the public route would refuse (its vendor suspended) is never linked, and no
// full-size image is.
func TestPhotoCardsRenderPublicThumbnails(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("cards_vendor", "vendor")
	heldID, _ := e.user("cards_held_vendor", "vendor")
	withPhotos := e.titledListing(vendorID, "Card with photos")
	e.titledListing(vendorID, "Card without photos")
	held := e.titledListing(heldID, "Card of a suspended vendor")
	first := e.image(vendorID, withPhotos, "photo")
	second := e.image(vendorID, withPhotos, "photo")
	e.mustExec("UPDATE images SET created=created+interval '1 second' WHERE id=$1", second.id)
	heldPhoto := e.image(heldID, held, "photo")
	e.mustExec("UPDATE users SET suspended_at=now() WHERE id=$1", heldID)

	for _, path := range []string{"/", "/vendor?id=" + vendorID, "/vendor?id=" + heldID} {
		body := e.body("GET", path, randomToken(), nil, 200)
		if strings.Contains(body, second.id) || strings.Contains(body, heldPhoto.id) || strings.Contains(body, "-f\"") {
			t.Fatalf("%s links a second, hidden or full-size photo:\n%s", path, body)
		}
		if c := cardFor(body, "Card with photos"); c != "" {
			want := `<img src="/img/` + first.id + `-t" alt="" width="320" height="240">`
			if !strings.Contains(c, want) || strings.Contains(c, "art-label") || strings.Contains(c, "ILLUSTRATION") || !strings.Contains(c, "has-photo") || strings.Contains(c, `class="device"`) {
				t.Fatalf("%s photo card, want %s and no illustration:\n%s", path, want, c)
			}
			if !strings.Contains(c, `<span class="art-badge">physical</span>`) {
				t.Fatalf("%s photo card lost its kind badge:\n%s", path, c)
			}
		} else if path != "/vendor?id="+heldID {
			t.Fatalf("%s: no card for the listing with photos", path)
		}
		for _, title := range []string{"Card without photos", "Card of a suspended vendor"} {
			if c := cardFor(body, title); c != "" && (!strings.Contains(c, "Hardware / ILLUSTRATION") || strings.Contains(c, "<img") || strings.Contains(c, "has-photo")) {
				t.Fatalf("%s %q: want the illustration:\n%s", path, title, c)
			}
		}
	}
	// Archiving the listing hides its photo: it leaves the catalog, and its card on the owner's desk stays an
	// illustration (the desk shows no public image of an archived listing).
	e.mustExec("UPDATE products SET archived=true WHERE id=$1", withPhotos)
	if body := e.body("GET", "/", randomToken(), nil, 200); strings.Contains(body, first.id) {
		t.Fatal("an archived listing's photo is still linked on the catalog")
	}
}

// The product page: photo 1 at full size, named by its description or "Photo 1 of N: Title"; photos 2..N as
// thumbnails linking to their full size, named with their number, type and size; the vendor-supplied notice
// instead of the illustration notice. Without photos, the illustration and its notice stay.
func TestProductPagePhotos(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("product_photos_vendor", "vendor")
	three := e.titledListing(vendorID, `Lamp "Model 3" & shade`)
	var photos []testImage
	for i := range 3 {
		im := e.image(vendorID, three, "photo")
		e.mustExec("UPDATE images SET created=created+make_interval(secs=>$2) WHERE id=$1", im.id, i)
		photos = append(photos, im)
	}
	e.setImageDescription(photos[1], "The lamp switched on, seen from the left")
	body := e.body("GET", "/product?id="+three, randomToken(), nil, 200)
	for _, want := range []string{
		`<img src="/img/` + photos[0].id + `-f" alt="Photo 1 of 3: Lamp &#34;Model 3&#34; &amp; shade" width="1200" height="900">`,
		`<a href="/img/` + photos[1].id + `-f">`,
		`<img src="/img/` + photos[1].id + `-t" alt="" width="160" height="120">`,
		"Photo 2 of 3, full size (JPEG, 1 KB)",
		"The lamp switched on, seen from the left",
		`<a href="/img/` + photos[2].id + `-f">`,
		"Photo 3 of 3, full size (JPEG, 1 KB)",
		"Photos are supplied by the vendor; the market does not verify them.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("product page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "ILLUSTRATION") || strings.Contains(body, "Product illustrations are decorative") || strings.Contains(body, photos[0].id+"-t") {
		t.Fatalf("product page with photos still shows the illustration, or photo 1 as a thumbnail:\n%s", body)
	}

	// Photo 1's description is its text alternative.
	e.setImageDescription(photos[0], "Brass lamp on a white table")
	body = e.body("GET", "/product?id="+three, randomToken(), nil, 200)
	if !strings.Contains(body, `<img src="/img/`+photos[0].id+`-f" alt="Brass lamp on a white table"`) {
		t.Fatalf("photo 1's description is not its alt text:\n%s", body)
	}

	// A listing without photos keeps the illustration and its notice.
	plain := e.titledListing(vendorID, "Plain listing")
	body = e.body("GET", "/product?id="+plain, randomToken(), nil, 200)
	if !strings.Contains(body, "Hardware / ILLUSTRATION") || !strings.Contains(body, "Product illustrations are decorative") || pageImageURL.MatchString(body) || strings.Contains(body, "Photos are supplied by the vendor") {
		t.Fatalf("listing without photos:\n%s", body)
	}
}

// The vendor page: the vendor's avatar thumbnail, decorative, at 58×58 (else the text glyph), and 25 listings a
// page with Previous/Next links, so a page links at most 25 thumbnails and the avatar.
func TestVendorPageAvatarAndPaging(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("paged_vendor", "vendor")
	avatar := e.image(vendorID, "", "avatar")
	for i := range 30 {
		p := e.titledListing(vendorID, "Paged listing "+strconv.Itoa(i+1))
		e.mustExec("UPDATE products SET created=now()+make_interval(secs=>$2) WHERE id=$1", p, i)
		e.image(vendorID, p, "photo")
	}
	body := e.body("GET", "/vendor?id="+vendorID+"&currency=XMR", randomToken(), nil, 200)
	if want := `<img class="vendor-avatar" src="/img/` + avatar.id + `-t" alt="" width="58" height="58">`; !strings.Contains(body, want) || strings.Contains(body, ">V/<") {
		t.Fatalf("vendor page avatar, want %s:\n%s", want, body)
	}
	if n := strings.Count(body, `<article class="product-card`); n != 25 {
		t.Fatalf("page 1 shows %d cards, want 25", n)
	}
	if n := len(pageImageURL.FindAllString(body, -1)); n != 26 {
		t.Fatalf("page 1 links %d images, want 25 thumbnails and the avatar", n)
	}
	next := `href="/vendor?currency=XMR&amp;id=` + vendorID + `&amp;page=2"`
	if !strings.Contains(body, "Page 1 of 2") || !strings.Contains(body, next) || !strings.Contains(body, cardFor(body, "Paged listing 30")) || cardFor(body, "Paged listing 5") != "" {
		t.Fatalf("page 1: want the newest 25 and a link %s:\n%s", next, body)
	}
	body = e.body("GET", "/vendor?id="+vendorID+"&currency=XMR&page=2", randomToken(), nil, 200)
	if n := strings.Count(body, `<article class="product-card`); n != 5 || !strings.Contains(body, "Page 2 of 2") || cardFor(body, "Paged listing 1") == "" {
		t.Fatalf("page 2 shows %d cards, want the oldest 5:\n%s", n, body)
	}
	if !strings.Contains(body, `href="/vendor?currency=XMR&amp;id=`+vendorID+`"`) {
		t.Fatalf("page 2 has no link back to page 1:\n%s", body)
	}

	// No avatar: the glyph. An administrator's avatar is a staff avatar: not public while the staff list is off.
	plainID, _ := e.user("plain_vendor", "vendor")
	e.titledListing(plainID, "Plain vendor listing")
	adminID, _ := e.user("selling_admin", "admin")
	e.titledListing(adminID, "Admin listing")
	adminAvatar := e.image(adminID, "", "avatar")
	for _, id := range []string{plainID, adminID} {
		body = e.body("GET", "/vendor?id="+id, randomToken(), nil, 200)
		if !strings.Contains(body, `<div class="vendor-avatar" aria-hidden="true">V/</div>`) || strings.Contains(body, adminAvatar.id) || strings.Contains(body, "Page 1 of 1") {
			t.Fatalf("vendor %s without a public avatar:\n%s", id, body)
		}
	}
}

// Owner decision 1(a): the public staff list is off by default: /canary shows no staff list even for a
// moderator who is not hidden and has an avatar, and the admin page offers to turn it on.
func TestStaffListOffByDefault(t *testing.T) {
	e := newTestApp(t)
	modID, _ := e.user("default_list_moderator", "moderator")
	_, admin := e.user("default_list_admin", "admin")
	avatar := e.image(modID, "", "avatar")
	e.mustExec("UPDATE users SET staff_hidden=false WHERE id=$1", modID)
	var setting string
	if err := e.DB.QueryRow("SELECT value FROM settings WHERE key='staff_list_public'").Scan(&setting); err != nil || setting != "false" {
		t.Fatalf("staff_list_public=%q %v", setting, err)
	}
	body := e.body("GET", "/canary", randomToken(), nil, 200)
	if strings.Contains(body, `id="staff"`) || strings.Contains(body, "Market staff") || strings.Contains(body, "default_list_moderator") || strings.Contains(body, avatar.id) {
		t.Fatalf("/canary shows a staff list while the setting is off:\n%s", body)
	}
	body = e.body("GET", "/admin", admin, nil, 200)
	if !strings.Contains(body, `action="/admin/staff-list"`) || !strings.Contains(body, "Show a public staff list: <strong>Off</strong>") || !strings.Contains(body, `name="staff_list_public" value="true"`) {
		t.Fatalf("admin page staff list control:\n%s", body)
	}
}

// Owner decision 1(a),(b),(d), the page side: while the list is on, /canary lists each moderator and
// administrator who has not hidden themselves and is not suspended, with their avatar on the public route;
// hidden, suspended and non-staff accounts are not listed at all. Any value but "true" is off.
func TestHiddenStaffAvatarNotPublicOnCanary(t *testing.T) {
	e := newTestApp(t)
	shownID, _ := e.user("listed_moderator", "moderator")
	hiddenID, _ := e.user("hidden_moderator", "moderator")
	heldID, _ := e.user("held_moderator", "moderator")
	adminID, _ := e.user("listed_admin", "admin")
	vendorID, _ := e.user("not_staff_vendor", "vendor")
	shown, hidden, held, vendor := e.image(shownID, "", "avatar"), e.image(hiddenID, "", "avatar"), e.image(heldID, "", "avatar"), e.image(vendorID, "", "avatar")
	e.mustExec("UPDATE users SET staff_hidden=false WHERE id IN ($1,$2,$3,$4)", shownID, heldID, adminID, vendorID)
	e.mustExec("UPDATE users SET suspended_at=now() WHERE id=$1", heldID)
	e.mustExec("UPDATE settings SET value='true' WHERE key='staff_list_public'")

	body := e.body("GET", "/canary", randomToken(), nil, 200)
	_, list, ok := strings.Cut(body, `id="staff"`)
	list, _, _ = strings.Cut(list, "</section>")
	if !ok {
		t.Fatalf("no staff list while the setting is on:\n%s", body)
	}
	for _, want := range []string{`<img src="/img/` + shown.id + `-t" alt="" width="64" height="64">`, "listed_moderator", "listed_admin", "Moderator", "Administrator"} {
		if !strings.Contains(list, want) {
			t.Fatalf("staff list lacks %q:\n%s", want, list)
		}
	}
	for _, absent := range []string{"hidden_moderator", hidden.id, "held_moderator", held.id, "not_staff_vendor", vendor.id} {
		if strings.Contains(body, absent) {
			t.Fatalf("/canary shows %q:\n%s", absent, body)
		}
	}
	if strings.Index(list, "listed_admin") > strings.Index(list, "listed_moderator") {
		t.Fatalf("administrators are not listed first:\n%s", list)
	}
	// Every avatar the list links is served on the public route.
	for _, u := range pageImageURL.FindAllString(list, -1) {
		if w := e.imgGet("GET", u); w.Code != 200 {
			t.Fatalf("%s: %d", u, w.Code)
		}
	}
	// Nobody listed: the section says so.
	e.mustExec("UPDATE users SET staff_hidden=true WHERE id IN ($1,$2)", shownID, adminID)
	if body = e.body("GET", "/canary", randomToken(), nil, 200); !strings.Contains(body, "No moderator or administrator has chosen to be listed.") || strings.Contains(body, "listed_moderator") {
		t.Fatalf("empty staff list:\n%s", body)
	}
	// Anything but "true" is off.
	e.mustExec("UPDATE users SET staff_hidden=false WHERE id=$1", shownID)
	e.mustExec("UPDATE settings SET value='yes' WHERE key='staff_list_public'")
	if body = e.body("GET", "/canary", randomToken(), nil, 200); strings.Contains(body, `id="staff"`) || strings.Contains(body, shown.id) {
		t.Fatalf("staff list shown for setting 'yes':\n%s", body)
	}
}

// Owner decision 1(c), the page side: the order page shows the deciding staff member's avatar (the desk route's
// 64 px thumbnail, alt="") beside the dispute decision to the order's buyer and vendor and to staff, even while
// that member is hidden from the public list; every other history entry, and every other page, shows none.
func TestStaffAvatarVisibleToOrderPartiesOnOrderPage(t *testing.T) {
	e := newTestApp(t)
	modID, mod := e.user("decision_moderator", "moderator")
	_, otherMod := e.user("decision_other_moderator", "moderator")
	vendorID, vendor := e.user("decision_vendor", "vendor")
	buyerID, buyer := e.user("decision_buyer", "buyer")
	_, admin := e.user("decision_admin", "admin")
	listing := e.product(vendorID, "physical")
	avatar := e.image(modID, "", "avatar")
	vendorAvatar := e.image(vendorID, "", "avatar")
	order := e.order(buyerID, listing, "BTC", "resolved")
	for _, ev := range [][4]string{{"paid", "shipped", vendorID, ""}, {"shipped", "disputed", buyerID, ""}, {"disputed", "resolved", modID, "Resolved — refund to buyer"}} {
		e.mustExec("INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$3,$4,$5)", order, ev[0], ev[1], ev[2], ev[3])
	}
	img := `<img class="decision-avatar" src="/desk/img/` + avatar.id + `-t" alt="" width="64" height="64">`
	for name, s := range map[string]string{"buyer": buyer, "vendor": vendor, "moderator": mod, "another moderator": otherMod, "administrator": admin} {
		body := e.body("GET", "/order?id="+order, s, nil, 200)
		if strings.Count(body, img) != 1 {
			t.Fatalf("%s: want the decider's avatar once, %s:\n%s", name, img, body)
		}
		_, after, _ := strings.Cut(body, img)
		if before, _, _ := strings.Cut(after, "</li>"); !strings.Contains(before, "decision_moderator (moderator)") {
			t.Fatalf("%s: the avatar is not beside the decision:\n%s", name, body)
		}
		if strings.Contains(body, vendorAvatar.id) || strings.Contains(body, `"/img/`+avatar.id) {
			t.Fatalf("%s: the order page shows a party's avatar or a public avatar URL:\n%s", name, body)
		}
		// The link works for this viewer.
		wantImage(t, e.desk(s, avatar.id, false), avatar.thumb, name+" desk avatar")
	}
	// Not on the other pages that list orders or messages.
	for _, path := range []string{"/orders", "/messages", "/disputes"} {
		if body := e.body("GET", path, buyer, nil, 200); strings.Contains(body, avatar.id) || strings.Contains(body, vendorAvatar.id) {
			t.Fatalf("%s shows an avatar:\n%s", path, body)
		}
	}
	// A decider who is no longer staff, or is suspended: no avatar (the desk route would refuse it).
	e.mustExec("UPDATE users SET role='buyer' WHERE id=$1", modID)
	if body := e.body("GET", "/order?id="+order, buyer, nil, 200); strings.Contains(body, avatar.id) {
		t.Fatalf("demoted decider's avatar still shown:\n%s", body)
	}
	// Administrators are served any image on the desk route, so they still see it.
	if body := e.body("GET", "/order?id="+order, admin, nil, 200); !strings.Contains(body, img) {
		t.Fatalf("administrator no longer sees the demoted decider's avatar:\n%s", body)
	}
	e.mustExec("UPDATE users SET role='moderator', suspended_at=now() WHERE id=$1", modID)
	if body := e.body("GET", "/order?id="+order, vendor, nil, 200); strings.Contains(body, avatar.id) {
		t.Fatalf("suspended decider's avatar still shown:\n%s", body)
	}
}

// Owner decision 1(a): only an administrator changes the setting, with their password; each change is audited
// old→new, and a form for the state already in force changes nothing.
func TestStaffListToggleAudited(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("toggle_admin", "admin")
	_, mod := e.user("toggle_moderator", "moderator")
	setting := func() string {
		t.Helper()
		var v string
		if err := e.DB.QueryRow("SELECT value FROM settings WHERE key='staff_list_public'").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	on := url.Values{"staff_list_public": {"true"}, "password": {testPassword}}
	e.check(e.do("POST", "/admin/staff-list", mod, url.Values{"staff_list_public": {"true"}, "password": {testPassword}}), 403)
	e.check(e.do("POST", "/admin/staff-list", admin, url.Values{"staff_list_public": {"true"}}), 400)
	e.check(e.do("POST", "/admin/staff-list", admin, url.Values{"staff_list_public": {"true"}, "password": {"not-the-password-at-all"}}), 401)
	e.check(e.do("POST", "/admin/staff-list", admin, url.Values{"staff_list_public": {"maybe"}, "password": {testPassword}}), 400)
	e.check(e.do("POST", "/admin/staff-list", admin, url.Values{"staff_list_public": {"false"}, "password": {testPassword}}), 409)
	if v := setting(); v != "false" {
		t.Fatalf("setting %q after refused changes", v)
	}
	w := e.do("POST", "/admin/staff-list", admin, on)
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/admin?saved=1#staff-list" {
		t.Fatalf("redirect %q", loc)
	}
	if v := setting(); v != "true" || !e.auditExact(adminID, "Public staff list changed from off to on (confirmed with password)") {
		t.Fatalf("setting %q, or no audit row", v)
	}
	if body := e.body("GET", "/admin", admin, nil, 200); !strings.Contains(body, "Show a public staff list: <strong>On</strong>") || !strings.Contains(body, `name="staff_list_public" value="false"`) {
		t.Fatalf("admin page after turning it on:\n%s", body)
	}
	if body := e.body("GET", "/canary", randomToken(), nil, 200); !strings.Contains(body, `id="staff"`) {
		t.Fatalf("no staff list once on:\n%s", body)
	}
	e.check(e.do("POST", "/admin/staff-list", admin, on), 409)
	e.check(e.do("POST", "/admin/staff-list", admin, url.Values{"staff_list_public": {"false"}, "password": {testPassword}}), 303)
	if v := setting(); v != "false" || !e.auditExact(adminID, "Public staff list changed from on to off (confirmed with password)") {
		t.Fatalf("setting %q, or no audit row for off", v)
	}
	var n int
	if err := e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE action LIKE 'Public staff list%'").Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d audit rows, want 2 (%v)", n, err)
	}
	// A missing row counts as off, and turning it on writes it.
	e.mustExec("DELETE FROM settings WHERE key='staff_list_public'")
	e.check(e.do("POST", "/admin/staff-list", admin, on), 303)
	if v := setting(); v != "true" {
		t.Fatalf("setting %q after re-creating it", v)
	}
}

// Preview mode has no database: the public image route serves the preview's generated sample photos through
// the same writer and header set, and the catalog and product pages link them.
func TestPreviewServesSamplePhotos(t *testing.T) {
	a, err := New(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	catalog := get("/").Body.String()
	thumbs := regexp.MustCompile(`<img src="(/img/[0-9a-f]{32}-t)" alt="" width="320" height="240">`).FindAllStringSubmatch(catalog, -1)
	if len(thumbs) != 2 {
		t.Fatalf("preview catalog links %d photo thumbnails, want 2:\n%s", len(thumbs), catalog)
	}
	product := get("/product?id=encrypted-drive").Body.String()
	full := regexp.MustCompile(`<img src="(/img/[0-9a-f]{32}-f)" alt="Photo 1 of 2: Encrypted USB Drive — 256GB"`).FindStringSubmatch(product)
	if full == nil || !strings.Contains(product, "Photo 2 of 2, full size (JPEG, ") {
		t.Fatalf("preview product page:\n%s", product)
	}
	ids := map[string]bool{}
	for _, u := range append(pageImageURL.FindAllString(catalog, -1), pageImageURL.FindAllString(product, -1)...) {
		ids[u[5:37]] = true
		w := get(u)
		checkImageHeaders(t, w.Header(), w.Body.Len(), u)
		img, err := jpeg.Decode(bytes.NewReader(w.Body.Bytes()))
		if w.Code != 200 || err != nil {
			t.Fatalf("%s: %d %v", u, w.Code, err)
		}
		if strings.HasSuffix(u, "-t") && (img.Bounds().Dx() != thumbWidth || img.Bounds().Dy() != thumbHeight || w.Body.Len() > 32<<10) {
			t.Fatalf("%s: thumbnail %v, %d bytes", u, img.Bounds(), w.Body.Len())
		}
	}
	if len(ids) != 3 {
		t.Fatalf("preview links %d sample photos, want 3", len(ids))
	}
	// An unknown id, and any id in preview's desk route, get the unknown-id answer.
	unknown := get(publicImageURL(randomToken()[:32], false))
	ref := httptest.NewRecorder()
	(&App{}).servePublicImage(ref, httptest.NewRequest("GET", "/img/x", nil), "x")
	if unknown.Code != 404 || unknown.Body.String() != ref.Body.String() {
		t.Fatalf("unknown id in preview: %d %q", unknown.Code, unknown.Body.String())
	}
	for id := range ids {
		if w := get(deskImageURL(id, false)); w.Code != 404 {
			t.Fatalf("preview desk route served %s: %d", id, w.Code)
		}
	}
}
