package market

import (
	"html"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOrderMessagesStayOnTheirOrderAndPreserveCiphertext(t *testing.T) {
	p := newPayEnv(t)
	vendorKey, vendorPub := testPGPKey(t, "order_recipient")
	p.setKey(p.vendor.ID, vendorPub)
	order, _ := p.newOrder(statePaid)
	other, _ := p.newOrder(statePaid)
	plaintext := "Order-only shipping details; never global mail"
	ciphertext := encryptToRecipients(t, plaintext, vendorKey)
	f := url.Values{"order_id": {order}, "body": {strings.ReplaceAll(ciphertext, "\n", "\r\n")}, formNonceField: {randomToken()[:32]}}
	p.check(p.do("POST", "/orders/message", p.buyerSess, f), 303)
	p.check(p.do("POST", "/orders/message", p.buyerSess, f), 303)
	if n := p.count("SELECT count(*) FROM order_messages WHERE order_id=$1", order); n != 1 {
		t.Fatalf("nonce replay left %d messages", n)
	}
	stored := p.str("SELECT body FROM order_messages WHERE order_id=$1", order)
	canonical, err := canonicalMessage(ciphertext)
	if err != nil || stored != canonical || testDecrypt(t, vendorKey, stored) != plaintext {
		t.Fatal("ciphertext changed or needs a second decrypt")
	}
	for _, session := range []string{p.buyerSess, p.vendorSess} {
		page := html.UnescapeString(p.page(orderPage(order), session))
		if !strings.Contains(page, "Order correspondence") || !strings.Contains(page, strings.TrimSpace(canonical)) {
			t.Fatal("order page omitted message")
		}
		if strings.Contains(p.page(orderPage(other), session), strings.TrimSpace(canonical)) {
			t.Fatal("message leaked to second order")
		}
		if strings.Contains(p.page("/messages", session), strings.TrimSpace(canonical)) {
			t.Fatal("message leaked into direct inbox")
		}
	}
	if p.count("SELECT count(*) FROM messages") != 0 {
		t.Fatal("order message copied to direct inbox")
	}
	if p.count("SELECT count(*) FROM notifications WHERE order_id=$1 AND body NOT LIKE '%Order-only%'", order) != 1 {
		t.Fatal("missing generic order-linked notification")
	}
	if p.count("SELECT count(*) FROM audit_events WHERE action LIKE '%Order-only%'") != 0 {
		t.Fatal("message plaintext audited")
	}
}

func TestOrderChannelAuthorizationAndReplacementHistory(t *testing.T) {
	p := newPayEnv(t)
	vendorKey, vendorPub := testPGPKey(t, "order_auth_recipient")
	p.setKey(p.vendor.ID, vendorPub)
	order, _ := p.newOrder(statePaid)
	_, outsider := p.user("order_channel_outsider", "buyer")
	_, moderator := p.user("order_channel_reviewer", "moderator")
	ciphertext := encryptToRecipients(t, "private order text", vendorKey)
	for _, session := range []string{outsider, moderator, p.adminSess} {
		p.check(p.do("POST", "/orders/message", session, url.Values{"order_id": {order}, "body": {ciphertext}}), 404)
	}
	p.check(p.do("POST", "/orders/message", p.buyerSess, url.Values{"order_id": {p.order}, "body": {ciphertext}}), 409)
	product := p.product(p.vendor.ID, "digital")
	digital := p.testEnv.order(p.buyer.ID, product, "XMR", stateCompleted)
	agExec(p.testEnv, `INSERT INTO deliveries(order_id,content) VALUES($1,'initial download')`, digital)
	for _, session := range []string{p.buyerSess, outsider, moderator, p.adminSess} {
		want := 404
		if session == p.buyerSess {
			want = 403
		}
		p.check(p.do("POST", "/orders/delivery-update", session, url.Values{"order_id": {digital}, "content": {"replacement download"}}), want)
	}
	f := url.Values{"order_id": {digital}, "content": {"replacement download"}, formNonceField: {randomToken()[:32]}}
	p.check(p.do("POST", "/orders/delivery-update", p.vendorSess, f), 303)
	p.check(p.do("POST", "/orders/delivery-update", p.vendorSess, f), 303)
	if p.count("SELECT count(*) FROM order_delivery_updates WHERE order_id=$1", digital) != 1 {
		t.Fatal("replacement replay duplicated")
	}
	page := p.page(orderPage(digital), p.buyerSess)
	if !strings.Contains(page, "initial download") || !strings.Contains(page, "replacement download") {
		t.Fatal("delivery history missing")
	}
	if strings.Contains(p.page("/messages", p.buyerSess), "replacement download") {
		t.Fatal("delivery copied to messages")
	}
	p.check(p.do("POST", "/orders/delivery-update", p.vendorSess, url.Values{"order_id": {order}, "content": {"physical replacement"}}), 409)
	agExec(p.testEnv, `UPDATE orders SET private_expired_at=now() WHERE id=$1`, digital)
	p.check(p.do("POST", "/orders/delivery-update", p.vendorSess, url.Values{"order_id": {digital}, "content": {"resurrect"}}), 409)
	agExec(p.testEnv, `UPDATE orders SET private_expired_at=now() WHERE id=$1`, order)
	p.check(p.do("POST", "/orders/message", p.buyerSess, url.Values{"order_id": {order}, "body": {ciphertext}}), 409)
}

func TestOrderChannelRetentionReplayErasesOnlyDuePrivateContent(t *testing.T) {
	p := newPayEnv(t)
	order, _ := p.newOrder(stateCompleted)
	now := time.Now().UTC()
	agExec(p.testEnv, `UPDATE orders SET financial_closed_at=$2 WHERE id=$1`, order, now.Add(-31*24*time.Hour))
	agExec(p.testEnv, `INSERT INTO order_messages(id,order_id,sender_id,recipient_id,body,recipient_match,created) VALUES('retained-order-message',$1,$2,$3,'old packets','yes',$4)`, order, p.buyer.ID, p.vendor.ID, now.Add(-time.Hour))
	agExec(p.testEnv, `INSERT INTO order_delivery_updates(id,order_id,author_id,content,created) VALUES('retained-order-replacement',$1,$2,'old replacement',$3)`, order, p.vendor.ID, now.Add(-time.Hour))
	agExec(p.testEnv, `INSERT INTO messages(id,sender_id,recipient_id,body) VALUES('separate-direct-mail',$1,$2,'direct stays')`, p.buyer.ID, p.vendor.ID)
	tx, err := p.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "order_private", ID: order, At: now}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"order_messages", "order_delivery_updates"} {
		if p.count("SELECT count(*) FROM "+table+" WHERE order_id=$1", order) != 0 {
			t.Fatal(table + " survived private retention")
		}
	}
	if p.count("SELECT count(*) FROM messages WHERE id='separate-direct-mail'") != 1 {
		t.Fatal("direct inbox purged with order")
	}
}

