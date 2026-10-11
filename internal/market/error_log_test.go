package market

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

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
	http5xxLog.reset() // a previous test's burst of 5xx lines must not suppress this test's
	t.Cleanup(func() { log.SetOutput(w); log.SetFlags(flags) })
	return buf
}

// runBackground starts the registered background work (the escrow watchers, the sweeps) as the server does and returns
// a function that stops it and waits for it to finish; calling it again does nothing.
func runBackground(t *testing.T, a *App) (stop func()) {
	t.Helper()
	a.Start(context.Background())
	var once sync.Once
	return func() {
		t.Helper()
		once.Do(func() {
			done := make(chan struct{})
			go func() { a.stop(); a.background.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("background work did not stop")
			}
			a.stop = nil // allow another Start
		})
	}
}

// waitForLine waits until a captured line starts with prefix and returns the first such line.
func waitForLine(t *testing.T, logs *lockedBuffer, prefix string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		for _, l := range logs.lines(prefix) {
			if strings.HasPrefix(l, prefix) {
				return l
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no log line starting with %q: %q", prefix, logs.String())
		}
	}
}

// A-170 on escrow (A-493): the running escrow watcher's error line names its label and the error class only, never the
// node's error text, which may quote an address. Here the watch-only wallet refuses listreceivedbyaddress with a message
// carrying a marker and an address; the line keeps "wallet read failed", the method and the RPC code.
func TestLogsContainNoSecretsFromEscrowWatcher(t *testing.T) {
	x := newEscrowEnv(t)
	refund := regtestAddr("elog-refund")
	escrowAddr := x.mustCheckout(x.order, refund)
	const marker = "A493-NODE-MARKER"
	leaked := regtestAddr("elog-leaked-address")
	x.core.mu.Lock()
	x.core.txErr, x.core.txErrMsg = -5, marker+" Fixture refused to list "+leaked+" and "+escrowAddr
	x.core.mu.Unlock()
	logs := captureLog(t)
	stop := runBackground(t, x.A)
	defer stop()
	line := waitForLine(t, logs, "escrow watcher: ")
	stop()
	if line != "escrow watcher: escrow: wallet read failed: listreceivedbyaddress rpc:-5" {
		t.Fatalf("escrow watcher line %q", line)
	}
	for _, s := range []string{marker, leaked, escrowAddr, refund, x.order, "Fixture refused"} {
		if strings.Contains(logs.String(), s) {
			t.Fatalf("log contains %q: %q", s, logs.String())
		}
	}
	if x.state(x.order) != stateAwaitingPayment {
		t.Fatalf("an unread escrow moved its order to %s", x.state(x.order))
	}
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
	// A page 500: the product page's query fails. A NUL byte in its id no longer reaches PostgreSQL (refused with 400
	// since QA5W4B-5), so the products table is renamed away for this request (42P01, undefined table).
	if _, err := e.DB.Exec("ALTER TABLE products RENAME TO products_gone"); err != nil {
		t.Fatal(err)
	}
	w := send("GET", "/product?id="+handle+"&q=query-secret", nil)
	check(w, "page", "product", "42P01")
	if _, err := e.DB.Exec("ALTER TABLE products_gone RENAME TO products"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "/product") || strings.Contains(logs.String(), "id=") || strings.Contains(logs.String(), "q=") {
		t.Fatalf("log contains the URL path or query: %q", logs.String())
	}
	// An action 500: the UPDATE fails. A NUL in a form value no longer reaches it (refused with 400 since
	// QA5W3B-5), so a trigger in this test's database refuses it with the SQLSTATE the NUL raised.
	if _, err := e.DB.Exec(`CREATE FUNCTION refuse_notification_update() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'refused' USING ERRCODE='22021'; END$$;
		CREATE TRIGGER refuse_notification_update BEFORE UPDATE ON notifications FOR EACH STATEMENT EXECUTE FUNCTION refuse_notification_update()`); err != nil {
		t.Fatal(err)
	}
	logs.mu.Lock()
	logs.b.Reset()
	logs.mu.Unlock()
	w = send("POST", "/notifications?query-secret=1", url.Values{"id": {handle + "form-secret"}, formNonceField: {randomToken()[:32]}})
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

// QA-178-2: a statement timeout, a lock timeout, a server that is shutting down or refusing connections, and
// a lost connection (errorCause's db-connection class) answer the busy 503; application errors and a client
// that went away do not.
func TestDBBusyClasses(t *testing.T) {
	for _, code := range []string{"57014", "55P03", "57P01", "57P02", "57P03", "53300", "08000", "08001", "08003", "08004", "08006"} {
		if !dbBusy(fmt.Errorf("x: %w", &pgconn.PgError{Code: code})) {
			t.Errorf("SQLSTATE %s is not busy", code)
		}
	}
	for _, err := range []error{driver.ErrBadConn, sql.ErrConnDone, fmt.Errorf("x: %w", io.EOF), io.ErrUnexpectedEOF,
		&net.OpError{Op: "read", Err: errors.New("connection reset by peer")}} {
		if !dbBusy(err) || errorCause(err) != "db-connection" {
			t.Errorf("%#v: busy=%v cause=%q", err, dbBusy(err), errorCause(err))
		}
	}
	for _, err := range []error{nil, sql.ErrNoRows, errors.New("x"), fail(500, "x"), context.Canceled, context.DeadlineExceeded,
		&pgconn.ConnectError{},
		&pgconn.PgError{Code: "23505"}, &pgconn.PgError{Code: "40001"}, &pgconn.PgError{Code: "53100"}, &pgconn.PgError{Code: "42P01"}, &pgconn.PgError{Code: "08P01"}, &pgconn.PgError{Code: "08007"}} {
		if dbBusy(err) {
			t.Errorf("%#v is busy", err)
		}
	}
}

// A payout outcome is logged by sendPayouts itself, so it is not lost when the watcher is stopping (the
// watcher drops a pass's error once its context is cancelled): the payout id, order short id, currency,
// amount and the error class or txid, never the wallet's error text or the destination address.
func TestErrorCauseDroppedConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	}))
	defer srv.Close()
	c, err := newRPCClient("monero wallet", srv.URL, "2.0", true)
	if err != nil {
		t.Fatal(err)
	}
	err = c.call(context.Background(), "/json_rpc", "transfer", map[string]any{}, nil)
	var dropped *rpcDroppedError
	if !errors.As(err, &dropped) {
		t.Fatalf("not a dropped connection: %v", err)
	}
	for _, c := range []struct{ what, got, want string }{
		{"errorCause", errorCause(err), "*errors.errorString"},
		{"watcher", watcherCause(&watchError{label: "XMR", err: fmt.Errorf("wallet read: %w", err)}), "XMR: *errors.errorString"},
	} {
		if c.got != c.want {
			t.Errorf("%s class of a dropped connection = %q, want %q", c.what, c.got, c.want)
		}
	}
}

// watcherCause keeps a pass's labels (currency, order short id, payout id) and replaces every error text by
// its class, on one line; Error() keeps the text for payment_status and the admin pages.
func TestWatcherCause(t *testing.T) {
	pass := errors.Join(
		&watchError{label: "XMR", class: "wallet check failed: rpc:-13", err: errors.New("secret wallet text")},
		&watchError{label: "BTC", err: errors.Join(&watchError{class: "wallet read failed: rpc:-5", err: errors.New("secret node text")}, &watchError{label: "order 5f2c9e7a", err: &pgconn.PgError{Code: "40001", Message: "secret"}})},
	)
	got := watcherCause(pass)
	for _, want := range []string{"XMR: wallet check failed: rpc:-13", "BTC: wallet read failed: rpc:-5", "order 5f2c9e7a: 40001"} {
		if !strings.Contains(got, want) {
			t.Fatalf("cause %q lacks %q", got, want)
		}
	}
	for _, secret := range []string{"secret wallet text", "secret node text", "secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("cause disclosed %q", secret)
		}
	}
}
