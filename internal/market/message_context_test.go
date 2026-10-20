package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

func setMessageTestKey(e *testEnv, id, public string) {
	e.t.Helper()
	_, fp, err := parsePublicKey(public)
	if err != nil {
		e.t.Fatal(err)
	}
	agExec(e, `UPDATE users SET pgp=$2,pgp_fingerprint=$3 WHERE id=$1`, id, public, fp)
}

func TestMessageOrderContextIsExplicitAuthorizedAndForwardedAsCiphertext(t *testing.T) {
	e := newTestApp(t)
	buyerID, buyer := e.user("context_buyer", "buyer")
	vendorID, vendor := e.user("context_vendor", "vendor")
	otherID, other := e.user("context_other", "buyer")
	_, buyerPub := testPGPKey(t, "context_buyer")
	vendorKey, vendorPub := testPGPKey(t, "context_vendor")
	_, otherPub := testPGPKey(t, "context_other")
	setMessageTestKey(e, buyerID, buyerPub)
	setMessageTestKey(e, vendorID, vendorPub)
	setMessageTestKey(e, otherID, otherPub)
	order := e.order(buyerID, e.product(vendorID, "physical"), "XMR", statePaid)
	enableXMPPDelivery(e)
	agExec(e, `UPDATE settings SET value=(value::jsonb || '{"allow_contents":true,"content_policy_version":1}')::text WHERE key='xmpp_config'`)
	xmppOptIn(e, vendorID)
	agExec(e, `UPDATE users SET xmpp_include_contents=true WHERE id=$1`, vendorID)
	const private = "Private shipping information and product link must stay encrypted."
	ciphertext, err := encryptTo(vendorKey, private)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"recipient": {"context_vendor"}, "body": {ciphertext}, "order_id": {order}}
	e.check(e.do("POST", "/messages", buyer, form), 303)
	if agStr(e, `SELECT order_id FROM messages WHERE sender_id=$1`, buyerID) != order {
		t.Fatal("authorized explicit order context was not persisted")
	}
	if agStr(e, `SELECT order_id FROM notifications WHERE user_id=$1`, vendorID) != order {
		t.Fatal("message notice did not link its authorized order")
	}
	var external string
	worked, err := e.A.deliverXMPPNotification(context.Background(), func(_ context.Context, _ xmppConfig, _, _, body string) error {
		external = body
		return nil
	})
	if err != nil || !worked || !strings.Contains(external, "New message from context_buyer for Order #"+order) {
		t.Fatalf("contextual XMPP delivery failed: worked=%v err=%v", worked, err)
	}
	start := strings.Index(external, "-----BEGIN PGP MESSAGE-----")
	if start < 0 || strings.Contains(external, private) || testDecrypt(t, vendorKey, external[start:]) != private {
		t.Fatal("XMPP did not forward the decryptable ciphertext privately")
	}
	page := e.body("GET", "/notifications", vendor, nil, 200)
	mustContain(t, page, "New message from context_buyer", order)
	mustNotContain(t, page, "-----BEGIN PGP MESSAGE-----", private)
	page = e.body("GET", "/messages?to=context_buyer", vendor, nil, 200)
	mustContain(t, page, "order_id="+order)
	page = e.body("GET", "/messages?to=context_vendor&order_id="+order, buyer, nil, 200)
	mustContain(t, page, `name="order_id" value="`+order+`"`)

	// Sharing a vendor does not authorize another buyer to label a message with
	// this buyer's order, even though the recipient handle itself is permitted.
	for _, session := range []string{other} {
		e.check(e.do("POST", "/messages", session, form), 404)
		e.check(e.do("GET", "/messages?to=context_vendor&order_id="+order, session, nil), 404)
	}
	bad := cloneValues(form)
	bad.Set("dispute_id", randomToken())
	e.check(e.do("POST", "/messages", buyer, bad), 400)
	if agInt(e, `SELECT count(*) FROM messages`) != 1 || agInt(e, `SELECT count(*) FROM notifications`) != 1 {
		t.Fatal("refused forged context created message or alert")
	}
}

