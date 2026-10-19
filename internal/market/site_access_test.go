package market

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSiteAccessInitialConfiguration(t *testing.T) {
	for _, mode := range []string{"", "public", "members"} {
		for _, registrations := range []string{"", "open", "closed"} {
			getenv := func(key string) string {
				if key == "INITIAL_SITE_ACCESS" {
					return mode
				}
				if key == "INITIAL_REGISTRATIONS" {
					return registrations
				}
				return ""
			}
			got, err := initialSiteAccessFromEnv(getenv)
			if err != nil || got.mode != "public" && got.mode != "members" || got.registrations != "open" && got.registrations != "closed" {
				t.Fatalf("valid initial policy rejected: %#v %v", got, err)
			}
		}
	}
	for key, value := range map[string]string{"INITIAL_SITE_ACCESS": "members ", "INITIAL_REGISTRATIONS": "false"} {
		if _, err := initialSiteAccessFromEnv(func(k string) string {
			if k == key {
				return value
			}
			return ""
		}); err == nil || !strings.Contains(err.Error(), key) || strings.Contains(err.Error(), value) {
			t.Fatalf("invalid policy must name the key without echoing its value: %v", err)
		}
	}
}

func TestSiteAccessPolicyFailsClosed(t *testing.T) {
	for _, settings := range []map[string]string{nil, {}, {settingSiteAccess: "PUBLIC", settingRegistrations: "OPEN"}, {settingSiteAccess: "unknown", settingRegistrations: "unknown"}} {
		p := siteAccessFromSettings(settings)
		if !p.MembersOnly || p.RegistrationOpen {
			t.Fatalf("invalid or missing settings reopened access: %#v", p)
		}
	}
	p := siteAccessFromSettings(map[string]string{settingSiteAccess: "public", settingRegistrations: "open"})
	if p.MembersOnly || !p.RegistrationOpen {
		t.Fatal("explicit public/open policy was not honored")
	}
}

func TestSiteAccessImagesRequireLiveMembership(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("access_image_vendor", "vendor")
	_, buyer := e.user("access_image_buyer", "buyer")
	product := e.product(vendorID, "physical")
	photo := e.image(vendorID, product, "photo")
	if _, err := e.DB.Exec(`UPDATE settings SET value=CASE key WHEN 'site_access' THEN 'members' ELSE 'closed' END WHERE key IN ('site_access','registrations')`); err != nil {
		t.Fatal(err)
	}
	ref := e.notFound()
	for _, method := range []string{"GET", "HEAD"} {
		for _, token := range []string{"", randomToken()} {
			w := e.imgGet(method, publicImageURL(photo.id, false), sessionCookie(token))
			if w.Code != 404 || strings.Contains(w.Body.String(), photo.id) || w.Header().Get("Cache-Control") != "no-store" || len(w.Result().Cookies()) != 0 {
				t.Fatalf("unauthenticated image response leaked content: %d %v", w.Code, w.Header())
			}
			if method == "GET" {
				sameResponse(t, w, ref, "known image without a membership")
			}
		}
		w := e.imgGet(method, publicImageURL(photo.id, false), sessionCookie(buyer))
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || len(w.Result().Cookies()) != 0 || method == "HEAD" && w.Body.Len() != 0 {
			t.Fatalf("member image not served privately: %d %v", w.Code, w.Header())
		}
	}
	for _, state := range []string{"expired", "suspended", "closed"} {
		id, token := e.user("access_image_"+state, "buyer")
		switch state {
		case "expired":
			_, _ = e.DB.Exec(`UPDATE sessions SET expires=now()-interval '1 minute' WHERE user_id=$1`, id)
		case "suspended":
			_, _ = e.DB.Exec(`UPDATE users SET suspended_at=now() WHERE id=$1`, id)
		case "closed":
			_, _ = e.DB.Exec(`UPDATE users SET closed_at=now() WHERE id=$1`, id)
		}
		w := e.imgGet("GET", publicImageURL(photo.id, false), sessionCookie(token))
		sameResponse(t, w, ref, state+" membership")
	}
	// A members-only public image retains the ordinary published-image predicate.
	if _, err := e.DB.Exec(`UPDATE products SET archived=true WHERE id=$1`, product); err != nil {
		t.Fatal(err)
	}
	e.check(e.imgGet("GET", publicImageURL(photo.id, false), sessionCookie(buyer)), 404)
	w := e.desk(vendor, photo.id, false)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("owner desk image: %d %v", w.Code, w.Header())
	}
	if _, err := e.DB.Exec(`UPDATE products SET archived=false WHERE id=$1`, product); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`UPDATE settings SET value='public' WHERE key='site_access'`); err != nil {
		t.Fatal(err)
	}
	w = e.imgGet("GET", publicImageURL(photo.id, false))
	wantImage(t, w, photo.thumb, "public policy restored")
	if w.Header().Get("Cache-Control") != "private, max-age=86400, immutable" {
		t.Fatal("public image caching changed")
	}
}

