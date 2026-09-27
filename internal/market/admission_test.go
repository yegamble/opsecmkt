package market

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if len(pageWork) != 0 || len(writeWork) != 0 {
		t.Fatalf("slots leaked: pages=%d writes=%d", len(pageWork), len(writeWork))
	}
}

func TestAdmissionWriteClassFull(t *testing.T) {
	a := admitPreviewApp(t)
	holdSlots(t, writeWork)
	w := admitServe(a, "POST", "/login")
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" {
		t.Fatalf("POST with the write class full: %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := admitServe(a, "GET", "/"); w.Code != 200 {
		t.Fatalf("GET with only the write class full: %d", w.Code)
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
		_, release, _ := admit(httptest.NewRecorder(), httptest.NewRequest("POST", "/login", nil))
		defer release()
		panic("handler bug")
	}()
	if len(writeWork) != 0 {
		t.Fatal("slot leaked by a panicking handler")
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
