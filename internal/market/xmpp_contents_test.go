package market

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func TestXMPPCiphertextRequiresActualEncryptionAndCurrentVisibleKey(t *testing.T) {
	key, _ := testPGPKey(t, "xmpp_ciphertext_owner")
	other, _ := testPGPKey(t, "xmpp_ciphertext_other")
	good, err := encryptTo(key, "private-message-content")
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := encryptTo(other, "private-message-content")
	if err != nil {
		t.Fatal(err)
	}
	withHeaders := strings.Replace(good, "-----BEGIN PGP MESSAGE-----\n", "-----BEGIN PGP MESSAGE-----\nComment: PRIVATE-ARMOR-HEADER-MUST-NOT-LEAVE\nVersion: private-client\n", 1)
	var signed bytes.Buffer
	w, err := armor.Encode(&signed, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := openpgp.Sign(w, key, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(signer, "signed plaintext must not leave")
	if err = signer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	mixedHidden := hideRecipientIDs(t, encryptToRecipients(t, "hidden extra recipient", key, other), other, other.Subkeys[0].PublicKey.KeyId)
	for _, test := range []struct {
		name, content string
		want          bool
	}{
		{"matching ciphertext", good, true},
		{"canonical strips private armor headers", withHeaders, true},
		{"plaintext", "private plaintext", false},
		{"fake armor", "-----BEGIN PGP MESSAGE-----\n\nZmFrZSBjaXBoZXJ0ZXh0\n-----END PGP MESSAGE-----", false},
		{"signed only", signed.String(), false},
		{"passphrase only", symmetricMessage(t, "private plaintext"), false},
		{"different recipient key", wrong, false},
		{"hidden recipient", hideRecipientIDs(t, good, key), false},
		{"visible owner and hidden extra recipient", mixedHidden, false},
		{"plaintext prefix", "PLAINTEXT BEFORE ARMOR\n" + good, false},
		{"plaintext suffix", good + "\nPLAINTEXT AFTER ARMOR", false},
		{"oversized", strings.Repeat("x", xmppMaxBodyBytes+1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			canonical, ok := xmppCiphertextForKey(test.content, key, time.Now())
			if ok != test.want {
				t.Fatalf("accepted=%v want=%v", ok, test.want)
			}
			if ok {
				if strings.Contains(canonical, "PRIVATE-ARMOR-HEADER") || strings.Contains(canonical, "private-client") || testDecrypt(t, key, canonical) != "private-message-content" {
					t.Fatal("canonical forwarding changed the encrypted message or retained armor headers")
				}
			} else if canonical != "" {
				t.Fatal("rejected content escaped the validator")
			}
		})
	}
	if _, ok := xmppCiphertextForKey(good, nil, time.Now()); ok {
		t.Fatal("ciphertext forwarded without a recipient key")
	}
	for name, public := range testUnusableKeys(t, "xmpp_unusable_contents") {
		unusable, _, err := parsePublicKey(public)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := xmppCiphertextForKey(good, unusable, time.Now()); ok {
			t.Fatalf("forwarded to %s", name)
		}
	}
	// A primary fingerprint can stay unchanged while its usable encryption
	// subkey changes. A stale PKESK must not pass solely because its ID is still
	// listed among the entity's revoked subkeys.
	if err := key.AddEncryptionSubkey(nil); err != nil {
		t.Fatal(err)
	}
	if err := key.RevokeSubkey(&key.Subkeys[0], packet.KeyCompromised, "test-only retired encryption subkey", nil); err != nil {
		t.Fatal(err)
	}
	if !canReceiveEncrypted(key, time.Now()) {
		t.Fatal("subkey rotation fixture lost every usable encryption key")
	}
	if _, ok := xmppCiphertextForKey(good, key, time.Now()); ok {
		t.Fatal("forwarded ciphertext naming only the revoked encryption subkey")
	}
}

func TestXMPPContentsLargeUnicodeDeliverySurvivesTLS(t *testing.T) {
	key, _ := testPGPKey(t, "xmpp_large_delivery")
	plaintext := strings.Repeat("🔐", maxDeliveryChars)
	ciphertext, err := encryptTo(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	canonical, ok := xmppCiphertextForKey(ciphertext, key, time.Now())
	if !ok || len(canonical) <= 32768 || len(canonical) >= xmppMaxBodyBytes {
		t.Fatalf("maximum Unicode delivery not supported: accepted=%v bytes=%d", ok, len(canonical))
	}
	body := "Order 12345678: digital delivery is available.\n\nEncrypted digital delivery:\n" + canonical
	cfg, roots, done := xmppTestServer(t, "direct", "success")
	if err := sendXMPPWithRoots(context.Background(), cfg, "buyer@example.test", "large-delivery", body, roots); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || result.message == nil || result.message.child(xmppClientNS, "body").Text != body {
		t.Fatalf("large ciphertext altered during submission: %v", result.err)
	}
	if strings.Contains(body, "🔐") || testDecrypt(t, key, canonical) != plaintext {
		t.Fatal("large delivery leaked plaintext or did not decrypt exactly")
	}
}

func enableXMPPContents(e *testEnv, userID string) {
	e.t.Helper()
	enableXMPPDelivery(e)
	agExec(e, `UPDATE settings SET value=jsonb_set(jsonb_set(value::jsonb,'{allow_contents}','true'),'{content_policy_version}','1')::text WHERE key='xmpp_config'`)
	xmppOptIn(e, userID)
	agExec(e, `UPDATE users SET xmpp_include_contents=true WHERE id=$1`, userID)
}

func xmppLinkedMessage(e *testEnv, sender, recipient, ciphertext, orderID, disputeID string) (string, string) {
	e.t.Helper()
	messageID, noticeID := randomToken(), randomToken()
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body,encrypted,recipient_match,order_id,dispute_id) VALUES($1,$2,$3,$4,true,'yes',NULLIF($5,''),NULLIF($6,''))`, messageID, sender, recipient, ciphertext, orderID, disputeID)
	agExec(e, `INSERT INTO notifications(id,user_id,body,message_id,order_id) VALUES($1,$2,'New encrypted message',$3,NULLIF($4,''))`, noticeID, recipient, messageID, orderID)
	return messageID, noticeID
}

func TestXMPPContentsMessagePolicyOwnershipAndKeyRechecks(t *testing.T) {
	e := newTestApp(t)
	sender, _ := e.user("xmpp_contents_sender", "vendor")
	uid, _ := e.user("xmpp_contents_recipient", "buyer")
	otherID, _ := e.user("xmpp_contents_other", "buyer")
	key, public := testPGPKey(t, "xmpp_contents_recipient")
	otherKey, _ := testPGPKey(t, "xmpp_contents_other")
	e.setKey(uid, public)
	fingerprint := agStr(e, `SELECT pgp_fingerprint FROM users WHERE id=$1`, uid)
	ciphertext, err := encryptTo(key, "PRIVATE-MESSAGE-MUST-STAY-ENCRYPTED")
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := encryptTo(otherKey, "PRIVATE-MESSAGE-MUST-STAY-ENCRYPTED")
	if err != nil {
		t.Fatal(err)
	}
	enableXMPPContents(e, uid)
	messageID, noticeID := xmppLinkedMessage(e, sender, uid, ciphertext, "", "")
	const summary = "New encrypted message"
	check := func(want bool, include bool, fp string) {
		t.Helper()
		body, err := xmppNotificationBody(context.Background(), e.DB, noticeID, uid, summary, include, fp)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, "PRIVATE-MESSAGE-MUST-STAY-ENCRYPTED") || strings.HasPrefix(body, summary+"\n\nEncrypted message:\n-----BEGIN PGP MESSAGE-----") != want {
			t.Fatalf("included=%v want=%v; plaintext leaked=%v", body != summary, want, strings.Contains(body, "PRIVATE-MESSAGE-MUST-STAY-ENCRYPTED"))
		}
		if !want && body != summary {
			t.Fatal("unsafe inclusion did not preserve the frontend summary")
		}
	}
	check(true, true, fingerprint)
	check(false, false, fingerprint)
	check(false, true, "")
	check(false, true, strings.Repeat("0", len(fingerprint)))
	for _, test := range []struct {
		name, query string
		args        []any
		restore     string
		restoreArgs []any
	}{
		{"admin revoked contents", `UPDATE settings SET value=jsonb_set(value::jsonb,'{allow_contents}','false')::text WHERE key='xmpp_config'`, nil, `UPDATE settings SET value=jsonb_set(value::jsonb,'{allow_contents}','true')::text WHERE key='xmpp_config'`, nil},
		{"user revoked contents", `UPDATE users SET xmpp_include_contents=false WHERE id=$1`, []any{uid}, `UPDATE users SET xmpp_include_contents=true WHERE id=$1`, []any{uid}},
		{"user disabled XMPP", `UPDATE users SET xmpp_enabled=false WHERE id=$1`, []any{uid}, `UPDATE users SET xmpp_enabled=true WHERE id=$1`, []any{uid}},
		{"suspended recipient", `UPDATE users SET suspended_at=now() WHERE id=$1`, []any{uid}, `UPDATE users SET suspended_at=NULL WHERE id=$1`, []any{uid}},
		{"not message recipient", `UPDATE messages SET recipient_id=$2 WHERE id=$1`, []any{messageID, otherID}, `UPDATE messages SET recipient_id=$2 WHERE id=$1`, []any{messageID, uid}},
		{"forged encrypted flag", `UPDATE messages SET body='PRIVATE-MESSAGE-MUST-STAY-ENCRYPTED',encrypted=true WHERE id=$1`, []any{messageID}, `UPDATE messages SET body=$2 WHERE id=$1`, []any{messageID, ciphertext}},
		{"uninspected legacy flag", `UPDATE messages SET encrypted=NULL WHERE id=$1`, []any{messageID}, `UPDATE messages SET encrypted=true WHERE id=$1`, []any{messageID}},
		{"unmatched recipient metadata", `UPDATE messages SET recipient_match='unknown' WHERE id=$1`, []any{messageID}, `UPDATE messages SET recipient_match='yes' WHERE id=$1`, []any{messageID}},
		{"forged recipient match", `UPDATE messages SET body=$2 WHERE id=$1`, []any{messageID, wrong}, `UPDATE messages SET body=$2 WHERE id=$1`, []any{messageID, ciphertext}},
		{"passphrase-only ciphertext", `UPDATE messages SET body=$2 WHERE id=$1`, []any{messageID, symmetricMessage(t, "PRIVATE-MESSAGE-MUST-STAY-ENCRYPTED")}, `UPDATE messages SET body=$2 WHERE id=$1`, []any{messageID, ciphertext}},
		{"hidden ciphertext", `UPDATE messages SET body=$2 WHERE id=$1`, []any{messageID, hideRecipientIDs(t, ciphertext, key)}, `UPDATE messages SET body=$2 WHERE id=$1`, []any{messageID, ciphertext}},
	} {
		t.Run(test.name, func(t *testing.T) {
			agExec(e, test.query, test.args...)
			check(false, true, fingerprint)
			agExec(e, test.restore, test.restoreArgs...)
		})
	}
	_, replacement := testPGPKey(t, "xmpp_replacement_key")
	e.setKey(uid, replacement)
	check(false, true, fingerprint)
	e.setKey(uid, "")
	check(false, true, fingerprint)
	e.setKey(uid, public)
	check(true, true, fingerprint)
	agExec(e, `DELETE FROM messages WHERE id=$1`, messageID)
	check(false, true, fingerprint)
}

func TestXMPPContentsMessageUsesSavedAuthorizedOrderAndDisputeContext(t *testing.T) {
	w := newOrderWorld(t)
	e := w.e
	key, public := testPGPKey(t, "xmpp_context_buyer")
	e.setKey(w.buyerID, public)
	enableXMPPContents(e, w.buyerID)
	fingerprint := agStr(e, `SELECT pgp_fingerprint FROM users WHERE id=$1`, w.buyerID)
	orderID := e.order(w.buyerID, e.product(w.vendorID, "physical"), "XMR", stateDisputed)
	otherOrder := e.order(w.otherID, e.product(w.vendorID, "physical"), "XMR", statePaid)
	disputeID := randomToken()
	agExec(e, `INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'test context')`, disputeID, orderID)
	ciphertext, err := encryptTo(key, "encrypted contextual message")
	if err != nil {
		t.Fatal(err)
	}
	messageID, noticeID := xmppLinkedMessage(e, w.vendorID, w.buyerID, ciphertext, "", disputeID)
	agExec(e, `UPDATE notifications SET order_id=$2 WHERE id=$1`, noticeID, orderID)
	body, err := xmppNotificationBody(context.Background(), e.DB, noticeID, w.buyerID, "New encrypted message", true, fingerprint)
	if err != nil || !strings.Contains(body, "Dispute "+shortID(disputeID)+" (order "+shortID(orderID)+"): Encrypted message:") {
		t.Fatalf("missing saved context: %v", err)
	}
	agExec(e, `UPDATE messages SET order_id=$2,dispute_id=NULL WHERE id=$1`, messageID, orderID)
	body, err = xmppNotificationBody(context.Background(), e.DB, noticeID, w.buyerID, "New encrypted message", true, fingerprint)
	if err != nil || !strings.Contains(body, "Order "+shortID(orderID)+": Encrypted message:") {
		t.Fatalf("missing saved order context: %v", err)
	}
	agExec(e, `UPDATE messages SET order_id=$2 WHERE id=$1`, messageID, otherOrder)
	body, err = xmppNotificationBody(context.Background(), e.DB, noticeID, w.buyerID, "New encrypted message", true, fingerprint)
	if err != nil || body != "New encrypted message" {
		t.Fatalf("forwarded unrelated/mismatched order context: %v", err)
	}
}

func TestXMPPContentsDigitalDeliveryIsEncryptedAndBuyerOnly(t *testing.T) {
	w := newOrderWorld(t)
	e := w.e
	adminID, _ := e.user("xmpp_delivery_admin", "admin")
	key, public := testPGPKey(t, "xmpp_delivery_buyer")
	e.setKey(w.buyerID, public)
	enableXMPPContents(e, w.buyerID)
	fingerprint := agStr(e, `SELECT pgp_fingerprint FROM users WHERE id=$1`, w.buyerID)
	orderID := e.order(w.buyerID, e.product(w.vendorID, "digital"), "XMR", stateCompleted)
	const plaintext = "PRIVATE-DIGITAL-LICENCE-CONTENT"
	ciphertext, err := encryptTo(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, `INSERT INTO deliveries(order_id,content,encrypted,recipient_fingerprint) VALUES($1,$2,true,$3)`, orderID, ciphertext, fingerprint)
	for role, uid := range map[string]string{"buyer": w.buyerID, "vendor": w.vendorID, "moderator": w.modID, "admin": adminID, "unrelated buyer": w.otherID} {
		if uid != w.buyerID {
			e.setKey(uid, public) // Even possessing the same saved key does not authorize another party's delivery.
			xmppOptIn(e, uid)
			agExec(e, `UPDATE users SET xmpp_include_contents=true WHERE id=$1`, uid)
		}
		noticeID := randomToken()
		agExec(e, `INSERT INTO notifications(id,user_id,body,order_id,delivery_order_id) VALUES($1,$2,'Digital delivery is available',$3,$3)`, noticeID, uid, orderID)
		body, err := xmppNotificationBody(context.Background(), e.DB, noticeID, uid, "Digital delivery is available", true, fingerprint)
		if err != nil || strings.Contains(body, plaintext) {
			t.Fatalf("%s: unsafe delivery body/error: %v", role, err)
		}
		if role == "buyer" {
			prefix := "Digital delivery is available\n\nOrder " + shortID(orderID) + ": Encrypted digital delivery:\n"
			if !strings.HasPrefix(body, prefix) || testDecrypt(t, key, strings.TrimPrefix(body, prefix)) != plaintext {
				t.Fatal("buyer did not receive its exact encrypted delivery")
			}
		} else if body != "Digital delivery is available" {
			t.Fatalf("%s received another buyer's encrypted delivery", role)
		}
	}
	noticeID := randomToken()
	agExec(e, `INSERT INTO notifications(id,user_id,body,order_id,delivery_order_id) VALUES($1,$2,'Digital delivery is available',$3,$3)`, noticeID, w.buyerID, orderID)
	for _, test := range []struct {
		content, fingerprint string
		encrypted            bool
	}{
		{plaintext, "", false},         // Legacy plaintext is never upgraded by forwarding it.
		{plaintext, fingerprint, true}, // A forged flag cannot turn plaintext into ciphertext.
		{ciphertext, "wrong-fingerprint", true},
	} {
		agExec(e, `UPDATE deliveries SET content=$2,encrypted=$3,recipient_fingerprint=$4 WHERE order_id=$1`, orderID, test.content, test.encrypted, test.fingerprint)
		body, err := xmppNotificationBody(context.Background(), e.DB, noticeID, w.buyerID, "Digital delivery is available", true, fingerprint)
		if err != nil || body != "Digital delivery is available" {
			t.Fatalf("legacy/forged/mismatched delivery was forwarded: %v", err)
		}
	}
}

func TestXMPPContentsWorkerKeepsEnqueueConsentAndKeySnapshots(t *testing.T) {
	e := newTestApp(t)
	sender, _ := e.user("xmpp_snapshot_sender", "vendor")
	uid, _ := e.user("xmpp_snapshot_recipient", "buyer")
	key, public := testPGPKey(t, "xmpp_snapshot_recipient")
	_, replacement := testPGPKey(t, "xmpp_snapshot_replacement")
	e.setKey(uid, public)
	enableXMPPContents(e, uid)
	fingerprint := agStr(e, `SELECT pgp_fingerprint FROM users WHERE id=$1`, uid)
	ciphertext, err := encryptTo(key, "PRIVATE-SNAPSHOT-MESSAGE-CONTENT")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"matching snapshot", "admin allowed only after enqueue", "user consented only after enqueue", "admin toggled permission", "recipient replaced key", "missing enqueue key fingerprint", "source removed"} {
		t.Run(name, func(t *testing.T) {
			e.setKey(uid, public)
			agExec(e, `UPDATE users SET xmpp_include_contents=true WHERE id=$1`, uid)
			agExec(e, `UPDATE settings SET value=jsonb_set(jsonb_set(value::jsonb,'{allow_contents}','true'),'{content_policy_version}','1')::text WHERE key='xmpp_config'`)
			switch name {
			case "admin allowed only after enqueue":
				agExec(e, `UPDATE settings SET value=jsonb_set(value::jsonb,'{allow_contents}','false')::text WHERE key='xmpp_config'`)
			case "user consented only after enqueue":
				agExec(e, `UPDATE users SET xmpp_include_contents=false WHERE id=$1`, uid)
			case "missing enqueue key fingerprint":
				agExec(e, `UPDATE users SET pgp_fingerprint='' WHERE id=$1`, uid)
			}
			messageID, noticeID := xmppLinkedMessage(e, sender, uid, ciphertext, "", "")
			switch name {
			case "admin allowed only after enqueue":
				agExec(e, `UPDATE settings SET value=jsonb_set(jsonb_set(value::jsonb,'{allow_contents}','true'),'{content_policy_version}','2')::text WHERE key='xmpp_config'`)
			case "user consented only after enqueue":
				agExec(e, `UPDATE users SET xmpp_include_contents=true WHERE id=$1`, uid)
			case "admin toggled permission":
				agExec(e, `UPDATE settings SET value=jsonb_set(value::jsonb,'{allow_contents}','false')::text WHERE key='xmpp_config'`)
				agExec(e, `UPDATE settings SET value=jsonb_set(jsonb_set(value::jsonb,'{allow_contents}','true'),'{content_policy_version}','3')::text WHERE key='xmpp_config'`)
			case "recipient replaced key":
				e.setKey(uid, replacement)
			case "missing enqueue key fingerprint":
				agExec(e, `UPDATE users SET pgp_fingerprint=$2 WHERE id=$1`, uid, fingerprint)
			case "source removed":
				agExec(e, `DELETE FROM messages WHERE id=$1`, messageID)
			}
			calls := 0
			worked, err := e.A.deliverXMPPNotification(context.Background(), func(_ context.Context, _ xmppConfig, _, messageID, body string) error {
				calls++
				if messageID != "market-"+noticeID || strings.Contains(body, "PRIVATE-SNAPSHOT-MESSAGE-CONTENT") {
					t.Fatal("worker changed notification identity or leaked plaintext")
				}
				if name == "matching snapshot" {
					prefix := "New encrypted message\n\nEncrypted message:\n"
					if !strings.HasPrefix(body, prefix) || testDecrypt(t, key, strings.TrimPrefix(body, prefix)) != "PRIVATE-SNAPSHOT-MESSAGE-CONTENT" {
						t.Fatal("worker did not deliver the checked ciphertext")
					}
				} else if body != "New encrypted message" {
					t.Fatal("worker appended contents outside enqueue-time and current permission/key boundaries")
				}
				return nil
			})
			if err != nil || !worked || calls != 1 || agStr(e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, noticeID) != "sent" {
				t.Fatalf("worker submission failed: worked=%v calls=%d err=%v", worked, calls, err)
			}
		})
	}
}
