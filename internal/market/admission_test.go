package market

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// holdSlots takes every slot of sem until the returned release (also run at cleanup) gives them back.
func holdSlots(t *testing.T, sem chan struct{}) (release func()) {
	t.Helper()
	for range cap(sem) {
		sem <- struct{}{}
	}
	done := false
	release = func() {
		if done {
			return
		}
		done = true
		for range cap(sem) {
			<-sem
		}
	}
	t.Cleanup(release)
	return release
}

// admitPreviewApp is the read-only preview (no database) with the real templates.
func admitPreviewApp(t *testing.T) *App {
	t.Helper()
	a := testHTTPApp(true)
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	a.templates = tmpl
	return a
}

func admitServe(a *App, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader("csrf=x")))
	return w
}

// A-178: a full page class refuses at once with 503 and Retry-After, while /healthz and static files, which
// take no slot, still answer; POSTs have their own class.
func TestAdmissionPageClassFull(t *testing.T) {
	a := admitPreviewApp(t)
	release := holdSlots(t, pageWork)
	for _, path := range []string{"/", "/login", "/captcha?id=preview", "/no-such-page"} {
		w := admitServe(a, "GET", path)
		if w.Code != 503 || w.Header().Get("Retry-After") != "5" || !strings.Contains(w.Body.String(), "busy") {
			t.Fatalf("GET %s with the page class full: %d Retry-After=%q %q", path, w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
	}
	if w := admitServe(a, "GET", "/healthz"); w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("/healthz with the page class full: %d %q", w.Code, w.Body.String())
	}
	if w := admitServe(a, "GET", "/static/style.css"); w.Code != 200 {
		t.Fatalf("static file with the page class full: %d", w.Code)
	}
	if w := admitServe(a, "POST", "/login"); w.Code != 403 { // the read-only preview's own refusal, past admission
		t.Fatalf("POST with only the page class full: %d", w.Code)
	}
	release()
	if w := admitServe(a, "GET", "/"); w.Code != 200 {
		t.Fatalf("GET / after the slots were released: %d", w.Code)
	}
	if len(pageWork) != 0 || len(anonPostWork) != 0 || len(userPostWork) != 0 {
		t.Fatalf("slots leaked: pages=%d anonymous posts=%d signed-in posts=%d", len(pageWork), len(anonPostWork), len(userPostWork))
	}
}

// admitAnonPost is an anonymous browser's same-site form post: an anon cookie and its CSRF token.
func admitAnonPost(a *App, path string, form url.Values) *http.Request {
	anon := randomToken()
	if form == nil {
		form = url.Values{}
	}
	if !form.Has("csrf") {
		form.Set("csrf", a.csrf(anon))
	}
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "anon", Value: anon})
	return r
}

// admitTestAction registers a public and a signed-in action that report how many slots of each post class
// were held while they ran.
func admitTestAction(t *testing.T) (held func() [2]int) {
	t.Helper()
	var mu sync.Mutex
	var last [2]int
	run := func(c *actionCtx) (actionResult, error) {
		mu.Lock()
		last = [2]int{len(anonPostWork), len(userPostWork)}
		mu.Unlock()
		if c.Form.Get("panic") != "" {
			panic("handler bug")
		}
		return actionResult{Redirect: "/done"}, nil
	}
	withAction(t, "/test/admit", actionSpec{Public: true, OwnTx: true, Run: run})
	withAction(t, "/test/admit-signed", actionSpec{OwnTx: true, Run: run})
	return func() [2]int { mu.Lock(); defer mu.Unlock(); return last }
}

