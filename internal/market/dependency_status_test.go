package market

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/dependencies"
)

func TestDependencyTopologyNeverDisclosesEndpointIdentifiers(t *testing.T) {
	values := map[string]string{
		"OPSECMKT_PROFILE": "dev", "OPSECMKT_APP_HOST": "private-app.example",
		"DATABASE_URL": "postgres://secret-user:secret-password@private-db.example/db?sslmode=verify-full",
		"ESCROW_XMR":   "on", "ESCROW_XMR_WALLET_RPC_URL": "https://secret-rpc:secret-login@private-wallet.example:18083/json_rpc",
		"ESCROW_XMR_DAEMON_URL": "https://private-node.example:18081/json_rpc",
		"UPLOAD_MODE":           "isolated", "OPSECMKT_GATEWAY_URL": "https://private-gateway.example", "APP_MODE": "tor",
	}
	warnings := strings.Join(dependencyTopology(func(key string) string { return values[key] }), " ")
	for _, secret := range []string{"secret-user", "secret-password", "secret-rpc", "secret-login", "private-app.example", "private-db.example", "private-wallet.example", "private-node.example", "private-gateway.example"} {
		if strings.Contains(warnings, secret) {
			t.Fatalf("notice disclosed endpoint component %s", secret)
		}
	}
	if !strings.Contains(warnings, "physical host separation remains unverified") || !strings.Contains(warnings, "trusted with browser credentials and sessions") {
		t.Fatal("missing boundary notice")
	}
	values["DATABASE_URL"] = "postgres://secret:secret@127.0.0.1/db"
	warnings = strings.Join(dependencyTopology(func(key string) string { return values[key] }), " ")
	if !strings.Contains(warnings, "Database: configured on the app host") {
		t.Fatal("missed shared-host database")
	}
}

func TestDependencyStatusNonAdminDoesNotInspectFiles(t *testing.T) {
	t.Setenv("DEPENDENCY_REPORT_FILE", "/nonexistent-private-report")
	for _, user := range []*User{nil, {Role: "buyer"}, {Role: "moderator"}, {Role: "vendor"}} {
		d := &PageData{User: user, DependencyStatus: &DependencyStatusView{ReportState: "previous"}}
		if err := loadDependencyStatus(context.Background(), nil, nil, d); err != nil || d.DependencyStatus != nil {
			t.Fatal("non-admin received status")
		}
	}
	d := &PageData{User: &User{Role: "admin"}}
	if err := loadDependencyStatus(context.Background(), &App{preview: true}, nil, d); err != nil || !strings.Contains(d.DependencyStatus.ReportState, "preview") {
		t.Fatal("preview inspected deployment")
	}
}

func TestDependencyStatusAdminOnlyAndNoEndpointDisclosure(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("dependency_admin", "admin")
	_, buyer := e.user("dependency_buyer", "buyer")
	_, mod := e.user("dependency_mod", "moderator")
	t.Setenv("OPSECMKT_PROFILE", "dev")
	t.Setenv("OPSECMKT_APP_HOST", "secret-app.example")
	// These are display-only inventory inputs after app startup; never contacted.
	t.Setenv("ESCROW_XMR", "on")
	t.Setenv("ESCROW_XMR_WALLET_RPC_URL", "https://secret-user:secret-password@secret-wallet.example/json_rpc")
	t.Setenv("ESCROW_XMR_DAEMON_URL", "https://secret-node.example/json_rpc")
	t.Setenv("DEPENDENCY_REPORT_FILE", "/nonexistent-private-report")
	t.Setenv("DEPENDENCY_REPORT_PUBLIC_KEY", "/nonexistent-pinned-key")
	page := e.do("GET", "/admin/system", admin, nil)
	e.check(page, 200)
	if !strings.Contains(page.Body.String(), "Service isolation and updates") || !strings.Contains(page.Body.String(), "Go application runtime") {
		t.Fatal("admin panel missing")
	}
	installDependencyReportFixture(t)
	page = e.do("GET", "/admin/system", admin, nil)
	e.check(page, 200)
	for _, text := range []string{"Signature verified", "Report stale", "Newer upstream release available", "Review required: service isolation or dependency status needs attention."} {
		if !strings.Contains(page.Body.String(), text) {
			t.Fatalf("admin status missing %q", text)
		}
	}
	for _, secret := range []string{"secret-app.example", "secret-wallet.example", "secret-node.example", "secret-user", "secret-password", "/nonexistent-private-report"} {
		if strings.Contains(page.Body.String(), secret) {
			t.Fatal("admin panel disclosed endpoint or path")
		}
	}
	for _, token := range []string{buyer, mod, ""} {
		page = e.do("GET", "/admin/system", token, nil)
		if page.Code == 200 || strings.Contains(page.Body.String(), "Service isolation and updates") {
			t.Fatal("non-admin received panel")
		}
	}
	agExec(e, "UPDATE users SET role='buyer' WHERE id=$1", adminID)
	d := &PageData{User: &User{ID: adminID, Role: "admin"}}
	if err := loadDependencyStatus(t.Context(), e.A, httptest.NewRequest("GET", "/admin", nil), d); err != nil || d.DependencyStatus != nil {
		t.Fatal("stale role received dependency inventory")
	}
	// Also verify view structs carry no endpoint-bearing environment values.
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), os.Getenv("ESCROW_XMR_WALLET_RPC_URL")) {
		t.Fatal("view retained endpoint")
	}
}

