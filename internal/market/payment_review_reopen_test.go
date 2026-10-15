package market

import (
	"html"
	"net/url"
	"strings"
	"testing"
)

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
