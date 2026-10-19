package market

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
)

// A-549: an authenticator code typed on /login with a correct password completes sign-in in one step for an account
// whose only second factor is TOTP. It goes through the same check, replay guard and per-account factor budget as
// /challenge; a blank code keeps the two-step flow, and the code never skips another enrolled factor.

const signInCodeRefusedText = "The authenticator code was not accepted"

func loginWithCode(handle, code, next string) url.Values {
	f := url.Values{"handle": {handle}, "password": {testPassword}, "rescue_code": {code}}
	if next != "" {
		f.Set("next", next)
	}
	return f
}

// limiterCount is the number of attempts the in-memory limiter holds for key.
func limiterCount(e *testEnv, key string) int {
	e.A.mu.Lock()
	defer e.A.mu.Unlock()
	return e.A.limits[key].Count
}

func factorBudgetCount(e *testEnv, userID string) int { return limiterCount(e, "totp-verify:"+userID) }

// expectChallenge checks a 303 to /challenge (with next) that opened a pending sign-in and no session.
func expectChallenge(t *testing.T, e *testEnv, w *httptest.ResponseRecorder, next string) {
	t.Helper()
	if loc := w.Header().Get("Location"); w.Code != http.StatusSeeOther || loc != challengePath(next) || e.cookie(w, "pending") == "" || e.cookie(w, "session") != "" {
		t.Fatalf("want a pending sign-in at %q, got %d Location=%q cookies=%v", challengePath(next), w.Code, loc, w.Header().Values("Set-Cookie"))
	}
}

