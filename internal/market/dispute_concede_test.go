package market

import (
	"net/url"

	"strings"

	"testing"
)

func concessionForm(id, outcome, nonce string, seen int64) url.Values {
	return payoutConfirmed(url.Values{
		"id": {id}, "outcome": {outcome}, "password": {testPassword}, formNonceField: {nonce},
	}, seen)
}

// QA-A770-2: a concession at zero counted writes the dispute once, so it adds as many moderation jobs as one with coins.
func TestConcessionAtZeroAddsNoExtraModerationJob(t *testing.T) {
	for _, c := range []struct{ outcome, party string }{{"release", roleBuyer}, {"refund", roleVendor}} {
		t.Run(c.outcome, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			agExec(x.testEnv, `UPDATE settings SET value='true' WHERE key='moderation_enabled'`)
			added := func(zero bool) int {
				t.Helper()
				order, buyer := x.undeliveredXMR()
				if code, body := x.xmrDispute(order, buyer); code != 303 {
					t.Fatalf("dispute: %d %s", code, body)
				}
				seen := int64(xmrListingPrice)
				if zero {
					x.dropTransfer(hex64("fund " + order))
					x.mustPoll()
					seen = 0
				}
				dispute := agStrings(t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)[0]
				sess := x.sessOf(order, c.party)
				page := x.page(orderPage(order), sess)
				before := agInt(x.testEnv, "SELECT count(*) FROM moderation_jobs")
				x.check(x.do("POST", "/disputes/concede", sess, concessionForm(dispute, c.outcome, renderedFormNonce(t, page, "/disputes/concede"), seen)), 303)
				return agInt(x.testEnv, "SELECT count(*) FROM moderation_jobs") - before
			}
			coins, nothing := added(false), added(true)
			if coins == 0 || nothing != coins {
				t.Fatalf("moderation jobs added: %d with coins, %d at zero", coins, nothing)
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
