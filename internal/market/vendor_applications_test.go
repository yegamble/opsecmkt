package market

import (
	"html"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// A-544: vendor onboarding. Sign-up (or /account later) chooses a vendor account; the application follows the
// administrator's policy (bond in XMR, 0 = none; approval manual or automatic). The bond is a non-refundable fee
// that an administrator records as received; no wallet is involved.

const halfXMR = "500000000000" // 0.5 XMR in piconero

func setVendorPolicy(e *testEnv, bond, approval, instructions string) {
	e.t.Helper()
	for key, value := range map[string]string{settingVendorBond: bond, settingVendorApproval: approval, settingVendorInstructions: instructions} {
		agExec(e, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	}
}

type vendorAppRow struct {
	ID, State, Approval, How string
	Bond                     int64
}

// latestVendorApp reads userID's newest application ("" State when there is none).
func latestVendorApp(e *testEnv, userID string) vendorAppRow {
	e.t.Helper()
	var r vendorAppRow
	err := e.DB.QueryRow(`SELECT id,state,approval,coalesce(bond_received_how,''),bond_piconero FROM vendor_applications
		WHERE user_id=$1 ORDER BY created DESC,id DESC LIMIT 1`, userID).Scan(&r.ID, &r.State, &r.Approval, &r.How, &r.Bond)
	if err != nil && err.Error() != "sql: no rows in result set" {
		e.t.Fatal(err)
	}
	return r
}

func userRole(e *testEnv, id string) string {
	e.t.Helper()
	return agStr(e, "SELECT role FROM users WHERE id=$1", id)
}

// vendorSignUp registers handle as a vendor account and returns its id and session.
func vendorSignUp(e *testEnv, handle string) (string, string) {
	e.t.Helper()
	w := e.do("POST", "/register", randomToken(), url.Values{"handle": {handle}, "password": {testPassword}, "account_type": {"vendor"}})
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/account#vendor-application" {
		e.t.Fatalf("vendor sign-up landed on %q", loc)
	}
	return agStr(e, "SELECT id FROM users WHERE handle=$1", handle), e.session(w)
}

func adminVendorPost(e *testEnv, admin, path string, form url.Values, want int) {
	e.t.Helper()
	form = cloneValues(form)
	if !form.Has("password") {
		form.Set("password", testPassword)
	}
	w := e.do("POST", path, admin, form)
	if w.Code != want {
		e.t.Fatalf("POST %s %v: status=%d want %d body=%s", path, form, w.Code, want, w.Body.String())
	}
	if want == 303 && w.Header().Get("Location") != "/admin/vendors#vendors" {
		e.t.Fatalf("POST %s redirected to %q, want /admin/vendors#vendors", path, w.Header().Get("Location"))
	}
}

func hasAudit(e *testEnv, user, fragment string) bool {
	e.t.Helper()
	return slices.ContainsFunc(e.audits(user), func(a string) bool { return strings.Contains(a, fragment) })
}

func hasNote(e *testEnv, user, fragment string) bool {
	e.t.Helper()
	return slices.ContainsFunc(e.notes(user), func(n string) bool { return strings.Contains(n, fragment) })
}

func TestVendorApplicationModes(t *testing.T) {
	for _, mode := range []struct {
		name, bond, approval string
	}{
		{"bond+approval", halfXMR, "manual"},
		{"bond+automatic", halfXMR, "automatic"},
		{"nobond+approval", "0", "manual"},
		{"nobond+automatic", "0", "automatic"},
	} {
		for _, entry := range []string{"signup", "account"} {
			t.Run(mode.name+"/"+entry, func(t *testing.T) {
				e := newTestApp(t)
				admin := e.setup()
				adminID := agStr(e, "SELECT id FROM users WHERE handle='admin_user'")
				setVendorPolicy(e, mode.bond, mode.approval, "Send the bond to 4ExampleTestnetAddress and message staff.")
				handle := "va_" + strings.ReplaceAll(strings.ReplaceAll(mode.name, "+", "_"), "nobond", "nb") + "_" + entry[:3]
				var id, session string
				if entry == "signup" {
					id, session = vendorSignUp(e, handle)
				} else {
					id, session = e.user(handle, "buyer")
					page := html.UnescapeString(e.body("GET", "/account", session, nil, 200))
					mustContain(t, page, `id="vendor-application"`, "Apply to sell")
					w := e.do("POST", "/account/vendor-application", session, url.Values{"action": {"apply"}})
					e.check(w, 303)
					if loc := w.Header().Get("Location"); loc != "/account#vendor-application" {
						t.Fatalf("apply redirected to %q", loc)
					}
				}
				app := latestVendorApp(e, id)
				bonded, automatic := mode.bond != "0", mode.approval == "automatic"
				wantState := map[[2]bool]string{{true, false}: "awaiting_bond", {true, true}: "awaiting_bond", {false, false}: "awaiting_approval", {false, true}: "approved"}[[2]bool{bonded, automatic}]
				if app.State != wantState || app.Approval != mode.approval || (bonded && app.Bond != 500000000000) || (!bonded && app.Bond != 0) {
					t.Fatalf("application = %+v, want state %s", app, wantState)
				}
				if !hasAudit(e, id, "Vendor application submitted") {
					t.Errorf("no submission audit: %v", e.audits(id))
				}
				page := html.UnescapeString(e.body("GET", "/account", session, nil, 200))
				if bonded {
					mustContain(t, page, "Waiting for bond", "0.5 XMR", "non-refundable fee", "Send the bond to 4ExampleTestnetAddress and message staff.", "Withdraw application")
					if userRole(e, id) != "buyer" {
						t.Fatal("bond application made the account a vendor before the bond was recorded")
					}
					// Approving before the bond is recorded is refused.
					adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {app.ID}, "action": {"approve"}}, 409)
					adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {app.ID}, "action": {"bond"}}, 303)
					app = latestVendorApp(e, id)
					if app.How != "manual" || agStr(e, "SELECT bond_received_by FROM vendor_applications WHERE id=$1", app.ID) != adminID {
						t.Fatalf("bond not recorded as manual by the administrator: %+v", app)
					}
					if !hasAudit(e, id, "Vendor bond of 0.5 XMR recorded as received by administrator admin_user") ||
						!hasAudit(e, adminID, "Recorded vendor bond of 0.5 XMR received from "+handle) {
						t.Errorf("bond audits missing: applicant %v admin %v", e.audits(id), e.audits(adminID))
					}
					if !hasNote(e, id, "recorded your vendor bond") {
						t.Errorf("no bond notification: %v", e.notes(id))
					}
					if !strings.Contains(e.audits(adminID)[len(e.audits(adminID))-1], "(confirmed with password)") {
						t.Errorf("bond record not confirmed with password: %v", e.audits(adminID))
					}
					if automatic {
						if app.State != "approved" {
							t.Fatalf("bond + automatic: state %s after bond", app.State)
						}
						// One notice names both the bond step and the approval.
						if !hasNote(e, id, "as received. Your vendor application was approved.") {
							t.Errorf("bond + automatic notices: %v", e.notes(id))
						}
					} else if app.State != "awaiting_approval" {
						t.Fatalf("bond + manual: state %s after bond", app.State)
					}
				} else if !automatic {
					mustContain(t, page, "Waiting for administrator approval", "Withdraw application")
				}
				if !automatic {
					if userRole(e, id) != "buyer" {
						t.Fatal("manual application made the account a vendor before approval")
					}
					adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {app.ID}, "action": {"approve"}, "note": {"Welcome aboard"}}, 303)
					app = latestVendorApp(e, id)
					if app.State != "approved" || agStr(e, "SELECT decided_by FROM vendor_applications WHERE id=$1", app.ID) != adminID ||
						agStr(e, "SELECT note FROM vendor_applications WHERE id=$1", app.ID) != "Welcome aboard" {
						t.Fatalf("approval not recorded: %+v", app)
					}
					if !hasAudit(e, adminID, "Approved vendor application of "+handle) || !hasNote(e, id, "Welcome aboard") {
						t.Errorf("approval audit/notification missing: %v %v", e.audits(adminID), e.notes(id))
					}
				}
				if userRole(e, id) != "vendor" {
					t.Fatalf("approved applicant has role %s", userRole(e, id))
				}
				if !hasAudit(e, id, "Role changed from buyer to vendor by") || !hasNote(e, id, "Your vendor application was approved") {
					t.Errorf("approval role change not audited/notified: %v %v", e.audits(id), e.notes(id))
				}
				if automatic && !hasAudit(e, id, "Role changed from buyer to vendor by automatic vendor approval") {
					t.Errorf("automatic approval not named in the role change: %v", e.audits(id))
				}
				mustContain(t, html.UnescapeString(e.body("GET", "/account", session, nil, 200)), "Approved")
				// The approved vendor can reach the listing form's action.
				if w := e.do("POST", "/listings", session, url.Values{}); w.Code == 403 || w.Code == 401 {
					t.Fatalf("approved vendor refused at /listings: %d", w.Code)
				}
			})
		}
	}
}

