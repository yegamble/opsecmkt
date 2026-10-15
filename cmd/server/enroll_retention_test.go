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

func TestEnrollRetentionRefusesBadInvocation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/none")
	t.Setenv("RETENTION_ENABLED", "on")
	t.Setenv("RETENTION_STATE_DIR", t.TempDir())
	for _, other := range []string{"preview", "healthcheck", "status", "reset-admin-password", "reset-admin-factors", "set-role", "bootstrap-admin", "site-name", "export-audit", "export-audit-from", "export-audit-dir"} {
		var out bytes.Buffer
		err := enrollRetention(t.Context(), given("enroll-retention", other), nil, &out)
		if err == nil || !strings.Contains(err.Error(), "-"+other) || out.Len() != 0 {
			t.Fatalf("mixed command %s: %v %q", other, err, out.String())
		}
	}
	for _, tc := range []struct{ enabled, path, want string }{
		{"off", t.TempDir(), "RETENTION_ENABLED=on"},
		{"on", "relative", "absolute clean path"},
		{"on", "", "absolute clean path"},
	} {
		t.Setenv("RETENTION_ENABLED", tc.enabled)
		t.Setenv("RETENTION_STATE_DIR", tc.path)
		var out bytes.Buffer
		err := enrollRetention(t.Context(), given("enroll-retention"), nil, &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) || out.Len() != 0 {
			t.Fatalf("bad config %+v: %v %q", tc, err, out.String())
		}
	}
}

func TestEnrollRetentionCLISQLASCIIOriginal(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set TEST_DATABASE_URL for disposable PostgreSQL integration tests")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	var salt [8]byte
	rand.Read(salt[:])
	name := "opsecmkt_enroll_ascii_" + hex.EncodeToString(salt[:])
	if _, err = admin.Exec("CREATE DATABASE " + name + " ENCODING 'SQL_ASCII' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0"); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE " + name + " WITH (FORCE)")
		admin.Close()
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	u.RawQuery = "sslmode=disable"
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE private_fixture(id integer PRIMARY KEY, body text); INSERT INTO private_fixture VALUES(1,convert_from(decode('fffe','hex'),'SQL_ASCII'))`); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", u.String())
	t.Setenv("RETENTION_ENABLED", "on")
	t.Setenv("RETENTION_STATE_DIR", dir)
	t.Setenv("SETUP_TOKEN", "")
	t.Setenv("ESCROW_BTC", "invalid-must-not-be-read")
	t.Setenv("ESCROW_XMR", "invalid-must-not-be-read")
	stdout, stderr, code := runServer(t, nil, "-enroll-retention")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "Original database retention enrolled") {
		t.Fatalf("SQL_ASCII host enrollment: %d %q %q", code, stdout, stderr)
	}
	var body string
	var runs, instance, version int
	if err = db.QueryRow(`SELECT encode(convert_to(body,'SQL_ASCII'),'hex'),(SELECT count(*) FROM retention_runs),(SELECT count(*) FROM settings WHERE key='retention_instance_id'),(SELECT max(version) FROM schema_migrations) FROM private_fixture WHERE id=1`).Scan(&body, &runs, &instance, &version); err != nil {
		t.Fatal(err)
	}
	if body != "fffe" || runs != 0 || instance != 1 || version < 116 {
		t.Fatalf("enrollment changed content or ran maintenance: body=%s runs=%d instance=%d version=%d", body, runs, instance, version)
	}
	if err = db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}
