package market

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"text/template/parse"
)

func postAs(a *App, u *User, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ParseForm()
	w := httptest.NewRecorder()
	a.post(w, r, u, strings.Repeat("a", 64))
	return w
}

func withAction(t *testing.T, path string, s actionSpec) {
	t.Helper()
	registerAction(path, s)
	t.Cleanup(func() { delete(actions, path) })
}

func TestActionDispatch(t *testing.T) {
	a := testHTTPApp(false)
	if w := postAs(a, &User{ID: "u", Role: "admin"}, "/no-such-action"); w.Code != 404 {
		t.Fatalf("unknown action: %d", w.Code)
	}
	ran := 0
	run := func(c *actionCtx) (actionResult, error) {
		ran++
		switch c.Form.Get("mode") {
		case "fail":
			return actionResult{}, fail(409, "Specific conflict")
		case "err":
			return actionResult{}, errors.New("internal detail")
		}
		return actionResult{Redirect: "/done", AfterCommit: func(w http.ResponseWriter) { w.Header().Set("X-After", "1") }}, nil
	}
	withAction(t, "/test/mod", actionSpec{Roles: []string{"moderator", "admin"}, OwnTx: true, Run: run})
	withAction(t, "/test/public", actionSpec{Public: true, OwnTx: true, Run: run})
	withAction(t, "/test/signed", actionSpec{OwnTx: true, Run: run})
	for _, tc := range []struct {
		path string
		user *User
		want int
		msg  string
	}{
		{"/test/mod", nil, 401, "Sign in required"},
		{"/test/mod", &User{ID: "b", Role: "buyer"}, 403, "Moderator access required"},
		{"/test/mod", &User{ID: "v", Role: "vendor"}, 403, "Moderator access required"},
		{"/test/mod", &User{ID: "m", Role: "moderator"}, 303, ""},
		{"/test/mod", &User{ID: "a", Role: "admin"}, 303, ""},
		{"/test/signed", nil, 401, "Sign in required"},
		{"/test/signed", &User{ID: "b", Role: "buyer"}, 303, ""},
		{"/test/public", nil, 303, ""},
	} {
		before := ran
		w := postAs(a, tc.user, tc.path)
		if w.Code != tc.want || !strings.Contains(w.Body.String(), tc.msg) {
			t.Errorf("%s as %+v: %d %q", tc.path, tc.user, w.Code, w.Body)
		}
		if (tc.want == 303) != (ran > before) {
			t.Errorf("%s as %+v: Run executed=%v", tc.path, tc.user, ran > before)
		}
		if tc.want == 303 && (w.Header().Get("Location") != "/done" || w.Header().Get("X-After") != "1") {
			t.Errorf("%s: redirect/AfterCommit missing: %v", tc.path, w.Header())
		}
	}
	for mode, want := range map[string]int{"fail": 409, "err": 500} {
		r := httptest.NewRequest(http.MethodPost, "/test/public", strings.NewReader("mode="+mode))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.ParseForm()
		w := httptest.NewRecorder()
		a.post(w, r, nil, "")
		if w.Code != want || strings.Contains(w.Body.String(), "internal detail") || w.Header().Get("X-After") != "" {
			t.Errorf("mode %s: %d %q", mode, w.Code, w.Body)
		}
	}
	if roleRequired([]string{"admin"}) != "Administrator access required" || roleRequired([]string{"vendor", "admin"}) != "Vendor access required" {
		t.Error("role messages changed")
	}
}

