package market

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A-725: the human check's three modes. Default ("under_attack"): no image on sign-in until a global attack signal,
// a written question on registration. "true": always the image. "false": no check. DB integration.

const (
	attackImageCopy    = "This market is seeing many failed sign-ins, so signing in needs this image for now."
	attackFallbackCopy = "If you cannot read it, wait and try again: the image goes away by itself at most 30 minutes after the failed sign-ins stop."
)

var captchaQuestion = regexp.MustCompile(`<label>([^<]*\?)<input name="captcha"`)

func setCaptchaMode(e *testEnv, mode string) {
	agExec(e, "UPDATE settings SET value=$1 WHERE key='captcha_required'", mode)
}

// endSignInAttack ends both ingresses' holds, as the clock passing them would.
func endSignInAttack(e *testEnv) {
	e.A.mu.Lock()
	defer e.A.mu.Unlock()
	past := time.Now().Add(-time.Second)
	e.A.signInAttackPublic.until, e.A.signInAttackPrivate.until = past, past
}

func tripSignInAttack(ctx context.Context, a *App) {
	for range signInAttackFailures {
		a.noteSignInFailure(ctx)
	}
}

func signInForm(handle, password string, extra ...string) url.Values {
	f := url.Values{"handle": {handle}, "password": {password}}
	for i := 0; i+1 < len(extra); i += 2 {
		f.Set(extra[i], extra[i+1])
	}
	return f
}

// Default mode, no signal: sign-in shows no image and asks for none, and the contact line for someone locked out
// (A-703) is on the page.
func TestDefaultModeSignInHasNoImageStep(t *testing.T) {
	e := newTestApp(t)
	e.user("dm_buyer", "buyer")
	setCaptchaMode(e, captchaModeUnderAttack)
	agExec(e, "INSERT INTO settings(key,value) VALUES('staff_contact',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value", staffContactValue)
	page := html.UnescapeString(e.body("GET", "/login", randomToken(), nil, 200))
	mustNotContain(t, page, "captcha-image", `name="captcha_id"`, "Human check")
	mustContain(t, page, "Locked out or reporting a stolen account? Contact the market staff: "+staffContactValue+".")
	// A stale id on a form that now asks for no check: no new one to answer, and nothing to go back for (A-758).
	stale := html.UnescapeString(e.body("POST", "/login", randomToken(), signInForm("dm_buyer", testPassword, "captcha_id", randomToken(), "captcha", "X"), 400))
	mustContain(t, stale, "This CAPTCHA or question expired or was already used.")
	mustNotContain(t, stale, "Answer the new one", "Go back", `name="captcha_id"`)
	w := e.do("POST", "/login", randomToken(), signInForm("dm_buyer", testPassword))
	e.check(w, 303)
	e.session(w)
}

// The signal, reached through the form itself: 100 failed passwords with no image. After that, sign-in needs the
// image, a stale form is shown it instead of a dead end, and no further password is checked without an answer.
func TestFailedPasswordsTurnTheImageOnForSignIn(t *testing.T) {
	e := newTestApp(t)
	e.user("dm_real", "buyer")
	setCaptchaMode(e, captchaModeUnderAttack)
	for i := range signInAttackFailures {
		// Fresh handles, so no handle's own budget of 10 is what stops this.
		w := e.do("POST", "/login", randomToken(), signInForm(fmt.Sprintf("flood_%d", i), "wrong-password-123"))
		if w.Code != 401 {
			t.Fatalf("failure %d: %d", i, w.Code)
		}
		if i == signInAttackFailures-2 && strings.Contains(w.Body.String(), "captcha-image") {
			t.Fatal("image on before the threshold")
		}
	}
	checks := countSignInPasswordChecks(t)
	session := randomToken()
	page := html.UnescapeString(e.body("GET", "/login", session, nil, 200))
	mustContain(t, page, "captcha-image", attackImageCopy, attackFallbackCopy, captchaHelpLine+" "+askOperatorLine)
	mustNotContain(t, page, "Locked out or reporting a stolen account?") // named once, in the image's help (A-703)

	// A form shown before the signal posts no challenge: the form comes back with the image and the handle, no password checked.
	w := e.do("POST", "/login", session, signInForm("dm_real", testPassword))
	body := html.UnescapeString(w.Body.String())
	if w.Code != 400 || !strings.Contains(body, "signing in needs an image") || !strings.Contains(body, `name="handle" value="dm_real"`) || !captchaField.MatchString(body) {
		t.Fatalf("form from before the signal: %d\n%s", w.Code, body)
	}
	id := captchaField.FindStringSubmatch(body)[1]
	w = e.do("POST", "/login", session, signInForm("dm_real", testPassword, "captcha_id", id, "captcha", "WRONG1"))
	id = wrongCaptchaRetry(e, w, "/login", id)
	if checks.Load() != 0 {
		t.Fatalf("%d passwords checked without a right answer", checks.Load())
	}
	w = e.do("POST", "/login", session, signInForm("dm_real", testPassword, "captcha_id", id, "captcha", e.A.captchaAnswer(id)))
	e.check(w, 303)
	e.session(w)
	if checks.Load() != 1 {
		t.Fatalf("%d password checks for one right answer", checks.Load())
	}
	// The answered id stays single use.
	w = e.do("POST", "/login", session, signInForm("dm_real", testPassword, "captcha_id", id, "captcha", e.A.captchaAnswer(id)))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "expired or was already used") {
		t.Fatalf("replayed answer: %d", w.Code)
	}

	// The hold ends by itself: no image again, and an open form that still carries an answer is not refused.
	stale := e.captcha("/login", session)
	endSignInAttack(e)
	mustNotContain(t, e.body("GET", "/login", randomToken(), nil, 200), "captcha-image")
	e.check(e.do("POST", "/login", session, signInForm("dm_real", testPassword, "captcha_id", stale, "captcha", "WRONG1")), 303)
	e.check(e.do("POST", "/login", randomToken(), signInForm("dm_real", testPassword)), 303)
}

