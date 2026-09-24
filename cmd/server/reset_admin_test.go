package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"opsecmkt/internal/market"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func pipe(t *testing.T, s string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.WriteString(s); err != nil {
		t.Fatal(err)
	}
	w.Close()
	t.Cleanup(func() { r.Close() })
	return r
}

func notTerminal() (func(), error) { return nil, errNotTerminal }

func TestReadNewPasswordFromPipe(t *testing.T) {
	for _, tc := range []struct{ in, want, err string }{
		{"operator-chosen-password\n", "operator-chosen-password", ""},
		{"operator-chosen-password", "operator-chosen-password", ""},
		{"operator-chosen-password\r\n", "operator-chosen-password", ""},
		{" spaces are kept \n", " spaces are kept ", ""},
		{"first-line-password\nsecond-line\n", "", "single line"},
		{"", "", "no password"},
		{"\n", "", "no password"},
	} {
		got, err := readNewPassword(strings.NewReader(tc.in), &bytes.Buffer{}, "root_admin", notTerminal)
		if tc.err == "" && (err != nil || got != tc.want) {
			t.Fatalf("%q: got %q, %v", tc.in, got, err)
		}
		if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Fatalf("%q: err %v, want %q", tc.in, err, tc.err)
		}
	}
	// Our own echoOff treats a pipe as not a terminal.
	f := pipe(t, "operator-chosen-password\n")
	if got, err := readNewPassword(f, &bytes.Buffer{}, "root_admin", func() (func(), error) { return echoOff(f) }); err != nil || got != "operator-chosen-password" {
		t.Fatalf("pipe via echoOff: %q, %v", got, err)
	}
}

func TestReadNewPasswordFromTerminal(t *testing.T) {
	for _, tc := range []struct{ in, want, err string }{
		{"operator-chosen-password\noperator-chosen-password\n", "operator-chosen-password", ""},
		{"operator-chosen-password\r\noperator-chosen-password\r\n", "operator-chosen-password", ""},
		{"operator-chosen-password\noperator-chosen-passwrd\n", "", "do not match"},
		{"operator-chosen-password\n", "", "no password entered"},
	} {
		hidden, restored := false, false
		var prompt bytes.Buffer
		got, err := readNewPassword(strings.NewReader(tc.in), &prompt, "root_admin", func() (func(), error) {
			hidden = true
			return func() { restored = true }, nil
		})
		if !hidden || !restored {
			t.Fatalf("%q: echo hidden=%v restored=%v", tc.in, hidden, restored)
		}
		if tc.err == "" && (err != nil || got != tc.want) {
			t.Fatalf("%q: got %q, %v", tc.in, got, err)
		}
		if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Fatalf("%q: err %v, want %q", tc.in, err, tc.err)
		}
		if p := prompt.String(); !strings.Contains(p, "New password for administrator root_admin") || strings.Contains(p, "operator-chosen") {
			t.Fatalf("prompt %q", p)
		}
	}
	// A terminal whose echo cannot be turned off is refused rather than read visibly.
	want := errors.New("cannot hide typed input")
	if _, err := readNewPassword(strings.NewReader("x\nx\n"), &bytes.Buffer{}, "root_admin", func() (func(), error) { return nil, want }); !errors.Is(err, want) {
		t.Fatalf("echo failure: %v", err)
	}
}