func TestOrderMessagesReuseStrictPacketChecksAndKeepConfirmationOnOrder(t *testing.T) {
	p := newPayEnv(t)
	key, pub := testPGPKey(t, "strict_order_vendor")
	other, _ := testPGPKey(t, "strict_other")
	p.setKey(p.vendor.ID, pub)
	order, _ := p.newOrder(statePaid)
	ciphertext := encryptToRecipients(t, "one decrypt only", key)
	for _, bad := range []string{
		"plain private shipping text that is not an armored message at all",
		"-----BEGIN PGP MESSAGE-----\nnot ciphertext\n-----END PGP MESSAGE-----",
		ciphertext + "\n" + ciphertext,
		encryptToRecipients(t, "wrong recipient", other),
		testClearsign(t, key, "signed only; not encrypted"),
	} {
		p.check(p.do("POST", "/orders/message", p.buyerSess, url.Values{"order_id": {order}, "body": {bad}}), 400)
	}
	if p.count("SELECT count(*) FROM order_messages") != 0 {
		t.Fatal("invalid message stored")
	}
	hidden := hideRecipientIDs(t, ciphertext, key)
	response := p.do("POST", "/orders/message", p.buyerSess, url.Values{"order_id": {order}, "body": {hidden}})
	p.check(response, 400)
	if !strings.Contains(response.Body.String(), `action="/orders/message"`) || !strings.Contains(response.Body.String(), `name="hidden_recipients"`) || strings.Contains(response.Body.String(), `action="/messages"`) {
		t.Fatal("hidden-recipient confirmation left order page")
	}
	p.check(p.do("POST", "/orders/message", p.buyerSess, url.Values{"order_id": {order}, "body": {hidden}, "hidden_recipients": {"confirmed"}}), 303)
	stored := p.str("SELECT body FROM order_messages WHERE order_id=$1", order)
	if armorPackets(t, stored) != armorPackets(t, hidden) || testDecrypt(t, key, stored) != "one decrypt only" {
		t.Fatal("hidden-recipient ciphertext re-encrypted")
	}
	if p.str("SELECT recipient_match FROM order_messages WHERE order_id=$1", order) != matchUnknown {
		t.Fatal("hidden recipients claimed verified")
	}
	agExec(p.testEnv, `UPDATE users SET role='buyer' WHERE id=$1`, p.vendor.ID)
	p.check(p.do("POST", "/orders/message", p.buyerSess, url.Values{"order_id": {order}, "body": {ciphertext}}), 303)
	p.setKey(p.vendor.ID, "")
	p.check(p.do("POST", "/orders/message", p.buyerSess, url.Values{"order_id": {order}, "body": {ciphertext}}), 409)
}