func TestVendorApplicationPolicyAppliesToNewApplicationsAndWaiver(t *testing.T) {
	e := newTestApp(t)
	admin := e.setup()
	adminID := agStr(e, "SELECT id FROM users WHERE handle='admin_user'")
	// The policy form saves bond, approval and instructions together, confirmed and audited.
	adminVendorPost(e, admin, "/admin/vendor-policy", url.Values{"vendor_bond_xmr": {"0.5"}, "vendor_approval": {"manual"}, "vendor_bond_instructions": {"Pay to the address in the canary.\nThen message staff."}}, 303)
	if got := agStr(e, "SELECT value FROM settings WHERE key='vendor_bond_xmr'"); got != halfXMR {
		t.Fatalf("bond setting = %q", got)
	}
	if !hasAudit(e, adminID, "Vendor policy changed: bond from none to 0.5 XMR") {
		t.Errorf("policy audit missing: %v", e.audits(adminID))
	}
	adminVendorPost(e, admin, "/admin/vendor-policy", url.Values{"vendor_bond_xmr": {"0.5"}, "vendor_approval": {"manual"}, "vendor_bond_instructions": {"Pay to the address in the canary.\nThen message staff."}}, 409)
	for _, bad := range []url.Values{
		{"vendor_bond_xmr": {"-1"}, "vendor_approval": {"manual"}},
		{"vendor_bond_xmr": {"0.0000000000001"}, "vendor_approval": {"manual"}},
		{"vendor_bond_xmr": {"1"}, "vendor_approval": {"sometimes"}},
		{"vendor_bond_xmr": {"1"}, "vendor_approval": {"manual"}, "vendor_bond_instructions": {"bad\x00text"}},
	} {
		adminVendorPost(e, admin, "/admin/vendor-policy", bad, 400)
	}
	page := html.UnescapeString(e.body("GET", "/admin/vendors", admin, nil, 200))
	mustContain(t, page, `id="vendors"`, "Vendor applications", "non-refundable fee")
	id, session := e.user("va_policy_buyer", "buyer")
	e.check(e.do("POST", "/account/vendor-application", session, url.Values{"action": {"apply"}}), 303)
	// A later policy change does not alter the open application.
	adminVendorPost(e, admin, "/admin/vendor-policy", url.Values{"vendor_bond_xmr": {"0"}, "vendor_approval": {"automatic"}}, 303)
	app := latestVendorApp(e, id)
	if app.State != "awaiting_bond" || app.Bond != 500000000000 || app.Approval != "manual" {
		t.Fatalf("policy change altered an existing application: %+v", app)
	}
	mustContain(t, html.UnescapeString(e.body("GET", "/admin/vendors", admin, nil, 200)), "va_policy_buyer", "Waiting for bond")
	// The administrator waives the bond: recorded as waived, then approval (manual, as captured) is still needed.
	adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {app.ID}, "action": {"waive"}, "note": {"Long-standing member"}}, 303)
	app = latestVendorApp(e, id)
	if app.State != "awaiting_approval" || app.How != "waived" {
		t.Fatalf("waiver: %+v", app)
	}
	if !hasAudit(e, id, "Vendor bond waived by administrator admin_user") || !hasAudit(e, adminID, "Waived vendor bond of 0.5 XMR for va_policy_buyer") || !hasNote(e, id, "waived your vendor bond") {
		t.Errorf("waiver audit/notification missing: %v %v %v", e.audits(id), e.audits(adminID), e.notes(id))
	}
	// Reject with a note: the applicant stays a buyer and is told why.
	adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {app.ID}, "action": {"reject"}, "note": {"Not this time"}}, 303)
	if app = latestVendorApp(e, id); app.State != "rejected" || userRole(e, id) != "buyer" {
		t.Fatalf("rejection: %+v role %s", app, userRole(e, id))
	}
	if !hasNote(e, id, "Your vendor application was rejected") || !hasNote(e, id, "Not this time") || !hasAudit(e, id, "Vendor application rejected by administrator admin_user") {
		t.Errorf("rejection audit/notification missing: %v %v", e.audits(id), e.notes(id))
	}
	mustContain(t, html.UnescapeString(e.body("GET", "/account", session, nil, 200)), "Rejected", "Not this time", "Apply to sell")
	// A new application follows the new policy (no bond, automatic): the account becomes a vendor at once.
	e.check(e.do("POST", "/account/vendor-application", session, url.Values{"action": {"apply"}}), 303)
	if app = latestVendorApp(e, id); app.State != "approved" || app.Bond != 0 || userRole(e, id) != "vendor" {
		t.Fatalf("second application under the new policy: %+v role %s", app, userRole(e, id))
	}
}

