package market

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-545: the user directory on /admin/users (search, filters, newest first, paged) and each account's page
// (/admin/users?handle=), which shows stored data only and carries the existing account actions.

var directoryRow = regexp.MustCompile(`<a class="text-link" href="/admin/users\?handle=([^"&#]+)">`)

// directoryHandles lists the accounts a directory page links to, in order.
func directoryHandles(body string) []string {
	var handles []string
	for _, m := range directoryRow.FindAllStringSubmatch(body, -1) {
		handles = append(handles, m[1])
	}
	return handles
}

// userPageNotice follows a successful post's 303 the way a browser does: to its Location, with its notice cookie.
func userPageNotice(t *testing.T, e *testEnv, w *httptest.ResponseRecorder, session, location string) string {
	t.Helper()
	if w.Code != 303 || w.Header().Get("Location") != location {
		t.Fatalf("redirect: %d %q, want 303 %q: %s", w.Code, w.Header().Get("Location"), location, refusalText(w.Body.String()))
	}
	path, _, _ := strings.Cut(location, "#")
	return e.body("GET", path, session, nil, 200, &http.Cookie{Name: noticeCookie, Value: e.cookie(w, noticeCookie)})
}

func TestAdminUserDirectory(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("dir_admin", "admin")
	e.user("dir_admin2", "admin")
	_, moderator := e.user("dir_mod", "moderator")
	_, buyerSession := e.user("dir_x_y", "buyer")
	e.user("dirxzy", "buyer")
	betaID, _ := e.user("Dir_Beta", "vendor")
	agExec(e, "UPDATE users SET vendor_verified=true,vendor_verified_set_by=$2,vendor_verified_set_at=now() WHERE id=$1", betaID, adminID)
	suspID, _ := e.user("dir_susp", "buyer")
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", suspID)
	agExec(e, "INSERT INTO audit_events(user_id,action) VALUES($1,'Account suspended by administrator dir_admin; ended 0 session(s) and any pending sign-ins')", suspID)
	bannedID, _ := e.user("dir_banned", "buyer")
	reason := `<b>Spam & "fraud"</b> in every message`
	agExec(e, "UPDATE users SET suspended_at=now(),banned_at=now(),banned_by=$2,ban_reason=$3 WHERE id=$1", bannedID, adminID, reason)
	closingID, _ := e.user("dir_closing", "buyer")
	agExec(e, "UPDATE users SET deletion_requested_at=now() WHERE id=$1", closingID)
	bondID, _ := e.user("dir_bond", "buyer")
	agExec(e, "INSERT INTO vendor_applications(id,user_id,state,approval,bond_piconero) VALUES('dir-app-bond',$1,'awaiting_bond','manual',"+halfXMR+")", bondID)
	approvalID, _ := e.user("dir_approval", "buyer")
	agExec(e, "INSERT INTO vendor_applications(id,user_id,state,approval,bond_piconero) VALUES('dir-app-approval',$1,'awaiting_approval','manual',0)", approvalID)
	alphaID, _ := e.user("dir_alpha", "buyer")
	// Legacy account data is displayed as stored, including a missing key beside an enabled factor flag.
	statsID, _ := e.keylessUser("dir_stats", "vendor")
	// Creation order is the directory's order: the accounts above one second apart, oldest first, then 55 more.
	agExec(e, `UPDATE users u SET created=now()-interval '1 hour'+o.n*interval '1 second' FROM (SELECT id,row_number() OVER (ORDER BY created,handle) n FROM users) o WHERE o.id=u.id`)
	agExec(e, `INSERT INTO users(id,handle,password_hash,role,created)
		SELECT 'dir-page-'||g,'dir_page_'||lpad(g::text,2,'0'),'unused','buyer',now()+g*interval '1 second' FROM generate_series(1,55) g`)

	// Stats fixtures for dir_stats: listings, orders as vendor and as buyer, disputes both ways.
	mine := e.product(statsID, "physical")
	e.product(statsID, "digital")
	old := e.product(statsID, "physical")
	agExec(e, "UPDATE products SET archived=true,archived_at=now() WHERE id=$1", old)
	theirs := e.product(betaID, "physical")
	for _, s := range []string{stateCompleted, stateCompleted, stateShipped} {
		e.order(alphaID, mine, "XMR", s)
	}
	e.order(statsID, theirs, "XMR", stateCancelled)
	e.order(statsID, theirs, "XMR", stateDraft)
	dispute := func(buyer, product, state, opener, outcome string) {
		order := e.order(buyer, product, "XMR", state)
		agExec(e, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,'shipped','disputed',$2,'Dispute opened')", order, opener)
		status := "Open"
		if outcome != "" {
			status = "Resolved"
		}
		agExec(e, "INSERT INTO disputes(id,order_id,reason,status,outcome) VALUES($1,$2,'Fixture dispute reason text',$3,$4)", randomToken(), order, status, outcome)
	}
	dispute(alphaID, mine, stateDisputed, alphaID, "")          // against dir_stats, open
	dispute(alphaID, mine, stateResolved, alphaID, "refund")    // against dir_stats, refunded
	dispute(statsID, theirs, stateResolved, statsID, "release") // opened by dir_stats, released
	agExec(e, "UPDATE users SET last_authenticated_at='2001-02-03 04:05:06+00',totp_enabled=true,pgp_2fa=true WHERE id=$1", statsID)

	directory := func(query string) (string, []string) {
		t.Helper()
		body := e.body("GET", "/admin/users"+query, admin, nil, 200)
		return body, directoryHandles(body)
	}
	expect := func(query string, want ...string) {
		t.Helper()
		if _, got := directory(query); !slices.Equal(got, want) {
			t.Fatalf("%s: got %v, want %v", query, got, want)
		}
	}

	// Newest first, 50 a page, every account reachable; the old "Newest accounts" list is gone.
	first, rows := directory("")
	if len(rows) != 50 || rows[0] != "dir_page_55" || rows[49] != "dir_page_06" {
		t.Fatalf("first page: %v", rows)
	}
	mustContain(t, first, `id="user-directory"`, "Showing 1–50 of 68 accounts", `href="/admin/users?users_page=2"`, `<form class="user-directory-search" method="get" action="/admin/users"`)
	mustNotContain(t, first, "Newest accounts")
	_, rows = directory("?users_page=2")
	if len(rows) != 18 || rows[0] != "dir_page_05" || rows[5] != "dir_stats" || rows[17] != "dir_admin" {
		t.Fatalf("second page: %v", rows)
	}

	// Case-insensitive substring search, % and _ matched literally.
	expect("?find=BETA", "Dir_Beta")
	expect("?find=x_y", "dir_x_y")
	expect("?find=%25")
	expect("?find=dir%25beta")
	body, _ := directory("?find=" + url.QueryEscape(`"><script>`))
	mustContain(t, body, `value="&#34;&gt;&lt;script&gt;"`, "No account matches")
	mustNotContain(t, body, `"><script>`)
	// Filters, each alone and combined with the search; paging links keep them.
	expect("?role=moderator", "dir_mod")
	expect("?role=admin", "dir_admin2", "dir_admin")
	expect("?role=vendor", "dir_stats", "Dir_Beta")
	expect("?status=suspended", "dir_susp")
	expect("?status=banned", "dir_banned")
	expect("?status=closed", "dir_closing")
	expect("?vendor=awaiting_bond", "dir_bond")
	expect("?vendor=awaiting_approval", "dir_approval")
	expect("?vendor=approved", "dir_stats", "Dir_Beta", "dir_admin2", "dir_admin")
	expect("?vendor=verified", "Dir_Beta", "dir_admin2", "dir_admin")
	expect("?find=dir_&role=buyer&status=active&vendor=awaiting_bond", "dir_bond")
	if _, got := directory("?status=active&role=buyer"); slices.Contains(got, "dir_susp") || slices.Contains(got, "dir_banned") || slices.Contains(got, "dir_closing") || len(got) != 50 {
		t.Fatalf("active buyers: %v", got)
	}
	body, _ = directory("?status=active&role=buyer")
	mustContain(t, body, `href="/admin/users?role=buyer&amp;status=active&amp;users_page=2"`, `<option value="buyer" selected>`, `<option value="active" selected>`)
	expect("?role=nonsense", directoryAll(t, e)[:50]...) // an unknown filter is no filter
	// Rows say each account's role, status and vendor state; the ban reason is not in the list.
	body, _ = directory("?find=dir_b")
	list, _, _ := strings.Cut(body[strings.Index(body, `id="user-directory"`):], "</section>")
	mustContain(t, list, "Banned", "Applicant: waiting for bond")
	mustNotContain(t, list, "Spam")
	body, _ = directory("?find=Dir_Beta")
	mustContain(t, body, "Approved vendor, verified")

	// The account page: stored data only.
	detail := e.body("GET", "/admin/users?handle=dir_stats", admin, nil, 200)
	mustContain(t, detail,
		`<h2 id="user-account-heading">dir_stats</h2>`, `href="/admin/users">← All users</a>`,
		"<dt>Role</dt>\n<dd>vendor</dd>", "<dt>Status</dt>\n<dd>Active</dd>",
		"<dt>Authenticator (TOTP)</dt>\n<dd>Enabled</dd>", "<dt>PGP sign-in</dt>\n<dd>Enabled</dd>", "<dt>PGP key</dt>\n<dd>None saved</dd>",
		"<dt>Active listings</dt>\n<dd>2</dd>", "<dt>Archived listings</dt>\n<dd>1</dd>",
		`<tr><th scope="row">Completed</th><td>0</td><td>2</td></tr>`, `<tr><th scope="row">Shipped</th><td>0</td><td>1</td></tr>`,
		`<tr><th scope="row">Cancelled</th><td>1</td><td>0</td></tr>`, `<tr><th scope="row">Draft — unfunded</th><td>1</td><td>0</td></tr>`,
		`<tr><th scope="row">Disputed</th><td>0</td><td>1</td></tr>`, `<tr><th scope="row">Resolved</th><td>1</td><td>1</td></tr>`,
		`<tr><th scope="row">Total</th><td>3</td><td>5</td></tr>`,
		`<tr><th scope="row">Opened by this account</th><td>1</td><td>0</td><td>1</td><td>0</td></tr>`,
		`<tr><th scope="row">Opened against this account</th><td>2</td><td>1</td><td>0</td><td>1</td></tr>`,
		`href="/messages?to=dir_stats"`, "<dt>Retention</dt>\n<dd>Open account</dd>")
	// No sign-in time, IP address or visit record is shown, even though the last sign-in time is stored for retention.
	mustNotContain(t, detail, "2001-02-03", "IP address", "Last sign-in", "last seen")
	banned := e.body("GET", "/admin/users?handle=dir_banned", admin, nil, 200)
	mustContain(t, banned, "<dt>Status</dt>\n<dd>Banned</dd>", "by dir_admin", "&lt;b&gt;Spam &amp; &#34;fraud&#34;&lt;/b&gt; in every message", "A banned account cannot be messaged")
	mustNotContain(t, banned, reason, `href="/messages?to=dir_banned"`, `action="/admin/suspend"`)
	mustContain(t, adminForm(t, banned, "/admin/ban", ""), `name="action" value="unban"`, `name="from" value="user"`)
	mustContain(t, e.body("GET", "/admin/users?handle=dir_susp", admin, nil, 200), "<dt>Status</dt>\n<dd>Suspended</dd>", "by dir_admin")
	mustContain(t, e.body("GET", "/admin/users?handle=dir_closing", admin, nil, 200), "<dt>Retention</dt>\n<dd>Closure requested")
	bond := e.body("GET", "/admin/users?handle=dir_bond", admin, nil, 200)
	mustContain(t, bond, "Waiting for bond", "0.5 XMR")
	mustContain(t, adminForm(t, bond, "/admin/vendor-applications", ""), `name="id" value="dir-app-bond"`, `value="bond"`, `value="waive"`, `value="reject"`, `name="from" value="user"`)
	e.body("GET", "/admin/users?handle=no_such_account", admin, nil, 404)
	e.body("GET", "/admin/users?handle=DIR_STATS", admin, nil, 404) // exact handle, as every admin form

	// Neither the viewing administrator nor another administrator gets account actions here.
	for _, h := range []string{"dir_admin", "dir_admin2"} {
		page := e.body("GET", "/admin/users?handle="+h, admin, nil, 200)
		mustNotContain(t, page, `action="/admin/suspend"`, `action="/admin/ban"`, `action="/admin/reset-factors"`, `action="/admin/vendor-apply"`, `action="/admin/vendor-verify"`, `name="action" value="role"`)
		mustContain(t, page, "Administrator accounts are managed on the host")
	}
	mustNotContain(t, e.body("GET", "/admin/users?handle=dir_admin", admin, nil, 200), `href="/messages?to=dir_admin"`)
	mustContain(t, e.body("GET", "/admin/users?handle=dir_admin2", admin, nil, 200), `href="/messages?to=dir_admin2"`)

	// Every action posted from the account page returns to it with its notice, through the existing handlers.
	back := "/admin/users?handle=dir_alpha"
	alpha := e.body("GET", back, admin, nil, 200)
	for _, f := range []struct{ action, value string }{{"/admin/suspend", "suspend"}, {"/admin/ban", "ban"}, {"/admin", "role"}, {"/admin/vendor-apply", ""}} {
		mustContain(t, adminForm(t, alpha, f.action, f.value), `name="handle" value="dir_alpha"`, `name="from" value="user"`, `name="password" required`)
	}
	post := func(path string, f url.Values) *httptest.ResponseRecorder {
		f.Set("password", testPassword)
		f.Set("from", "user")
		return e.do("POST", path, admin, f)
	}
	page := userPageNotice(t, e, post("/admin/suspend", form("handle", "dir_alpha", "action", "suspend")), admin, back+"#suspend")
	mustContain(t, page, "<dt>Status</dt>\n<dd>Suspended</dd>", `<div class="alert" role="status">Account suspended.</div>`)
	page = userPageNotice(t, e, post("/admin/suspend", form("handle", "dir_alpha", "action", "restore")), admin, back+"#suspend")
	mustContain(t, page, "<dt>Status</dt>\n<dd>Active</dd>", "Account restored.")
	agExec(e, "UPDATE users SET totp_enabled=true WHERE id=$1", alphaID)
	page = userPageNotice(t, e, post("/admin/reset-factors", form("handle", "dir_alpha")), admin, back+"#reset-factors")
	mustContain(t, page, "Second factors reset.", "<dt>Authenticator (TOTP)</dt>\n<dd>Off</dd>")
	page = userPageNotice(t, e, post("/admin", form("handle", "dir_alpha", "action", "role", "role", "moderator")), admin, back)
	mustContain(t, page, "Account role saved.", "<dt>Role</dt>\n<dd>moderator</dd>")
	page = userPageNotice(t, e, post("/admin", form("handle", "dir_alpha", "action", "role", "role", "buyer")), admin, back)
	mustContain(t, page, "<dt>Role</dt>\n<dd>buyer</dd>")
	// Vendor: an application opened on the account's behalf under the current policy, then the bond, approval and mark.
	setVendorPolicy(e, halfXMR, "manual", "Send it to the address the staff gave you.")
	mustContain(t, adminForm(t, e.body("GET", back, admin, nil, 200), "/admin/vendor-apply", ""), `name="start" value="awaiting_bond"`, `name="start" value="awaiting_approval"`, "Waiting for the bond of 0.5 XMR")
	page = userPageNotice(t, e, post("/admin/vendor-apply", form("handle", "dir_alpha", "start", "awaiting_bond")), admin, back+"#vendors")
	mustContain(t, page, "Vendor application opened for this account", "Waiting for bond")
	app := latestVendorApp(e, alphaID)
	if app.State != "awaiting_bond" || app.Bond != 500000000000 || app.Approval != "manual" {
		t.Fatalf("application on the account's behalf: %+v", app)
	}
	if !e.auditExact(alphaID, "Vendor application opened on the account's behalf by administrator dir_admin: waiting for bond, bond 0.5 XMR, manual approval") ||
		!e.auditExact(adminID, "Opened a vendor application for dir_alpha: waiting for bond, bond 0.5 XMR, manual approval (confirmed with password)") ||
		agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator opened a vendor application for your account.%'", alphaID) != 1 {
		t.Fatal("vendor application on the account's behalf not audited or notified")
	}
	agExpect(e, "/admin/vendor-apply", admin, form("handle", "dir_alpha", "start", "awaiting_approval", "password", testPassword), 409, "dir_alpha already has an open vendor application")
	page = userPageNotice(t, e, post("/admin/vendor-applications", form("id", app.ID, "action", "bond")), admin, back+"#vendors")
	mustContain(t, page, "Vendor application updated.", "Waiting for administrator approval")
	page = userPageNotice(t, e, post("/admin/vendor-applications", form("id", app.ID, "action", "approve")), admin, back+"#vendors")
	mustContain(t, page, "<dt>Role</dt>\n<dd>vendor</dd>", "Approved")
	page = userPageNotice(t, e, post("/admin/vendor-verify", form("handle", "dir_alpha", "action", "verify")), admin, back+"#vendors")
	mustContain(t, page, "Verified vendor mark saved.", "<dt>Verified vendor</dt>\n<dd>Yes")
	page = userPageNotice(t, e, post("/admin/vendor-verify", form("handle", "dir_alpha", "action", "unverify")), admin, back+"#vendors")
	mustContain(t, page, "<dt>Verified vendor</dt>\n<dd>No</dd>")
	page = userPageNotice(t, e, post("/admin/ban", form("handle", "dir_alpha", "action", "ban", "reason", testBanReason)), admin, back+"#ban")
	mustContain(t, page, "Account banned.", "<dt>Status</dt>\n<dd>Banned</dd>")
	page = userPageNotice(t, e, post("/admin/ban", form("handle", "dir_alpha", "action", "unban", "reason", testLiftReason)), admin, back+"#ban")
	mustContain(t, page, "Ban lifted.", "<dt>Status</dt>\n<dd>Active</dd>")
	// A suspension recorded without an administrator's audit row (as retention closure sets one) names nobody: the
	// rows of the earlier suspension above belong to that one.
	agExec(e, "UPDATE users SET suspended_at=now()+interval '1 minute' WHERE id=$1", alphaID)
	page = e.body("GET", back, admin, nil, 200)
	mustContain(t, page, "<dt>Status</dt>\n<dd>Suspended</dd>")
	mustNotContain(t, page, "by dir_admin</dd>")
	agExec(e, "UPDATE users SET suspended_at=NULL WHERE id=$1", alphaID)
	// The forms elsewhere keep their own return pages.
	w := e.do("POST", "/admin/suspend", admin, form("handle", "dir_alpha", "action", "suspend", "password", testPassword))
	if w.Code != 303 || w.Header().Get("Location") != "/admin/users#suspend" {
		t.Fatalf("Users-section suspend: %d %q", w.Code, w.Header().Get("Location"))
	}
	// The application form refuses what the account's own form would: not a buyer, banned, closing; and
	// administrators, the caller included.
	for handle, msg := range map[string]string{"dir_banned": "dir_banned is banned", "dir_closing": "dir_closing is closed or closing",
		"dir_susp":  "dir_susp is suspended: restore the account first",
		"dir_admin": "You cannot open a vendor application for your own account", "dir_admin2": "already has selling access or cannot apply to sell"} {
		code := 409
		if handle == "dir_admin" {
			code = 400
		}
		agExpect(e, "/admin/vendor-apply", admin, form("handle", handle, "start", "awaiting_approval", "password", testPassword), code, msg)
	}
	agExpect(e, "/admin/vendor-apply", admin, form("handle", "no_such_account", "start", "awaiting_approval", "password", testPassword), 404, "No account has the handle")
	agExpect(e, "/admin/vendor-apply", moderator, form("handle", "dir_x_y", "start", "awaiting_approval", "password", testPassword), 403, "")
	// The suspended account's page offers no application form; the banned and closed ones offer no message link.
	mustNotContain(t, e.body("GET", "/admin/users?handle=dir_susp", admin, nil, 200), `action="/admin/vendor-apply"`)
	mustContain(t, e.body("GET", "/admin/users?handle=dir_susp", admin, nil, 200), "Restore suspended accounts first")
	if latestVendorApp(e, suspID).State != "" || latestVendorApp(e, closingID).State != "" {
		t.Fatal("a refused application was stored")
	}

	// Administrators only, and only over the private administration ingress when one is configured.
	for _, who := range []string{moderator, buyerSession} {
		w := e.do("GET", "/admin/users?handle=dir_stats", who, nil)
		if w.Code != 403 || strings.Contains(w.Body.String(), "Opened against this account") {
			t.Fatalf("non-administrator account page: %d", w.Code)
		}
		if w := e.do("GET", "/admin/users?find=dir", who, nil); w.Code != 403 || strings.Contains(w.Body.String(), "dir_stats") {
			t.Fatalf("non-administrator directory: %d", w.Code)
		}
	}
	e.A.adminSocket = "/run/market-admin/http.sock"
	for _, path := range []string{"/admin/users?handle=dir_stats", "/admin/users?find=dir"} {
		if w := e.do("GET", path, admin, nil); w.Code != 404 || strings.Contains(w.Body.String(), "dir_stats") {
			t.Fatalf("public ingress %s: %d", path, w.Code)
		}
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: "session", Value: admin})
		w := httptest.NewRecorder()
		e.A.AdminHandler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("private ingress %s: %d", path, w.Code)
		}
	}
	e.A.adminSocket = ""
}

