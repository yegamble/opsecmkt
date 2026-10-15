package market

import (
	"time"
)

// offlineBuyer leaves a shipped order's release signed by the vendor through the market and by the buyer offline: both
// signatures are returned, and only the vendor's stays stored.
func (x *escrowEnv) offlineBuyer(order string, id int64) (vendor, buyer partySigs) {
	x.t.Helper()
	x.signAs(order, roleVendor, id)
	x.signAs(order, roleBuyer, id)
	vendor, buyer = x.storedSigs(id, roleVendor), x.storedSigs(id, roleBuyer)
	agExec(x.testEnv, "DELETE FROM settlement_signatures WHERE settlement_id=$1 AND party='buyer'", id) // the buyer never pasted
	return vendor, buyer
}

// disputeID is order's dispute.
func (x *escrowEnv) disputeID(order string) string {
	x.t.Helper()
	return physString(x.t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)
}

// waitForLockWait waits until a backend of this database waits for a row lock (a request queued behind tx).
func (x *escrowEnv) waitForLockWait() {
	x.t.Helper()
	if !waitUntil(x.t, 2*time.Second, func() bool {
		return agInt(x.testEnv, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'") > 0
	}) {
		x.t.Fatal("the request did not wait for the order row lock")
	}
}