// A flood that fills the password-check slots turns the image on as well: refusals are the A-151 signal that real
// sign-ins are failing, and they need no failed password at all.
func TestRefusedPasswordSlotsTurnTheImageOn(t *testing.T) {
	e := newTestApp(t)
	e.user("dm_slots", "buyer")
	setCaptchaMode(e, captchaModeUnderAttack)
	release := holdSlots(t, passwordWork)
	defer release()
	for i := range signInAttackBusy {
		if w := e.do("POST", "/login", randomToken(), signInForm("dm_slots", testPassword)); w.Code != 503 {
			t.Fatalf("refusal %d: %d", i, w.Code)
		}
	}
	mustContain(t, e.body("GET", "/login", randomToken(), nil, 200), "captcha-image")
	release()
	// Registration's slot refusals are not sign-in's signal.
	endSignInAttack(e)
	release2 := holdSlots(t, passwordWork)
	defer release2()
	session := randomToken()
	for i := range signInAttackBusy {
		id := e.captcha("/register", session)
		w := e.do("POST", "/register", session, signInForm(fmt.Sprintf("dm_new_%d", i), testPassword, "captcha_id", id, "captcha", e.A.captchaTextChallenge(id).Answers[0]))
		if w.Code != 503 {
			t.Fatalf("registration %d with no slot: %d", i, w.Code)
		}
	}
	mustNotContain(t, e.body("GET", "/login", randomToken(), nil, 200), "captcha-image")
}

// Refused sign-ins that a held registration slot used to hide (SEC102-1): a password that is right refunds its budget and
// never fails, so nothing else sees them. They have their own count, its threshold turns the existing signal on (image,
// one notification, one log line naming no one), and the ordinary counts stay untouched. No timing: the slots are held,
// as a registration flood holds them.
func TestRegistrationHeldSlotSignInFloodIsVisible(t *testing.T) {
	e := newTestApp(t)
	e.user("held_victim", "buyer")
	adminID, _ := e.user("held_admin", "admin")
	setCaptchaMode(e, captchaModeUnderAttack)
	logs := captureLog(t)
	releaseRegister, releasePassword := holdSlots(t, registerWork), holdSlots(t, passwordWork)
	defer releaseRegister()
	defer releasePassword()
	refused := func() {
		t.Helper()
		if w := e.do("POST", "/login", randomToken(), signInForm("held_victim", testPassword)); w.Code != 503 {
			t.Fatalf("sign-in with no slot: %d", w.Code)
		}
	}
	for range signInAttackHeldBusy - 1 {
		refused()
	}
	e.A.mu.Lock()
	before := e.A.signInAttackPublic
	e.A.mu.Unlock()
	if before.heldBusy != signInAttackHeldBusy-1 || before.busy != 0 || before.failures != 0 || before.trips != 0 {
		t.Fatalf("before the threshold: held %d busy %d failures %d trips %d", before.heldBusy, before.busy, before.failures, before.trips)
	}
	mustNotContain(t, e.body("GET", "/login", randomToken(), nil, 200), "captcha-image")
	if n := signalNotices(e, adminID); n != 0 {
		t.Fatalf("%d notifications before the threshold", n)
	}
	if strings.Contains(logs.String(), "sign-in attack signal on") {
		t.Fatalf("logged before the threshold:\n%s", logs.String())
	}

	refused()
	if !e.A.signInAttackOn(context.Background()) {
		t.Fatal("the threshold did not turn the signal on")
	}
	mustContain(t, html.UnescapeString(e.body("GET", "/login", randomToken(), nil, 200)), "captcha-image", attackImageCopy)
	if n := signalNotices(e, adminID); n != 1 {
		t.Fatalf("%d notifications at the threshold", n)
	}
	line := logs.String()
	if !strings.Contains(line, "sign-in attack signal on private_ingress=false route=registration_held_slot") {
		t.Fatalf("no log line naming the route:\n%s", line)
	}
	if strings.Contains(line, "held_victim") || strings.Contains(line, "127.0.0.1") {
		t.Fatalf("the log line names someone:\n%s", line)
	}
	e.A.mu.Lock()
	after := e.A.signInAttackPublic
	e.A.mu.Unlock()
	if after.trips != 1 || after.busy != 0 || after.failures != 0 {
		t.Fatalf("after the threshold: trips %d busy %d failures %d", after.trips, after.busy, after.failures)
	}
}

