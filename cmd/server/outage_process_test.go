package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// cuttableProxy forwards TCP to a PostgreSQL server until cut: every connection is closed and new ones are closed on arrival, as
// a stopped or unreachable database looks to the application. PostgreSQL itself keeps running.
type cuttableProxy struct {
	listener net.Listener
	target   string

	mu   sync.Mutex
	down bool
	open map[net.Conn]struct{}
}

func newCuttableProxy(t *testing.T, target string) *cuttableProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cuttableProxy{listener: listener, target: target, open: map[net.Conn]struct{}{}}
	t.Cleanup(func() { listener.Close(); p.set(true) })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go p.forward(client)
		}
	}()
	return p
}

// track records a connection, or closes it and reports false while the proxy is cut.
func (p *cuttableProxy) track(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		conn.Close()
		return false
	}
	p.open[conn] = struct{}{}
	return true
}

func (p *cuttableProxy) forward(client net.Conn) {
	if !p.track(client) {
		return
	}
	server, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		client.Close()
		return
	}
	if !p.track(server) {
		client.Close()
		return
	}
	go func() { io.Copy(server, client); server.Close() }()
	io.Copy(client, server)
	client.Close()
}

// set cuts the proxy, or lets connections through again.
func (p *cuttableProxy) set(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
	if down {
		for conn := range p.open {
			conn.Close()
		}
		clear(p.open)
	}
}

