package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type adminSettingFormCase struct {
	path, field, value, changed string
	form                        url.Values
	redirect                    string
}

func adminSettingForms(t *testing.T, e *testEnv) ([]adminSettingFormCase, string, string) {
	t.Helper()
	adminID, admin := e.user("nonce_admin_settings", "admin")
	operator, operatorPub := canaryTestKey(t, "nonce_admin_operator")
	_, otherPub := canaryTestKey(t, "nonce_admin_other")
	_, _, operatorCanonical, err := parseCanonicalPublicKey(operatorPub)
	if err != nil {
		t.Fatal(err)
	}
	_, _, otherCanonical, err := parseCanonicalPublicKey(otherPub)
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, "INSERT INTO settings(key,value) VALUES($1,$2)", settingOperatorKey, operatorPub)
	statement := "No warrants as of 2026-10-08.\nNext update by 2026-11-08."
	signed := canarySign(t, operator, statement, canaryKeyTime.Add(time.Hour))
	return []adminSettingFormCase{
		{path: "/admin/captcha", field: "captcha_required", value: "false", changed: "true", form: url.Values{"captcha_required": {"false"}}, redirect: "/admin/security"},
		{path: "/admin/staff-contact", field: "staff_contact", value: "security@example.test", changed: "changed@example.test", form: url.Values{"staff_contact": {"security@example.test"}}, redirect: "/admin/site#staff-contact"},
		{path: "/admin/staff-list", field: "staff_list_public", value: "true", changed: "false", form: url.Values{"staff_list_public": {"true"}}, redirect: "/admin/transparency#staff-list"},
		{path: "/admin/canary", field: "clearsigned", value: signed, changed: signed + "\nchanged", form: url.Values{"canary": {signed}}, redirect: "/canary"},
		{path: "/admin/operator-key", field: settingOperatorKey, value: otherCanonical, changed: operatorCanonical, form: url.Values{"operator_key": {otherPub}}, redirect: "/admin/transparency#transparency"},
	}, adminID, admin
}

// adminSettingPages renders the administration sections holding the setting forms (A-542) as one body.
func adminSettingPages(t *testing.T, e *testEnv, admin string) *httptest.ResponseRecorder {
	t.Helper()
	all := httptest.NewRecorder()
	for _, path := range []string{"/admin/security", "/admin/site", "/admin/transparency"} {
		w := e.do("GET", path, admin, nil)
		e.check(w, 200)
		all.Body.WriteString(w.Body.String())
	}
	return all
}

func TestAdminSettingsConfirmedFormsRenderWithoutWrites(t *testing.T) {
	e := newTestApp(t)
	cases, _, admin := adminSettingForms(t, e)
	beforeAudits := listingFormCount(t, e, "SELECT count(*) FROM audit_events")
	page := adminSettingPages(t, e, admin)
	seen := map[string]bool{}
	for _, tc := range cases {
		nonce := renderedFormNonce(t, page.Body.String(), tc.path)
		if seen[nonce] {
			t.Fatalf("admin setting forms reused nonce %s", nonce)
		}
		seen[nonce] = true
	}
	if got := submissionCount(t, e); got != 0 {
		t.Fatalf("admin GET wrote %d form receipts", got)
	}
	if got := listingFormCount(t, e, "SELECT count(*) FROM audit_events"); got != beforeAudits {
		t.Fatalf("admin GET wrote %d audit rows", got-beforeAudits)
	}
}

