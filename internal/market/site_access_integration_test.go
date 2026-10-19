package market

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func accessSettingsState(e *testEnv) string {
	e.t.Helper()
	return agStr(e, `SELECT coalesce(string_agg(key||'='||value, ',' ORDER BY key),'') FROM settings WHERE key IN ('site_access','registrations')`)
}

func accessSaveForm(t *testing.T, e *testEnv, session, mode, registrations string) url.Values {
	t.Helper()
	page := e.do("GET", "/admin/security", session, nil)
	e.check(page, http.StatusOK)
	return url.Values{"site_access": {mode}, "registrations": {registrations}, "password": {testPassword}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/admin/access")}}
}

func TestSiteAccessIndependentVisibilityAndRegistrationPolicies(t *testing.T) {
	for _, mode := range []string{"public", "members"} {
		for _, registrations := range []string{"open", "closed"} {
			t.Run(mode+"/"+registrations, func(t *testing.T) {
				t.Setenv("INITIAL_SITE_ACCESS", mode)
				t.Setenv("INITIAL_REGISTRATIONS", registrations)
				e := newTestApp(t)
				vendorID, _ := e.user("access_vendor", "vendor")
				_, member := e.user("access_member", "buyer")
				product := e.product(vendorID, "physical")
				anon := randomToken()
				for _, path := range []string{"/", "/product?id=" + product} {
					w := e.do("GET", path, anon, nil)
					if mode == "public" {
						e.check(w, http.StatusOK)
						if !strings.Contains(w.Body.String(), "Test listing") {
							t.Fatal("public listing was not rendered")
						}
					} else {
						location := "/login"
						if path != "/" {
							location += "?next=" + url.QueryEscape(path)
						}
						if w.Code != http.StatusSeeOther || w.Header().Get("Location") != location || strings.Contains(w.Body.String(), "Test listing") {
							t.Fatalf("members-only guest content: status=%d location=%q", w.Code, w.Header().Get("Location"))
						}
					}
					e.check(e.do("GET", path, member, nil), http.StatusOK)
				}
				page := e.do("GET", "/register", anon, nil)
				registrationStatus := http.StatusOK
				if registrations == "closed" {
					registrationStatus = http.StatusForbidden
				}
				e.check(page, registrationStatus)
				if got := strings.Contains(page.Body.String(), `action="/register"`); got != (registrations == "open") {
					t.Fatal("registration form did not follow its independent setting")
				}
				beforeUsers := agInt(e, "SELECT count(*) FROM users")
				beforeSessions := agInt(e, "SELECT count(*) FROM sessions")
				beforeAudits := agInt(e, "SELECT count(*) FROM audit_events")
				w := e.do("POST", "/register", anon, url.Values{"handle": {"access_new_member"}, "password": {testPassword}})
				if registrations == "open" {
					e.check(w, http.StatusSeeOther)
					newSession := e.session(w)
					e.check(e.do("GET", "/", newSession, nil), http.StatusOK)
					if agInt(e, "SELECT count(*) FROM users WHERE handle='access_new_member' AND role='buyer'") != 1 || agInt(e, "SELECT count(*) FROM users") != beforeUsers+1 || agInt(e, "SELECT count(*) FROM sessions") != beforeSessions+1 || agInt(e, "SELECT count(*) FROM audit_events WHERE action='Account created: buyer'") != 1 {
						t.Fatal("open registration did not create exactly one buyer and session")
					}
				} else {
					e.check(w, http.StatusForbidden)
					if !strings.Contains(w.Body.String(), registrationClosedMessage) || agInt(e, "SELECT count(*) FROM users") != beforeUsers || agInt(e, "SELECT count(*) FROM sessions") != beforeSessions || agInt(e, "SELECT count(*) FROM audit_events") != beforeAudits || submissionCount(t, e) != 0 {
						t.Fatal("closed registration changed accounts, sessions, audit or form receipts")
					}
					// Malformed forms still fail the database-free request precheck.
					e.check(e.do("POST", "/register", anon, url.Values{"handle": {"!"}}), http.StatusBadRequest)
				}
				login := e.do("POST", "/login", randomToken(), url.Values{"handle": {"access_member"}, "password": {testPassword}})
				e.check(login, http.StatusSeeOther)
				e.check(e.do("GET", "/account", e.session(login), nil), http.StatusOK)
			})
		}
	}
}

func TestSiteAccessAdministratorAuthorizationConfirmationAndReplay(t *testing.T) {
	t.Setenv("INITIAL_SITE_ACCESS", "public")
	t.Setenv("INITIAL_REGISTRATIONS", "open")
	e := newTestApp(t)
	adminID, admin := e.user("access_admin", "admin")
	unchanged := func() {
		t.Helper()
		if accessSettingsState(e) != "registrations=open,site_access=public" || len(e.audits(adminID)) != 0 || submissionCount(t, e) != 0 {
			t.Fatal("refused change saved policy, audit or nonce receipt")
		}
	}
	for _, role := range []string{"buyer", "vendor", "moderator"} {
		_, session := e.user("access_"+role, role)
		e.check(e.do("POST", "/admin/access", session, url.Values{"site_access": {"members"}, "registrations": {"closed"}, "password": {testPassword}}), http.StatusForbidden)
		unchanged()
	}
	e.check(e.do("POST", "/admin/access", randomToken(), url.Values{"site_access": {"members"}, "registrations": {"closed"}, "password": {testPassword}}), http.StatusUnauthorized)
	unchanged()
	f := accessSaveForm(t, e, admin, "members", "closed")
	f.Set("csrf", "forged")
	r := httptest.NewRequest(http.MethodPost, "/admin/access", strings.NewReader(f.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: admin})
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	e.check(w, http.StatusForbidden)
	unchanged()
	for _, tc := range []struct {
		password string
		status   int
	}{{"", http.StatusBadRequest}, {"incorrect-password", http.StatusUnauthorized}} {
		f.Set("password", tc.password)
		e.check(e.do("POST", "/admin/access", admin, f), tc.status)
		unchanged()
	}
	f.Set("password", testPassword)
	f.Set(formNonceField, "")
	e.check(e.do("POST", "/admin/access", admin, f), http.StatusBadRequest)
	unchanged()
	f = accessSaveForm(t, e, admin, "private", "closed")
	e.check(e.do("POST", "/admin/access", admin, f), http.StatusBadRequest)
	unchanged()
	key, _ := agEnableTOTP(e, adminID)
	f = accessSaveForm(t, e, admin, "members", "closed")
	adminForm := adminForm(t, e.body("GET", "/admin/security", admin, nil, http.StatusOK), "/admin/access", "")
	mustContain(t, adminForm, `name="password" required`, `name="code" required`)
	good := agTOTPNow(key)
	bad := e.wrongTOTP(adminID)
	for _, code := range []string{"", bad} {
		f.Set("code", code)
		e.check(e.do("POST", "/admin/access", admin, f), http.StatusUnauthorized)
		unchanged()
	}
	f.Set("code", good)
	first := e.do("POST", "/admin/access", admin, f)
	e.check(first, http.StatusSeeOther)
	if first.Header().Get("Location") != "/admin/security#access" || noticeSetCookie(first) == nil || noticeSetCookie(first).Value != "access" || accessSettingsState(e) != "registrations=closed,site_access=members" {
		t.Fatal("confirmed setting did not redirect, notify and persist")
	}
	wantAudit := "Changed site access from public to members; registrations from open to closed (confirmed with password and authenticator code)"
	if !e.auditExact(adminID, wantAudit) || len(e.audits(adminID)) != 1 || submissionCount(t, e) != 1 {
		t.Fatal("confirmed policy did not write exactly one audit and receipt")
	}
	// An exact retry returns its saved result despite its already-spent authenticator code.
	e.check(e.do("POST", "/admin/access", admin, f), http.StatusSeeOther)
	f.Set("registrations", "open")
	e.check(e.do("POST", "/admin/access", admin, f), http.StatusConflict)
	f.Set(formNonceField, formNonceFor("/admin/access"))
	e.check(e.do("POST", "/admin/access", admin, f), http.StatusUnauthorized)
	if accessSettingsState(e) != "registrations=closed,site_access=members" || len(e.audits(adminID)) != 1 || submissionCount(t, e) != 1 {
		t.Fatal("replay or reused code changed policy or duplicated audit/receipt")
	}
	receipts := agStr(e, "SELECT string_agg(s::text,'') FROM form_submissions s")
	if strings.Contains(receipts, testPassword) || strings.Contains(receipts, good) {
		t.Fatal("confirmation secrets retained in form receipt")
	}
}

func TestSiteAccessSavedChoicesSurviveRestartAndAreSharedBetweenApps(t *testing.T) {
	t.Setenv("INITIAL_SITE_ACCESS", "members")
	t.Setenv("INITIAL_REGISTRATIONS", "closed")
	e := newTestApp(t)
	_, admin := e.user("persistent_access_admin", "admin")
	t.Setenv("INITIAL_SITE_ACCESS", "public")
	t.Setenv("INITIAL_REGISTRATIONS", "open")
	e.restart()
	if accessSettingsState(e) != "registrations=closed,site_access=members" {
		t.Fatal("restart overrode saved policy with initial defaults")
	}
	otherApp, err := New(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer otherApp.Close()
	other := &testEnv{t: t, A: otherApp, DB: otherApp.db}
	e.check(e.do("POST", "/admin/access", admin, accessSaveForm(t, e, admin, "public", "open")), http.StatusSeeOther)
	other.check(other.do("GET", "/", randomToken(), nil), http.StatusOK)
	if accessSettingsState(other) != "registrations=open,site_access=public" {
		t.Fatal("second process did not observe administrator change")
	}
	t.Setenv("INITIAL_SITE_ACCESS", "members")
	t.Setenv("INITIAL_REGISTRATIONS", "closed")
	e.restart()
	if accessSettingsState(e) != "registrations=open,site_access=public" {
		t.Fatal("administrator choices did not survive restart")
	}
	// Fill one missing key while preserving the other existing value.
	agExec(e, "DELETE FROM settings WHERE key='registrations'")
	e.restart()
	if accessSettingsState(e) != "registrations=closed,site_access=public" {
		t.Fatal("initialization did not fill only the missing setting")
	}
}

func TestSiteAccessClosedRegistrationDoesNotPreventFirstAdministratorSetup(t *testing.T) {
	t.Setenv("INITIAL_SITE_ACCESS", "members")
	t.Setenv("INITIAL_REGISTRATIONS", "closed")
	e := newTestApp(t)
	e.check(e.do("GET", "/setup", randomToken(), nil), http.StatusOK)
	admin := e.setup()
	e.check(e.do("GET", "/admin", admin, nil), http.StatusOK)
	if agInt(e, "SELECT count(*) FROM users WHERE role='admin'") != 1 || accessSettingsState(e) != "registrations=closed,site_access=members" {
		t.Fatal("first setup changed policy or failed to create administrator")
	}
	agExpect(e, "/register", randomToken(), url.Values{"handle": {"blocked_after_setup"}, "password": {testPassword}}, http.StatusForbidden, registrationClosedMessage)
}

func accessWaitForLock(t *testing.T, e *testEnv, lock int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if agInt(e, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=$1 AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`, lock) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("request never waited on advisory lock %d", lock)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSiteAccessClosingRefusesStaleRegistrationBeforeAccountCreation(t *testing.T) {
	t.Setenv("INITIAL_SITE_ACCESS", "public")
	t.Setenv("INITIAL_REGISTRATIONS", "open")
	e := newTestApp(t)
	_, admin := e.user("close_race_admin", "admin")
	anon := randomToken()
	e.check(e.do("GET", "/register", anon, nil), http.StatusOK)
	gate, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	if _, err = gate.ExecContext(t.Context(), "SELECT pg_advisory_xact_lock(782493)"); err != nil {
		t.Fatal(err)
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- e.do("POST", "/register", anon, url.Values{"handle": {"late_registration"}, "password": {testPassword}})
	}()
	accessWaitForLock(t, e, 782493) // passed the early open check, now waits before its final authorization
	e.check(e.do("POST", "/admin/access", admin, accessSaveForm(t, e, admin, "public", "closed")), http.StatusSeeOther)
	if err = gate.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-result:
		e.check(w, http.StatusForbidden)
		if !strings.Contains(w.Body.String(), registrationClosedMessage) {
			t.Fatal("stale form did not explain closure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale registration did not finish")
	}
	if agInt(e, "SELECT count(*) FROM users WHERE handle='late_registration'") != 0 || agInt(e, "SELECT count(*) FROM audit_events WHERE action='Account created: buyer'") != 0 || agInt(e, "SELECT count(*) FROM sessions") != 1 || submissionCount(t, e) != 1 {
		t.Fatal("registration committed after closure or created an extra receipt")
	}
}

func TestSiteAccessClosingWaitsForAuthorizedRegistrationCommit(t *testing.T) {
	t.Setenv("INITIAL_SITE_ACCESS", "public")
	t.Setenv("INITIAL_REGISTRATIONS", "open")
	e := newTestApp(t)
	_, admin := e.user("waiting_close_admin", "admin")
	const pauseLock = 198273642
	gate, err := e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	if _, err = gate.ExecContext(t.Context(), "SELECT pg_advisory_xact_lock($1)", pauseLock); err != nil {
		t.Fatal(err)
	}
	agExec(e, fmt.Sprintf(`CREATE FUNCTION pause_access_registration() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.handle='authorized_registration' THEN PERFORM pg_advisory_xact_lock(%d); END IF; RETURN NEW; END $$;
CREATE TRIGGER pause_access_registration BEFORE INSERT ON users FOR EACH ROW EXECUTE FUNCTION pause_access_registration()`, pauseLock))
	registered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		registered <- e.do("POST", "/register", randomToken(), url.Values{"handle": {"authorized_registration"}, "password": {testPassword}})
	}()
	accessWaitForLock(t, e, pauseLock) // real registration holds its shared access lock through its pending insert
	closeForm := accessSaveForm(t, e, admin, "public", "closed")
	closed := make(chan *httptest.ResponseRecorder, 1)
	go func() { closed <- e.do("POST", "/admin/access", admin, closeForm) }()
	accessWaitForLock(t, e, siteAccessLock)
	select {
	case w := <-closed:
		t.Fatalf("closure returned %d before authorized registration committed", w.Code)
	default:
	}
	if err = gate.Rollback(); err != nil {
		t.Fatal(err)
	}
	for _, result := range []chan *httptest.ResponseRecorder{registered, closed} {
		select {
		case w := <-result:
			e.check(w, http.StatusSeeOther)
		case <-time.After(5 * time.Second):
			t.Fatal("registration/closure deadlocked")
		}
	}
	agExpect(e, "/register", randomToken(), url.Values{"handle": {"after_closed"}, "password": {testPassword}}, http.StatusForbidden, registrationClosedMessage)
	if agInt(e, "SELECT count(*) FROM users WHERE handle='authorized_registration'") != 1 || agInt(e, "SELECT count(*) FROM users WHERE handle='after_closed'") != 0 || accessSettingsState(e) != "registrations=closed,site_access=public" {
		t.Fatal("closure did not serialize with account creation")
	}
}

func TestSiteAccessRuntimeMissingOrInvalidSettingsFailClosed(t *testing.T) {
	for _, missing := range []bool{true, false} {
		t.Run(fmt.Sprintf("missing=%v", missing), func(t *testing.T) {
			t.Setenv("INITIAL_SITE_ACCESS", "public")
			t.Setenv("INITIAL_REGISTRATIONS", "open")
			e := newTestApp(t)
			e.user("access_fail_closed_member", "buyer")
			if missing {
				agExec(e, "DELETE FROM settings WHERE key IN ('site_access','registrations')")
			} else {
				agExec(e, "UPDATE settings SET value='unexpected' WHERE key IN ('site_access','registrations')")
			}
			anon := randomToken()
			w := e.do("GET", "/", anon, nil)
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
				t.Fatal("missing or malformed runtime settings reopened browsing")
			}
			e.check(e.do("GET", "/register", anon, nil), http.StatusForbidden)
			agExpect(e, "/register", anon, url.Values{"handle": {"fail_closed_registration"}, "password": {testPassword}}, http.StatusForbidden, registrationClosedMessage)
			if agInt(e, "SELECT count(*) FROM users") != 1 || agInt(e, "SELECT count(*) FROM sessions") != 1 || agInt(e, "SELECT count(*) FROM audit_events") != 0 {
				t.Fatal("failed-closed registration mutated account state")
			}
			login := e.do("POST", "/login", anon, url.Values{"handle": {"access_fail_closed_member"}, "password": {testPassword}})
			e.check(login, http.StatusSeeOther)
			e.check(e.do("GET", "/", e.session(login), nil), http.StatusOK)
		})
	}
}

func TestSiteAccessAuditFailureRollsBackPolicyAndNonce(t *testing.T) {
	t.Setenv("INITIAL_SITE_ACCESS", "public")
	t.Setenv("INITIAL_REGISTRATIONS", "open")
	e := newTestApp(t)
	adminID, admin := e.user("access_audit_failure_admin", "admin")
	f := accessSaveForm(t, e, admin, "members", "closed")
	agExec(e, `CREATE FUNCTION refuse_access_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action LIKE 'Changed site access %' THEN RAISE EXCEPTION 'synthetic audit failure'; END IF; RETURN NEW; END $$;
CREATE TRIGGER refuse_access_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION refuse_access_audit()`)
	e.check(e.do("POST", "/admin/access", admin, f), http.StatusInternalServerError)
	if accessSettingsState(e) != "registrations=open,site_access=public" || len(e.audits(adminID)) != 0 || submissionCount(t, e) != 0 {
		t.Fatal("audit failure left a partial policy change or consumed the retry nonce")
	}
	agExec(e, "DROP TRIGGER refuse_access_audit ON audit_events")
	e.check(e.do("POST", "/admin/access", admin, f), http.StatusSeeOther)
	if accessSettingsState(e) != "registrations=closed,site_access=members" || len(e.audits(adminID)) != 1 || submissionCount(t, e) != 1 {
		t.Fatal("corrected unchanged form did not save once after rollback")
	}
}
