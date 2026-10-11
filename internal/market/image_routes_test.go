package market

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	for _, path := range []string{"/catalog", deskImageURL(photo.id, false)} {
		// Give each control its own live deadline. The session lookup waits on the held table until its
		// caller expires; this is not a retryable database outage (A-209).
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		start := time.Now()
		r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
		r.AddCookie(sessionCookie(vendor))
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		cancel()
		if w.Code != 500 || w.Header().Get("Retry-After") != "" || time.Since(start) < 200*time.Millisecond {
			t.Fatalf("control: GET %s with the sessions table locked answered %d after %s (retry=%q); want a blocked lookup until caller expiry", path, w.Code, time.Since(start), w.Header().Get("Retry-After"))
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
	// A decider who is no longer staff: their avatar is no longer shown beside the decision.
	if _, err := e.DB.Exec("UPDATE users SET role='buyer' WHERE id=$1", modID); err != nil {
		t.Fatal(err)
	}
	e.wantDeskHidden(buyer, avatar, "buyer, demoted moderator")
	e.wantDeskHidden(otherMod, avatar, "another moderator, demoted moderator")
	e.wantDesk(mod, avatar, "the demoted moderator's own avatar")
	if _, err := e.DB.Exec("UPDATE users SET role='moderator' WHERE id=$1", modID); err != nil {
		t.Fatal(err)
	}
	e.wantDesk(buyer, avatar, "buyer, moderator again")
	// A suspended staff member's avatar is no longer shown to the parties.
	if _, err := e.DB.Exec("UPDATE users SET suspended_at=now() WHERE id=$1", modID); err != nil {
		t.Fatal(err)
	}
	e.wantDeskHidden(buyer, avatar, "buyer, suspended moderator")
}

// imageByteReaders are the helpers in image_routes.go that build a query reading image bytes; no other file may
// name them.
var imageByteReaders = []string{"imageSelect"}

// imageBytesChecker finds SQL that could read image bytes in Go sources (file name -> source) outside
// image_routes.go. It checks every string literal, and every concatenation of literals and string constants
// (constants resolved across all the files given, anything else standing in as " ? "), for:
//
//   - the full or thumb column named anywhere (any case, qualified or quoted) but inside octet_length(...) or an
//     INSERT INTO images(...) column list;
//   - whole rows of images: SELECT * or RETURNING *, alias.*, the table or its alias as a select or RETURNING
//     item or as a function argument, and row_to_json, to_json(b) and the other row-to-value functions;
//   - any reference to imageByteReaders.
//
// It returns one line per finding, and how many texts in image_routes.go name the columns (the check's anchor).
func imageBytesChecker(t *testing.T, files map[string]string) (findings []string, anchors int) {
	t.Helper()
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	constExprs := map[string]ast.Expr{}
	for name, src := range files {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed[name] = f
		ast.Inspect(f, func(n ast.Node) bool {
			if d, ok := n.(*ast.GenDecl); ok && d.Tok == token.CONST {
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, id := range vs.Names {
						if i < len(vs.Values) {
							constExprs[id.Name] = vs.Values[i]
						}
					}
				}
			}
			return true
		})
	}
	consts := map[string]string{}
	// fold renders a string expression: literals and resolved constants as their text, anything else as " ? ".
	var fold func(e ast.Expr) (string, bool)
	fold = func(e ast.Expr) (string, bool) {
		switch e := e.(type) {
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				if s, err := strconv.Unquote(e.Value); err == nil {
					return s, true
				}
			}
		case *ast.ParenExpr:
			return fold(e.X)
		case *ast.Ident:
			if s, ok := consts[e.Name]; ok {
				return s, true
			}
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				l, lok := fold(e.X)
				r, rok := fold(e.Y)
				if !lok {
					l = " ? "
				}
				if !rok {
					r = " ? "
				}
				return l + r, lok || rok
			}
		}
		return "", false
	}
	for changed := true; changed; { // constants defined from other constants
		changed = false
		for name, e := range constExprs {
			if s, ok := fold(e); ok && consts[name] != s {
				consts[name], changed = s, true
			}
		}
	}
	octet := regexp.MustCompile(`(?i)octet_length\s*\([^()]*\)`)
	insertColumns := regexp.MustCompile(`(?is)\binsert\s+into\s+images\s*\([^)]*\)`)
	column := regexp.MustCompile(`(?i)\b(full|thumb)\b`)
	sqlish := regexp.MustCompile(`(?i)\b(select|insert|update|delete|returning|from|join|where)\b|\.\s*"?(full|thumb)\b|"(full|thumb)"`)
	tableRef := regexp.MustCompile(`(?i)\bimages\b(?:\s+(?:as\s+)?([a-z_][a-z0-9_]*))?`)
	notAlias := map[string]bool{"where": true, "join": true, "left": true, "right": true, "inner": true, "full": true, "cross": true,
		"natural": true, "on": true, "set": true, "values": true, "order": true, "group": true, "limit": true, "offset": true,
		"returning": true, "using": true, "for": true, "union": true, "lateral": true, "and": true, "or": true}
	rowFunc := regexp.MustCompile(`(?i)\b(row_to_json|to_json|to_jsonb|json_agg|jsonb_agg|json_build_object|jsonb_build_object|hstore|json_populate_record|jsonb_populate_record|array_agg|string_agg|format|concat|concat_ws|encode)\s*\(`)
	star := regexp.MustCompile(`(?i)\b(select\s+(distinct\s+)?|returning\s+)\*`)
	check := func(file, text string) {
		rest := insertColumns.ReplaceAllString(octet.ReplaceAllString(text, ""), "images")
		if column.MatchString(rest) && sqlish.MatchString(rest) {
			if file == "image_routes.go" {
				anchors++
			} else {
				findings = append(findings, file+": names an image column: "+text)
			}
		}
		if file == "image_routes.go" || !tableRef.MatchString(text) {
			return
		}
		aliases := []string{"images"}
		for _, m := range tableRef.FindAllStringSubmatch(text, -1) {
			if m[1] != "" && !notAlias[strings.ToLower(m[1])] {
				aliases = append(aliases, m[1])
			}
		}
		whole := star.MatchString(text) || rowFunc.MatchString(text)
		for _, a := range aliases {
			q := regexp.QuoteMeta(a)
			for _, re := range []string{
				`(?i)\b` + q + `\s*\.\s*\*`, // alias.*
				`(?i)(\bselect\s+(distinct\s+)?|\breturning\s+|,\s*)` + q + `\s*(,|\bfrom\b|$)`, // the row as an item
				`(?i)\(\s*` + q + `\s*[),]`, // the row as an argument
				`(?i)(\bselect\s+(distinct\s+)?|\breturning\s+|,\s*|\(\s*)` + q + `\s+as\b`, // the row renamed or CAST(row AS …)
				`(?i)\b` + q + `\s*::`,       // the row cast (i::text)
				`(?i)\[\s*` + q + `\s*[\],]`, // the row in an array (ARRAY[i])
			} {
				whole = whole || regexp.MustCompile(re).MatchString(text)
			}
		}
		if whole {
			findings = append(findings, file+": reads whole rows of images: "+text)
		}
	}
	for name, f := range parsed {
		inner := map[ast.Node]bool{} // parts of a concatenation already checked as a whole
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BinaryExpr:
				if n.Op != token.ADD {
					return true
				}
				if !inner[n] {
					if s, ok := fold(n); ok {
						check(name, s)
					}
				}
				for _, side := range []ast.Expr{n.X, n.Y} {
					if b, ok := side.(*ast.BinaryExpr); ok {
						inner[b] = true
					}
				}
			case *ast.BasicLit:
				if s, ok := fold(n); ok {
					check(name, s)
				}
			case *ast.Ident:
				if name != "image_routes.go" && slices.Contains(imageByteReaders, n.Name) {
					findings = append(findings, name+": uses "+n.Name+", which reads image bytes")
				}
			}
			return true
		})
	}
	return findings, anchors
}

