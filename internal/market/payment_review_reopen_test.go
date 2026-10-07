package market

import (
	"html"
	"net/url"
	"strings"
	"testing"
)

// QA462F: every note that flags a deposit names it, so the desk dates the flag by the latest such note; a reviewed flag
// that a new event flags again (newFlagSQL) reopens with no review, a form, and the new note's date. The order history
// and the audit log keep the earlier review.

// latestFlagNote is the latest system note on order that names txid ("… <truncate(txid, 20)> …") and asks for review:
// the note the moderation desk dates txid's flag by (loadPaymentReviews), or "" when there is none.
func latestFlagNote(t *testing.T, e *testEnv, order, txid string) string {
	t.Helper()
	notes := agStrings(t, e, `SELECT note FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND strpos(note,'Moderator review required.')>0
		AND strpos(note,' '||$2||' ')>0 ORDER BY created DESC,id DESC LIMIT 1`, order, truncate(txid, 20))
	if len(notes) == 0 {
		return ""
	}
	return notes[0]
}

// reviewFlag records outcome on the flag of payment pid as the moderator session sess and fails unless it answers 303.
func reviewFlag(t *testing.T, e *testEnv, sess, pid, outcome, note string) {
	t.Helper()
	if w := e.do("POST", "/moderator/payment-review", sess, url.Values{"payment_id": {pid}, "outcome": {outcome}, "note": {note}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("review %s of payment %s: %d %s", outcome, pid, w.Code, strings.TrimSpace(w.Body.String()))
	}
}

// reopened checks that payment pid's flag is open again with no review and dated, with a form for the moderator, and
// that the latest note naming txid (the one dating it) contains phrase; then records it again (303). reviews is the
// number of review events the order history must hold before that (the earlier review is kept).
func reopened(t *testing.T, e *testEnv, page func(path, sess string) string, sess, order, txid, pid, phrase string, reviews int) {
	t.Helper()
	desk := html.UnescapeString(page("/moderator", sess))
	row := flagRow(desk, txid)
	if !flaggedAt.MatchString(row) || !strings.Contains(row, "<strong>Open:</strong>") || strings.Contains(row, "escalated") || !strings.Contains(desk, `name="payment_id" value="`+pid+`"`) {
		t.Fatalf("the flag set anew on the desk: %s\nflag %q", row, agStrings(t, e, "SELECT flagged||' '||disposition FROM payments WHERE id=$1", pid))
	}
	if note := latestFlagNote(t, e, order, txid); !strings.Contains(note, phrase) {
		t.Fatalf("the flag is dated by %q, not by the note of the new event (%q)", note, phrase)
	}
	if n := agInt(e, "SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NOT NULL AND strpos(note,'Payment review of')=1", order); n != reviews {
		t.Fatalf("%d review events in the order history, want %d", n, reviews)
	}
	reviewFlag(t, e, sess, pid, "no_action", "Recorded after the new event.")
}

// QA462F-1 (markOutsideSpend): a credited escrow deposit spent by a transaction the market did not make is dated on the
// desk by its own note, which names it.
func TestEscrowOutsideSpendDeskDate(t *testing.T) {
	x := newEscrowEnv(t)
	_, mod := x.user("odmod_"+randomToken()[:6], "moderator")
	order := x.completed(regtestAddr("outside spend date"), 100000)
	funded := txidOf(order)
	x.coSpend(funded, 0, 1)
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND txid=$2 AND spent_outside AND flagged", order, funded) != 1 {
		t.Fatalf("the deposit spent outside: events %q", x.events(order))
	}
	if row := deskRow(html.UnescapeString(x.page("/moderator", mod)), funded); !flaggedAt.MatchString(row) || !strings.Contains(latestFlagNote(t, x.testEnv, order, funded), "spent by a transaction this market did not make") {
		t.Fatalf("the spent-outside flag on the desk: %s\nevents %q", row, x.events(order))
	}
	// The parties' notice asks nothing of staff.
	if agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role NOT IN ('moderator','admin') AND strpos(n.body,'Moderator review required')>0") != 0 {
		t.Fatal("a party's notice asks for a moderator review")
	}
}

