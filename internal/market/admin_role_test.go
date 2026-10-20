package market

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// A restore may predate the replacement administrator whose predecessor is
// erased by the current manifest. Recovery creates a fresh identity; host role
// assignment cannot revive closed or already-closing accounts.
func TestRetentionAdministratorRecoveryUsesFreshAccount(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	e.setup()
	agExec(e, `UPDATE users SET closed_at=now(),password_hash='',handle='erased_admin',pgp='' WHERE role='admin'`)
	agExec(e, `DELETE FROM sessions`)
	if _, err := SetRole(t.Context(), e.DB, "erased_admin", "buyer"); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed administrator revived: %v", err)
	}
	if err := ResetAdminPassword(t.Context(), e.DB, "erased_admin", testPassword); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("erased administrator credentials recreated: %v", err)
	}
	w := e.do("POST", "/register", randomToken(), url.Values{"handle": {"recovery_operator"}, "password": {testPassword}})
	e.check(w, 303)
	if _, err := SetRole(t.Context(), e.DB, "recovery_operator", "admin"); err != nil {
		t.Fatal(err)
	}
	if code, _ := loginStatus(e, "recovery_operator", testPassword); code != 303 {
		t.Fatalf("replacement administrator cannot sign in: %d", code)
	}
	for _, column := range []string{"deletion_requested_at", "retention_restricted_at"} {
		id, _ := e.user("closing_"+column[:8], "buyer")
		agExec(e, "UPDATE users SET "+column+"=now() WHERE id=$1", id)
		handle := agStr(e, `SELECT handle FROM users WHERE id=$1`, id)
		if _, err := SetRole(t.Context(), e.DB, handle, "admin"); err == nil || !strings.Contains(err.Error(), "awaiting account closure") {
			t.Fatalf("closing account became replacement administrator: %v", err)
		}
	}
}