func TestSiteAccessAnonymousImagesNeverReadImageBytes(t *testing.T) {
	e := newTestApp(t)
	owner, _ := e.user("access_blocked_image", "vendor")
	photo := e.image(owner, e.product(owner, "physical"), "photo")
	if _, err := e.DB.Exec(`UPDATE settings SET value='members' WHERE key='site_access'`); err != nil {
		t.Fatal(err)
	}
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`LOCK TABLE images IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("GET", publicImageURL(photo.id, false), nil).WithContext(ctx)
	for _, name := range []string{"X-Admin-Authenticated", "X-Forwarded-User", "X-Forwarded-For", "X-SSL-Client-Verify", "X-Client-Cert"} {
		r.Header.Set(name, "SUCCESS")
	}
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	if w.Code != 404 || ctx.Err() != nil {
		t.Fatalf("anonymous image reached locked image storage or trusted a header: %d %v", w.Code, ctx.Err())
	}
}

func TestSiteAccessPrivateAdministratorRemainsPrivate(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("access_private_admin", "admin")
	owner, _ := e.user("access_private_vendor", "vendor")
	photo := e.image(owner, e.product(owner, "physical"), "photo")
	if _, err := e.DB.Exec(`UPDATE settings SET value='members' WHERE key='site_access'`); err != nil {
		t.Fatal(err)
	}
	e.A.adminSocket = "/run/test-private-admin.sock"
	for _, path := range []string{"/admin", "/admin/security", "/admin/access", publicImageURL(photo.id, false)} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(sessionCookie(admin))
		r.Header.Set("X-Admin-Authenticated", "true")
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		e.check(w, 404)
	}
	r := httptest.NewRequest("GET", "/admin/security", nil)
	r.AddCookie(sessionCookie(admin))
	w := httptest.NewRecorder()
	e.A.AdminHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `action="/admin/access"`) {
		t.Fatalf("private admin setting unavailable: %d", w.Code)
	}
}

func TestSiteAccessAllContentGuardAndEssentialEndpoints(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("access_route_vendor", "vendor")
	_, member := e.user("access_route_member", "buyer")
	product := e.product(vendor, "physical")
	if _, err := e.DB.Exec(`UPDATE settings SET value=CASE key WHEN 'site_access' THEN 'members' ELSE 'closed' END WHERE key IN ('site_access','registrations')`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/catalog", "/product?id=" + product, "/vendor?id=" + vendor, "/canary"} {
		for _, method := range []string{"GET", "HEAD"} {
			r := httptest.NewRequest(method, path, nil)
			r.AddCookie(sessionCookie(randomToken()))
			r.Header.Set("X-Forwarded-User", "administrator")
			r.Header.Set("X-Admin-Authenticated", "true")
			w := httptest.NewRecorder()
			e.A.ServeHTTP(w, r)
			if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") || strings.Contains(w.Body.String(), "Test listing") {
				t.Fatalf("%s %s exposed guest content: %d", method, path, w.Code)
			}
		}
		e.check(e.do("GET", path, member, nil), http.StatusOK)
	}
	for path, code := range map[string]int{"/login": 200, "/register": 403, "/healthz": 200, "/static/style.css": 200, "/captcha?id=unknown": 404, "/unknown-access-page": 404} {
		e.check(e.do("GET", path, randomToken(), nil), code)
	}
	login := e.body("GET", "/login", randomToken(), nil, 200)
	for _, forbidden := range []string{`role="search"`, `aria-label="Main navigation"`, `href="/register"`, `class="footer-status"`} {
		if strings.Contains(login, forbidden) {
			t.Fatalf("member-only sign-in page exposes a closed content control %s", forbidden)
		}
	}
}

func TestSiteAccessClosedRegistrationSkipsEnabledCaptcha(t *testing.T) {
	e := newTestApp(t)
	e.user("access_captcha_admin", "admin")
	if _, err := e.DB.Exec(`UPDATE settings SET value=CASE key WHEN 'registrations' THEN 'closed' ELSE 'true' END WHERE key IN ('registrations','captcha_required')`); err != nil {
		t.Fatal(err)
	}
	before := agInt(e, `SELECT count(*) FROM captcha_spent`)
	for _, method := range []string{"GET", "POST"} {
		var form url.Values
		if method == "POST" {
			form = url.Values{"handle": {"access_captcha_new"}, "password": {testPassword}}
		}
		w := e.do(method, "/register", randomToken(), form)
		e.check(w, http.StatusForbidden)
		if strings.Contains(w.Body.String(), `action="/register"`) || strings.Contains(w.Body.String(), `captcha-image`) {
			t.Fatal("closed registration offered an account form or CAPTCHA")
		}
	}
	if agInt(e, `SELECT count(*) FROM captcha_spent`) != before || agInt(e, `SELECT count(*) FROM users WHERE handle='access_captcha_new'`) != 0 {
		t.Fatal("closed registration consumed CAPTCHA or created an account")
	}
	if login := e.body("GET", "/login", randomToken(), nil, 200); !strings.Contains(login, `captcha-image`) {
		t.Fatal("login CAPTCHA was incorrectly disabled")
	}
}
