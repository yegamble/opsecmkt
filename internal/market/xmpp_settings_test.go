package market

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func xmppSettingsForm() url.Values {
	return url.Values{"xmpp_enabled": {"1"}, "xmpp_sender": {"market@example.test"}, "xmpp_host": {"xmpp.example.test"}, "xmpp_port": {"5222"}, "xmpp_tls_mode": {"starttls"}, "xmpp_password": {"disposable-xmpp-password"}, "password": {testPassword}}
}

func TestXMPPSettingsAdminOnlyAndPrivate(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("xmpp_settings_admin", "admin")
	for _, role := range []string{"buyer", "vendor", "moderator"} {
		_, session := e.user("xmpp_settings_"+role, role)
		e.check(e.do("POST", "/admin/xmpp", session, xmppSettingsForm()), 403)
	}
	f := xmppSettingsForm()
	f.Set("password", "incorrect-password")
	e.check(e.do("POST", "/admin/xmpp", admin, f), 401)
	f.Set("password", testPassword)
	e.check(e.do("POST", "/admin/xmpp", admin, f), 303)
	stored := agStr(e, "SELECT value FROM settings WHERE key='xmpp_config'")
	if strings.Contains(stored, f.Get("xmpp_password")) || !strings.Contains(stored, "password_sealed") {
		t.Fatal("XMPP sender password was not sealed at rest")
	}
	cfg, err := e.A.loadXMPPConfig(context.Background(), e.DB)
	if err != nil || cfg.Password != f.Get("xmpp_password") || !cfg.Enabled || cfg.Sender != f.Get("xmpp_sender") {
		t.Fatalf("saved sender config did not round trip: %v", err)
	}
	for _, path := range []string{"/", "/canary", "/login", "/account", "/notifications", "/admin/site"} {
		page := e.do("GET", path, admin, nil)
		e.check(page, 200)
		if strings.Contains(page.Body.String(), cfg.Password) || strings.Contains(page.Body.String(), "password_sealed") || strings.Contains(page.Body.String(), stored) {
			t.Fatalf("sender credentials exposed on %s", path)
		}
	}
	page := e.do("GET", "/admin/site", admin, nil)
	if !strings.Contains(page.Body.String(), cfg.Sender) || !strings.Contains(page.Body.String(), "Outgoing XMPP") {
		t.Fatal("saved outgoing sender missing from admin form")
	}
	f.Set("xmpp_password", "")
	f.Set("xmpp_port", "5223")
	f.Set("xmpp_tls_mode", "direct")
	e.check(e.do("POST", "/admin/xmpp", admin, f), 303)
	updated, err := e.A.loadXMPPConfig(context.Background(), e.DB)
	if err != nil || updated.Password != cfg.Password || updated.TLSMode != "direct" || updated.Port != 5223 {
		t.Fatalf("blank password failed to retain sealed credential: %v", err)
	}
	f.Del("xmpp_enabled")
	e.check(e.do("POST", "/admin/xmpp", admin, f), 303)
	updated, err = e.A.loadXMPPConfig(context.Background(), e.DB)
	if err != nil || updated.Enabled {
		t.Fatalf("administrator could not disable outgoing delivery: %v", err)
	}
}