// QA462F-1, QA462F-2 (markOutsideSpend, FM1): a top-up left out of a built release, whose review staff recorded (no
// action, or escalated), is then spent outside the market. It is a new flag: open, unreviewed, dated by the
// spent-outside note, with a form; later passes do not flag it again, and the history and audit keep the first review.
func TestEscrowReviewedLeftOutThenSpentOutside(t *testing.T) {
	for _, first := range []string{"no_action", "escalated"} {
		t.Run(first, func(t *testing.T) {
			x := newEscrowEnv(t)
			_, mod := x.user("rlmod_"+randomToken()[:6], "moderator")
			x.user("rladm_"+randomToken()[:6], "admin")
			order := x.paidPhysical(regtestAddr("reviewed left out "+first), 100000)
			addr := physString(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)
			if code, body := x.shipEscrow(order, x.vendorSess, regtestAddr("reviewed left out claim "+first), testPassword); code != 303 {
				t.Fatalf("ship: %d %s", code, body)
			}
			x.poll()
			top := txidOf("reviewed left out top-up " + first + order)
			x.core.deposit(addr, top, 0, 30000, 3)
			x.poll()
			x.poll()
			pid := agStrings(t, x.testEnv, "SELECT id::text FROM payments WHERE order_id=$1 AND txid=$2 AND escrow_left_out AND flagged", order, top)[0]
			reviewFlag(t, x.testEnv, mod, pid, first, "Recorded before the outside spend.")
			x.coSpend(top, 0, 1)
			x.poll()
			staff := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE strpos(body,$1)=1", "Payment review needed for order "+shortID(order))
			for range 3 {
				x.poll()
			}
			if n := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE strpos(body,$1)=1", "Payment review needed for order "+shortID(order)); n != staff {
				t.Fatalf("later passes told staff again: %d then %d", staff, n)
			}
			reopened(t, x.testEnv, x.page, mod, order, top, pid, "spent by a transaction this market did not make", 1)
			if n := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE strpos(action,'Recorded payment review')=1"); n != 2 {
				t.Fatalf("%d review audit rows, want both", n)
			}
		})
	}
}

// QA462F-1, QA462F-2 (markExternal: building FM2, past building FM3): the stray refund of a deposit after the close
// is still being built, or waits for signatures; staff recorded no action on the deposit's flag; another transaction
// spends the deposit, so the stray refund is external: the deposit's flag reopens, dated by the external note, which
// names it.
func TestEscrowReviewedStrayThenExternal(t *testing.T) {
	for _, leg := range []string{"building", "awaiting_signature"} {
		t.Run(leg, func(t *testing.T) {
			x := newEscrowEnv(t)
			_, mod := x.user("semod_"+randomToken()[:6], "moderator")
			order := x.completed(regtestAddr("reviewed stray external "+leg), 100000)
			addr := agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
			late := txidOf("reviewed stray external late " + leg + order)
			x.core.deposit(addr, late, 0, 30000, 3)
			if leg == "building" {
				x.setGate(true) // builds pause (restore gate): the stray refund stays being built
			}
			x.poll() // recorded after the close and flagged; its stray refund is requested (and built)
			sid, state := x.stray(order)
			if state != leg {
				t.Fatalf("stray refund %d %s; events %q", sid, state, x.events(order))
			}
			pid := agStrings(t, x.testEnv, "SELECT id::text FROM payments WHERE order_id=$1 AND txid=$2 AND flagged", order, late)[0]
			reviewFlag(t, x.testEnv, mod, pid, "no_action", "The stray refund returns it.")
			x.coSpend(late, 0, 1)
			x.poll()
			x.setGate(false)
			x.poll() // a build over a spent input waits one pass first
			x.poll()
			if got := agStrings(t, x.testEnv, "SELECT state FROM settlements WHERE id=$1", sid); got[0] != "external" {
				t.Fatalf("stray refund %s; events %q", got[0], x.events(order))
			}
			phrase := "before any party signed it"
			if leg == "building" {
				phrase = "so the market cannot build it"
			}
			reopened(t, x.testEnv, x.page, mod, order, late, pid, phrase, 1)
		})
	}
}

