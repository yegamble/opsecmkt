package market

import (
	"net/url"
	"strings"
	"testing"
)

func escrowReceiptForm(order, addr, nonce string) url.Values {
	f := url.Values{"order_id": {order}, "password": {testPassword}, formNonceField: {nonce}}
	if addr != "" {
		f.Set("address", addr)
	}
	return f
}

func escrowReceiptCounts(t *testing.T, e *testEnv, path, order, actor string) (receipt, audits, events, notices, btc, xmr int) {
	t.Helper()
	for _, q := range []struct {
		sql  string
		args []any
		out  *int
	}{
		{"SELECT count(*) FROM form_submissions WHERE action_path=$1", []any{path}, &receipt},
		{"SELECT count(*) FROM audit_events WHERE user_id=$1", []any{actor}, &audits},
		{"SELECT count(*) FROM order_events WHERE order_id=$1", []any{order}, &events},
		{"SELECT count(*) FROM notifications WHERE user_id IN (SELECT buyer_id FROM orders WHERE id=$1 UNION SELECT p.vendor_id FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=$1)", []any{order}, &notices},
		{"SELECT count(*) FROM settlements WHERE order_id=$1", []any{order}, &btc},
		{"SELECT count(*) FROM xmr_settlements WHERE order_id=$1", []any{order}, &xmr},
	} {
		if err := e.DB.QueryRow(q.sql, q.args...).Scan(q.out); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestEscrowConfirmedNonceBitcoinClaimReplay(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.completed(refundAddr, 100000)
	page := x.page("/order?id="+order, x.vendorSess)
	nonce := renderedFormNonce(t, page, "/escrow/claim")
	if submissionCount(t, x.testEnv) != 0 {
		t.Fatal("GET wrote a receipt")
	}
	f := escrowReceiptForm(order, claimAddr, nonce)
	before := [6]int{}
	before[0], before[1], before[2], before[3], before[4], before[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/claim", order, x.vendor)
	first := x.do("POST", "/escrow/claim", x.vendorSess, f)
	x.check(first, 303)
	after := [6]int{}
	after[0], after[1], after[2], after[3], after[4], after[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/claim", order, x.vendor)
	if after[0] != 1 || after[1] != before[1]+1 || after[2] != before[2]+1 || after[4] != before[4]+1 {
		t.Fatalf("BTC claim effects before=%v after=%v", before, after)
	}
	rpcCalls := len(x.core.methods())
	// A receipt remains retrievable during a later node outage. A new claim
	// would still fail because its callback needs the provider.
	x.A.escrow.Store(nil)
	replay := x.do("POST", "/escrow/claim", x.vendorSess, f)
	x.check(replay, 303)
	if calls := len(x.core.methods()); calls != rpcCalls {
		t.Fatalf("BTC replay called the wallet %d times", calls-rpcCalls)
	}
	newForm := escrowReceiptForm(order, claimAddr, randomToken()[:32])
	x.check(x.do("POST", "/escrow/claim", x.vendorSess, newForm), 409)
	if first.Header().Get("Location") == "" || replay.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatal("BTC claim replay lost the original redirect")
	}
	changed := escrowReceiptForm(order, "bcrt1qotherclaim000000000000000000000001", nonce)
	x.check(x.do("POST", "/escrow/claim", x.vendorSess, changed), 409)
	x.check(x.do("POST", "/escrow/claim", agSession(x.testEnv, x.vendor), f), 409)
	x.check(x.do("POST", "/escrow/claim", x.buyerSess, f), 403)
	_, outsider := x.user("btc_claim_outsider_"+randomToken()[:6], "buyer")
	x.check(x.do("POST", "/escrow/claim", outsider, f), 404)
	f.Set("password", "wrong password")
	x.check(x.do("POST", "/escrow/claim", x.vendorSess, f), 401)
	got := [6]int{}
	got[0], got[1], got[2], got[3], got[4], got[5] = escrowReceiptCounts(t, x.testEnv, "/escrow/claim", order, x.vendor)
	if got != after {
		t.Fatalf("BTC replay/refusals added effects: %v vs %v", got, after)
	}
	var saved string
	if err := x.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", nonce).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testPassword, claimAddr, x.vendorSess} {
		if strings.Contains(saved, secret) {
			t.Fatal("BTC claim receipt retained raw form or session data")
		}
	}
}

func TestEscrowConfirmedNonceCurrentPartyAndFactor(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _ := x.fundedXMR(x.product)
	key, _ := agEnableTOTP(x.testEnv, x.vendor)
	f := escrowReceiptForm(order, testXMRAddr("party-claim-"+order, true), randomToken()[:32])
	f.Set("code", agTOTPNow(key))
	originalNonce := f.Get(formNonceField)
	x.check(x.do("POST", "/escrow-xmr/claim", x.vendorSess, f), 303)
	step := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", x.vendor)
	x.check(x.do("POST", "/escrow-xmr/claim", x.vendorSess, f), 303)
	if got := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", x.vendor); got != step {
		t.Fatalf("XMR claim replay spent TOTP again: %d to %d", step, got)
	}
	f.Set(formNonceField, randomToken()[:32])
	x.check(x.do("POST", "/escrow-xmr/claim", x.vendorSess, f), 401)
	otherVendor, _ := x.user("new_claim_vendor_"+randomToken()[:6], "vendor")
	agExec(x.testEnv, "UPDATE products SET vendor_id=$1 WHERE id=$2", otherVendor, x.product)
	f.Set(formNonceField, originalNonce) // even an exact saved result needs current party eligibility
	x.check(x.do("POST", "/escrow-xmr/claim", x.vendorSess, f), 404)
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order); n != 1 {
		t.Fatalf("changed party created %d XMR settlements", n)
	}
}

