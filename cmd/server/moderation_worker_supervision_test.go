package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"opsecmkt/internal/market"
)

// moderationWorkerSchema migrates a fresh schema of TEST_DATABASE_URL and returns a handle on the base database, the
// schema name and a worker DSN that selects it.
func moderationWorkerSchema(t *testing.T) (*sql.DB, string, string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	b := make([]byte, 6)
	rand.Read(b)
	schema := "modsupervise_" + hex.EncodeToString(b)
	if _, err = db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	t.Chdir("../..")
	t.Setenv("DATABASE_URL", u.String())
	t.Setenv("SETUP_TOKEN", "integration-test-setup-token-5f2c9e7a1b4d8036cafe0123456789abcdef")
	app, err := market.New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	app.Close()
	t.Setenv("MODERATION_ENABLED", "")
	t.Setenv("DATABASE_URL_FILE", "")
	return db, schema, u.String()
}

// moderationRecordingListener accepts TCP connections and counts them and every byte received, then closes them: it
// stands in for a database server (or an attacker's) that a worker must not send a StartupMessage to.
type moderationRecordingListener struct {
	conns, bytes atomic.Int64
	port         string
}

func newModerationRecordingListener(t *testing.T, host string) *moderationRecordingListener {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	r := &moderationRecordingListener{}
	_, r.port, _ = net.SplitHostPort(l.Addr().String())
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			r.conns.Add(1)
			go func() {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				n, _ := io.Copy(io.Discard, io.LimitReader(c, 8))
				r.bytes.Add(n)
			}()
		}
	}()
	return r
}

// moderationNonLoopbackHost is an address of this host that is not a loopback address, so a listener on it is local
// to the test but "remote" to the worker's check. Without one, 0.0.0.0 (unspecified, not loopback) still reaches a
// local listener on both Linux and macOS.
func moderationNonLoopbackHost(t *testing.T) (listen, dial string) {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ip, _, err := net.ParseCIDR(a.String())
		if err == nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			if l, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0")); err == nil {
				l.Close()
				return ip.String(), ip.String()
			}
		}
	}
	return "127.0.0.1", "0.0.0.0"
}

// A-682 (SEC94-1): the worker holds the application's database authority. Before any network write it refuses a
// DSN that reaches a host other than a loopback address or a Unix socket unless that connection uses
// sslmode=verify-full, whatever OPSECMKT_PROFILE says (`env -i` drops it). verify-full, loopback and Unix-socket
// DSNs proceed to a dial as before.
func TestModerationWorkerRefusesUnverifiedRemoteDatabase(t *testing.T) {
	for _, name := range []string{"MODERATION_ENABLED", "DATABASE_URL_FILE", "PGHOST", "PGPORT", "PGSSLMODE", "PGSSLROOTCERT", "PGSERVICE", "PGSERVICEFILE"} {
		t.Setenv(name, "")
	}
	listenHost, remote := moderationNonLoopbackHost(t)
	const refused = "moderation-worker: remote database requires sslmode=verify-full"
	run := func(t *testing.T, dsn string, args ...string) error {
		t.Helper()
		t.Setenv("DATABASE_URL", dsn)
		var out bytes.Buffer
		err := runModerationWorker(context.Background(), args, &out)
		if strings.Contains(out.String(), "test-only-password") || err != nil && strings.Contains(err.Error(), "test-only-password") {
			t.Fatal("worker output names the DSN password")
		}
		return err
	}
	for _, c := range []struct{ name, dsn, profile string }{
		{"no sslmode", "postgres://worker:test-only-password@HOST/market", ""},
		{"require", "postgres://worker:test-only-password@HOST/market?sslmode=require", ""},
		{"disable", "postgres://worker:test-only-password@HOST/market?sslmode=disable", ""},
		{"prefer", "postgres://worker:test-only-password@HOST/market?sslmode=prefer", ""},
		{"allow", "postgres://worker:test-only-password@HOST/market?sslmode=allow", ""},
		{"verify-ca", "postgres://worker:test-only-password@HOST/market?sslmode=verify-ca", ""},
		{"key/value require", "host=HOSTNAME port=PORT user=worker password=test-only-password dbname=market sslmode=require", ""},
		{"loopback first, remote fallback", "postgres://worker:test-only-password@127.0.0.1:1,HOST/market?sslmode=disable", ""},
		{"development profile", "postgres://worker:test-only-password@HOST/market?sslmode=disable", "dev"},
		{"production profile", "postgres://worker:test-only-password@HOST/market?sslmode=require", "prod"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("OPSECMKT_PROFILE", c.profile)
			for _, args := range [][]string{{"--enabled", "--once"}, {"--disable"}} {
				l := newModerationRecordingListener(t, listenHost)
				dsn := strings.NewReplacer("HOSTNAME", remote, "PORT", l.port, "HOST", net.JoinHostPort(remote, l.port)).Replace(c.dsn)
				err := run(t, dsn, args...)
				if err == nil || err.Error() != refused {
					t.Fatalf("%v: got %v, want %q", args, err, refused)
				}
				time.Sleep(50 * time.Millisecond)
				if n, b := l.conns.Load(), l.bytes.Load(); n != 0 || b != 0 {
					t.Fatalf("%v: refused DSN reached the listener: %d connection(s), %d byte(s)", args, n, b)
				}
			}
		})
	}
	// verify-full to the same remote listener is attempted: the listener receives the TLS request and closes, so the
	// worker reports the database unavailable (not the transport refusal).
	l := newModerationRecordingListener(t, listenHost)
	err := run(t, "postgres://worker:test-only-password@"+net.JoinHostPort(remote, l.port)+"/market?sslmode=verify-full", "--enabled", "--once")
	if err == nil || err.Error() != "moderation-worker: database unavailable" {
		t.Fatalf("verify-full: %v", err)
	}
	if l.conns.Load() == 0 || l.bytes.Load() == 0 {
		t.Fatal("verify-full DSN did not dial the remote listener")
	}
	// Loopback and Unix-socket DSNs proceed as before, with any sslmode.
	for _, host := range []string{"127.0.0.1", "localhost"} {
		l := newModerationRecordingListener(t, "127.0.0.1")
		err := run(t, "postgres://worker:test-only-password@"+net.JoinHostPort(host, l.port)+"/market?sslmode=disable", "--enabled", "--once")
		if err == nil || err.Error() != "moderation-worker: database unavailable" {
			t.Fatalf("%s: %v", host, err)
		}
		if l.conns.Load() == 0 {
			t.Fatalf("%s: loopback DSN did not dial", host)
		}
	}
	socketDir := t.TempDir()
	if err := run(t, "host="+socketDir+" user=worker dbname=market sslmode=disable", "--enabled", "--once"); err == nil || err.Error() != "moderation-worker: database unavailable" {
		t.Fatalf("Unix socket: %v", err)
	}
}

