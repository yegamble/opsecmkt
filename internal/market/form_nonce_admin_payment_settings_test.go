package market

import (
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func renderedCurrencyIntakeNonce(t *testing.T, body, currency string) string {
	t.Helper()
	forms := regexp.MustCompile(`(?s)<form class="stack" method="post" action="/admin/payment-intake">(.*?)</form>`).FindAllStringSubmatch(body, -1)
	noncePattern := regexp.MustCompile(`name="form_nonce" value="([0-9a-f]{32})"`)
	for _, form := range forms {
		if !strings.Contains(form[1], `name="currency" value="`+currency+`"`) {
			continue
		}
		if nonce := noncePattern.FindStringSubmatch(form[1]); nonce != nil {
			return nonce[1]
		}
	}
	t.Fatalf("payment intake form for %s was not rendered with a nonce", currency)
	return ""
}

func copyFormValues(src url.Values) url.Values {
	dst := url.Values{}
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func TestEscrowReleaseDaysConfirmedReceiptAndLostAcknowledgement(t *testing.T) {
	x := newEscrowEnv(t)
	adminID, admin := x.user("nonce_days_admin", "admin")
	otherSession := agSession(x.testEnv, adminID)
	page := x.page("/admin", admin)
	firstNonce := renderedFormNonce(t, page, "/admin/escrow-release-days")
	if got := submissionCount(t, x.testEnv); got != 0 {
		t.Fatalf("admin GET wrote %d receipts", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID); got != 0 {
		t.Fatalf("admin GET wrote %d audit rows", got)
	}
	firstForm := url.Values{"release_days": {"21"}, "password": {testPassword}, formNonceField: {firstNonce}}
	lost := loseNextActionCommit(t, x.testEnv)
	first := x.do("POST", "/admin/escrow-release-days", admin, firstForm)
	if lost.Load() {
		t.Fatal("release-days Commit did not reach the injected acknowledgement loss")
	}
	if first.Code != 500 || first.Header().Get("Retry-After") != "" || !strings.Contains(first.Body.String(), "Escrow release deadline setting outcome unknown") || !strings.Contains(first.Body.String(), "may have been saved") || !strings.Contains(first.Body.String(), "resubmit it unchanged") {
		t.Fatalf("release-days lost-ack response: status=%d retry=%q body=%s", first.Code, first.Header().Get("Retry-After"), first.Body)
	}
	if got := agStr(x.testEnv, "SELECT value FROM settings WHERE key=$1", escrowReleaseDaysSetting); got != "21" {
		t.Fatalf("release deadline after lost acknowledgement = %q", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Changed the Bitcoin escrow release deadline %'", adminID); got != 1 {
		t.Fatalf("lost-ack release audit rows=%d, want 1", got)
	}
	replay := x.do("POST", "/admin/escrow-release-days", admin, firstForm)
	x.check(replay, 303)
	if replay.Header().Get("Location") != "/admin#escrow-release-days" || x.cookie(replay, "notice") != "escrow-release-days" {
		t.Fatalf("release-days replay did not return saved response: location=%q notice=%q", replay.Header().Get("Location"), x.cookie(replay, "notice"))
	}

	newerPage := x.page("/admin", admin)
	newerNonce := renderedFormNonce(t, newerPage, "/admin/escrow-release-days")
	newerForm := url.Values{"release_days": {"28"}, "password": {testPassword}, formNonceField: {newerNonce}}
	x.check(x.do("POST", "/admin/escrow-release-days", admin, newerForm), 303)
	beforeReplayAudits := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID)
	beforeReplayNotices := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1", adminID)
	x.check(x.do("POST", "/admin/escrow-release-days", admin, firstForm), 303)
	if got := agStr(x.testEnv, "SELECT value FROM settings WHERE key=$1", escrowReleaseDaysSetting); got != "28" {
		t.Fatalf("old receipt reverted newer release deadline: %q", got)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID) != beforeReplayAudits || agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1", adminID) != beforeReplayNotices {
		t.Fatal("old release-days receipt repeated activity")
	}
	changed := copyFormValues(firstForm)
	changed.Set("release_days", "22")
	x.check(x.do("POST", "/admin/escrow-release-days", admin, changed), 409)
	x.check(x.do("POST", "/admin/escrow-release-days", otherSession, firstForm), 409)

	// A no-op refusal rolls its nonce back; the same rendered form may be corrected and reused.
	guardPage := x.page("/admin", admin)
	guard := url.Values{"release_days": {"28"}, "password": {testPassword}, formNonceField: {renderedFormNonce(t, guardPage, "/admin/escrow-release-days")}}
	x.check(x.do("POST", "/admin/escrow-release-days", admin, guard), 409)
	guard.Set("release_days", "29")
	x.check(x.do("POST", "/admin/escrow-release-days", admin, guard), 303)
	if got := agStr(x.testEnv, "SELECT value FROM settings WHERE key=$1", escrowReleaseDaysSetting); got != "29" {
		t.Fatalf("corrected release deadline = %q", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Changed the Bitcoin escrow release deadline %'", adminID); got != 3 {
		t.Fatalf("release deadline audit rows=%d, want 3 actual changes", got)
	}
}

func TestPaymentIntakeConfirmedReceiptAndLostAcknowledgement(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("nonce_intake_admin", "admin")
	otherSession := agSession(e, adminID)
	page := e.do("GET", "/admin", admin, nil)
	e.check(page, 200)
	firstNonce := renderedCurrencyIntakeNonce(t, page.Body.String(), "BTC")
	xmrNonce := renderedCurrencyIntakeNonce(t, page.Body.String(), "XMR")
	if firstNonce == xmrNonce {
		t.Fatal("BTC and XMR forms reused a nonce")
	}
	if got := submissionCount(t, e); got != 0 || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID) != 0 {
		t.Fatalf("admin GET wrote receipts=%d or audit rows", got)
	}
	firstForm := url.Values{"currency": {"BTC"}, "enabled": {"false"}, "password": {testPassword}, formNonceField: {firstNonce}}
	lost := loseNextActionCommit(t, e)
	first := e.do("POST", "/admin/payment-intake", admin, firstForm)
	if lost.Load() {
		t.Fatal("payment-intake Commit did not reach the injected acknowledgement loss")
	}
	if first.Code != 500 || first.Header().Get("Retry-After") != "" || !strings.Contains(first.Body.String(), "Payment intake setting outcome unknown") || !strings.Contains(first.Body.String(), "may have been saved") || !strings.Contains(first.Body.String(), "resubmit it unchanged") {
		t.Fatalf("payment-intake lost-ack response: status=%d retry=%q body=%s", first.Code, first.Header().Get("Retry-After"), first.Body)
	}
	if got := agStr(e, "SELECT value FROM settings WHERE key=$1", paymentIntakeKey("BTC")); got != intakePaused {
		t.Fatalf("BTC intake after lost acknowledgement = %q", got)
	}
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Paused new BTC escrow checkouts%'", adminID); got != 1 {
		t.Fatalf("lost-ack intake audit rows=%d, want 1", got)
	}
	replay := e.do("POST", "/admin/payment-intake", admin, firstForm)
	e.check(replay, 303)
	if replay.Header().Get("Location") != "/admin#payment-intake" || noticeSetCookie(replay) == nil || noticeSetCookie(replay).Value != "intake-paused" {
		t.Fatalf("payment-intake replay did not return saved response: location=%q notice=%v", replay.Header().Get("Location"), noticeSetCookie(replay))
	}

	newerPage := e.do("GET", "/admin", admin, nil)
	e.check(newerPage, 200)
	newerForm := url.Values{"currency": {"BTC"}, "enabled": {intakeEscrow}, "password": {testPassword}, formNonceField: {renderedCurrencyIntakeNonce(t, newerPage.Body.String(), "BTC")}}
	e.check(e.do("POST", "/admin/payment-intake", admin, newerForm), 303)
	beforeReplayAudits := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID)
	beforeReplayNotices := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", adminID)
	e.check(e.do("POST", "/admin/payment-intake", admin, firstForm), 303)
	if got := agStr(e, "SELECT value FROM settings WHERE key=$1", paymentIntakeKey("BTC")); got != intakeEscrow {
		t.Fatalf("old receipt reverted newer BTC intake: %q", got)
	}
	if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID) != beforeReplayAudits || agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", adminID) != beforeReplayNotices {
		t.Fatal("old payment-intake receipt repeated activity")
	}
	changed := copyFormValues(firstForm)
	changed.Set("enabled", intakeEscrow)
	e.check(e.do("POST", "/admin/payment-intake", admin, changed), 409)
	e.check(e.do("POST", "/admin/payment-intake", otherSession, firstForm), 409)
}