// QA462F-1, QA462F-2 (markXMRExternal FM4, failXMRSettlement FM5): the stray refund of a Monero deposit after the close
// is built, not relayed; staff recorded no action on the deposit's flag; then the stray refund is spent outside the
// market (external), or the daemon refuses it for good (failed). The deposit's flag reopens, dated by the new note.
func TestXMRReviewedStrayThenExternalOrFailed(t *testing.T) {
	for _, leg := range []string{"external", "failed"} {
		t.Run(leg, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			_, mod := x.user("xsmod_"+randomToken()[:6], "moderator")
			order, addr, _ := x.completed()
			late := hex64("reviewed xmr stray " + leg + " " + order)
			x.fake.payAddress(addr, late, 7000000000, 12)
			// The pass records it after the close, flags it and builds its stray refund, whose relay is refused.
			fault := fakeXMRNotOpen
			if leg == "failed" {
				fault = fakeXMRRejected
			}
			x.fake.fault("relay_tx", fault)
			x.poll()
			if s := x.settlementOf(order, "stray_refund"); s.State != "built" {
				t.Fatalf("stray refund %+v; events %q", s, x.events(order))
			}
			pid := agStrings(t, x.testEnv, "SELECT id::text FROM payments WHERE order_id=$1 AND txid=$2 AND flagged", order, late)[0]
			reviewFlag(t, x.testEnv, mod, pid, "no_action", "The stray refund returns it.")
			phrase := "were spent by a transaction this market did not make"
			if leg == "external" {
				x.fake.spendOutside(x.settlementOf(order, "stray_refund").KeyImages...)
				x.mustPoll()
			} else {
				phrase = "failed:"
				for range xmrRelayAttempts - 1 {
					x.fake.fault("relay_tx", fakeXMRRejected)
					x.mustPoll()
				}
			}
			if s := x.settlementOf(order, "stray_refund"); s.State != leg {
				t.Fatalf("stray refund %+v; events %q", s, x.events(order))
			}
			reopened(t, x.testEnv, x.page, mod, order, late, pid, phrase, 1)
		})
	}
}

// QA462F-2 (flagBelowFloor, FM6): a pooled order's credited deposit regresses (flagged, staff record no action) and
// confirms again; the order then completes with a release below the minimum automatic payout, which flags the same
// deposit below the floor: a new flag, open with no review and a form.
func TestReviewedRegressionThenBelowFloor(t *testing.T) {
	p := newPayEnv(t)
	_, mod := p.user("bfmod_"+randomToken()[:6], "moderator")
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	if _, err := p.DB.Exec("UPDATE orders SET amount=5000 WHERE id=$1", p.order); err != nil {
		t.Fatal(err)
	}
	tx := longTxid("f7")
	p.fake.Deposit(p.addr, tx, 0, 5000, 3)
	p.poll()
	if s := p.state(p.order); s != statePaid {
		t.Fatalf("state %s", s)
	}
	p.fake.SetConfirmations(tx, -1)
	p.poll()
	pid := p.flagPaymentID(p.order, tx)
	if n := p.count("SELECT count(*) FROM payments WHERE id=$1::bigint AND flagged AND regress_notice", pid); n != 1 {
		t.Fatal("the regression was not flagged")
	}
	reviewFlag(t, p.testEnv, mod, pid, "no_action", "Watching the reorganisation.")
	p.fake.SetConfirmations(tx, 3)
	p.poll()
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	p.poll()
	p.poll()
	if n := p.count("SELECT count(*) FROM payments WHERE id=$1::bigint AND flagged AND below_floor AND disposition=''", pid); n != 1 {
		t.Fatalf("below the floor: %q", p.str("SELECT flagged||' '||below_floor||' '||disposition FROM payments WHERE id=$1::bigint", pid))
	}
	desk := html.UnescapeString(p.page("/moderator", mod))
	if row := flagRow(desk, tx); !strings.Contains(row, "<strong>Open:</strong> Below the minimum automatic payout") || !strings.Contains(desk, `name="payment_id" value="`+pid+`"`) {
		t.Fatalf("the below-floor flag on the desk: %s", row)
	}
	reviewFlag(t, p.testEnv, mod, pid, "no_action", "Dust kept for now.")
}
