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

// Lock order between a paste and the watcher (QA-F2-1). The watcher's storeFinal locks the settlement row and then
// inserts an order note, whose foreign key takes KEY SHARE on the order row; the second party's sign locks the order
// row (partyOrderLocked) and then the settlement row. With FOR UPDATE on the order row the two deadlocked (40P01, a
// 500 to the party or a failed watcher pass); FOR NO KEY UPDATE does not conflict with KEY SHARE. The real
// cosignSettlement and storeFinal run, paused at the order note by a test-only trigger. Fake escrow Core: simulated
// RPC, not chain evidence.
func TestEscrowSignNoDeadlockWithStoreFinal(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.built(regtestAddr("dl-refund"), regtestAddr("dl-claim"), 100000)
	x.signAs(order, roleVendor, id)
	bsess, bfp := x.partySess(roleBuyer)
	paste := walletSign(t, x.partyCopy(order, bsess, id), bfp, 1)
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
	waitUntil(t, 5*time.Second, func() bool { return waiters() >= 2 }) // the sign queues behind the settlement row
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
