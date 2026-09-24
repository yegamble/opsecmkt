package market

import (
	"html"
	"strings"
	"testing"
)

// The admin role and second-factor reset forms take a handle and look it up on the server, so every account
// can be reached however many accounts were created after it (A-26, A-71).

// adminForm returns the first form on page posting to action (and, when value is set, whose hidden "action"
// field has that value), up to its closing tag.
func adminForm(t *testing.T, page, action, value string) string {
	t.Helper()
	for _, f := range strings.Split(page, "<form ")[1:] {
		f, _, _ = strings.Cut(f, "</form>")
		if strings.Contains(f, `action="`+action+`"`) && (value == "" || strings.Contains(f, `name="action" value="`+value+`"`)) {
			return f
		}
	}
	t.Fatalf("form %s %s missing", action, value)
	return ""
}

func TestAdminChangesRoleOfAnyAccountByHandle(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("lookup_admin", "admin")
	vendorID, _ := e.user("lookup_old_vendor", "vendor")
	e.user("lookup_admin2", "admin")
	product := e.product(vendorID, "physical")
	agExec(e, "UPDATE users SET created=now()-interval '1 day' WHERE id=$1", vendorID)
	// 101 accounts registered after the vendor push it out of the page's list of newest accounts.
	agExec(e, `INSERT INTO users(id,handle,password_hash,role,created)
		SELECT 'newer-'||g, 'newer_buyer_'||g, 'x', 'buyer', now()+g*interval '1 second' FROM generate_series(1,101) g`)

	page := e.body("GET", "/admin", admin, nil, 200)
	f := adminForm(t, page, "/admin", "role")
	mustContain(t, f, `name="handle"`, `pattern="[a-zA-Z0-9_]{3,32}"`, "Account handle")
	mustNotContain(t, f, `name="user_id"`, "<option value=\"\">Select an account")
	mustContain(t, page, "newer_buyer_101 · buyer", "the 100 newest accounts")
	mustNotContain(t, page, "lookup_old_vendor ·")

	// The vendor is demoted by handle; its listing is archived and both accounts are audited as before.
	agExpect(e, "/admin", admin, form("action", "role", "role", "buyer", "handle", " lookup_old_vendor "), 303, "")
	if agStr(e, "SELECT role FROM users WHERE id=$1", vendorID) != "buyer" || agInt(e, "SELECT count(*) FROM products WHERE id=$1 AND archived", product) != 1 {
		t.Fatal("oldest vendor not demoted by handle or listing not archived")
	}
	if !e.auditExact(adminID, "Changed role of lookup_old_vendor from vendor to buyer; archived 1 active listing(s)") ||
		!e.auditExact(vendorID, "Role changed from vendor to buyer by an administrator") ||
		!e.auditExact(vendorID, "Archived 1 active listing(s): role changed to buyer by an administrator") {
		t.Fatal("role change audit rows changed")
	}

	before := agInt(e, "SELECT count(*) FROM audit_events")
	// An unknown handle answers 404 with the admin page: the error, and the form keeping what was typed.
	for _, typed := range []string{"no_such_account", "Lookup_Old_Vendor", "bad handle!", "lookup_admin2"} {
		w := e.do("POST", "/admin", admin, form("action", "role", "role", "moderator", "handle", typed))
		if w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("unknown handle %q: status %d (%s)", typed, w.Code, w.Header().Get("Content-Type"))
		}
		body := w.Body.String()
		mustContain(t, body, `<div class="alert error" role="alert">No buyer, vendor or moderator has the handle`, "Control room")
		rf := adminForm(t, body, "/admin", "role")
		mustContain(t, rf, `value="`+html.EscapeString(typed)+`"`, `<option value="moderator" selected>`)
		mustNotContain(t, adminForm(t, body, "/admin/reset-factors", ""), `value="`+html.EscapeString(typed)+`"`)
	}
	// Administrator is never assignable, and the administrator's own role stays fixed.
	agExpect(e, "/admin", admin, form("action", "role", "role", "admin", "handle", "lookup_old_vendor"), 400, "Choose buyer, vendor, or moderator")
	agExpect(e, "/admin", admin, form("action", "role", "role", "buyer", "handle", "lookup_admin"), 400, "Cannot change your own administrator role")
	agExpect(e, "/admin", admin, form("action", "role", "role", "buyer", "handle", ""), 400, "Enter an account handle")
	if agInt(e, "SELECT count(*) FROM audit_events") != before || agInt(e, "SELECT count(*) FROM users WHERE role='admin'") != 2 || agStr(e, "SELECT role FROM users WHERE id=$1", vendorID) != "buyer" {
		t.Fatal("refused role change altered state")
	}
}

func TestAdminResetsFactorsOfAnyAccountByHandle(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("aaa_reset_admin", "admin")
	agExec(e, `INSERT INTO users(id,handle,password_hash,role,totp_enabled)
		SELECT 'bulk-'||g, 'm_user_'||lpad(g::text,4,'0'), 'x', 'buyer', true FROM generate_series(1,1000) g`)
	lockedID, _ := e.user("zz_locked_out_vendor", "vendor")
	agExec(e, "UPDATE users SET totp_enabled=true WHERE id=$1", lockedID)
	e.user("zz_no_factor", "buyer")

	page := e.body("GET", "/admin", admin, nil, 200)
	f := adminForm(t, page, "/admin/reset-factors", "")
	mustContain(t, f, `name="handle"`, `pattern="[a-zA-Z0-9_]{3,32}"`, "Account handle", `name="password" required`)
	mustNotContain(t, f, `name="user_id"`)
	// The helper list is capped and says so; the account after the cap is not listed but can still be reset.
	mustContain(t, page, "m_user_0001 · TOTP", "Up to 100 accounts")
	mustNotContain(t, page, "m_user_0101 · TOTP", "zz_locked_out_vendor · TOTP")

	// An unknown handle is refused before the password is checked: 404, the page again, handle kept.
	w := e.do("POST", "/admin/reset-factors", admin, form("handle", "zz_nobody", "password", testPassword))
	if w.Code != 404 {
		t.Fatalf("unknown handle: status %d", w.Code)
	}
	body := w.Body.String()
	mustContain(t, body, `<div class="alert error" role="alert">No account has the handle`, "Control room")
	mustContain(t, adminForm(t, body, "/admin/reset-factors", ""), `value="zz_nobody"`)
	mustNotContain(t, adminForm(t, body, "/admin", "role"), `value="zz_nobody"`)
	agExpect(e, "/admin/reset-factors", admin, form("handle", "zz_no_factor", "password", testPassword), 409, "no second factor enrolled")
	agExpect(e, "/admin/reset-factors", admin, form("handle", "aaa_reset_admin", "password", testPassword), 400, "cannot reset your own second factors")

	agExpect(e, "/admin/reset-factors", admin, form("handle", "zz_locked_out_vendor", "password", testPassword), 303, "")
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND NOT totp_enabled", lockedID) != 1 {
		t.Fatal("account after the list cap not reset by handle")
	}
	if !e.auditExact(adminID, "Reset second factors of zz_locked_out_vendor: TOTP and recovery codes turned off; ended 1 session(s) (confirmed with password)") ||
		!e.auditExact(lockedID, "Second factors reset by administrator aaa_reset_admin: TOTP and recovery codes turned off; ended 1 session(s)") {
		t.Fatal("reset audit rows changed")
	}
}
