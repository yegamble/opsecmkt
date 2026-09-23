package market

import (
	"html/template"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseAmountExactAtomicUnits(t *testing.T) {
	for _, tc := range []struct {
		input    string
		decimals int
		want     int64
	}{
		{"0.00000001", 8, 1}, {"0.000000000001", 12, 1},
		{"1.23", 8, 123000000}, {"2", 12, 2000000000000},
		{"92233720368.54775807", 8, math.MaxInt64},
		{"9.223372036854", 12, 9223372036854},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseAmount(tc.input, tc.decimals)
			if err != nil || got != tc.want {
				t.Fatalf("parseAmount = %d, %v; want %d", got, err, tc.want)
			}
			back, err := parseAmount(amount(got, tc.decimals), tc.decimals)
			if err != nil || back != got {
				t.Fatalf("atomic-unit round trip = %d, %v; want %d", back, err, got)
			}
		})
	}
}

func TestParseAmountRejectsInvalidAndOverflow(t *testing.T) {
	for _, input := range []string{"", "0", "0.00000000", "-1", "+1", "1e2", " 1", "1 ", ".1", "1.2.3", "NaN", "0.000000001", "92233720368.54775808", "999999999999999999999999999", "１"} {
		t.Run(input, func(t *testing.T) {
			if got, err := parseAmount(input, 8); err == nil {
				t.Fatalf("accepted %q as %d", input, got)
			}
		})
	}
}

func TestPrivilegedPageAuthorization(t *testing.T) {
	for _, role := range []string{"anonymous", "buyer", "vendor", "moderator", "admin", "unknown"} {
		var user *User
		if role != "anonymous" {
			user = &User{Role: role}
		}
		for _, page := range []string{"admin", "moderator", "vendor-dashboard"} {
			want := role == "admin" || page == "moderator" && role == "moderator" || page == "vendor-dashboard" && role == "vendor"
			if got := authorized(page, user); got != want {
				t.Errorf("role=%s page=%s: authorized=%v want=%v", role, page, got, want)
			}
			if !protected(page) {
				t.Errorf("privileged page %q is not authentication-protected", page)
			}
		}
	}
	for _, page := range []string{"checkout", "orders", "order", "messages", "notifications", "disputes", "account"} {
		if !protected(page) {
			t.Errorf("private page %q is public", page)
		}
	}
}

func testHTTPApp(preview bool) *App {
	return &App{preview: preview, key: []byte("test-only-csrf-key"), limits: make(map[string]bucket), templates: template.Must(template.New("page").Parse(`{{define "page"}}{{.Title}}{{end}}`))}
}

func TestPreviewRejectsStateChanges(t *testing.T) {
	a := testHTTPApp(true)
	for _, path := range []string{"/setup", "/login", "/register", "/account", "/orders", "/messages", "/listings", "/disputes", "/resolve", "/admin", "/logout", "/revoke-sessions", "/notifications"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("csrf=anything"))
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Read-only preview") {
				t.Fatalf("preview mutation: status=%d body=%s", w.Code, w.Body)
			}
		})
	}
}

func TestCSRFRejectsCrossOriginAndWrongSession(t *testing.T) {
	a := testHTTPApp(false)
	token := strings.Repeat("a", 64)
	for _, tc := range []struct{ name, csrf, origin, fetch string }{
		{"missing", "", "", ""},
		{"wrong-session", a.csrf(strings.Repeat("b", 64)), "", ""},
		{"cross-origin", a.csrf(token), "https://attacker.example", ""},
		{"opaque-origin", a.csrf(token), "null", ""},
		{"cross-site-metadata", a.csrf(token), "", "cross-site"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://market.example/account", strings.NewReader(url.Values{"csrf": {tc.csrf}}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.fetch)
			r.AddCookie(&http.Cookie{Name: "session", Value: token})
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("got %d; expected rejection before database access", w.Code)
			}
		})
	}
}

func TestValidCSRFMustStillAuthenticate(t *testing.T) {
	a := testHTTPApp(false)
	token := strings.Repeat("a", 64)
	r := httptest.NewRequest(http.MethodPost, "http://market.example/account", strings.NewReader(url.Values{"csrf": {a.csrf(token)}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://market.example")
	r.AddCookie(&http.Cookie{Name: "session", Value: token})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("valid CSRF bypassed authentication: %d", w.Code)
	}
}

func TestResponseSecurityHeadersAndMethods(t *testing.T) {
	a := testHTTPApp(true)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	for name, want := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY", "Cache-Control": "no-store"} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("%s = %q; want %q", name, got, want)
		}
	}
	for _, directive := range []string{"default-src 'none'", "form-action 'self'", "base-uri 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), directive) {
			t.Errorf("CSP missing %s", directive)
		}
	}
	for _, method := range []string{http.MethodDelete, http.MethodPut, http.MethodPatch, http.MethodTrace} {
		w = httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest(method, "/account", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s accepted: %d", method, w.Code)
		}
	}
}

func TestSessionCookieSecurityAttributes(t *testing.T) {
	for _, secure := range []bool{false, true} {
		a := testHTTPApp(true)
		a.secure = secure
		w := httptest.NewRecorder()
		a.cookie(w, "session", strings.Repeat("a", 64), 43200)
		cookies := w.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatal("expected one cookie")
		}
		c := cookies[0]
		if !c.HttpOnly || c.Secure != secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge != 43200 {
			t.Errorf("cookie attributes incorrect: %+v", c)
		}
	}
}

func TestOversizedFormRejected(t *testing.T) {
	a := testHTTPApp(false)
	r := httptest.NewRequest(http.MethodPost, "/account", strings.NewReader("pgp="+strings.Repeat("x", 65537)))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized body returned %d", w.Code)
	}
}
