package market

import (
	"context"
	"html"
	"strings"
	"testing"
)

// PM-3: a market-wide deadline recheck tells each active administrator once, naming its cause and no order, and the
// overview flags Payments while one is pending. A pass that finds none pending adds nothing.
func TestXMRDeadlineRecheckTellsAdministrators(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("xnotice_"+randomToken()[:6], "admin")
	gone, _ := x.user("xnoticeg_"+randomToken()[:6], "admin")
	agExec(x.testEnv, "UPDATE users SET suspended_at=now() WHERE id=$1", gone)
	_, moderator := x.user("xnoticem_"+randomToken()[:6], "moderator")
	order, _, _ := x.shippedXMR(x.physical(x.vendor))
	x.forceDeadline(order, "release_at=now()+interval '24 hours'")
	const flag = "A Monero deadline recheck is pending."
	overview := func() string { return html.UnescapeString(x.page("/admin", admin)) }
	notices := func(where string, args ...any) []string {
		t.Helper()
		rows, err := x.DB.Query("SELECT body FROM notifications WHERE body LIKE 'A Monero escrow deadline recheck%' "+where+" ORDER BY created", args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var bodies []string
		for rows.Next() {
			var b string
			if err = rows.Scan(&b); err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, b)
		}
		return bodies
	}
	active := agInt(x.testEnv, "SELECT count(*) FROM users WHERE role='admin' AND suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL")
	if active < 2 {
		t.Fatalf("%d active administrators in the fixture", active)
	}

	// Nothing pending: no flag, no notice, whatever a pass finds.
	mustNotContain(t, overview(), flag)
	x.mustPoll()
	if got := notices(""); len(got) != 0 {
		t.Fatalf("a pass with nothing pending: %q", got)
	}

	// An outage's request: flagged while pending, on a reload too, with no notice yet.
	if _, err := RequestDeadlineRecheck(context.Background(), x.DB); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		mustContain(t, overview(), flag)
	}
	if got := notices(""); len(got) != 0 {
		t.Fatalf("a pending request: %q", got)
	}
	// Only an administrator reads the overview.
	if w := x.do("GET", "/admin", moderator, nil); w.Code == 200 || strings.Contains(html.UnescapeString(w.Body.String()), flag) {
		t.Fatalf("a moderator reads the flag: %d", w.Code)
	}

	// The recheck: one notice for each active administrator, none for anyone else, then no flag.
	x.mustPoll()
	got := notices("AND audience='admin' AND order_id IS NULL")
	if len(got) != active || len(notices("")) != active {
		t.Fatalf("after the recheck: %d notices of %d, want %d admin-scoped: %q", len(got), len(notices("")), active, got)
	}
	if body := got[0]; !strings.Contains(body, "after an outage") || !strings.Contains(body, "Admin > Payments") ||
		strings.Contains(body, order) || strings.Contains(body, shortID(order)) || strings.Contains(body, "moved 1") {
		t.Fatalf("the notice: %q", body)
	}
	if got := notices("AND user_id=$1", gone); len(got) != 0 {
		t.Fatalf("a suspended administrator: %q", got)
	}
	if got := notices("AND user_id=$1", x.partyOf(order, roleBuyer)); len(got) != 0 {
		t.Fatalf("the buyer: %q", got)
	}
	mustNotContain(t, overview(), flag)
	x.mustPoll()
	if got := notices(""); len(got) != active {
		t.Fatalf("a later pass with nothing pending added notices: %d, want %d", len(got), active)
	}

	// A recheck that moves nothing still tells them, since it still holds cancels, and a restore says a restore.
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", xmrReleaseRecheckKey)
	mustContain(t, overview(), flag)
	x.mustPoll()
	got = notices("AND body LIKE '%after a restore from backup%'")
	if len(got) != active || len(notices("")) != 2*active {
		t.Fatalf("after a restore's recheck: %d of %d notices: %q", len(got), len(notices("")), got)
	}
}
