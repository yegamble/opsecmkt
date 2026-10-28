package market

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/moderation"
)

// A-683 (STAFF94-1): a stopped worker leaves capture enqueueing with no pass. The private moderation panel and the
// /admin overview say so once the last pass is older than three pass intervals plus a five-minute margin, using the
// interval the worker stored with its status (migration 152).

var moderationStaleBlock = regexp.MustCompile(`(?s)<div class="alert" role="status" id="moderation-worker-stale">(.*?)</div>`)

func moderationStaleText(body string) string {
	m := moderationStaleBlock.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.Join(strings.Fields(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(m[1], "")), " ")
}

func TestModerationAdminShowsStaleWorker(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("stale_worker_admin", "admin")
	_, mod := e.user("stale_worker_mod", "moderator")
	e.A.adminSocket = "/run/test-private-admin/http.sock"
	panel := func() string {
		t.Helper()
		w := privateHostRequest(e, "GET", "/admin/moderation", admin, nil, true)
		e.check(w, 200)
		return w.Body.String()
	}
	overview := func() string {
		t.Helper()
		w := ingressDo(e, true, "GET", "/admin", admin, nil)
		e.check(w, 200)
		return w.Body.String()
	}
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	// Capture on and no pass reported yet.
	const never = "No pass reported yet; the worker may have stopped."
	if got := moderationStaleText(panel()); !strings.HasPrefix(got, never) {
		t.Fatalf("panel before any pass: %q", got)
	}
	if got := moderationStaleText(overview()); !strings.HasPrefix(got, never) {
		t.Fatalf("overview before any pass: %q", got)
	}
	w := &ModerationWorker{DB: e.DB, Engine: moderation.Engine{Config: moderation.DefaultConfig()}, Interval: time.Minute}
	if _, err := w.Pass(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT pass_interval_seconds FROM moderation_worker_status`) != 60 {
		t.Fatal("pass interval not stored with the status")
	}
	body := panel()
	if got := moderationStaleText(body); got != "" {
		t.Fatalf("fresh pass shown as stale: %q", got)
	}
	mustContain(t, body, "<dt>Pass interval</dt><dd>60 seconds</dd>")
	if got := moderationStaleText(overview()); got != "" {
		t.Fatalf("overview: fresh pass shown as stale: %q", got)
	}
	// 3 x 60 s + 5 min = 8 minutes: 7 minutes is still within it, 9 is past it.
	agExec(e, `UPDATE moderation_worker_status SET last_pass=now()-interval '7 minutes 10 seconds'`)
	if got := moderationStaleText(panel()); got != "" {
		t.Fatalf("within the threshold shown as stale: %q", got)
	}
	agExec(e, `UPDATE moderation_worker_status SET last_pass=now()-interval '9 minutes 10 seconds'`)
	const stale = "No pass for 9 minutes; the worker may have stopped."
	if got := moderationStaleText(panel()); !strings.HasPrefix(got, stale) {
		t.Fatalf("panel past the threshold: %q", got)
	}
	ob := overview()
	if got := moderationStaleText(ob); !strings.HasPrefix(got, stale) {
		t.Fatalf("overview past the threshold: %q", got)
	}
	mustContain(t, moderationStaleBlock.FindString(ob), `href="/admin/moderation"`)
	// Section pages do not repeat it.
	sw := ingressDo(e, true, "GET", "/admin/system", admin, nil)
	e.check(sw, 200)
	if got := moderationStaleText(sw.Body.String()); got != "" {
		t.Fatalf("stale warning on a section page: %q", got)
	}
	// The stored interval decides: a ten-minute worker is not stale after nine minutes (threshold 35).
	agExec(e, `UPDATE moderation_worker_status SET pass_interval_seconds=600`)
	if got := moderationStaleText(panel()); got != "" {
		t.Fatalf("ten-minute interval shown as stale: %q", got)
	}
	// A status written before migration 152 has no interval: the default minute applies.
	agExec(e, `UPDATE moderation_worker_status SET pass_interval_seconds=NULL`)
	if got := moderationStaleText(panel()); !strings.HasPrefix(got, stale) {
		t.Fatalf("no stored interval: %q", got)
	}
	// Private ingress, administrators only: the public listener refuses /admin with ADMIN_SOCKET set, a moderator
	// gets no overview, and without ADMIN_SOCKET there is no private panel and no warning.
	e.check(ingressDo(e, false, "GET", "/admin", admin, nil), 404)
	mw := ingressDo(e, true, "GET", "/admin", mod, nil)
	mustNotContain(t, mw.Body.String(), "the worker may have stopped")
	e.A.adminSocket = ""
	pw := ingressDo(e, false, "GET", "/admin", admin, nil)
	e.check(pw, 200)
	mustNotContain(t, pw.Body.String(), "the worker may have stopped")
	e.A.adminSocket = "/run/test-private-admin/http.sock"
	// Capture off: no worker is expected, so no warning, however old the last pass.
	if err := SetModerationEnabled(t.Context(), e.DB, false); err != nil {
		t.Fatal(err)
	}
	agExec(e, `UPDATE moderation_worker_status SET last_pass=now()-interval '3 days'`)
	if got := moderationStaleText(panel()); got != "" {
		t.Fatalf("capture off shown as stale: %q", got)
	}
	if got := moderationStaleText(overview()); got != "" {
		t.Fatalf("overview: capture off shown as stale: %q", got)
	}
}

// A-683: the panel shows "Capture: on" or "Capture: off" from settings.moderation_enabled, and with capture off it
// does not present the model as merely absent. A recovered database outage is shown with its times.
func TestModerationAdminShowsCaptureState(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("capture_state_admin", "admin")
	e.A.adminSocket = "/run/test-private-admin/http.sock"
	panel := func() string {
		t.Helper()
		w := privateHostRequest(e, "GET", "/admin/moderation", admin, nil, true)
		e.check(w, 200)
		return w.Body.String()
	}
	body := panel()
	mustContain(t, body, "Capture: off.", "<dt>Local model</dt><dd>Not in use while capture is off</dd>")
	mustNotContain(t, body, "Capture: on", "<dd>Absent</dd>")
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	w := &ModerationWorker{DB: e.DB, Engine: moderation.Engine{Config: moderation.DefaultConfig()}, Interval: time.Minute}
	if _, err := w.Pass(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	body = panel()
	mustContain(t, body, "Capture: on.", "<dt>Local model</dt><dd>Absent</dd>", "No model configured")
	mustNotContain(t, body, "Capture: off", "Last database outage")
	agExec(e, `UPDATE moderation_worker_status SET failure_class='db_unavailable',db_outage_from='2026-10-27 01:02:03+00',db_outage_until='2026-10-27 01:09:03+00'`)
	mustContain(t, panel(), "Database unavailable", "<dt>Last database outage (UTC)</dt><dd>2026-10-27T01:02:03Z until 2026-10-27T01:09:03Z</dd>")
	if err := SetModerationEnabled(t.Context(), e.DB, false); err != nil {
		t.Fatal(err)
	}
	body = panel()
	mustContain(t, body, "Capture: off.", "Not in use while capture is off", "Last database outage")
	mustNotContain(t, body, "Capture: on")
}