func TestRegistriesHaveTemplatesAndHooks(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for name := range pages {
		if tmpl.Lookup("page:"+name) == nil {
			t.Errorf("registered page %q has no web/templates/pages/%s.html", name, name)
		}
	}
	files, _ := filepath.Glob("web/templates/pages/*.html")
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".html")
		if _, ok := pages[name]; !ok {
			t.Errorf("template %s has no registerPage(%q)", f, name)
		}
	}
	all, _ := filepath.Glob("web/templates/*/*.html")
	layout, _ := filepath.Glob("web/templates/*.html")
	for _, f := range append(all, layout...) {
		b, _ := os.ReadFile(f)
		if strings.Contains(strings.ToLower(string(b)), "<script") {
			t.Errorf("%s contains a script tag", f)
		}
		// A-81: the UI states what the server keeps (/canary#records) instead of a privacy slogan.
		if strings.Contains(strings.ToUpper(string(b)), "YOUR CHOICE") {
			t.Errorf("%s contains the \"YOUR CHOICE\" slogan", f)
		}
	}
	var walk func(n parse.Node)
	walk = func(n parse.Node) {
		switch n := n.(type) {
		case *parse.ListNode:
			if n != nil {
				for _, c := range n.Nodes {
					walk(c)
				}
			}
		case *parse.TemplateNode:
			if tmpl.Lookup(n.Name) == nil {
				t.Errorf("template %q is referenced but not defined", n.Name)
			}
		case *parse.IfNode:
			walk(n.List)
			walk(n.ElseList)
		case *parse.RangeNode:
			walk(n.List)
			walk(n.ElseList)
		case *parse.WithNode:
			walk(n.List)
			walk(n.ElseList)
		}
	}
	for _, x := range tmpl.Templates() {
		if x.Tree != nil {
			walk(x.Tree.Root)
		}
	}
}

func TestPreviewRendersEveryPage(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	a := testHTTPApp(true)
	a.templates = tmpl
	for name := range pages {
		path := "/" + name + "?id=encrypted-drive"
		if name == "vendor" {
			path = "/vendor?id=ghost"
		}
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || strings.Contains(w.Body.String(), "<script") || !strings.Contains(w.Body.String(), "READ-ONLY PREVIEW") {
			t.Errorf("preview %s: %d", path, w.Code)
		}
	}
}

// oneSpace joins s's words with single spaces, so a check does not depend on where a file wraps its lines.
func oneSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// clamavLine is the sentence every place that describes the malware scanner to users or operators carries (A-191).
const clamavLine = "ClamAV checks uploads for known malware; it does not review picture content"

// A-81, A-191: the records disclosure lists listing photos and avatars in both places it is kept,
// /canary#records and "What the server keeps": what an image row holds, who sees it, the one-day browser cache,
// and that deleting one leaves copies in older backups and in PostgreSQL's data files. The migration range that
// "What the server keeps" was checked against ends at the newest migration.
func TestRecordsListPhotosAndAvatars(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	a := testHTTPApp(true)
	a.templates = tmpl
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/canary", nil))
	page := w.Body.String()
	start := strings.Index(page, `<section class="panel records" id="records"`)
	if w.Code != 200 || start < 0 {
		t.Fatalf("/canary: %d, records section found: %v", w.Code, start >= 0)
	}
	records := page[start:]
	records = oneSpace(html.UnescapeString(records[:strings.Index(records, "</section>")]))
	doc, err := os.ReadFile("docs/implementation-status.md")
	if err != nil {
		t.Fatal(err)
	}
	status, _, _ := strings.Cut(string(doc)[strings.Index(string(doc), "## What the server keeps"):], "\n## ")
	status = oneSpace(status)
	for _, want := range []string{"re-encoded JPEG, its thumbnail, its dimensions, an optional description",
		"never kept", "public to anyone", "public staff list", "normal browser (not Tor Browser) may keep viewed images in its disk cache for a day",
		"deletes it from the database at once", "copies remain in backups made before that and, until PostgreSQL reuses the space, in its data files",
		"its images from the public", clamavLine} {
		if !strings.Contains(records, want) {
			t.Errorf("/canary#records lacks %q", want)
		}
		if !strings.Contains(status, want) {
			t.Errorf("docs/implementation-status.md \"What the server keeps\" lacks %q", want)
		}
	}
	for _, want := range []string{"<strong>Photos and avatars.</strong>", "Upload refused by the malware scanner"} {
		if !strings.Contains(records, want) {
			t.Errorf("/canary#records lacks %q", want)
		}
	}
	if !strings.Contains(status, "**Listing photos and avatars** (`images`, migration 063") {
		t.Error("\"What the server keeps\" has no images entry naming its table")
	}
	migrations, err := filepath.Glob("internal/market/migrations/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("no migrations", err)
	}
	sort.Strings(migrations)
	if last := filepath.Base(migrations[len(migrations)-1])[:3]; !strings.Contains(status, "(001–"+last+")") {
		t.Errorf("\"What the server keeps\" is not stated as checked against migrations 001–%s", last)
	}
}

