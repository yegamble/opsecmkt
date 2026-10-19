package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestFormNonceDraftDisputeReplyAndReviewFormsReplay(t *testing.T) {
	w := newDisputeOrderWorld(t)
	e := w.e
	// An escrow dispute's resolver is an independent administrator (A-558): it receives the one staff notice.
	resolverID, _ := e.user("nonce_resolver_"+randomToken()[:6], "admin")
	vendorID, vendor := w.vendorID, w.vendor
	buyerID, buyer := w.buyerID, w.buyer
	product := w.x.product

	checkout := e.do("GET", "/checkout?id="+product, buyer, nil)
	e.check(checkout, 200)
	draftForm := url.Values{"product_id": {product}, "currency": {"XMR"}, formNonceField: {renderedFormNonce(t, checkout.Body.String(), "/orders")}}
	if submissionCount(t, e) != 0 {
		t.Fatal("rendering checkout wrote a form receipt")
	}
	draftFirst := e.do("POST", "/orders", buyer, draftForm)
	e.check(draftFirst, 303)
	draftReplay := e.do("POST", "/orders", buyer, draftForm)
	e.check(draftReplay, 303)
	if draftFirst.Header().Get("Location") != draftReplay.Header().Get("Location") {
		t.Fatalf("draft replay redirect changed: %q != %q", draftFirst.Header().Get("Location"), draftReplay.Header().Get("Location"))
	}
	if n := countFor(t, e, "SELECT count(*) FROM orders WHERE buyer_id=$1 AND product_id=$2 AND state='draft'", buyerID, product); n != 1 {
		t.Fatalf("draft replay left %d drafts", n)
	}
	if n := countFor(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Saved unfunded order draft'", buyerID); n != 1 {
		t.Fatalf("draft replay wrote %d audits", n)
	}

	disputeOrder := w.fundedFor(buyerID, buyer, vendorID, vendor)
	beforeDisputesPage := submissionCount(t, e)
	disputes := e.do("GET", "/disputes", buyer, nil)
	e.check(disputes, 200)
	// Both entry points must render a nonce; exercise the inline order form that the wallet journey uses.
	renderedFormNonce(t, disputes.Body.String(), "/disputes")
	inlineDispute := e.do("GET", orderPage(disputeOrder), buyer, nil)
	e.check(inlineDispute, 200)
	disputeForm := url.Values{"order_id": {disputeOrder}, "reason": {"The delivered item does not match the listing."}, formNonceField: {renderedFormNonce(t, inlineDispute.Body.String(), "/disputes")}}
	if submissionCount(t, e) != beforeDisputesPage {
		t.Fatal("rendering the disputes page wrote a form receipt")
	}
	disputeFirst := e.do("POST", "/disputes", buyer, disputeForm)
	e.check(disputeFirst, 303)
	disputeReplay := e.do("POST", "/disputes", buyer, disputeForm)
	e.check(disputeReplay, 303)
	if disputeFirst.Header().Get("Location") != disputeReplay.Header().Get("Location") ||
		noticeSetCookie(disputeReplay) == nil || noticeSetCookie(disputeReplay).Value != "dispute-opened" {
		t.Fatal("dispute replay did not return the original redirect and notice")
	}
	if n := countFor(t, e, "SELECT count(*) FROM disputes WHERE order_id=$1", disputeOrder); n != 1 {
		t.Fatalf("dispute replay stored %d dispute records", n)
	}
	if n := countFor(t, e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Dispute opened on order %'", resolverID); n != 1 {
		t.Fatalf("dispute replay sent %d staff notices", n)
	}
	if n := countFor(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Opened dispute on order %'", buyerID); n != 1 {
		t.Fatalf("dispute replay wrote %d audits", n)
	}

	beforeReplyPage := submissionCount(t, e)
	orderPageView := e.do("GET", orderPage(disputeOrder), vendor, nil)
	e.check(orderPageView, 200)
	reply := "The package left our warehouse sealed and complete."
	replyForm := url.Values{"order_id": {disputeOrder}, "reply": {reply}, formNonceField: {renderedFormNonce(t, orderPageView.Body.String(), "/disputes/reply")}}
	if submissionCount(t, e) != beforeReplyPage {
		t.Fatal("rendering the reply form wrote a form receipt")
	}
	replyFirst := e.do("POST", "/disputes/reply", vendor, replyForm)
	e.check(replyFirst, 303)
	replyReplay := e.do("POST", "/disputes/reply", vendor, replyForm)
	e.check(replyReplay, 303)
	if replyFirst.Header().Get("Location") != replyReplay.Header().Get("Location") ||
		noticeSetCookie(replyReplay) == nil || noticeSetCookie(replyReplay).Value != "dispute-reply" {
		t.Fatal("reply replay did not return the original redirect and notice")
	}
	if n := countFor(t, e, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'Dispute reply: %'", disputeOrder); n != 1 {
		t.Fatalf("reply replay stored %d reply events", n)
	}
	if n := countFor(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Replied to the dispute on order %'", vendorID); n != 1 {
		t.Fatalf("reply replay wrote %d audits", n)
	}

	completed := w.x.draft(w.x.digital(vendorID))
	address := w.x.mustCheckout(completed)
	w.x.fake.payAddress(address, hex64("nonce-completed-"+completed), xmrListingPrice, 10)
	w.x.mustPoll()
	beforeReviewPage := submissionCount(t, e)
	completedPage := e.do("GET", orderPage(completed), buyer, nil)
	e.check(completedPage, 200)
	reviewForm := url.Values{"order_id": {completed}, "rating": {"5"}, "body": {"Exactly as described."}, formNonceField: {renderedFormNonce(t, completedPage.Body.String(), "/reviews")}}
	if submissionCount(t, e) != beforeReviewPage {
		t.Fatal("rendering the review form wrote a form receipt")
	}
	reviewFirst := e.do("POST", "/reviews", buyer, reviewForm)
	e.check(reviewFirst, 303)
	reviewReplay := e.do("POST", "/reviews", buyer, reviewForm)
	e.check(reviewReplay, 303)
	if reviewFirst.Header().Get("Location") != reviewReplay.Header().Get("Location") ||
		noticeSetCookie(reviewReplay) == nil || noticeSetCookie(reviewReplay).Value != "review-saved" {
		t.Fatal("review replay did not return the original redirect and notice")
	}
	if n := countFor(t, e, "SELECT count(*) FROM reviews WHERE order_id=$1", completed); n != 1 {
		t.Fatalf("review replay stored %d reviews", n)
	}
	if n := countFor(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Reviewed order %'", buyerID); n != 1 {
		t.Fatalf("review replay wrote %d audits", n)
	}
}

