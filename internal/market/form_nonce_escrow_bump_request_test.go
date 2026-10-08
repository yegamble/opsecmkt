package market

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func bumpNonceForm(order string, sid int64, rate, nonce string) url.Values {
	return url.Values{
		"order_id": {order}, "settlement_id": {strconv.FormatInt(sid, 10)},
		"fee_rate": {rate}, "password": {testPassword}, formNonceField: {nonce},
	}
}

type bumpNonceEffects struct {
	receipts, bumps, audits, events, notices int
}

func bumpEffects(t *testing.T, x *escrowEnv, order string, sid int64) bumpNonceEffects {
	t.Helper()
	return bumpNonceEffects{
		agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/escrow/bump'"),
		agInt(x.testEnv, "SELECT count(*) FROM settlement_bumps WHERE settlement_id=$1", sid),
		agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1", x.vendor),
		agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", order),
		agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1", x.buyer),
	}
}

func bumpCreatedUpdated(t *testing.T, x *escrowEnv, id int64) (time.Time, time.Time) {
	t.Helper()
	var created, updated time.Time
	if err := x.DB.QueryRow("SELECT created,updated FROM settlement_bumps WHERE id=$1", id).Scan(&created, &updated); err != nil {
		t.Fatal(err)
	}
	return created, updated
}

func TestEscrowBumpConfirmedNonceReplaySupersedeAndProviderOutage(t *testing.T) {
	x := newEscrowEnv(t)
	order, sid := x.broadcastClaim("bump-nonce-replay", 100000)
	nonce := renderedFormNonce(t, x.page("/order?id="+order, x.vendorSess), "/escrow/bump")
	if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/escrow/bump'"); n != 0 {
		t.Fatalf("GET wrote %d bump receipts", n)
	}
	f := bumpNonceForm(order, sid, "5", nonce)
	release, ok := x.A.enterOneAtATime("/escrow/bump", x.vendor)
	if !ok {
		t.Fatal("could not hold existing bump admission slot")
	}
	busy := x.do("POST", "/escrow/bump", x.vendorSess, f)
	if busy.Code != 429 || busy.Header().Get("Retry-After") != busyRetryAfter || !strings.Contains(busy.Body.String(), "resubmit it unchanged") {
		t.Fatalf("in-flight bump status=%d retry=%q body=%q", busy.Code, busy.Header().Get("Retry-After"), busy.Body.String())
	}
	release()
	before, rpcBefore := bumpEffects(t, x, order, sid), len(x.core.methods())
	first := x.do("POST", "/escrow/bump", x.vendorSess, f)
	x.check(first, 303)
	if len(x.core.methods()) != rpcBefore {
		t.Fatal("fee bump request called Bitcoin Core")
	}
	after := bumpEffects(t, x, order, sid)
	if after.receipts != before.receipts+1 || after.bumps != before.bumps+1 || after.audits != before.audits+1 ||
		after.events != before.events+1 || after.notices != before.notices+1 {
		t.Fatalf("first bump effects before=%+v after=%+v", before, after)
	}
	b1 := x.lastBump(sid)
	if b1.State != "building" || b1.Rate != 5 {
		t.Fatalf("first bump: %+v", b1)
	}
	created1, updated1 := bumpCreatedUpdated(t, x, b1.ID)
	provider := x.A.escrow.Load()
	x.A.escrow.Store(nil)
	replay := x.do("POST", "/escrow/bump", x.vendorSess, f)
	x.check(replay, 303)
	if first.Header().Get("Location") == "" || replay.Header().Get("Location") != first.Header().Get("Location") ||
		len(x.core.methods()) != rpcBefore || bumpEffects(t, x, order, sid) != after {
		t.Fatal("exact bump replay changed redirect/effects or contacted Bitcoin Core")
	}
	changed := bumpNonceForm(order, sid, "6", nonce)
	x.check(x.do("POST", "/escrow/bump", x.vendorSess, changed), 409)
	x.check(x.do("POST", "/escrow/bump", agSession(x.testEnv, x.vendor), f), 409)
	x.check(x.do("POST", "/escrow/bump", x.buyerSess, f), 403)
	_, outsider := x.user("bump_nonce_outsider_"+randomToken()[:6], "vendor")
	x.check(x.do("POST", "/escrow/bump", outsider, f), 404)
	f.Set("password", "wrong password")
	x.check(x.do("POST", "/escrow/bump", x.vendorSess, f), 401)
	if bumpEffects(t, x, order, sid) != after {
		t.Fatal("bump replay/refusals added effects")
	}
	if created, updated := bumpCreatedUpdated(t, x, b1.ID); !created.Equal(created1) || !updated.Equal(updated1) {
		t.Fatal("replay/refusals changed first bump timestamps")
	}
	var saved string
	if err := x.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", nonce).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testPassword, x.vendorSess} {
		if strings.Contains(saved, secret) {
			t.Fatal("bump receipt retained raw password or session")
		}
	}
	x.A.escrow.Store(provider)
	f.Set("password", testPassword)
	f2 := bumpNonceForm(order, sid, "6", randomToken()[:32])
	x.check(x.do("POST", "/escrow/bump", x.vendorSess, f2), 303)
	b2 := x.lastBump(sid)
	if b2.N != 2 || b2.Rate != 6 || b2.State != "building" || x.bumpByID(b1.ID).State != "superseded" {
		t.Fatalf("new bump did not supersede earlier open bump: first=%+v second=%+v", x.bumpByID(b1.ID), b2)
	}
	closedCreated, closedUpdated := bumpCreatedUpdated(t, x, b1.ID)
	secondCreated, secondUpdated := bumpCreatedUpdated(t, x, b2.ID)
	committed := bumpEffects(t, x, order, sid)
	x.check(x.do("POST", "/escrow/bump", x.vendorSess, f2), 303)
	x.check(x.do("POST", "/escrow/bump", x.vendorSess, f), 303)
	if bumpEffects(t, x, order, sid) != committed || x.bumpByID(b1.ID).State != "superseded" || x.bumpByID(b2.ID).State != "building" {
		t.Fatal("old or current receipt opened or superseded a bump twice")
	}
	if c, u := bumpCreatedUpdated(t, x, b1.ID); !c.Equal(closedCreated) || !u.Equal(closedUpdated) {
		t.Fatal("replay changed superseded bump timestamps")
	}
	if c, u := bumpCreatedUpdated(t, x, b2.ID); !c.Equal(secondCreated) || !u.Equal(secondUpdated) {
		t.Fatal("replay changed current bump timestamps")
	}
}