// A form post takes its class slot only after its body is read and its Origin, Sec-Fetch-Site and CSRF checks
// pass (PR #15 review): with both post classes full, a bad post is refused for what it is, not as busy; only a
// post that passed every check is refused 503.
func TestAdmissionPostClassFullChecksFirst(t *testing.T) {
	a := testHTTPApp(false)
	admitTestAction(t)
	holdSlots(t, anonPostWork)
	holdSlots(t, userPostWork)
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	bad := admitAnonPost(a, "/test/admit", url.Values{"csrf": {"wrong"}})
	if w := serve(bad); w.Code != 403 {
		t.Fatalf("wrong CSRF with the post classes full: %d %q", w.Code, w.Body.String())
	}
	cross := admitAnonPost(a, "/test/admit", nil)
	cross.Header.Set("Origin", "https://attacker.example")
	if w := serve(cross); w.Code != 403 {
		t.Fatalf("cross-origin post with the post classes full: %d", w.Code)
	}
	huge := admitAnonPost(a, "/test/admit", url.Values{"x": {strings.Repeat("a", 70000)}})
	if w := serve(huge); w.Code != 400 {
		t.Fatalf("oversized post with the post classes full: %d", w.Code)
	}
	if w := serve(admitAnonPost(a, "/test/admit-signed", nil)); w.Code != 401 {
		t.Fatalf("signed-in action without a session, post classes full: %d", w.Code)
	}
	w := serve(admitAnonPost(a, "/test/admit", nil))
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" || !strings.Contains(w.Body.String(), "busy") {
		t.Fatalf("valid post with its class full: %d Retry-After=%q %q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if w := admitServe(admitPreviewApp(t), "GET", "/"); w.Code != 200 {
		t.Fatalf("GET with only the post classes full: %d", w.Code)
	}
}

