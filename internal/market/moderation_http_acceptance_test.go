package market

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/moderation"
)

// followModerationNotice fetches a confirmed post's 303 with its one-shot notice cookie, as the browser does.
func followModerationNotice(t *testing.T, e *testEnv, session string, posted *httptest.ResponseRecorder) string {
	t.Helper()
	cookie := noticeSetCookie(posted)
	if cookie == nil {
		t.Fatal("the post set no notice")
	}
	path, _, _ := strings.Cut(posted.Header().Get("Location"), "#")
	page := ingressDo(e, true, "GET", path, session, nil, &http.Cookie{Name: noticeCookie, Value: cookie.Value})
	e.check(page, 200)
	return page.Body.String()
}

// A-723: a recorded review confirms itself in the title and the queue, keeps the filter and page it was submitted
// from and lands at the queue, including when the same rendered form replays its stored result.
func TestReviewKeepsFilterAndConfirms(t *testing.T) {
	e, _, admin, _, id := moderationAdminFixture(t, 0, false)
	const confirmed = "Review recorded; marketplace state unchanged."

	// The rendered form carries the view it was shown in, as validated values.
	view := privateHostRequest(e, "GET", "/admin/moderation?kind=listing&disposition=ESCALATE_ADMIN", admin, nil, true)
	e.check(view, 200)
	mustContain(t, view.Body.String(), `<input type="hidden" name="kind" value="listing">`, `<input type="hidden" name="disposition" value="ESCALATE_ADMIN">`)
	if strings.Contains(view.Body.String(), `<input type="hidden" name="page"`) {
		t.Fatal("the first page needs no return page")
	}
	// Every form on a later page or another view carries it, the budget form included.
	expiring := privateHostRequest(e, "GET", "/admin/moderation?state=reviewed&page=3", admin, nil, true)
	e.check(expiring, 200)
	mustContain(t, expiring.Body.String(), `<input type="hidden" name="state" value="reviewed">`, `<input type="hidden" name="page" value="3">`)

	// Refused return values record nothing and are never echoed into a redirect.
	for _, bad := range [][2]string{{"state", "open"}, {"kind", "image"}, {"disposition", "AUTO"}, {"page", "0"}, {"page", "10001"}, {"page", "abc"}, {"page", "//evil.invalid"}} {
		form := moderationReviewForm(id, "hold")
		form.Set(bad[0], bad[1])
		e.check(privateHostRequest(e, "POST", moderationReviewPath, admin, form, true), 400)
	}
	if n := agInt(e, `SELECT count(*) FROM moderation_reviews`); n != 0 {
		t.Fatalf("a refused return value recorded %d reviews", n)
	}

	form := moderationReviewForm(id, "hold")
	form.Set("kind", "listing")
	form.Set("disposition", "ESCALATE_ADMIN")
	form.Set("page", "2")
	const want = "/admin/moderation?disposition=ESCALATE_ADMIN&kind=listing&page=2#moderation-queue"
	first := privateHostRequest(e, "POST", moderationReviewPath, admin, form, true)
	e.check(first, 303)
	if loc := first.Header().Get("Location"); loc != want {
		t.Fatalf("redirect %q, want %q", loc, want)
	}
	page := followModerationNotice(t, e, admin, first)
	mustContain(t, page, "<title>"+confirmed+" – Moderation review")
	_, queue, ok := strings.Cut(page, `<h2 id="moderation-queue">Review queue</h2>`)
	if !ok || !strings.HasPrefix(queue, `<div class="alert" role="status">`+confirmed+`</div>`) {
		t.Fatalf("the queue section does not open with the confirmation:\n%s", page)
	}
	// The view it returned to: a later page of a filter with no rows left there still says what it is showing.
	mustContain(t, page, `<option value="listing" selected>`, `<option value="ESCALATE_ADMIN" selected>`, "Page 2",
		"No proposals match Kind: Listing, Proposed: Escalate to an administrator on page 2.")
	if ids := moderationQueueIDs(t, page); len(ids) != 0 {
		t.Fatalf("the reviewed proposal is still listed: %v", ids)
	}
	mustContain(t, page, `href="/admin/moderation?disposition=ESCALATE_ADMIN&amp;kind=listing&amp;page=1">Previous page</a>`)

	// The same rendered form replays its stored result, and a plain reload confirms nothing.
	replay := privateHostRequest(e, "POST", moderationReviewPath, admin, form, true)
	e.check(replay, 303)
	if loc := replay.Header().Get("Location"); loc != want {
		t.Fatalf("replay redirect %q, want %q", loc, want)
	}
	if n := agInt(e, `SELECT count(*) FROM moderation_reviews WHERE suggestion_id=$1`, id); n != 1 {
		t.Fatalf("replay wrote %d reviews", n)
	}
	reload := privateHostRequest(e, "GET", "/admin/moderation?disposition=ESCALATE_ADMIN&kind=listing&page=2", admin, nil, true)
	e.check(reload, 200)
	mustNotContain(t, reload.Body.String(), confirmed)
}

