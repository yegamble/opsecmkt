package market

import (
	"bytes"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
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

// rescueHelpText is the sign-in page's explanation of the paused-handle rescue (A-571).
const rescueHelpText = "your password with your current six-digit authenticator code still signs you in"

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
	if page.Code != 200 || !strings.Contains(page.Body.String(), `name="rescue_code"`) || !strings.Contains(page.Body.String(), rescueHelpText) {
		t.Fatal("optional paused-sign-in authenticator field or explanation missing")
	}
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, ""), 429, signInPausedText)
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, "not-six-digits"), 401, "Invalid handle or password")
	if agLimited(e, "totp-verify:"+id) {
		t.Fatal("malformed code spent factor budget")
	}
	correct := agTOTPNow(key)
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", "wrong-long-password", correct), 401, "Invalid handle or password")
	if !agLimited(e, "totp-verify:"+id) || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
		t.Fatal("wrong recovery password escaped its budget or spent a TOTP step")
	}
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, e.wrongTOTP(id)), 401, "Invalid handle or password")
	if factorBudgetCount(e, id) != 1 || agUserSessions(e, id) != 1 {
		t.Fatal("wrong code charged the factor budget (A-571: it is refused before bcrypt) or created a session")
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

func TestSoleAdminPausedSignInWrongPasswordsExhaustRecoveryBeforeBcrypt(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("sole_admin", "admin")
	key, _ := agEnableTOTP(e, id)
	pauseSignIn(t, e, "sole_admin")
	budget := "totp-verify:" + id
	for i := range 10 {
		// Varying the case, the (right) code and the browser cannot create a fresh recovery allowance. Only a right
		// code reaches the password check and spends an attempt (A-571).
		handle := "sole_admin"
		if i%2 == 1 {
			handle = "SOLE_ADMIN"
		}
		code, _ := e.totpCodeFor(id, int64(i%2))
		agExpect(e, "/login", randomToken(), rescueForm(handle, "wrong-long-password", code), 401, "Invalid handle or password")
		e.A.mu.Lock()
		count := e.A.limits[budget].Count
		e.A.mu.Unlock()
		if count != i+1 {
			t.Fatalf("wrong recovery password %d: budget count=%d", i+1, count)
		}
	}
	if !e.A.limited(budget, 10) || agUserSessions(e, id) != 1 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
		t.Fatal("wrong recovery passwords escaped the cap or changed authentication state")
	}
	// An exhausted request must fail before trying to acquire bcrypt work, even with both factors right.
	release := agHoldPasswordWork(t)
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, agTOTPNow(key)), 401, "Invalid handle or password")
	release()
	// Once the existing factor window expires, legitimate recovery still bypasses the paused auth budget.
	e.A.mu.Lock()
	b := e.A.limits[budget]
	b.Until = time.Now().Add(-time.Second)
	e.A.limits[budget] = b
	e.A.mu.Unlock()
	w := e.do("POST", "/login", randomToken(), rescueForm("sole_admin", testPassword, agTOTPNow(key)))
	e.check(w, 303)
	e.session(w)
	e.A.mu.Lock()
	count := e.A.limits[budget].Count
	e.A.mu.Unlock()
	if count != 1 || !e.A.limited(signInKey("sole_admin"), 10) || agUserSessions(e, id) != 2 ||
		agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Signed in with password and TOTP'", id) != 1 {
		t.Fatal("successful recovery was double charged, cleared the password pause or failed to create one session")
	}
}

