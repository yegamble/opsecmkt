package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestEscrowCheckoutReceiptReplaysWithoutSecondNodeOrSignerCall(t *testing.T) {
	x, signer := newPerOrderEnv(t)
	page := x.page("/order?id="+x.order, x.buyerSess)
	nonce := renderedFormNonce(t, page, "/orders/escrow")
	if got := escrowFormReceiptCount(t, x.testEnv, "/orders/escrow"); got != 0 {
		t.Fatalf("checkout form GET wrote %d receipts", got)
	}
	refund := regtestAddr("nonce-receipt-refund")
	form := url.Values{"order_id": {x.order}, "refund_address": {refund}, formNonceField: {nonce}}
	beforeWallet, beforeSigner := len(x.core.methods()), len(signer.reqs())
	if w := x.do("POST", "/orders/escrow", x.buyerSess, url.Values{"order_id": {x.order}, "refund_address": {"invalid"}, formNonceField: {nonce}}); w.Code != 400 {
		t.Fatalf("invalid refund response = %d %s", w.Code, w.Body.String())
	}
	if got := escrowFormReceiptCount(t, x.testEnv, "/orders/escrow"); got != 0 {
		t.Fatalf("refused checkout left %d receipts", got)
	}
	first := x.do("POST", "/orders/escrow", x.buyerSess, form)
	if first.Code != 303 || first.Header().Get("Location") != "/order?id="+x.order || x.cookie(first, "notice") != "escrow-created" {
		t.Fatalf("checkout: %d %q %s", first.Code, first.Header().Get("Location"), first.Body.String())
	}
	afterWallet, afterSigner := len(x.core.methods()), len(signer.reqs())
	if afterWallet <= beforeWallet || afterSigner <= beforeSigner {
		t.Fatalf("checkout did not exercise expected node/signer calls: wallet %d→%d, signer %d→%d", beforeWallet, afterWallet, beforeSigner, afterSigner)
	}
	replay := x.do("POST", "/orders/escrow", x.buyerSess, form)
	if replay.Code != 303 || replay.Header().Get("Location") != first.Header().Get("Location") || x.cookie(replay, "notice") != x.cookie(first, "notice") {
		t.Fatalf("exact replay: %d %q notice=%q", replay.Code, replay.Header().Get("Location"), x.cookie(replay, "notice"))
	}
	if got := len(x.core.methods()); got != afterWallet {
		t.Fatalf("wallet RPC count after exact replay = %d, after first checkout %d", got, afterWallet)
	}
	if got := len(signer.reqs()); got != afterSigner {
		t.Fatalf("signer RPC count after exact replay = %d, after first checkout %d", got, afterSigner)
	}
	changed := url.Values{"order_id": {x.order}, "refund_address": {regtestAddr("nonce-receipt-refund-changed")}, formNonceField: {nonce}}
	if w := x.do("POST", "/orders/escrow", x.buyerSess, changed); w.Code != 409 {
		t.Fatalf("changed refund replay = %d %s, want 409", w.Code, w.Body.String())
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", x.order); got != 1 {
		t.Fatalf("escrow count = %d, want 1", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM escrow_pubkeys p JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=$1", x.order); got != 3 {
		t.Fatalf("escrow party keys = %d, want 3", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id=$2 AND note='Escrow address requested'", x.order, x.buyer); got != 1 {
		t.Fatalf("checkout order events = %d, want 1", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", x.buyer, "Created Bitcoin escrow (regtest) for order "+shortID(x.order)); got != 1 {
		t.Fatalf("checkout audit events = %d, want 1", got)
	}
	if x.state(x.order) != stateAwaitingPayment || agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", x.product) != 4 {
		t.Fatal("checkout replay changed the order state or reserved stock twice")
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE nonce=$1", nonce); got != 1 {
		t.Fatalf("checkout receipts = %d, want 1", got)
	}
	var receipt string
	if err := x.DB.QueryRow("SELECT concat_ws('|',nonce,user_id,session_hash,action_path,request_hmac,redirect,notice) FROM form_submissions WHERE nonce=$1", nonce).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receipt, refund) {
		t.Fatal("receipt retained the refund address")
	}
}

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
