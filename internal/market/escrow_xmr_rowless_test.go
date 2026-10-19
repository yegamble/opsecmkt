package market

import (
	"slices"
	"testing"
)

// A-556 (PAY66): an account of the active wallet that has no slot row may already have been given to a buyer (a
// database restored to an earlier point without scripts/restore.sh), and so may each slot the records show free. The
// watcher retires every free slot of that wallet with the row-less accounts and tells the administrators once; the
// top-up then creates fresh accounts. DB integration tests against the simulated wallet-rpc of escrow_xmr_fake_test.go
// (simulated RPC, not chain evidence).

const rowlessNotice = "account(s) the market has no record of"

// rowlessAudits counts the watcher's audit rows for wallet name's retirement of row-less accounts.
func (x *xmrEscrowEnv) rowlessAudits(name string) int {
	return agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id IS NULL AND action LIKE 'Monero escrow wallet '||$1||': retired %'", name)
}

// slotAccount is the account index of order's escrow slot.
func (x *xmrEscrowEnv) slotAccount(order string) int {
	return agInt(x.testEnv, "SELECT s.account_index FROM escrows e JOIN xmr_escrow_slots s ON s.id=e.xmr_slot_id WHERE e.order_id=$1", order)
}

// A database restored to an earlier point without restore.sh: the live wallet made account 6 after that point (so it
// has no row), and the restored records show account 1 free although a buyer was given it after that point. Both are
// retired with every other free slot, the administrators are told once (a notification and an audit row), and the
// next checkout uses a freshly created account; the returning buyer's deposit to account 1 funds no order.
func TestXMREscrowRowlessAccountRetiresFreeSlots(t *testing.T) {
	const name = "escrow-202610-01"
	x := newXMREscrowEnv(t)
	x.mustPoll()
	if x.slots(name, "free") != 5 {
		t.Fatalf("free slots %d", x.slots(name, "free"))
	}
	x.fake.edit(func(f *fakeXMREscrow) {
		w := f.files[name]
		w.accounts = append(w.accounts, testXMRAddr("fake "+name+" account 6", true))
	})
	x.mustPoll()
	if got := agStrings(t, x.testEnv, "SELECT account_index||':'||state FROM xmr_escrow_slots WHERE account_index<=6 ORDER BY account_index"); !slices.Equal(got,
		[]string{"1:retired", "2:retired", "3:retired", "4:retired", "5:retired", "6:retired"}) {
		t.Fatalf("after a row-less account: %v", got)
	}
	if x.slots(name, "free") != 5 || agInt(x.testEnv, "SELECT min(account_index) FROM xmr_escrow_slots WHERE state='free'") != 7 {
		t.Fatalf("the top-up made %d free slots", x.slots(name, "free"))
	}
	if x.notified(x.admin, rowlessNotice) != 1 || x.rowlessAudits(name) != 1 {
		t.Fatalf("notices %d, audit rows %d", x.notified(x.admin, rowlessNotice), x.rowlessAudits(name))
	}
	if gate := agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key IN ('payments_recovery_required',$1) AND value='true'", xmrEscrowSweepKey); gate != 0 {
		t.Fatal("the retirement paused payments")
	}
	x.mustPoll()
	if x.notified(x.admin, rowlessNotice) != 1 || x.rowlessAudits(name) != 1 || x.slots(name, "free") != 5 {
		t.Fatalf("a second pass: notices %d, audit rows %d, free %d", x.notified(x.admin, rowlessNotice), x.rowlessAudits(name), x.slots(name, "free"))
	}
	order, _ := x.freshCheckout(x.product)
	if got := x.slotAccount(order); got < 7 {
		t.Fatalf("the checkout after the retirement took account %d", got)
	}
	// The buyer given account 1 after the restore point pays it now.
	x.fake.pay(name, 1, hex64("returning buyer"), xmrListingPrice, 12)
	x.mustPoll()
	if x.state(order) != stateAwaitingPayment || agInt(x.testEnv, "SELECT count(*) FROM payments") != 0 {
		t.Fatalf("a deposit to a retired account funded order %s (%s)", shortID(order), x.state(order))
	}
}

