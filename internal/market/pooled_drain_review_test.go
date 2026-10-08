package market

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A-487: a pooled deposit that stays below the confirmation threshold has a staff-review exit
// 14 days after the address's normal drain horizon, on both pooled currencies.
func TestPooledDrainStalledDepositReview(t *testing.T) {
	for _, cur := range []string{"BTC", "XMR"} {
		t.Run(cur, func(t *testing.T) {
			p := newPayEnv(t)
			p.move(p.order, stateAwaitingPayment, stateCancelled, p.buyer)
			order, addr, wallet := p.order, p.addr, p.fake
			if cur == "XMR" {
				// The BTC fixture order must not mask the XMR drain result.
				agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '40 days' WHERE id=$1", p.order)
				agExec(p.testEnv, "UPDATE payment_addresses SET created=now()-interval '41 days' WHERE order_id=$1", p.order)
				wallet = newFakeProvider("XMR", 10)
				p.A.payments["XMR"] = wallet
				order = p.testEnv.order(p.buyer.ID, p.productID, "XMR", stateCancelled)
				addr = "fake-testnet-" + order[:8]
				agExec(p.testEnv, "INSERT INTO payment_addresses(order_id,currency,address,provider,created) VALUES($1,'XMR',$2,'fake',now()-interval '50 days')", order, addr)
			} else {
				agExec(p.testEnv, "UPDATE payment_addresses SET created=now()-interval '50 days' WHERE order_id=$1", order)
			}
			// First record the late deposit inside the address horizon.
			agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '23 hours' WHERE id=$1", order)
			txid := "stalled-" + strings.ToLower(cur)
			wallet.Deposit(addr, txid, 0, 50000, 1)
			p.poll()
			paymentID := p.str("SELECT id::text FROM payments WHERE order_id=$1 AND txid=$2", order, txid)
			if got := p.str("SELECT credited::text||','||flagged::text||','||after_close::text FROM payments WHERE id=$1::bigint", paymentID); got != "false,false,true" {
				t.Fatalf("initial late payment: %s", got)
			}
			// Simulate a deposit first seen before the horizon; the database clock cannot be advanced by 15 days.
			agExec(p.testEnv, "UPDATE payments SET created=now()-interval '16 days' WHERE id=$1::bigint", paymentID)
			// Horizon = updated+24h here; one second short of 14 more days still waits.
			agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '15 days'+interval '2 seconds' WHERE id=$1", order)
			p.poll()
			p.poll()
			if n := p.count("SELECT count(*) FROM payments WHERE id=$1::bigint AND flagged", paymentID); n != 0 {
				t.Fatal("pending deposit flagged before the 14-day boundary")
			}
			wantRows(t, "still watched before boundary", listedKinds(t, p.testEnv), "watched_address")
			// Crossing the fixed boundary raises one review; repeated passes do not raise another.
			agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '15 days'-interval '2 seconds' WHERE id=$1", order)
			wallet.SetWallet(errors.New("wallet read unavailable"), nil, false)
			if err := p.A.pollOnce(context.Background()); err == nil {
				t.Fatal("unread wallet pass unexpectedly succeeded")
			}
			if n := p.count("SELECT count(*) FROM payments WHERE id=$1::bigint AND flagged", paymentID); n != 0 {
				t.Fatal("unread wallet pass flagged the stale ledger row")
			}
			wantRows(t, "unread wallet still blocks drain", listedKinds(t, p.testEnv), "watched_address")
			wallet.SetWallet(nil, nil, false)
			before := p.allReviewNotices(order)
			p.poll()
			if n := p.count("SELECT count(*) FROM payments WHERE id=$1::bigint AND flagged AND NOT credited AND disposition=''", paymentID); n != 1 {
				t.Fatalf("stalled payment review rows: %d", n)
			}
			wantRows(t, "staff review replaces watch", listedKinds(t, p.testEnv), "flagged_payment")
			wantRows(t, "review blocker", drainRows(t, p.testEnv, "flagged_payment"), fmt.Sprintf("%s|%s|%s|%s:0", paymentID, order, cur, txid))
			desk := p.page("/moderator", p.adminSess)
			mustContain(t, desk, txid+":0", `name="payment_id" value="`+paymentID+`"`,
				fmt.Sprintf("Deposit still confirming (1 of %d confirmations)", wallet.Confirmations()),
				"verify confirmation in the wallet before paying by hand")
			mustNotContain(t, desk, "Not recorded")
			if p.allReviewNotices(order) <= before {
				t.Fatal("staff were not notified of the stalled payment")
			}
			events, notices := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%still unconfirmed%'", order), p.allReviewNotices(order)
			if events != 1 {
				t.Fatalf("stalled review history events: %d", events)
			}
			p.poll()
			p.poll()
			if got := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%still unconfirmed%'", order); got != events || p.allReviewNotices(order) != notices {
				t.Fatal("repeated passes emitted another review")
			}
			if len(wallet.Sends()) != 0 {
				t.Fatal("stalled deposit was paid automatically")
			}
			if code, body := p.dispose(p.adminSess, paymentID, "no_action", "note", "Reviewed the stalled incoming transfer with the buyer."); code != 303 {
				t.Fatalf("review disposition: %d %s", code, body)
			}
			wantRows(t, "disposed; drain clear", listedKinds(t, p.testEnv))
			p.poll()
			wantRows(t, "repeat after disposition", listedKinds(t, p.testEnv))
			if len(wallet.Sends()) != 0 {
				t.Fatal("disposed stalled deposit was paid automatically")
			}
		})
	}
}