func signalNotices(e *testEnv, adminID string) int {
	return agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Sign-in attack signal on%'", adminID)
}

// The window and threshold of that count, and that the two counts of refused sign-ins do not add up.
func TestRegistrationHeldSlotCountStaysItsOwn(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	if signInAttackHeldBusy != 30 {
		t.Fatal("the stated threshold changed: update the operator guide and the /admin/security copy with it")
	}
	var s signInAttack
	for range signInAttackHeldBusy - 1 {
		if s.noteHeldBusy(now) {
			t.Fatal("on too early")
		}
	}
	for range signInAttackBusy - 1 {
		if s.noteBusy(now) {
			t.Fatal("the held count fed the ordinary one")
		}
	}
	if s.active(now) {
		t.Fatal("the two counts added up")
	}
	later := now.Add(signInAttackBusyWindow)
	if s.noteHeldBusy(later) || s.active(later) || s.heldBusy != 1 {
		t.Fatalf("refusals from an ended window still counted: %+v", s)
	}
	if v := s.view(later.Add(signInAttackBusyWindow)); v.HeldBusy != 0 {
		t.Fatalf("an ended window is still shown: %d", v.HeldBusy)
	}
	var r signInAttack
	for range signInAttackHeldBusy * 2 {
		r.noteRegisterBusy(now)
	}
	if r.active(now) || r.trips != 0 {
		t.Fatal("refused registrations turned the signal on")
	}
	if v := r.view(now); v.RegisterBusy != signInAttackHeldBusy*2 || v.HeldBusy != 0 || v.Busy != 0 {
		t.Fatalf("registration count: %+v", v)
	}
	if v := r.view(now.Add(signInAttackBusyWindow)); v.RegisterBusy != 0 {
		t.Fatalf("an ended window is still shown: %d", v.RegisterBusy)
	}
}

// The private ingress keeps its behaviour: its slots are its own, so a registration held on the public listener changes
// nothing there, and its refusals trip its own signal at the ordinary threshold.
func TestRegistrationHeldSlotLeavesPrivateIngressAlone(t *testing.T) {
	e := newTestApp(t)
	e.A.adminSocket = "/run/market-admin/http.sock"
	e.user("held_private", "buyer")
	setCaptchaMode(e, captchaModeUnderAttack)
	defer holdSlots(t, registerWork)()
	defer holdSlots(t, adminPasswordWork)()
	for i := range signInAttackBusy {
		if w := ingressDo(e, true, "POST", "/login", randomToken(), signInForm("held_private", testPassword)); w.Code != 503 {
			t.Fatalf("private refusal %d: %d", i, w.Code)
		}
	}
	mustContain(t, ingressDo(e, true, "GET", "/login", randomToken(), nil).Body.String(), "captcha-image")
	mustNotContain(t, ingressDo(e, false, "GET", "/login", randomToken(), nil).Body.String(), "captcha-image")
	e.A.mu.Lock()
	defer e.A.mu.Unlock()
	if e.A.signInAttackPrivate.heldBusy != 0 || e.A.signInAttackPublic.heldBusy != 0 {
		t.Fatalf("the held count moved: private %d public %d", e.A.signInAttackPrivate.heldBusy, e.A.signInAttackPublic.heldBusy)
	}
}

// A registration flood with no sign-in attempt puts no image on sign-in (SEC725-2's rule stays), however long it runs:
// no refused registration counts toward the signal. It is counted, for the panel.
func TestRegistrationOnlyFloodShowsNoSignInImage(t *testing.T) {
	e := newTestApp(t)
	e.user("regonly_install", "buyer")
	adminID, _ := e.user("regonly_admin", "admin")
	setCaptchaMode(e, captchaModeUnderAttack)
	defer holdSlots(t, registerWork)()
	defer holdSlots(t, passwordWork)()
	session := randomToken()
	for i := range signInAttackHeldBusy + 1 {
		id := e.captcha("/register", session)
		w := e.do("POST", "/register", session, signInForm(fmt.Sprintf("regonly_%d", i), testPassword, "captcha_id", id, "captcha", e.A.captchaTextChallenge(id).Answers[0]))
		if w.Code != 503 || !strings.Contains(w.Body.String(), "Registration is busy") {
			t.Fatalf("registration %d with no slot: %d", i, w.Code)
		}
	}
	mustNotContain(t, e.body("GET", "/login", randomToken(), nil, 200), "captcha-image")
	if n := signalNotices(e, adminID); n != 0 {
		t.Fatalf("%d notifications", n)
	}
	e.A.mu.Lock()
	defer e.A.mu.Unlock()
	s := e.A.signInAttackPublic
	if s.trips != 0 || s.heldBusy != 0 || s.busy != 0 || s.registerBusy != signInAttackHeldBusy+1 {
		t.Fatalf("trips %d held %d busy %d registrations %d", s.trips, s.heldBusy, s.busy, s.registerBusy)
	}
}

