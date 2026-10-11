package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func recoverNonceForm(order, nonce string) url.Values {
	return url.Values{"order_id": {order}, "password": {testPassword}, formNonceField: {nonce}}
}

func recoverNonceFixture(t *testing.T) *escrowEnv {
	t.Helper()
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, regtestAddr("nonce-recover-refund"))
	x.check(x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {x.order}, "from": {stateAwaitingPayment}}), 303)
	return x
}

func recoverRequestedAt(t *testing.T, x *escrowEnv) time.Time {
	t.Helper()
	var at time.Time
	if err := x.DB.QueryRow("SELECT recover_requested FROM escrows WHERE order_id=$1", x.order).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestEscrowRecoverConfirmedNonceReplayAndClearedRequest(t *testing.T) {
	x := recoverNonceFixture(t)
	nonce := renderedFormNonce(t, x.page("/order?id="+x.order, x.buyerSess), "/escrow/recover")
	if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/escrow/recover'"); n != 0 {
		t.Fatalf("GET wrote %d recovery receipts", n)
	}
	f := recoverNonceForm(x.order, nonce)
	before := [6]int{}
	before[0], before[1], before[2], before[3], before[4], before[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/recover", x.order, x.buyer)
	first := x.do("POST", "/escrow/recover", x.buyerSess, f)
	x.check(first, 303)
	after := [6]int{}
	after[0], after[1], after[2], after[3], after[4], after[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/recover", x.order, x.buyer)
	if after[0] != 1 || after[1] != before[1]+1 || after[2] != before[2]+1 || after[3] != before[3] || after[4] != before[4] {
		t.Fatalf("first recovery effects before=%v after=%v", before, after)
	}
	requestedAt := recoverRequestedAt(t, x)
	rpcCalls := len(x.core.methods())
	x.A.escrow.Store(nil) // an exact receipt needs no provider read or watcher RPC
	replay := x.do("POST", "/escrow/recover", x.buyerSess, f)
	x.check(replay, 303)
	if first.Header().Get("Location") == "" || replay.Header().Get("Location") != first.Header().Get("Location") || len(x.core.methods()) != rpcCalls {
		t.Fatal("recovery replay changed redirect or contacted the Bitcoin node")
	}
	changed := recoverNonceForm(x.order, nonce)
	changed.Set("extra", "different valid form body")
	x.check(x.do("POST", "/escrow/recover", x.buyerSess, changed), 409)
	x.check(x.do("POST", "/escrow/recover", agSession(x.testEnv, x.buyer), f), 409)
	x.check(x.do("POST", "/escrow/recover", x.vendorSess, f), 403)
	_, outsider := x.user("recover_nonce_outsider_"+randomToken()[:6], "buyer")
	x.check(x.do("POST", "/escrow/recover", outsider, f), 404)
	f.Set("password", "wrong password")
	x.check(x.do("POST", "/escrow/recover", x.buyerSess, f), 401)
	got := [6]int{}
	got[0], got[1], got[2], got[3], got[4], got[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/recover", x.order, x.buyer)
	if got != after || !recoverRequestedAt(t, x).Equal(requestedAt) {
		t.Fatalf("replay/refusals changed recovery effects: %v vs %v", got, after)
	}
	var saved string
	if err := x.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", nonce).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testPassword, x.buyerSess} {
		if strings.Contains(saved, secret) {
			t.Fatal("recovery receipt retained raw password or session")
		}
	}
}

func TestEscrowRecoverConfirmedNonceCurrentPartyCoinAndTOTP(t *testing.T) {
	x := recoverNonceFixture(t)
	key, _ := agEnableTOTP(x.testEnv, x.buyer)
	f := recoverNonceForm(x.order, randomToken()[:32])
	f.Set("code", agTOTPNow(key))
	original := f.Get(formNonceField)
	x.check(x.do("POST", "/escrow/recover", x.buyerSess, f), 303)
	step := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", x.buyer)
	x.check(x.do("POST", "/escrow/recover", x.buyerSess, f), 303)
	if got := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", x.buyer); got != step {
		t.Fatalf("replay spent TOTP again: %d to %d", step, got)
	}
	f.Set(formNonceField, randomToken()[:32])
	x.check(x.do("POST", "/escrow/recover", x.buyerSess, f), 401)
	newBuyer, _ := x.user("new_recover_buyer_"+randomToken()[:6], "buyer")
	agExec(x.testEnv, "UPDATE orders SET buyer_id=$1 WHERE id=$2", newBuyer, x.order)
	f.Set(formNonceField, original)
	x.check(x.do("POST", "/escrow/recover", x.buyerSess, f), 404)
	if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/escrow/recover'"); n != 1 {
		t.Fatalf("changed party wrote %d recovery receipts", n)
	}

	// A current buyer of an XMR escrow never becomes eligible through the BTC
	// recovery hook, even if its order is already closed.
	m := newXMREscrowEnv(t)
	m.mustPoll()
	order, _ := m.fundedXMR(m.product)
	tx, err := m.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	c := &actionCtx{Tx: tx, R: httptest.NewRequest("POST", "/escrow/recover", nil),
		User: &User{ID: m.partyOf(order, roleBuyer)}, Form: url.Values{"order_id": {order}}}
	if err := btcRecoverReceiptAuthorize(c); err != errNoOrder {
		t.Fatalf("XMR escrow accepted by BTC recovery hook: %v", err)
	}
}

