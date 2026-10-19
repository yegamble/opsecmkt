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

func TestRefusedListingNamesFieldAndKeepsText(t *testing.T) {
	p := newPayEnv(t)
	base := form("title", "My <b>own</b> listing", "description", "A long description I typed & do not want to lose.", "category", "Hardware",
		"region", "Worldwide", "kind", "physical", "stock", "3", "price_btc", "0.001", "price_xmr", "0.5")
	for _, c := range []struct{ field, value, msg string }{
		{"price_xmr", "0.1234567890123", "Monero price: use a positive amount with at most 12 decimal places."},
		{"price_xmr", "abc", "Monero price: use a positive amount with at most 12 decimal places."},
		{"stock", "-1", "Stock: use a whole number from 0 to 1000000."},
		{"category", "Weapons", "Category: choose Hardware, Digital or Services."},
		{"title", "  ", "Listing title: use 3 to 140 characters."},
		{"region", "", "Region: use 2 to 80 characters."},
		{"kind", "service", "Fulfillment type: choose physical or digital."},
	} {
		f := cloneValues(base)
		f.Set(c.field, c.value)
		w := refusalPost(p.testEnv, "POST", "/listings", p.vendorSess, f, "http://example.com/vendor-dashboard")
		b := checkRefusalPage(t, w, 400, c.msg, "/vendor-dashboard")
		if !echoed(b, "Description", base.Get("description")) || (c.field != "title" && !echoed(b, "Listing title", base.Get("title"))) {
			t.Fatalf("%s: the listing text is not echoed:\n%s", c.field, b)
		}
	}
	// Two problems are both named.
	f := cloneValues(base)
	f.Set("price_xmr", "1.1234567890123")
	f.Set("stock", "x")
	w := refusalPost(p.testEnv, "POST", "/listings", p.vendorSess, f, "")
	mustContain(t, w.Body.String(), "Monero price: use a positive amount", "Stock: use a whole number")
	if n := p.count("SELECT count(*) FROM products WHERE vendor_id=$1", p.vendor.ID); n != 1 {
		t.Fatalf("refused listings wrote %d products", n-1)
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
