package market

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A-126: an anonymous visitor sent to /login from a protected page returns to that page after signing in, registering
// or finishing the second sign-in step. The return target (next) is only a registered GET page with its id (or the
// /messages recipient) query; anything else signs in to / as before.

// a126Login posts /login for handle with testPassword and next; it returns the response and the anonymous token used.
func a126Login(e *testEnv, handle, next string) (string, *http.Cookie, string) {
	e.t.Helper()
	anon := randomToken()
	w := e.do("POST", "/login", anon, url.Values{"handle": {handle}, "password": {testPassword}, "next": {next}})
	e.check(w, 303)
	var pc *http.Cookie
	if p := e.cookie(w, "pending"); p != "" {
		pc = &http.Cookie{Name: "pending", Value: p}
	}
	return anon, pc, w.Header().Get("Location")
}

func TestSignInReturnTargets(t *testing.T) {
	e := newTestApp(t)
	buyerID, _ := e.user("a126_pw", "buyer")
	vendorID, _ := e.user("a126_vendor", "vendor")
	e.enableOrderPGP(buyerID, vendorID)
	product := e.product(vendorID, "digital")
	checkout := "/checkout?id=" + product

	// Anonymous visits to protected pages go to /login?next=<the page>, and /login carries it.
	for path, want := range map[string]string{
		checkout:                       "/login?next=" + url.QueryEscape(checkout),
		"/orders":                      "/login?next=%2Forders",
		"/messages?to=a126_vendor":     "/login?next=" + url.QueryEscape("/messages?to=a126_vendor"),
		"/order?id=abc&page=2":         "/login?next=" + url.QueryEscape("/order?id=abc"),
		"/order?id=%2F%2Fevil.test":    "/login",
		"/messages?to=%0d%0aSet-X:%20": "/login",
	} {
		w := e.do("GET", path, randomToken(), nil)
		e.check(w, 303)
		if loc := w.Header().Get("Location"); loc != want {
			t.Errorf("GET %s redirected to %q, want %q", path, loc, want)
		}
	}
	escapedCheckout := strings.ToLower(url.QueryEscape(checkout)) // html/template writes lower-case %-escapes
	page := e.body("GET", "/login?next="+url.QueryEscape(checkout), randomToken(), nil, 200)
	if !strings.Contains(page, `<input type="hidden" name="next" value="`+checkout+`">`) || !strings.Contains(page, "Sign in to see Checkout.") ||
		!strings.Contains(page, `href="/register?next=`+escapedCheckout+`"`) {
		t.Fatalf("/login does not carry next:\n%s", page)
	}
	if page = e.body("GET", "/login?next=%2Forders", randomToken(), nil, 200); !strings.Contains(page, "Sign in to see Orders.") {
		t.Fatal(`/login?next=/orders does not say "Sign in to see Orders."`)
	}
	page = e.body("GET", "/register?next="+url.QueryEscape(checkout), randomToken(), nil, 200)
	if !strings.Contains(page, `<input type="hidden" name="next" value="`+checkout+`">`) || !strings.Contains(page, `href="/login?next=`+escapedCheckout+`"`) ||
		!strings.Contains(page, `<a href="/login?next=`+escapedCheckout+`">Sign in</a>`) {
		t.Fatalf("/register does not carry next:\n%s", page)
	}

	// Accepted targets are rebuilt from the page name and its id/to query only.
	for next, want := range map[string]string{
		checkout:                          checkout,
		"/orders":                         "/orders",
		"/account":                        "/account",
		"/messages?to=a126_vendor":        "/messages?to=a126_vendor",
		"/order?id=abc&page=2&next=//x.y": "/order?id=abc",
		"/catalog":                        "/catalog",
	} {
		if _, _, loc := a126Login(e, "a126_pw", next); loc != want {
			t.Errorf("next=%q signed in to %q, want %q", next, loc, want)
		}
	}
	// Everything else is refused: the sign-in lands on / and /login shows no return target.
	for _, next := range []string{
		"", "/", "https://evil.test/", "http://evil.test", "//evil.test/checkout", "///evil.test", "/\\evil.test", "\\\\evil.test",
		"/%2F%2Fevil.test", "/%2fevil.test", "/%5Cevil.test", "%2F%2Fevil.test", "http:/checkout", "javascript:alert(1)", "data:text/html,x",
		"checkout?id=abc", " /checkout", "/checkout\r\nLocation: https://evil.test", "/checkout%0d%0aSet-Cookie:x=1", "/checkout\t",
		"/Checkout", "/checkout/", "/checkout?id=//evil.test", "/checkout?id=a&id=b", "/checkout?id=%zz", "/checkout#top", "/checkout;x",
		"/orders/pay", "/logout", "/login", "/register", "/setup", "/challenge", "/challenge/pgp", "/captcha?id=x", "/static/app.css",
		"/nonexistent", "/messages?to=a", "/@evil.test", "/." + strings.Repeat("a", 300),
	} {
		if _, _, loc := a126Login(e, "a126_pw", next); loc != "/" {
			t.Errorf("next=%q signed in to %q, want /", next, loc)
		}
		page := e.body("GET", "/login?next="+url.QueryEscape(next), randomToken(), nil, 200)
		if strings.Contains(page, `name="next"`) || strings.Contains(page, "Sign in to see") || strings.Contains(page, "evil.test") {
			t.Errorf("/login?next=%q carries a refused target", next)
		}
	}

	// Registration returns to the page too.
	anon := randomToken()
	w := e.do("POST", "/register", anon, url.Values{"handle": {"a126_new"}, "password": {testPassword}, "next": {checkout}})
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != checkout || e.cookie(w, "session") == "" {
		t.Fatalf("register landed on %q, want %q with a session", loc, checkout)
	}
	w = e.do("POST", "/register", randomToken(), url.Values{"handle": {"a126_new2"}, "password": {testPassword}, "next": {"//evil.test"}})
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Fatalf("register with a refused next landed on %q", loc)
	}
	// The signed-in checkout renders: the redirect was to a real page.
	if body := e.body("GET", checkout, e.session(e.do("POST", "/login", randomToken(), url.Values{"handle": {"a126_pw"}, "password": {testPassword}})), nil, 200); !strings.Contains(body, `action="/orders"`) {
		t.Fatal("checkout page did not render for the signed-in buyer")
	}
}

