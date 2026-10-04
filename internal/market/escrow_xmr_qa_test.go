package market

import (
	"html"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Escrow Phase 3, final cross-slice QA (WR-escrow A-248; A-424 to A-426): DB integration tests against the simulated
// wallet-rpc and monerod of escrow_xmr_fake_test.go (simulated RPC, not chain evidence; scripts/test-escrow-xmr-regtest.sh
// has the real-chain legs).

// A-424 (QA's probe through expiry): a deposit the wallet reads as conflicted (double_spend_seen), or no longer lists,
// when the payment window ends does not count, so the order is cancelled unpaid and no refund is requested. When that
// transaction is mined after all, the deposit is left out and flagged, the buyer and the staff are told, and a stray
// refund returns it, less the network fee, to the refund address pinned at checkout.
func TestXMREscrowConflictedDepositThroughExpiry(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	for _, c := range []struct {
		what       string
		lose, back func(addr, txid string)
	}{
		{"double spend seen", func(_, txid string) {
			x.fake.setTransfer(txid, func(t *fakeXMRTransfer) { t.DoubleSpend = true })
		}, func(_, txid string) {
			x.fake.setTransfer(txid, func(t *fakeXMRTransfer) { t.DoubleSpend, t.Confs, t.Pool = false, 20, false })
		}},
		{"no longer listed", func(_, txid string) {
			x.fake.edit(func(f *fakeXMREscrow) {
				for _, w := range f.files {
					w.transfers = slices.DeleteFunc(w.transfers, func(t *fakeXMRTransfer) bool { return t.TxID == txid })
				}
			})
		}, func(addr, txid string) {
			x.fake.payAddress(addr, txid, xmrListingPrice, 20)
		}},
	} {
		order, addr := x.freshCheckout(x.digital(x.vendor))
		buyer := agStrings(t, x.testEnv, "SELECT buyer_id FROM orders WHERE id=$1", order)[0]
		txid := hex64("conflicted " + order)
		x.fake.payAddress(addr, txid, xmrListingPrice, 0)
		x.mustPoll()
		c.lose(addr, txid)
		x.mustPoll()
		agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", order)
		x.mustPoll()
		if x.state(order) != stateCancelled || len(x.xmrSettlements(order)) != 0 {
			t.Fatalf("%s: at the end of the window: %s, settlements %v", c.what, x.state(order), x.xmrSettlements(order))
		}
		c.back(addr, txid)
		x.mustPoll()
		if got := agStrings(t, x.testEnv, "SELECT confirmations||' '||credited||' '||flagged||' '||after_close||' '||escrow_left_out FROM payments WHERE order_id=$1", order); !slices.Equal(got, []string{"20 false true false true"}) ||
			x.notified(buyer, "did not count for the order") != 1 || x.staffFlags(order) == 0 {
			t.Fatalf("%s: mined after the cancellation: deposit %v, buyer notices %d, staff flags %d", c.what, got, x.notified(buyer, "did not count for the order"), x.staffFlags(order))
		}
		x.mustPoll()
		stray := x.settlementOf(order, "stray_refund")
		refund := agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0]
		if want := []string{"claimant:" + refund + ":" + strconv.FormatInt(xmrListingPrice-stray.Fee, 10)}; stray.State != "relayed" || !slices.Equal(x.outputsOf(stray.ID), want) ||
			x.notified(buyer, "Stray refund requested") != 1 {
			t.Fatalf("%s: stray refund %+v outputs %v, want %v", c.what, stray, x.outputsOf(stray.ID), want)
		}
		x.mustPoll()
		if x.notified(buyer, "did not count for the order") != 1 || len(x.xmrSettlements(order)) != 1 {
			t.Fatalf("%s: told again, or another settlement: %v", c.what, x.xmrSettlements(order))
		}
	}

	// A cancelled order whose refund is requested keeps its deposits for that refund: here the wallet refuses the refund
	// address, so the refund stays requested, and the deposit's row changes after the cancellation.
	order, addr := x.freshCheckout(x.physical(x.vendor))
	txid := hex64("paid " + order)
	x.fake.payAddress(addr, txid, xmrListingPrice, 10)
	x.mustPoll()
	refund := agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0]
	x.fake.edit(func(f *fakeXMREscrow) { f.invalid[refund] = true })
	vsess := x.sessOf(order, roleVendor)
	for _, f := range x.orderForms(order, vsess) {
		if f.action == "/orders/cancel" {
			if w := x.do("POST", f.action, vsess, f.values); w.Code != 303 {
				t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
			}
		}
	}
	x.mustPoll()
	x.fake.setConfs(txid, 11)
	x.mustPoll()
	if got := x.xmrSettlements(order); x.state(order) != stateCancelled || !slices.Equal(got, []string{"refund requested " + refund}) ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out", order) != 0 {
		t.Fatalf("a requested refund's deposit: %s, settlements %v, deposits %v", x.state(order), got,
			agStrings(t, x.testEnv, "SELECT confirmations||' '||escrow_left_out FROM payments WHERE order_id=$1", order))
	}
}

