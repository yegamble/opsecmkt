package market

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

func TestRegisterRefusesCasefoldCollisionAndConcurrentVariants(t *testing.T) {
	e := newTestApp(t)
	e.user("LegacyBuyer", "buyer")
	agExpect(e, "/register", randomToken(), url.Values{"handle": {"legacybuyer"}, "password": {testPassword}}, 400, "Handle unavailable")
	if agInt(e, "SELECT count(*) FROM users WHERE lower(handle)='legacybuyer'") != 1 {
		t.Fatal("casefold refusal created another account")
	}
	const handle = "RaceBuyer"
	results := agRace(2, func(i int) (int, string) {
		variant := handle
		if i == 1 {
			variant = strings.ToLower(handle)
		}
		w := e.do("POST", "/register", randomToken(), url.Values{"handle": {variant}, "password": {testPassword}})
		return w.Code, w.Body.String()
	})
	if got := agTally(t, results, 303, 400); got[303] != 1 || got[400] != 1 {
		t.Fatalf("concurrent registrations: %+v", got)
	}
	if agInt(e, "SELECT count(*) FROM users WHERE lower(handle)=lower('RaceBuyer')") != 1 {
		t.Fatal("concurrent case variants created more than one account")
	}
}

func TestSetupAndBootstrapRefuseLegacyCasefoldHandle(t *testing.T) {
	e := newTestApp(t)
	setup := e.do("GET", "/setup", randomToken(), nil)
	if setup.Code != http.StatusOK || !strings.Contains(setup.Body.String(), `name="token" type="text" required autocomplete="off" spellcheck="false"`) {
		t.Fatalf("setup token input attributes: status=%d", setup.Code)
	}
	// Simulate a pre-setup legacy account without marking the market installed.
	agExec(e, "INSERT INTO users(id,handle,password_hash,role) VALUES($1,'LegacyAdmin',$2,'buyer')", randomToken(), testHash())
	agExpect(e, "/setup", randomToken(), url.Values{"token": {testSetupToken}, "handle": {"legacyadmin"}, "password": {testPassword}}, 400, "Handle unavailable")
	if err := BootstrapAdmin(context.Background(), e.DB, "LEGACYADMIN", testPassword, "Market"); err == nil || !strings.Contains(err.Error(), "Handle unavailable") {
		t.Fatalf("bootstrap casefold collision: %v", err)
	}
	if agInt(e, "SELECT count(*) FROM users") != 1 || agInt(e, "SELECT count(*) FROM settings WHERE key='installed'") != 0 || agInt(e, "SELECT count(*) FROM audit_events") != 0 {
		t.Fatal("refused setup or CLI bootstrap changed the market")
	}
}

func TestSignInExactFirstAndUniqueCasefoldFallback(t *testing.T) {
	e := newTestApp(t)
	e.user("CaseOnly", "buyer")
	for _, h := range []string{"CaseOnly", "caseonly", "CASEONLY"} {
		if code, _ := loginStatus(e, h, testPassword); code != http.StatusSeeOther {
			t.Fatalf("unique casefold sign-in %q: %d", h, code)
		}
	}
	agExpect(e, "/login", randomToken(), url.Values{"handle": {"caseonly"}, "password": {"wrong-password"}}, 401, "Invalid handle or password")

	// The migration must accept a legacy database with two case variants. Exact names remain usable;
	// a third spelling has no unambiguous account and receives the generic password refusal.
	e.user("LegacyTwin", "buyer")
	e.user("legacytwin", "buyer")
	otherPassword := "another-long-password"
	otherHash, err := bcrypt.GenerateFromPassword([]byte(otherPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, "UPDATE users SET password_hash=$1 WHERE handle='legacytwin'", string(otherHash))
	for _, tc := range []struct{ handle, password string }{{"LegacyTwin", testPassword}, {"legacytwin", otherPassword}} {
		if code, _ := loginStatus(e, tc.handle, tc.password); code != http.StatusSeeOther {
			t.Fatalf("legacy exact sign-in %q: %d", tc.handle, code)
		}
	}
	agExpect(e, "/login", randomToken(), url.Values{"handle": {"LegacyTwin"}, "password": {otherPassword}}, 401, "Invalid handle or password")
	w := e.do("POST", "/login", randomToken(), url.Values{"handle": {"LEGACYTWIN"}, "password": {testPassword}})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "Invalid handle or password") || e.cookie(w, "session") != "" || e.cookie(w, "pending") != "" {
		t.Fatalf("ambiguous fallback: status=%d body=%q", w.Code, w.Body.String())
	}
	if n := agInt(e, "SELECT count(*) FROM users WHERE lower(handle)='legacytwin'"); n != 2 {
		t.Fatalf("legacy case variants failed to coexist: %d", n)
	}
}

