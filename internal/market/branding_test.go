package market

import (
	"bytes"
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

const simpleBrandingSVG = `<svg xmlns="http://www.w3.org/2000/svg" width="400" height="100" viewBox="0 0 400 100" role="img" aria-labelledby="title"><title id="title">Example &amp; Market</title><g fill="#f59e0b" transform="translate(5 5) scale(1 -1)"><path d="M10 20L30 40H50V60Z"/></g><text x="60" y="50" font-family="monospace" font-weight="700">MARKET</text></svg>`

func TestBrandingSVGStrictInertCanonicalization(t *testing.T) {
	clean, width, height, err := normalizeBrandingSVG([]byte(simpleBrandingSVG))
	if err != nil || width != 400 || height != 100 || !bytes.Contains(clean, []byte(`Example &amp; Market`)) {
		t.Fatalf("simple SVG refused: dimensions=%dx%d error=%v", width, height, err)
	}
	again, w, h, err := normalizeBrandingSVG(clean)
	if err != nil || w != width || h != height || !bytes.Equal(clean, again) {
		t.Fatalf("canonical SVG not stable: %v", err)
	}
	for _, bad := range []string{
		`<script>alert(1)</script>`, `<style>path{fill:url(https://example.invalid)}</style>`,
		`<foreignObject><p xmlns="http://www.w3.org/1999/xhtml">html</p></foreignObject>`,
		`<image href="data:image/png;base64,aA=="/>`, `<use href="#title"/>`,
		`<a href="https://example.invalid"><path d="M0 0L1 1"/></a>`, `<animate attributeName="x"/>`,
		`<path onload="alert(1)" d="M0 0"/>`, `<path style="fill:red" d="M0 0"/>`,
		`<path fill="url(#a)" d="M0 0"/>`, `<path d="M0 0L1e999999 1"/>`,
		`<path d="M0 0L100001 1"/>`, `<g transform="scale(1e999999)"></g>`,
		`<polyline points="0 0 1e999999 1"/>`, `<path stroke-dasharray="1e999999 1" d="M0 0"/>`,
		`<path xmlns="http://example.invalid" d="M0 0"/>`, `<path fill="red" fill="blue" d="M0 0"/>`,
		`<title id="duplicate">one</title><title id="duplicate">two</title>`,
		`<path xmlns:xlink="http://www.w3.org/1999/xlink" xlink:href="https://example.invalid"/>`,
	} {
		input := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 400 100">` + bad + `</svg>`
		if _, _, _, err := normalizeBrandingSVG([]byte(input)); err == nil {
			t.Errorf("unsafe or unbounded SVG accepted: %s", bad)
		}
	}
	for _, bad := range []string{
		`<!DOCTYPE svg [<!ENTITY x "entity">]><svg viewBox="0 0 1 1"><text>&x;</text></svg>`,
		`<?xml-stylesheet href="https://example.invalid"?><svg viewBox="0 0 1 1"/>`,
		`<?xml version="1.0"?><svg viewBox="0 0 1 1"/>`,
		`<svg viewBox="0 0 -1 100"/>`, `<svg viewBox="0 0 1e999999 100"/>`,
		`<svg width="0" height="100"/>`, `<svg viewBox="0 0 100 100"/><svg viewBox="0 0 1 1"/>`,
		`<svg viewBox="0 0 100 100">` + strings.Repeat(`<g>`, 34) + strings.Repeat(`</g>`, 34) + `</svg>`,
		`<svg viewBox="0 0 100 100">` + strings.Repeat(`<path d="M0 0"/>`, 1024) + `</svg>`,
		`<svg viewBox="0 0 100 100"><path d="M` + strings.Repeat(`1 1 `, 1025) + `"/></svg>`,
		`<svg viewBox="0 0 100 100"><path d="M0 0` + strings.Repeat(`z`, 2049) + `"/></svg>`,
		`<svg viewBox="0 0 100 100"><text font-size="513">large</text></svg>`,
		`<svg viewBox="0 0 100 100"><text>` + strings.Repeat(`a`, 4097) + `</text></svg>`,
		strings.Repeat(" ", brandingSVGCap+1),
	} {
		if _, _, _, err := normalizeBrandingSVG([]byte(bad)); err == nil {
			t.Errorf("invalid or complex SVG accepted (length %d)", len(bad))
		}
	}
	large := `<svg width="100000" height="50000"><rect width="100000" height="50000" fill="#000"/></svg>`
	clean, width, height, err = normalizeBrandingSVG([]byte(large))
	if err != nil || width != 1200 || height != 600 || !bytes.Contains(clean, []byte(`width="1200"`)) || !bytes.Contains(clean, []byte(`viewBox="0 0 100000 50000"`)) {
		t.Fatalf("large viewport did not normalize: %dx%d error=%v", width, height, err)
	}
}

func brandingAdminRequest(e *testEnv, private bool, method, path, session string, form url.Values) *httptest.ResponseRecorder {
	e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if method == "POST" {
		if !form.Has("csrf") {
			form.Set("csrf", e.A.csrf(session))
		}
		if !form.Has(formNonceField) {
			form.Set(formNonceField, formNonceFor(path))
		}
	}
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: session})
	w := httptest.NewRecorder()
	if private {
		e.A.AdminHandler().ServeHTTP(w, r)
	} else {
		e.A.ServeHTTP(w, r)
	}
	return w
}

