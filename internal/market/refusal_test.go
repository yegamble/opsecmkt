package market

import (
	"html"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// refusalPost is e.do with a Referer header (httptest's Host is example.com).
func refusalPost(e *testEnv, method, path, token string, form url.Values, referer string) *httptest.ResponseRecorder {
	e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form = cloneValues(form)
	if method == "POST" {
		form.Set("csrf", e.A.csrf(token))
		if spec, ok := actions[path]; ok && (spec.FormNonce || spec.ConfirmedFormNonce || spec.OwnFormNonce) && !form.Has(formNonceField) {
			form.Set(formNonceField, formNonceFor(path))
		}
	}
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if referer != "" {
		r.Header.Set("Referer", referer)
	}
	r.AddCookie(&http.Cookie{Name: "session", Value: token})
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	return w
}

var refusalAlert = regexp.MustCompile(`<div class="alert error" role="alert">([^<]*)</div>`)

// refusalText is the reason a refusal page, or a form shown again after a refusal, gives in its error alert, unescaped;
// any other body (plain text, a redirect) is returned unescaped and trimmed.
func refusalText(body string) string {
	if m := refusalAlert.FindStringSubmatch(body); m != nil {
		return html.UnescapeString(m[1])
	}
	return html.UnescapeString(strings.TrimSpace(body))
}

var refusalBackLink = regexp.MustCompile(`<a class="text-link refusal-back" href="([^"]*)">([^<]*)</a>`)

// checkRefusalPage checks the shared refusal page's shape and returns its body: status, an HTML type with the
// usual security headers, an "Error:" title, one h1, the skip link and the main navigation, msg in the alert, and
// the link back to back.
func checkRefusalPage(t *testing.T, w *httptest.ResponseRecorder, code int, msg, back string) string {
	t.Helper()
	b := w.Body.String()
	h := w.Header()
	if w.Code != code || !strings.HasPrefix(h.Get("Content-Type"), "text/html") {
		t.Fatalf("refusal: status=%d type=%q want %d text/html\n%s", w.Code, h.Get("Content-Type"), code, b)
	}
	if h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("refusal headers: %v", h)
	}
	if !strings.Contains(b, "<title>Error: ") || strings.Count(b, "<h1") != 1 || !strings.Contains(b, `<a class="skip-link" href="#main">`) ||
		!strings.Contains(b, `aria-label="Main navigation"`) || !strings.Contains(b, `role="alert">`+template.HTMLEscapeString(msg)) {
		t.Fatalf("refusal page shape (want alert %q):\n%s", msg, b)
	}
	if m := refusalBackLink.FindStringSubmatch(b); m == nil || m[1] != back {
		t.Fatalf("refusal back link: %v, want %q\n%s", m, back, b)
	}
	return b
}

var echoField = regexp.MustCompile(`<label for="refusal-echo-(\d+)">([^<]*)</label>\n<textarea id="refusal-echo-(\d+)" readonly rows="6" spellcheck="false">\n([^<]*)</textarea>`)

// echoed reports whether body shows text in a read-only, unnamed textarea labelled label.
func echoed(body, label, text string) bool {
	for _, m := range echoField.FindAllStringSubmatch(body, -1) {
		if m[1] == m[3] && m[2] == template.HTMLEscapeString(label) && m[4] == template.HTMLEscapeString(text) {
			return true
		}
	}
	return false
}

const xssProbe = `"><img src=x onerror=alert(1)><script>alert(1)</script> and enough text to pass the length check`

