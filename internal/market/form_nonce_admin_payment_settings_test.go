package market

import (
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

func TestPaymentIntakeConfirmedReceiptAndLostAcknowledgement(t *testing.T) {
	e := newFreshTestApp(t)
	adminID, admin := e.user("nonce_intake_admin", "admin")
	otherSession := agSession(e, adminID)
	page := e.do("GET", "/admin", admin, nil)
	e.check(page, 200)
	firstNonce := renderedCurrencyIntakeNonce(t, page.Body.String(), "XMR")
	if strings.Contains(page.Body.String(), `name="currency" value="BTC"`) {
		t.Fatal("Bitcoin intake form is still rendered")
	}
	if got := submissionCount(t, e); got != 0 || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID) != 0 {
		t.Fatalf("admin GET wrote receipts=%d or audit rows", got)
	}
	firstForm := url.Values{"currency": {"XMR"}, "enabled": {"false"}, "password": {testPassword}, formNonceField: {firstNonce}}
	lost := loseNextActionCommit(t, e)
	first := e.do("POST", "/admin/payment-intake", admin, firstForm)
	if lost.Load() {
		t.Fatal("payment-intake Commit did not reach the injected acknowledgement loss")
	}
	if first.Code != 500 || first.Header().Get("Retry-After") != "" || !strings.Contains(first.Body.String(), "Payment intake setting outcome unknown") || !strings.Contains(first.Body.String(), "may have been saved") || !strings.Contains(first.Body.String(), "resubmit it unchanged") {
		t.Fatalf("payment-intake lost-ack response: status=%d retry=%q body=%s", first.Code, first.Header().Get("Retry-After"), first.Body)
	}
	if got := agStr(e, "SELECT value FROM settings WHERE key=$1", paymentIntakeKey("XMR")); got != intakePaused {
		t.Fatalf("XMR intake after lost acknowledgement = %q", got)
	}
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Paused new XMR escrow checkouts%'", adminID); got != 1 {
		t.Fatalf("lost-ack intake audit rows=%d, want 1", got)
	}
	replay := e.do("POST", "/admin/payment-intake", admin, firstForm)
	e.check(replay, 303)
	if replay.Header().Get("Location") != "/admin#payment-intake" || noticeSetCookie(replay) == nil || noticeSetCookie(replay).Value != "intake-paused" {
		t.Fatalf("payment-intake replay did not return saved response: location=%q notice=%v", replay.Header().Get("Location"), noticeSetCookie(replay))
	}

	newerPage := e.do("GET", "/admin", admin, nil)
	e.check(newerPage, 200)
	newerForm := url.Values{"currency": {"XMR"}, "enabled": {intakeEscrow}, "password": {testPassword}, formNonceField: {renderedCurrencyIntakeNonce(t, newerPage.Body.String(), "XMR")}}
	e.check(e.do("POST", "/admin/payment-intake", admin, newerForm), 303)
	beforeReplayAudits := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", adminID)
	beforeReplayNotices := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", adminID)
	e.check(e.do("POST", "/admin/payment-intake", admin, firstForm), 303)
	if got := agStr(e, "SELECT value FROM settings WHERE key=$1", paymentIntakeKey("XMR")); got != intakeEscrow {
		t.Fatalf("old receipt reverted newer XMR intake: %q", got)
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
	e := newFreshTestApp(t)
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
