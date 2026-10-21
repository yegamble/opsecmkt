package market

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestAdminSocketConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, mode, socket, compose string
		wantError                   bool
	}{
		{"default", "tor", "", "", false},
		{"bare binary", "tor", "/run/market-admin/http.sock", "", false},
		{"overlay", "tor", "/run/market-admin/http.sock", "compose.yaml:compose.tor.yaml:compose.admin-tor.yaml", false},
		{"clearnet", "clearnet", "/run/market-admin/http.sock", "", true},
		{"relative", "tor", "http.sock", "", true},
		{"unclean", "tor", "/run/../tmp/http.sock", "", true},
		{"root", "tor", "/http.sock", "", true},
		{"overlay missing socket", "tor", "", "compose.yaml:compose.tor.yaml:compose.admin-tor.yaml", true},
		{"socket without overlay", "tor", "/run/market-admin/http.sock", "compose.yaml:compose.tor.yaml", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := adminSocketConfig(env(map[string]string{"ADMIN_SOCKET": tc.socket, "COMPOSE_FILE": tc.compose}), tc.mode)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error %v", err, tc.wantError)
			}
		})
	}
}

func TestPrivateAdminIngressSessionsAndSharedStaffRoutes(t *testing.T) {
	e := newTestApp(t)
	admin := e.setup()
	_, moderator := e.user("private_mod", "moderator")
	_, buyer := e.user("public_buyer", "buyer")
	e.A.adminSocket = "/run/market-admin/http.sock"
	request := func(private bool, method, path, token string, form url.Values) *httptest.ResponseRecorder {
		if form == nil {
			form = url.Values{}
		}
		if method == "POST" {
			form.Set("csrf", e.A.csrf(token))
		}
		r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Tor-Onion-Role", "admin")
		r.Host = strings.Repeat("a", 56) + ".onion"
		r.AddCookie(&http.Cookie{Name: "session", Value: token})
		w := httptest.NewRecorder()
		if private {
			e.A.AdminHandler().ServeHTTP(w, r)
		} else {
			e.A.ServeHTTP(w, r)
		}
		return w
	}
	for _, path := range []string{"/admin", "/moderator", "/order?id=unknown", "/account", "/admin/audit-export?upto=1", "/desk/img/" + strings.Repeat("a", 32) + "-f"} {
		if w := request(false, "GET", path, admin, nil); w.Code != 404 {
			t.Fatalf("public administrator %s: status=%d", path, w.Code)
		}
	}
	for _, path := range adminSectionPaths {
		if w := request(false, "GET", path, admin, nil); w.Code != 404 {
			t.Fatalf("public administrator %s: status=%d", path, w.Code)
		}
		if w := request(true, "GET", path, admin, nil); w.Code != 200 {
			t.Fatalf("private administrator %s: status=%d body=%s", path, w.Code, w.Body)
		}
		if w := request(true, "GET", path, moderator, nil); w.Code != 403 {
			t.Fatalf("private onion granted admin role to moderator on %s: status=%d", path, w.Code)
		}
	}
	if w := request(false, "GET", "/moderator", moderator, nil); w.Code != 200 {
		t.Fatalf("public moderator lost desk access: status=%d", w.Code)
	}
	if w := request(false, "GET", "/account", buyer, nil); w.Code != 200 {
		t.Fatalf("public buyer lost account access: status=%d", w.Code)
	}
	for _, private := range []bool{false, true} {
		w := request(private, "POST", "/login", randomToken(), url.Values{"handle": {"admin_user"}, "password": {testPassword}})
		if private && (w.Code != 303 || e.cookie(w, "session") == "") {
			t.Fatalf("private sign-in failed: status=%d body=%s", w.Code, w.Body)
		}
		// A-611: refused as a wrong password is, so the answer does not confirm the administrator's password.
		if !private && (w.Code != 401 || !strings.Contains(refusalText(w.Body.String()), "Invalid handle or password") || e.cookie(w, "session") != "" || e.cookie(w, "pending") != "") {
			t.Fatalf("public administrator sign-in was accepted: status=%d headers=%v", w.Code, w.Header())
		}
	}
	// startSession's locked role check also covers pending factor logins created before private ingress was enabled.
	var uid, hash string
	if err := e.DB.QueryRow("SELECT id,password_hash FROM users WHERE handle='admin_user'").Scan(&uid, &hash); err != nil {
		t.Fatal(err)
	}
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if token, err := e.A.startSession(context.Background(), tx, uid, "", "password and TOTP", digest(hash)); token != "" || err == nil {
		t.Fatalf("public factor completion issued an admin session: token present=%v err=%v", token != "", err)
	}
}

