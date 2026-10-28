package operator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newerReleaseRefusal is the application's own refusal (internal/market/migrate.go) as serve --check-startup prints it.
const newerReleaseRefusal = `startup check: refused: database migration failed: the database was upgraded by a newer release: it records migration(s) 999_from_a_newer_release, which this server does not include. Nothing was changed. Start that newer release again, or roll back by restoring the pre-upgrade backup as described in UPGRADING.md, section "Rolling back"`

const startCheckArgs = "run --rm --no-deps -T app serve --check-startup"

// fakeStartCheckDocker installs a docker that records each invocation's arguments, one line each, in the returned
// file, and runs body (shell) with "$*" available as $args.
func fakeStartCheckDocker(t *testing.T, body string) string {
	t.Helper()
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "docker-args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOCKER_CONFIG\"\nargs=\"$*\"\n" + body
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONFIG", record)
	return record
}

func dockerInvocations(t *testing.T, record string) []string {
	t.Helper()
	b, err := os.ReadFile(record)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// deploy doctor --start-check runs the app image once (compose run --rm --no-deps -T app serve --check-startup):
// no dependency service, no up, nothing published; its output passes the same secret redaction as deploy logs,
// and the run is bounded. Plain doctor never runs the app.
func TestDoctorStartCheckRunsAppOnceWithoutDeps(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := invoke(t, "doctor", "--dir", dir, "--start-check"); err == nil || !strings.Contains(err.Error(), "runtime configuration is missing") {
		t.Fatalf("start check without runtime configuration: %v (%s)", err, out)
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	dsn, err := secretValue(dir, c, "DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	// Docker gets a scrubbed environment (dockerEnvironment), so the secret is part of the fake's own output.
	record := fakeStartCheckDocker(t, `case "$args" in *" run "*) printf 'startup check: database: connected (%s)\nstartup check: passed\n' '`+dsn+`' ;; esac
exit 0
`)
	out, err := invoke(t, "doctor", "--dir", dir)
	if err != nil || strings.Contains(out, "startup check") {
		t.Fatalf("plain doctor: %v (%s)", err, out)
	}
	if calls := dockerInvocations(t, record); len(calls) != 1 || calls[0] != "--context default compose version" {
		t.Fatalf("plain doctor ran more than the Compose version check: %q", calls)
	}
	if err := os.WriteFile(record, nil, 0600); err != nil {
		t.Fatal(err)
	}
	out, err = invoke(t, "doctor", "--dir", dir, "--start-check")
	if err != nil || !strings.Contains(out, "startup check: passed") || !strings.Contains(out, "[REDACTED DATABASE_URL]") || strings.Contains(out, dsn) {
		t.Fatalf("doctor --start-check: %v (%s)", err, out)
	}
	calls := dockerInvocations(t, record)
	runs := 0
	for _, call := range calls {
		if strings.Contains(call, " up ") || strings.HasSuffix(call, " up") || strings.Contains(call, "--service-ports") {
			t.Fatalf("start check started services or published ports: %q", calls)
		}
		if strings.Contains(call, " run ") {
			runs++
			if !strings.HasSuffix(call, " "+startCheckArgs) || !strings.Contains(call, "--file "+filepath.Join(dir, "compose.json")) {
				t.Fatalf("start check invocation: %q", call)
			}
		}
	}
	if runs != 1 {
		t.Fatalf("start check ran the app %d times: %q", runs, calls)
	}

	// A refusal is printed and fails doctor; a run that does not finish is cut off at the bound.
	fakeStartCheckDocker(t, `case "$args" in *" run "*) echo 'startup check: refused: database connection failed: connection refused; check the host and port in DATABASE_URL and that PostgreSQL is running' ; exit 1 ;; esac
exit 0
`)
	if out, err = invoke(t, "doctor", "--dir", dir, "--start-check"); err == nil || !strings.Contains(out, "startup check: refused: database connection failed: connection refused") || !strings.Contains(err.Error(), "would not start") {
		t.Fatalf("refused start check: %v (%s)", err, out)
	}
	previous := startupCheckTimeout
	startupCheckTimeout = 300 * time.Millisecond
	t.Cleanup(func() { startupCheckTimeout = previous })
	fakeStartCheckDocker(t, `case "$args" in *" run "*) exec sleep 30 ;; esac
exit 0
`)
	began := time.Now()
	if out, err = invoke(t, "doctor", "--dir", dir, "--start-check"); err == nil || !strings.Contains(err.Error(), "deadline") || time.Since(began) > 10*time.Second {
		t.Fatalf("unbounded start check: %v after %s (%s)", err, time.Since(began), out)
	}
}

// A production up or redeploy whose app does not become healthy runs the startup check once and prints why
// (production keeps no container logs), redacted, then still fails: the operator sees the exact refusal and the
// rollback section, never the database password, and nothing else is started.
func TestProdRedeployFailurePrintsRedactedStartupError(t *testing.T) {
	dir := initProduction(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	receiptBytes, _ := json.Marshal(receipt{Project: c.Project, DockerContext: c.DockerContext, Version: 1})
	if err := os.WriteFile(filepath.Join(dir, "installation.json"), receiptBytes, 0600); err != nil {
		t.Fatal(err)
	}
	record := fakeStartCheckDocker(t, `case "$args" in
*" up "*) echo 'container opsecmkt-app-1 is unhealthy'; exit 1 ;;
*" run "*) echo 'startup check: database: connected to postgres://market:dedicated-password@db.internal/market'
  printf '%s\n' '`+newerReleaseRefusal+`'; exit 1 ;;
esac
exit 0
`)
	next := "example.invalid/market@sha256:" + strings.Repeat("12345678", 8)
	for _, command := range [][]string{{"redeploy", "--dir", dir, "--app-image", next}, {"up", "--dir", dir}} {
		if err := os.WriteFile(record, nil, 0600); err != nil {
			t.Fatal(err)
		}
		out, err := invoke(t, command...)
		if err == nil {
			t.Fatalf("%s: failed start reported success (%s)", command[0], out)
		}
		if !strings.Contains(out, newerReleaseRefusal) || !strings.Contains(out, "is unhealthy") || strings.Contains(out+err.Error(), "dedicated-password") || !strings.Contains(out, "[REDACTED DATABASE_URL]") {
			t.Fatalf("%s: startup cause missing or unredacted: %v (%s)", command[0], err, out)
		}
		if !strings.Contains(err.Error(), "Docker command failed") || !strings.Contains(err.Error(), "startup check") {
			t.Fatalf("%s: error does not point at the startup check: %v", command[0], err)
		}
		calls := dockerInvocations(t, record)
		if len(calls) != 2 || !strings.Contains(calls[0], " up -d --wait") || !strings.HasSuffix(calls[1], " "+startCheckArgs) {
			t.Fatalf("%s: docker invocations %q", command[0], calls)
		}
	}
	after, err := loadConfig(dir)
	if err != nil || after.AppImage != next {
		t.Fatalf("failed redeploy did not retain the desired image: %v", err)
	}
}

// A failed development start does not run the check: deploy logs shows the cause there.
func TestDevStartFailureDoesNotRunStartupCheck(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	receiptBytes, _ := json.Marshal(receipt{Project: c.Project, DockerContext: c.DockerContext, Version: 1})
	if err := os.WriteFile(filepath.Join(dir, "installation.json"), receiptBytes, 0600); err != nil {
		t.Fatal(err)
	}
	record := fakeStartCheckDocker(t, "exit 1\n")
	if _, err := invoke(t, "redeploy", "--dir", dir, "--app-image", "example.invalid/market@sha256:"+strings.Repeat("12345678", 8)); err == nil {
		t.Fatal("failed restart reported success")
	}
	if calls := dockerInvocations(t, record); len(calls) != 1 || strings.Contains(calls[0], " run ") {
		t.Fatalf("development failure ran the startup check: %q", calls)
	}
}

// An app image older than A-694 has no serve --check-startup: Go's flag package refuses the flag and prints its usage.
// The start check says the image predates it instead of pointing at a refusal that was never printed (QA95v-2), for
// doctor --start-check and for the check a failed production start runs (a rollback to an older image).
func TestStartCheckOnImageWithoutCheckStartupSaysItPredatesIt(t *testing.T) {
	const flagError = "flag provided but not defined: -check-startup"
	dir := initProduction(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	receiptBytes, _ := json.Marshal(receipt{Project: c.Project, DockerContext: c.DockerContext, Version: 1})
	if err := os.WriteFile(filepath.Join(dir, "installation.json"), receiptBytes, 0600); err != nil {
		t.Fatal(err)
	}
	fakeStartCheckDocker(t, `case "$args" in
*" up "*) echo 'container opsecmkt-app-1 is unhealthy'; exit 1 ;;
*" run "*) printf '%s\nUsage of serve:\n  -preview\n    \tread-only preview\n' '`+flagError+`' >&2; exit 2 ;;
esac
exit 0
`)
	out, err := invoke(t, "doctor", "--dir", dir, "--start-check")
	if err == nil || !strings.Contains(out, flagError) || !strings.Contains(err.Error(), "predates the startup check") || strings.Contains(err.Error(), "printed above") {
		t.Fatalf("doctor --start-check on an older image: %v (%s)", err, out)
	}
	previous := "example.invalid/market@sha256:" + strings.Repeat("0badc0de", 8)
	out, err = invoke(t, "redeploy", "--dir", dir, "--app-image", previous)
	if err == nil || !strings.Contains(err.Error(), "Docker command failed") || !strings.Contains(out, "is unhealthy") ||
		!strings.Contains(err.Error(), "predates the startup check") || strings.Contains(err.Error(), "printed above") {
		t.Fatalf("redeploy to an older image: %v (%s)", err, out)
	}

	// Any other failure still points at the refusal the check printed.
	fakeStartCheckDocker(t, `case "$args" in *" run "*) printf '%s\n' '`+newerReleaseRefusal+`'; exit 1 ;; esac
exit 0
`)
	if _, err = invoke(t, "doctor", "--dir", dir, "--start-check"); err == nil || !strings.Contains(err.Error(), "printed above") || strings.Contains(err.Error(), "predates") {
		t.Fatalf("refusal: %v", err)
	}
}

// A failed production redeploy --admin-tor-image replaced only tor-admin: the application was not touched, so the
// application's startup check does not run and the failure is reported as it is.
func TestProdAdminTorRedeployFailureDoesNotRunStartupCheck(t *testing.T) {
	dir, args := productionInitArgs(t, "database-ca.pem")
	args = append(args, "--exposure", "tor", "--admin-tor-image", testImage, "--admin-client-auth-dir", adminAuthInput(t))
	if out, err := invoke(t, args...); err != nil {
		t.Fatalf("init: %v (%s)", err, out)
	}
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	receiptBytes, _ := json.Marshal(receipt{Project: c.Project, DockerContext: c.DockerContext, Version: 1})
	if err := os.WriteFile(filepath.Join(dir, "installation.json"), receiptBytes, 0600); err != nil {
		t.Fatal(err)
	}
	record := fakeStartCheckDocker(t, `case "$args" in *" up "*) echo 'container opsecmkt-tor-admin-1 exited (1)'; exit 1 ;; esac
exit 0
`)
	next := "example.invalid/tor-admin@sha256:" + strings.Repeat("fedcba98", 8)
	out, err := invoke(t, "redeploy", "--dir", dir, "--admin-tor-image", next)
	if err == nil || strings.Contains(out+err.Error(), "startup check") {
		t.Fatalf("failed admin Tor redeploy: %v (%s)", err, out)
	}
	calls := dockerInvocations(t, record)
	if len(calls) != 1 || !strings.HasSuffix(calls[0], " up -d --wait --wait-timeout 600 --no-deps tor-admin") {
		t.Fatalf("failed admin Tor redeploy ran more than its own up: %q", calls)
	}
	if after, err := loadConfig(dir); err != nil || after.AdminTorImage != next || after.AppImage != c.AppImage {
		t.Fatalf("failed admin Tor redeploy did not retain the requested image only: %v", err)
	}
}
