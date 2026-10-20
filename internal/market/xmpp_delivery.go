package market

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"
)

const xmppMaxAttempts = 12

type xmppSender func(context.Context, xmppConfig, string, string, string) error

func init() { registerBackground(xmppDeliveryWorker) }

func xmppDeliveryWorker(ctx context.Context, a *App) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		// Each transaction handles one receipt. SKIP LOCKED permits multiple app
		// instances without concurrent sends of the same notification.
		for range 20 {
			worked, err := a.deliverXMPPNotification(ctx, sendXMPP)
			if err != nil {
				if ctx.Err() == nil {
					log.Print("XMPP delivery: queue processing failed; will retry")
				}
				break
			}
			if !worked {
				break
			}
		}
		timer.Reset(5 * time.Second)
	}
}

// deliverXMPPNotification sends only committed frontend records. Its user share
// lock prevents a role/destination change during the final authorization check
// and send. A failed/ambiguous send is retried with the same stanza ID; XMPP
// server acceptance is not a guarantee the recipient read the message.
func (a *App) deliverXMPPNotification(ctx context.Context, send xmppSender) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id, destination, body, userID string
	var version int64
	var includeContents bool
	var recipientFingerprint string
	var contentPolicyVersion int64
	var ticketID sql.NullString
	var attempts int
	var created time.Time
	err = tx.QueryRowContext(ctx, `SELECT q.notification_id,q.destination,q.preference_version,q.attempts,n.user_id,n.body,n.created,
		q.include_contents,q.recipient_fingerprint,q.content_policy_version,n.ticket_id
		FROM xmpp_outbox q JOIN notifications n ON n.id=q.notification_id
		WHERE q.finished_at IS NULL AND q.next_attempt<=now()
		ORDER BY q.next_attempt,q.notification_id LIMIT 1 FOR UPDATE OF q SKIP LOCKED`).Scan(&id, &destination, &version, &attempts, &userID, &body, &created,
		&includeContents, &recipientFingerprint, &contentPolicyVersion, &ticketID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	finish := func(outcome string) (bool, error) {
		_, err := tx.ExecContext(ctx, `UPDATE xmpp_outbox SET outcome=$2,finished_at=now() WHERE notification_id=$1`, id, outcome)
		if err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	if time.Since(created) >= 24*time.Hour || attempts >= xmppMaxAttempts {
		return finish("failed")
	}
	var allowed bool
	err = tx.QueryRowContext(ctx, `SELECT u.xmpp_enabled AND u.xmpp=$3 AND u.xmpp_version=$4
		AND u.suspended_at IS NULL AND u.closed_at IS NULL AND u.retention_restricted_at IS NULL
		AND `+notificationAudienceSQL("n", "u")+`
		FROM users u JOIN notifications n ON n.user_id=u.id WHERE u.id=$1 AND n.id=$2 FOR SHARE OF u`, userID, id, destination, version).Scan(&allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return finish("cancelled")
	}
	if err != nil {
		return false, err
	}
	if !allowed || !validateXMPPJID(destination) {
		return finish("cancelled")
	}
	if ticketID.Valid {
		// Metadata carries ticket context even when contents are disabled. Hold
		// that ticket through submission, then check access again against a fresh
		// statement snapshot. Escalation must either commit before this check or
		// wait for the already-authorized submission to finish. NOWAIT avoids a
		// cycle with maintenance/actions that already hold the ticket row.
		var lockedTicket string
		err = tx.QueryRowContext(ctx, `SELECT id FROM tickets WHERE id=$1 FOR SHARE NOWAIT`, ticketID.String).Scan(&lockedTicket)
		if errors.Is(err, sql.ErrNoRows) {
			return finish("cancelled")
		}
		if err != nil {
			return false, err
		}
		var ticketAllowed bool
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(`+ticketReadableSQL("t", "u.id", "u.role")+`,false)
			FROM tickets t JOIN users u ON u.id=$2 WHERE t.id=$1`, lockedTicket, userID).Scan(&ticketAllowed)
		if errors.Is(err, sql.ErrNoRows) {
			return finish("cancelled")
		}
		if err != nil {
			return false, err
		}
		if !ticketAllowed {
			return finish("cancelled")
		}
	}
	// Hold the policy row through submission. A permission revocation either
	// commits before this check or waits for an already-authorized send to finish.
	var rawConfig string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1 FOR SHARE`, settingXMPPConfig).Scan(&rawConfig)
	if errors.Is(err, sql.ErrNoRows) {
		return finish("cancelled")
	}
	if err != nil {
		return false, err
	}
	cfg, err := a.loadXMPPConfig(ctx, tx)
	if err != nil {
		return false, err
	}
	if !cfg.Enabled {
		return finish("cancelled")
	}
	includeContents = includeContents && cfg.AllowContents && cfg.ContentPolicyVersion == contentPolicyVersion
	body, err = xmppNotificationBody(ctx, tx, id, userID, body, includeContents, recipientFingerprint)
	if err != nil {
		return false, err
	}
	sendCtx, stopSend := context.WithTimeout(ctx, 10*time.Second)
	sendErr := send(sendCtx, cfg, destination, "market-"+id, body)
	stopSend()
	// Store no server error text, destinations, message contents or credentials
	// in logs/status. Transient failures leave the frontend notification intact.
	if sendErr == nil {
		return finish("sent")
	}
	attempts++
	if attempts >= xmppMaxAttempts {
		if _, err = tx.ExecContext(ctx, `UPDATE xmpp_outbox SET attempts=$2 WHERE notification_id=$1`, id, attempts); err != nil {
			return false, err
		}
		return finish("failed")
	}
	delay := min(30*time.Second*time.Duration(1<<min(attempts-1, 10)), time.Hour)
	_, err = tx.ExecContext(ctx, `UPDATE xmpp_outbox SET attempts=$2,next_attempt=now()+make_interval(secs=>$3) WHERE notification_id=$1`, id, attempts, delay.Seconds())
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}
