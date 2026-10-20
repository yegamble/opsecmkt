package market

import (
	"net/url"
	"strings"
	"testing"
)

// Away preferences apply to every buyer-to-vendor path, including real orders and earlier inbound messages.
// Deactivation hides a vendor from strangers while optionally preserving existing customer conversations.
func TestMessageVendorAwayPreferences(t *testing.T) {
	e := newTestApp(t)
	customerID, customer := e.user("away_customer", "buyer")
	historyID, history := e.user("away_history", "buyer")
	_, stranger := e.user("away_stranger", "buyer")
	vendorID, _ := e.user("away_vendor", "vendor")
	_, staff := e.user("away_staff", "moderator")
	key, pub := testPGPKey(t, "away-recipient")
	e.setKey(vendorID, pub)
	body, err := encryptTo(key, "existing customer message")
	if err != nil {
		t.Fatal(err)
	}
	orderID := e.order(customerID, e.product(vendorID, "physical"), "XMR", stateAwaitingPayment)
	agExec(e, "INSERT INTO messages(id,sender_id,recipient_id,body,encrypted) VALUES($1,$2,$3,'earlier vendor reply',true),($4,$2,$5,'earlier vendor reply',true)",
		randomToken(), vendorID, customerID, randomToken(), historyID)
	written := func() int {
		return abuseCount(e, "SELECT count(*) FROM messages") + abuseCount(e, "SELECT count(*) FROM notifications") +
			abuseCount(e, "SELECT count(*) FROM audit_events WHERE action LIKE 'Sent OpenPGP-encrypted message%'")
	}
	send := func(session string, allowed bool) {
		t.Helper()
		before := written()
		w := e.do("POST", "/messages", session, url.Values{"recipient": {"away_vendor"}, "body": {body}})
		if allowed {
			e.check(w, 303)
			if written() != before+3 {
				t.Fatal("allowed send did not write its message, notification and audit row")
			}
			return
		}
		e.check(w, 403)
		mustContain(t, w.Body.String(), messageNotPermitted)
		if written() != before {
			t.Fatal("unavailable vendor received a message or notification")
		}
	}
	lookup := func(session, handle string) string {
		t.Helper()
		return e.body("GET", "/messages?to="+handle, session, nil, 200)
	}

	agExec(e, "UPDATE users SET vendor_vacation=true,vendor_messages_off_while_away=false WHERE id=$1", vendorID)
	send(customer, true)
	send(history, true)
	send(stranger, true)

	agExec(e, "UPDATE users SET vendor_messages_off_while_away=true WHERE id=$1", vendorID)
	for _, session := range []string{customer, history, stranger} {
		send(session, false)
		page := lookup(session, "away_vendor")
		mustContain(t, page, messageNotPermitted)
		mustNotContain(t, page, "-----BEGIN PGP PUBLIC KEY BLOCK-----", "name=\"recipient\"", "name=\"body\"")
	}
	send(staff, true)
	mustNotContain(t, e.body("GET", "/messages", customer, nil, 200), "href=\"/messages?to=away_vendor\"")
	orderPage := e.body("GET", "/order?id="+orderID, customer, nil, 200)
	mustContain(t, orderPage, "-----BEGIN PGP PUBLIC KEY BLOCK-----")
	mustNotContain(t, orderPage, "href=\"/messages?to=away_vendor\"")

	agExec(e, "UPDATE users SET vendor_vacation=false,vendor_deactivated=true,vendor_messages_off_while_away=false WHERE id=$1", vendorID)
	send(customer, true)
	send(history, true)
	// Use another stranger: a successful message while public creates a legitimate conversation the vendor can answer.
	_, hiddenStranger := e.user("away_hidden_stranger", "buyer")
	send(hiddenStranger, false)
	forbidden := lookup(hiddenStranger, "away_vendor")
	unknown := lookup(hiddenStranger, "away_missing")
	if strings.ReplaceAll(forbidden, "away_vendor", "HANDLE") != strings.ReplaceAll(unknown, "away_missing", "HANDLE") {
		t.Fatal("hidden vendor lookup differs from an unknown handle")
	}

	agExec(e, "UPDATE users SET vendor_messages_off_while_away=true WHERE id=$1", vendorID)
	send(customer, false)
	send(history, false)
	send(staff, true)

	// Existing buyers can still reach current dispute staff while the vendor has opted out.
	agExec(e, "UPDATE orders SET state=$1 WHERE id=$2", stateDisputed, orderID)
	mustContain(t, lookup(customer, "away_staff"), "name=\"recipient\" value=\"away_staff\"")
}

// A legacy vendor without a public key is not a public messaging target for strangers.
func TestMessageKeylessVendorIsNotEnumerable(t *testing.T) {
	e := newTestApp(t)
	_, session := e.user("keyless_vendor_viewer", "buyer")
	_, _ = e.keylessUser("keyless_legacy_vendor", "vendor")
	forbidden := e.body("GET", "/messages?to=keyless_legacy_vendor", session, nil, 200)
	unknown := e.body("GET", "/messages?to=keyless_missing_vendor", session, nil, 200)
	if strings.ReplaceAll(forbidden, "keyless_legacy_vendor", "HANDLE") != strings.ReplaceAll(unknown, "keyless_missing_vendor", "HANDLE") {
		t.Fatal("keyless vendor lookup differs from an unknown handle")
	}
	mustContain(t, forbidden, messageNotPermitted)
	mustNotContain(t, forbidden, "name=\"recipient\"", "name=\"body\"", "-----BEGIN PGP PUBLIC KEY BLOCK-----")
	key, _ := testPGPKey(t, "keyless-vendor-unavailable")
	body, err := encryptTo(key, "unavailable recipient")
	if err != nil {
		t.Fatal(err)
	}
	before := abuseCount(e, "SELECT count(*) FROM messages") + abuseCount(e, "SELECT count(*) FROM notifications")
	w := e.do("POST", "/messages", session, url.Values{"recipient": {"keyless_legacy_vendor"}, "body": {body}})
	e.check(w, 403)
	mustContain(t, w.Body.String(), messageNotPermitted)
	if after := abuseCount(e, "SELECT count(*) FROM messages") + abuseCount(e, "SELECT count(*) FROM notifications"); after != before {
		t.Fatal("a refused legacy vendor message wrote a message or notification")
	}
}