func TestAdminSettingsConfirmedFormsReplayOnlyExactRequest(t *testing.T) {
	e := newTestApp(t)
	cases, adminID, admin := adminSettingForms(t, e)
	// Use another session for the same actor so replay mismatch is attributable to the session,
	// not a role or password change.
	otherSession := randomToken()
	if _, err := e.DB.Exec("INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '1 hour')", digest(otherSession), adminID); err != nil {
		t.Fatal(err)
	}
	page := adminSettingPages(t, e, admin)
	var oldCanaryForm url.Values
	var oldCanaryPosted time.Time
	for _, tc := range cases {
		t.Run(strings.TrimPrefix(tc.path, "/admin/"), func(t *testing.T) {
			form := url.Values{}
			for key, values := range tc.form {
				form[key] = append([]string(nil), values...)
			}
			form.Set("password", testPassword)
			form.Set(formNonceField, renderedFormNonce(t, page.Body.String(), tc.path))
			beforeAudit := listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID)
			first := e.do("POST", tc.path, admin, form)
			e.check(first, 303)
			if got := first.Header().Get("Location"); got != tc.redirect {
				t.Fatalf("redirect = %q, want %q", got, tc.redirect)
			}
			var firstCanaryPosted time.Time
			if tc.path == "/admin/canary" {
				if err := e.DB.QueryRow("SELECT posted FROM canary WHERE id=1").Scan(&firstCanaryPosted); err != nil {
					t.Fatal(err)
				}
				oldCanaryForm = url.Values{}
				for key, values := range form {
					oldCanaryForm[key] = append([]string(nil), values...)
				}
				oldCanaryPosted = firstCanaryPosted
				time.Sleep(20 * time.Millisecond)
			}
			replay := e.do("POST", tc.path, admin, form)
			e.check(replay, 303)
			if replay.Header().Get("Location") != first.Header().Get("Location") {
				t.Fatalf("exact replay redirect differs: first=%q replay=%q", first.Header().Get("Location"), replay.Header().Get("Location"))
			}
			firstNotice, replayNotice := noticeSetCookie(first), noticeSetCookie(replay)
			if (firstNotice == nil) != (replayNotice == nil) || firstNotice != nil && firstNotice.Value != replayNotice.Value {
				t.Fatalf("exact replay notice differs: first=%v replay=%v", firstNotice, replayNotice)
			}
			if got := listingFormCount(t, e, "SELECT count(*) FROM form_submissions WHERE action_path=$1", tc.path); got != 1 {
				t.Fatalf("receipt count = %d, want 1", got)
			}
			if got := listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID); got != beforeAudit+1 {
				t.Fatalf("replay wrote an extra audit: before=%d after=%d", beforeAudit, got)
			}
			if tc.path == "/admin/canary" {
				if got := listingFormCount(t, e, "SELECT count(*) FROM canary WHERE clearsigned=$1", strings.TrimSpace(tc.value)); got != 1 {
					t.Fatalf("published canary rows = %d, want one matching submission", got)
				}
			} else if got := listingFormCount(t, e, "SELECT count(*) FROM settings WHERE key=$1 AND value=$2", tc.field, tc.value); got != 1 {
				t.Fatalf("saved setting %s=%q rows = %d", tc.field, tc.value, got)
			}
			if tc.path == "/admin/canary" {
				var after time.Time
				if err := e.DB.QueryRow("SELECT posted FROM canary WHERE id=1").Scan(&after); err != nil || !after.Equal(firstCanaryPosted) {
					t.Fatalf("replay changed canary posted time: first=%s after=%s err=%v", firstCanaryPosted, after, err)
				}
			}
			changed := url.Values{}
			for key, values := range form {
				changed[key] = append([]string(nil), values...)
			}
			changed.Set(tc.formField(), tc.changed)
			e.check(e.do("POST", tc.path, admin, changed), 409)
			e.check(e.do("POST", tc.path, otherSession, form), 409)
		})
	}
	// The saved canary receipt remains replayable even after the operator key changes. A fresh
	// nonce still runs signature verification against the new key and refuses the old signature.
	beforeAudit := listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID)
	oldReplay := e.do("POST", "/admin/canary", admin, oldCanaryForm)
	e.check(oldReplay, 303)
	var replayedCanaryPosted time.Time
	if err := e.DB.QueryRow("SELECT posted FROM canary WHERE id=1").Scan(&replayedCanaryPosted); err != nil || !replayedCanaryPosted.Equal(oldCanaryPosted) {
		t.Fatalf("old receipt replay changed canary posted time: first=%s after=%s err=%v", oldCanaryPosted, replayedCanaryPosted, err)
	}
	if got := listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID); got != beforeAudit {
		t.Fatalf("old receipt replay added audit rows: before=%d after=%d", beforeAudit, got)
	}
	freshPage := e.do("GET", "/admin/transparency", admin, nil)
	e.check(freshPage, 200)
	fresh := url.Values{"canary": {oldCanaryForm.Get("canary")}, "password": {testPassword}, formNonceField: {renderedFormNonce(t, freshPage.Body.String(), "/admin/canary")}}
	if w := e.do("POST", "/admin/canary", admin, fresh); w.Code != 400 || !strings.Contains(w.Body.String(), "does not verify") {
		t.Fatalf("fresh old-key canary was not refused: status=%d body=%s", w.Code, w.Body)
	}
	if got := listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID); got != beforeAudit {
		t.Fatalf("fresh refused canary added audit rows: before=%d after=%d", beforeAudit, got)
	}
}

