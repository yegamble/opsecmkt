package market

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
		// An administration section (A-542) renders page:admin with its section.
		if base, section := adminSectionPage(name); section != "" {
			name = base
		}
		if tmpl.Lookup("page:"+name) == nil {
			t.Errorf("registered page %q has no web/templates/pages/%s.html", name, name)
		}
	}
	// A nested private page can live in one embedded pages/*.html file. Check
	// its declared page template, rather than inventing a public basename route.
	for _, declared := range tmpl.Templates() {
		if name, page := strings.CutPrefix(declared.Name(), "page:"); page {
			if name == "refusal" || name == "upload-error" {
				continue // Existing internal error layouts are rendered directly, never routed.
			}
			if _, ok := pages[name]; !ok {
				t.Errorf("template %s has no registerPage(%q)", declared.Name(), name)
			}
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
	unknown := httptest.NewRecorder()
	a.ServeHTTP(unknown, httptest.NewRequest("GET", "/no-such-page?id=encrypted-drive", nil))
	for name := range pages {
		path := "/" + name + "?id=encrypted-drive"
		if name == "vendor" {
			path = "/vendor?id=ghost"
		}
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if moderationPrivateRoute("/" + name) {
			// These routes never expose queue data or token forms on a public
			// listener, including read-only preview and missing-socket installs.
			if w.Code != 404 || w.Body.String() != "404 page not found\n" || w.Header().Get("Set-Cookie") != "" {
				t.Errorf("private moderation preview %s: %d %q", path, w.Code, w.Body)
			}
			continue
		}
		if !a.pageOn(name) { // switched off in the preview (ESCROW_BTC is never on there): an unknown page
			if w.Code != 404 || unknown.Code != 404 || w.Body.String() != unknown.Body.String() {
				t.Errorf("preview %s (off): %d", path, w.Code)
			}
			continue
		}
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
// /canary#records and "What the server keeps". Each needle is matched only inside the photos and avatars entry
// (its <li> on /canary, its bullet in the document), case-insensitively and with ’ read as ': what an image row
// holds, who sees it, the browser cache, what demotion, suspension and deletion do, what a restore brings back,
// and the scanner's log line. Both introductions except photos and avatars from "nothing is deleted", and the
// migration range that "What the server keeps" was checked against ends at the newest migration.
func TestRecordsListPhotosAndAvatars(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	a := testHTTPApp(true)
	a.templates = tmpl
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/canary", nil))
	if w.Code != 200 {
		t.Fatalf("/canary: %d", w.Code)
	}
	norm := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(oneSpace(html.UnescapeString(s)), "’", "'"))
	}
	entry := func(where, text, from, to string) string {
		t.Helper()
		i := strings.Index(text, from)
		if i < 0 {
			t.Fatalf("%s has no photos and avatars entry (%q)", where, from)
		}
		e, _, _ := strings.Cut(text[i:], to)
		return norm(e)
	}
	page := w.Body.String()
	records := entry("/canary#records", page, "<li><strong>Photos and avatars.</strong>", "</li>")
	doc, err := os.ReadFile("docs/implementation-status.md")
	if err != nil {
		t.Fatal(err)
	}
	status := entry("docs/implementation-status.md", string(doc), "- **Listing photos and avatars** (`images`, migration 063", "\n")
	for _, want := range []string{
		"only the re-encoded jpeg, its thumbnail, its dimensions, an optional description",
		"its name and its metadata are never kept",
		"listing photos and their descriptions are public to anyone",
		"a vendor's avatar is public while",
		"a staff avatar is public",
		"and on an administrator's vendor page",
		"public staff list",
		"otherwise it is shown only beside that staff member's dispute decisions",
		"demoting a staff member removes their staff avatar",
		"suspending the account",
		"its images from the public",
		"a normal browser may keep viewed images in its disk cache: it reuses them for a day without asking the server, and the files can stay until its cache is cleared. tor browser keeps them in memory only, until it closes (not yet tried in tor browser, a-199)",
		"deletes it from the database at once, except the image fingerprints that abuse-review records keep (above)",
		"a perceptual fingerprint (",
		"copies remain in backups made before that and, until postgresql reuses the space, in its data files and write-ahead log",
		"if the operator restores a backup made before a removal, the image returns until it is removed again",
		"upload refused by the malware scanner",
		"the server log keeps the signature name and the reference shown to the uploader, for as long as docker's logs are kept (3 × 10 mb rotation), and its time can be matched to the uploader's activity-log row",
		strings.ToLower(clamavLine),
	} {
		if !strings.Contains(records, want) {
			t.Errorf("/canary#records photos and avatars entry lacks %q", want)
		}
		if !strings.Contains(status, want) {
			t.Errorf("docs/implementation-status.md photos and avatars entry lacks %q", want)
		}
	}
	for where, intro := range map[string]string{"/canary#records": page, "docs/implementation-status.md": string(doc)} {
		if !strings.Contains(norm(intro), "except photos and avatars (below)") {
			t.Errorf("%s: the records introduction does not except photos and avatars from \"nothing is deleted\"", where)
		}
	}
	migrations, err := filepath.Glob("internal/market/migrations/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("no migrations", err)
	}
	sort.Strings(migrations)
	if last := filepath.Base(migrations[len(migrations)-1])[:3]; !strings.Contains(string(doc), "`internal/market/migrations/` (001–"+last+")") {
		t.Errorf("\"What the server keeps\" is not stated as checked against migrations 001–%s", last)
	}
}

func TestCanaryRecordsDescribeCurrentActivityAndTemporarySignInRetention(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	a := testHTTPApp(true)
	a.templates = tmpl
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/canary", nil))
	if w.Code != 200 {
		t.Fatalf("/canary: %d", w.Code)
	}
	body := oneSpace(html.UnescapeString(w.Body.String()))
	for _, want := range []string{
		"message sent",
		"mark-all notification read",
		"Message-send entries do not name the recipient",
		"Marking all notifications read is recorded; reading one notification is not.",
		"Temporary sign-in records",
		"stop working within 12 hours and are deleted later",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("canary records copy lacks %q", want)
		}
	}
	// A-492: pooled payouts are removed, so no activity row records releasing a suspension-held payout.
	if strings.Contains(body, "suspension-held payout") {
		t.Error("canary records copy still lists a pooled payout release")
	}
	if strings.Contains(body, "expire within 12 hours") {
		t.Fatal("temporary sign-in copy claims opportunistic deletion completes within 12 hours")
	}
}

