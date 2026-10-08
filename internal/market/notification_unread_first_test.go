package market

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNotificationsUnreadFirstAndMarkAllReadIsPrivateAndIdempotent(t *testing.T) {
	e := newTestApp(t)
	viewerID, session := e.user("notification_order_viewer", "buyer")
	otherID, _ := e.user("notification_order_other", "buyer")
	created := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	if _, err := e.DB.Exec(`INSERT INTO notifications(id,user_id,body,is_read,created)
		SELECT lpad(to_hex(g),32,'0'),$1,'own-notice-'||lpad(g::text,3,'0'),g%2=0,$2
		FROM generate_series(1,120) g`, viewerID, created); err != nil {
		t.Fatal(err)
	}
	for _, update := range []struct {
		id      int
		created time.Time
	}{{3, created.Add(4 * time.Hour)}, {1, created.Add(3 * time.Hour)}, {2, created.Add(4 * time.Hour)}, {4, created.Add(3 * time.Hour)}} {
		if _, err := e.DB.Exec(`UPDATE notifications SET created=$3 WHERE user_id=$1 AND id=lpad(to_hex($2::int),32,'0')`, viewerID, update.id, update.created); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.DB.Exec(`INSERT INTO notifications(id,user_id,body,is_read,created)
		VALUES($1,$2,'private-other-notice',false,$3)`, randomToken(), otherID, created); err != nil {
		t.Fatal(err)
	}

	get := func(page string) string {
		t.Helper()
		w := e.do("GET", "/notifications"+page, session, nil)
		e.check(w, http.StatusOK)
		return w.Body.String()
	}
	articleRE := regexp.MustCompile(`(?s)<article class="message">(.*?)</article>`)
	markerRE := regexp.MustCompile(`own-notice-(\d{3})`)
	checkPage := func(label, path string, ids []int, unreadCount int) {
		t.Helper()
		body := get(path)
		articles := articleRE.FindAllStringSubmatch(body, -1)
		if len(articles) != len(ids) {
			t.Fatalf("%s has %d notifications, want %d", label, len(articles), len(ids))
		}
		if strings.Contains(body, "private-other-notice") {
			t.Fatalf("%s exposed another user's notification", label)
		}
		for i, article := range articles {
			marker := markerRE.FindStringSubmatch(article[1])
			if marker == nil || marker[1] != fmt.Sprintf("%03d", ids[i]) {
				t.Fatalf("%s row %d has marker %v, want %03d", label, i+1, marker, ids[i])
			}
			unread := strings.Contains(article[1], ">Unread</span>")
			if unread != (i < unreadCount) {
				t.Fatalf("%s row %d read status=%t, unread rows=%d", label, i+1, unread, unreadCount)
			}
		}
	}
	var firstIDs, secondIDs, thirdIDs []int
	firstIDs = append(firstIDs, 3, 1)
	for n := 119; n >= 25; n -= 2 {
		firstIDs = append(firstIDs, n)
	}
	for n := 23; n >= 5; n -= 2 {
		secondIDs = append(secondIDs, n)
	}
	secondIDs = append(secondIDs, 2, 4)
	for n := 120; n >= 46; n -= 2 {
		secondIDs = append(secondIDs, n)
	}
	for n := 44; n >= 6; n -= 2 {
		thirdIDs = append(thirdIDs, n)
	}

	first := get("")
	if !strings.Contains(first, "Page 1 of 3") || !strings.Contains(first, `href="/notifications?page=2"`) {
		t.Fatal("notification paging did not preserve its 50-row page count")
	}
	mustContain(t, first, `<form class="notification-mark-all" method="post" action="/notifications">`,
		`name="csrf" value="`+e.A.csrf(session)+`"`, `name="mark_all" value="1"`, `>Mark all as read</button>`)
	checkPage("page 1", "", firstIDs, 50)
	if body := get("?page=2"); !strings.Contains(body, "Page 2 of 3") || !strings.Contains(body, `href="/notifications?page=3"`) {
		t.Fatal("notification paging lost the second page")
	}
	checkPage("page 2", "?page=2", secondIDs, 10)
	if body := get("?page=3"); !strings.Contains(body, "Page 3 of 3") || strings.Contains(body, `rel="next"`) {
		t.Fatal("notification paging lost the last-page boundary")
	}
	checkPage("page 3", "?page=3", thirdIDs, 0)

	// A POST without the rendered CSRF value cannot mark any of the current user's unread rows.
	r := httptest.NewRequest(http.MethodPost, "/notifications", strings.NewReader("mark_all=1"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: session})
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND NOT is_read", viewerID) != 60 {
		t.Fatalf("CSRF-less mark-all: status=%d unread=%d", w.Code, agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND NOT is_read", viewerID))
	}

	markAll := form("mark_all", "1")
	e.check(e.do(http.MethodPost, "/notifications", session, markAll), http.StatusSeeOther)
	if unread := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND NOT is_read", viewerID); unread != 0 {
		t.Fatalf("mark-all left %d of the current user's notifications unread", unread)
	}
	if read := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND is_read", viewerID); read != 120 {
		t.Fatalf("mark-all changed or lost existing read rows: read=%d", read)
	}
	if agStr(e, "SELECT is_read::text FROM notifications WHERE user_id=$1 AND body='private-other-notice'", otherID) != "false" {
		t.Fatal("mark-all changed another user's unread notification")
	}
	if n := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Marked all notifications read'", viewerID); n != 1 {
		t.Fatalf("mark-all audit entries after first write: %d", n)
	}
	e.check(e.do(http.MethodPost, "/notifications", session, markAll), http.StatusSeeOther)
	if n := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Marked all notifications read'", viewerID); n != 1 {
		t.Fatalf("idempotent repeat duplicated the audit row: %d", n)
	}
	if page := get(""); strings.Contains(page, `class="notification-mark-all"`) {
		t.Fatal("mark-all form remained after all of this user's notifications were read")
	}
}
