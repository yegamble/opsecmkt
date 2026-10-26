package market

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// A-542: /admin is an overview and every administration panel lives on exactly one section page /admin/<section>.
var adminSectionPaths = []string{"/admin", "/admin/users", "/admin/vendors", "/admin/payments", "/admin/security", "/admin/site", "/admin/transparency", "/admin/system"}

// adminPanelSection names the one page each known panel id renders on. A panel shown only in some configurations
// (dependency report, retention, Monero escrow) must still appear on no other page.
var adminPanelSection = map[string]string{
	"getting-started": "/admin", "admin-sections": "/admin",
	"user-directory": "/admin/users", "assign-role": "/admin/users", "reset-factors": "/admin/users", "suspend": "/admin/users", "ban": "/admin/users", "staff-roster": "/admin/users",
	"vendors": "/admin/vendors", "vendor-policy": "/admin/vendors", "vendor-queue": "/admin/vendors", "vendor-verified": "/admin/vendors",
	"payment-providers": "/admin/payments", "xmr-escrow-node": "/admin/payments", "escrow-release-days": "/admin/payments",
	"xmr-escrow-recovery": "/admin/payments", "payment-intake": "/admin/payments", "escrow-readiness": "/admin/payments",
	"access": "/admin/security", "sign-in-protection": "/admin/security", "paused-handles-heading": "/admin/security",
	"branding": "/admin/site", "staff-contact": "/admin/site",
	"transparency": "/admin/transparency", "operator-key-title": "/admin/transparency", "canary-admin-title": "/admin/transparency",
	"audit-export-title": "/admin/transparency", "staff-list": "/admin/transparency",
	"dependency-status": "/admin/system", "host-control": "/admin/system", "image-scanner": "/admin/system", "retention": "/admin/system",
	"recent-server-errors": "/admin/system", "audit-trail": "/admin/system",
}

var adminNavPattern = regexp.MustCompile(`(?s)<nav class="admin-nav" aria-label="Administration sections">(.*?)</nav>`)

// checkAdminSectionPages checks that each page in bodies (by path) has the section navigation with only its own link
// current, and that every known panel id renders on its own page only; required ids must render somewhere.
func checkAdminSectionPages(t *testing.T, bodies map[string]string, required []string) {
	t.Helper()
	for path, body := range bodies {
		m := adminNavPattern.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s has no section navigation", path)
		}
		nav := m[1]
		for _, other := range adminSectionPaths {
			if !strings.Contains(nav, `href="`+other+`"`) {
				t.Fatalf("%s navigation lacks a link to %s", path, other)
			}
		}
		if strings.Count(nav, `aria-current="page"`) != 1 || !strings.Contains(nav, `href="`+path+`" aria-current="page"`) {
			t.Fatalf("%s navigation does not mark exactly its own link current: %s", path, nav)
		}
	}
	for id, want := range adminPanelSection {
		var on []string
		for path, body := range bodies {
			if strings.Contains(body, `id="`+id+`"`) {
				on = append(on, path)
			}
		}
		if len(on) > 1 || len(on) == 1 && on[0] != want {
			t.Fatalf("panel %q renders on %v, want only %s", id, on, want)
		}
		if len(on) == 0 && slices.Contains(required, id) {
			t.Fatalf("panel %q renders on no administration page", id)
		}
	}
}

// The read-only preview renders every section with its sample panels and the same navigation (no database).
func TestAdminSectionsPreview(t *testing.T) {
	a, err := New(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{}
	for _, path := range adminSectionPaths {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("preview %s: %d %s", path, w.Code, w.Body)
		}
		bodies[path] = w.Body.String()
	}
	checkAdminSectionPages(t, bodies, []string{"getting-started", "admin-sections", "user-directory", "assign-role", "reset-factors", "suspend", "ban", "staff-roster",
		"vendors", "vendor-policy", "vendor-queue", "vendor-verified", "payment-providers", "payment-intake", "escrow-readiness", "access", "sign-in-protection", "paused-handles-heading",
		"branding", "staff-contact", "transparency", "operator-key-title", "canary-admin-title", "audit-export-title", "staff-list",
		"host-control", "image-scanner", "recent-server-errors", "audit-trail"})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/admin/unknown", nil))
	if w.Code != 404 {
		t.Fatalf("unknown admin section: %d", w.Code)
	}
	// No section address is also an action or raw route, so a GET and a form post never share a path's meaning.
	for _, path := range adminSectionPaths[1:] {
		_, action := actions[path]
		_, raw := raws[path]
		if action || raw {
			t.Fatalf("admin section %s collides with a registered action or raw route", path)
		}
	}
}

