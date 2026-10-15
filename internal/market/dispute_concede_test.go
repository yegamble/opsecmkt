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
