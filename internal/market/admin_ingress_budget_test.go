package market

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A-570: requests through the private administrator ingress (AdminHandler) have their own limiter namespace and
// their own admission classes, so the public listener cannot pause or refuse an administrator's sign-in there.

// spoofAdminIngress adds everything a public caller could send to claim the private onion: its Host, the
// forwarding headers a proxy would set, a made-up ingress header and cookie, and a loopback peer address.
func spoofAdminIngress(r *http.Request) {
	onion := strings.Repeat("a", 56) + ".onion"
	r.Host = onion
	r.RemoteAddr = "127.0.0.1:9"
	for k, v := range map[string]string{
		"X-Tor-Onion-Role": "admin", "X-Forwarded-Host": onion, "X-Forwarded-For": "127.0.0.1", "X-Real-IP": "127.0.0.1",
		"Forwarded": "for=127.0.0.1;host=" + onion + ";proto=http", "X-Admin-Ingress": "1", "X-Forwarded-Proto": "http",
	} {
		r.Header.Set(k, v)
	}
	r.AddCookie(&http.Cookie{Name: "adm", Value: "1"})
}

// serveIngress serves r through the private ingress, or through the public listener with spoofed headers.
func serveIngress(a *App, private bool, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	if private {
		a.AdminHandler().ServeHTTP(w, r)
	} else {
		spoofAdminIngress(r)
		a.ServeHTTP(w, r)
	}
	return w
}

// ingressDo is e.do through the private ingress or the (spoofed) public listener.
func ingressDo(e *testEnv, private bool, method, path, token string, form url.Values, extra ...*http.Cookie) *httptest.ResponseRecorder {
	form = maps.Clone(form)
	if form == nil {
		form = url.Values{}
	}
	if method == "POST" {
		form.Set("csrf", e.A.csrf(token))
		if spec, ok := actions[path]; ok && (spec.FormNonce || spec.ConfirmedFormNonce || spec.OwnFormNonce) && !form.Has(formNonceField) {
			form.Set(formNonceField, formNonceFor(path))
		}
	}
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: token})
	for _, c := range extra {
		r.AddCookie(c)
	}
	return serveIngress(e.A, private, r)
}

// publicClasses are every admission class a public request can take.
func publicClasses() map[string]chan struct{} {
	return map[string]chan struct{}{"page": pageWork, "image": imageWork, "captcha": captchaWork, "anonymous post": anonPostWork,
		"signed-in post": userPostWork, "session lookup": sessionLookupWork, "password": passwordWork}
}

// adminIngressClasses are the private ingress's classes, by the name of the public class each replaces.
func adminIngressClasses() map[string]chan struct{} {
	return map[string]chan struct{}{"page": adminPageWork, "image": adminImageWork, "captcha": adminCaptchaWork, "anonymous post": adminAnonPostWork,
		"signed-in post": adminUserPostWork, "session lookup": adminSessionLookupWork, "password": adminPasswordWork}
}

