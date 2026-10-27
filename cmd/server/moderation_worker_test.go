package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"opsecmkt/internal/market"
)

func TestModerationWorkerDisabledAndHelpRequireNoSecretsOrNetwork(t *testing.T) {
	t.Setenv("MODERATION_ENABLED", "")
	t.Setenv("DATABASE_URL", "postgres://must-never-be-used.invalid/disabled")
	t.Setenv("APP_SECRET_KEY_FILE", "/nonexistent-secret")
	for _, args := range [][]string{nil, {"--help"}} {
		var out bytes.Buffer
		if err := runModerationWorker(context.Background(), args, &out); err != nil {
			t.Fatal(err)
		}
		if out.Len() == 0 || strings.Contains(out.String(), "must-never-be-used") {
			t.Fatal("missing or unsafe worker output")
		}
	}
}

func TestModerationWorkerRejectsInvalidConfiguration(t *testing.T) {
	t.Setenv("MODERATION_ENABLED", "")
	for _, args := range [][]string{{"--enabled", "--disable"}, {"--interval", "1ms"}, {"--batch", "101"}, {"--sla", "0s"}, {"--confidence-floor", "NaN"}, {"--new-actor-days", "-1"}, {"--prompt-version", "unreviewed"}, {"--enabled", "--model"}, {"--unknown"}} {
		if err := runModerationWorker(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestModerationWorkerRejectsRetiredPromptProtocol(t *testing.T) {
	t.Setenv("MODERATION_ENABLED", "")
	for _, retired := range []string{"moderation-v1", "moderation-v2", "moderation-v3", "moderation-v4", "moderation-v5"} {
		if err := runModerationWorker(context.Background(), []string{"--prompt-version", retired}, &bytes.Buffer{}); err == nil {
			t.Fatalf("retired model protocol %s was accepted", retired)
		}
	}
}

func TestModerationWorkerRejectsUnsafeCapacityBeforeDatabaseAccess(t *testing.T) {
	t.Setenv("MODERATION_ENABLED", "")
	for _, args := range [][]string{{"--max-pending", "0"}, {"--max-pending", "10001"}, {"--max-records", "0"}, {"--max-records", "1000001"}, {"--max-reserved-bytes", "524287"}, {"--max-reserved-bytes", "1099511627777"}} {
		err := runModerationWorker(context.Background(), args, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "invalid capacity limits") {
			t.Fatalf("unsafe capacity was not refused before database access: %v: %v", args, err)
		}
	}
}

func TestModerationWorkerDisableOverridesInheritedEnabledSetting(t *testing.T) {
	t.Setenv("MODERATION_ENABLED", "on")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_URL_FILE", "")
	err := runModerationWorker(context.Background(), []string{"--disable"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "database input required") {
		t.Fatalf("disable did not override inherited enabled setting: %v", err)
	}
}

// Every pass calls migration 146's backfill sweep, so the controller must not
// enable capture against a database that lacks it.
func TestModerationWorkerRequiresBackfillMigration(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := make([]byte, 6)
	rand.Read(b)
	schema := "modworker_" + hex.EncodeToString(b)
	if _, err = db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	if _, err = db.Exec(`CREATE TABLE ` + schema + `.schema_migrations(version integer PRIMARY KEY, name text NOT NULL, applied timestamptz NOT NULL DEFAULT now());
	 INSERT INTO ` + schema + `.schema_migrations(version,name) SELECT v,'m'||v FROM generate_series(1,145) v`); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	t.Setenv("MODERATION_ENABLED", "")
	t.Setenv("DATABASE_URL_FILE", "")
	t.Setenv("DATABASE_URL", u.String())
	err = runModerationWorker(context.Background(), []string{"--enabled", "--once"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "application migrations required") {
		t.Fatalf("worker enabled without migration 146: %v", err)
	}
	if _, err = db.Exec(`INSERT INTO ` + schema + `.schema_migrations(version,name) VALUES(146,'146_moderation_backfill.sql')`); err != nil {
		t.Fatal(err)
	}
	err = runModerationWorker(context.Background(), []string{"--enabled", "--once"}, &bytes.Buffer{})
	if err == nil || strings.Contains(err.Error(), "application migrations required") {
		t.Fatalf("migration 146 check did not pass once recorded: %v", err)
	}
}

func TestModerationWorkerReadsOnlyPrivateSelectedSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker-database")
	if err := os.WriteFile(path, []byte("test-only-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readModerationPrivate(path)
	if err != nil || string(got) != "test-only-value" {
		t.Fatalf("read=%q err=%v", got, err)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = readModerationPrivate(path); err == nil {
		t.Fatal("accepted public secret")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := path + ".link"
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = readModerationPrivate(link); err == nil {
		t.Fatal("accepted secret symlink")
	}
}

// A-654: the budget can be raised from the private panel without a restart, so a controller restarted with an
// older, lower --max-records or --max-reserved-bytes must not lower the stored value (or exit because it is below
// the allocations made since); it logs a fixed line instead. A higher flag still raises it.
func TestWorkerFlagNeverLowersStoredBudget(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := make([]byte, 6)
	rand.Read(b)
	schema := "modbudget_" + hex.EncodeToString(b)
	if _, err = db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
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
	limits := func() (v [3]int64) {
		t.Helper()
		if err := db.QueryRow(`SELECT max_pending,max_records,max_bytes FROM `+schema+`.moderation_capacity WHERE singleton`).Scan(&v[0], &v[1], &v[2]); err != nil {
			t.Fatal(err)
		}
		return v
	}
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := runModerationWorker(context.Background(), append([]string{"--enabled", "--once"}, args...), &out); err != nil {
			t.Fatalf("%v: %v (output %q)", args, err, out.String())
		}
		return out.String()
	}
	const ignored = "moderation-worker: stored moderation budget is higher; flag ignored"
	// Raised on the panel to 8192 / 512 MiB, with 5000 allocations made since: the old flags are below both.
	if _, err = db.Exec(`UPDATE ` + schema + `.moderation_capacity SET max_records=8192,max_bytes=536870912,allocated_records=5000`); err != nil {
		t.Fatal(err)
	}
	out := run("--max-records", "4096", "--max-reserved-bytes", "268435456")
	if strings.Count(out, ignored) != 2 || !strings.Contains(out, ignored+" (--max-records)") || !strings.Contains(out, ignored+" (--max-reserved-bytes)") {
		t.Fatalf("lower flags not reported as ignored: %q", out)
	}
	if got := limits(); got != [3]int64{256, 8192, 536870912} {
		t.Fatalf("lower flags changed the stored budget: %v", got)
	}
	// An equal flag is no change and no warning; a higher one still raises, and the other limit is kept.
	if out = run("--max-records", "10000", "--max-reserved-bytes", "536870912"); strings.Contains(out, ignored) {
		t.Fatalf("equal or higher flag reported as ignored: %q", out)
	}
	if got := limits(); got != [3]int64{256, 10000, 536870912} {
		t.Fatalf("higher --max-records: %v", got)
	}
	run("--max-reserved-bytes", "1073741824", "--max-pending", "300")
	if got := limits(); got != [3]int64{300, 10000, 1073741824} {
		t.Fatalf("higher --max-reserved-bytes: %v", got)
	}
	// One lower and one higher flag: the higher one applies, the lower one is reported.
	out = run("--max-records", "20000", "--max-reserved-bytes", "268435456")
	if strings.Count(out, ignored) != 1 || !strings.Contains(out, ignored+" (--max-reserved-bytes)") {
		t.Fatalf("mixed flags: %q", out)
	}
	if got := limits(); got != [3]int64{300, 20000, 1073741824} {
		t.Fatalf("mixed flags: %v", got)
	}
	// --max-pending is not a lifetime budget: it may still be lowered, never below the active jobs.
	run("--max-pending", "200")
	if got := limits(); got != [3]int64{200, 20000, 1073741824} {
		t.Fatalf("--max-pending: %v", got)
	}
}
