package market

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// noticeSetCookie returns the notice Set-Cookie on w, or nil.
func noticeSetCookie(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == noticeCookie {
			return c
		}
	}
	return nil
}

// A-117: a successful signed-in POST's 303 sets a one-shot notice cookie holding a code from the fixed table; the
// first page render shows its text (and prefixes the title) and clears the cookie; nothing else sets it, and a
// forged or unknown code shows nothing.
func TestNoticeCookieRoundTrip(t *testing.T) {
	e := newTestApp(t)
	_, sess := e.user("notice_user", "buyer")
	change := func(sess, repeat string) *httptest.ResponseRecorder {
		return e.do("POST", "/account/password", sess, url.Values{"password": {testPassword}, "new_password": {"notice-new-password-1"}, "new_password_confirm": {repeat}})
	}

	// A refused POST sets no notice.
	w := change(sess, "notice-new-password-typo")
	e.check(w, 400)
	if c := noticeSetCookie(w); c != nil {
		t.Fatalf("refused POST set a notice cookie: %v", c)
	}

	// The successful POST's 303 sets it: the code only, HttpOnly, SameSite=Strict, Path=/, 60 s, Secure off here.
	w = change(sess, "notice-new-password-1")
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/account" {
		t.Fatalf("Location=%q, want /account (no URL flag)", loc)
	}
	c := noticeSetCookie(w)
	if c == nil || c.Value != "password" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge != noticeMaxAge || c.Secure {
		t.Fatalf("notice cookie = %+v", c)
	}
	sess = e.session(w)

	// The first render shows the text inside the page and in the title, and clears the cookie.
	w = e.do("GET", "/account", sess, nil, &http.Cookie{Name: noticeCookie, Value: c.Value})
	e.check(w, 200)
	body := w.Body.String()
	if !strings.Contains(body, `<div class="alert" role="status">Password changed.</div>`) || !strings.Contains(body, "<title>Password changed. – Account · ") {
		t.Fatalf("first render lacks the notice or its title prefix:\n%s", body[:min(len(body), 1500)])
	}
	if cl := noticeSetCookie(w); cl == nil || cl.MaxAge >= 0 || cl.Value != "" {
		t.Fatalf("first render did not clear the notice cookie: %+v", cl)
	}

	// The next render (the browser dropped the cookie) shows nothing and sets nothing.
	w = e.do("GET", "/account", sess, nil)
	e.check(w, 200)
	if strings.Contains(w.Body.String(), "Password changed.") || noticeSetCookie(w) != nil {
		t.Fatal("second render repeated the notice or touched its cookie")
	}

	// A forged or unknown code shows nothing (never its value), and is cleared all the same.
	for _, forged := range []string{"forged", "Password changed.", "<b>x</b>", "PASSWORD", "password "} {
		w = e.do("GET", "/account", sess, nil, &http.Cookie{Name: noticeCookie, Value: forged})
		e.check(w, 200)
		b := w.Body.String()
		if strings.Contains(b, `role="status"`) || strings.Contains(b, "Password changed.") || strings.Contains(b, "forged") {
			t.Fatalf("forged code %q rendered a notice", forged)
		}
		if cl := noticeSetCookie(w); cl == nil || cl.MaxAge >= 0 {
			t.Fatalf("forged code %q was not cleared", forged)
		}
	}

	// A render that fails still clears it.
	w = e.do("GET", "/order?id=nope", sess, nil, &http.Cookie{Name: noticeCookie, Value: "password"})
	e.check(w, 404)
	if cl := noticeSetCookie(w); cl == nil || cl.MaxAge >= 0 {
		t.Fatal("a failed render did not clear the notice cookie")
	}

	// A cookieless request (a cross-site link withholds SameSite=Strict cookies, A-48) gets no notice cookie.
	r := httptest.NewRequest("GET", "/account", nil)
	w = httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	if noticeSetCookie(w) != nil {
		t.Fatal("a cookieless request touched the notice cookie")
	}

	// A signed-in form served as the anonymous request it was rendered for (A-48) is not signed in: no notice.
	anon := randomToken()
	form := url.Values{"password": {testPassword}, "new_password": {"notice-new-password-2"}, "new_password_confirm": {"notice-new-password-2"}, "csrf": {e.A.csrf(anon)}}
	r = httptest.NewRequest("POST", "/account/password", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: sess})
	r.AddCookie(&http.Cookie{Name: "anon", Value: anon})
	w = httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	if w.Code == 303 || noticeSetCookie(w) != nil {
		t.Fatalf("anonymous-served POST: status %d, notice %v", w.Code, noticeSetCookie(w))
	}

	// Sign-out: its 303 goes to /login, which reads the notice without a session.
	w = e.do("POST", "/logout", sess, nil)
	e.check(w, 303)
	c = noticeSetCookie(w)
	if w.Header().Get("Location") != "/login" || c == nil || c.Value != "signed-out" {
		t.Fatalf("sign-out: Location=%q notice=%+v", w.Header().Get("Location"), c)
	}
	r = httptest.NewRequest("GET", "/login", nil)
	r.AddCookie(&http.Cookie{Name: "anon", Value: anon})
	r.AddCookie(c)
	w = httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	e.check(w, 200)
	if !strings.Contains(w.Body.String(), `role="status">Signed out.</div>`) || !strings.Contains(w.Body.String(), "<title>Signed out. – Login · ") {
		t.Fatal("/login after sign-out lacks the notice")
	}

	// Secure mirrors the session cookie's.
	e.A.secure = true
	_, sess2 := e.user("notice_user2", "buyer")
	w = e.do("POST", "/account/password", sess2, url.Values{"password": {testPassword}, "new_password": {"notice-new-password-3"}, "new_password_confirm": {"notice-new-password-3"}})
	e.check(w, 303)
	if c = noticeSetCookie(w); c == nil || !c.Secure {
		t.Fatalf("notice cookie not Secure on a Secure market: %+v", c)
	}
}

