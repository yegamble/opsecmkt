package market

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func submissionCount(t *testing.T, e *testEnv) int {
	t.Helper()
	var n int
	if err := e.DB.QueryRow("SELECT count(*) FROM form_submissions").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func renderedFormNonce(t *testing.T, body, action string) string {
	t.Helper()
	actionAt := strings.Index(body, `method="post" action="`+action+`"`)
	if actionAt < 0 {
		t.Fatalf("POST form %s not rendered", action)
	}
	start := strings.LastIndex(body[:actionAt], "<form")
	if start < 0 {
		t.Fatalf("POST form %s has no start", action)
	}
	end := strings.Index(body[start:], "</form>")
	if end < 0 {
		t.Fatalf("form %s has no end", action)
	}
	part := body[start : start+end]
	re := regexp.MustCompile(`name="form_nonce" value="([0-9a-f]{32})"`)
	m := re.FindStringSubmatch(part)
	if m == nil {
		t.Fatalf("form %s has no nonce", action)
	}
	return m[1]
}

func TestFormNonceRenderedWithoutGETWrites(t *testing.T) {
	e := newTestApp(t)
	vendor, vendorSession := e.user("nonce_vendor", "vendor")
	buyer, buyerSession := e.user("nonce_buyer", "buyer")
	order := e.order(buyer, e.product(vendor, "digital"), "BTC", stateDraft)
	if got := submissionCount(t, e); got != 0 {
		t.Fatalf("new database has %d form submissions", got)
	}
	listing := e.do("GET", "/vendor-dashboard", vendorSession, nil)
	e.check(listing, 200)
	first := renderedFormNonce(t, listing.Body.String(), "/listings")
	secondPage := e.do("GET", "/vendor-dashboard", vendorSession, nil)
	e.check(secondPage, 200)
	if first == renderedFormNonce(t, secondPage.Body.String(), "/listings") {
		t.Fatal("two rendered listing forms shared a nonce")
	}
	messages := e.do("GET", "/messages", buyerSession, nil)
	e.check(messages, 200)
	if first == renderedFormNonce(t, messages.Body.String(), "/messages") {
		t.Fatal("listing and message form shared a nonce")
	}
	page := e.do("GET", orderPage(order), buyerSession, nil)
	e.check(page, 200)
	renderedFormNonce(t, page.Body.String(), "/orders/cancel")
	if got := submissionCount(t, e); got != 0 {
		t.Fatalf("GET wrote %d form submissions", got)
	}
}

func TestFormNonceListingSuccessReplayAndRollback(t *testing.T) {
	e := newTestApp(t)
	vendorID, session := e.user("nonce_listing", "vendor")
	_, otherSession := e.user("nonce_other_vendor", "vendor")
	f := inventoryForm("", nil)
	f.Set(formNonceField, randomToken()[:32])
	first := e.do("POST", "/listings", session, f)
	e.check(first, 303)
	repeat := e.do("POST", "/listings", session, f)
	e.check(repeat, 303)
	if repeat.Header().Get("Location") != first.Header().Get("Location") ||
		noticeSetCookie(repeat) == nil || noticeSetCookie(repeat).Value != "listing-created" {
		t.Fatalf("repeat redirect/notice differs: first=%q repeat=%q", first.Header().Get("Location"), repeat.Header().Get("Location"))
	}
	var products, audits int
	if err := e.DB.QueryRow("SELECT count(*) FROM products WHERE vendor_id=$1", vendorID).Scan(&products); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Created listing'", vendorID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if products != 1 || audits != 1 || submissionCount(t, e) != 1 {
		t.Fatalf("exact replay stored products=%d audits=%d receipts=%d", products, audits, submissionCount(t, e))
	}
	changed := url.Values{}
	for key, values := range f {
		changed[key] = values
	}
	changed.Set("title", "Changed after first use")
	e.check(e.do("POST", "/listings", session, changed), 409)
	e.check(e.do("POST", "/listings", otherSession, f), 409)
	e.check(e.do("POST", "/listings", session, url.Values{formNonceField: {""}}), 400)
	missing := inventoryForm("", nil)
	missing.Set("csrf", e.A.csrf(session))
	r := httptest.NewRequest(http.MethodPost, "/listings", strings.NewReader(missing.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: session})
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	e.check(w, 400) // raw POST omits the nonce; e.do supplies one for ordinary fixture forms
	if submissionCount(t, e) != 1 {
		t.Fatal("mismatched or missing nonce stored a receipt")
	}

	// A refused first submission rolls back its claim; the corrected form may reuse the nonce.
	invalid := inventoryForm("", map[string]string{"price_btc": "0"})
	invalid.Set(formNonceField, randomToken()[:32])
	e.check(e.do("POST", "/listings", session, invalid), 400)
	if submissionCount(t, e) != 1 {
		t.Fatal("refused form kept its nonce")
	}
	invalid.Set("price_btc", "0.002")
	e.check(e.do("POST", "/listings", session, invalid), 303)
	if submissionCount(t, e) != 2 {
		t.Fatal("corrected form was not stored")
	}
}