func TestPaymentIntakeInvalidChoiceRollsBackNonceForCorrection(t *testing.T) {
	e := newTestApp(t)
	id, admin := e.user("nonce_intake_invalid", "admin")
	page := e.do("GET", "/admin", admin, nil)
	e.check(page, 200)
	form := url.Values{"currency": {"XMR"}, "enabled": {"true"}, "password": {testPassword}, formNonceField: {renderedCurrencyIntakeNonce(t, page.Body.String(), "XMR")}}
	if w := e.do("POST", "/admin/payment-intake", admin, form); w.Code != 400 {
		t.Fatalf("invalid legacy choice: %d %s", w.Code, w.Body)
	}
	if agInt(e, "SELECT count(*) FROM form_submissions WHERE nonce=$1", form.Get(formNonceField)) != 0 {
		t.Fatal("invalid choice retained receipt")
	}
	form.Set("enabled", intakePaused)
	e.check(e.do("POST", "/admin/payment-intake", admin, form), 303)
	if agStr(e, "SELECT value FROM settings WHERE key=$1", paymentIntakeKey("XMR")) != intakePaused || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", id) != 1 {
		t.Fatal("corrected payment-intake form did not save once")
	}
}

func TestAdminPaymentSettingFormsKeepTOTPGuardAndNonceRollback(t *testing.T) {
	t.Run("release deadline", func(t *testing.T) {
		x := newEscrowEnv(t)
		adminID, admin := x.user("nonce_days_totp", "admin")
		key, _ := agEnableTOTP(x.testEnv, adminID)
		page := x.page("/admin", admin)
		form := url.Values{"release_days": {"21"}, "password": {testPassword}, "code": {x.wrongTOTP(adminID)}, formNonceField: {renderedFormNonce(t, page, "/admin/escrow-release-days")}}
		if w := x.do("POST", "/admin/escrow-release-days", admin, form); w.Code != 401 || !strings.Contains(w.Body.String(), "Verification code incorrect") {
			t.Fatalf("wrong TOTP: %d %s", w.Code, w.Body)
		}
		if agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE nonce=$1", form.Get(formNonceField)) != 0 {
			t.Fatal("wrong TOTP retained a receipt")
		}
		form.Set("code", agTOTPNow(key))
		savedCode := form.Get("code")
		auditsBefore := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID)
		first := x.do("POST", "/admin/escrow-release-days", admin, form)
		x.check(first, 303)
		if agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID) != auditsBefore+1 || agStr(x.testEnv, "SELECT value FROM settings WHERE key=$1", escrowReleaseDaysSetting) != "21" {
			t.Fatal("release-days action did not persist exactly one confirmed change")
		}
		replay := x.do("POST", "/admin/escrow-release-days", admin, form)
		x.check(replay, 303)
		if agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID) != auditsBefore+1 || agStr(x.testEnv, "SELECT value FROM settings WHERE key=$1", escrowReleaseDaysSetting) != "21" {
			t.Fatal("exact release-days replay changed state or repeated its audit")
		}
		receipt := agStr(x.testEnv, "SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", form.Get(formNonceField))
		if strings.Contains(receipt, savedCode) {
			t.Fatal("release-days receipt retained the authenticator code")
		}
		freshPage := x.page("/admin", admin)
		fresh := url.Values{"release_days": {"22"}, "password": {testPassword}, "code": {savedCode}, formNonceField: {renderedFormNonce(t, freshPage, "/admin/escrow-release-days")}}
		if w := x.do("POST", "/admin/escrow-release-days", admin, fresh); w.Code != 401 || !strings.Contains(w.Body.String(), "already used") {
			t.Fatalf("fresh release-days change reused a spent TOTP: %d %s", w.Code, w.Body)
		}
	})
	t.Run("payment intake", func(t *testing.T) {
		e := newTestApp(t)
		adminID, admin := e.user("nonce_intake_totp", "admin")
		key, _ := agEnableTOTP(e, adminID)
		page := e.do("GET", "/admin", admin, nil)
		e.check(page, 200)
		form := url.Values{"currency": {"BTC"}, "enabled": {intakePaused}, "password": {testPassword}, "code": {e.wrongTOTP(adminID)}, formNonceField: {renderedCurrencyIntakeNonce(t, page.Body.String(), "BTC")}}
		if w := e.do("POST", "/admin/payment-intake", admin, form); w.Code != 401 || !strings.Contains(w.Body.String(), "Verification code incorrect") {
			t.Fatalf("wrong TOTP: %d %s", w.Code, w.Body)
		}
		if agInt(e, "SELECT count(*) FROM form_submissions WHERE nonce=$1", form.Get(formNonceField)) != 0 {
			t.Fatal("wrong TOTP retained a receipt")
		}
		form.Set("code", agTOTPNow(key))
		savedCode := form.Get("code")
		auditsBefore := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID)
		first := e.do("POST", "/admin/payment-intake", admin, form)
		e.check(first, 303)
		if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID) != auditsBefore+1 || agStr(e, "SELECT value FROM settings WHERE key=$1", paymentIntakeKey("BTC")) != intakePaused {
			t.Fatal("payment-intake action did not persist exactly one confirmed change")
		}
		replay := e.do("POST", "/admin/payment-intake", admin, form)
		e.check(replay, 303)
		if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID) != auditsBefore+1 || agStr(e, "SELECT value FROM settings WHERE key=$1", paymentIntakeKey("BTC")) != intakePaused {
			t.Fatal("exact payment-intake replay changed state or repeated its audit")
		}
		freshPage := e.do("GET", "/admin", admin, nil)
		e.check(freshPage, 200)
		fresh := url.Values{"currency": {"BTC"}, "enabled": {intakeEscrow}, "password": {testPassword}, "code": {savedCode}, formNonceField: {renderedCurrencyIntakeNonce(t, freshPage.Body.String(), "BTC")}}
		if w := e.do("POST", "/admin/payment-intake", admin, fresh); w.Code != 401 || !strings.Contains(w.Body.String(), "already used") {
			t.Fatalf("fresh payment-intake change reused a spent TOTP: %d %s", w.Code, w.Body)
		}
	})
}

