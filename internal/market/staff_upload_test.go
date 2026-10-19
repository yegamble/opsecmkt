package market

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A-551/A-590 (war room iteration 66): in the separated topology a staff member on the private administrator
// connection cannot reach the public upload gateway with their session (the app refuses originals, and the public
// gateway refuses administrator sessions). They authorize one upload on the private connection with their
// password (and authenticator code when enrolled); the next page posts only that one-use capability and the file
// to the fixed configured gateway origin.

var staffTicketPattern = regexp.MustCompile(`name="ticket" value="([0-9a-f]{64})"`)

// staffUploadEnv is an isolated-upload app with a private admin socket, a configured gateway origin and a gateway
// whose fresh scanner report says Ready.
func staffUploadEnv(t *testing.T) (*testEnv, *fakeClamd, *uploadGateway) {
	t.Helper()
	e := newTestApp(t)
	f := newFakeClamd(t)
	g := uploadTestGateway(t, e, f)
	e.A.adminSocket = "/run/market-admin/http.sock"
	e.A.brandingUploadOrigin = "https://upload.example"
	return e, f, g
}

// staffAuthorize confirms one staff upload on the private connection and returns the handoff page.
func staffAuthorize(t *testing.T, e *testEnv, session string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if !form.Has("password") {
		form.Set("password", testPassword)
	}
	w := brandingAdminRequest(e, true, "POST", "/admin/uploads/authorize", session, form)
	e.check(w, 303)
	if w.Header().Get("Location") != "/admin/uploads/upload" {
		t.Fatalf("authorize redirect %q", w.Header().Get("Location"))
	}
	w = brandingAdminRequest(e, true, "GET", "/admin/uploads/upload", session, nil)
	e.check(w, 200)
	return w
}

func staffTicket(t *testing.T, page string) string {
	t.Helper()
	m := staffTicketPattern.FindStringSubmatch(page)
	if len(m) != 2 {
		t.Fatalf("handoff lacks a capability:\n%s", page)
	}
	return m[1]
}

func staffAvatarParts(ticket string, photo []byte) []part {
	return []part{{name: "ticket", body: []byte(ticket)}, {name: "photo", filename: "me.png", ctype: "image/png", body: photo}}
}