// /admin/security shows both counts, labelled as what they count, after a reload, whichever slot refused the
// registration, while the signal is off and while it is on; a reload moves neither. Only an administrator reads them.
func TestAdminSecurityShowsBusyCounts(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("counts_admin", "admin")
	_, buyer := e.user("counts_buyer", "buyer")
	e.check(e.do("GET", "/admin/security", buyer, nil), 403)
	setCaptchaMode(e, captchaModeUnderAttack)
	page := func() string { return html.UnescapeString(e.body("GET", "/admin/security", admin, nil, 200)) }
	counts := func(signIns, registrations int) string {
		return fmt.Sprintf("%d of %d sign-ins refused while a registration held a password slot, and %d registrations refused because no slot was free.", signIns, signInAttackHeldBusy, registrations)
	}
	mustContain(t, page(), counts(0, 0))

	session := randomToken()
	register := func(handle string) {
		t.Helper()
		id := e.captcha("/register", session)
		w := e.do("POST", "/register", session, signInForm(handle, testPassword, "captcha_id", id, "captcha", e.A.captchaTextChallenge(id).Answers[0]))
		if w.Code != 503 || !strings.Contains(w.Body.String(), "Registration is busy") {
			t.Fatalf("registration with no slot: %d", w.Code)
		}
	}
	releasePassword := holdSlots(t, passwordWork)
	defer releasePassword()
	register("counts_no_password_slot") // the registration slot is free; every password slot is not
	releaseRegister := holdSlots(t, registerWork)
	defer releaseRegister()
	register("counts_no_register_slot")
	for range 3 {
		if w := e.do("POST", "/login", randomToken(), signInForm("counts_buyer", testPassword)); w.Code != 503 {
			t.Fatalf("sign-in with no slot: %d", w.Code)
		}
	}
	releaseRegister()
	releasePassword()
	mustContain(t, page(), counts(3, 2))
	mustContain(t, page(), counts(3, 2)) // after a reload

	// The signal's own line hides its counts while it is on; these stay, since the registrations may still be going on.
	tripSignInAttack(context.Background(), e.A)
	mustContain(t, page(), "Attack signal: on.", counts(3, 2))
}

// SEC102-2: a registration that can only be refused for its missing key is refused before the human check and the slot,
// so it spends no bcrypt, no question and no slot: with every slot held it still gets its own answer. A registration
// with the same question and a key then succeeds.
func TestRegisterWithoutRequiredKeyTakesNoSlot(t *testing.T) {
	e := newTestApp(t)
	e.user("nokey_install", "buyer")
	setCaptchaMode(e, captchaModeUnderAttack)
	hashes := countRegistrationHashes(t)
	const keyRequired = "A usable PGP public key is required"
	_, pub := testPGPKey(t, "nokey_vendor")
	session := randomToken()
	id := e.captcha("/register", session)
	answer := e.A.captchaTextChallenge(id).Answers[0]
	users := agInt(e, "SELECT count(*) FROM users")
	refused := func(handle string, extra ...string) {
		t.Helper()
		form := signInForm(handle, testPassword, append([]string{"captcha_id", id, "captcha", answer}, extra...)...)
		agExpect(e, "/register", session, form, 400, keyRequired)
	}
	unspent := func() {
		t.Helper()
		if n := agInt(e, "SELECT count(*) FROM captcha_spent"); n != 0 {
			t.Fatalf("%d questions spent", n)
		}
		if n := agInt(e, "SELECT count(*) FROM users"); n != users || hashes.Load() != 0 {
			t.Fatalf("%d accounts (was %d), %d hashes", n, users, hashes.Load())
		}
		e.A.mu.Lock()
		defer e.A.mu.Unlock()
		if s := e.A.signInAttackPublic; s.registerBusy != 0 {
			t.Fatalf("refused for want of a slot %d times", s.registerBusy)
		}
	}
	releaseRegister, releasePassword := holdSlots(t, registerWork), holdSlots(t, passwordWork)
	defer releaseRegister()
	defer releasePassword()
	refused("nokey_vendor", "account_type", "vendor")
	refused("nokey_vendor", "account_type", "vendor", "pgp", " \n ") // what the transaction's check reads as no key
	unspent()
	agExec(e, "UPDATE settings SET value='false' WHERE key=$1", settingAllowLoginWithoutPGP)
	refused("nokey_strict")
	unspent()
	agExec(e, "UPDATE settings SET value='true' WHERE key=$1", settingAllowLoginWithoutPGP)
	// A buyer on a market that requires no key still waits for a slot like any other registration.
	buyerSession := randomToken()
	buyerID := e.captcha("/register", buyerSession)
	agExpect(e, "/register", buyerSession, signInForm("nokey_buyer", testPassword, "captcha_id", buyerID, "captcha", e.A.captchaTextChallenge(buyerID).Answers[0]), 503, "Registration is busy")
	releaseRegister()
	releasePassword()
	w := e.do("POST", "/register", session, signInForm("nokey_vendor", testPassword, "captcha_id", id, "captcha", answer, "account_type", "vendor", "pgp", pub))
	e.check(w, 303)
	if n := agInt(e, "SELECT count(*) FROM users WHERE handle='nokey_vendor'"); n != 1 || hashes.Load() != 1 {
		t.Fatalf("the unspent question did not register: %d accounts, %d hashes", n, hashes.Load())
	}
	if len(registerWork) != 0 || len(passwordWork) != 0 {
		t.Fatalf("slots leaked: register %d password %d", len(registerWork), len(passwordWork))
	}
}