func TestEscrowConfirmedNonceMoneroClaimShipAndConfirmReplay(t *testing.T) {
	for _, path := range []string{"/escrow-xmr/claim", "/escrow-xmr/ship", "/escrow-xmr/confirm"} {
		t.Run(path, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			var order, actor, session, addr string
			switch path {
			case "/escrow-xmr/claim":
				order, _ = x.fundedXMR(x.product)
				actor, session, addr = x.vendor, x.vendorSess, testXMRAddr("nonce-claim-"+order, true)
			case "/escrow-xmr/ship":
				order, _ = x.paidXMRPhysical(x.physical(x.vendor))
				actor, session, addr = x.vendor, x.vendorSess, testXMRAddr("nonce-ship-"+order, true)
			case "/escrow-xmr/confirm":
				order, session, _ = x.shippedXMR(x.physical(x.vendor))
				actor = x.partyOf(order, roleBuyer)
			}
			nonce := renderedFormNonce(t, x.page("/order?id="+order, session), path)
			if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path=$1", path); n != 0 {
				t.Fatalf("GET wrote %d receipts", n)
			}
			f := escrowReceiptForm(order, addr, nonce)
			before := [6]int{}
			before[0], before[1], before[2], before[3], before[4], before[5] = escrowReceiptCounts(t, x.testEnv, path, order, actor)
			first := x.do("POST", path, session, f)
			x.check(first, 303)
			after := [6]int{}
			after[0], after[1], after[2], after[3], after[4], after[5] = escrowReceiptCounts(t, x.testEnv, path, order, actor)
			if after[0] != 1 || after[1] != before[1]+1 || after[2] <= before[2] ||
				(path != "/escrow-xmr/confirm" && after[5] != before[5]+1) ||
				(path == "/escrow-xmr/confirm" && after[3] <= before[3]) {
				t.Fatalf("XMR action effects before=%v after=%v", before, after)
			}
			var saved string
			if err := x.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", nonce).Scan(&saved); err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{testPassword, addr, session} {
				if secret != "" && strings.Contains(saved, secret) {
					t.Fatal("XMR receipt retained raw form or session data")
				}
			}
			replay := x.do("POST", path, session, f)
			x.check(replay, 303)
			if first.Header().Get("Location") == "" || replay.Header().Get("Location") != first.Header().Get("Location") {
				t.Fatal("XMR replay lost the original redirect")
			}
			changed := escrowReceiptForm(order, addr, nonce)
			if addr == "" {
				changed.Set("extra", "different valid form body")
			} else {
				changed.Set("address", testXMRAddr("different-"+order, true))
			}
			x.check(x.do("POST", path, session, changed), 409)
			x.check(x.do("POST", path, agSession(x.testEnv, actor), f), 409)
			other := x.vendorSess
			if actor == x.vendor {
				other = x.sessOf(order, roleBuyer)
			}
			x.check(x.do("POST", path, other, f), 403)
			f.Set("password", "wrong password")
			x.check(x.do("POST", path, session, f), 401)
			got := [6]int{}
			got[0], got[1], got[2], got[3], got[4], got[5] = escrowReceiptCounts(t, x.testEnv, path, order, actor)
			if got != after {
				t.Fatalf("XMR replay/refusals added effects: %v vs %v", got, after)
			}
		})
	}
}

