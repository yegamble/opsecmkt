package market

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// staffOnlyActionPaths is every registered action whose Roles are a non-empty subset of {moderator, admin} (A-706).
// TestStaffActionsRequireEnrolledFactor derives the same list from the registry, so a new staff-only route fails
// here until it is reviewed and added.
var staffOnlyActionPaths = []string{
	"/account/staff-list",
	"/admin", "/admin/access", "/admin/ban", "/admin/ban-hold-refund-days", "/admin/ban-orders",
	"/admin/branding/authorize", "/admin/branding/reset", "/admin/canary", "/admin/captcha",
	"/admin/escrow-release-days", "/admin/escrow-xmr-destroyed", "/admin/escrow-xmr-hold-refund",
	"/admin/escrow-xmr-rebroadcast", "/admin/escrow-xmr-rerequest", "/admin/escrow-xmr-retry-build",
	"/admin/escrow-xmr-settled-by-hand", "/admin/listing", "/admin/listing-review", "/admin/moderation/budget",
	"/admin/moderation/review", "/admin/operator-key", "/admin/payment-intake", "/admin/pgp-access", "/admin/reboot",
	"/admin/reset-factors", "/admin/retention/account-review", "/admin/retention/hold", "/admin/retention/release",
	"/admin/retention/review", "/admin/review", "/admin/staff-contact", "/admin/staff-list", "/admin/suspend",
	"/admin/uploads/authorize", "/admin/vendor-applications", "/admin/vendor-apply", "/admin/vendor-auto-approve",
	"/admin/vendor-policy", "/admin/vendor-revoke", "/admin/vendor-verify", "/admin/xmpp",
	"/images/remove", "/moderator/listing-review", "/moderator/payment-review", "/moderator/xmr-collision",
	"/resolve", "/tickets/assign", "/tickets/escalate",
}

// factorlessStaff is a staff account with neither TOTP nor PGP sign-in on, as /setup, -bootstrap-admin and -set-role
// leave a new one.
func (e *testEnv) factorlessStaff(handle, role string) (id, session string) {
	e.t.Helper()
	id, session = e.user(handle, role)
	if _, err := e.DB.Exec("UPDATE users SET totp_enabled=false,totp_secret='',pgp_2fa=false WHERE id=$1", id); err != nil {
		e.t.Fatal(err)
	}
	return id, session
}

// candidateFactor gives an account about to be appointed to staff the fixture's PGP sign-in: only an account with a
// second factor is made a moderator or administrator (A-706, SEC706-2).
func (e *testEnv) candidateFactor(handle string) {
	e.t.Helper()
	agExec(e, "UPDATE users SET pgp_2fa=true WHERE handle=$1", handle)
}