// uploadClaims returns the sentences of text that call an upload one of the words the war room banned for
// uploads (A-191; the PR #28 review added "malware-free" and "harmless"): ClamAV checks uploads for known
// malware only, so no page, document or message may promise more. kind is "doc" (Markdown), "template" (an HTML
// template) or "go" (a Go source file, whose string literals are read as template text).
//
// The text is split into sentences: template actions are dropped; alt, title and aria-label attribute values are
// kept, each as a paragraph of its own; every other tag but an inline one ends a paragraph; in Markdown a blank
// line, list item, table row, heading or code fence does; then [.!?] and whitespace end a sentence. A banned word
// counts only in a sentence about uploads: one naming an upload, photo, avatar, picture, ClamAV, clamd, the
// scanner, a scan, malware or a virus (in templates and Go strings also an image). The words stay free elsewhere:
// a verified PGP key, canary signature or review, a "clean" gofmt or CI run. In documents a bare "image" is not a
// topic, because there it mostly means a Docker image ("the verified release images"). Out of scope, by design: a
// claim split over two sentences ("Upload a photo. It is safe."), and Go strings assembled at run time.
func uploadClaims(t *testing.T, kind, text string) []string {
	t.Helper()
	const topic = `upload\w*|photo\w*|avatar\w*|pictures?|clamav|clamd|scanner\w*|scan|scans|scanned|scanning|malware|virus\w*`
	about := regexp.MustCompile(`(?i)\b(` + topic + `|images?)\b`)
	banned := regexp.MustCompile(`(?i)\b(safe|clean|virus[- ]free|malware[- ]free|verified|harmless)\b`)
	actions := regexp.MustCompile(`(?s)\{\{.*?\}\}`)
	inline := regexp.MustCompile(`(?i)^</?(strong|em|span|a|code|abbr|b|i|kbd|small|sub|sup|mark)\b`)
	tags := regexp.MustCompile(`<[^>]*>`)
	attrs := regexp.MustCompile(`(?i)\b(alt|title|aria-label)\s*=\s*"([^"]*)"`)
	block := regexp.MustCompile(`\n(\s*([-*|#>]|\d+\.|` + "```" + `))`)
	paragraphs := regexp.MustCompile(`\n\s*\n`)
	sentences := regexp.MustCompile(`[.!?]\s+`)
	htmlText := func(s string) string {
		return tags.ReplaceAllStringFunc(actions.ReplaceAllString(s, " "), func(tag string) string {
			var kept []string
			for _, m := range attrs.FindAllStringSubmatch(tag, -1) {
				kept = append(kept, m[2])
			}
			if len(kept) > 0 {
				return "\n\n" + strings.Join(kept, "\n\n") + "\n\n"
			}
			if inline.MatchString(tag) {
				return " "
			}
			return "\n\n"
		})
	}
	var parts []string
	switch kind {
	case "template":
		parts = []string{htmlText(text)}
	case "doc":
		about = regexp.MustCompile(`(?i)\b(` + topic + `)\b`)
		parts = []string{block.ReplaceAllString(text, "\n\n$1")}
	case "go":
		f, err := parser.ParseFile(token.NewFileSet(), "", text, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					parts = append(parts, htmlText(s))
				}
			}
			return true
		})
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	var found []string
	for _, part := range parts {
		for _, p := range paragraphs.Split(part, -1) {
			for _, s := range sentences.Split(oneSpace(p), -1) {
				if about.MatchString(s) && banned.MatchString(s) {
					found = append(found, s)
				}
			}
		}
	}
	return found
}