func TestStaffAvatarViaGatewayCapability(t *testing.T) {
	e, f, g := staffUploadEnv(t)
	adminID, admin := e.user("staff_cap_admin", "admin")
	key, _ := agEnableTOTP(e, adminID)

	// The private account page offers the confirmation, never a multipart form that would post originals to the app.
	w := brandingAdminRequest(e, true, "GET", "/account", admin, nil)
	e.check(w, 200)
	avatar := sectionOf(t, w.Body.String(), "avatar")
	if !strings.Contains(avatar, `action="/admin/uploads/authorize"`) || !strings.Contains(avatar, `name="kind" value="avatar"`) || !strings.Contains(avatar, `name="password"`) || !strings.Contains(avatar, `name="code"`) || strings.Contains(avatar, "multipart/form-data") || strings.Contains(avatar, `type="file"`) {
		t.Fatalf("private avatar panel:\n%s", avatar)
	}

	// Refusals mint nothing: the public listener, a wrong password and a missing authenticator code.
	form := func() url.Values { return url.Values{"kind": {"avatar"}, formNonceField: {randomToken()[:32]}} }
	e.check(brandingAdminRequest(e, false, "POST", "/admin/uploads/authorize", admin, url.Values{"kind": {"avatar"}, "password": {testPassword}}), 404)
	bad := form()
	bad.Set("password", "incorrect-password")
	e.check(brandingAdminRequest(e, true, "POST", "/admin/uploads/authorize", admin, bad), 401)
	noCode := form()
	noCode.Set("password", testPassword)
	e.check(brandingAdminRequest(e, true, "POST", "/admin/uploads/authorize", admin, noCode), 401)
	e.check(brandingAdminRequest(e, true, "GET", "/admin/uploads/upload", admin, nil), 409)
	if len(e.audits(adminID)) != 0 {
		t.Fatal("a refused confirmation was audited")
	}

	// Confirmed: the handoff page carries the capability and the fixed gateway origin, and no credential.
	ok := form()
	ok.Set("code", agTOTPNow(key))
	page := staffAuthorize(t, e, admin, ok)
	body := page.Body.String()
	if !strings.Contains(page.Header().Get("Content-Security-Policy"), "form-action 'self' https://upload.example;") || page.Header().Get("Referrer-Policy") != "no-referrer" || page.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(body, `action="https://upload.example`+staffUploadPath+`"`) || strings.Contains(body, admin) || strings.Contains(body, testPassword) ||
		strings.Contains(body, `name="csrf"`) || strings.Contains(body, `name="password"`) || strings.Contains(body, `name="code"`) || strings.Contains(body, `name="id"`) {
		t.Fatalf("handoff leaked a credential or lost its narrow CSP:\n%s", body)
	}
	ticket := staffTicket(t, body)
	// The exact replay of the confirmed form answers with its saved redirect and mints nothing new.
	e.check(brandingAdminRequest(e, true, "POST", "/admin/uploads/authorize", admin, ok), 303)
	if again := staffTicket(t, brandingAdminRequest(e, true, "GET", "/admin/uploads/upload", admin, nil).Body.String()); again != ticket {
		t.Fatal("the form replay minted another capability")
	}
	if got := e.audits(adminID); len(got) != 1 || !strings.HasPrefix(got[0], "Authorized one avatar upload") || !strings.Contains(got[0], "password and authenticator code") {
		t.Fatalf("authorization audit %q", got)
	}

	// The public listener never serves the hand-off, whatever Host or headers it is sent; the backend's staff
	// admission needs the gateway's verified client certificate.
	spoofed := httptest.NewRequest("GET", "/admin/uploads/upload", nil)
	spoofed.Host = "private-admin.onion"
	spoofed.Header.Set("X-Admin-Authenticated", "true")
	spoofed.Header.Set("X-Forwarded-Host", "private-admin.onion")
	spoofed.AddCookie(&http.Cookie{Name: "session", Value: admin})
	pub := httptest.NewRecorder()
	e.A.ServeHTTP(pub, spoofed)
	if pub.Code != 404 || strings.Contains(pub.Body.String(), ticket) {
		t.Fatalf("public hand-off: %d", pub.Code)
	}
	body2, _ := json.Marshal(staffGatewayAdmission{Ticket: ticket})
	plain := httptest.NewRequest("POST", "/v1/uploads/staff-admit", strings.NewReader(string(body2)))
	plain.Header.Set("Content-Type", "application/json")
	plain.Header.Set("X-Tor-Onion-Role", "admin")
	pub = httptest.NewRecorder()
	e.A.UploadBackendHandler().ServeHTTP(pub, plain)
	e.check(pub, 404)

	// Another account neither sees the capability nor gains anything by sending its own session with it.
	otherID, other := e.user("staff_cap_other", "admin")
	e.check(brandingAdminRequest(e, true, "GET", "/admin/uploads/upload", other, nil), 409)

	// The original goes to the gateway only; posted to the isolated app it is refused unread.
	original := fixturePNG(t, fixturePicture(300, 200))
	r, counter := uploadReq(staffUploadPath, "", staffAvatarParts(ticket, original)...)
	direct := httptest.NewRecorder()
	e.A.ServeHTTP(direct, r)
	if direct.Code != 415 || counter.read() != 0 {
		t.Fatalf("original entered the isolated app: %d read=%d", direct.Code, counter.read())
	}
	w = uploadThroughGateway(g, staffUploadPath, other, staffAvatarParts(ticket, original)...)
	checkPage(t, w, 200, "Avatar saved")
	if len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), e.A.csrf(admin)) {
		t.Fatal("the gateway result page carried a cookie or the session's CSRF token")
	}
	var owner string
	var full []byte
	var width, height int
	if err := e.DB.QueryRow(`SELECT owner_user_id,"full",width,height FROM images WHERE kind='avatar'`).Scan(&owner, &full, &width, &height); err != nil {
		t.Fatal(err)
	}
	if owner != adminID || width != 200 || height != 200 || e.imageCount() != 1 {
		t.Fatalf("avatar owner=%s (other=%s) %dx%d images=%d", owner, otherID, width, height, e.imageCount())
	}
	assertCleanJPEG(t, "staff avatar", full)
	if got := e.audits(adminID); len(got) != 2 || got[1] != "Added avatar" {
		t.Fatalf("upload audit %q", got)
	}

	// One use: the same capability again is refused before the scanner sees the file.
	scans := scansOf(f)
	w = uploadThroughGateway(g, staffUploadPath, "", staffAvatarParts(ticket, original)...)
	if w.Code != 409 || scansOf(f) != scans || e.imageCount() != 1 {
		t.Fatalf("replayed capability: %d scans %d→%d", w.Code, scans, scansOf(f))
	}
	e.check(brandingAdminRequest(e, true, "GET", "/admin/uploads/upload", admin, nil), 409)

	// A capability for a staff account without TOTP still needs the password; it expires unused after its lifetime.
	_, mod := e.user("staff_cap_moderator", "moderator")
	expired := staffTicket(t, staffAuthorize(t, e, mod, form()).Body.String())
	e.A.staffUploads.mu.Lock()
	for id, c := range e.A.staffUploads.pending {
		c.expires = time.Now().Add(-time.Second)
		e.A.staffUploads.pending[id] = c
	}
	e.A.staffUploads.mu.Unlock()
	e.check(brandingAdminRequest(e, true, "GET", "/admin/uploads/upload", mod, nil), 409)
	w = uploadThroughGateway(g, staffUploadPath, "", staffAvatarParts(expired, original)...)
	if w.Code != 409 || scansOf(f) != scans {
		t.Fatalf("expired capability: %d", w.Code)
	}

	// A newer authorization replaces an unused one; a revoked session ends it.
	first := staffTicket(t, staffAuthorize(t, e, mod, form()).Body.String())
	second := staffTicket(t, staffAuthorize(t, e, mod, form()).Body.String())
	if w = uploadThroughGateway(g, staffUploadPath, "", staffAvatarParts(first, original)...); w.Code != 409 {
		t.Fatalf("replaced capability: %d", w.Code)
	}
	agExec(e, `DELETE FROM sessions WHERE token_hash=$1`, digest(mod))
	if w = uploadThroughGateway(g, staffUploadPath, "", staffAvatarParts(second, original)...); w.Code != 409 || scansOf(f) != scans {
		t.Fatalf("revoked session: %d", w.Code)
	}

	// Bound to its kind: an avatar capability names no listing, and its normalized result must be an avatar.
	_, mod2 := e.user("staff_cap_moderator2", "moderator")
	withID := staffTicket(t, staffAuthorize(t, e, mod2, form()).Body.String())
	parts := append([]part{{name: "ticket", body: []byte(withID)}, {name: "id", body: []byte("x")}}, staffAvatarParts(withID, original)[1])
	if w = uploadThroughGateway(g, staffUploadPath, "", parts...); w.Code != 400 || scansOf(f) != scans {
		t.Fatalf("avatar capability with a listing id: %d", w.Code)
	}
	envelope := staffTicket(t, staffAuthorize(t, e, mod2, form()).Body.String())
	resp, err := g.call(context.Background(), "/v1/uploads/staff-admit", staffGatewayAdmission{Ticket: envelope})
	if err != nil || resp.status != 200 {
		t.Fatalf("admit %v %d %s", err, resp.status, resp.body)
	}
	var admitted struct{ Ticket, Kind string }
	if json.Unmarshal(resp.body, &admitted) != nil || admitted.Kind != "avatar" || len(admitted.Ticket) != 64 {
		t.Fatalf("admission %s", resp.body)
	}
	photo := fixtureJPEG(t, fixturePicture(400, 300), 85)
	resp, err = g.call(context.Background(), "/v1/uploads/finish", uploadFinish{Ticket: admitted.Ticket, Full: photo, Thumb: smallJPEG(t), Width: 400, Height: 300})
	if err != nil || resp.status != 422 || e.imageCount() != 1 {
		t.Fatalf("photo-shaped result for an avatar capability: %d %v", resp.status, err)
	}
	if resp, _ = g.call(context.Background(), "/v1/uploads/staff-admit", staffGatewayAdmission{Ticket: envelope}); resp.status != 409 {
		t.Fatalf("capability admitted twice: %d", resp.status)
	}

	// A confirmation whose transaction rolled back (here its audit row fails) leaves no usable capability.
	mod3ID, mod3 := e.user("staff_cap_moderator3", "moderator")
	agExec(e, `CREATE FUNCTION staff_upload_audit_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action LIKE 'Authorized one avatar upload%' THEN RAISE EXCEPTION 'test audit unavailable'; END IF; RETURN NEW; END $$`)
	agExec(e, `CREATE TRIGGER staff_upload_audit_failure BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION staff_upload_audit_failure()`)
	rolled := form()
	rolled.Set("password", testPassword)
	if w = brandingAdminRequest(e, true, "POST", "/admin/uploads/authorize", mod3, rolled); w.Code < 500 {
		t.Fatalf("audit failure: %d", w.Code)
	}
	agExec(e, `DROP TRIGGER staff_upload_audit_failure ON audit_events`)
	e.check(brandingAdminRequest(e, true, "GET", "/admin/uploads/upload", mod3, nil), 409)
	e.A.staffUploads.mu.Lock()
	uncommitted := e.A.staffUploads.pending[mod3ID].token
	e.A.staffUploads.mu.Unlock()
	if uncommitted == "" {
		t.Fatal("expected the rolled-back capability to remain in memory for this check")
	}
	if resp, _ = g.call(context.Background(), "/v1/uploads/staff-admit", staffGatewayAdmission{Ticket: uncommitted}); resp.status != 409 {
		t.Fatalf("rolled-back capability admitted: %d", resp.status)
	}
}

