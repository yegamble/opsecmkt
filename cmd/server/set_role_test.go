package main

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
)

// Invocation mistakes are refused before the database is touched.
func TestSetRoleRefusesBadInvocation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	for _, tc := range []struct {
		handle string
		flags  []string
		args   []string
		dsn    bool
		msg    string
	}{
		{handle: "root_admin", flags: []string{"set-role", "preview"}, args: []string{"buyer"}, dsn: true, msg: "-preview"},
		{handle: "root_admin", flags: []string{"set-role", "reset-admin-password"}, args: []string{"buyer"}, dsn: true, msg: "-reset-admin-password"},
		{handle: "root_admin", flags: []string{"set-role", "bootstrap-admin"}, args: []string{"buyer"}, dsn: true, msg: "-bootstrap-admin"},
		{handle: "root_admin", flags: []string{"set-role", "site-name"}, args: []string{"buyer"}, dsn: true, msg: "-site-name"},
		{handle: "", flags: []string{"set-role"}, args: []string{"buyer"}, dsn: true, msg: "-set-role HANDLE ROLE"},
		{handle: "root_admin", flags: []string{"set-role"}, dsn: true, msg: "-set-role HANDLE ROLE"},
		{handle: "root_admin", flags: []string{"set-role"}, args: []string{"buyer", "extra"}, dsn: true, msg: "-set-role HANDLE ROLE"},
		{handle: "root_admin", flags: []string{"set-role"}, args: []string{"root"}, dsn: true, msg: "buyer, vendor, moderator or admin"},
		{handle: "root_admin", flags: []string{"set-role"}, args: []string{"buyer"}, msg: "DATABASE_URL required"},
	} {
		if !tc.dsn {
			t.Setenv("DATABASE_URL", "")
		}
		var out bytes.Buffer
		err := setRole(context.Background(), tc.handle, given(tc.flags...), tc.args, &out)
		if err == nil || !strings.Contains(err.Error(), tc.msg) || out.Len() != 0 {
			t.Fatalf("%+v: err %v out %q", tc, err, out.String())
		}
	}
}

