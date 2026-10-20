package market

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func xmppLinkedTicketNotice(e *testEnv, uid, ticketID, messageID, audience string) string {
	e.t.Helper()
	notice := randomToken()
	agExec(e, `INSERT INTO notifications(id,user_id,body,audience,ticket_id,ticket_message_id) VALUES($1,$2,'Support ticket update',$3,$4,NULLIF($5,''))`, notice, uid, audience, ticketID, messageID)
	return notice
}

func TestXMPPContentsTicketRequiresCurrentParticipantAndCurrentTicketAccess(t *testing.T) {
	e := newTestApp(t)
	roles := map[string]string{"opener": "buyer", "assignee": "moderator", "other_mod": "moderator", "admin": "admin", "buyer": "buyer", "vendor": "vendor", "past_mod": "moderator"}
	uids := map[string]string{}
	key, public := testPGPKey(t, "xmpp_ticket_shared_test_key")
	for name, role := range roles {
		uid, _ := e.user("xmpp_ticket_"+name, role)
		uids[name] = uid
		// Sharing this test-only key proves that ciphertext/key possession alone
		// cannot bypass ownership, assignment, or escalation authorization.
		e.setKey(uid, public)
		xmppOptIn(e, uid)
		agExec(e, `UPDATE users SET xmpp_include_contents=true WHERE id=$1`, uid)
	}
	enableXMPPContents(e, uids["opener"])
	fingerprint := agStr(e, `SELECT pgp_fingerprint FROM users WHERE id=$1`, uids["opener"])
	ticketID, messageID := randomToken(), randomToken()
	retentionTestTicket(e, ticketID, uids["opener"], uids["assignee"], "open", 0)
	agExec(e, `INSERT INTO ticket_participants(ticket_id,user_id,ever_assigned) VALUES($1,$2,true)`, ticketID, uids["past_mod"])
	const plaintext = "PRIVATE-SUPPORT-TICKET-BODY"
	ciphertext, err := encryptTo(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, `INSERT INTO ticket_messages(id,ticket_id,sender_id,body,encrypted) VALUES($1,$2,$3,$4,true)`, messageID, ticketID, uids["opener"], ciphertext)
	check := func(name string, want bool) {
		t.Helper()
		audience := "ticket"
		if name == "opener" {
			audience = "personal"
		}
		notice := xmppLinkedTicketNotice(e, uids[name], ticketID, messageID, audience)
		body, err := xmppNotificationBody(context.Background(), e.DB, notice, uids[name], "Support ticket update", true, fingerprint)
		if err != nil || strings.Contains(body, plaintext) {
			t.Fatalf("%s: unsafe ticket content or query failure: %v", name, err)
		}
		prefix := "Support ticket update\n\nTicket " + ticketID + ": Encrypted ticket message:\n"
		if want {
			if !strings.HasPrefix(body, prefix) || testDecrypt(t, key, strings.TrimPrefix(body, prefix)) != plaintext {
				t.Fatalf("%s did not receive its authorized encrypted ticket message", name)
			}
		} else if body != "Support ticket update" {
			t.Fatalf("%s received ticket contents through a staff/global/history grant", name)
		}
	}
	for name := range roles {
		check(name, name == "opener" || name == "assignee")
	}
	// Admin escalation revokes the former moderator's external body access,
	// even though they still possess the same key and historical participation.
	agExec(e, `UPDATE tickets SET admin_only=true,assigned_to=$2 WHERE id=$1`, ticketID, uids["admin"])
	for name := range roles {
		check(name, name == "opener" || name == "admin")
	}
	// A stale/forged assignment cannot override the administrator-only boundary.
	agExec(e, `UPDATE tickets SET assigned_to=$2 WHERE id=$1`, ticketID, uids["assignee"])
	check("assignee", false)
	agExec(e, `UPDATE tickets SET admin_only=false WHERE id=$1`, ticketID)
	check("assignee", true)
	agExec(e, `UPDATE users SET role='buyer' WHERE id=$1`, uids["assignee"])
	check("assignee", false)
	agExec(e, `UPDATE users SET role='moderator' WHERE id=$1`, uids["assignee"])
	check("assignee", true)
	// Opening/closing/reassigning alerts carry metadata only unless their
	// notification is explicitly linked to a checked source body.
	notice := xmppLinkedTicketNotice(e, uids["assignee"], ticketID, "", "ticket")
	body, err := xmppNotificationBody(context.Background(), e.DB, notice, uids["assignee"], "Support ticket update", true, fingerprint)
	if err != nil || body != "Support ticket update" {
		t.Fatalf("metadata-only ticket notice appended contents: %v", err)
	}
	for _, unsafe := range []string{plaintext, symmetricMessage(t, plaintext), hideRecipientIDs(t, ciphertext, key)} {
		agExec(e, `UPDATE ticket_messages SET body=$2,encrypted=true WHERE id=$1`, messageID, unsafe)
		check("opener", false)
		check("assignee", false)
	}
	agExec(e, `UPDATE ticket_messages SET body=$2,encrypted=false WHERE id=$1`, messageID, ciphertext)
	check("opener", false)
	agExec(e, `UPDATE ticket_messages SET encrypted=true WHERE id=$1`, messageID)
	check("opener", true)
	wrongTicket := randomToken()
	retentionTestTicket(e, wrongTicket, uids["opener"], uids["assignee"], "open", 0)
	notice = xmppLinkedTicketNotice(e, uids["opener"], wrongTicket, messageID, "personal")
	body, err = xmppNotificationBody(context.Background(), e.DB, notice, uids["opener"], "Support ticket update", true, fingerprint)
	if err != nil || body != "Support ticket update" {
		t.Fatalf("a different linked ticket authorized another thread's body: %v", err)
	}
}

