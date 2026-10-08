package market

import (
	"net/url"
	"strings"
	"sync"
	"testing"
)

func concessionForm(id, outcome, nonce string, seen int64) url.Values {
	return payoutConfirmed(url.Values{
		"id": {id}, "outcome": {outcome}, "password": {testPassword}, formNonceField: {nonce},
	}, seen)
}

func concessionEffects(t *testing.T, e *testEnv, order string) (receipt, payout, event, audit, staffNotice int) {
	t.Helper()
	for _, query := range []struct {
		stmt string
		args []any
		out  *int
	}{
		{"SELECT count(*) FROM form_submissions WHERE action_path='/disputes/concede'", nil, &receipt},
		{"SELECT count(*) FROM payouts WHERE order_id=$1", []any{order}, &payout},
		{"SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state='disputed' AND to_state IN ('completed','cancelled')", []any{order}, &event},
		{"SELECT count(*) FROM audit_events WHERE action LIKE $1", []any{"Conceded dispute on order " + shortID(order) + "%"}, &audit},
		{"SELECT count(*) FROM notifications WHERE body LIKE $1", []any{"Dispute on order " + shortID(order) + " is now Withdrawn%"}, &staffNotice},
	} {
		if err := e.DB.QueryRow(query.stmt, query.args...).Scan(query.out); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestConcessionPooledRefundReleaseReceiptAndViews(t *testing.T) {
	for _, outcome := range []string{"release", "refund"} {
		t.Run(outcome, func(t *testing.T) {
			p := newPayEnv(t)
			p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
			p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
			dispute, staff := p.disputedOrder(100000)
			sess, otherSess, actorID, wantState, wantStatus := p.buyerSess, p.vendorSess, p.buyer.ID, stateCompleted, concededByBuyer
			if outcome == "refund" {
				sess, otherSess, actorID, wantState, wantStatus = p.vendorSess, p.buyerSess, p.vendor.ID, stateCancelled, concededByVendor
			}
			page := p.page(orderPage(p.order), sess)
			nonce := renderedFormNonce(t, page, "/disputes/concede")
			mustContain(t, page, `name="amount_seen" value="100000"`, `name="payout_confirmed" value="confirmed" required`, "not returned to stock")
			mustNotContain(t, p.page(orderPage(p.order), otherSess), `name="outcome" value="`+outcome+`"`)
			if got := p.count("SELECT count(*) FROM form_submissions WHERE action_path='/disputes/concede'"); got != 0 {
				t.Fatalf("GET wrote %d receipts", got)
			}
			if outcome == "release" {
				if _, err := p.DB.Exec("UPDATE users SET suspended_at=now() WHERE id=$1", p.mod.ID); err != nil {
					t.Fatal(err)
				}
			}
			stock := p.count("SELECT stock FROM products WHERE id=$1", p.productID)
			staffRecipients := p.count("SELECT count(*) FROM users WHERE role IN ('moderator','admin') AND suspended_at IS NULL AND id<>$1 AND id<>$2", p.buyer.ID, p.vendor.ID)
			f := concessionForm(dispute, outcome, nonce, 100000)
			first := p.do("POST", "/disputes/concede", sess, f)
			p.check(first, 303)
			replay := p.do("POST", "/disputes/concede", sess, f)
			p.check(replay, 303)
			if first.Header().Get("Location") == "" || first.Header().Get("Location") != replay.Header().Get("Location") {
				t.Fatal("exact concession replay changed redirect")
			}
			oneShot(t, p.testEnv, sess, first, orderPage(p.order)+"#order-dispute", "dispute-conceded",
				"Dispute conceded. Check the order and payout or escrow settlement below.", "", 2, orderPage(p.order))
			if p.state(p.order) != wantState || p.count("SELECT stock FROM products WHERE id=$1", p.productID) != stock ||
				p.count("SELECT count(*) FROM disputes WHERE id=$1 AND status=$2 AND outcome=$3", dispute, wantStatus, outcome) != 1 {
				t.Fatal("concession changed wrong state, status, outcome or stock")
			}
			if receipt, payout, event, audit, notices := concessionEffects(t, p.testEnv, p.order); receipt != 1 || payout != 1 || event != 1 || audit != 1 || notices != staffRecipients {
				t.Fatalf("effects %d %d %d %d %d", receipt, payout, event, audit, notices)
			}
			if outcome == "release" && p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", p.mod.ID, "Dispute on order "+shortID(p.order)+" is now Withdrawn%") != 0 {
				t.Fatal("suspended moderator got a closure notice")
			}
			var kind string
			if err := p.DB.QueryRow("SELECT kind FROM payouts WHERE order_id=$1", p.order).Scan(&kind); err != nil || kind != outcome {
				t.Fatalf("payout kind=%q err=%v", kind, err)
			}
			mustContain(t, p.page(orderPage(p.order), staff), "Concession", wantStatus)
			mustContain(t, p.page("/disputes", sess), "1 closed", "1 withdrawn by a party")
			var receiptJSON string
			if err := p.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", nonce).Scan(&receiptJSON); err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{testPassword, sess, "fake-testnet-buyer", "fake-testnet-vendor"} {
				if strings.Contains(receiptJSON, secret) {
					t.Fatal("receipt retained a secret or destination")
				}
			}
			changed := concessionForm(dispute, outcome, nonce, 100001)
			p.check(p.do("POST", "/disputes/concede", sess, changed), 409)
			p.check(p.do("POST", "/disputes/concede", agSession(p.testEnv, actorID), f), 409)
			p.check(p.do("POST", "/disputes/concede", sess, concessionForm(dispute, outcome, randomToken()[:32], 100000)), 409)
			if receipt, payout, event, audit, notices := concessionEffects(t, p.testEnv, p.order); receipt != 1 || payout != 1 || event != 1 || audit != 1 || notices != staffRecipients {
				t.Fatalf("refusals changed effects %d %d %d %d %d", receipt, payout, event, audit, notices)
			}
		})
	}
}

