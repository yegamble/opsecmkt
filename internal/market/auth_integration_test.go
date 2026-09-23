package market

import (
	"image/png"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var recoveryCodePattern = regexp.MustCompile(`<li>([a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4})</li>`)

// totpCodeFor returns the code for step offset d from now using the user's stored secret (active or pending).
func (e *testEnv) totpCodeFor(userID string, d int64) (string, int64) {
	e.t.Helper()
	var active, pending string
	if err := e.DB.QueryRow("SELECT totp_secret,totp_pending FROM users WHERE id=$1", userID).Scan(&active, &pending); err != nil {
		e.t.Fatal(err)
	}
	if active == "" {
		active = pending
	}
	secret, err := e.A.open("totp", active)
	if err != nil {
		e.t.Fatal(err)
	}
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		e.t.Fatal(err)
	}
	step := totpStep(time.Now()) + d
	return hotp(key, uint64(step), totpDigits), step
}

// wrongTOTP returns a 6-digit code that is not valid anywhere in the acceptance window.
func (e *testEnv) wrongTOTP(userID string) string {
	valid := map[string]bool{}
	for d := int64(-2); d <= 2; d++ {
		c, _ := e.totpCodeFor(userID, d)
		valid[c] = true
	}
	for _, c := range []string{"000000", "111111", "222222", "333333"} {
		if !valid[c] {
			return c
		}
	}
	e.t.Fatal("no wrong code")
	return ""
}

func (e *testEnv) auditHas(userID, fragment string) bool {
	e.t.Helper()
	var n int
	if err := e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE '%'||$2||'%'", userID, fragment).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n > 0
}

// passwordLogin posts /login for handle and returns the pending-login cookie (fails if a session is issued instead).
func (e *testEnv) passwordLogin(handle string) (string, *http.Cookie) {
	e.t.Helper()
	anon := randomToken()
	w := e.do("POST", "/login", anon, url.Values{"handle": {handle}, "password": {testPassword}})
	e.check(w, 303)
	pending := e.cookie(w, "pending")
	if w.Header().Get("Location") != "/challenge" || pending == "" || e.cookie(w, "session") != "" {
		e.t.Fatalf("login bypassed the challenge: %v", w.Header())
	}
	return anon, &http.Cookie{Name: "pending", Value: pending}
}

