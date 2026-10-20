package market

import (
	"context"
	"database/sql"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A request can retain a User snapshot while a confirmed role change commits.
// Neither that old role nor a valid PGP session grants its former contact rights.
func TestMessageRecipientUsesCurrentSenderRole(t *testing.T) {
	for _, role := range []string{"moderator", "vendor"} {
		t.Run(role, func(t *testing.T) {
			e := newTestApp(t)
			senderID, session := e.user("role_sender_"+role, role)
			buyerID, _ := e.user("role_target_"+role, "buyer")
			if role == "vendor" {
				e.order(buyerID, e.product(senderID, "digital"), "XMR", stateAwaitingPayment)
			}
			key, pub := testPGPKey(t, "role-recipient")
			e.setKey(buyerID, pub)
			body, err := encryptTo(key, "message after sender demotion")
			if err != nil {
				t.Fatal(err)
			}
			handle := "role_target_" + role
			snapshot := e.loadUser(senderID)
			id, err := messageRecipient(context.Background(), e.DB, snapshot, handle)
			if err != nil || id != buyerID {
				t.Fatalf("original %s cannot reach its authorized recipient: %q %v", role, id, err)
			}
			agExec(e, "UPDATE users SET role='buyer' WHERE id=$1", senderID)
			// This is the request object obtained before demotion, not a refreshed User.
			if snapshot.Role != role {
				t.Fatal("the fixture lost its stale role snapshot")
			}
			id, err = messageRecipient(context.Background(), e.DB, snapshot, handle)
			if err != nil || id != "" {
				t.Fatalf("stale %s role granted buyer access: %q %v", role, id, err)
			}
			unknown, err := messageRecipient(context.Background(), e.DB, snapshot, "role_missing_recipient")
			if err != nil || unknown != id {
				t.Fatalf("forbidden and unknown recipient results differ: %q %q %v", id, unknown, err)
			}
			request := httptest.NewRequest(http.MethodGet, "/messages?to="+handle, nil)
			page := &PageData{User: snapshot}
			if err = messageRecipientLoader(request.Context(), e.A, request, page); err != nil {
				t.Fatal(err)
			}
			if len(page.Contacts) != 0 {
				t.Fatal("a stale role exposed the private recipient key")
			}
			counts := func() [4]int {
				return [4]int{
					abuseCount(e, "SELECT count(*) FROM messages"),
					abuseCount(e, "SELECT count(*) FROM notifications"),
					abuseCount(e, "SELECT count(*) FROM audit_events"),
					abuseCount(e, "SELECT count(*) FROM form_submissions"),
				}
			}
			before := counts()
			tx, err := e.DB.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			post := httptest.NewRequest(http.MethodPost, "/messages", nil)
			_, err = messageAction(&actionCtx{A: e.A, R: post, Tx: tx, User: snapshot, Token: session,
				Form: url.Values{"recipient": {handle}, "body": {body}}})
			var refusal *httpError
			if !errors.As(err, &refusal) || refusal.Code != http.StatusForbidden {
				t.Fatalf("message with stale %s role: %v", role, err)
			}
			mustContain(t, refusal.Msg, messageNotPermitted)
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if after := counts(); after != before {
				t.Fatalf("a stale-role refusal wrote data: before %v after %v", before, after)
			}
		})
	}
}

// Authorization locks protect the current sender role and recipient role/key
// until the authorized key read (or message transaction) has finished.
func TestMessageAuthorizationLocksCurrentRolesAndRecipientKey(t *testing.T) {
	e := newTestApp(t)
	senderID, _ := e.user("role_lock_buyer", "buyer")
	vendorID, _ := e.user("role_lock_vendor", "vendor")
	_, public := testPGPKey(t, "role-lock-public")
	e.setKey(vendorID, public)
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	id, err := messageRecipient(context.Background(), tx, e.loadUser(senderID), "role_lock_vendor")
	if err != nil || id != vendorID {
		t.Fatalf("authorized recipient: %q %v", id, err)
	}
	for _, userID := range []string{senderID, vendorID} {
		var lockedID string
		err = e.DB.QueryRow("SELECT id FROM users WHERE id=$1 FOR UPDATE NOWAIT", userID).Scan(&lockedID)
		var locked *pgconn.PgError
		if !errors.As(err, &locked) || locked.Code != "55P03" {
			t.Fatalf("authorization did not keep account %s locked: %v", userID, err)
		}
	}
	key, err := contactKey(context.Background(), tx, vendorID, "role_lock_vendor", "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, canonical, err := parseCanonicalPublicKey(public)
	if err != nil || key.Armored != canonical {
		t.Fatalf("authorized key read changed: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, privatePublic := testPGPKey(t, "role-lock-now-private")
	agExec(e, "UPDATE users SET role='buyer',pgp=$2 WHERE id=$1", vendorID, privatePublic)
	page := &PageData{User: e.loadUser(senderID)}
	request := httptest.NewRequest(http.MethodGet, "/messages?to=role_lock_vendor", nil)
	if err = messageRecipientLoader(request.Context(), e.A, request, page); err != nil {
		t.Fatal(err)
	}
	if len(page.Contacts) != 0 {
		t.Fatal("a demoted recipient exposed its replacement public key")
	}
}

// Public profile eligibility and the displayed public key must be read under
// the same row lock; existing order-party key access has its separate rule.
func TestPublicVendorContactKeyRechecksVisibility(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("public_role_vendor", "vendor")
	buyerID, buyer := e.user("public_role_order_buyer", "buyer")
	orderID := e.order(buyerID, e.product(id, "physical"), "XMR", statePaid)
	key, err := publicVendorContactKey(context.Background(), e.DB, id)
	if err != nil || key.Armored == "" {
		t.Fatalf("public vendor key: %v", err)
	}
	_, privatePublic := testPGPKey(t, "public-role-now-private")
	agExec(e, "UPDATE users SET role='buyer',pgp=$2 WHERE id=$1", id, privatePublic)
	if key, err = publicVendorContactKey(context.Background(), e.DB, id); !errors.Is(err, sql.ErrNoRows) || key.Armored != "" || key.Fingerprint != "" {
		t.Fatalf("private account key became public after demotion: %v", err)
	}
	_, fingerprint, canonical, err := parseCanonicalPublicKey(privatePublic)
	if err != nil {
		t.Fatal(err)
	}
	// Public discovery exposes neither the replacement key nor its fingerprint.
	publicPage := e.body(http.MethodGet, "/vendor?id="+id, buyer, nil, http.StatusNotFound)
	mustNotContain(t, html.UnescapeString(publicPage), "-----BEGIN PGP PUBLIC KEY BLOCK-----", formatFingerprint(fingerprint))
	// The existing buyer still needs exactly this order partner's canonical key
	// and fingerprint. HTML escapes armor characters such as '+', so decode the
	// rendered block before comparing its entire contents.
	orderPage := e.body(http.MethodGet, "/order?id="+orderID, buyer, nil, http.StatusOK)
	shown := armoredKeyPattern.FindAllString(orderPage, -1)
	if len(shown) != 1 || html.UnescapeString(shown[0]) != canonical {
		t.Fatal("the order buyer did not receive exactly its counterparty's canonical key")
	}
	mustContain(t, orderPage, `<dd class="pgp-fingerprint">`+formatFingerprint(fingerprint)+`</dd>`)
	agExec(e, "UPDATE users SET role='vendor',vendor_deactivated=true WHERE id=$1", id)
	if key, err = publicVendorContactKey(context.Background(), e.DB, id); !errors.Is(err, sql.ErrNoRows) || key.Armored != "" || key.Fingerprint != "" {
		t.Fatalf("deactivated vendor key became public: %v", err)
	}
	publicPage = e.body(http.MethodGet, "/vendor?id="+id, buyer, nil, http.StatusNotFound)
	mustNotContain(t, html.UnescapeString(publicPage), "-----BEGIN PGP PUBLIC KEY BLOCK-----", formatFingerprint(fingerprint))
}

// A vendor restricted for closure still has to resolve paid orders. Restriction
// permits only the necessary open-financial-order contact, not public discovery.
func TestRestrictedVendorKeepsNecessaryOrderMessages(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("restricted_message_vendor", "vendor")
	buyerID, buyer := e.user("restricted_message_buyer", "buyer")
	closedID, closedBuyer := e.user("restricted_message_closed", "buyer")
	historyID, history := e.user("restricted_message_history", "buyer")
	_, stranger := e.user("restricted_message_stranger", "buyer")
	_, staff := e.user("restricted_message_staff", "moderator")
	product := e.product(vendorID, "digital")
	e.order(buyerID, product, "XMR", statePaid)
	closedOrder := e.order(closedID, product, "XMR", stateCompleted)
	agExec(e, "UPDATE orders SET financial_closed_at=now() WHERE id=$1", closedOrder)
	agExec(e, "INSERT INTO messages(id,sender_id,recipient_id,body,encrypted) VALUES($1,$2,$3,'earlier vendor reply',true)",
		randomToken(), vendorID, historyID)
	key, public := testPGPKey(t, "restricted-message-recipient")
	e.setKey(vendorID, public)
	body, err := encryptTo(key, "necessary order communication")
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, "UPDATE users SET retention_restricted_at=now() WHERE id=$1", vendorID)
	send := func(session string, allowed bool) {
		t.Helper()
		before := abuseCount(e, "SELECT count(*) FROM messages") + abuseCount(e, "SELECT count(*) FROM notifications") +
			abuseCount(e, "SELECT count(*) FROM audit_events") + abuseCount(e, "SELECT count(*) FROM form_submissions")
		w := e.do(http.MethodPost, "/messages", session, url.Values{"recipient": {"restricted_message_vendor"}, "body": {body}})
		if allowed {
			e.check(w, http.StatusSeeOther)
			return
		}
		e.check(w, http.StatusForbidden)
		mustContain(t, w.Body.String(), messageNotPermitted)
		after := abuseCount(e, "SELECT count(*) FROM messages") + abuseCount(e, "SELECT count(*) FROM notifications") +
			abuseCount(e, "SELECT count(*) FROM audit_events") + abuseCount(e, "SELECT count(*) FROM form_submissions")
		if after != before {
			t.Fatal("an unnecessary restricted-vendor contact wrote data")
		}
	}
	send(buyer, true)
	page := e.body(http.MethodGet, "/messages?to=restricted_message_vendor", buyer, nil, http.StatusOK)
	mustContain(t, page, "-----BEGIN PGP PUBLIC KEY BLOCK-----", "name=\"recipient\" value=\"restricted_message_vendor\"")
	for _, session := range []string{closedBuyer, history, stranger} {
		send(session, false)
		page = e.body(http.MethodGet, "/messages?to=restricted_message_vendor", session, nil, http.StatusOK)
		mustContain(t, page, messageNotPermitted)
		mustNotContain(t, page, "-----BEGIN PGP PUBLIC KEY BLOCK-----", "name=\"recipient\"")
	}
	agExec(e, "UPDATE users SET vendor_deactivated=true,vendor_messages_off_while_away=true WHERE id=$1", vendorID)
	send(buyer, false)
	send(staff, true)
}

// A disputed order's staff directory must not publish a replacement private
// account key after a staff role change between its directory and key reads.
func TestDisputeStaffContactKeyRechecksRole(t *testing.T) {
	e := newTestApp(t)
	staffID, _ := e.user("current_dispute_staff", "moderator")
	buyerID, _ := e.user("current_dispute_buyer", "buyer")
	vendorID, _ := e.user("current_dispute_vendor", "vendor")
	key, err := disputeStaffContactKey(context.Background(), e.DB, staffID, buyerID, vendorID)
	if err != nil || key.Armored == "" || key.Relation != "moderator" {
		t.Fatalf("current staff key: %v", err)
	}
	_, privatePublic := testPGPKey(t, "dispute-staff-now-private")
	agExec(e, "UPDATE users SET role='buyer',pgp=$2 WHERE id=$1", staffID, privatePublic)
	if key, err = disputeStaffContactKey(context.Background(), e.DB, staffID, buyerID, vendorID); !errors.Is(err, sql.ErrNoRows) || key.Armored != "" {
		t.Fatalf("former staff replacement key was exposed: %v", err)
	}
	agExec(e, "UPDATE users SET role='moderator',suspended_at=now() WHERE id=$1", staffID)
	if key, err = disputeStaffContactKey(context.Background(), e.DB, staffID, buyerID, vendorID); !errors.Is(err, sql.ErrNoRows) || key.Armored != "" {
		t.Fatalf("suspended staff replacement key was exposed: %v", err)
	}
	if key, err = disputeStaffContactKey(context.Background(), e.DB, vendorID, buyerID, vendorID); !errors.Is(err, sql.ErrNoRows) || key.Armored != "" {
		t.Fatalf("order party was exposed as independent dispute staff: %v", err)
	}
}