// The preflight reads the policy without a lock: a market that turns strict after it, while the registration hashes, is
// still refused by the check under the lock in the transaction, and no account is made.
func TestRegisterKeyCheckStillHoldsInTheTransaction(t *testing.T) {
	e := newTestApp(t)
	e.user("race_install", "buyer")
	setCaptchaMode(e, captchaModeOff)
	orig := generateAccountHash
	generateAccountHash = func(password []byte, cost int) ([]byte, error) {
		agExec(e, "UPDATE settings SET value='false' WHERE key=$1", settingAllowLoginWithoutPGP)
		return orig(password, cost)
	}
	t.Cleanup(func() { generateAccountHash = orig })
	agExpect(e, "/register", randomToken(), signInForm("race_strict", testPassword), 400, "A usable PGP public key is required")
	if n := agInt(e, "SELECT count(*) FROM users WHERE handle='race_strict'"); n != 0 {
		t.Fatalf("%d accounts", n)
	}
	if len(registerWork) != 0 || len(passwordWork) != 0 {
		t.Fatalf("slots leaked: register %d password %d", len(registerWork), len(passwordWork))
	}
}

// countRegistrationHashes counts the password hashes /register and /setup generate, as countSignInPasswordChecks counts
// sign-in's checks.
func countRegistrationHashes(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := generateAccountHash
	generateAccountHash = func(password []byte, cost int) ([]byte, error) {
		n.Add(1)
		return orig(password, cost)
	}
	t.Cleanup(func() { generateAccountHash = orig })
	return &n
}

// The public listener, with every header that claims the private onion, cannot put the image on the private
// sign-in, and failures there do not put it on the storefront (A-570's rule).
func TestPublicFailuresCannotPutTheImageOnAdminIngress(t *testing.T) {
	e := newTestApp(t)
	e.A.adminSocket = "/run/market-admin/http.sock"
	e.user("dm_someone", "buyer")
	setCaptchaMode(e, captchaModeUnderAttack)
	for i := range signInAttackFailures {
		w := ingressDo(e, false, "POST", "/login", randomToken(), signInForm(fmt.Sprintf("spoof_%d", i), "wrong-password-123"))
		if w.Code != 401 {
			t.Fatalf("spoofed public failure %d: %d", i, w.Code)
		}
	}
	login := func(private bool) string {
		t.Helper()
		w := ingressDo(e, private, "GET", "/login", randomToken(), nil)
		e.check(w, 200)
		return w.Body.String()
	}
	mustContain(t, login(false), "captcha-image")
	mustNotContain(t, login(true), "captcha-image")
	endSignInAttack(e)
	for i := range signInAttackFailures {
		if w := ingressDo(e, true, "POST", "/login", randomToken(), signInForm(fmt.Sprintf("onion_%d", i), "wrong-password-123")); w.Code != 401 {
			t.Fatalf("private failure %d: %d", i, w.Code)
		}
	}
	mustContain(t, login(true), "captcha-image")
	mustNotContain(t, login(false), "captcha-image")
}

// The per-handle budget and its pause hold in the default mode, with no image anywhere: 10 failures, then 429 with no
// password checked. The paused-handle rescue (A-571) still signs in, with no image and then behind it.
func TestPerHandleBudgetAndRescueHoldInDefaultMode(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("dm_paused", "buyer")
	key, _ := agEnableTOTP(e, id)
	setCaptchaMode(e, captchaModeUnderAttack)
	for i := range 10 {
		if w := e.do("POST", "/login", randomToken(), signInForm("dm_paused", "wrong-password-123")); w.Code != 401 {
			t.Fatalf("failure %d: %d", i, w.Code)
		}
	}
	checks := countSignInPasswordChecks(t)
	agExpect(e, "/login", randomToken(), signInForm("dm_paused", testPassword), 429, signInPausedText)
	if checks.Load() != 0 {
		t.Fatalf("a paused handle ran %d password checks", checks.Load())
	}
	w := e.do("POST", "/login", randomToken(), rescueForm("dm_paused", testPassword, agTOTPNow(key)))
	e.check(w, 303)
	e.session(w)

	// Under the signal the rescue is behind the image: a wrong answer checks no code and no password, so the right code
	// stays unused and signs in once the image is answered.
	second, _ := e.user("dm_paused_two", "buyer")
	secondKey, _ := agEnableTOTP(e, second)
	pauseSignIn(t, e, "dm_paused_two")
	tripSignInAttack(context.Background(), e.A)
	session := randomToken()
	cid := e.captcha("/login", session)
	form := rescueForm("dm_paused_two", testPassword, agTOTPNow(secondKey))
	form.Set("captcha_id", cid)
	form.Set("captcha", "WRONG1")
	cid = wrongCaptchaRetry(e, e.do("POST", "/login", session, form), "/login", cid)
	form.Set("captcha_id", cid)
	form.Set("captcha", e.A.captchaAnswer(cid))
	w = e.do("POST", "/login", session, form)
	e.check(w, 303)
	e.session(w)
}

