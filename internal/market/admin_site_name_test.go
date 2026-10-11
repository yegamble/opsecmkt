package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestAdminSettingsUsesSharedSiteNameLimit(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("site_name_limit_admin", "admin")
	valid := strings.Repeat("é", 80)
	w := e.do("POST", "/admin", admin, url.Values{"action": {"settings"}, "site_name": {valid}, "password": {testPassword}})
	if w.Code != 303 || agStr(e, "SELECT value FROM settings WHERE key='site_name'") != valid {
		t.Fatalf("80-character site name: status=%d saved=%q", w.Code, agStr(e, "SELECT value FROM settings WHERE key='site_name'"))
	}
	audits := len(e.audits(adminID))
	for _, invalid := range []string{"", strings.Repeat("é", 81)} {
		w = e.do("POST", "/admin", admin, url.Values{"action": {"settings"}, "site_name": {invalid}, "password": {testPassword}})
		if w.Code != 400 || agStr(e, "SELECT value FROM settings WHERE key='site_name'") != valid || len(e.audits(adminID)) != audits {
			t.Fatalf("invalid site name changed state: status=%d saved=%q audits=%v", w.Code, agStr(e, "SELECT value FROM settings WHERE key='site_name'"), e.audits(adminID))
		}
	}
}