func TestFormNonceConcurrentListingStoresOnce(t *testing.T) {
	e := newTestApp(t)
	vendor, session := e.user("nonce_concurrent", "vendor")
	f := inventoryForm("", nil)
	f.Set(formNonceField, randomToken()[:32])
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- e.do("POST", "/listings", session, f)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var redirect string
	for w := range results {
		e.check(w, 303)
		if redirect == "" {
			redirect = w.Header().Get("Location")
		} else if w.Header().Get("Location") != redirect {
			t.Fatalf("concurrent redirects differed: %q, %q", redirect, w.Header().Get("Location"))
		}
	}
	var products, audits int
	e.DB.QueryRow("SELECT count(*) FROM products WHERE vendor_id=$1", vendor).Scan(&products)
	e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Created listing'", vendor).Scan(&audits)
	if products != 1 || audits != 1 || submissionCount(t, e) != 1 {
		t.Fatalf("concurrent submit stored products=%d audits=%d receipts=%d", products, audits, submissionCount(t, e))
	}
}

func TestFormNonceMessageAndOrderTransition(t *testing.T) {
	e := newTestApp(t)
	vendor, vendorSession := e.user("nonce_message_vendor", "vendor")
	buyer, buyerSession := e.user("nonce_message_buyer", "buyer")
	key, _ := testPGPKey(t, "nonce-recipient")
	body, err := encryptTo(key, "one encrypted message")
	if err != nil {
		t.Fatal(err)
	}
	message := url.Values{"recipient": {"nonce_message_buyer"}, "body": {body}, formNonceField: {randomToken()[:32]}}
	first := e.do("POST", "/messages", vendorSession, message)
	e.check(first, 303)
	repeat := e.do("POST", "/messages", vendorSession, message)
	e.check(repeat, 303)
	if first.Header().Get("Location") != repeat.Header().Get("Location") {
		t.Fatal("message repeat changed redirect")
	}
	var messages, notifications, audits int
	e.DB.QueryRow("SELECT count(*) FROM messages WHERE sender_id=$1", vendor).Scan(&messages)
	e.DB.QueryRow("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'New message%'", buyer).Scan(&notifications)
	e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Sent OpenPGP-encrypted message%'", vendor).Scan(&audits)
	if messages != 1 || notifications != 1 || audits != 1 {
		t.Fatalf("message repeat stored messages=%d notices=%d audits=%d", messages, notifications, audits)
	}
	order := e.order(buyer, e.product(vendor, "digital"), "BTC", stateDraft)
	cancel := url.Values{"order_id": {order}, "from": {stateDraft}, formNonceField: {randomToken()[:32]}}
	first = e.do("POST", "/orders/cancel", buyerSession, cancel)
	e.check(first, 303)
	repeat = e.do("POST", "/orders/cancel", buyerSession, cancel)
	e.check(repeat, 303)
	if first.Header().Get("Location") != repeat.Header().Get("Location") {
		t.Fatal("order transition repeat changed redirect")
	}
	var events int
	e.DB.QueryRow("SELECT count(*) FROM order_events WHERE order_id=$1 AND to_state='cancelled'", order).Scan(&events)
	if events != 1 {
		t.Fatalf("cancel repeat wrote %d events", events)
	}
	changed := url.Values{"order_id": {order}, "from": {stateDraft}, "note": {"altered"}, formNonceField: {cancel.Get(formNonceField)}}
	e.check(e.do("POST", "/orders/cancel", buyerSession, changed), 409)
	// A newly rendered form carries a different nonce, so the existing state-machine 409 still applies.
	e.check(e.do("POST", "/orders/cancel", buyerSession, url.Values{"order_id": {order}, "from": {stateDraft}}), 409)
}