// moderationOutageProxy forwards TCP connections to a database. While down it drops every forwarded connection and
// closes new ones at once, as a restarting PostgreSQL does.
type moderationOutageProxy struct {
	addr  string
	down  atomic.Bool
	mu    sync.Mutex
	conns []net.Conn
}

func newModerationOutageProxy(t *testing.T, target string) *moderationOutageProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &moderationOutageProxy{addr: l.Addr().String()}
	t.Cleanup(func() { l.Close(); p.cut() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if p.down.Load() {
				c.Close()
				continue
			}
			s, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, s)
			p.mu.Unlock()
			go func() { io.Copy(s, c); s.Close() }()
			go func() { io.Copy(c, s); c.Close() }()
		}
	}()
	return p
}

func (p *moderationOutageProxy) cut() {
	p.down.Store(true)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

// A-683 (STAFF94-1): a failed pass (here: the database drops every connection, then refuses new ones) is retried with
// a bounded backoff instead of ending the worker. After it reconnects, the worker records the outage as
// db_unavailable with its times, and stores its pass interval with the status.
func TestModerationWorkerSurvivesDBOutage(t *testing.T) {
	db, schema, dsn := moderationWorkerSchema(t)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newModerationOutageProxy(t, u.Host)
	u.Host = proxy.addr
	t.Setenv("DATABASE_URL", u.String())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var waits []time.Duration
	previous := moderationWorkerSleep
	t.Cleanup(func() { moderationWorkerSleep = previous })
	moderationWorkerSleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		switch len(waits) {
		case 1: // after the first pass: the database goes away
			proxy.cut()
		case 3: // after two failed passes: it is back
			proxy.down.Store(false)
		case 4:
			cancel()
		}
		return ctx.Err()
	}
	var out bytes.Buffer
	if err = runModerationWorker(ctx, []string{"--enabled", "--interval", "30s"}, &out); err != nil {
		t.Fatalf("worker exited: %v (output %q)", err, out.String())
	}
	if want := []time.Duration{30 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}; !slices.Equal(waits, want) {
		t.Fatalf("waits %v, want %v (output %q)", waits, want, out.String())
	}
	if n := strings.Count(out.String(), "moderation-worker: processed="); n != 2 {
		t.Fatalf("%d successful passes: %q", n, out.String())
	}
	for _, retry := range []string{"retrying in 5s", "retrying in 10s"} {
		if !strings.Contains(out.String(), "moderation-worker: pass unavailable; durable jobs preserved; "+retry+"\n") {
			t.Fatalf("missing %q: %q", retry, out.String())
		}
	}
	var failure string
	var interval int
	var outage bool
	if err = db.QueryRow(`SELECT failure_class,pass_interval_seconds,db_outage_from<=db_outage_until AND db_outage_until<=last_pass AND last_pass>now()-interval '1 minute' FROM `+schema+`.moderation_worker_status`).Scan(&failure, &interval, &outage); err != nil {
		t.Fatal(err)
	}
	if failure != "db_unavailable" || interval != 30 || !outage {
		t.Fatalf("status after the outage: failure=%q interval=%d outage recorded=%v", failure, interval, outage)
	}
}

