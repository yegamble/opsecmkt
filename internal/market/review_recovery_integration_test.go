package market

import (
	"context"
	"strings"
	"testing"
)

func TestReviewRestoreGateBlocksNewPayoutsUntilReconciled(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	agExec(p.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true')")
	// This payout did not exist when the restore held existing rows. Monitoring
	// and order completion must still work without permitting a wallet send.
	order := p.completedWithPayout("tx-after-restore-gate")
	p.poll()
	p.poll()
	if state, _, _, _ := p.payout(order); state != "pending" || len(p.fake.Sends()) != 0 {
		t.Fatalf("new payout bypassed restore gate: state=%s sends=%d", state, len(p.fake.Sends()))
	}
	if !strings.Contains(p.page("/admin", p.adminSess), "Outbound payments paused for restore reconciliation.") {
		t.Fatal("administrator cannot see the restore recovery lock")
	}
	// Reconciliation is an explicit operator action; clearing it permits one send.
	agExec(p.testEnv, "UPDATE settings SET value='false' WHERE key='payments_recovery_required'")
	p.poll()
	p.poll()
	if state, _, _, _ := p.payout(order); state != "sent" || len(p.fake.Sends()) != 1 {
		t.Fatalf("reconciled payout did not send once: state=%s sends=%d", state, len(p.fake.Sends()))
	}
	if strings.Contains(p.page("/admin", p.adminSess), "Outbound payments paused for restore reconciliation.") {
		t.Fatal("administrator still sees the cleared recovery lock")
	}
}

func TestReviewRestoreGateActivatedDuringPayoutPass(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	first := p.completedWithPayout("tx-before-midpass-gate")
	// Avoid a watcher poll, which would send the first payout before this test.
	second, addr := p.newOrder(stateAwaitingPayment)
	p.fake.Deposit(addr, "tx-after-midpass-gate", 0, 100000, 3)
	if err := p.A.recordIncoming(context.Background(), p.fake, []string{addr}, map[string]string{addr: second}); err != nil {
		t.Fatal(err)
	}
	if err := p.A.settleOrder(context.Background(), p.fake, second, true); err != nil {
		t.Fatal(err)
	}
	p.complete(second)
	p.fake.sendHook = func() {
		agExec(p.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true')")
	}
	if err := p.A.sendPayouts(context.Background(), p.fake); err != nil {
		t.Fatal(err)
	}
	p.fake.sendHook = nil
	if state, _, _, _ := p.payout(first); state != "sent" {
		t.Fatalf("already claimed payout: %s", state)
	}
	if state, _, _, _ := p.payout(second); state != "pending" || len(p.fake.Sends()) != 1 {
		t.Fatalf("second claim bypassed newly activated gate: state=%s sends=%d", state, len(p.fake.Sends()))
	}
}