func TestLoginWithAuthenticatorCode(t *testing.T) {
	const next = "/orders"
	t.Run("right code signs in directly and is not replayable", func(t *testing.T) {
		e := newTestApp(t)
		id, _ := e.user("a549_right", "buyer")
		key, _ := agEnableTOTP(e, id)
		browser := randomToken()
		staleBrowserPending, otherPending := agPending(e, id), agPending(e, id)
		code := agTOTPNow(key)
		w := e.do("POST", "/login", browser, loginWithCode("a549_right", code, next), staleBrowserPending)
		e.check(w, http.StatusSeeOther)
		sess := e.session(w)
		if loc := w.Header().Get("Location"); loc != next {
			t.Fatalf("signed-in redirect=%q, want %q", loc, next)
		}
		if sess == browser || agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1", digest(browser)) != 0 {
			t.Fatal("sign-in did not rotate the browser's session token")
		}
		if agUserSessions(e, id) != 2 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) <= 0 ||
			a152Audits(e, id, "Signed in with password and TOTP") != 1 || factorBudgetCount(e, id) != 1 {
			t.Fatal("one-step sign-in did not record one session, TOTP step, audit row and factor attempt")
		}
		if agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(staleBrowserPending.Value)) != 0 ||
			agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(otherPending.Value)) != 1 {
			t.Fatal("sign-in did not consume only this browser's pending sign-in")
		}
		cleared := false
		for _, c := range w.Result().Cookies() {
			cleared = cleared || c.Name == "pending" && c.MaxAge < 0
		}
		if !cleared {
			t.Fatal("sign-in did not expire this browser's pending cookie")
		}
		e.check(e.do("GET", "/account", sess, nil), http.StatusOK)
		// The same code again is refused by the replay guard: no session, a pending sign-in and the notice.
		w = e.do("POST", "/login", randomToken(), loginWithCode("a549_right", code, next))
		e.check(w, http.StatusSeeOther)
		expectChallenge(t, e, w, next)
		if agUserSessions(e, id) != 2 || a152Audits(e, id, "Signed in with password and TOTP") != 1 || e.cookie(w, noticeCookie) != "signin-code-wrong" {
			t.Fatal("a replayed code signed in again or gave no notice")
		}
	})

	t.Run("wrong code lands on the challenge with a notice and spends the budget once", func(t *testing.T) {
		e := newTestApp(t)
		id, _ := e.user("a549_wrong", "buyer")
		key, _ := agEnableTOTP(e, id)
		anon := randomToken()
		stale := agPending(e, id)
		w := e.do("POST", "/login", anon, loginWithCode("a549_wrong", e.wrongTOTP(id), next), stale)
		e.check(w, http.StatusSeeOther)
		expectChallenge(t, e, w, next)
		if agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(stale.Value)) != 0 ||
			agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1 AND factor_failed", id) != 1 {
			t.Fatal("the new pending sign-in did not replace this browser's earlier one or was not marked failed")
		}
		if factorBudgetCount(e, id) != 1 || agUserSessions(e, id) != 1 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
			t.Fatal("wrong code was not charged exactly once or changed authentication state")
		}
		if limiterCount(e, signInKey("a549_wrong")) != 0 {
			t.Fatal("a correct password with a wrong code spent the sign-in password budget")
		}
		// The owner learns that someone has the password, as for a failed /challenge (A-152), once per pending sign-in.
		if a152Audits(e, id, "Correct password but failed second sign-in step (TOTP)") != 1 ||
			a152Notes(e, id, "failed the second sign-in step (TOTP)") != 1 {
			t.Fatal("a wrong code at sign-in was not recorded for the owner")
		}
		notice := &http.Cookie{Name: noticeCookie, Value: e.cookie(w, noticeCookie)}
		pending := &http.Cookie{Name: "pending", Value: e.cookie(w, "pending")}
		page := html.UnescapeString(e.body("GET", challengePath(next), anon, nil, http.StatusOK, notice, pending))
		mustContain(t, page, signInCodeRefusedText, `class="factor-totp"`)
		// The challenge still completes with a current code, landing on next; the failure was recorded only once.
		w = e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, pending)
		e.check(w, http.StatusSeeOther)
		e.session(w)
		if w.Header().Get("Location") != next || factorBudgetCount(e, id) != 2 ||
			a152Audits(e, id, "Correct password but failed second sign-in step") != 1 {
			t.Fatal("challenge after a wrong sign-in code did not complete as usual")
		}
	})

	t.Run("concurrent sign-ins with one code open one session", func(t *testing.T) {
		e := newTestApp(t)
		id, _ := e.user("a549_race", "buyer")
		key, _ := agEnableTOTP(e, id)
		code := agTOTPNow(key)
		var wg sync.WaitGroup
		locations := make([]string, 4)
		for i := range locations {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := e.do("POST", "/login", randomToken(), loginWithCode("a549_race", code, ""))
				locations[i] = strconv.Itoa(w.Code) + " " + w.Header().Get("Location")
			}()
		}
		wg.Wait()
		signedIn, challenged := 0, 0
		for _, l := range locations {
			switch l {
			case "303 /":
				signedIn++
			case "303 /challenge":
				challenged++
			}
		}
		if signedIn != 1 || challenged != 3 || agUserSessions(e, id) != 2 || a152Audits(e, id, "Signed in with password and TOTP") != 1 {
			t.Fatalf("one code signed in %d times (%v)", signedIn, locations)
		}
	})

	t.Run("blank code keeps the two-step flow", func(t *testing.T) {
		e := newTestApp(t)
		id, _ := e.user("a549_blank", "buyer")
		agEnableTOTP(e, id)
		w := e.do("POST", "/login", randomToken(), loginWithCode("a549_blank", "", next))
		e.check(w, http.StatusSeeOther)
		expectChallenge(t, e, w, next)
		if agLimited(e, "totp-verify:"+id) || e.cookie(w, noticeCookie) != "" {
			t.Fatal("a blank code spent factor budget or set a notice")
		}
	})

	t.Run("account without TOTP ignores the field", func(t *testing.T) {
		e := newTestApp(t)
		id, _ := e.user("a549_plain", "buyer")
		w := e.do("POST", "/login", randomToken(), loginWithCode("a549_plain", "123456", next))
		e.check(w, http.StatusSeeOther)
		e.session(w)
		if w.Header().Get("Location") != next || agLimited(e, "totp-verify:"+id) || a152Audits(e, id, "Signed in with password") != 1 {
			t.Fatal("a password-only account did not sign in as before")
		}
	})

	t.Run("TOTP and PGP account still reaches the challenge", func(t *testing.T) {
		e := newTestApp(t)
		id, _ := e.user("a549_pgp", "vendor")
		key, _ := agEnableTOTP(e, id)
		_, pub := testPGPKey(t, "a549_pgp")
		agEnablePGP(e, id, pub)
		w := e.do("POST", "/login", randomToken(), loginWithCode("a549_pgp", agTOTPNow(key), next))
		e.check(w, http.StatusSeeOther)
		expectChallenge(t, e, w, next)
		if agUserSessions(e, id) != 1 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 || agLimited(e, "totp-verify:"+id) {
			t.Fatal("the code skipped or spent part of another enrolled factor's challenge")
		}
	})

	t.Run("lost PGP key is reported before the TOTP shortcut", func(t *testing.T) {
		e := newTestApp(t)
		id, _ := e.user("a549_lost", "vendor")
		storeKeyBySQL(e, id, garbageArmoredKey, garbageFingerprint)
		key, _ := agEnableTOTP(e, id)
		w := e.do("POST", "/login", randomToken(), loginWithCode("a549_lost", agTOTPNow(key), ""))
		e.check(w, http.StatusSeeOther)
		e.session(w)
		if a152Notes(e, id, unreadableKeyNote) != 1 || a152Audits(e, id, "Signed in with password and TOTP") != 1 {
			t.Fatal("TOTP sign-in with a lost PGP key did not notify the owner first")
		}
	})

	t.Run("suspended account is refused", func(t *testing.T) {
		e := newTestApp(t)
		id, _ := e.user("a549_susp", "buyer")
		key, _ := agEnableTOTP(e, id)
		agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", id)
		w := e.do("POST", "/login", randomToken(), loginWithCode("a549_susp", agTOTPNow(key), next))
		if w.Code != http.StatusForbidden || e.cookie(w, "session") != "" || e.cookie(w, "pending") != "" {
			t.Fatalf("suspended sign-in: status=%d cookies=%v", w.Code, w.Header().Values("Set-Cookie"))
		}
		mustContain(t, refusalText(w.Body.String()), "This account is suspended.")
		if agUserSessions(e, id) != 1 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
			t.Fatal("a suspended account's code was consumed or opened a session")
		}
	})

	t.Run("exhausted factor budget refuses before checking the code", func(t *testing.T) {
		e := newTestApp(t)
		id, _ := e.user("a549_budget", "buyer")
		key, _ := agEnableTOTP(e, id)
		for range 10 {
			w := e.do("POST", "/login", randomToken(), loginWithCode("a549_budget", e.wrongTOTP(id), ""))
			e.check(w, http.StatusSeeOther)
		}
		agExpect(e, "/login", randomToken(), loginWithCode("a549_budget", agTOTPNow(key), ""), http.StatusTooManyRequests, "Too many verification attempts")
		if agUserSessions(e, id) != 1 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
			t.Fatal("an exhausted factor budget still checked the code")
		}
	})
}