func TestRefusalPageForRefusedPostEchoesText(t *testing.T) {
	p := newPayEnv(t)
	// A dispute reply to an order that is not the buyer's: refused (404), nothing written, the reply kept.
	before := p.count("SELECT count(*) FROM order_events")
	nonce := formNonceFor("/disputes/reply")
	w := refusalPost(p.testEnv, "POST", "/disputes/reply", p.buyerSess, form("order_id", randomToken(), "reply", xssProbe, formNonceField, nonce,
		"password", "marker-field-a1", "code", "654321", "recovery", "rcvy-code-9", "captcha", "CAPT01", "new_password", "marker-field-b2",
		"rescue_code", "112233", "pgp_code", "pgp-code-7", "token", "setup-token-8"), "")
	b := checkRefusalPage(t, w, 404, "Order not found", "/disputes")
	if !echoed(b, "Dispute reply", xssProbe) || !strings.Contains(b, "Your text, to copy") {
		t.Fatalf("the refused reply is not echoed:\n%s", b)
	}
	if strings.Contains(b, "<script>alert") || strings.Contains(b, "<img src=x") {
		t.Fatalf("the echoed probe is not escaped:\n%s", b)
	}
	// The session's CSRF token appears once, in the header's sign-out form (A-548) as on every signed-in page,
	// never as an echo of the post.
	header := siteHeader(b)
	headerSignOut(t, "refusal page", header, p.A.csrf(p.buyerSess))
	unechoed := strings.Replace(b, headerSignOutForm.FindString(header), "", 1)
	for _, secret := range []string{"marker-field-a1", "654321", "rcvy-code-9", "CAPT01", "marker-field-b2", "112233", "pgp-code-7", "setup-token-8",
		p.A.csrf(p.buyerSess), nonce} {
		if strings.Contains(unechoed, secret) {
			t.Fatalf("the refusal page echoes %q", secret)
		}
	}
	if n := p.count("SELECT count(*) FROM order_events"); n != before {
		t.Fatalf("a refused reply wrote %d order events", n-before)
	}
	// A refused message keeps its armored text; the link goes back to the conversation the form was on.
	body := "not armored, so refused: <b>bold</b> & more"
	w = refusalPost(p.testEnv, "POST", "/messages", p.buyerSess, form("recipient", p.vendor.Handle, "body", body), "http://example.com/messages?to="+p.vendor.Handle)
	b = checkRefusalPage(t, w, 400, "Encrypt the message locally and paste the complete armored PGP message (up to 20 KB).", "/messages?to="+p.vendor.Handle)
	if !echoed(b, "Message or review", body) {
		t.Fatalf("the refused message is not echoed:\n%s", b)
	}
	// The signed-in header is shown on the refusal page.
	mustContain(t, b, `<a class="account-name" href="/account">`+p.buyer.Handle+`</a>`)
}

func TestRefusalPageBackLinkIsLocal(t *testing.T) {
	p := newPayEnv(t)
	for _, c := range []struct{ referer, back string }{
		{"", "/"},
		{"http://example.com/orders", "/orders"},
		{"http://example.com/order?id=" + p.order + "&x=<y>", "/order?id=" + p.order},
		{"http://evil.example/orders", "/"},
		{"http://example.com//evil.example/orders", "/"},
		{"javascript:alert(1)", "/"},
		{"http://example.com/no-such-page", "/"},
		{"http://example.com/login?next=/orders", "/"},
	} {
		w := refusalPost(p.testEnv, "GET", "/no-such-page", p.buyerSess, nil, c.referer)
		checkRefusalPage(t, w, 404, "Page not found.", c.back)
	}
	// A known page whose loader finds nothing is the same page.
	w := refusalPost(p.testEnv, "GET", "/product?id="+randomToken(), "", nil, "")
	checkRefusalPage(t, w, 404, "Page not found.", "/")
	// A role-gated page refused to a signed-in buyer.
	w = refusalPost(p.testEnv, "GET", "/admin", p.buyerSess, nil, "")
	checkRefusalPage(t, w, 403, "You do not have access to this page", "/")
}

