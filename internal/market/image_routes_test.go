package market

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A-187 (war room 2026-09-27, rulings D7 and D8, owner decision 1): serving stored photos and avatars.

// testImage is a stored image row inserted directly (the pipeline itself is A-186's, uploads_*_test.go).
type testImage struct {
	id          string
	full, thumb []byte
}

// image inserts an image of kind ("photo" with product, or "avatar" with product "") owned by owner.
func (e *testEnv) image(owner, product, kind string) testImage {
	e.t.Helper()
	im := testImage{id: randomToken()[:32]}
	im.full = []byte("\xff\xd8\xff full image " + im.id + strings.Repeat("F", 300))
	im.thumb = []byte("\xff\xd8\xff thumbnail " + im.id)
	var p any
	if product != "" {
		p = product
	}
	if _, err := e.DB.Exec(`INSERT INTO images(id,owner_user_id,product_id,kind,"full",thumb,width,height) VALUES($1,$2,$3,$4,$5,$6,1200,900)`, im.id, owner, p, kind, im.full, im.thumb); err != nil {
		e.t.Fatal(err)
	}
	return im
}

// imgGet requests path with only the given cookies (none: a logged-out browser's first request).
func (e *testEnv) imgGet(method, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	return w
}

// desk requests the authenticated route as session.
func (e *testEnv) desk(session, id string, full bool) *httptest.ResponseRecorder {
	return e.imgGet("GET", deskImageURL(id, full), &http.Cookie{Name: "session", Value: session})
}

func sessionCookie(s string) *http.Cookie { return &http.Cookie{Name: "session", Value: s} }

// wantImage asserts a 200 carrying exactly want.
func wantImage(t *testing.T, w *httptest.ResponseRecorder, want []byte, what string) {
	t.Helper()
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), want) {
		t.Fatalf("%s: status=%d body=%.60q, want 200 with the image", what, w.Code, w.Body.String())
	}
}

// notFound is the reference 404: an unknown, well-formed id on the public route.
func (e *testEnv) notFound() *httptest.ResponseRecorder {
	return e.imgGet("GET", publicImageURL(randomToken()[:32], false))
}

// sameResponse fails unless w and ref have the same status, body and headers (Date aside).
func sameResponse(t *testing.T, w, ref *httptest.ResponseRecorder, what string) {
	t.Helper()
	strip := func(h http.Header) http.Header { h = h.Clone(); h.Del("Date"); return h }
	a, b := strip(w.Header()), strip(ref.Header())
	if w.Code != ref.Code || !bytes.Equal(w.Body.Bytes(), ref.Body.Bytes()) || len(a) != len(b) {
		t.Fatalf("%s: %d %q %v, want the unknown-id answer %d %q %v", what, w.Code, w.Body.String(), a, ref.Code, ref.Body.String(), b)
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			t.Fatalf("%s: header %s=%q, the unknown-id answer has %q", what, k, v, b[k])
		}
	}
}

// wantHidden asserts the unknown-id 404 for both sizes of im on the public route.
func (e *testEnv) wantHidden(im testImage, what string) {
	e.t.Helper()
	ref := e.notFound()
	if ref.Code != 404 {
		e.t.Fatalf("unknown id: %d", ref.Code)
	}
	for _, full := range []bool{false, true} {
		sameResponse(e.t, e.imgGet("GET", publicImageURL(im.id, full)), ref, what+" "+strconv.FormatBool(full))
	}
}

// wantPublic asserts both sizes of im on the public route.
func (e *testEnv) wantPublic(im testImage, what string) {
	e.t.Helper()
	wantImage(e.t, e.imgGet("GET", publicImageURL(im.id, false)), im.thumb, what+" thumbnail")
	wantImage(e.t, e.imgGet("GET", publicImageURL(im.id, true)), im.full, what+" full")
}

// wantDesk asserts both sizes of im on the desk route as session.
func (e *testEnv) wantDesk(session string, im testImage, what string) {
	e.t.Helper()
	wantImage(e.t, e.desk(session, im.id, false), im.thumb, what+" thumbnail")
	wantImage(e.t, e.desk(session, im.id, true), im.full, what+" full")
}