func TestTOTPEnrollmentChallengeAndRecovery(t *testing.T) {
	e := newTestApp(t)
	id, sess := e.user("totp_user", "buyer")
	body := func(path string) string {
		w := e.do("GET", path, sess, nil)
		e.check(w, 200)
		return w.Body.String()
	}
	if b := body("/account"); !strings.Contains(b, "Not enrolled") || strings.Contains(b, ">Enabled<") {
		t.Fatal("account page must say Not enrolled before enrollment")
	}
	e.check(e.do("POST", "/totp/activate", sess, url.Values{"code": {"123456"}}), 409)
	w := e.do("POST", "/totp/enroll", sess, nil)
	e.check(w, 303)
	if w.Header().Get("Location") != "/totp" {
		t.Fatal("enroll should return to /totp")
	}
	var pending string
	var enabled bool
	e.DB.QueryRow("SELECT totp_pending,totp_enabled FROM users WHERE id=$1", id).Scan(&pending, &enabled)
	secret, err := e.A.open("totp", pending)
	if err != nil || enabled || strings.Contains(pending, secret) {
		t.Fatalf("pending secret must be sealed and inactive: %v enabled=%v", err, enabled)
	}
	if b := body("/totp"); !strings.Contains(b, groupSecret(secret)) || !strings.Contains(b, "otpauth://totp/") || !strings.Contains(b, "No QR code") {
		t.Fatal("pending secret not shown for manual entry")
	}
	if !strings.Contains(body("/account"), "setup not confirmed") {
		t.Fatal("unconfirmed enrollment must not look enabled")
	}
	e.check(e.do("POST", "/totp/activate", sess, url.Values{"code": {e.wrongTOTP(id)}}), 400)
	if e.DB.QueryRow("SELECT totp_enabled FROM users WHERE id=$1", id).Scan(&enabled); enabled {
		t.Fatal("wrong code activated TOTP")
	}
	code, step := e.totpCodeFor(id, 0)
	e.check(e.do("POST", "/totp/activate", sess, url.Values{"code": {code}}), 303)
	var hashes []string
	rows, _ := e.DB.Query("SELECT code_hash FROM recovery_codes WHERE user_id=$1 AND used_at IS NULL", id)
	for rows.Next() {
		var h string
		rows.Scan(&h)
		hashes = append(hashes, h)
	}
	rows.Close()
	if len(hashes) != 10 {
		t.Fatalf("%d recovery codes", len(hashes))
	}
	page := body("/totp")
	codes := recoveryCodePattern.FindAllStringSubmatch(page, -1)
	if len(codes) != 10 || !strings.Contains(page, "Shown once") {
		t.Fatalf("recovery codes not shown once after activation: %d", len(codes))
	}
	stored := strings.Join(hashes, ",")
	for _, c := range codes {
		if !strings.Contains(stored, recoveryHash(c[1])) || strings.Contains(stored, strings.ReplaceAll(c[1], "-", "")) {
			t.Fatal("recovery codes must be stored only as hashes")
		}
	}
	if recoveryCodePattern.MatchString(body("/totp")) {
		t.Fatal("recovery codes shown a second time")
	}
	if b := body("/account"); !strings.Contains(b, "<dd>Enabled</dd>") || !strings.Contains(b, "<dd>10</dd>") {
		t.Fatal("account page must say Enabled with 10 recovery codes")
	}
	e.check(e.do("POST", "/totp/enroll", sess, nil), 409)
	if !e.auditHas(id, "Enabled TOTP") {
		t.Fatal("activation not audited")
	}

	// Password alone never signs in; the activation code cannot be replayed; the next step's code works.
	anon, pc := e.passwordLogin("totp_user")
	e.check(e.do("GET", "/account", anon, nil, pc), 303)
	if !strings.Contains(e.do("GET", "/challenge", anon, nil, pc).Body.String(), `name="method" value="totp"`) {
		t.Fatal("challenge page lacks the TOTP form")
	}
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {e.wrongTOTP(id)}}, pc), 401)
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {code}}, pc), 401)
	next, _ := e.totpCodeFor(id, step+1-totpStep(time.Now()))
	w = e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {next}}, pc)
	e.check(w, 303)
	e.check(e.do("GET", "/account", e.session(w), nil), 200)

	// Recovery codes: single use, any case/format, audited with the remaining count.
	anon, pc = e.passwordLogin("totp_user")
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {codes[0][1]}}, pc), 303)
	anon, pc = e.passwordLogin("totp_user")
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {codes[0][1]}}, pc), 401)
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {strings.ToUpper(strings.ReplaceAll(codes[1][1], "-", ""))}}, pc), 303)
	if !e.auditHas(id, "recovery code to sign in (8 remaining)") {
		t.Fatal("recovery sign-in not audited with remaining count")
	}

	// Replacing recovery codes needs a current code and invalidates every old code.
	e.DB.Exec("UPDATE users SET totp_last_step=0 WHERE id=$1", id) // simulate the passage of time for the replay guard
	e.check(e.do("POST", "/totp/recovery", sess, url.Values{"code": {e.wrongTOTP(id)}}), 401)
	now, _ := e.totpCodeFor(id, 0)
	e.check(e.do("POST", "/totp/recovery", sess, url.Values{"code": {now}}), 303)
	fresh := recoveryCodePattern.FindAllStringSubmatch(body("/totp"), -1)
	if len(fresh) != 10 {
		t.Fatal("new recovery codes not shown")
	}
	anon, pc = e.passwordLogin("totp_user")
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {codes[2][1]}}, pc), 401)

	// Disabling needs the password and a code; afterwards password sign-in is direct again.
	e.check(e.do("POST", "/totp/disable", sess, url.Values{"password": {"wrong-password-123"}, "code": {fresh[0][1]}}), 401)
	e.check(e.do("POST", "/totp/disable", sess, url.Values{"password": {testPassword}, "code": {"nonsense"}}), 401)
	e.check(e.do("POST", "/totp/disable", sess, url.Values{"password": {testPassword}, "code": {fresh[0][1]}}), 303)
	var left int
	e.DB.QueryRow("SELECT totp_enabled,(SELECT count(*) FROM recovery_codes WHERE user_id=$1) FROM users WHERE id=$1", id).Scan(&enabled, &left)
	if enabled || left != 0 || !e.auditHas(id, "Turned off TOTP") {
		t.Fatalf("disable: enabled=%v codes=%d", enabled, left)
	}
	w = e.do("POST", "/login", randomToken(), url.Values{"handle": {"totp_user"}, "password": {testPassword}})
	e.check(w, 303)
	if w.Header().Get("Location") != "/" || e.cookie(w, "session") == "" {
		t.Fatal("login after disabling TOTP should sign in directly")
	}
}

func TestTOTPConcurrentReplay(t *testing.T) {
	e := newTestApp(t)
	id, sess := e.user("race_user", "buyer")
	e.check(e.do("POST", "/totp/enroll", sess, nil), 303)
	code, _ := e.totpCodeFor(id, 0)
	e.check(e.do("POST", "/totp/activate", sess, url.Values{"code": {code}}), 303)
	e.DB.Exec("UPDATE users SET totp_last_step=0 WHERE id=$1", id)
	type attempt struct {
		anon string
		pc   *http.Cookie
	}
	var tries []attempt
	for range 4 {
		anon, pc := e.passwordLogin("race_user")
		tries = append(tries, attempt{anon, pc})
	}
	var wg sync.WaitGroup
	codes := make(chan int, len(tries))
	for _, a := range tries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- e.do("POST", "/challenge", a.anon, url.Values{"method": {"totp"}, "code": {code}}, a.pc).Code
		}()
	}
	wg.Wait()
	close(codes)
	ok := 0
	for c := range codes {
		if c == 303 {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("one TOTP code signed in %d times", ok)
	}
}

