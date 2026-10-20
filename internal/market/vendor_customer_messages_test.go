package market

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A vendor account can also purchase. Its current account role must not break
// necessary communication in either direction of a real order relationship.
func TestVendorAccountsKeepExistingOrderMessages(t *testing.T) {
	for _, direction := range []string{"purchaser", "seller"} {
		t.Run(direction, func(t *testing.T) {
			e := newTestApp(t)
			newVendor := func(handle string) (string, string, string) {
				t.Helper()
				id, session := e.keylessUser(handle, "vendor")
				key, public := testPGPKey(t, handle)
				e.check(e.do(http.MethodPost, "/account", session, url.Values{"pgp": {public}, "password": {testPassword}}), http.StatusSeeOther)
				proveAccess(e, id, session, key)
				return id, session, public
			}
			actorID, actor, _ := newVendor("vendor_contact_" + direction)
			targetHandle := "vendor_partner_" + direction
			targetID, _, public := newVendor(targetHandle)
			strangerID, stranger, _ := newVendor("vendor_outsider_" + direction)
			otherID, _ := e.user("vendor_unrelated_"+direction, "vendor")
			// An unrelated purchase does not establish contact with this target.
			e.order(strangerID, e.product(otherID, "digital"), "XMR", statePaid)
			key, fingerprint, canonical, err := parseCanonicalPublicKey(public)
			if err != nil {
				t.Fatal(err)
			}
			body, err := encryptTo(key, "necessary communication for an existing vendor account order")
			if err != nil {
				t.Fatal(err)
			}
			buyerID, sellerID := actorID, targetID
			if direction == "seller" {
				buyerID, sellerID = targetID, actorID
			}
			orderID := e.order(buyerID, e.product(sellerID, "digital"), "XMR", stateDraft)
			agExec(e, "UPDATE users SET vendor_deactivated=true,vendor_messages_off_while_away=false WHERE id=$1", targetID)
			counts := func() [4]int {
				return [4]int{
					agInt(e, "SELECT count(*) FROM messages"),
					agInt(e, "SELECT count(*) FROM notifications"),
					agInt(e, "SELECT count(*) FROM audit_events"),
					agInt(e, "SELECT count(*) FROM form_submissions"),
				}
			}
			allow := func() {
				t.Helper()
				before := counts()
				w := e.do(http.MethodPost, "/messages", actor, url.Values{
					"recipient": {targetHandle}, "body": {body},
				})
				e.check(w, http.StatusSeeOther)
				after := counts()
				for i := range before {
					if after[i] != before[i]+1 {
						t.Fatalf("authorized send records before %v after %v", before, after)
					}
				}
				page := e.body(http.MethodGet, "/messages?to="+targetHandle, actor, nil, http.StatusOK)
				mustContain(t, html.UnescapeString(page), canonical, formatFingerprint(fingerprint))
				orderPage := e.body(http.MethodGet, "/order?id="+orderID, actor, nil, http.StatusOK)
				mustContain(t, orderPage, `href="#order-messages"`, `action="/orders/message"`)
				mustNotContain(t, orderPage, "href=\"/messages?to="+targetHandle+"&amp;order_id="+orderID+"\"")
			}
			deny := func(session, when string) {
				t.Helper()
				before := counts()
				known := e.do(http.MethodPost, "/messages", session, url.Values{
					"recipient": {targetHandle}, "body": {body},
				})
				unknown := e.do(http.MethodPost, "/messages", session, url.Values{
					"recipient": {"vendor_contact_missing"}, "body": {body},
				})
				e.check(known, http.StatusForbidden)
				e.check(unknown, http.StatusForbidden)
				mustContain(t, known.Body.String(), messageNotPermitted)
				if known.Body.String() != unknown.Body.String() {
					t.Fatalf("%s: private and unknown refusals differ", when)
				}
				if after := counts(); after != before {
					t.Fatalf("%s: refusal wrote records: before %v after %v", when, before, after)
				}
				page := e.body(http.MethodGet, "/messages?to="+targetHandle, session, nil, http.StatusOK)
				mustContain(t, page, messageNotPermitted)
				mustNotContain(t, html.UnescapeString(page), canonical, formatFingerprint(fingerprint), "-----BEGIN PGP PUBLIC KEY BLOCK-----", "name=\"recipient\"", "name=\"body\"")
			}
			if direction == "seller" {
				deny(actor, "draft is not visible to its seller")
				agExec(e, "UPDATE orders SET state=$2 WHERE id=$1", orderID, stateCancelled)
				deny(actor, "cancelled draft had no escrow or payment")
			}
			agExec(e, "UPDATE orders SET state=$2 WHERE id=$1", orderID, statePaid)
			allow()
			deny(stranger, "unrelated vendor account")
			forbidden := e.body(http.MethodGet, "/messages?to="+targetHandle, stranger, nil, http.StatusOK)
			unknown := e.body(http.MethodGet, "/messages?to=vendor_contact_missing", stranger, nil, http.StatusOK)
			if strings.ReplaceAll(forbidden, targetHandle, "HANDLE") != strings.ReplaceAll(unknown, "vendor_contact_missing", "HANDLE") {
				t.Fatal("private vendor key lookup differs from an unknown handle")
			}
			agExec(e, "INSERT INTO messages(id,sender_id,recipient_id,body,encrypted) VALUES($1,$2,$3,'earlier reply',true)",
				randomToken(), targetID, strangerID)
			deny(stranger, "vendor history alone without a shared order")
			// Ordinary established contacts survive financial closure. Restriction
			// narrows that permission back to unresolved financial work.
			agExec(e, "UPDATE orders SET state=$2,financial_closed_at=now() WHERE id=$1", orderID, stateCompleted)
			allow()
			agExec(e, "UPDATE orders SET state=$2,financial_closed_at=NULL WHERE id=$1", orderID, statePaid)
			agExec(e, "UPDATE users SET retention_restricted_at=now() WHERE id=$1", targetID)
			allow()
			agExec(e, "UPDATE orders SET state=$2,financial_closed_at=now() WHERE id=$1", orderID, stateCompleted)
			agExec(e, "INSERT INTO messages(id,sender_id,recipient_id,body,encrypted) VALUES($1,$2,$3,'earlier reply',true)",
				randomToken(), targetID, actorID)
			deny(actor, "financially closed restricted order despite history")
			deny(stranger, "restricted history without a shared order")
			agExec(e, "UPDATE orders SET state=$2,financial_closed_at=NULL WHERE id=$1", orderID, statePaid)
			agExec(e, "UPDATE users SET vendor_messages_off_while_away=true WHERE id=$1", targetID)
			deny(actor, "recipient opted out while away")
			agExec(e, "UPDATE users SET vendor_messages_off_while_away=false,suspended_at=now() WHERE id=$1", targetID)
			deny(actor, "recipient suspended")
			agExec(e, "UPDATE users SET suspended_at=NULL,closed_at=now() WHERE id=$1", targetID)
			deny(actor, "recipient closed")
			agExec(e, "UPDATE users SET closed_at=NULL WHERE id=$1", targetID)
			if direction == "seller" {
				snapshot := e.loadUser(actorID)
				agExec(e, "UPDATE users SET role='buyer' WHERE id=$1", actorID)
				id, err := messageRecipient(context.Background(), e.DB, snapshot, targetHandle)
				if err != nil || id != "" {
					t.Fatalf("stale seller role retained customer access: %q %v", id, err)
				}
				deny(actor, "seller demoted after request snapshot")
				agExec(e, "UPDATE users SET role='vendor' WHERE id=$1", actorID)
			}
			_, admin := e.user("vendor_ban_admin_"+direction, "admin")
			e.check(e.do(http.MethodPost, "/admin/ban", admin, banForm("ban", targetHandle, testBanReason)), http.StatusSeeOther)
			deny(actor, "recipient banned")
		})
	}
}

