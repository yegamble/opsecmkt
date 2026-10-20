package main

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
)

// Invocation mistakes are refused before the database is touched.
func TestResetAdminFactorsRefusesBadInvocation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	for _, tc := range []struct {
		handle string
		flags  []string
		args   []string
		dsn    bool
		msg    string
	}{
		{handle: "root_admin", flags: []string{"reset-admin-factors", "preview"}, dsn: true, msg: "-preview"},
		{handle: "root_admin", flags: []string{"reset-admin-factors", "reset-admin-password"}, dsn: true, msg: "-reset-admin-password"},
		{handle: "root_admin", flags: []string{"reset-admin-factors", "bootstrap-admin"}, dsn: true, msg: "-bootstrap-admin"},
		{handle: "root_admin", flags: []string{"reset-admin-factors", "site-name"}, dsn: true, msg: "-site-name"},
		{handle: "root_admin", flags: []string{"reset-admin-factors", "set-role"}, dsn: true, msg: "-set-role"},
		{handle: "", flags: []string{"reset-admin-factors"}, dsn: true, msg: "-reset-admin-factors HANDLE"},
		{handle: "root_admin", flags: []string{"reset-admin-factors"}, args: []string{"extra"}, dsn: true, msg: "-reset-admin-factors HANDLE"},
		{handle: "root_admin", flags: []string{"reset-admin-factors"}, msg: "DATABASE_URL required"},
	} {
		if !tc.dsn {
			t.Setenv("DATABASE_URL", "")
		}
		var out bytes.Buffer
		err := resetAdminFactors(context.Background(), tc.handle, given(tc.flags...), tc.args, &out)
		if err == nil || !strings.Contains(err.Error(), tc.msg) || out.Len() != 0 {
			t.Fatalf("%+v: err %v out %q", tc, err, out.String())
		}
	}
}

// The server binary's -reset-admin-factors HANDLE against a real database: exit status, output and the change made.
// It reads nothing from stdin (the helper process inherits the test's stdin, which is never a terminal here).
func TestResetAdminFactorsCLI(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"INSERT INTO users(id,handle,password_hash,role,totp_enabled,totp_secret,pgp_2fa) VALUES('a1','root_admin','x','admin',true,'sealed-under-the-old-key',true),('a2','plain_admin','x','admin',false,'',false),('v1','some_vendor','x','vendor',true,'sealed',false)",
		"INSERT INTO recovery_codes(code_hash,user_id) VALUES('r1','a1'),('r2','v1')",
		"INSERT INTO sessions(token_hash,user_id,expires) VALUES('s1','a1',now()+interval '1 hour'),('s2','v1',now()+interval '1 hour')",
		"INSERT INTO pending_logins(token_hash,user_id,expires) VALUES('p1','a1',now()+interval '10 minutes')",
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	count := func(q string) (n int) {
		t.Helper()
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	startupAudits := count("SELECT count(*) FROM audit_events")
	untouched := func(when string) {
		t.Helper()
		if count("SELECT count(*) FROM users WHERE id='a1' AND totp_enabled AND pgp_2fa") != 1 || count("SELECT count(*) FROM recovery_codes") != 2 ||
			count("SELECT count(*) FROM sessions") != 2 || count("SELECT count(*) FROM pending_logins") != 1 || count("SELECT count(*) FROM audit_events") != startupAudits {
			t.Fatalf("%s: a refused reset changed something", when)
		}
	}

	stdout, stderr, code := runServer(t, nil, "-reset-admin-factors", "nobody_here")
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "reset-admin-factors: no account has the handle \"nobody_here\"") {
		t.Fatalf("unknown handle: %d %q %q", code, stdout, stderr)
	}
	untouched("unknown handle")
	stdout, stderr, code = runServer(t, nil, "-reset-admin-factors", "some_vendor")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "not an administrator") || !strings.Contains(stderr, "Reset a user's second factors") {
		t.Fatalf("non-admin: %d %q %q", code, stdout, stderr)
	}
	untouched("non-admin")
	stdout, stderr, code = runServer(t, nil, "-reset-admin-factors", "plain_admin")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "no second factor enrolled") {
		t.Fatalf("nothing enrolled: %d %q %q", code, stdout, stderr)
	}
	untouched("nothing enrolled")
	stdout, stderr, code = runServer(t, nil, "-reset-admin-factors", "root_admin", "-set-role", "root_admin")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "cannot be combined with -set-role") {
		t.Fatalf("combined flags: %d %q %q", code, stdout, stderr)
	}
	untouched("combined flags")
	if _, err = db.Exec("INSERT INTO schema_migrations(version,name) VALUES(999,'from_the_future')"); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = runServer(t, nil, "-reset-admin-factors", "root_admin")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "999_from_the_future") {
		t.Fatalf("newer schema: %d %q %q", code, stdout, stderr)
	}
	untouched("newer schema")
	if _, err = db.Exec("DELETE FROM schema_migrations WHERE version=999"); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code = runServer(t, nil, "-reset-admin-factors", "root_admin")
	if code != 0 || stderr != "" || stdout != "Second factors of administrator root_admin reset: TOTP and recovery codes and PGP sign-in turned off; ended 1 session(s) and any pending sign-ins. Sign in again, then set up TOTP or PGP sign-in again. Mandatory PGP authentication still applies.\n" {
		t.Fatalf("reset: %d %q %q", code, stdout, stderr)
	}
	if count("SELECT count(*) FROM users WHERE id='a1' AND NOT totp_enabled AND totp_secret='' AND NOT pgp_2fa") != 1 ||
		count("SELECT count(*) FROM recovery_codes WHERE user_id='a1'") != 0 || count("SELECT count(*) FROM sessions WHERE user_id='a1'") != 0 ||
		count("SELECT count(*) FROM pending_logins WHERE user_id='a1'") != 0 || count("SELECT count(*) FROM audit_events") != startupAudits+1 ||
		count("SELECT count(*) FROM audit_events WHERE user_id='a1' AND action='Second factors reset by host command: TOTP and recovery codes and PGP sign-in turned off; ended 1 session(s) and any pending sign-ins'") != 1 {
		t.Fatal("reset not applied or not audited")
	}
	if count("SELECT count(*) FROM users WHERE id='v1' AND totp_enabled") != 1 || count("SELECT count(*) FROM recovery_codes WHERE user_id='v1'") != 1 || count("SELECT count(*) FROM sessions WHERE user_id='v1'") != 1 {
		t.Fatal("the reset changed another account")
	}
}
