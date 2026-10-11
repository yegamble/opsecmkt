package market

import (
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func concessionForm(id, outcome, nonce string, seen int64) url.Values {
	return payoutConfirmed(url.Values{
		"id": {id}, "outcome": {outcome}, "password": {testPassword}, formNonceField: {nonce},
	}, seen)
}

// concessionEffects counts what a concession on an escrow order writes: its form receipt, the order's live release or
// refund, the move out of disputed, its audit row and the staff notices.
func concessionEffects(x *escrowEnv, order string) (receipt, settlement, event, audit, staffNotice int) {
	x.t.Helper()
	return agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/disputes/concede'"),
		agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind IN ('release','refund') AND state<>'superseded'", order),
		agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state='disputed' AND to_state IN ('completed','cancelled')", order),
		agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE action LIKE $1", "Conceded dispute on order "+shortID(order)+"%"),
		agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE body LIKE $1", "Dispute on order "+shortID(order)+" is now Withdrawn%")
}

// A-166 on escrow (A-493): a concession moves escrowed coins, so every refusal (no confirmation, no amount, the wrong
// party, staff or an outsider, a changed amount) leaves the order, the dispute, the settlements and the form receipt as
// they were; the ordinary Complete and Cancel forms, and a transition without the concession's own context, cannot take
// the concession's state edge; and once the buyer enrolled TOTP the concession needs a current code, which an exact
// replay does not spend again and a new form cannot reuse. Fake escrow Core: simulated RPC.
func TestConcessionRefusalsRollbackAndTOTP(t *testing.T) {
	x := newEscrowEnv(t)
	s := randomToken()[:6]
	_, admin := x.user("econcede_admin_"+s, "admin")
	_, mod := x.user("econcede_mod_"+s, "moderator")
	_, outsider := x.user("econcede_outsider_"+s, "buyer")
	order, dispute := x.disputedEscrow(regtestAddr("concede-refusals-refund"))
	nonce := renderedFormNonce(t, x.page(orderPage(order), x.buyerSess), "/disputes/concede")
	f := concessionForm(dispute, "release", nonce, 100000)
	before := x.escrowUnchanged(order)
	refuse := func(name, path, sess string, form url.Values, code int) {
		t.Helper()
		if w := x.do("POST", path, sess, form); w.Code != code {
			t.Fatalf("%s: %d, want %d: %s", name, w.Code, code, w.Body.String())
		}
		if after := x.escrowUnchanged(order); after != before {
			t.Fatalf("%s wrote something:\n before %s\n after  %s", name, before, after)
		}
	}
	noTick := cloneValues(f)
	noTick.Del("payout_confirmed")
	refuse("no confirmation", "/disputes/concede", x.buyerSess, noTick, 400)
	noAmount := cloneValues(f)
	noAmount.Del("amount_seen")
	refuse("no amount_seen", "/disputes/concede", x.buyerSess, noAmount, 400)
	refuse("the vendor posting the buyer's concession", "/disputes/concede", x.vendorSess, f, 403)
	refuse("an administrator", "/disputes/concede", admin, f, 403)
	refuse("a moderator", "/disputes/concede", mod, f, 403)
	refuse("an outsider", "/disputes/concede", outsider, f, 403)
	refuse("the buyer posting the vendor's concession", "/disputes/concede", x.buyerSess, concessionForm(dispute, "refund", nonce, 100000), 403)
	// A forged ordinary action cannot bypass the dispute compare-and-set, the confirmation or the step-up.
	refuse("a forged Complete", "/orders/complete", x.buyerSess, payoutConfirmed(url.Values{"order_id": {order}}, 100000), 409)
	refuse("a forged paid cancel", "/orders/cancel", x.vendorSess, payoutConfirmed(url.Values{"order_id": {order}, "from": {statePaid}}, 100000), 409)
	refuse("a forged disputed cancel", "/orders/cancel", x.vendorSess, url.Values{"order_id": {order}, "from": {stateDisputed}}, 400)
	buyer, vendor := x.loadUser(x.buyer), x.loadUser(x.vendor)
	for _, c := range []struct {
		name, concededBy string // "" = no concession context
		actor            *User
	}{
		{"no concession context", "", buyer},
		{"another actor's concession context", x.vendor, buyer},
		{"the buyer's concession context used by the vendor", x.buyer, vendor},
	} {
		tx, err := x.DB.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := withPayoutSeen(t.Context(), order, 100000)
		if c.concededBy != "" {
			ctx = withConcessionTransition(ctx, order, stateCompleted, c.concededBy)
		}
		_, err = x.A.transition(ctx, tx, order, stateDisputed, stateCompleted, c.actor, "forged concession")
		_ = tx.Rollback()
		if err != errForbidden {
			t.Fatalf("direct transition with %s: %v", c.name, err)
		}
	}
	refuse("a changed amount", "/disputes/concede", x.buyerSess, concessionForm(dispute, "release", nonce, 100001), 409)

	key, _ := agEnableTOTP(x.testEnv, x.buyer)
	before = x.escrowUnchanged(order)
	refuse("no authenticator code once TOTP is on", "/disputes/concede", x.buyerSess, f, 401)
	f.Set("code", agTOTPNow(key))
	x.check(x.do("POST", "/disputes/concede", x.buyerSess, f), 303)
	if x.state(order) != stateCompleted || agInt(x.testEnv, "SELECT count(*) FROM disputes WHERE id=$1 AND status=$2 AND outcome='release'", dispute, concededByBuyer) != 1 {
		t.Fatalf("the confirmed concession: order %s", x.state(order))
	}
	step := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", x.buyer)
	x.check(x.do("POST", "/disputes/concede", x.buyerSess, f), 303)
	if agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", x.buyer) != step {
		t.Fatal("the concession's replay spent the authenticator code again")
	}
	f.Set(formNonceField, randomToken()[:32])
	x.check(x.do("POST", "/disputes/concede", x.buyerSess, f), 401)
	if receipt, _, event, audit, _ := concessionEffects(x, order); receipt != 1 || event != 1 || audit != 1 {
		t.Fatalf("after the replays: receipts %d, events %d, audits %d", receipt, event, audit)
	}
}

