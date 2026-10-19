package market

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A-546: a ban is an administrator's permanent closure of an account (owner decision 2026-10-18). DB integration
// tests against the simulated Monero wallet RPC, not chain evidence.

const (
	bannedMsg      = "This account is closed and cannot sign in. Ask the operator of this market."
	testBanReason  = "Took payment for three orders and shipped nothing."
	testLiftReason = "Appeal upheld: the parcels were lost in the post."
)

func banForm(action, handle, reason string, extra ...string) url.Values {
	return form(append([]string{"action", action, "handle", handle, "reason", reason, "password", testPassword}, extra...)...)
}

func TestBanAccount(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	e := x.testEnv
	adminID, admin := e.user("ban_admin", "admin")
	admin2ID, admin2 := e.user("ban_admin2", "admin")
	_, moderator := e.user("ban_moderator", "moderator")
	vendorID, vendor := e.user("ban_vendor", "vendor")
	other := agSession(e, vendorID)
	agPending(e, vendorID)
	buyerID, buyer := e.user("ban_buyer", "buyer")
	agExec(e, "UPDATE users SET vendor_verified=true,vendor_verified_set_by=$2,vendor_verified_set_at=now() WHERE id=$1", vendorID, admin2ID)

	// Two shipped, funded escrow orders with the vendor, each disputed by the buyer, and a draft not yet checked out.
	funded := func() (string, string) {
		order := e.order(buyerID, x.physical(vendorID), "XMR", stateDraft)
		e.check(x.checkout(buyer, order, testXMRAddr("ban refund "+order, true)), 303)
		address := agStr(e, "SELECT address FROM escrows WHERE order_id=$1", order)
		x.fake.payAddress(address, hex64("ban funding "+order), xmrListingPrice, 10)
		x.mustPoll()
		if code, body := x.xmrShip(order, vendor, testXMRAddr("ban claim "+order, true), testPassword); code != 303 {
			t.Fatalf("ship escrow before the ban: %d %s", code, body)
		}
		agExpect(e, "/disputes", buyer, form("order_id", order, "reason", "The parcel never arrived at the agreed drop."), 303, "")
		return order, agStr(e, "SELECT id FROM disputes WHERE order_id=$1", order)
	}
	resolvedOrder, resolvedDispute := funded()
	concededOrder, concededDispute := funded()
	draft := e.order(buyerID, x.physical(vendorID), "XMR", stateDraft)
	listings := agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1 AND NOT archived", vendorID)

	// A buyer with an open vendor application.
	setVendorPolicy(e, "0", "manual", "")
	applicantID, applicant := e.user("ban_applicant", "buyer")
	e.check(e.do("POST", "/account/vendor-application", applicant, url.Values{"action": {"apply"}}), 303)

	page := e.body("GET", "/admin/users", admin, nil, 200)
	f := adminForm(t, page, "/admin/ban", "")
	mustContain(t, f, `name="handle"`, `name="reason" required minlength="10" maxlength="500"`, `name="password" required`, `value="ban"`, `value="unban"`)
	mustContain(t, page, `id="ban"`, "A ban is permanent", "No account is banned.")

	before := agInt(e, "SELECT count(*) FROM audit_events")
	unchanged := func(when string) {
		t.Helper()
		if agInt(e, "SELECT count(*) FROM users WHERE banned_at IS NOT NULL OR suspended_at IS NOT NULL") != 0 || agUserSessions(e, vendorID) != 2 ||
			agInt(e, "SELECT count(*) FROM audit_events") != before {
			t.Fatalf("%s: a refused ban changed state", when)
		}
	}
	// Only administrators; a reason of 10–500 characters; the handle must exist.
	agExpect(e, "/admin/ban", moderator, banForm("ban", "ban_vendor", testBanReason), 403, "")
	agExpect(e, "/admin/ban", vendor, banForm("ban", "ban_buyer", testBanReason), 403, "")
	agExpect(e, "/admin/ban", admin, banForm("ban", "ban_vendor", ""), 400, "Give a reason of 10–500 characters")
	agExpect(e, "/admin/ban", admin, banForm("ban", "ban_vendor", "   too short   "), 400, "Give a reason of 10–500 characters")
	agExpect(e, "/admin/ban", admin, banForm("ban", "ban_vendor", strings.Repeat("x", 501)), 400, "Give a reason of 10–500 characters")
	agExpect(e, "/admin/ban", admin, banForm("ban", "ban_vendor", "Line one\u202e and a text-direction override."), 400, "Give a reason of 10–500 characters")
	agExpect(e, "/admin/ban", admin, banForm("ban", "ban_vendor", "Line one\nand line two."), 400, "Give a reason of 10–500 characters")
	agExpect(e, "/admin/ban", admin, banForm("close", "ban_vendor", testBanReason), 400, "Choose ban or unban")
	w := e.do("POST", "/admin/ban", admin, banForm("ban", "no_such_account", testBanReason))
	if w.Code != 404 {
		t.Fatalf("unknown handle: %d", w.Code)
	}
	mustContain(t, adminForm(t, w.Body.String(), "/admin/ban", ""), `value="no_such_account"`, testBanReason)
	// Not their own account, another administrator, or the last administrator.
	agExpect(e, "/admin/ban", admin, banForm("ban", "ban_admin", testBanReason), 400, "You cannot ban your own account.")
	agExpect(e, "/admin/ban", admin, banForm("ban", "ban_admin2", testBanReason), 403, "Administrator accounts cannot be banned")
	agExec(e, "UPDATE users SET role='buyer' WHERE role='admin' AND id<>$1", adminID)
	agExpect(e, "/admin/ban", admin, banForm("ban", "ban_admin", testBanReason), 400, "You cannot ban your own account.")
	agExec(e, "UPDATE users SET role='admin' WHERE id IN ($1,$2)", admin2ID, x.admin)
	// The administrator's own password (and authenticator code when enrolled).
	agExpect(e, "/admin/ban", admin, form("action", "ban", "handle", "ban_vendor", "reason", testBanReason), 400, "Enter your current password")
	agExpect(e, "/admin/ban", admin, form("action", "ban", "handle", "ban_vendor", "reason", testBanReason, "password", "wrong-password-here"), 401, "Password incorrect")
	unchanged("refusals")

	// The ban.
	w = e.do("POST", "/admin/ban", admin, banForm("ban", " ban_vendor ", testBanReason))
	if w.Code != 303 || w.Header().Get("Location") != "/admin/users#ban" {
		t.Fatalf("ban: %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	agExpect(e, "/admin/ban", admin, banForm("ban", "ban_vendor", testBanReason), 409, "ban_vendor is already banned")
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND banned_at IS NOT NULL AND banned_by=$2 AND ban_reason=$3 AND suspended_at IS NOT NULL", vendorID, adminID, testBanReason) != 1 {
		t.Fatal("ban not stored")
	}
	if agUserSessions(e, vendorID) != 0 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", vendorID) != 0 || !signedOut(e, vendor) || !signedOut(e, other) {
		t.Fatal("sessions or pending sign-ins survived the ban")
	}
	if agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1 AND NOT archived", vendorID) != 0 {
		t.Fatal("listings not archived")
	}
	if agStr(e, "SELECT vendor_verified::text FROM users WHERE id=$1", vendorID) != "false" || agStr(e, "SELECT vendor_verified_set_by FROM users WHERE id=$1", vendorID) != adminID {
		t.Fatal("verified vendor mark not removed")
	}
	archived := strconv.Itoa(listings)
	for who, row := range map[string]string{
		adminID:  "Banned account ban_vendor: " + testBanReason + "; ended 2 session(s) and any pending sign-ins; archived " + archived + " active listing(s); removed the verified vendor mark (confirmed with password)",
		vendorID: "Account banned by administrator ban_admin: " + testBanReason + "; ended 2 session(s) and any pending sign-ins",
	} {
		if !e.auditExact(who, row) {
			t.Fatalf("missing audit row %q", row)
		}
	}
	if !e.auditExact(vendorID, "Archived "+archived+" active listing(s): account banned by administrator ban_admin") ||
		!e.auditExact(vendorID, "Verified vendor mark removed by administrator ban_admin: account banned") {
		t.Fatal("listing archive or verified mark removal not audited on the account")
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator closed your account permanently%'", vendorID) != 1 {
		t.Fatal("banned account not notified")
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'The account ban_vendor, with which you have an active order, is suspended and cannot sign in.%'", buyerID) != 1 {
		t.Fatal("order counterparty not told")
	}

	// Banned state on /admin/users: in its own list, not among the suspended accounts.
	page = e.body("GET", "/admin/users", admin, nil, 200)
	mustContain(t, page, "ban_vendor · vendor · banned ", " by ban_admin · reason: "+testBanReason, "No account is suspended.")
	mustNotContain(t, page, "No account is banned.")

	// Sign-in: a wrong password answers as for any handle; the right one is refused neutrally, with no session.
	agExpect(e, "/login", randomToken(), form("handle", "ban_vendor", "password", "not-the-password-at-all"), 401, "Invalid handle or password")
	agExpect(e, "/login", randomToken(), form("handle", "no_such_handle", "password", "not-the-password-at-all"), 401, "Invalid handle or password")
	w = e.do("POST", "/login", randomToken(), form("handle", "ban_vendor", "password", testPassword))
	if w.Code != 403 || refusalText(w.Body.String()) != bannedMsg || e.cookie(w, "session") != "" || e.cookie(w, "pending") != "" {
		t.Fatalf("banned sign-in: %d %q", w.Code, refusalText(w.Body.String()))
	}
	mustNotContain(t, w.Body.String(), testBanReason)
	// A pending sign-in that survived (a race with the ban) cannot finish either.
	key, _ := agEnableTOTP(e, vendorID)
	agExpect(e, "/challenge", randomToken(), form("method", "totp", "code", agTOTPNow(key)), 403, bannedMsg, agPending(e, vendorID))
	agExec(e, "UPDATE users SET totp_enabled=false,totp_secret='' WHERE id=$1", vendorID)
	agExec(e, "DELETE FROM pending_logins WHERE user_id=$1", vendorID)

	// The handle stays reserved, in any letter case.
	for _, h := range []string{"ban_vendor", "BAN_VENDOR"} {
		agExpect(e, "/register", randomToken(), url.Values{"handle": {h}, "password": {"a-new-long-password"}}, 400, "Handle unavailable")
	}
	// No message reaches it, from an order partner or from staff, refused like an unknown handle (A-543).
	msgKey, _ := testPGPKey(t, "ban-recipient")
	body, err := encryptTo(msgKey, "are you there?")
	if err != nil {
		t.Fatal(err)
	}
	unknown := e.do("POST", "/messages", buyer, url.Values{"recipient": {"no_such_handle"}, "body": {body}})
	for _, s := range []string{buyer, admin2} {
		got := e.do("POST", "/messages", s, url.Values{"recipient": {"ban_vendor"}, "body": {body}})
		if got.Code != 403 || got.Code != unknown.Code || refusalText(got.Body.String()) != refusalText(unknown.Body.String()) {
			t.Fatalf("message to a banned account: %d %q", got.Code, refusalText(got.Body.String()))
		}
	}
	if agInt(e, "SELECT count(*) FROM messages WHERE recipient_id=$1", vendorID) != 0 {
		t.Fatal("a message to the banned account was stored")
	}
	if id, err := messageRecipient(t.Context(), e.DB, e.loadUser(buyerID), "ban_vendor"); err != nil || id != "" {
		t.Fatalf("messageRecipient for a banned vendor: %q %v", id, err)
	}
	// No new draft on its listing, and no new payment request on an existing draft.
	product := agStr(e, "SELECT product_id FROM orders WHERE id=$1", draft)
	drafts := agInt(e, "SELECT count(*) FROM orders WHERE buyer_id=$1", buyerID)
	if w = e.do("POST", "/orders", buyer, url.Values{"product_id": {product}, "currency": {"XMR"}}); w.Code < 400 || agInt(e, "SELECT count(*) FROM orders WHERE buyer_id=$1", buyerID) != drafts {
		t.Fatalf("new draft with a banned vendor: %d %s", w.Code, refusalText(w.Body.String()))
	}
	w = x.checkout(buyer, draft, testXMRAddr("ban draft "+draft, true))
	if w.Code != 409 || agStr(e, "SELECT state FROM orders WHERE id=$1", draft) != stateDraft {
		t.Fatalf("checkout with a banned vendor: %d %s", w.Code, refusalText(w.Body.String()))
	}
	// Suspension controls do not lift or repeat a ban.
	agExpect(e, "/admin/suspend", admin, suspendForm("restore", "ban_vendor"), 409, "ban_vendor is banned")
	agExpect(e, "/admin/suspend", admin, suspendForm("suspend", "ban_vendor"), 409, "ban_vendor is banned")
	// Verifying the banned vendor again is refused.
	agExpect(e, "/admin/vendor-verify", admin, form("action", "verify", "handle", "ban_vendor", "password", testPassword), 409, "ban_vendor is banned")

	// Open escrow orders keep settling: an independent administrator decides one dispute, the buyer concedes the other.
	if r := e.do("POST", "/resolve", admin2, resolverForm(resolvedDispute, "refund", xmrListingPrice)); r.Code != 303 {
		t.Fatalf("independent administrator's decision: %d %s", r.Code, refusalText(r.Body.String()))
	}
	if agStr(e, "SELECT state FROM orders WHERE id=$1", resolvedOrder) != stateResolved || agInt(e, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='refund'", resolvedOrder) != 1 {
		t.Fatal("decision on the banned vendor's order not recorded")
	}
	orderPageBody := e.body("GET", orderPage(concededOrder), buyer, nil, 200)
	if r := e.do("POST", "/disputes/concede", buyer, concessionForm(concededDispute, "release", renderedFormNonce(t, orderPageBody, "/disputes/concede"), xmrListingPrice)); r.Code != 303 {
		t.Fatalf("buyer concession: %d %s", r.Code, refusalText(r.Body.String()))
	}
	if agStr(e, "SELECT state FROM orders WHERE id=$1", concededOrder) != stateCompleted {
		t.Fatal("concession on the banned vendor's order not recorded")
	}
	x.mustPoll()
	// The release requested when the vendor shipped is superseded by the decision's refund, which is paid.
	live := func(order string) []string {
		return agStrings(t, e, "SELECT kind||' '||state FROM xmr_settlements WHERE order_id=$1 AND state<>'superseded' ORDER BY id", order)
	}
	if got := live(resolvedOrder); len(got) != 1 || got[0] != "refund relayed" {
		t.Fatalf("refund settlement after the ban: %v", x.xmrSettlements(resolvedOrder))
	}
	if got := live(concededOrder); len(got) != 1 || got[0] != "release relayed" {
		t.Fatalf("release settlement after the ban: %v", x.xmrSettlements(concededOrder))
	}

	// Banning an applicant withdraws the open application. An administrator with an authenticator confirms with its code.
	adminKey, _ := agEnableTOTP(e, admin2ID)
	agExpect(e, "/admin/ban", admin2, banForm("ban", "ban_applicant", testBanReason), 401, "")
	agExpect(e, "/admin/ban", admin2, banForm("ban", "ban_applicant", testBanReason, "code", e.wrongTOTP(admin2ID)), 401, "")
	if latestVendorApp(e, applicantID).State != "awaiting_approval" {
		t.Fatal("an unconfirmed ban changed the application")
	}
	agExpect(e, "/admin/ban", admin2, banForm("ban", "ban_applicant", testBanReason, "code", agTOTPNow(adminKey)), 303, "")
	if app := latestVendorApp(e, applicantID); app.State != "withdrawn" {
		t.Fatalf("application after the ban: %+v", app)
	}
	if !e.auditExact(applicantID, "Vendor application withdrawn: account banned by administrator ban_admin2") ||
		!e.auditExact(admin2ID, "Banned account ban_applicant: "+testBanReason+"; ended 1 session(s) and any pending sign-ins; withdrew the open vendor application (confirmed with password and authenticator code)") {
		t.Fatal("application withdrawal not audited")
	}

	// Lifting the ban: a reason is required; sign-in comes back, listings and the verified mark do not.
	agExpect(e, "/admin/ban", admin, banForm("unban", "ban_vendor", ""), 400, "Give a reason of 10–500 characters")
	agExpect(e, "/admin/ban", admin, banForm("unban", "ban_buyer", testLiftReason), 409, "ban_buyer is not banned")
	agExpect(e, "/admin/ban", admin, banForm("unban", "ban_vendor", testLiftReason), 303, "")
	agExpect(e, "/admin/ban", admin, banForm("unban", "ban_vendor", testLiftReason), 409, "ban_vendor is not banned")
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND banned_at IS NULL AND banned_by IS NULL AND ban_reason IS NULL AND suspended_at IS NULL", vendorID) != 1 {
		t.Fatal("ban not lifted")
	}
	if !e.auditExact(adminID, "Lifted the ban on ban_vendor: "+testLiftReason+" (confirmed with password)") ||
		!e.auditExact(vendorID, "Ban lifted by administrator ban_admin: "+testLiftReason) {
		t.Fatal("lifted ban not audited on both accounts")
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator lifted the ban on your account.%'", vendorID) != 1 {
		t.Fatal("unbanned account not notified")
	}
	w = e.do("POST", "/login", randomToken(), form("handle", "ban_vendor", "password", testPassword))
	e.check(w, 303)
	e.check(e.do("GET", "/account", e.session(w), nil), 200)
	if agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1 AND NOT archived", vendorID) != 0 || agStr(e, "SELECT vendor_verified::text FROM users WHERE id=$1", vendorID) != "false" {
		t.Fatal("lifting the ban restored listings or the verified mark")
	}
	page = e.body("GET", "/admin/users", admin, nil, 200)
	mustContain(t, page, "ban_applicant · buyer · banned ")
	mustNotContain(t, page, "ban_vendor · vendor · banned ")
}