type unreadAdminBody struct{ t *testing.T }

func (b unreadAdminBody) Read([]byte) (int, error) {
	b.t.Fatal("public admin route read the request body")
	return 0, nil
}

func (unreadAdminBody) Close() error { return nil }

func TestPublicAdminIngressDeniesBeforeRequestWork(t *testing.T) {
	a := testHTTPApp(true)
	a.adminSocket = "/run/market-admin/http.sock"
	paths := []string{"/admin", "/admin/audit-export?upto=1", "/admin/reset-factors", "/administrator", "/%61dmin", "/setup"}
	// Every administrator-only action is covered even when a future feature puts it outside /admin.
	for path, spec := range actions {
		if adminOnlyRoles(spec.Roles) {
			paths = append(paths, path)
		}
	}
	// And every administrator-only page, the administration sections (A-542) included.
	for name, spec := range pages {
		if adminOnlyRoles(spec.Roles) {
			paths = append(paths, "/"+name)
		}
	}
	if !slices.Contains(paths, "/admin/system") || !slices.Contains(paths, "/admin/users") {
		t.Fatal("administration sections are not administrator-only pages")
	}
	for _, path := range paths {
		for _, method := range []string{"GET", "HEAD", "POST", "PUT"} {
			t.Run(method+path, func(t *testing.T) {
				r := httptest.NewRequest(method, path, nil)
				r.Host = strings.Repeat("a", 56) + ".onion"
				r.Header.Set("X-Tor-Onion-Role", "admin")
				r.Header.Set("X-Forwarded-Host", r.Host)
				r.Body = unreadAdminBody{t}
				w := httptest.NewRecorder()
				a.ServeHTTP(w, r)
				if w.Code != http.StatusNotFound || w.Header().Get("Set-Cookie") != "" {
					t.Fatalf("status=%d headers=%v", w.Code, w.Header())
				}
			})
		}
	}
}

func TestPrivateAdminIngressDoesNotGrantAccountAuthorization(t *testing.T) {
	e := newTestApp(t)
	e.setup()
	a := e.A
	a.adminSocket = "/run/market-admin/http.sock"
	for _, path := range append([]string{"/admin/audit-export?upto=1"}, adminSectionPaths...) {
		w := httptest.NewRecorder()
		a.AdminHandler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusSeeOther && w.Code != http.StatusForbidden {
			t.Fatalf("private anonymous %s: status=%d, body=%s", path, w.Code, w.Body.String())
		}
	}
}

func TestPublicAdministratorSessionRule(t *testing.T) {
	a := &App{adminSocket: "/run/market-admin/http.sock"}
	for _, role := range []string{"buyer", "vendor", "moderator", "admin"} {
		if denied := a.publicAdministrator(context.Background(), &User{Role: role}); denied != (role == "admin") {
			t.Errorf("public role %s: denied=%v", role, denied)
		}
		ctx := context.WithValue(context.Background(), adminIngressKey{}, true)
		if a.publicAdministrator(ctx, &User{Role: role}) {
			t.Errorf("private role %s was denied by ingress", role)
		}
	}
	if a.publicAdministrator(context.Background(), nil) {
		t.Fatal("anonymous public customer was denied")
	}
	for _, path := range []string{"/moderator", "/moderator/payment-review", "/resolve", "/listings"} {
		if privateAdminRoute(path) {
			t.Errorf("shared route %s denied to public moderators/vendors", path)
		}
	}
}
