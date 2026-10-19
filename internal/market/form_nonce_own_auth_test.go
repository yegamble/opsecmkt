package market

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestA130TOTPFactorFormsRenderWithoutGETWrites(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("a130_factor_render", "buyer")
	e.check(e.do("POST", "/totp/enroll", session, nil), 303)
	page := e.do("GET", "/totp", session, nil)
	e.check(page, 200)
	renderedFormNonce(t, page.Body.String(), "/totp/activate")
	if got := submissionCount(t, e); got != 1 { // enrollment's own receipt, no GET write
		t.Fatalf("activation GET wrote receipts: %d", got)
	}
	key, _ := e.totpCodeFor(uid, 0)
	activation := url.Values{"password": {testPassword}, "code": {key}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/totp/activate")}}
	e.check(e.do("POST", "/totp/activate", session, activation), 303)
	firstStep := agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", uid)
	e.check(e.do("POST", "/totp/activate", session, activation), 303)
	if agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", uid) != firstStep ||
		a152Audits(e, uid, "Enabled TOTP") != 1 {
		t.Fatal("activation replay spent its code or wrote another audit")
	}
	newSecret, sealErr := e.A.seal("totp", newTOTPSecret())
	if sealErr != nil {
		t.Fatal(sealErr)
	}
	agExec(e, "UPDATE users SET totp_secret=$2,totp_last_step=totp_last_step+1 WHERE id=$1", uid, newSecret)
	agExec(e, "DELETE FROM recovery_codes WHERE user_id=$1", uid)
	agExec(e, "INSERT INTO recovery_codes(code_hash,user_id) VALUES($1,$2)", recoveryHash(newRecoveryCode()), uid)
	laterSecret := agStr(e, "SELECT totp_secret FROM users WHERE id=$1", uid)
	laterStep := agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", uid)
	laterCodes := agStr(e, "SELECT string_agg(code_hash,'|' ORDER BY code_hash) FROM recovery_codes WHERE user_id=$1", uid)
	e.check(e.do("POST", "/totp/activate", session, activation), 303)
	if agStr(e, "SELECT totp_secret FROM users WHERE id=$1", uid) != laterSecret ||
		agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", uid) != laterStep ||
		agStr(e, "SELECT string_agg(code_hash,'|' ORDER BY code_hash) FROM recovery_codes WHERE user_id=$1", uid) != laterCodes ||
		a152Audits(e, uid, "Enabled TOTP") != 1 {
		t.Fatal("old activation replay replaced a later authenticator or recovery batch")
	}
	page = e.do("GET", "/totp", session, nil)
	e.check(page, 200)
	renderedFormNonce(t, page.Body.String(), "/totp/recovery")
	renderedFormNonce(t, page.Body.String(), "/totp/disable")
	if got := submissionCount(t, e); got != 2 {
		t.Fatalf("enabled TOTP GET wrote receipts: %d", got)
	}
}

func TestA130TOTPDisableRecoveryReceiptReplaysAfterCodeDeletion(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("a130_factor_disable", "buyer")
	_, recovery := agEnableTOTP(e, uid)
	page := e.do("GET", "/totp", session, nil)
	e.check(page, 200)
	form := url.Values{"password": {testPassword}, "code": {recovery}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/totp/disable")}}
	beforeAudit := a152Audits(e, uid, "Turned off TOTP")
	first := e.do("POST", "/totp/disable", session, form)
	e.check(first, 303)
	if got := agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", uid); got != 0 {
		t.Fatalf("disable retained %d recovery codes", got)
	}
	repeat := e.do("POST", "/totp/disable", session, form)
	e.check(repeat, 303)
	if repeat.Header().Get("Location") != first.Header().Get("Location") || a152Audits(e, uid, "Turned off TOTP") != beforeAudit+1 {
		t.Fatal("replay changed the original redirect or duplicated the audit")
	}
	if got := agInt(e, "SELECT count(*) FROM form_submissions WHERE action_path='/totp/disable'"); got != 1 {
		t.Fatalf("disable stored %d receipts", got)
	}
	var saved string
	if err := e.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", form.Get(formNonceField)).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testPassword, recovery, session} {
		if strings.Contains(saved, secret) {
			t.Fatal("factor receipt retained submitted secret or session")
		}
	}
	changed := url.Values{"password": {testPassword}, "code": {"aaaa-bbbb-cccc-dddd"}, formNonceField: {form.Get(formNonceField)}}
	e.check(e.do("POST", "/totp/disable", session, changed), 409)
	other := agSession(e, uid)
	e.check(e.do("POST", "/totp/disable", other, form), 409)
	e.check(e.do("POST", "/totp/enroll", session, nil), 303)
	nextCode, _ := e.totpCodeFor(uid, 0)
	e.check(e.do("POST", "/totp/activate", session, url.Values{"password": {testPassword}, "code": {nextCode}}), 303)
	laterSecret := agStr(e, "SELECT totp_secret FROM users WHERE id=$1", uid)
	laterStep := agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", uid)
	laterCodes := agStr(e, "SELECT string_agg(code_hash,'|' ORDER BY code_hash) FROM recovery_codes WHERE user_id=$1", uid)
	laterAudit := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", uid)
	laterNotices := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", uid)
	e.check(e.do("POST", "/totp/disable", session, form), 303)
	if agStr(e, "SELECT totp_secret FROM users WHERE id=$1", uid) != laterSecret ||
		agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", uid) != laterStep ||
		agStr(e, "SELECT string_agg(code_hash,'|' ORDER BY code_hash) FROM recovery_codes WHERE user_id=$1", uid) != laterCodes ||
		agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", uid) != laterAudit ||
		agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", uid) != laterNotices {
		t.Fatal("old disable replay changed a newly enrolled authenticator, its codes, audit or notices")
	}
	form.Set("csrf", e.A.csrf(session)) // direct handler below bypasses the HTTP middleware
	agExec(e, "UPDATE sessions SET expires=now()-interval '1 second' WHERE token_hash=$1", digest(session))
	c := &actionCtx{A: e.A, R: httptest.NewRequest("POST", "/totp/disable", nil), User: e.loadUser(uid), Token: session,
		Form: form, ConfirmedFormNonce: true, FactorFormNonce: true}
	_, err := confirmedTxWith(c, &confirmation{Recovery: true, Before: lockOwnRecoveryCodes}, func(*actionCtx) (actionResult, error) {
		t.Fatal("expired-session factor replay reached mutation")
		return actionResult{}, nil
	})
	var he *httpError
	if !errors.As(err, &he) || he.Code != 401 {
		t.Fatalf("expired session replay error=%v, want current-session refusal", err)
	}
}

