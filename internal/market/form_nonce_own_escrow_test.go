package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestA130EscrowKeyDynamicReceiptDoesNotRestoreOlderKeyOrCallProvider(t *testing.T) {
	e, core, _ := escrowTestApp(t)
	uid, session := e.user("own_escrow_key", "buyer")
	page := e.do("GET", "/escrow-key", session, nil)
	e.check(page, 200)
	form := url.Values{"key": {testBIP48Key}, "password": {testPassword}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/escrow-key")}}
	e.check(e.do("POST", "/escrow-key", session, form), 303)
	if n := agInt(e, "SELECT count(*) FROM form_submissions WHERE nonce=$1 AND confirmation_required", form.Get(formNonceField)); n != 1 {
		t.Fatalf("registered key lost original password mode: %d", n)
	}
	e.check(e.do("POST", "/escrow-key", session, url.Values{"key": {testBIP84Key}, "dedicated": {"1"}, "password": {testPassword}}), 303)
	beforeKey := agStr(e, "SELECT xpub_hmac FROM user_escrow_keys WHERE user_id=$1", uid)
	beforeAudit := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE '%Bitcoin escrow key%'", uid)
	beforeRPC := len(core.methods())
	e.A.escrow.Store(nil) // configured feature, temporarily unavailable provider
	e.check(e.do("POST", "/escrow-key", session, form), 303)
	if agStr(e, "SELECT xpub_hmac FROM user_escrow_keys WHERE user_id=$1", uid) != beforeKey ||
		agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE '%Bitcoin escrow key%'", uid) != beforeAudit ||
		len(core.methods()) != beforeRPC {
		t.Fatal("old key replay restored a key, duplicated audit, or reached Core")
	}
	other := agSession(e, uid)
	e.check(e.do("POST", "/escrow-key", other, form), 409)
}

func TestA130PracticeReceiptReplaysWithoutRPCOrSecondActivation(t *testing.T) {
	e, core, srv := escrowTestApp(t)
	uid, session := e.user("own_escrow_practice", "buyer")
	if code, body := registerKey(e, session, testBIP48Key); code != 303 {
		t.Fatalf("register key: %d %s", code, body)
	}
	psbt := fakeOwnerSign(t, practicePSBT(t, e, session), "7450f935", 1)
	page := e.do("GET", "/escrow-key?practice=1", session, nil)
	e.check(page, 200)
	form := url.Values{"psbt": {psbt}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/escrow-key/practice")}}
	e.check(e.do("POST", "/escrow-key/practice", session, form), 303)
	activeAt := agStr(e, "SELECT active_at::text FROM user_escrow_keys WHERE user_id=$1", uid)
	auditBefore := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Activated Bitcoin escrow key%'", uid)
	beforeRPC := len(core.methods())
	srv.down.Store(true)
	e.check(e.do("POST", "/escrow-key/practice", session, form), 303)
	if agStr(e, "SELECT active_at::text FROM user_escrow_keys WHERE user_id=$1", uid) != activeAt ||
		agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Activated Bitcoin escrow key%'", uid) != auditBefore ||
		len(core.methods()) != beforeRPC {
		t.Fatal("practice replay changed activation or called an unavailable Core")
	}
	var saved string
	if err := e.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", form.Get(formNonceField)).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(saved, psbt[:48]) || strings.Contains(saved, session) {
		t.Fatal("practice receipt retained PSBT or raw session")
	}
}