// Signed-in posts and anonymous posts have separate classes (PR #15 review): a flood of anonymous posts
// cannot refuse a signed-in user's or an administrator's change, and the reverse.
func TestAdmissionPostClassesAreSeparate(t *testing.T) {
	a := testHTTPApp(false)
	held := admitTestAction(t)
	user := &User{ID: "u1", Role: "buyer"}
	release := holdSlots(t, anonPostWork)
	if w := postAs(a, nil, "/test/admit"); w.Code != 503 || w.Header().Get("Retry-After") != "5" {
		t.Fatalf("anonymous post with the anonymous class full: %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := postAs(a, user, "/test/admit-signed"); w.Code != 303 || held() != [2]int{cap(anonPostWork), 1} {
		t.Fatalf("signed-in post with the anonymous class full: %d held=%v", w.Code, held())
	}
	// A public action (sign-in, registration, the sign-in challenge) skips the write:<user> budget, so a
	// signed-in account posting it uses the anonymous class (PR #15 re-review, NEW-1).
	if w := postAs(a, user, "/test/admit"); w.Code != 503 {
		t.Fatalf("signed-in public post with the anonymous class full: %d held=%v", w.Code, held())
	}
	release()
	release = holdSlots(t, userPostWork)
	if w := postAs(a, user, "/test/admit-signed"); w.Code != 503 {
		t.Fatalf("signed-in post with the signed-in class full: %d", w.Code)
	}
	if w := postAs(a, nil, "/test/admit"); w.Code != 303 || held() != [2]int{1, cap(userPostWork)} {
		t.Fatalf("anonymous post with the signed-in class full: %d held=%v", w.Code, held())
	}
	if w := postAs(a, user, "/test/admit"); w.Code != 303 || held() != [2]int{1, cap(userPostWork)} {
		t.Fatalf("signed-in public post with the signed-in class full: %d held=%v", w.Code, held())
	}
	release()
	if len(anonPostWork) != 0 || len(userPostWork) != 0 {
		t.Fatalf("post slots leaked: anonymous=%d signed-in=%d", len(anonPostWork), len(userPostWork))
	}
}

// PR #15 re-review (NEW-1): one signed-in account flooding public actions, which skip its write budget, fills
// only the anonymous class; an administrator's change is still served.
func TestSignedInPublicFloodLeavesStaffPosts(t *testing.T) {
	a := testHTTPApp(false)
	gate := make(chan struct{})
	var inside sync.WaitGroup
	withAction(t, "/test/public-slow", actionSpec{Public: true, OwnTx: true, Run: func(c *actionCtx) (actionResult, error) {
		inside.Done()
		<-gate
		return actionResult{Redirect: "/done"}, nil
	}})
	withAction(t, "/test/admin-change", actionSpec{Roles: []string{"admin"}, OwnTx: true, Run: func(c *actionCtx) (actionResult, error) {
		return actionResult{Redirect: "/admin"}, nil
	}})
	flooder := &User{ID: "flooder", Role: "buyer"}
	n := cap(anonPostWork)
	inside.Add(n)
	var done sync.WaitGroup
	for range n {
		done.Add(1)
		go func() { defer done.Done(); postAs(a, flooder, "/test/public-slow") }()
	}
	inside.Wait()
	defer func() { close(gate); done.Wait() }()
	if len(anonPostWork) != n || len(userPostWork) != 0 {
		t.Fatalf("signed-in public posts hold anonymous=%d signed-in=%d", len(anonPostWork), len(userPostWork))
	}
	if w := postAs(a, flooder, "/test/public-slow"); w.Code != 503 {
		t.Fatalf("one more public post from the flooder: %d", w.Code)
	}
	if w := postAs(a, &User{ID: "admin", Role: "admin"}, "/test/admin-change"); w.Code != 303 {
		t.Fatalf("administrator's change during a signed-in public-post flood: %d %q", w.Code, w.Body.String())
	}
}

// PR #15 re-review (NEW-4): a form is parsed before any slot, on up to 2048 connections at once, so a post
// with more fields than any form has (maxFormFields) is refused (400) before parsing. A 64 KiB body of 9 999
// distinct empty fields (net/url's own limit is 10 000) cost about 3.7 MB of allocation and 2 ms of CPU to
// parse; refused, it costs little more than its body. Ordinary posts, the largest form included, still parse.
func TestFormFieldFloodIsBounded(t *testing.T) {
	a := testHTTPApp(false)
	held := admitTestAction(t)
	anon := randomToken()
	var b strings.Builder
	for i := range 9999 {
		fmt.Fprintf(&b, "k%d=&", i)
	}
	junk := b.String()[:65536]
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/test/admit", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "anon", Value: anon})
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	if w := post(junk); w.Code != 400 {
		t.Fatalf("a post of %d empty fields: %d %q", strings.Count(junk, "&"), w.Code, w.Body.String())
	}
	const n = 256
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() { defer wg.Done(); post(junk) }()
	}
	wg.Wait()
	took := time.Since(start)
	runtime.ReadMemStats(&after)
	perPost := (after.TotalAlloc - before.TotalAlloc) / n
	t.Logf("%d concurrent maximum-field junk posts: %d bytes allocated per post, %v in all", n, perPost, took)
	if perPost > 3*65536 {
		t.Fatalf("a maximum-field junk post allocates %d bytes", perPost)
	}
	// The largest form (listing edit, 12 fields) and one just under the limit are served.
	form := url.Values{"csrf": {a.csrf(anon)}}
	for i := range maxFormFields - 1 {
		form.Set(fmt.Sprintf("f%d", i), "x")
	}
	if w := post(form.Encode()); w.Code != 303 || held() != [2]int{1, 0} {
		t.Fatalf("a post of %d fields: %d %q", len(form), w.Code, w.Body.String())
	}
	form.Set("one_more", "x")
	if w := post(form.Encode()); w.Code != 400 {
		t.Fatalf("a post of %d fields: %d", len(form), w.Code)
	}
}

