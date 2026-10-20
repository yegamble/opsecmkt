package market

import (
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
)

type ticketTestWorld struct {
	e        *testEnv
	ids      map[string]string
	sessions map[string]string
	keys     map[string]*openpgp.Entity
	public   map[string]string
}

func newTicketTestWorld(t *testing.T) *ticketTestWorld {
	t.Helper()
	w := &ticketTestWorld{e: newTestApp(t), ids: map[string]string{}, sessions: map[string]string{}, keys: map[string]*openpgp.Entity{}, public: map[string]string{}}
	for _, account := range []struct{ name, role string }{
		{"opener", "buyer"}, {"vendor", "vendor"}, {"stranger", "buyer"},
		{"mod", "moderator"}, {"othermod", "moderator"}, {"admin", "admin"}, {"otheradmin", "admin"},
	} {
		w.ids[account.name], w.sessions[account.name] = w.e.user("ticket_"+account.name, account.role)
		w.keys[account.name], w.public[account.name] = testPGPKey(t, "ticket-"+account.name)
		w.e.setKey(w.ids[account.name], w.public[account.name])
		w.e.pgpSessionKey(w.sessions[account.name])
	}
	agExec(w.e, "UPDATE users SET staff_hidden=false WHERE role IN ('moderator','admin')")
	agExec(w.e, `INSERT INTO settings(key,value) VALUES('staff_list_public','true') ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	enableXMPPDelivery(w.e)
	for _, id := range w.ids {
		xmppOptIn(w.e, id)
	}
	return w
}

func (w *ticketTestWorld) ciphertext(assignee, plaintext string) string {
	w.e.t.Helper()
	return encryptToRecipients(w.e.t, plaintext, w.keys["opener"], w.keys[assignee])
}

func (w *ticketTestWorld) open(assignee string) (string, string) {
	w.e.t.Helper()
	ciphertext := w.ciphertext(assignee, "Private opening ticket evidence that must remain encrypted.")
	w.e.check(w.e.do("POST", "/tickets", w.sessions["opener"], url.Values{"staff_id": {w.ids[assignee]}, "body": {ciphertext}}), 303)
	id := agStr(w.e, "SELECT id FROM tickets WHERE opener_id=$1 ORDER BY created DESC,id DESC LIMIT 1", w.ids["opener"])
	return id, agStr(w.e, "SELECT id FROM ticket_messages WHERE ticket_id=$1 ORDER BY created,id LIMIT 1", id)
}

func ticketTestCounts(e *testEnv) []int {
	e.t.Helper()
	var out []int
	for _, table := range []string{"tickets", "ticket_messages", "ticket_events", "ticket_participants", "notifications", "xmpp_outbox", "audit_events", "form_submissions"} {
		out = append(out, agInt(e, "SELECT count(*) FROM "+table))
	}
	return out
}

func (w *ticketTestWorld) refused(path, actor string, form url.Values) {
	w.e.t.Helper()
	before := ticketTestCounts(w.e)
	response := w.e.do("POST", path, w.sessions[actor], form)
	if response.Code < 400 || response.Code >= 500 || response.Code == 429 {
		w.e.t.Fatalf("%s by %s status=%d, want a client refusal: %s", path, actor, response.Code, response.Body.String())
	}
	if after := ticketTestCounts(w.e); !slices.Equal(before, after) {
		w.e.t.Fatalf("refused %s by %s wrote ticket, message, event, participant, notice, outbox, audit or receipt rows: before=%v after=%v", path, actor, before, after)
	}
}

type ticketTestNotice struct {
	user, audience, body, ticket, message string
}

func ticketTestNotices(e *testEnv) map[string]ticketTestNotice {
	e.t.Helper()
	rows, err := e.DB.Query(`SELECT id,user_id,audience,body,COALESCE(ticket_id,''),COALESCE(ticket_message_id,'') FROM notifications`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]ticketTestNotice{}
	for rows.Next() {
		var id string
		var notice ticketTestNotice
		if err = rows.Scan(&id, &notice.user, &notice.audience, &notice.body, &notice.ticket, &notice.message); err != nil {
			e.t.Fatal(err)
		}
		out[id] = notice
	}
	if err = rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func ticketTestNewNotices(e *testEnv, before map[string]ticketTestNotice) map[string]ticketTestNotice {
	e.t.Helper()
	out := ticketTestNotices(e)
	for id := range before {
		delete(out, id)
	}
	return out
}

func ticketTestNoticeRecipients(e *testEnv, notices map[string]ticketTestNotice, want ...string) {
	e.t.Helper()
	got := []string{}
	for id, notice := range notices {
		got = append(got, notice.user)
		if strings.Contains(notice.body, "-----BEGIN PGP MESSAGE-----") || strings.Contains(notice.body, "Private opening ticket evidence") {
			e.t.Fatal("ticket notification copied private message text or ciphertext into its frontend summary")
		}
		if queued := agInt(e, "SELECT count(*) FROM xmpp_outbox WHERE notification_id=$1", id); queued != 1 {
			e.t.Fatalf("frontend notice for user %s has %d XMPP queue entries, want 1", notice.user, queued)
		}
	}
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		e.t.Fatalf("ticket notification recipients=%v, want %v", got, want)
	}
}

func TestTicketsOpeningKeepsEncryptedContentsPrivateAndMatchesNotifications(t *testing.T) {
	w := newTicketTestWorld(t)
	id, messageID := w.open("mod")
	if agStr(w.e, "SELECT status FROM tickets WHERE id=$1", id) != "open" || agStr(w.e, "SELECT assigned_to FROM tickets WHERE id=$1", id) != w.ids["mod"] {
		t.Fatal("new ticket did not retain its selected assignee and open status")
	}
	if agInt(w.e, "SELECT count(*) FROM ticket_events WHERE ticket_id=$1 AND event='opened'", id) != 1 || agInt(w.e, "SELECT count(*) FROM ticket_participants WHERE ticket_id=$1 AND user_id=$2 AND ever_assigned", id, w.ids["mod"]) != 1 {
		t.Fatal("ticket opening did not record its event and assignment history")
	}
	body := agStr(w.e, "SELECT body FROM ticket_messages WHERE id=$1 AND encrypted", messageID)
	for _, name := range []string{"opener", "mod"} {
		if plaintext := testDecrypt(t, w.keys[name], body); plaintext != "Private opening ticket evidence that must remain encrypted." {
			t.Fatalf("%s could not decrypt the initial ticket message", name)
		}
	}
	for _, name := range []string{"vendor", "stranger", "othermod", "admin", "otheradmin"} {
		if deliveryDecryptsWith(w.keys[name], body) {
			t.Fatalf("initial ticket body was encrypted to unrelated %s", name)
		}
	}
	for _, name := range []string{"opener", "mod", "othermod", "admin", "otheradmin"} {
		if _, err := readTicket(t.Context(), w.e.DB, id, w.e.loadUser(w.ids[name]), false); err != nil {
			t.Fatalf("authorized %s could not read ordinary ticket: %v", name, err)
		}
	}
	for _, name := range []string{"vendor", "stranger"} {
		if _, err := readTicket(t.Context(), w.e.DB, id, w.e.loadUser(w.ids[name]), false); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("unrelated %s read a private ticket: %v", name, err)
		}
	}
	notices := ticketTestNotices(w.e)
	ticketTestNoticeRecipients(w.e, notices, w.ids["mod"], w.ids["othermod"], w.ids["admin"], w.ids["otheradmin"])
	for _, notice := range notices {
		if notice.ticket != id {
			t.Fatal("new ticket notification lost its explicit ticket association")
		}
		if notice.user == w.ids["mod"] {
			if !strings.Contains(notice.body, "new message from ticket_opener regarding Ticket #"+id) {
				t.Fatal("ticket message alert did not identify the sender and ticket")
			}
			if notice.audience != "ticket" || notice.message != messageID {
				t.Fatal("selected staff did not receive exactly one encrypted-message notification with revocable staff scope")
			}
		} else if notice.audience != "ticket" || notice.message != "" {
			t.Fatal("staff opening broadcast exposed a message source or incorrect audience")
		}
	}
}

func TestTicketsEveryOpenerRoleRequiresCurrentSessionDecryptionWithoutMandatoryTwoFactor(t *testing.T) {
	for _, role := range []string{"buyer", "vendor", "moderator", "admin"} {
		t.Run(role, func(t *testing.T) {
			w := newTicketTestWorld(t)
			agExec(w.e, "UPDATE users SET role=$2 WHERE id=$1", w.ids["opener"], role)
			agExec(w.e, "UPDATE sessions SET pgp_access_key='' WHERE user_id=$1", w.ids["opener"])
			body := w.ciphertext("mod", "Private ticket message for two explicit encryption recipients.")
			w.e.setKey(w.ids["opener"], "")
			w.refused("/tickets", "opener", url.Values{"staff_id": {w.ids["mod"]}, "body": {body}})
			w.e.setKey(w.ids["opener"], w.public["opener"])
			w.refused("/tickets", "opener", url.Values{"staff_id": {w.ids["mod"]}, "body": {body}})
			proveAccess(w.e, w.ids["opener"], w.sessions["opener"], w.keys["opener"])
			if agInt(w.e, "SELECT count(*) FROM users WHERE id=$1 AND NOT pgp_2fa AND NOT totp_enabled", w.ids["opener"]) != 1 {
				t.Fatal("ticket access unexpectedly required optional sign-in factor enrollment")
			}
			w.e.check(w.e.do("POST", "/tickets", w.sessions["opener"], url.Values{"staff_id": {w.ids["mod"]}, "body": {body}}), 303)
		})
	}
}

func TestTicketsOpeningRejectsInvalidCiphertextAtomically(t *testing.T) {
	for name, body := range map[string]func(*ticketTestWorld) string{
		"plaintext":             func(*ticketTestWorld) string { return "Private ticket plaintext must never be accepted." },
		"opener missing":        func(w *ticketTestWorld) string { return encryptToRecipients(t, "Private evidence", w.keys["mod"]) },
		"assignee missing":      func(w *ticketTestWorld) string { return encryptToRecipients(t, "Private evidence", w.keys["opener"]) },
		"wrong staff recipient": func(w *ticketTestWorld) string { return w.ciphertext("admin", "Private evidence") },
		"hidden recipients": func(w *ticketTestWorld) string {
			return hideRecipientIDs(t, w.ciphertext("mod", "Private evidence"), w.keys["opener"])
		},
		"passphrase only": func(*ticketTestWorld) string { return symmetricMessage(t, "Private evidence") },
	} {
		t.Run(name, func(t *testing.T) {
			w := newTicketTestWorld(t)
			w.refused("/tickets", "opener", url.Values{"staff_id": {w.ids["mod"]}, "body": {body(w)}})
			w.open("mod")
		})
	}
}

func TestTicketsOpeningRequiresAvailableVisibleStaff(t *testing.T) {
	for _, name := range []string{"opener", "vendor", "stranger", "missing"} {
		t.Run(name, func(t *testing.T) {
			w := newTicketTestWorld(t)
			target := w.ids[name]
			if name == "missing" {
				target = randomToken()
			}
			w.refused("/tickets", "opener", url.Values{"staff_id": {target}, "body": {w.ciphertext("mod", "Private staff selection test message.")}})
		})
	}
	for _, column := range []string{"suspended_at", "closed_at", "retention_restricted_at"} {
		t.Run(column, func(t *testing.T) {
			w := newTicketTestWorld(t)
			agExec(w.e, "UPDATE users SET "+column+"=now() WHERE id=$1", w.ids["mod"])
			w.refused("/tickets", "opener", url.Values{"staff_id": {w.ids["mod"]}, "body": {w.ciphertext("mod", "Private staff selection test message.")}})
		})
	}
	w := newTicketTestWorld(t)
	body := w.ciphertext("mod", "Private staff selection test message.")
	agExec(w.e, "UPDATE users SET staff_hidden=true WHERE id=$1", w.ids["mod"])
	w.refused("/tickets", "opener", url.Values{"staff_id": {w.ids["mod"]}, "body": {body}})
	// A configured contact can be selected even without a public directory entry.
	agExec(w.e, `INSERT INTO settings(key,value) VALUES('staff_contact','ticket_mod') ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	w.e.check(w.e.do("POST", "/tickets", w.sessions["opener"], url.Values{"staff_id": {w.ids["mod"]}, "body": {body}}), 303)
}

func TestTicketsRejectUnusableOpenerAndAssigneeKeys(t *testing.T) {
	for name, public := range testUnusableKeys(t, "ticket-unusable") {
		for _, owner := range []string{"opener", "mod"} {
			t.Run(name+"/"+owner, func(t *testing.T) {
				w := newTicketTestWorld(t)
				body := w.ciphertext("mod", "Private ticket must not use an unusable saved key.")
				w.e.setKey(w.ids[owner], public)
				w.refused("/tickets", "opener", url.Values{"staff_id": {w.ids["mod"]}, "body": {body}})
			})
		}
	}
}

func TestTicketsStaffCanSelectHiddenColleaguesWithoutSelectingTheOpener(t *testing.T) {
	w := newTicketTestWorld(t)
	agExec(w.e, "UPDATE users SET role='moderator' WHERE id=$1", w.ids["opener"])
	agExec(w.e, "UPDATE users SET staff_hidden=true WHERE id=$1", w.ids["mod"])
	agExec(w.e, "UPDATE settings SET value='false' WHERE key='staff_list_public'")
	w.refused("/tickets", "opener", url.Values{"staff_id": {w.ids["opener"]}, "body": {w.ciphertext("mod", "Private self-assignment refusal.")}})
	id, _ := w.open("mod")
	w.refused("/tickets/assign", "admin", url.Values{"id": {id}, "staff_id": {w.ids["opener"]}})
	// A manager may claim the ticket personally, provided that account is not its opener.
	w.e.check(w.e.do("POST", "/tickets/assign", w.sessions["admin"], url.Values{"id": {id}, "staff_id": {w.ids["admin"]}}), 303)
	if agStr(w.e, "SELECT assigned_to FROM tickets WHERE id=$1", id) != w.ids["admin"] {
		t.Fatal("staff could not claim another account's ticket")
	}
}

func TestTicketsRepliesRecheckTheOpenerAndCurrentAssignee(t *testing.T) {
	w := newTicketTestWorld(t)
	id, _ := w.open("mod")
	body := w.ciphertext("mod", "Private ticket reply must use both current saved keys.")
	for _, account := range []string{"opener", "mod"} {
		w.e.setKey(w.ids[account], "")
		w.refused("/tickets/reply", "opener", url.Values{"id": {id}, "body": {body}})
		w.e.setKey(w.ids[account], w.public[account])
	}
	for _, column := range []string{"suspended_at", "closed_at", "retention_restricted_at"} {
		agExec(w.e, "UPDATE users SET "+column+"=now() WHERE id=$1", w.ids["mod"])
		w.refused("/tickets/reply", "opener", url.Values{"id": {id}, "body": {body}})
		agExec(w.e, "UPDATE users SET "+column+"=NULL WHERE id=$1", w.ids["mod"])
	}
	agExec(w.e, "UPDATE users SET role='buyer' WHERE id=$1", w.ids["mod"])
	w.refused("/tickets/reply", "opener", url.Values{"id": {id}, "body": {body}})
	w.refused("/tickets/assign", "mod", url.Values{"id": {id}, "staff_id": {w.ids["othermod"]}})
	// A current staff member can repair the assignment; only a fresh ciphertext to that assignee can be sent.
	w.e.check(w.e.do("POST", "/tickets/assign", w.sessions["admin"], url.Values{"id": {id}, "staff_id": {w.ids["othermod"]}}), 303)
	w.refused("/tickets/reply", "opener", url.Values{"id": {id}, "body": {body}})
	w.e.check(w.e.do("POST", "/tickets/reply", w.sessions["opener"], url.Values{"id": {id}, "body": {w.ciphertext("othermod", "Private reply after repairing the selected staff account.")}}), 303)
	if agInt(w.e, "SELECT count(*) FROM ticket_messages WHERE ticket_id=$1", id) != 2 {
		t.Fatal("a refused reply was stored or the repaired assignment could not receive its message")
	}
}

func TestTicketsClosedOrRestrictedHistoricalStaffReceiveNoClosureBroadcast(t *testing.T) {
	for _, column := range []string{"closed_at", "retention_restricted_at"} {
		t.Run(column, func(t *testing.T) {
			w := newTicketTestWorld(t)
			id, _ := w.open("mod")
			w.e.check(w.e.do("POST", "/tickets/assign", w.sessions["admin"], url.Values{"id": {id}, "staff_id": {w.ids["othermod"]}}), 303)
			agExec(w.e, "UPDATE users SET "+column+"=now() WHERE id=$1", w.ids["mod"])
			before := ticketTestNotices(w.e)
			w.e.check(w.e.do("POST", "/tickets/status", w.sessions["opener"], url.Values{"id": {id}, "status": {"closed"}}), 303)
			ticketTestNoticeRecipients(w.e, ticketTestNewNotices(w.e, before), w.ids["opener"], w.ids["othermod"])
		})
	}
}

func TestTicketsAssignmentReplyUsesCurrentKeysAndRecordsHistory(t *testing.T) {
	w := newTicketTestWorld(t)
	id, firstMessage := w.open("mod")
	w.e.check(w.e.do("POST", "/tickets/assign", w.sessions["mod"], url.Values{"id": {id}, "staff_id": {w.ids["othermod"]}}), 303)
	if agStr(w.e, "SELECT assigned_to FROM tickets WHERE id=$1", id) != w.ids["othermod"] || agInt(w.e, "SELECT count(*) FROM ticket_participants WHERE ticket_id=$1 AND ever_assigned", id) != 2 {
		t.Fatal("assignment did not keep both historical assignees")
	}
	w.refused("/tickets/reply", "opener", url.Values{"id": {id}, "body": {w.ciphertext("mod", "Stale recipient after ticket reassignment.")}})
	newKey, newPublic := testPGPKey(t, "ticket-new-assignee-key")
	w.e.setKey(w.ids["othermod"], newPublic)
	w.refused("/tickets/reply", "opener", url.Values{"id": {id}, "body": {w.ciphertext("othermod", "Stale key after assignee key replacement.")}})
	w.keys["othermod"], w.public["othermod"] = newKey, newPublic
	body := w.ciphertext("othermod", "Private new reply for current ticket recipients.")
	before := ticketTestNotices(w.e)
	w.e.check(w.e.do("POST", "/tickets/reply", w.sessions["opener"], url.Values{"id": {id}, "body": {body}}), 303)
	ticketTestNoticeRecipients(w.e, ticketTestNewNotices(w.e, before), w.ids["othermod"])
	if deliveryDecryptsWith(w.keys["othermod"], agStr(w.e, "SELECT body FROM ticket_messages WHERE id=$1", firstMessage)) {
		t.Fatal("new assignee unexpectedly decrypted old ciphertext addressed to the previous staff key")
	}
	// An authorized staff member can update the ticket, but still needs their own usable key and must encrypt to
	// the opener and current assignee. Only those two correspondents receive the private message notification.
	w.e.setKey(w.ids["admin"], "")
	w.refused("/tickets/reply", "admin", url.Values{"id": {id}, "body": {body}})
	w.e.setKey(w.ids["admin"], w.public["admin"])
	before = ticketTestNotices(w.e)
	w.e.check(w.e.do("POST", "/tickets/reply", w.sessions["admin"], url.Values{"id": {id}, "body": {body}}), 303)
	ticketTestNoticeRecipients(w.e, ticketTestNewNotices(w.e, before), w.ids["opener"], w.ids["othermod"])
	for _, notice := range ticketTestNewNotices(w.e, before) {
		if !strings.Contains(notice.body, "new message from ticket_admin regarding Ticket #"+id) || strings.Contains(notice.body, "-----BEGIN PGP MESSAGE-----") {
			t.Fatal("private ticket summary must identify its sender and ticket without embedding ciphertext")
		}
	}
	if agInt(w.e, "SELECT count(*) FROM ticket_participants WHERE ticket_id=$1 AND user_id=$2 AND ever_messaged", id, w.ids["admin"]) != 1 {
		t.Fatal("replying staff was not retained in the ticket participation history")
	}
}

func TestTicketsClosureNotifiesActiveHistoricalStaffAndOwner(t *testing.T) {
	w := newTicketTestWorld(t)
	id, _ := w.open("mod")
	w.e.check(w.e.do("POST", "/tickets/reply", w.sessions["mod"], url.Values{"id": {id}, "body": {w.ciphertext("mod", "Private first staff response.")}}), 303)
	w.e.check(w.e.do("POST", "/tickets/assign", w.sessions["admin"], url.Values{"id": {id}, "staff_id": {w.ids["othermod"]}}), 303)
	w.e.check(w.e.do("POST", "/tickets/reply", w.sessions["admin"], url.Values{"id": {id}, "body": {w.ciphertext("othermod", "Private independent staff response.")}}), 303)
	w.e.check(w.e.do("POST", "/tickets/assign", w.sessions["admin"], url.Values{"id": {id}, "staff_id": {w.ids["otheradmin"]}}), 303)
	before := ticketTestNotices(w.e)
	w.e.check(w.e.do("POST", "/tickets/status", w.sessions["opener"], url.Values{"id": {id}, "status": {"closed"}}), 303)
	ticketTestNoticeRecipients(w.e, ticketTestNewNotices(w.e, before), w.ids["opener"], w.ids["mod"], w.ids["othermod"], w.ids["admin"], w.ids["otheradmin"])
	if agStr(w.e, "SELECT status FROM tickets WHERE id=$1", id) != "closed" || agInt(w.e, "SELECT count(*) FROM ticket_events WHERE ticket_id=$1 AND event='closed'", id) != 1 {
		t.Fatal("owner closure did not persist exactly one closed event")
	}
	w.refused("/tickets/reply", "opener", url.Values{"id": {id}, "body": {w.ciphertext("otheradmin", "A closed ticket must refuse new messages.")}})
	w.refused("/tickets/reply", "admin", url.Values{"id": {id}, "body": {w.ciphertext("otheradmin", "Staff cannot append a message until reopening.")}})
	w.refused("/tickets/status", "opener", url.Values{"id": {id}, "status": {"open"}})
	w.e.check(w.e.do("POST", "/tickets/status", w.sessions["admin"], url.Values{"id": {id}, "status": {"open"}}), 303)
	if agInt(w.e, "SELECT count(*) FROM ticket_events WHERE ticket_id=$1 AND event='reopened'", id) != 1 {
		t.Fatal("staff reopening was not recorded")
	}
	// A past staff role does not authorize a broadcast, even when its participant history remains intact.
	agExec(w.e, "UPDATE users SET role='buyer' WHERE id=$1", w.ids["mod"])
	agExec(w.e, "UPDATE users SET suspended_at=now() WHERE id=$1", w.ids["othermod"])
	before = ticketTestNotices(w.e)
	w.e.check(w.e.do("POST", "/tickets/status", w.sessions["admin"], url.Values{"id": {id}, "status": {"ignored"}}), 303)
	ticketTestNoticeRecipients(w.e, ticketTestNewNotices(w.e, before), w.ids["opener"], w.ids["admin"], w.ids["otheradmin"])
	w.refused("/tickets/status", "opener", url.Values{"id": {id}, "status": {"ignored"}})
}

func TestTicketsEscalationRevokesModeratorAccessAndKeepsGenericClosureHistory(t *testing.T) {
	w := newTicketTestWorld(t)
	id, _ := w.open("mod")
	w.e.check(w.e.do("POST", "/tickets/assign", w.sessions["admin"], url.Values{"id": {id}, "staff_id": {w.ids["othermod"]}}), 303)
	w.refused("/tickets/escalate", "opener", url.Values{"id": {id}, "staff_id": {w.ids["admin"]}})
	w.refused("/tickets/escalate", "mod", url.Values{"id": {id}, "staff_id": {w.ids["othermod"]}})
	before := ticketTestNotices(w.e)
	w.e.check(w.e.do("POST", "/tickets/escalate", w.sessions["mod"], url.Values{"id": {id}, "staff_id": {w.ids["admin"]}}), 303)
	ticketTestNoticeRecipients(w.e, ticketTestNewNotices(w.e, before), w.ids["opener"], w.ids["admin"], w.ids["otheradmin"])
	if agInt(w.e, "SELECT count(*) FROM tickets WHERE id=$1 AND admin_only AND assigned_to=$2", id, w.ids["admin"]) != 1 || agInt(w.e, "SELECT count(*) FROM ticket_events WHERE ticket_id=$1 AND event='escalated'", id) != 1 {
		t.Fatal("escalation did not permanently restrict the ticket and record the chosen administrator")
	}
	for _, name := range []string{"mod", "othermod", "vendor", "stranger"} {
		if _, err := readTicket(t.Context(), w.e.DB, id, w.e.loadUser(w.ids[name]), false); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s could read an escalated ticket: %v", name, err)
		}
		w.refused("/tickets/reply", name, url.Values{"id": {id}, "body": {w.ciphertext("admin", "An unauthorized escalated-ticket update.")}})
		w.refused("/tickets/assign", name, url.Values{"id": {id}, "staff_id": {w.ids["otheradmin"]}})
		w.refused("/tickets/status", name, url.Values{"id": {id}, "status": {"closed"}})
	}
	// A stale or caller-supplied role must not reopen the staff boundary.
	if _, err := readTicket(t.Context(), w.e.DB, id, &User{ID: w.ids["mod"], Role: "admin"}, false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a stale in-memory administrator role bypassed escalation: %v", err)
	}
	w.refused("/tickets/assign", "admin", url.Values{"id": {id}, "staff_id": {w.ids["mod"]}})
	w.refused("/tickets/escalate", "admin", url.Values{"id": {id}, "staff_id": {w.ids["otheradmin"]}})
	before = ticketTestNotices(w.e)
	w.e.check(w.e.do("POST", "/tickets/status", w.sessions["admin"], url.Values{"id": {id}, "status": {"closed"}}), 303)
	closed := ticketTestNewNotices(w.e, before)
	// Closing staff receive the same single alert as every other historical participant.
	ticketTestNoticeRecipients(w.e, closed, w.ids["opener"], w.ids["mod"], w.ids["othermod"], w.ids["admin"])
	for _, notice := range closed {
		if (notice.user == w.ids["mod"] || notice.user == w.ids["othermod"]) && (notice.ticket != "" || notice.message != "") {
			t.Fatal("historical moderator closure exposed an administrator-only thread or message link")
		}
	}
}