// A-166 on escrow: a party's concession and an administrator's decision of the same dispute race; exactly one commits
// and the other finds the dispute closed (409), so the dispute has one outcome, the order one move out of disputed and
// the escrow at most one settlement.
func TestConcessionResolveRaceOneOutcome(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("erace_resolver_"+randomToken()[:6], "admin")
	for i := range 20 {
		outcome, sess, conceded := "release", x.buyerSess, stateCompleted
		if i%2 == 1 {
			outcome, sess, conceded = "refund", x.vendorSess, stateCancelled
		}
		order, dispute := x.disputedEscrow(regtestAddr("concede-race-" + strconv.Itoa(i)))
		concede := concessionForm(dispute, outcome, randomToken()[:32], 100000)
		resolve := resolveNonceForm(dispute, outcome, randomToken()[:32])
		start := make(chan struct{})
		codes := make(chan int, 2)
		var wg sync.WaitGroup
		for _, call := range []func() int{
			func() int { return x.do("POST", "/disputes/concede", sess, concede).Code },
			func() int { return x.do("POST", "/resolve", admin, resolve).Code },
		} {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; codes <- call() }()
		}
		close(start)
		wg.Wait()
		a, b := <-codes, <-codes
		if !(a == 303 && b == 409 || a == 409 && b == 303) {
			t.Fatalf("race %d (%s): statuses %d, %d", i, outcome, a, b)
		}
		wantSettlements := 0
		if outcome == "refund" {
			wantSettlements = 1
		}
		if got := agInt(x.testEnv, "SELECT count(*) FROM disputes WHERE id=$1 AND outcome=$2 AND status<>'Open'", dispute, outcome); got != 1 {
			t.Fatalf("race %d (%s): %d closed disputes with that outcome", i, outcome, got)
		}
		if got := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state='disputed' AND to_state IN ('resolved',$2)", order, conceded); got != 1 {
			t.Fatalf("race %d (%s): %d moves out of disputed", i, outcome, got)
		}
		if got := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind IN ('release','refund')", order); got != wantSettlements {
			t.Fatalf("race %d (%s): %d settlements, want %d", i, outcome, got, wantSettlements)
		}
	}
}

// A-166 on escrow: the vendor's refund concession commits but its acknowledgement is lost. The answer says the outcome
// is unknown and how to retrieve it; the concession and its refund are on record; and resubmitting the same form returns
// the saved response without a second receipt, settlement, move, audit row or staff notice.
func TestConcessionLostCommitAcknowledgement(t *testing.T) {
	x := newEscrowEnv(t)
	x.user("elost_admin_"+randomToken()[:6], "admin") // an independent staff account the concession tells
	order, dispute := x.disputedEscrow(regtestAddr("concede-lost-ack"))
	staff := agInt(x.testEnv, "SELECT count(*) FROM users WHERE role IN ('moderator','admin') AND suspended_at IS NULL AND id<>$1 AND id<>$2", x.buyer, x.vendor)
	f := concessionForm(dispute, "refund", randomToken()[:32], 100000)
	lost := loseNextActionCommit(t, x.testEnv)
	w := x.do("POST", "/disputes/concede", x.vendorSess, f)
	if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" || !strings.Contains(w.Body.String(), "Concession outcome unknown") ||
		!strings.Contains(w.Body.String(), "resubmit it unchanged") {
		t.Fatalf("lost acknowledgement: lost=%v status=%d retry=%q body=%q", lost.Load(), w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if x.state(order) != stateCancelled {
		t.Fatalf("the concession whose acknowledgement was lost left the order %s", x.state(order))
	}
	if _, kind, state := x.mainSettlement(order); kind != "refund" || state != "building" {
		t.Fatalf("refund after the lost acknowledgement: %s %s", kind, state)
	}
	x.check(x.do("POST", "/disputes/concede", x.vendorSess, f), 303)
	if receipt, settlement, event, audit, notices := concessionEffects(x, order); receipt != 1 || settlement != 1 || event != 1 || audit != 1 || notices != staff || staff == 0 {
		t.Fatalf("after the replay: receipts %d, settlements %d, events %d, audits %d, staff notices %d of %d", receipt, settlement, event, audit, notices, staff)
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
