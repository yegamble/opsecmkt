package market

import (
	"net/url"
	"testing"
)

// A-270: every administrator release, requeue and move of a payout states its amount with the fee sentence the
// other payout notes carry since A-250 (the network fee is taken from every payout, A-232), in the order history
// and in the move notification. Mark sent records a payout already paid, so its note is unchanged.
func TestAdminPayoutNotesStateTheFee(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-new")
	const fee = " The network fee is taken from it."
	const txid = "ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34"
	for _, c := range []struct {
		name, orderState, kind, state, errText string
		ambiguous                              bool
		op                                     string
		extra                                  url.Values
		note                                   string
	}{
		{"release", stateCompleted, "release", "held", heldReason, false, "release", nil,
			"Administrator released the held TESTNET payout of 0.001 BTC; it is queued for a single send." + fee},
		{"release restored", stateCompleted, "release", "held", restoredHoldPrefix + " held by the restore.", false, "release", url.Values{"not_broadcast": {"confirmed"}},
			"Administrator confirmed the wallet shows no broadcast transaction for the TESTNET payout of 0.001 BTC (held after being restored from backup) and released it; it is queued for a single send." + fee},
		{"release suspended", stateCompleted, "release", "held", suspendedHold, false, "release", url.Values{"address_checked": {"confirmed"}},
			"Administrator checked the payout address and released the held TESTNET payout of 0.001 BTC; it is queued for a single send." + fee},
		{"release restored suspended", stateCompleted, "release", "held", restoredHoldPrefix + " held by the restore. " + suspendedHold, false, "release",
			url.Values{"not_broadcast": {"confirmed"}, "address_checked": {"confirmed"}},
			"Administrator confirmed the wallet shows no broadcast transaction for the TESTNET payout of 0.001 BTC (held after being restored from backup) and checked the payout address (held for an account suspension), and released it; it is queued for a single send." + fee},
		{"release large", stateCancelled, "refund", "held", largeHold, false, "release", url.Values{"large_reviewed": {"confirmed"}},
			"Administrator released the held TESTNET payout of 0.001 BTC; it is queued for a single send." + fee + " It is larger than the order price; an administrator reviewed it before releasing it."},
		{"requeue", stateCompleted, "release", "failed", "The wallet reported a pre-broadcast error; nothing was broadcast. Insufficient funds", false, "requeue", nil,
			"Administrator requeued the TESTNET payout of 0.001 BTC after checking the wallet; it will be sent once more." + fee},
		{"requeue restored", stateCompleted, "release", "failed", restoredHoldPrefix + " marked as possibly sent.", true, "requeue", url.Values{"not_broadcast": {"confirmed"}},
			"Administrator confirmed the wallet shows no broadcast transaction for the TESTNET payout of 0.001 BTC (failed, restored from backup) and requeued it; it will be sent once more." + fee},
		{"requeue unknown", stateCompleted, "release", "failed", "Wallet call failed without a definite answer.", true, "requeue", url.Values{"not_broadcast": {"confirmed"}},
			"Administrator confirmed the wallet shows no broadcast transaction for the TESTNET payout of 0.001 BTC (last send outcome unknown) and requeued it; it will be sent once more." + fee},
		{"repoint", stateCompleted, "release", "held", suspendedHold, false, "repoint", url.Values{"from_address": {"fake-testnet-old"}, "to_address": {"fake-testnet-new"}, "address_checked": {"confirmed"}},
			"Administrator moved the TESTNET payout of 0.001 BTC to the recipient's current payout address after checking it with them; nothing was sent and its status is unchanged." + fee},
		{"sent", stateCompleted, "release", "failed", "Wallet call failed without a definite answer.", true, "sent", url.Values{"txid": {txid}},
			"TESTNET payout of 0.001 BTC recorded as sent by an administrator after wallet reconciliation."},
	} {
		t.Run(c.name, func(t *testing.T) {
			recipient := p.vendor
			if c.kind == "refund" {
				recipient = p.buyer
			}
			order := p.insertPayout(c.orderState, c.kind, recipient.ID, c.state, "fake-testnet-old", c.errText, c.ambiguous, "")
			short := order[:8]
			f := url.Values{"payout_id": {p.payoutID(order)}, "op": {c.op}, "password": {testPassword}}
			for k, v := range c.extra {
				f[k] = v
			}
			if w := p.do("POST", "/admin/payout", p.adminSess, f); w.Code != 303 {
				t.Fatalf("%s: %d %s", c.op, w.Code, w.Body.String())
			}
			if got := p.str("SELECT note FROM order_events WHERE order_id=$1 ORDER BY id DESC LIMIT 1", order); got != c.note {
				t.Fatalf("order note\n got %q\nwant %q", got, c.note)
			}
			switch c.op {
			case "repoint":
				want := "Order " + short + ": an administrator moved your unsent TESTNET payout of 0.001 BTC to your current BTC payout address after checking it with you; nothing was sent and its status is unchanged." + fee + " If you did not confirm this, contact the market staff."
				if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", recipient.ID, want); n != 1 {
					t.Fatalf("move notification %q: %d", want, n)
				}
			case "sent":
				want := "Order " + short + ": " + c.note + " Transaction ID: " + txid
				if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", recipient.ID, want); n != 1 {
					t.Fatalf("sent notification %q: %d", want, n)
				}
			}
		})
	}
}
