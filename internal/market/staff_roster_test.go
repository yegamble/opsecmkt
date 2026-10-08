package market

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaffRosterPagesAllHiddenSuspendedStaffAndKeyStatus(t *testing.T) {
	e := newTestApp(t)
	admin := e.setup()
	if _, err := e.DB.Exec(`INSERT INTO users(id,handle,password_hash,role,created) SELECT lpad(to_hex(g),32,'0'),'private_staff_'||lpad(g::text,3,'0'),'unused-test-hash','moderator',now() FROM generate_series(1,101) g`); err != nil {
		t.Fatal(err)
	}
	_, key := testPGPKey(t, "roster_private_identity")
	agExec(e, `UPDATE users SET pgp=$1 WHERE handle='private_staff_001'`, key)
	agExec(e, `UPDATE users SET pgp='not-a-key',suspended_at=now() WHERE handle='private_staff_002'`)
	_, buyer := e.user("roster_buyer", "buyer")
	_, mod := e.user("roster_viewer", "moderator")
	for _, tc := range []struct {
		path        string
		count       int
		first, last string
	}{
		{"/admin", 50, "admin_user", "private_staff_049"},
		{"/admin?staff_page=2", 50, "private_staff_050", "private_staff_099"},
		{"/admin?staff_page=3", 3, "private_staff_100", "roster_viewer"},
	} {
		w := e.do("GET", tc.path, admin, nil)
		e.check(w, 200)
		s := w.Body.String()
		start := strings.Index(s, `<section class="panel" aria-labelledby="staff-roster">`)
		if start < 0 {
			t.Fatal("missing roster")
		}
		end := strings.Index(s[start:], "</section>")
		if end < 0 {
			t.Fatal("unclosed roster")
		}
		s = s[start : start+end]
		if got := strings.Count(s, "<tr><td>"); got != tc.count || !strings.Contains(s, tc.first) || !strings.Contains(s, tc.last) || !strings.Contains(s, "of 103 staff accounts") {
			t.Fatalf("%s: rows=%d %s", tc.path, got, s)
		}
		if strings.Contains(s, "roster_private_identity") || strings.Contains(s, "BEGIN PGP") || strings.Contains(s, "roster_buyer") {
			t.Fatal("roster leaked extra data")
		}
		if tc.path == "/admin" {
			for _, text := range []string{"Can receive encrypted messages", "Cannot receive encrypted messages", "No saved key", "Suspended", "#staff-roster"} {
				if !strings.Contains(s, text) {
					t.Errorf("missing %q", text)
				}
			}
		}
	}
	for _, token := range []string{buyer, mod, ""} {
		w := e.do("GET", "/admin?staff_page=3", token, nil)
		if w.Code == 200 || strings.Contains(w.Body.String(), "private_staff_100") {
			t.Fatal("unauthorized roster")
		}
	}
	// The public staff-list privacy choice remains off; this private roster does not publish staff.
	w := e.do("GET", "/canary", "", nil)
	if strings.Contains(w.Body.String(), "private_staff_") {
		t.Fatal("published hidden staff")
	}
	adminID := agStr(e, "SELECT id FROM users WHERE handle='admin_user'")
	for _, update := range []string{"role='buyer'", "role='admin',suspended_at=now()"} {
		agExec(e, "UPDATE users SET "+update+" WHERE id=$1", adminID)
		d := &PageData{User: &User{ID: adminID, Role: "admin"}}
		if err := loadStaffRoster(t.Context(), e.A, httptest.NewRequest("GET", "/admin", nil), d); err != nil || len(d.StaffRoster) != 0 || d.StaffRosterPaging != nil {
			t.Fatal(fmt.Sprintf("stale admin: %v %#v", err, d))
		}
	}
}