func TestA130TOTPRegenerationReceiptDoesNotSpendCurrentCodeTwice(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("a130_factor_recovery", "buyer")
	key, _ := agEnableTOTP(e, uid)
	page := e.do("GET", "/totp", session, nil)
	e.check(page, 200)
	form := url.Values{"password": {testPassword}, "code": {agTOTPNow(key)}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/totp/recovery")}}
	first := e.do("POST", "/totp/recovery", session, form)
	e.check(first, 303)
	step := agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", uid)
	repeat := e.do("POST", "/totp/recovery", session, form)
	e.check(repeat, 303)
	if got := agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", uid); got != step {
		t.Fatalf("exact replay spent TOTP again: %d to %d", step, got)
	}
	if got := a152Audits(e, uid, "Replaced recovery codes"); got != 1 {
		t.Fatalf("regeneration audit rows %d", got)
	}
	agExec(e, "DELETE FROM recovery_codes WHERE user_id=$1", uid)
	agExec(e, "INSERT INTO recovery_codes(code_hash,user_id) VALUES($1,$2)", recoveryHash(newRecoveryCode()), uid)
	laterCodes := agStr(e, "SELECT string_agg(code_hash,'|' ORDER BY code_hash) FROM recovery_codes WHERE user_id=$1", uid)
	e.check(e.do("POST", "/totp/recovery", session, form), 303)
	if agStr(e, "SELECT string_agg(code_hash,'|' ORDER BY code_hash) FROM recovery_codes WHERE user_id=$1", uid) != laterCodes ||
		a152Audits(e, uid, "Replaced recovery codes") != 1 {
		t.Fatal("old regeneration replay replaced a newer recovery batch")
	}
	form.Set(formNonceField, randomToken()[:32])
	e.check(e.do("POST", "/totp/recovery", session, form), 401)
}

func TestA130OwnFormRemembersOriginalAdminConfirmationAndDoesNotUndoNewerSetting(t *testing.T) {
	e := newTestApp(t)
	adminID, session := e.user("own_receipt_admin", "admin")
	page := e.do("GET", "/admin/site", session, nil)
	e.check(page, 200)
	form := url.Values{"action": {"settings"}, "site_name": {"First Name"}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/admin")}}
	first := e.do("POST", "/admin", session, form)
	e.check(first, 303)
	if n := agInt(e, "SELECT count(*) FROM form_submissions WHERE nonce=$1 AND confirmation_required=false", form.Get(formNonceField)); n != 1 {
		t.Fatalf("original no-password mode not recorded: %d", n)
	}
	e.check(e.do("POST", "/admin", session, url.Values{"action": {"settings"}, "site_name": {"Newer Name"}}), 303)
	auditBefore := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Saved marketplace site name%'", adminID)
	replay := e.do("POST", "/admin", session, form)
	e.check(replay, 303)
	if replay.Header().Get("Location") != first.Header().Get("Location") ||
		agStr(e, "SELECT value FROM settings WHERE key='site_name'") != "Newer Name" ||
		agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Saved marketplace site name%'", adminID) != auditBefore {
		t.Fatal("saved settings replay reverted a newer setting or repeated the audit")
	}
	changed := url.Values{"action": {"settings"}, "site_name": {"Changed Body"}, formNonceField: {form.Get(formNonceField)}}
	e.check(e.do("POST", "/admin", session, changed), 409)
	other := agSession(e, adminID)
	e.check(e.do("POST", "/admin", other, form), 409)
}

