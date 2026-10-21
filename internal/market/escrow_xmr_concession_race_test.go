package market

import (
	"net/url"
	"strings"
	"sync"
	"testing"
)

// A-564 (QA66-5), A-636: a party's concession and an independent administrator's decision on the same disputed,
// funded Monero escrow order race through the HTTP handlers. Both take the dispute row's compare-and-set
// (status='Open' AND outcome=”) before the order lock, and transition() rechecks the locked order's state, so exactly
// one takes effect: one dispute outcome, one move out of disputed, the winner's settlement only, and one transaction
// sent from the escrow (the refund, or the release once the vendor claims it). The loser is refused 409 with nothing written. Each race holds the dispute row from the
// test until both requests wait for it, alternating which queues first, so they meet at the lock rather than one after
// the other. DB integration
// against the simulated Monero wallet-rpc (escrow_xmr_fake_test.go): simulated RPC, not chain evidence.
func TestXMRConcessionResolveRaceOneOutcome(t *testing.T) {
	for _, c := range []struct {
		party    string // who concedes
		concede  string // the concession's outcome: the buyer releases, the vendor refunds
		decide   string // the administrator's outcome, the opposite one
		conceded string // the order's state after the concession
	}{
		{roleBuyer, "release", "refund", stateCompleted},  // the buyer releases, the administrator refunds
		{roleVendor, "refund", "release", stateCancelled}, // the vendor refunds, the administrator releases
	} {
		t.Run(c.party+" concedes", func(t *testing.T) {
			w := newDisputeOrderWorld(t) // its fixture administrator is made a buyer; w.mod is an independent moderator
			adminID, admin := w.e.user("race_admin_"+randomToken()[:6], "admin")
			wins := map[string]int{}
			for i := range 4 {
				order, dispute := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "The licence key was never delivered to me.")
				escrowAddr := agStr(w.e, "SELECT address FROM escrows WHERE order_id=$1", order)
				seen := resolverSeen(w, order)
				sess := w.buyer
				if c.party == roleVendor {
					sess = w.vendor
				}
				tx, err := w.e.DB.Begin()
				if err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec("SELECT 1 FROM disputes WHERE id=$1 FOR UPDATE", dispute); err != nil {
					t.Fatal(err)
				}
				var concedeCode, resolveCode int
				var concedeBody, resolveBody string
				var wg sync.WaitGroup
				concede := func() {
					defer wg.Done()
					r := w.e.do("POST", "/disputes/concede", sess, payoutConfirmed(url.Values{"id": {dispute}, "outcome": {c.concede}, "password": {testPassword}}, seen))
					concedeCode, concedeBody = r.Code, refusalText(r.Body.String())
				}
				resolve := func() {
					defer wg.Done()
					r := w.e.do("POST", "/resolve", admin, resolverForm(dispute, c.decide, seen))
					resolveCode, resolveBody = r.Code, refusalText(r.Body.String())
				}
				first, second := concede, resolve // alternate which request queues for the dispute row first
				if i%2 == 1 {
					first, second = resolve, concede
				}
				wg.Add(2)
				go first()
				w.x.waitForLockWaits(1)
				go second()
				w.x.waitForLockWaits(2)
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
				wg.Wait()

				// The winner's outcome, and what it alone writes.
				var state, outcome, loserBody string
				var refunds int
				switch {
				case concedeCode == 303 && resolveCode == 409:
					wins["concession"]++
					state, outcome = c.conceded, c.concede
					if c.concede == "refund" {
						refunds = 1
					}
					loserBody = resolveBody
				case resolveCode == 303 && concedeCode == 409:
					wins["resolution"]++
					state, outcome = stateResolved, c.decide
					if c.decide == "refund" {
						refunds = 1
					}
					loserBody = concedeBody
				default:
					t.Fatalf("concession %d %q; resolution %d %q; order %s", concedeCode, concedeBody, resolveCode, resolveBody, w.state(order))
				}
				if !strings.Contains(loserBody, "Open dispute not found") {
					t.Fatalf("the losing request's refusal: %q", loserBody)
				}
				if got := w.state(order); got != state {
					t.Fatalf("order %s, want %s", got, state)
				}
				if n := w.count("SELECT count(*) FROM disputes WHERE id=$1 AND status<>'Open' AND outcome=$2", dispute, outcome); n != 1 {
					t.Fatalf("dispute outcome is not %s", outcome)
				}
				if n := w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state='disputed' AND to_state<>'disputed'", order); n != 1 {
					t.Fatalf("%d moves out of disputed, want 1", n)
				}
				if n := w.count("SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='refund'", order); n != refunds {
					t.Fatalf("%d refund settlements, want %d: %v", n, refunds, w.x.xmrSettlements(order))
				}
				if n := w.count("SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind<>'refund'", order); n != 0 {
					t.Fatalf("a release was requested before the vendor's claim: %v", w.x.xmrSettlements(order))
				}
				if n := w.count("SELECT count(*) FROM payouts WHERE order_id=$1", order); n != 0 {
					t.Fatalf("%d pooled payouts queued", n)
				}
				audits := 0
				if state == stateResolved {
					audits = 1
				}
				if n := w.count("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", adminID, "Resolved dispute on order "+shortID(order)+"%"); n != audits {
					t.Fatalf("%d resolution audit rows, want %d", n, audits)
				}
				w.x.mustPoll()
				if got := spentFrom(w.x.fake, escrowAddr); got != refunds {
					t.Fatalf("the watcher relayed %d transactions from the escrow, want %d: %v", got, refunds, w.x.xmrSettlements(order))
				}
				if got := w.state(order); got != state {
					t.Fatalf("after the watcher's pass the order is %s, want %s", got, state)
				}
				if refunds == 0 {
					// A release outcome pays only once the vendor claims it: one release, sent once, and no refund.
					if code, body := w.x.xmrClaim(order, w.vendor, testXMRAddr("race claim "+order, true), testPassword); code != 303 {
						t.Fatalf("the vendor's claim after a release outcome: %d %s", code, body)
					}
					w.x.mustPoll()
					if got := w.x.xmrSettlements(order); len(got) != 1 || !strings.HasPrefix(got[0], "release relayed ") {
						t.Fatalf("settlements after the claim: %v", got)
					}
					if got := spentFrom(w.x.fake, escrowAddr); got != 1 {
						t.Fatalf("the watcher relayed %d transactions from the escrow after the claim, want 1", got)
					}
				}
			}
			if wins["concession"] == 0 || wins["resolution"] == 0 {
				t.Fatalf("each side should win the races it queued first in: %v", wins)
			}
		})
	}
}

// spentFrom counts the transactions the fake daemon knows (in its pool or mined) that escrow address addr's account
// built: the coins the market sent out of that escrow.
func spentFrom(f *fakeXMREscrow, addr string) int {
	name, account := f.accountOf(addr)
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, tx := range f.txs {
		if tx.Wallet == name && tx.Account == account {
			n++
		}
	}
	return n
}
