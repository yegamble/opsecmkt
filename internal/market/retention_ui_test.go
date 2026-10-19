package market

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func uiRetentionEnv(t *testing.T) (*testEnv, retentionConfig) {
	t.Helper()
	e := newTestApp(t)
	cfg := retentionConfig{Enabled: true, StateDir: filepath.Join(t.TempDir(), "private-state")}
	if err := e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	return e, cfg
}

func TestRetentionDeleteRequiresConfirmationAndPreservesReplay(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	uid, session := e.user("retention_request_user", "buyer")
	page := e.body("GET", "/account", session, nil, 200)
	mustContain(t, page, "Request account deletion", "Retention maintenance is enabled")
	form := url.Values{"confirm_delete": {"yes"}, "password": {"wrong-password"}}
	e.check(e.do("POST", "/account/delete", session, form), 401)
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND deletion_requested_at IS NOT NULL`, uid) != 0 {
		t.Fatal("bad password accepted deletion")
	}
	form.Set("password", testPassword)
	form.Set(formNonceField, renderedFormNonce(t, page, "/account/delete"))
	e.check(e.do("POST", "/account/delete", session, form), 303)
	e.check(e.do("POST", "/account/delete", session, form), 303)
	if agInt(e, `SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Requested account deletion%'`, uid) != 1 {
		t.Fatal("deletion replay recorded another request")
	}
	mustContain(t, e.body("GET", "/account", session, nil, 200), "Your deletion request was accepted")
}

func TestRetentionDeleteKeepsSoleAdministrator(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	uid, session := e.user("retention_only_admin", "admin")
	w := e.do("POST", "/account/delete", session, url.Values{"confirm_delete": {"yes"}, "password": {testPassword}})
	e.check(w, 409)
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND deletion_requested_at IS NOT NULL`, uid) != 0 {
		t.Fatal("sole administrator deletion accepted")
	}
}

func TestRetentionHoldIsPrivateConfirmedAndReplayable(t *testing.T) {
	e, cfg := uiRetentionEnv(t)
	_, admin := e.user("retention_hold_admin", "admin")
	uid, buyer := e.user("retention_held_buyer", "buyer")
	form := url.Values{"scope": {"account"}, "record_id": {uid}, "authority": {"Specific preservation obligation"}, "reason": {"Necessary case records"}, "readers": {"Assigned case handlers"}, "release_condition": {"Written confirmation that case ended"}, "confirm_hold": {"yes"}, "password": {testPassword}}
	e.check(e.do("POST", "/admin/retention/hold", buyer, form), 403)
	page := e.body("GET", "/admin/system", admin, nil, 200)
	form.Set(formNonceField, renderedFormNonce(t, page, "/admin/retention/hold"))
	e.check(e.do("POST", "/admin/retention/hold", admin, form), 303)
	e.check(e.do("POST", "/admin/retention/hold", admin, form), 303)
	if agInt(e, `SELECT count(*) FROM retention_holds WHERE record_id=$1 AND released_at IS NULL`, uid) != 1 {
		t.Fatal("hold replay duplicated preservation")
	}
	if strings.Contains(e.body("GET", "/canary", buyer, nil, 200), "Necessary case records") {
		t.Fatal("public disclosure leaked preservation metadata")
	}
	holdID := agStr(e, `SELECT id FROM retention_holds WHERE record_id=$1`, uid)
	e.check(e.do("POST", "/admin/retention/release", admin, url.Values{"hold_id": {holdID}, "confirm_release": {"yes"}, "password": {testPassword}}), 303)
	if err := e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM retention_holds WHERE record_id=$1 AND released_at IS NOT NULL`, uid) != 1 {
		t.Fatal("released hold returned on restart")
	}
}