// Migration 123: a ban needs its reason and implies a suspension.
func TestBanMigration(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("ban_mig", "buyer")
	adminID, _ := e.user("ban_mig_admin", "admin")
	agExec(e, `DELETE FROM schema_migrations WHERE version=123;
		ALTER TABLE users DROP COLUMN banned_at, DROP COLUMN banned_by, DROP COLUMN ban_reason`)
	if err := applyLatestMigrations(t, e); err != nil {
		t.Fatal(err)
	}
	if agInt(e, "SELECT count(*) FROM users WHERE banned_at IS NOT NULL OR banned_by IS NOT NULL OR ban_reason IS NOT NULL") != 0 {
		t.Fatal("migration banned an existing account")
	}
	for _, bad := range []string{
		"UPDATE users SET banned_at=now(),suspended_at=now() WHERE id=$1",                            // no reason
		"UPDATE users SET ban_reason='A reason long enough.',suspended_at=now() WHERE id=$1",         // a reason without a ban
		"UPDATE users SET banned_at=now(),ban_reason='A reason long enough.' WHERE id=$1",            // not suspended
		"UPDATE users SET banned_at=now(),ban_reason='   short   ',suspended_at=now() WHERE id=$1",   // too short
		"UPDATE users SET banned_at=now(),ban_reason=repeat('x',501),suspended_at=now() WHERE id=$1", // too long
	} {
		if _, err := e.DB.Exec(bad, id); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	agExec(e, "UPDATE users SET banned_at=now(),banned_by=$2,ban_reason='A reason long enough.',suspended_at=now() WHERE id=$1", id, adminID)
	if _, err := e.DB.Exec("UPDATE users SET suspended_at=NULL WHERE id=$1", id); err == nil {
		t.Fatal("a banned account was unsuspended")
	}
	// Erasing the administrator unlinks them; the ban stays.
	agExec(e, "DELETE FROM sessions WHERE user_id=$1", adminID)
	agExec(e, "DELETE FROM audit_events WHERE user_id=$1", adminID)
	agExec(e, "DELETE FROM users WHERE id=$1", adminID)
	if agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND banned_at IS NOT NULL AND banned_by IS NULL", id) != 1 {
		t.Fatal("erasing the banning administrator removed the ban")
	}
}

