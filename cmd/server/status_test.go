package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"opsecmkt/internal/market"
	"os"
	"strings"
	"testing"
)

func TestStatusFlagPrintsNoSecrets(t *testing.T) {
	const secret = "st4tus-secret-db-password"
	for name, c := range map[string]struct{ dsn, stderr string }{
		"no DATABASE_URL":   {"", "status: DATABASE_URL required"},
		"invalid URL":       {"postgres://opsecmkt:" + secret + "@[::1/opsecmkt", "status: database configuration invalid"},
		"nothing listening": {"postgres://opsecmkt:" + secret + "@127.0.0.1:1/opsecmkt?sslmode=disable&connect_timeout=5", "status: database connection failed: connection refused"},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := runServer(t, []string{"DATABASE_URL=" + c.dsn, "SETUP_TOKEN=" + secret + "-setup-token-0123456789abcdef"}, "-status")
			if code != 1 || stdout != "" || !strings.HasPrefix(stderr, c.stderr) {
				t.Fatalf("exit %d stdout %q stderr %q; want exit 1 and %q", code, stdout, stderr, c.stderr)
			}
			if strings.Contains(stderr, secret) || strings.Contains(stderr, "opsecmkt:") || strings.Contains(stderr, "127.0.0.1") {
				t.Fatalf("stderr repeats DATABASE_URL or a secret: %q", stderr)
			}
		})
	}

	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set TEST_DATABASE_URL for the database-backed -status checks")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 8)
	rand.Read(b)
	schema := "test_status_" + hex.EncodeToString(b)
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
	dsn := u.String()
	password, _ := u.User.Password()
	const setupToken = "status-test-setup-token-5f2c9e7a1b4d8036cafe0123456789abcdef"
	check := func(want string) {
		t.Helper()
		stdout, stderr, code := runServer(t, []string{"DATABASE_URL=" + dsn, "SETUP_TOKEN=" + setupToken}, "-status")
		if code != 0 || stdout != want || stderr != "" {
			t.Fatalf("exit %d stdout %q stderr %q; want %q", code, stdout, stderr, want)
		}
		if strings.Contains(stdout, setupToken) || password != "" && strings.Contains(stdout, password) {
			t.Fatalf("stdout carries a secret: %q", stdout)
		}
	}
	// A database that was never migrated: nothing installed, no schema, and -status changes nothing.
	check("installed=false schema=000\n")
	var tables int
	if err = admin.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema=$1", schema).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("-status created %d tables (err %v)", tables, err)
	}

	// Migrated by the server, not yet set up.
	t.Chdir("../..")
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("SETUP_TOKEN", setupToken)
	t.Setenv("APP_SECRET_KEY", "")
	t.Setenv("COMPOSE_FILE", "")
	app, err := market.New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	app.Close()
	var latest int
	if err = admin.QueryRow("SELECT max(version) FROM " + schema + ".schema_migrations").Scan(&latest); err != nil || latest < 1 {
		t.Fatalf("max migration %d (err %v)", latest, err)
	}
	check(fmt.Sprintf("installed=false schema=%03d\n", latest))
	if _, err = admin.Exec("INSERT INTO " + schema + ".settings(key,value) VALUES('installed','true')"); err != nil {
		t.Fatal(err)
	}
	check(fmt.Sprintf("installed=true schema=%03d\n", latest))

	// A wrong password against the real server: a class, never the URL or the password tried.
	if password != "" {
		wrong := *u
		wrong.User = url.UserPassword(u.User.Username(), secret)
		stdout, stderr, code := runServer(t, []string{"DATABASE_URL=" + wrong.String()}, "-status")
		if code != 1 || stdout != "" || !strings.Contains(stderr, "authentication failed") || strings.Contains(stderr, secret) || strings.Contains(stderr, u.Host) {
			t.Fatalf("wrong password: exit %d stdout %q stderr %q", code, stdout, stderr)
		}
	}
}