func TestStaffListingPhotoViaGatewayCapability(t *testing.T) {
	e, f, g := staffUploadEnv(t)
	adminID, admin := e.user("staff_photo_admin", "admin")
	vendorID, _ := e.user("staff_photo_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	otherListing := e.product(vendorID, "physical")

	w := brandingAdminRequest(e, true, "GET", "/listing-edit?id="+listing, admin, nil)
	e.check(w, 200)
	photos := sectionOf(t, w.Body.String(), "photos")
	if !strings.Contains(photos, `action="/admin/uploads/authorize"`) || !strings.Contains(photos, `name="kind" value="photo"`) || !strings.Contains(photos, `name="id" value="`+listing+`"`) || !strings.Contains(photos, `name="password"`) || strings.Contains(photos, "multipart/form-data") || strings.Contains(photos, `type="file"`) {
		t.Fatalf("private photo panel:\n%s", photos)
	}
	form := func(id string) url.Values {
		return url.Values{"kind": {"photo"}, "id": {id}, formNonceField: {randomToken()[:32]}}
	}
	// A moderator cannot add listing photos; an unknown listing mints nothing.
	_, mod := e.user("staff_photo_moderator", "moderator")
	missing := form(randomToken()[:32])
	missing.Set("password", testPassword)
	e.check(brandingAdminRequest(e, true, "POST", "/admin/uploads/authorize", admin, missing), 404)
	modForm := form(listing)
	modForm.Set("password", testPassword)
	e.check(brandingAdminRequest(e, true, "POST", "/admin/uploads/authorize", mod, modForm), 403)

	page := staffAuthorize(t, e, admin, form(listing)).Body.String()
	if !strings.Contains(page, `name="description"`) || strings.Contains(page, `name="id"`) {
		t.Fatalf("photo handoff:\n%s", page)
	}
	ticket := staffTicket(t, page)
	original := metadataJPEG(t, 400, 300)
	w = uploadThroughGateway(g, staffUploadPath, "", part{name: "ticket", body: []byte(ticket)}, part{name: "description", body: []byte("Front of the box")}, part{name: "photo", filename: "x.jpg", ctype: "image/jpeg", body: original})
	checkPage(t, w, 200, "Photo saved")
	var owner, product, desc string
	var full []byte
	if err := e.DB.QueryRow(`SELECT owner_user_id,product_id,description,"full" FROM images WHERE kind='photo'`).Scan(&owner, &product, &desc, &full); err != nil {
		t.Fatal(err)
	}
	if owner != vendorID || product != listing || desc != "Front of the box" || e.imageCount() != 1 {
		t.Fatalf("photo owner=%s product=%s desc=%q", owner, product, desc)
	}
	assertCleanJPEG(t, "staff photo", full)
	if agInt(e, `SELECT count(*) FROM notifications WHERE user_id=$1`, vendorID) != 1 || agInt(e, `SELECT count(*) FROM images WHERE kind='avatar'`) != 0 {
		t.Fatal("vendor not notified, or the photo capability stored an avatar")
	}
	if got := e.audits(adminID); len(got) != 2 || !strings.HasPrefix(got[0], "Authorized one photo upload for listing "+listing[:8]) || !strings.HasPrefix(got[1], "Added a photo to listing") {
		t.Fatalf("audits %q", got)
	}
	scans := scansOf(f)
	if w = uploadThroughGateway(g, staffUploadPath, "", part{name: "ticket", body: []byte(ticket)}, part{name: "photo", body: original}); w.Code != 409 || scansOf(f) != scans {
		t.Fatalf("replayed photo capability: %d", w.Code)
	}

	// Bound to its listing: a gateway that names another listing is refused and nothing is stored.
	ticket = staffTicket(t, staffAuthorize(t, e, admin, form(listing)).Body.String())
	resp, err := g.call(context.Background(), "/v1/uploads/staff-admit", staffGatewayAdmission{Ticket: ticket})
	if err != nil || resp.status != 200 {
		t.Fatalf("admit %v %d", err, resp.status)
	}
	var admitted struct{ Ticket, Kind string }
	if json.Unmarshal(resp.body, &admitted) != nil || admitted.Kind != "photo" {
		t.Fatalf("admission %s", resp.body)
	}
	// A valid normalized photo envelope: the one the gateway produced for the stored photo.
	var thumb []byte
	var width, height int
	if err := e.DB.QueryRow(`SELECT thumb,width,height FROM images WHERE kind='photo'`).Scan(&thumb, &width, &height); err != nil {
		t.Fatal(err)
	}
	resp, err = g.call(context.Background(), "/v1/uploads/finish", uploadFinish{Ticket: admitted.Ticket, ID: otherListing, Full: full, Thumb: thumb, Width: width, Height: height})
	if err != nil || resp.status != 400 || e.imageCount() != 1 {
		t.Fatalf("other listing: %d %v", resp.status, err)
	}
	// An expired photo capability is refused before its file is read.
	ticket = staffTicket(t, staffAuthorize(t, e, admin, form(listing)).Body.String())
	e.A.staffUploads.mu.Lock()
	c := e.A.staffUploads.pending[adminID]
	c.expires = time.Now().Add(-time.Second)
	e.A.staffUploads.pending[adminID] = c
	e.A.staffUploads.mu.Unlock()
	if w = uploadThroughGateway(g, staffUploadPath, "", part{name: "ticket", body: []byte(ticket)}, part{name: "photo", body: original}); w.Code != 409 || scansOf(f) != scans {
		t.Fatalf("expired photo capability: %d", w.Code)
	}
}

// Where no upload path exists, the panels say why instead of showing a form that always fails.
func TestUploadFormHiddenWithoutPath(t *testing.T) {
	e, _, _ := staffUploadEnv(t)
	e.A.brandingUploadOrigin = ""
	adminID, admin := e.user("no_path_admin", "admin")
	vendorID, vendor := e.user("no_path_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	for _, tc := range []struct{ path, section string }{{"/account", "avatar"}, {"/listing-edit?id=" + listing, "photos"}} {
		w := brandingAdminRequest(e, true, "GET", tc.path, admin, nil)
		e.check(w, 200)
		s := sectionOf(t, w.Body.String(), tc.section)
		if !strings.Contains(s, "BRANDING_UPLOAD_ORIGIN") || strings.Contains(s, "<form class=\"stack upload-form\"") || strings.Contains(s, `type="file"`) || strings.Contains(s, "/admin/uploads/authorize") {
			t.Fatalf("%s without an upload path:\n%s", tc.path, s)
		}
	}
	w := brandingAdminRequest(e, true, "POST", "/admin/uploads/authorize", admin, url.Values{"kind": {"avatar"}, "password": {testPassword}})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "BRANDING_UPLOAD_ORIGIN") || len(e.audits(adminID)) != 0 {
		t.Fatalf("authorize without a path: %d", w.Code)
	}
	// A vendor on the public gateway still has the ordinary form.
	w = e.do("GET", "/account", vendor, nil)
	e.check(w, 200)
	assertUploadForm(t, sectionOf(t, w.Body.String(), "avatar"), "/account/avatar", vendor, e)

	// Single-host mode is unchanged: the private connection posts originals to the app itself.
	e.A.isolatedUploads = false
	e.withScanner(newFakeClamd(t))
	w = brandingAdminRequest(e, true, "GET", "/account", admin, nil)
	e.check(w, 200)
	assertUploadForm(t, sectionOf(t, w.Body.String(), "avatar"), "/account/avatar", admin, e)
	e.check(brandingAdminRequest(e, true, "POST", "/admin/uploads/authorize", admin, url.Values{"kind": {"avatar"}, "password": {testPassword}}), http.StatusConflict)
}

