package market

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A-553 (owner request 2026-10-18) and STAFF66-3: the System panel shows the market's own build, the Monero nodes'
// versions as this app observes them over their RPC, and a Health column; the Monero escrow watcher's health is one
// database row every app process reads. Simulated wallet RPC and daemon only: not live-chain evidence.

// adminPage is path as session sees it from a (any app process on the test database).
func adminPage(t *testing.T, a *App, path, session string) string {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.AddCookie(&http.Cookie{Name: "session", Value: session})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("GET %s: %d", path, w.Code)
	}
	return w.Body.String()
}

// dependencyRow is the HTML of the dependency table's row for service.
func dependencyRow(t *testing.T, deps, service string) string {
	t.Helper()
	i := strings.Index(deps, ">"+service+"<")
	if i < 0 {
		t.Fatalf("no %s row:\n%s", service, deps)
	}
	row := deps[i:]
	return row[:strings.Index(row, "</tr>")]
}

func wantAll(t *testing.T, what, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("%s lacks %q:\n%s", what, w, got)
		}
	}
}

func TestDependencyStatusObservesMonero(t *testing.T) {
	prevVersion, prevCommit := BuildVersion, BuildCommit
	BuildVersion, BuildCommit = "v9.9.9-test", "0123456789abcdef0123456789abcdef01234567"
	t.Cleanup(func() { BuildVersion, BuildCommit = prevVersion, prevCommit })
	t.Setenv("DEPENDENCY_REPORT_FILE", "")
	t.Setenv("DEPENDENCY_REPORT_PUBLIC_KEY", "")
	x := newXMREscrowEnv(t)
	_, admin := x.user("status_admin", "admin")
	private := []string{x.fake.daemon.URL, x.fake.wallet.URL, strings.TrimPrefix(x.fake.daemon.URL, "http://"), strings.TrimPrefix(x.fake.wallet.URL, "http://"), "127.0.0.1"}
	noPrivate := func(what, s string) {
		t.Helper()
		for _, p := range private {
			if strings.Contains(s, p) {
				t.Fatalf("%s names %q", what, p)
			}
		}
	}
	deps := func() string { return pageSection(t, adminPage(t, x.A, "/admin/system", admin), "dependency-status") }

	// Before any watcher pass the Monero rows say so; the market's own build is shown.
	d := deps()
	wantAll(t, "dependency table", d, `<th scope="col">Health</th>`)
	// scripts/ci-smoke.sh matches these exact strings against the image CI built (A-696).
	wantAll(t, "market row", dependencyRow(t, d, "OPSEC Market (this app)"), "<td>v9.9.9-test<br>", "This running build; commit 0123456789ab")
	wantAll(t, "daemon row before a pass", dependencyRow(t, d, "Monero daemon"), "No Monero escrow watcher pass recorded yet")

	// After a pass: the daemon's release from get_info and the wallet-rpc's interface version, observed by this app,
	// and each node reachable; the daemon synced at its height.
	x.mustPoll()
	d = deps()
	wantAll(t, "daemon row", dependencyRow(t, d, "Monero daemon"), "0.18.5.1", "Observed by this app", "Reachable", "Synced at height 120", "Last successful check:")
	wantAll(t, "wallet row", dependencyRow(t, d, "Monero wallet RPC"), "wallet RPC interface 1.31", "Observed by this app", "Reachable", "Last successful check:")
	wantAll(t, "Tor row", dependencyRow(t, d, "Tor"), "Not observed by this app")
	wantAll(t, "PostgreSQL row", dependencyRow(t, d, "PostgreSQL"), "Observed by this app", "Reachable")
	// No versions worker: the panel names the operator step instead of a bare per-row "Release check unavailable".
	wantAll(t, "dependency panel", d, "docs/install/service-topology.md#version-worker-host", "DEPENDENCY_REPORT_FILE", "Not compared: versions worker not configured")
	if strings.Contains(d, "Release check unavailable") {
		t.Fatal("rows still say only that the release check is unavailable")
	}
	noPrivate("dependency panel", d)
	noPrivate("stored health", agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", settingXMRHealth)[0])
	x.mustPoll() // within the hour: no second get_version
	if x.fake.countOf("get_version") != 1 {
		t.Fatalf("get_version calls: %d (the wallet-rpc once an hour; the daemon's release came from get_info)", x.fake.countOf("get_version"))
	}

	// A restricted daemon RPC hides its release: the app observes its RPC interface version instead.
	x.fake.edit(func(f *fakeXMREscrow) { f.restricted = true })
	x.restart()
	x.mustPoll()
	daemon := dependencyRow(t, deps(), "Monero daemon")
	wantAll(t, "restricted daemon row", daemon, "RPC interface 3.16", "Observed by this app", "does not report its release")
	if strings.Contains(daemon, "0.18.5.1") {
		t.Fatalf("a restricted daemon still shows the release it no longer reports:\n%s", daemon)
	}

	// The daemon goes away after startup: its row names the failing class and keeps the last successful check.
	x.fake.daemon.Close()
	if err := x.poll(); err == nil {
		t.Fatal("a pass with the daemon gone succeeded")
	}
	d = deps()
	wantAll(t, "unreachable daemon row", dependencyRow(t, d, "Monero daemon"), "Not reachable or failing", "get_info unreachable", "Last successful check:")
	noPrivate("dependency panel with the daemon gone", d)
}

// Escrow off: the Monero rows say the app does not check them, and nothing contacts a node.
func TestDependencyStatusMoneroOff(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("status_off_admin", "admin")
	d := pageSection(t, adminPage(t, e.A, "/admin/system", admin), "dependency-status")
	for _, service := range []string{"Monero daemon", "Monero wallet RPC"} {
		wantAll(t, service+" row", dependencyRow(t, d, service), "Monero escrow is off on this app")
	}
}

func TestXMREscrowAdminShowsPassFailures(t *testing.T) {
	x := newXMREscrowEnv(t)
	_, admin := x.user("health_admin", "admin")
	health := func(a *App) string {
		t.Helper()
		return pageSection(t, adminPage(t, a, "/admin/payments", admin), "xmr-escrow-health")
	}
	if h := health(x.A); !strings.Contains(h, "No Monero escrow watcher pass has been recorded") {
		t.Fatalf("before any pass:\n%s", h)
	}

	// A seed key that is not the pinned one: wallet creation stopped, shown on Payments and flagged on the overview.
	keyPath := x.keyPath
	_, other := testSeedKey(t, nil)
	t.Setenv("ESCROW_XMR_SEED_KEY", writeSeedKey(t, other))
	x.restart()
	x.mustPoll()
	wantAll(t, "health with another seed key", health(x.A), "Monero escrow wallet creation stopped", "ESCROW_XMR_SEED_KEY")
	wantAll(t, "overview", adminPage(t, x.A, "/admin", admin), "Monero escrow needs attention.")
	t.Setenv("ESCROW_XMR_SEED_KEY", keyPath)
	x.restart()
	x.mustPoll()
	h := health(x.A)
	if strings.Contains(h, "wallet creation stopped") || strings.Contains(h, "Last pass failed") {
		t.Fatalf("after the pinned key is back:\n%s", h)
	}
	wantAll(t, "healthy pass", h, "Last pass:", "Last good pass:", "completed without errors")

	// The wallet-rpc goes away after startup: the pass fails, the panel names the failing class and the last good
	// pass's age.
	x.fake.wallet.Close()
	if err := x.poll(); err == nil {
		t.Fatal("a pass with the wallet-rpc gone succeeded")
	}
	h = health(x.A)
	wantAll(t, "failing pass", h, "Last pass failed", "open_wallet unreachable", "Last good pass:", "ago")
	wantAll(t, "overview with a failing pass", adminPage(t, x.A, "/admin", admin), "Monero escrow needs attention.")

	// A second app process on the same database shows the same state, and so does this one after a restart.
	second, err := New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	wantAll(t, "second app", health(second), "Last pass failed", "open_wallet unreachable")
	x.restart()
	wantAll(t, "after a restart", health(x.A), "Last pass failed", "open_wallet unreachable", "Last good pass:")

	// Then the daemon too: the not-ready state with its class.
	x.fake.daemon.Close()
	x.A.retryEscrowXMR(context.Background(), &escrowRetry{last: x.A.escrowXMR.startState})
	x.poll()
	wantAll(t, "daemon gone", health(x.A), "Last pass failed", "not ready", "get_info unreachable")
	for _, p := range []string{x.fake.daemon.URL, x.fake.wallet.URL, "127.0.0.1"} {
		if strings.Contains(health(x.A), p) {
			t.Fatalf("health panel names %q", p)
		}
	}
}
