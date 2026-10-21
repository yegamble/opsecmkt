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

// A-589: the server refuses, before listening, an UPLOAD_CA that shares a
// certificate with MONERO_RPC_CA or DATABASE_CA; a dedicated CA still starts.
func TestUploadBackendRefusesSharedUploadCAAtStartup(t *testing.T) {
	ca, cert, key, _, _ := cliTestKeys(t)
	otherCA, _, _, _, _ := cliTestKeys(t)
	bundle := filepath.Join(t.TempDir(), "bundle.pem")
	a, err := os.ReadFile(otherCA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundle, append(a, b...), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UPLOAD_MODE", "isolated")
	t.Setenv("UPLOAD_BACKEND_ADDR", "127.0.0.1:0")
	t.Setenv("UPLOAD_CA", ca)
	t.Setenv("UPLOAD_CERT", cert)
	t.Setenv("UPLOAD_KEY", key)
	const want = "UPLOAD_CA must be a dedicated upload transport CA: it shares a certificate with MONERO_RPC_CA or DATABASE_CA"
	for _, tc := range []struct{ name, monero, database, want string }{
		{"monero", ca, "", want},
		{"database", "", ca, want},
		{"database bundle", "", bundle, want},
		{"dedicated", otherCA, otherCA, ""},
		{"unset", "", "", ""},
		{"unreadable monero", filepath.Join(t.TempDir(), "missing.pem"), "", "MONERO_RPC_CA: cannot read TLS CA file"},
	} {
		t.Setenv("MONERO_RPC_CA", tc.monero)
		t.Setenv("DATABASE_CA", tc.database)
		got := ""
		if err := checkListenerSettings(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s: startup check %q, want %q", tc.name, got, tc.want)
		}
		// The nil app is never served: the listener is closed unused.
		_, ln, err := privateUploadListener(nil)
		if err == nil {
			ln.Close()
		}
		if got = ""; err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s: listener %q, want %q", tc.name, got, tc.want)
		}
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

func TestServeInitialAccessFlagsRespectExistingEnvironmentWhenOmitted(t *testing.T) {
	for _, policy := range []struct {
		name, siteAccess, registrations string
		args                            []string
	}{
		{"omitted", "members", "closed", nil},
		{"explicit-public-open", "public", "open", []string{"--site-access", "public", "--registrations", "open"}},
		{"explicit-members-closed", "members", "closed", []string{"--site-access", "members", "--registrations", "closed"}},
	} {
		t.Run(policy.name, func(t *testing.T) {
			t.Setenv("INITIAL_SITE_ACCESS", "members")
			t.Setenv("INITIAL_REGISTRATIONS", "closed")
			originalArgs := os.Args
			t.Cleanup(func() { os.Args = originalArgs })
			if err := configureServe(policy.args, io.Discard); err != nil {
				t.Fatal(err)
			}
			if got := os.Getenv("INITIAL_SITE_ACCESS"); got != policy.siteAccess {
				t.Fatalf("INITIAL_SITE_ACCESS=%q want %q", got, policy.siteAccess)
			}
			if got := os.Getenv("INITIAL_REGISTRATIONS"); got != policy.registrations {
				t.Fatalf("INITIAL_REGISTRATIONS=%q want %q", got, policy.registrations)
			}
		})
	}
}

func TestServeInvalidInitialAccessFlagsFailBeforeMutatingEnvironment(t *testing.T) {
	for _, args := range [][]string{
		{"--site-access", ""}, {"--site-access", "private"}, {"--site-access", "Members"}, {"--site-access", "members "},
		{"--registrations", ""}, {"--registrations", "off"}, {"--registrations", "CLOSED"}, {"--registrations", " closed"},
	} {
		t.Run(strings.Join(args, "="), func(t *testing.T) {
			t.Setenv("INITIAL_SITE_ACCESS", "public")
			t.Setenv("INITIAL_REGISTRATIONS", "open")
			t.Setenv("ADDR", "original")
			originalArgs := os.Args
			err := configureServe(append([]string{"--addr", "changed"}, args...), io.Discard)
			if err == nil || !strings.Contains(err.Error(), args[0]+" must be") {
				t.Fatalf("invalid option accepted or wrong error: %v", err)
			}
			if os.Getenv("ADDR") != "original" || os.Getenv("INITIAL_SITE_ACCESS") != "public" || os.Getenv("INITIAL_REGISTRATIONS") != "open" || len(os.Args) != len(originalArgs) {
				t.Fatal("invalid option changed process settings")
			}
		})
	}
}

func TestServeBrandingUploadOriginFlag(t *testing.T) {
	for _, tc := range []struct {
		profile, origin, want string
		valid                 bool
	}{
		{"prod", "https://UPLOAD.MARKET.EXAMPLE:443", "https://upload.market.example", true},
		{"dev", "http://127.0.0.1:8080", "http://127.0.0.1:8080", true},
		{"prod", "", "", true},
		{"prod", "http://127.0.0.1:8080", "", false},
		{"dev", "https://operator:private-value@public.example", "", false},
		{"dev", "https://public.example/path", "", false},
	} {
		t.Run(tc.profile+"/"+tc.origin, func(t *testing.T) {
			t.Setenv("BRANDING_UPLOAD_ORIGIN", "https://existing.example")
			t.Setenv("OPSECMKT_PROFILE", "prod")
			t.Setenv("ADDR", "unchanged")
			originalArgs := os.Args
			t.Cleanup(func() { os.Args = originalArgs })
			err := configureServe([]string{"--addr", "changed", "--branding-upload-origin", tc.origin, "--profile", tc.profile}, io.Discard)
			if tc.valid {
				if err != nil || os.Getenv("BRANDING_UPLOAD_ORIGIN") != tc.want {
					t.Fatalf("flag not forwarded canonically: %v %q", err, os.Getenv("BRANDING_UPLOAD_ORIGIN"))
				}
			} else if err == nil || !strings.Contains(err.Error(), "BRANDING_UPLOAD_ORIGIN") || strings.Contains(err.Error(), "private-value") || os.Getenv("BRANDING_UPLOAD_ORIGIN") != "https://existing.example" || os.Getenv("ADDR") != "unchanged" {
				t.Fatalf("invalid flag mutated environment or leaked value: %v", err)
			}
		})
	}
}

// --official-addresses forwards the canonical list to OFFICIAL_ADDRESSES (A-572); an invalid list changes nothing.
func TestServeOfficialAddressesFlag(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		valid     bool
	}{
		{"HTTPS://Market.Example:443, https://mirror.example", "https://market.example,https://mirror.example", true},
		{"", "", true},
		{"https://private-value.example/path", "", false},
		{"http://private-value.example", "", false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("OFFICIAL_ADDRESSES", "https://existing.example")
			t.Setenv("OPSECMKT_PROFILE", "prod")
			t.Setenv("ADDR", "unchanged")
			originalArgs := os.Args
			t.Cleanup(func() { os.Args = originalArgs })
			err := configureServe([]string{"--addr", "changed", "--official-addresses", tc.raw}, io.Discard)
			if tc.valid {
				if err != nil || os.Getenv("OFFICIAL_ADDRESSES") != tc.want {
					t.Fatalf("flag not forwarded canonically: %v %q", err, os.Getenv("OFFICIAL_ADDRESSES"))
				}
			} else if err == nil || !strings.Contains(err.Error(), "OFFICIAL_ADDRESSES") || strings.Contains(err.Error(), "private-value") || os.Getenv("OFFICIAL_ADDRESSES") != "https://existing.example" || os.Getenv("ADDR") != "unchanged" {
				t.Fatalf("invalid flag mutated environment or leaked value: %v", err)
			}
		})
	}
}