func TestXMPPSettingsRecoverUnreadableCredentials(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("xmpp_settings_recovery", "admin")
	f := xmppSettingsForm()
	e.check(e.do("POST", "/admin/xmpp", admin, f), 303)
	var cfg storedXMPPConfig
	if err := json.Unmarshal([]byte(agStr(e, "SELECT value FROM settings WHERE key='xmpp_config'")), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.PasswordSealed = "unreadable-private-ciphertext"
	encoded, _ := json.Marshal(cfg)
	agExec(e, "UPDATE settings SET value=$1 WHERE key='xmpp_config'", string(encoded))
	if _, err := e.A.loadXMPPConfig(context.Background(), e.DB); err == nil {
		t.Fatal("unreadable sender password enabled delivery")
	}
	page := e.body("GET", "/admin/site", admin, nil, 200)
	if !strings.Contains(page, "The sender password cannot be opened") || strings.Contains(page, cfg.PasswordSealed) {
		t.Fatal("unreadable password was not safely described on administrator page")
	}
	f.Set("xmpp_password", "")
	e.check(e.do("POST", "/admin/xmpp", admin, f), 400)
	f.Set("xmpp_password", "replacement-disposable-password")
	e.check(e.do("POST", "/admin/xmpp", admin, f), 303)
	if restored, err := e.A.loadXMPPConfig(context.Background(), e.DB); err != nil || restored.Password != f.Get("xmpp_password") {
		t.Fatalf("administrator could not replace unreadable sender password: %v", err)
	}
}

func TestXMPPAccountOptInConfirmationAndPrivacy(t *testing.T) {
	e := newTestApp(t)
	uid, user := e.user("xmpp_optin_user", "buyer")
	_, other := e.user("xmpp_optin_other", "buyer")
	f := url.Values{"xmpp": {"private-destination@example.test"}, "xmpp_enabled": {"1"}}
	e.check(e.do("POST", "/account/xmpp", user, f), 400)
	if agInt(e, "SELECT xmpp_version FROM users WHERE id=$1", uid) != 0 {
		t.Fatal("unconfirmed preference changed")
	}
	f.Set("password", testPassword)
	e.check(e.do("POST", "/account/xmpp", user, f), 303)
	if agStr(e, "SELECT xmpp FROM users WHERE id=$1", uid) != f.Get("xmpp") || agInt(e, "SELECT xmpp_version FROM users WHERE id=$1", uid) != 1 || agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND xmpp_enabled", uid) != 1 {
		t.Fatal("XMPP opt-in not saved")
	}
	page := e.do("GET", "/account", user, nil)
	e.check(page, 200)
	if !strings.Contains(page.Body.String(), f.Get("xmpp")) {
		t.Fatal("private destination missing from own preferences")
	}
	for _, path := range []string{"/account", "/canary", "/notifications"} {
		page := e.do("GET", path, other, nil)
		e.check(page, 200)
		if strings.Contains(page.Body.String(), f.Get("xmpp")) {
			t.Fatalf("private destination exposed to another user on %s", path)
		}
	}
	f.Del("xmpp_enabled")
	e.check(e.do("POST", "/account/xmpp", user, f), 303)
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND xmpp_enabled", uid) != 0 || agInt(e, "SELECT xmpp_version FROM users WHERE id=$1", uid) != 2 {
		t.Fatal("opt-out failed to invalidate queued destination version")
	}
}

func TestXMPPSettingsConfirmedFormsTOTPReplayAndCSRF(t *testing.T) {
	for _, adminSetting := range []bool{false, true} {
		t.Run(map[bool]string{false: "account", true: "administrator"}[adminSetting], func(t *testing.T) {
			e := newTestApp(t)
			role, path, page := "buyer", "/account/xmpp", "/account"
			form := url.Values{"xmpp": {"private@example.test"}, "xmpp_enabled": {"1"}, "password": {testPassword}}
			if adminSetting {
				role, path, page, form = "admin", "/admin/xmpp", "/admin/site", xmppSettingsForm()
			}
			uid, token := e.user("xmpp_confirmed_"+role, role)
			key, _ := agEnableTOTP(e, uid)
			body := e.body("GET", page, token, nil, 200)
			form.Set(formNonceField, renderedFormNonce(t, body, path))
			mustContain(t, adminForm(t, body, path, ""), `name="password" required`, `name="code" required`)
			form.Set("code", "invalid")
			e.check(e.do("POST", path, token, form), 401)
			form.Set("code", agTOTPNow(key))
			request := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.AddCookie(&http.Cookie{Name: "session", Value: token})
			refused := httptest.NewRecorder()
			e.A.ServeHTTP(refused, request)
			e.check(refused, 403)
			before := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", uid)
			e.check(e.do("POST", path, token, form), 303)
			e.check(e.do("POST", path, token, form), 303)
			if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", uid) != before+1 {
				t.Fatal("exact replay wrote another preference/configuration audit")
			}
			if adminSetting {
				form.Set("xmpp_host", "different.example.test")
			} else {
				form.Set("xmpp", "different@example.test")
			}
			e.check(e.do("POST", path, token, form), 409)
		})
	}
}