var captchaField = regexp.MustCompile(`name="captcha_id" value="([0-9a-f]{64})"`)

func (e *testEnv) captcha(page, session string) string {
	e.t.Helper()
	w := e.do("GET", page, session, nil)
	e.check(w, 200)
	m := captchaField.FindStringSubmatch(w.Body.String())
	if m == nil {
		e.t.Fatalf("%s has no CAPTCHA", page)
	}
	return m[1]
}

func TestCaptchaOnRegisterAndLogin(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("cap_admin", "admin")
	e.user("cap_login", "buyer")
	_, buyer := e.user("cap_buyer", "buyer")
	e.check(e.do("POST", "/admin/captcha", buyer, url.Values{"captcha_required": {"true"}}), 403)
	e.check(e.do("POST", "/admin/captcha", admin, url.Values{"captcha_required": {"maybe"}}), 400)
	e.check(e.do("POST", "/admin/captcha", admin, url.Values{"captcha_required": {"true"}}), 303)
	if !e.auditHas(adminID, "Turned on CAPTCHA") || !strings.Contains(e.do("GET", "/admin", admin, nil).Body.String(), "Turn CAPTCHA off") {
		t.Fatal("captcha toggle not audited or not shown")
	}

	anon := randomToken()
	id := e.captcha("/register", anon)
	w := e.do("GET", "/captcha?id="+id, anon, nil)
	e.check(w, 200)
	if w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("image headers %v", w.Header())
	}
	if _, err := png.Decode(w.Body); err != nil {
		t.Fatal(err)
	}
	e.check(e.do("GET", "/captcha?id="+id, randomToken(), nil), 404) // bound to the issuing session
	e.check(e.do("GET", "/captcha?id="+randomToken(), anon, nil), 404)
	reg := func(handle, id, answer string) url.Values {
		return url.Values{"handle": {handle}, "password": {"a-long-buyer-password"}, "captcha_id": {id}, "captcha": {answer}}
	}
	e.check(e.do("POST", "/register", anon, url.Values{"handle": {"cap_new"}, "password": {"a-long-buyer-password"}}), 400)
	e.check(e.do("POST", "/register", anon, reg("cap_new", id, "WRONG1")), 400)
	e.check(e.do("POST", "/register", anon, reg("cap_new", id, e.A.captchaAnswer(id))), 400) // consumed by the wrong attempt
	e.check(e.do("GET", "/captcha?id="+id, anon, nil), 404)
	id = e.captcha("/register", anon)
	e.check(e.do("POST", "/register", randomToken(), reg("cap_new", id, e.A.captchaAnswer(id))), 400) // other session
	id = e.captcha("/register", anon)
	w = e.do("POST", "/register", anon, reg("cap_new", id, strings.ToLower(e.A.captchaAnswer(id))))
	e.check(w, 303)
	e.session(w)

	login := func(session, id, answer string) int {
		return e.do("POST", "/login", session, url.Values{"handle": {"cap_login"}, "password": {testPassword}, "captcha_id": {id}, "captcha": {answer}}).Code
	}
	anon = randomToken()
	id = e.captcha("/login", anon)
	if c := login(anon, id, "WRONG1"); c != 400 {
		t.Fatalf("wrong captcha on login: %d", c)
	}
	id = e.captcha("/login", anon)
	e.DB.Exec("UPDATE captchas SET expires=now()-interval '1 second' WHERE id=$1", id)
	if c := login(anon, id, e.A.captchaAnswer(id)); c != 400 {
		t.Fatalf("expired captcha on login: %d", c)
	}
	id = e.captcha("/login", anon)
	if c := login(anon, id, e.A.captchaAnswer(id)); c != 303 {
		t.Fatalf("correct captcha on login: %d", c)
	}

	// 30 challenges per session per ten minutes; then the form says why instead of showing an image.
	flood := randomToken()
	for range 30 {
		e.captcha("/login", flood)
	}
	if b := e.do("GET", "/login", flood, nil).Body.String(); captchaField.MatchString(b) || !strings.Contains(b, "Too many CAPTCHA images") {
		t.Fatal("captcha issuance not rate limited")
	}

	e.check(e.do("POST", "/admin/captcha", admin, url.Values{"captcha_required": {"false"}}), 303)
	if !e.auditHas(adminID, "Turned off CAPTCHA") {
		t.Fatal("turning the captcha off was not audited")
	}
	if b := e.do("GET", "/login", randomToken(), nil).Body.String(); strings.Contains(b, "captcha_id") {
		t.Fatal("captcha shown while turned off")
	}
	if c := login(randomToken(), "", ""); c != 303 {
		t.Fatalf("login without captcha while off: %d", c)
	}
}

func TestSetupSkipsCaptcha(t *testing.T) {
	e := newTestApp(t)
	e.DB.Exec("UPDATE settings SET value='true' WHERE key='captcha_required'")
	e.setup()
}
