package market

import (
	"testing"
	"time"
)

func retentionTestTicket(e *testEnv, id, opener, assignee, status string, ageDays int) {
	e.t.Helper()
	agExec(e, `INSERT INTO tickets(id,opener_id,assigned_to,status,created,updated) VALUES($1,NULLIF($2,''),NULLIF($3,''),$4,now()-make_interval(days=>$5),now()-make_interval(days=>$5))`, id, opener, assignee, status, ageDays)
}

func TestRetentionTicketExpiryDrainsBatchesAndKeepsRecentAndOpenRecords(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	opener, _ := e.user("retention_ticket_opener", "buyer")
	staff, _ := e.user("retention_ticket_assignee", "moderator")
	retentionTestTicket(e, "expired_closed_ticket", opener, staff, "closed", 100)
	retentionTestTicket(e, "expired_ignored_ticket", opener, staff, "ignored", 100)
	retentionTestTicket(e, "old_open_ticket", opener, staff, "open", 100)
	retentionTestTicket(e, "recent_closed_ticket", opener, staff, "closed", 89)
	retentionTestTicket(e, "closed_ticket_with_recent_message", opener, staff, "closed", 100)
	agExec(e, `INSERT INTO ticket_messages(id,ticket_id,sender_id,body,created) SELECT 'expired_ticket_message_'||n,'expired_closed_ticket',$1,'old test ciphertext',now()-interval '91 days' FROM generate_series(1,450)n`, opener)
	agExec(e, `INSERT INTO ticket_messages(id,ticket_id,sender_id,body,created) VALUES
	 ('expired_open_ticket_message','old_open_ticket',$1,'old ciphertext',now()-interval '91 days'),
	 ('recent_ticket_message','closed_ticket_with_recent_message',$1,'recent ciphertext',now()-interval '89 days')`, opener)
	agExec(e, `INSERT INTO notifications(id,user_id,body,ticket_id,ticket_message_id) VALUES('retention_ticket_linked_notice',$1,'Ticket update','expired_closed_ticket','expired_ticket_message_1')`, opener)
	ageRetentionEnrollment(t, e.A, time.Now().UTC().Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, time.Now().UTC())
	if agInt(e, `SELECT count(*) FROM ticket_messages`) != 1 || agInt(e, `SELECT count(*) FROM ticket_messages WHERE id='recent_ticket_message'`) != 1 {
		t.Fatal("ticket-message age or bounded-batch drain was incorrect")
	}
	if agInt(e, `SELECT count(*) FROM tickets WHERE id IN('expired_closed_ticket','expired_ignored_ticket')`) != 0 || agInt(e, `SELECT count(*) FROM tickets WHERE id IN('old_open_ticket','recent_closed_ticket','closed_ticket_with_recent_message')`) != 3 {
		t.Fatal("ticket expiry removed open/recent/nonempty metadata or kept expired closed threads")
	}
	if agInt(e, `SELECT count(*) FROM notifications WHERE id='retention_ticket_linked_notice' AND ticket_id IS NULL AND ticket_message_id IS NULL`) != 1 {
		t.Fatal("expired private sources remained linked to the notification")
	}
	if agInt(e, `SELECT COALESCE(sum((counts->>'ticket_message')::integer),0) FROM retention_runs`) != 451 || agInt(e, `SELECT COALESCE(sum((counts->>'ticket')::integer),0) FROM retention_runs`) != 2 {
		t.Fatal("ticket expiry was not recorded in the ordinary retention journal/run counts")
	}
}