// Invocation mistakes are refused before stdin is read or the database is touched.
func TestResetAdminPasswordRefusesBadInvocation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	for _, tc := range []struct {
		handle  string
		preview bool
		args    []string
		dsn     bool
		msg     string
	}{
		{handle: "root_admin", preview: true, dsn: true, msg: "-preview"},
		{handle: "", dsn: true, msg: "give the administrator's handle"},
		{handle: "root_admin", args: []string{"operator-chosen-password"}, dsn: true, msg: "never from the command line"},
		{handle: "root_admin", msg: "DATABASE_URL required"},
	} {
		if !tc.dsn {
			t.Setenv("DATABASE_URL", "")
		}
		in := pipe(t, "operator-chosen-password\n")
		var out bytes.Buffer
		err := resetAdminPassword(context.Background(), tc.handle, tc.preview, tc.args, in, &out, &out)
		if err == nil || !strings.Contains(err.Error(), tc.msg) || out.Len() != 0 {
			t.Fatalf("%+v: err %v out %q", tc, err, out.String())
		}
		if rest, _ := readRest(in); rest != "operator-chosen-password\n" {
			t.Fatalf("%+v: stdin was read", tc)
		}
	}
}

func readRest(f *os.File) (string, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(f)
	return b.String(), err
}

// The CLI path against a real database: stdin pipe -> DATABASE_URL -> market.ResetAdminPassword.
func TestResetAdminPasswordCLI(t *testing.T) {
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

	// Install the schema the way the server does (templates are read relative to the repository root).
	t.Chdir("../..")
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("SETUP_TOKEN", "integration-test-setup-token-5f2c9e7a1b4d8036cafe0123456789abcdef")
	app, err := market.New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	app.Close()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old, _ := bcrypt.GenerateFromPassword([]byte("the-forgotten-password"), bcrypt.MinCost)
	for _, q := range []string{
		"INSERT INTO users(id,handle,password_hash,role) VALUES('a1','root_admin','" + string(old) + "','admin'),('b1','plain_buyer','" + string(old) + "','buyer')",
		"INSERT INTO sessions(token_hash,user_id,expires) VALUES('s1','a1',now()+interval '1 hour'),('s2','b1',now()+interval '1 hour')",
		"INSERT INTO pending_logins(token_hash,user_id,expires) VALUES('p1','a1',now()+interval '10 minutes')",
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	const next = "operator-chosen-password"
	var out, prompt bytes.Buffer
	if err = resetAdminPassword(context.Background(), "plain_buyer", false, nil, pipe(t, next+"\n"), &out, &prompt); err == nil || !strings.Contains(err.Error(), "not an administrator") || out.Len() != 0 {
		t.Fatalf("non-admin: %v %q", err, out.String())
	}
	if err = resetAdminPassword(context.Background(), "root_admin", false, nil, pipe(t, next+"\n"), &out, &prompt); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "Password reset for administrator root_admin. Its sessions and pending sign-ins have ended; sign in with the new password.\n" {
		t.Fatalf("output %q", got)
	}
	if prompt.Len() != 0 {
		t.Fatalf("piped input prompted: %q", prompt.String())
	}
	var hash, buyerHash string
	var sessions, pending, buyerSessions, audits int
	if err = db.QueryRow(`SELECT (SELECT password_hash FROM users WHERE id='a1'),(SELECT password_hash FROM users WHERE id='b1'),
		(SELECT count(*) FROM sessions WHERE user_id='a1'),(SELECT count(*) FROM pending_logins WHERE user_id='a1'),
		(SELECT count(*) FROM sessions WHERE user_id='b1'),
		(SELECT count(*) FROM audit_events WHERE user_id='a1' AND action='Password reset by the operator on the host; ended 1 session(s) and any pending sign-ins')`).Scan(&hash, &buyerHash, &sessions, &pending, &buyerSessions, &audits); err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(next)) != nil {
		t.Fatal("new password does not match the stored hash")
	}
	if cost, _ := bcrypt.Cost([]byte(hash)); cost != 12 {
		t.Fatalf("bcrypt cost %d, want the application's 12", cost)
	}
	if sessions != 0 || pending != 0 || audits != 1 || buyerSessions != 1 || buyerHash != string(old) {
		t.Fatalf("sessions=%d pending=%d audits=%d buyerSessions=%d buyerChanged=%v", sessions, pending, audits, buyerSessions, buyerHash != string(old))
	}
}