// Sign-in, registration and setup posts with a malformed handle or password, or a CAPTCHA id that is not a
// live one issued to their session, and sign-in challenges with no pending sign-in cookie, are refused before
// they take a post slot (PR #15 review): a flood of
// them cannot refuse a real sign-in as busy. A well-formed post with a live id goes on to the slot.
func TestAdmissionAuthJunkTakesNoSlot(t *testing.T) {
	a := testHTTPApp(false)
	a.captchaIDKey = []byte("test-captcha-id-key")
	holdSlots(t, anonPostWork)
	anon := randomToken()
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		form.Set("csrf", a.csrf(anon))
		r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "anon", Value: anon})
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	creds := func(handle, password, id string) url.Values {
		return url.Values{"handle": {handle}, "password": {password}, "captcha_id": {id}, "captcha": {"XXXXXX"}}
	}
	live := a.newCaptchaID(digest(anon), time.Now().Add(captchaTTL))
	for _, c := range []struct {
		name, path string
		form       url.Values
		want       int
		body       string
	}{
		{"random id", "/login", creds("junk_user", "guess-guess-guess", randomToken()), 400, "expired or was already used"},
		{"other session's id", "/login", creds("junk_user", "guess-guess-guess", a.newCaptchaID(digest(randomToken()), time.Now().Add(captchaTTL))), 400, "expired or was already used"},
		{"expired id", "/register", creds("junk_user", "guess-guess-guess", a.newCaptchaID(digest(anon), time.Now().Add(-time.Second))), 400, "expired or was already used"},
		{"malformed handle", "/login", creds("x", "guess-guess-guess", live), 400, "3–32 character handle"},
		{"short password", "/register", creds("junk_user", "short", live), 400, "3–32 character handle"},
		{"setup, malformed handle", "/setup", url.Values{"handle": {"no spaces"}, "password": {"guess-guess-guess"}}, 400, "3–32 character handle"},
		{"challenge without a sign-in in progress", "/challenge", url.Values{"method": {"totp"}, "code": {"123456"}}, 401, "Sign in again"},
		{"PGP challenge without a sign-in in progress", "/challenge/pgp", url.Values{}, 401, "Sign in again"},
		{"live id", "/login", creds("junk_user", "guess-guess-guess", live), 503, "busy"},
		{"no id (the CAPTCHA may be off)", "/login", url.Values{"handle": {"junk_user"}, "password": {"guess-guess-guess"}}, 503, "busy"},
	} {
		if w := post(c.path, c.form); w.Code != c.want || !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, w.Body.String(), c.want, c.body)
		}
	}
}

// Slow-body posts hold no post slot (PR #15 review, the blocker): with twice the anonymous class's size of
// posts trickling their bodies, a normal post is still served.
func TestAdmissionSlowBodiesHoldNoPostSlot(t *testing.T) {
	a := testHTTPApp(false)
	held := admitTestAction(t)
	var entered atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered.Add(1)
		a.ServeHTTP(w, r)
	}))
	defer srv.Close()
	host := srv.Listener.Addr().String()
	n := 2 * cap(anonPostWork)
	for range n {
		c, err := net.Dial("tcp", host)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		anon := randomToken()
		fmt.Fprintf(c, "POST /test/admit HTTP/1.1\r\nHost: %s\r\nContent-Type: application/x-www-form-urlencoded\r\nCookie: anon=%s\r\nContent-Length: 1000\r\n\r\ncsrf=%s&x=", host, anon, a.csrf(anon))
	}
	for deadline := time.Now().Add(5 * time.Second); entered.Load() < int64(n); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d slow posts reached the handler", entered.Load(), n)
		}
	}
	time.Sleep(100 * time.Millisecond) // each is now blocked reading its body
	if len(anonPostWork) != 0 || len(userPostWork) != 0 {
		t.Fatalf("slow-body posts hold post slots: anonymous=%d signed-in=%d", len(anonPostWork), len(userPostWork))
	}
	r := admitAnonPost(a, "/test/admit", nil)
	r.RequestURI, r.URL.Scheme, r.URL.Host = "", "http", host
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 303 || held()[0] != 1 {
		t.Fatalf("normal post during %d slow-body posts: %d held=%v", n, resp.StatusCode, held())
	}
}