// dbFingerprint digests every row of every table in the test schema except the session table (which records the
// request's own session use), so a refused post can be shown to have changed nothing.
func (e *testEnv) dbFingerprint() string {
	e.t.Helper()
	rows, err := e.DB.Query(`SELECT quote_ident(table_name) FROM information_schema.tables
		WHERE table_schema=current_schema() AND table_type='BASE TABLE' AND table_name<>'sessions' ORDER BY table_name`)
	if err != nil {
		e.t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			e.t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	var b strings.Builder
	for _, table := range tables {
		var sum string
		if err = e.DB.QueryRow("SELECT md5(coalesce(string_agg(x::text, '|' ORDER BY x::text), '')) FROM " + table + " x").Scan(&sum); err != nil {
			e.t.Fatal(table, err)
		}
		b.WriteString(table + "=" + sum + "\n")
	}
	return b.String()
}

func mustBeStaffFactorRefusal(t *testing.T, path string, w interface {
	Result() *http.Response
}, body string) {
	t.Helper()
	if code := w.Result().StatusCode; code != http.StatusForbidden || !strings.Contains(body, staffFactorRequiredMessage) ||
		!strings.Contains(body, `href="/totp"`) || !strings.Contains(body, `href="/pgp"`) {
		t.Fatalf("%s: status %d, want 403 with the second-factor refusal and links to /totp and /pgp; body:\n%s", path, code, body)
	}
}

// A-706: a staff member without TOTP or PGP sign-in is refused every staff-only action (403, nothing changed, links
// to enrolment), on the A-222 locked re-read for shared-transaction actions and before an OwnTx action runs; the
// enrolment, password, sign-out and shared routes keep working, staff pages still render, and after enrolment the
// same posts are no longer refused.
func TestStaffActionsRequireEnrolledFactor(t *testing.T) {
	e := newTestApp(t)
	var derived []string
	for path, spec := range actions {
		if staffOnlyRoles(spec.Roles) && (spec.Off == nil || !spec.Off(e.A)) {
			derived = append(derived, path)
		}
	}
	slices.Sort(derived)
	var want []string
	for _, path := range staffOnlyActionPaths {
		if spec, ok := actions[path]; ok && (spec.Off == nil || !spec.Off(e.A)) {
			want = append(want, path)
		}
	}
	slices.Sort(want)
	if !slices.Equal(derived, want) {
		t.Fatalf("staff-only actions changed; review A-706's gate for them and update staffOnlyActionPaths:\nderived %v\nlisted  %v", derived, want)
	}
	if len(derived) < 30 {
		t.Fatalf("only %d staff-only actions are on: %v", len(derived), derived)
	}
	// Administrators act on the private ingress (A-570), which also serves the private moderation routes; on the
	// public listener their posts stay the plain 404 and never reach this refusal.
	e.A.adminSocket = "/run/test-admin/http.sock"
	adminID, admin := e.factorlessStaff("factorless_admin", "admin")
	modID, moderator := e.factorlessStaff("factorless_mod", "moderator")
	post := func(actor, session, path string, form url.Values) *httptest.ResponseRecorder {
		if actor == "admin" || privateAdminRoute(path) {
			return privateHostRequest(e, "POST", path, session, form, true)
		}
		return e.do("POST", path, session, form)
	}
	if w := e.do("POST", "/admin/captcha", admin, url.Values{"password": {testPassword}}); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), staffFactorRequiredMessage) {
		t.Fatalf("public listener: status %d; want the unchanged 404 without the refusal", w.Code)
	}
	for _, path := range derived {
		actors := []struct{ name, session string }{{"admin", admin}}
		if slices.Contains(actions[path].Roles, "moderator") {
			actors = append(actors, struct{ name, session string }{"moderator", moderator})
		}
		for _, actor := range actors {
			if actor.name == "moderator" && moderationPrivateRoute(path) {
				continue
			}
			before := e.dbFingerprint()
			w := post(actor.name, actor.session, path, url.Values{"password": {testPassword}, "action": {"settings"}, "site_name": {"Changed by a password alone"}})
			mustBeStaffFactorRefusal(t, actor.name+" "+path, w, w.Body.String())
			if after := e.dbFingerprint(); after != before {
				t.Fatalf("%s %s: a refused post changed the database", actor.name, path)
			}
		}
	}

	// Staff pages still render, and enrolment, the password change and sign-out are not staff-only actions.
	for _, page := range []string{"/admin", "/admin/users", "/admin/security", "/moderator", "/totp", "/pgp", "/account"} {
		e.check(privateHostRequest(e, "GET", page, admin, nil, false), 200)
	}
	e.check(e.do("GET", "/moderator", moderator, nil), 200)
	e.check(e.do("POST", "/totp/enroll", moderator, nil), 303)
	code, _ := e.totpCodeFor(modID, 0)
	e.check(e.do("POST", "/totp/activate", moderator, url.Values{"code": {code}, "password": {testPassword}}), 303)

	// After enrolment the same posts are not refused for the missing factor: the moderator just turned TOTP on
	// through the real form; the administrator turns PGP sign-in on.
	if _, err := e.DB.Exec("UPDATE users SET pgp_verified_at=now(),pgp_2fa=true WHERE id=$1", adminID); err != nil {
		t.Fatal(err)
	}
	for _, path := range derived {
		actors := []struct{ name, session string }{{"admin", admin}}
		if slices.Contains(actions[path].Roles, "moderator") && !moderationPrivateRoute(path) {
			actors = append(actors, struct{ name, session string }{"moderator", moderator})
		}
		for _, actor := range actors {
			w := post(actor.name, actor.session, path, url.Values{"password": {testPassword}})
			if strings.Contains(w.Body.String(), staffFactorRequiredMessage) {
				t.Fatalf("%s %s: still refused for a missing second factor after enrolment (status %d)", actor.name, path, w.Code)
			}
		}
	}
	// And a complete staff change now succeeds: the /setup probe (a confirmed role change), and a shared-transaction
	// staff action.
	targetID, _ := e.user("factor_target", "buyer")
	e.check(post("admin", admin, "/admin", url.Values{"action": {"role"}, "handle": {"factor_target"}, "role": {"vendor"}, "password": {testPassword}}), 303)
	var role string
	if err := e.DB.QueryRow("SELECT role FROM users WHERE id=$1", targetID).Scan(&role); err != nil || role != "vendor" {
		t.Fatalf("role change after enrolment: role %q err %v", role, err)
	}
	e.check(e.do("POST", "/account/staff-list", moderator, url.Values{"hide": {"yes"}}), 303)
	var hidden bool
	if err := e.DB.QueryRow("SELECT staff_hidden FROM users WHERE id=$1", modID).Scan(&hidden); err != nil || !hidden {
		t.Fatalf("staff-list choice after enrolment: hidden %v err %v", hidden, err)
	}
}