// A pre-030 resolved dispute can have outcome=” and no queued payout. Its confirmed,
// uncredited deposit must be reviewable instead of keeping the address watched forever.
func TestPooledDrainLegacyResolvedEmptyOutcomeReview(t *testing.T) {
	p := newPayEnv(t)
	agExec(p.testEnv, "UPDATE orders SET state='resolved',updated=now()-interval '40 days' WHERE id=$1", p.order)
	agExec(p.testEnv, "UPDATE payment_addresses SET created=now()-interval '41 days' WHERE order_id=$1", p.order)
	agExec(p.testEnv, "INSERT INTO disputes(id,order_id,reason,status,outcome) VALUES($1,$2,'Legacy dispute','resolved','')", randomToken(), p.order)
	// This row predates after_close and is already in the ledger. The wallet still reports it.
	agExec(p.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,after_close) VALUES($1,'BTC','legacy-empty-outcome',0,$2,50000,3,false)", p.order, p.addr)
	p.fake.Deposit(p.addr, "legacy-empty-outcome", 0, 50000, 3)
	p.poll()
	id := p.str("SELECT id::text FROM payments WHERE txid='legacy-empty-outcome'")
	wantRows(t, "legacy review blocks drain", listedKinds(t, p.testEnv), "flagged_payment")
	wantRows(t, "legacy review flag", drainRows(t, p.testEnv, "flagged_payment"), id+"|"+p.order+"|BTC|legacy-empty-outcome:0")
	legacyNote := p.str("SELECT note FROM order_events WHERE order_id=$1 AND note LIKE '%legacy-empty-outcome%' ORDER BY id DESC LIMIT 1", p.order)
	if !strings.Contains(legacyNote, "legacy resolved dispute has no settlement outcome") || strings.Contains(legacyNote, "arrived after this order was closed") {
		t.Fatalf("legacy review note: %q", legacyNote)
	}
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 0 {
		t.Fatal("empty legacy outcome produced an automatic payout")
	}
	p.poll()
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%legacy-empty-outcome%'", p.order); n != 1 {
		t.Fatalf("repeat pass wrote %d review events", n)
	}
	if code, body := p.dispose(p.adminSess, id, "no_action", "note", "Reviewed the legacy deposit against the wallet ledger."); code != 303 {
		t.Fatalf("review disposition: %d %s", code, body)
	}
	wantRows(t, "legacy drain clear", listedKinds(t, p.testEnv))
}

func TestPooledDrainStalledReviewWaitsForFirstSeen(t *testing.T) {
	p := newPayEnv(t)
	p.move(p.order, stateAwaitingPayment, stateCancelled, p.buyer)
	agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '40 days' WHERE id=$1", p.order)
	agExec(p.testEnv, "UPDATE payment_addresses SET created=now()-interval '50 days' WHERE order_id=$1", p.order)
	// The watcher can be reading this old address because the pending row was first seen recently.
	agExec(p.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,after_close,created) VALUES($1,'BTC','recent-first-seen',0,$2,50000,1,true,now()-interval '1 day')", p.order, p.addr)
	p.fake.Deposit(p.addr, "recent-first-seen", 0, 50000, 1)
	p.poll()
	if n := p.count("SELECT count(*) FROM payments WHERE txid='recent-first-seen' AND flagged"); n != 0 {
		t.Fatal("recently seen deposit flagged solely because the address horizon was old")
	}
	wantRows(t, "recent deposit still watched", listedKinds(t, p.testEnv), "watched_address")
	agExec(p.testEnv, "UPDATE payments SET created=now()-interval '14 days'-interval '2 seconds' WHERE txid='recent-first-seen'")
	p.poll()
	wantRows(t, "first-seen boundary opens review", listedKinds(t, p.testEnv), "flagged_payment")
	if n := p.count("SELECT count(*) FROM payments WHERE txid='recent-first-seen' AND flagged AND NOT credited"); n != 1 {
		t.Fatalf("review after first-seen age: %d", n)
	}
}

func TestPooledDrainReviewLeavesCreditedSettlementAlone(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.paidByWatcher(p.order, p.addr, "already-credited")
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	p.poll()
	if state, _, _, _ := p.payout(p.order); state != "sent" {
		t.Fatalf("settlement %s", state)
	}
	agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '16 days' WHERE id=$1", p.order)
	agExec(p.testEnv, "UPDATE payment_addresses SET created=now()-interval '50 days' WHERE order_id=$1", p.order)
	p.poll()
	if got := p.str("SELECT credited::text||','||flagged::text FROM payments WHERE txid='already-credited'"); got != "true,false" {
		t.Fatalf("credited deposit changed: %s", got)
	}
	wantRows(t, "credited settlement drains", listedKinds(t, p.testEnv))
	if len(p.fake.Sends()) != 1 {
		t.Fatalf("credited settlement sent %d times", len(p.fake.Sends()))
	}
}