func TestTicketsModeratorOpenerKeepsPersonalRightsAfterEscalation(t *testing.T) {
	w := newTicketTestWorld(t)
	agExec(w.e, "UPDATE users SET role='moderator' WHERE id=$1", w.ids["opener"])
	id, _ := w.open("mod")
	w.e.check(w.e.do("POST", "/tickets/escalate", w.sessions["mod"], url.Values{"id": {id}, "staff_id": {w.ids["admin"]}}), 303)
	if _, err := readTicket(t.Context(), w.e.DB, id, w.e.loadUser(w.ids["opener"]), false); err != nil {
		t.Fatalf("moderator opener lost personal ticket access: %v", err)
	}
	w.e.check(w.e.do("POST", "/tickets/reply", w.sessions["opener"], url.Values{"id": {id}, "body": {w.ciphertext("admin", "Private owner reply after escalation.")}}), 303)
	w.refused("/tickets/assign", "opener", url.Values{"id": {id}, "staff_id": {w.ids["otheradmin"]}})
	w.refused("/tickets/status", "opener", url.Values{"id": {id}, "status": {"ignored"}})
	w.refused("/tickets/escalate", "opener", url.Values{"id": {id}, "staff_id": {w.ids["otheradmin"]}})
	before := ticketTestNotices(w.e)
	w.e.check(w.e.do("POST", "/tickets/status", w.sessions["opener"], url.Values{"id": {id}, "status": {"closed"}}), 303)
	ownerNotice := false
	for _, notice := range ticketTestNewNotices(w.e, before) {
		ownerNotice = ownerNotice || notice.user == w.ids["opener"]
	}
	if !ownerNotice {
		t.Fatal("the moderator who closed their own ticket did not receive its personal closure notification")
	}
	w.refused("/tickets/status", "opener", url.Values{"id": {id}, "status": {"open"}})
}