func (tc adminSettingFormCase) formField() string {
	switch tc.path {
	case "/admin/canary":
		return "canary"
	case "/admin/operator-key":
		return "operator_key"
	default:
		return tc.field
	}
}

func TestAdminCanaryRefusalRollsBackNonceForCorrection(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("nonce_admin_canary_refusal", "admin")
	operator, operatorPub := canaryTestKey(t, "nonce_admin_canary_refusal")
	other, _ := canaryTestKey(t, "nonce_admin_canary_wrong")
	agExec(e, "INSERT INTO settings(key,value) VALUES($1,$2)", settingOperatorKey, operatorPub)
	statement := "No warrants as of 2026-10-08.\nNext update by 2026-11-08."
	wrong := canarySign(t, other, statement, canaryKeyTime.Add(time.Hour))
	good := canarySign(t, operator, statement, canaryKeyTime.Add(time.Hour))
	page := e.do("GET", "/admin/transparency", admin, nil)
	e.check(page, 200)
	form := url.Values{"canary": {wrong}, "password": {testPassword}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/admin/canary")}}
	if w := e.do("POST", "/admin/canary", admin, form); w.Code != 400 || !strings.Contains(w.Body.String(), "does not verify") {
		t.Fatalf("invalid canary refusal: %d %s", w.Code, w.Body)
	}
	if got := submissionCount(t, e); got != 0 {
		t.Fatalf("refused canary kept %d receipt(s)", got)
	}
	form.Set("canary", good)
	e.check(e.do("POST", "/admin/canary", admin, form), 303)
	if got := submissionCount(t, e); got != 1 {
		t.Fatalf("corrected canary receipt count = %d, want 1", got)
	}
}

func TestAdminCanaryLostCommitAcknowledgementCanReplaySavedResponse(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("nonce_admin_canary_lost_commit", "admin")
	operator, operatorPub := canaryTestKey(t, "nonce_admin_canary_lost_commit")
	agExec(e, "INSERT INTO settings(key,value) VALUES($1,$2)", settingOperatorKey, operatorPub)
	statement := "No warrants as of 2026-10-08.\nNext update by 2026-11-08."
	signed := canarySign(t, operator, statement, canaryKeyTime.Add(time.Hour))
	page := e.do("GET", "/admin/transparency", admin, nil)
	e.check(page, 200)
	form := url.Values{"canary": {signed}, "password": {testPassword}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/admin/canary")}}
	lost := loseNextActionCommit(t, e)
	w := e.do("POST", "/admin/canary", admin, form)
	if lost.Load() {
		t.Fatal("Commit did not reach the injected acknowledgement loss")
	}
	if w.Code != 500 || w.Header().Get("Retry-After") != "" || !strings.Contains(w.Body.String(), "Admin setting outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
		t.Fatalf("lost Commit acknowledgement: status=%d body=%s", w.Code, w.Body)
	}
	var posted time.Time
	if err := e.DB.QueryRow("SELECT posted FROM canary WHERE id=1").Scan(&posted); err != nil {
		t.Fatal(err)
	}
	audits := listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID)
	if audits != 1 || submissionCount(t, e) != 1 {
		t.Fatalf("lost acknowledgement committed audits=%d receipts=%d; want one each", audits, submissionCount(t, e))
	}
	replay := e.do("POST", "/admin/canary", admin, form)
	e.check(replay, 303)
	var after time.Time
	if err := e.DB.QueryRow("SELECT posted FROM canary WHERE id=1").Scan(&after); err != nil || !after.Equal(posted) {
		t.Fatalf("saved replay changed canary posted time: first=%s after=%s err=%v", posted, after, err)
	}
	if got := listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID); got != audits || submissionCount(t, e) != 1 {
		t.Fatalf("saved replay duplicated audit/receipt: audits=%d receipts=%d", got, submissionCount(t, e))
	}
}