// The checker itself: each of these reads image bytes on PostgreSQL 17 and must be found; the allowed forms
// must not be.
func TestImageBytesCheckerCatchesEvasions(t *testing.T) {
	for name, src := range map[string]string{
		"qualified unquoted":  "const q = `SELECT i.full FROM images i WHERE i.id=$1`",
		"upper case":          "const q = `SELECT THUMB FROM images WHERE id=$1`",
		"whole-row alias":     "const q = `SELECT i FROM images i WHERE i.id=$1`",
		"row_to_json":         "const q = `SELECT row_to_json(i) FROM images i WHERE i.id=$1`",
		"insert returning":    "const q = `INSERT INTO images(id,owner_user_id,kind,\"full\",thumb) VALUES($1,$2,$3,$4,$5) RETURNING thumb`",
		"reader reused":       "func q(full bool) string { return imageSelect(full, false) + \"images i WHERE i.id=$1\" }",
		"to_jsonb of table":   "const q = `SELECT to_jsonb(images) FROM images WHERE id=$1`",
		"select star":         "const q = `SELECT * FROM images WHERE id=$1`",
		"alias star":          "const q = `SELECT i.* FROM images i`",
		"returning star":      "const q = `DELETE FROM images WHERE id=$1 RETURNING *`",
		"concatenated alias":  "const from = `images i JOIN users o ON o.id=i.owner_user_id`\nconst q = \"SELECT i FROM \" + from",
		"concatenated column": "const col = `i.\"full\"`\nfunc q() string { return \"SELECT \" + col + \" FROM x\" }",
		"row cast":            "const q = `SELECT i::text FROM images i WHERE i.id=$1`",
		"row in array":        "const q = `SELECT ARRAY[i] FROM images i WHERE i.id=$1`",
		"CAST row":            "const q = `SELECT CAST(i AS text) FROM images i WHERE i.id=$1`",
		"lateral row":         "const q = `SELECT x.v FROM images i, LATERAL (SELECT i AS v) x WHERE i.id=$1`",
	} {
		findings, _ := imageBytesChecker(t, map[string]string{"load.go": "package market\n" + src})
		if len(findings) == 0 {
			t.Errorf("%s: not found in %s", name, src)
		}
	}
	for name, src := range map[string]string{
		"sizes":        "const q = `SELECT count(*),coalesce(sum(octet_length(\"full\")+octet_length(thumb)),0) FROM images`",
		"insert":       "const q = `INSERT INTO images(id,owner_user_id,product_id,kind,\"full\",thumb,width,height,description) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`",
		"ids":          "const q = `SELECT i.id,i.width,i.height,i.description FROM images i WHERE i.product_id=$1 ORDER BY i.created`",
		"public ids":   "const from = `images i JOIN users o ON o.id=i.owner_user_id`\nconst q = \"SELECT i.id FROM \" + from + \" WHERE i.product_id=$1\"",
		"delete":       "const q = `DELETE FROM images WHERE owner_user_id=$1 AND kind='avatar'`",
		"prose":        "const m = \"Photo storage on this market is full; contact the operator.\"",
		"prose verify": "const m = \"set sslmode=require or verify-full in DATABASE_URL\"",
	} {
		if findings, _ := imageBytesChecker(t, map[string]string{"load.go": "package market\n" + src}); len(findings) != 0 {
			t.Errorf("%s: allowed form flagged: %q", name, findings)
		}
	}
}