func TestSecureCookieModeHSTSAndHTTPOrigin(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			a := testHTTPApp(false)
			a.secure = secure
			get := httptest.NewRecorder()
			a.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "https://market.example/healthz", nil))
			if got := get.Header().Get("Strict-Transport-Security"); (got == "max-age=31536000") != secure {
				t.Fatalf("secure=%v HSTS=%q", secure, got)
			}
			for _, origin := range []string{"http://market.example", "https://market.example"} {
				token := strings.Repeat("a", 64)
				r := httptest.NewRequest(http.MethodPost, "https://market.example/account", strings.NewReader(url.Values{"csrf": {a.csrf(token)}}.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.Header.Set("Origin", origin)
				r.Header.Set("X-Forwarded-Proto", "https")
				r.Header.Set("X-Forwarded-Host", "market.example")
				r.AddCookie(&http.Cookie{Name: "session", Value: token})
				w := httptest.NewRecorder()
				a.ServeHTTP(w, r)
				refused := secure && strings.HasPrefix(origin, "http://")
				if refused && (w.Code != http.StatusForbidden || w.Body.String() != originHostMismatch) {
					t.Fatalf("secure HTTP origin: status=%d body=%q", w.Code, w.Body.String())
				}
				if !refused && strings.Contains(w.Body.String(), "Origin does not match Host") {
					t.Fatalf("allowed origin refused in secure=%v mode: %q", secure, w.Body.String())
				}
			}
		})
	}
}

func TestAccountInsertErrorDistinguishesCollisionFromDatabaseFailure(t *testing.T) {
	for _, tc := range []struct {
		state string
		code  int
		msg   string
	}{{"23505", 400, "Handle unavailable"}, {"XX000", 503, "Service unavailable"}} {
		err, ok := accountInsertError(&pgconn.PgError{Code: tc.state}).(*httpError)
		if !ok || err.Code != tc.code || err.Msg != tc.msg {
			t.Fatalf("SQLSTATE %s: %v", tc.state, err)
		}
	}
}

func TestSetupSiteNameCountsCharacters(t *testing.T) {
	name := strings.Repeat("é", 80)
	if got, err := setupSiteName(name); err != nil || got != name {
		t.Fatalf("80 multibyte characters: %q %v", got, err)
	}
	if _, err := setupSiteName(name + "é"); err == nil {
		t.Fatal("accepted 81 characters")
	}
	if err := CheckBootstrapInput("first_admin", name); err != nil {
		t.Fatalf("CLI refused 80 characters: %v", err)
	}
}

func TestRegisterDatabaseFailureDoesNotPretendHandleTaken(t *testing.T) {
	e := newTestApp(t)
	e.user("existing_user", "buyer")
	agExec(e, `CREATE FUNCTION fail_account_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'private database detail' USING ERRCODE='XX000'; END $$`)
	agExec(e, `CREATE TRIGGER fail_account_insert BEFORE INSERT ON users
		FOR EACH ROW EXECUTE FUNCTION fail_account_insert()`)
	w := e.do("POST", "/register", randomToken(), url.Values{"handle": {"new_user"}, "password": {testPassword}})
	if w.Code != 503 || !strings.Contains(w.Body.String(), "Service unavailable") || strings.Contains(w.Body.String(), "private database detail") || strings.Contains(w.Body.String(), "Handle unavailable") {
		t.Fatalf("database failure classified as collision or leaked: status=%d body=%q", w.Code, w.Body.String())
	}
	if agInt(e, "SELECT count(*) FROM users") != 1 || agInt(e, "SELECT count(*) FROM audit_events") != 0 {
		t.Fatal("failed insert wrote account or audit")
	}
}
