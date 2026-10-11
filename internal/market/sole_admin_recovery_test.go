package market

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func pauseSignIn(t *testing.T, e *testEnv, handle string) {
	t.Helper()
	for i := range 10 {
		agExpect(e, "/login", randomToken(), url.Values{"handle": {handle}, "password": {"wrong-password-" + string(rune('a'+i))}}, 401, "Invalid handle or password")
	}
	if !e.A.limited(signInKey(handle), 10) {
		t.Fatal("sign-in handle was not paused")
	}
}

func rescueForm(handle, password, code string) url.Values {
	return url.Values{"handle": {handle}, "password": {password}, "rescue_code": {code}}
}

func TestSixDigitTOTPSyntax(t *testing.T) {
	for _, tc := range []struct {
		code string
		want bool
	}{{"000000", true}, {"123456", true}, {"12345", false}, {"1234567", false}, {"１２３４５６", false}, {"12345a", false}, {"", false}} {
		if got := sixDigitTOTP(tc.code); got != tc.want {
			t.Fatalf("code %q: got %v want %v", tc.code, got, tc.want)
		}
	}
}

func TestSoleAdminPausedSignInNeedsCurrentPasswordAndTOTP(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("sole_admin", "admin")
	key, _ := agEnableTOTP(e, id)
	pauseSignIn(t, e, "sole_admin")
	page := e.do("GET", "/login", randomToken(), nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `name="rescue_code"`) || !strings.Contains(page.Body.String(), "only administrator") {
		t.Fatal("optional paused-admin authenticator field or explanation missing")
	}
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, ""), 429, signInPausedText)
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, "not-six-digits"), 401, "Invalid handle or password")
	if agLimited(e, "totp-verify:"+id) {
		t.Fatal("malformed code spent factor budget")
	}
	correct := agTOTPNow(key)
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", "wrong-long-password", correct), 401, "Invalid handle or password")
	if agLimited(e, "totp-verify:"+id) || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
		t.Fatal("wrong password spent factor budget or TOTP step")
	}
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, e.wrongTOTP(id)), 401, "Invalid handle or password")
	if !agLimited(e, "totp-verify:"+id) || agUserSessions(e, id) != 1 {
		t.Fatal("wrong code did not charge factor budget or created a session")
	}
	pending, unrelated := agPending(e, id), agPending(e, id)
	w := e.do("POST", "/login", randomToken(), rescueForm("SOLE_ADMIN", testPassword, correct), pending)
	e.check(w, 303)
	e.session(w)
	if agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(pending.Value)) != 0 ||
		agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(unrelated.Value)) != 1 {
		t.Fatal("recovery failed to clear only this browser's pending sign-in")
	}
	clearedPending := false
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "pending" && cookie.MaxAge < 0 {
			clearedPending = true
		}
	}
	if !clearedPending {
		t.Fatal("recovery did not expire this browser's pending cookie")
	}
	if !e.A.limited(signInKey("sole_admin"), 10) || agUserSessions(e, id) != 2 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) <= 0 ||
		agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Signed in with password and TOTP'", id) != 1 {
		t.Fatal("paused sign-in did not atomically record one TOTP step and session")
	}
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, correct), 401, "Invalid handle or password")
	if agUserSessions(e, id) != 2 || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Signed in with password and TOTP'", id) != 1 {
		t.Fatal("reused code created another session or sign-in audit")
	}
}

func TestSoleAdminPausedSignInRefusesIneligibleAccountsBeforeFactor(t *testing.T) {
	for _, tc := range []string{"second admin", "suspended second admin", "disabled TOTP", "suspended owner", "nonadmin", "ambiguous handle"} {
		t.Run(tc, func(t *testing.T) {
			e := newTestApp(t)
			id, _ := e.user("SoleAdmin", "admin")
			key, _ := agEnableTOTP(e, id)
			pauseSignIn(t, e, "SoleAdmin")
			handle := "SOLEADMIN"
			switch tc {
			case "second admin", "suspended second admin":
				otherID, _ := e.user("other_admin", "admin")
				if tc == "suspended second admin" {
					agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", otherID)
				}
			case "disabled TOTP":
				agExec(e, "UPDATE users SET totp_enabled=false WHERE id=$1", id)
			case "suspended owner":
				agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", id)
			case "nonadmin":
				agExec(e, "UPDATE users SET role='buyer' WHERE id=$1", id)
			case "ambiguous handle":
				e.user("soleadmin", "buyer") // a legacy casefold twin; no exact uppercase handle
			}
			agExpect(e, "/login", randomToken(), rescueForm(handle, testPassword, agTOTPNow(key)), 401, "Invalid handle or password")
			if agLimited(e, "totp-verify:"+id) || agUserSessions(e, id) != 1 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
				t.Fatal("ineligible account spent factor budget or signed in")
			}
		})
	}
}

