package market

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// The receipt is for one committed market payment request. A wallet may generate an address before a
// failed transaction rolls back; the receipt makes no claim about that external RPC side effect.
func TestFormNoncePaymentRequestReplayKeepsOneMarketEffect(t *testing.T) {
	w := newOrderWorld(t)
	w.e.usableKey(w.vendorID)
	wallet := &stubProvider{currency: "BTC"}
	w.e.A.payments["BTC"] = wallet
	product := w.e.product(w.vendorID, "physical")
	order := w.e.order(w.buyerID, product, "BTC", stateDraft)
	if got := submissionCount(t, w.e); got != 0 {
		t.Fatalf("before GET: %d receipts", got)
	}
	page := w.page(orderPage(order), w.buyer, 200)
	nonce := renderedFormNonce(t, page, "/orders/pay")
	if got := submissionCount(t, w.e); got != 0 {
		t.Fatalf("GET wrote %d receipts", got)
	}
	f := url.Values{"order_id": {order}, formNonceField: {nonce}}
	first := w.post("/orders/pay", w.buyer, f)
	w.e.check(first, 303)
	replay := w.post("/orders/pay", w.buyer, f)
	w.e.check(replay, 303)
	if replay.Header().Get("Location") != first.Header().Get("Location") ||
		noticeSetCookie(replay) == nil || noticeSetCookie(replay).Value != "payment-address" {
		t.Fatalf("replay result differs: first=%q replay=%q", first.Header().Get("Location"), replay.Header().Get("Location"))
	}
	if w.state(order) != stateAwaitingPayment || w.stock(product) != 4 ||
		len(wallet.created) != 1 || wallet.created[0] != order ||
		w.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", order) != 1 ||
		w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND to_state='awaiting_payment'", order) != 1 ||
		w.count("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Requested BTC%payment address for order%'", w.buyerID) != 1 ||
		submissionCount(t, w.e) != 1 {
		t.Fatal("replayed payment request changed stock, address, event, audit or receipt")
	}
	changed := url.Values{"order_id": {"another-order"}, formNonceField: {nonce}}
	w.e.check(w.post("/orders/pay", w.buyer, changed), 409)
	w.e.check(w.post("/orders/pay", w.other, f), 409)
	if len(wallet.created) != 1 || w.stock(product) != 4 {
		t.Fatal("mismatched replay called wallet or changed stock")
	}
}

func TestFormNoncePaymentRequestRollbackAndLostCommitAcknowledgement(t *testing.T) {
	w := newOrderWorld(t)
	w.e.usableKey(w.vendorID)
	wallet := &stubProvider{currency: "BTC", broken: true}
	w.e.A.payments["BTC"] = wallet
	product := w.e.product(w.vendorID, "physical")
	order := w.e.order(w.buyerID, product, "BTC", stateDraft)
	f := url.Values{"order_id": {order}, formNonceField: {renderedFormNonce(t, w.page(orderPage(order), w.buyer, 200), "/orders/pay")}}
	w.e.check(w.post("/orders/pay", w.buyer, f), 503)
	if w.state(order) != stateDraft || w.stock(product) != 5 ||
		w.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", order) != 0 || submissionCount(t, w.e) != 0 {
		t.Fatal("failed payment request kept a market change or nonce claim")
	}
	wallet.broken = false
	lost := loseNextActionCommit(t, w.e)
	first := w.post("/orders/pay", w.buyer, f)
	if lost.Load() || first.Code != 500 || first.Header().Get("Retry-After") != "" ||
		!strings.Contains(first.Body.String(), "Submission outcome unknown") ||
		!strings.Contains(first.Body.String(), "before starting a new form") ||
		!strings.Contains(first.Body.String(), "resubmit it unchanged") ||
		strings.Contains(first.Body.String(), "Nothing was changed") {
		t.Fatalf("lost commit acknowledgement: status=%d retry=%q injected=%v body=%q", first.Code, first.Header().Get("Retry-After"), !lost.Load(), first.Body.String())
	}
	replay := w.post("/orders/pay", w.buyer, f)
	w.e.check(replay, 303)
	if w.state(order) != stateAwaitingPayment || w.stock(product) != 4 || len(wallet.created) != 1 ||
		w.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", order) != 1 ||
		w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND to_state='awaiting_payment'", order) != 1 ||
		w.count("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Requested BTC%payment address for order%'", w.buyerID) != 1 ||
		submissionCount(t, w.e) != 1 {
		t.Fatal("lost-ack retry duplicated or lost the committed market payment request")
	}
}

func TestFormNoncePaymentRequestInFlightRefusesFastThenReplays(t *testing.T) {
	w := newOrderWorld(t)
	w.e.usableKey(w.vendorID)
	product := w.e.product(w.vendorID, "physical")
	order := w.e.order(w.buyerID, product, "BTC", stateDraft)
	wallet := &slowWallet{hold: 2 * time.Second, slow: map[string]bool{order: true}, entered: make(chan string, 4)}
	w.e.A.payments["BTC"] = wallet
	f := url.Values{"order_id": {order}, formNonceField: {renderedFormNonce(t, w.page(orderPage(order), w.buyer, 200), "/orders/pay")}}
	first := make(chan int, 1)
	go func() { first <- w.post("/orders/pay", w.buyer, f).Code }()
	select {
	case <-wallet.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not reach the wallet")
	}
	start := time.Now()
	pending := w.post("/orders/pay", w.buyer, f)
	if pending.Code != 429 || pending.Header().Get("Retry-After") != busyRetryAfter ||
		time.Since(start) > time.Second || !strings.Contains(pending.Body.String(), "resubmit it unchanged") ||
		strings.Contains(pending.Body.String(), "reload the page") {
		t.Fatalf("in-flight repeat: status=%d retry=%q delay=%s body=%q", pending.Code, pending.Header().Get("Retry-After"), time.Since(start), pending.Body.String())
	}
	other := url.Values{"order_id": {order}, formNonceField: {randomToken()[:32]}}
	if r := w.post("/orders/pay", w.buyer, other); r.Code != 429 || r.Header().Get("Retry-After") != busyRetryAfter {
		t.Fatalf("different nonce bypassed one-at-a-time cap: %d %q", r.Code, r.Header().Get("Retry-After"))
	}
	if got := <-first; got != 303 {
		t.Fatalf("first payment request = %d", got)
	}
	w.e.check(w.post("/orders/pay", w.buyer, f), 303)
	if len(wallet.entered) != 0 || w.stock(product) != 4 ||
		w.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", order) != 1 || submissionCount(t, w.e) != 1 {
		t.Fatal("completed replay called wallet or changed market state")
	}
}