// A section notice lands in its section: the 303 names the section's fragment and the render puts the text there
// (not at the top of the page).
func TestNoticeRendersInItsSection(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("notice_admin", "admin")
	e.user("notice_target", "buyer")
	w := e.do("POST", "/admin/suspend", admin, url.Values{"handle": {"notice_target"}, "action": {"suspend"}, "password": {testPassword}})
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/admin/users#suspend" {
		t.Fatalf("Location=%q", loc)
	}
	c := noticeSetCookie(w)
	if c == nil || c.Value != "suspended" {
		t.Fatalf("notice=%+v", c)
	}
	w = e.do("GET", "/admin/users", admin, nil, c)
	e.check(w, 200)
	b := w.Body.String()
	section := b[strings.Index(b, `id="suspend"`):]
	section = section[:strings.Index(section, "</section>")]
	if !strings.Contains(section, `role="status">Account suspended.</div>`) || strings.Count(b, "Account suspended.") != 2 { // the section and the title
		t.Fatalf("suspend notice not (only) in its section:\n%s", section)
	}
	if !strings.Contains(b, "<title>Account suspended. – Admin: Users · ") {
		t.Fatal("title not prefixed")
	}
	// A-542: consumed on another administration section, it renders at the top of that section once.
	w = e.do("GET", "/admin/system", admin, nil, c)
	e.check(w, 200)
	b = w.Body.String()
	if !strings.Contains(b, `<div class="alert" role="status">Account suspended.</div>`) || strings.Count(b, "Account suspended.") != 2 {
		t.Fatalf("suspend notice on /admin/system not shown at the top:\n%s", b)
	}

	// A-291: consumed on a page without its section (the 303 unfollowed and /login loaded in another tab), the
	// notice renders at the top of that page, once, as well as in the title.
	w = e.do("GET", "/login", "", nil, c)
	e.check(w, 200)
	b = w.Body.String()
	if !strings.Contains(b, `<div class="alert" role="status">Account suspended.</div>`) || strings.Count(b, "Account suspended.") != 2 || !strings.Contains(b, "<title>Account suspended. – ") {
		t.Fatalf("suspend notice on /login not shown at the top:\n%s", b)
	}

	// Retired pooled actions cannot show a success through an old notice cookie.
	for _, code := range []string{"payout-released", "payout-address", "payment-address", "intake-enabled", "address-warning"} {
		w = e.do("GET", "/admin", admin, nil, &http.Cookie{Name: noticeCookie, Value: code})
		e.check(w, 200)
		if strings.Contains(w.Body.String(), `<div class="alert" role="status">Payout released.</div>`) || strings.Contains(w.Body.String(), "Payment address ready.") {
			t.Fatalf("retired notice %q rendered a success", code)
		}
	}

	// A failing form's page says Error in its title.
	w = e.do("POST", "/admin/suspend", admin, url.Values{"handle": {"notice_nobody"}, "action": {"suspend"}, "password": {testPassword}})
	e.check(w, 404)
	if !strings.Contains(w.Body.String(), "<title>Error: Admin: Users · ") {
		t.Fatal("error page title lacks the Error prefix")
	}
}