func brandingAuthorize(t *testing.T, e *testEnv, session string) string {
	t.Helper()
	w := brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", session, url.Values{"password": {testPassword}})
	e.check(w, 303)
	if w.Header().Get("Location") != "/admin/branding/upload" {
		t.Fatal("capability appeared in redirect")
	}
	w = brandingAdminRequest(e, true, "GET", "/admin/branding/upload", session, nil)
	e.check(w, 200)
	m := regexp.MustCompile(`name="ticket" value="([0-9a-f]{64})"`).FindStringSubmatch(w.Body.String())
	if len(m) != 2 {
		t.Fatal("handoff lacks capability")
	}
	return m[1]
}

func TestBrandingPrivateAuthorizationHandoffAndPublicLogo(t *testing.T) {
	e := newTestApp(t)
	f := newFakeClamd(t)
	e.A.scanner = readyScanner(t, f)
	adminID, admin := e.user("branding_admin", "admin")
	e.A.adminSocket = "/run/market-admin/http.sock"
	e.A.brandingUploadOrigin = "https://upload.example"
	for _, role := range []string{"buyer", "vendor", "moderator"} {
		_, session := e.user("branding_"+role, role)
		e.check(brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", session, url.Values{"password": {testPassword}}), 403)
	}
	e.check(brandingAdminRequest(e, false, "POST", "/admin/branding/authorize", admin, url.Values{"password": {testPassword}}), 404)
	e.check(brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", admin, url.Values{"password": {"incorrect-password"}}), 401)
	e.check(brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", admin, url.Values{"password": {testPassword}, "csrf": {"forged"}}), 403)
	if agInt(e, "SELECT count(*) FROM branding_upload_tickets") != 0 {
		t.Fatal("refusal minted ticket")
	}
	form := url.Values{"password": {testPassword}, formNonceField: {randomToken()[:32]}}
	w := brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", admin, form)
	e.check(w, 303)
	var originalHash string
	if err := e.DB.QueryRow(`SELECT token_hash FROM branding_upload_tickets WHERE user_id=$1`, adminID).Scan(&originalHash); err != nil {
		t.Fatal(err)
	}
	e.check(brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", admin, form), 303)
	if agStr(e, `SELECT token_hash FROM branding_upload_tickets`) != originalHash || len(e.audits(adminID)) != 1 {
		t.Fatal("exact replay extended/replaced authorization")
	}
	w = brandingAdminRequest(e, true, "GET", "/admin/branding/upload", admin, nil)
	e.check(w, 200)
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "form-action 'self' https://upload.example;") || w.Header().Get("Referrer-Policy") != "no-referrer" || !strings.Contains(w.Body.String(), `action="https://upload.example/branding/logo/upload"`) || strings.Contains(w.Body.String(), admin) || strings.Contains(w.Body.String(), testPassword) || strings.Contains(w.Body.String(), `name="csrf"`) || strings.Contains(w.Body.String(), `name="password"`) || strings.Contains(w.Body.String(), `name="code"`) {
		t.Fatal("handoff leaked credentials or lost narrow CSP")
	}
	ticket := regexp.MustCompile(`name="ticket" value="([0-9a-f]{64})"`).FindStringSubmatch(w.Body.String())[1]
	if err := e.A.admitBranding(t.Context(), ticket); err != nil {
		t.Fatal(err)
	}
	if err := e.A.admitBranding(t.Context(), ticket); err == nil {
		t.Fatal("capability admitted twice")
	}
	clean, wid, hei, err := normalizeBrandingSVG([]byte(simpleBrandingSVG))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.A.finishBranding(t.Context(), ticket, normalizedBranding{"image/svg+xml", clean, wid, hei}, false); err != nil {
		t.Fatal(err)
	}
	view, err := e.A.loadBranding(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, `UPDATE settings SET value='members' WHERE key='site_access'`)
	for _, method := range []string{"GET", "HEAD"} {
		w = e.do(method, view.LogoURL, "", nil)
		e.check(w, 200)
		if w.Header().Get("Content-Type") != "image/svg+xml" || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") || w.Header().Get("X-Content-Type-Options") != "nosniff" || len(w.Result().Cookies()) != 0 || method == "HEAD" && w.Body.Len() != 0 {
			t.Fatalf("unsafe logo response %s", method)
		}
	}
	w = e.do("GET", "/login", "", nil)
	e.check(w, 200)
	if !strings.Contains(w.Body.String(), view.LogoURL) {
		t.Fatal("members-only login lost public logo")
	}
	if err := e.A.finishBranding(t.Context(), ticket, normalizedBranding{"image/svg+xml", clean, wid, hei}, false); err == nil {
		t.Fatal("consumed capability saved twice")
	}
	e.check(brandingAdminRequest(e, true, "POST", "/admin/branding/reset", admin, url.Values{"password": {testPassword}}), 303)
	e.check(e.do("GET", view.LogoURL, "", nil), 404)
	if agInt(e, `SELECT count(*) FROM market_branding`) != 0 || agInt(e, `SELECT count(*) FROM images`) != 0 {
		t.Fatal("reset retained logo or mixed listing images")
	}
}

func TestBrandingIsolatedGatewayNormalizationAndRevocation(t *testing.T) {
	e := newTestApp(t)
	f := newFakeClamd(t)
	g := uploadTestGateway(t, e, f)
	adminID, admin := e.user("branding_gateway_admin", "admin")
	e.A.adminSocket = "/run/market-admin/http.sock"
	e.A.brandingUploadOrigin = "https://upload.example"
	for _, tc := range []struct {
		name string
		data []byte
		mime string
	}{
		{"jpeg", metadataJPEG(t, 1600, 300), "image/jpeg"},
		{"png", fixturePNG(t, fixturePicture(100, 1600)), "image/jpeg"},
		{"svg", []byte(simpleBrandingSVG), "image/svg+xml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ticket := brandingAuthorize(t, e, admin)
			r, body := uploadReq(brandingUploadPath, "", part{name: "ticket", body: []byte(ticket)}, part{name: "logo", filename: "ignored.exe", ctype: "text/html", body: tc.data})
			w := httptest.NewRecorder()
			e.A.ServeHTTP(w, r)
			if w.Code != 415 || body.read() != 0 {
				t.Fatal("original entered isolated application")
			}
			w = uploadThroughGateway(g, brandingUploadPath, "", part{name: "ticket", body: []byte(ticket)}, part{name: "logo", filename: "ignored.exe", ctype: "text/html", body: tc.data})
			e.check(w, 200)
			var mime string
			var data []byte
			var width, height int
			if err := e.DB.QueryRow(`SELECT content_type,body,width,height FROM market_branding`).Scan(&mime, &data, &width, &height); err != nil {
				t.Fatal(err)
			}
			if mime != tc.mime || width > 1200 || height > 1200 || agInt(e, `SELECT count(*) FROM branding_upload_tickets`) != 0 {
				t.Fatal("normalization or one-use capability failed")
			}
			if mime == "image/jpeg" {
				assertCleanJPEG(t, "branding raster", data)
			}
			if !bytes.Equal(f.Scans()[scansOf(f)-1], tc.data) {
				t.Fatal("scanner did not scan original")
			}
		})
	}
	for _, change := range []string{"expire", "revoke", "demote", "suspend"} {
		t.Run(change, func(t *testing.T) {
			agExec(e, `UPDATE users SET role='admin',suspended_at=NULL WHERE id=$1`, adminID)
			agExec(e, `INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '1 day') ON CONFLICT(token_hash) DO UPDATE SET expires=excluded.expires`, digest(admin), adminID)
			ticket := brandingAuthorize(t, e, admin)
			if err := e.A.admitBranding(t.Context(), ticket); err != nil {
				t.Fatal(err)
			}
			before := agStr(e, `SELECT version FROM market_branding`)
			switch change {
			case "expire":
				agExec(e, `UPDATE branding_upload_tickets SET expires=now()-interval '1 second'`)
			case "revoke":
				agExec(e, `DELETE FROM sessions WHERE token_hash=$1`, digest(admin))
			case "demote":
				agExec(e, `UPDATE users SET role='buyer' WHERE id=$1`, adminID)
			case "suspend":
				agExec(e, `UPDATE users SET suspended_at=now() WHERE id=$1`, adminID)
			}
			clean, wid, hei, err := normalizeBrandingSVG([]byte(simpleBrandingSVG))
			if err != nil {
				t.Fatal(err)
			}
			if err = e.A.finishBranding(t.Context(), ticket, normalizedBranding{"image/svg+xml", clean, wid, hei}, false); err == nil {
				t.Fatal("changed actor/session/capability was accepted")
			}
			if agStr(e, `SELECT version FROM market_branding`) != before {
				t.Fatal("revoked capability changed logo")
			}
		})
	}
}

func TestBrandingGatewayRefusalsAndAtomicAudit(t *testing.T) {
	e := newTestApp(t)
	f := newFakeClamd(t)
	g := uploadTestGateway(t, e, f)
	_, admin := e.user("branding_failure_admin", "admin")
	e.A.adminSocket = "/run/market-admin/http.sock"
	e.A.brandingUploadOrigin = "https://upload.example"
	before := scansOf(f)
	w := uploadThroughGateway(g, brandingUploadPath, "", part{name: "ticket", body: []byte(randomToken())}, part{name: "logo", filename: "x.svg", body: []byte(simpleBrandingSVG)})
	if w.Code != 409 || scansOf(f) != before {
		t.Fatal("unknown ticket reached scanner")
	}
	ticket := brandingAuthorize(t, e, admin)
	w = uploadThroughGateway(g, brandingUploadPath, "", part{name: "ticket", body: []byte(ticket)}, part{name: "logo", filename: "x.svg", body: []byte(`<svg viewBox="0 0 10 10"><script>alert(1)</script></svg>`)})
	if w.Code != 415 || agInt(e, `SELECT count(*) FROM market_branding`) != 0 || agInt(e, `SELECT count(*) FROM branding_upload_tickets`) != 0 {
		t.Fatal("active SVG changed branding or kept admission")
	}
	ticket = brandingAuthorize(t, e, admin)
	if err := e.A.admitBranding(t.Context(), ticket); err != nil {
		t.Fatal(err)
	}
	clean, wid, hei, err := normalizeBrandingSVG([]byte(simpleBrandingSVG))
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, `CREATE FUNCTION branding_audit_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action LIKE 'Replaced marketplace logo%' THEN RAISE EXCEPTION 'test audit unavailable'; END IF; RETURN NEW; END $$`)
	agExec(e, `CREATE TRIGGER branding_audit_failure BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION branding_audit_failure()`)
	if err = e.A.finishBranding(t.Context(), ticket, normalizedBranding{"image/svg+xml", clean, wid, hei}, false); err == nil {
		t.Fatal("audit failure was ignored")
	}
	if agInt(e, `SELECT count(*) FROM market_branding`) != 0 || agInt(e, `SELECT count(*) FROM branding_upload_tickets WHERE admitted`) != 1 {
		t.Fatal("failed transaction partially consumed or saved")
	}
	agExec(e, `DROP TRIGGER branding_audit_failure ON audit_events`)
	if err = e.A.finishBranding(t.Context(), ticket, normalizedBranding{"image/svg+xml", clean, wid, hei}, false); err != nil {
		t.Fatal(err)
	}
	// Public JSON and forged private headers never select the authenticated backend.
	body, _ := json.Marshal(brandingGatewayAdmission{Ticket: randomToken()})
	r := httptest.NewRequest("POST", "/v1/uploads/branding-admit", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tor-Onion-Role", "admin")
	w = httptest.NewRecorder()
	e.A.UploadBackendHandler().ServeHTTP(w, r)
	e.check(w, 404)
}

func TestBrandingTicketExpiryDoesNotRenewOnReplayAndRetentionCleans(t *testing.T) {
	e := newTestApp(t)
	e.A.scanner = readyScanner(t, newFakeClamd(t))
	_, admin := e.user("branding_expiry_admin", "admin")
	e.A.adminSocket = "/run/market-admin/http.sock"
	form := url.Values{"password": {testPassword}, formNonceField: {randomToken()[:32]}}
	e.check(brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", admin, form), 303)
	agExec(e, `UPDATE branding_upload_tickets SET expires=now()-interval '1 second'`)
	e.check(brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", admin, form), 303)
	e.check(brandingAdminRequest(e, true, "GET", "/admin/branding/upload", admin, nil), 409)
	tx, err := e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = retentionAuthSweep(t.Context(), tx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM branding_upload_tickets`) != 0 {
		t.Fatal("expired sealed capability retained after auth sweep")
	}
	e.check(brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", admin, url.Values{"password": {testPassword}}), 303)
	agExec(e, `UPDATE branding_upload_tickets SET expires=now()-interval '1 second'`)
	if err := e.A.sweepBrandingTickets(t.Context()); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM branding_upload_tickets`) != 0 {
		t.Fatal("ordinary background cleanup retained expired capability")
	}
}

func TestBrandingAuthorizationRequiresCurrentTOTPAndExactNonce(t *testing.T) {
	e := newTestApp(t)
	e.A.scanner = readyScanner(t, newFakeClamd(t))
	adminID, admin := e.user("branding_totp_admin", "admin")
	e.A.adminSocket = "/run/market-admin/http.sock"
	key, _ := agEnableTOTP(e, adminID)
	form := url.Values{"password": {testPassword}, formNonceField: {randomToken()[:32]}}
	e.check(brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", admin, form), 401)
	if agInt(e, `SELECT count(*) FROM branding_upload_tickets`) != 0 || len(e.audits(adminID)) != 0 {
		t.Fatal("missing TOTP changed state")
	}
	form.Set("code", agTOTPNow(key))
	e.check(brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", admin, form), 303)
	hash := agStr(e, `SELECT token_hash FROM branding_upload_tickets`)
	e.check(brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", admin, form), 303)
	if agStr(e, `SELECT token_hash FROM branding_upload_tickets`) != hash || len(e.audits(adminID)) != 1 {
		t.Fatal("TOTP replay minted a new authorization")
	}
	form.Set("extra", "changed-body")
	e.check(brandingAdminRequest(e, true, "POST", "/admin/branding/authorize", admin, form), 409)
}
