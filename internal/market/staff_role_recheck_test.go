package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A-222: these staff forms may already have passed the session and
// route-role checks when a demotion or suspension commits. None may then run or
// record an audit row; the caller's current row must be locked in the action tx.
func TestStaffPostsRefuseCallerChangedAfterAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, role, path string
		form             url.Values
		change           []string
		unchanged        string
	}{
		{
			name: "admin settings after demotion", role: "admin", path: "/admin",
			form:      url.Values{"action": {"settings"}, "site_name": {"Racy market"}},
			change:    []string{"UPDATE users SET role='buyer' WHERE id=$1", "DELETE FROM sessions WHERE user_id=$1"},
			unchanged: "SELECT count(*) FROM settings WHERE key='site_name' AND value='Racy market'",
		},
		{
			name: "admin captcha after suspension", role: "admin", path: "/admin/captcha",
			form:      url.Values{"captcha_required": {"false"}, "password": {testPassword}},
			change:    []string{"UPDATE users SET suspended_at=now() WHERE id=$1", "DELETE FROM sessions WHERE user_id=$1"},
			unchanged: "SELECT count(*) FROM settings WHERE key='captcha_required' AND value='false'",
		},
		{
			name: "moderator staff list after suspension", role: "moderator", path: "/account/staff-list",
			form:      url.Values{},
			change:    []string{"UPDATE users SET suspended_at=now() WHERE id=$1", "DELETE FROM sessions WHERE user_id=$1"},
			unchanged: "SELECT count(*) FROM users WHERE handle='staff_recheck_actor' AND staff_hidden=false",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestApp(t)
			id, session := e.user("staff_recheck_actor", tc.role)
			if tc.role == "admin" {
				e.user("staff_recheck_spare", "admin")
			}
			if tc.path == "/admin/captcha" {
				// newTestApp disables CAPTCHA for fixtures; start with it on.
				agExec(e, "UPDATE settings SET value='true' WHERE key='captcha_required'")
			}
			w := besideStaffAccountChange(t, e, id, tc.change, func() *httptest.ResponseRecorder {
				return e.do("POST", tc.path, session, tc.form)
			})
			if w.Code != 403 || !strings.Contains(w.Body.String(), "nothing was changed") {
				t.Fatalf("stale staff form: %d %q", w.Code, w.Body.String())
			}
			if n := agInt(e, tc.unchanged); n != 0 || len(e.audits(id)) != 0 {
				t.Fatalf("stale staff form changed state (%d rows, %d audits)", n, len(e.audits(id)))
			}
		})
	}
}

// A-223(a): /totp/disable takes its own transaction after checking the
// password. A suspension committed while it waits on the account row must
// leave TOTP and its recovery code intact and produce no audit or notice.
func TestTOTPDisableRefusesSuspensionAfterAdmission(t *testing.T) {
	e := newTestApp(t)
	id, session := e.user("totp_recheck_actor", "buyer")
	_, recovery := agEnableTOTP(e, id)
	w := besideStaffAccountChange(t, e, id,
		[]string{"UPDATE users SET suspended_at=now() WHERE id=$1", "DELETE FROM sessions WHERE user_id=$1"},
		func() *httptest.ResponseRecorder {
			return e.do("POST", "/totp/disable", session, url.Values{"password": {testPassword}, "code": {recovery}})
		})
	if w.Code != 403 || !strings.Contains(w.Body.String(), "nothing was changed") {
		t.Fatalf("stale TOTP disable: %d %q", w.Code, w.Body.String())
	}
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND totp_enabled", id) != 1 ||
		agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND used_at IS NULL", id) != 1 ||
		len(e.audits(id)) != 0 || len(e.notes(id)) != 0 {
		t.Fatal("refused TOTP disable changed the factor, recovery code, audit, or notification")
	}
}

// The existing besideAccountChange waits specifically for the confirmedTx
// account-row SELECT. A password-less staff POST can instead be waiting on the
// audit row's foreign-key lock before this fix; commit the demotion as soon as
// either wait is visible, well before the database's lock timeout.
func besideStaffAccountChange(t *testing.T, e *testEnv, userID string, change []string, act func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
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
	waiting := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		select {
		case w := <-done:
			t.Fatalf("action finished before account-row wait: %d %s", w.Code, w.Body.String())
		default:
		}
		if err = e.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
			AND pid<>pg_backend_pid() AND wait_event_type='Lock'
			AND (query LIKE '%FROM users WHERE id=$1 FOR NO KEY UPDATE%'
			     OR query LIKE 'INSERT INTO audit_events%'))`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
	}
	if !waiting {
		t.Fatal("action did not wait on the account row")
	}
	for _, q := range change {
		if _, err = tx.ExecContext(ctx, q, userID); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-done:
		return w
	case <-time.After(15 * time.Second):
		t.Fatal("action did not finish after the account changed")
		return nil
	}
}