func TestPublicListenerCannotPauseAdminIngressSignIn(t *testing.T) {
	e := newTestApp(t)
	e.A.adminSocket = "/run/market-admin/http.sock"
	id, _ := e.user("onion_admin", "admin")
	agEnableTOTP(e, id)
	login := func(private bool, password, code string, extra ...*http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		return ingressDo(e, private, "POST", "/login", randomToken(), url.Values{"handle": {"onion_admin"}, "password": {password}, "rescue_code": {code}}, extra...)
	}
	expect := func(w *httptest.ResponseRecorder, code int, msg, leg string) {
		t.Helper()
		if w.Code != code || !strings.Contains(refusalText(w.Body.String()), msg) {
			t.Fatalf("%s: status=%d body=%q, want %d containing %q", leg, w.Code, w.Body.String(), code, msg)
		}
	}
	// The public listener, with every header that claims the private onion, spends the handle's sign-in budget...
	for i := range 10 {
		expect(login(false, "wrong-password-"+string(rune('a'+i)), ""), 401, "Invalid handle or password", "public wrong password")
	}
	expect(login(false, testPassword, ""), 429, signInPausedText, "public right password once paused")
	// ...and, through the paused sign-in's rescue, the account's code budget (the right code with wrong passwords) and
	// the rescue path's own wrong-code budget (A-571), all in the public namespace. Wrong codes are refused before the
	// password check and never spend the code budget; once the rescue-code budget is spent, no code is checked.
	publicCode, _ := e.totpCodeFor(id, 0)
	for range 10 {
		expect(login(false, "wrong-password-z", publicCode), 401, "Invalid handle or password", "public rescue password guess")
	}
	wrong := e.wrongTOTP(id)
	for range 10 {
		expect(login(false, testPassword, wrong), 401, "Invalid handle or password", "public rescue code guess")
	}
	if e.A.limited("totp-verify:"+id, 11) || e.A.limited("adm:totp-verify:"+id, 1) {
		t.Fatal("wrong rescue codes spent a sign-in code budget")
	}
	if !e.A.limited(signInKey("onion_admin"), 10) || !e.A.limited("totp-verify:"+id, 10) || !e.A.limited(rescueCodeKey(id), 10) {
		t.Fatal("the public listener did not spend the public budgets")
	}
	if e.A.limited("adm:"+signInKey("onion_admin"), 1) || e.A.limited("adm:totp-verify:"+id, 1) || e.A.limited("adm:"+rescueCodeKey(id), 1) {
		t.Fatal("spoofed public requests spent the private ingress's budgets")
	}
	// Every public admission class is full as well; a post's session lookup gives up quickly here.
	defer func(d time.Duration) { postLookupWait = d }(postLookupWait)
	postLookupWait = 50 * time.Millisecond
	for _, sem := range publicClasses() {
		holdSlots(t, sem)
	}
	if w := ingressDo(e, false, "GET", "/login", randomToken(), nil); w.Code != 503 {
		t.Fatalf("public sign-in page with the public classes full: %d", w.Code)
	}
	expect(login(false, testPassword, ""), 503, "busy", "public sign-in with the public classes full")

	// Through the private ingress the administrator signs in at once: the page, its CAPTCHA image, the password and
	// the code on /challenge ...
	token := randomToken()
	if w := ingressDo(e, true, "GET", "/login", token, nil); w.Code != 200 {
		t.Fatalf("private sign-in page: %d", w.Code)
	}
	captcha := e.A.newCaptchaID(digest(token), time.Now().Add(captchaTTL))
	if w := ingressDo(e, true, "GET", "/captcha?id="+captcha, token, nil); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("private CAPTCHA image: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	w := login(true, testPassword, "")
	pending := e.cookie(w, "pending")
	if w.Code != 303 || pending == "" || !strings.HasPrefix(w.Header().Get("Location"), "/challenge") {
		t.Fatalf("private password step: %d %q %q", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	code, _ := e.totpCodeFor(id, 0)
	w = ingressDo(e, true, "POST", "/challenge", randomToken(), url.Values{"method": {"totp"}, "code": {code}}, &http.Cookie{Name: "pending", Value: pending})
	session := e.cookie(w, "session")
	if w.Code != 303 || session == "" {
		t.Fatalf("private /challenge: %d %q", w.Code, w.Body.String())
	}
	// ... and the password with the code on /login (A-549).
	code, _ = e.totpCodeFor(id, 1)
	if w = login(true, testPassword, code); w.Code != 303 || e.cookie(w, "session") == "" {
		t.Fatalf("private sign-in with an authenticator code: %d %q %q", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	if !e.A.limited(signInKey("onion_admin"), 10) || e.A.limited(signInKey("onion_admin"), 11) {
		t.Fatal("private sign-ins changed the public sign-in budget")
	}

	// The private ingress keeps budgets of its own: someone holding the client-authorization key is still limited.
	for range 8 { // every code is an attempt: the two accepted above and eight wrong ones spend the budget of 10
		w = login(true, testPassword, wrong)
		if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/challenge") || e.cookie(w, "session") != "" {
			t.Fatalf("private wrong code: %d %q", w.Code, w.Header().Get("Location"))
		}
	}
	code, _ = e.totpCodeFor(id, 0)
	expect(login(true, testPassword, code), 429, "Too many verification attempts", "private code once its budget is spent")
	for i := range 10 {
		expect(login(true, "wrong-password-"+string(rune('a'+i)), ""), 401, "Invalid handle or password", "private wrong password")
	}
	w = login(true, testPassword, "")
	expect(w, 429, signInPausedText, "private right password once paused")
	if e.cookie(w, "session") != "" || e.cookie(w, "pending") != "" {
		t.Fatal("paused private sign-in issued credentials")
	}
	if !e.A.limited(signInKey("onion_admin"), 10) || e.A.limited(signInKey("onion_admin"), 11) || !e.A.limited("totp-verify:"+id, 10) || e.A.limited("totp-verify:"+id, 11) {
		t.Fatal("private failures spent the public budgets")
	}
	// The security panel, still served with every public class full, lists both pauses, the private one marked.
	w = ingressDo(e, true, "GET", "/admin/security", session, nil)
	body := w.Body.String()
	if w.Code != 200 || strings.Count(body, "<strong>onion_admin</strong> — paused until") != 2 || strings.Count(body, "on the private administrator onion</li>") != 1 {
		t.Fatalf("security panel: %d %q", w.Code, body)
	}
}

// TestAdminIngressAdmissionReserved: with every public class full, private-ingress pages, CAPTCHA images, image
// routes, posts, session lookups and password work are still admitted; with every private class full they are
// refused while public requests are served; the sizes are pinned.
func TestAdminIngressAdmissionReserved(t *testing.T) {
	pa := admitPreviewApp(t)
	pa.adminSocket = "/run/market-admin/http.sock"
	a := testHTTPApp(false)
	a.adminSocket = pa.adminSocket
	admitTestAction(t)
	private := context.WithValue(context.Background(), adminIngressKey{}, true)
	get := func(isPrivate bool, path string) int {
		return serveIngress(pa, isPrivate, httptest.NewRequest("GET", path, nil)).Code
	}
	anonPost := func(isPrivate bool) int { return serveIngress(a, isPrivate, admitAnonPost(a, "/test/admit", nil)).Code }
	signedPost := func(isPrivate bool) int {
		r := httptest.NewRequest("POST", "/test/admit-signed", strings.NewReader(""))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if isPrivate {
			r = r.WithContext(private)
		} else {
			spoofAdminIngress(r)
		}
		r.ParseForm()
		w := httptest.NewRecorder()
		a.post(w, r, &User{ID: "u-admin", Role: "admin"}, strings.Repeat("a", 64))
		return w.Code
	}
	pages := []string{"/", "/login", "/captcha?id=preview", publicImageURL(strings.Repeat("a", 32), false)}
	defer func(d time.Duration) { postLookupWait = d }(postLookupWait)
	postLookupWait = 20 * time.Millisecond

	releases := []func(){}
	for _, sem := range publicClasses() {
		releases = append(releases, holdSlots(t, sem))
	}
	for _, path := range pages {
		if got := get(false, path); got != 503 {
			t.Errorf("public GET %s with the public classes full: %d", path, got)
		}
		if got := get(true, path); got == 503 {
			t.Errorf("private GET %s with the public classes full: %d", path, got)
		}
	}
	if anonPost(false) != 503 || signedPost(false) != 503 {
		t.Error("public posts were admitted with the public classes full")
	}
	if got := anonPost(true); got != 303 {
		t.Errorf("private anonymous post with the public classes full: %d", got)
	}
	if got := signedPost(true); got != 303 {
		t.Errorf("private signed-in post with the public classes full: %d", got)
	}
	if done, ok := takeLookupSlot(context.Background()); ok {
		done()
		t.Error("public session lookup admitted with its class full")
	}
	if done, ok := takeLookupSlot(private); !ok {
		t.Error("private session lookup refused with the public class full")
	} else {
		done()
	}
	if _, err := hashPassword("a-long-test-password"); err == nil {
		t.Error("public password hashing admitted with its class full")
	}
	if _, err := hashPasswordFor(private, "a-long-test-password"); err != nil {
		t.Errorf("private password hashing refused with the public class full: %v", err)
	}
	for _, release := range releases {
		release()
	}

	// The private classes are bounded too, and filling them leaves the public listener alone.
	for _, sem := range adminIngressClasses() {
		holdSlots(t, sem)
	}
	for _, path := range pages {
		if got := get(true, path); got != 503 {
			t.Errorf("private GET %s with the private classes full: %d", path, got)
		}
		if got := get(false, path); got == 503 {
			t.Errorf("public GET %s with the private classes full: %d", path, got)
		}
	}
	if anonPost(true) != 503 || signedPost(true) != 503 {
		t.Error("private posts were admitted with the private classes full")
	}
	if anonPost(false) != 303 || signedPost(false) != 303 {
		t.Error("public posts were refused with only the private classes full")
	}
	if done, ok := takeLookupSlot(private); ok {
		done()
		t.Error("private session lookup admitted with its class full")
	}
	if _, err := hashPasswordFor(private, "a-long-test-password"); err == nil {
		t.Error("private password hashing admitted with its class full")
	}
	if _, err := hashPassword("a-long-test-password"); err != nil {
		t.Errorf("public password hashing refused with only the private class full: %v", err)
	}

	for name, want := range map[string]int{"page": 8, "image": 8, "captcha": 2, "anonymous post": 4, "signed-in post": 4, "session lookup": 4, "password": 2} {
		if got := cap(adminIngressClasses()[name]); got != want {
			t.Errorf("private %s class has %d slots, want %d", name, got, want)
		}
	}
}

// The repository's ingress rule: a public-listener request cannot select the private ingress's budgets or
// classes with a Host, forwarding or made-up header, a cookie, a form field or a loopback peer. Only
// AdminHandler's context does.
func TestSpoofedHeadersCannotSelectAdminIngressBudgets(t *testing.T) {
	a := testHTTPApp(false)
	a.adminSocket = "/run/market-admin/http.sock"
	var seen []string
	withAction(t, "/test/ingress-key", actionSpec{Public: true, OwnTx: true, Run: func(c *actionCtx) (actionResult, error) {
		seen = append(seen, ingressKey(c.Ctx(), signInKey("onion_admin")))
		return actionResult{Redirect: "/done"}, nil
	}})
	post := func(private bool, form url.Values) *httptest.ResponseRecorder {
		return serveIngress(a, private, admitAnonPost(a, "/test/ingress-key", form))
	}
	for _, form := range []url.Values{nil, {"ingress": {"admin"}, "adm": {"1"}, "admin_ingress": {"true"}}} {
		if w := post(false, form); w.Code != 303 {
			t.Fatalf("spoofed public post: %d %q", w.Code, w.Body.String())
		}
	}
	if w := post(true, nil); w.Code != 303 {
		t.Fatalf("private post: %d", w.Code)
	}
	if want := []string{"auth:onion_admin", "auth:onion_admin", "adm:auth:onion_admin"}; !slices.Equal(seen, want) {
		t.Fatalf("limiter keys = %q, want %q", seen, want)
	}
	// The admission classes follow the same rule: with only the private classes full a spoofed public request is
	// served, and with the public classes full it is refused.
	pa := admitPreviewApp(t)
	pa.adminSocket = a.adminSocket
	var releases []func()
	for _, sem := range adminIngressClasses() {
		releases = append(releases, holdSlots(t, sem))
	}
	if w := serveIngress(pa, false, httptest.NewRequest("GET", "/login", nil)); w.Code != 200 {
		t.Fatalf("spoofed public page with the private classes full: %d", w.Code)
	}
	if w := post(false, nil); w.Code != 303 {
		t.Fatalf("spoofed public post with the private classes full: %d", w.Code)
	}
	for _, release := range releases {
		release()
	}
	holdSlots(t, pageWork)
	holdSlots(t, anonPostWork)
	if w := serveIngress(pa, false, httptest.NewRequest("GET", "/login", nil)); w.Code != 503 {
		t.Fatalf("spoofed public page with the public classes full: %d", w.Code)
	}
	if w := post(false, nil); w.Code != 503 {
		t.Fatalf("spoofed public post with the public classes full: %d", w.Code)
	}
	// Without ADMIN_SOCKET, AdminHandler serves nothing, so its context reaches no action.
	a.adminSocket = ""
	if w := post(true, nil); w.Code != 404 || len(seen) != 4 {
		t.Fatalf("AdminHandler without ADMIN_SOCKET: %d keys=%q", w.Code, seen)
	}
}

// The private namespace has its own table: a public table full of blocking entries, which refuses every new
// public key, refuses no private one, and the private table protects its code budgets from eviction as the
// public one does.
func TestAdminIngressLimiterTableIsSeparate(t *testing.T) {
	a := testHTTPApp(false)
	private := context.WithValue(context.Background(), adminIngressKey{}, true)
	until := time.Now().Add(10 * time.Minute)
	for i := range 4096 {
		a.limits["auth:junk_"+strconv.Itoa(i)] = bucket{Count: 10, Limit: 10, Until: until}
	}
	if a.allow(signInKey("onion_admin"), 10) {
		t.Fatal("a full public table admitted a new public key")
	}
	if !a.allow(ingressKey(private, signInKey("onion_admin")), 10) || len(a.limits) != 4096 || len(a.adminLimits) != 1 {
		t.Fatalf("private key refused or stored in the public table: public=%d private=%d", len(a.limits), len(a.adminLimits))
	}
	a.adminLimits = map[string]bucket{"adm:totp-verify:u1": {Count: 1, Limit: 10, Until: until}}
	for i := range 4095 {
		a.adminLimits["adm:auth:junk_"+strconv.Itoa(i)] = bucket{Count: 1, Limit: 10, Until: until}
	}
	if !a.allow(ingressKey(private, signInKey("fresh")), 10) {
		t.Fatal("a full private table of non-blocking entries refused a new key")
	}
	if _, ok := a.adminLimits["adm:totp-verify:u1"]; !ok {
		t.Fatal("a private code budget was evicted")
	}
}

// A public limiter table full of blocking entries refuses every new public key, as documented, but no step of an
// administrator's sign-in or password-confirmed change through the private ingress: its pending-login, code and
// confirmation budgets are namespaced too.
func TestFullPublicLimiterTableRefusesNoAdminIngressStep(t *testing.T) {
	e := newTestApp(t)
	e.A.adminSocket = "/run/market-admin/http.sock"
	id, _ := e.user("table_admin", "admin")
	agEnableTOTP(e, id)
	e.user("table_buyer", "buyer")
	until := time.Now().Add(10 * time.Minute)
	e.A.mu.Lock()
	for i := range 4096 {
		e.A.limits["auth:junk_"+strconv.Itoa(i)] = bucket{Count: 10, Limit: 10, Until: until}
	}
	e.A.mu.Unlock()
	if w := ingressDo(e, false, "POST", "/login", randomToken(), url.Values{"handle": {"table_buyer"}, "password": {testPassword}}); w.Code != 429 {
		t.Fatalf("public sign-in with the public table full: %d", w.Code)
	}
	w := ingressDo(e, true, "POST", "/login", randomToken(), url.Values{"handle": {"table_admin"}, "password": {testPassword}})
	pending := e.cookie(w, "pending")
	if w.Code != 303 || pending == "" {
		t.Fatalf("private password step: %d %q", w.Code, w.Body.String())
	}
	code, _ := e.totpCodeFor(id, -1)
	w = ingressDo(e, true, "POST", "/challenge", randomToken(), url.Values{"method": {"totp"}, "code": {code}}, &http.Cookie{Name: "pending", Value: pending})
	session := e.cookie(w, "session")
	if w.Code != 303 || session == "" {
		t.Fatalf("private /challenge: %d %q", w.Code, w.Body.String())
	}
	// TOTP management (totp:) and a password-confirmed change (confirm:), each with its code.
	code, _ = e.totpCodeFor(id, 0)
	if w = ingressDo(e, true, "POST", "/totp/recovery", session, url.Values{"code": {code}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("private recovery-code replacement: %d %q", w.Code, refusalText(w.Body.String()))
	}
	code, _ = e.totpCodeFor(id, 1)
	if w = ingressDo(e, true, "POST", "/account/password", session, pwForm(testPassword, testPassword+"-changed", "code", code)); w.Code != 303 {
		t.Fatalf("private password change: %d %q", w.Code, w.Body.String())
	}
	e.A.mu.Lock()
	public := len(e.A.limits)
	e.A.mu.Unlock()
	if public != 4096 {
		t.Fatalf("private steps wrote to the public table: %d entries", public)
	}
}

// fillPublicLimiter fills the public limiter table with blocking entries, so it refuses every new public key.
func fillPublicLimiter(e *testEnv) {
	until := time.Now().Add(10 * time.Minute)
	e.A.mu.Lock()
	defer e.A.mu.Unlock()
	for i := range 4096 {
		e.A.limits["challenge:junk_"+strconv.Itoa(i)] = bucket{Count: 10, Limit: 10, Until: until}
	}
}

// A PGP-only administrator's sign-in code and key-ownership steps through the private ingress use its namespace
// too (pgp-login:, pgp-challenge:, pgp-verify:), so a saturated public table refuses none of them.
func TestFullPublicLimiterTableRefusesNoAdminIngressPGPStep(t *testing.T) {
	e := newTestApp(t)
	e.A.adminSocket = "/run/market-admin/http.sock"
	id, _ := e.user("pgp_admin", "admin")
	_, pub := testPGPKey(t, "pgp_admin")
	agEnablePGP(e, id, pub)
	fillPublicLimiter(e)
	w := ingressDo(e, true, "POST", "/login", randomToken(), url.Values{"handle": {"pgp_admin"}, "password": {testPassword}})
	pending := &http.Cookie{Name: "pending", Value: e.cookie(w, "pending")}
	if w.Code != 303 || pending.Value == "" {
		t.Fatalf("private password step: %d %q", w.Code, refusalText(w.Body.String()))
	}
	if w = ingressDo(e, true, "POST", "/challenge/pgp", randomToken(), nil, pending); w.Code != 303 {
		t.Fatalf("private PGP sign-in code with the public table full: %d %q", w.Code, refusalText(w.Body.String()))
	}
	if agStr(e, "SELECT coalesce(pgp_challenge,'') FROM pending_logins WHERE token_hash=$1", digest(pending.Value)) == "" {
		t.Fatal("no PGP sign-in code was encrypted")
	}
	session := agSession(e, id)
	if w = ingressDo(e, true, "POST", "/pgp/challenge", session, url.Values{"kind": {"sign"}}); w.Code != 303 {
		t.Fatalf("private PGP ownership challenge with the public table full: %d %q", w.Code, refusalText(w.Body.String()))
	}
	if w = ingressDo(e, true, "POST", "/pgp/verify", session, url.Values{"signature": {"not a signature"}}); w.Code != 400 || !strings.Contains(refusalText(w.Body.String()), "Not verified") {
		t.Fatalf("private PGP proof with the public table full: %d %q", w.Code, refusalText(w.Body.String()))
	}
	e.A.mu.Lock()
	public := len(e.A.limits)
	e.A.mu.Unlock()
	if public != 4096 {
		t.Fatalf("private PGP steps wrote to the public table: %d entries", public)
	}
}

// A-159(a) and A-571 through the private ingress, for an administrator of a market with more than one: the public
// listener has spent the handle's sign-in budget, with the right code and wrong passwords the account's code budget,
// and with wrong codes the rescue path's own wrong-code budget, and the private ingress's sign-in budget is spent too;
// the paused sign-in's rescue on the private onion, whose wrong-code budget is its own, charges only private budgets
// and signs in.
func TestPausedRescueThroughAdminIngress(t *testing.T) {
	e := newTestApp(t)
	e.A.adminSocket = "/run/market-admin/http.sock"
	id, _ := e.user("rescue_onion", "admin")
	e.user("rescue_other_admin", "admin")
	agEnableTOTP(e, id)
	login := func(private bool, password, code string) *httptest.ResponseRecorder {
		return ingressDo(e, private, "POST", "/login", randomToken(), url.Values{"handle": {"rescue_onion"}, "password": {password}, "rescue_code": {code}})
	}
	wrong := e.wrongTOTP(id)
	for i := range 10 {
		if w := login(false, "wrong-password-"+string(rune('a'+i)), ""); w.Code != 401 {
			t.Fatalf("public wrong password: %d", w.Code)
		}
	}
	publicCode, _ := e.totpCodeFor(id, 0)
	for range 10 {
		if w := login(false, "wrong-password-z", publicCode); w.Code != 401 {
			t.Fatalf("public rescue password guess: %d", w.Code)
		}
	}
	for range 10 {
		if w := login(false, testPassword, wrong); w.Code != 401 {
			t.Fatalf("public rescue code guess: %d", w.Code)
		}
	}
	if e.A.limited("totp-verify:"+id, 11) || e.A.limited("adm:totp-verify:"+id, 1) {
		t.Fatal("wrong rescue codes on the public listener spent a sign-in code budget")
	}
	if !e.A.limited(rescueCodeKey(id), 10) || e.A.limited("adm:"+rescueCodeKey(id), 1) {
		t.Fatal("wrong rescue codes on the public listener did not spend only the public rescue-code budget")
	}
	for i := range 10 {
		if w := login(true, "wrong-password-"+string(rune('a'+i)), ""); w.Code != 401 {
			t.Fatalf("private wrong password: %d", w.Code)
		}
	}
	if !e.A.limited("totp-verify:"+id, 10) || !e.A.limited("adm:"+signInKey("rescue_onion"), 10) || e.A.limited("adm:totp-verify:"+id, 1) {
		t.Fatal("budgets not in the expected state before the rescue")
	}
	// A wrong code on the private onion is refused without spending the private code budget either; it spends one
	// attempt of the private rescue-code budget, which the spent public one does not block.
	if w := login(true, testPassword, wrong); w.Code != 401 || e.A.limited("adm:totp-verify:"+id, 1) || limiterCountIn(e, "adm:"+rescueCodeKey(id)) != 1 {
		t.Fatalf("private rescue code guess: %d", w.Code)
	}
	code, _ := e.totpCodeFor(id, 0)
	w := login(true, testPassword, code)
	if w.Code != 303 || e.cookie(w, "session") == "" {
		t.Fatalf("private paused sign-in rescue: %d %q", w.Code, refusalText(w.Body.String()))
	}
	if !e.A.limited("adm:totp-verify:"+id, 1) || e.A.limited("adm:totp-verify:"+id, 2) || e.A.limited("totp-verify:"+id, 11) {
		t.Fatal("the private rescue did not charge exactly one private code attempt")
	}
	if limiterCountIn(e, "adm:"+rescueCodeKey(id)) != 1 {
		t.Fatal("the right code was charged to the private rescue-code budget")
	}
}

// limiterCountIn is the number of attempts key holds in whichever limiter table it lives in (limitTable).
func limiterCountIn(e *testEnv, key string) int {
	e.A.mu.Lock()
	defer e.A.mu.Unlock()
	return e.A.limitTable(key)[key].Count
}