func TestXMPPContentsTicketWorkerRechecksEscalationAndDemotion(t *testing.T) {
	for _, change := range []string{"escalation", "reassignment", "demotion"} {
		t.Run(change, func(t *testing.T) {
			e := newTestApp(t)
			opener, _ := e.user("xmpp_worker_ticket_opener", "buyer")
			mod, _ := e.user("xmpp_worker_ticket_mod", "moderator")
			admin, _ := e.user("xmpp_worker_ticket_admin", "admin")
			key, public := testPGPKey(t, "xmpp_worker_ticket_shared")
			e.setKey(opener, public)
			e.setKey(mod, public)
			e.setKey(admin, public)
			enableXMPPContents(e, mod)
			ticketID, messageID := randomToken(), randomToken()
			retentionTestTicket(e, ticketID, opener, mod, "open", 0)
			ciphertext, err := encryptTo(key, "PRIVATE-QUEUED-TICKET-CONTENT")
			if err != nil {
				t.Fatal(err)
			}
			agExec(e, `INSERT INTO ticket_messages(id,ticket_id,sender_id,body) VALUES($1,$2,$3,$4)`, messageID, ticketID, opener, ciphertext)
			notice := xmppLinkedTicketNotice(e, mod, ticketID, messageID, "ticket")
			switch change {
			case "escalation":
				agExec(e, `UPDATE tickets SET admin_only=true,assigned_to=$2 WHERE id=$1`, ticketID, admin)
			case "reassignment":
				agExec(e, `UPDATE tickets SET assigned_to=$2 WHERE id=$1`, ticketID, admin)
			case "demotion":
				agExec(e, `UPDATE users SET role='buyer' WHERE id=$1`, mod)
			}
			calls := 0
			worked, err := e.A.deliverXMPPNotification(context.Background(), func(_ context.Context, _ xmppConfig, _, _, body string) error {
				calls++
				if body != "Support ticket update" {
					t.Fatal("a queued former assignee received ticket ciphertext after permission changed")
				}
				return nil
			})
			wantCalls, wantOutcome := 1, "sent"
			if change == "demotion" || change == "escalation" {
				wantCalls, wantOutcome = 0, "cancelled"
			}
			if err != nil || !worked || calls != wantCalls || agStr(e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, notice) != wantOutcome {
				t.Fatalf("worker ticket boundary: worked=%v calls=%d error=%v", worked, calls, err)
			}
		})
	}
}

func TestXMPPContentsTicketLockedSourceRetriesWithoutWaitingOrSending(t *testing.T) {
	e := newTestApp(t)
	opener, _ := e.user("xmpp_locked_ticket_opener", "buyer")
	mod, _ := e.user("xmpp_locked_ticket_mod", "moderator")
	key, public := testPGPKey(t, "xmpp_locked_ticket_shared")
	e.setKey(opener, public)
	e.setKey(mod, public)
	enableXMPPContents(e, mod)
	ticketID, messageID := randomToken(), randomToken()
	retentionTestTicket(e, ticketID, opener, mod, "open", 0)
	ciphertext, err := encryptTo(key, "PRIVATE-LOCKED-TICKET-CONTENT")
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, `INSERT INTO ticket_messages(id,ticket_id,sender_id,body) VALUES($1,$2,$3,$4)`, messageID, ticketID, opener, ciphertext)
	notice := xmppLinkedTicketNotice(e, mod, ticketID, messageID, "ticket")
	blocking, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocking.Rollback()
	var lockedID string
	if err := blocking.QueryRowContext(t.Context(), `SELECT id FROM tickets WHERE id=$1 FOR UPDATE`, ticketID).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	calls := 0
	send := func(_ context.Context, _ xmppConfig, _, _, body string) error {
		calls++
		if !strings.Contains(body, "-----BEGIN PGP MESSAGE-----") || strings.Contains(body, "PRIVATE-LOCKED-TICKET-CONTENT") {
			t.Fatal("the retry did not submit only checked ciphertext")
		}
		return nil
	}
	// This is shorter than PostgreSQL's normal 3-second application lock
	// timeout. NOWAIT must release the worker's earlier recipient/config locks
	// instead of waiting in the opposite order to retention or assignment.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	worked, err := e.A.deliverXMPPNotification(ctx, send)
	cancel()
	var pgErr *pgconn.PgError
	if worked || !errors.As(err, &pgErr) || pgErr.Code != "55P03" || calls != 0 || agStr(e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, notice) != "pending" {
		t.Fatalf("locked source did not defer safely: worked=%v calls=%d error=%v", worked, calls, err)
	}
	if err := blocking.Commit(); err != nil {
		t.Fatal(err)
	}
	worked, err = e.A.deliverXMPPNotification(t.Context(), send)
	if err != nil || !worked || calls != 1 || agStr(e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, notice) != "sent" {
		t.Fatalf("unlocked source did not retry: worked=%v calls=%d error=%v", worked, calls, err)
	}
}