func TestVendorApplicationWithdrawAndVerify(t *testing.T) {
	e := newTestApp(t)
	admin := e.setup()
	adminID := agStr(e, "SELECT id FROM users WHERE handle='admin_user'")
	setVendorPolicy(e, "0", "manual", "")
	id, session := e.user("va_withdraw", "buyer")
	e.check(e.do("POST", "/account/vendor-application", session, url.Values{"action": {"apply"}}), 303)
	// A second open application is refused.
	if w := e.do("POST", "/account/vendor-application", session, url.Values{"action": {"apply"}}); w.Code != 409 {
		t.Fatalf("second open application: %d", w.Code)
	}
	e.check(e.do("POST", "/account/vendor-application", session, url.Values{"action": {"withdraw"}}), 303)
	app := latestVendorApp(e, id)
	if app.State != "withdrawn" || agStr(e, "SELECT decided_by FROM vendor_applications WHERE id=$1", app.ID) != id || !hasAudit(e, id, "Withdrew vendor application") {
		t.Fatalf("withdrawal: %+v %v", app, e.audits(id))
	}
	if w := e.do("POST", "/account/vendor-application", session, url.Values{"action": {"withdraw"}}); w.Code != 409 {
		t.Fatalf("withdrawing with nothing open: %d", w.Code)
	}
	// A withdrawn application cannot be decided.
	adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {app.ID}, "action": {"approve"}}, 409)

	// Verify / unverify a vendor: badge on the vendor page, the product page and the listing card.
	vendorID, _ := e.user("va_verified", "vendor")
	product := e.product(vendorID, "digital")
	adminVendorPost(e, admin, "/admin/vendor-verify", url.Values{"handle": {"va_buyer_none"}, "action": {"verify"}}, 404)
	adminVendorPost(e, admin, "/admin/vendor-verify", url.Values{"handle": {"va_withdraw"}, "action": {"verify"}}, 409) // not a vendor
	adminVendorPost(e, admin, "/admin/vendor-verify", url.Values{"handle": {"va_verified"}, "action": {"verify"}}, 303)
	if agStr(e, "SELECT vendor_verified::text||':'||coalesce(vendor_verified_set_by,'') FROM users WHERE id=$1", vendorID) != "true:"+adminID {
		t.Fatal("verified flag not set by the administrator")
	}
	if !hasAudit(e, vendorID, "Marked as a verified vendor by administrator admin_user") || !hasAudit(e, adminID, "Marked va_verified as a verified vendor") || !hasNote(e, vendorID, "marked your account as a verified vendor") {
		t.Errorf("verify audit/notification missing")
	}
	adminVendorPost(e, admin, "/admin/vendor-verify", url.Values{"handle": {"va_verified"}, "action": {"verify"}}, 409)
	for _, path := range []string{"/vendor?id=" + vendorID, "/product?id=" + product, "/"} {
		mustContain(t, e.body("GET", path, session, nil, 200), "Verified vendor")
	}
	adminVendorPost(e, admin, "/admin/vendor-verify", url.Values{"handle": {"va_verified"}, "action": {"unverify"}}, 303)
	for _, path := range []string{"/vendor?id=" + vendorID, "/product?id=" + product, "/"} {
		mustNotContain(t, e.body("GET", path, session, nil, 200), "Verified vendor")
	}
	if !hasAudit(e, vendorID, "Verified vendor mark removed by administrator admin_user") {
		t.Errorf("unverify not audited: %v", e.audits(vendorID))
	}
}