// The pending sign-in keeps the target through /challenge (TOTP, recovery code, PGP code); a next sent to /challenge
// itself is ignored.
func TestSignInReturnThroughChallenge(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("a126_v2", "vendor")
	checkout := "/checkout?id=" + e.product(vendorID, "digital")

	t.Run("totp", func(t *testing.T) {
		id, _ := e.user("a126_totp", "buyer")
		key, _ := agEnableTOTP(e, id)
		anon, pc, loc := a126Login(e, "a126_totp", checkout)
		if loc != "/challenge?next="+url.QueryEscape(checkout) || pc == nil {
			t.Fatalf("TOTP sign-in went to %q", loc)
		}
		body := e.body("GET", loc, anon, nil, 200, pc)
		if !strings.Contains(body, `action="/challenge?next=`+url.QueryEscape(checkout)+`"`) {
			t.Fatalf("TOTP challenge form dropped the URL return target:\n%s", body)
		}
		// A next posted to /challenge does not replace the pending sign-in's.
		w := e.do("POST", "/challenge?next=%2Faccount", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}, "next": {"/account"}}, pc)
		e.check(w, 303)
		if loc := w.Header().Get("Location"); loc != checkout || e.cookie(w, "session") == "" {
			t.Fatalf("TOTP challenge landed on %q, want %q", loc, checkout)
		}
	})
	t.Run("totp-no-next", func(t *testing.T) {
		id, _ := e.user("a126_totp0", "buyer")
		key, _ := agEnableTOTP(e, id)
		anon, pc, _ := a126Login(e, "a126_totp0", "//evil.test")
		w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}, "next": {checkout}}, pc)
		e.check(w, 303)
		if loc := w.Header().Get("Location"); loc != "/" {
			t.Fatalf("TOTP challenge without a pending target landed on %q, want /", loc)
		}
	})
	t.Run("stored-target-rechecked", func(t *testing.T) {
		id, _ := e.user("a126_totp1", "buyer")
		key, _ := agEnableTOTP(e, id)
		anon, pc, _ := a126Login(e, "a126_totp1", checkout)
		agExec(e, "UPDATE pending_logins SET next='//evil.test' WHERE token_hash=$1", digest(pc.Value))
		w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, pc)
		e.check(w, 303)
		if loc := w.Header().Get("Location"); loc != "/" {
			t.Fatalf("a stored target that fails the check landed on %q, want /", loc)
		}
	})
	t.Run("recovery", func(t *testing.T) {
		id, _ := e.user("a126_rec", "buyer")
		_, recovery := agEnableTOTP(e, id)
		a154Codes(e, id, 1)
		anon, pc, _ := a126Login(e, "a126_rec", checkout)
		w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}}, pc)
		e.check(w, 303)
		want := "/totp?next=" + url.QueryEscape(checkout) // with the one-shot signin-recovery notice (A-287)
		if loc := w.Header().Get("Location"); loc != want || noticeSetCookie(w) == nil || noticeSetCookie(w).Value != "signin-recovery" {
			t.Fatalf("recovery sign-in landed on %q, want %q", loc, want)
		}
		body := e.body("GET", want, e.session(w), nil, 200, noticeSetCookie(w))
		if !strings.Contains(body, "1 recovery code remains") || !strings.Contains(body, `<a class="button primary" href="`+strings.ReplaceAll(checkout, "&", "&amp;")+`">Continue to Checkout</a>`) {
			t.Fatalf("/totp after a recovery sign-in lacks the notice or the continue link:\n%s", body)
		}
		// Without the sign-in notice (a stale or crafted link) there is no continue link, and a refused target never shows.
		if body := e.body("GET", "/totp?next="+url.QueryEscape(checkout), e.session(w), nil, 200); strings.Contains(body, "Continue to") {
			t.Fatal("/totp shows a continue link without a recovery sign-in")
		}
		if body := e.body("GET", "/totp?next=%2F%2Fevil.test", e.session(w), nil, 200, noticeSetCookie(w)); strings.Contains(body, "Continue to") || strings.Contains(body, "evil.test") {
			t.Fatal("/totp shows a refused target")
		}
	})
	t.Run("recovery-totp-off", func(t *testing.T) {
		id, _ := e.user("a126_off", "buyer")
		_, recovery := agEnableTOTP(e, id)
		anon, pc, _ := a126Login(e, "a126_off", "/orders")
		w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}, "turn_off_totp": {"1"}}, pc)
		e.check(w, 303)
		if loc := w.Header().Get("Location"); loc != "/totp?next=%2Forders" || noticeSetCookie(w) == nil || noticeSetCookie(w).Value != "signin-totp-off" {
			t.Fatalf("recovery turn-off sign-in landed on %q", loc)
		}
		if body := e.body("GET", "/totp?next=%2Forders", e.session(w), nil, 200, noticeSetCookie(w)); !strings.Contains(body, `href="/orders">Continue to Orders</a>`) {
			t.Fatal("/totp after turning TOTP off lacks the continue link")
		}
	})
	t.Run("pgp", func(t *testing.T) {
		id, _ := e.user("a126_pgp", "buyer")
		key, pub := testPGPKey(t, "a126")
		agEnablePGP(e, id, pub)
		anon, pc, loc := a126Login(e, "a126_pgp", checkout)
		if loc != "/challenge?next="+url.QueryEscape(checkout) {
			t.Fatalf("PGP sign-in went to %q", loc)
		}
		body := e.body("GET", loc, anon, nil, 200, pc)
		if !strings.Contains(body, `action="/challenge/pgp?next=`+url.QueryEscape(checkout)+`"`) {
			t.Fatalf("PGP request form dropped the URL return target:\n%s", body)
		}
		w := e.do("POST", "/challenge/pgp", anon, nil, pc)
		e.check(w, 303)
		if loc := w.Header().Get("Location"); loc != "/challenge?next="+url.QueryEscape(checkout) {
			t.Fatalf("code request went to %q", loc)
		}
		code := testSignInCode(t, key, agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(pc.Value)))
		w = e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {code}}, pc)
		e.check(w, 303)
		if loc := w.Header().Get("Location"); loc != checkout || e.cookie(w, "session") == "" {
			t.Fatalf("PGP challenge landed on %q, want %q", loc, checkout)
		}
	})
}