func TestRetentionTicketAccountHoldsCoverHistoricalParticipantsAndStaffEvents(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	opener, _ := e.user("retention_ticket_hold_opener", "buyer")
	staff, _ := e.user("retention_ticket_hold_staff", "moderator")
	now := time.Now().UTC()
	var releaseID string
	for _, relation := range []string{"opener", "assignee", "sender", "past_participant", "event_actor"} {
		held, _ := e.user("retention_ticket_held_"+relation, "buyer")
		ticketID := "held_ticket_" + relation
		owner, assignee, sender := opener, staff, opener
		if relation == "opener" {
			owner = held
		} else if relation == "assignee" {
			assignee = held
		} else if relation == "sender" {
			sender = held
		}
		retentionTestTicket(e, ticketID, owner, assignee, "closed", 100)
		agExec(e, `INSERT INTO ticket_messages(id,ticket_id,sender_id,body,created) VALUES($1,$2,$3,'preserved test ciphertext',now()-interval '100 days')`, "held_message_"+relation, ticketID, sender)
		if relation == "past_participant" {
			agExec(e, `INSERT INTO ticket_participants(ticket_id,user_id,ever_assigned) VALUES($1,$2,true)`, ticketID, held)
		} else if relation == "event_actor" {
			agExec(e, `INSERT INTO ticket_events(ticket_id,actor_id,event) VALUES($1,$2,'closed')`, ticketID, held)
		}
		holdID := commitRetentionHold(t, e, retentionHold{Scope: "account", RecordID: held, Authority: "Specific test preservation obligation", Reason: "Relevant ticket evidence", Owner: "Assigned test owner", Readers: "Assigned test handlers", ReleaseCondition: "Written release", BeganAt: now.Add(-60 * 24 * time.Hour), ReviewAt: now.Add(-31 * 24 * time.Hour)})
		if relation == "past_participant" {
			releaseID = holdID
		}
		agExec(e, `UPDATE users SET closed_at=now(),suspended_at=now() WHERE id=$1`, held)
	}
	ageRetentionEnrollment(t, e.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM ticket_messages WHERE id LIKE 'held_message_%'`) != 5 || agInt(e, `SELECT count(*) FROM tickets WHERE id LIKE 'held_ticket_%'`) != 5 || agInt(e, `SELECT count(*) FROM users WHERE handle LIKE 'retention_ticket_held_%'`) != 5 {
		t.Fatal("an account hold lost ticket evidence or its preserved identity after closure/assignment changes")
	}
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	op, err := e.A.releaseRetentionHoldTx(t.Context(), tx, releaseID, "Assigned test owner")
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = e.A.finalizeRetentionOperation(t.Context(), op); err != nil {
		t.Fatal(err)
	}
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM ticket_messages WHERE id='held_message_past_participant'`) != 0 || agInt(e, `SELECT count(*) FROM tickets WHERE id='held_ticket_past_participant'`) != 0 || agInt(e, `SELECT count(*) FROM ticket_messages WHERE id LIKE 'held_message_%'`) != 4 {
		t.Fatal("release restarted the original expiry clock or released unrelated holds")
	}
}

func TestRetentionTicketClosureExpiresFreshSenderAndOpenerBodies(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	active, _ := e.user("retention_ticket_active_opener", "buyer")
	closed, _ := e.user("retention_ticket_closed_identity", "buyer")
	staff, _ := e.user("retention_ticket_active_staff", "moderator")
	for _, id := range []string{"closed_opener_ticket", "closed_sender_ticket", "closed_assignee_ticket"} {
		owner, assignee := active, staff
		if id == "closed_opener_ticket" {
			owner = closed
		} else if id == "closed_assignee_ticket" {
			assignee = closed
		}
		retentionTestTicket(e, id, owner, assignee, "open", 0)
	}
	agExec(e, `INSERT INTO ticket_messages(id,ticket_id,sender_id,body) VALUES
	 ('fresh_closed_opener_body','closed_opener_ticket',$1,'test ciphertext'),
	 ('fresh_closed_sender_body','closed_sender_ticket',$2,'test ciphertext'),
	 ('fresh_active_sender_body','closed_sender_ticket',$1,'test ciphertext'),
	 ('fresh_closed_assignee_body','closed_assignee_ticket',$1,'test ciphertext')`, active, closed)
	agExec(e, `UPDATE users SET closed_at=now(),suspended_at=now() WHERE id=$1`, closed)
	ageRetentionEnrollment(t, e.A, time.Now().UTC().Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, time.Now().UTC())
	if agInt(e, `SELECT count(*) FROM ticket_messages WHERE id IN('fresh_closed_opener_body','fresh_closed_sender_body')`) != 0 || agInt(e, `SELECT count(*) FROM ticket_messages WHERE id IN('fresh_active_sender_body','fresh_closed_assignee_body')`) != 2 {
		t.Fatal("account closure expiry did not distinguish opener/sender from unrelated current assignee")
	}
}