func TestTicketsFormNonceReplayDoesNotDuplicateMessagesEventsOrNotifications(t *testing.T) {
	w := newTicketTestWorld(t)
	open := url.Values{"staff_id": {w.ids["mod"]}, "body": {w.ciphertext("mod", "Private replay-safe initial ticket message.")}, formNonceField: {formNonceFor("/tickets")}}
	w.e.check(w.e.do("POST", "/tickets", w.sessions["opener"], open), 303)
	counts := ticketTestCounts(w.e)
	w.e.check(w.e.do("POST", "/tickets", w.sessions["opener"], open), 303)
	if !slices.Equal(counts, ticketTestCounts(w.e)) {
		t.Fatal("ticket opening replay duplicated durable records")
	}
	id := agStr(w.e, "SELECT id FROM tickets")
	for _, action := range []struct {
		path, actor string
		form        url.Values
	}{
		{"/tickets/reply", "opener", url.Values{"id": {id}, "body": {w.ciphertext("mod", "Private replay-safe follow-up.")}}},
		{"/tickets/assign", "admin", url.Values{"id": {id}, "staff_id": {w.ids["othermod"]}}},
		{"/tickets/escalate", "othermod", url.Values{"id": {id}, "staff_id": {w.ids["admin"]}}},
		{"/tickets/status", "admin", url.Values{"id": {id}, "status": {"closed"}}},
	} {
		action.form.Set(formNonceField, formNonceFor(action.path))
		w.e.check(w.e.do("POST", action.path, w.sessions[action.actor], action.form), 303)
		counts = ticketTestCounts(w.e)
		w.e.check(w.e.do("POST", action.path, w.sessions[action.actor], action.form), 303)
		if !slices.Equal(counts, ticketTestCounts(w.e)) {
			t.Fatalf("%s replay duplicated durable records", action.path)
		}
	}
	if agInt(w.e, "SELECT count(*) FROM tickets") != 1 || agInt(w.e, "SELECT count(*) FROM ticket_messages") != 2 {
		t.Fatal("replayed ticket forms changed the ticket or message count")
	}
}