func TestExpiredChallengeSignInKeepsReturnTarget(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("a258_expired", "buyer")
	p := agPending(e, id)
	agExec(e, "UPDATE pending_logins SET next=$1,expires=now()-interval '1 second' WHERE token_hash=$2", "/orders", digest(p.Value))
	body := e.body("GET", "/challenge", randomToken(), nil, 200, p)
	if !strings.Contains(body, `<a class="button" href="/login?next=%2Forders">Sign in</a>`) || strings.Contains(body, `value="totp"`) {
		t.Fatalf("expired challenge did not preserve its safe target or still offered verification:\n%s", body)
	}
	agExec(e, "UPDATE pending_logins SET next=$1 WHERE token_hash=$2", "//evil.test", digest(p.Value))
	body = e.body("GET", "/challenge", randomToken(), nil, 200, p)
	if !strings.Contains(body, `<a class="button" href="/login">Sign in</a>`) || strings.Contains(body, "evil.test") {
		t.Fatalf("expired challenge carried an unsafe target:\n%s", body)
	}
}

func TestChallengeURLKeepsReturnTargetWithoutPendingRow(t *testing.T) {
	e := newTestApp(t)
	e.user("a258_swept", "buyer")
	target := "/checkout?id=" + strings.Repeat("a", 64)
	challengeURL := "/challenge?next=" + url.QueryEscape(target)
	wantLink := `<a class="button" href="/login?next=` + url.QueryEscape(target) + `">Sign in</a>`

	// The pending cookie can expire in the browser before the visitor returns to this URL.
	body := e.body("GET", challengeURL, randomToken(), nil, 200)
	if !strings.Contains(body, wantLink) || strings.Contains(body, `value="totp"`) {
		t.Fatalf("challenge without a cookie did not preserve the validated return target:\n%s", body)
	}

	// A scheduled expiry sweep can delete the row before the browser follows the same challenge URL.
	id, _ := e.user("a258_swept2", "buyer")
	p := agPending(e, id)
	agExec(e, "UPDATE pending_logins SET next=$1 WHERE token_hash=$2", target, digest(p.Value))
	agExec(e, "DELETE FROM pending_logins WHERE token_hash=$1", digest(p.Value))
	body = e.body("GET", challengeURL, randomToken(), nil, 200, p)
	if !strings.Contains(body, wantLink) || strings.Contains(body, `value="totp"`) {
		t.Fatalf("swept challenge did not preserve the validated return target:\n%s", body)
	}

	for _, raw := range []string{"//evil.test", "/login", "/challenge", "/orders/pay", "/checkout?id=a&id=b"} {
		body = e.body("GET", "/challenge?next="+url.QueryEscape(raw), randomToken(), nil, 200)
		if !strings.Contains(body, `<a class="button" href="/login">Sign in</a>`) || strings.Contains(body, "evil.test") {
			t.Errorf("unsafe challenge next %q was retained:\n%s", raw, body)
		}
	}
}

