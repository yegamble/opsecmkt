package market

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func btcShipNonceForm(order, address, nonce string) url.Values {
	f := escrowReceiptForm(order, address, nonce)
	f.Set("note", "Sent by tracked post.")
	return f
}

func TestEscrowShipConfirmedNonceReplayOnceAndProviderOutage(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.paidPhysical(regtestAddr("nonce-ship-refund"), 100000)
	nonce := renderedFormNonce(t, x.page("/order?id="+order, x.vendorSess), "/escrow/ship")
	if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/escrow/ship'"); n != 0 {
		t.Fatalf("GET wrote %d ship receipts", n)
	}
	f := btcShipNonceForm(order, regtestAddr("nonce-ship-claim"), nonce)
	before := [6]int{}
	before[0], before[1], before[2], before[3], before[4], before[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/ship", order, x.vendor)
	release, ok := x.A.enterOneAtATime("/escrow/ship", x.vendor)
	if !ok {
		t.Fatal("could not hold the existing one-at-a-time slot")
	}
	busy := x.do("POST", "/escrow/ship", x.vendorSess, f)
	if busy.Code != 429 || busy.Header().Get("Retry-After") != busyRetryAfter || !strings.Contains(busy.Body.String(), "resubmit it unchanged") {
		t.Fatalf("in-flight ship status=%d retry=%q body=%q", busy.Code, busy.Header().Get("Retry-After"), busy.Body.String())
	}
	release()
	first := x.do("POST", "/escrow/ship", x.vendorSess, f)
	x.check(first, 303)
	after := [6]int{}
	after[0], after[1], after[2], after[3], after[4], after[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/ship", order, x.vendor)
	if after[0] != 1 || after[1] != before[1]+1 || after[2] <= before[2] || after[3] <= before[3] || after[4] != before[4]+1 ||
		x.state(order) != stateShipped || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND release_at IS NOT NULL", order) != 1 {
		t.Fatalf("first ship effects before=%v after=%v state=%s", before, after, x.state(order))
	}
	var releaseAt time.Time
	if err := x.DB.QueryRow("SELECT release_at FROM escrows WHERE order_id=$1", order).Scan(&releaseAt); err != nil {
		t.Fatal(err)
	}
	rpcCalls := len(x.core.methods())
	x.A.escrow.Store(nil) // an exact saved result must not depend on the later node state
	replay := x.do("POST", "/escrow/ship", x.vendorSess, f)
	x.check(replay, 303)
	if first.Header().Get("Location") == "" || first.Header().Get("Location") != replay.Header().Get("Location") || len(x.core.methods()) != rpcCalls {
		t.Fatal("ship replay changed redirect or contacted the Bitcoin node")
	}
	changed := btcShipNonceForm(order, regtestAddr("nonce-ship-other"), nonce)
	x.check(x.do("POST", "/escrow/ship", x.vendorSess, changed), 409)
	changed = btcShipNonceForm(order, f.Get("address"), nonce)
	changed.Set("note", "A different shipping note")
	x.check(x.do("POST", "/escrow/ship", x.vendorSess, changed), 409)
	x.check(x.do("POST", "/escrow/ship", agSession(x.testEnv, x.vendor), f), 409)
	x.check(x.do("POST", "/escrow/ship", x.buyerSess, f), 403)
	_, outsider := x.user("ship_nonce_outsider_"+randomToken()[:6], "vendor")
	x.check(x.do("POST", "/escrow/ship", outsider, f), 404)
	f.Set("password", "wrong password")
	x.check(x.do("POST", "/escrow/ship", x.vendorSess, f), 401)
	got := [6]int{}
	got[0], got[1], got[2], got[3], got[4], got[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/ship", order, x.vendor)
	if got != after {
		t.Fatalf("replay/refusals changed ship effects: %v vs %v", got, after)
	}
	var unchangedAt time.Time
	if err := x.DB.QueryRow("SELECT release_at FROM escrows WHERE order_id=$1", order).Scan(&unchangedAt); err != nil || !unchangedAt.Equal(releaseAt) {
		t.Fatalf("replay/refusals moved release deadline from %s to %s: %v", releaseAt, unchangedAt, err)
	}
	var saved string
	if err := x.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", nonce).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testPassword, f.Get("address"), f.Get("note"), x.vendorSess} {
		if strings.Contains(saved, secret) {
			t.Fatal("ship receipt retained raw form or session data")
		}
	}
}