func TestXMPPSettingsRefuseAdministratorDemotionAfterAdmission(t *testing.T) {
	e := newTestApp(t)
	uid, token := e.user("xmpp_race_admin", "admin")
	e.user("xmpp_spare_admin", "admin")
	w := besideAccountChange(t, e, uid, []string{"UPDATE users SET role='moderator' WHERE id=$1", "DELETE FROM sessions WHERE user_id=$1"}, func() *httptest.ResponseRecorder {
		return e.do("POST", "/admin/xmpp", token, xmppSettingsForm())
	})
	e.check(w, 403)
	if agInt(e, "SELECT count(*) FROM settings WHERE key='xmpp_config'") != 0 {
		t.Fatal("demoted administrator changed outgoing settings")
	}
}

func TestXMPPSettingsRejectMalformedInputs(t *testing.T) {
	e := newTestApp(t)
	uid, user := e.user("xmpp_invalid_user", "buyer")
	_, admin := e.user("xmpp_invalid_admin", "admin")
	for _, jid := range []string{"", "without-domain", "user@example.test/resource", "user@example.test\nother@example.test", strings.Repeat("x", 1025) + "@example.test"} {
		e.check(e.do("POST", "/account/xmpp", user, url.Values{"xmpp": {jid}, "xmpp_enabled": {"1"}, "password": {testPassword}}), 400)
	}
	if agInt(e, "SELECT xmpp_version FROM users WHERE id=$1", uid) != 0 {
		t.Fatal("malformed destination changed preferences")
	}
	for field, value := range map[string]string{"xmpp_sender": "invalid", "xmpp_host": "https://example.test/path", "xmpp_port": "65536", "xmpp_tls_mode": "plaintext", "xmpp_password": "nul\x00password", "xmpp_enabled": "yes"} {
		f := xmppSettingsForm()
		f.Set(field, value)
		e.check(e.do("POST", "/admin/xmpp", admin, f), 400)
	}
}