func TestAdminSections(t *testing.T) {
	e, _ := uiRetentionEnv(t) // retention on, so its panel renders too
	_, admin := e.user("sections_admin", "admin")
	_, buyer := e.user("sections_buyer", "buyer")
	_, vendor := e.user("sections_vendor", "vendor")
	_, moderator := e.user("sections_moderator", "moderator")
	bodies := map[string]string{}
	for _, path := range adminSectionPaths {
		bodies[path] = e.body("GET", path, admin, nil, 200)
	}
	checkAdminSectionPages(t, bodies, []string{"getting-started", "admin-sections", "user-directory", "assign-role", "reset-factors", "suspend", "ban", "staff-roster",
		"vendors", "vendor-policy", "vendor-queue", "vendor-verified", "payment-providers", "payment-intake", "escrow-readiness", "access", "sign-in-protection", "paused-handles-heading",
		"branding", "staff-contact", "transparency", "operator-key-title", "canary-admin-title", "audit-export-title", "staff-list",
		"dependency-status", "host-control", "image-scanner", "retention", "recent-server-errors", "audit-trail"})
	overview := bodies["/admin"]
	mustContain(t, overview, "Get your marketplace ready", `href="/admin/payments#payment-providers"`, `href="/admin/users#assign-role"`, `aria-label="Staff attention counts"`)
	mustNotContain(t, withoutSiteHeader(overview), `action="/admin/`, `method="post"`, "The CAPTCHA is on without a staff contact") // the header keeps its sign-out (A-548)
	// A warning inside a section is flagged beside its link on the overview: no signed dependency report here, and
	// the CAPTCHA on with no staff contact.
	mustContain(t, overview, "Review required: service isolation or dependency status needs attention.")
	agExec(e, "UPDATE settings SET value='true' WHERE key='captcha_required'")
	mustContain(t, e.body("GET", "/admin", admin, nil, 200), "<strong>The CAPTCHA is on without a staff contact that works without an account.</strong>")
	agExec(e, "UPDATE settings SET value='false' WHERE key='captcha_required'")
	mustNotContain(t, overview, "open vendor application(s)")
	setVendorPolicy(e, "0", "manual", "")
	_, applicant := e.user("sections_applicant", "buyer")
	e.check(e.do("POST", "/account/vendor-application", applicant, url.Values{"action": {"apply"}}), 303)
	mustContain(t, e.body("GET", "/admin", admin, nil, 200), "<strong>1 open vendor application(s) await a decision or bond.</strong>")
	// Every section refuses a buyer, vendor or moderator exactly as /admin does, and sends a visitor to sign in.
	refusal := map[string]string{}
	for _, who := range []string{buyer, vendor, moderator} {
		refusal[who] = e.body("GET", "/admin", who, nil, 403)
	}
	anon := e.do("GET", "/admin", "", nil)
	if anon.Code != 303 || anon.Header().Get("Location") != "/login?next=%2Fadmin" {
		t.Fatalf("anonymous /admin: %d %v", anon.Code, anon.Header())
	}
	for _, path := range adminSectionPaths[1:] {
		for _, who := range []string{buyer, vendor, moderator} {
			if body := e.body("GET", path, who, nil, 403); !strings.Contains(body, "You do not have access to this page") || strings.Contains(body, `class="admin-nav"`) {
				t.Fatalf("%s refusal differs from /admin's: %s", path, body)
			}
		}
		w := e.do("GET", path, "", nil)
		if w.Code != 303 || w.Header().Get("Location") != "/login?next="+url.QueryEscape(path) {
			t.Fatalf("anonymous %s: %d %v", path, w.Code, w.Header())
		}
	}
	for who, body := range refusal {
		if !strings.Contains(body, "You do not have access to this page") {
			t.Fatalf("/admin refusal for %s changed: %s", who, body)
		}
	}
	// A sign-in started from a section returns there.
	login := e.body("GET", "/login?next=%2Fadmin%2Fpayments", "", nil, 200)
	mustContain(t, login, `name="next" value="/admin/payments"`)
	// A refused admin post links back to the section it came from.
	r := httptest.NewRequest("POST", "/admin/suspend", strings.NewReader(url.Values{"csrf": {e.A.csrf(admin)}, formNonceField: {formNonceFor("/admin/suspend")}, "action": {"bogus"}, "handle": {"x"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Referer", "http://example.com/admin/users")
	r.Host = "example.com"
	r.AddCookie(&http.Cookie{Name: "session", Value: admin})
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `href="/admin/users"`) {
		t.Fatalf("refused admin post back link: %d %s", w.Code, w.Body)
	}
}

// adminRedirectsCoveredElsewhere are the admin actions whose success needs a fixture of its own (a host agent, Monero
// escrow, an uploadable logo, a signed canary, an order or listing); the named test asserts each one's Location.
var adminRedirectsCoveredElsewhere = map[string]string{
	"/admin/moderation/budget":      "TestModerationBudgetRaiseConfirmedAudited: /admin/moderation",
	"/admin/moderation/review":      "TestModerationAdminQueueAuthorizationAndReview: /admin/moderation",
	"/admin/moderation/offline":     "TestModerationOfflineReviewNoLoginReplayOrStaleAcceptance: /admin/moderation/offline",
	"/admin/reboot":                 "TestHostRebootCommitsAuditBeforeOneDispatchAndNeverReplays: /admin/system#host-control",
	"/admin/escrow-release-days":    "TestXMREscrowOnlyReleaseDaysAndDispatch: /admin/payments#escrow-release-days",
	"/admin/escrow-xmr-rebroadcast": "runRebroadcastReceiptContract (form_nonce_escrow_rebroadcast_test.go): /admin/payments#xmr-escrow-recovery",
	"/admin/escrow-xmr-destroyed":   "form_nonce_xmr_destroyed_test.go: /admin/payments#xmr-escrow-recovery",
	"/admin/escrow-xmr-rerequest":   "escrow_xmr_rerequest tests: the order page's settlement, where its form is",
	"/admin/escrow-xmr-retry-build": "escrow_xmr_rerequest tests: the order page's settlement, where its form is",
	"/admin/branding/authorize":     "branding_test.go: /admin/branding/upload, the upload hand-off",
	"/admin/uploads/authorize":      "staff_upload_test.go (staffAuthorize): /admin/uploads/upload, the upload hand-off",
	"/admin/canary":                 "form_nonce_admin_settings_test.go: /canary, where the statement is published",
	"/admin/review":                 "review tests: the product or vendor page the form is on",
	"/admin/listing":                "inventory tests: the listing edit page the form is on",

	"/admin/escrow-xmr-settled-by-hand": "TestXMREscrowSettledByHand: /moderator, because the order page may no longer be readable once its alert clears",
	"/admin/ban-orders":                 "TestBanOrdersRefundsUnpaidAndUnshipped: the banned account's page (/admin/users?handle=…#ban-orders), the only page holding its form",
}

// A-542: every admin post that succeeds returns to the section holding its form, with its notice's fragment, and a
// form re-rendered with an error is that section.
func TestAdminActionRedirectsToSection(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	_, admin := e.user("redirect_admin", "admin")
	e.user("redirect_role", "buyer")
	factorsID, _ := e.user("redirect_factors", "buyer")
	agExec(e, "UPDATE users SET totp_enabled=true WHERE id=$1", factorsID)
	e.user("redirect_suspend", "buyer")
	e.user("redirect_ban", "buyer")
	vendorID, _ := e.user("redirect_vendor", "vendor")
	e.user("redirect_revoke", "vendor")
	e.user("redirect_revoke_user", "vendor")
	closingID, _ := e.user("redirect_closing", "buyer")
	order := e.order(closingID, e.product(vendorID, "physical"), "XMR", "shipped")
	agExec(e, `INSERT INTO retention_escrow_exceptions(order_id,reason,first_seen,last_checked,review_due) VALUES($1,'dispute_open',now(),now(),now())`, order)
	agExec(e, `INSERT INTO retention_account_exceptions(user_id,reason,first_seen,last_checked,review_due) VALUES($1,'open_escrow',now(),now(),now())`, closingID)
	_, operatorPub := canaryTestKey(t, "redirect_operator")
	// A-544: an open application (manual approval, no bond) to decide, and a vendor to mark verified.
	setVendorPolicy(e, "0", "manual", "")
	applicantID, applicant := e.user("redirect_applicant", "buyer")
	e.check(e.do("POST", "/account/vendor-application", applicant, url.Values{"action": {"apply"}}), 303)
	application := latestVendorApp(e, applicantID).ID
	hold := url.Values{"scope": {"account"}, "record_id": {closingID}, "authority": {"Specific preservation obligation"}, "reason": {"Necessary case records"},
		"readers": {"Assigned case handlers"}, "release_condition": {"Written confirmation that case ended"}, "confirm_hold": {"yes"}}
	cases := []struct {
		path string
		form url.Values
		want string
	}{
		{"/admin", url.Values{"action": {"role"}, "handle": {"redirect_role"}, "role": {"vendor"}}, "/admin/users"},
		{"/admin", url.Values{"action": {"settings"}, "site_name": {"Redirect Market"}}, "/admin/site"},
		{"/admin/reset-factors", url.Values{"handle": {"redirect_factors"}}, "/admin/users#reset-factors"},
		{"/admin/suspend", url.Values{"action": {"suspend"}, "handle": {"redirect_suspend"}}, "/admin/users#suspend"},
		{"/admin/ban", url.Values{"action": {"ban"}, "handle": {"redirect_ban"}, "reason": {"Redirect check reason."}}, "/admin/users#ban"},
		{"/admin/payment-intake", url.Values{"currency": {"XMR"}, "enabled": {"false"}}, "/admin/payments#payment-intake"},
		{"/admin/access", url.Values{"site_access": {"public"}, "registrations": {"closed"}}, "/admin/security#access"},
		{"/admin/pgp-access", url.Values{"allow_login_without_pgp": {"true"}}, "/admin/security#pgp-access"},
		{"/admin/captcha", url.Values{"captcha_required": {"true"}}, "/admin/security"},
		{"/admin/staff-contact", url.Values{"staff_contact": {"security@example.test"}}, "/admin/site#staff-contact"},
		{"/admin/xmpp", url.Values{"xmpp_sender": {"market@example.test"}, "xmpp_host": {"xmpp.example.test"}, "xmpp_port": {"5222"}, "xmpp_tls_mode": {"starttls"}}, "/admin/site#xmpp-outgoing"},
		{"/admin/branding/reset", url.Values{}, "/admin/site#branding"},
		{"/admin/staff-list", url.Values{"staff_list_public": {"true"}}, "/admin/transparency#staff-list"},
		{"/admin/operator-key", url.Values{"operator_key": {operatorPub}}, "/admin/transparency#transparency"},
		{"/admin/vendor-applications", url.Values{"id": {application}, "action": {"approve"}}, "/admin/vendors#vendors"},
		{"/admin/vendor-verify", url.Values{"action": {"verify"}, "handle": {"redirect_vendor"}}, "/admin/vendors#vendors"},
		// MOD72-6 listing review (listing_review.go).
		{"/admin/listing-review", url.Values{"listing_review_mode": {"hold"}}, "/admin/vendors#listing-review"},
		{"/admin/vendor-auto-approve", url.Values{"action": {"on"}, "handle": {"redirect_vendor"}}, "/admin/vendors#vendors"},
		{"/admin/vendor-revoke", url.Values{"handle": {"redirect_revoke"}}, "/admin/vendors#vendors"},
		// A-545: posted from an account's page in the user directory (from=user), every account form returns there.
		{"/admin", url.Values{"action": {"role"}, "handle": {"redirect_role"}, "role": {"buyer"}, "from": {"user"}}, "/admin/users?handle=redirect_role"},
		{"/admin/vendor-apply", url.Values{"handle": {"redirect_role"}, "start": {"awaiting_approval"}}, "/admin/users?handle=redirect_role#vendors"},
		{"/admin/vendor-revoke", url.Values{"handle": {"redirect_revoke_user"}, "from": {"user"}}, "/admin/users?handle=redirect_revoke_user#vendors"},
		{"/admin/suspend", url.Values{"action": {"restore"}, "handle": {"redirect_suspend"}, "from": {"user"}}, "/admin/users?handle=redirect_suspend#suspend"},
		{"/admin/vendor-policy", url.Values{"vendor_bond_xmr": {"0.1"}, "vendor_approval": {"manual"}}, "/admin/vendors#vendors"},
		{"/admin/retention/hold", hold, "/admin/system#retention"},
		{"/admin/retention/review", url.Values{"order_id": {order}}, "/admin/system#retention"},
		{"/admin/retention/account-review", url.Values{"user_id": {closingID}}, "/admin/system#retention"},
	}
	tested := map[string]bool{}
	for _, tc := range cases {
		form := maps.Clone(tc.form)
		form.Set("password", testPassword)
		w := e.do("POST", tc.path, admin, form)
		if w.Code != 303 || w.Header().Get("Location") != tc.want {
			t.Fatalf("%s %v: %d %q, want 303 %q; body=%s", tc.path, tc.form, w.Code, w.Header().Get("Location"), tc.want, w.Body)
		}
		tested[tc.path] = true
	}
	holdID := agStr(e, `SELECT id FROM retention_holds WHERE record_id=$1`, closingID)
	w := e.do("POST", "/admin/retention/release", admin, url.Values{"hold_id": {holdID}, "confirm_release": {"yes"}, "password": {testPassword}})
	if w.Code != 303 || w.Header().Get("Location") != "/admin/system#retention" {
		t.Fatalf("retention release: %d %q", w.Code, w.Header().Get("Location"))
	}
	tested["/admin/retention/release"] = true
	for path := range actions {
		if strings.HasPrefix(path, "/admin") && !tested[path] && adminRedirectsCoveredElsewhere[path] == "" {
			t.Errorf("admin action %s has no redirect check", path)
		}
	}
	for _, tc := range []struct {
		path string
		form url.Values
	}{
		{"/admin", url.Values{"action": {"role"}, "handle": {"nobody_here"}, "role": {"vendor"}}},
		{"/admin/suspend", url.Values{"action": {"suspend"}, "handle": {"nobody_here"}}},
		{"/admin/ban", url.Values{"action": {"ban"}, "handle": {"nobody_here"}, "reason": {"Redirect check reason."}}},
		{"/admin/reset-factors", url.Values{"handle": {"nobody_here"}}},
	} {
		body := e.body("POST", tc.path, admin, tc.form, 404)
		mustContain(t, body, `href="/admin/users" aria-current="page"`, `value="nobody_here"`, `aria-invalid="true"`)
	}
}

// With Monero escrow on (and its daemon reporting not synced, so the node alert renders) the escrow-only panels
// render on /admin/payments alone, and the overview flags Payments (A-542, QA542-3).
func TestXMREscrowAdminPanelsOnlyOnPaymentsSection(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("xsections_"+randomToken()[:6], "admin")
	x.fake.edit(func(f *fakeXMREscrow) { f.unsynced = true })
	x.mustPoll()
	bodies := map[string]string{}
	for _, path := range adminSectionPaths {
		bodies[path] = x.page(path, admin)
	}
	checkAdminSectionPages(t, bodies, []string{"xmr-escrow-node", "xmr-escrow-recovery", "escrow-release-days", "payment-intake", "vendors"})
	mustContain(t, bodies["/admin/payments"], "Monero escrow daemon not synced: daemon not synchronized.")
	mustContain(t, bodies["/admin"], "<strong>Monero escrow needs attention.</strong>")
}