func TestRetentionTicketRestoreReappliesMessageAndThreadTombstones(t *testing.T) {
	e, cfg := uiRetentionEnv(t)
	opener, _ := e.user("retention_ticket_restore_opener", "buyer")
	retentionTestTicket(e, "resurrected_ticket", opener, "", "closed", 100)
	agExec(e, `INSERT INTO ticket_messages(id,ticket_id,sender_id,body,created) VALUES('resurrected_ticket_message','resurrected_ticket',$1,'old test ciphertext',now()-interval '100 days')`, opener)
	ageRetentionEnrollment(t, e.A, time.Now().UTC().Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, time.Now().UTC())
	if agInt(e, `SELECT count(*) FROM tickets WHERE id='resurrected_ticket'`) != 0 {
		t.Fatal("initial ticket expiry did not complete")
	}
	retentionTestTicket(e, "resurrected_ticket", opener, "", "open", 0)
	agExec(e, `INSERT INTO ticket_messages(id,ticket_id,sender_id,body) VALUES('resurrected_ticket_message','resurrected_ticket',$1,'restored test ciphertext')`, opener)
	agExec(e, `INSERT INTO settings(key,value) VALUES('retention_restore_required','true') ON CONFLICT(key) DO UPDATE SET value='true'`)
	if err := e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM ticket_messages WHERE id='resurrected_ticket_message'`) != 0 || agInt(e, `SELECT count(*) FROM tickets WHERE id='resurrected_ticket'`) != 0 {
		t.Fatal("restore replay resurrected a purged ticket or private body")
	}
}

func TestRetentionReviewedTicketReferencesPermitAccountErasure(t *testing.T) {
	e := newTestApp(t)
	uid, _ := e.user("retention_ticket_erase_identity", "buyer")
	retentionTestTicket(e, "nullable_ticket_references", uid, uid, "open", 0)
	agExec(e, `INSERT INTO ticket_messages(id,ticket_id,sender_id,body) VALUES('nullable_ticket_message','nullable_ticket_references',$1,'test ciphertext')`, uid)
	agExec(e, `INSERT INTO ticket_events(ticket_id,actor_id,event,staff_id) VALUES('nullable_ticket_references',$1,'assigned',$1)`, uid)
	agExec(e, `INSERT INTO ticket_participants(ticket_id,user_id,ever_assigned,ever_messaged) VALUES('nullable_ticket_references',$1,true,true)`, uid)
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if unknown, err := retentionUnknownUserReferences(t.Context(), tx); err != nil || unknown {
		t.Fatalf("ticket identity lifecycles were not reviewed: unknown=%v error=%v", unknown, err)
	}
	if err := applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "account_close", ID: uid, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if needed, err := retentionUserReferenced(t.Context(), tx, uid); err != nil || needed {
		t.Fatalf("nullable/cascade ticket references blocked account erasure: referenced=%v error=%v", needed, err)
	}
	if err := applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "account", ID: uid, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1`, uid) != 0 || agInt(e, `SELECT count(*) FROM ticket_participants WHERE user_id=$1`, uid) != 0 || agInt(e, `SELECT count(*) FROM tickets WHERE id='nullable_ticket_references' AND opener_id IS NULL AND assigned_to IS NULL`) != 1 || agInt(e, `SELECT count(*) FROM ticket_messages WHERE id='nullable_ticket_message' AND sender_id IS NULL`) != 1 || agInt(e, `SELECT count(*) FROM ticket_events WHERE ticket_id='nullable_ticket_references' AND actor_id IS NULL AND staff_id IS NULL`) != 1 {
		t.Fatal("erased ticket identity was retained or its safe nullable/cascade lifecycle failed")
	}
}

func TestRetentionTicketHoldReleaseKeepsClosureDeadlineAfterIdentityErasure(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	opener, _ := e.user("retention_ticket_release_opener", "buyer")
	staff, _ := e.user("retention_ticket_release_staff", "moderator")
	sender, _ := e.user("retention_ticket_release_sender", "buyer")
	retentionTestTicket(e, "ticket_held_through_erasure", opener, staff, "closed", 100)
	agExec(e, `INSERT INTO ticket_messages(id,ticket_id,sender_id,body) VALUES('fresh_held_erased_sender','ticket_held_through_erasure',$1,'fresh preserved test ciphertext')`, sender)
	now := time.Now().UTC()
	holdID := commitRetentionHold(t, e, retentionHold{Scope: "account", RecordID: staff, Authority: "Specific test preservation obligation", Reason: "Relevant ticket evidence", Owner: "Assigned test owner", Readers: "Assigned test handlers", ReleaseCondition: "Written release", BeganAt: now.Add(-60 * 24 * time.Hour), ReviewAt: now.Add(-31 * 24 * time.Hour)})
	agExec(e, `DELETE FROM sessions WHERE user_id=$1`, sender)
	agExec(e, `UPDATE users SET closed_at=now(),suspended_at=now() WHERE id=$1`, sender)
	ageRetentionEnrollment(t, e.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1`, sender) != 0 || agInt(e, `SELECT count(*) FROM ticket_messages WHERE id='fresh_held_erased_sender' AND sender_id IS NULL`) != 1 {
		t.Fatal("test did not preserve held content while unlinking the unrelated closed sender")
	}
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	op, err := e.A.releaseRetentionHoldTx(t.Context(), tx, holdID, "Assigned test owner")
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = e.A.finalizeRetentionOperation(t.Context(), op); err != nil {
		t.Fatal(err)
	}
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM ticket_messages WHERE id='fresh_held_erased_sender'`) != 0 || agInt(e, `SELECT count(*) FROM tickets WHERE id='ticket_held_through_erasure'`) != 0 {
		t.Fatal("identity erasure or hold release restarted the already-due closure expiry clock")
	}
}
