package market

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// untrustedTLSDB uses a real TLS endpoint with a self-signed certificate; pgx talks directly to it
// using PostgreSQL's direct TLS negotiation and rejects the certificate before any SQL can run.
func untrustedTLSDB(t *testing.T) *sql.DB {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{NextProtos: []string{"postgresql"}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "https://")
	dsn := "postgres://market:secret@" + addr + "/market?sslmode=verify-full&sslnegotiation=direct&connect_timeout=2"
	db, err := OpenDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// A-209: a reachable server with an untrusted TLS certificate is a configuration failure, not a
// transiently busy database. pgx wraps the certificate error in ConnectError just as it does a dial failure.
func TestDBBusyConnectErrorCertificateFailure(t *testing.T) {
	db := untrustedTLSDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := db.PingContext(ctx)
	var ce *pgconn.ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("TLS validation did not produce a pgx ConnectError: %T", err)
	}
	var untrusted x509.UnknownAuthorityError
	if !errors.As(err, &untrusted) {
		t.Fatalf("test did not reach certificate validation: %s", safeErrorTypes(err))
	}
	if dbBusy(err) || errorCause(err) != "db-connection" {
		t.Fatalf("TLS validation classified as retryable: busy=%v cause=%q", dbBusy(err), errorCause(err))
	}
	if dbConnectTransportFailure(errors.Join(err, syscall.ECONNREFUSED)) {
		t.Fatal("a second unreachable host masked certificate failure")
	}
}

// A real HTTP request normally reads installation status or its session before reaching the page/action
// mapper. Those early failures must not promise Retry-After for a certificate that needs operator repair.
func TestBadCertificateEarlyHTTPPathsReturn500(t *testing.T) {
	e := newTestApp(t)
	buyerID, buyerSession := e.user("bad_cert_buyer", "buyer")
	bad := untrustedTLSDB(t)
	original := e.A.db
	e.A.db = bad
	t.Cleanup(func() { e.A.db = original })
	for _, tc := range []struct {
		name string
		get  func() *httptest.ResponseRecorder
	}{
		{"installation read", func() *httptest.ResponseRecorder { return e.do("GET", "/", "", nil) }},
		{"session lookup", func() *httptest.ResponseRecorder { return e.do("GET", "/orders", buyerSession, nil) }},
		{"action begin", func() *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/logout", nil)
			r.PostForm = url.Values{}
			e.A.post(w, r, &User{ID: buyerID, Role: "buyer"}, buyerSession)
			return w
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.get()
			if w.Code != http.StatusInternalServerError || w.Header().Get("Retry-After") != "" ||
				strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "127.0.0.1") {
				t.Fatalf("certificate failure: status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
			}
		})
	}
	originalHealth := e.A.health
	e.A.health = bad
	t.Cleanup(func() { e.A.health = originalHealth })
	if w := e.do("GET", "/healthz", "", nil); w.Code != http.StatusInternalServerError || w.Header().Get("Retry-After") != "" {
		t.Fatalf("unhealthy database certificate response: status=%d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
}

// A refused PostgreSQL startup handshake is a real transport failure in ConnectError and remains retryable.
func TestDBBusyConnectErrorTransportFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			conn.Close()
		}
	}()
	db, err := OpenDB("postgres://market@" + listener.Addr().String() + "/market?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = db.PingContext(ctx)
	var ce *pgconn.ConnectError
	if !errors.As(err, &ce) || !dbBusyForContext(ctx, err) || errorCause(err) != "db-connection" {
		t.Fatalf("closed startup transport: type=%s busy=%v cause=%q", safeErrorTypes(err), dbBusy(err), errorCause(err))
	}
	w := httptest.NewRecorder()
	writeDBFailure(w, httptest.NewRequest("GET", "/", nil), err)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != busyRetryAfter {
		t.Fatalf("transient connect HTTP response: status=%d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
}

// pgx's configured connect_timeout has its own deadline inside a still-live HTTP request. A caller's
// already-expired context is different and must not be offered Retry-After.
func TestDBBusyConfiguredConnectTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	closed := make(chan struct{})
	defer close(closed)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			<-closed // deliberately never answer PostgreSQL startup
		}
	}()
	db, err := OpenDB("postgres://market@" + listener.Addr().String() + "/market?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = db.PingContext(requestCtx)
	var ce *pgconn.ConnectError
	if !errors.As(err, &ce) || requestCtx.Err() != nil || !dbBusyForContext(requestCtx, err) {
		t.Fatalf("configured timeout: type=%s request_done=%v busy=%v", safeErrorTypes(err), requestCtx.Err() != nil, dbBusy(err))
	}
	expired, end := context.WithCancel(context.Background())
	end()
	if dbBusyForContext(expired, err) {
		t.Fatal("expired caller context retained a retry promise")
	}
}

