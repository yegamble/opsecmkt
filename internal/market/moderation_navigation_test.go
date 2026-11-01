package market

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestModerationNavigationRequiresPrivateIngress(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("moderation_navigation_admin", "admin")
	public := e.body("GET", "/admin", admin, nil, 200)
	if strings.Contains(public, `href="/admin/moderation"`) {
		t.Fatal("public administration offers a private-only action that returns 404")
	}
	e.A.adminSocket = "/run/test-private-admin/http.sock"
	w := ingressDo(e, true, "GET", "/admin", admin, nil)
	e.check(w, 200)
	mustContain(t, w.Body.String(), `href="/admin/moderation"`, "Private moderation review and worker status")
	e.check(ingressDo(e, false, "GET", "/admin/moderation", admin, nil), 404)
}

// moderationSummaryBlock is the overview's queue line.
var moderationSummaryBlock = regexp.MustCompile(`(?s)<p class="help staff-attention" id="moderation-queue-summary">(.*?)</p>`)

// A-723: the overview says how much waits in the private queue: counts only, above the checklist, on the private
// administrator connection only, and the section navigation reaches the queue from every administration page there.
func TestOverviewCountsUnreviewedProposals(t *testing.T) {
	e, _, admin, product, escalation := moderationAdminFixture(t, 0, false)
	_, moderator := e.user("overview_queue_mod", "moderator")
	vendor, _ := e.user("overview_queue_vendor", "vendor")
	e.titledListing(vendor, "Plain lamp with a deadline ahead")
	if n, err := moderationTestWorker(e).Pass(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("open pass: n=%d err=%v", n, err)
	}
	late, _ := e.user("overview_queue_late", "vendor")
	e.titledListing(late, "Plain lamp past its deadline")
	expiring := moderationTestWorker(e)
	expiring.Engine.Config.HumanSLA = time.Nanosecond
	if n, err := expiring.Pass(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("expired pass: n=%d err=%v", n, err)
	}

	w := ingressDo(e, true, "GET", "/admin", admin, nil)
	e.check(w, 200)
	body := w.Body.String()
	line := moderationSummaryBlock.FindStringSubmatch(body)
	if line == nil {
		t.Fatalf("no queue line on the private overview:\n%s", body)
	}
	mustContain(t, line[1], `href="/admin/moderation"`, "2 unreviewed, 1 escalation, 1 past deadline")
	if strings.Index(body, `id="moderation-queue-summary"`) > strings.Index(body, `id="getting-started"`) {
		t.Fatal("the queue line is below the checklist")
	}
	// Counts only: the overview names no proposal, target or account.
	mustNotContain(t, body, escalation, product, "overview_queue_vendor", "overview_queue_late", "moderation_ui_vendor")
	mustContain(t, adminNavBlock(t, body), `href="/admin/moderation"`, ">Moderation review<")

	section := ingressDo(e, true, "GET", "/admin/users", admin, nil)
	e.check(section, 200)
	mustContain(t, adminNavBlock(t, section.Body.String()), `href="/admin/moderation"`)
	mustNotContain(t, section.Body.String(), "moderation-queue-summary")

	// The panel carries the navigation too, with itself current, and the same counts.
	panel := ingressDo(e, true, "GET", "/admin/moderation", admin, nil)
	e.check(panel, 200)
	mustContain(t, adminNavBlock(t, panel.Body.String()), `<a href="/admin/moderation" aria-current="page">Moderation review</a>`)
	mustContain(t, panel.Body.String(), `href="/admin/moderation">2 unreviewed</a>`)

	// A moderator has no overview, on either connection.
	e.check(ingressDo(e, true, "GET", "/admin", moderator, nil), 403)
	// With a private connection configured the public listener refuses the overview outright.
	e.check(ingressDo(e, false, "GET", "/admin", admin, nil), 404)
	// Without one there is no panel: no line, no count, no entry.
	e.A.adminSocket = ""
	public := e.body("GET", "/admin", admin, nil, 200)
	mustNotContain(t, public, "moderation-queue-summary", "unreviewed", `href="/admin/moderation"`)
	mustNotContain(t, e.body("GET", "/admin/users", admin, nil, 200), `href="/admin/moderation"`)
}

func adminNavBlock(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `aria-label="Administration sections"`)
	if !ok {
		t.Fatalf("no section navigation:\n%s", body)
	}
	nav, _, _ := strings.Cut(rest, "</nav>")
	return nav
}