// The CAPTCHA image has its own class of 8 inside the page class (A-176).
func TestAdmissionCaptchaImageClassFull(t *testing.T) {
	a := admitPreviewApp(t)
	release := holdSlots(t, captchaWork)
	w := admitServe(a, "GET", "/captcha?id=preview")
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" {
		t.Fatalf("CAPTCHA image with its class full: %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	release()
	if w := admitServe(a, "GET", "/captcha?id=preview"); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("CAPTCHA image after release: %d", w.Code)
	}
	if len(captchaWork) != 0 || len(pageWork) != 0 {
		t.Fatal("CAPTCHA or page slot leaked")
	}
}

// A slot is released when the response starts, and on every way out of the handler, a panic included.
func TestAdmissionSlotReleased(t *testing.T) {
	r := httptest.NewRequest("GET", "/catalog", nil)
	aw, release, ok := admit(httptest.NewRecorder(), r)
	if !ok || len(pageWork) != 1 {
		t.Fatalf("admit: ok=%v held=%d", ok, len(pageWork))
	}
	aw.Write([]byte("body")) // the response started: the rest is the client's reading time
	if len(pageWork) != 0 {
		t.Fatal("slot held after the response started")
	}
	release() // the deferred release is then a no-op, never a second receive
	if len(pageWork) != 0 {
		t.Fatal("double release")
	}

	func() {
		defer func() { recover() }()
		_, release, _ := admit(httptest.NewRecorder(), httptest.NewRequest("GET", "/login", nil))
		defer release()
		panic("handler bug")
	}()
	if len(pageWork) != 0 {
		t.Fatal("slot leaked by a panicking handler")
	}
	// A post's slot, taken late (after its checks), is held while its action runs and comes back when the
	// response starts, when the action returns and when it panics.
	pa := testHTTPApp(false)
	held := admitTestAction(t)
	w := httptest.NewRecorder()
	pa.ServeHTTP(w, admitAnonPost(pa, "/test/admit", nil))
	if w.Code != 303 || held() != [2]int{1, 0} || len(anonPostWork) != 0 {
		t.Fatalf("post: %d held while running=%v after=%d", w.Code, held(), len(anonPostWork))
	}
	func() {
		defer func() { recover() }()
		pa.ServeHTTP(httptest.NewRecorder(), admitAnonPost(pa, "/test/admit", url.Values{"panic": {"1"}}))
	}()
	if len(anonPostWork) != 0 || len(userPostWork) != 0 {
		t.Fatal("post slot leaked by a panicking action")
	}
	// Through ServeHTTP: net/http recovers a handler panic; the slot must come back with it. Without a
	// database, the installed check dereferences a nil *sql.DB before any response is written.
	a := testHTTPApp(false)
	func() {
		defer func() { recover() }()
		a.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/catalog", nil))
		t.Error("ServeHTTP did not panic without a database")
	}()
	if len(pageWork) != 0 {
		t.Fatal("slot leaked by a panic inside ServeHTTP")
	}
}

// Every 503 and 429 carries Retry-After; one a handler set is kept.
func TestAdmitWriterRetryAfter(t *testing.T) {
	for _, c := range []struct {
		code      int
		preset    string
		wantAfter string
	}{{503, "", "5"}, {429, "", "600"}, {503, "30", "30"}, {500, "", ""}, {200, "", ""}, {303, "", ""}} {
		rec := httptest.NewRecorder()
		released := 0
		w := &admitWriter{ResponseWriter: rec, release: func() { released++ }}
		if c.preset != "" {
			w.Header().Set("Retry-After", c.preset)
		}
		http.Error(w, "x", c.code)
		if got := rec.Header().Get("Retry-After"); got != c.wantAfter || rec.Code != c.code || released != 1 {
			t.Errorf("%d (preset %q): Retry-After=%q code=%d released=%d", c.code, c.preset, got, rec.Code, released)
		}
	}
}