// A local proxy completes PostgreSQL startup against the real test server, then drops the next query.
// This pins the transport path without a synthetic error or a PostgreSQL backend termination.
func TestDBBusyEstablishedTCPConnectionLoss(t *testing.T) {
	upstreamDSN := testMigratedDSN(t)
	upstream, err := url.Parse(upstreamDSN)
	if err != nil {
		t.Fatal(err)
	}
	for _, reset := range []bool{false, true} {
		name := "graceful-EOF"
		if reset {
			name = "TCP-reset"
		}
		t.Run(name, func(t *testing.T) {
			proxy, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer proxy.Close()
			var drop atomic.Bool
			go func() {
				for {
					client, err := proxy.Accept()
					if err != nil {
						return
					}
					server, err := net.Dial("tcp", upstream.Host)
					if err != nil {
						client.Close()
						continue
					}
					go func() {
						defer client.Close()
						defer server.Close()
						go io.Copy(client, server)
						buf := make([]byte, 32<<10)
						for {
							n, err := client.Read(buf)
							if n > 0 && drop.Load() {
								if reset {
									if tcp, ok := client.(*net.TCPConn); ok {
										tcp.SetLinger(0)
									}
								}
								return
							}
							if n > 0 {
								if _, err := server.Write(buf[:n]); err != nil {
									return
								}
							}
							if err != nil {
								return
							}
						}
					}()
				}
			}()
			viaProxy := *upstream
			viaProxy.Host = proxy.Addr().String()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := pgconn.Connect(ctx, viaProxy.String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(context.Background())
			if conn.IsClosed() {
				t.Fatal("PostgreSQL connection closed before the proxy drop")
			}
			// Connect completed PostgreSQL's startup/authentication over this proxy. Drop its first query.
			drop.Store(true)
			_, err = conn.ExecParams(ctx, "SELECT 2", nil, nil, nil, nil).Close()
			if err == nil {
				t.Fatal("query succeeded after proxy dropped its established connection")
			}
			var ne net.Error
			if !errors.As(err, &ne) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("query loss was not a network error or EOF: %s", safeErrorTypes(err))
			}
			if !dbBusy(fmt.Errorf("query: %w", err)) || errorCause(err) != "db-connection" {
				t.Fatalf("query loss not retryable: busy=%v cause=%q", dbBusy(err), errorCause(err))
			}
		})
	}
}

// The export's settled-id read succeeds; only its later JOIN with users is locked out. This exercises
// the direct auditExport error branch and does not depend on the regular page/action error mapper.
func TestAuditExportQueryBusyReturns503(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", testAuditSeed)
	e := newTestApp(t)
	id, _ := e.user("busy_export_admin", "admin")
	e.mustExec("INSERT INTO audit_events(user_id,action) VALUES($1,'Busy export probe')", id)
	var upto int64
	if err := e.DB.QueryRow("SELECT max(id) FROM audit_events").Scan(&upto); err != nil {
		t.Fatal(err)
	}
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("LOCK TABLE users IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/admin/audit-export?upto=%d", upto)
	serve := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		// Export owns an arrival-derived build context; the ordinary request bound may already be over.
		arrival := context.Background()
		requestCtx, cancel := context.WithCancel(context.WithValue(arrival, arrivalKey{}, arrival))
		cancel()
		r := httptest.NewRequest("GET", path, nil).WithContext(requestCtx)
		auditExportHandler(e.A, w, r, &User{ID: id, Role: "admin"})
		return w
	}
	w := serve()
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != busyRetryAfter ||
		!strings.Contains(w.Body.String(), errBusyMsg) || w.Header().Get("Content-Disposition") != "" {
		t.Fatalf("busy audit export: status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatal(err)
	}
	w = serve()
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Busy export probe") {
		t.Fatalf("audit export did not recover: status=%d body=%q", w.Code, w.Body.String())
	}
}

// safeErrorTypes is diagnostic-only and never formats driver errors, which can contain a DSN.
func safeErrorTypes(err error) string {
	var types []string
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		types = append(types, fmt.Sprintf("%T", e))
		if joined, ok := e.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				walk(child)
			}
		} else {
			walk(errors.Unwrap(e))
		}
	}
	walk(err)
	return strings.Join(types, " -> ")
}
