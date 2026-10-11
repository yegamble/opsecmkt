package market

import (
	"net/url"
	"strconv"
	"testing"
)

// TestXMREscrowRegtestRefundBelowFee (A-467; scripts/test-escrow-xmr-regtest.sh, run by the escrow-xmr-regtest CI job): three physical Monero
// escrow orders priced 0.001, 0.0015 and 0.0018 XMR are funded with their price in one real transaction and decided for
// the buyer. The real monero-wallet-rpc refuses each refund (transfer with subtract_fee_from_outputs: -17, -17, -16, as
// the A-463 probe saw); the market reads the fee it would take (get_default_fee_priority, estimate_tx_size_and_weight,
// the daemon's get_fee_estimate) and each refund ends too few: failed with the too-small class, its deposit left out
// with the one too-few note, the stray refund waiting too-small, and no note or notice of an outputs mismatch or a
// refusing wallet. A later deposit of 0.005 XMR to each escrow then lets that one stray refund return both deposits; it
// is relayed, decoded by the daemon (RingCT type 6, ring 16, fee within the cap) and confirmed.
func TestXMREscrowRegtestRefundBelowFee(t *testing.T) {
	r, p := newXMRSettleRegtest(t)
	_, adminSess := r.user("xr_bfadmin_"+randomToken()[:6], "admin")
	type leg struct {
		amount              int64
		code                int
		order, sess, refund string
		addr, txid          string
	}
	legs := []*leg{{amount: 1000000000, code: -17}, {amount: 1500000000, code: -17}, {amount: 1800000000, code: -16}}
	pay := map[string]int64{}
	for _, l := range legs {
		product := r.physical(r.vendor)
		agExec(r.testEnv, "UPDATE products SET xmr=$2 WHERE id=$1", product, l.amount)
		l.order, l.sess, l.refund, l.addr = r.physicalReal(product)
		pay[l.addr] = l.amount
	}
	tx := r.pay(pay)
	r.mineTo(tx, 10)
	heights := r.synced()
	r.mustPoll()
	for _, l := range legs {
		if r.state(l.order) != statePaid {
			t.Fatalf("%s XMR at 10 confirmations (%s): %s; deposits %v", amount(l.amount, 12), heights, r.state(l.order), r.depositConfs(l.order))
		}
		l.txid = agStrings(t, r.testEnv, "SELECT txid FROM payments WHERE order_id=$1", l.order)[0]
		if code, body := r.xmrDispute(l.order, l.sess); code != 303 {
			t.Fatalf("dispute: %d %s", code, body)
		}
		dispute := agStrings(t, r.testEnv, "SELECT id FROM disputes WHERE order_id=$1", l.order)[0]
		if code, body := r.xmrPost("/resolve", adminSess, url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"Decided from the evidence both parties sent."},
			"payout_confirmed": {"confirmed"}, "amount_seen": {strconv.FormatInt(l.amount, 10)}, "reply_seen": {"0"}, "password": {testPassword}}); code != 303 {
			t.Fatalf("resolve: %d %s", code, body)
		}
	}
	t.Logf("transaction %s funded orders of 0.001, 0.0015 and 0.0018 XMR, paid at 10 confirmations (%s); each decided for the buyer", tx, heights)
	transfers := p.count("transfer")
	r.mustPoll()
	for _, l := range legs {
		what := amount(l.amount, 12) + " XMR"
		refund := r.settlementOf(l.order, "refund")
		if refund.State != "failed" || r.waitClass(refund.ID) != xmrTooSmall {
			t.Fatalf("%s: the refund %+v wait %q; events %q", what, refund, r.waitClass(refund.ID), r.events(l.order))
		}
		// The real wallet's own answer to the refund's build, asked again the same way (do_not_relay: nothing is sent).
		account := agInt64(t, r.testEnv, "SELECT sl.account_index FROM escrows es JOIN xmr_escrow_slots sl ON sl.id=es.xmr_slot_id WHERE es.order_id=$1", l.order)
		_, refused := r.A.escrowXMR.rpc.buildTransfer(t.Context(), account, []xmrDestination{{l.refund, l.amount}})
		if !xmrRefusedWith(refused, l.code) {
			t.Fatalf("%s: the wallet answers %v, want %d", what, refused, l.code)
		}
		fee, err := r.A.escrowXMR.rpc.estimateFee(t.Context(), 1, 2)
		if err != nil {
			t.Fatal(err)
		}
		if got := agStrings(t, r.testEnv, "SELECT flagged||' '||escrow_left_out||' '||COALESCE(xmr_settlement_id,0) FROM payments WHERE order_id=$1", l.order); len(got) != 1 || got[0] != "true true 0" {
			t.Fatalf("%s: deposits %v", what, got)
		}
		note := xmrTooFewNote(1, l.amount, true, "deposit "+truncate(l.txid, 20)+" ("+amount(l.amount, 12)+" XMR)")
		if n := agInt(r.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2", l.order, note); n != 1 {
			t.Fatalf("%s: %d too-few notes; events %q", what, n, r.events(l.order))
		}
		t.Logf("%s: transfer refused with %s; estimated fee %s XMR (the coins less it: %d piconero, below the 0.001 XMR floor); refund failed too-small, deposit %s left out and flagged with one note", what, xmrErrorClass(refused), amount(fee, 12), l.amount-fee, l.txid)
	}
	if p.count("transfer") < transfers+3 || p.count("get_default_fee_priority") < 3 || p.count("estimate_tx_size_and_weight") < 3 {
		t.Fatalf("escrow wallet-rpc calls: transfer %d (before %d), get_default_fee_priority %d, estimate_tx_size_and_weight %d", p.count("transfer"), transfers,
			p.count("get_default_fee_priority"), p.count("estimate_tx_size_and_weight"))
	}
	r.mustPoll()
	r.mustPoll()
	later := map[string]int64{}
	for _, l := range legs {
		what := amount(l.amount, 12) + " XMR"
		stray := r.settlementOf(l.order, "stray_refund")
		if stray.State != "requested" || r.waitClass(stray.ID) != xmrTooSmall {
			t.Fatalf("%s: the stray refund %+v wait %q; events %q", what, stray, r.waitClass(stray.ID), r.events(l.order))
		}
		r.noFalseClass(what, l.order)
		later[l.addr] = 5 * minPayoutXMR
	}
	t.Log("each stray refund of the coins left out waits too-small; no outputs-mismatch or refusing-wallet note or notice")
	tx = r.pay(later)
	r.mineTo(tx, 10)
	r.synced()
	r.mustPoll()
	r.mustPoll()
	for _, l := range legs {
		what := amount(l.amount, 12) + " XMR"
		s := r.settlementOf(l.order, "stray_refund")
		if s.State != "relayed" || s.InputTotal != l.amount+5*minPayoutXMR || len(s.KeyImages) != 2 || len(r.xmrSettlements(l.order)) != 2 {
			t.Fatalf("%s: the stray refund %+v wait %q; settlements %v; deposits %v", what, s, r.waitClass(s.ID), r.xmrSettlements(l.order), r.depositConfs(l.order))
		}
		if out := r.outputsOf(s.ID); len(out) != 1 || out[0] != "claimant:"+l.refund+":"+strconv.FormatInt(l.amount+5*minPayoutXMR-s.Fee, 10) {
			t.Fatalf("%s: the stray refund's outputs %v", what, out)
		}
		r.xmrShape(s)
		r.noFalseClass(what, l.order)
		t.Logf("%s: stray refund %s returned both deposits (%s XMR less the fee) to the refund address", what, s.TxHash, amount(s.InputTotal, 12))
	}
	r.mine(10 + xmrMineMargin)
	r.mustPoll()
	for _, l := range legs {
		if s := r.settlementOf(l.order, "stray_refund"); s.State != "confirmed" {
			t.Fatalf("%s XMR: the stray refund after 10 blocks: %s", amount(l.amount, 12), s.State)
		}
	}
	t.Log("all three stray refunds confirmed")
}