// SetRole is the operator's host command for role changes (cmd/server -set-role, A-156): the only way to add or
// remove an administrator. It refuses the last administrator, ends a removed administrator's sessions and pending
// sign-ins, archives the listings of an account that is no longer a vendor and audits the change.
func TestSetRole(t *testing.T) {
	e := newTestApp(t)
	ctx := context.Background()
	rootID, root := e.user("root_admin", "admin")
	rootOther := agSession(e, rootID)
	rootPending := agPending(e, rootID)
	rootAvatar := e.image(rootID, "", "avatar")
	secondID, second := e.user("second_admin", "admin")
	vendorID, vendor := e.user("role_vendor", "vendor")
	e.product(vendorID, "physical")
	e.product(vendorID, "digital")
	archived := e.product(vendorID, "physical")
	agExec(e, "UPDATE products SET archived=true,archived_at=now() WHERE id=$1", archived)
	buyerID, buyer := e.user("role_buyer", "buyer")
	heldID, _ := e.user("held_buyer", "buyer")
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", heldID)

	roles := func() string {
		return agStr(e, "SELECT string_agg(handle||'='||role, ',' ORDER BY handle) FROM users")
	}
	audits := func() int { return agInt(e, "SELECT count(*) FROM audit_events") }
	before, auditsBefore := roles(), audits()
	unchanged := func(when string) {
		t.Helper()
		if got := roles(); got != before {
			t.Fatalf("%s: roles changed: %s, want %s", when, got, before)
		}
		if audits() != auditsBefore || agUserSessions(e, rootID) != 2 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", rootID) != 1 {
			t.Fatalf("%s: a refused change wrote an audit row or ended sessions", when)
		}
		if agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1 AND NOT archived", vendorID) != 2 {
			t.Fatalf("%s: a refused change archived listings", when)
		}
	}
	for _, tc := range []struct{ handle, role, msg string }{
		{"root_admin", "superuser", "buyer, vendor, moderator or admin"},
		{"root_admin", "Buyer", "buyer, vendor, moderator or admin"},
		{"root_admin", "", "buyer, vendor, moderator or admin"},
		{"nobody_here", "buyer", "no account has the handle"},
		{"ROOT_ADMIN", "buyer", "no account has the handle"}, // handles are exact and case-sensitive
		{"role_vendor", "vendor", "already a vendor"},
		{"held_buyer", "admin", "suspended"},
	} {
		if _, err := SetRole(ctx, e.DB, tc.handle, tc.role); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Fatalf("SetRole(%q, %q) = %v, want %q", tc.handle, tc.role, err, tc.msg)
		}
		unchanged(tc.handle + " " + tc.role)
	}
	// A database upgraded by a newer release is refused, like server startup, before anything changes.
	agExec(e, "INSERT INTO schema_migrations(version,name) VALUES(999,'from_the_future')")
	if _, err := SetRole(ctx, e.DB, "root_admin", "buyer"); err == nil || !strings.Contains(err.Error(), "999_from_the_future") {
		t.Fatalf("unknown migration not refused: %v", err)
	}
	unchanged("unknown migration")
	agExec(e, "DELETE FROM schema_migrations WHERE version=999")

	// Leaving vendor archives the active listings, as the admin form does; sessions stay.
	got, err := SetRole(ctx, e.DB, "role_vendor", "buyer")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Role of role_vendor changed from vendor to buyer; archived 2 active listing(s)" {
		t.Fatalf("summary %q", got)
	}
	if agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1 AND NOT archived", vendorID) != 0 {
		t.Fatal("listings of a former vendor stay active")
	}
	if want := []string{"Role changed from vendor to buyer by host command", "Archived 2 active listing(s): role changed to buyer by host command"}; !slices.Equal(e.audits(vendorID), want) {
		t.Fatalf("vendor audit %q, want %q", e.audits(vendorID), want)
	}
	e.check(e.do("GET", "/account", vendor, nil), 200)

	// Adding an administrator: the account's existing session reaches /admin at once.
	if got, err = SetRole(ctx, e.DB, "role_buyer", "admin"); err != nil || got != "Role of role_buyer changed from buyer to admin" {
		t.Fatalf("promotion: %q %v", got, err)
	}
	if !e.auditExact(buyerID, "Role changed from buyer to admin by host command") {
		t.Fatal("promotion not audited")
	}
	e.check(e.do("GET", "/admin", buyer, nil), 200)

	// Removing an administrator ends its sessions and pending sign-ins and, leaving staff, removes its staff avatar.
	if got, err = SetRole(ctx, e.DB, "root_admin", "buyer"); err != nil {
		t.Fatal(err)
	}
	if got != "Role of root_admin changed from admin to buyer; ended 2 session(s) and any pending sign-ins; removed their staff avatar" {
		t.Fatalf("summary %q", got)
	}
	if notice := agStr(e, "SELECT body FROM notifications WHERE user_id=$1", rootID); notice != "Your account role changed from admin to buyer. Your staff avatar was removed. Your existing sessions and pending sign-ins were ended." || agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", rootID) != 1 {
		t.Fatalf("host demotion notice %q", notice)
	}
	if agUserSessions(e, rootID) != 0 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", rootID) != 0 {
		t.Fatal("a removed administrator's sessions or pending sign-ins survived")
	}
	for _, s := range []string{root, rootOther} {
		if loc := e.do("GET", "/admin", s, nil).Header().Get("Location"); loc != "/login?next=%2Fadmin" {
			t.Fatalf("old administrator session still authenticates (location %q)", loc)
		}
	}
	agExpect(e, "/challenge", randomToken(), form("method", "totp", "code", "000000"), 401, "expired", rootPending)
	if e.imageRows(rootAvatar.id) != 0 || agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND staff_hidden", rootID) != 1 {
		t.Fatal("staff avatar or staff-list choice survived leaving staff")
	}
	if want := []string{"Role changed from admin to buyer by host command", "Staff avatar removed when the role changed"}; !slices.Equal(e.audits(rootID), want) {
		t.Fatalf("root audit %q, want %q", e.audits(rootID), want)
	}
	// It can sign in again with its password, as a buyer.
	if code, loc := loginStatus(e, "root_admin", testPassword); code != 303 || loc != "/" {
		t.Fatalf("demoted administrator sign-in: %d %q", code, loc)
	}

	// Administrator to moderator keeps staff but still ends the sessions. The last administrator cannot be removed.
	if got, err = SetRole(ctx, e.DB, "second_admin", "moderator"); err != nil || got != "Role of second_admin changed from admin to moderator; ended 1 session(s) and any pending sign-ins" {
		t.Fatalf("admin to moderator: %q %v", got, err)
	}
	if agUserSessions(e, secondID) != 0 || !signedOut(e, second) {
		t.Fatal("administrator to moderator kept its sessions")
	}
	auditsBefore = audits()
	if _, err = SetRole(ctx, e.DB, "role_buyer", "vendor"); err == nil || !strings.Contains(err.Error(), "last administrator") {
		t.Fatalf("last administrator removed: %v", err)
	}
	if agStr(e, "SELECT role FROM users WHERE id=$1", buyerID) != "admin" || audits() != auditsBefore || agUserSessions(e, buyerID) != 1 {
		t.Fatal("refusing the last administrator changed something")
	}
	// A suspended administrator (set in the database) does not count as the other administrator.
	agExec(e, "UPDATE users SET role='admin',suspended_at=now() WHERE id=$1", secondID)
	if _, err = SetRole(ctx, e.DB, "role_buyer", "buyer"); err == nil || !strings.Contains(err.Error(), "last administrator") {
		t.Fatalf("last active administrator removed beside a suspended one: %v", err)
	}
}

