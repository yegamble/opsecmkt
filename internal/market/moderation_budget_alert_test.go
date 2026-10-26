package market

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A-648: the /admin overview warns while the moderation lifetime pool runs low or refused captures wait after a
// capacity overflow. Counts appear only on private administrator ingress; the alert never names an account, a
// target, a kind or a time.

const moderationBudgetPublicText = "Moderation capture needs attention: open the private moderation panel on the private administrator connection."

var moderationBudgetBlock = regexp.MustCompile(`(?s)<div class="alert" role="status" id="moderation-budget">(.*?)</div>`)

// moderationBudgetAlertText is the overview's alert with its markup removed, "" when there is none.
func moderationBudgetAlertText(t *testing.T, body string) string {
	t.Helper()
	m := moderationBudgetBlock.FindAllStringSubmatch(body, -1)
	if len(m) == 0 {
		if strings.Contains(body, `id="moderation-budget"`) {
			t.Fatal("moderation budget alert has unexpected markup")
		}
		return ""
	}
	if len(m) > 1 {
		t.Fatalf("moderation budget alert shown %d times", len(m))
	}
	return strings.Join(strings.Fields(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(m[0][1], "")), " ")
}

// moderationBudgetRefuseIdentifiers fails when the alert carries anything that could name an account, a target or a
// moment: a timestamp, a 64-hex digest, a UUID, a token-shaped id, a moderation kind, or one of the given values.
func moderationBudgetRefuseIdentifiers(t *testing.T, alert string, values ...string) {
	t.Helper()
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`\d{4}-\d{2}-\d{2}|\d{2}:\d{2}`),
		regexp.MustCompile(`(?i)[0-9a-f]{64}`),
		regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`),
		regexp.MustCompile(`[A-Za-z0-9_]{16,}`),
		regexp.MustCompile(`(?i)\b(listing|upload|dispute|review|payment_flag|actor_share|retained_capacity|backfill_capacity)\b`),
	} {
		if s := re.FindString(alert); s != "" {
			t.Fatalf("moderation budget alert carries %q: %s", s, alert)
		}
	}
	for _, v := range values {
		if strings.Contains(alert, v) {
			t.Fatalf("moderation budget alert names %q: %s", v, alert)
		}
	}
}

// moderationBudgetExhaust gives a two-allocation pool to one vendor's three listings: two are captured, the third
// is refused for retained capacity and waits as a marker.
func moderationBudgetExhaust(t *testing.T, e *testEnv, vendor string) []string {
	t.Helper()
	limits := DefaultModerationLimits()
	limits.MaxRecords = 2
	if err := ConfigureModerationLimits(t.Context(), e.DB, limits); err != nil {
		t.Fatal(err)
	}
	moderationEnable(t, e)
	var ids []string
	for range 3 {
		ids = append(ids, e.product(vendor, "digital"))
	}
	if agInt(e, `SELECT allocated_records FROM moderation_capacity`) != 2 || agInt(e, `SELECT backfill FROM moderation_capacity`) != 1 ||
		agInt(e, `SELECT occurrences FROM moderation_overflow WHERE kind='listing' AND reason='retained_capacity'`) != 1 {
		t.Fatal("precondition: the pool is not exhausted with one waiting marker")
	}
	return ids
}

func TestAdminOverviewModerationBudgetAlert(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("budget_alert_vendor", "vendor")
	_, admin := e.user("budget_alert_admin", "admin")
	e.A.adminSocket = "/run/test-private-admin/http.sock"
	overview := func() string {
		w := ingressDo(e, true, "GET", "/admin", admin, nil)
		e.check(w, 200)
		return w.Body.String()
	}
	if got := moderationBudgetAlertText(t, overview()); got != "" {
		t.Fatalf("alert with capture off and a full pool: %s", got)
	}
	ids := moderationBudgetExhaust(t, e, vendor)
	body := overview()
	got := moderationBudgetAlertText(t, body)
	want := "Moderation capture needs attention: 2 of 2 lifetime allocations used; 1 target(s) waiting for capture. " +
		"Raise the limits under Moderation budget on the private moderation panel, or --max-records and --max-reserved-bytes on the moderation worker (each allocation reserves 524288 bytes). " +
		"Open the private moderation panel."
	if got != want {
		t.Fatalf("private alert\n got: %s\nwant: %s", got, want)
	}
	mustContain(t, moderationBudgetBlock.FindString(body), `href="/admin/moderation"`)
	mustContain(t, body, "Private moderation review and worker status")
	moderationBudgetRefuseIdentifiers(t, got, append(ids, vendor, "budget_alert_vendor")...)
	for _, id := range ids {
		mustNotContain(t, body, id)
	}
	// With the private listener configured, the public listener refuses /admin before any read, spoofed headers or not.
	e.check(ingressDo(e, false, "GET", "/admin", admin, nil), 404)

	// Accounts flagged for a rescan are counted, never named.
	agExec(e, `UPDATE moderation_actor_allocations SET rescan=true WHERE actor_id=$1`, vendor)
	got = moderationBudgetAlertText(t, overview())
	mustContain(t, got, "1 target(s) waiting for capture; 1 account(s) awaiting a rescan.")
	moderationBudgetRefuseIdentifiers(t, got, append(ids, vendor, "budget_alert_vendor")...)

	// The alert stays on the overview: no section page queries or shows it.
	w := ingressDo(e, true, "GET", "/admin/system", admin, nil)
	e.check(w, 200)
	if got := moderationBudgetAlertText(t, w.Body.String()); got != "" {
		t.Fatalf("alert on a section page: %s", got)
	}
}

// The byte budget binds at the defaults: remaining allocations are the smaller of the record and byte headroom, and
// the alert starts when they are at most two daily actor shares (81 each at --max-records 4096).
func TestAdminOverviewModerationBudgetAlertByteThreshold(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("budget_bytes_admin", "admin")
	e.A.adminSocket = "/run/test-private-admin/http.sock"
	moderationEnable(t, e)
	for _, c := range []struct {
		allocations int64
		want        string
	}{
		{163, ""},
		{162, "Moderation capture needs attention: 0 of 162 lifetime allocations used; 0 target(s) waiting for capture. " +
			"Raise the limits under Moderation budget on the private moderation panel, or --max-records and --max-reserved-bytes on the moderation worker (each allocation reserves 524288 bytes). " +
			"Open the private moderation panel."},
	} {
		limits := DefaultModerationLimits()
		limits.MaxBytes = c.allocations * moderationAllocationBytes
		if err := ConfigureModerationLimits(t.Context(), e.DB, limits); err != nil {
			t.Fatal(err)
		}
		w := ingressDo(e, true, "GET", "/admin", admin, nil)
		e.check(w, 200)
		if got := moderationBudgetAlertText(t, w.Body.String()); got != c.want {
			t.Fatalf("%d allocations left\n got: %s\nwant: %s", c.allocations, got, c.want)
		}
	}
}

func TestAdminOverviewModerationBudgetAlertPublicIngressNoCounts(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("budget_public_vendor", "vendor")
	_, admin := e.user("budget_public_admin", "admin")
	ids := moderationBudgetExhaust(t, e, vendor)
	// The public listener with spoofed onion Host and forwarding headers (ingressDo): ingress comes only from the
	// private listener's context, so the counts stay hidden.
	w := ingressDo(e, false, "GET", "/admin", admin, nil)
	e.check(w, 200)
	body := w.Body.String()
	got := moderationBudgetAlertText(t, body)
	if got != moderationBudgetPublicText {
		t.Fatalf("public alert\n got: %s\nwant: %s", got, moderationBudgetPublicText)
	}
	if regexp.MustCompile(`[0-9]`).MatchString(got) || strings.Contains(got, "--max") {
		t.Fatalf("public alert carries counts or settings: %s", got)
	}
	mustNotContain(t, moderationBudgetBlock.FindString(body), "/admin/moderation")
	mustNotContain(t, body, `href="/admin/moderation"`)
	moderationBudgetRefuseIdentifiers(t, got, append(ids, vendor, "budget_public_vendor")...)
}

func TestAdminOverviewModerationBudgetAlertClears(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("budget_clear_vendor", "vendor")
	_, admin := e.user("budget_clear_admin", "admin")
	e.A.adminSocket = "/run/test-private-admin/http.sock"
	overview := func() string {
		w := ingressDo(e, true, "GET", "/admin", admin, nil)
		e.check(w, 200)
		return moderationBudgetAlertText(t, w.Body.String())
	}
	moderationBudgetExhaust(t, e, vendor)
	mustContain(t, overview(), "2 of 2 lifetime allocations used; 1 target(s) waiting for capture.")
	if err := ConfigureModerationLimits(t.Context(), e.DB, DefaultModerationLimits()); err != nil {
		t.Fatal(err)
	}
	// The pool has room again, but the refused target still waits after the recorded overflow.
	mustContain(t, overview(), "2 of 512 lifetime allocations used; 1 target(s) waiting for capture.")
	moderationPasses(t, moderationTestWorker(e), 3)
	if agInt(e, `SELECT backfill FROM moderation_capacity`) != 0 {
		t.Fatal("precondition: the worker did not capture the waiting target")
	}
	if got := overview(); got != "" {
		t.Fatalf("alert after the limits were raised and the target was captured: %s", got)
	}
}

func TestAdminOverviewModerationBudgetAlertAbsent(t *testing.T) {
	e := newTestApp(t)
	vendor, vendorSession := e.user("budget_absent_vendor", "vendor")
	_, moderator := e.user("budget_absent_moderator", "moderator")
	_, admin := e.user("budget_absent_admin", "admin")
	moderationEnable(t, e)
	// Healthy: the default pool with nothing allocated.
	if got := moderationBudgetAlertText(t, e.body("GET", "/admin", admin, nil, 200)); got != "" {
		t.Fatalf("alert on a healthy pool: %s", got)
	}
	moderationBudgetExhaust(t, e, vendor)
	for _, session := range []string{vendorSession, moderator} {
		w := e.do("GET", "/admin", session, nil)
		if w.Code == 200 || strings.Contains(w.Body.String(), "moderation-budget") || strings.Contains(w.Body.String(), "Moderation capture") {
			t.Fatalf("non-administrator reached the alert: %d", w.Code)
		}
	}
	// An overflow with nothing waiting is history, not an alert.
	if err := ConfigureModerationLimits(t.Context(), e.DB, DefaultModerationLimits()); err != nil {
		t.Fatal(err)
	}
	moderationPasses(t, moderationTestWorker(e), 3)
	if agInt(e, `SELECT backfill FROM moderation_capacity`) != 0 || agInt(e, `SELECT count(*) FROM moderation_overflow WHERE reason='retained_capacity' AND occurrences>0`) == 0 {
		t.Fatal("precondition: expected a recorded overflow with no waiting target")
	}
	if got := moderationBudgetAlertText(t, e.body("GET", "/admin", admin, nil, 200)); got != "" {
		t.Fatalf("alert with nothing waiting: %s", got)
	}
	// Capture switched off: nothing is captured, so the budget needs no attention even when exhausted.
	limits := DefaultModerationLimits()
	limits.MaxRecords = 3
	if err := ConfigureModerationLimits(t.Context(), e.DB, limits); err != nil {
		t.Fatal(err)
	}
	if got := moderationBudgetAlertText(t, e.body("GET", "/admin", admin, nil, 200)); got == "" {
		t.Fatal("precondition: an exhausted pool shows the alert")
	}
	if err := SetModerationEnabled(t.Context(), e.DB, false); err != nil {
		t.Fatal(err)
	}
	if got := moderationBudgetAlertText(t, e.body("GET", "/admin", admin, nil, 200)); got != "" {
		t.Fatalf("alert with capture off: %s", got)
	}
}

// The overview loader only reads: it never takes the capacity row lock admission and the worker serialise on.
func TestAdminOverviewModerationBudgetAlertPlainRead(t *testing.T) {
	lock := regexp.MustCompile(`(?i)\bFOR\s+(UPDATE|SHARE|NO\s+KEY\s+UPDATE|KEY\s+SHARE)\b|\bLOCK\s+TABLE\b|pg_advisory`)
	if lock.MatchString(moderationBudgetAlertSQL) {
		t.Fatalf("budget alert query takes a lock: %s", moderationBudgetAlertSQL)
	}
	src, err := os.ReadFile("internal/market/moderation_budget_alert.go") // TestMain runs the tests from the repository root
	if err != nil {
		t.Fatal(err)
	}
	if lock.Match(src) || strings.Count(string(src), "QueryRowContext(") != 1 || strings.Contains(string(src), "QueryContext(") ||
		strings.Contains(string(src), "ExecContext(") || strings.Contains(string(src), "BeginTx(") {
		t.Fatal("budget alert loader must be one plain read")
	}
}

// QA87-1: a parked marker (an unflagged payment, kept until it is flagged again) is not waiting for capture budget.
// It is not counted, and with a past overflow it never keeps the alert up once the limits are raised.
func TestAdminOverviewModerationBudgetAlertIgnoresParkedMarkers(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("budget_parked_vendor", "vendor")
	buyer, _ := e.user("budget_parked_buyer", "buyer")
	_, admin := e.user("budget_parked_admin", "admin")
	e.A.adminSocket = "/run/test-private-admin/http.sock"
	overview := func() string {
		w := ingressDo(e, true, "GET", "/admin", admin, nil)
		e.check(w, 200)
		return moderationBudgetAlertText(t, w.Body.String())
	}
	order := e.order(buyer, e.product(vendor, "physical"), "XMR", statePaid)
	moderationBudgetExhaust(t, e, vendor)
	payment := agStr(e, `INSERT INTO payments(order_id,currency,txid,idx,address,amount,flagged) VALUES($1,'XMR',$2,0,'fixture-only',1,true) RETURNING id::text`, order, randomToken())
	if agInt(e, `SELECT count(*) FROM moderation_backfill WHERE kind='payment_flag' AND target_id=$1 AND NOT parked`, payment) != 1 {
		t.Fatal("precondition: the refused payment flag left no active marker")
	}
	mustContain(t, overview(), "2 of 2 lifetime allocations used; 2 target(s) waiting for capture.")
	agExec(e, `UPDATE payments SET flagged=false WHERE id::text=$1`, payment)
	if err := ConfigureModerationLimits(t.Context(), e.DB, DefaultModerationLimits()); err != nil {
		t.Fatal(err)
	}
	moderationPasses(t, moderationTestWorker(e), 4)
	if agInt(e, `SELECT count(*) FROM moderation_backfill WHERE parked`) != 1 || agInt(e, `SELECT count(*) FROM moderation_backfill WHERE NOT parked`) != 0 {
		t.Fatal("precondition: expected only the unflagged payment's parked marker")
	}
	if got := overview(); got != "" {
		t.Fatalf("alert with only a parked marker after the limits were raised: %s", got)
	}
	// While the pool is low, the count still leaves the parked marker out.
	used := int64(agInt(e, `SELECT allocated_records FROM moderation_capacity`))
	limits := DefaultModerationLimits()
	limits.MaxRecords = used
	if err := ConfigureModerationLimits(t.Context(), e.DB, limits); err != nil {
		t.Fatal(err)
	}
	mustContain(t, overview(), strconv.FormatInt(used, 10)+" of "+strconv.FormatInt(used, 10)+" lifetime allocations used; 0 target(s) waiting for capture.")
}

// A-662(1): an old capacity overflow does not turn later per-account throttle markers into a "raise the limits"
// alert. Only an overflow recorded at or after the oldest active marker's first refusal counts.
func TestAdminOverviewModerationBudgetAlertStaleOverflow(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("budget_stale_vendor", "vendor")
	other, _ := e.user("budget_stale_other", "vendor")
	_, admin := e.user("budget_stale_admin", "admin")
	e.A.adminSocket = "/run/test-private-admin/http.sock"
	overview := func() string {
		w := ingressDo(e, true, "GET", "/admin", admin, nil)
		e.check(w, 200)
		return moderationBudgetAlertText(t, w.Body.String())
	}
	moderationBudgetExhaust(t, e, vendor)
	if err := ConfigureModerationLimits(t.Context(), e.DB, DefaultModerationLimits()); err != nil {
		t.Fatal(err)
	}
	moderationPasses(t, moderationTestWorker(e), 3)
	if agInt(e, `SELECT backfill FROM moderation_capacity`) != 0 {
		t.Fatal("precondition: the refused target was not captured")
	}
	// The overflow is history; the account then edits faster than its three active jobs allow.
	agExec(e, `UPDATE moderation_overflow SET last_seen=last_seen-interval '1 hour' WHERE occurrences>0`)
	// An even older overflow of another kind: the latest overflow is the one compared.
	agExec(e, `UPDATE moderation_overflow SET occurrences=1,last_seen=now()-interval '2 days' WHERE kind='upload' AND reason='retained_capacity'`)
	for range 4 {
		e.product(vendor, "digital")
	}
	if agInt(e, `SELECT count(*) FROM moderation_backfill WHERE NOT parked`) != 1 || agInt(e, `SELECT occurrences FROM moderation_overflow WHERE kind='listing' AND reason='actor_capacity'`) == 0 {
		t.Fatal("precondition: expected one throttle marker")
	}
	if got := overview(); got != "" {
		t.Fatalf("stale overflow turned a throttle marker into an alert: %s", got)
	}
	// A fresh retained_capacity refusal while markers wait alerts again, even after the limits are raised.
	used := int64(agInt(e, `SELECT allocated_records FROM moderation_capacity`))
	limits := DefaultModerationLimits()
	limits.MaxRecords = used
	if err := ConfigureModerationLimits(t.Context(), e.DB, limits); err != nil {
		t.Fatal(err)
	}
	e.product(other, "digital")
	if err := ConfigureModerationLimits(t.Context(), e.DB, DefaultModerationLimits()); err != nil {
		t.Fatal(err)
	}
	mustContain(t, overview(), strconv.FormatInt(used, 10)+" of 512 lifetime allocations used; 2 target(s) waiting for capture.")
}