func TestA130OwnFormOriginalAccountPasswordModeSurvivesNewerKey(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("own_receipt_profile", "buyer")
	_, firstKey := testPGPKey(t, "first receipt key")
	_, newerKey := testPGPKey(t, "newer receipt key")
	page := e.do("GET", "/account", session, nil)
	e.check(page, 200)
	form := url.Values{"pgp": {firstKey}, "password": {testPassword}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/account")}}
	e.check(e.do("POST", "/account", session, form), 303)
	if n := agInt(e, "SELECT count(*) FROM form_submissions WHERE nonce=$1 AND confirmation_required=true", form.Get(formNonceField)); n != 1 {
		t.Fatalf("original password mode not recorded: %d", n)
	}
	e.check(e.do("POST", "/account", session, url.Values{"pgp": {newerKey}, "password": {testPassword}}), 303)
	audits := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE '%PGP%'", uid)
	e.check(e.do("POST", "/account", session, form), 303)
	if got := agStr(e, "SELECT pgp FROM users WHERE id=$1", uid); got != newerKey {
		t.Fatal("old account receipt reverted a newer PGP key")
	}
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE '%PGP%'", uid); got != audits {
		t.Fatal("old account receipt repeated the PGP audit")
	}
	changedHash, err := hashPassword("another-long-current-password")
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, "UPDATE users SET password_hash=$2 WHERE id=$1", uid, changedHash)
	e.check(e.do("POST", "/account", session, form), 401)
}

func TestA130AdminRoleReceiptStillRequiresOriginalPasswordMode(t *testing.T) {
	e := newTestApp(t)
	adminID, session := e.user("own_role_admin", "admin")
	targetID, _ := e.user("own_role_target", "buyer")
	form := url.Values{"action": {"role"}, "handle": {"own_role_target"}, "role": {"vendor"}, "password": {testPassword}, formNonceField: {randomToken()[:32]}}
	e.check(e.do("POST", "/admin", session, form), 303)
	if agInt(e, "SELECT count(*) FROM form_submissions WHERE nonce=$1 AND confirmation_required", form.Get(formNonceField)) != 1 {
		t.Fatal("role receipt lost its original password requirement")
	}
	agExec(e, "UPDATE users SET role='moderator' WHERE id=$1", targetID)
	audits := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID)
	e.check(e.do("POST", "/admin", session, form), 303)
	if got := agStr(e, "SELECT role FROM users WHERE id=$1", targetID); got != "moderator" {
		t.Fatal("role replay changed a newer target role")
	}
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID); got != audits {
		t.Fatal("role replay wrote a second audit")
	}
	bad := url.Values{"action": {"role"}, "handle": {"own_role_target"}, "role": {"vendor"}, "password": {"wrong-current-password"}, formNonceField: {form.Get(formNonceField)}}
	e.check(e.do("POST", "/admin", session, bad), 409) // request identity differs; it cannot select an old mode
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", adminID)
	if w := e.do("POST", "/admin", session, form); w.Code == 303 {
		t.Fatal("suspended administrator replayed old receipt")
	}
}

// A-159(h): a profile save that changes nothing writes no audit row (it is not a profile change), needs no password,
// and still leaves its receipt so an exact repeat replays the same success notice. The receipt is written by
// finishFormSubmission whether or not the action audits.
func TestA130UnchangedProfileSaveWritesNoAuditAndReplaysWithoutPassword(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("own_profile_noop", "buyer")
	base := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", uid)
	audits := func() int { // any audit row the account gained, whatever its text
		return agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", uid) - base
	}
	form := url.Values{"pgp": {""}, formNonceField: {randomToken()[:32]}}
	w := e.do("POST", "/account", session, form)
	e.check(w, 303)
	if w.Header().Get("Location") != "/account" || e.cookie(w, "notice") != "profile" {
		t.Fatalf("unchanged profile save did not answer with the success notice: %q notice=%q", w.Header().Get("Location"), e.cookie(w, "notice"))
	}
	if n := audits(); n != 0 {
		t.Fatalf("unchanged profile submission wrote %d audit rows, want none", n)
	}
	if n := agInt(e, "SELECT count(*) FROM form_submissions WHERE nonce=$1 AND NOT confirmation_required AND redirect='/account' AND notice='profile'", form.Get(formNonceField)); n != 1 {
		t.Fatalf("unchanged profile receipt missing its no-password mode or saved notice: %d", n)
	}
	w = e.do("POST", "/account", session, form)
	e.check(w, 303)
	if w.Header().Get("Location") != "/account" || e.cookie(w, "notice") != "profile" {
		t.Fatalf("exact replay did not return the recorded success: %q notice=%q", w.Header().Get("Location"), e.cookie(w, "notice"))
	}
	if n := audits(); n != 0 {
		t.Fatalf("exact replay audited an unchanged profile: %d", n)
	}
	if n := agInt(e, "SELECT count(*) FROM form_submissions WHERE nonce=$1", form.Get(formNonceField)); n != 1 {
		t.Fatalf("exact replay changed the receipt count: %d", n)
	}
	// Distinct unchanged submissions (fresh forms) stay unaudited however often they are made.
	for range 3 {
		e.check(e.do("POST", "/account", session, url.Values{"pgp": {""}, formNonceField: {randomToken()[:32]}}), 303)
	}
	if n := audits(); n != 0 {
		t.Fatalf("repeated unchanged profile saves wrote %d audit rows, want none", n)
	}
}

