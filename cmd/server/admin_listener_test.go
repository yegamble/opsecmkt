package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"opsecmkt/internal/market"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Darwin's Unix socket path limit is shorter than t.TempDir's long generated test-name paths.
func adminSocketTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "market-admin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir // MkdirTemp creates mode 0700, independently of the process umask.
}

// Run the real main wiring with a migrated isolated database: one TCP/public listener and one Unix/private
// listener. Direct App unit calls cannot prove that main actually selects the right handler for each socket.
func TestMainPrivateAdminSocketKeepsPublicRequestsOut(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := market.BootstrapAdmin(context.Background(), db, "private_admin", bootstrapTestPassword, "Private Ingress Test"); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 64)
	hash := sha256.Sum256([]byte(token))
	if _, err := db.Exec("INSERT INTO sessions(token_hash,user_id,expires) SELECT $1,id,now()+interval '1 hour' FROM users WHERE handle='private_admin'", hex.EncodeToString(hash[:])); err != nil {
		t.Fatal(err)
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reservation.Addr().String()
	reservation.Close()
	socket := filepath.Join(adminSocketTestDir(t), "http.sock")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(bin, "-test.run=^TestHelperServerProcess$")
	command.Env = []string{
		"OPSMKT_HELPER_ARGS=", "ADDR=" + addr, "ADMIN_SOCKET=" + socket, "APP_MODE=tor", "COOKIE_SECURE=false",
		"DATABASE_URL=" + dsn, "APP_SECRET_KEY=integration-test-admin-key-5f2c9e7a1b4d8036cafe0123456789abcdef",
	}
	log, err := os.CreateTemp(t.TempDir(), "private-ingress-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	command.Stdout, command.Stderr = io.Discard, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() {
		command.Process.Signal(os.Interrupt)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(15 * time.Second):
			command.Process.Kill()
			<-done
			t.Error("server did not stop both listeners")
		}
	}()
	public := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ready := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		response, err := public.Get("http://" + addr + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		bytes, _ := os.ReadFile(log.Name())
		t.Fatalf("server did not become ready: %s", bytes)
	}
	// Readiness retries should be quick, but authenticated pages can use the app's 12-second request
	// budget, especially with race/coverage instrumentation and concurrent PostgreSQL suites in CI.
	public.Timeout = 15 * time.Second
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	private := &http.Client{Timeout: 15 * time.Second, Transport: transport}
	for _, path := range []string{"/admin", "/moderator", "/account"} {
		for _, tc := range []struct {
			client *http.Client
			want   int
		}{
			{public, 404}, {private, 200},
		} {
			r, err := http.NewRequest("GET", "http://"+addr+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			r.AddCookie(&http.Cookie{Name: "session", Value: token})
			r.Host = strings.Repeat("a", 56) + ".onion"
			r.Header.Set("X-Tor-Onion-Role", "admin")
			response, err := tc.client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != tc.want {
				t.Fatalf("main %s: status=%d want=%d", path, response.StatusCode, tc.want)
			}
		}
	}
}

func TestAdminListenerRestrictsSocketAndRefusesUnsafeExistingPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{"others", 0755}, {"group write", 0770},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			if ln, err := listenAdmin(newServer(filepath.Join(dir, "http.sock"), http.NotFoundHandler())); err == nil {
				ln.Close()
				t.Fatal("unsafe directory admitted")
			}
		})
	}
	dir := adminSocketTestDir(t)
	path := filepath.Join(dir, "http.sock")
	if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := newServer(path, http.NotFoundHandler())
	if ln, err := listenAdmin(srv); err == nil {
		ln.Close()
		t.Fatal("existing regular file was replaced")
	}
	if bytes, _ := os.ReadFile(path); string(bytes) != "preserve" {
		t.Fatal("existing file changed")
	}
	os.Remove(path)
	ln, err := listenAdmin(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0660 {
		t.Fatalf("socket permissions: %v, %v", info, err)
	}
	if duplicate, err := listenAdmin(srv); err == nil {
		duplicate.Close()
		t.Fatal("live socket was replaced")
	}
}

func TestServeAllServesUnixAndTCPAndWaitsForBothShutdowns(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	public := newServer("127.0.0.1:0", handler)
	dir := adminSocketTestDir(t)
	private := newServer(filepath.Join(dir, "http.sock"), handler)
	publicListener, err := listen(public)
	if err != nil {
		t.Fatal(err)
	}
	defer publicListener.Close()
	privateListener, err := listenAdmin(private)
	if err != nil {
		t.Fatal(err)
	}
	defer privateListener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveAll(ctx, []*http.Server{public, private}, []net.Listener{publicListener, privateListener}, time.Second)
	}()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", private.Addr)
	}}
	defer transport.CloseIdleConnections()
	for _, client := range []struct {
		client *http.Client
		url    string
	}{
		{&http.Client{Timeout: time.Second}, "http://" + publicListener.Addr().String()},
		{&http.Client{Timeout: time.Second, Transport: transport}, "http://private.onion"},
	} {
		response, err := client.client.Get(client.url)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || string(body) != "ok" {
			t.Fatalf("listener response: %q %v", body, err)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(private.Addr); !os.IsNotExist(err) {
		t.Fatalf("shutdown left a private socket: %v", err)
	}
}

func TestServeAllWaitsForFailedListenerInFlightRequests(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	public := newServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "ok")
	}))
	private := newServer(filepath.Join(adminSocketTestDir(t), "http.sock"), http.NotFoundHandler())
	publicListener, err := listen(public)
	if err != nil {
		t.Fatal(err)
	}
	defer publicListener.Close()
	privateListener, err := listenAdmin(private)
	if err != nil {
		t.Fatal(err)
	}
	defer privateListener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveAll(ctx, []*http.Server{public, private}, []net.Listener{publicListener, privateListener}, 5*time.Second)
	}()
	requested := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + publicListener.Addr().String())
		if err == nil {
			response.Body.Close()
		}
		requested <- err
	}()
	<-started
	publicListener.Close() // accepted handler survives the listener error
	select {
	case err := <-done:
		close(release)
		t.Fatalf("serveAll returned before failed listener's handler finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("failed listener error was hidden")
	}
	if err := <-requested; err != nil {
		t.Fatalf("accepted request did not finish: %v", err)
	}
}
