package market

import (
	"context"

	"testing"
	"time"
)

func (x *escrowEnv) poll() {
	x.t.Helper()
	if err := x.A.escrowPollOnce(context.Background()); err != nil {
		x.t.Fatalf("escrow watcher pass: %v", err)
	}
}

func (x *escrowEnv) events(order string) []string {
	return agStrings(x.t, x.testEnv, "SELECT from_state||'>'||to_state||' '||coalesce(actor_id,'system')||' '||note FROM order_events WHERE order_id=$1 ORDER BY id", order)
}

func (x *escrowEnv) noPayouts(order string) {
	x.t.Helper()
	if n := agInt(x.testEnv, "SELECT count(*) FROM payouts WHERE order_id=$1", order); n != 0 {
		x.t.Fatalf("%d payouts rows for an escrow order", n)
	}
}

// srvDown makes the escrow env's fake node unreachable (down) or reachable again.
func srvDown(x *escrowEnv, down bool) { x.srv.down.Store(down) }

// waitUntil polls cond every 100 ms until it holds or d has passed, and reports whether it held.
func waitUntil(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// pruneEnv is a per-order escrow env with two escrows awaiting payment, each paid its price in the fake node: x.order's
// escrow created 10 days ago and a newer one created now. The watch-only wallet then forgets both (restored from a backup
// older than them), the restore gate and the sweep marker are set as scripts/restore.sh sets them, and the App restarts.
// setup runs on the fake node before the restart.
func pruneEnv(t *testing.T, setup func(c *fakeEscrowCore)) (x *escrowEnv, newer string, created map[string]float64) {
	t.Helper()
	x, _ = newPerOrderEnv(t)
	newer = x.draft()
	created = map[string]float64{}
	for _, o := range []string{x.order, newer} {
		addr := x.mustCheckout(o, regtestAddr("prune-"+o))
		x.core.deposit(addr, txidOf("prune "+o), 0, agInt64(t, x.testEnv, "SELECT amount FROM orders WHERE id=$1", o), 6)
	}
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '10 days' WHERE order_id=$1", x.order)
	for _, o := range []string{x.order, newer} {
		created[o] = float64(agInt64(t, x.testEnv, "SELECT extract(epoch FROM created)::bigint FROM escrows WHERE order_id=$1", o))
	}
	x.setGate(true)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('escrow_sweep_required','true')")
	x.core.forgetWatched()
	x.core.mu.Lock()
	setup(x.core)
	x.core.mu.Unlock()
	x.restart()
	return x, newer, created
}

// unwatchedRows is escrow_watch_unrepaired as "<order> pruned=<bool> import_failed=<bool>", by order.
func (x *escrowEnv) unwatchedRows() []string {
	return agStrings(x.t, x.testEnv, `SELECT es.order_id||' pruned='||u.pruned||' import_failed='||u.import_failed FROM escrow_watch_unrepaired u
		JOIN escrows es ON es.id=u.escrow_id ORDER BY es.order_id`)
}

// importedSince counts the import requests made after the first n by the order whose escrow creation time is their
// timestamp.
func importedSince(t *testing.T, x *escrowEnv, n int, created map[string]float64) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, r := range x.core.importRequests()[n:] {
		for o, c := range created {
			if r["timestamp"] == c {
				out[o]++
			}
		}
	}
	return out
}