// Registration in the default mode: a written question, no image, single use, bound to the session and expiring,
// while the refused form keeps what it kept before (A-514, A-722). The signal does not change it.
func TestRegisterTextChallenge(t *testing.T) {
	e := newTestApp(t)
	e.user("dm_install", "buyer") // any account: with none, every page redirects to /setup
	setCaptchaMode(e, captchaModeUnderAttack)
	session := randomToken()
	page := e.body("GET", "/register", session, nil, 200)
	mustNotContain(t, page, "captcha-image", "<img", `src="/captcha`)
	m := captchaQuestion.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no labelled question on /register:\n%s", page)
	}
	id := captchaField.FindStringSubmatch(page)[1]
	c := e.A.captchaTextChallenge(id)
	if html.UnescapeString(m[1]) != c.Question {
		t.Fatalf("page asks %q, the id's question is %q", m[1], c.Question)
	}
	mustNotContain(t, page, `value="`+c.Answers[0]+`"`)

	// No id, then a wrong answer: the form again with a new question, handle and key kept, no account, no budget.
	_, pub := testPGPKey(t, "dm_text")
	refused := func(w *httptest.ResponseRecorder, msg, spent string) string {
		t.Helper()
		b := html.UnescapeString(w.Body.String())
		if w.Code != 400 || !strings.Contains(b, msg) || !strings.Contains(b, `name="handle" value="dm_text"`) || !strings.Contains(b, "BEGIN PGP PUBLIC KEY BLOCK") || strings.Contains(b, "captcha-image") {
			t.Fatalf("refused registration: %d\n%s", w.Code, b)
		}
		next := captchaField.FindStringSubmatch(b)
		if next == nil || next[1] == spent {
			t.Fatalf("no new question:\n%s", b)
		}
		return next[1]
	}
	const askAgain = "Answer the new question below."
	id = refused(e.do("POST", "/register", session, signInForm("dm_text", testPassword, "pgp", pub)), askAgain, "")
	spent := id
	id = refused(e.do("POST", "/register", session, signInForm("dm_text", testPassword, "pgp", pub, "captcha_id", id, "captcha", "not an answer")), askAgain, id)
	if n := agInt(e, "SELECT count(*) FROM users WHERE handle='dm_text'"); n != 0 || limiterCount(e, "register:dm_text") != 0 {
		t.Fatalf("a refused check made %d accounts or spent budget", n)
	}
	// The spent id cannot be replayed with its right answer; another session cannot use a live one; an expired one is refused.
	expired := func(w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != 400 || !strings.Contains(w.Body.String(), "expired or was already used") {
			t.Fatalf("want an expired refusal: %d", w.Code)
		}
	}
	// The form comes back with a new question and what was typed, so the alert does not send the person away from it (A-758).
	again := e.do("POST", "/register", session, signInForm("dm_text", testPassword, "pgp", pub, "captcha_id", spent, "captcha", e.A.captchaTextChallenge(spent).Answers[0]))
	expired(again)
	if b := again.Body.String(); !strings.Contains(b, "Answer the new one below.") || !strings.Contains(b, `value="dm_text"`) || !strings.Contains(b, "BEGIN PGP PUBLIC KEY BLOCK") || strings.Contains(b, "Go back") || strings.Contains(b, "reload the page") {
		t.Fatalf("expired question's form: %s", b)
	}
	if m := captchaField.FindStringSubmatch(again.Body.String()); m == nil || m[1] == spent {
		t.Fatalf("no new question: %v", m)
	}
	expired(e.do("POST", "/register", randomToken(), signInForm("dm_text", testPassword, "captcha_id", id, "captcha", e.A.captchaTextChallenge(id).Answers[0])))
	old := e.A.newCaptchaID(digest(session), time.Now().Add(-time.Second))
	expired(e.do("POST", "/register", session, signInForm("dm_text", testPassword, "captcha_id", old, "captcha", e.A.captchaTextChallenge(old).Answers[0])))

	// The image's answer does not answer the question, so an id from /login's image is no way round it.
	tripSignInAttack(context.Background(), e.A)
	imageID := e.captcha("/login", session)
	id = refused(e.do("POST", "/register", session, signInForm("dm_text", testPassword, "pgp", pub, "captcha_id", imageID, "captcha", e.A.captchaAnswer(imageID))), askAgain, imageID)

	// The signal leaves registration on the question.
	mustNotContain(t, e.body("GET", "/register", randomToken(), nil, 200), "captcha-image")
	w := e.do("POST", "/register", session, signInForm("dm_text", testPassword, "pgp", pub, "captcha_id", id, "captcha", "  "+strings.ToUpper(e.A.captchaTextChallenge(id).Answers[0])+" "))
	e.check(w, 303)
	e.session(w)
	if n := agInt(e, "SELECT count(*) FROM users WHERE handle='dm_text'"); n != 1 {
		t.Fatalf("%d accounts", n)
	}
	expired(e.do("POST", "/register", session, signInForm("dm_text_two", testPassword, "captcha_id", id, "captcha", e.A.captchaTextChallenge(id).Answers[0])))
}