func TestConcessionRefusalsRollbackAndTOTP(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	dispute, staff := p.disputedOrder(100000)
	nonce := renderedFormNonce(t, p.page(orderPage(p.order), p.buyerSess), "/disputes/concede")
	f := concessionForm(dispute, "release", nonce, 100000)
	noTick := concessionForm(dispute, "release", nonce, 100000)
	noTick.Del("payout_confirmed")
	p.check(p.do("POST", "/disputes/concede", p.buyerSess, noTick), 400)
	noAmount := concessionForm(dispute, "release", nonce, 100000)
	noAmount.Del("amount_seen")
	p.check(p.do("POST", "/disputes/concede", p.buyerSess, noAmount), 400)
	p.check(p.do("POST", "/disputes/concede", p.vendorSess, f), 403)
	p.check(p.do("POST", "/disputes/concede", staff, f), 403)
	_, outsider := p.user("concession_outsider_"+randomToken()[:6], "buyer")
	p.check(p.do("POST", "/disputes/concede", outsider, f), 403)
	// A forged ordinary action cannot bypass the dispute CAS or step-up.
	p.check(p.do("POST", "/orders/complete", p.buyerSess, payoutConfirmed(url.Values{"order_id": {p.order}}, 100000)), 409)
	p.check(p.do("POST", "/orders/cancel", p.vendorSess, payoutConfirmed(url.Values{"order_id": {p.order}, "from": {statePaid}}, 100000)), 409)
	p.check(p.do("POST", "/orders/cancel", p.vendorSess, url.Values{"order_id": {p.order}, "from": {stateDisputed}}), 400)
	tx, err := p.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.A.transition(withPayoutSeen(t.Context(), p.order, 100000), tx, p.order, stateDisputed, stateCompleted, p.buyer, "forged concession")
	_ = tx.Rollback()
	if err != errForbidden {
		t.Fatalf("direct transition without concession context: %v", err)
	}
	p.check(p.do("POST", "/disputes/concede", p.buyerSess, concessionForm(dispute, "release", nonce, 100001)), 409)
	if p.state(p.order) != stateDisputed || p.count("SELECT count(*) FROM form_submissions WHERE action_path='/disputes/concede'") != 0 ||
		p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order) != 0 {
		t.Fatal("refusal changed the order or kept a receipt")
	}
	key, _ := agEnableTOTP(p.testEnv, p.buyer.ID)
	f.Set("code", agTOTPNow(key))
	p.check(p.do("POST", "/disputes/concede", p.buyerSess, f), 303)
	step := p.count("SELECT totp_last_step FROM users WHERE id=$1", p.buyer.ID)
	p.check(p.do("POST", "/disputes/concede", p.buyerSess, f), 303)
	if p.count("SELECT totp_last_step FROM users WHERE id=$1", p.buyer.ID) != step {
		t.Fatal("concession replay consumed TOTP")
	}
	f.Set(formNonceField, randomToken()[:32])
	p.check(p.do("POST", "/disputes/concede", p.buyerSess, f), 401)
}