// The server binary's -set-role HANDLE ROLE against a real database: exit status, output and the change made.
func TestSetRoleCLI(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"INSERT INTO users(id,handle,password_hash,role) VALUES('a1','root_admin','x','admin'),('a2','spare_admin','x','admin'),('v1','some_vendor','x','vendor')",
		"UPDATE users SET pgp_2fa=true WHERE id IN ('a2','v1')", // A-706: staff appointments need a second factor
		"INSERT INTO sessions(token_hash,user_id,expires) VALUES('s1','a1',now()+interval '1 hour'),('s2','v1',now()+interval '1 hour')",
		"INSERT INTO pending_logins(token_hash,user_id,expires) VALUES('p1','a1',now()+interval '10 minutes')",
		"INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES('l1','v1','Listing','Fixture','Hardware','Worldwide','physical',100000,500000000000,5)",
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	role := func(id string) (r string) {
		t.Helper()
		if err := db.QueryRow("SELECT role FROM users WHERE id=$1", id).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	count := func(q string) (n int) {
		t.Helper()
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	stdout, stderr, code := runServer(t, nil, "-set-role", "nobody_here", "buyer")
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "set-role: no account has the handle \"nobody_here\"") {
		t.Fatalf("unknown handle: %d %q %q", code, stdout, stderr)
	}
	stdout, stderr, code = runServer(t, nil, "-set-role", "root_admin", "superuser")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "buyer, vendor, moderator or admin") || role("a1") != "admin" {
		t.Fatalf("bad role: %d %q %q", code, stdout, stderr)
	}

	stdout, stderr, code = runServer(t, nil, "-set-role", "root_admin", "buyer")
	if code != 0 || stderr != "" || stdout != "Role of root_admin changed from admin to buyer; ended 1 session(s) and any pending sign-ins.\n" {
		t.Fatalf("demotion: %d %q %q", code, stdout, stderr)
	}
	if role("a1") != "buyer" || count("SELECT count(*) FROM sessions WHERE user_id='a1'") != 0 || count("SELECT count(*) FROM pending_logins WHERE user_id='a1'") != 0 ||
		count("SELECT count(*) FROM audit_events WHERE user_id='a1' AND action='Role changed from admin to buyer by host command'") != 1 ||
		count("SELECT count(*) FROM users WHERE id='a1' AND vendor_approved") != 0 {
		t.Fatal("demotion not applied, audited or its sessions not ended")
	}

	stdout, stderr, code = runServer(t, nil, "-set-role", "some_vendor", "admin")
	if code != 0 || stderr != "" || stdout != "Role of some_vendor changed from vendor to admin.\n" {
		t.Fatalf("promotion: %d %q %q", code, stdout, stderr)
	}
	if role("v1") != "admin" || count("SELECT count(*) FROM products WHERE vendor_id='v1' AND NOT archived") != 1 || count("SELECT count(*) FROM sessions WHERE user_id='v1'") != 1 ||
		count("SELECT count(*) FROM users WHERE id='v1' AND vendor_approved") != 1 {
		t.Fatal("promotion from vendor lost its listing, sessions or independent selling approval")
	}

	if _, _, code = runServer(t, nil, "-set-role", "spare_admin", "moderator"); code != 0 {
		t.Fatal("removing a second administrator failed")
	}
	if count("SELECT count(*) FROM users WHERE id='a2' AND vendor_approved") != 0 {
		t.Fatal("administrator-derived selling access became moderator selling approval")
	}
	stdout, stderr, code = runServer(t, nil, "-set-role", "some_vendor", "buyer")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "last administrator") || role("v1") != "admin" {
		t.Fatalf("last administrator: %d %q %q", code, stdout, stderr)
	}

	// Once another administrator exists, genuine vendor approval survives a
	// move into moderation; assigning Buyer explicitly revokes it.
	if _, _, code = runServer(t, nil, "-set-role", "spare_admin", "admin"); code != 0 {
		t.Fatal("restoring the second administrator failed")
	}
	stdout, stderr, code = runServer(t, nil, "-set-role", "some_vendor", "moderator")
	if code != 0 || stderr != "" || stdout != "Role of some_vendor changed from admin to moderator; ended 1 session(s) and any pending sign-ins.\n" ||
		role("v1") != "moderator" || count("SELECT count(*) FROM products WHERE vendor_id='v1' AND NOT archived") != 1 ||
		count("SELECT count(*) FROM users WHERE id='v1' AND vendor_approved") != 1 {
		t.Fatalf("approved moderator: %d %q %q", code, stdout, stderr)
	}
	stdout, stderr, code = runServer(t, nil, "-set-role", "some_vendor", "buyer")
	if code != 0 || stderr != "" || stdout != "Role of some_vendor changed from moderator to buyer; archived 1 active listing(s).\n" ||
		role("v1") != "buyer" || count("SELECT count(*) FROM products WHERE vendor_id='v1' AND NOT archived") != 0 ||
		count("SELECT count(*) FROM users WHERE id='v1' AND vendor_approved") != 0 {
		t.Fatalf("selling revocation: %d %q %q", code, stdout, stderr)
	}
}

// A-706 (SEC706-2): -set-role does not make an account without TOTP or PGP sign-in a moderator or administrator,
// and says why; removing staff roles stays allowed.
func TestSetRoleRefusesFactorlessStaffAppointment(t *testing.T) {
	dsn := cliSchema(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("INSERT INTO users(id,handle,password_hash,role) VALUES('k1','keeper_admin','x','admin'),('b1','plain_buyer','x','buyer'),('m1','plain_mod','x','moderator')"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"plain_buyer", "moderator"}, {"plain_buyer", "admin"}, {"plain_mod", "admin"}} {
		stdout, stderr, code := runServer(t, nil, append([]string{"-set-role"}, args...)...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "has no second factor") {
			t.Fatalf("%v: %d %q %q", args, code, stdout, stderr)
		}
	}
	var buyer, mod string
	if err = db.QueryRow("SELECT (SELECT role FROM users WHERE id='b1'),(SELECT role FROM users WHERE id='m1')").Scan(&buyer, &mod); err != nil || buyer != "buyer" || mod != "moderator" {
		t.Fatalf("roles after refusals: %q %q %v", buyer, mod, err)
	}
	if _, stderr, code := runServer(t, nil, "-set-role", "plain_mod", "buyer"); code != 0 {
		t.Fatalf("demotion of a factor-less moderator: %d %q", code, stderr)
	}
	if _, err = db.Exec("UPDATE users SET totp_enabled=true WHERE id='b1'"); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := runServer(t, nil, "-set-role", "plain_buyer", "moderator"); code != 0 {
		t.Fatalf("appointment with a factor: %d %q", code, stderr)
	}
}