// A-191: no template or user-facing document calls an upload "safe", "clean", "virus-free" or "verified": ClamAV
// checks uploads for known malware only. The check reads every template under web/templates and README.md,
// UPGRADING.md, CONTRIBUTING.md and docs/*.md, and splits them into sentences (template actions dropped; in a
// template every tag but an inline one ends a paragraph; in Markdown a blank line, list item, table row, heading
// or code fence does). It flags a banned word only in a sentence about uploads, one naming an upload, photo,
// avatar, picture, ClamAV, clamd, the scanner, a scan, malware or a virus (in templates also an image). The
// words stay free elsewhere: a verified PGP key, canary signature or review, a "clean" gofmt or CI run. In the
// documents a bare "image" is not a topic, because there it mostly means a Docker image ("the verified release
// images"). The internal notes (.claude, .ralph) are not user-facing and are not read. Every place that
// describes the scanner to users or operators carries the ClamAV line.
func TestNoUploadCalledSafeOrClean(t *testing.T) {
	const topic = `upload\w*|photo\w*|avatar\w*|pictures?|clamav|clamd|scanner\w*|scan|scans|scanned|scanning|malware|virus\w*`
	topicDocs := regexp.MustCompile(`(?i)\b(` + topic + `)\b`)
	topicTemplates := regexp.MustCompile(`(?i)\b(` + topic + `|images?)\b`)
	banned := regexp.MustCompile(`(?i)\b(safe|clean|virus[- ]free|verified)\b`)
	actions := regexp.MustCompile(`(?s)\{\{.*?\}\}`)
	inline := regexp.MustCompile(`(?i)</?(strong|em|span|a|code|abbr|b|i|kbd|small|sub|sup|mark)\b[^>]*>`)
	tags := regexp.MustCompile(`<[^>]*>`)
	block := regexp.MustCompile(`\n(\s*([-*|#>]|\d+\.|` + "```" + `))`)
	paragraphs := regexp.MustCompile(`\n\s*\n`)
	sentences := regexp.MustCompile(`[.!?]\s+`)
	templates, _ := filepath.Glob("web/templates/*.html")
	more, _ := filepath.Glob("web/templates/*/*.html")
	docs, _ := filepath.Glob("docs/*.md")
	files := append(append(templates, more...), append([]string{"README.md", "UPGRADING.md", "CONTRIBUTING.md"}, docs...)...)
	if len(templates)+len(more) < 20 || len(docs) < 5 {
		t.Fatalf("found %d templates and %d docs", len(templates)+len(more), len(docs))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text, about := string(b), topicDocs
		if strings.HasSuffix(f, ".html") {
			text = tags.ReplaceAllString(inline.ReplaceAllString(actions.ReplaceAllString(text, " "), " "), "\n\n")
			about = topicTemplates
		} else {
			text = block.ReplaceAllString(text, "\n\n$1")
		}
		for _, p := range paragraphs.Split(text, -1) {
			for _, s := range sentences.Split(oneSpace(p), -1) {
				if about.MatchString(s) && banned.MatchString(s) {
					t.Errorf("%s calls an upload %q: %q", f, banned.FindString(s), s)
				}
			}
		}
	}
	for _, f := range []string{"web/templates/partials/uploads.html", "web/templates/partials/admin-scanner.html",
		"web/templates/pages/canary.html", "README.md", "docs/operator-guide.md", "docs/implementation-status.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(oneSpace(string(b)), clamavLine) {
			t.Errorf("%s describes the malware scanner without %q", f, clamavLine)
		}
	}
}

