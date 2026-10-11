package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestAdminAuditFilterIsExactBoundedAndAccountScoped(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("audit_admin", "admin")
	aliceID, _ := e.user("AuditAlice", "buyer")
	lowerID, _ := e.user("auditalice", "vendor")
	otherID, _ := e.user("audit_other", "buyer")

	// More than the display cap for the exact handle, plus events for a case-variant and an unrelated account.
	agExec(e, `INSERT INTO audit_events(user_id,action,created)
		SELECT $1, 'alice-event-'||lpad(g::text,3,'0'), now()+g*interval '1 second' FROM generate_series(1,105) g`, aliceID)
	agExec(e, `INSERT INTO audit_events(user_id,action) VALUES($1,'case-variant-only'),($2,'other-account-only'),($3,'admin-own-event')`, lowerID, otherID, adminID)

	path := "/admin?audit_handle=" + url.QueryEscape("AuditAlice")
	page := e.body("GET", path, admin, nil, 200)
	if strings.Count(page, "alice-event-") != 100 || !strings.Contains(page, "alice-event-105") || !strings.Contains(page, "alice-event-006") || strings.Contains(page, "alice-event-005") {
		t.Fatalf("audit filter did not show the latest 100 matching rows: count=%d", strings.Count(page, "alice-event-"))
	}
	mustContain(t, page, "AuditAlice", `name="audit_handle"`, `value="AuditAlice"`, "latest 100 matching")
	mustNotContain(t, page, "case-variant-only", "other-account-only", "admin-own-event")

	// Equality against handle is case-sensitive: the lowercase account has its own event; another spelling is empty.
	lowercase := e.body("GET", "/admin?audit_handle="+url.QueryEscape("auditalice"), admin, nil, 200)
	mustContain(t, lowercase, `value="auditalice"`, "case-variant-only")
	mustNotContain(t, lowercase, "alice-event-", "other-account-only", "admin-own-event")
	for _, handle := range []string{"AUDITALICE", "missing_handle"} {
		filtered := e.body("GET", "/admin?audit_handle="+url.QueryEscape(handle), admin, nil, 200)
		mustContain(t, filtered, `value="`+handle+`"`, "No audit events recorded.")
		mustNotContain(t, filtered, "alice-event-", "case-variant-only", "other-account-only")
	}

	// Reject invalid filters rather than truncating into a different, valid account.
	for _, handle := range []string{"aa", strings.Repeat("a", 33), "AuditAlice ", "AuditAlice<script>"} {
		invalid := e.body("GET", "/admin?audit_handle="+url.QueryEscape(handle), admin, nil, 400)
		mustContain(t, invalid, "Enter an exact account handle")
		mustNotContain(t, invalid, "alice-event-", "case-variant-only", "other-account-only")
	}

	// Account history remains limited to the signed-in viewer even if the admin-only query value is supplied.
	_, buyer := e.user("audit_viewer", "buyer")
	viewerID := agStr(e, "SELECT id FROM users WHERE handle='audit_viewer'")
	agExec(e, "INSERT INTO audit_events(user_id,action) VALUES($1,'viewer-own-event')", viewerID)
	account := e.body("GET", "/account?audit_handle="+url.QueryEscape("AuditAlice"), buyer, nil, 200)
	mustContain(t, account, "viewer-own-event")
	mustNotContain(t, account, "alice-event-", "case-variant-only", "other-account-only")

}
