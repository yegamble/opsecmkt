package market

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Signing and the watcher lock the order before its financial rows. Migration
// 113's retention triggers write the order, so compatibility with a foreign-key
// KEY SHARE lock alone is insufficient. Pause the real co-sign at its order note
// while a second signature arrives; also cover a check marker set by maintenance.
// Fake escrow Core: simulated RPC, not chain evidence.
func TestEscrowSignNoDeadlockWithStoreFinal(t *testing.T) {
	for _, checked := range []bool{false, true} {
		name := "unchecked"
		if checked {
			name = "retention_checked"
		}
		t.Run(name, func(t *testing.T) {
			x := newEscrowEnv(t)
			order, id := x.built(regtestAddr("dl-refund"), regtestAddr("dl-claim"), 100000)
			x.signAs(order, roleVendor, id)
			bsess, bfp := x.partySess(roleBuyer)
			paste := walletSign(t, x.partyCopy(order, bsess, id), bfp, 1)
			if checked {
				// Maintenance records checks even while a settlement is pending.
				agExec(x.testEnv, "UPDATE orders SET financial_checked_at=now() WHERE id=$1", order)
			}
			agExec(x.testEnv, `CREATE OR REPLACE FUNCTION test_pause_note() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
				IF NEW.note LIKE 'The market co-signed%' THEN PERFORM pg_advisory_xact_lock(424242); END IF; RETURN NEW; END $$`)
			agExec(x.testEnv, "CREATE TRIGGER test_pause_note BEFORE INSERT ON order_events FOR EACH ROW EXECUTE FUNCTION test_pause_note()")
			t.Cleanup(func() {
				x.DB.Exec("DROP TRIGGER IF EXISTS test_pause_note ON order_events")
				x.DB.Exec("DROP FUNCTION IF EXISTS test_pause_note()")
			})
			conn, err := x.DB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err = conn.ExecContext(context.Background(), "SELECT pg_advisory_lock(424242)"); err != nil {
				t.Fatal(err)
			}
			pollErr := make(chan error, 1)
			go func() { pollErr <- x.A.escrowPollOnce(context.Background()) }()
			waiters := func() int {
				return agInt(x.testEnv, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'")
			}
			if !waitUntil(t, 5*time.Second, func() bool { return waiters() >= 1 }) {
				t.Fatal("storeFinal did not reach the paused order note")
			}
			signed := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				signed <- x.do("POST", "/escrow/sign", bsess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {paste}})
			}()
			if !waitUntil(t, 5*time.Second, func() bool { return waiters() >= 2 }) {
				t.Error("the sign did not queue behind the watcher")
			}
			if _, err = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(424242)"); err != nil {
				t.Fatal(err)
			}
			w := <-signed
			perr := <-pollErr
			if w.Code == 500 || perr != nil && strings.Contains(perr.Error(), "40P01") {
				t.Fatalf("deadlock between storeFinal and the sign action: sign %d %.160s; watcher pass %v", w.Code, w.Body.String(), perr)
			}
			if r := x.settleByID(id); r.Cosigned != "market" {
				t.Fatalf("the watcher's co-sign did not complete: settlement %s cosigned %q", r.State, r.Cosigned)
			}
		})
	}
}

