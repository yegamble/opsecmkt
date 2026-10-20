package market

import (
	"context"
	"net/http/httptest"
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

// The settings clause must return its write failure instead of shadowing the
// error returned by adminChange. A trigger allows the SELECT FOR UPDATE
// while PostgreSQL refuses the subsequent INSERT/UPDATE.
func TestAdminSettingsReturnsWriteFailure(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("site_name_write_failure_admin", "admin")
	u := e.loadUser(id)
	tx, err := e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE FUNCTION refuse_site_name_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'site-name-write-refused'; END $$;
		CREATE TRIGGER refuse_site_name_write BEFORE INSERT OR UPDATE ON settings
		FOR EACH ROW EXECUTE FUNCTION refuse_site_name_write()`); err != nil {
		t.Fatal(err)
	}
	_, err = adminChange(&actionCtx{
		A: e.A, Tx: tx, User: u, R: httptest.NewRequest("POST", "/admin", nil),
		Form: url.Values{"action": {"settings"}, "site_name": {"Refused site name"}},
	})
	if err == nil {
		t.Fatal("adminChange discarded the settings write failure")
	}
}