func TestEscrowBumpConfirmedNonceCurrentClaimantAndTOTP(t *testing.T) {
	x := newEscrowEnv(t)
	order, sid := x.broadcastRefund("bump-nonce-buyer", 40000)
	key, _ := agEnableTOTP(x.testEnv, x.buyer)
	f := bumpNonceForm(order, sid, "5", randomToken()[:32])
	f.Set("code", agTOTPNow(key))
	original := f.Get(formNonceField)
	x.check(x.do("POST", "/escrow/bump", x.buyerSess, f), 303)
	step := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", x.buyer)
	x.check(x.do("POST", "/escrow/bump", x.buyerSess, f), 303)
	if got := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", x.buyer); got != step {
		t.Fatalf("bump replay spent TOTP again: %d to %d", step, got)
	}
	f.Set(formNonceField, randomToken()[:32])
	x.check(x.do("POST", "/escrow/bump", x.buyerSess, f), 401)
	f.Set(formNonceField, original)
	x.check(x.do("POST", "/escrow/bump", x.vendorSess, f), 403)
	newBuyer, _ := x.user("new_bump_buyer_"+randomToken()[:6], "buyer")
	agExec(x.testEnv, "UPDATE orders SET buyer_id=$1 WHERE id=$2", newBuyer, order)
	x.check(x.do("POST", "/escrow/bump", x.buyerSess, f), 404)
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlement_bumps WHERE settlement_id=$1", sid); n != 1 {
		t.Fatalf("changed claimant created %d fee bumps", n)
	}

	// A settlement id from another order is not authorization to retrieve its
	// saved response; an XMR order cannot own a Bitcoin settlement.
	otherOrder, _ := x.broadcastClaim("bump-nonce-other-order", 100000)
	wrong := bumpNonceForm(otherOrder, sid, "5", randomToken()[:32])
	x.check(x.do("POST", "/escrow/bump", x.vendorSess, wrong), 404)
	m := newXMREscrowEnv(t)
	m.mustPoll()
	xmrOrder, _ := m.fundedXMR(m.product)
	tx, err := m.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	c := &actionCtx{Tx: tx, R: httptest.NewRequest("POST", "/escrow/bump", nil), User: &User{ID: m.partyOf(xmrOrder, roleBuyer)},
		Form: url.Values{"order_id": {xmrOrder}, "settlement_id": {strconv.FormatInt(sid, 10)}}}
	if err := btcBumpReceiptAuthorize(c); err != errNoOrder {
		t.Fatalf("XMR order accepted by BTC bump hook: %v", err)
	}

	// Legacy data can name the same account as buyer and vendor. The original
	// callback gives vendor precedence; the replay gate must make the same
	// choice for a buyer-claimant refund.
	z := newEscrowEnv(t)
	coincidentOrder, coincidentSID := z.broadcastRefund("bump-nonce-coincident", 40000)
	agExec(z.testEnv, "UPDATE products SET vendor_id=$1 WHERE id=(SELECT product_id FROM orders WHERE id=$2)", z.buyer, coincidentOrder)
	coincident := bumpNonceForm(coincidentOrder, coincidentSID, "5", randomToken()[:32])
	z.check(z.do("POST", "/escrow/bump", z.buyerSess, coincident), 403)
	if n := agInt(z.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/escrow/bump'"); n != 0 {
		t.Fatalf("coincident-party refusal wrote %d receipts", n)
	}
}

