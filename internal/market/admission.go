package market

import (
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Admission control (A-178). Overload is refused at once with 503 and Retry-After instead of queueing until
// the 12 s request context expires:
//
//   - LimitListener caps open connections (2048 in cmd/server); further clients wait in the kernel backlog.
//   - pageWork admits 64 concurrent GET/HEAD requests. /healthz and /static/ take no slot, so a flood of pages
//     cannot starve the health probe.
//   - A form post takes a slot only once its body has been read (at most 64 KiB) and its Origin,
//     Sec-Fetch-Site and CSRF checks and its action's stateless checks have passed (App.post), so a client
//     trickling a body, or posting junk, holds no slot; the server's ReadTimeout bounds the trickle. Signed-in
//     posts (a live session) take one of userPostWork's 16 slots and anonymous posts (sign-in, registration,
//     the sign-in challenge) one of anonPostWork's 16, so a flood of anonymous posts cannot refuse a signed-in
//     user's or an administrator's change. A post's session lookup, a single read, takes a page slot for that
//     query only (ServeHTTP).
//   - captchaWork (captcha_actions.go) admits 8 concurrent CAPTCHA image renders and passwordWork
//     (actions_auth.go) 4 concurrent bcrypt checks, each inside the class slot of its request.
//
// A class slot is released when the response starts (admitWriter), so it bounds the database and render work
// of a request but not the time a slow client takes to read the response.
var (
	pageWork     = make(chan struct{}, 64)
	anonPostWork = make(chan struct{}, 16)
	userPostWork = make(chan struct{}, 16)
)

// busyRetryAfter is the Retry-After, in seconds, of an admission refusal and of any other 503.
const busyRetryAfter = "5"

// limitedRetryAfter is the Retry-After, in seconds, of a 429: every budget in App.allow is a ten-minute window.
const limitedRetryAfter = "600"

// busy refuses a request that found every slot of its class taken. It logs nothing: under a flood, one line
// per refusal would be the flood. The connection is closed after the answer, so a refused client does not
// keep one of the listener's slots on keep-alive while others wait to connect.
func busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", busyRetryAfter)
	w.Header().Set("Connection", "close")
	http.Error(w, "The market is busy. Try again in a few seconds.", http.StatusServiceUnavailable)
}

// admitClass returns the admission semaphore r takes on arrival, or nil when it takes none there: /healthz
// and static files take none, and a form post takes its slot later, in App.post (admitPost).
func admitClass(r *http.Request) chan struct{} {
	if r.Method == "POST" || r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/static/") {
		return nil
	}
	return pageWork
}

// tryTake takes a slot of sem if one is free, returning its release (safe to call more than once).
func tryTake(sem chan struct{}) (release func(), ok bool) {
	select {
	case sem <- struct{}{}:
		return sync.OnceFunc(func() { <-sem }), true
	default:
		return nil, false
	}
}

// admitPost takes a slot of sem for a form post that passed its checks, or answers 503 (busy) and returns
// ok=false. The caller defers release, which also runs when the response starts if w is ServeHTTP's
// admitWriter, like an arrival slot.
func admitPost(w http.ResponseWriter, sem chan struct{}) (release func(), ok bool) {
	release, ok = tryTake(sem)
	if !ok {
		busy(w)
		return nil, false
	}
	if aw, wrapped := w.(*admitWriter); wrapped && !aw.started {
		aw.release = release
	}
	return release, true
}

// admitWriter wraps every response. It adds Retry-After to a 503 or 429 that lacks one, and calls release
// once, when the response starts (the handler's work is done by then: pages render into a buffer first).
type admitWriter struct {
	http.ResponseWriter
	release func()
	started bool
}