// directoryAll is every account handle, newest first, as the unfiltered directory orders them.
func directoryAll(t *testing.T, e *testEnv) []string {
	t.Helper()
	rows, err := e.DB.Query("SELECT handle FROM users ORDER BY created DESC,id DESC")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		all = append(all, h)
	}
	return all
}

// The read-only preview shows the directory and an account page with sample data and no database.
func TestAdminUserDirectoryPreview(t *testing.T) {
	a, err := New(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string, want int) string {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Fatalf("preview %s: %d", path, w.Code)
		}
		return w.Body.String()
	}
	list := get("/admin/users", 200)
	mustContain(t, list, `id="user-directory"`, `href="/admin/users?handle=ghost_circuit"`)
	page := get("/admin/users?handle=ghost_circuit", 200)
	mustContain(t, page, `<h2 id="user-account-heading">ghost_circuit</h2>`, "Opened against this account", `href="/messages?to=ghost_circuit"`, "Preview only")
	get("/admin/users?handle=nobody_here", 404)
}

// Owner decision 2026-10-19 ("Admin picks"): an application opened from the account page starts where the
// administrator chooses, whatever the policy's approval mode, and is never approved on this path: waiting for the
// bond takes the policy's bond (refused while it sets none), waiting for approval asks for none; both record manual
// approval, so recording the bond later still leaves it waiting for approval.
func TestAdminVendorApplyStartState(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("start_admin", "admin")
	n := 0
	for _, policy := range []struct{ bond, approval string }{{halfXMR, "manual"}, {halfXMR, "automatic"}, {"0", "manual"}, {"0", "automatic"}} {
		setVendorPolicy(e, policy.bond, policy.approval, "")
		for _, start := range []string{"awaiting_bond", "awaiting_approval"} {
			n++
			handle := "start_buyer_" + strconv.Itoa(n)
			id, _ := e.user(handle, "buyer")
			name := policy.bond + "/" + policy.approval + "/" + start
			f := form("handle", handle, "start", start, "password", testPassword, "from", "user")
			if start == "awaiting_bond" && policy.bond == "0" {
				before := agInt(e, "SELECT count(*) FROM audit_events")
				agExpect(e, "/admin/vendor-apply", admin, f, 409, "The vendor policy sets no bond")
				if latestVendorApp(e, id).State != "" || agInt(e, "SELECT count(*) FROM audit_events") != before {
					t.Fatalf("%s: a refused application changed state", name)
				}
				continue
			}
			w := e.do("POST", "/admin/vendor-apply", admin, f)
			if w.Code != 303 || w.Header().Get("Location") != "/admin/users?handle="+handle+"#vendors" {
				t.Fatalf("%s: %d %q %s", name, w.Code, w.Header().Get("Location"), refusalText(w.Body.String()))
			}
			app := latestVendorApp(e, id)
			wantBond := int64(0)
			if start == "awaiting_bond" {
				wantBond = 500000000000
			}
			if app.State != start || app.Approval != "manual" || app.Bond != wantBond || agStr(e, "SELECT role FROM users WHERE id=$1", id) != "buyer" {
				t.Fatalf("%s: application %+v", name, app)
			}
			label := map[string]string{"awaiting_bond": "waiting for bond, bond 0.5 XMR", "awaiting_approval": "waiting for administrator approval, bond none"}[start]
			if !e.auditExact(id, "Vendor application opened on the account's behalf by administrator start_admin: "+label+", manual approval") ||
				!e.auditExact(adminID, "Opened a vendor application for "+handle+": "+label+", manual approval (confirmed with password)") ||
				agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator opened a vendor application for your account.%'", id) != 1 {
				t.Fatalf("%s: audit rows or notification missing", name)
			}
			if start == "awaiting_bond" { // even under automatic approval the bond leads to waiting for approval
				agExpect(e, "/admin/vendor-applications", admin, form("id", app.ID, "action", "bond", "password", testPassword), 303, "")
				if s := latestVendorApp(e, id).State; s != "awaiting_approval" || agStr(e, "SELECT role FROM users WHERE id=$1", id) != "buyer" {
					t.Fatalf("%s: after the bond: %s", name, s)
				}
			}
		}
	}
	e.user("start_other", "buyer")
	for _, start := range []string{"", "approved", "awaiting"} {
		agExpect(e, "/admin/vendor-apply", admin, form("handle", "start_other", "start", start, "password", testPassword), 400, "Choose whether the application starts")
	}
	// The form offers waiting for approval alone while the policy sets no bond.
	page := e.body("GET", "/admin/users?handle=start_other", admin, nil, 200)
	mustContain(t, adminForm(t, page, "/admin/vendor-apply", ""), `name="start" value="awaiting_approval"`)
	mustNotContain(t, adminForm(t, page, "/admin/vendor-apply", ""), `value="awaiting_bond"`)
}