// A-187: catalog, vendor, desk and every other query read image bytes nowhere but the image routes
// (imageBytesChecker over the package's non-test Go files).
func TestOnlyImageRoutesSelectImageBytes(t *testing.T) {
	names, err := filepath.Glob("internal/market/*.go")
	if err != nil || len(names) < 20 {
		t.Fatalf("package files: %d %v", len(names), err)
	}
	files := map[string]string{}
	for _, f := range names {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.Base(f)] = string(src)
	}
	findings, anchors := imageBytesChecker(t, files)
	for _, f := range findings {
		t.Error(f)
	}
	if anchors == 0 {
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
	logs := captureLog(t)
	w := httptest.NewRecorder()
	writeImage(w, httptest.NewRequest("GET", publicImageURL(id, true), nil), 0, nil, io.ErrUnexpectedEOF)
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" || !strings.Contains(logs.String(), "name=/img/ ") || strings.Contains(logs.String(), id) {
		t.Fatalf("database failure: %d %q, log %q", w.Code, w.Body.String(), logs.String())
	}
}

// smallSendBuffers gives each accepted connection an 8 KiB send buffer, so a response to a client that does not
// read blocks in Write after a few KiB instead of filling the kernel's buffers.
type smallSendBuffers struct{ net.Listener }

func (l smallSendBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetWriteBuffer(8 << 10)
	}
	return c, err
}