func TestSoleAdminPausedSignInConcurrentRecoveryAllowance(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("sole_admin", "admin")
	key, _ := agEnableTOTP(e, id)
	pauseSignIn(t, e, "sole_admin")
	for range 9 {
		if !e.A.allow("totp-verify:"+id, 10) {
			t.Fatal("could not reserve nine factor attempts")
		}
	}
	code := agTOTPNow(key)
	// Exercise admission directly: only one concurrent request may reach the password-check stage.
	results := agRace(2, func(i int) (int, string) {
		handle := "sole_admin"
		if i == 1 {
			handle = "SOLE_ADMIN"
		}
		c := &actionCtx{A: e.A, W: httptest.NewRecorder(), R: httptest.NewRequest(http.MethodPost, "/login", nil),
			Form: rescueForm(handle, "wrong-long-password", code), Token: randomToken()}
		_, _, rescueID, release, err := authGate(c)
		if err == errRescueRefused {
			return 401, "refused"
		}
		if err != nil {
			return 500, err.Error()
		}
		defer release()
		if rescueID != id {
			return 500, "allowance belongs to a different account"
		}
		return 200, "admitted"
	})
	if got := agTally(t, results, 200, 401); got[200] != 1 || got[401] != 1 {
		t.Fatalf("concurrent recovery budget: %+v", got)
	}
	if len(passwordWork) != 0 || agUserSessions(e, id) != 1 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
		t.Fatal("recovery admission leaked a work slot or changed authentication state")
	}
}

func TestSoleAdminRecoveryBudgetSurvivesBogusHandleChurn(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("sole_admin", "admin")
	key, _ := agEnableTOTP(e, id)
	pauseSignIn(t, e, "sole_admin")
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", "wrong-long-password", agTOTPNow(key)), 401, "Invalid handle or password")
	budget := "totp-verify:" + id
	e.A.mu.Lock()
	started := e.A.limits[budget]
	e.A.mu.Unlock()
	// Installed /setup posts with fresh syntactically valid handles create cheap shared limiter
	// entries before their setup refusal. They must not evict the oldest, one-attempt factor bucket.
	anon := randomToken()
	for i := range 4096 + 32 {
		agExpect(e, "/setup", anon, url.Values{"handle": {"churn_" + strconv.Itoa(i)}, "password": {testPassword}}, 403, "Setup unavailable")
	}
	e.A.mu.Lock()
	retained, size := e.A.limits[budget], len(e.A.limits)
	e.A.mu.Unlock()
	if retained.Count != started.Count || !retained.Until.Equal(started.Until) || size != 4096 {
		t.Fatalf("bogus handles reset recovery allowance or grew table: before=%+v after=%+v size=%d", started, retained, size)
	}
	for range 9 {
		agExpect(e, "/login", randomToken(), rescueForm("sole_admin", "wrong-long-password", agTOTPNow(key)), 401, "Invalid handle or password")
	}
	if !e.A.limited(budget, 10) {
		t.Fatal("churn restored spent recovery attempts")
	}
	release := agHoldPasswordWork(t)
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, agTOTPNow(key)), 401, "Invalid handle or password")
	release()
	if agUserSessions(e, id) != 1 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
		t.Fatal("churn enabled a recovery session or spent a TOTP step")
	}
}