func TestFormNonceOtherOrderTransitionsReplayOnce(t *testing.T) {
	// Shipping and buyer receipt confirmation are escrow-specific actions with
	// dedicated replay/lost-ack coverage. Digital escrow normally delivers and
	// completes in the funding watcher; when a listing is taken down, its vendor
	// can still deliver by hand through the existing order action. Exercise that
	// real escrow exception here rather than constructing a pooled order.
	x := newEscrowEnv(t)
	order := x.draft()
	addr := x.mustCheckout(order, regtestAddr("nonce-manual-delivery-refund"))
	if _, err := x.DB.Exec("UPDATE products SET archived=true,archived_at=now(),admin_archive_reason='test takedown' WHERE id=(SELECT product_id FROM orders WHERE id=$1)", order); err != nil {
		t.Fatal(err)
	}
	x.core.deposit(addr, txidOf("nonce-manual-delivery"), 0, 100000, 3)
	x.poll()
	if x.state(order) != statePaid {
		t.Fatalf("taken-down funded escrow was not left paid for manual delivery: %s", x.state(order))
	}
	page := x.page("/order?id="+order, x.vendorSess)
	nonce := renderedFormNonce(t, page, "/orders/deliver")
	if got := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/orders/deliver'"); got != 0 {
		t.Fatalf("GET wrote %d delivery receipts", got)
	}
	f := url.Values{"order_id": {order}, "content": {"One-time licence"}, formNonceField: {nonce}}
	first := x.do("POST", "/orders/deliver", x.vendorSess, f)
	x.check(first, 303)
	if x.state(order) != stateCompleted {
		t.Fatalf("manual escrow delivery did not complete through the system hook: %s", x.state(order))
	}
	repeat := x.do("POST", "/orders/deliver", x.vendorSess, f)
	x.check(repeat, 303)
	if first.Header().Get("Location") != repeat.Header().Get("Location") ||
		noticeSetCookie(repeat) == nil || noticeSetCookie(repeat).Value != "order-delivered" {
		t.Fatal("manual delivery replay changed its recorded response")
	}
	var delivered, completed, content, receipts int
	if err := x.DB.QueryRow("SELECT count(*) FILTER (WHERE to_state='delivered'), count(*) FILTER (WHERE to_state='completed') FROM order_events WHERE order_id=$1", order).Scan(&delivered, &completed); err != nil {
		t.Fatal(err)
	}
	if err := x.DB.QueryRow("SELECT count(*) FROM deliveries WHERE order_id=$1", order).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if err := x.DB.QueryRow("SELECT count(*) FROM form_submissions WHERE action_path='/orders/deliver'").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 || completed != 1 || content != 1 || receipts != 1 {
		t.Fatalf("manual delivery replay wrote delivered=%d completed=%d deliveries=%d receipts=%d", delivered, completed, content, receipts)
	}
}

func TestFormNonceLostCommitAcknowledgementCanReplayRecordedSuccess(t *testing.T) {
	e := newTestApp(t)
	vendor, session := e.user("nonce_lost_ack", "vendor")
	f := inventoryForm("", nil)
	f.Set(formNonceField, randomToken()[:32])
	lost := loseNextActionCommit(t, e)
	first := e.do("POST", "/listings", session, f)
	if lost.Load() || first.Code != 500 || first.Header().Get("Retry-After") != "" ||
		!strings.Contains(first.Body.String(), "Submission outcome unknown") ||
		strings.Contains(first.Body.String(), "Nothing was changed") {
		t.Fatalf("lost acknowledgement: code=%d retry=%q injected=%v", first.Code, first.Header().Get("Retry-After"), !lost.Load())
	}
	replay := e.do("POST", "/listings", session, f)
	e.check(replay, 303)
	var products, audits int
	e.DB.QueryRow("SELECT count(*) FROM products WHERE vendor_id=$1", vendor).Scan(&products)
	e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Created listing'", vendor).Scan(&audits)
	if products != 1 || audits != 1 || submissionCount(t, e) != 1 {
		t.Fatalf("lost-ack replay stored products=%d audits=%d receipts=%d", products, audits, submissionCount(t, e))
	}
}

func TestFormNonceCSRFStillPrecedesReplay(t *testing.T) {
	e := newTestApp(t)
	_, session := e.user("nonce_csrf", "vendor")
	f := inventoryForm("", nil)
	f.Set(formNonceField, randomToken()[:32])
	e.check(e.do("POST", "/listings", session, f), 303)
	f.Set("csrf", "forged")
	r := httptest.NewRequest(http.MethodPost, "/listings", strings.NewReader(f.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: session})
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	e.check(w, 403)
}

func TestFormNonceExpiredReceiptsSwept(t *testing.T) {
	e := newTestApp(t)
	_, session := e.user("nonce_expire", "vendor")
	f := inventoryForm("", nil)
	f.Set(formNonceField, randomToken()[:32])
	e.check(e.do("POST", "/listings", session, f), 303)
	if _, err := e.DB.Exec("UPDATE form_submissions SET expires=$2 WHERE nonce=$1", f.Get(formNonceField), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := e.A.sweepFormSubmissions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if submissionCount(t, e) != 0 {
		t.Fatal("expired receipt was not swept")
	}
}