// Keep this channel check separate from the availability matrix so its extra
// submissions do not exhaust that matrix's independently tested message budget.
func TestVendorCustomerOrderContextUsesOrderChannel(t *testing.T) {
	for _, direction := range []string{"purchaser", "seller"} {
		t.Run(direction, func(t *testing.T) {
			e := newTestApp(t)
			actorID, actor := e.user("channel_vendor_actor", "vendor")
			targetID, _ := e.user("channel_vendor_target", "vendor")
			buyerID, sellerID := actorID, targetID
			if direction == "seller" {
				buyerID, sellerID = targetID, actorID
			}
			orderID := e.order(buyerID, e.product(sellerID, "physical"), "XMR", statePaid)
			body := encryptToRecipients(t, "transaction-specific vendor account correspondence", accountPGPFixture.key)
			// General conversations retain their direct inbox. Explicit order
			// correspondence belongs to its separate order-private channel.
			e.check(e.do(http.MethodPost, "/messages", actor, url.Values{
				"recipient": {"channel_vendor_target"}, "body": {body}, "order_id": {orderID},
			}), http.StatusConflict)
			e.check(e.do(http.MethodGet, "/messages?to=channel_vendor_target&order_id="+orderID, actor, nil), http.StatusConflict)
			for _, table := range []string{"messages", "notifications", "audit_events", "form_submissions"} {
				if agInt(e, "SELECT count(*) FROM "+table) != 0 {
					t.Fatal("refused order-context direct message wrote " + table)
				}
			}
			e.check(e.do(http.MethodPost, "/orders/message", actor, url.Values{
				"order_id": {orderID}, "body": {body},
			}), http.StatusSeeOther)
			if agInt(e, "SELECT count(*) FROM order_messages WHERE order_id=$1", orderID) != 1 || agInt(e, "SELECT count(*) FROM messages") != 0 {
				t.Fatal("order correspondence escaped into the direct inbox")
			}
		})
	}
}
