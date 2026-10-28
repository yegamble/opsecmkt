package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"opsecmkt/internal/market"
	"os"
	"strings"
	"testing"
)

const checkStartupSetupToken = "check-startup-setup-token-5f2c9e7a1b4d8036cafe0123456789abcdef"

// emptyCheckSchema is a fresh PostgreSQL schema that no server has migrated, with a DSN scoped to it, the admin
// connection, and the password the DSN carries (empty when it has none).
func emptyCheckSchema(t *testing.T) (dsn, schema string, admin *sql.DB, password string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set TEST_DATABASE_URL for isolated PostgreSQL integration tests")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 8)
	rand.Read(b)
	schema = "test_check_startup_" + hex.EncodeToString(b)
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	password, _ = u.User.Password()
	if _, err = os.Stat("go.mod"); err != nil {
		t.Chdir("../..") // templates are read relative to the repository root
	}
	for _, k := range []string{"COMPOSE_FILE", "APP_SECRET_KEY", "RETENTION_ENABLED", "RETENTION_STATE_DIR", "MIGRATION_TIMEOUT", "ESCROW_XMR"} {
		t.Setenv(k, "")
	}
	t.Setenv("DATABASE_URL", u.String())
	t.Setenv("SETUP_TOKEN", checkStartupSetupToken)
	return u.String(), schema, admin, password
}

// snapshot is everything a startup would change: the recorded migrations, the audit log, the settings and the
// tables in schema. A check must leave all of it as it was.
func checkStartupSnapshot(t *testing.T, admin *sql.DB, schema string) string {
	t.Helper()
	var tables string
	if err := admin.QueryRow("SELECT COALESCE(string_agg(table_name, ',' ORDER BY table_name), '') FROM information_schema.tables WHERE table_schema=$1", schema).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(","+tables+",", ",schema_migrations,") {
		return tables
	}
	var migrations, audits, settings string
	for q, v := range map[string]*string{
		"SELECT string_agg(version || ' ' || name || ' ' || applied, ',' ORDER BY version) FROM " + schema + ".schema_migrations": &migrations,
		"SELECT count(*)::text || ' ' || COALESCE(max(id), 0)::text FROM " + schema + ".audit_events":                             &audits,
		"SELECT COALESCE(string_agg(key || '=' || value, ',' ORDER BY key), '') FROM " + schema + ".settings":                     &settings,
	} {
		if err := admin.QueryRow(q).Scan(v); err != nil {
			t.Fatal(err)
		}
	}
	return tables + "|" + migrations + "|" + audits + "|" + settings
}