// A-426: monero-wallet-rpc answers -4 for any failure of commit_tx, a lost connection to its daemon included, so a
// refusal of a stored transaction that the daemon does not have and whose coins nobody spent is relayed again, with
// the same stored metadata, on the next passes; only xmrRelayAttempts refusals in a row record it failed, flagged once.
// transfer is never called again. A stored key image spent by another transaction is external at once.
func TestXMREscrowRelayRefusalRetried(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	relayed := func() []string {
		var metas []string
		for _, c := range x.fake.callLog() {
			if c.Method == "relay_tx" {
				metas = append(metas, c.Params)
			}
		}
		return metas
	}

	// Refused once, then relayed on the next pass, and confirmed.
	order, _, _ := x.completed()
	id, _ := x.release(order)
	x.fake.fault("relay_tx", fakeXMRRejected)
	x.mustPoll()
	if s := x.settlement(id); s.State != "built" || x.waitClass(id) != xmrRelayRefusing+" 1" || x.staffFlags(order) != 0 {
		t.Fatalf("refused once: %s %q, staff flags %d", s.State, x.waitClass(id), x.staffFlags(order))
	}
	if p := html.UnescapeString(x.panel(order, x.sessOf(order, roleBuyer))); !strings.Contains(p, xmrSettleWaitText(xmrRelayRefusing+" 1", "release", 10)) {
		t.Fatalf("the order page does not say the relay is tried again:\n%s", p)
	}
	x.mustPoll()
	if s := x.settlement(id); s.State != "relayed" || x.waitClass(id) != "" || x.fake.countOf("transfer") != 1 || len(relayed()) != 2 || relayed()[0] != relayed()[1] {
		t.Fatalf("relayed on the next pass: %s %q, transfer %d, relay_tx %v", s.State, x.waitClass(id), x.fake.countOf("transfer"), relayed())
	}
	x.fake.mine(10)
	x.mustPoll()
	if x.settlement(id).State != "confirmed" || x.staffFlags(order) != 0 {
		t.Fatalf("after 10 blocks: %s", x.settlement(id).State)
	}

	// The daemon lost a relayed transaction and refuses it once: still relayed. The count ends when the daemon has it
	// again (here it arrived meanwhile, so nothing is relayed), or when the next relay takes effect.
	order, _, _ = x.completed()
	id, _ = x.release(order)
	x.mustPoll()
	hash := x.settlement(id).TxHash
	for _, back := range []bool{true, false} {
		x.fake.flushPool(hash)
		x.fake.fault("relay_tx", fakeXMRRejected)
		x.mustPoll()
		if x.settlement(id).State != "relayed" || x.waitClass(id) != xmrRelayRefusing+" 1" {
			t.Fatalf("a relayed transaction refused once: %s %q", x.settlement(id).State, x.waitClass(id))
		}
		relays := len(relayed())
		if back {
			x.fake.edit(func(f *fakeXMREscrow) { f.relay(f.built[hexMeta(f, hash)]) })
		}
		x.mustPoll()
		if _, ok := x.fake.tx(hash); x.settlement(id).State != "relayed" || x.waitClass(id) != "" || !ok || (len(relayed()) == relays) != back {
			t.Fatalf("in the daemon again (it arrived itself: %v): %s %q, in the daemon %v, relay_tx %d", back, x.settlement(id).State, x.waitClass(id), ok, len(relayed())-relays)
		}
	}

	// Refused on xmrRelayAttempts passes in a row: failed and flagged once; the same metadata each time, transfer once.
	order, _, _ = x.completed()
	id, _ = x.release(order)
	transfers, relays := x.fake.countOf("transfer"), len(relayed())
	for range xmrRelayAttempts {
		x.fake.fault("relay_tx", fakeXMRRejected)
	}
	for n := 1; n < xmrRelayAttempts; n++ {
		x.mustPoll()
		if s := x.settlement(id); s.State != "built" || x.waitClass(id) != xmrRelayRefusing+" "+strconv.Itoa(n) || x.staffFlags(order) != 0 {
			t.Fatalf("refused %d times: %s %q, staff flags %d", n, s.State, x.waitClass(id), x.staffFlags(order))
		}
	}
	x.mustPoll()
	x.mustPoll()
	metas := relayed()[relays:]
	if s := x.settlement(id); s.State != "failed" || x.waitClass(id) != xmrRelayRefused || x.staffFlags(order) != 1 || x.fake.countOf("transfer") != transfers+1 ||
		len(metas) != xmrRelayAttempts || metas[0] != metas[1] || metas[1] != metas[2] {
		t.Fatalf("refused %d times: %s %q, staff flags %d, transfer %d, relay_tx %d", xmrRelayAttempts, s.State, x.waitClass(id), x.staffFlags(order),
			x.fake.countOf("transfer")-transfers, len(metas))
	}

	// Refused once, then its coins spent by another transaction: external at the next refusal, not counted.
	order, _, _ = x.completed()
	id, _ = x.release(order)
	x.fake.fault("relay_tx", fakeXMRRejected)
	x.mustPoll()
	x.fake.spendOutside(x.settlement(id).KeyImages...)
	x.mustPoll()
	if x.settlement(id).State != "external" || x.staffFlags(order) != 1 {
		t.Fatalf("spent outside after a refusal: %s, staff flags %d", x.settlement(id).State, x.staffFlags(order))
	}
}
