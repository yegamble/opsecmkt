package market

import (
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

func TestOrderPGPRequiredForEveryBuyerRoleAndRecheckedAtCheckout(t *testing.T) {
	x := newXMREscrowEnv(t)
	e := x.testEnv
	x.mustPoll()
	product := x.digital(x.vendor)
	for _, role := range []string{"buyer", "vendor", "moderator", "admin"} {
		id, session := e.keylessUser("order_key_"+role, role)
		form := url.Values{"product_id": {product}, "currency": {"XMR"}}
		e.check(e.do("POST", "/orders", session, cloneValues(form)), 403)
		if n := agInt(e, "SELECT count(*) FROM orders WHERE buyer_id=$1", id); n != 0 {
			t.Fatalf("keyless %s created an order", role)
		}
		page := e.body("GET", "/checkout?id="+product, session, nil, 200)
		mustContain(t, page, "Add a usable PGP public key", `href="/account"`)
		mustNotContain(t, page, `action="/orders"`)
		_, public := testPGPKey(t, "order-key-"+role)
		e.setKey(id, public)
		// A saved public key alone cannot authorize purchases: this session must prove decryption too.
		e.check(e.do("POST", "/orders", session, cloneValues(form)), 403)
		if n := agInt(e, "SELECT count(*) FROM orders WHERE buyer_id=$1", id); n != 0 {
			t.Fatalf("unverified %s created an order", role)
		}
		e.pgpSessionKey(session)
		// Session proof does not require enabling the optional global PGP sign-in factor.
		if n := agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND pgp_verified_at IS NULL AND NOT pgp_2fa", id); n != 1 {
			t.Fatal("fixture unexpectedly enabled PGP ownership proof or sign-in")
		}
		e.check(e.do("POST", "/orders", session, cloneValues(form)), 303)
		order := agStr(e, "SELECT id FROM orders WHERE buyer_id=$1 AND state='draft'", id)
		stock := agInt(e, "SELECT stock FROM products WHERE id=$1", product)
		agExec(e, "UPDATE users SET pgp='',pgp_fingerprint='' WHERE id=$1", id)
		checkout := url.Values{"order_id": {order}, "refund_address": {testXMRAddr("order-key-refund-"+role, true)}}
		e.check(e.do("POST", "/orders/escrow-xmr", session, cloneValues(checkout)), 403)
		if x.state(order) != stateDraft || agInt(e, "SELECT stock FROM products WHERE id=$1", product) != stock || agInt(e, "SELECT count(*) FROM escrows WHERE order_id=$1", order) != 0 {
			t.Fatalf("missing-key %s checkout changed payment or stock", role)
		}
		page = e.body("GET", orderPage(order), session, nil, 200)
		mustContain(t, page, pgpAccessRequiredMessage)
		mustNotContain(t, page, `action="/orders/escrow-xmr"`)
		e.setKey(id, public)
		e.pgpSessionKey(session)
		e.check(e.do("POST", "/orders/escrow-xmr", session, checkout), 303)
	}
	// A digital vendor also needs an encryption key; a valid buyer key does not bypass it.
	agExec(e, "UPDATE users SET pgp='',pgp_fingerprint='' WHERE id=$1", x.vendor)
	e.check(e.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {product}, "currency": {"XMR"}}), 409)
	mustNotContain(t, e.body("GET", "/checkout?id="+product, x.buyerSess, nil, 200), `action="/orders"`)
}