// The first receipt can still be uncommitted when the second request's
// read-only mode probe runs. A changed live key can then make it derive false
// even though the winning submission required a password. The unique-key
// conflict conveys the saved mode only after exact identity verification;
// retry it outside the actor lock and require the current password again.
func TestA130OwnReceiptReconcilesStaleComputedModeAfterExactConflict(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("own_mode_drift", "buyer")
	_, public := testPGPKey(t, "mode drift")
	form := url.Values{"pgp": {public}, "password": {testPassword}, formNonceField: {randomToken()[:32]}}
	e.check(e.do("POST", "/account", session, form), 303)
	form.Set("csrf", e.A.csrf(session)) // e.do clones the form before adding its CSRF field
	c := &actionCtx{A: e.A, R: httptest.NewRequest("POST", "/account", nil), User: e.loadUser(uid), Token: session, Form: form, OwnFormNonce: true}
	res, err := ownConfirmedTx(c, false, func(*actionCtx) (actionResult, error) {
		t.Fatal("stale-mode exact replay reached mutation callback")
		return actionResult{}, nil
	})
	if err != nil || res.Redirect != "/account" {
		t.Fatalf("exact stale-mode replay: result=%+v err=%v", res, err)
	}
	if n := agInt(e, "SELECT count(*) FROM form_submissions WHERE nonce=$1", form.Get(formNonceField)); n != 1 {
		t.Fatalf("stale-mode retry added a receipt: %d", n)
	}
}

func TestA130OwnReceiptReconcilesStaleTrueModeWithOriginalNoPassword(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("own_mode_false", "buyer")
	form := url.Values{"pgp": {""}, formNonceField: {randomToken()[:32]}}
	e.check(e.do("POST", "/account", session, form), 303)
	form.Set("csrf", e.A.csrf(session))
	c := &actionCtx{A: e.A, R: httptest.NewRequest("POST", "/account", nil), User: e.loadUser(uid), Token: session, Form: form, OwnFormNonce: true}
	res, err := ownConfirmedTx(c, true, func(*actionCtx) (actionResult, error) {
		t.Fatal("stale true mode reached mutation callback")
		return actionResult{}, nil
	})
	if err != nil || res.Redirect != "/account" {
		t.Fatalf("original no-password mode replay: result=%+v err=%v", res, err)
	}
	if n := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Updated profile'", uid); n != 0 {
		t.Fatalf("stale-mode replay of an unchanged profile save wrote account audit: %d", n)
	}
}

func TestA130OwnAndFactorFormsRejectMissingNonceAndBadCSRF(t *testing.T) {
	e := newTestApp(t)
	adminID, session := e.user("own_raw_guard_admin", "admin")
	agEnableTOTP(e, adminID)
	postRaw := func(path string, f url.Values) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "session", Value: session})
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		return w.Code
	}
	for _, tc := range []struct {
		path string
		form url.Values
	}{
		{"/admin", url.Values{"action": {"settings"}, "site_name": {"Guard Test"}}},
		{"/totp/disable", url.Values{"password": {testPassword}, "code": {"000000"}}},
	} {
		f := tc.form
		f.Set("csrf", e.A.csrf(session))
		if got := postRaw(tc.path, f); got != 400 {
			t.Fatalf("%s missing nonce status=%d", tc.path, got)
		}
		f.Set(formNonceField, randomToken()[:32])
		f.Set("csrf", "bad-csrf")
		if got := postRaw(tc.path, f); got != 403 {
			t.Fatalf("%s bad CSRF status=%d", tc.path, got)
		}
	}
	if n := submissionCount(t, e); n != 0 {
		t.Fatalf("refused raw forms wrote %d receipts", n)
	}
}