func TestStaffDisputeMessagesRequirePGPAndKeepPrivateContext(t *testing.T) {
	for _, role := range []string{"moderator", "admin"} {
		t.Run(role, func(t *testing.T) {
			e := newTestApp(t)
			buyerID, buyer := e.user("context_party", "buyer")
			vendorID, _ := e.user("context_seller", "vendor")
			staffID, staff := e.user("context_staff", role)
			_, watcher := e.user("context_observer", "admin")
			buyerKey, buyerPub := testPGPKey(t, "context_party")
			_, staffPub := testPGPKey(t, "context_staff")
			setMessageTestKey(e, buyerID, buyerPub)
			order := e.order(buyerID, e.product(vendorID, "physical"), "XMR", stateDisputed)
			dispute := randomToken()
			agExec(e, `INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Private original dispute evidence')`, dispute, order)
			ciphertext, err := encryptTo(buyerKey, "Private administrator or moderator update")
			if err != nil {
				t.Fatal(err)
			}
			f := url.Values{"recipient": {"context_party"}, "body": {ciphertext}, "dispute_id": {dispute}}
			// Staff status never permits sending without a public key or sending
			// plaintext to a party as a purported dispute/ticket message.
			e.check(e.do("POST", "/messages", staff, f), 409)
			setMessageTestKey(e, staffID, staffPub)
			plain := cloneValues(f)
			plain.Set("body", "An unencrypted private staff update must never be stored or sent.")
			e.check(e.do("POST", "/messages", staff, plain), 400)
			e.check(e.do("POST", "/messages", staff, f), 303)
			if agStr(e, `SELECT dispute_id FROM messages`) != dispute || agInt(e, `SELECT count(*) FROM messages WHERE order_id IS NOT NULL`) != 0 {
				t.Fatal("dispute context was missing or falsely duplicated as an order context")
			}
			page := e.body("GET", "/notifications", buyer, nil, 200)
			mustContain(t, page, "New message from context_staff regarding Dispute #"+dispute)
			mustNotContain(t, page, "-----BEGIN PGP MESSAGE-----", "Private original dispute evidence", "Private administrator or moderator update")
			mustNotContain(t, e.body("GET", "/notifications", watcher, nil, 200), dispute, "New message from context_staff")
			mustContain(t, e.body("GET", "/messages?to=context_staff", buyer, nil, 200), "dispute_id="+dispute)
		})
	}
}

func TestHiddenRecipientConfirmationPreservesMessageContext(t *testing.T) {
	e := newTestApp(t)
	buyerID, buyer := e.user("context_hidden_buyer", "buyer")
	vendorID, _ := e.user("context_hidden_vendor", "vendor")
	_, buyerPub := testPGPKey(t, "context_hidden_buyer")
	vendorKey, vendorPub := testPGPKey(t, "context_hidden_vendor")
	setMessageTestKey(e, buyerID, buyerPub)
	setMessageTestKey(e, vendorID, vendorPub)
	order := e.order(buyerID, e.product(vendorID, "physical"), "XMR", statePaid)
	ciphertext, err := encryptTo(vendorKey, "Hidden recipient order correspondence")
	if err != nil {
		t.Fatal(err)
	}
	f := url.Values{"recipient": {"context_hidden_vendor"}, "body": {hideRecipientIDs(t, ciphertext, vendorKey)}, "order_id": {order}}
	page := e.body("POST", "/messages", buyer, f, 400)
	mustContain(t, page, `name="order_id" value="`+order+`"`, `name="hidden_recipients"`)
	f.Set("hidden_recipients", "confirmed")
	e.check(e.do("POST", "/messages", buyer, f), 303)
	if agStr(e, `SELECT order_id FROM messages`) != order {
		t.Fatal("hidden-recipient confirmation lost the explicit order context")
	}
}

func TestPrivateMessagesRequireUsableSenderKeyForEveryRole(t *testing.T) {
	e := newTestApp(t)
	recipientID, _ := e.user("sender_key_target", "admin")
	key, public := testPGPKey(t, "sender_key_target")
	setMessageTestKey(e, recipientID, public)
	agExec(e, `INSERT INTO settings(key,value) VALUES('staff_contact','sender_key_target') ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	body, err := encryptTo(key, "Private staff support correspondence")
	if err != nil {
		t.Fatal(err)
	}
	unusable := testUnusableKeys(t, "sender_unusable")
	unusable["missing"] = ""
	unusable["unreadable"] = "not a public key"
	for _, role := range []string{"buyer", "vendor", "moderator", "admin"} {
		uid, session := e.user("sender_key_"+role, role)
		for name, public := range unusable {
			t.Run(role+"/"+name, func(t *testing.T) {
				if name == "unreadable" {
					agExec(e, `UPDATE users SET pgp=$2,pgp_fingerprint='' WHERE id=$1`, uid, public)
				} else {
					e.setKey(uid, public)
				}
				e.check(e.do("POST", "/messages", session, url.Values{"recipient": {"sender_key_target"}, "body": {body}}), 409)
				if agInt(e, `SELECT count(*) FROM messages`) != 0 || agInt(e, `SELECT count(*) FROM notifications`) != 0 {
					t.Fatal("a sender without a usable encryption key created private correspondence or alerts")
				}
			})
		}
	}
}
