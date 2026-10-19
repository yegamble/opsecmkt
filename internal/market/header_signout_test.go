package market

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var (
	headerSignOutForm = regexp.MustCompile(`(?s)<form[^>]*action="/logout"[^>]*>(.*?)</form>`)
	headerFormInput   = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)
)

// siteHeader is the layout's <header> element of a rendered page, or "" when the page has none.
func siteHeader(body string) string {
	start := strings.Index(body, "<header>")
	end := strings.Index(body, "</header>")
	if start < 0 || end < start {
		return ""
	}
	return body[start : end+len("</header>")]
}

// withoutSiteHeader is body with its <header> removed: what a page offers besides the header's sign-out form (A-548).
func withoutSiteHeader(body string) string {
	return strings.Replace(body, siteHeader(body), "", 1)
}

// sessionNeutralBody is w's body with session's own CSRF token (the header sign-out form's, A-548) replaced, so two
// sessions' refusals compare equal when they differ only in the token each browser already holds.
func sessionNeutralBody(e *testEnv, w *httptest.ResponseRecorder, session string) string {
	return strings.ReplaceAll(w.Body.String(), e.A.csrf(session), "SESSION-CSRF")
}

// headerSignOut checks that header carries exactly one sign-out form, a plain POST to /logout with the CSRF token
// csrf (any token when csrf is "") and a "Sign out" button after the handle and before the main navigation, and returns the fields it submits.
func headerSignOut(t *testing.T, path, header, csrf string) url.Values {
	t.Helper()
	forms := headerSignOutForm.FindAllStringSubmatch(header, -1)
	if len(forms) != 1 {
		t.Fatalf("%s: the header has %d sign-out forms, want 1:\n%s", path, len(forms), header)
	}
	form := forms[0][0]
	if !strings.Contains(form, `method="post"`) || !strings.Contains(form, `name="csrf" value="`+csrf) || !strings.Contains(form, `<button type="submit"`) || !strings.Contains(form, `>Sign out</button>`) {
		t.Fatalf("%s: the header sign-out form lost POST, CSRF or its button: %s", path, form)
	}
	// Focus order: search (when shown), the handle, Sign out, then the main navigation.
	search, handle, signOut, nav := strings.Index(header, `id="header-search"`), strings.Index(header, `class="account-name"`), strings.Index(header, form), strings.Index(header, `aria-label="Main navigation"`)
	if handle < 0 || signOut < handle || nav >= 0 && nav < signOut || search >= 0 && search > handle {
		t.Fatalf("%s: header order search=%d handle=%d sign-out=%d nav=%d", path, search, handle, signOut, nav)
	}
	fields := url.Values{}
	for _, m := range headerFormInput.FindAllStringSubmatch(forms[0][1], -1) {
		fields.Add(m[1], html.UnescapeString(m[2]))
	}
	return fields
}

// headerPost submits fields as the browser would send the header form, with only the session cookie.
func headerPost(h http.Handler, path, session string, fields url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(fields.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: session})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// A-548: every signed-in page offers Sign out next to the handle in the site header; anonymous pages do not.
func TestHeaderSignOutFormOnSignedInPages(t *testing.T) {
	e := newTestApp(t)
	id, session := e.user("a548_header", "buyer")
	var fields url.Values
	for _, path := range []string{"/", "/account", "/orders", "/messages", "/notifications", "/disputes", "/canary"} {
		w := e.do("GET", path, session, nil)
		e.check(w, 200)
		fields = headerSignOut(t, path, siteHeader(w.Body.String()), e.A.csrf(session))
		if path == "/account" && len(headerSignOutForm.FindAllString(w.Body.String(), -1)) != 2 {
			t.Fatal("/account no longer keeps its own sign-out form beside the header's")
		}
	}
	for _, path := range []string{"/", "/login", "/register", "/canary"} {
		w := e.do("GET", path, randomToken(), nil)
		e.check(w, 200)
		if strings.Contains(w.Body.String(), `action="/logout"`) {
			t.Fatalf("anonymous %s offers a sign-out form", path)
		}
	}

	// The header form, posted as rendered, ends this session and lands on the sign-in page's notice.
	if len(fields) != 1 || fields.Get("csrf") == "" {
		t.Fatalf("the header form submits %v, want only csrf", fields)
	}
	w := headerPost(e.A, "/logout", session, fields)
	e.check(w, 303)
	c := noticeSetCookie(w)
	if w.Header().Get("Location") != "/login" || c == nil || c.Value != "signed-out" || !agClearedCookie(w, "session") {
		t.Fatalf("header sign-out: Location=%q notice=%+v headers=%v", w.Header().Get("Location"), c, w.Header())
	}
	if agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1", digest(session)) != 0 || !e.auditHas(id, "Signed out") {
		t.Fatal("the header sign-out left its session or was not audited")
	}
	r := httptest.NewRequest("GET", "/login", nil)
	r.AddCookie(c)
	login := httptest.NewRecorder()
	e.A.ServeHTTP(login, r)
	e.check(login, 200)
	if !strings.Contains(login.Body.String(), `role="status">Signed out.</div>`) || strings.Contains(login.Body.String(), `action="/logout"`) {
		t.Fatal("/login after the header sign-out lacks the notice or still offers sign-out")
	}
	if w := e.do("GET", "/account", session, nil); w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Fatalf("the signed-out session still opens /account: %d %s", w.Code, w.Header().Get("Location"))
	}
}

// A-548: the private administrator ingress renders the same header sign-out, and it signs the administrator out.
func TestHeaderSignOutOnPrivateAdminIngress(t *testing.T) {
	e := newTestApp(t)
	admin := e.setup()
	e.A.adminSocket = "/run/market-admin/http.sock"
	r := httptest.NewRequest("GET", "/admin", nil)
	r.AddCookie(&http.Cookie{Name: "session", Value: admin})
	w := httptest.NewRecorder()
	e.A.AdminHandler().ServeHTTP(w, r)
	e.check(w, 200)
	fields := headerSignOut(t, "private /admin", siteHeader(w.Body.String()), e.A.csrf(admin))
	w = headerPost(e.A.AdminHandler(), "/logout", admin, fields)
	e.check(w, 303)
	if w.Header().Get("Location") != "/login" || agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1", digest(admin)) != 0 {
		t.Fatalf("private header sign-out: Location=%q", w.Header().Get("Location"))
	}
}

// A-548: the read-only preview shows the header sign-out like its other forms and refuses the post with its notice.
func TestPreviewHeaderSignOutIsRefusedReadOnly(t *testing.T) {
	a := previewTestApp(t)
	r := httptest.NewRequest("GET", "/account", nil)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("preview /account: %d", w.Code)
	}
	fields := headerSignOut(t, "preview /account", siteHeader(w.Body.String()), "")
	w = headerPost(a, "/logout", "", fields)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Read-only preview. Start with PostgreSQL to save changes.") {
		t.Fatalf("preview header sign-out: %d %s", w.Code, w.Body)
	}
}