// wantDeskHidden asserts the unknown-id 404 for im on the desk route as session.
func (e *testEnv) wantDeskHidden(session string, im testImage, what string) {
	e.t.Helper()
	ref := e.desk(session, randomToken()[:32], false)
	if ref.Code != 404 {
		e.t.Fatalf("unknown id on the desk route: %d", ref.Code)
	}
	sameResponse(e.t, ref, e.notFound(), "the desk route's unknown id")
	for _, full := range []bool{false, true} {
		sameResponse(e.t, e.desk(session, im.id, full), ref, what+" "+strconv.FormatBool(full))
	}
}

// D8: the public route reads no session, however many images a signed-in page shows: with the sessions table
// locked (any session lookup would wait), a signed-in browser's image request is answered at once and sets no
// cookie, while a page with the same cookie waits for the lock (the control).
func TestImageGetNoSessionLookup(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("nolookup_vendor", "vendor")
	photo := e.image(vendorID, e.product(vendorID, "physical"), "photo")
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("LOCK TABLE sessions IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	cookies := []*http.Cookie{sessionCookie(vendor), {Name: "anon", Value: randomToken()}}
	start := time.Now()
	for range 25 {
		w := e.imgGet("GET", publicImageURL(photo.id, false), cookies...)
		wantImage(t, w, photo.thumb, "signed-in thumbnail")
		if c := w.Header().Values("Set-Cookie"); len(c) != 0 {
			t.Fatalf("the image route set a cookie: %q", c)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("25 thumbnails took %s with the sessions table locked", d)
	}
	if w := e.imgGet("GET", publicImageURL(photo.id, false)); len(w.Header().Values("Set-Cookie")) != 0 {
		t.Fatal("the image route issued an anonymous cookie to a browser without one")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	for _, path := range []string{"/catalog", deskImageURL(photo.id, false)} {
		r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
		r.AddCookie(sessionCookie(vendor))
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		if w.Code != 503 {
			t.Fatalf("control: GET %s with the sessions table locked answered %d; the lock does not catch a lookup", path, w.Code)
		}
	}
}

// D8 and the admission ruling: images have their own class. With every page slot taken images are still
// served; with every image slot taken an image gets 503 with Retry-After and pages still load.
func TestImageFloodDoesNotRefusePages(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("flood_vendor", "vendor")
	photo := e.image(vendorID, e.product(vendorID, "physical"), "photo")
	if cap(imageWork) != 16 {
		t.Fatalf("imageWork has %d slots, want 16", cap(imageWork))
	}
	release := holdSlots(t, pageWork)
	wantImage(t, e.imgGet("GET", publicImageURL(photo.id, true)), photo.full, "image with the page class full")
	if w := e.imgGet("GET", "/catalog"); w.Code != 503 {
		t.Fatalf("page with the page class full: %d", w.Code)
	}
	release()
	release = holdSlots(t, imageWork)
	for _, method := range []string{"GET", "HEAD"} {
		w := e.imgGet(method, publicImageURL(photo.id, false))
		if w.Code != 503 || w.Header().Get("Retry-After") != "5" {
			t.Fatalf("%s image with the image class full: %d Retry-After=%q", method, w.Code, w.Header().Get("Retry-After"))
		}
	}
	if w := e.imgGet("GET", "/catalog"); w.Code != 200 {
		t.Fatalf("page with the image class full: %d", w.Code)
	}
	if w := e.imgGet("GET", "/product?id="+photo.id); w.Code != 404 { // a page, not an image: no image slot
		t.Fatalf("unknown product with the image class full: %d", w.Code)
	}
	release()
	wantImage(t, e.imgGet("GET", publicImageURL(photo.id, false)), photo.thumb, "image after the slots were released")
	if len(imageWork) != 0 || len(pageWork) != 0 {
		t.Fatalf("slots leaked: images=%d pages=%d", len(imageWork), len(pageWork))
	}
}

// D8 and D4: an archived, taken-down, demoted or suspended vendor's images are hidden from the public route
// (the unknown-id 404), while the owner, or for a suspended owner an administrator, still gets them on the desk.
func TestImageHiddenAfterArchiveTakedownDemotionSuspension(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("hidden_admin", "admin")
	vendorID, vendor := e.user("hidden_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	photo := e.image(vendorID, listing, "photo")
	avatar := e.image(vendorID, "", "avatar")
	e.wantPublic(photo, "published listing's photo")
	e.wantPublic(avatar, "vendor's avatar")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}

	e.check(e.do("POST", "/listings/archive", vendor, map[string][]string{"id": {listing}}), 303)
	e.wantHidden(photo, "archived listing's photo")
	e.wantPublic(avatar, "avatar of a vendor with an archived listing")
	e.wantDesk(vendor, photo, "owner, archived listing")
	e.check(e.do("POST", "/listings/restore", vendor, map[string][]string{"id": {listing}}), 303)
	e.wantPublic(photo, "restored listing's photo")

	exec("UPDATE products SET archived=true,archived_at=now(),admin_archive_reason='Counterfeit goods reported' WHERE id=$1", listing)
	e.wantHidden(photo, "taken-down listing's photo")
	e.wantDesk(vendor, photo, "owner, taken-down listing")
	exec("UPDATE products SET archived=false,archived_at=NULL,admin_archive_reason=NULL WHERE id=$1", listing)
	e.wantPublic(photo, "listing restored by an administrator")

	// Demotion archives the listings (actions_admin.go); the role alone hides both images too.
	exec("UPDATE users SET role='buyer' WHERE id=$1", vendorID)
	e.wantHidden(photo, "demoted vendor's photo")
	e.wantHidden(avatar, "demoted vendor's avatar")
	e.wantDesk(vendor, avatar, "demoted owner")
	exec("UPDATE users SET role='vendor' WHERE id=$1", vendorID)
	e.wantPublic(photo, "vendor again")

	exec("UPDATE users SET suspended_at=now() WHERE id=$1", vendorID)
	exec("DELETE FROM sessions WHERE user_id=$1", vendorID)
	e.wantHidden(photo, "suspended vendor's photo")
	e.wantHidden(avatar, "suspended vendor's avatar")
	e.wantDesk(admin, photo, "administrator, suspended owner")
	e.wantDesk(admin, avatar, "administrator, suspended owner's avatar")
	e.wantDeskHidden(vendor, photo, "suspended owner's ended session")

	// Takedown by hard delete (A-190) leaves the unknown-id answer on both routes.
	exec("UPDATE users SET suspended_at=NULL WHERE id=$1", vendorID)
	e.wantPublic(photo, "restored account")
	exec("DELETE FROM images WHERE id=$1", photo.id)
	e.wantHidden(photo, "deleted photo")
	e.wantDeskHidden(admin, photo, "deleted photo, administrator")
}

// D8: a hidden image and an unknown or malformed id get byte-identical answers (status, headers, body) on both
// routes, with or without a session, so an id cannot be probed.
func TestImageUnknownAndHiddenIdentical(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("probe_vendor", "vendor")
	_, other := e.user("probe_other", "vendor")
	_, buyer := e.user("probe_buyer", "buyer")
	listing := e.product(vendorID, "physical")
	photo := e.image(vendorID, listing, "photo")
	if _, err := e.DB.Exec("UPDATE products SET archived=true WHERE id=$1", listing); err != nil {
		t.Fatal(err)
	}
	ref := e.notFound()
	if ref.Code != 404 || ref.Header().Get("Cache-Control") != "no-store" || len(ref.Header().Values("Set-Cookie")) != 0 {
		t.Fatalf("unknown id: %d %v", ref.Code, ref.Header())
	}
	unknown := randomToken()[:32]
	for _, path := range []string{
		publicImageURL(photo.id, false), publicImageURL(photo.id, true),
		"/img/" + unknown + "-t", "/img/" + unknown + "-f",
		"/img/" + strings.ToUpper(photo.id) + "-t", "/img/" + photo.id[:31] + "-t", "/img/" + photo.id + "0-t",
		"/img/" + photo.id + "-x", "/img/" + photo.id, "/img/" + photo.id + "-t/", "/img/" + photo.id + "-t.jpg",
		"/img/", "/img/../static/style.css", "/img/" + photo.id + "-t%00",
	} {
		sameResponse(t, e.imgGet("GET", path), ref, "public "+path)
		sameResponse(t, e.imgGet("GET", path, sessionCookie(other), &http.Cookie{Name: "anon", Value: randomToken()}), ref, "public, signed in, "+path)
	}
	for _, s := range []string{"", randomToken(), other, buyer} {
		for _, path := range []string{deskImageURL(photo.id, false), deskImageURL(photo.id, true), deskImageURL(unknown, false), "/desk/img/" + photo.id, "/desk/img/"} {
			sameResponse(t, e.imgGet("GET", path, sessionCookie(s)), ref, "desk "+path)
		}
	}
	sameResponse(t, e.imgGet("GET", deskImageURL(photo.id, false)), ref, "desk without any cookie")
	e.wantDesk(vendor, photo, "owner")
	sameResponse(t, e.imgGet("HEAD", publicImageURL(photo.id, false)), e.imgGet("HEAD", "/img/"+unknown+"-t"), "HEAD hidden")
	sameResponse(t, e.imgGet("HEAD", deskImageURL(photo.id, false), sessionCookie(other)), e.imgGet("HEAD", "/img/"+unknown+"-t"), "HEAD desk hidden")
}

// D8: ids are 128 random bits as 32 lowercase hex, distinct for the same picture, and the routes accept that
// form only. Stored through the real upload pipeline, then served.
func TestImageIDEntropy(t *testing.T) {
	e, _ := uploadEnv(t)
	idForm := regexp.MustCompile(`^[0-9a-f]{32}$`)
	var ids []string
	for i := range 4 {
		_, s := e.user("entropy_vendor_"+strconv.Itoa(i), "vendor")
		w := e.upload("/account/avatar", s, e.avatarParts(s, smallJPEG(t))...)
		e.check(w, 303)
	}
	rows, err := e.DB.Query(`SELECT id,thumb FROM images ORDER BY created`)
	if err != nil {
		t.Fatal(err)
	}
	var thumbs [][]byte
	for rows.Next() {
		var id string
		var thumb []byte
		if err = rows.Scan(&id, &thumb); err != nil {
			t.Fatal(err)
		}
		ids, thumbs = append(ids, id), append(thumbs, thumb)
	}
	rows.Close()
	if len(ids) != 4 {
		t.Fatalf("%d images stored", len(ids))
	}
	for i, id := range ids {
		if !idForm.MatchString(id) || slices.Contains(ids[:i], id) {
			t.Fatalf("image id %q: not 32 lowercase hex, or repeated in %q", id, ids)
		}
		wantImage(t, e.imgGet("GET", publicImageURL(id, false)), thumbs[i], "uploaded avatar")
	}
	// A 128-bit id: every hex digit position takes many values across ids (no fixed prefix, counter or time).
	seen := map[string]bool{}
	for range 64 {
		id := randomToken()[:32] // storeImage's generator
		if !idForm.MatchString(id) || seen[id] {
			t.Fatalf("generated id %q", id)
		}
		seen[id] = true
	}
	for pos := range 32 {
		digits := map[byte]bool{}
		for id := range seen {
			digits[id[pos]] = true
		}
		if len(digits) < 4 {
			t.Fatalf("position %d takes only %d values across 64 ids", pos, len(digits))
		}
	}
	if _, ok := parseImagePath(ids[0] + "-t"); !ok {
		t.Fatal("a stored id does not parse")
	}
	for _, bad := range []string{strings.ToUpper(ids[0]) + "-t", ids[0][:31] + "-t", ids[0] + "a-t", ids[0] + "-T", ids[0] + "-", ids[0], ""} {
		if _, ok := parseImagePath(bad); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
}

// Owner decision and D8: the owner sees their own photos on the desk route whatever the listing's state;
// another vendor gets the unknown-id answer and an administrator sees them.
func TestOwnerSeesArchivedOnDeskRoute(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("desk_admin", "admin")
	vendorID, vendor := e.user("desk_vendor", "vendor")
	_, other := e.user("desk_other_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	photo := e.image(vendorID, listing, "photo")
	avatar := e.image(vendorID, "", "avatar")
	e.check(e.do("POST", "/listings/archive", vendor, map[string][]string{"id": {listing}}), 303)
	e.wantHidden(photo, "archived, public route")
	e.wantDesk(vendor, photo, "owner, archived")
	e.wantDesk(vendor, avatar, "owner's avatar")
	e.wantDesk(admin, photo, "administrator, archived")
	e.wantDeskHidden(other, photo, "another vendor, archived")
	e.wantDeskHidden(other, avatar, "another vendor's avatar on the desk route")
}

// D8: the desk route is for the owner and administrators; a moderator gets the unknown-id answer for a vendor's
// photo or avatar (moderators see vendor images on the public route, A-190), but their own avatar.
func TestModeratorDeskRouteRefused(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("modrefused_vendor", "vendor")
	modID, mod := e.user("modrefused_moderator", "moderator")
	listing := e.product(vendorID, "physical")
	photo := e.image(vendorID, listing, "photo")
	avatar := e.image(vendorID, "", "avatar")
	own := e.image(modID, "", "avatar")
	e.wantDeskHidden(mod, photo, "moderator, published photo")
	e.wantDeskHidden(mod, avatar, "moderator, vendor avatar")
	if _, err := e.DB.Exec("UPDATE products SET archived=true WHERE id=$1", listing); err != nil {
		t.Fatal(err)
	}
	e.wantDeskHidden(mod, photo, "moderator, archived photo")
	e.wantDesk(mod, own, "moderator's own avatar")
}

// imageHeaders is D7's header set for a served image, with Content-Length.
func imageHeaders(n int) map[string]string {
	return map[string]string{
		"Content-Type":                 "image/jpeg",
		"Content-Length":               strconv.Itoa(n),
		"X-Content-Type-Options":       "nosniff",
		"Content-Security-Policy":      "default-src 'none'; sandbox",
		"Cache-Control":                "private, max-age=86400, immutable",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Content-Disposition":          `inline; filename="image.jpg"`,
	}
}

// checkImageHeaders asserts exactly D7's set (and the server's Date), nothing else.
func checkImageHeaders(t *testing.T, h http.Header, n int, what string) {
	t.Helper()
	want := imageHeaders(n)
	for k, v := range h {
		if k == "Date" {
			continue
		}
		if w, ok := want[k]; !ok || len(v) != 1 || v[0] != w {
			t.Fatalf("%s: header %s=%q, want %q (D7 set: %v)", what, k, v, w, want)
		}
	}
	for k := range want {
		if h.Get(k) == "" {
			t.Fatalf("%s: no %s", what, k)
		}
	}
}

// imageServer serves e.A over a real loopback listener (the Go server's own headers included).
func imageServer(t *testing.T, e *testEnv) *httptest.Server {
	s := httptest.NewServer(e.A)
	t.Cleanup(s.Close)
	return s
}

func fetch(t *testing.T, method, url string, cookies ...*http.Cookie) (*http.Response, []byte) {
	t.Helper()
	r, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

// D7: both routes answer with exactly the ruled header set, over httptest and a real connection: no ETag,
// Last-Modified or Set-Cookie, and none of the page headers.
func TestImageHeaders(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("headers_vendor", "vendor")
	photo := e.image(vendorID, e.product(vendorID, "physical"), "photo")
	checkImageHeaders(t, e.imgGet("GET", publicImageURL(photo.id, false)).Header(), len(photo.thumb), "public thumbnail")
	checkImageHeaders(t, e.imgGet("GET", publicImageURL(photo.id, true)).Header(), len(photo.full), "public full")
	checkImageHeaders(t, e.desk(vendor, photo.id, false).Header(), len(photo.thumb), "desk thumbnail")
	checkImageHeaders(t, e.desk(vendor, photo.id, true).Header(), len(photo.full), "desk full")
	s := imageServer(t, e)
	for _, c := range []struct {
		path    string
		cookies []*http.Cookie
		body    []byte
	}{
		{publicImageURL(photo.id, true), nil, photo.full},
		{publicImageURL(photo.id, false), []*http.Cookie{sessionCookie(vendor)}, photo.thumb},
		{deskImageURL(photo.id, true), []*http.Cookie{sessionCookie(vendor)}, photo.full},
	} {
		resp, body := fetch(t, "GET", s.URL+c.path, c.cookies...)
		if resp.StatusCode != 200 || !bytes.Equal(body, c.body) || resp.ContentLength != int64(len(c.body)) {
			t.Fatalf("%s over a connection: %d length %d", c.path, resp.StatusCode, resp.ContentLength)
		}
		checkImageHeaders(t, resp.Header, len(c.body), c.path+" over a connection")
	}
	// A POST is not a way in: refused without a slot, a session or a cookie.
	if w := e.imgGet("POST", publicImageURL(photo.id, false)); w.Code != 405 || len(w.Header().Values("Set-Cookie")) != 0 {
		t.Fatalf("POST image: %d %v", w.Code, w.Header())
	}
}

// D7/D8: HEAD answers like GET without the body, on both routes.
func TestImageHeadSupported(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("head_vendor", "vendor")
	photo := e.image(vendorID, e.product(vendorID, "physical"), "photo")
	w := e.imgGet("HEAD", publicImageURL(photo.id, true))
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD public: %d body %d bytes", w.Code, w.Body.Len())
	}
	checkImageHeaders(t, w.Header(), len(photo.full), "HEAD public")
	w = e.imgGet("HEAD", deskImageURL(photo.id, false), sessionCookie(vendor))
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD desk: %d body %d bytes", w.Code, w.Body.Len())
	}
	checkImageHeaders(t, w.Header(), len(photo.thumb), "HEAD desk")
	s := imageServer(t, e)
	resp, body := fetch(t, "HEAD", s.URL+publicImageURL(photo.id, false))
	if resp.StatusCode != 200 || len(body) != 0 || resp.Header.Get("Content-Length") != strconv.Itoa(len(photo.thumb)) {
		t.Fatalf("HEAD over a connection: %d %v", resp.StatusCode, resp.Header)
	}
}

// Owner decision 1(a),(b),(d): a staff avatar is public only while the public staff list is on and that
// account has not hidden itself (hidden by default); otherwise the unknown-id answer. A vendor's avatar does
// not depend on either.
func TestHiddenStaffAvatarNotPublic(t *testing.T) {
	e := newTestApp(t)
	modID, _ := e.user("staffhide_moderator", "moderator")
	adminID, _ := e.user("staffhide_admin", "admin")
	vendorID, _ := e.user("staffhide_vendor", "vendor")
	mod, admin, vendor := e.image(modID, "", "avatar"), e.image(adminID, "", "avatar"), e.image(vendorID, "", "avatar")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	list := func(on bool) {
		exec("UPDATE settings SET value=$1 WHERE key='staff_list_public'", strconv.FormatBool(on))
	}
	var setting string
	if err := e.DB.QueryRow("SELECT value FROM settings WHERE key='staff_list_public'").Scan(&setting); err != nil || setting != "false" {
		t.Fatalf("staff_list_public=%q %v, want false by default", setting, err)
	}
	e.wantHidden(mod, "list off, moderator hidden (defaults)")
	e.wantHidden(admin, "list off, administrator hidden (defaults)")
	e.wantPublic(vendor, "vendor, list off")

	exec("UPDATE users SET staff_hidden=false WHERE id IN ($1,$2)", modID, adminID)
	e.wantHidden(mod, "list off, moderator not hidden")
	e.wantHidden(admin, "list off, administrator not hidden")

	list(true)
	e.wantPublic(mod, "list on, moderator not hidden")
	e.wantPublic(admin, "list on, administrator not hidden")
	exec("UPDATE users SET staff_hidden=true WHERE id=$1", modID)
	e.wantHidden(mod, "list on, moderator hidden")
	e.wantPublic(admin, "list on, administrator still shown")

	exec("UPDATE users SET staff_hidden=false, suspended_at=now() WHERE id=$1", modID)
	e.wantHidden(mod, "list on, suspended moderator")
	exec("UPDATE users SET suspended_at=NULL WHERE id=$1", modID)
	e.wantPublic(mod, "list on, moderator restored")

	// An unknown value is off (fail closed), and a missing row too.
	exec("UPDATE settings SET value='yes' WHERE key='staff_list_public'")
	e.wantHidden(mod, "list setting 'yes'")
	exec("DELETE FROM settings WHERE key='staff_list_public'")
	e.wantHidden(admin, "list setting missing")
	e.wantPublic(vendor, "vendor, list setting missing")
}

// Owner decision 1(c): a hidden staff member's avatar is served on the desk route to the buyer, the vendor and
// the staff of an order on which that member recorded the dispute decision; an unrelated buyer, or the parties
// of an order someone else decided, get the unknown-id answer.
func TestStaffAvatarVisibleToOrderParties(t *testing.T) {
	e := newTestApp(t)
	modID, mod := e.user("parties_moderator", "moderator")
	_, otherMod := e.user("parties_other_moderator", "moderator")
	decider2ID, _ := e.user("parties_second_moderator", "moderator")
	vendorID, vendor := e.user("parties_vendor", "vendor")
	buyerID, buyer := e.user("parties_buyer", "buyer")
	_, stranger := e.user("parties_stranger", "buyer")
	buyer2ID, buyer2 := e.user("parties_buyer_two", "buyer")
	_, otherVendor := e.user("parties_other_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	avatar := e.image(modID, "", "avatar")

	decided := e.order(buyerID, listing, "BTC", "resolved")
	other := e.order(buyer2ID, listing, "BTC", "resolved")
	for _, ev := range [][3]string{{decided, "paid", "disputed"}, {other, "paid", "disputed"}} {
		if _, err := e.DB.Exec("INSERT INTO order_events(order_id,from_state,to_state,actor_id) VALUES($1,$2,$3,$4)", ev[0], ev[1], ev[2], buyerID); err != nil {
			t.Fatal(err)
		}
	}
	// Before any decision: nobody but the owner.
	e.wantDeskHidden(buyer, avatar, "buyer before the decision")
	for _, ev := range []struct{ order, actor string }{{decided, modID}, {other, decider2ID}} {
		if _, err := e.DB.Exec("INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,'disputed','resolved',$2,'Resolved — refund to buyer')", ev.order, ev.actor); err != nil {
			t.Fatal(err)
		}
	}
	e.wantHidden(avatar, "public route (list off, hidden)")
	e.wantDesk(buyer, avatar, "buyer of the decided order")
	e.wantDesk(vendor, avatar, "vendor of the decided order")
	e.wantDesk(otherMod, avatar, "another moderator (staff of the order)")
	e.wantDesk(mod, avatar, "the moderator")
	e.wantDeskHidden(stranger, avatar, "unrelated buyer")
	e.wantDeskHidden(otherVendor, avatar, "unrelated vendor")
	// buyer2's order was decided by another moderator; buyer2 is no party to modID's decision.
	e.wantDeskHidden(buyer2, avatar, "party to an order another moderator decided")
	// A suspended staff member's avatar is no longer shown to the parties.
	if _, err := e.DB.Exec("UPDATE users SET suspended_at=now() WHERE id=$1", modID); err != nil {
		t.Fatal(err)
	}
	e.wantDeskHidden(buyer, avatar, "buyer, suspended moderator")
}

// A-187: catalog, vendor, desk and every other query read image bytes nowhere but the image routes. Every SQL
// string literal in the package's non-test Go files that names the "full" or thumb column must use it only
// inside octet_length(...) or as an INSERT column, and no query selects every column of images.
func TestOnlyImageRoutesSelectImageBytes(t *testing.T) {
	files, err := filepath.Glob("internal/market/*.go")
	if err != nil || len(files) < 20 {
		t.Fatalf("package files: %d %v", len(files), err)
	}
	column := regexp.MustCompile(`"full"|\bthumb\b`)
	allowed := regexp.MustCompile(`octet_length\(("full"|thumb)\)`)
	star := regexp.MustCompile(`(?i)select\s+(\*|\w+\.\*)[^;]*\bimages\b|\bimages\b[^;]*\bi\.\*|images\.\*`)
	servedBy := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if star.MatchString(s) {
				t.Errorf("%s selects every column of images: %q", f, s)
			}
			rest := allowed.ReplaceAllString(s, "")
			if strings.HasPrefix(strings.TrimSpace(rest), "INSERT INTO images(") {
				return true
			}
			if column.MatchString(rest) {
				servedBy[filepath.Base(f)]++
				if filepath.Base(f) != "image_routes.go" {
					t.Errorf("%s reads image bytes: %q", f, s)
				}
			}
			return true
		})
	}
	if servedBy["image_routes.go"] == 0 {
		t.Fatal("found no image query in image_routes.go: the check is not looking at the right strings")
	}
}

// A server error on an image route is logged under the route's name, never the image id (A-141's log line).
func TestImageRouteLogNameHasNoID(t *testing.T) {
	id := randomToken()[:32]
	for path, want := range map[string]string{publicImageURL(id, false): "/img/", deskImageURL(id, true): "/desk/img/"} {
		if kind, name := routeOf(httptest.NewRequest("GET", path, nil)); kind != "page" || name != want {
			t.Fatalf("routeOf(%s) = %s %s, want page %s", path, kind, name, want)
		}
	}
	a := testHTTPApp(false)
	logs := captureLog(t)
	w := httptest.NewRecorder()
	a.writeImage(w, httptest.NewRequest("GET", publicImageURL(id, true), nil), nil, io.ErrUnexpectedEOF)
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" || !strings.Contains(logs.String(), "name=/img/ ") || strings.Contains(logs.String(), id) {
		t.Fatalf("database failure: %d %q, log %q", w.Code, w.Body.String(), logs.String())
	}
}