// serve --check-startup on a database a newer release migrated (A-694): it exits 1 with the same refusal the
// server's own start gives, names the unknown migration and the rollback section, prints no secret, and changes
// nothing. Configuration refusals come first and never reach the database.
func TestCheckStartupRefusesNewerMigrationWithoutChanging(t *testing.T) {
	stdout, stderr, code := runServer(t, []string{"DATABASE_URL=postgres://u:check-secret-pw@127.0.0.1:1/x?sslmode=disable", "SETUP_TOKEN=" + checkStartupSetupToken, "MIGRATION_TIMEOUT=forever"}, "serve", "--check-startup")
	if code != 1 || !strings.Contains(stderr, "startup check: refused: MIGRATION_TIMEOUT must be a duration from 1s to 24h") || strings.Contains(stdout+stderr, "check-secret-pw") || strings.Contains(stdout, "database") {
		t.Fatalf("configuration refusal: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, stderr, code = runServer(t, nil, "-check-startup", "-preview"); code != 2 || !strings.Contains(stderr, "-check-startup cannot be combined") {
		t.Fatalf("-check-startup -preview: exit %d stderr %q", code, stderr)
	}
	if _, stderr, code = runServer(t, nil, "-check-startup", "-status"); code != 2 || !strings.Contains(stderr, "-check-startup cannot be combined") {
		t.Fatalf("-check-startup -status: exit %d stderr %q", code, stderr)
	}

	_, schema, admin, password := emptyCheckSchema(t)
	app, err := market.New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	app.Close()
	if _, err = admin.Exec("INSERT INTO " + schema + ".schema_migrations(version,name) VALUES(999,'from_a_newer_release')"); err != nil {
		t.Fatal(err)
	}
	before := checkStartupSnapshot(t, admin, schema)
	stdout, stderr, code = runServer(t, nil, "serve", "--check-startup")
	if code != 1 || !strings.Contains(stderr, "startup check: refused: database migration failed: the database was upgraded by a newer release: it records migration(s) 999_from_a_newer_release, which this server does not include. Nothing was changed.") ||
		!strings.Contains(stderr, `UPGRADING.md, section "Rolling back"`) || !strings.Contains(stdout, "configuration: valid") || !strings.Contains(stdout, "database: connected") {
		t.Fatalf("newer schema: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if password != "" && strings.Contains(stdout+stderr, password) || strings.Contains(stdout+stderr, schema) || strings.Contains(stdout+stderr, "postgres://") {
		t.Fatalf("check printed DATABASE_URL or a secret: %q %q", stdout, stderr)
	}
	if after := checkStartupSnapshot(t, admin, schema); after != before {
		t.Fatalf("a refused check changed the database:\n%s\n%s", before, after)
	}

	// The same number under another name: a different build migrated it.
	if _, err = admin.Exec("DELETE FROM " + schema + ".schema_migrations WHERE version=999; UPDATE " + schema + ".schema_migrations SET name='from_a_branch' WHERE version=(SELECT max(version) FROM " + schema + ".schema_migrations)"); err != nil {
		t.Fatal(err)
	}
	before = checkStartupSnapshot(t, admin, schema)
	if _, stderr, code = runServer(t, nil, "serve", "--check-startup"); code != 1 || !strings.Contains(stderr, "the database was migrated by a different build") || !strings.Contains(stderr, "from_a_branch") {
		t.Fatalf("different build: exit %d stderr %q", code, stderr)
	}
	if after := checkStartupSnapshot(t, admin, schema); after != before {
		t.Fatal("a refused check changed the database")
	}
}

// serve --check-startup exits 0 when the server would start: on a database no server has migrated it says how
// many migrations startup would apply and creates nothing; on a current schema it says so; while an application
// serves the database it says that, not a refusal; and it never takes a lock or writes a row.
func TestCheckStartupSucceedsOnCurrentSchema(t *testing.T) {
	_, schema, admin, _ := emptyCheckSchema(t)
	stdout, stderr, code := runServer(t, nil, "serve", "--check-startup")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "migrations: startup would apply ") || !strings.Contains(stdout, "startup check: passed") || !strings.Contains(stdout, "nothing was changed") {
		t.Fatalf("empty database: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if tables := checkStartupSnapshot(t, admin, schema); tables != "" {
		t.Fatalf("check created tables: %s", tables)
	}
	// Uninstalled without SETUP_TOKEN: nobody could complete /setup, so startup would refuse.
	if _, stderr, code = runServer(t, []string{"SETUP_TOKEN=", "APP_SECRET_KEY=check-startup-app-secret-key-9d3b7f1e5a2c8046beef0123456789abcdef"}, "serve", "--check-startup"); code != 1 || !strings.Contains(stderr, "SETUP_TOKEN is not set and installation is not complete") {
		t.Fatalf("no setup token: exit %d stderr %q", code, stderr)
	}

	app, err := market.New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	app.Close()
	before := checkStartupSnapshot(t, admin, schema)
	stdout, stderr, code = runServer(t, nil, "serve", "--check-startup")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "migrations: schema current at ") || !strings.Contains(stdout, "serving lock: free") ||
		!strings.Contains(stdout, "retention: off") || !strings.Contains(stdout, "startup check: passed") {
		t.Fatalf("current schema: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if after := checkStartupSnapshot(t, admin, schema); after != before {
		t.Fatalf("a passing check changed the database:\n%s\n%s", before, after)
	}

	// A serving application holds the serving lock: the check reports it and still passes.
	app, err = market.New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = runServer(t, nil, "serve", "--check-startup")
	app.Close()
	if code != 0 || stderr != "" || !strings.Contains(stdout, "serving lock: held by 1 session(s): an application is already serving this database") || !strings.Contains(stdout, "startup check: passed") {
		t.Fatalf("while serving: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	var holders int
	if err = admin.QueryRow("SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid IN (782493, 782499) AND database=(SELECT oid FROM pg_database WHERE datname=current_database())").Scan(&holders); err != nil || holders != 0 {
		t.Fatalf("advisory locks left behind: %d (%v)", holders, err)
	}
}

// The retention presence check (A-694): a database enrolled in retention whose external ledger is missing is
// refused with startup's own message, and the missing ledger is not created.
func TestCheckStartupRefusesMissingRetentionLedger(t *testing.T) {
	_, schema, admin, _ := emptyCheckSchema(t)
	app, err := market.New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	app.Close()
	if _, err = admin.Exec("INSERT INTO " + schema + ".settings(key,value) VALUES('retention_instance_id','enrolled-elsewhere')"); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := runServer(t, nil, "serve", "--check-startup"); code != 1 || !strings.Contains(stderr, "retention previously enrolled or restore pending: RETENTION_ENABLED and the current external RETENTION_STATE_DIR are required") {
		t.Fatalf("retention off: exit %d stderr %q", code, stderr)
	}
	dir := t.TempDir()
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	before := checkStartupSnapshot(t, admin, schema)
	_, stderr, code := runServer(t, []string{"RETENTION_ENABLED=on", "RETENTION_STATE_DIR=" + dir}, "serve", "--check-startup")
	if code != 1 || !strings.Contains(stderr, "current external retention state is required before traffic") {
		t.Fatalf("missing ledger: exit %d stderr %q", code, stderr)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("check wrote into the retention directory: %v", entries)
	}
	if after := checkStartupSnapshot(t, admin, schema); after != before {
		t.Fatal("a refused check changed the database")
	}
}