func TestEscrowShipConfirmedNonceCurrentPartyAndTOTP(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.paidPhysical(regtestAddr("nonce-ship-party-refund"), 100000)
	key, _ := agEnableTOTP(x.testEnv, x.vendor)
	f := btcShipNonceForm(order, regtestAddr("nonce-ship-party-claim"), randomToken()[:32])
	f.Set("code", agTOTPNow(key))
	original := f.Get(formNonceField)
	x.check(x.do("POST", "/escrow/ship", x.vendorSess, f), 303)
	step := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", x.vendor)
	x.check(x.do("POST", "/escrow/ship", x.vendorSess, f), 303)
	if got := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", x.vendor); got != step {
		t.Fatalf("ship replay spent TOTP again: %d to %d", step, got)
	}
	f.Set(formNonceField, randomToken()[:32])
	x.check(x.do("POST", "/escrow/ship", x.vendorSess, f), 401)
	newVendor, _ := x.user("new_ship_vendor_"+randomToken()[:6], "vendor")
	agExec(x.testEnv, "UPDATE products SET vendor_id=$1 WHERE id=(SELECT product_id FROM orders WHERE id=$2)", newVendor, order)
	f.Set(formNonceField, original)
	x.check(x.do("POST", "/escrow/ship", x.vendorSess, f), 404)
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", order); n != 1 {
		t.Fatalf("changed party created %d settlements", n)
	}
}

func TestEscrowShipConfirmedNonceRollbackAndLostAcknowledgement(t *testing.T) {
	t.Run("state refusal rolls back claim", func(t *testing.T) {
		x := newEscrowEnv(t)
		order := x.paidPhysical(regtestAddr("nonce-ship-state-refund"), 100000)
		f := btcShipNonceForm(order, regtestAddr("nonce-ship-state-claim"), randomToken()[:32])
		agExec(x.testEnv, "UPDATE orders SET state='shipped' WHERE id=$1", order)
		x.check(x.do("POST", "/escrow/ship", x.vendorSess, f), 409)
		if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/escrow/ship'"); n != 0 {
			t.Fatalf("state refusal kept %d receipts", n)
		}
		agExec(x.testEnv, "UPDATE orders SET state='paid' WHERE id=$1", order)
		x.check(x.do("POST", "/escrow/ship", x.vendorSess, f), 303)
	})
	t.Run("invalid address rolls back claim", func(t *testing.T) {
		x := newEscrowEnv(t)
		order := x.paidPhysical(regtestAddr("nonce-ship-retry-refund"), 100000)
		f := btcShipNonceForm(order, "bad address", randomToken()[:32])
		x.check(x.do("POST", "/escrow/ship", x.vendorSess, f), 400)
		if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/escrow/ship'"); n != 0 {
			t.Fatalf("invalid address kept %d receipts", n)
		}
		f.Set("address", regtestAddr("nonce-ship-retry-claim"))
		x.check(x.do("POST", "/escrow/ship", x.vendorSess, f), 303)
	})
	t.Run("lost commit acknowledgement", func(t *testing.T) {
		x := newEscrowEnv(t)
		order := x.paidPhysical(regtestAddr("nonce-ship-lost-refund"), 100000)
		f := btcShipNonceForm(order, regtestAddr("nonce-ship-lost-claim"), randomToken()[:32])
		before := [6]int{}
		before[0], before[1], before[2], before[3], before[4], before[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/ship", order, x.vendor)
		lost := loseNextActionCommit(t, x.testEnv)
		w := x.do("POST", "/escrow/ship", x.vendorSess, f)
		if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
			!strings.Contains(w.Body.String(), "Escrow action outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
			t.Fatalf("lost ship ack status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
		if x.state(order) != stateShipped {
			t.Fatal("ship did not commit before acknowledgement was lost")
		}
		committed := [6]int{}
		committed[0], committed[1], committed[2], committed[3], committed[4], committed[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/ship", order, x.vendor)
		if committed[0] != 1 || committed[1] != before[1]+1 || committed[2] <= before[2] || committed[3] <= before[3] || committed[4] != before[4]+1 {
			t.Fatalf("lost-ack Commit did not persist one ship: before=%v committed=%v", before, committed)
		}
		var releaseAt time.Time
		if err := x.DB.QueryRow("SELECT release_at FROM escrows WHERE order_id=$1", order).Scan(&releaseAt); err != nil {
			t.Fatal(err)
		}
		x.check(x.do("POST", "/escrow/ship", x.vendorSess, f), 303)
		after := [6]int{}
		after[0], after[1], after[2], after[3], after[4], after[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/ship", order, x.vendor)
		var replayAt time.Time
		if err := x.DB.QueryRow("SELECT release_at FROM escrows WHERE order_id=$1", order).Scan(&replayAt); err != nil || after != committed || !replayAt.Equal(releaseAt) {
			t.Fatalf("lost-ack replay changed effects: committed=%v after=%v deadline=%s/%s err=%v", committed, after, releaseAt, replayAt, err)
		}
	})
}