// PR #23 review R-1: the image bytes held by responses still being written are bounded by imageBytes (64 MiB),
// though the image slot is released when the response starts. Clients that never read pin at most that much;
// once it is spent a full image gets the busy 503 with Retry-After at once, pages still load, and a HEAD, which
// holds no bytes, still answers. Closing the clients gives it all back.
func TestImageSlowReadersBoundedByByteBudget(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("slow_reader_vendor", "vendor")
	full := bytes.Repeat([]byte{0xab}, 2_000_000)
	id := randomToken()[:32]
	if _, err := e.DB.Exec(`INSERT INTO images(id,owner_user_id,product_id,kind,"full",thumb,width,height) VALUES($1,$2,$3,'photo',$4,$5,1200,900)`,
		id, vendorID, e.product(vendorID, "physical"), full, []byte("\xff\xd8\xff thumbnail")); err != nil {
		t.Fatal(err)
	}
	if imageBytes.limit != 64<<20 || imageBytes.inUse() != 0 {
		t.Fatalf("image byte budget %d, %d in use; want 64 MiB, none", imageBytes.limit, imageBytes.inUse())
	}
	s := httptest.NewUnstartedServer(e.A)
	s.Listener = smallSendBuffers{s.Listener}
	s.Start()
	defer s.Close()
	path := publicImageURL(id, true)
	get := func(method, p string) *http.Response {
		t.Helper()
		resp, _ := fetch(t, method, s.URL+p)
		return resp
	}
	for range 3 { // warm the pools the baseline should include
		if resp := get("GET", path); resp.StatusCode != 200 || resp.ContentLength != int64(len(full)) {
			t.Fatalf("warm-up: %d", resp.StatusCode)
		}
	}
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	base := heap()
	var slow []net.Conn
	defer func() {
		for _, c := range slow {
			c.Close()
		}
	}()
	send := func() net.Conn {
		c, err := net.Dial("tcp", s.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.(*net.TCPConn).SetReadBuffer(4 << 10)
		if _, err = io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: market.test\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		return c
	}
	size := int64(len(full))
	for imageBytes.inUse()+size <= imageBytes.limit {
		before := imageBytes.inUse()
		slow = append(slow, send())
		// Wait for the reservation trimmed to the image's size: the request first reserves maxFullBytes, and
		// counting that transient 2 MiB stops the loop one client early (seen under load: "32 … want 33").
		waitFor(t, func() bool { return imageBytes.inUse() == before+size })
	}
	if n := len(slow); n != int(imageBytes.limit/size) {
		t.Fatalf("%d clients that never read filled the budget, want %d", n, imageBytes.limit/size)
	}
	// Further clients get the busy answer at once, whether or not they read it.
	for range 16 {
		c := send()
		slow = append(slow, c)
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		status := make([]byte, 12)
		if _, err := io.ReadFull(c, status); err != nil || string(status) != "HTTP/1.1 503" {
			t.Fatalf("a client past the budget got %q %v", status, err)
		}
	}
	resp := get("GET", path)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("full image with the byte budget spent: %d Retry-After=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if resp := get("GET", "/catalog"); resp.StatusCode != 200 {
		t.Fatalf("page with the byte budget spent: %d", resp.StatusCode)
	}
	if resp := get("HEAD", path); resp.StatusCode != 200 || resp.Header.Get("Content-Length") != strconv.Itoa(len(full)) {
		t.Fatalf("HEAD with the byte budget spent: %d %v", resp.StatusCode, resp.Header)
	}
	if grown := int64(heap()) - int64(base); grown > imageBytes.limit+16<<20 {
		t.Fatalf("heap grew %d MiB with %d clients not reading; the budget is %d MiB", grown>>20, len(slow), imageBytes.limit>>20)
	} else {
		t.Logf("heap grew %d MiB with %d clients not reading", grown>>20, len(slow))
	}
	for _, c := range slow {
		c.Close()
	}
	slow = nil
	waitFor(t, func() bool { return imageBytes.inUse() == 0 })
	if resp := get("GET", path); resp.StatusCode != 200 {
		t.Fatalf("full image after the clients went away: %d", resp.StatusCode)
	}
}

// PR #23 re-check N-1: /desk/img has its own byte budget. A signed-in account (anyone can register) flooding the
// desk route with unknown full-image ids reserves 2 MiB each before its query under the 64 page slots; with one
// shared budget that refused public images. With the desk budget spent, the desk route answers busy and the
// public route still serves, full images included.
func TestDeskImageFloodLeavesPublicImages(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendorSession := e.user("desk_flood_vendor", "vendor")
	photo := e.image(vendorID, e.product(vendorID, "physical"), "photo")
	if imageBytes == deskImageBytes || deskImageBytes.limit != 32<<20 || deskImageBytes.inUse() != 0 || imageBytes.inUse() != 0 {
		t.Fatalf("budgets: public %d/%d, desk %d/%d", imageBytes.inUse(), imageBytes.limit, deskImageBytes.inUse(), deskImageBytes.limit)
	}
	if !deskImageBytes.take(deskImageBytes.limit) { // what a flood of 16 unknown -f ids in flight holds
		t.Fatal("could not fill the desk budget")
	}
	filled := true
	t.Cleanup(func() { // a failure below must not leave the package-wide budget spent for later tests
		if filled {
			deskImageBytes.give(deskImageBytes.limit)
		}
	})
	for _, full := range []bool{false, true} {
		if w := e.desk(vendorSession, photo.id, full); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
			t.Fatalf("desk route with its budget spent (full=%v): %d %q", full, w.Code, w.Header().Get("Retry-After"))
		}
		if w := e.imgGet("GET", publicImageURL(photo.id, full)); w.Code != 200 {
			t.Fatalf("public route while the desk budget is spent (full=%v): %d", full, w.Code)
		}
	}
	if imageBytes.inUse() != 0 {
		t.Fatalf("public budget holds %d after its responses", imageBytes.inUse())
	}
	deskImageBytes.give(deskImageBytes.limit)
	filled = false
	if w := e.desk(vendorSession, photo.id, true); w.Code != 200 || deskImageBytes.inUse() != 0 {
		t.Fatalf("desk route once its budget is back: %d, %d in use", w.Code, deskImageBytes.inUse())
	}
}