func TestXMPPContentsRequireSeparateConsentPolicyAndUsableKey(t *testing.T) {
	e := newTestApp(t)
	uid, user := e.user("xmpp_contents_user", "buyer")
	_, admin := e.user("xmpp_contents_admin", "admin")
	adminForm := xmppSettingsForm()
	e.check(e.do("POST", "/admin/xmpp", admin, adminForm), 303)
	form := url.Values{"xmpp": {"contents@example.test"}, "xmpp_enabled": {"1"}, "password": {testPassword}}
	e.check(e.do("POST", "/account/xmpp", user, form), 303)
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND xmpp_include_contents", uid) != 0 {
		t.Fatal("metadata opt-in also enabled message contents")
	}
	_, public := testPGPKey(t, "xmpp_contents_key")
	e.setKey(uid, public)
	form.Set("xmpp_include_contents", "1")
	e.check(e.do("POST", "/account/xmpp", user, form), 403)
	adminForm.Set("xmpp_allow_contents", "1")
	e.check(e.do("POST", "/admin/xmpp", admin, adminForm), 303)
	policy, err := readStoredXMPPConfig(context.Background(), e.DB)
	if err != nil || !policy.AllowContents || policy.ContentPolicyVersion != 1 {
		t.Fatalf("administrator content permission not separately versioned: %v", err)
	}
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND xmpp_include_contents", uid) != 0 {
		t.Fatal("administrator permission opted in an existing account")
	}
	for variant, key := range testUnusableKeys(t, "xmpp_contents_unusable") {
		e.setKey(uid, key)
		w := e.do("POST", "/account/xmpp", user, form)
		if w.Code != 400 {
			t.Fatalf("%s accepted for content forwarding: %d", variant, w.Code)
		}
	}
	e.setKey(uid, "")
	e.check(e.do("POST", "/account/xmpp", user, form), 400)
	e.setKey(uid, public)
	form.Del("xmpp_enabled")
	e.check(e.do("POST", "/account/xmpp", user, form), 400)
	form.Set("xmpp_enabled", "1")
	form.Set("password", "incorrect-password")
	e.check(e.do("POST", "/account/xmpp", user, form), 401)
	form.Set("password", testPassword)
	e.check(e.do("POST", "/account/xmpp", user, form), 303)
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND xmpp_include_contents AND xmpp_enabled", uid) != 1 || agInt(e, "SELECT xmpp_version FROM users WHERE id=$1", uid) != 2 {
		t.Fatal("content consent did not save or invalidate earlier queued preferences")
	}
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND (pgp_2fa OR pgp_verified_at IS NOT NULL)", uid) != 0 {
		t.Fatal("content consent unexpectedly required or enabled PGP sign-in or ownership proof")
	}
	form.Del("xmpp_include_contents")
	e.check(e.do("POST", "/account/xmpp", user, form), 303)
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND xmpp_include_contents", uid) != 0 || agInt(e, "SELECT xmpp_version FROM users WHERE id=$1", uid) != 3 {
		t.Fatal("content opt-out failed to invalidate queued consent")
	}
	adminForm.Del("xmpp_allow_contents")
	e.check(e.do("POST", "/admin/xmpp", admin, adminForm), 303)
	policy, err = readStoredXMPPConfig(context.Background(), e.DB)
	if err != nil || policy.AllowContents || policy.ContentPolicyVersion != 2 {
		t.Fatalf("revoked content permission did not advance policy version: %v", err)
	}
}

func TestXMPPContentsPreferencesExposeOnlyPrivateEligibility(t *testing.T) {
	e := newTestApp(t)
	uid, user := e.user("xmpp_contents_view", "buyer")
	e.setKey(uid, "")
	_, admin := e.user("xmpp_contents_view_admin", "admin")
	adminForm := xmppSettingsForm()
	adminForm.Set("xmpp_allow_contents", "1")
	e.check(e.do("POST", "/admin/xmpp", admin, adminForm), 303)
	page := e.body("GET", "/account", user, nil, 200)
	if !strings.Contains(page, "Add a usable PGP encryption public key") || !strings.Contains(xmppPreferenceForm(t, page), "disabled") {
		t.Fatal("missing encryption key was not explained beside disabled content consent")
	}
	_, public := testPGPKey(t, "xmpp_contents_view_key")
	e.setKey(uid, public)
	page = e.body("GET", "/account", user, nil, 200)
	if strings.Contains(xmppPreferenceForm(t, page), "disabled") || strings.Contains(page, adminForm.Get("xmpp_sender")) || strings.Contains(page, adminForm.Get("xmpp_password")) {
		t.Fatal("eligible account content form disabled or exposed outgoing credentials")
	}
	adminForm.Del("xmpp_allow_contents")
	e.check(e.do("POST", "/admin/xmpp", admin, adminForm), 303)
	page = e.body("GET", "/account", user, nil, 200)
	if !strings.Contains(page, "The administrator has not allowed message contents") || !strings.Contains(xmppPreferenceForm(t, page), "disabled") {
		t.Fatal("revoked administrator permission not reflected in private content controls")
	}
}

func xmppPreferenceForm(t *testing.T, page string) string {
	t.Helper()
	return adminForm(t, page, "/account/xmpp", "")
}