// The admin role form and the -set-role host command keep vendor applications consistent: a direct promotion records
// an approved application (closing an open one); a demotion keeps it, /account says the role was removed, and the
// account can apply again.
func TestVendorRoleFormKeepsApplicationsConsistent(t *testing.T) {
	e := newTestApp(t)
	admin := e.setup()
	adminID := agStr(e, "SELECT id FROM users WHERE handle='admin_user'")
	setVendorPolicy(e, "0", "manual", "")
	id, session := e.user("va_roleform", "buyer")
	e.check(e.do("POST", "/account/vendor-application", session, url.Values{"action": {"apply"}}), 303)
	open := latestVendorApp(e, id).ID
	setRoleForm := func(role string) {
		t.Helper()
		e.check(e.do("POST", "/admin", admin, url.Values{"action": {"role"}, "role": {role}, "handle": {"va_roleform"}, "password": {testPassword}}), 303)
	}
	setRoleForm("vendor")
	app := latestVendorApp(e, id)
	if app.State != "approved" || app.ID == open || agStr(e, "SELECT coalesce(decided_by,'')||'|'||note FROM vendor_applications WHERE id=$1", app.ID) != adminID+"|Role assigned by an administrator" {
		t.Fatalf("role form promotion: %+v", app)
	}
	if agStr(e, "SELECT state FROM vendor_applications WHERE id=$1", open) != "withdrawn" || agInt(e, "SELECT count(*) FROM vendor_applications WHERE user_id=$1 AND state IN "+vendorOpenStates, id) != 0 {
		t.Fatal("the open application was not closed by the promotion")
	}
	if !hasAudit(e, id, "Role changed from buyer to vendor by administrator admin_user") {
		t.Errorf("promotion audit missing: %v", e.audits(id))
	}
	removed := func() {
		t.Helper()
		page := html.UnescapeString(e.body("GET", "/account", session, nil, 200))
		mustContain(t, page, "Your selling approval was removed by an administrator", "Apply to sell")
		mustNotContain(t, page, "<dd>Approved</dd>")
	}
	setRoleForm("buyer")
	if latestVendorApp(e, id).State != "approved" {
		t.Fatal("demotion changed the approved application")
	}
	removed()
	e.check(e.do("POST", "/account/vendor-application", session, url.Values{"action": {"apply"}}), 303)
	if latestVendorApp(e, id).State != "awaiting_approval" {
		t.Fatal("a demoted vendor could not apply again")
	}
	// The host command does the same, decided by no account.
	if _, err := SetRole(t.Context(), e.DB, "va_roleform", "vendor"); err != nil {
		t.Fatal(err)
	}
	app = latestVendorApp(e, id)
	if app.State != "approved" || agStr(e, "SELECT coalesce(decided_by,'none')||'|'||note FROM vendor_applications WHERE id=$1", app.ID) != "none|Role assigned by an administrator (host command)" {
		t.Fatalf("host command promotion: %+v", app)
	}
	if _, err := SetRole(t.Context(), e.DB, "va_roleform", "buyer"); err != nil {
		t.Fatal(err)
	}
	removed()
	mustContain(t, html.UnescapeString(e.body("GET", "/account", session, nil, 200)), "Selling approval removed by an administrator")
}