// A blocked watcher must leave the child row available to the transaction that
// owns the order. Otherwise the retention trigger inverts their locks and a
// normal order action can deadlock. Check real settlement, bump and ledger
// writers with non-NULL retention markers, then verify invalidation still runs.
func TestEscrowWatcherLocksOrderBeforeFinancialRows(t *testing.T) {
	for _, kind := range []string{"settlement", "fee_bump", "payment"} {
		t.Run(kind, func(t *testing.T) {
			x := newEscrowEnv(t)
			order, id := x.broadcastClaim("order-lock-"+kind, 100000)
			var child string
			var childID int64
			var mutate func(context.Context) error
			switch kind {
			case "settlement":
				child, childID = "settlements", id
				mutate = func(ctx context.Context) error { return x.A.settlementSendWaits(ctx, id, order, sendRejected) }
			case "fee_bump":
				b := x.mustBump(order, x.vendorSess, id, "5")
				child, childID = "settlement_bumps", b.ID
				mutate = func(ctx context.Context) error { return x.A.bumpSendWaits(ctx, b.ID, order, sendRejected) }
			case "payment":
				var in Incoming
				if err := x.DB.QueryRow("SELECT id,address,txid,idx,amount,confirmations,locked FROM payments WHERE settlement_id=$1 LIMIT 1", id).Scan(&childID, &in.Address, &in.TxID, &in.Index, &in.Amount, &in.Confirmations, &in.Locked); err != nil {
					t.Fatal(err)
				}
				child = "payments"
				in.Confirmations++
				mutate = func(ctx context.Context) error { _, err := x.A.upsertIncoming(ctx, "BTC", order, in); return err }
			}
			agExec(x.testEnv, "UPDATE orders SET financial_closed_at=now(),financial_checked_at=now(),private_expired_at=now() WHERE id=$1", order)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			tx, err := x.DB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err = tx.ExecContext(ctx, "SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE", order); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- mutate(ctx) }()
			if !waitUntil(t, 5*time.Second, func() bool {
				return agInt(x.testEnv, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'") > 0
			}) {
				tx.Rollback()
				t.Fatal("watcher did not wait for the order lock")
			}
			// child is one of the fixed table names above; NOWAIT turns an inverted
			// lock into an immediate test failure rather than waiting for a deadlock.
			_, err = tx.ExecContext(ctx, "SELECT id FROM "+child+" WHERE id=$1 FOR UPDATE NOWAIT", childID)
			tx.Rollback()
			mutationErr := <-done
			if err != nil {
				t.Fatalf("watcher locked %s before the order: %v", child, err)
			}
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if n := agInt(x.testEnv, "SELECT count(*) FROM orders WHERE id=$1 AND financial_closed_at IS NULL AND financial_checked_at IS NULL AND private_expired_at IS NULL", order); n != 1 {
				t.Fatal("financial work did not invalidate retention closure")
			}
		})
	}
}

// Migration 082's rule for a fee bump found on the network (A-382), by SQL against built bumps: only cosigned
// 'network' from a pre-final state, only while the settlement is broadcast, never 'market', and a seen bump never
// changes afterwards.
func TestEscrowMigration082Rules(t *testing.T) {
	x := newEscrowEnv(t)
	try := func(q string, args ...any) bool {
		_, err := x.DB.Exec(q, args...)
		if err != nil && !strings.Contains(err.Error(), "settlement_bumps") && !strings.Contains(err.Error(), "check") {
			t.Fatalf("unexpected error: %v", err)
		}
		return err == nil
	}
	set := func(bid int64, cos, state string) string {
		return "UPDATE settlement_bumps SET raw_tx='00',cosigned='" + cos + "',state='" + state + "',final_at=now() WHERE id=" + strconv.FormatInt(bid, 10)
	}
	order, id := x.broadcastClaim("m82", 100000)
	awaiting := x.mustBump(order, x.vendorSess, id, "5")
	for _, c := range []struct {
		label string
		q     string
	}{
		{"awaiting_signature to signed, cosigned market", set(awaiting.ID, "market", "signed")},
		{"awaiting_signature to broadcast (skipping signed)", set(awaiting.ID, "network", "broadcast")},
		{"awaiting_signature to signed, cosigned bogus", set(awaiting.ID, "bogus", "signed")},
		{"awaiting_signature to signed, cosigned NULL", "UPDATE settlement_bumps SET raw_tx='00',cosigned=NULL,state='signed' WHERE id=" + strconv.FormatInt(awaiting.ID, 10)},
	} {
		if try(c.q) {
			t.Fatalf("%s: allowed", c.label)
		}
	}
	x.mustBump(order, x.vendorSess, id, "6")
	if x.bumpByID(awaiting.ID).State != "superseded" {
		t.Fatal("the first bump was not superseded by the second")
	}
	if try(set(awaiting.ID, "market", "signed")) {
		t.Fatal("superseded to signed, cosigned market: allowed")
	}
	if try("UPDATE settlement_bumps SET state='party_signed' WHERE id=$1", awaiting.ID) {
		t.Fatal("superseded back to party_signed: allowed")
	}
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE settlements SET state='confirmed',confirmed_at=now() WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(set(awaiting.ID, "network", "signed")); err == nil {
		t.Fatal("seen bump stored while the settlement is confirmed")
	}
	tx.Rollback()
	if !try(set(awaiting.ID, "network", "signed")) {
		t.Fatal("superseded to signed, cosigned network (the seen path) refused")
	}
	if try("UPDATE settlement_bumps SET raw_tx='01' WHERE id=$1", awaiting.ID) {
		t.Fatal("a seen bump's raw_tx changed")
	}
	if try("UPDATE settlement_bumps SET cosigned='market' WHERE id=$1", awaiting.ID) {
		t.Fatal("a seen bump's cosigned changed to market")
	}
}
