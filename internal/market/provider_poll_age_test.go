package market

import (
	"html"
	"strings"
	"testing"
	"time"
)

func TestProviderPollAgeBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	t.Setenv("PAYMENT_POLL_INTERVAL", "20m")
	for _, tc := range []struct {
		name  string
		raw   string
		want  string
		stale bool
	}{
		{name: "missing", want: "Not polled yet"},
		{name: "malformed", raw: "not a timestamp", want: "Unknown"},
		{name: "future", raw: "2026-10-08 12:01:00 UTC", want: "Clock differs from database"},
		{name: "below configured threshold", raw: "2026-10-08 11:30:00 UTC", want: "30 min ago"},
		{name: "past configured threshold", raw: "2026-10-08 11:19:00 UTC", want: "41 min ago", stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, stale := providerPollAge(tc.raw, now)
			if got != tc.want || stale != tc.stale {
				t.Fatalf("providerPollAge(%q) = (%q, %v), want (%q, %v)", tc.raw, got, stale, tc.want, tc.stale)
			}
		})
	}
}

func TestAdminProviderPollAgeAndStaleWarning(t *testing.T) {
	t.Setenv("PAYMENT_POLL_INTERVAL", "1s")
	p := newPayEnv(t)
	p.poll()
	row := func() string {
		t.Helper()
		body := html.UnescapeString(p.page("/admin", p.adminSess))
		start := strings.Index(body, "<td>BTC</td>")
		if start < 0 {
			t.Fatal("BTC provider row missing")
		}
		end := strings.Index(body[start:], "</tr>")
		if end < 0 {
			t.Fatal("BTC provider row is unterminated")
		}
		return body[start : start+end]
	}
	if _, err := p.DB.Exec("UPDATE payment_status SET last_poll=now()-interval '3 minutes' WHERE currency='BTC'"); err != nil {
		t.Fatal(err)
	}
	fresh := row()
	if !strings.Contains(fresh, "3 min ago") || strings.Contains(fresh, "Stale") {
		t.Fatalf("fresh poll diagnostics: %s", fresh)
	}
	if _, err := p.DB.Exec("UPDATE payment_status SET last_poll=now()-interval '12 minutes' WHERE currency='BTC'"); err != nil {
		t.Fatal(err)
	}
	stale := row()
	if !strings.Contains(stale, "12 min ago") || !strings.Contains(stale, "Stale") {
		t.Fatalf("stale poll warning missing: %s", stale)
	}
}