func TestVendorApplicationAuthz(t *testing.T) {
	e := newTestApp(t)
	admin := e.setup()
	setVendorPolicy(e, halfXMR, "manual", "")
	applicantID, applicant := e.user("va_authz_buyer", "buyer")
	_, vendor := e.user("va_authz_vendor", "vendor")
	_, mod := e.user("va_authz_mod", "moderator")
	e.check(e.do("POST", "/account/vendor-application", applicant, url.Values{"action": {"apply"}}), 303)
	app := latestVendorApp(e, applicantID)
	paths := map[string]url.Values{
		"/admin/vendor-policy":       {"vendor_bond_xmr": {"0"}, "vendor_approval": {"automatic"}, "password": {testPassword}},
		"/admin/vendor-applications": {"id": {app.ID}, "action": {"bond"}, "password": {testPassword}},
		"/admin/vendor-verify":       {"handle": {"va_authz_vendor"}, "action": {"verify"}, "password": {testPassword}},
	}
	for path, form := range paths {
		if w := e.do("POST", path, randomToken(), form); w.Code != 401 {
			t.Errorf("anonymous POST %s: %d", path, w.Code)
		}
		for _, s := range []string{applicant, vendor, mod} {
			if w := e.do("POST", path, s, form); w.Code != 403 {
				t.Errorf("non-admin POST %s: %d", path, w.Code)
			}
		}
	}
	if latestVendorApp(e, applicantID).State != "awaiting_bond" || agStr(e, "SELECT value FROM settings WHERE key='vendor_bond_xmr'") != halfXMR ||
		agInt(e, "SELECT count(*) FROM users WHERE vendor_verified") != 0 {
		t.Fatal("a refused post changed state")
	}
	// The applicant has no self-approval path on their own form.
	for _, action := range []string{"approve", "bond", "waive", ""} {
		if w := e.do("POST", "/account/vendor-application", applicant, url.Values{"action": {action}, "id": {app.ID}}); w.Code != 400 {
			t.Errorf("applicant action %q: %d", action, w.Code)
		}
	}
	// Buyers and moderators without selling approval may apply.
	e.check(e.do("POST", "/account/vendor-application", mod, url.Values{"action": {"apply"}}), 303)
	for _, s := range []string{vendor, admin} {
		if w := e.do("POST", "/account/vendor-application", s, url.Values{"action": {"apply"}}); w.Code != 403 {
			t.Errorf("non-buyer apply: %d", w.Code)
		}
	}
	// A wrong password records nothing.
	adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {app.ID}, "action": {"bond"}, "password": {"wrong-password-xyz"}}, 401)
	if latestVendorApp(e, applicantID).State != "awaiting_bond" {
		t.Fatal("wrong password recorded the bond")
	}
	adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {"nonexistent"}, "action": {"bond"}}, 404)
	adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {app.ID}, "action": {"promote"}}, 400)
	adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {app.ID}, "action": {"reject"}, "note": {strings.Repeat("n", 501)}}, 400)
	// Rejected and withdrawn applicants cannot list.
	adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {app.ID}, "action": {"reject"}}, 303)
	if w := e.do("POST", "/listings", applicant, url.Values{}); w.Code != 403 {
		t.Fatalf("rejected applicant reached /listings: %d", w.Code)
	}
	withdrawnID, withdrawn := e.user("va_authz_withdrawn", "buyer")
	e.check(e.do("POST", "/account/vendor-application", withdrawn, url.Values{"action": {"apply"}}), 303)
	e.check(e.do("POST", "/account/vendor-application", withdrawn, url.Values{"action": {"withdraw"}}), 303)
	if w := e.do("POST", "/listings", withdrawn, url.Values{}); w.Code != 403 || userRole(e, withdrawnID) != "buyer" {
		t.Fatalf("withdrawn applicant reached /listings: %d", w.Code)
	}
	// An invalid account type at sign-up creates nothing.
	w := e.do("POST", "/register", randomToken(), url.Values{"handle": {"va_badtype"}, "password": {testPassword}, "account_type": {"admin"}})
	if w.Code != 400 || agInt(e, "SELECT count(*) FROM users WHERE handle='va_badtype'") != 0 {
		t.Fatalf("invalid account type: %d", w.Code)
	}
	// A buyer sign-up (the default) creates no application.
	w = e.do("POST", "/register", randomToken(), url.Values{"handle": {"va_plain_buyer"}, "password": {testPassword}})
	e.check(w, 303)
	if agInt(e, "SELECT count(*) FROM vendor_applications a JOIN users u ON u.id=a.user_id WHERE u.handle='va_plain_buyer'") != 0 {
		t.Fatal("buyer sign-up created an application")
	}
}

