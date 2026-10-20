package market

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestTicketsRetentionUnlinkCannotGrantHistoricalNotificationAccess(t *testing.T) {
	w := newTicketTestWorld(t)
	id, _ := w.open("mod")
	w.e.check(w.e.do("POST", "/tickets/escalate", w.sessions["mod"], url.Values{"id": {id}, "staff_id": {w.ids["admin"]}}), 303)
	escalationBody := "Ticket #" + id + " was escalated to administrators."
	escalationNotice := agStr(w.e, `SELECT id FROM notifications WHERE user_id=$1 AND body=$2`, w.ids["otheradmin"], escalationBody)
	closeForm := url.Values{"id": {id}, "status": {"closed"}, formNonceField: {formNonceFor("/tickets/status")}}
	w.e.check(w.e.do("POST", "/tickets/status", w.sessions["admin"], closeForm), 303)
	beforeReplay := ticketTestCounts(w.e)
	w.e.check(w.e.do("POST", "/tickets/status", w.sessions["admin"], closeForm), 303)
	if after := ticketTestCounts(w.e); len(after) != len(beforeReplay) {
		t.Fatal("closure replay changed the count shape")
	} else {
		for i := range after {
			if after[i] != beforeReplay[i] {
				t.Fatal("closure replay duplicated history or notification records")
			}
		}
	}
	closureBody := "Ticket #" + id + " is now Closed."
	historyNotice := agStr(w.e, `SELECT id FROM notifications WHERE user_id=$1 AND body=$2 AND ticket_history_only AND ticket_id IS NULL AND ticket_message_id IS NULL`, w.ids["mod"], closureBody)
	if agInt(w.e, `SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2`, w.ids["mod"], closureBody) != 1 {
		t.Fatal("the historical participant did not receive exactly one intentional closure notice")
	}
	agExec(w.e, `UPDATE users SET role='moderator' WHERE id=$1`, w.ids["otheradmin"])
	if strings.Contains(w.e.body("GET", "/notifications", w.sessions["otheradmin"], nil, 200), escalationBody) {
		t.Fatal("demoted administrator could read the linked administrator-only escalation notice")
	}
	if agInt(w.e, `SELECT count(*) FROM ticket_participants WHERE ticket_id=$1 AND user_id=$2`, id, w.ids["otheradmin"]) != 0 {
		t.Fatal("the global notification recipient unexpectedly became a ticket participant")
	}
	cfg := retentionConfig{Enabled: true, StateDir: retentionPrivateTestDir(t)}
	if err := w.e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	commitRetentionHold(t, w.e, retentionHold{Scope: "account", RecordID: w.ids["otheradmin"], Authority: "Specific test preservation obligation", Reason: "Preserve recipient notifications", Owner: "Assigned test owner", Readers: "Assigned test handlers", ReleaseCondition: "Written release", BeganAt: now.Add(-60 * 24 * time.Hour), ReviewAt: now.Add(-31 * 24 * time.Hour)})
	// A global recipient's account hold preserves their old notifications, but
	// they have no participation/assignment/event relationship holding the ticket.
	agExec(w.e, `UPDATE notifications SET created=now()-interval '91 days' WHERE user_id=$1`, w.ids["otheradmin"])
	agExec(w.e, `UPDATE tickets SET created=now()-interval '100 days',updated=now()-interval '91 days' WHERE id=$1`, id)
	agExec(w.e, `UPDATE ticket_messages SET created=now()-interval '91 days' WHERE ticket_id=$1`, id)
	ageRetentionEnrollment(t, w.e.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, w.e.A, now)
	if agInt(w.e, `SELECT count(*) FROM tickets WHERE id=$1`, id) != 0 || agInt(w.e, `SELECT count(*) FROM notifications WHERE id=$1 AND ticket_id IS NULL AND NOT ticket_history_only`, escalationNotice) != 1 {
		t.Fatal("fixture did not erase the ticket and retain the ordinary notification without a historical grant")
	}
	if strings.Contains(w.e.body("GET", "/notifications", w.sessions["otheradmin"], nil, 200), escalationBody) {
		t.Fatal("retention FK unlinking reclassified an administrator-only notice as historical moderator metadata")
	}
	if strings.Count(w.e.body("GET", "/notifications", w.sessions["mod"], nil, 200), closureBody) != 1 {
		t.Fatal("the intentionally granted historical closure notice was hidden or duplicated after ticket erasure")
	}
	// Keep unrelated fixture deliveries out of this targeted worker observation.
	// After proving the old held notice survives cleanup without gaining access,
	// synthetically refresh its age to isolate the worker authorization gate from
	// the separate maximum-delivery-age refusal for old held notifications.
	agExec(w.e, `UPDATE notifications SET created=now() WHERE id=$1`, escalationNotice)
	agExec(w.e, `UPDATE xmpp_outbox SET next_attempt=now()+interval '2 days' WHERE notification_id<>$1`, escalationNotice)
	calls := 0
	worked, err := w.e.A.deliverXMPPNotification(t.Context(), func(context.Context, xmppConfig, string, string, string) error {
		calls++
		return nil
	})
	if err != nil || !worked || calls != 0 || agStr(w.e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, escalationNotice) != "cancelled" {
		t.Fatalf("an orphaned ordinary notice escaped the worker gate: worked=%v calls=%d error=%v", worked, calls, err)
	}
	agExec(w.e, `UPDATE xmpp_outbox SET next_attempt=now() WHERE notification_id=$1`, historyNotice)
	worked, err = w.e.A.deliverXMPPNotification(t.Context(), func(_ context.Context, _ xmppConfig, _, _, body string) error {
		calls++
		if body != closureBody {
			return errors.New("historical closure did not retain its fixed metadata body")
		}
		return nil
	})
	if err != nil || !worked || calls != 1 || agStr(w.e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, historyNotice) != "sent" {
		t.Fatalf("an explicit historical closure grant was blocked: worked=%v calls=%d error=%v", worked, calls, err)
	}
}

