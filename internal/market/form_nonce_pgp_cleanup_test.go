package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestPGP2FAConfirmedReceiptDoesNotReenableAfterDisable(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("nonce_pgp_owner", "buyer")
	_, pub := testPGPKey(t, "nonce_pgp_owner")
	agEnablePGP(e, uid, pub)
	agExec(e, "UPDATE users SET pgp_2fa=false WHERE id=$1", uid)
	otherSession := agSession(e, uid)

	page := e.do("GET", "/pgp", session, nil)
	e.check(page, 200)
	enableNonce := renderedFormNonce(t, page.Body.String(), "/pgp/2fa")
	if got := submissionCount(t, e); got != 0 {
		t.Fatalf("PGP GET wrote %d receipts", got)
	}
	enable := url.Values{"enable": {"1"}, "password": {testPassword}, formNonceField: {enableNonce}}
	first := e.do("POST", "/pgp/2fa", session, enable)
	e.check(first, 303)
	if !strings.Contains(first.Header().Get("Location"), "/pgp") || agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND pgp_2fa", uid) != 1 {
		t.Fatal("PGP sign-in was not enabled")
	}
	enableAudits := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Turned on PGP sign-in verification (fingerprint % (confirmed with password)'", uid)
	enableNotices := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'PGP sign-in verification was turned on%'", uid)
	if enableAudits != 1 || enableNotices != 1 {
		t.Fatalf("enable wrote audits=%d notices=%d, want one each", enableAudits, enableNotices)
	}
	replay := e.do("POST", "/pgp/2fa", session, enable)
	e.check(replay, 303)
	if first.Header().Get("Location") != replay.Header().Get("Location") || noticeSetCookie(first) == nil || noticeSetCookie(replay) == nil || noticeSetCookie(first).Value != noticeSetCookie(replay).Value {
		t.Fatalf("exact enable replay differs: first=%q replay=%q", first.Header().Get("Location"), replay.Header().Get("Location"))
	}
	changed := url.Values{}
	for key, values := range enable {
		changed[key] = append([]string(nil), values...)
	}
	changed.Set("enable", "0")
	e.check(e.do("POST", "/pgp/2fa", session, changed), 409)
	e.check(e.do("POST", "/pgp/2fa", otherSession, enable), 409)
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND pgp_2fa", uid) != 1 || agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'PGP sign-in verification was turned on%'", uid) != 1 {
		t.Fatal("changed or other-session replay changed the enabled factor")
	}

	disablePage := e.do("GET", "/pgp", session, nil)
	e.check(disablePage, 200)
	disableNonce := renderedFormNonce(t, disablePage.Body.String(), "/pgp/2fa")
	disable := url.Values{"enable": {"0"}, "password": {testPassword}, formNonceField: {disableNonce}}
	e.check(e.do("POST", "/pgp/2fa", session, disable), 303)
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND pgp_2fa", uid) != 0 {
		t.Fatal("legitimate disable did not turn PGP sign-in off")
	}
	beforeReplayAudits := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", uid)
	beforeReplayNotices := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", uid)
	// This is the old enable request after a separate, legitimate disable. The receipt returns
	// its saved response without re-running the mutation or sending the notification again.
	e.check(e.do("POST", "/pgp/2fa", session, enable), 303)
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND pgp_2fa", uid) != 0 ||
		agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", uid) != beforeReplayAudits ||
		agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", uid) != beforeReplayNotices {
		t.Fatal("old enable receipt re-enabled PGP sign-in or repeated its audit/notification")
	}

	freshPage := e.do("GET", "/pgp", session, nil)
	e.check(freshPage, 200)
	freshEnable := url.Values{"enable": {"1"}, "password": {testPassword}, formNonceField: {renderedFormNonce(t, freshPage.Body.String(), "/pgp/2fa")}}
	agExec(e, "UPDATE users SET pgp_verified_at=NULL WHERE id=$1", uid)
	if w := e.do("POST", "/pgp/2fa", session, freshEnable); w.Code != 409 || !strings.Contains(w.Body.String(), "Verify ownership") {
		t.Fatalf("fresh enable without ownership proof: %d %s", w.Code, w.Body)
	}
	if got := agInt(e, "SELECT count(*) FROM form_submissions WHERE nonce=$1", freshEnable.Get(formNonceField)); got != 0 {
		t.Fatalf("refused fresh enable kept %d receipt(s)", got)
	}
	agExec(e, "UPDATE users SET pgp_verified_at=now() WHERE id=$1", uid)
	e.check(e.do("POST", "/pgp/2fa", session, freshEnable), 303)
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND pgp_2fa", uid) != 1 {
		t.Fatal("same fresh enable form did not work after ownership proof was restored")
	}
	if got := agInt(e, "SELECT count(*) FROM form_submissions WHERE action_path='/pgp/2fa'"); got != 3 {
		t.Fatalf("PGP setting receipts=%d, want enable, disable, fresh enable", got)
	}
	var saved string
	if err := e.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", enableNonce).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testPassword, pub, session} {
		if strings.Contains(saved, secret) {
			t.Fatalf("PGP receipt retained raw submitted data")
		}
	}
}