// A-706: the password change, sign-out and shared routes stay open to a factor-less staff member.
func TestFactorlessStaffKeepAccountAndSharedRoutes(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.factorlessStaff("factorless_pw", "admin")
	w := e.do("POST", "/account/password", admin, url.Values{"password": {testPassword}, "new_password": {"another-long-test-password"}, "new_password_confirm": {"another-long-test-password"}})
	if strings.Contains(w.Body.String(), staffFactorRequiredMessage) || w.Code != http.StatusSeeOther {
		t.Fatalf("password change refused: %d %s", w.Code, w.Body.String())
	}
	_, mod := e.factorlessStaff("factorless_out", "moderator")
	if w = e.do("POST", "/logout", mod, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("sign-out: %d", w.Code)
	}
}

// A-706: confirmedTx repeats the check on the locked actor row, so a factor turned off after post()'s early check
// (a host reset, or the account's own /totp/disable) still refuses the change.
func TestConfirmedTxRechecksStaffFactorUnderLock(t *testing.T) {
	e := newTestApp(t)
	id, session := e.factorlessStaff("factor_lock", "admin")
	r, _ := http.NewRequestWithContext(context.Background(), "POST", "/admin/captcha", nil)
	ran := false
	c := &actionCtx{A: e.A, R: r, User: e.loadUser(id), Token: session, Form: url.Values{}, StaffOnly: true}
	_, err := confirmedTx(c, false, func(*actionCtx) (actionResult, error) { ran = true; return actionResult{}, nil })
	var he *httpError
	if !errors.As(err, &he) || he.Code != http.StatusForbidden || he.Msg != staffFactorRequiredMessage || ran {
		t.Fatalf("confirmedTx: err %v ran %v", err, ran)
	}
	c.StaffOnly = false // the same account's own non-staff confirmed change is not gated
	if _, err = confirmedTx(c, false, func(*actionCtx) (actionResult, error) { ran = true; return actionResult{}, nil }); err != nil || !ran {
		t.Fatalf("non-staff confirmedTx: err %v ran %v", err, ran)
	}
}

// A-706: the session's flag only refuses early; the locked actor row decides. A factor turned off after the request's
// session was loaded (the account's own /totp/disable, a reset) refuses the change on the A-222 re-read of a
// shared-transaction action and in confirmedTx for an OwnTx action, and nothing is changed or recorded.
func TestStaffFactorRecheckedOnLockedActorRow(t *testing.T) {
	e := newTestApp(t)
	modID, moderator := e.user("factor_race_mod", "moderator")
	agExec(e, "UPDATE users SET staff_hidden=false WHERE id=$1", modID)
	w := besideAccountChange(t, e, modID, []string{"UPDATE users SET pgp_2fa=false WHERE id=$1"}, func() *httptest.ResponseRecorder {
		return e.do("POST", "/account/staff-list", moderator, url.Values{"hide": {"yes"}})
	})
	mustBeStaffFactorRefusal(t, "/account/staff-list", w, w.Body.String())
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND staff_hidden", modID) != 0 || len(e.audits(modID)) != 0 {
		t.Fatal("a refused staff-list choice changed or recorded something")
	}
	adminID, admin := e.user("factor_race_admin", "admin")
	w = besideAccountChange(t, e, adminID, []string{"UPDATE users SET pgp_2fa=false WHERE id=$1"}, func() *httptest.ResponseRecorder {
		return e.do("POST", "/admin/captcha", admin, url.Values{"password": {testPassword}, "captcha_required": {"true"}})
	})
	mustBeStaffFactorRefusal(t, "/admin/captcha", w, w.Body.String())
	if agStr(e, "SELECT value FROM settings WHERE key='captcha_required'") != "false" || len(e.audits(adminID)) != 0 {
		t.Fatal("a refused CAPTCHA change changed or recorded something")
	}
}