func TestActiveVendorSQL(t *testing.T) {
	e := newTestApp(t)
	active, _ := e.user("av_active", "vendor")
	suspended, _ := e.user("av_suspended", "vendor")
	buyer, _ := e.user("av_buyer", "buyer")
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", suspended)
	for id, want := range map[string]bool{active: true, suspended: false, buyer: false} {
		var got bool
		if err := e.DB.QueryRow("SELECT "+activeVendorSQL("u")+" FROM users u WHERE u.id=$1", id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("activeVendorSQL(%s) = %v, want %v", id, got, want)
		}
	}
}

// Existing vendors are grandfathered with an approved application and are not verified.
func TestVendorMigrationGrandfathers(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("mig_vendor", "vendor")
	buyerID, _ := e.user("mig_buyer", "buyer")
	// Put the database back as migration 120 left it (migration 124's bond tables reference vendor_applications, so they
	// go too and are made again after it).
	agExec(e, `DELETE FROM schema_migrations WHERE version IN (121,124);
		DROP TABLE vendor_bond_deposits, vendor_bond_accounts, xmr_bond_wallets;
		DROP FUNCTION xmr_bond_wallets_kept();
		DROP TABLE vendor_applications;
		ALTER TABLE users DROP COLUMN vendor_verified, DROP COLUMN vendor_verified_set_by, DROP COLUMN vendor_verified_set_at;
		DELETE FROM settings WHERE key IN ('vendor_bond_xmr','vendor_approval','vendor_bond_instructions')`)
	if err := applyLatestMigrations(t, e); err != nil {
		t.Fatal(err)
	}
	app := latestVendorApp(e, vendorID)
	if app.State != "approved" || app.Bond != 0 {
		t.Fatalf("grandfathered vendor application = %+v", app)
	}
	if latestVendorApp(e, buyerID).State != "" {
		t.Fatal("a buyer was given an application")
	}
	if agStr(e, "SELECT vendor_verified::text FROM users WHERE id=$1", vendorID) != "false" {
		t.Fatal("grandfathered vendor is verified")
	}
	for key, want := range map[string]string{"vendor_bond_xmr": "0", "vendor_approval": "manual", "vendor_bond_instructions": ""} {
		if got := agStr(e, "SELECT value FROM settings WHERE key=$1", key); got != want {
			t.Errorf("setting %s = %q, want %q", key, got, want)
		}
	}
	// At most one open application per account.
	agExec(e, "INSERT INTO vendor_applications(id,user_id,state,approval,bond_piconero) VALUES('a1',$1,'awaiting_approval','manual',0)", buyerID)
	if _, err := e.DB.Exec("INSERT INTO vendor_applications(id,user_id,state,approval,bond_piconero) VALUES('a2',$1,'awaiting_approval','manual',0)", buyerID); err == nil {
		t.Fatal("a second open application was accepted")
	}
	// Waiting for a bond needs a bond and no receipt.
	if _, err := e.DB.Exec("INSERT INTO vendor_applications(id,user_id,state,approval,bond_piconero) VALUES('a3',$1,'awaiting_bond','manual',0)", vendorID); err == nil {
		t.Fatal("awaiting_bond without a bond accepted")
	}
}

