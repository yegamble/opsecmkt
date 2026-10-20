package market

import (
	"net/url"
	"strings"
	"testing"
)

// The server may normalize transport armor, but must never wrap user-encrypted
// content in another encryption layer or change the encrypted packet stream.
func TestPastedCiphertextIsNeverEncryptedAgain(t *testing.T) {
	e := newTestApp(t)
	key, pub := testPGPKey(t, "preserved_recipient")
	other, _ := testPGPKey(t, "preserved_additional")
	senderID, sender := e.user("preserved_sender", "buyer")
	recipientID, recipient := e.user("preserved_vendor", "vendor")
	e.setKey(recipientID, pub)
	const plaintext = "Private original text: résumé, 日本語, and order note 42.\nSecond line."
	ciphertext := encryptToRecipients(t, plaintext, key)
	withHeaders := strings.Replace(ciphertext, "-----BEGIN PGP MESSAGE-----\n", "-----BEGIN PGP MESSAGE-----\nVersion: Local PGP application\nComment: metadata must be removed\n", 1)
	for _, tc := range []struct{ name, body string }{
		{"normal", ciphertext},
		{"surrounding whitespace", " \n\t" + ciphertext + "\n\t "},
		{"CRLF and armor headers", strings.ReplaceAll(withHeaders, "\n", "\r\n")},
		{"additional recipient", encryptToRecipients(t, plaintext, key, other)},
		{"hidden recipient", hideRecipientIDs(t, ciphertext, key)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := agInt(e, "SELECT count(*) FROM messages WHERE sender_id=$1", senderID)
			form := url.Values{"recipient": {"preserved_vendor"}, "body": {tc.body}, "hidden_recipients": {"confirmed"}, formNonceField: {randomToken()[:32]}}
			e.check(e.do("POST", "/messages", sender, form), 303)
			stored := agStr(e, "SELECT body FROM messages WHERE sender_id=$1 ORDER BY created DESC,id LIMIT 1", senderID)
			if armorPackets(t, stored) != armorPackets(t, tc.body) {
				t.Fatal("encrypted packet bytes changed during submission")
			}
			if got := testDecrypt(t, key, stored); got != plaintext {
				t.Fatalf("one decryption returned %q instead of the original plaintext", got)
			}
			if strings.Contains(stored, "Local PGP application") || strings.Contains(stored, "metadata must be removed") || strings.Contains(stored, plaintext) {
				t.Fatal("plaintext or identifying armor headers were stored")
			}
			// A repeat of the exact form is the same send, including its ciphertext.
			e.check(e.do("POST", "/messages", sender, form), 303)
			if got := agInt(e, "SELECT count(*) FROM messages WHERE sender_id=$1", senderID); got != before+1 {
				t.Fatalf("replay stored %d rows, expected %d", got, before+1)
			}
		})
	}
	for _, session := range []string{sender, recipient} {
		page := e.body("GET", "/messages", session, nil, 200)
		if strings.Contains(page, plaintext) || strings.Contains(page, "metadata must be removed") {
			t.Fatal("a reloaded inbox exposed plaintext or discarded armor metadata")
		}
	}
	if agInt(e, "SELECT count(*) FROM audit_events WHERE action LIKE $1", "%"+plaintext+"%") != 0 ||
		agInt(e, "SELECT count(*) FROM notifications WHERE body LIKE $1", "%"+plaintext+"%") != 0 {
		t.Fatal("message plaintext appeared in audit or notification content")
	}
}