func TestRetentionFailedAuditDoesNotReplayDeletion(t *testing.T) {
	e, cfg := uiRetentionEnv(t)
	uid, session := e.user("retention_rollback_user", "buyer")
	agExec(e, `CREATE FUNCTION retention_test_refuse_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action LIKE 'Requested account deletion%' THEN RAISE EXCEPTION 'synthetic audit failure'; END IF; RETURN NEW; END $$`)
	agExec(e, `CREATE TRIGGER retention_test_refuse_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION retention_test_refuse_audit()`)
	e.check(e.do("POST", "/account/delete", session, url.Values{"confirm_delete": {"yes"}, "password": {testPassword}}), 500)
	if err := e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND deletion_requested_at IS NOT NULL`, uid) != 0 {
		t.Fatal("rolled-back deletion request returned on replay")
	}
}

func TestRetentionClosedAccountRejectsOldSessionAndPassword(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	uid, session := e.user("retention_closed_user", "buyer")
	agExec(e, `UPDATE users SET closed_at=now() WHERE id=$1`, uid)
	user, err := e.A.sessionUser(t.Context(), session)
	if err != nil || user != nil {
		t.Fatalf("closed account authenticated: %v %v", user, err)
	}
	w := e.do("POST", "/login", session, url.Values{"handle": {"retention_closed_user"}, "password": {testPassword}})
	e.check(w, 401)
}

func TestRetentionRestrictedAccountSettlesButCannotStartNewObligations(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	uid, session := e.user("retention_restricted_buyer", "buyer")
	vendor, _ := e.user("retention_restricted_vendor", "vendor")
	product := e.product(vendor, "physical")
	agExec(e, `UPDATE users SET deletion_requested_at=now()-interval '31 days',retention_restricted_at=now() WHERE id=$1`, uid)
	e.check(e.do("GET", "/orders", session, nil), 200)
	e.check(e.do("POST", "/orders", session, url.Values{"id": {product}, "currency": {"XMR"}}), 403)
	_, public := testPGPKey(t, "retention_rotation")
	e.check(e.do("POST", "/account", session, url.Values{"pgp": {public}, "password": {"wrong-password"}}), 401)
	e.check(e.do("POST", "/account", session, url.Values{"pgp": {public}, "password": {testPassword}}), 303)
	if agStr(e, `SELECT pgp FROM users WHERE id=$1`, uid) == "" || agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND deletion_requested_at IS NOT NULL`, uid) != 1 {
		t.Fatal("PGP security update was lost or cancelled the deletion request")
	}
	if _, err := e.DB.Exec(`INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES($1,$2,$3,'XMR',100000,'draft')`, randomToken(), uid, product); err == nil {
		t.Fatal("database accepted a new obligation from a stale restricted session")
	}
	if !retentionPageAllowed("/escrow/psbt") || !retentionActionAllowed("/escrow/sign") || retentionActionAllowed("/escrow/checkout") {
		t.Fatal("restriction prevents settlement or allows checkout")
	}
}

func TestRetentionRestrictedStaffOnlyReadTheirOwnDisputes(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	staffID, staff := e.user("retention_restricted_staff", "admin")
	buyer, _ := e.user("retention_unrelated_buyer", "buyer")
	vendor, _ := e.user("retention_unrelated_vendor", "vendor")
	order := e.order(buyer, e.product(vendor, "physical"), "XMR", stateDisputed)
	agExec(e, `INSERT INTO disputes(id,order_id,reason,status) VALUES($1,$2,'Unrelated private dispute evidence','Open')`, randomToken(), order)
	own := e.order(staffID, e.product(vendor, "physical"), "XMR", stateDisputed)
	agExec(e, `INSERT INTO disputes(id,order_id,reason,status) VALUES($1,$2,'Own dispute evidence','Open')`, randomToken(), own)
	e.check(e.do("GET", "/order?id="+order, staff, nil), 200)
	agExec(e, `UPDATE users SET deletion_requested_at=now()-interval '31 days',retention_restricted_at=now() WHERE id=$1`, staffID)
	e.check(e.do("GET", "/admin", staff, nil), 403)
	e.check(e.do("GET", "/order?id="+order, staff, nil), 404)
	e.check(e.do("GET", "/order?id="+own, staff, nil), 200)
	page := e.body("GET", "/disputes", staff, nil, 200)
	mustContain(t, page, "Own dispute evidence")
	if strings.Contains(page, "Unrelated private dispute evidence") {
		t.Fatal("restricted staff retained unrelated dispute access")
	}
}