func TestLimitListenerCapsConnections(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := LimitListener(inner, 2, nil)
	defer ln.Close()
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- c
		}
	}()
	var clients []net.Conn
	for range 3 {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		clients = append(clients, c)
	}
	var server []net.Conn
	for range 2 {
		select {
		case c := <-accepted:
			server = append(server, c)
		case <-time.After(2 * time.Second):
			t.Fatal("connections under the cap were not accepted")
		}
	}
	select {
	case <-accepted:
		t.Fatal("a third connection was accepted with the cap at 2")
	case <-time.After(200 * time.Millisecond):
	}
	server[0].Close()
	server[0].Close() // a second Close frees nothing more
	select {
	case c := <-accepted:
		server = append(server, c)
	case <-time.After(2 * time.Second):
		t.Fatal("closing a connection did not admit the waiting one")
	}
	if len(ln.(*limitListener).sem) != 2 {
		t.Fatalf("slots held: %d, want 2", len(ln.(*limitListener).sem))
	}
	// Close unblocks an Accept waiting for a slot.
	ln.Close()
	select {
	case _, open := <-accepted:
		if open {
			t.Fatal("accepted after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept still blocked after Close")
	}
	for _, c := range server[1:] {
		c.Close()
	}
}

// Idle keep-alive connections cannot hold the listener's slots: when a new client finds them all taken, the
// server turns keep-alive off, which closes the idle ones, and turns it back on once use has fallen.
func TestLimitListenerReclaimsIdleKeepAlive(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }), IdleTimeout: time.Minute}
	states := make(chan bool, 16)
	go srv.Serve(LimitListener(inner, 2, func(full bool) { srv.SetKeepAlivesEnabled(!full); states <- full }))
	defer srv.Close()
	url := "http://" + inner.Addr().String() + "/"
	get := func(c *http.Client) error {
		resp, err := c.Get(url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		return err
	}
	idle := []*http.Client{{Transport: &http.Transport{}}, {Transport: &http.Transport{}}}
	for _, c := range idle {
		if err := get(c); err != nil { // each now holds one idle keep-alive connection
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if err := get(&http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}); err != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("new client with both slots idle-held: err=%v after %v", err, time.Since(start))
	}
	for _, want := range []bool{true, false} {
		select {
		case got := <-states:
			if got != want {
				t.Fatalf("saturation reported %v, want %v", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("saturation %v never reported", want)
		}
	}
	for _, c := range idle { // their connections were closed: a new request reconnects
		if err := get(c); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLogThrottle(t *testing.T) {
	var lines []string
	var timers []func()
	clock := time.Unix(1_700_000_000, 0)
	th := newLogThrottle(10, time.Second, "http 5xx")
	th.now = func() time.Time { return clock }
	th.printf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	th.after = func(_ time.Duration, f func()) any { timers = append(timers, f); return nil }
	for i := range 25 {
		th.Printf("http 5xx line %d", i)
	}
	if len(lines) != 10 || lines[9] != "http 5xx line 9" {
		t.Fatalf("first window wrote %d lines: %q", len(lines), lines)
	}
	if len(timers) != 1 {
		t.Fatalf("%d summary timers scheduled, want 1", len(timers))
	}
	// The window ends with no further line: the timer writes the summary.
	clock = clock.Add(time.Second)
	timers[0]()
	if len(lines) != 11 || lines[10] != "http 5xx suppressed=15 (more than 10 lines in 1s)" {
		t.Fatalf("summary: %q", lines[len(lines)-1])
	}
	timers[0]() // a late second firing writes nothing more
	// A new window passes lines again.
	th.Printf("http 5xx next")
	if len(lines) != 12 || lines[11] != "http 5xx next" {
		t.Fatalf("new window: %q", lines)
	}
	// A window whose summary had no timer run yet: the next window's first line writes it first.
	for range 12 {
		th.Printf("burst")
	}
	clock = clock.Add(1500 * time.Millisecond)
	th.Printf("after")
	if got := lines[len(lines)-2:]; got[0] != "http 5xx suppressed=3 (more than 10 lines in 1s)" || got[1] != "after" {
		t.Fatalf("summary before the next line: %q", got)
	}
	th.reset()
	for range 10 {
		th.Printf("again")
	}
	if lines[len(lines)-1] != "again" {
		t.Fatal("reset did not open a new window")
	}
}
