package market

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"
)

// ResetAdminPassword is the operator's host-only break-glass (cmd/server -reset-admin-password): it sets a new
// password for an administrator, ends that account's sessions and pending sign-ins and audits the reset.
func TestResetAdminPassword(t *testing.T) {
	e := newTestApp(t)
	ctx := context.Background()
	id, current := e.user("root_admin", "admin")
	other := agSession(e, id)
	pending := agPending(e, id)
	otherAdminID, otherAdmin := e.user("second_admin", "admin")
	buyerID, buyer := e.user("plain_buyer", "buyer")
	const next = "operator-chosen-password"
	oldHash, buyerHash := passwordHash(e, id), passwordHash(e, buyerID)
	unchanged := func(when string) {
		t.Helper()
		if passwordHash(e, id) != oldHash || agUserSessions(e, id) != 2 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", id) != 1 {
			t.Fatalf("%s: refused reset altered the administrator", when)
		}
		if passwordHash(e, buyerID) != buyerHash || agUserSessions(e, buyerID) != 1 {
			t.Fatalf("%s: refused reset altered the buyer", when)
		}
		if agInt(e, "SELECT count(*) FROM audit_events WHERE action LIKE 'Password reset by the operator%'") != 0 {
			t.Fatalf("%s: refused reset was audited", when)
		}
	}

	for _, tc := range []struct{ handle, password, msg string }{
		{"root_admin", "short-pass1", "12–72 bytes"},
		{"root_admin", strings.Repeat("x", 73), "12–72 bytes"},
		{"root_admin", "", "12–72 bytes"},
		{"plain_buyer", next, "not an administrator"},
		{"nobody_here", next, "no account"},
		{"ROOT_ADMIN", next, "no account"}, // handles are exact and case-sensitive
	} {
		err := ResetAdminPassword(ctx, e.DB, tc.handle, tc.password)
		if err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Fatalf("ResetAdminPassword(%q, %d bytes) = %v, want %q", tc.handle, len(tc.password), err, tc.msg)
		}
		if strings.Contains(err.Error(), tc.password) && tc.password != "" {
			t.Fatalf("error repeats the password: %v", err)
		}
		unchanged(tc.handle)
	}

	// A database upgraded by a newer release is refused, like server startup, before anything changes.
	agExec(e, "INSERT INTO schema_migrations(version,name) VALUES(999,'from_the_future')")
	if err := ResetAdminPassword(ctx, e.DB, "root_admin", next); err == nil || !strings.Contains(err.Error(), "999_from_the_future") {
		t.Fatalf("unknown migration not refused: %v", err)
	}
	unchanged("unknown migration")
	agExec(e, "DELETE FROM schema_migrations WHERE version=999")

	if err := ResetAdminPassword(ctx, e.DB, "root_admin", next); err != nil {
		t.Fatal(err)
	}
	if passwordHash(e, id) == oldHash {
		t.Fatal("password hash unchanged")
	}
	if agUserSessions(e, id) != 0 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", id) != 0 {
		t.Fatal("the administrator's sessions or pending sign-ins survived the reset")
	}
	for _, s := range []string{current, other} {
		if loc := e.do("GET", "/admin", s, nil).Header().Get("Location"); loc != "/login" {
			t.Fatalf("old session still authenticates (location %q)", loc)
		}
	}
	agExpect(e, "/challenge", randomToken(), url.Values{"method": {"totp"}, "code": {"000000"}}, 401, "expired", pending)
	if !e.auditExact(id, "Password reset by the operator on the host; ended 2 session(s) and any pending sign-ins") {
		t.Fatal("reset not audited")
	}
	// Other accounts keep their sessions and passwords.
	e.check(e.do("GET", "/admin", otherAdmin, nil), 200)
	e.check(e.do("GET", "/account", buyer, nil), 200)
	if agUserSessions(e, otherAdminID) != 1 || passwordHash(e, buyerID) != buyerHash {
		t.Fatal("another account was changed")
	}
	if code, _ := loginStatus(e, "root_admin", testPassword); code != 401 {
		t.Fatalf("old password still signs in: %d", code)
	}
	if code, loc := loginStatus(e, "root_admin", next); code != 303 || loc != "/" {
		t.Fatalf("new password sign-in: %d %q", code, loc)
	}
}

// A database the server has never set up is refused with a clear message instead of a relation error.
func TestResetAdminPasswordNeedsInstalledSchema(t *testing.T) {
	db, err := sql.Open("pgx", testSchemaDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = ResetAdminPassword(context.Background(), db, "root_admin", "operator-chosen-password")
	if err == nil || !strings.Contains(err.Error(), "start the server") {
		t.Fatalf("empty database: %v", err)
	}
}