func TestSoleAdminPausedSignInCAPTCHAAndBusySlot(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("sole_admin", "admin")
	key, _ := agEnableTOTP(e, id)
	pauseSignIn(t, e, "sole_admin")
	agExec(e, "UPDATE settings SET value='true' WHERE key='captcha_required'")
	form := rescueForm("sole_admin", testPassword, agTOTPNow(key))
	agExpect(e, "/login", randomToken(), form, 400, "CAPTCHA")
	if agLimited(e, "totp-verify:"+id) {
		t.Fatal("failed CAPTCHA spent factor budget")
	}
	anon := randomToken()
	captchaID := e.captcha("/login", anon)
	form.Set("captcha_id", captchaID)
	form.Set("captcha", e.A.captchaAnswer(captchaID))
	release := agHoldPasswordWork(t)
	agExpect(e, "/login", anon, form, 503, "Authentication is busy")
	if agLimited(e, "totp-verify:"+id) || agUserSessions(e, id) != 1 {
		t.Fatal("busy refusal charged factor budget or signed in")
	}
	release()
}

func TestSoleAdminPausedSignInRespectsFactorCapAndConcurrentReplay(t *testing.T) {
	t.Run("factor cap", func(t *testing.T) {
		e := newTestApp(t)
		id, _ := e.user("sole_admin", "admin")
		key, _ := agEnableTOTP(e, id)
		pauseSignIn(t, e, "sole_admin")
		for range 10 {
			if !e.A.allow("totp-verify:"+id, 10) {
				t.Fatal("could not fill factor budget")
			}
		}
		agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, agTOTPNow(key)), 401, "Invalid handle or password")
		if agUserSessions(e, id) != 1 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
			t.Fatal("spent factor budget still allowed a session")
		}
	})
	t.Run("concurrent code", func(t *testing.T) {
		e := newTestApp(t)
		id, _ := e.user("sole_admin", "admin")
		key, _ := agEnableTOTP(e, id)
		pauseSignIn(t, e, "sole_admin")
		code := agTOTPNow(key)
		results := agRace(2, func(int) (int, string) {
			w := e.do("POST", "/login", randomToken(), rescueForm("sole_admin", testPassword, code))
			return w.Code, w.Body.String()
		})
		if got := agTally(t, results, 303, 401); got[303] != 1 || got[401] != 1 {
			t.Fatalf("concurrent TOTP replay: %+v", got)
		}
		if agUserSessions(e, id) != 2 || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Signed in with password and TOTP'", id) != 1 {
			t.Fatal("concurrent code created more than one session")
		}
	})
}

func TestSoleAdminPausedSignInNormalLoginRemainsChallenge(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("sole_admin", "admin")
	agEnableTOTP(e, id)
	w := e.do(http.MethodPost, "/login", randomToken(), url.Values{"handle": {"sole_admin"}, "password": {testPassword}})
	e.check(w, http.StatusSeeOther)
	if w.Header().Get("Location") != "/challenge" || e.cookie(w, "pending") == "" || e.cookie(w, "session") != "" {
		t.Fatalf("unpaused sign-in skipped normal factor challenge: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestSoleAdminRecoveryRechecksLockedCurrentAccount(t *testing.T) {
	for _, changed := range []string{"password", "role", "suspension", "TOTP"} {
		t.Run(changed, func(t *testing.T) {
			e := newTestApp(t)
			id, _ := e.user("sole_admin", "admin")
			key, _ := agEnableTOTP(e, id)
			checkedHash := passwordHash(e, id) // the hash whose password passed bcrypt before this change
			switch changed {
			case "password":
				agExec(e, "UPDATE users SET password_hash='replacement-hash' WHERE id=$1", id)
			case "role":
				agExec(e, "UPDATE users SET role='buyer' WHERE id=$1", id)
			case "suspension":
				agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", id)
			case "TOTP":
				agExec(e, "UPDATE users SET totp_enabled=false WHERE id=$1", id)
			}
			c := &actionCtx{A: e.A, W: httptest.NewRecorder(), R: httptest.NewRequest(http.MethodPost, "/login", nil), Token: randomToken()}
			if _, err := e.A.soleAdminRecovery(c, id, checkedHash, agTOTPNow(key), ""); err != errSoleAdminRecovery {
				t.Fatalf("stale %s passed locked recovery check: %v", changed, err)
			}
			if agLimited(e, "totp-verify:"+id) || agUserSessions(e, id) != 1 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
				t.Fatal("stale account consumed factor or signed in")
			}
		})
	}
}
