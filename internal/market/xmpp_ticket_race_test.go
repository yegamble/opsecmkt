package market

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func xmppTicketSummaryFixture(t *testing.T) (*testEnv, string, string, string) {
	t.Helper()
	e := newTestApp(t)
	opener, _ := e.user("xmpp_summary_ticket_opener", "buyer")
	mod, _ := e.user("xmpp_summary_ticket_mod", "moderator")
	admin, _ := e.user("xmpp_summary_ticket_admin", "admin")
	enableXMPPDelivery(e) // Content permission remains off; these alerts have no body source.
	xmppOptIn(e, mod)
	ticketID := randomToken()
	retentionTestTicket(e, ticketID, opener, mod, "open", 0)
	notice := xmppLinkedTicketNotice(e, mod, ticketID, "", "ticket")
	return e, ticketID, admin, notice
}

func TestXMPPTicketSummarySubmissionSerializesEscalation(t *testing.T) {
	e, ticketID, admin, notice := xmppTicketSummaryFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	type result struct {
		worked bool
		err    error
	}
	workerDone := make(chan result, 1)
	go func() {
		worked, err := e.A.deliverXMPPNotification(t.Context(), func(ctx context.Context, _ xmppConfig, _, _, body string) error {
			if body != "Support ticket update" {
				return errors.New("summary-only notification changed")
			}
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		workerDone <- result{worked, err}
	}()
	select {
	case <-entered:
	case outcome := <-workerDone:
		t.Fatalf("worker returned before submission: %#v", outcome)
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not enter summary submission")
	}
	escalation, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		releaseOnce.Do(func() { close(release) })
		_ = escalation.Rollback()
	}()
	var escalationPID int
	if err := escalation.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&escalationPID); err != nil {
		t.Fatal(err)
	}
	escalated := make(chan error, 1)
	go func() {
		if _, err := escalation.ExecContext(t.Context(), `UPDATE tickets SET admin_only=true,assigned_to=$2 WHERE id=$1`, ticketID, admin); err != nil {
			escalated <- err
			return
		}
		escalated <- escalation.Commit()
	}()
	// Observe the actual database lock wait; an elapsed-time-only assertion can
	// pass accidentally when the escalation goroutine has not run yet.
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case err := <-escalated:
			t.Fatalf("escalation finished while summary submission was still in flight: %v", err)
		default:
		}
		if agInt(e, `SELECT count(*) FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND cardinality(pg_blocking_pids(pid))>0`, escalationPID) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("escalation did not wait on the in-flight summary's ticket lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if agInt(e, `SELECT count(*) FROM tickets WHERE id=$1 AND NOT admin_only`, ticketID) != 1 {
		t.Fatal("revocation committed before the authorized submission finished")
	}
	releaseOnce.Do(func() { close(release) })
	if outcome := <-workerDone; !outcome.worked || outcome.err != nil {
		t.Fatalf("summary submission failed: %#v", outcome)
	}
	if err := <-escalated; err != nil {
		t.Fatal(err)
	}
	if agStr(e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, notice) != "sent" || agInt(e, `SELECT count(*) FROM tickets WHERE id=$1 AND admin_only`, ticketID) != 1 {
		t.Fatal("serialized submission/revocation did not complete")
	}
}

func TestXMPPTicketSummaryRevocationBeforeLockCancelsWithoutSending(t *testing.T) {
	e, ticketID, admin, notice := xmppTicketSummaryFixture(t)
	escalation, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer escalation.Rollback()
	if _, err := escalation.ExecContext(t.Context(), `UPDATE tickets SET admin_only=true,assigned_to=$2 WHERE id=$1`, ticketID, admin); err != nil {
		t.Fatal(err)
	}
	calls := 0
	send := func(context.Context, xmppConfig, string, string, string) error {
		calls++
		return errors.New("revoked summary must never be submitted")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	worked, err := e.A.deliverXMPPNotification(ctx, send)
	cancel()
	var pgErr *pgconn.PgError
	if worked || !errors.As(err, &pgErr) || pgErr.Code != "55P03" || calls != 0 || agStr(e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, notice) != "pending" {
		t.Fatalf("uncommitted escalation did not defer summary safely: worked=%v calls=%d error=%v", worked, calls, err)
	}
	if err := escalation.Commit(); err != nil {
		t.Fatal(err)
	}
	worked, err = e.A.deliverXMPPNotification(t.Context(), send)
	if err != nil || !worked || calls != 0 || agStr(e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, notice) != "cancelled" {
		t.Fatalf("committed escalation did not cancel summary: worked=%v calls=%d error=%v", worked, calls, err)
	}
}

func TestXMPPTicketSummaryAllowsOrdinaryReassignmentAndUnlinkedHistoricalClosure(t *testing.T) {
	e, ticketID, admin, notice := xmppTicketSummaryFixture(t)
	agExec(e, `UPDATE tickets SET assigned_to=$2 WHERE id=$1`, ticketID, admin)
	worked, err := e.A.deliverXMPPNotification(t.Context(), func(_ context.Context, _ xmppConfig, _, _, body string) error {
		if body != "Support ticket update" {
			return errors.New("ordinary reassignment changed the summary")
		}
		return nil
	})
	if err != nil || !worked || agStr(e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, notice) != "sent" {
		t.Fatalf("ordinary reassignment revoked permitted summary metadata: worked=%v error=%v", worked, err)
	}
	mod := agStr(e, `SELECT id FROM users WHERE handle='xmpp_summary_ticket_mod'`)
	agExec(e, `UPDATE tickets SET admin_only=true WHERE id=$1`, ticketID)
	closure := randomToken()
	agExec(e, `INSERT INTO notifications(id,user_id,body,audience,ticket_history_only) VALUES($1,$2,'A support ticket you participated in was closed.','ticket',true)`, closure, mod)
	worked, err = e.A.deliverXMPPNotification(t.Context(), func(_ context.Context, _ xmppConfig, _, _, body string) error {
		if body != "A support ticket you participated in was closed." {
			return errors.New("historical closure metadata changed")
		}
		return nil
	})
	if err != nil || !worked || agStr(e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, closure) != "sent" {
		t.Fatalf("unlinked generic historical closure was blocked: worked=%v error=%v", worked, err)
	}
}