// "true" is the operator's forced image: on both forms with no signal, and the text question's answer is not
// accepted in its place. "false" asks nothing, signal or not.
func TestOperatorForcedAndOffModes(t *testing.T) {
	e := newTestApp(t)
	e.user("dm_forced", "buyer")
	setCaptchaMode(e, captchaModeAlways)
	session := randomToken()
	for _, path := range []string{"/login", "/register"} {
		page := html.UnescapeString(e.body("GET", path, session, nil, 200))
		mustContain(t, page, "captcha-image", "There is no audio or text alternative.", "reload the page for a new one")
		mustNotContain(t, page, attackImageCopy)
		// A form rendered again holds what was typed, so nothing on it says to reload (A-758).
		stale := html.UnescapeString(e.body("POST", path, session, signInForm("dm_forced", testPassword, "captcha_id", randomToken(), "captcha", "WRONG1"), 400))
		mustContain(t, stale, "Answer the new one below.", "captcha-image", `value="dm_forced"`, "a wrong answer gets a new one")
		mustNotContain(t, stale, "reload", "Go back")
		if captchaQuestion.MatchString(page) {
			t.Fatalf("%s asks a question while the image is forced", path)
		}
	}
	e.check(e.do("POST", "/login", randomToken(), signInForm("dm_forced", testPassword)), 400)
	id := e.captcha("/register", session)
	w := e.do("POST", "/register", session, signInForm("dm_forced_new", testPassword, "captcha_id", id, "captcha", e.A.captchaTextChallenge(id).Answers[0]))
	wrongCaptchaRetry(e, w, "/register", id)

	setCaptchaMode(e, captchaModeOff)
	tripSignInAttack(context.Background(), e.A)
	for _, path := range []string{"/login", "/register"} {
		mustNotContain(t, e.body("GET", path, randomToken(), nil, 200), `name="captcha_id"`, "captcha-image", "Human check")
	}
	e.check(e.do("POST", "/login", randomToken(), signInForm("dm_forced", testPassword)), 303)
}

// /setup stays exempt in every mode and under the signal.
func TestSetupNeverAsksTheHumanCheck(t *testing.T) {
	for _, mode := range []string{captchaModeAlways, captchaModeUnderAttack} {
		t.Run(mode, func(t *testing.T) {
			e := newTestApp(t)
			setCaptchaMode(e, mode)
			tripSignInAttack(context.Background(), e.A)
			mustNotContain(t, e.body("GET", "/setup", randomToken(), nil, 200), `name="captcha_id"`, "Human check")
			w := e.do("POST", "/setup", randomToken(), url.Values{"handle": {"dm_first_admin"}, "password": {testPassword}, "token": {e.A.setupToken}})
			e.check(w, 303)
		})
	}
}

