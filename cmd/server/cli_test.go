package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/keymaterial"
)

func cliTestKeys(t *testing.T) (string, string, string, string, string) {
	t.Helper()
	d := t.TempDir()
	ca, caKey := filepath.Join(d, "ca.pem"), filepath.Join(d, "ca.key")
	server, serverKey := filepath.Join(d, "server.pem"), filepath.Join(d, "server.key")
	client, clientKey := filepath.Join(d, "client.pem"), filepath.Join(d, "client.key")
	for _, args := range [][]string{
		{"generate", "tls-ca", "--name", "test CA", "--cert-out", ca, "--key-out", caKey},
		{"generate", "tls-server", "--name", "backend", "--ca-cert", ca, "--ca-key", caKey, "--host", "127.0.0.1", "--cert-out", server, "--key-out", serverKey},
		{"generate", "tls-client", "--name", "gateway", "--ca-cert", ca, "--ca-key", caKey, "--cert-out", client, "--key-out", clientKey},
	} {
		if err := keymaterial.Run(args, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	return ca, server, serverKey, client, clientKey
}

func TestUploadTLSRequiresVerifiedClientAndServer(t *testing.T) {
	ca, cert, key, clientCert, clientKey := cliTestKeys(t)
	serverConfig, err := loadTLS(ca, cert, key, true)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	s.TLS = serverConfig
	var tlsOutput bytes.Buffer
	s.Config.ErrorLog = httpErrorLogger(&tlsOutput)
	s.StartTLS()
	defer s.Close()
	clientConfig, err := loadTLS(ca, clientCert, clientKey, false)
	if err != nil {
		t.Fatal(err)
	}
	request := func(c *tls.Config) error {
		tr := &http.Transport{TLSClientConfig: c, Proxy: nil}
		defer tr.CloseIdleConnections()
		cl := &http.Client{Transport: tr, Timeout: 2 * time.Second}
		resp, err := cl.Get(s.URL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("HTTP %d", resp.StatusCode)
			}
		}
		return err
	}
	if err := request(clientConfig); err != nil {
		t.Fatalf("authorized client: %v", err)
	}
	missing := clientConfig.Clone()
	missing.Certificates = nil
	if request(missing) == nil {
		t.Fatal("accepted missing client certificate")
	}
	wrongHost := clientConfig.Clone()
	wrongHost.ServerName = "unrelated.invalid"
	if request(wrongHost) == nil {
		t.Fatal("accepted wrong server hostname")
	}
	wrongCA := clientConfig.Clone()
	wrongCA.RootCAs = x509.NewCertPool()
	if request(wrongCA) == nil {
		t.Fatal("accepted untrusted server CA")
	}
	otherCA, _, _, otherCert, otherKey := cliTestKeys(t)
	wrongClient, err := loadTLS(otherCA, otherCert, otherKey, false)
	if err != nil {
		t.Fatal(err)
	}
	wrongClient.RootCAs = clientConfig.RootCAs
	if request(wrongClient) == nil {
		t.Fatal("accepted client signed by unrelated CA")
	}
	s.Close()
	if got := tlsOutput.String(); got != "HTTP TLS handshake refused (details suppressed)\n" {
		t.Fatalf("unsafe or unbounded TLS diagnostic: %q", got)
	}
}

func TestHealthcheckUsesVerifiedApplicationTLS(t *testing.T) {
	_, cert, key, _, _ := cliTestKeys(t)
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}
	s.StartTLS()
	defer s.Close()
	t.Setenv("TLS_CERT", cert)
	t.Setenv("OPSECMKT_APP_HOST", "127.0.0.1")
	if err := healthcheck(strings.TrimPrefix(s.URL, "https://"), time.Second); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPSECMKT_APP_HOST", "wrong-host.invalid")
	if healthcheck(strings.TrimPrefix(s.URL, "https://"), time.Second) == nil {
		t.Fatal("healthcheck ignored server hostname mismatch")
	}
}

func TestServerCLIHelpAndValidationBeforeDatabase(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"serve", "--help"}, {"upload-gateway", "--help"}, {"keys", "help"}, {"version"}} {
		stdout, stderr, code := runServer(t, nil, args...)
		if code != 0 || stdout+stderr == "" {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
	}
	_, stderr, code := runServer(t, []string{"DATABASE_URL=", "UPLOAD_MODE=isolated", "UPLOAD_BACKEND_ADDR="}, "serve")
	if code == 0 || !strings.Contains(stderr, "isolated uploads require") {
		t.Fatalf("preflight: %d %q", code, stderr)
	}
	_, stderr, code = runServer(t, nil, "unknown-command")
	if code == 0 || !strings.Contains(stderr, "unknown command") {
		t.Fatalf("unknown command: %d %q", code, stderr)
	}
}

func TestEmbeddedPreviewRunsOutsideCheckout(t *testing.T) {
	bin, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	// A helper process exercises the actual startup path with no web directory.
	cmd := exec.Command(bin, "-test.run=^TestHelperServerProcess$")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"OPSMKT_HELPER_ARGS=serve --preview --addr invalid", "APP_MODE=clearnet"}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err = cmd.Run()
	if err == nil || !strings.Contains(output.String(), "PREVIEW_ADDR must be a loopback") || strings.Contains(output.String(), "templates") {
		t.Fatalf("startup failed before embedded templates loaded: %v %s", err, output.String())
	}
}
