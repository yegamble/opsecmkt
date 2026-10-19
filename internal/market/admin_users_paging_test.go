package market

import (
	"fmt"
	"strings"
	"testing"
)

// The user directory (A-545, replacing the role form's "Newest accounts" picker of A-16) pages every account,
// newest first, and keeps the exact-handle role form; odd page numbers land on a real page.
func TestAdminUserDirectoryPagesNewestAccountsAndKeepsOtherRolesReachable(t *testing.T) {
	e := newTestApp(t)
	admin := e.setup()
	if _, err := e.DB.Exec(`INSERT INTO users(id,handle,password_hash,role,created)
		SELECT lpad(to_hex(g),32,'0'),'paged_user_'||lpad(g::text,3,'0'),'unused-test-hash',
		CASE g%3 WHEN 0 THEN 'vendor' WHEN 1 THEN 'buyer' ELSE 'moderator' END,now()+g*interval '1 second'
		FROM generate_series(1,101) g`); err != nil {
		t.Fatal(err)
	}
	page := func(path string) (string, []string) {
		t.Helper()
		w := e.do("GET", path, admin, nil)
		e.check(w, 200)
		body := w.Body.String()
		return body, directoryHandles(body)
	}
	first, rows := page("/admin/users")
	if len(rows) != 50 || rows[0] != "paged_user_101" || !strings.Contains(first, "Showing 1–50 of 102 accounts") || !strings.Contains(first, `href="/admin/users?users_page=2"`) || strings.Contains(first, `aria-label="Previous page of accounts`) {
		t.Fatalf("first directory page rows=%v", rows)
	}
	if !strings.Contains(adminForm(t, first, "/admin", "role"), `pattern="[a-zA-Z0-9_]{3,32}"`) {
		t.Fatal("the directory replaced or changed the exact-handle role form")
	}
	second, secondRows := page("/admin/users?users_page=2")
	if len(secondRows) != 50 || secondRows[0] != "paged_user_051" || secondRows[1] != "paged_user_050" || !strings.Contains(second, `href="/admin/users?users_page=3"`) || !strings.Contains(second, `href="/admin/users" rel="prev"`) {
		t.Fatalf("second directory page rows=%v", secondRows)
	}
	last, lastRows := page("/admin/users?users_page=3")
	if len(lastRows) != 2 || lastRows[0] != "paged_user_001" || lastRows[1] != "admin_user" || !strings.Contains(last, "Showing 101–102 of 102 accounts") || strings.Contains(last, `rel="next"`) {
		t.Fatalf("last directory page rows=%v", lastRows)
	}
	if len(rows)+len(secondRows)+len(lastRows) != 102 {
		t.Fatal("directory pages did not cover every account")
	}
	for _, tc := range []struct{ path, page string }{
		{"/admin/users?users_page=bad", "Page 1 of 3"},
		{"/admin/users?users_page=0", "Page 1 of 3"},
		{"/admin/users?users_page=-2", "Page 1 of 3"},
		{"/admin/users?users_page=999999999999999999999999", "Page 3 of 3"},
		// The generic catalog query parameters must not change the directory page or its search.
		{"/admin/users?page=3", "Page 1 of 3"},
		{"/admin/users?q=paged_user_001", "Page 1 of 3"},
	} {
		body, _ := page(tc.path)
		if !strings.Contains(body, tc.page) {
			t.Errorf("%s: wanted %q", tc.path, tc.page)
		}
	}
	_, buyer := e.user("picker_nonadmin", "buyer")
	w := e.do("GET", "/admin/users?users_page=2", buyer, nil)
	e.check(w, 403)
	if strings.Contains(w.Body.String(), fmt.Sprintf("paged_user_%03d", 51)) {
		t.Fatal("non-admin received directory rows")
	}
}