func TestChallengeURLCannotReplaceActivePendingTarget(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("a258_active", "buyer")
	key, _ := agEnableTOTP(e, id)
	pc := agPending(e, id)
	target := "/checkout?id=" + strings.Repeat("b", 64)
	agExec(e, "UPDATE pending_logins SET next=$1 WHERE token_hash=$2", target, digest(pc.Value))
	attackerTarget := "/account"
	body := e.body("GET", "/challenge?next="+url.QueryEscape(attackerTarget), randomToken(), nil, 200, pc)
	if !strings.Contains(body, `action="/challenge?next=`+url.QueryEscape(target)+`"`) || strings.Contains(body, "next=%2Faccount") {
		t.Fatalf("active pending target did not override the challenge URL:\n%s", body)
	}
	w := e.do("POST", "/challenge?next="+url.QueryEscape(attackerTarget), randomToken(), url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}, "next": {attackerTarget}}, pc)
	e.check(w, 303)
	if got := w.Header().Get("Location"); got != target {
		t.Fatalf("tampered challenge URL/form changed the stored destination to %q, want %q", got, target)
	}
}

// A-269: with the CAPTCHA on, a wrong answer on /login or /register re-renders the form from the POST; it keeps the
// posted return target (through the same check), so answering the new challenge still lands on the page.
func TestSignInReturnAfterWrongCaptcha(t *testing.T) {
	e := newTestApp(t)
	e.user("a269_buyer", "buyer")
	e.mustExec("UPDATE settings SET value='true' WHERE key='captcha_required'")
	// /login says where the sign-in leads; /register carries the target in its link back to /login.
	for form, carried := range map[string]string{"/login": "Sign in to see Orders.", "/register": `href="/login?next=%2forders"`} {
		handle := map[string]string{"/login": "a269_buyer", "/register": "a269_new"}[form]
		anon := randomToken()
		id := e.captcha(form+"?next=%2Forders", anon)
		w := e.do("POST", form, anon, url.Values{"handle": {handle}, "password": {testPassword}, "next": {"/orders"}, "captcha_id": {id}, "captcha": {"WRONG1"}})
		e.check(w, 400)
		retry := wrongCaptchaRetry(e, w, form, id, "/orders")
		if b := w.Body.String(); !strings.Contains(b, `<input type="hidden" name="next" value="/orders">`) || !strings.Contains(b, carried) {
			t.Fatalf("wrong CAPTCHA answer on %s dropped next:\n%s", form, b)
		}
		w = e.do("POST", form, anon, url.Values{"handle": {handle}, "password": {testPassword}, "next": {"/orders"}, "captcha_id": {retry}, "captcha": {e.A.captchaAnswer(retry)}})
		e.check(w, 303)
		if loc := w.Header().Get("Location"); loc != "/orders" || e.cookie(w, "session") == "" {
			t.Fatalf("%s after a wrong CAPTCHA answer landed on %q, want /orders with a session", form, loc)
		}

		// A refused target posted with a wrong answer renders no return target on the retry form.
		anon = randomToken()
		id = e.captcha(form, anon)
		w = e.do("POST", form, anon, url.Values{"handle": {handle + "x"}, "password": {testPassword}, "next": {"//evil.test"}, "captcha_id": {id}, "captcha": {"WRONG1"}})
		e.check(w, 400)
		wrongCaptchaRetry(e, w, form, id)
		if b := w.Body.String(); strings.Contains(b, `name="next"`) || strings.Contains(b, "Sign in to see") || strings.Contains(b, "evil.test") {
			t.Fatalf("wrong CAPTCHA answer on %s carries a refused target:\n%s", form, b)
		}
	}
}