func installDependencyReportFixture(t *testing.T, optional ...dependencies.Entry) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-96 * time.Hour)
	report := dependencies.Report{Version: 1, GeneratedAt: at}
	for service, source := range map[string]string{
		"postgresql":        "https://www.postgresql.org/versions.json",
		"monerod":           "https://api.github.com/repos/monero-project/monero/releases/latest",
		"monero-wallet-rpc": "https://api.github.com/repos/monero-project/monero/releases/latest",
		"tor":               "https://dist.torproject.org/", "clamav": "https://api.github.com/repos/Cisco-Talos/clamav/releases/latest",
		"go": "https://go.dev/dl/?mode=json",
	} {
		report.Entries = append(report.Entries, dependencies.Entry{Service: service, Source: source, Current: "1.0", Latest: "999.0", CheckedAt: at})
	}
	for _, entry := range optional {
		entry.CheckedAt = at
		report.Entries = append(report.Entries, entry)
	}
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(struct {
		Report    json.RawMessage `json:"report"`
		Signature string          `json:"signature"`
	}{payload, hex.EncodeToString(ed25519.Sign(private, payload))})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	reportPath, publicPath := filepath.Join(dir, "report.json"), filepath.Join(dir, "report.pub")
	if err := os.WriteFile(reportPath, body, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, []byte(hex.EncodeToString(public)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEPENDENCY_REPORT_FILE", reportPath)
	t.Setenv("DEPENDENCY_REPORT_PUBLIC_KEY", publicPath)
}

func TestDependencyStatusInferenceRequiresAuthenticatedInventory(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("runtime_inventory_admin", "admin")
	installDependencyReportFixture(t)
	page := e.do("GET", "/admin/system", admin, nil)
	e.check(page, 200)
	const label = "llama.cpp inference runtime"
	if strings.Contains(page.Body.String(), label) {
		t.Fatal("legacy report added an inference runtime row")
	}
	installDependencyReportFixture(t, dependencies.Entry{Service: dependencies.InferenceRuntimeService, Current: "b9999", Latest: "b10000", Source: "https://api.github.com/repos/ggml-org/llama.cpp/releases/latest"})
	page = e.do("GET", "/admin/system", admin, nil)
	e.check(page, 200)
	for _, text := range []string{label, "b9999", "b10000", "Not observed by this app: inference runs in a separate worker"} {
		if !strings.Contains(page.Body.String(), text) {
			t.Fatalf("authenticated inference inventory missing %q", text)
		}
	}
	if strings.Contains(page.Body.String(), "ggml-org") {
		t.Fatal("runtime inventory displayed a source endpoint")
	}
	path := os.Getenv("DEPENDENCY_REPORT_FILE")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), "b10000", "b10001", 1)), 0644); err != nil {
		t.Fatal(err)
	}
	page = e.do("GET", "/admin/system", admin, nil)
	e.check(page, 200)
	if strings.Contains(page.Body.String(), label) || strings.Contains(page.Body.String(), "b10001") {
		t.Fatal("unauthenticated inference inventory was shown")
	}
}