func (w *admitWriter) WriteHeader(code int) {
	if !w.started {
		w.started = true
		if w.Header().Get("Retry-After") == "" {
			switch code {
			case http.StatusServiceUnavailable:
				w.Header().Set("Retry-After", busyRetryAfter)
			case http.StatusTooManyRequests:
				w.Header().Set("Retry-After", limitedRetryAfter)
			}
		}
		w.release()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *admitWriter) Write(b []byte) (int, error) {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *admitWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// admit takes r's class slot. It returns the writer to serve r through and a release to defer (also safe
// after a panic or early return), or ok=false after answering 503 when the class is full.
func admit(w http.ResponseWriter, r *http.Request) (aw *admitWriter, release func(), ok bool) {
	release = func() {}
	if sem := admitClass(r); sem != nil {
		if release, ok = tryTake(sem); !ok {
			busy(w)
			return nil, nil, false
		}
	}
	return &admitWriter{ResponseWriter: w, release: release}, release, true
}

// LimitListener returns a Listener that accepts at most n simultaneous connections from l; Accept waits for
// a closed connection before taking the next one (golang.org/x/net/netutil.LimitListener, kept local so no
// module is added). When Accept finds every slot taken it calls saturated(true), and once no more than
// three quarters are in use again, saturated(false); cmd/server turns HTTP keep-alive off in between, which
// closes idle connections, so browsers' idle keep-alive connections cannot hold the slots new visitors wait
// for. saturated may be nil.
func LimitListener(l net.Listener, n int, saturated func(bool)) net.Listener {
	return &limitListener{Listener: l, sem: make(chan struct{}, n), done: make(chan struct{}), saturated: saturated}
}

type limitListener struct {
	net.Listener
	sem       chan struct{}
	closeOnce sync.Once
	done      chan struct{}
	saturated func(bool)
	mu        sync.Mutex
	full      bool   // Accept found no slot, and use has not yet fallen to three quarters
	gen       uint64 // counts changes of full
	notifyMu  sync.Mutex
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.sem <- struct{}{}:
	default:
		l.setFull(true)
		select {
		case l.sem <- struct{}{}:
		case <-l.done:
			return nil, net.ErrClosed
		}
	}
	c, err := l.Listener.Accept()
	if err != nil {
		l.release()
		return nil, err
	}
	return &limitConn{Conn: c, release: sync.OnceFunc(l.release)}, nil
}

func (l *limitListener) release() {
	<-l.sem
	if len(l.sem) <= cap(l.sem)*3/4 {
		l.setFull(false)
	}
}

// setFull records a change and reports it from another goroutine: saturated may close connections, whose
// Close calls back into release. Reports run one at a time and a stale one is skipped, so the last call
// always carries the current state.
func (l *limitListener) setFull(full bool) {
	if l.saturated == nil {
		return
	}
	l.mu.Lock()
	if l.full == full {
		l.mu.Unlock()
		return
	}
	l.full = full
	l.gen++
	gen := l.gen
	l.mu.Unlock()
	go func() {
		l.notifyMu.Lock()
		defer l.notifyMu.Unlock()
		l.mu.Lock()
		current, latest := l.full, l.gen
		l.mu.Unlock()
		if gen == latest {
			l.saturated(current)
		}
	}()
}

func (l *limitListener) Close() error {
	err := l.Listener.Close()
	l.closeOnce.Do(func() { close(l.done) })
	return err
}

type limitConn struct {
	net.Conn
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}

// logThrottle passes at most limit lines per window to printf and counts the rest; once a window with
// suppressed lines is over it writes one "suppressed" line (on the next line, or from a timer).
type logThrottle struct {
	limit  int
	window time.Duration
	prefix string                              // of the summary line
	now    func() time.Time                    // tests replace the clock,
	printf func(format string, args ...any)    // the output
	after  func(d time.Duration, f func()) any // and the timer

	mu         sync.Mutex
	start      time.Time
	n          int
	suppressed int
	timer      bool
}

func newLogThrottle(limit int, window time.Duration, prefix string) *logThrottle {
	return &logThrottle{limit: limit, window: window, prefix: prefix, now: time.Now, printf: log.Printf,
		after: func(d time.Duration, f func()) any { return time.AfterFunc(d, f) }}
}

// http5xxLog throttles serverError's "http 5xx" lines (A-178): at most 10 a second, then one summary.
var http5xxLog = newLogThrottle(10, time.Second, "http 5xx")

// Printf writes the line unless this window's limit is spent.
func (t *logThrottle) Printf(format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if now.Sub(t.start) >= t.window {
		t.flushLocked()
		t.start, t.n = now, 0
	}
	if t.n < t.limit {
		t.n++
		t.printf(format, args...)
		return
	}
	t.suppressed++
	if !t.timer {
		t.timer = true
		t.after(t.start.Add(t.window).Sub(now), t.flush)
	}
}

// flush writes the summary line for suppressed lines, if any.
func (t *logThrottle) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.flushLocked()
}

func (t *logThrottle) flushLocked() {
	t.timer = false
	if t.suppressed > 0 {
		t.printf("%s suppressed=%d (more than %d lines in %s)", t.prefix, t.suppressed, t.limit, t.window)
		t.suppressed = 0
	}
}

// reset clears the throttle's state (tests that count log lines).
func (t *logThrottle) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.start, t.n, t.suppressed = time.Time{}, 0, 0
}
