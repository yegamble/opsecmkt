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
	const appSecretKey = "status-test-app-secret-key-9d3b7f1e5a2c8046beef0123456789abcdef"
	// check runs -status with SETUP_TOKEN alone (the legacy key root), then with APP_SECRET_KEY as well: the
	// line names the root in use (keyroot=, for the installer) and never a value.
	check := func(want string) {
		t.Helper()
		for keyroot, environment := range map[string][]string{
			"setup_token":    {"DATABASE_URL=" + dsn, "SETUP_TOKEN=" + setupToken, "APP_SECRET_KEY="},
			"app_secret_key": {"DATABASE_URL=" + dsn, "SETUP_TOKEN=" + setupToken, "APP_SECRET_KEY=" + appSecretKey},
		} {
			stdout, stderr, code := runServer(t, environment, "-status")
			if line := want + " keyroot=" + keyroot + "\n"; code != 0 || stdout != line || stderr != "" {
				t.Fatalf("exit %d stdout %q stderr %q; want %q", code, stdout, stderr, line)
			}
			if strings.Contains(stdout, setupToken) || strings.Contains(stdout, appSecretKey) || password != "" && strings.Contains(stdout, password) {
				t.Fatalf("stdout carries a secret: %q", stdout)
			}
		}
	}
	// A database that was never migrated: nothing installed, no schema, and -status changes nothing.
	check("installed=false schema=000")
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
	check(fmt.Sprintf("installed=false schema=%03d", latest))
	if _, err = admin.Exec("INSERT INTO " + schema + ".settings(key,value) VALUES('installed','true')"); err != nil {
		t.Fatal(err)
	}
	check(fmt.Sprintf("installed=true schema=%03d", latest))

	// A-706: active staff without a second factor are counted on stderr (never by handle); the stdout line the
	// installer parses is unchanged. Suspended staff and staff with TOTP or PGP sign-in are not counted.
	for _, q := range []string{
		"INSERT INTO " + schema + ".users(id,handle,password_hash,role) VALUES('s1','status_factorless_admin','x','admin'),('s2','status_factorless_mod','x','moderator'),('s3','status_suspended_mod','x','moderator'),('s4','status_totp_mod','x','moderator'),('s5','status_buyer','x','buyer')",
		"UPDATE " + schema + ".users SET suspended_at=now() WHERE id='s3'",
		"UPDATE " + schema + ".users SET totp_enabled=true WHERE id='s4'",
	} {
		if _, err = admin.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	stdout, stderr, code := runServer(t, []string{"DATABASE_URL=" + dsn, "SETUP_TOKEN=" + setupToken, "APP_SECRET_KEY="}, "-status")
	if want := fmt.Sprintf("installed=true schema=%03d keyroot=setup_token\n", latest); code != 0 || stdout != want ||
		!strings.HasPrefix(stderr, "warning: 2 active staff account(s) have no second factor and cannot take staff actions") || strings.Contains(stderr, "status_") {
		t.Fatalf("factor-less staff: exit %d stdout %q stderr %q", code, stdout, stderr)
	}

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