// A-590: in the separated topology the logo form follows the gateway's scanner report like the other upload forms.
func TestBrandingFollowsGatewayScannerReport(t *testing.T) {
	e, f, g := staffUploadEnv(t)
	_, admin := e.user("logo_report_admin", "admin")
	logo := func() string {
		w := brandingAdminRequest(e, true, "GET", "/admin/site", admin, nil)
		e.check(w, 200)
		return sectionOf(t, w.Body.String(), "branding")
	}
	if s := logo(); !strings.Contains(s, `action="/admin/branding/authorize"`) {
		t.Fatalf("fresh Ready report: logo form missing\n%s", s)
	}
	agExec(e, `UPDATE settings SET value=jsonb_set(value::jsonb,'{received}',to_jsonb(clock_timestamp()-$2::interval))::text WHERE key=$1`, settingGatewayScanner, (gatewayScannerStaleAfter + time.Minute).String())
	if s := logo(); strings.Contains(s, `action="/admin/branding/authorize"`) || !strings.Contains(s, "Logo uploads are paused") {
		t.Fatalf("stale report: logo form still offered\n%s", s)
	}
	w := brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", admin, url.Values{"password": {testPassword}})
	if w.Code != http.StatusServiceUnavailable || agInt(e, `SELECT count(*) FROM branding_upload_tickets`) != 0 {
		t.Fatalf("stale report: authorize %d", w.Code)
	}
	// The staff upload capability follows the same report.
	w = brandingAdminRequest(e, true, "POST", "/admin/uploads/authorize", admin, url.Values{"kind": {"avatar"}, "password": {testPassword}})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale report: staff authorize %d", w.Code)
	}
	// A fresh report again: the form returns. Then a fresh but unhealthy one (the gateway's signatures are stale).
	if err := g.reportScanner(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := logo(); !strings.Contains(s, `action="/admin/branding/authorize"`) {
		t.Fatalf("fresh Ready report again: logo form missing\n%s", s)
	}
	f.SetVersion(fakeClamdVersion(40 * 24 * time.Hour))
	g.scanner.poll(context.Background())
	if err := g.reportScanner(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := logo(); strings.Contains(s, `action="/admin/branding/authorize"`) || !strings.Contains(s, "Logo uploads are paused") {
		t.Fatalf("unhealthy report: logo form still offered\n%s", s)
	}
	e.check(brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", admin, url.Values{"password": {testPassword}}), http.StatusServiceUnavailable)
}