func TestOrderPGPRefusesUnusableKeysAtDraftAndCheckout(t *testing.T) {
	x := newXMREscrowEnv(t)
	e := x.testEnv
	x.mustPoll()
	product := x.digital(x.vendor)
	for name, public := range testUnusableKeys(t, "order_unusable") {
		id, session := e.user("unusable_"+randomToken()[:8], "buyer")
		e.setKey(id, public)
		e.check(e.do("POST", "/orders", session, url.Values{"product_id": {product}, "currency": {"XMR"}}), 403)
		order := e.order(id, product, "XMR", stateDraft)
		e.check(x.checkout(session, order, testXMRAddr("unusable-order-"+name, true)), 403)
		if x.state(order) != stateDraft || agInt(e, "SELECT count(*) FROM escrows WHERE order_id=$1", order) != 0 {
			t.Fatalf("%s opened payment despite an unusable buyer key", name)
		}
	}
	// An unreadable legacy value cannot act as a saved encryption key either.
	id, session := e.user("unreadable_order_key", "buyer")
	agExec(e, "UPDATE users SET pgp='not a public key' WHERE id=$1", id)
	e.check(e.do("POST", "/orders", session, url.Values{"product_id": {product}, "currency": {"XMR"}}), 403)
}

func deliveryDecryptsWith(key *openpgp.Entity, ciphertext string) bool {
	block, err := armor.Decode(strings.NewReader(ciphertext))
	if err != nil {
		return false
	}
	message, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{key}, nil, nil)
	if err != nil {
		return false
	}
	_, err = io.ReadAll(message.UnverifiedBody)
	return err == nil
}

func TestDigitalDeliveryEncryptsEachOrderOnlyForItsBuyer(t *testing.T) {
	x := newXMREscrowEnv(t)
	e := x.testEnv
	firstKey, firstPublic := testPGPKey(t, "delivery-first-buyer")
	secondKey, secondPublic := testPGPKey(t, "delivery-second-buyer")
	e.setKey(x.buyer, firstPublic)
	e.pgpSessionKey(x.buyerSess)
	secondBuyer, secondSession := e.user("delivery_second_buyer", "buyer")
	e.setKey(secondBuyer, secondPublic)
	e.pgpSessionKey(secondSession)
	moderator, _ := e.user("delivery_observer_mod", "moderator")
	enableXMPPContents(e, x.buyer)
	xmppOptIn(e, secondBuyer)
	agExec(e, "UPDATE users SET xmpp_include_contents=true WHERE id=$1", secondBuyer)
	for _, id := range []string{x.vendor, x.admin, moderator} {
		xmppOptIn(e, id)
		agExec(e, "UPDATE users SET xmpp_include_contents=true WHERE id=$1", id)
	}
	const plaintext = "PRIVATE-DIGITAL-LICENCE-KEY-FOR-PAID-BUYER"
	product := x.digital(x.vendor)
	agExec(e, "UPDATE products SET delivery_content=$2 WHERE id=$1", product, plaintext)
	x.mustPoll()
	var ciphertexts []string
	for _, buyer := range []struct {
		id, session string
		key, other  *openpgp.Entity
	}{{x.buyer, x.buyerSess, firstKey, secondKey}, {secondBuyer, secondSession, secondKey, firstKey}} {
		order := e.order(buyer.id, product, "XMR", stateDraft)
		e.check(x.checkout(buyer.session, order, testXMRAddr("encrypted-delivery-"+order, true)), 303)
		x.fake.payAddress(agStr(e, "SELECT address FROM escrows WHERE order_id=$1", order), hex64("encrypted-delivery-"+order), xmrListingPrice, 10)
		x.mustPoll()
		ciphertext := agStr(e, "SELECT content FROM deliveries WHERE order_id=$1 AND encrypted", order)
		if x.state(order) != stateDelivered || strings.Contains(ciphertext, plaintext) || testDecrypt(t, buyer.key, ciphertext) != plaintext || deliveryDecryptsWith(buyer.other, ciphertext) {
			t.Fatal("per-order delivery did not enforce the buyer encryption boundary")
		}
		ciphertexts = append(ciphertexts, ciphertext)
		notice := agStr(e, "SELECT id FROM notifications WHERE delivery_order_id=$1", order)
		if n := agInt(e, "SELECT count(*) FROM notifications WHERE delivery_order_id=$1 AND (user_id<>$2 OR audience<>'personal')", order, buyer.id); n != 0 {
			t.Fatal("digital content notification was linked to a vendor or staff recipient")
		}
		if n := agInt(e, "SELECT count(*) FROM xmpp_outbox WHERE notification_id=$1 AND include_contents", notice); n != 1 {
			t.Fatal("opted-in buyer did not get a linked encrypted delivery queue entry")
		}
		summary := agStr(e, "SELECT body FROM notifications WHERE id=$1", notice)
		fingerprint := agStr(e, "SELECT pgp_fingerprint FROM users WHERE id=$1", buyer.id)
		body, err := xmppNotificationBody(t.Context(), e.DB, notice, buyer.id, summary, true, fingerprint)
		if err != nil || !strings.Contains(body, strings.TrimSpace(ciphertext)) || strings.Contains(body, plaintext) || !strings.Contains(body, "Order "+shortID(order)) {
			t.Fatalf("encrypted delivery was not safely composed for its buyer: %v", err)
		}
		page := e.body("GET", orderPage(order), buyer.session, nil, 200)
		mustContain(t, page, "Encrypted to the buyer", "-----BEGIN PGP MESSAGE-----")
		mustNotContain(t, page, plaintext)
	}
	if ciphertexts[0] == ciphertexts[1] || agStr(e, "SELECT delivery_content FROM products WHERE id=$1", product) != plaintext {
		t.Fatal("shared inventory was replaced with one buyer's delivery ciphertext")
	}
}

