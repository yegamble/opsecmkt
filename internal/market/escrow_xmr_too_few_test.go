package market

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-437, the Monero probe: a Monero refund whose coins are too few for the network fee and the Monero floor waits with
// the too-small class (it never fails), and a deposit sent after the order closed gets a stray refund of its own, which
// waits too if it is too few on its own, or returns that deposit alone. Either way coins that together could be
// refunded stay in the escrow account for good. Simulated wallet-rpc and monerod (escrow_xmr_fake_test.go).

// xmrTooFewNote is the order note on n deposits worth sum piconero that a Monero refund could not send because they
// were too few (A-437); decided says whether the refund was a dispute decision's.
func xmrTooFewNote(n int, sum int64, decided bool) string {
	refund := "its refund"
	if decided {
		refund = "the refund its dispute decision requested"
	}
	return strconv.Itoa(n) + " deposit(s) (" + amount(sum, 12) + " XMR, TESTNET fakechain) to this order's Monero escrow are not spent by " + refund + ": the coins were too few to pay the network fee and an output of at least 0.001 XMR, so the market could not send the refund, which ends. " +
		"They stay in the escrow account; a stray refund returns them, together with any later deposit to this escrow, less the network fee, to the buyer's refund address given at checkout once together they can pay the network fee."
}