func TestEscrowConfirmedNonceRollbackAndLostAcknowledgement(t *testing.T) {
	t.Run("Monero claim rollback", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, _ := x.fundedXMR(x.product)
		f := escrowReceiptForm(order, "bad address", randomToken()[:32])
		x.check(x.do("POST", "/escrow-xmr/claim", x.vendorSess, f), 400)
		if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/escrow-xmr/claim'"); n != 0 {
			t.Fatalf("refused address kept %d receipts", n)
		}
		f.Set("address", testXMRAddr("recovered-claim-"+order, true))
		x.check(x.do("POST", "/escrow-xmr/claim", x.vendorSess, f), 303)
	})
	t.Run("Bitcoin claim lost acknowledgement", func(t *testing.T) {
		x := newEscrowEnv(t)
		order := x.completed(refundAddr, 100000)
		f := escrowReceiptForm(order, claimAddr, randomToken()[:32])
		lost := loseNextActionCommit(t, x.testEnv)
		w := x.do("POST", "/escrow/claim", x.vendorSess, f)
		if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
			!strings.Contains(w.Body.String(), "Escrow action outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
			t.Fatalf("BTC lost ack status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
		x.check(x.do("POST", "/escrow/claim", x.vendorSess, f), 303)
		if receipt, _, events, _, settlements, _ := escrowReceiptCounts(t, x.testEnv, "/escrow/claim", order, x.vendor); receipt != 1 || settlements != 1 || events == 0 {
			t.Fatalf("BTC lost ack receipt=%d settlement=%d events=%d", receipt, settlements, events)
		}
	})
	t.Run("Monero confirm lost acknowledgement", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, buyer, _ := x.shippedXMR(x.physical(x.vendor))
		f := escrowReceiptForm(order, "", randomToken()[:32])
		before := [6]int{}
		before[0], before[1], before[2], before[3], before[4], before[5] = escrowReceiptCounts(t, x.testEnv, "/escrow-xmr/confirm", order, x.partyOf(order, roleBuyer))
		lost := loseNextActionCommit(t, x.testEnv)
		w := x.do("POST", "/escrow-xmr/confirm", buyer, f)
		if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
			!strings.Contains(w.Body.String(), "Escrow action outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
			t.Fatalf("XMR lost ack status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
		if state := x.state(order); state != stateCompleted {
			t.Fatalf("XMR confirmation not committed after lost acknowledgement: %s", state)
		}
		x.check(x.do("POST", "/escrow-xmr/confirm", buyer, f), 303)
		after := [6]int{}
		after[0], after[1], after[2], after[3], after[4], after[5] = escrowReceiptCounts(t, x.testEnv, "/escrow-xmr/confirm", order, x.partyOf(order, roleBuyer))
		if after[0] != 1 || after[1] != before[1]+1 || after[2] != before[2]+1 || after[3] <= before[3] || after[5] != before[5] {
			t.Fatalf("XMR lost ack effects before=%v after=%v", before, after)
		}
	})
}
