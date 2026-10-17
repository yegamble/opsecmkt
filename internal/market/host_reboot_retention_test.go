package market

import (
	"testing"
	"time"
)

// A reboot receipt retains its chronology after account erasure, with the
// nullable actor link removed by the reviewed ON DELETE SET NULL lifecycle.
// Its existence must not block this account, or unrelated accounts, from purge.
func TestHostRebootHistoryUnlinksActorDuringAccountPurge(t *testing.T) {
	e := newTestApp(t)
	actor, _ := e.user("reboot_retention_actor", "admin")
	request := randomToken()[:32]
	agExec(e, `INSERT INTO host_reboot_requests(id,user_id,state,requested_at) VALUES($1,$2,'recorded',clock_timestamp())`, request, actor)
	agExec(e, `DELETE FROM sessions WHERE user_id=$1`, actor)
	agExec(e, `UPDATE users SET closed_at=clock_timestamp(),profile_scrubbed_at=clock_timestamp() WHERE id=$1`, actor)
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	needed, err := retentionUserReferenced(t.Context(), tx, actor)
	if err != nil || needed {
		t.Fatalf("historical reboot receipt blocked account purge: needed=%t err=%v", needed, err)
	}
	if err := e.A.applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "account", ID: actor, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err := tx.QueryRowContext(t.Context(), `SELECT user_id IS NULL AND state='recorded' FROM host_reboot_requests WHERE id=$1`, request).Scan(&retained); err != nil || !retained {
		t.Fatalf("account erasure lost reboot chronology or retained the actor: retained=%t err=%v", retained, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1`, actor) != 0 {
		t.Fatal("actor account was not removed")
	}
}