// A-437 (the Monero probe, reproduced on 44c7ab4): the refund that waits too-small ends instead (failed, too-small),
// its deposits are left out (flagged; one note, the parties and the staff told), and the one stray refund, which waits
// rather than fails, returns them with a later deposit once together they pay the fee.
//   - Cancelled at the end of its payment window with 0.0005 XMR, below the floor before any fee (the plan refuses it);
//     a deposit of 0.003 XMR after the close, which could be returned alone.
//   - A dispute decided for the buyer on a 0.002 XMR order: the wallet's transaction leaves less than the floor after
//     its fee (the build's check refuses it); a deposit of 0.002 XMR the wallet read as conflicted when the dispute was
//     decided, too few on its own, is mined since: it is left out (A-424, as on a cancelled order) and joins them.
func TestXMREscrowTooSmallRefundReturned(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.user("xmod_"+randomToken()[:6], "moderator")
	x.mustPoll()
	refundOf := func(order string) string {
		return agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0]
	}
	deposits := func(order string) []string {
		return agStrings(t, x.testEnv, "SELECT amount||' '||flagged||' '||escrow_left_out||' '||after_close||' '||COALESCE(xmr_settlement_id,0) FROM payments WHERE order_id=$1 AND confirmations>=0 ORDER BY id", order)
	}
	notes := func(order string, decided bool, sum int64) int {
		return agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND note=$2", order, xmrTooFewNote(1, sum, decided))
	}
	told := func(uid, order string) int {
		return agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0 AND strpos(body,'not spent by')>0", uid, shortID(order))
	}
	// returned checks order after its refund of tiny piconero ended too few, until more (worth later) comes and the
	// stray refund returns both; waiting checks the page while the stray refund of the coins left out waits.
	returned := func(what, order string, decided bool, tiny, later int64, waiting, more func()) {
		t.Helper()
		buyer := agStrings(t, x.testEnv, "SELECT buyer_id FROM orders WHERE id=$1", order)[0]
		r := x.settlementOf(order, "refund")
		if r.State != "failed" || x.waitClass(r.ID) != xmrTooSmall {
			t.Fatalf("%s: the refund %+v wait %q; settlements %v; events %q", what, r, x.waitClass(r.ID), x.xmrSettlements(order), x.events(order))
		}
		if got := deposits(order); !slices.Equal(got, []string{strconv.FormatInt(tiny, 10) + " true true false 0"}) {
			t.Fatalf("%s: deposits %v", what, got)
		}
		if notes(order, decided, tiny) != 1 || told(buyer, order) != 1 || told(x.vendor, order) != 1 ||
			agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,$1)>0", "Payment review needed for order "+shortID(order)+": "+xmrTooFewNote(1, tiny, decided)) == 0 {
			t.Fatalf("%s: told: notes %d, buyer %d, vendor %d; events %q", what, notes(order, decided, tiny), told(buyer, order), told(x.vendor, order), x.events(order))
		}
		// The stray refund of the coins left out waits: they are too few on their own.
		x.mustPoll()
		x.mustPoll()
		stray := x.settlementOf(order, "stray_refund")
		if stray.State != "requested" || x.waitClass(stray.ID) != xmrTooSmall {
			t.Fatalf("%s: the stray refund of the coins left out: %+v %q", what, stray, x.waitClass(stray.ID))
		}
		waiting()
		more()
		x.mustPoll() // a deposit mined is left out in the pass that reads it, after its settlements are built
		x.mustPoll()
		s := x.settlement(stray.ID)
		if want := []string{"claimant:" + refundOf(order) + ":" + strconv.FormatInt(tiny+later-s.Fee, 10)}; s.State != "relayed" || s.InputTotal != tiny+later || !slices.Equal(x.outputsOf(s.ID), want) {
			t.Fatalf("%s: the stray refund %+v (wait %q) outputs %v, want %v; deposits %v", what, s, x.waitClass(s.ID), x.outputsOf(s.ID), want, deposits(order))
		}
		x.mustPoll()
		if got := x.xmrSettlements(order); len(got) != 2 || notes(order, decided, tiny) != 1 || told(buyer, order) != 1 {
			t.Fatalf("%s: settlements %v, notes %d; events %q", what, got, notes(order, decided, tiny), x.events(order))
		}
	}

	// Cancelled at the end of its payment window, underpaid.
	tiny := int64(minPayoutXMR / 2)
	order, addr := x.freshCheckout(x.digital(x.vendor))
	bsess := x.sessOf(order, roleBuyer)
	x.fake.payAddress(addr, hex64("too few "+order), tiny, 20)
	x.mustPoll()
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", order)
	agExec(x.testEnv, "UPDATE payments SET created=now()-interval '3 days' WHERE order_id=$1", order)
	x.mustPoll()
	if x.state(order) != stateCancelled {
		t.Fatalf("underpaid: %s", x.state(order))
	}
	x.mustPoll()
	const waits = "Cancelled; the market could not send the refund because the coins were too few. The escrowed amount stays in the escrow account, because the coins there are too few to pay the network fee and an output of at least 0.001 XMR, so the market cannot send the stray refund below."
	returned("cancelled", order, false, tiny, 3*minPayoutXMR, func() {
		if panel := x.panel(order, bsess); !strings.Contains(panel, waits) {
			t.Fatalf("cancelled: waiting: panel:\n%s", panel)
		}
	}, func() { x.fake.payAddress(addr, hex64("later "+order), 3*minPayoutXMR, 20) })
	const sent = "Cancelled; the market could not send the refund because the coins were too few. The escrowed amount is being returned, less the network fee, to the buyer's refund address by the stray refund below, which is sent and waits for confirmations."
	if panel := x.panel(order, bsess); !strings.Contains(panel, sent) || strings.Contains(panel, "did not count for the order. It") || strings.Contains(panel, "by a refund the market builds and sends") {
		t.Fatalf("cancelled: panel:\n%s", panel)
	}

	// A dispute decided for the buyer, with a deposit of 0.002 XMR the wallet read as conflicted then.
	p := x.physical(x.vendor)
	agExec(x.testEnv, "UPDATE products SET xmr=2000000000 WHERE id=$1", p)
	order, addr = x.freshCheckout(p)
	bsess = x.sessOf(order, roleBuyer)
	x.fake.payAddress(addr, hex64("decided "+order), 2000000000, 10)
	conflicted := hex64("decided conflicted " + order)
	x.fake.payAddress(addr, conflicted, 2000000000, 0)
	x.fake.setTransfer(conflicted, func(t *fakeXMRTransfer) { t.DoubleSpend = true })
	x.mustPoll()
	if x.state(order) != statePaid {
		t.Fatalf("decided: funded order %s", x.state(order))
	}
	x.disputeAndResolve(order, "refund")
	x.mustPoll()
	const decided = "The dispute was decided for the buyer, but the market could not send the refund because the coins were too few to pay the network fee and an output of at least 0.001 XMR. A stray refund returns them, together with any later deposit, less the network fee, to the buyer's refund address once together they can pay the network fee."
	returned("decided", order, true, 2000000000, 2000000000, func() {
		if panel := x.panel(order, bsess); !strings.Contains(panel, decided) || strings.Contains(panel, "by a refund the market builds and sends") {
			t.Fatalf("decided: panel:\n%s", panel)
		}
	}, func() {
		// Mined after all: it did not count when the dispute was decided, so it is left out (A-424) and joins the stray refund.
		x.fake.setTransfer(conflicted, func(t *fakeXMRTransfer) { t.DoubleSpend, t.Confs, t.Pool = false, 20, false })
	})
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,'did not count for the order when its dispute was decided')>0", order); n != 1 {
		t.Fatalf("decided: %d did-not-count notes; events %q", n, x.events(order))
	}
}

