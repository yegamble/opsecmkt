package market

import (
	"net/url"
	"strings"
	"testing"
)

// authRows is the account state a sign-in or registration could write: users, sessions, pending sign-ins and audit
// rows, as one comparable string.
func authRows(e *testEnv) string {
	e.t.Helper()
	return agStr(e, `SELECT concat_ws(',',(SELECT count(*) FROM users),(SELECT count(*) FROM sessions),
		(SELECT count(*) FROM pending_logins),(SELECT count(*) FROM audit_events))`)
}

// A-794: a forged /login or /register post (no CSRF token, one for another browser, a cross-origin or opaque Origin,
// or cross-site fetch metadata) is refused with 403 before /login's password check or either handle budget, and writes
// no user, session, pending sign-in or audit row; the same form from this browser and origin then succeeds. Neither form carries a form_nonce
// (their specs opt out), so the CSRF token and Origin are the whole forgery guard.
func TestLoginRegisterRefuseForgedPostsWithoutWrites(t *testing.T) {
	e := newTestApp(t)
	e.user("forged_signin", "buyer")
	checks := countSignInPasswordChecks(t)
	for _, path := range []string{"/login", "/register"} {
		if spec := actions[path]; spec.FormNonce || spec.ConfirmedFormNonce || spec.OwnFormNonce {
			t.Fatalf("%s now takes a form nonce: cover its refusal here", path)
		}
		handle := map[string]string{"/login": "forged_signin", "/register": "forged_register"}[path]
		anon := randomToken()
		post := func(csrf string, header ...string) (int, string) {
			t.Helper()
			form := url.Values{"handle": {handle}, "password": {testPassword}}
			if csrf != "" {
				form.Set("csrf", csrf)
			}
			w := a48Do(e, "POST", path, form, map[string]string{"anon": anon}, header...)
			if w.Code != 303 && (e.cookie(w, "session") != "" || e.cookie(w, "pending") != "") {
				t.Fatalf("%s: a refused post set a sign-in cookie: %v", path, w.Header().Values("Set-Cookie"))
			}
			return w.Code, refusalText(w.Body.String())
		}
		before, compared := authRows(e), checks.Load()
		for _, tc := range []struct {
			name, csrf string
			header     []string
			msg        string
		}{
			{"missing", "", nil, formExpired},
			{"other-browser", e.A.csrf(randomToken()), nil, formExpired},
			{"cross-origin", e.A.csrf(anon), []string{"Origin", "https://attacker.example"}, strings.TrimSpace(originHostMismatch)},
			{"opaque-origin", e.A.csrf(anon), []string{"Origin", "null"}, strings.TrimSpace(originHostMismatch)},
			{"cross-site-metadata", e.A.csrf(anon), []string{"Sec-Fetch-Site", "cross-site"}, formExpired},
		} {
			if code, msg := post(tc.csrf, tc.header...); code != 403 || !strings.Contains(msg, tc.msg) {
				t.Fatalf("%s %s: status=%d body=%q; want 403 containing %q", path, tc.name, code, msg, tc.msg)
			}
			if after := authRows(e); after != before {
				t.Fatalf("%s %s: users,sessions,pending,audit went %s -> %s", path, tc.name, before, after)
			}
		}
		if checks.Load() != compared {
			t.Fatalf("%s: a forged post ran bcrypt", path)
		}
		if limiterCount(e, signInKey(handle)) != 0 || limiterCount(e, "register:"+handle) != 0 {
			t.Fatalf("%s: a forged post spent the handle's budget", path)
		}
		if code, msg := post(e.A.csrf(anon), "Origin", "http://example.com", "Sec-Fetch-Site", "same-origin"); code != 303 {
			t.Fatalf("%s from this browser and origin: %d %q", path, code, msg)
		}
	}
}

// A-794: the handle is 3–32 characters (handlePattern) and the password 12–72 bytes (validPassword: bcrypt ignores
// bytes past 72). /register and /login accept exactly 32 and 72 and refuse 33 and 73 with errAuthInput before any
// password work, budget or write. In particular a 73-byte password whose first 72 bytes are right never signs in.
func TestLoginRegisterHandleAndPasswordUpperBounds(t *testing.T) {
	e := newTestApp(t)
	e.user("bounds_install", "buyer") // any account: with none, every page redirects to /setup
	handle32 := "edge_" + strings.Repeat("q", 27)
	password72 := "lantern quarry violet thimble orchard ember saddle juniper walnut cobalt harbor"[:72]
	if len(handle32) != 32 || len(password72) != 72 || !handlePattern.MatchString(handle32) || !validPassword(password72) {
		t.Fatal("fixture is not at the bounds")
	}
	refused := func(path, handle, password string) {
		t.Helper()
		checks := countSignInPasswordChecks(t)
		before := authRows(e)
		budgets := limiterCount(e, signInKey(handle)) + limiterCount(e, "register:"+strings.ToLower(handle))
		w := e.do("POST", path, randomToken(), url.Values{"handle": {handle}, "password": {password}})
		if w.Code != 400 || refusalText(w.Body.String()) != errAuthInput.(*httpError).Msg {
			t.Fatalf("%s handle %d chars, password %d bytes: %d %q", path, len(handle), len(password), w.Code, refusalText(w.Body.String()))
		}
		if e.cookie(w, "session") != "" || authRows(e) != before || checks.Load() != 0 {
			t.Fatalf("%s handle %d chars, password %d bytes: a refused post signed in, wrote rows or ran bcrypt", path, len(handle), len(password))
		}
		if limiterCount(e, signInKey(handle))+limiterCount(e, "register:"+strings.ToLower(handle)) != budgets {
			t.Fatalf("%s: an out-of-bounds post spent a budget", path)
		}
	}
	refused("/register", handle32+"q", password72)
	refused("/register", handle32, password72+"x")
	if n := agInt(e, "SELECT count(*) FROM users WHERE lower(handle) LIKE 'edge_%'"); n != 0 {
		t.Fatalf("out-of-bounds registrations made %d accounts", n)
	}
	e.check(e.do("POST", "/register", randomToken(), url.Values{"handle": {handle32}, "password": {password72}}), 303)
	refused("/login", handle32+"q", password72)
	refused("/login", handle32, password72+"x")
	w := e.do("POST", "/login", randomToken(), url.Values{"handle": {handle32}, "password": {password72}})
	e.check(w, 303)
	e.session(w)
}