func TestConcessionZeroSeenRejectsNewDepositAndReusesRolledBackNonce(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order, addr := p.newOrder(stateDisputed)
	dispute := randomToken()
	if _, err := p.DB.Exec("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The item was not delivered; please review.')", dispute, order); err != nil {
		t.Fatal(err)
	}
	page := p.page(orderPage(order), p.buyerSess)
	nonce := renderedFormNonce(t, page, "/disputes/concede")
	mustContain(t, page, `name="amount_seen" value="0"`)
	p.fake.Deposit(addr, "tx-"+randomToken()[:8], 0, 100000, 3)
	p.poll()
	f := concessionForm(dispute, "release", nonce, 0)
	p.check(p.do("POST", "/disputes/concede", p.buyerSess, f), 409)
	if p.state(order) != stateDisputed || p.count("SELECT count(*) FROM disputes WHERE id=$1 AND status='Open' AND outcome=''", dispute) != 1 ||
		p.count("SELECT count(*) FROM payouts WHERE order_id=$1", order) != 0 ||
		p.count("SELECT count(*) FROM form_submissions WHERE nonce=$1", nonce) != 0 {
		t.Fatal("changed zero amount kept a concession or receipt")
	}
	f.Set("amount_seen", "100000")
	p.check(p.do("POST", "/disputes/concede", p.buyerSess, f), 303)
	if p.state(order) != stateCompleted || p.count("SELECT count(*) FROM payouts WHERE order_id=$1", order) != 1 {
		t.Fatal("corrected concession failed to release counted deposit")
	}
}

func TestConcessionLostCommitAcknowledgement(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	dispute, _ := p.disputedOrder(100000)
	staffRecipients := p.count("SELECT count(*) FROM users WHERE role IN ('moderator','admin') AND suspended_at IS NULL AND id<>$1 AND id<>$2", p.buyer.ID, p.vendor.ID)
	f := concessionForm(dispute, "refund", randomToken()[:32], 100000)
	lost := loseNextActionCommit(t, p.testEnv)
	w := p.do("POST", "/disputes/concede", p.vendorSess, f)
	if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" || !strings.Contains(w.Body.String(), "Concession outcome unknown") ||
		!strings.Contains(w.Body.String(), "resubmit it unchanged") {
		t.Fatalf("lost acknowledgement status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if p.state(p.order) != stateCancelled {
		t.Fatal("lost acknowledgement did not persist concession")
	}
	p.check(p.do("POST", "/disputes/concede", p.vendorSess, f), 303)
	if receipt, payout, event, audit, notices := concessionEffects(t, p.testEnv, p.order); receipt != 1 || payout != 1 || event != 1 || audit != 1 || notices != staffRecipients {
		t.Fatalf("lost-ack replay changed effects %d %d %d %d %d", receipt, payout, event, audit, notices)
	}
}

func TestConcessionBitcoinEscrowKeepsSettlementSigningPath(t *testing.T) {
	for _, outcome := range []string{"release", "refund"} {
		t.Run(outcome, func(t *testing.T) {
			x := newEscrowEnv(t)
			order, dispute := x.disputedEscrow(regtestAddr("concession-escrow-refund-" + outcome))
			sess, state, status := x.buyerSess, stateCompleted, concededByBuyer
			if outcome == "refund" {
				sess, state, status = x.vendorSess, stateCancelled, concededByVendor
			}
			page := x.page(orderPage(order), sess)
			f := concessionForm(dispute, outcome, renderedFormNonce(t, page, "/disputes/concede"), 100000)
			if outcome == "refund" {
				if _, err := x.DB.Exec("UPDATE payments SET amount=amount+1 WHERE order_id=$1", order); err != nil {
					t.Fatal(err)
				}
				x.check(x.do("POST", "/disputes/concede", sess, f), 409)
				if x.state(order) != stateDisputed || agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/disputes/concede'") != 0 ||
					agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", order) != 0 {
					t.Fatal("changed Bitcoin escrow amount kept a concession or settlement")
				}
				if _, err := x.DB.Exec("UPDATE payments SET amount=amount-1 WHERE order_id=$1", order); err != nil {
					t.Fatal(err)
				}
			}
			calls := len(x.core.methods())
			x.check(x.do("POST", "/disputes/concede", sess, f), 303)
			if len(x.core.methods()) != calls || x.state(order) != state ||
				agInt(x.testEnv, "SELECT count(*) FROM disputes WHERE id=$1 AND status=$2 AND outcome=$3", dispute, status, outcome) != 1 ||
				agInt(x.testEnv, "SELECT count(*) FROM payouts WHERE order_id=$1", order) != 0 {
				t.Fatal("Bitcoin concession called Core or changed wrong state, dispute or pooled payout")
			}
			if outcome == "refund" {
				_, kind, settlementState := x.mainSettlement(order)
				if kind != "refund" || settlementState != "building" {
					t.Fatalf("refund settlement %s %s", kind, settlementState)
				}
			} else if n := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", order); n != 0 {
				t.Fatalf("fresh release unexpectedly made %d settlements before vendor claim", n)
			}
			x.check(x.do("POST", "/disputes/concede", sess, f), 303)
			if len(x.core.methods()) != calls || agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/disputes/concede'") != 1 {
				t.Fatal("Bitcoin escrow replay contacted Core or wrote another receipt")
			}
		})
	}
}