// A-723: a review from the default view returns to the default view.
func TestModerationReviewDefaultViewReturnsToQueue(t *testing.T) {
	e, _, admin, _, id := moderationAdminFixture(t, 0, false)
	posted := privateHostRequest(e, "POST", moderationReviewPath, admin, moderationReviewForm(id, "dismiss"), true)
	e.check(posted, 303)
	if loc := posted.Header().Get("Location"); loc != "/admin/moderation?page=1#moderation-queue" {
		t.Fatalf("redirect %q", loc)
	}
}

// A-723: the budget raise confirms itself where its form is, opened, and keeps the queue view.
func TestModerationBudgetRaiseKeepsFilterAndConfirms(t *testing.T) {
	e, _, admin, _, _ := moderationAdminFixture(t, 0, false)
	before := moderationBudgetLimits(e)
	bad := moderationBudgetForm("8192", "512")
	bad.Set("state", "open")
	refused := privateHostRequest(e, "POST", moderationBudgetPath, admin, bad, true)
	e.check(refused, 400)
	mustContain(t, refused.Body.String(), "Nothing was changed")
	if got := moderationBudgetLimits(e); got != before {
		t.Fatalf("a refused return value changed the budget: %v", got)
	}
	form := moderationBudgetForm("8192", "512")
	form.Set("state", "reviewed")
	form.Set("kind", "listing")
	posted := privateHostRequest(e, "POST", moderationBudgetPath, admin, form, true)
	e.check(posted, 303)
	if loc, want := posted.Header().Get("Location"), "/admin/moderation?kind=listing&page=1&state=reviewed#moderation-budget-panel"; loc != want {
		t.Fatalf("redirect %q, want %q", loc, want)
	}
	page := followModerationNotice(t, e, admin, posted)
	mustContain(t, page, "<title>Moderation budget raised.", `<details class="moderation-panel" open>`, `<option value="listing" selected>`)
	_, panel, ok := strings.Cut(page, `id="moderation-budget-panel"`)
	if !ok || !strings.Contains(panel, `<div class="alert" role="status">Moderation budget raised.`) {
		t.Fatalf("the budget panel does not confirm the raise:\n%s", page)
	}
}

var moderationTabStop = regexp.MustCompile(`<(?:a|button|select|textarea|summary)\b|<input\b[^>]*>`)

// moderationTabStops counts keyboard stops in a fragment of a page (hidden inputs are not stops).
func moderationTabStops(fragment string) int {
	n := 0
	for _, m := range moderationTabStop.FindAllString(fragment, -1) {
		if !strings.Contains(m, `type="hidden"`) {
			n++
		}
	}
	return n
}

// A-723: the queue comes first in main, at most one stop in, and the capacity and budget panels stay behind details
// until the same rule as the overview's capture alert (A-648) says they need the administrator.
func TestModerationQueueWithinTwoTabStops(t *testing.T) {
	e, _, admin, _, _ := moderationAdminFixture(t, 0, false)
	stopsBeforeQueue := func(body string) int {
		t.Helper()
		_, main, ok := strings.Cut(body, `<main id="main"`)
		before, _, found := strings.Cut(main, `id="moderation-queue-section"`)
		if !ok || !found {
			t.Fatalf("no queue section in main:\n%s", body)
		}
		return moderationTabStops(before)
	}
	calm := privateHostRequest(e, "GET", "/admin/moderation", admin, nil, true)
	e.check(calm, 200)
	body := calm.Body.String()
	if n := stopsBeforeQueue(body); n != 0 {
		t.Fatalf("%d tab stops before the queue", n)
	}
	mustContain(t, body, `<details class="moderation-panel">`, `id="moderation-capacity-panel"`, `id="moderation-budget-panel"`)
	mustNotContain(t, body, `<details class="moderation-panel" open>`, "moderation-capture-attention")

	// Spend the lifetime pool: the overview's alert applies, so both panels open and the queue is still within two
	// stops (the pointer to them, then its first link).
	agExec(e, `UPDATE moderation_capacity SET allocated_records=max_records WHERE singleton`)
	alert := privateHostRequest(e, "GET", "/admin/moderation", admin, nil, true)
	e.check(alert, 200)
	body = alert.Body.String()
	if n := stopsBeforeQueue(body); n != 1 {
		t.Fatalf("%d tab stops before the queue while an alert applies", n)
	}
	mustContain(t, body, `id="moderation-capture-attention"`, `href="#moderation-capacity-panel"`)
	if n := strings.Count(body, `<details class="moderation-panel" open>`); n != 2 {
		t.Fatalf("%d panels open while an alert applies", n)
	}
	// The overview agrees.
	mustContain(t, ingressDo(e, true, "GET", "/admin", admin, nil).Body.String(), `id="moderation-budget"`)

	// Capture off: the alert no longer applies.
	if err := SetModerationEnabled(t.Context(), e.DB, false); err != nil {
		t.Fatal(err)
	}
	off := privateHostRequest(e, "GET", "/admin/moderation", admin, nil, true)
	e.check(off, 200)
	mustNotContain(t, off.Body.String(), `<details class="moderation-panel" open>`, "moderation-capture-attention")
}

