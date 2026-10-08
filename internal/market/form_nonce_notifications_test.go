package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestFormNonceNotificationFormsRenderUniqueNoncesWithoutGETWrites(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("nonce_notification_forms", "buyer")
	if _, err := e.DB.Exec(`INSERT INTO notifications(id,user_id,body) VALUES
		('notification-form-old',$1,'older'),('notification-form-new',$1,'newer')`, uid); err != nil {
		t.Fatal(err)
	}
	if got := submissionCount(t, e); got != 0 {
		t.Fatalf("fixture creation wrote %d receipts", got)
	}
	w := e.do("GET", "/notifications", session, nil)
	e.check(w, 200)
	nonces := renderedActionNonces(t, w.Body.String(), "/notifications")
	if len(nonces) != 3 {
		t.Fatalf("rendered %d nonces for mark-all and two per-item forms", len(nonces))
	}
	if got := submissionCount(t, e); got != 0 {
		t.Fatalf("rendering notifications wrote %d receipts", got)
	}
}

func TestFormNonceNotificationMarkReadReplaysAndBindsActorSessionAndBody(t *testing.T) {
	e := newTestApp(t)
	ownerID, owner := e.user("nonce_notification_owner", "buyer")
	otherID, other := e.user("nonce_notification_other", "buyer")
	if _, err := e.DB.Exec(`INSERT INTO notifications(id,user_id,body) VALUES
		('notification-own',$1,'for owner'),('notification-other',$2,'for other')`, ownerID, otherID); err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "/notifications", owner, nil)
	e.check(w, 200)
	nonces := renderedActionNonces(t, w.Body.String(), "/notifications")
	if len(nonces) != 2 {
		t.Fatalf("owner page rendered %d forms; want mark-all and one item", len(nonces))
	}
	form := url.Values{"id": {"notification-own"}, formNonceField: {nonces[1]}}
	first := e.do("POST", "/notifications", owner, form)
	e.check(first, 303)
	replay := e.do("POST", "/notifications", owner, form)
	e.check(replay, 303)
	if replay.Header().Get("Location") != first.Header().Get("Location") ||
		noticeSetCookie(replay) != nil || noticeSetCookie(first) != nil {
		t.Fatal("exact replay did not preserve the original redirect and empty notice")
	}
	if got := agInt(e, "SELECT count(*) FROM notifications WHERE id='notification-own' AND is_read"); got != 1 {
		t.Fatalf("owner row read state count = %d, want one", got)
	}
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Marked notification read'", ownerID); got != 1 {
		t.Fatalf("read audit count after replay = %d, want one", got)
	}
	if got := agInt(e, "SELECT count(*) FROM form_submissions WHERE nonce=$1", nonces[1]); got != 1 {
		t.Fatalf("receipt count = %d, want one", got)
	}

	changed := url.Values{"id": {"notification-other"}, formNonceField: {nonces[1]}}
	bodyMismatch := e.do("POST", "/notifications", owner, changed)
	e.check(bodyMismatch, 409)
	otherSession := randomToken()
	if _, err := e.DB.Exec("INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '1 hour')", digest(otherSession), ownerID); err != nil {
		t.Fatal(err)
	}
	sessionMismatch := e.do("POST", "/notifications", otherSession, form)
	e.check(sessionMismatch, 409)
	actorMismatch := e.do("POST", "/notifications", other, form)
	e.check(actorMismatch, 409)
	if bodyMismatch.Body.String() != sessionMismatch.Body.String() || bodyMismatch.Body.String() != actorMismatch.Body.String() {
		t.Fatal("nonce mismatch response reveals whether another account owns the receipt")
	}
	if got := agStr(e, "SELECT is_read::text FROM notifications WHERE id='notification-other'"); got != "false" {
		t.Fatalf("mismatched request changed other account's row: is_read=%s", got)
	}
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Marked notification read'", ownerID); got != 1 {
		t.Fatalf("mismatches wrote extra audit rows: %d", got)
	}
}

