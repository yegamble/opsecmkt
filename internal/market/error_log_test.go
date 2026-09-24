package market

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"text/template"

	"github.com/jackc/pgx/v5/pgconn"
)

// A-141: every 5xx response writes one log line naming the route key, status, a reference and a cause class,
// and the user sees the same reference; the line never carries error text or anything about the request or
// its sender (acceptance.md: no secrets in logs; A-93's TestLogsContainNoSecrets).

// lockedBuffer is a log destination safe to read while background goroutines may still log.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// lines returns the captured lines containing substr.
func (l *lockedBuffer) lines(substr string) []string {
	var out []string
	for _, line := range strings.Split(l.String(), "\n") {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

// captureLog sends the standard logger to a buffer (no timestamp prefix) until the test ends.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	w, flags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(w); log.SetFlags(flags) })
	return buf
}

var http5xxLine = regexp.MustCompile(`^http 5xx kind=(page|action) name=(\S+) status=(5\d\d) ref=([0-9a-f]{8}) cause=(.+)$`)

func TestLogsContainNoSecrets(t *testing.T) {
	e := newTestApp(t)
	const handle = "secret_handle_q7x"
	uid, session := e.user(handle, "buyer")
	logs := captureLog(t)
	forbidden := []string{handle, uid, session, digest(session), "203.0.113.77", "hidden-host.invalid", "SecretAgent/9.9", "198.51.100.9", "trace-me", "form-secret", "query-secret"}
	send := func(method, target string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		body := ""
		if form != nil {
			form.Set("csrf", e.A.csrf(session))
			body = form.Encode()
		}
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = "203.0.113.77:4444"
		r.Host = "hidden-host.invalid"
		r.Header.Set("User-Agent", "SecretAgent/9.9")
		r.Header.Set("X-Forwarded-For", "198.51.100.9")
		r.AddCookie(&http.Cookie{Name: "session", Value: session})
		r.AddCookie(&http.Cookie{Name: "trace", Value: "trace-me"})
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		return w
	}
	check := func(w *httptest.ResponseRecorder, kind, name, cause string) {
		t.Helper()
		if w.Code != 500 {
			t.Fatalf("status %d, want 500: %s", w.Code, w.Body.String())
		}
		lines := logs.lines("http 5xx")
		if len(lines) != 1 {
			t.Fatalf("want exactly one http 5xx line, got %q (all: %q)", lines, logs.String())
		}
		m := http5xxLine.FindStringSubmatch(lines[0])
		if m == nil || m[1] != kind || m[2] != name || m[3] != "500" || m[5] != cause {
			t.Fatalf("log line %q: want kind=%s name=%s status=500 cause=%s", lines[0], kind, name, cause)
		}
		if !strings.Contains(w.Body.String(), "Reference: "+m[4]) {
			t.Fatalf("body %q does not show ref %s", w.Body.String(), m[4])
		}
		for _, s := range forbidden {
			if strings.Contains(logs.String(), s) {
				t.Fatalf("log contains %q: %q", s, logs.String())
			}
		}
	}
	// A page 500: the catalog search passes the query to PostgreSQL, which refuses a NUL byte (22021).
	w := send("GET", "/catalog?q="+handle+"%00&category=query-secret", nil)
	check(w, "page", "catalog", "22021")
	if strings.Contains(logs.String(), "/catalog") || strings.Contains(logs.String(), "q=") {
		t.Fatalf("log contains the URL path or query: %q", logs.String())
	}
	// An action 500: a NUL in a form value reaches the UPDATE.
	logs.mu.Lock()
	logs.b.Reset()
	logs.mu.Unlock()
	w = send("POST", "/notifications?query-secret=1", url.Values{"id": {handle + "\x00form-secret"}})
	check(w, "action", "/notifications", "22021")
}

// A 503 from a failing database names the route and the connection class; the ref reaches the user.
func TestServiceUnavailableIsLoggedWithRef(t *testing.T) {
	e := newTestApp(t)
	_, session := e.user("down_user", "buyer")
	// The database becomes unreachable: nothing listens on port 1.
	orig := e.A.db
	bad, err := OpenDB("postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	e.A.db = bad
	t.Cleanup(func() { e.A.db = orig; bad.Close() }) // before newTestApp's Close
	logs := captureLog(t)
	w := e.do("GET", "/orders?x=1", session, nil)
	if w.Code != 503 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	lines := logs.lines("http 5xx")
	if len(lines) != 1 {
		t.Fatalf("lines %q", lines)
	}
	m := http5xxLine.FindStringSubmatch(lines[0])
	if m == nil || m[1] != "page" || m[2] != "orders" || m[3] != "503" || m[5] != "db-connection" || !strings.Contains(w.Body.String(), "Reference: "+m[4]) {
		t.Fatalf("line %q body %q", lines[0], w.Body.String())
	}
}

func TestErrorCauseClasses(t *testing.T) {
	type wrapped struct{ error }
	for _, c := range []struct {
		err  error
		want string
	}{
		{&pgconn.PgError{Code: "23505", TableName: "payouts", ConstraintName: "payouts_order_id_key", Message: "duplicate key value (secret)"}, "23505 payouts payouts_order_id_key"},
		{fmt.Errorf("saving: %w", &pgconn.PgError{Code: "40001", Message: "could not serialize"}), "40001"},
		{&pgconn.PgError{Code: "23502", TableName: "orders"}, "23502 orders"},
		{context.DeadlineExceeded, "timeout"},
		{fmt.Errorf("x: %w", context.Canceled), "canceled"},
		{driver.ErrBadConn, "db-connection"},
		{sql.ErrConnDone, "db-connection"},
		{&pgconn.ConnectError{}, "db-connection"},
		{fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", errors.New("secret text"))), "*errors.errorString"},
		{template.ExecError{Name: "partials/x", Err: errors.New("secret")}, "template:partials/x"},
		{wrapped{errors.New("secret")}, "market.wrapped"},
		{nil, "none"},
	} {
		if got := errorCause(c.err); got != c.want {
			t.Errorf("errorCause(%#v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// A payout outcome is logged by sendPayouts itself, so it is not lost when the watcher is stopping (the
// watcher drops a pass's error once its context is cancelled): the payout id, order short id, currency,
// amount and the error class or txid, never the wallet's error text or the destination address.
func TestPayoutOutcomeLoggedDuringShutdown(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	failed := p.completedWithPayout("tx-log-failed")
	failedID := p.payoutID(failed)
	logs := captureLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	p.fake.sendErr = &rpcError{Method: "fake RPC send", Code: -6, Message: "Insufficient funds for fake-testnet-vendor"}
	p.fake.sendHook = cancel // SIGTERM arrives while the wallet call is in flight
	p.A.pollOnce(ctx)
	p.fake.sendErr, p.fake.sendHook = nil, nil
	want := "payout id=" + failedID + " order=" + failed[:8] + " currency=BTC amount=0.001 outcome=failed error=definite/rpc:-6 recorded=yes"
	if got := logs.lines("payout id=" + failedID + " "); len(got) != 1 || got[0] != want {
		t.Fatalf("failure log %q, want %q", got, want)
	}
	sent := p.completedWithPayout("tx-log-sent")
	sentID := p.payoutID(sent)
	ctx, cancel = context.WithCancel(context.Background())
	p.fake.sendHook = cancel
	p.A.pollOnce(ctx)
	p.fake.sendHook = nil
	_, txid, _, _ := p.payout(sent)
	want = "payout id=" + sentID + " order=" + sent[:8] + " currency=BTC amount=0.001 outcome=sent txid=" + txid + " recorded=yes"
	if got := logs.lines("payout id=" + sentID + " "); txid == "" || len(got) != 1 || got[0] != want {
		t.Fatalf("sent log %q, want %q", got, want)
	}
	for _, s := range []string{"Insufficient", "fake-testnet-vendor", failed, sent} {
		if strings.Contains(logs.String(), s) {
			t.Fatalf("log contains %q: %q", s, logs.String())
		}
	}
}

func TestPayoutErrorClass(t *testing.T) {
	for _, c := range []struct {
		cur  string
		err  error
		want string
	}{
		{"BTC", &rpcError{Method: "sendtoaddress", Code: -6, Message: "secret"}, "definite/rpc:-6"},
		{"XMR", &rpcError{Method: "transfer", Code: -38, Message: "secret"}, "ambiguous/rpc:-38"},
		{"XMR", &rpcError{Method: "transfer", Code: -17, Message: "secret"}, "definite/rpc:-17"},
		{"BTC", fmt.Errorf("send: %w", context.DeadlineExceeded), "ambiguous/timeout"},
		{"BTC", errors.New("bitcoin RPC unreachable: timed out"), "ambiguous/*errors.errorString"},
	} {
		if got := payoutErrorClass(c.cur, c.err); got != c.want {
			t.Errorf("payoutErrorClass(%s, %v) = %q, want %q", c.cur, c.err, got, c.want)
		}
	}
}