// A create_account that took effect without an answer (a crash or a lost connection) is the ordinary cause of a
// row-less account: it costs the batch of free slots, never a pause, and checkout goes on working.
func TestXMREscrowRowlessAccountNormalCrashBurnsBatch(t *testing.T) {
	const name = "escrow-202610-01"
	x := newXMREscrowEnv(t)
	x.mustPoll()
	claimed, _ := x.freshCheckout(x.product)
	x.fake.fault("create_account", fakeXMRDrop)
	if err := x.poll(); err == nil {
		t.Fatal("a create_account with no answer passed")
	}
	file, _ := x.fake.file(name)
	lost := len(file.accounts) - 1
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE account_index=$1", lost); n != 0 || lost != 6 {
		t.Fatalf("account %d, %d rows", lost, n)
	}
	x.mustPoll()
	got := agStrings(t, x.testEnv, "SELECT account_index||':'||state FROM xmr_escrow_slots WHERE account_index<=6 ORDER BY account_index")
	want := []string{"1:claimed", "2:retired", "3:retired", "4:retired", "5:retired", "6:retired"}
	if !slices.Equal(got, want) {
		t.Fatalf("after the lost answer: %v", got)
	}
	if x.slotAccount(claimed) != 1 || x.state(claimed) != stateAwaitingPayment {
		t.Fatal("the claimed slot's order changed")
	}
	if x.notified(x.admin, rowlessNotice) != 1 || x.notified(x.admin, "holds 1 "+rowlessNotice) != 1 || x.rowlessAudits(name) != 1 {
		t.Fatalf("notices %d, audit rows %d", x.notified(x.admin, rowlessNotice), x.rowlessAudits(name))
	}
	if gate := agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key IN ('payments_recovery_required',$1) AND value='true'", xmrEscrowSweepKey); gate != 0 || x.walletState(name) != "active" {
		t.Fatalf("a lost create_account answer paused payments (wallet %s)", x.walletState(name))
	}
	for range 3 {
		order, addr := x.freshCheckout(x.product)
		if a := x.slotAccount(order); a <= 6 {
			t.Fatalf("a checkout took account %d", a)
		}
		x.fake.payAddress(addr, hex64("crash "+order), xmrListingPrice, 12)
	}
	x.mustPoll()
	if n := agInt(x.testEnv, "SELECT count(*) FROM orders WHERE state=$1", stateCompleted); n != 3 {
		t.Fatalf("%d orders funded after the burnt batch", n)
	}
}

// Normal operation: a wallet whose every account has a row keeps its free slots from pass to pass, and nobody is told.
func TestXMREscrowRecordedAccountsKeepFreeSlots(t *testing.T) {
	const name = "escrow-202610-01"
	x := newXMREscrowEnv(t)
	x.mustPoll()
	free := func() []string {
		return agStrings(t, x.testEnv, "SELECT id::text FROM xmr_escrow_slots WHERE state='free' ORDER BY id")
	}
	before := free()
	x.mustPoll()
	order, _ := x.freshCheckout(x.product)
	x.mustPoll()
	x.mustPoll()
	after := free()
	if len(before) != 5 || len(after) != 5 || !slices.Equal(before[1:], after[:4]) {
		t.Fatalf("free slots %v then %v", before, after)
	}
	if x.slotAccount(order) != 1 || x.slots(name, "retired") != 0 {
		t.Fatalf("the checkout took account %d; retired %d", x.slotAccount(order), x.slots(name, "retired"))
	}
	if x.notified(x.admin, rowlessNotice) != 0 || x.rowlessAudits(name) != 0 {
		t.Fatal("a wallet with only recorded accounts raised the row-less alert")
	}
}