// Since A-571 any account with TOTP may rescue a paused sign-in (TestPausedRescueSignsInEveryTOTPAccount): other
// administrators and the role no longer matter, but an account without TOTP, a suspended one and an ambiguous handle
// are still refused before any factor or password check.
func TestSoleAdminPausedSignInRefusesIneligibleAccountsBeforeFactor(t *testing.T) {
	for _, tc := range []string{"disabled TOTP", "suspended owner", "ambiguous handle"} {
		t.Run(tc, func(t *testing.T) {
			e := newTestApp(t)
			id, _ := e.user("SoleAdmin", "admin")
			key, _ := agEnableTOTP(e, id)
			pauseSignIn(t, e, "SoleAdmin")
			handle := "SOLEADMIN"
			switch tc {
			case "disabled TOTP":
				agExec(e, "UPDATE users SET totp_enabled=false WHERE id=$1", id)
			case "suspended owner":
				agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", id)
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
	for _, changed := range []string{"password", "closure", "suspension", "TOTP"} {
		t.Run(changed, func(t *testing.T) {
			e := newTestApp(t)
			id, _ := e.user("sole_admin", "admin")
			key, _ := agEnableTOTP(e, id)
			checkedHash := passwordHash(e, id) // the hash whose password passed bcrypt before this change
			switch changed {
			case "password":
				agExec(e, "UPDATE users SET password_hash='replacement-hash' WHERE id=$1", id)
			case "closure":
				agExec(e, "UPDATE users SET closed_at=now() WHERE id=$1", id)
			case "suspension":
				agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", id)
			case "TOTP":
				agExec(e, "UPDATE users SET totp_enabled=false WHERE id=$1", id)
			}
			c := &actionCtx{A: e.A, W: httptest.NewRecorder(), R: httptest.NewRequest(http.MethodPost, "/login", nil), Token: randomToken()}
			if _, err := e.A.rescueSignIn(c, id, checkedHash, agTOTPNow(key), "", false); err != errRescueRefused {
				t.Fatalf("stale %s passed locked recovery check: %v", changed, err)
			}
			if agLimited(e, "totp-verify:"+id) || agUserSessions(e, id) != 1 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
				t.Fatal("stale account consumed factor or signed in")
			}
		})
	}
}

// rescueWithCAPTCHA adds a fresh CAPTCHA to form for a new anonymous browser and returns that browser's token.
func rescueWithCAPTCHA(e *testEnv, form url.Values) (string, url.Values) {
	e.t.Helper()
	anon := randomToken()
	id := e.captcha("/login", anon)
	form = maps.Clone(form)
	form.Set("captcha_id", id)
	form.Set("captcha", e.A.captchaAnswer(id))
	return anon, form
}

// A-534 signaling and main's throttling share the recovery path: solved CAPTCHAs do not exempt wrong
// passwords from the factor budget, and valid recovery still works within its remaining allowance.
func TestSoleAdminRescueWrongPasswordsSpendFactorBudgetWithCAPTCHA(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("sole_admin", "admin")
	key, _ := agEnableTOTP(e, id)
	pauseSignIn(t, e, "sole_admin")
	agExec(e, "UPDATE settings SET value='true' WHERE key='captcha_required'")
	for i := range 9 {
		anon, form := rescueWithCAPTCHA(e, rescueForm("sole_admin", "wrong-rescue-password-"+string(rune('a'+i)), agTOTPNow(key)))
		agExpect(e, "/login", anon, form, 401, "Invalid handle or password")
	}
	e.A.mu.Lock()
	count := e.A.limits["totp-verify:"+id].Count
	e.A.mu.Unlock()
	if count != 9 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 || agUserSessions(e, id) != 1 {
		t.Fatal("wrong rescue passwords escaped their factor budget, spent a TOTP step or made a session")
	}
	anon, form := rescueWithCAPTCHA(e, rescueForm("sole_admin", testPassword, agTOTPNow(key)))
	w := e.do("POST", "/login", anon, form)
	e.check(w, 303)
	e.session(w)
	if agUserSessions(e, id) != 2 || !e.A.limited(signInKey("sole_admin"), 10) || !e.A.limited("totp-verify:"+id, 10) {
		t.Fatal("rescue within remaining allowance did not sign in, escaped the factor cap or lifted the sign-in pause")
	}
}

// A-534: a wrong password and the right password with a wrong code get the same answer, byte for byte, so the
// rescue path never confirms a password on its own.
func TestSoleAdminRescueRefusalsIndistinguishable(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("sole_admin", "admin")
	key, _ := agEnableTOTP(e, id)
	pauseSignIn(t, e, "sole_admin")
	browser := randomToken() // one browser for both, so the re-rendered form's CSRF token is the same
	wrongPassword := e.do("POST", "/login", browser, rescueForm("sole_admin", "wrong-long-password", agTOTPNow(key)))
	wrongCode := e.do("POST", "/login", browser, rescueForm("sole_admin", testPassword, e.wrongTOTP(id)))
	if wrongPassword.Code != 401 || wrongCode.Code != wrongPassword.Code {
		t.Fatalf("statuses differ or are not 401: %d vs %d", wrongPassword.Code, wrongCode.Code)
	}
	if !bytes.Equal(wrongPassword.Body.Bytes(), wrongCode.Body.Bytes()) {
		t.Fatalf("bodies differ:\n%s\n---\n%s", wrongPassword.Body.String(), wrongCode.Body.String())
	}
	if !reflect.DeepEqual(wrongPassword.Header(), wrongCode.Header()) {
		t.Fatalf("headers differ: %v vs %v", wrongPassword.Header(), wrongCode.Header())
	}
	if c := wrongPassword.Header().Values("Set-Cookie"); len(c) != 0 {
		t.Fatalf("refusal sets a cookie: %q", c)
	}
	if agUserSessions(e, id) != 1 {
		t.Fatal("a refusal signed in")
	}
	for range 9 { // with the wrong password above, ten; the wrong code spent nothing (A-571)
		agExpect(e, "/login", browser, rescueForm("sole_admin", "wrong-long-password", agTOTPNow(key)), 401, "Invalid handle or password")
	}
	exhausted := e.do("POST", "/login", browser, rescueForm("sole_admin", testPassword, agTOTPNow(key)))
	if exhausted.Code != wrongPassword.Code || !bytes.Equal(exhausted.Body.Bytes(), wrongPassword.Body.Bytes()) ||
		!reflect.DeepEqual(exhausted.Header(), wrongPassword.Header()) {
		t.Fatal("exhausted recovery budget exposed a different refusal")
	}
}

// A-534: rescue refusals are the operator's signal of guessing on this path: counted and logged at most once a
// minute, count only (no handle, account id or client), as TestSignInLimiterLogsCountOnly checks for sign-in.
func TestSoleAdminRescueRefusalsLogged(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("sole_admin", "admin")
	key, _ := agEnableTOTP(e, id)
	logs := captureLog(t)
	const line = "paused sign-in rescue refused attempts="
	pauseSignIn(t, e, "sole_admin")
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, ""), 429, signInPausedText)
	if got := logs.lines(line); len(got) != 0 {
		t.Fatalf("ordinary sign-in refusals logged as rescue refusals: %q", got)
	}
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", "wrong-long-password", agTOTPNow(key)), 401, "Invalid handle or password")
	if got := logs.lines(line); len(got) != 1 || got[0] != line+"1" {
		t.Fatalf("after the first refusal: %q", got)
	}
	// Refusals within the same minute add no line; both kinds are counted toward the next one.
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", "wrong-long-password", agTOTPNow(key)), 401, "Invalid handle or password")
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, e.wrongTOTP(id)), 401, "Invalid handle or password")
	if got := logs.lines(line); len(got) != 1 {
		t.Fatalf("more than one line within a minute: %q", got)
	}
	e.A.mu.Lock()
	e.A.rescueLogAt = time.Now().Add(-61 * time.Second)
	e.A.mu.Unlock()
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, e.wrongTOTP(id)), 401, "Invalid handle or password")
	if got := logs.lines(line); len(got) != 2 || got[1] != line+"3" {
		t.Fatalf("after a minute: %q", got)
	}
	// Pre-bcrypt exhaustion remains a count-only operator signal and is charged only once. Two wrong passwords above
	// spent two attempts and the wrong codes none (A-571), so eight more spend the allowance.
	for range 8 {
		agExpect(e, "/login", randomToken(), rescueForm("sole_admin", "wrong-long-password", agTOTPNow(key)), 401, "Invalid handle or password")
	}
	e.A.mu.Lock()
	e.A.rescueLogAt = time.Now().Add(-61 * time.Second)
	e.A.mu.Unlock()
	agExpect(e, "/login", randomToken(), rescueForm("sole_admin", testPassword, agTOTPNow(key)), 401, "Invalid handle or password")
	if got := logs.lines(line); len(got) != 3 || got[2] != line+"9" {
		t.Fatalf("exhausted recovery refusal was missing or double counted: %q", got)
	}
	all := strings.ToLower(logs.String())
	for _, s := range []string{"sole_admin", strings.ToLower(id)} {
		if strings.Contains(all, s) {
			t.Fatalf("log names the account: %q", logs.String())
		}
	}
}
