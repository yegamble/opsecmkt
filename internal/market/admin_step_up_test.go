package market

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAdminStepUpRequiresPasswordAndTOTP(t *testing.T) {
	for _, name := range []string{"role", "captcha", "staff contact", "staff list", "operator key", "canary"} {
		t.Run(name, func(t *testing.T) {
			e := newTestApp(t)
			adminID, admin := e.user("step_up_admin", "admin")
			targetID, _ := e.user("step_up_target", "buyer")
			operator, pub := canaryTestKey(t, "step_up_operator")
			signed := canarySign(t, operator, "A test-only statement.", canaryKeyTime.Add(time.Hour))
			if name == "canary" {
				agExec(e, "INSERT INTO settings(key,value) VALUES($1,$2)", settingOperatorKey, pub)
			}
			path := "/admin"
			form := url.Values{"action": {"role"}, "handle": {"step_up_target"}, "role": {"vendor"}}
			unchanged := "SELECT count(*) FROM users WHERE id=$1 AND role='vendor'"
			arg := any(targetID)
			switch name {
			case "captcha":
				path, form = "/admin/captcha", url.Values{"captcha_required": {"true"}}
				unchanged, arg = "SELECT count(*) FROM settings WHERE key='captcha_required' AND value='true'", nil
			case "staff contact":
				path, form = "/admin/staff-contact", url.Values{"staff_contact": {"security@example.test"}}
				unchanged, arg = "SELECT count(*) FROM settings WHERE key='staff_contact' AND value='security@example.test'", nil
			case "staff list":
				path, form = "/admin/staff-list", url.Values{"staff_list_public": {"true"}}
				unchanged, arg = "SELECT count(*) FROM settings WHERE key='staff_list_public' AND value='true'", nil
			case "operator key":
				path, form = "/admin/operator-key", url.Values{"operator_key": {pub}}
				unchanged, arg = "SELECT count(*) FROM settings WHERE key=$1", settingOperatorFingerprint
			case "canary":
				path, form = "/admin/canary", url.Values{"canary": {signed}}
				unchanged, arg = "SELECT count(*) FROM canary", nil
			}
			state := func() int {
				if arg == nil {
					return agInt(e, unchanged)
				}
				return agInt(e, unchanged, arg)
			}
			for _, tc := range []struct {
				password string
				status   int
				message  string
			}{{"", 400, "current password"}, {"wrong-password-123", 401, "Password incorrect"}} {
				f := url.Values{}
				for k, values := range form {
					f[k] = append([]string(nil), values...)
				}
				f.Set("password", tc.password)
				w := e.do("POST", path, admin, f)
				if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.message) || state() != 0 || len(e.audits(adminID)) != 0 {
					t.Fatalf("%s password %q: status=%d state=%d audits=%v body=%s", name, tc.password, w.Code, state(), e.audits(adminID), w.Body)
				}
			}
			key, _ := agEnableTOTP(e, adminID)
			page := e.body("GET", "/admin", admin, nil, 200)
			var renderedForm string
			if path == "/admin" {
				renderedForm = adminForm(t, page, path, "role")
			} else {
				renderedForm = adminForm(t, page, path, "")
			}
			mustContain(t, renderedForm, `name="password" required`, `name="code" required`)
			form.Set("password", testPassword)
			good := agTOTPNow(key)
			bad := "000000"
			if bad == good {
				bad = "111111"
			}
			form.Set("code", bad)
			if w := e.do("POST", path, admin, form); w.Code != 401 || state() != 0 || len(e.audits(adminID)) != 0 {
				t.Fatalf("%s wrong code: status=%d state=%d audits=%v", name, w.Code, state(), e.audits(adminID))
			}
			form.Set("code", good)
			if w := e.do("POST", path, admin, form); w.Code != 303 || state() != 1 {
				t.Fatalf("%s confirmed: status=%d state=%d body=%s", name, w.Code, state(), w.Body)
			}
			if !strings.Contains(strings.Join(e.audits(adminID), "\n"), "(confirmed with password and authenticator code)") {
				t.Fatalf("%s confirmation missing from audit: %v", name, e.audits(adminID))
			}
		})
	}
}

func TestAdminSiteNameAuditShowsOldAndNewWithoutPassword(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("step_up_settings", "admin")
	old := agStr(e, "SELECT value FROM settings WHERE key='site_name'")
	w := e.do("POST", "/admin", admin, url.Values{"action": {"settings"}, "site_name": {"New Market"}})
	if w.Code != 303 || !e.auditExact(adminID, "Saved marketplace site name from "+strconv.Quote(old)+" to \"New Market\"") {
		t.Fatalf("site settings: %d audits=%v", w.Code, e.audits(adminID))
	}
}

func TestAdminStepUpRefusesAccountChangeAfterAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		form       url.Values
		change     string
		unchanged  string
	}{
		{"role after demotion", "/admin", url.Values{"action": {"role"}, "handle": {"step_race_target"}, "role": {"vendor"}, "password": {testPassword}}, "UPDATE users SET role='buyer' WHERE id=$1", "SELECT count(*) FROM users WHERE handle='step_race_target' AND role='vendor'"},
		{"operator key after suspension", "/admin/operator-key", url.Values{"password": {testPassword}}, "UPDATE users SET suspended_at=now() WHERE id=$1", "SELECT count(*) FROM settings WHERE key=$1"},
		{"canary after demotion", "/admin/canary", url.Values{"password": {testPassword}}, "UPDATE users SET role='buyer' WHERE id=$1", "SELECT count(*) FROM canary"},
		{"staff contact after demotion", "/admin/staff-contact", url.Values{"password": {testPassword}, "staff_contact": {"raced@example.test"}}, "UPDATE users SET role='buyer' WHERE id=$1", "SELECT count(*) FROM settings WHERE key='staff_contact' AND value='raced@example.test'"},
		{"staff list after demotion", "/admin/staff-list", url.Values{"password": {testPassword}, "staff_list_public": {"true"}}, "UPDATE users SET role='buyer' WHERE id=$1", "SELECT count(*) FROM settings WHERE key='staff_list_public' AND value='true'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestApp(t)
			adminID, admin := e.user("step_race_admin", "admin")
			e.user("step_race_spare", "admin")
			e.user("step_race_target", "buyer")
			operator, pub := canaryTestKey(t, "step_race_operator")
			if tc.path == "/admin/operator-key" {
				tc.form.Set("operator_key", pub)
			}
			if tc.path == "/admin/canary" {
				agExec(e, "INSERT INTO settings(key,value) VALUES($1,$2)", settingOperatorKey, pub)
				tc.form.Set("canary", canarySign(t, operator, "Race statement.", canaryKeyTime.Add(time.Hour)))
			}
			w := besideAccountChange(t, e, adminID, []string{tc.change, "DELETE FROM sessions WHERE user_id=$1"}, func() *httptest.ResponseRecorder {
				return e.do("POST", tc.path, admin, tc.form)
			})
			if w.Code != 403 || !strings.Contains(w.Body.String(), "nothing was changed") {
				t.Fatalf("stale staff step-up: %d %s", w.Code, w.Body)
			}
			if tc.path == "/admin/operator-key" {
				if agInt(e, tc.unchanged, settingOperatorFingerprint) != 0 {
					t.Fatal("operator key changed after suspension")
				}
			} else if agInt(e, tc.unchanged) != 0 {
				t.Fatal("staff change committed after actor change")
			}
			if len(e.audits(adminID)) != 0 {
				t.Fatal("refused staff step-up wrote audit")
			}
		})
	}
}
