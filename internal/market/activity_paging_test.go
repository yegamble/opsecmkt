package market

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestAccountActivityPagesAreBoundedPrivateAndStable(t *testing.T) {
	e := newTestApp(t)
	viewer, session := e.user("activity_viewer", "buyer")
	other, _ := e.user("activity_other", "buyer")
	recipient, _ := e.user("activity_recipient", "buyer")
	vendor, _ := e.user("activity_vendor", "vendor")
	product := e.product(vendor, "physical")

	// Give all rows the same timestamp so the ID tie-breaker alone determines page boundaries.
	if _, err := e.DB.Exec(`INSERT INTO orders(id,buyer_id,product_id,currency,amount,state,created)
		SELECT lpad(to_hex(g),32,'0'),$1,$2,'XMR',100000,'awaiting_payment',now()
		FROM generate_series(1,101) g`, viewer, product); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO orders(id,buyer_id,product_id,currency,amount,state,created)
		SELECT lpad(to_hex(1000+g),32,'0'),$1,$2,'XMR',100000,'awaiting_payment',now()
		FROM generate_series(1,2) g`, other, product); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO messages(id,sender_id,recipient_id,body,encrypted,created)
		SELECT lpad(to_hex(g),32,'0'),$1,$2,'own-message-'||lpad(g::text,3,'0'),true,now()
		FROM generate_series(1,101) g`, viewer, recipient); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO messages(id,sender_id,recipient_id,body,encrypted,created)
		SELECT lpad(to_hex(1000+g),32,'0'),$1,$2,'private-other-message-'||g,true,now()
		FROM generate_series(1,2) g`, other, recipient); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO notifications(id,user_id,body,created)
		SELECT lpad(to_hex(g),32,'0'),$1,'own-notice-'||lpad(g::text,3,'0'),now()
		FROM generate_series(1,101) g`, viewer); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO notifications(id,user_id,body,created)
		SELECT lpad(to_hex(1000+g),32,'0'),$1,'private-other-notice-'||g,now()
		FROM generate_series(1,2) g`, other); err != nil {
		t.Fatal(err)
	}

	get := func(path string) string {
		t.Helper()
		w := e.do("GET", path, session, nil)
		e.check(w, 200)
		return w.Body.String()
	}
	orderIDs := func(body string) []string {
		t.Helper()
		matches := regexp.MustCompile(`<small class="muted mono">([a-f0-9]+)</small>`).FindAllStringSubmatch(body, -1)
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m[1]
		}
		return ids
	}
	checkPages := func(label, path, private, firstMarker, middleMarker, lastMarker string, extract func(string) []string) {
		t.Helper()
		first := get(path)
		if strings.Contains(first, private) || !strings.Contains(first, firstMarker) || !strings.Contains(first, "Showing 1–50 of 101") || !strings.Contains(first, `href="`+path+`?page=2"`) || strings.Contains(first, `rel="prev"`) {
			t.Fatalf("%s first page is incomplete, leaks another user's rows, or lacks next link", label)
		}
		firstIDs := extract(first)
		if len(firstIDs) != 50 {
			t.Fatalf("%s first page has %d rows, want 50", label, len(firstIDs))
		}
		second := get(path + "?page=2")
		secondIDs := extract(second)
		if len(secondIDs) != 50 || strings.Contains(second, firstMarker) || !strings.Contains(second, middleMarker) || !strings.Contains(second, `href="`+path+`?page=3"`) || !strings.Contains(second, `href="`+path+`" rel="prev"`) {
			t.Fatalf("%s second page has wrong boundary or controls", label)
		}
		last := get(path + "?page=3")
		lastIDs := extract(last)
		if len(lastIDs) != 1 || !strings.Contains(last, lastMarker) || !strings.Contains(last, "Showing 101–101 of 101") || strings.Contains(last, `rel="next"`) {
			t.Fatalf("%s last page has wrong boundary or controls", label)
		}
		seen := map[string]bool{}
		for _, id := range append(append(firstIDs, secondIDs...), lastIDs...) {
			if seen[id] {
				t.Fatalf("%s repeated row %q across pages", label, id)
			}
			seen[id] = true
		}
		if len(seen) != 101 {
			t.Fatalf("%s covered %d rows, want 101", label, len(seen))
		}
		if again := extract(get(path + "?page=1")); strings.Join(again, ",") != strings.Join(firstIDs, ",") {
			t.Fatalf("%s timestamp ties do not have stable page ordering", label)
		}
		if malformed := get(path + "?page=oops"); !strings.Contains(malformed, "Page 1 of 3") {
			t.Fatalf("%s malformed page did not resolve to first page", label)
		}
		if oversized := get(path + "?page=999999999999999999999999"); !strings.Contains(oversized, "Page 3 of 3") {
			t.Fatalf("%s oversized page did not resolve to last page", label)
		}
	}
	checkPages("orders", "/orders", fmt.Sprintf(`%032x`, 1001), fmt.Sprintf("%032x", 101), fmt.Sprintf("%032x", 51), fmt.Sprintf("%032x", 1), orderIDs)
	// The page links must retain the validated recipient prefill, and unrelated correspondence stays hidden.
	var recipientHandle string
	if err := e.DB.QueryRow(`SELECT handle FROM users WHERE id=$1`, recipient).Scan(&recipientHandle); err != nil {
		t.Fatal(err)
	}
	messagePath := "/messages?to=" + url.QueryEscape(recipientHandle)
	checkPages("messages", "/messages", "private-other-message-", "own-message-101", "own-message-051", "own-message-001", func(body string) []string {
		return regexp.MustCompile(`own-message-([0-9]+)`).FindAllString(body, -1)
	})
	if !strings.Contains(get(messagePath), `href="/messages?page=2&amp;to=`+recipientHandle+`"`) {
		t.Fatal("message paging dropped the recipient prefill")
	}
	checkPages("notifications", "/notifications", "private-other-notice-", "own-notice-101", "own-notice-051", "own-notice-001", func(body string) []string {
		return regexp.MustCompile(`own-notice-([0-9]+)`).FindAllString(body, -1)
	})
}