func TestXMPPTicketHistoryGrantRequiresExplicitMetadataWithoutSources(t *testing.T) {
	e := newTestApp(t)
	mod, modSession := e.user("xmpp_history_mod", "moderator")
	opener, _ := e.user("xmpp_history_opener", "buyer")
	vendor, _ := e.user("xmpp_history_vendor", "vendor")
	enableXMPPContents(e, mod)
	ticketID, ticketMessageID, messageID := randomToken(), randomToken(), randomToken()
	retentionTestTicket(e, ticketID, opener, mod, "open", 0)
	agExec(e, `INSERT INTO ticket_messages(id,ticket_id,sender_id,body) VALUES($1,$2,$3,'test ciphertext')`, ticketMessageID, ticketID, opener)
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body) VALUES($1,$2,$3,'test ciphertext')`, messageID, opener, mod)
	orderID := e.order(opener, e.product(vendor, "digital"), "XMR", stateCompleted)
	orphan := enqueueXMPPTestNotice(e, mod, "ticket", "Unlinked ordinary ticket notice")
	if agInt(e, `SELECT count(*) FROM xmpp_outbox WHERE notification_id=$1`, orphan) != 0 || strings.Contains(e.body("GET", "/notifications", modSession, nil, 200), "Unlinked ordinary ticket notice") {
		t.Fatal("a default-false absent ticket context acquired a historical grant")
	}
	baseline := agInt(e, `SELECT count(*) FROM notifications`)
	for _, test := range []struct{ name, audience, source, sourceID string }{
		{"personal audience", "personal", "", ""},
		{"administrator audience", "admin", "", ""},
		{"ticket link", "ticket", "ticket_id", ticketID},
		{"ticket ciphertext", "ticket", "ticket_message_id", ticketMessageID},
		{"direct message ciphertext", "ticket", "message_id", messageID},
		{"digital delivery ciphertext", "ticket", "delivery_order_id", orderID},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := `INSERT INTO notifications(id,user_id,body,audience,ticket_history_only) VALUES($1,$2,'invalid history grant',$3,true)`
			args := []any{randomToken(), mod, test.audience}
			if test.source != "" {
				query = `INSERT INTO notifications(id,user_id,body,audience,ticket_history_only,` + test.source + `) VALUES($1,$2,'invalid history grant',$3,true,$4)`
				args = append(args, test.sourceID)
			}
			_, err := e.DB.Exec(query, args...)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "notifications_ticket_history_only" || agInt(e, `SELECT count(*) FROM notifications`) != baseline {
				t.Fatalf("invalid historical grant was not atomically refused: %v", err)
			}
		})
	}
	history := randomToken()
	agExec(e, `INSERT INTO notifications(id,user_id,body,audience,ticket_history_only) VALUES($1,$2,'Intentional historical closure metadata','ticket',true)`, history, mod)
	if agInt(e, `SELECT count(*) FROM xmpp_outbox WHERE notification_id=$1 AND NOT include_contents AND recipient_fingerprint=''`, history) != 1 {
		t.Fatal("explicit historical metadata did not queue without private contents or key metadata")
	}
}