// Retention: a closed account leaves the vendor queue, its applications go with the account, and the administrator
// named on an application or a verified mark is unlinked (ON DELETE SET NULL) without blocking either purge.
func TestVendorApplicationRetentionLifecycle(t *testing.T) {
	e := newTestApp(t)
	adminID, _ := e.user("va_ret_admin", "admin")
	applicant, _ := e.user("va_ret_applicant", "buyer")
	vendorID, _ := e.user("va_ret_vendor", "vendor")
	agExec(e, "INSERT INTO vendor_applications(id,user_id,state,approval,bond_piconero) VALUES('ret_open',$1,'awaiting_bond','manual',5)", applicant)
	agExec(e, `INSERT INTO vendor_applications(id,user_id,state,approval,bond_piconero,bond_received_at,bond_received_by,bond_received_how,decided_by,decided_at)
		VALUES('ret_done',$1,'approved','manual',5,now(),$2,'manual',$2,now())`, vendorID, adminID)
	agExec(e, "UPDATE users SET vendor_verified=true,vendor_verified_set_by=$2,vendor_verified_set_at=now() WHERE id=$1", vendorID, adminID)
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = e.A.applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "account_close", ID: applicant, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = tx.QueryRow("SELECT state FROM vendor_applications WHERE id='ret_open'").Scan(&state); err != nil || state != "withdrawn" {
		t.Fatalf("closed account's open application: %q %v", state, err)
	}
	for _, id := range []string{applicant, adminID} {
		if _, err = tx.Exec("DELETE FROM sessions WHERE user_id=$1", id); err != nil {
			t.Fatal(err)
		}
		needed, err := retentionUserReferenced(t.Context(), tx, id)
		if err != nil || needed {
			t.Fatalf("vendor records blocked purge of %s: %t %v", id, needed, err)
		}
		if err = e.A.applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "account", ID: id, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	var gone, unlinked bool
	if err = tx.QueryRow(`SELECT NOT EXISTS(SELECT 1 FROM vendor_applications WHERE id='ret_open'),
		(SELECT decided_by IS NULL AND bond_received_by IS NULL FROM vendor_applications WHERE id='ret_done') AND
		(SELECT vendor_verified AND vendor_verified_set_by IS NULL FROM users WHERE id=$1)`, vendorID).Scan(&gone, &unlinked); err != nil || !gone || !unlinked {
		t.Fatalf("purge lifecycle: applicant rows gone=%t admin unlinked=%t err=%v", gone, unlinked, err)
	}
}