func TestAdminPaymentSettingReceiptsRecheckCurrentActor(t *testing.T) {
	for _, route := range []struct {
		name  string
		path  string
		setup func(*testing.T) (*testEnv, string, string, url.Values)
	}{
		{
			name: "release deadline",
			path: "/admin/escrow-release-days",
			setup: func(t *testing.T) (*testEnv, string, string, url.Values) {
				x := newEscrowEnv(t)
				id, admin := x.user("nonce_days_actor_guard", "admin")
				form := url.Values{"release_days": {"21"}, "password": {testPassword}, formNonceField: {renderedFormNonce(t, x.page("/admin", admin), "/admin/escrow-release-days")}}
				return x.testEnv, id, admin, form
			},
		},
		{
			name: "payment intake",
			path: "/admin/payment-intake",
			setup: func(t *testing.T) (*testEnv, string, string, url.Values) {
				e := newTestApp(t)
				id, admin := e.user("nonce_intake_actor_guard", "admin")
				page := e.do("GET", "/admin", admin, nil)
				e.check(page, 200)
				form := url.Values{"currency": {"BTC"}, "enabled": {intakePaused}, "password": {testPassword}, formNonceField: {renderedCurrencyIntakeNonce(t, page.Body.String(), "BTC")}}
				return e, id, admin, form
			},
		},
	} {
		for _, guard := range []struct {
			name   string
			change func(t *testing.T, e *testEnv, userID string) []string
			want   int
		}{
			{name: "password changed", want: 401, change: func(t *testing.T, _ *testEnv, _ string) []string {
				hash, err := hashPassword("a-different-long-password")
				if err != nil {
					t.Fatal(err)
				}
				return []string{"UPDATE users SET password_hash='" + hash + "' WHERE id=$1"}
			}},
			{name: "demoted", want: 403, change: func(_ *testing.T, _ *testEnv, _ string) []string {
				return []string{"UPDATE users SET role='buyer' WHERE id=$1", "DELETE FROM sessions WHERE user_id=$1"}
			}},
			{name: "suspended", want: 403, change: func(_ *testing.T, _ *testEnv, _ string) []string {
				return []string{"UPDATE users SET suspended_at=now() WHERE id=$1", "DELETE FROM sessions WHERE user_id=$1"}
			}},
			{name: "session revoked", want: 401, change: func(_ *testing.T, _ *testEnv, _ string) []string {
				return []string{"DELETE FROM sessions WHERE user_id=$1"}
			}},
		} {
			t.Run(route.name+"/"+guard.name, func(t *testing.T) {
				e, adminID, session, form := route.setup(t)
				updates := guard.change(t, e, adminID)
				w := besideAccountChange(t, e, adminID, updates, func() *httptest.ResponseRecorder {
					return e.do("POST", route.path, session, form)
				})
				if w.Code != guard.want {
					t.Fatalf("stale actor response=%d body=%q, want %d", w.Code, w.Body.String(), guard.want)
				}
				if got := agInt(e, "SELECT count(*) FROM form_submissions WHERE nonce=$1", form.Get(formNonceField)); got != 0 {
					t.Fatalf("stale actor retained a receipt: %d", got)
				}
				if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID); got != 0 {
					t.Fatalf("stale actor wrote audit rows: %d", got)
				}
				if route.name == "release deadline" {
					if got := agStr(e, "SELECT COALESCE((SELECT value FROM settings WHERE key=$1), '14')", escrowReleaseDaysSetting); got != "14" {
						t.Fatalf("stale actor changed release deadline: %q", got)
					}
				} else if got := agStr(e, "SELECT value FROM settings WHERE key=$1", paymentIntakeKey("BTC")); got != intakeEscrow {
					t.Fatalf("stale actor changed BTC intake: %q", got)
				}
			})
		}
	}
}