// The backoff doubles from 5 seconds and never exceeds the pass interval or 5 minutes.
func TestModerationWorkerBackoffBounded(t *testing.T) {
	for _, c := range []struct {
		interval time.Duration
		want     []time.Duration
	}{
		{30 * time.Second, []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second}},
		{time.Hour, []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}},
		{10 * time.Second, []time.Duration{5 * time.Second, 10 * time.Second, 10 * time.Second}},
	} {
		var got []time.Duration
		var d time.Duration
		for range c.want {
			d = moderationWorkerBackoff(d, c.interval)
			got = append(got, d)
		}
		if !slices.Equal(got, c.want) {
			t.Fatalf("interval %s: %v, want %v", c.interval, got, c.want)
		}
	}
}

func moderationAuditRows(t *testing.T, db *sql.DB, schema, like string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT action FROM `+schema+`.audit_events WHERE action LIKE $1 AND user_id IS NULL ORDER BY id`, like)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err = rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// A-687 (STAFF94-7): turning capture on or off by host command writes one actorless audit row, only when the stored
// value changes. Restarting a worker while capture is already on, or disabling it twice, writes none.
func TestModerationEnableDisableAudited(t *testing.T) {
	db, schema, _ := moderationWorkerSchema(t)
	run := func(args ...string) {
		t.Helper()
		var out bytes.Buffer
		if err := runModerationWorker(context.Background(), args, &out); err != nil {
			t.Fatalf("%v: %v (output %q)", args, err, out.String())
		}
	}
	const on, off = "Moderation capture enabled by host command", "Moderation capture disabled by host command"
	audits := func() []string { return moderationAuditRows(t, db, schema, "Moderation capture %") }
	run("--disable") // already off after the migration
	if got := audits(); len(got) != 0 {
		t.Fatalf("disabling while off audited: %q", got)
	}
	run("--enabled", "--once")
	run("--enabled", "--once") // a restart while enabled
	if got := audits(); !slices.Equal(got, []string{on}) {
		t.Fatalf("after enable and restart: %q", got)
	}
	run("--disable")
	run("--enabled", "--once")
	if got := audits(); !slices.Equal(got, []string{on, off, on}) {
		t.Fatalf("--disable then --enabled: %q", got)
	}
	run("--disable")
	run("--disable")
	if got := audits(); !slices.Equal(got, []string{on, off, on, off}) {
		t.Fatalf("repeated --disable: %q", got)
	}
}

// A-687: a capacity flag that changes the stored limits writes one actorless audit row naming both values; one that
// changes nothing (an equal value, or a lower lifetime budget that is ignored) writes none.
func TestModerationCapacityFlagAudited(t *testing.T) {
	db, schema, _ := moderationWorkerSchema(t)
	run := func(args ...string) {
		t.Helper()
		var out bytes.Buffer
		if err := runModerationWorker(context.Background(), append([]string{"--enabled", "--once"}, args...), &out); err != nil {
			t.Fatalf("%v: %v (output %q)", args, err, out.String())
		}
	}
	audits := func() []string { return moderationAuditRows(t, db, schema, "Moderation limits changed %") }
	run("--max-pending", "256", "--max-records", "4096")
	if got := audits(); len(got) != 0 {
		t.Fatalf("unchanged limits audited: %q", got)
	}
	run("--max-pending", "300", "--max-records", "8192")
	want := []string{"Moderation limits changed from max-pending 256, max-records 4096, max-reserved-bytes 268435456 to max-pending 300, max-records 8192, max-reserved-bytes 268435456 by host command"}
	if got := audits(); !slices.Equal(got, want) {
		t.Fatalf("changed limits: %q", got)
	}
	run("--max-records", "4096") // below the stored budget: ignored, nothing changes
	run("--max-pending", "300")
	if got := audits(); !slices.Equal(got, want) {
		t.Fatalf("ignored or equal flags audited: %q", got)
	}
	run("--max-reserved-bytes", "536870912")
	want = append(want, "Moderation limits changed from max-pending 300, max-records 8192, max-reserved-bytes 268435456 to max-pending 300, max-records 8192, max-reserved-bytes 536870912 by host command")
	if got := audits(); !slices.Equal(got, want) {
		t.Fatalf("raised bytes: %q", got)
	}
}