// A-191: no template, user-facing document or message in the application's Go code calls an upload one of the
// banned words (uploadClaims says what is read and what is out of scope). It reads every template under
// web/templates, README.md, UPGRADING.md, CONTRIBUTING.md, docs/*.md and the string literals of the non-test Go
// files in internal/market; the internal notes (.claude, .ralph) are not user-facing and are not read. Every place
// that describes the scanner to users or operators carries the ClamAV line.
func TestNoUploadCalledSafeOrClean(t *testing.T) {
	templates, _ := filepath.Glob("web/templates/*.html")
	more, _ := filepath.Glob("web/templates/*/*.html")
	docs, _ := filepath.Glob("docs/*.md")
	goFiles, _ := filepath.Glob("internal/market/*.go")
	files := append(append(templates, more...), append([]string{"README.md", "UPGRADING.md", "CONTRIBUTING.md"}, docs...)...)
	for _, f := range goFiles {
		if !strings.HasSuffix(f, "_test.go") {
			files = append(files, f)
		}
	}
	if len(templates)+len(more) < 20 || len(docs) < 5 || len(goFiles) < 20 {
		t.Fatalf("found %d templates, %d docs and %d Go files", len(templates)+len(more), len(docs), len(goFiles))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		kind := map[string]string{".html": "template", ".md": "doc", ".go": "go"}[filepath.Ext(f)]
		for _, s := range uploadClaims(t, kind, string(b)) {
			t.Errorf("%s calls an upload one of the banned words: %q", f, s)
		}
	}
	for _, f := range []string{"web/templates/partials/uploads.html", "web/templates/partials/admin-scanner.html",
		"web/templates/pages/canary.html", "README.md", "UPGRADING.md", "docs/operator-guide.md", "docs/implementation-status.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(oneSpace(string(b)), clamavLine) {
			t.Errorf("%s describes the malware scanner without %q", f, clamavLine)
		}
	}
}

// PR #28 review (SHOULD-4): the evasions the first version of the check missed are caught (a Go string such as
// the upload notice in notice.go, an alt or title attribute, "malware-free" and "harmless"), and the
// legitimate uses and the out-of-scope two-sentence split stay free.
func TestUploadClaimsCatchesEvasions(t *testing.T) {
	src, err := os.ReadFile("internal/market/notice.go")
	if err != nil {
		t.Fatal(err)
	}
	const notice = `"Photo saved as a re-encoded JPEG;`
	if !strings.Contains(string(src), notice) {
		t.Fatalf("notice.go no longer has the notice %s", notice)
	}
	for _, c := range []struct{ kind, text string }{
		{"go", strings.Replace(string(src), notice, `"Photo saved as a clean re-encoded JPEG;`, 1)},
		{"go", "package x\n\nconst m = `<p>Your avatar is harmless.</p>`\n"},
		{"template", `<a href="/product?id=1"><img src="/img/0-t" alt="Verified photo of the item"></a>`},
		{"template", `<button title="This picture is safe">Show</button>`},
		{"doc", "Every upload is checked, so\nstored photos are malware-free."},
		{"doc", "- A scanned upload is harmless.\n"},
		{"doc", "Photos are virus free."},
	} {
		if len(uploadClaims(t, c.kind, c.text)) == 0 {
			t.Errorf("%s evasion not caught: %.200q", c.kind, c.text)
		}
	}
	for _, c := range []struct{ kind, text string }{
		{"go", string(src)},
		{"template", `<h2>Verified reviews</h2><p>Upload a photo of the item.</p>`},
		{"template", `<li>PGP key verified.</li><li>Photos are supplied by the vendor.</li>`},
		{"doc", "Run gofmt until the tree is clean. Then upload the release."},
		{"doc", "The pinned Docker images are the verified releases."},
		{"doc", "Upload a photo. It is safe."}, // two sentences: out of scope, by design
	} {
		if got := uploadClaims(t, c.kind, c.text); len(got) != 0 {
			t.Errorf("%s: legitimate text flagged: %q", c.kind, got)
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
	if body := get(previewDraftID); !strings.Contains(body, previewDraftID) || strings.Contains(body, "Recorded deposits") {
		t.Error("preview draft: want its 64-hex ID and no deposit table")
	}
	body := get(previewPaidID)
	for _, want := range []string{previewPaidID, ">Paid<"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview paid sample: missing %q", want)
		}
	}
	if strings.Contains(body, `class="payment-address"`) {
		t.Error("preview paid sample shows a deposit address")
	}
	moderator := httptest.NewRecorder()
	a.ServeHTTP(moderator, httptest.NewRequest("GET", "/moderator", nil))
	if moderator.Code != 200 || !strings.Contains(moderator.Body.String(), "Order "+previewDisputedID) {
		t.Fatalf("preview moderator row does not use its real-shaped order ID: status=%d", moderator.Code)
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