func TestPGP2FALostCommitAcknowledgementReplaysSavedResponse(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("nonce_pgp_lost_ack", "vendor")
	_, pub := testPGPKey(t, "nonce_pgp_lost_ack")
	agEnablePGP(e, uid, pub)
	agExec(e, "UPDATE users SET pgp_2fa=false WHERE id=$1", uid)
	page := e.do("GET", "/pgp", session, nil)
	e.check(page, 200)
	form := url.Values{"enable": {"1"}, "password": {testPassword}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/pgp/2fa")}}
	lost := loseNextActionCommit(t, e)
	w := e.do("POST", "/pgp/2fa", session, form)
	if lost.Load() {
		t.Fatal("PGP sign-in Commit did not reach the injected acknowledgement loss")
	}
	if w.Code != 500 || w.Header().Get("Retry-After") != "" || !strings.Contains(w.Body.String(), "PGP sign-in setting outcome unknown") || !strings.Contains(w.Body.String(), "may have been saved") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
		t.Fatalf("PGP lost-ack response: status=%d retry=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body)
	}
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND pgp_2fa", uid) != 1 ||
		agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Turned on PGP sign-in verification (fingerprint % (confirmed with password)'", uid) != 1 ||
		agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'PGP sign-in verification was turned on%'", uid) != 1 {
		t.Fatal("lost acknowledgement did not commit the setting, audit and notification exactly once")
	}
	replay := e.do("POST", "/pgp/2fa", session, form)
	e.check(replay, 303)
	if replay.Header().Get("Location") != "/pgp" || noticeSetCookie(replay) == nil || noticeSetCookie(replay).Value != "pgp-2fa-on" {
		t.Fatalf("unchanged PGP form did not retrieve saved response: status=%d location=%q notice=%v", replay.Code, replay.Header().Get("Location"), noticeSetCookie(replay))
	}
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND pgp_2fa", uid) != 1 ||
		agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Turned on PGP sign-in verification (fingerprint % (confirmed with password)'", uid) != 1 ||
		agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'PGP sign-in verification was turned on%'", uid) != 1 {
		t.Fatal("saved PGP replay repeated the mutation, audit or notification")
	}
}

func TestDeliveryCleanupReceiptSurvivesRestoreAndNewDeliveryText(t *testing.T) {
	e := newTestApp(t)
	owner, session := cleanupDemote(t, e, "nonce_cleanup_owner", "buyer")
	product := cleanupListing(t, e, owner, "Archived private download", "old-delivery-template-secret")
	otherSession := agSession(e, owner)
	page := e.do("GET", "/delivery-cleanup", session, nil)
	e.check(page, 200)
	if strings.Contains(page.Body.String(), "old-delivery-template-secret") {
		t.Fatal("cleanup page rendered delivery text")
	}
	nonce := renderedFormNonce(t, page.Body.String(), "/delivery-cleanup")
	if got := submissionCount(t, e); got != 0 {
		t.Fatalf("cleanup GET wrote %d receipts", got)
	}
	form := url.Values{"id": {product}, "password": {testPassword}, formNonceField: {nonce}}
	lost := loseNextActionCommit(t, e)
	first := e.do("POST", "/delivery-cleanup", session, form)
	if lost.Load() {
		t.Fatal("cleanup Commit did not reach the injected acknowledgement loss")
	}
	if first.Code != 500 || first.Header().Get("Retry-After") != "" || !strings.Contains(first.Body.String(), "Delivery cleanup outcome unknown") || !strings.Contains(first.Body.String(), "may have been saved") || !strings.Contains(first.Body.String(), "resubmit it unchanged") {
		t.Fatalf("cleanup lost-ack response: status=%d retry=%q body=%s", first.Code, first.Header().Get("Retry-After"), first.Body)
	}
	if got := agStr(e, "SELECT delivery_content FROM products WHERE id=$1", product); got != "" {
		t.Fatalf("cleanup did not commit after lost acknowledgement: %q", got)
	}
	checkEffects := func(wantAudits, wantNotices int) {
		t.Helper()
		if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Cleared stored automatic delivery from archived listing %'", owner); got != wantAudits {
			t.Fatalf("cleanup audits=%d, want %d", got, wantAudits)
		}
		if got := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Stored automatic-delivery text was cleared from archived listing %'", owner); got != wantNotices {
			t.Fatalf("cleanup notices=%d, want %d", got, wantNotices)
		}
	}
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Cleared stored automatic delivery from archived listing %'", owner); got != 1 {
		t.Fatalf("lost-ack cleanup audits=%d, want 1", got)
	}
	if got := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Stored automatic-delivery text was cleared from archived listing %'", owner); got != 1 {
		t.Fatalf("lost-ack cleanup notices=%d, want 1", got)
	}
	replay := e.do("POST", "/delivery-cleanup", session, form)
	e.check(replay, 303)
	if replay.Header().Get("Location") != "/delivery-cleanup" || noticeSetCookie(replay) == nil || noticeSetCookie(replay).Value != "delivery-cleared" {
		t.Fatalf("unchanged cleanup form did not retrieve saved response: location=%q notice=%v", replay.Header().Get("Location"), noticeSetCookie(replay))
	}
	checkEffects(1, 1)
	changed := url.Values{}
	for key, values := range form {
		changed[key] = append([]string(nil), values...)
	}
	changed.Set("extra", "different request")
	e.check(e.do("POST", "/delivery-cleanup", session, changed), 409)
	e.check(e.do("POST", "/delivery-cleanup", otherSession, form), 409)
	checkEffects(1, 1)

	// The old receipt is a saved historical response: replay after restore must not clear new text.
	if _, err := e.DB.Exec("UPDATE products SET archived=false,archived_at=NULL,delivery_content=$2 WHERE id=$1", product, "new-delivery-template-secret"); err != nil {
		t.Fatal(err)
	}
	e.check(e.do("POST", "/delivery-cleanup", session, form), 303)
	if got := agStr(e, "SELECT delivery_content FROM products WHERE id=$1", product); got != "new-delivery-template-secret" {
		t.Fatalf("old receipt replay changed new delivery text: %q", got)
	}
	checkEffects(1, 1)

	fresh := url.Values{}
	for key, values := range form {
		fresh[key] = append([]string(nil), values...)
	}
	fresh.Set(formNonceField, randomToken()[:32])
	if w := e.do("POST", "/delivery-cleanup", session, fresh); w.Code != 404 {
		t.Fatalf("fresh cleanup request for restored listing: %d %s", w.Code, w.Body)
	}
	if got := agStr(e, "SELECT delivery_content FROM products WHERE id=$1", product); got != "new-delivery-template-secret" {
		t.Fatalf("fresh refused request changed new delivery text: %q", got)
	}
	checkEffects(1, 1)
	if got := agInt(e, "SELECT count(*) FROM form_submissions WHERE action_path='/delivery-cleanup'"); got != 1 {
		t.Fatalf("cleanup receipts=%d, want exactly the original committed action", got)
	}
	newOwner, _ := e.user("nonce_cleanup_new_owner", "vendor")
	if _, err := e.DB.Exec("UPDATE products SET vendor_id=$2 WHERE id=$1", product, newOwner); err != nil {
		t.Fatal(err)
	}
	if w := e.do("POST", "/delivery-cleanup", session, form); w.Code != 404 {
		t.Fatalf("old receipt after ownership reassignment: %d %s", w.Code, w.Body)
	}
	if got := agStr(e, "SELECT delivery_content FROM products WHERE id=$1", product); got != "new-delivery-template-secret" {
		t.Fatalf("old owner replay changed reassigned delivery text: %q", got)
	}
	checkEffects(1, 1)
	var receipt string
	if err := e.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", nonce).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testPassword, "old-delivery-template-secret", "new-delivery-template-secret", session} {
		if strings.Contains(receipt, secret) {
			t.Fatal("cleanup receipt retained a password, delivery text or session token")
		}
	}
}
