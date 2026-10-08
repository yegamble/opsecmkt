package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"opsecmkt/internal/market"
	"os"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

const bootstrapTestPassword = "operator-chosen-password"

// given reports the named flags as set on the command line.
func given(names ...string) func(string) bool {
	return func(name string) bool {
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
}

// Invocation mistakes are refused before stdin is read or the database is touched.
func TestBootstrapAdminRefusesBadInvocation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	for _, tc := range []struct {
		handle, site string
		flags        []string
		args         []string
		dsn          bool
		msg          string
	}{
		{handle: "first_admin", flags: []string{"bootstrap-admin", "preview"}, dsn: true, msg: "-preview"},
		{handle: "first_admin", flags: []string{"bootstrap-admin", "reset-admin-password"}, dsn: true, msg: "-reset-admin-password"},
		{site: "Market", flags: []string{"site-name"}, dsn: true, msg: "-site-name needs -bootstrap-admin"},
		{handle: "", flags: []string{"bootstrap-admin"}, dsn: true, msg: "give the first administrator's handle"},
		{handle: "first_admin", flags: []string{"bootstrap-admin"}, args: []string{bootstrapTestPassword}, dsn: true, msg: "never from the command line"},
		{handle: "bad-handle", flags: []string{"bootstrap-admin"}, dsn: true, msg: "3–32 character handle"},
		{handle: "first_admin", site: strings.Repeat("n", 81), flags: []string{"bootstrap-admin", "site-name"}, dsn: true, msg: "at most 80 characters"},
		{handle: "first_admin", flags: []string{"bootstrap-admin"}, msg: "DATABASE_URL required"},
	} {
		if !tc.dsn {
			t.Setenv("DATABASE_URL", "")
		}
		in := pipe(t, bootstrapTestPassword+"\n")
		var out, prompt bytes.Buffer
		err := bootstrapAdmin(context.Background(), tc.handle, tc.site, given(tc.flags...), tc.args, in, &out, &prompt)
		if err == nil || !strings.Contains(err.Error(), tc.msg) || out.Len() != 0 || prompt.Len() != 0 {
			t.Fatalf("%+v: err %v out %q prompt %q", tc, err, out.String(), prompt.String())
		}
		if rest, _ := readRest(in); rest != bootstrapTestPassword+"\n" {
			t.Fatalf("%+v: stdin was read", tc)
		}
	}
}

// cliSchema installs the application schema in a fresh PostgreSQL schema, the way the server does on start,
// and returns a DSN scoped to it. SETUP_TOKEN is then removed from the environment: the host tool must not
// need it.
func cliSchema(t *testing.T) string {
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
	schema := "test_cli_" + hex.EncodeToString(b)
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
	if _, err = os.Stat("go.mod"); err != nil {
		t.Chdir("../..") // templates are read relative to the repository root
	}
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("SETUP_TOKEN", "integration-test-setup-token-5f2c9e7a1b4d8036cafe0123456789abcdef")
	app, err := market.New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	app.Close()
	os.Unsetenv("SETUP_TOKEN") // restored by t.Setenv's cleanup
	return dsn
}

// installation reports the users, the installed flag, the site name and the setup audit rows.
func installation(t *testing.T, db *sql.DB) (users []string, installed bool, site string, audits int) {
	t.Helper()
	var list sql.NullString
	if err := db.QueryRow(`SELECT (SELECT string_agg(handle||':'||role, ',' ORDER BY handle) FROM users),
		EXISTS(SELECT 1 FROM settings WHERE key='installed' AND value='true'),
		(SELECT value FROM settings WHERE key='site_name'),
		(SELECT count(*) FROM audit_events WHERE action='Account created: admin' AND user_id IN (SELECT id FROM users))`).Scan(&list, &installed, &site, &audits); err != nil {
		t.Fatal(err)
	}
	if list.Valid {
		users = strings.Split(list.String, ",")
	}
	return users, installed, site, audits
}