func TestRefusalPageExpiredSessionOffersSignIn(t *testing.T) {
	p := newPayEnv(t)
	agExec(p.testEnv, "UPDATE sessions SET expires=now()-interval '1 minute' WHERE token_hash=$1", digest(p.buyerSess))
	reply := "My long dispute reply that the expired session must not lose."
	w := refusalPost(p.testEnv, "POST", "/disputes/reply", p.buyerSess, form("order_id", p.order, "reply", reply), "")
	b := checkRefusalPage(t, w, 401, "Sign in required", "/disputes")
	if !strings.Contains(b, `<a class="button primary refusal-sign-in" href="/login?next=%2fdisputes">Sign in</a>`) || !echoed(b, "Dispute reply", reply) {
		t.Fatalf("expired session refusal:\n%s", b)
	}
	// Once the browser has dropped the session cookie, the post's CSRF token is not this browser's: an expired form,
	// with the same sign-in link and the text kept.
	w = refusalPost(p.testEnv, "POST", "/disputes/reply", "", form("order_id", p.order, "reply", reply, "csrf", p.A.csrf(p.buyerSess)), "")
	b = checkRefusalPage(t, w, 403, formExpired, "/disputes")
	if !strings.Contains(b, "refusal-sign-in") || !echoed(b, "Dispute reply", reply) {
		t.Fatalf("expired form refusal:\n%s", b)
	}
	// A wrong confirmation password from a live session is a 401 too, but the account is signed in: no sign-in link.
	_, sess := p.user("rf_live_"+randomToken()[:6], "buyer")
	w = refusalPost(p.testEnv, "POST", "/account/password", sess, form("password", "wrong-password-xyz", "new_password", "another-long-password", "new_password_confirm", "another-long-password"), "")
	if strings.Contains(w.Body.String(), "refusal-sign-in") || w.Code != 401 {
		t.Fatalf("signed-in 401: %d\n%s", w.Code, w.Body.String())
	}
}

func TestRefusedSignInKeepsHandle(t *testing.T) {
	e := newTestApp(t)
	e.user("rf_keeper", "buyer")
	token := randomToken()
	w := refusalPost(e, "POST", "/login", token, form("handle", "rf_keeper", "password", "wrong-password-1234", "next", "/orders"), "")
	b := w.Body.String()
	if w.Code != 401 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(b, `role="alert">Invalid handle or password`) ||
		!strings.Contains(b, `name="handle" value="rf_keeper"`) || !strings.Contains(b, `action="/login"`) || !strings.Contains(b, `name="next" value="/orders"`) ||
		strings.Contains(b, "wrong-password-1234") || !strings.Contains(b, "<title>Error: ") || !strings.Contains(b, `name="csrf" value="`+e.A.csrf(token)+`"`) {
		t.Fatalf("wrong password re-render:\n%s", b)
	}
	// Registration with a taken handle keeps it too; with the CAPTCHA on the form carries a fresh challenge.
	agExec(e, "UPDATE settings SET value='true' WHERE key='captcha_required'")
	spent := e.captcha("/register", token)
	w = refusalPost(e, "POST", "/register", token, form("handle", "rf_keeper", "password", "another-password-123", "captcha_id", spent, "captcha", "ZZZZZZ"), "")
	b = w.Body.String()
	m := captchaField.FindStringSubmatch(b)
	if w.Code != 400 || !strings.Contains(b, `name="handle" value="rf_keeper"`) || !strings.Contains(b, `action="/register"`) || m == nil || m[1] == spent ||
		strings.Contains(b, "another-password-123") || strings.Contains(b, "ZZZZZZ") {
		t.Fatalf("refused registration re-render: %d\n%s", w.Code, b)
	}
	// A hostile handle is escaped.
	w = refusalPost(e, "POST", "/login", token, form("handle", `"><script>x</script>`, "password", "wrong-password-1234"), "")
	if b = w.Body.String(); w.Code != 400 || strings.Contains(b, "<script>x") || !strings.Contains(b, `value="&#34;&gt;&lt;script&gt;x&lt;/script&gt;"`) {
		t.Fatalf("hostile handle: %d\n%s", w.Code, b)
	}
}

