package market

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
)

// A-556 follow-ups (PAY66-9, PAY66-10): Monero escrow checkout waits for the watcher's first reconcile of the active
// wallet after each start, and coins at a retired account are reported once. DB integration tests against the
// simulated wallet-rpc of escrow_xmr_fake_test.go (simulated RPC, not chain evidence).

// snapshotDB stands in for a provider's restore point: it closes the App, copies its database and reopens the App on
// the original, returning the copy's DSN (dropped on cleanup).
func (x *xmrEscrowEnv) snapshotDB() string {
	x.t.Helper()
	if testTemplate.admin == nil {
		x.t.Skip("needs CREATE DATABASE (a database copy stands in for a provider restore point)")
	}
	x.A.Close()
	u, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		x.t.Fatal(err)
	}
	snap := "a556snap_" + randomToken()[:10]
	if _, err = testTemplate.admin.Exec("CREATE DATABASE " + snap + " TEMPLATE " + strings.TrimPrefix(u.Path, "/")); err != nil {
		x.t.Fatal(err)
	}
	x.t.Cleanup(func() { testTemplate.admin.Exec("DROP DATABASE " + snap + " WITH (FORCE)") })
	x.restart()
	return testDatabaseURL(testTemplate.base, snap)
}

// pitr starts the App again on the snapshot without scripts/restore.sh: the database goes back, the wallet keeps
// everything it did since.
func (x *xmrEscrowEnv) pitr(dsn string) {
	x.t.Helper()
	x.A.Close()
	x.t.Setenv("DATABASE_URL", dsn)
	x.restart()
}

// PAY66-9: a database restored to an earlier point without restore.sh, where buyer B was given account 1 after that
// point and the top-up then made account 6. A checkout taken after the start and before the watcher's first pass would
// get account 1 again (the pass then skips it as claimed, and B's payment funds it): checkout is refused with nothing
// changed until the watcher has reconciled the active wallet in this process; that pass retires the restored free
// slots, and the checkout then gets a fresh account that B's payment does not fund.
func TestXMREscrowRestoredCheckoutWaitsForReconcile(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	snap := x.snapshotDB()
	x.mustPoll()
	ob, addrB := x.freshCheckout(x.product)
	if x.slotAccount(ob) != 1 {
		t.Fatalf("B took account %d", x.slotAccount(ob))
	}
	x.mustPoll() // the top-up makes account 6 after the restore point
	x.pitr(snap)
	buyer, sess := x.user("xc_"+randomToken()[:8], "buyer")
	oc := x.order(buyer, x.product, "XMR", stateDraft)
	refund := testXMRAddrOn(x.refundNet(), "refund "+oc, true)
	if p := x.page("/order?id="+oc, sess); !strings.Contains(p, "No Monero escrow account is free right now") || strings.Contains(p, `action="/orders/escrow-xmr"`) {
		t.Fatalf("the order page offered Monero escrow before the watcher's first pass:\n%s", p)
	}
	if w := x.checkout(sess, oc, refund); w.Code != 503 || !strings.Contains(w.Body.String(), "No Monero escrow account is free right now") {
		t.Fatalf("a checkout before the first pass: %d", w.Code)
	}
	if x.state(oc) != stateDraft || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", oc) != 0 || x.slots("escrow-202610-01", "claimed") != 0 {
		t.Fatal("the refused checkout changed something")
	}
	x.mustPoll()
	if w := x.checkout(sess, oc, refund); w.Code != 303 {
		t.Fatalf("a checkout after the first pass: %d %s", w.Code, w.Body.String())
	}
	if a := x.slotAccount(oc); a <= 6 || x.notified(x.admin, rowlessNotice) != 1 {
		t.Fatalf("C took account %d; notices %d", a, x.notified(x.admin, rowlessNotice))
	}
	x.fake.payAddress(addrB, hex64("returning B"), xmrListingPrice, 12)
	x.mustPoll()
	if x.state(oc) != stateAwaitingPayment || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1", oc) != 0 {
		t.Fatalf("B's payment funded C's order (%s)", x.state(oc))
	}
}