// The CLI path against a real database: stdin pipe -> DATABASE_URL -> market.BootstrapAdmin, with no SETUP_TOKEN.
func TestBootstrapAdminCLIFromPipe(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out, prompt bytes.Buffer
	if err = bootstrapAdmin(context.Background(), "first_admin", " Host Market ", given("bootstrap-admin", "site-name"), nil, pipe(t, bootstrapTestPassword+"\n"), &out, &prompt); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "Setup complete: administrator first_admin created. Sign in with that handle and password.\n" {
		t.Fatalf("output %q", got)
	}
	if prompt.Len() != 0 {
		t.Fatalf("piped input prompted: %q", prompt.String())
	}
	users, installed, site, audits := installation(t, db)
	if len(users) != 1 || users[0] != "first_admin:admin" || !installed || site != "Host Market" || audits != 1 {
		t.Fatalf("users=%v installed=%v site=%q audits=%d", users, installed, site, audits)
	}
	var hash string
	if err = db.QueryRow("SELECT password_hash FROM users WHERE handle='first_admin'").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(bootstrapTestPassword)) != nil {
		t.Fatal("stored hash does not match the piped password")
	}
	if cost, _ := bcrypt.Cost([]byte(hash)); cost != 12 {
		t.Fatalf("bcrypt cost %d, want the application's 12", cost)
	}

	// A second run is refused and changes nothing.
	out.Reset()
	err = bootstrapAdmin(context.Background(), "late_admin", "", given("bootstrap-admin"), nil, pipe(t, bootstrapTestPassword+"\n"), &out, &prompt)
	if err == nil || !strings.Contains(err.Error(), "already set up") || out.Len() != 0 {
		t.Fatalf("second bootstrap: %v %q", err, out.String())
	}
	if users, _, site, audits = installation(t, db); len(users) != 1 || site != "Host Market" || audits != 1 {
		t.Fatalf("refused bootstrap changed the installation: %v %q %d", users, site, audits)
	}

	// A piped password that breaks the rule is refused with nothing written (fresh schema).
	cliSchemaFresh := cliSchema(t)
	db2, err := sql.Open("pgx", cliSchemaFresh)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	err = bootstrapAdmin(context.Background(), "first_admin", "", given("bootstrap-admin"), nil, pipe(t, "short-pass1\n"), &out, &prompt)
	if err == nil || !strings.Contains(err.Error(), "12–72 bytes") || out.Len() != 0 {
		t.Fatalf("short password: %v %q", err, out.String())
	}
	if users, installed, _, _ = installation(t, db2); len(users) != 0 || installed {
		t.Fatal("refused bootstrap installed the market")
	}
}

// On a terminal the password is asked twice with echo off, exactly as -reset-admin-password asks.
func TestBootstrapAdminCLIFromTerminal(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	terminal := func(hidden, restored *bool) func() (func(), error) {
		return func() (func(), error) {
			*hidden = true
			return func() { *restored = true }, nil
		}
	}
	var out, prompt bytes.Buffer
	var hidden, restored bool
	err = runBootstrap(context.Background(), dsn, "first_admin", "", strings.NewReader(bootstrapTestPassword+"\n"+bootstrapTestPassword+"x\n"), terminal(&hidden, &restored), &out, &prompt)
	if err == nil || !strings.Contains(err.Error(), "do not match") || out.Len() != 0 || !hidden || !restored {
		t.Fatalf("mismatch: %v out %q hidden=%v restored=%v", err, out.String(), hidden, restored)
	}
	if users, installed, _, _ := installation(t, db); len(users) != 0 || installed {
		t.Fatal("mismatched passwords installed the market")
	}

	prompt.Reset()
	hidden, restored = false, false
	if err = runBootstrap(context.Background(), dsn, "first_admin", "Terminal Market", strings.NewReader(bootstrapTestPassword+"\n"+bootstrapTestPassword+"\n"), terminal(&hidden, &restored), &out, &prompt); err != nil {
		t.Fatal(err)
	}
	if !hidden || !restored {
		t.Fatalf("echo hidden=%v restored=%v", hidden, restored)
	}
	if p := prompt.String(); !strings.Contains(p, "New password for administrator first_admin") || !strings.Contains(p, "Repeat the new password") || strings.Contains(p, bootstrapTestPassword) {
		t.Fatalf("prompt %q", p)
	}
	if got := out.String(); got != "Setup complete: administrator first_admin created. Sign in with that handle and password.\n" {
		t.Fatalf("output %q", got)
	}
	if users, installed, site, audits := installation(t, db); len(users) != 1 || !installed || site != "Terminal Market" || audits != 1 {
		t.Fatalf("users=%v installed=%v site=%q audits=%d", users, installed, site, audits)
	}
}