// Without a database: the preview's unknown page and a refused post are the refusal page, not plain text.
func TestRefusalPageInPreview(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	a := testHTTPApp(true)
	a.templates = tmpl
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/no-such-page", nil))
	b := checkRefusalPage(t, w, 404, "Page not found.", "/")
	mustContain(t, b, "READ-ONLY PREVIEW")
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("POST", "/messages", strings.NewReader("body=x")))
	checkRefusalPage(t, w, 403, "Read-only preview. Start with PostgreSQL to save changes.", "/messages")
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/?q=%ffprobe", nil))
	b = checkRefusalPage(t, w, 400, invalidQueryText, "/")
	mustContain(t, b, `name="q" value=""`) // the refused query is not put back in the header's search box
	mustNotContain(t, b, "probe")
}

// refusalHeadings is a refusal page's <title> and <h1> text.
func refusalHeadings(t *testing.T, body string) string {
	t.Helper()
	title, h1 := regexp.MustCompile(`<title>([^<]*)</title>`).FindStringSubmatch(body), regexp.MustCompile(`<h1>([^<]*)<`).FindStringSubmatch(body)
	if title == nil || h1 == nil {
		t.Fatalf("no title or h1:\n%s", body)
	}
	return title[1] + " | " + h1[1]
}

func TestRefusalTitleSignedInWrongPassword(t *testing.T) {
	p := newPayEnv(t)
	id, sess := p.user("rf_title_"+randomToken()[:6], "buyer")
	hash := func() string {
		t.Helper()
		var h string
		if err := p.DB.QueryRow("SELECT password_hash FROM users WHERE id=$1", id).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	before := hash()
	w := refusalPost(p.testEnv, "POST", "/account/password", sess, form("password", "wrong-password-xyz", "new_password", "another-long-password", "new_password_confirm", "another-long-password"), "")
	b := checkRefusalPage(t, w, 401, "Password incorrect. Nothing was changed.", "/account")
	if head := refusalHeadings(t, b); !strings.Contains(head, "Error: Password not accepted") || !strings.Contains(head, "| Password not accepted") {
		t.Fatalf("signed-in wrong password headings: %q", head)
	}
	mustNotContain(t, b, "Sign in required", "refusal-sign-in", "You are not signed in")
	if hash() != before {
		t.Fatal("the refused change altered the password")
	}
	// A signed-out 401 still says so, with its sign-in link.
	w = refusalPost(p.testEnv, "POST", "/disputes/reply", randomToken(), form("order_id", p.order, "reply", "reply text that is long enough"), "")
	b = checkRefusalPage(t, w, 401, "Sign in required", "/disputes")
	if head := refusalHeadings(t, b); !strings.Contains(head, "Error: Sign in required") || !strings.Contains(head, "| Sign in required") {
		t.Fatalf("signed-out headings: %q", head)
	}
	mustContain(t, b, "refusal-sign-in")
	mustNotContain(t, b, "Nothing was changed")
	for _, c := range []struct {
		signedIn bool
		msg      string
		title    string
	}{
		{true, passwordIncorrectText, "Password not accepted"},
		{false, passwordIncorrectText, "Password not accepted"}, // the session lookup was busy: the message still names a signed-in check
		{true, "Verification code incorrect, expired or already used", "Confirmation not accepted"},
		{true, "Recovery code incorrect or already used", "Confirmation not accepted"},
		{true, "Your session ended while changing the password. Sign in again.", "Sign in required"},
		{true, "Sign in again; nothing was changed.", "Sign in required"},
		{false, "Sign in required", "Sign in required"},
		{false, "Verification code incorrect, expired or already used", "Sign in required"}, // the sign-in challenge, not signed in yet
	} {
		if got := refusalTitle(401, true, c.signedIn, c.msg); got != c.title {
			t.Errorf("refusalTitle(401, signedIn=%v, %q) = %q, want %q", c.signedIn, c.msg, got, c.title)
		}
	}
	// The other codes do not depend on the account or the message.
	if got := refusalTitle(409, true, true, passwordIncorrectText); got != "Form not accepted" {
		t.Errorf("409 title: %q", got)
	}
}

func TestRefusalEchoesReplacementKeyAndNote(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("rf_echo_admin", "admin")
	e.user("rf_echo_target", "vendor")
	const key, note = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nreplacement-key-marker\n-----END PGP PUBLIC KEY BLOCK-----", "Verified a signed message from the holder <b>today</b>"
	_, usable := testPGPKey(t, "rf echo")
	const account = "/admin/users?handle=rf_echo_target"
	// A key that is not usable, and a usable one with a wrong password: both refused, both kept, nothing else shown.
	for _, c := range []struct {
		key  string
		code int
		msg  string
	}{{key, 400, "The replacement must be a valid PGP public key"}, {usable, 401, "Password incorrect"}} {
		w := refusalPost(e, "POST", "/admin/reset-factors", admin, resetForm("rf_echo_target", "password", "wrong-marker-password", "code", "654321", "replacement_pgp", c.key, "recovery_note", note, "from", "user"), "")
		b := checkRefusalPage(t, w, c.code, c.msg, account)
		// An armored key's "+" is escaped by html/template and not by echoed's helper, so the key is compared unescaped.
		if !strings.Contains(html.UnescapeString(b), c.key) || !strings.Contains(b, `">Replacement PGP public key</label>`) || !echoed(b, "Verification note", note) {
			t.Fatalf("the refused replacement (%d) is not echoed:\n%s", c.code, b)
		}
		mustNotContain(t, b, "wrong-marker-password", "654321", `name="replacement_pgp"`, `name="recovery_note"`, "<b>today")
	}
	// A pasted private key is a secret the form refuses: it is not shown back; the note still is.
	private := "-----BEGIN PGP PRIVATE KEY BLOCK-----\nprivate-key-marker\n-----END PGP PRIVATE KEY BLOCK-----"
	w := refusalPost(e, "POST", "/admin/reset-factors", admin, resetForm("rf_echo_target", "replacement_pgp", private, "recovery_note", note, "from", "user"), "")
	b := checkRefusalPage(t, w, 400, "The replacement must be a valid PGP public key", account)
	mustNotContain(t, b, "private-key-marker")
	if !echoed(b, "Verification note", note) {
		t.Fatalf("the note beside a refused private key is not echoed:\n%s", b)
	}
	f := resetForm("rf_echo_target", "password", "wrong-marker-password", "replacement_pgp", key, "recovery_note", note, "from", "user")
	// A cross-site post echoes nothing.
	r := httptest.NewRequest("POST", "/admin/reset-factors", strings.NewReader(f.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://evil.example")
	r.AddCookie(&http.Cookie{Name: "session", Value: admin})
	x := httptest.NewRecorder()
	e.A.ServeHTTP(x, r)
	if x.Code != 403 || strings.Contains(x.Body.String(), "replacement-key-marker") || strings.Contains(x.Body.String(), "Verified a signed message") {
		t.Fatalf("cross-site post: %d\n%s", x.Code, x.Body.String())
	}
	// The other administrator free text forms keep theirs, each cut to its form's limit.
	echo := refusalEcho(url.Values{"staff_contact": {strings.Repeat("s", 300)}, "vendor_bond_instructions": {strings.Repeat("i", 3000)}, "release_condition": {strings.Repeat("r", 3000)},
		"recovery_note": {strings.Repeat("n", 800)}, "replacement_pgp": {strings.Repeat("k", 20000)}})
	want := map[string]int{"Staff contact": 200, "Bond payment instructions": 2000, "Release condition": 2000, "Verification note": 500, "Replacement PGP public key": 16384}
	if len(echo) != len(want) {
		t.Fatalf("echo fields: %+v", echo)
	}
	for _, f := range echo {
		if want[f.Label] != len(f.Text) {
			t.Errorf("%s: %d characters, want %d", f.Label, len(f.Text), want[f.Label])
		}
	}
}

func TestRefusalBackReturnsToOrderPage(t *testing.T) {
	p := newPayEnv(t)
	order := "/order?id=" + p.order
	for _, c := range []struct{ name, path, referer, back string }{
		{"a reply", "/disputes/reply", "http://example.com" + order, order},
		{"a cancel", "/orders/cancel", "http://example.com" + order, order},
		{"extra query dropped", "/orders/cancel", "http://example.com" + order + "&x=<y>", order},
		{"no referer", "/orders/cancel", "", "/orders"},
		{"another host", "/orders/cancel", "http://evil.example" + order, "/orders"},
		{"a double slash", "/orders/cancel", "http://example.com//evil.example" + order, "/orders"},
		{"a tampered id", "/orders/cancel", `http://example.com/order?id="><script>`, "/orders"},
		{"two ids", "/orders/cancel", "http://example.com" + order + "&id=" + p.order, "/orders"},
		{"no id", "/orders/cancel", "http://example.com/order", "/orders"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := refusalPost(p.testEnv, "POST", c.path, p.buyerSess, form("order_id", randomToken(), "reply", "reply text that is long enough", "from", stateDraft), c.referer)
			if m := refusalBackLink.FindStringSubmatch(w.Body.String()); m == nil || m[1] != c.back {
				t.Fatalf("back link %v, want %q (status %d)", m, c.back, w.Code)
			}
		})
	}
}

func TestRefusalBackReturnsToAccountPage(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("rf_back_admin", "admin")
	e.user("rf_back_user", "vendor")
	const account = "/admin/users?handle=rf_back_user"
	wrong := func(extra ...string) url.Values {
		return resetForm("rf_back_user", append([]string{"password", "wrong-password-123"}, extra...)...)
	}
	for _, c := range []struct {
		name    string
		path    string
		f       url.Values
		referer string
		back    string
	}{
		{"from the account page", "/admin/reset-factors", wrong("from", "user"), "", account},
		{"a form without a handle, by its Referer", "/admin/vendor-applications", form("id", randomToken(), "action", "reject", "from", "user"), "http://example.com" + account, account},
		{"Referer alone", "/admin/reset-factors", wrong(), "http://example.com" + account, account},
		{"the directory's form", "/admin/reset-factors", wrong(), "http://example.com/admin/users", "/admin/users"},
		{"no way back named", "/admin/reset-factors", wrong(), "", "/admin"},
		{"a tampered from", "/admin/reset-factors", wrong("from", "elsewhere"), "", "/admin"},
		{"a Referer on another host", "/admin/reset-factors", wrong(), "http://evil.example" + account, "/admin"},
		{"a Referer with a tampered handle", "/admin/reset-factors", wrong(), `http://example.com/admin/users?handle="><script>`, "/admin/users"},
		{"a Referer with two handles", "/admin/reset-factors", wrong(), "http://example.com" + account + "&handle=rf_back_admin", "/admin/users"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := refusalPost(e, "POST", c.path, admin, c.f, c.referer)
			m := refusalBackLink.FindStringSubmatch(w.Body.String())
			if m == nil || html.UnescapeString(m[1]) != c.back {
				t.Fatalf("back link %v, want %q (status %d)\n%s", m, c.back, w.Code, w.Body.String())
			}
		})
	}
	// A tampered handle names no account (posted to the action directly: the form's unknown-handle 404 has its own page).
	for _, handle := range []string{"../../evil", "rf_back_user&next=//evil.example", "//evil.example", ""} {
		r := httptest.NewRequest("POST", "/admin/reset-factors", nil)
		r.PostForm = form("from", "user", "handle", handle)
		if got := e.A.refusalBack(r); got != "/admin" {
			t.Errorf("handle %q: back %q", handle, got)
		}
	}
	// from=user on an action outside the administration names no account.
	w := refusalPost(e, "POST", "/messages", admin, form("from", "user", "handle", "rf_back_user", "recipient", "rf_back_user", "body", "not armored"), "")
	if m := refusalBackLink.FindStringSubmatch(w.Body.String()); m == nil || m[1] != "/messages" {
		t.Fatalf("non-admin action's back link: %v", m)
	}
}

func TestRefusedRegisterKeepsPGPKey(t *testing.T) {
	e := newTestApp(t)
	const key = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nregister-key-marker <b>\n-----END PGP PUBLIC KEY BLOCK-----"
	e.user("rf_register_admin", "admin") // setup is complete: /register is the form, not a redirect to /setup
	token := randomToken()
	field := func(body string) string {
		t.Helper()
		m := regexp.MustCompile(`<textarea name="pgp"[^>]*>([^<]*)</textarea>`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no PGP key field:\n%s", body)
		}
		return strings.TrimPrefix(html.UnescapeString(m[1]), "\n")
	}
	// A wrong CAPTCHA answer: the same form, key and handle kept, a fresh challenge, not the password.
	agExec(e, "UPDATE settings SET value='true' WHERE key='captcha_required'")
	spent := e.captcha("/register", token)
	f := form("handle", "rf_register", "password", "marker-register-password", "pgp", key, "account_type", "vendor", "captcha_id", spent, "captcha", "ZZZZZZ")
	w := refusalPost(e, "POST", "/register", token, f, "")
	b := w.Body.String()
	if w.Code != 400 || !strings.Contains(b, `action="/register"`) || field(b) != key || !strings.Contains(b, `name="handle" value="rf_register"`) {
		t.Fatalf("wrong CAPTCHA: %d, key %q\n%s", w.Code, field(b), b)
	}
	mustNotContain(t, b, "marker-register-password", "ZZZZZZ", "register-key-marker <b>")
	// A refusal after the CAPTCHA keeps it too; the field stays empty on a form never posted.
	agExec(e, "UPDATE settings SET value='false' WHERE key='captcha_required'")
	e.user("rf_taken", "buyer")
	w = refusalPost(e, "POST", "/register", token, form("handle", "rf_taken", "password", "marker-register-password", "pgp", key), "")
	if b = w.Body.String(); w.Code < 400 || field(b) != key || strings.Contains(b, "marker-register-password") {
		t.Fatalf("taken handle: %d\n%s", w.Code, b)
	}
	if got := field(e.body("GET", "/register", token, nil, 200)); got != "" {
		t.Fatalf("a fresh form holds %q", got)
	}
	// Bounded and valid UTF-8.
	w = refusalPost(e, "POST", "/register", token, form("handle", "rf_taken", "password", "marker-register-password", "pgp", strings.Repeat("k", 20000)+"\xff\x00"), "")
	if got := field(w.Body.String()); utf8.RuneCountInString(got) != 16384 {
		t.Fatalf("oversized key kept %d characters", utf8.RuneCountInString(got))
	}
	w = refusalPost(e, "POST", "/register", token, form("handle", "rf_taken", "password", "marker-register-password", "pgp", "bad\xff\x00key"), "")
	if got := field(w.Body.String()); !utf8.ValidString(got) || strings.Contains(got, "\x00") || !strings.HasPrefix(got, "bad") {
		t.Fatalf("invalid key text kept as %q", got)
	}
	// A pasted private key is not kept.
	w = refusalPost(e, "POST", "/register", token, form("handle", "rf_taken", "password", "marker-register-password", "pgp", "-----BEGIN PGP PRIVATE KEY BLOCK-----\nprivate-key-marker"), "")
	if b = w.Body.String(); field(b) != "" || strings.Contains(b, "private-key-marker") {
		t.Fatalf("a private key was kept:\n%s", b)
	}
	// A cross-site post echoes nothing: no key, handle or account type.
	for _, h := range [][2]string{{"Origin", "http://evil.example"}, {"Sec-Fetch-Site", "cross-site"}} {
		c := form("handle", "rf_crosssite", "password", "marker-register-password", "pgp", key, "account_type", "vendor", "csrf", e.A.csrf(token))
		r := httptest.NewRequest("POST", "/register", strings.NewReader(c.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set(h[0], h[1])
		r.AddCookie(&http.Cookie{Name: "session", Value: token})
		x := httptest.NewRecorder()
		e.A.ServeHTTP(x, r)
		b := x.Body.String()
		if x.Code != 403 || field(b) != "" {
			t.Fatalf("cross-site (%s): %d\n%s", h[0], x.Code, b)
		}
		mustNotContain(t, b, "register-key-marker", "rf_crosssite", "marker-register-password", `value="vendor" checked`)
	}
}