// The operator's control: three modes, audited, with the password; the state and its cost to users are on
// /admin/security and flagged beside the section's link on /admin, and the no-contact warning follows the image.
func TestAdminSeesAndChoosesTheHumanCheckMode(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("dm_admin", "admin")
	_, buyer := e.user("dm_buyer2", "buyer")
	e.check(e.do("POST", "/admin/captcha", buyer, url.Values{"captcha_required": {"under_attack"}}), 403)
	e.check(e.do("POST", "/admin/captcha", admin, url.Values{"captcha_required": {"attack"}, "password": {testPassword}}), 400)
	e.check(e.do("POST", "/admin/captcha", admin, url.Values{"captcha_required": {"under_attack"}}), 400)
	e.check(e.do("POST", "/admin/captcha", admin, url.Values{"captcha_required": {"under_attack"}, "password": {testPassword}}), 303)
	if agStr(e, "SELECT value FROM settings WHERE key='captcha_required'") != "under_attack" ||
		!e.auditHas(adminID, "Set CAPTCHA to only under attack") || !e.auditHas(adminID, "required changed from false to under_attack") {
		t.Fatal("mode not saved or not audited")
	}
	page := func() string { return html.UnescapeString(e.body("GET", "/admin/security", admin, nil, 200)) }
	const (
		attackAlert = "Attack signal: on."
		overviewTag = "Sign-in is asking everyone for the image: the attack signal is on."
	)
	checked := `name="captcha_required" value="under_attack" checked`
	idle := page()
	mustContain(t, idle, checked, "Only under attack", "Always the image",
		"Attack signal: off.", "100 failed passwords", "10 refused password checks", "30 minutes",
		"The question on registration stops only simple bots",
		"about 100 wrong passwords, or about 14 simultaneous sign-in posts, every 30 minutes hold the image on", // SEC725-5
		"can test as many handles as they like", "no budget limits that",
		"A sign-in refused while a registration holds a password slot counts apart from those: 30 in one minute also turn the signal on. Refused registrations alone never do.") // SEC102-1
	mustNotContain(t, idle, attackAlert, captchaWarningMark, "on the private administrator onion: off")
	mustNotContain(t, e.body("GET", "/admin", admin, nil, 200), overviewTag)

	// The signal: the page says it is on, since and until when, what users see and that it may be deliberate.
	tripSignInAttack(context.Background(), e.A)
	on := page()
	mustContain(t, on, attackAlert, "Everyone signing in must read the image", "Someone may be holding this on deliberately",
		"about 100 wrong passwords, or about 14 simultaneous sign-in posts, every 30 minutes are enough",
		"Turned on 1 time since this process started", captchaWarningMark, captchaWarningUnset)
	e.A.mu.Lock()
	since, until := e.A.signInAttackPublic.since.UTC().Format(signInAttackTime), e.A.signInAttackPublic.until.UTC().Format(signInAttackTime)
	e.A.mu.Unlock()
	mustContain(t, on, "since "+since, "until "+until)
	mustContain(t, e.body("GET", "/admin", admin, nil, 200), overviewTag)
	// A contact that works without an account clears the warning, not the signal.
	agExec(e, "INSERT INTO settings(key,value) VALUES('staff_contact',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value", staffContactValue)
	mustContain(t, page(), attackAlert)
	mustNotContain(t, page(), captchaWarningMark)
	agExec(e, "DELETE FROM settings WHERE key='staff_contact'")

	// With a private onion its own state is shown too.
	e.A.adminSocket = "/run/market-admin/http.sock"
	private := ingressDo(e, true, "GET", "/admin/security", admin, nil)
	e.check(private, 200)
	mustContain(t, html.UnescapeString(private.Body.String()), "Attack signal on the private administrator onion: off.", "Attack signal: on.")
	e.A.adminSocket = ""

	// Off: the signal has no effect, and the operator is told.
	setCaptchaMode(e, captchaModeOff)
	off := page()
	mustContain(t, off, `name="captcha_required" value="false" checked`, "It has no effect in this mode")
	mustNotContain(t, off, attackAlert, captchaWarningMark)
	mustNotContain(t, e.body("GET", "/admin", admin, nil, 200), overviewTag)

	// Always: the image is forced, signal or not; the old warning applies at once.
	setCaptchaMode(e, captchaModeAlways)
	endSignInAttack(e)
	always := page()
	mustContain(t, always, `name="captcha_required" value="true" checked`, captchaWarningMark)
	setCaptchaMode(e, captchaModeUnderAttack)
	mustNotContain(t, page(), captchaWarningMark)
	e.check(e.do("POST", "/admin/captcha", admin, url.Values{"captcha_required": {"true"}, "password": {testPassword}}), 303)
	if !e.auditHas(adminID, "Turned on CAPTCHA for sign-in and registration") {
		t.Fatal("forcing the image is not audited as turning it on")
	}
}

// Migration 155: a market whose setting is still migration 010's untouched "true" moves to the new default; an
// operator's recorded choice to turn it on, and "false", stay. DB integration.
func TestMigration155KeepsAnOperatorsChoice(t *testing.T) {
	apply := func(t *testing.T, value string, audits ...string) string {
		t.Helper()
		db := schemaDB(t)
		ctx := context.Background()
		list, err := loadMigrations(migrationFiles)
		if err != nil {
			t.Fatal(err)
		}
		var before, only []migration
		for _, m := range list {
			switch {
			case m.Version < 155:
				before = append(before, m)
			case m.Version == 155:
				only = append(only, m)
			}
		}
		if len(only) != 1 {
			t.Fatalf("migration 155: %d files", len(only))
		}
		runInTx(t, db, func(tx *sql.Tx) error {
			if _, err := tx.Exec(schema); err != nil {
				return err
			}
			return applyMigrations(ctx, tx, before)
		})
		if _, err := db.Exec("UPDATE settings SET value=$1 WHERE key='captcha_required'", value); err != nil {
			t.Fatal(err)
		}
		for _, action := range audits {
			if _, err := db.Exec("INSERT INTO audit_events(action) VALUES($1)", action); err != nil {
				t.Fatal(err)
			}
		}
		rows := func() int {
			var n int
			if err := db.QueryRow("SELECT count(*) FROM audit_events").Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		count := rows()
		runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(ctx, tx, only) })
		if rows() != count {
			t.Fatal("the migration changed the audit trail")
		}
		var got string
		if err := db.QueryRow("SELECT value FROM settings WHERE key='captcha_required'").Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	const (
		on  = "Turned on CAPTCHA for sign-in and registration; required changed from false to true (confirmed with password)"
		off = "Turned off CAPTCHA for sign-in and registration; required changed from true to false (confirmed with password)"
	)
	for _, c := range []struct {
		name, value string
		audits      []string
		want        string
	}{
		{"untouched default moves", "true", nil, "under_attack"},
		{"an unrelated audit row does not count", "true", []string{"Turned on something else"}, "under_attack"},
		{"explicitly turned on stays", "true", []string{off, on}, "true"},
		{"explicitly off stays", "false", []string{off}, "false"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := apply(t, c.value, c.audits...); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