func TestDigitalDeliveryKeyLossDoesNotRollBackConfirmedPayment(t *testing.T) {
	x := newXMREscrowEnv(t)
	e := x.testEnv
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	address := x.mustCheckout(order)
	agExec(e, "UPDATE users SET pgp='',pgp_fingerprint='' WHERE id=$1", x.buyer)
	x.fake.payAddress(address, hex64("delivery-key-lost"), xmrListingPrice, 10)
	x.mustPoll()
	if x.state(order) != statePaid || agInt(e, "SELECT count(*) FROM payments WHERE order_id=$1 AND credited", order) != 1 || agInt(e, "SELECT count(*) FROM deliveries WHERE order_id=$1", order) != 0 {
		t.Fatal("loss of a delivery key lost the payment confirmation or stored plaintext")
	}
	if n := agInt(e, "SELECT count(*) FROM notifications WHERE delivery_order_id=$1", order); n != 0 {
		t.Fatal("unavailable encrypted delivery still created a content notification")
	}
	before := agInt(e, "SELECT count(*) FROM notifications WHERE order_id=$1", order)
	x.mustPoll()
	if n := agInt(e, "SELECT count(*) FROM notifications WHERE order_id=$1", order); n != before {
		t.Fatal("a repeated payment pass repeated the paused delivery alerts")
	}
	newKey, newPublic := testPGPKey(t, "repaired-delivery-key")
	e.setKey(x.buyer, newPublic)
	e.check(e.do("POST", "/orders/deliver", x.vendorSess, url.Values{"order_id": {order}, "content": {"repaired buyer delivery"}}), 303)
	if x.state(order) != stateDelivered || testDecrypt(t, newKey, agStr(e, "SELECT content FROM deliveries WHERE order_id=$1", order)) != "repaired buyer delivery" {
		t.Fatal("vendor could not safely deliver after the buyer repaired their PGP key")
	}
}

