package market

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// UI107-3: a refused /setup shows its form again with the handle and marketplace name typed, never the token or password.
func TestSetupRefusalKeepsTypedFields(t *testing.T) {
	e := newTestApp(t)
	const weak, typedName, wrongToken = "password1234", "Harbor <Market>", "typed-wrong-setup-token"
	for _, c := range []struct {
		name     string
		f        url.Values
		code     int
		reason   string
		secrets  []string
		siteName string
	}{
		{"a weak password", url.Values{"handle": {"first_admin"}, "site_name": {typedName}, "password": {weak}, "token": {e.A.setupToken}}, 400, passwordAdvice, []string{weak, e.A.setupToken}, typedName},
		{"a wrong token", url.Values{"handle": {"first_admin"}, "site_name": {typedName}, "password": {testPassword}, "token": {wrongToken}}, 403, "token incorrect", []string{testPassword, wrongToken, e.A.setupToken}, typedName},
		{"no name typed keeps the default", url.Values{"handle": {"first_admin"}, "password": {weak}, "token": {e.A.setupToken}}, 400, passwordAdvice, []string{weak, e.A.setupToken}, "OPSEC Market"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := e.do("POST", "/setup", randomToken(), c.f)
			e.check(w, c.code)
			b := w.Body.String()
			if !strings.Contains(refusalText(b), c.reason) {
				t.Fatalf("reason %q, want %q", refusalText(b), c.reason)
			}
			mustContain(t, b, `action="/setup"`, `name="handle" value="first_admin"`, `value="`+html.EscapeString(c.siteName)+`">`, `<input name="token" type="text" required`, `autocomplete="new-password"`)
			mustNotContain(t, b, `refusal-back`)
			for _, secret := range c.secrets {
				mustNotContain(t, b, secret)
			}
		})
	}
	// The password rule is the field's description, on a first view and on a refused one.
	first := e.body("GET", "/setup", randomToken(), nil, 200)
	mustContain(t, first, `name="password" required minlength="12" autocomplete="new-password" aria-describedby="password-help"`, `<span class="help" id="password-help">Use at least 12 characters`, `name="site_name" required maxlength="80" value="OPSEC Market"`)
	// A reload is a fresh form: nothing typed is stored.
	mustNotContain(t, first, "first_admin", "Harbor")
	if n := agInt(e, "SELECT count(*) FROM users"); n != 0 {
		t.Fatalf("a refused setup left %d accounts", n)
	}
	if n := agInt(e, "SELECT count(*) FROM settings WHERE key='installed'"); n != 0 {
		t.Fatal("a refused setup installed the market")
	}
	// A cross-site post shows nothing it sent.
	for _, h := range [][2]string{{"Origin", "http://evil.example"}, {"Sec-Fetch-Site", "cross-site"}} {
		f := url.Values{"handle": {"evil_handle"}, "site_name": {"Evil Market"}, "password": {testPassword}, "token": {e.A.setupToken}}
		r := httptest.NewRequest("POST", "/setup", strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set(h[0], h[1])
		r.AddCookie(&http.Cookie{Name: "session", Value: randomToken()})
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		e.check(w, 403)
		mustNotContain(t, w.Body.String(), "evil_handle", "Evil Market", e.A.setupToken, testPassword)
	}
	// A corrected post installs, and from then on a refusal is the shared page: no form to take over.
	e.check(e.do("POST", "/setup", randomToken(), url.Values{"handle": {"first_admin"}, "site_name": {"Harbor"}, "password": {testPassword}, "token": {e.A.setupToken}}), 303)
	late := e.do("POST", "/setup", randomToken(), url.Values{"handle": {"late_admin"}, "site_name": {"Late Market"}, "password": {testPassword}, "token": {e.A.setupToken}})
	e.check(late, 403)
	mustContain(t, late.Body.String(), "refusal-back")
	mustNotContain(t, late.Body.String(), `action="/setup"`, "late_admin", "Late Market", e.A.setupToken, testPassword)
}
