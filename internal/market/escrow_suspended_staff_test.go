package market

import (
	"testing"
)

// A-118 on escrow (A-493): a suspended staff account cannot sign in, so the escrow watcher's payment-review flag
// (flagOrder), the dispute-opened notice (notifyDisputeStaff) and the concession notice (notifyConcededStaff) skip it
// while every other independent staff account is told; once an administrator restores it, it is told again. Fake escrow
// Core: simulated RPC, not chain evidence.
func TestEscrowFlagNotificationSkipsSuspendedStaff(t *testing.T) {
	x := newEscrowEnv(t)
	s := randomToken()[:6]
	adminID, admin := x.user("esusp_admin_"+s, "admin")
	modHandle := "esusp_mod_" + s
	modID, _ := x.user(modHandle, "moderator")
	otherModID, _ := x.user("esusp_other_mod_"+s, "moderator")
	notices := func(uid, order, prefix string) int {
		return agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND order_id=$2 AND body LIKE $3", uid, order, prefix+" order "+shortID(order)+"%")
	}
	// Each leg's notice reaches the administrator and the active moderator once, and the first moderator want times.
	check := func(leg, order, prefix string, want int) {
		t.Helper()
		if a, m, o := notices(adminID, order, prefix), notices(modID, order, prefix), notices(otherModID, order, prefix); a != 1 || o != 1 || m != want {
			t.Fatalf("%s: administrator %d, moderator %d (want %d), other moderator %d", leg, a, m, want, o)
		}
	}
	// lateFlag completes a funded escrow order and sends its escrow another confirmed deposit, which the watcher flags
	// for staff review (flagLateDeposits -> flagOrder).
	lateFlag := func(label string) string {
		t.Helper()
		order := x.completed(regtestAddr(label+"-refund"), 100000)
		x.core.deposit(x.escrowAddr(order), txidOf(label+"-late"), 0, 30000, 3)
		x.poll()
		if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged AND after_close", order) != 1 {
			t.Fatalf("%s: the late deposit was not flagged", label)
		}
		return order
	}
	concede := func(order, dispute string) {
		t.Helper()
		x.check(x.do("POST", "/disputes/concede", x.buyerSess, concessionForm(dispute, "release", randomToken()[:32], 100000)), 303)
		if x.state(order) != stateCompleted {
			t.Fatalf("conceded order is %s", x.state(order))
		}
	}

	x.check(x.do("POST", "/admin/suspend", admin, suspendForm("suspend", modHandle)), 303)
	flagged := lateFlag("esusp-flag-1")
	check("payment review with the moderator suspended", flagged, "Payment review needed for", 0)
	disputed, dispute := x.disputedEscrow(regtestAddr("esusp-dispute-1"))
	check("dispute opened with the moderator suspended", disputed, "Dispute opened on", 0)
	concede(disputed, dispute)
	check("concession with the moderator suspended", disputed, "Dispute on", 0)
	for _, order := range []string{flagged, disputed} {
		if n := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND order_id=$2", modID, order); n != 0 {
			t.Fatalf("the suspended moderator was sent %d notices about order %s", n, shortID(order))
		}
	}

	x.check(x.do("POST", "/admin/suspend", admin, suspendForm("restore", modHandle)), 303)
	flagged = lateFlag("esusp-flag-2")
	check("payment review after the restore", flagged, "Payment review needed for", 1)
	disputed, dispute = x.disputedEscrow(regtestAddr("esusp-dispute-2"))
	check("dispute opened after the restore", disputed, "Dispute opened on", 1)
	concede(disputed, dispute)
	check("concession after the restore", disputed, "Dispute on", 1)
}
