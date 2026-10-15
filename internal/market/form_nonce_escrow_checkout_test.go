package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestXMREscrowCheckoutReceiptReplaysAndRefusedFormCanRetry(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	page := x.page("/order?id="+order, x.buyerSess)
	nonce := renderedFormNonce(t, page, "/orders/escrow-xmr")
	if got := escrowFormReceiptCount(t, x.testEnv, "/orders/escrow-xmr"); got != 0 {
		t.Fatalf("XMR checkout form GET wrote %d receipts", got)
	}
	refund := testXMRAddr("receipt checkout "+order, true)
	form := url.Values{"order_id": {order}, "refund_address": {refund}, formNonceField: {nonce}}
	beforeRPC := x.fake.count()
	invalid := url.Values{"order_id": {order}, "refund_address": {"invalid"}, formNonceField: {nonce}}
	if w := x.do("POST", "/orders/escrow-xmr", x.buyerSess, invalid); w.Code != 400 {
		t.Fatalf("invalid refund response = %d %s", w.Code, w.Body.String())
	}
	if got := escrowFormReceiptCount(t, x.testEnv, "/orders/escrow-xmr"); got != 0 {
		t.Fatalf("refused XMR checkout left %d receipts", got)
	}
	first := x.do("POST", "/orders/escrow-xmr", x.buyerSess, form)
	if first.Code != 303 || first.Header().Get("Location") != "/order?id="+order || x.cookie(first, "notice") != "escrow-xmr-created" {
		t.Fatalf("checkout: %d %q %s", first.Code, first.Header().Get("Location"), first.Body.String())
	}
	replay := x.do("POST", "/orders/escrow-xmr", x.buyerSess, form)
	if replay.Code != 303 || replay.Header().Get("Location") != first.Header().Get("Location") || x.cookie(replay, "notice") != x.cookie(first, "notice") {
		t.Fatalf("exact replay: %d %q notice=%q", replay.Code, replay.Header().Get("Location"), x.cookie(replay, "notice"))
	}
	changed := url.Values{"order_id": {order}, "refund_address": {testXMRAddr("changed receipt checkout "+order, true)}, formNonceField: {nonce}}
	if w := x.do("POST", "/orders/escrow-xmr", x.buyerSess, changed); w.Code != 409 {
		t.Fatalf("changed refund replay = %d %s, want 409", w.Code, w.Body.String())
	}
	if got := x.fake.count(); got != beforeRPC {
		t.Fatalf("XMR checkout contacted wallet/daemon: %d RPCs before, %d after", beforeRPC, got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND coin='XMR'", order); got != 1 {
		t.Fatalf("XMR escrow count = %d, want 1", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE order_id=$1 AND state='claimed'", order); got != 1 {
		t.Fatalf("claimed XMR slots = %d, want 1", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id=$2 AND note='Monero escrow address requested'", order, x.buyer); got != 1 {
		t.Fatalf("XMR checkout events = %d, want 1", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Created Monero escrow%'", x.buyer); got != 1 {
		t.Fatalf("XMR checkout audit events = %d, want 1", got)
	}
	var state string
	if err := x.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&state); err != nil || state != stateAwaitingPayment {
		t.Fatalf("order state = %q, err=%v", state, err)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE nonce=$1", nonce); got != 1 {
		t.Fatalf("XMR checkout receipts = %d, want 1", got)
	}
	var receipt string
	if err := x.DB.QueryRow("SELECT concat_ws('|',nonce,user_id,session_hash,action_path,request_hmac,redirect,notice) FROM form_submissions WHERE nonce=$1", nonce).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receipt, refund) {
		t.Fatal("receipt retained the XMR refund address")
	}
}
