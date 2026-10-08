package main

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHelperServerProcess runs main with the flags in OPSMKT_HELPER_ARGS, so the flag tests see the real exit
// code and output of the binary. It is skipped in a normal run.
func TestHelperServerProcess(t *testing.T) {
	args, ok := os.LookupEnv("OPSMKT_HELPER_ARGS")
	if !ok {
		t.Skip("helper process for the -healthcheck and -status tests")
	}
	os.Args = append([]string{"server"}, strings.Fields(args)...)
	main()
	os.Exit(0)
}

// runServer runs the server binary (this test binary re-entering main) with args and extra environment.
func runServer(t *testing.T, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperServerProcess$")
	cmd.Env = append(append(os.Environ(), "OPSMKT_HELPER_ARGS="+strings.Join(args, " ")), env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatal(err)
	}
	return out.String(), errOut.String(), code
}

func TestHealthcheckFlagExitCodes(t *testing.T) {
	serve := func(code int, body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(code)
			w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv.Listener.Addr().String()
	}
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	closed.Close()
	_, port, _ := net.SplitHostPort(serve(200, "ok"))
	for _, c := range []struct {
		name, addr string
		code       int
		stderr     string
	}{
		{"healthy", serve(200, "ok"), 0, ""},
		{"healthy on :port (all interfaces)", ":" + port, 0, ""},
		{"database down", serve(503, "unavailable\nReference: 0123abcd"), 1, "answered HTTP 503"},
		{"wrong body", serve(200, "okay"), 1, "answered HTTP 200"},
		{"not the app", serve(404, "ok"), 1, "answered HTTP 404"},
		{"nothing listening", closedAddr, 1, "no answer from the server"},
		{"bad ADDR", "8080", 1, "ADDR must be host:port"},
	} {
		t.Run(c.name, func(t *testing.T) {
			stdout, stderr, code := runServer(t, []string{"ADDR=" + c.addr, "DATABASE_URL=postgres://u:hc-secret-pw@127.0.0.1:1/x"}, "-healthcheck")
			if code != c.code || stdout != "" {
				t.Fatalf("exit %d stdout %q stderr %q; want exit %d and no stdout", code, stdout, stderr, c.code)
			}
			if c.code == 0 && stderr != "" {
				t.Fatalf("healthy check printed %q", stderr)
			}
			if !strings.Contains(stderr, c.stderr) || strings.Contains(stderr, "Reference") || strings.Contains(stderr, "hc-secret-pw") {
				t.Fatalf("stderr %q; want %q and no response body or secret", stderr, c.stderr)
			}
		})
	}
}

func TestHealthcheckURLFollowsADDR(t *testing.T) {
	for addr, want := range map[string]string{
		"":                "http://127.0.0.1:8080/healthz",
		":8080":           "http://127.0.0.1:8080/healthz",
		":9000":           "http://127.0.0.1:9000/healthz",
		"0.0.0.0:9001":    "http://127.0.0.1:9001/healthz",
		"[::]:9002":       "http://127.0.0.1:9002/healthz",
		"127.0.0.1:18081": "http://127.0.0.1:18081/healthz",
		"[::1]:8080":      "http://[::1]:8080/healthz",
	} {
		if got, err := healthcheckURL(addr); err != nil || got != want {
			t.Errorf("healthcheckURL(%q) = %q, %v; want %q", addr, got, err, want)
		}
	}
}

func TestStatusAndHealthcheckRefuseHostCommands(t *testing.T) {
	for _, args := range [][]string{
		{"-status", "-set-role", "alice", "buyer"},
		{"-healthcheck", "-reset-admin-factors", "alice"},
		{"-status", "-bootstrap-admin", "alice"},
		{"-healthcheck", "-reset-admin-password", "alice"},
		{"-status", "-site-name", "Example"},
		{"-healthcheck", "-set-role="},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			stdout, stderr, code := runServer(t, nil, args...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, "cannot be combined with host commands") {
				t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
			}
		})
	}
}