// PAY66-9: after an ordinary restart, Monero escrow checkout opens only once the watcher's first pass has reconciled
// the active wallet; a pass that does not read it (the daemon not synced) keeps it closed.
func TestXMREscrowCheckoutWaitsForFirstPass(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	x.restart()
	order := x.draft(x.product)
	refund := testXMRAddrOn(x.refundNet(), "refund "+order, true)
	if w := x.checkout(x.buyerSess, order, refund); w.Code != 503 || x.state(order) != stateDraft {
		t.Fatalf("a checkout before the first pass after a restart: %d %s", w.Code, x.state(order))
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.unsynced = true })
	x.mustPoll()
	if w := x.checkout(x.buyerSess, order, refund); w.Code != 503 || x.state(order) != stateDraft {
		t.Fatalf("a checkout after a pass that skipped the wallet: %d %s", w.Code, x.state(order))
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.unsynced = false })
	x.mustPoll()
	if w := x.checkout(x.buyerSess, order, refund); w.Code != 303 || x.state(order) != stateAwaitingPayment {
		t.Fatalf("a checkout after the first pass: %d %s", w.Code, x.state(order))
	}
	// Monero escrow started again (the watcher's retry after it was unavailable): closed until the next pass.
	if _, _, err := x.A.startEscrowXMR(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := x.draft(x.product)
	if w := x.checkout(x.buyerSess, second, testXMRAddrOn(x.refundNet(), "refund "+second, true)); w.Code != 503 {
		t.Fatalf("a checkout after startEscrowXMR ran again: %d", w.Code)
	}
	// A start under the restore gate: a pass under it does not open checkout; the first pass after it clears does.
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true')")
	x.restart()
	x.mustPoll()
	x.clearGate()
	if w := x.checkout(x.buyerSess, second, testXMRAddrOn(x.refundNet(), "refund "+second, true)); w.Code != 503 {
		t.Fatalf("a checkout after only a pass under the gate: %d", w.Code)
	}
	x.mustPoll()
	if w := x.checkout(x.buyerSess, second, testXMRAddrOn(x.refundNet(), "refund "+second, true)); w.Code != 303 {
		t.Fatalf("a checkout after a pass with the gate clear: %d", w.Code)
	}
}

// PAY66-10: coins at a retired account (one no order claimed: retired with a batch, at a wallet's rotation, or after a
// restore) are reported to the administrators once per account, naming the wallet and the account.
func TestXMREscrowRetiredAccountDepositAlerts(t *testing.T) {
	const name = "escrow-202610-01"
	x := newXMREscrowEnv(t)
	x.mustPoll()
	x.fake.edit(func(f *fakeXMREscrow) {
		w := f.files[name]
		w.accounts = append(w.accounts, testXMRAddr("fake "+name+" account 6", true))
	})
	x.mustPoll() // accounts 1 to 6 retired
	const notice = "Monero escrow wallet " + name + ": account 2, which no order had claimed, has received coins"
	if x.slots(name, "retired") != 6 || x.notified(x.admin, "which no order had claimed") != 0 {
		t.Fatalf("retired %d; an alert before any coins", x.slots(name, "retired"))
	}
	x.fake.pay(name, 2, hex64("stray to retired"), xmrListingPrice, 12)
	x.mustPoll()
	if n := x.notified(x.admin, notice); n != 1 {
		t.Fatalf("%d alerts for coins at a retired account", n)
	}
	x.fake.pay(name, 2, hex64("second stray to retired"), xmrListingPrice, 12)
	x.mustPoll()
	x.mustPoll()
	if n, all := x.notified(x.admin, notice), x.notified(x.admin, "which no order had claimed"); n != 1 || all != 1 {
		t.Fatalf("later passes: %d alerts for account 2, %d in all", n, all)
	}
}