func TestFormNonceNotificationMarkAllReplayDoesNotReadLaterRows(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("nonce_notification_all", "buyer")
	if _, err := e.DB.Exec(`INSERT INTO notifications(id,user_id,body) VALUES
		('notification-all-a',$1,'first'),('notification-all-b',$1,'second')`, uid); err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "/notifications", session, nil)
	e.check(w, 200)
	nonces := renderedActionNonces(t, w.Body.String(), "/notifications")
	if len(nonces) != 3 {
		t.Fatalf("rendered %d forms, want mark-all and two per-item", len(nonces))
	}
	form := url.Values{"mark_all": {"1"}, formNonceField: {nonces[0]}}
	first := e.do("POST", "/notifications", session, form)
	e.check(first, 303)
	if _, err := e.DB.Exec("INSERT INTO notifications(id,user_id,body) VALUES('notification-after-all',$1,'later')", uid); err != nil {
		t.Fatal(err)
	}
	replay := e.do("POST", "/notifications", session, form)
	e.check(replay, 303)
	if replay.Header().Get("Location") != first.Header().Get("Location") ||
		noticeSetCookie(replay) != nil || noticeSetCookie(first) != nil {
		t.Fatal("mark-all replay changed its original response")
	}
	if got := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND is_read", uid); got != 2 {
		t.Fatalf("mark-all replay read %d rows, want only the two from its first request", got)
	}
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Marked all notifications read'", uid); got != 1 {
		t.Fatalf("mark-all audit count = %d, want one", got)
	}
	changed := url.Values{"id": {"notification-after-all"}, formNonceField: {nonces[0]}}
	if mismatch := e.do("POST", "/notifications", session, changed); mismatch.Code != 409 {
		t.Fatalf("changed-body replay status = %d, want 409: %s", mismatch.Code, mismatch.Body.String())
	}
	if got := agStr(e, "SELECT is_read::text FROM notifications WHERE id='notification-after-all'"); got != "false" {
		t.Fatalf("changed-body replay read later row: %s", got)
	}
}

func TestFormNonceNotificationReadRollbackCanRetrySameForm(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("nonce_notification_rollback", "buyer")
	if _, err := e.DB.Exec("INSERT INTO notifications(id,user_id,body) VALUES('notification-rollback',$1,'retry me')", uid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`CREATE FUNCTION a130_reject_notification_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.action='Marked notification read' THEN RAISE EXCEPTION 'test-only notification audit refusal'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER a130_reject_notification_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION a130_reject_notification_audit()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.DB.Exec("DROP TRIGGER IF EXISTS a130_reject_notification_audit ON audit_events")
		_, _ = e.DB.Exec("DROP FUNCTION IF EXISTS a130_reject_notification_audit()")
	})
	form := url.Values{"id": {"notification-rollback"}, formNonceField: {randomToken()[:32]}}
	failed := e.do("POST", "/notifications", session, form)
	if failed.Code != 500 {
		t.Fatalf("forced audit failure status = %d, want 500: %s", failed.Code, failed.Body.String())
	}
	if got := agStr(e, "SELECT is_read::text FROM notifications WHERE id='notification-rollback'"); got != "false" {
		t.Fatalf("failed transaction left is_read=%s", got)
	}
	if got := submissionCount(t, e); got != 0 {
		t.Fatalf("failed transaction retained %d receipts", got)
	}
	if _, err := e.DB.Exec("DROP TRIGGER a130_reject_notification_audit ON audit_events; DROP FUNCTION a130_reject_notification_audit()"); err != nil {
		t.Fatal(err)
	}
	retry := e.do("POST", "/notifications", session, form)
	e.check(retry, 303)
	if got := agStr(e, "SELECT is_read::text FROM notifications WHERE id='notification-rollback'"); got != "true" {
		t.Fatalf("retry did not mark notification read: is_read=%s", got)
	}
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Marked notification read'", uid); got != 1 {
		t.Fatalf("retry wrote %d read audits, want one", got)
	}
	if got := agInt(e, "SELECT count(*) FROM form_submissions WHERE nonce=$1", form.Get(formNonceField)); got != 1 {
		t.Fatalf("retry stored %d receipts, want one", got)
	}
	if strings.Contains(failed.Body.String(), "test-only") {
		t.Fatal("database failure detail leaked to response")
	}
}