// A-764 DA108-4 and QA108-5: a real server process, the binary's own main, with Monero escrow on, its daemon unreachable and its
// watcher running. Every database connection is cut under it at a TCP proxy, and it stays up and answers its health check with a
// failure. An hour of downtime cannot be waited for, so the test writes the stored time of the last step back while the
// connection is cut. When connections return the process finds the gap and rechecks by itself, and then the real host command
// asks for another recheck beside it, which its next pass runs. PostgreSQL is not stopped and no time passes on any clock, and
// no order is moved here: the moved deadlines are TestXMROutageMovesNearDeadlines's, and the panel the escrow browser journey's.
func TestXMROutageRealProcessDatabaseCut(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newCuttableProxy(t, parsed.Host)
	parsed.Host = proxy.listener.Addr().String()

	reserve := func() string {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}
	addr, closed := reserve(), reserve() // closed: no daemon or wallet-rpc answers there
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	seedKey := filepath.Join(root, "tests", "e2e", "fixtures", "pgp", "other.pub.asc")
	if _, err := os.Stat(seedKey); err != nil {
		t.Fatal(err)
	}
	bin, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(bin, "-test.run=^TestHelperServerProcess$")
	command.Dir = root
	command.Env = []string{
		"OPSMKT_HELPER_ARGS=", "ADDR=" + addr, "APP_MODE=clearnet", "COOKIE_SECURE=false", "HOME=" + t.TempDir(),
		"DATABASE_URL=" + parsed.String(), "SETUP_TOKEN=integration-test-setup-token-5f2c9e7a1b4d8036cafe0123456789abcdef",
		"PAYMENT_POLL_INTERVAL=1s", "ESCROW_XMR=on", "MONERO_NETWORK=stagenet", "ESCROW_XMR_SEED_KEY=" + seedKey,
		"ESCROW_XMR_WALLET_RPC_URL=http://" + closed, "ESCROW_XMR_DAEMON_URL=http://" + closed,
		"MONERO_WALLET_SECRET=outage-process-wallet-secret-local-only-not-for-production",
	}
	logFile, err := os.CreateTemp(t.TempDir(), "outage-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	stopped := false
	defer func() {
		if stopped {
			return
		}
		command.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			command.Process.Kill() // only the child this test started
			<-done
			t.Error("the server did not stop")
		}
	}()
	logged := func() string {
		t.Helper()
		text, err := os.ReadFile(logFile.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(text)
	}
	alive := func() {
		t.Helper()
		select {
		case err := <-done:
			stopped = true
			t.Fatalf("the server exited: %v\n%s", err, logged())
		default:
		}
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(60 * time.Second); !ok(); {
			alive()
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s\n%s", what, logged())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	setting := func(key string) string {
		t.Helper()
		var v sql.NullString
		if err := db.QueryRow("SELECT (SELECT value FROM settings WHERE key=$1)", key).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v.String
	}
	const (
		stepKey     = "escrow_xmr_last_deadline_step"
		requestKey  = "escrow_xmr_release_recheck"
		lastKey     = "escrow_xmr_last_recheck"
		waitKey     = "escrow_xmr_cancel_wait_until"
		startKey    = "escrow_xmr_cancel_wait_from"
		recheckLine = "monero escrow deadline recheck: cause=outage moved=0"
	)
	health := &http.Client{Timeout: 5 * time.Second}
	healthy := func() bool {
		response, err := health.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == http.StatusOK
	}
	lastRecheck := func() (rec struct {
		At    time.Time `json:"at"`
		Cause string    `json:"cause"`
		Moved int       `json:"moved"`
	},
	) {
		t.Helper()
		if v := setting(lastKey); v != "" {
			if err := json.Unmarshal([]byte(v), &rec); err != nil {
				t.Fatalf("the recheck's record %q: %v", v, err)
			}
		}
		return rec
	}

	// Running: the watcher has recorded a step through the proxy, with its daemon unreachable.
	waitFor("the server to answer", healthy)
	waitFor("the watcher's first step", func() bool { return setting(stepKey) != "" })

	// The database is gone from under it. It reports that and keeps running.
	proxy.set(true)
	waitFor("the health check to fail", func() bool { return !healthy() })
	const gapStart = "2026-01-01T00:00:00.000000Z" // long before now: no clock is stepped, the gap is the stored step's age
	if _, err := db.Exec("UPDATE settings SET value=$2 WHERE key=$1", stepKey, gapStart); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second) // three watcher passes that cannot reach the database
	alive()
	if v := setting(stepKey); v != gapStart {
		t.Fatalf("a step was recorded through a cut connection: %q", v)
	}
	if v := setting(lastKey); v != "" {
		t.Fatalf("a recheck ran with the database gone: %q", v)
	}

	// It returns: the same process finds the gap and runs the recheck, with no restart and no daemon.
	proxy.set(false)
	waitFor("the health check to return", healthy)
	waitFor("the recheck after the gap", func() bool { return lastRecheck().Cause == "outage" })
	if rec, request := lastRecheck(), setting(requestKey); rec.Moved != 0 || request != "" {
		t.Fatalf("after the gap: %+v, request %q", rec, request)
	}
	if v := setting(startKey); v != gapStart {
		t.Fatalf("the grace's start, want the stored step before the gap: %q", v)
	}
	if v := setting(waitKey); v == "" {
		t.Fatal("no end of the vendor's grace was recorded")
	}
	waitFor("the recheck's log line", func() bool { return strings.Count(logged(), recheckLine) == 1 })

	// The real command, in a process of its own, beside the running server: the server's next pass consumes the request.
	first := lastRecheck().At
	stdout, stderr, code := runServer(t, []string{"DATABASE_URL=" + dsn}, "-request-deadline-recheck")
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "Deadline recheck ") {
		t.Fatalf("the command: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	waitFor("the recheck the command asked for", func() bool { return setting(requestKey) == "" && lastRecheck().At.After(first) })
	waitFor("its log line", func() bool { return strings.Count(logged(), recheckLine) == 2 })
	if text := logged(); strings.Contains(text, "postgres://") || strings.Contains(text, parsed.Host) {
		t.Fatalf("the log names the database:\n%s", text)
	}
	var requests int
	if err := db.QueryRow("SELECT count(*) FROM audit_events WHERE action='Monero deadline recheck requested from the host'").Scan(&requests); err != nil || requests != 1 {
		t.Fatalf("the command's audit rows: %d, %v", requests, err)
	}

	// A shutdown after all this is still clean.
	command.Process.Signal(os.Interrupt)
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Errorf("server shutdown: %v\n%s", err, logged())
		}
	case <-time.After(15 * time.Second):
		t.Error("the server did not stop")
	}
}