// Retention: closing a banned account erases the ban's reason with the profile (it stays suspended and closed), and
// the administrator named as banned_by never blocks their own erasure (ON DELETE SET NULL).
func TestBanRetentionClosure(t *testing.T) {
	e := newTestApp(t)
	adminID, _ := e.user("ban_ret_admin", "admin")
	bannedID, _ := e.user("ban_ret_target", "vendor")
	otherID, _ := e.user("ban_ret_other", "buyer")
	agExec(e, "UPDATE users SET banned_at=now(),banned_by=$2,ban_reason=$3,suspended_at=now() WHERE id IN ($1,$4)", bannedID, adminID, testBanReason, otherID)
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = e.A.applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "account_close", ID: bannedID, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var cleared bool
	if err = tx.QueryRow("SELECT banned_at IS NULL AND banned_by IS NULL AND ban_reason IS NULL AND suspended_at IS NOT NULL AND closed_at IS NOT NULL FROM users WHERE id=$1", bannedID).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("closed banned account kept its ban: %t %v", cleared, err)
	}
	if _, err = tx.Exec("DELETE FROM sessions WHERE user_id=$1", adminID); err != nil {
		t.Fatal(err)
	}
	needed, err := retentionUserReferenced(t.Context(), tx, adminID)
	if err != nil || needed {
		t.Fatalf("banned_by blocked the administrator's purge: %t %v", needed, err)
	}
	if err = e.A.applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "account", ID: adminID, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var kept bool
	if err = tx.QueryRow("SELECT banned_at IS NOT NULL AND banned_by IS NULL AND ban_reason IS NOT NULL FROM users WHERE id=$1", otherID).Scan(&kept); err != nil || !kept {
		t.Fatalf("erasing the administrator changed another ban: %t %v", kept, err)
	}
}
