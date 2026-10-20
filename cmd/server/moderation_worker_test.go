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
	for _, retired := range []string{"moderation-v1", "moderation-v2"} {
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

// Every pass calls migration 137's backfill sweep, so the controller must not
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
	 INSERT INTO ` + schema + `.schema_migrations(version,name) SELECT v,'m'||v FROM generate_series(1,136) v`); err != nil {
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
		t.Fatalf("worker enabled without migration 137: %v", err)
	}
	if _, err = db.Exec(`INSERT INTO ` + schema + `.schema_migrations(version,name) VALUES(137,'137_moderation_backfill.sql')`); err != nil {
		t.Fatal(err)
	}
	err = runModerationWorker(context.Background(), []string{"--enabled", "--once"}, &bytes.Buffer{})
	if err == nil || strings.Contains(err.Error(), "application migrations required") {
		t.Fatalf("migration 137 check did not pass once recorded: %v", err)
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