func TestManualDigitalDeliveryValidatesCiphertextAndEncryptsMaximumContent(t *testing.T) {
	w := newDisputeOrderWorld(t)
	e := w.e
	buyerKey, buyerPublic := testPGPKey(t, "manual-delivery-buyer")
	otherKey, _ := testPGPKey(t, "manual-delivery-other")
	e.setKey(w.buyerID, buyerPublic)
	e.pgpSessionKey(w.buyer)
	order := w.fundedFor(w.buyerID, w.buyer, w.vendorID, w.vendor)
	good, err := encryptTo(buyerKey, "already encrypted digital content")
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := encryptTo(otherKey, "wrong recipient digital content")
	if err != nil {
		t.Fatal(err)
	}
	before := agInt(e, "SELECT count(*) FROM notifications")
	for _, content := range []string{wrong, hideRecipientIDs(t, good, buyerKey), symmetricMessage(t, "passphrase digital content"), "-----BEGIN PGP MESSAGE-----\n\nZmFrZQ==\n-----END PGP MESSAGE-----"} {
		e.check(e.do("POST", "/orders/deliver", w.vendor, url.Values{"order_id": {order}, "content": {content}}), 400)
		if w.state(order) != statePaid || agInt(e, "SELECT count(*) FROM deliveries WHERE order_id=$1", order) != 0 || agInt(e, "SELECT count(*) FROM notifications") != before {
			t.Fatal("rejected digital ciphertext changed delivery, order state or alerts")
		}
	}
	e.check(e.do("POST", "/orders/deliver", w.buyer, url.Values{"order_id": {order}, "content": {"buyer cannot impersonate vendor delivery"}}), 403)
	plaintext := strings.Repeat("Q", maxDeliveryChars)
	e.check(e.do("POST", "/orders/deliver", w.vendor, url.Values{"order_id": {order}, "content": {plaintext}}), 303)
	ciphertext := agStr(e, "SELECT content FROM deliveries WHERE order_id=$1 AND encrypted", order)
	if len(ciphertext) <= maxDeliveryChars || len(ciphertext) >= 262144 || strings.Contains(ciphertext, strings.Repeat("Q", 64)) || testDecrypt(t, buyerKey, ciphertext) != plaintext {
		t.Fatal("maximum manual delivery was truncated, leaked plaintext or did not encrypt")
	}
	// A vendor may supply existing ciphertext without double encryption; private armor headers are discarded.
	second := w.fundedFor(w.buyerID, w.buyer, w.vendorID, w.vendor)
	withHeader := strings.Replace(good, "-----BEGIN PGP MESSAGE-----\n", "-----BEGIN PGP MESSAGE-----\nComment: PRIVATE-DELIVERY-HEADER\n", 1)
	e.check(e.do("POST", "/orders/deliver", w.vendor, url.Values{"order_id": {second}, "content": {withHeader}}), 303)
	stored := agStr(e, "SELECT content FROM deliveries WHERE order_id=$1", second)
	if strings.Contains(stored, "PRIVATE-DELIVERY-HEADER") || testDecrypt(t, buyerKey, stored) != "already encrypted digital content" {
		t.Fatal("manual existing ciphertext was not canonical or was encrypted twice")
	}
	// The source is encrypted; the frontend notification still contains no plaintext or armored copy.
	if n := agInt(e, "SELECT count(*) FROM notifications WHERE delivery_order_id=$1 AND (body LIKE '%PGP MESSAGE%' OR body LIKE '%already encrypted digital content%')", second); n != 0 {
		t.Fatal("digital content was copied into frontend notification text")
	}
}

func TestDigitalAutoDeliveryEncryptsMaximumUnicodeContent(t *testing.T) {
	x := newXMREscrowEnv(t)
	e := x.testEnv
	plaintext := strings.Repeat("🔐", maxDeliveryChars)
	product := x.digital(x.vendor)
	agExec(e, "UPDATE products SET delivery_content=$2 WHERE id=$1", product, plaintext)
	x.mustPoll()
	order := x.draft(product)
	address := x.mustCheckout(order)
	x.fake.payAddress(address, hex64("maximum-unicode-digital-delivery"), xmrListingPrice, 10)
	x.mustPoll()
	ciphertext := agStr(e, "SELECT content FROM deliveries WHERE order_id=$1 AND encrypted", order)
	if len(ciphertext) <= maxDeliveryChars || len(ciphertext) >= 262144 || strings.Contains(ciphertext, "🔐") || commerceDelivery(e, order) != plaintext {
		t.Fatal("maximum stored Unicode delivery was truncated, leaked plaintext or did not encrypt")
	}
}
