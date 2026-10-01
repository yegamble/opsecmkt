package market

import (
	"strings"
	"testing"
)

// Reorg re-tracking (P1-d1): a confirmed settlement is followed in the watch-only wallet until its transaction has
// escrowReorgDepth (100) confirmations. Dropped back to 0 confirmations, it returns to broadcast and the stored
// transaction is sent again (not while the restore gate is set); conflicted, it is external; past the depth it is no
// longer followed. Fake escrow Core (simulated RPC, not chain evidence).
func TestEscrowReorgRetracking(t *testing.T) {
	x := newEscrowEnv(t)
	gettx := func(txid string) int {
		x.core.mu.Lock()
		defer x.core.mu.Unlock()
		n := 0
		for _, c := range x.core.calls {
			if c.Method == "gettransaction" && strings.Contains(string(c.Params), `"`+txid+`"`) {
				n++
			}
		}
		return n
	}

	// Back to 0 confirmations: broadcast again, the same stored transaction resent, then confirmed again.
	order, id := x.confirmedSettlement("reorg-a")
	r := x.settleByID(id)
	x.core.setTxConfs(r.TxID, 1, "") // a shallower reorganisation: still in a block, still confirmed
	x.poll()
	if got := x.settleByID(id); got.State != "confirmed" {
		t.Fatalf("at 1 confirmation after confirming: %+v", got)
	}
	sends := x.sendsOf(r.Raw)
	x.core.setTxConfs(r.TxID, 0, "")
	x.poll()
	if got := x.settleByID(id); got.State != "broadcast" || got.Raw != r.Raw {
		t.Fatalf("dropped out of the chain: %+v", got)
	}
	if x.sendsOf(r.Raw) != sends+1 {
		t.Fatalf("the stored transaction was not sent again: %d sends, was %d", x.sendsOf(r.Raw), sends)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE id=$1 AND confirmed_at IS NULL", id) != 1 {
		t.Fatal("confirmed_at kept after the reorganisation")
	}
	ev := strings.Join(x.events(order), "\n")
	if !strings.Contains(ev, "is no longer in the regtest chain: a reorganisation of the chain took it out of the block that confirmed it") || !strings.Contains(ev, "The market sends the stored transaction again") {
		t.Fatalf("order notes:\n%s", ev)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%no longer confirmed%'", x.buyer) != 1 {
		t.Fatal("the buyer was not told")
	}
	if page := x.page("/order?id="+order, x.buyerSess); !strings.Contains(page, "Broadcast to the regtest network") || strings.Contains(page, "Confirmed on the regtest network") {
		t.Fatalf("order page after the reorganisation:\n%s", page)
	}
	x.core.setTxConfs(r.TxID, 3, "")
	x.poll()
	if got := x.settleByID(id); got.State != "confirmed" {
		t.Fatalf("confirmed again: %+v", got)
	}

	// Conflicted after confirming: a conflicting transaction was mined instead; external, flagged for staff.
	_, mod := x.user("reorgmod", "moderator")
	orderB, idB := x.confirmedSettlement("reorg-b")
	b := x.settleByID(idB)
	other := txidOf("the conflicting spend")
	x.core.setTxConfs(b.TxID, -2, other)
	x.poll()
	if got := x.settleByID(idB); got.State != "external" || got.SpentBy != other {
		t.Fatalf("conflicted after confirming: %+v", got)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE settlement_id=$1 AND flagged", idB) != 1 {
		t.Fatal("not flagged")
	}
	if ev := strings.Join(x.events(orderB), "\n"); !strings.Contains(ev, "was confirmed, but a reorganisation of the chain replaced it with a conflicting transaction this market did not make") {
		t.Fatalf("order notes:\n%s", ev)
	}
	if page := x.page("/order?id="+orderB, mod); !strings.Contains(page, "Spent outside the market") || !strings.Contains(page, other) {
		t.Fatalf("staff page:\n%s", page)
	}

	// Past the depth: no longer followed; a later drop changes nothing.
	_, idC := x.confirmedSettlement("reorg-c")
	c := x.settleByID(idC)
	x.core.setTxConfs(c.TxID, escrowReorgDepth, "")
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE id=$1 AND reorg_safe_at IS NOT NULL", idC) != 1 {
		t.Fatal("reorg_safe_at not recorded at the depth")
	}
	n := gettx(c.TxID)
	x.core.setTxConfs(c.TxID, 0, "")
	x.poll()
	x.poll()
	if got := x.settleByID(idC); got.State != "confirmed" || gettx(c.TxID) != n {
		t.Fatalf("followed past the depth: %+v, gettransaction %d -> %d", got, n, gettx(c.TxID))
	}

	// Under the restore gate the reorganisation is followed, but nothing is sent again.
	orderD, idD := x.confirmedSettlement("reorg-d")
	d := x.settleByID(idD)
	x.setGate(true)
	sends = x.sendsOf(d.Raw)
	x.core.setTxConfs(d.TxID, 0, "")
	x.poll()
	x.poll()
	if got := x.settleByID(idD); got.State != "broadcast" || x.sendsOf(d.Raw) != sends {
		t.Fatalf("reorganisation under the gate: %+v, %d sends (was %d)", got, x.sendsOf(d.Raw), sends)
	}
	if ev := strings.Join(x.events(orderD), "\n"); !strings.Contains(ev, "The market does not send it again while it reconciles its records after a restore from a backup") {
		t.Fatalf("order notes under the gate:\n%s", ev)
	}
	x.setGate(false)
	x.poll()
	if x.sendsOf(d.Raw) != sends+1 {
		t.Fatal("not sent again once the gate was cleared")
	}
}

// A-365 (T5): a confirmed settlement whose transaction the watch-only wallet does not know (it missed it, or was
// restored) proves nothing about a reorganisation: it stays confirmed, nothing is sent again and no note is written,
// and it is asked about again on the next pass.
func TestEscrowConfirmedUnknownToWalletNotReorg(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.confirmedSettlement("unknown-to-wallet")
	r := x.settleByID(id)
	notes, sends := len(x.events(order)), x.sendsOf(r.Raw)
	x.core.mu.Lock()
	x.core.walletForgets = true
	x.core.mu.Unlock()
	x.poll()
	x.poll()
	if got := x.settleByID(id); got.State != "confirmed" || x.sendsOf(r.Raw) != sends || len(x.events(order)) != notes {
		t.Fatalf("confirmed, unknown to the wallet: %+v, %d sends (was %d), %d notes (was %d)", got, x.sendsOf(r.Raw), sends, len(x.events(order)), notes)
	}
	if n := x.core.countWith("gettransaction", r.TxID); n < 2 {
		t.Fatalf("not asked again on each pass: %d gettransaction calls", n)
	}
}