// Two host commands removing the last two administrators at the same time: exactly one succeeds.
func TestSetRoleConcurrentDemotionsKeepAnAdministrator(t *testing.T) {
	e := newTestApp(t)
	e.user("first_admin", "admin")
	e.user("other_admin", "admin")
	holder, err := e.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err = holder.ExecContext(context.Background(), "SELECT pg_advisory_lock(782493)"); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, h := range []string{"first_admin", "other_admin"} {
		go func() { _, err := SetRole(context.Background(), e.DB, h, "buyer"); errs <- err }()
	}
	// Both wait on the lock before either runs.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if agInt(e, "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=782493 AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())") >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the host commands never waited on the lock")
		}
	}
	if _, err = holder.ExecContext(context.Background(), "SELECT pg_advisory_unlock(782493)"); err != nil {
		t.Fatal(err)
	}
	var ok, refused int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			ok++
		case strings.Contains(err.Error(), "last administrator"):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || refused != 1 || agInt(e, "SELECT count(*) FROM users WHERE role='admin'") != 1 {
		t.Fatalf("ok=%d refused=%d admins=%d", ok, refused, agInt(e, "SELECT count(*) FROM users WHERE role='admin'"))
	}
}

// A database the server has never set up is refused with a clear message instead of a relation error.
func TestSetRoleNeedsInstalledSchema(t *testing.T) {
	db, err := sql.Open("pgx", testSchemaDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = SetRole(context.Background(), db, "root_admin", "buyer"); err == nil || !strings.Contains(err.Error(), "start the server") {
		t.Fatalf("empty database: %v", err)
	}
}

// besideAccountChange runs act while another transaction holds userID's row and then applies change to it (a
// demotion or suspension committed after act's session was loaded), and returns act's response.
func besideAccountChange(t *testing.T, e *testEnv, userID string, change []string, act func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	ctx := t.Context()
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT 1 FROM users WHERE id=$1 FOR UPDATE", userID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- act() }()
	var w *httptest.ResponseRecorder
	for deadline := time.Now().Add(5 * time.Second); w == nil && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		select {
		case w = <-done:
			continue
		default:
		}
		// Waiting on this row: other test processes may be waiting on locks in the same database.
		var waiting int
		e.DB.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '% FROM users WHERE id=$1 FOR NO KEY UPDATE%'`).Scan(&waiting)
		if waiting > 0 {
			break
		}
	}
	if w != nil {
		t.Fatalf("the action finished without waiting on the account row: %d %s", w.Code, w.Body.String())
	}
	for _, q := range change {
		if _, err = tx.ExecContext(ctx, q, userID); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case w = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the action never finished")
	}
	return w
}

// A confirmed action authenticated before its caller was demoted or suspended does not complete after it (A-156):
// confirmedTx re-reads the caller's role and suspension from the row it locks.
func TestConfirmedActionRefusedAfterCallerDemotedOrSuspended(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("racing_admin", "admin")
	e.user("spare_admin", "admin")
	targetID, _ := e.user("racing_target", "buyer")
	// The host command's removal of an administrator: role, then sessions (the request already loaded its session).
	w := besideAccountChange(t, e, adminID, []string{"UPDATE users SET role='buyer' WHERE id=$1", "DELETE FROM sessions WHERE user_id=$1"}, func() *httptest.ResponseRecorder {
		return e.do("POST", "/admin/suspend", admin, suspendForm("suspend", "racing_target"))
	})
	if w.Code != 403 || !strings.Contains(w.Body.String(), "nothing was changed") {
		t.Fatalf("suspension by an administrator demoted meanwhile: %d %q", w.Code, w.Body.String())
	}
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND suspended_at IS NOT NULL", targetID) != 0 || agUserSessions(e, targetID) != 1 ||
		len(e.audits(targetID)) != 0 || len(e.audits(adminID)) != 0 || len(e.notes(targetID)) != 0 {
		t.Fatal("a refused suspension changed or recorded something")
	}

	// Without a password confirmation too: a profile save by an account suspended meanwhile.
	buyerID, buyer := e.keylessUser("racing_buyer", "buyer")
	w = besideAccountChange(t, e, buyerID, []string{"UPDATE users SET suspended_at=now() WHERE id=$1", "DELETE FROM sessions WHERE user_id=$1"}, func() *httptest.ResponseRecorder {
		return e.do("POST", "/account", buyer, form("pgp", "", "xmpp", "racer@example.test"))
	})
	if w.Code != 403 || agStr(e, "SELECT xmpp FROM users WHERE id=$1", buyerID) != "" || len(e.audits(buyerID)) != 0 {
		t.Fatalf("profile save by an account suspended meanwhile: %d %q", w.Code, w.Body.String())
	}
}