func TestOrderChannelPreservationAndWindowCannotBeExtended(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	p := newPayEnv(t, e)
	key, pub := testPGPKey(t, "window_order_vendor")
	p.setKey(p.vendor.ID, pub)
	product := p.product(p.vendor.ID, "digital")
	order := p.testEnv.order(p.buyer.ID, product, "XMR", stateCompleted)
	agExec(e, `INSERT INTO deliveries(order_id,content) VALUES($1,'initial')`, order)
	agExec(e, `UPDATE orders SET financial_closed_at=now()-interval '29 days' WHERE id=$1`, order)
	at := p.str("SELECT financial_closed_at::text FROM orders WHERE id=$1", order)
	p.check(p.do("POST", "/orders/delivery-update", p.vendorSess, url.Values{"order_id": {order}, "content": {"new link"}}), 303)
	p.check(p.do("POST", "/orders/message", p.buyerSess, url.Values{"order_id": {order}, "body": {encryptToRecipients(t, "order only", key)}}), 303)
	if p.str("SELECT financial_closed_at::text FROM orders WHERE id=$1", order) != at {
		t.Fatal("private update extended order clock")
	}
	agExec(e, `UPDATE orders SET financial_closed_at=now()-interval '31 days' WHERE id=$1`, order)
	now := time.Now().UTC()
	commitRetentionHold(t, e, retentionHold{Scope: "order", RecordID: order, Authority: "Case", Reason: "Order-only evidence", Owner: "Test owner", Readers: "Assigned handlers", ReleaseCondition: "Written release", BeganAt: now, ReviewAt: now.Add(24 * time.Hour)})
	ageRetentionEnrollment(t, e.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, now)
	if p.count("SELECT count(*) FROM order_messages WHERE order_id=$1", order) != 1 || p.count("SELECT count(*) FROM order_delivery_updates WHERE order_id=$1", order) != 1 {
		t.Fatal("order hold did not preserve private content")
	}
	p.check(p.do("POST", "/orders/delivery-update", p.vendorSess, url.Values{"order_id": {order}, "content": {"cannot extend held window"}}), 409)
	p.check(p.do("POST", "/orders/message", p.buyerSess, url.Values{"order_id": {order}, "body": {encryptToRecipients(t, "cannot extend held window", key)}}), 409)
}
