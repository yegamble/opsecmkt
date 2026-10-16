package dependencies

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOfficialReleaseParsers(t *testing.T) {
	for _, tc := range []struct{ service, body, want string }{
		{"postgresql", `[{"major":"17","latestMinor":"10","supported":true},{"major":"18","latestMinor":"5","supported":true},{"major":"99","latestMinor":"0","supported":false}]`, "18.5"},
		{"monerod", `{"tag_name":"v0.18.5.3","draft":false,"prerelease":false}`, "0.18.5.3"},
		{"clamav", `{"tag_name":"clamav-1.5.4","draft":false,"prerelease":false}`, "1.5.4"},
		{"go", `[{"version":"go1.28rc1","stable":false},{"version":"go1.27.2","stable":true}]`, "1.27.2"},
		{"tor", `<a href="tor-0.4.9.12.tar.gz">release</a><a href="tor-0.4.10.1-alpha.tar.gz">alpha</a>`, "0.4.9.12"},
	} {
		got, err := parseLatest(tc.service, []byte(tc.body))
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %q %v", tc.service, got, err)
		}
	}
	if _, err := parseLatest("monerod", []byte(`{"tag_name":"v99.0.0","prerelease":true}`)); err == nil {
		t.Fatal("accepted prerelease")
	}
}

func TestSignedReportIntegrityAndBounds(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	publicPath := filepath.Join(dir, "pub")
	os.WriteFile(publicPath, []byte(hex.EncodeToString(pub)+"\n"), 0644)
	now := time.Now().UTC().Truncate(time.Second)
	report := Report{Version: 1, GeneratedAt: now}
	for _, service := range serviceNames {
		report.Entries = append(report.Entries, Entry{Service: service, Current: "1.0", Latest: "2.0", Source: sourceFor(service), CheckedAt: now})
	}
	if err := writeReport(reportPath, report, priv); err != nil {
		t.Fatal(err)
	}
	got, err := ReadVerified(reportPath, publicPath, now)
	if err != nil || len(got.Entries) != 6 {
		t.Fatalf("%v %+v", err, got)
	}
	b, _ := os.ReadFile(reportPath)
	b = bytes.Replace(b, []byte("2.0"), []byte("3.0"), 1)
	os.WriteFile(reportPath, b, 0644)
	if _, err := ReadVerified(reportPath, publicPath, now); err == nil {
		t.Fatal("accepted tampered report")
	}
	if err := os.Remove(reportPath); err != nil {
		t.Fatal(err)
	}
	report.GeneratedAt = now.Add(time.Hour)
	if err := writeReport(reportPath, report, priv); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadVerified(reportPath, publicPath, now); err == nil {
		t.Fatal("accepted future report")
	}
}

func TestWorkerPreservesUnrelatedDestinationAndSigningKeyRotation(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	path := filepath.Join(t.TempDir(), "existing")
	original := []byte("private-unrelated-configuration")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	report := Report{Version: 1, GeneratedAt: time.Now()}
	if err := writeReport(path, report, key); err == nil {
		t.Fatal("overwrote unrelated file")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, original) {
		t.Fatal("modified unrelated file")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := writeReport(path, report, key); err != nil {
		t.Fatal(err)
	}
	if err := writeReport(path, report, key); err != nil {
		t.Fatalf("same-key periodic refresh failed: %v", err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if err := writeReport(path, report, other); err == nil {
		t.Fatal("unexpectedly replaced differently signed report")
	}
}

func TestPollUsesOnlyFixedSourcesAndRecordsUnavailable(t *testing.T) {
	calls := map[string]int{}
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		calls[r.URL.String()]++
		body := `{"tag_name":"v0.18.5.3"}`
		switch r.URL.String() {
		case sourceFor("go"):
			body = `[{"version":"go1.27.2","stable":true}]`
		case sourceFor("postgresql"):
			body = `[{"major":"18","latestMinor":"5","supported":true}]`
		case sourceFor("tor"):
			body = `<a href="tor-0.4.9.12.tar.gz">x</a>`
		case sourceFor("clamav"):
			body = `malformed`
		case sourceFor("monerod"):
		default:
			t.Fatalf("unexpected source %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	report := poll(context.Background(), client, map[string]string{"monerod": "0.18.5.1"}, time.Now())
	if len(calls) != 5 || calls[sourceFor("monerod")] != 1 {
		t.Fatalf("unbounded/redundant requests: %v", calls)
	}
	for _, entry := range report.Entries {
		if entry.Service == "clamav" && (entry.Latest != "" || entry.Error != "unavailable") {
			t.Fatal("failed feed became a successful version")
		}
	}
}

func TestVersionComparisonAndWorkerFlags(t *testing.T) {
	if !Newer("1.10.0", "1.9.9") || Newer("1.9.9", "1.10.0") || Newer("1.0", "unknown") {
		t.Fatal("version comparison")
	}
	for _, args := range [][]string{{"--once"}, {"--report", "a", "--signing-key", "b", "--current", "bitcoin=1.0"}, {"--report", "a", "--signing-key", "b", "--interval", "1s"}} {
		if err := Run(context.Background(), args, io.Discard); err == nil {
			t.Fatal("invalid worker config accepted")
		}
	}
	t.Setenv("HTTPS_PROXY", "http://invalid.proxy:1")
	client, err := newClient("")
	if err != nil {
		t.Fatal(err)
	}
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("worker uses ambient proxy")
	}
	if _, err := newClient("https://bad:1"); err == nil {
		t.Fatal("accepted invalid SOCKS address")
	}
}

func TestWorkerReportCannotOverwriteSigningKey(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	alias := filepath.Join(dir, "alias")
	original := []byte(strings.Repeat("a", 64) + "\n")
	if err := os.WriteFile(key, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(key, alias); err != nil {
		t.Fatal(err)
	}
	for _, report := range []string{key, alias} {
		if err := Run(context.Background(), []string{"--once", "--report", report, "--signing-key", key}, io.Discard); err == nil {
			t.Fatal("accepted signing key output collision")
		}
	}
	after, _ := os.ReadFile(key)
	if !bytes.Equal(original, after) {
		t.Fatal("modified signing key")
	}
}