func TestFormNonceConcurrentDisputeAndLostAcknowledgement(t *testing.T) {
	w := newDisputeOrderWorld(t)
	e := w.e
	vendorID, buyerID, buyer, vendor := w.vendorID, w.buyerID, w.buyer, w.vendor
	resolverID, _ := e.user("nonce_resolver_"+randomToken()[:6], "admin") // the escrow dispute's resolver (A-558)

	order := w.fundedFor(buyerID, buyer, vendorID, vendor)
	f := url.Values{"order_id": {order}, "reason": {"The shipped goods arrived damaged and incomplete."}, formNonceField: {randomToken()[:32]}}
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- e.do("POST", "/disputes", buyer, f)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for r := range results {
		e.check(r, 303)
		if noticeSetCookie(r) == nil || noticeSetCookie(r).Value != "dispute-opened" {
			t.Fatal("concurrent duplicate did not replay dispute success")
		}
	}
	if n := countFor(t, e, "SELECT count(*) FROM disputes WHERE order_id=$1", order); n != 1 {
		t.Fatalf("concurrent dispute stored %d records", n)
	}
	if n := countFor(t, e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Dispute opened on order %'", resolverID); n != 1 {
		t.Fatalf("concurrent dispute sent %d staff notices", n)
	}
	if n := countFor(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Opened dispute on order %'", buyerID); n != 1 {
		t.Fatalf("concurrent dispute wrote %d audits", n)
	}

	lostOrder := w.fundedFor(buyerID, buyer, vendorID, vendor)
	lostForm := url.Values{"order_id": {lostOrder}, "reason": {"The replacement order is also missing its required parts."}, formNonceField: {randomToken()[:32]}}
	lost := loseNextActionCommit(t, e)
	first := e.do("POST", "/disputes", buyer, lostForm)
	if lost.Load() || first.Code != 500 || !strings.Contains(first.Body.String(), "Submission outcome unknown") {
		t.Fatalf("lost acknowledgement response: code=%d injected=%v body=%q", first.Code, !lost.Load(), first.Body.String())
	}
	replay := e.do("POST", "/disputes", buyer, lostForm)
	e.check(replay, 303)
	if noticeSetCookie(replay) == nil || noticeSetCookie(replay).Value != "dispute-opened" {
		t.Fatal("lost-ack retry did not return saved dispute success")
	}
	if n := countFor(t, e, "SELECT count(*) FROM disputes WHERE order_id=$1", lostOrder); n != 1 {
		t.Fatalf("lost-ack replay stored %d disputes", n)
	}
	if n := countFor(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Opened dispute on order %'", buyerID); n != 2 {
		t.Fatalf("expected one audit per dispute, got %d", n)
	}
}

func countFor(t *testing.T, e *testEnv, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