// A-437: a Monero refund whose confirmed coins are too few while a deposit it would spend still confirms keeps waiting
// (as before): once that deposit confirms, the refund itself sends both, and nothing is left out.
func TestXMREscrowTooSmallRefundConfirming(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	p := x.physical(x.vendor)
	agExec(x.testEnv, "UPDATE products SET xmr=2000000000 WHERE id=$1", p)
	order, addr := x.freshCheckout(p)
	x.fake.payAddress(addr, hex64("paid "+order), 2000000000, 10)
	x.mustPoll()
	if x.state(order) != statePaid {
		t.Fatalf("funded order %s", x.state(order))
	}
	topUp := hex64("top up " + order)
	x.fake.payAddress(addr, topUp, 3*minPayoutXMR, 2)
	x.mustPoll()
	vsess := x.sessOf(order, roleVendor)
	cancelled := false
	for _, f := range x.orderForms(order, vsess) {
		if f.action == "/orders/cancel" {
			if w := x.do("POST", f.action, vsess, f.values); w.Code != 303 {
				t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
			}
			cancelled = true
		}
	}
	if !cancelled || x.state(order) != stateCancelled {
		t.Fatalf("cancel: %v %s", cancelled, x.state(order))
	}
	x.mustPoll()
	x.mustPoll()
	r := x.settlementOf(order, "refund")
	if r.State != "requested" || x.waitClass(r.ID) != xmrTooSmall || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out", order) != 0 {
		t.Fatalf("while a deposit confirms: the refund %+v wait %q; settlements %v", r, x.waitClass(r.ID), x.xmrSettlements(order))
	}
	x.fake.setConfs(topUp, 20)
	x.mustPoll()
	if s := x.settlement(r.ID); s.State != "relayed" || s.InputTotal != 5*minPayoutXMR || len(x.xmrSettlements(order)) != 1 {
		t.Fatalf("once it confirmed: the refund %+v; settlements %v", s, x.xmrSettlements(order))
	}
}

// A-437: an administrator's re-request (A-401) of a refund spends exactly the stored key images, which its deposits
// are claimed by, so a re-request the wallet builds too few (here a wallet whose fee leaves the claimant less than the
// floor) waits with the too-small class, as before; it does not end, and nothing is left out.
func TestXMREscrowTooSmallReRequestWaits(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, addr := x.freshCheckout(x.digital(x.vendor))
	x.fake.payAddress(addr, hex64("re-request "+order), 3*minPayoutXMR, 20)
	x.mustPoll()
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", order)
	agExec(x.testEnv, "UPDATE payments SET created=now()-interval '3 days' WHERE order_id=$1", order)
	x.mustPoll()
	if x.state(order) != stateCancelled {
		t.Fatalf("underpaid: %s", x.state(order))
	}
	id := x.settlementOf(order, "refund").ID
	for i := 0; i < 2*xmrRelayAttempts && x.settlement(id).State != "failed"; i++ {
		x.fake.fault("relay_tx", fakeXMRRejected)
		x.mustPoll()
	}
	if s := x.settlement(id); s.State != "failed" || s.TxHash == "" {
		t.Fatalf("refused refund: %+v", s)
	}
	_, adminSess := x.user("xd_admin_"+randomToken()[:6], "admin")
	if code := x.reRequest(adminSess, order, id, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	x.fake.edit(func(f *fakeXMREscrow) {
		f.transferLie = func(res map[string]any) {
			a := res["amounts_by_dest"].(map[string]any)["amounts"].([]int64)
			cut := a[0] - (minPayoutXMR - 1)
			a[0] -= cut
			res["fee"], res["amount"] = res["fee"].(int64)+cut, res["amount"].(int64)-cut
		}
	})
	x.mustPoll()
	x.fake.edit(func(f *fakeXMREscrow) { f.transferLie = nil })
	again := x.settlementOf(order, "refund")
	if again.Supersedes != id || again.State != "requested" || x.waitClass(again.ID) != xmrTooSmall ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out", order) != 0 {
		t.Fatalf("the re-request built too few: %+v wait %q; settlements %v", again, x.waitClass(again.ID), x.xmrSettlements(order))
	}
	x.mustPoll()
	if s := x.settlement(again.ID); s.State != "relayed" {
		t.Fatalf("an honest wallet again: %+v", s)
	}
}