func TestEscrowRecoverConfirmedNonceRollbackClearedAndLostAck(t *testing.T) {
	t.Run("state refusal can retry same nonce", func(t *testing.T) {
		x := newEscrowEnv(t)
		x.mustCheckout(x.order, regtestAddr("recover-state-refund"))
		f := recoverNonceForm(x.order, randomToken()[:32])
		x.check(x.do("POST", "/escrow/recover", x.buyerSess, f), 409)
		if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/escrow/recover'"); n != 0 {
			t.Fatalf("open-order refusal kept %d receipts", n)
		}
		x.check(x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {x.order}, "from": {stateAwaitingPayment}}), 303)
		x.check(x.do("POST", "/escrow/recover", x.buyerSess, f), 303)
	})
	t.Run("watcher cleared request needs a fresh form", func(t *testing.T) {
		x := recoverNonceFixture(t)
		f := recoverNonceForm(x.order, randomToken()[:32])
		x.check(x.do("POST", "/escrow/recover", x.buyerSess, f), 303)
		x.poll() // no later deposit: the watcher records no coins and clears recover_requested
		if n := agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND recover_requested IS NOT NULL", x.order); n != 0 {
			t.Fatal("watcher did not clear recovery request")
		}
		events := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", x.order)
		x.check(x.do("POST", "/escrow/recover", x.buyerSess, f), 303)
		if n := agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND recover_requested IS NOT NULL", x.order); n != 0 ||
			agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", x.order) != events {
			t.Fatal("old receipt re-requested recovery after watcher cleared it")
		}
		f.Set(formNonceField, randomToken()[:32])
		x.check(x.do("POST", "/escrow/recover", x.buyerSess, f), 303)
		if n := agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND recover_requested IS NOT NULL", x.order); n != 1 ||
			agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", x.order) != events+1 {
			t.Fatal("new form could not request another recovery")
		}
	})
	t.Run("lost commit acknowledgement", func(t *testing.T) {
		x := recoverNonceFixture(t)
		f := recoverNonceForm(x.order, randomToken()[:32])
		before := [6]int{}
		before[0], before[1], before[2], before[3], before[4], before[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/recover", x.order, x.buyer)
		lost := loseNextActionCommit(t, x.testEnv)
		w := x.do("POST", "/escrow/recover", x.buyerSess, f)
		if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
			!strings.Contains(w.Body.String(), "Escrow action outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
			t.Fatalf("lost recovery ack status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
		committed := [6]int{}
		committed[0], committed[1], committed[2], committed[3], committed[4], committed[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/recover", x.order, x.buyer)
		if committed[0] != 1 || committed[1] != before[1]+1 || committed[2] != before[2]+1 || committed[3] != before[3] {
			t.Fatalf("lost ACK did not persist one recovery: before=%v committed=%v", before, committed)
		}
		requestedAt := recoverRequestedAt(t, x)
		x.check(x.do("POST", "/escrow/recover", x.buyerSess, f), 303)
		after := [6]int{}
		after[0], after[1], after[2], after[3], after[4], after[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/recover", x.order, x.buyer)
		if after != committed || !recoverRequestedAt(t, x).Equal(requestedAt) {
			t.Fatalf("lost ACK replay changed recovery: committed=%v after=%v", committed, after)
		}
	})
}