// Concurrent host bootstraps (separate connections, as separate processes would use) create one administrator.
func TestBootstrapAdminCLIConcurrentRunsCreateOneAdmin(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const n = 4
	errs := make([]error, n)
	outs := make([]bytes.Buffer, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = runBootstrap(context.Background(), dsn, "admin_"+string(rune('a'+i)), "", strings.NewReader(bootstrapTestPassword+"\n"), notTerminal, &outs[i], &bytes.Buffer{})
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for i, err := range errs {
		if err == nil {
			ok++
		} else if !strings.Contains(err.Error(), "already set up") || outs[i].Len() != 0 {
			t.Fatalf("run %d: %v %q", i, err, outs[i].String())
		}
	}
	if users, installed, _, audits := installation(t, db); ok != 1 || len(users) != 1 || !installed || audits != 1 {
		t.Fatalf("%d runs succeeded; users=%v installed=%v audits=%d", ok, users, installed, audits)
	}
}

// Neither the password, DATABASE_URL (or its password) nor SETUP_TOKEN appears in anything the tool writes,
// on success or failure.
func TestBootstrapAdminPrintsNoSecrets(t *testing.T) {
	const token = "integration-test-setup-token-5f2c9e7a1b4d8036cafe0123456789abcdef"
	secrets := func(dsn string) []string {
		s := []string{bootstrapTestPassword, dsn, token}
		if u, err := url.Parse(dsn); err == nil {
			if p, ok := u.User.Password(); ok && len(p) >= 8 { // a short local test password can occur in plain words
				s = append(s, p)
			}
		}
		return s
	}
	check := func(when string, dsn string, texts ...string) {
		t.Helper()
		for _, text := range texts {
			for _, s := range secrets(dsn) {
				if s != "" && strings.Contains(text, s) {
					t.Fatalf("%s: output %q contains a secret", when, text)
				}
			}
		}
	}
	// An unreachable database with a password in DATABASE_URL.
	bad := "postgres://market:dsn-secret-password-4d1f@127.0.0.1:1/market?sslmode=disable&connect_timeout=2"
	t.Setenv("DATABASE_URL", bad)
	t.Setenv("SETUP_TOKEN", token)
	var out, prompt bytes.Buffer
	err := bootstrapAdmin(context.Background(), "first_admin", "", given("bootstrap-admin"), nil, pipe(t, bootstrapTestPassword+"\n"), &out, &prompt)
	if err == nil || !strings.Contains(err.Error(), "database connection failed") {
		t.Fatalf("unreachable database: %v", err)
	}
	check("unreachable database", bad, err.Error(), out.String(), prompt.String())

	dsn := cliSchema(t)
	t.Setenv("SETUP_TOKEN", token) // present but unused
	out.Reset()
	if err = bootstrapAdmin(context.Background(), "first_admin", "", given("bootstrap-admin"), nil, pipe(t, bootstrapTestPassword+"\n"), &out, &prompt); err != nil {
		t.Fatal(err)
	}
	check("success", dsn, out.String(), prompt.String())
	err = bootstrapAdmin(context.Background(), "first_admin", "", given("bootstrap-admin"), nil, pipe(t, bootstrapTestPassword+"\n"), &out, &prompt)
	if err == nil {
		t.Fatal("second bootstrap succeeded")
	}
	check("already installed", dsn, err.Error(), out.String(), prompt.String())
	if !strings.Contains(out.String(), "first_admin") {
		t.Fatalf("success output does not name the handle: %q", out.String())
	}
}
