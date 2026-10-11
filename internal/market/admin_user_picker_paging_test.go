package market

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestAdminUserPickerPagesNewestAccountsAndKeepsOtherRolesReachable(t *testing.T) {
	e := newTestApp(t)
	admin := e.setup()
	if _, err := e.DB.Exec(`INSERT INTO users(id,handle,password_hash,role,created)
		SELECT lpad(to_hex(g),32,'0'),'paged_user_'||lpad(g::text,3,'0'),'unused-test-hash',
		CASE g%3 WHEN 0 THEN 'vendor' WHEN 1 THEN 'buyer' ELSE 'moderator' END,now()
		FROM generate_series(1,101) g`); err != nil {
		t.Fatal(err)
	}
	page := func(path string) (string, string) {
		t.Helper()
		w := e.do("GET", path, admin, nil)
		e.check(w, 200)
		body := w.Body.String()
		start := strings.Index(body, `<details class="account-list"`)
		if start < 0 {
			t.Fatal("admin page has no account picker")
		}
		end := strings.Index(body[start:], `</details>`)
		if end < 0 {
			t.Fatal("account picker is not closed")
		}
		return body, body[start : start+end]
	}
	accounts := func(list string) []string {
		matches := regexp.MustCompile(`<li>([^<]+)</li>`).FindAllStringSubmatch(list, -1)
		rows := make([]string, len(matches))
		for i, match := range matches {
			rows[i] = match[1]
		}
		return rows
	}
	firstBody, firstList := page("/admin")
	first := accounts(firstList)
	if len(first) != 50 || first[0] != "paged_user_101 · moderator" || !strings.Contains(firstList, "Showing 1–50 of 102 accounts") || !strings.Contains(firstList, `href="/admin?users_page=2"`) || strings.Contains(firstList, `rel="prev"`) {
		t.Fatalf("first picker page rows=%d body=%s", len(first), firstList)
	}
	if !strings.Contains(firstBody, `name="handle"`) || !strings.Contains(firstBody, `pattern="[a-zA-Z0-9_]{3,32}"`) {
		t.Fatal("paging replaced or changed the exact-handle role form")
	}
	secondBody, secondList := page("/admin?users_page=2")
	second := accounts(secondList)
	if len(second) != 50 || second[0] != "paged_user_051 · vendor" || !strings.Contains(secondList, "paged_user_050 · moderator") || !strings.Contains(secondList, `href="/admin?users_page=3"`) || !strings.Contains(secondList, `href="/admin" rel="prev"`) || !strings.Contains(secondBody, `<details class="account-list" open>`) {
		t.Fatalf("second picker page rows=%d body=%s", len(second), secondList)
	}
	lastBody, lastList := page("/admin?users_page=3")
	last := accounts(lastList)
	if len(last) != 2 || last[0] != "paged_user_001 · buyer" || last[1] != "admin_user · admin" || !strings.Contains(lastList, "Showing 101–102 of 102 accounts") || strings.Contains(lastList, `rel="next"`) || !strings.Contains(lastBody, `<details class="account-list" open>`) {
		t.Fatalf("last picker page rows=%d body=%s", len(last), lastList)
	}
	if len(first)+len(second)+len(last) != 102 {
		t.Fatal("picker pages did not cover every account")
	}
	for _, tc := range []struct{ path, page string }{
		{"/admin?users_page=bad", "Page 1 of 3"},
		{"/admin?users_page=0", "Page 1 of 3"},
		{"/admin?users_page=-2", "Page 1 of 3"},
		{"/admin?users_page=999999999999999999999999", "Page 3 of 3"},
		// The generic catalog query parameter must not change the admin picker page.
		{"/admin?page=3", "Page 1 of 3"},
	} {
		body, _ := page(tc.path)
		if !strings.Contains(body, tc.page) {
			t.Errorf("%s: wanted %q", tc.path, tc.page)
		}
	}
	_, buyer := e.user("picker_nonadmin", "buyer")
	w := e.do("GET", "/admin?users_page=2", buyer, nil)
	e.check(w, 403)
	if strings.Contains(w.Body.String(), fmt.Sprintf("paged_user_%03d", 51)) {
		t.Fatal("non-admin received account-picker rows")
	}
}
