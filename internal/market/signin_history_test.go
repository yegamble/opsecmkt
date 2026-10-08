package market

import (
	"fmt"
	"strings"
	"testing"
)

func TestRecentSignInsRemainAccountScopedAndIndependentOfActivity(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("signin_history", "buyer")
	other, _ := e.user("signin_outsider", "buyer")
	for n := range 6 {
		agExec(e, `INSERT INTO audit_events(user_id,action,created) VALUES($1,$2,'2026-10-01 00:00+00'::timestamptz + $3*interval '1 minute')`, uid, fmt.Sprintf("Signed in with test-method-%d", n), n)
	}
	// Preserve and include old rows without inventing a verification method.
	agExec(e, `INSERT INTO audit_events(user_id,action,created) VALUES($1,'Signed in','2026-10-01 00:06+00')`, uid)
	for range 20 {
		agExec(e, "INSERT INTO audit_events(user_id,action) VALUES($1,'Updated profile')", uid)
	}
	agExec(e, "INSERT INTO audit_events(user_id,action) VALUES($1,'Signed in with outsider-secret')", other)
	before := agInt(e, "SELECT count(*) FROM audit_events")
	body := e.body("GET", "/account", session, nil, 200)
	start, end := strings.Index(body, `id="account-sign-ins"`), strings.Index(body, `id="account-activity"`)
	if start < 0 || end <= start {
		t.Fatal("missing separate sign-in history")
	}
	history := body[start:end]
	if strings.Contains(history, "test-method-0") || strings.Contains(history, "test-method-1") || strings.Contains(history, "outsider-secret") || strings.Contains(history, "Updated profile") {
		t.Fatal("sign-in list exceeded its bound or exposed another account/activity")
	}
	previous := -1
	for _, text := range []string{"2026-10-01 00:06 UTC", "test-method-5", "test-method-4", "test-method-3", "test-method-2"} {
		at := strings.Index(history, text)
		if at <= previous {
			t.Fatalf("missing or unordered %q", text)
		}
		previous = at
	}
	if agInt(e, "SELECT count(*) FROM audit_events") != before || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Signed in'", uid) != 1 {
		t.Fatal("reading sign-in history changed the audit trail")
	}
}
