package market

import (
	"slices"
	"strings"
	"testing"
)

// A-792 (a), the Monero port of A-209's retired Bitcoin TestConcessionLostCommitAcknowledgement: a party's concession
// or an administrator's decision of a dispute on a funded Monero escrow order commits, and the database's
// acknowledgement of that COMMIT is lost (loseNextActionCommit). confirmedTxWith reports commitOutcomeUnknown, so the
// answer is a 500 that says the outcome is unknown and how to retrieve it, with no Retry-After and no "try again"
// promise. The decision, its order move, settlement request, audit row, receipt and notices are on record; resubmitting
// the unchanged form returns the saved response and writes none of them again, and no wallet RPC is called. DB
// integration test against the simulated Monero wallet RPC, not chain evidence.
func TestXMRDisputeLostCommitAcknowledgement(t *testing.T) {
	for _, c := range []struct {
		name, path, outcome, by, to, unknown, audit, redirect string
		settlements                                           []string // the order's settlements' "kind state" once decided
	}{
		{"vendor concedes a refund", "/disputes/concede", "refund", roleVendor, stateCancelled, "Concession outcome unknown",
			"Conceded dispute on order %", "#order-dispute", []string{"refund requested"}},
		{"buyer concedes the release", "/disputes/concede", "release", roleBuyer, stateCompleted, "Concession outcome unknown",
			"Conceded dispute on order %", "#order-dispute", nil},
		{"administrator decides a refund", "/resolve", "refund", "admin", stateResolved, "Resolution outcome unknown",
			"Resolved dispute on order %", "/moderator", []string{"refund requested"}},
		{"administrator decides the release", "/resolve", "release", "admin", stateResolved, "Resolution outcome unknown",
			"Resolved dispute on order %", "/moderator", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			order, buyerSess := x.undeliveredXMR()
			if code, body := x.xmrDispute(order, buyerSess); code != 303 {
				t.Fatalf("open the dispute: %d %s", code, body)
			}
			dispute := agStrings(t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)[0]
			// An independent administrator: the decider of a resolution, and a staff observer told of a concession.
			_, adminSess := x.user("xlost_admin_"+randomToken()[:6], "admin")
			sess := map[string]string{roleBuyer: buyerSess, roleVendor: x.vendorSess, "admin": adminSess}[c.by]
			form := concessionForm(dispute, c.outcome, randomToken()[:32], xmrListingPrice)
			if c.path == "/resolve" {
				form = resolverForm(dispute, c.outcome, xmrListingPrice)
				form.Set(formNonceField, randomToken()[:32])
			}
			audit := strings.Replace(c.audit, "%", shortID(order)+"%", 1)
			effects := func() []int {
				return []int{
					agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path=$1", c.path),
					agInt(x.testEnv, "SELECT count(*) FROM disputes WHERE id=$1 AND outcome=$2 AND status<>'Open'", dispute, c.outcome),
					agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state='disputed' AND to_state=$2", order, c.to),
					agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order),
					agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE action LIKE $1", audit),
					agInt(x.testEnv, "SELECT count(*) FROM notifications"),
				}
			}
			before := effects()
			calls := len(x.fake.callLog())

			lost := loseNextActionCommit(t, x.testEnv)
			w := x.do("POST", c.path, sess, form)
			if lost.Load() {
				t.Fatal("the action's COMMIT did not reach the injected acknowledgement loss")
			}
			body := refusalText(w.Body.String())
			if w.Code != 500 || w.Header().Get("Retry-After") != "" || !strings.Contains(body, c.unknown+": it may have been saved.") ||
				!strings.Contains(body, "resubmit it unchanged to retrieve the saved response") || strings.Contains(strings.ToLower(body), "try again") {
				t.Fatalf("lost acknowledgement: status %d, Retry-After %q, body %q", w.Code, w.Header().Get("Retry-After"), body)
			}
			// The commit landed: everything the decision writes is on record once.
			if x.state(order) != c.to {
				t.Fatalf("the decision whose acknowledgement was lost left the order %s", x.state(order))
			}
			var got []string
			for _, s := range x.xmrSettlements(order) {
				kind, rest, _ := strings.Cut(s, " ")
				state, _, _ := strings.Cut(rest, " ")
				got = append(got, kind+" "+state)
			}
			if strings.Join(got, ",") != strings.Join(c.settlements, ",") {
				t.Fatalf("settlements after the lost acknowledgement: %v, want %v", got, c.settlements)
			}
			committed := effects()
			if committed[0] != before[0]+1 || committed[1] != 1 || committed[2] != 1 || committed[3] != len(c.settlements) || committed[4] != 1 || committed[5] <= before[5] {
				t.Fatalf("committed effects (receipts, closed disputes, moves, settlements, audits, notices): before %v, after %v", before, committed)
			}

			// The unchanged form returns the saved response and writes nothing again.
			replay := x.do("POST", c.path, sess, form)
			if replay.Code != 303 || !strings.Contains(replay.Header().Get("Location"), c.redirect) {
				t.Fatalf("replay: %d Location %q %s", replay.Code, replay.Header().Get("Location"), replay.Body.String())
			}
			if after := effects(); !slices.Equal(after, committed) {
				t.Fatalf("the replay wrote again: committed %v, after %v", committed, after)
			}
			if len(x.fake.callLog()) != calls {
				t.Fatalf("the decision or its replay called the wallet RPC: %v", x.fake.callLog()[calls:])
			}
		})
	}
}