// A-706: after the operator's host factor reset (-reset-admin-factors) the administrator is refused staff actions,
// re-enrols through the ordinary /totp forms and can act again.
func TestResetAdminFactorsThenReenrolUnblocksStaffActions(t *testing.T) {
	e := newTestApp(t)
	id, admin := e.factorlessStaff("reenrol_admin", "admin")
	e.check(e.do("POST", "/totp/enroll", admin, nil), 303)
	code, _ := e.totpCodeFor(id, 0)
	e.check(e.do("POST", "/totp/activate", admin, url.Values{"code": {code}, "password": {testPassword}}), 303)
	if _, err := ResetAdminFactors(context.Background(), e.DB, "reenrol_admin"); err != nil {
		t.Fatal(err)
	}
	// The reset ended every session: sign in again with the password alone (no factor is left), as the operator
	// procedure says.
	w := e.do("POST", "/login", randomToken(), url.Values{"handle": {"reenrol_admin"}, "password": {testPassword}})
	e.check(w, 303)
	admin = e.session(w)
	// The account keeps its PGP key; enabling a first authenticator then needs a decrypted challenge in this
	// browser (the fixture stands in for it).
	e.pgpSessionKey(admin)
	w = e.do("POST", "/admin/captcha", admin, url.Values{"password": {testPassword}, "captcha_required": {"true"}})
	mustBeStaffFactorRefusal(t, "/admin/captcha", w, w.Body.String())
	var captcha string
	if err := e.DB.QueryRow("SELECT value FROM settings WHERE key='captcha_required'").Scan(&captcha); err != nil || captcha != "false" {
		t.Fatalf("captcha after refusal %q err %v", captcha, err)
	}
	e.check(e.do("POST", "/totp/enroll", admin, nil), 303)
	code, step := e.totpCodeFor(id, 0)
	e.check(e.do("POST", "/totp/activate", admin, url.Values{"code": {code}, "password": {testPassword}}), 303)
	// The activation spent this step's code; the confirmation takes the next one.
	code, _ = e.totpCodeFor(id, step-totpStep(time.Now())+1)
	w = e.do("POST", "/admin/captcha", admin, url.Values{"password": {testPassword}, "code": {code}, "captcha_required": {"true"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("staff action after re-enrolment: %d %s", w.Code, w.Body.String())
	}
	if err := e.DB.QueryRow("SELECT value FROM settings WHERE key='captcha_required'").Scan(&captcha); err != nil || captcha != "true" {
		t.Fatalf("captcha after re-enrolment %q err %v", captcha, err)
	}
}

// A-706: the overview and the staff roster show factor-less staff to administrators.
func TestAdminListsFactorlessStaff(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.factorlessStaff("roster_factorless_admin", "admin")
	e.factorlessStaff("roster_factorless_mod", "moderator")
	totpID, _ := e.factorlessStaff("roster_totp_mod", "moderator")
	agEnableTOTP(e, totpID)
	suspendedID, _ := e.factorlessStaff("roster_suspended_mod", "moderator")
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", suspendedID)

	overview := e.body("GET", "/admin", admin, nil, 200)
	mustContain(t, overview, "Required before any staff action:", `href="/pgp"`,
		"2 staff account(s) have no second factor and cannot take staff actions.")
	users := e.body("GET", "/admin/users", admin, nil, 200)
	start := strings.Index(users, `aria-labelledby="staff-roster"`)
	if start < 0 {
		t.Fatal("missing roster")
	}
	roster := users[start:]
	roster = roster[:strings.Index(roster, "</section>")]
	mustContain(t, roster, `<th scope="col">Second factor</th>`, "2 active staff account(s) have no second factor",
		"<td>roster_factorless_mod</td><td>moderator</td><td>Active</td><td>Can receive encrypted messages</td><td>None: staff actions refused</td>",
		"<td>roster_totp_mod</td><td>moderator</td><td>Active</td><td>Can receive encrypted messages</td><td>TOTP</td>")

	agExec(e, "UPDATE users SET pgp_2fa=true WHERE id=$1", adminID)
	overview = e.body("GET", "/admin", admin, nil, 200)
	mustContain(t, overview, "a second factor is on.", "1 staff account(s) have no second factor")
	if strings.Contains(overview, "Required before any staff action:") {
		t.Fatal("overview still asks an enrolled administrator to enrol")
	}
}