// Every notice code an action names is in the table, and every table entry is a short cookie-safe code with text.
func TestNoticeCodesKnown(t *testing.T) {
	code := regexp.MustCompile(`^[a-z0-9-]{1,24}$`)
	for k, n := range notices {
		if !code.MatchString(k) || n.text == "" || !strings.HasSuffix(n.text, ".") || (n.section == "") != (n.pages == "") {
			t.Errorf("notice %q = %+v", k, n)
		}
		for _, page := range strings.Fields(n.pages) { // A-291: the pages that render the section exist
			if page == "admin/user" { // A-545: an account's page in the user directory, on page:admin's Users section
				page = "admin"
			} else if base, section := adminSectionPage(page); section != "" { // A-542: admin/<slug> is a section of page:admin
				page = base
			} else if strings.HasPrefix(page, "admin/") {
				t.Errorf("notice %q names unknown administration section %q", k, page)
			}
			if _, err := os.Stat(filepath.Join("web/templates/pages", page+".html")); err != nil {
				t.Errorf("notice %q names page %q: %v", k, page, err)
			}
		}
	}
	files, err := filepath.Glob("internal/market/*.go")
	if err != nil || len(files) == 0 {
		t.Fatal(files, err)
	}
	// actionResult{Notice: "…"}, res.Notice = "…" (or out.), and a local notice := "…" (never PageData's d.Notice text).
	used := regexp.MustCompile(`(?:Notice:|\b(?:res|out)\.Notice(?:, \w+\.\w+)? =|\bnotice :?=) *"([^"]*)"`)
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range used.FindAllStringSubmatch(string(src), -1) {
			seen[m[1]] = true
			if _, ok := notices[m[1]]; !ok {
				t.Errorf("%s names unknown notice code %q", f, m[1])
			}
		}
	}
	for k := range notices {
		if !seen[k] {
			t.Errorf("notice code %q is in the table but no action names it", k)
		}
	}
}

// Titles name the object a page shows (A-117), and the marketplace and transparency pages by those names.
func TestObjectTitles(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendorSess := e.user("title_vendor", "vendor")
	buyerID, buyerSess := e.user("title_buyer", "buyer")
	p := e.product(vendorID, "physical")
	o := e.order(buyerID, p, "XMR", "awaiting_payment")
	for _, c := range []struct{ path, sess, want string }{
		{"/", "", "<title>Marketplace · "},
		{"/canary", "", "<title>Transparency · "},
		{"/product?id=" + p, buyerSess, "<title>Test listing · "},
		{"/vendor?id=" + vendorID, "", "<title>Vendor title_vendor · "},
		{"/order?id=" + o, vendorSess, "<title>Order " + shortID(o) + " – Test listing · "},
	} {
		w := e.do("GET", c.path, c.sess, nil)
		e.check(w, 200)
		if !strings.Contains(w.Body.String(), c.want) {
			b := w.Body.String()
			i := strings.Index(b, "<title>")
			t.Errorf("%s: want %q, got %q", c.path, c.want, b[i:i+min(len(b)-i, 80)])
		}
	}
}