// moderationOfferedValues is the values a filter select offers.
func moderationOfferedValues(t *testing.T, body, name string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `<select name="`+name+`">`)
	if !ok {
		t.Fatalf("no %s filter:\n%s", name, body)
	}
	sel, _, _ := strings.Cut(rest, "</select>")
	var values []string
	for _, m := range regexp.MustCompile(`<option value="([^"]+)"`).FindAllStringSubmatch(sel, -1) {
		values = append(values, m[1])
	}
	return values
}

// A-723: a filter offers only values that can match the view, every offered value matches the rows it returns as
// they read, and an empty filtered view names the filter and links to clear it.
func TestModerationFilterMatchesWhatItShows(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("filter_match_admin", "admin")
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	vendor, _ := e.user("filter_match_vendor", "vendor")
	e.titledListing(vendor, "Plain lamp past its deadline")
	expiring := moderationTestWorker(e)
	expiring.Engine.Config.HumanSLA = time.Nanosecond
	if n, err := expiring.Pass(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("expired pass: n=%d err=%v", n, err)
	}
	var id string
	if err := e.DB.QueryRow(`SELECT id FROM moderation_suggestions`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	e.A.adminSocket = "/run/test-private-admin/http.sock"
	get := func(path string) string {
		t.Helper()
		w := privateHostRequest(e, "GET", path, admin, nil, true)
		e.check(w, 200)
		return w.Body.String()
	}

	unreviewed := []string{"ESCALATE_ADMIN", "QUEUE_HUMAN", "HOLD"}
	for path, want := range map[string][]string{
		"/admin/moderation":                unreviewed,
		"/admin/moderation?state=expired":  unreviewed,
		"/admin/moderation?state=reviewed": {"ESCALATE_ADMIN", "QUEUE_HUMAN", "HOLD", "AUTO_APPLY"},
	} {
		if got := moderationOfferedValues(t, get(path), "disposition"); !slices.Equal(got, want) {
			t.Fatalf("%s offers %v, want %v", path, got, want)
		}
	}

	// The expired row reads as what it proposed, and that is what the filter matches; the filter says so.
	past := get("/admin/moderation?state=expired")
	mustContain(t, past, "<dt>Proposed</dt><dd>Human review; deadline passed, on hold</dd>", `<label>Proposed<select name="disposition">`)
	mustNotContain(t, past, `<label>Disposition`)
	matched := get("/admin/moderation?state=expired&disposition=QUEUE_HUMAN")
	if ids := moderationQueueIDs(t, matched); !slices.Equal(ids, []string{id}) {
		t.Fatalf("Proposed: Human review lists %v", ids)
	}
	mustContain(t, matched, `href="/admin/moderation?state=expired">Clear the filter</a>`)

	// An empty filtered view names the filter, keeps the view when it clears, and does not say the view is empty.
	empty := get("/admin/moderation?state=expired&disposition=HOLD&kind=dispute")
	mustContain(t, empty, "No proposals match Kind: Dispute, Proposed: Hold.", `href="/admin/moderation?state=expired">Clear the filter</a>`)
	mustNotContain(t, empty, "No proposals in this view")
	// A value no longer offered in this view (an old link) still says what it asked for.
	mustContain(t, get("/admin/moderation?disposition=AUTO_APPLY"), "No proposals match Proposed: Automatic classification.", `href="/admin/moderation">Clear the filter</a>`)

	// The default view with nothing ahead points to what is past its deadline rather than reading as nothing to do.
	idle := get("/admin/moderation")
	mustContain(t, idle, "No proposals with a deadline ahead.", `href="/admin/moderation?state=expired">Review 1 past its deadline</a>`)
	mustNotContain(t, idle, "Clear the filter")
	// An unfiltered view with nothing at all says so.
	mustContain(t, get("/admin/moderation?state=reviewed"), "No proposals in this view.")
}

var moderationAriaLabel = regexp.MustCompile(`aria-label="([^"]*)"`)
var moderationMarkup = regexp.MustCompile(`<[^>]*>`)

// moderationVisibleText is a fragment without its tags and attributes.
func moderationVisibleText(fragment string) string {
	return moderationMarkup.ReplaceAllString(fragment, "|")
}

// A-723: every control of a queue item names that item, the item is a box of key-value rows, and staff read labels
// rather than stored enum values.
func TestModerationItemControlsNameTheirItem(t *testing.T) {
	e, adminID, admin, _, first := moderationAdminFixture(t, 0, false)
	agEnableTOTP(e, adminID)
	vendor, _ := e.user("item_names_vendor", "vendor")
	e.titledListing(vendor, "Plain lamp with a deadline ahead")
	if n, err := moderationTestWorker(e).Pass(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("second pass: n=%d err=%v", n, err)
	}
	w := privateHostRequest(e, "GET", "/admin/moderation", admin, nil, true)
	e.check(w, 200)
	body := w.Body.String()
	ids := moderationQueueIDs(t, body)
	if len(ids) != 2 || ids[0] != first {
		t.Fatalf("queue lists %v", ids)
	}
	_, list, _ := strings.Cut(body, `<article class="moderation-item">`)
	list, _, _ = strings.Cut(list, `aria-label="Moderation queue pages"`)
	items := strings.Split(list, `<article class="moderation-item">`)
	if len(items) != 2 {
		t.Fatalf("%d items", len(items))
	}
	seen := map[string]bool{}
	for i, item := range items {
		name := "Listing proposal " + shortID(ids[i])
		mustContain(t, item, "<h3>"+name+"</h3>", `<dl class="key-values">`, "<dt>Risk</dt>",
			`aria-label="Review action for `+name+`"`, `aria-label="Current password for `+name+`"`,
			`aria-label="Authenticator code for `+name+`"`, `aria-label="Record review for `+name+`"`,
			`aria-label="Open current source evidence for `+name+`"`, `Evidence digests<span class="sr-only"> for `+name+`</span>`)
		for _, m := range moderationAriaLabel.FindAllStringSubmatch(item, -1) {
			if !strings.HasSuffix(m[1], name) || seen[m[1]] {
				t.Fatalf("control name %q does not identify its item uniquely", m[1])
			}
			seen[m[1]] = true
		}
		mustNotContain(t, moderationVisibleText(item), "ESCALATE_ADMIN", "QUEUE_HUMAN", "payment_flag", "AUTO_APPLY")
	}
	mustContain(t, items[0], "<dt>Proposed</dt><dd>Escalate to an administrator</dd>")
	mustContain(t, items[1], "<dt>Proposed</dt><dd>Human review</dd>")
	// The filter's choices read as labels; their values stay the stored ones.
	mustContain(t, body, `<option value="payment_flag">Payment flag</option>`, `<option value="ESCALATE_ADMIN">Escalate to an administrator</option>`)
	mustNotContain(t, moderationVisibleText(body), "payment_flag", "QUEUE_HUMAN")
}

// A-723: every disposition and kind the schema allows has a label, and an unknown value never shows raw.
func TestModerationLabelsCoverAllowedValues(t *testing.T) {
	for _, kind := range moderationQueueKinds {
		if got := moderationKindLabel(kind); got == kind || got == "" || strings.Contains(got, "_") {
			t.Fatalf("kind %q reads %q", kind, got)
		}
	}
	for _, disposition := range []moderation.Disposition{moderation.AutoApply, moderation.QueueHuman, moderation.EscalateAdmin, moderation.Hold} {
		if got := moderationDispositionLabel(string(disposition)); got == string(disposition) || strings.Contains(got, "_") {
			t.Fatalf("disposition %q reads %q", disposition, got)
		}
	}
	if got := moderationKindLabel("something_new"); strings.Contains(got, "something_new") {
		t.Fatalf("an unknown kind shows raw: %q", got)
	}
	if got := moderationDispositionLabel("SOMETHING_NEW"); got != "Hold" {
		t.Fatalf("an unknown disposition reads %q", got)
	}
}

func TestModerationDeterministicScamAppearsInPrivateReviewQueue(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("qa_scam_established_vendor", "vendor")
	_, admin := e.user("qa_scam_queue_admin", "admin")
	agExec(e, `UPDATE users SET created=now()-interval '90 days' WHERE id=$1`, vendor)
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	product := e.titledListing(vendor, "Guaranteed profit if you send your seed")
	if count, err := moderationTestWorker(e).Pass(t.Context(), 1); count != 1 || err != nil {
		t.Fatalf("pass count=%d err=%v", count, err)
	}
	var id, disposition string
	if err := e.DB.QueryRow(`SELECT id,disposition FROM moderation_suggestions WHERE target_id=$1 AND reason_codes ? 'SCAM_TEMPLATE'`, product).Scan(&id, &disposition); err != nil {
		t.Fatal(err)
	}
	if disposition != "QUEUE_HUMAN" {
		t.Fatalf("scam finding hidden with disposition %s", disposition)
	}
	if count := agInt(e, `SELECT unreviewed FROM moderation_capacity WHERE singleton`); count != 1 {
		t.Fatalf("unreviewed=%d", count)
	}
	e.A.adminSocket = "/run/qa-private-admin/http.sock"
	page := privateHostRequest(e, "GET", "/admin/moderation", admin, nil, true)
	e.check(page, 200)
	mustContain(t, page.Body.String(), id, "SCAM_TEMPLATE", "QUEUE_HUMAN")
	e.check(e.do("GET", "/admin/moderation", admin, nil), 404)
	if count := agInt(e, `SELECT count(*) FROM products WHERE id=$1 AND NOT archived`, product); count != 1 {
		t.Fatal("classification changed listing state")
	}
}

func TestModerationQueueOverflowPreservesListingAndDisputeHTTPFlows(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	e := x.testEnv
	vendor, vendorSession := x.vendor, x.vendorSess
	adminID, admin := e.user("qa_overflow_admin", "admin")
	originalProduct := x.physical(vendor)
	order, buyerSession := x.paidXMRPhysical(originalProduct)
	limits := DefaultModerationLimits()
	limits.MaxPending = 1
	if err := ConfigureModerationLimits(t.Context(), e.DB, limits); err != nil {
		t.Fatal(err)
	}
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	e.product(vendor, "digital")
	listingForm := url.Values{"title": {"Listing while moderation capture is full"}, "description": {"A normal product description"}, "category": {"Hardware"}, "region": {"Worldwide"}, "kind": {"physical"}, "price_xmr": {"0.5"}, "stock": {"5"}}
	created := e.do("POST", "/listings", vendorSession, listingForm)
	e.check(created, 303)
	location, err := url.Parse(created.Header().Get("Location"))
	if err != nil || location.Path != "/listing-edit" {
		t.Fatalf("listing response Location=%q err=%v", created.Header().Get("Location"), err)
	}
	target := location.Query().Get("id")
	public := e.do("GET", "/product?id="+target, "", nil)
	e.check(public, 200)
	mustContain(t, public.Body.String(), listingForm.Get("title"))
	e.check(e.do("POST", "/disputes", buyerSession, url.Values{"order_id": {order}, "reason": {"The supplied parcel differs from its listing description."}}), 303)
	if count := agInt(e, `SELECT count(*) FROM disputes WHERE order_id=$1 AND status='Open'`, order); count != 1 {
		t.Fatal("overflow lost the dispute")
	}
	if state := agStr(e, `SELECT state FROM orders WHERE id=$1`, order); state != stateDisputed {
		t.Fatalf("dispute state=%s", state)
	}
	if count := agInt(e, `SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Dispute opened on order %'`, adminID); count != 1 {
		t.Fatalf("normal staff notices=%d", count)
	}
	if count := agInt(e, `SELECT pending FROM moderation_capacity WHERE singleton`); count != 1 {
		t.Fatalf("pending=%d exceeds capacity", count)
	}
	if count := agInt(e, `SELECT count(*) FROM moderation_suggestions`); count != 0 {
		t.Fatal("overflow invented a clean moderation decision")
	}
	for _, kind := range []string{"listing", "dispute"} {
		if count := agInt(e, `SELECT occurrences FROM moderation_overflow WHERE kind=$1 AND reason='pending_capacity'`, kind); count != 1 {
			t.Fatalf("%s overflow notices=%d", kind, count)
		}
	}
	e.A.adminSocket = "/run/qa-private-admin/http.sock"
	warning := privateHostRequest(e, "GET", "/admin/moderation", admin, nil, true)
	e.check(warning, 200)
	mustContain(t, warning.Body.String(), "pending_capacity", "Marketplace writes succeeded")
}
