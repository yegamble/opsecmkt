package market

import (
	"strconv"
	"testing"
)

// Escrow Phase 3, slice X-d (WR-escrow A-401, A-415): migration 085, by SQL. xmr_settlements.send_error changes on a
// settlement whose stored transaction stays frozen; xmr_settlement_rerequests records a re-request only for a failed
// settlement with a stored transaction, at most one open per settlement, moving only from requested to refused or done
// (done only by the settlement that supersedes it), and is never deleted.
func TestXMREscrowMigration085(t *testing.T) {
	e := newTestApp(t)
	buyer, _ := e.user("x85_buyer", "buyer")
	vendor, _ := e.user("x85_vendor", "vendor")
	admin, _ := e.user("x85_admin", "admin")
	w := xmrWallet(t, e, 1, "active")
	order, _, _ := xmrEscrow(t, e, w, 1, buyer, e.product(vendor, "digital"), stateCompleted)
	var id int64
	if err := e.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'release',$2) RETURNING id", order, xmrAddr("claim-85")).Scan(&id); err != nil {
		t.Fatal(err)
	}
	sid := strconv.FormatInt(id, 10)
	reRequest := "INSERT INTO xmr_settlement_rerequests(settlement_id,requested_by) VALUES($1,$2)"
	xmrRefused(t, e, "a re-request of a requested settlement", "for a failed settlement with a stored transaction", reRequest, id, admin)

	// send_error is writable on a built settlement; its stored transaction stays frozen.
	agExec(e, "UPDATE xmr_settlements SET state='built',tx_hash=$2,tx_metadata='ab01',tx_blob='cd02',fee=30000000,input_total=100000,key_images=ARRAY[$3] WHERE id=$1",
		id, txidOf("x85"), txidOf("x85-ki"))
	agExec(e, "UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", id, restoredSettlementHold)
	if got := agStrings(t, e, "SELECT send_error FROM xmr_settlements WHERE id=$1", id); got[0] != restoredSettlementHold {
		t.Fatalf("send_error on a built settlement: %v", got)
	}
	xmrRefused(t, e, "a stored transaction changed with send_error", "never rebuilt or changed", "UPDATE xmr_settlements SET send_error='',fee=1 WHERE id=$1", id)
	xmrRefused(t, e, "a re-request of a built settlement", "for a failed settlement with a stored transaction", reRequest, id, admin)

	agExec(e, "UPDATE xmr_settlements SET state='failed',send_error='relay-refused' WHERE id=$1", id)
	xmrRefused(t, e, "a re-request inserted refused", "for a failed settlement", "INSERT INTO xmr_settlement_rerequests(settlement_id,requested_by,state,reason,decided_at) VALUES($1,$2,'refused','in-pool',now())", id, admin)
	var first int64
	if err := e.DB.QueryRow(reRequest+" RETURNING id", id, admin).Scan(&first); err != nil {
		t.Fatal(err)
	}
	f := strconv.FormatInt(first, 10)
	xmrRefused(t, e, "a second open re-request", "xmr_settlement_rerequests_one_open", reRequest, id, admin)
	xmrRefused(t, e, "refused without a reason", "xmr_settlement_rerequests_reason", "UPDATE xmr_settlement_rerequests SET state='refused',decided_at=now() WHERE id="+f)
	xmrRefused(t, e, "refused with node text", "xmr_settlement_rerequests_reason_check", "UPDATE xmr_settlement_rerequests SET state='refused',reason='daemon said no',decided_at=now() WHERE id="+f)
	xmrRefused(t, e, "decided with no time", "xmr_settlement_rerequests_decided", "UPDATE xmr_settlement_rerequests SET state='refused',reason='in-pool' WHERE id="+f)
	xmrRefused(t, e, "done with no new settlement", "only by the settlement that supersedes", "UPDATE xmr_settlement_rerequests SET state='done',decided_at=now() WHERE id="+f)
	xmrRefused(t, e, "refused naming a new settlement", "xmr_settlement_rerequests_done", "UPDATE xmr_settlement_rerequests SET state='refused',reason='in-pool',decided_at=now(),new_settlement_id="+sid+" WHERE id="+f)
	xmrRefused(t, e, "moved to another settlement", "keeps its settlement and requester", "UPDATE xmr_settlement_rerequests SET settlement_id=settlement_id+1,state='refused',reason='in-pool',decided_at=now() WHERE id="+f)
	agExec(e, "UPDATE xmr_settlement_rerequests SET state='refused',reason='on-chain',decided_at=now() WHERE id="+f)
	for what, q := range map[string]string{
		"refused back to requested": "UPDATE xmr_settlement_rerequests SET state='requested',reason='',decided_at=NULL WHERE id=" + f,
		"refused to done":           "UPDATE xmr_settlement_rerequests SET state='done',reason='',new_settlement_id=" + sid + " WHERE id=" + f,
		"a refusal's reason":        "UPDATE xmr_settlement_rerequests SET reason='in-pool' WHERE id=" + f,
	} {
		xmrRefused(t, e, what, "moves only from requested to refused or done", q)
	}
	xmrRefused(t, e, "a re-request deleted", "escrow records are never deleted", "DELETE FROM xmr_settlement_rerequests WHERE id="+f)

	// Asked again after the refusal; done only by the settlement that supersedes it.
	var second int64
	if err := e.DB.QueryRow(reRequest+" RETURNING id", id, admin).Scan(&second); err != nil {
		t.Fatalf("a re-request after a refusal: %v", err)
	}
	s2 := strconv.FormatInt(second, 10)
	var other int64
	o2, _, _ := xmrEscrow(t, e, w, 2, buyer, e.product(vendor, "digital"), stateCancelled)
	if err := e.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'refund',$2) RETURNING id", o2, xmrAddr("refund-85")).Scan(&other); err != nil {
		t.Fatal(err)
	}
	xmrRefused(t, e, "done by an unrelated settlement", "only by the settlement that supersedes", "UPDATE xmr_settlement_rerequests SET state='done',new_settlement_id=$1,decided_at=now() WHERE id="+s2, other)
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var next int64
	if _, err = tx.Exec("UPDATE xmr_settlements SET state='superseded' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination,requested_by,supersedes) VALUES($1,'release',$2,$3,$4) RETURNING id", order, xmrAddr("claim-85"), admin, id).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE xmr_settlement_rerequests SET state='done',new_settlement_id=$1,decided_at=now() WHERE id="+s2, next); err != nil {
		t.Fatalf("done by the superseding settlement: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := agStrings(t, e, "SELECT state||':'||reason||':'||COALESCE(new_settlement_id,0) FROM xmr_settlement_rerequests WHERE settlement_id=$1 ORDER BY id", id); len(got) != 2 ||
		got[0] != "refused:on-chain:0" || got[1] != "done::"+strconv.FormatInt(next, 10) {
		t.Fatalf("re-requests: %v", got)
	}
	xmrRefused(t, e, "a done re-request reopened", "moves only from requested to refused or done", "UPDATE xmr_settlement_rerequests SET state='requested',new_settlement_id=NULL,decided_at=NULL WHERE id="+s2)
}
