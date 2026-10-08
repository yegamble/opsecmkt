package market

import (
	"context"
	"strings"
	"testing"
)

func TestRoleChangeNoticeAdminHostNoopAndRollback(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("role_notice_admin", "admin")
	otherID, _ := e.user("role_notice_other", "buyer")
	targetID, _ := e.user("role_notice_vendor", "vendor")
	e.product(targetID, "physical")
	e.product(targetID, "digital")

	agExpect(e, "/admin", admin, form("action", "role", "password", testPassword, "role", "buyer", "handle", "role_notice_vendor"), 303, "")
	adminNotice := agStr(e, "SELECT body FROM notifications WHERE user_id=$1", targetID)
	wantAdminNotice := "Your account role changed from vendor to buyer. Your 2 active listing(s) were archived; existing orders remain readable."
	if adminNotice != wantAdminNotice || agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", targetID) != 1 {
		t.Fatalf("admin role change notice %q, want exactly %q", adminNotice, wantAdminNotice)
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id IN ($1,$2)", adminID, otherID) != 0 {
		t.Fatal("role change notice was sent to the actor or another account")
	}
	if !e.auditExact(targetID, "Role changed from vendor to buyer by administrator role_notice_admin") || agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1 AND NOT archived", targetID) != 0 {
		t.Fatal("role change did not preserve its account audit and listing archive")
	}
	if strings.Contains(adminNotice, testPassword) || strings.Contains(adminNotice, "private") || strings.Contains(adminNotice, "PGP") {
		t.Fatalf("role notice included private material: %q", adminNotice)
	}

	hostID, _ := e.user("role_notice_host_target", "buyer")
	if _, err := SetRole(context.Background(), e.DB, "role_notice_host_target", "vendor"); err != nil {
		t.Fatal(err)
	}
	hostNotice := agStr(e, "SELECT body FROM notifications WHERE user_id=$1", hostID)
	if want := "Your account role changed from buyer to vendor."; hostNotice != want || agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", hostID) != 1 {
		t.Fatalf("host role change notice %q, want exactly %q", hostNotice, want)
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", adminID) != 0 {
		t.Fatal("host role change notified the administrator")
	}
	// The admin form may audit a no-op for the administrator, but must not notify the target again.
	agExpect(e, "/admin", admin, form("action", "role", "password", testPassword, "role", "vendor", "handle", "role_notice_host_target"), 303, "")
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", hostID) != 1 || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Role changed from %'", hostID) != 1 {
		t.Fatal("no-op role form repeated the target notice or wrote a target role-change audit")
	}

	rollbackID, _ := e.user("role_notice_rollback", "vendor")
	rollbackProduct := e.product(rollbackID, "physical")
	if _, err := e.DB.Exec(`CREATE FUNCTION reject_role_notice() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'test-only role notice failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`CREATE TRIGGER reject_role_notice BEFORE INSERT ON notifications FOR EACH ROW
		WHEN (NEW.body LIKE 'Your account role changed%') EXECUTE FUNCTION reject_role_notice()`); err != nil {
		t.Fatal(err)
	}
	if _, err := SetRole(context.Background(), e.DB, "role_notice_rollback", "buyer"); err == nil || !strings.Contains(err.Error(), "test-only role notice failure") {
		t.Fatalf("host role change with failing notice insert returned %v", err)
	}
	if agStr(e, "SELECT role FROM users WHERE id=$1", rollbackID) != "vendor" || agInt(e, "SELECT count(*) FROM products WHERE id=$1 AND NOT archived", rollbackProduct) != 1 ||
		agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Role changed from %'", rollbackID) != 0 ||
		agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", rollbackID) != 0 {
		t.Fatal("failed role notification did not roll back the role, archive, audit and notice together")
	}
}