// A-115: the preview draft and the paid sample carry real-shaped 64-hex IDs; only the paid sample shows a
// deposit row, and its status says no wallet is connected.
func TestPreviewOrderSamplesUseRealIDShapes(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	a := testHTTPApp(true)
	a.templates = tmpl
	get := func(id string) string {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", "/order?id="+id, nil))
		if w.Code != 200 {
			t.Fatalf("preview order %s: %d", id, w.Code)
		}
		return w.Body.String()
	}
	for _, id := range []string{previewDraftID, previewPaidID, previewIncomingID, previewDisputedID, previewFlaggedID} {
		if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
			t.Errorf("preview ID %q is not 64 lowercase hex", id)
		}
	}
	if body := get(previewDraftID); !strings.Contains(body, previewDraftID) || strings.Contains(body, "Deposits seen by the wallet") {
		t.Error("preview draft: want its 64-hex ID and no deposit table")
	}
	body := get(previewPaidID)
	for _, want := range []string{previewPaidID, "Deposits seen by the wallet", "926bb57bc9bcbc21ddd00aa2e6f70f9d445378bcbaf4e55b6bfa53010ba85ddb:0",
		"Preview sample: no wallet is connected and no funds exist", ">Paid<"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview paid sample: missing %q", want)
		}
	}
	if strings.Contains(body, `class="payment-address"`) {
		t.Error("preview paid sample shows a deposit address")
	}
}

func TestSecondFactorSeam(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("factor_user", "buyer")
	fake := &fakeFactor{name: "test-factor", user: id} // distinct from real providers registered by P1/P2
	factors = append(factors, fake)
	t.Cleanup(func() { factors = factors[:len(factors)-1] })
	anon := randomToken()
	w := e.do("POST", "/login", anon, url.Values{"handle": {"factor_user"}, "password": {testPassword}})
	e.check(w, 303)
	pending := e.cookie(w, "pending")
	if w.Header().Get("Location") != "/challenge" || pending == "" || e.cookie(w, "session") != "" {
		t.Fatalf("password alone signed in: %v", w.Header())
	}
	e.check(e.do("GET", "/account", anon, nil), 303)
	pc := &http.Cookie{Name: "pending", Value: pending}
	w = e.do("GET", "/challenge", anon, nil, pc)
	e.check(w, 200)
	if !strings.Contains(w.Body.String(), "Your password was accepted") {
		t.Fatal("challenge page does not show the pending factor")
	}
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"test-factor"}, "code": {"000000"}}, pc), 401)
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "code": {"123456"}}, pc), 400)
	w = e.do("POST", "/challenge", anon, url.Values{"method": {"test-factor"}, "code": {"123456"}}, pc)
	e.check(w, 303)
	session := e.session(w)
	e.check(e.do("GET", "/account", session, nil), 200)
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"test-factor"}, "code": {"123456"}}, pc), 401)
	if !strings.Contains(e.do("GET", "/challenge", anon, nil, pc).Body.String(), "No sign-in verification is pending") {
		t.Fatal("used pending login still offered")
	}
	fake.user = ""
	w = e.do("POST", "/login", randomToken(), url.Values{"handle": {"factor_user"}, "password": {testPassword}})
	e.check(w, 303)
	if w.Header().Get("Location") != "/" || e.cookie(w, "session") == "" {
		t.Fatal("login without enrolled factors must sign in directly")
	}
}

type fakeFactor struct{ name, user string }

func (f *fakeFactor) Name() string { return f.name }
func (f *fakeFactor) Enrolled(_ context.Context, _ rowQuerier, userID string) (bool, error) {
	return userID == f.user, nil
}
func (f *fakeFactor) Verify(c *actionCtx, userID string) error {
	if c.Tx == nil || c.Form.Get("code") != "123456" {
		return fail(401, "Invalid code")
	}
	return nil
}