func TestEscrowBumpConfirmedNonceRollbackAndLostAck(t *testing.T) {
	t.Run("fee refusal rolls back claim", func(t *testing.T) {
		x := newEscrowEnv(t)
		order, sid := x.broadcastClaim("bump-nonce-retry", 100000)
		f := bumpNonceForm(order, sid, "2", randomToken()[:32])
		x.check(x.do("POST", "/escrow/bump", x.vendorSess, f), 400)
		if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/escrow/bump'"); n != 0 {
			t.Fatalf("low fee kept %d receipts", n)
		}
		f.Set("fee_rate", "5")
		x.check(x.do("POST", "/escrow/bump", x.vendorSess, f), 303)
		if n := agInt(x.testEnv, "SELECT count(*) FROM settlement_bumps WHERE settlement_id=$1", sid); n != 1 {
			t.Fatalf("corrected retry created %d bumps", n)
		}
	})
	t.Run("lost commit acknowledgement", func(t *testing.T) {
		x := newEscrowEnv(t)
		order, sid := x.broadcastClaim("bump-nonce-lost", 100000)
		f := bumpNonceForm(order, sid, "5", randomToken()[:32])
		before := bumpEffects(t, x, order, sid)
		lost := loseNextActionCommit(t, x.testEnv)
		w := x.do("POST", "/escrow/bump", x.vendorSess, f)
		if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
			!strings.Contains(w.Body.String(), "Escrow action outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
			t.Fatalf("lost bump ack status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
		committed := bumpEffects(t, x, order, sid)
		if committed.receipts != before.receipts+1 || committed.bumps != before.bumps+1 || committed.audits != before.audits+1 ||
			committed.events != before.events+1 || committed.notices != before.notices+1 {
			t.Fatalf("lost ack did not persist one bump: before=%+v committed=%+v", before, committed)
		}
		b := x.lastBump(sid)
		created, updated := bumpCreatedUpdated(t, x, b.ID)
		x.check(x.do("POST", "/escrow/bump", x.vendorSess, f), 303)
		if bumpEffects(t, x, order, sid) != committed {
			t.Fatal("lost-ack replay added bump effects")
		}
		if c, u := bumpCreatedUpdated(t, x, b.ID); !c.Equal(created) || !u.Equal(updated) {
			t.Fatal("lost-ack replay changed bump timestamps")
		}
	})
}