func TestConcessionMoneroEscrowWatcherSettlement(t *testing.T) {
	for _, outcome := range []string{"release", "refund"} {
		t.Run(outcome, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll() // prepare the wallet/account slots that checkout needs
			order, buyerSess := x.undeliveredXMR()
			if code, body := x.xmrDispute(order, buyerSess); code != 303 {
				t.Fatalf("open Monero dispute: %d %s", code, body)
			}
			dispute := agStrings(t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)[0]
			sess, state := buyerSess, stateCompleted
			if outcome == "refund" {
				sess, state = x.vendorSess, stateCancelled
			}
			page := x.do("GET", orderPage(order), sess, nil)
			x.check(page, 200)
			f := concessionForm(dispute, outcome, renderedFormNonce(t, page.Body.String(), "/disputes/concede"), xmrListingPrice)
			if outcome == "refund" {
				agExec(x.testEnv, "UPDATE payments SET amount=amount+1 WHERE order_id=$1", order)
				x.check(x.do("POST", "/disputes/concede", sess, f), 409)
				if x.state(order) != stateDisputed || agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/disputes/concede'") != 0 ||
					agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order) != 0 {
					t.Fatal("changed Monero escrow amount kept a concession or settlement")
				}
				agExec(x.testEnv, "UPDATE payments SET amount=amount-1 WHERE order_id=$1", order)
			}
			calls := len(x.fake.callLog())
			x.check(x.do("POST", "/disputes/concede", sess, f), 303)
			if len(x.fake.callLog()) != calls || x.state(order) != state ||
				agInt(x.testEnv, "SELECT count(*) FROM payouts WHERE order_id=$1", order) != 0 {
				t.Fatal("Monero concession made wallet RPC, changed wrong state or queued pooled payout")
			}
			if outcome == "refund" {
				if got := x.xmrSettlements(order); len(got) != 1 || !strings.HasPrefix(got[0], "refund requested ") {
					t.Fatalf("refund watcher request %v", got)
				}
				x.mustPoll()
				if got := x.xmrSettlements(order); len(got) != 1 || !strings.HasPrefix(got[0], "refund relayed ") {
					t.Fatalf("refund watcher send %v", got)
				}
			} else if got := x.xmrSettlements(order); len(got) != 0 {
				t.Fatalf("release requires vendor claim, got %v", got)
			}
			x.check(x.do("POST", "/disputes/concede", sess, f), 303)
			if agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/disputes/concede'") != 1 ||
				agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order) > 1 {
				t.Fatal("Monero escrow replay changed receipt or settlement")
			}
		})
	}
}

func TestConcessionResolveRaceOneOutcome(t *testing.T) {
	for i := 0; i < 20; i++ {
		p := newPayEnv(t)
		p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
		dispute, staff := p.disputedOrder(100000)
		concede := concessionForm(dispute, "release", randomToken()[:32], 100000)
		resolve := resolveNonceForm(dispute, "release", randomToken()[:32])
		start := make(chan struct{})
		codes := make(chan int, 2)
		var wg sync.WaitGroup
		for _, call := range []func() int{
			func() int { return p.do("POST", "/disputes/concede", p.buyerSess, concede).Code },
			func() int { return p.do("POST", "/resolve", staff, resolve).Code },
		} {
			wg.Add(1)
			go func(fn func() int) { defer wg.Done(); <-start; codes <- fn() }(call)
		}
		close(start)
		wg.Wait()
		a, b := <-codes, <-codes
		if !(a == 303 && b == 409 || a == 409 && b == 303) {
			t.Fatalf("race %d statuses %d, %d", i, a, b)
		}
		if p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order) != 1 ||
			p.count("SELECT count(*) FROM disputes WHERE id=$1 AND outcome='release' AND status<>'Open'", dispute) != 1 ||
			p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state='disputed' AND to_state IN ('resolved','completed')", p.order) != 1 {
			t.Fatalf("race %d stored more than one outcome", i)
		}
	}
}
