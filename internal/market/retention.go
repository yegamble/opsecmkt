package market

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"
)

const retentionBatchSize = 200

const retentionOrderHeldSQL = `EXISTS(SELECT 1 FROM retention_holds h WHERE h.released_at IS NULL AND
 ((h.scope='order' AND h.record_id=o.id) OR (h.scope='account' AND h.record_id IN(o.buyer_id,p.vendor_id))))`

func (a *App) startRetention(ctx context.Context) {
	if a.retention == nil {
		return
	}
	a.background.Add(1)
	go func() {
		defer a.background.Done()
		timer := time.NewTimer(30 * time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				pass, cancel := context.WithTimeout(ctx, 5*time.Minute)
				next := 30 * time.Minute
				if err := a.runRetention(pass); err != nil {
					log.Print("retention maintenance failed; inspect external state and database before restoring access")
					next = time.Minute
				}
				cancel()
				timer.Reset(next)
			}
		}
	}()
}

// A pass uses finite transactions. Draining bounded batches prevents the daily
// deadline from silently becoming an unbounded backlog on busy instances.
func (a *App) runRetention(ctx context.Context) (returnErr error) {
	if a.retention == nil {
		return nil
	}
	initialFailure := a.retention.failure.Load()
	defer func() {
		class := ""
		for {
			current := a.retention.failure.Load()
			class = ""
			if returnErr != nil {
				class = "maintenance_failed"
				if errors.Is(returnErr, errRetentionStateUnavailable) || current != nil && *current == "state_unavailable" {
					class = "state_unavailable"
				}
			} else if current != initialFailure && current != nil && *current == "state_unavailable" {
				// A concurrent operation detected an unavailable manifest after
				// this pass began; this pass cannot prove that newer fault repaired.
				class = "state_unavailable"
			}
			if a.retention.failure.CompareAndSwap(current, &class) {
				break
			}
		}
		status, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		a.db.ExecContext(status, `INSERT INTO settings(key,value) VALUES('retention_last_error',$1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, class)
	}()
	for i := 0; i < 256; i++ {
		more, err := a.runRetentionBatch(ctx, time.Now().UTC())
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	return errors.New("retention backlog exceeds one maintenance run; keep restored traffic paused and rerun maintenance")
}

func (a *App) retentionMaintenanceError() string {
	if a.retention == nil {
		return ""
	}
	if p := a.retention.failure.Load(); p != nil {
		return *p
	}
	return ""
}

func (a *App) runRetentionBatch(ctx context.Context, now time.Time) (moreResult bool, returnErr error) {
	s := a.retention
	unlock, err := s.lock()
	if err != nil {
		return false, err
	}
	defer unlock()
	if err = s.read(); err != nil {
		return false, err
	}
	if err = a.checkRetentionSequence(ctx, a.db); err != nil {
		return false, err
	}
	if err = a.resolveRetentionPending(ctx); err != nil {
		return false, err
	}
	// Reclaim expired restore history before preparing a new bounded journal
	// batch, so old entries cannot prevent a valid sweep from being recorded.
	if err = a.compactRetention(ctx, now); err != nil {
		return false, err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	preparedID := ""
	commitAttempted := false
	defer func() {
		tx.Rollback()
		if preparedID != "" && !commitAttempted {
			s.removePending(preparedID)
			returnErr = errors.Join(returnErr, s.write())
		}
	}()
	// Account retirement shares host account/schema maintenance's lock order.
	// A nonblocking acquisition avoids cycles with factor or role recovery tools.
	var accountMaintenance bool
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(782493)`).Scan(&accountMaintenance); err != nil {
		return false, err
	}
	if !accountMaintenance {
		return false, errRetentionBusy
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('opsecmkt.retention_purge','on',true)`); err != nil {
		return false, err
	}
	if err = a.mirrorRetention(ctx, tx); err != nil {
		return false, err
	}
	if err = retentionAuthSweep(ctx, tx, now); err != nil {
		return false, err
	}
	if err = a.retentionAccountNotices(ctx, tx, now); err != nil {
		return false, err
	}
	// Financial closure is observed by the adapter's checked path, not inferred
	// from an order label. If an adapter cannot be checked, privacy expiry waits.
	if err = a.retentionFinancialClosure(ctx, tx, now); err != nil {
		return false, err
	}
	var tombstones []retentionTombstone
	if !now.Before(s.state.EnrollmentDeadline) {
		if tombstones, err = a.retentionCandidates(ctx, tx, now); err != nil {
			return false, err
		}
	}
	op := retentionOperation{ID: randomToken(), At: now, Tombstones: tombstones}
	counts := map[string]int{}
	if len(tombstones) > 0 {
		preparedID = op.ID
		if err = a.prepareRetentionOperation(ctx, tx, op); err != nil {
			return false, err
		}
		for _, t := range tombstones {
			if err = a.applyRetentionTombstone(ctx, tx, t); err != nil {
				return false, err
			}
			counts[t.Kind]++
		}
	}
	b, _ := json.Marshal(counts)
	if _, err = tx.ExecContext(ctx, `INSERT INTO retention_runs(started,counts) VALUES($1,$2)`, now, string(b)); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM retention_runs WHERE started<$1::timestamptz-interval '180 days'`, now); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM retention_account_exceptions WHERE resolved_at<$1::timestamptz-interval '180 days'`, now); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM retention_operations WHERE created<$1::timestamptz-interval '37 days' AND id<>$2`, now, op.ID); err != nil {
		return false, err
	}
	var more bool
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(current_setting('opsecmkt.retention_escrow_more',true),'')='true' OR COALESCE(current_setting('opsecmkt.retention_notice_more',true),'')='true'`).Scan(&more); err != nil {
		return false, err
	}
	commitAttempted = true
	if err = tx.Commit(); err != nil {
		return false, err
	} // ambiguity keeps the prepared journal
	if len(tombstones) > 0 {
		s.commitOperation(op)
	}
	if err = a.compactRetention(ctx, now); err != nil {
		return false, err
	}
	if len(tombstones) > 0 {
		if err = s.write(); err != nil {
			return false, err
		}
	}
	// Every category has a separate batch cap; another pass is necessary when a
	// cap was filled, and also after closure makes dependent rows eligible.
	return more || len(tombstones) > 0, nil
}

func (s *retentionService) removePending(id string) {
	for i, op := range s.state.Pending {
		if op.ID == id {
			s.state.Pending = append(s.state.Pending[:i], s.state.Pending[i+1:]...)
			return
		}
	}
}

func retentionAuthSweep(ctx context.Context, tx *sql.Tx, now time.Time) error {
	for _, table := range []string{"sessions", "pending_logins", "captcha_spent", "pgp_challenges", "form_submissions", "branding_upload_tickets"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE expires<=$1`, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM provider_episode_deliveries d USING provider_episodes p WHERE p.provider=d.provider AND p.bad_since IS NULL AND d.created<=$1::timestamptz-interval '30 days' AND NOT EXISTS(SELECT 1 FROM retention_holds h WHERE h.scope='account' AND h.record_id=d.user_id AND h.released_at IS NULL)`, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE users SET recovery_reveal='',recovery_reveal_until=NULL WHERE recovery_reveal_until IS NOT NULL AND recovery_reveal_until<=$1`, now)
	return err
}

func (a *App) mirrorRetention(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM retention_holds`); err != nil {
		return err
	}
	for _, h := range a.retention.state.Holds {
		if err := upsertRetentionHold(ctx, tx, h); err != nil {
			return err
		}
	}
	for _, r := range a.retention.state.Requests {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET deletion_requested_at=$2 WHERE id=$1 AND (deletion_requested_at IS NULL OR deletion_requested_at>$2)`, r.ID, r.At); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) replayRetention(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `SELECT set_config('opsecmkt.retention_purge','on',true)`); err != nil {
		return err
	}
	if err := a.mirrorRetention(ctx, tx); err != nil {
		return err
	}
	for _, t := range a.retention.state.Tombstones {
		if err := a.applyRetentionTombstone(ctx, tx, t); err != nil {
			return fmt.Errorf("replay retention %s: %w", t.Kind, err)
		}
	}
	var auditFloor int64
	for _, t := range a.retention.state.Tombstones {
		if t.Kind == "audit" {
			id, err := strconv.ParseInt(t.ID, 10, 64)
			if err != nil {
				return err
			}
			if id > auditFloor {
				auditFloor = id
			}
		}
	}
	for _, h := range a.retention.state.Holds {
		if h.Scope == "audit" {
			id, err := strconv.ParseInt(h.RecordID, 10, 64)
			if err != nil {
				return err
			}
			if id > auditFloor {
				auditFloor = id
			}
		}
	}
	if auditFloor > 0 {
		if _, err := tx.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('audit_events','id'),GREATEST((SELECT last_value FROM audit_events_id_seq),$1),true)`, auditFloor); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) retentionAccountNotices(ctx context.Context, tx *sql.Tx, now time.Time) error {
	// Historical accounts have no reliable last-use observation. They get 60
	// days to reconfirm activity, then the ordinary 30-day closure notice.
	legacyNoticeAt := a.retention.state.EnabledAt.Add(60 * 24 * time.Hour)
	rows, err := tx.QueryContext(ctx, `SELECT u.id FROM users u WHERE u.closed_at IS NULL AND u.deletion_requested_at IS NULL AND u.inactivity_notified_at IS NULL
	 AND ((u.last_authenticated_at IS NOT NULL AND u.last_authenticated_at<=$1::timestamptz-interval '12 months') OR (u.last_authenticated_at IS NULL AND $1>=$2 AND u.created<=$3) OR (u.last_authenticated_at IS NULL AND u.created>$3 AND u.created<=$1::timestamptz-interval '12 months'))
	 AND NOT EXISTS(SELECT 1 FROM retention_holds h WHERE h.scope='account' AND h.record_id=u.id AND h.released_at IS NULL)
	 ORDER BY u.created,u.id LIMIT $4 FOR UPDATE OF u`, now, legacyNoticeAt, a.retention.state.EnabledAt, retentionBatchSize)
	if err != nil {
		return err
	}
	ids, err := retentionScanIDs(rows)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `UPDATE users SET inactivity_notified_at=CASE WHEN last_authenticated_at IS NULL AND created<=$4 THEN $3::timestamptz ELSE $2::timestamptz END WHERE id=$1`, id, now, legacyNoticeAt, a.retention.state.EnabledAt); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)`, randomToken(), id, "This account is due for inactivity closure in 30 days. Sign in before then to keep it active. Save any messages or deliveries you need; applicable preservation and unresolved financial records may remain."); err != nil {
			return err
		}
	}
	if len(ids) == retentionBatchSize {
		_, err = tx.ExecContext(ctx, `SELECT set_config('opsecmkt.retention_notice_more','true',true)`)
	}
	return err
}

func retentionScanIDs(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (a *App) retentionCandidates(ctx context.Context, tx *sql.Tx, now time.Time) ([]retentionTombstone, error) {
	out, err := retentionTicketCandidates(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	kinds := []string{"message", "notification", "audit", "draft", "order_private", "listing_private", "account_close", "review", "order", "listing"}
	queries := []string{
		`SELECT m.id FROM messages m WHERE (m.created<=$1::timestamptz-interval '90 days' OR EXISTS(SELECT 1 FROM users u WHERE u.closed_at IS NOT NULL AND u.id IN(m.sender_id,m.recipient_id))) AND NOT EXISTS(SELECT 1 FROM retention_holds h WHERE h.released_at IS NULL AND ((h.scope='message' AND h.record_id=m.id) OR (h.scope='account' AND h.record_id IN(m.sender_id,m.recipient_id)))) ORDER BY m.created,m.id LIMIT 200 FOR UPDATE OF m`,
		`SELECT n.id FROM notifications n WHERE (n.created<=$1::timestamptz-interval '30 days' OR EXISTS(SELECT 1 FROM users u WHERE u.closed_at IS NOT NULL AND u.id=n.user_id)) AND NOT EXISTS(SELECT 1 FROM retention_holds h WHERE h.released_at IS NULL AND ((h.scope='account' AND h.record_id=n.user_id) OR (h.scope='order' AND h.record_id=n.order_id))) ORDER BY n.created,n.id LIMIT 200 FOR UPDATE OF n`,
		`SELECT e.id::text FROM audit_events e WHERE e.created<=$1::timestamptz-interval '180 days' AND NOT EXISTS(SELECT 1 FROM retention_holds h WHERE h.released_at IS NULL AND ((h.scope='audit' AND h.record_id=e.id::text) OR (h.scope='account' AND h.record_id=e.user_id))) ORDER BY e.created,e.id LIMIT 200 FOR UPDATE OF e`,
		`SELECT o.id FROM orders o JOIN products p ON p.id=o.product_id WHERE (o.state='draft' OR (o.state='cancelled' AND NOT EXISTS(SELECT 1 FROM order_events ev WHERE ev.order_id=o.id AND ev.to_state='awaiting_payment'))) AND o.created<=$1::timestamptz-interval '30 days' AND NOT ` + retentionOrderHeldSQL + ` AND NOT EXISTS(SELECT 1 FROM payments x WHERE x.order_id=o.id) AND NOT EXISTS(SELECT 1 FROM escrows x WHERE x.order_id=o.id) AND NOT EXISTS(SELECT 1 FROM payment_addresses x WHERE x.order_id=o.id) AND NOT EXISTS(SELECT 1 FROM payouts x WHERE x.order_id=o.id) AND NOT EXISTS(SELECT 1 FROM settlements x WHERE x.order_id=o.id) AND NOT EXISTS(SELECT 1 FROM xmr_settlements x WHERE x.order_id=o.id) AND NOT EXISTS(SELECT 1 FROM disputes x WHERE x.order_id=o.id) AND NOT EXISTS(SELECT 1 FROM reviews x WHERE x.order_id=o.id) AND NOT EXISTS(SELECT 1 FROM deliveries x WHERE x.order_id=o.id) ORDER BY o.created,o.id LIMIT 200 FOR UPDATE OF o,p`,
		`SELECT o.id FROM orders o JOIN products p ON p.id=o.product_id WHERE o.financial_closed_at<=$1::timestamptz-interval '30 days' AND o.private_expired_at IS NULL AND NOT ` + retentionOrderHeldSQL + ` AND NOT EXISTS(SELECT 1 FROM retention_escrow_exceptions x WHERE x.order_id=o.id AND x.resolved_at IS NULL AND x.reason IN('wallet_unavailable','wallet_unverified','watch_incomplete','recovery_gate','pending_recovery')) AND o.state IN('completed','resolved','cancelled') ORDER BY o.financial_closed_at,o.id LIMIT 200 FOR UPDATE OF o,p`,
		`SELECT p.id FROM products p WHERE p.archived AND COALESCE(p.archived_at,p.updated)<=$1::timestamptz-interval '30 days' AND p.private_expired_at IS NULL AND NOT EXISTS(SELECT 1 FROM retention_holds h WHERE h.released_at IS NULL AND h.scope='account' AND h.record_id=p.vendor_id) AND NOT EXISTS(SELECT 1 FROM orders o WHERE o.product_id=p.id AND (o.state IN('awaiting_payment','disputed') OR EXISTS(SELECT 1 FROM retention_holds h WHERE h.released_at IS NULL AND h.scope='order' AND h.record_id=o.id))) ORDER BY p.archived_at,p.id LIMIT 200 FOR UPDATE OF p`,
		`SELECT u.id FROM users u WHERE u.closed_at IS NULL AND (u.retention_checked_at IS NULL OR u.retention_checked_at<=$1::timestamptz-interval '30 minutes') AND (u.deletion_requested_at<=$1::timestamptz-interval '30 days' OR (u.deletion_requested_at IS NULL AND u.inactivity_notified_at<=$1::timestamptz-interval '30 days')) AND NOT EXISTS(SELECT 1 FROM retention_holds h WHERE h.scope='account' AND h.record_id=u.id AND h.released_at IS NULL) ORDER BY u.retention_checked_at NULLS FIRST,u.created,u.id LIMIT 200 FOR UPDATE OF u`,
		`SELECT r.id FROM reviews r JOIN products p ON p.id=r.product_id JOIN orders o ON o.id=r.order_id JOIN users u ON u.id=r.buyer_id WHERE (p.archived AND COALESCE(p.archived_at,p.updated)<=$1::timestamptz-interval '30 days' OR u.closed_at IS NOT NULL) AND o.state<>'disputed' AND NOT ` + retentionOrderHeldSQL + ` AND NOT EXISTS(SELECT 1 FROM retention_holds h WHERE h.scope='account' AND h.record_id=r.buyer_id AND h.released_at IS NULL) ORDER BY r.created,r.id LIMIT 200 FOR UPDATE OF r`,
		`SELECT o.id FROM orders o JOIN products p ON p.id=o.product_id WHERE o.financial_closed_at<=$1::timestamptz-interval '180 days' AND o.state IN('completed','resolved','cancelled') AND NOT ` + retentionOrderHeldSQL + ` AND NOT EXISTS(SELECT 1 FROM payments x WHERE x.order_id=o.id) AND NOT EXISTS(SELECT 1 FROM escrows x WHERE x.order_id=o.id) AND NOT EXISTS(SELECT 1 FROM payment_addresses x WHERE x.order_id=o.id) AND NOT EXISTS(SELECT 1 FROM payouts x WHERE x.order_id=o.id) AND NOT EXISTS(SELECT 1 FROM settlements x WHERE x.order_id=o.id) AND NOT EXISTS(SELECT 1 FROM xmr_settlements x WHERE x.order_id=o.id) ORDER BY o.financial_closed_at,o.id LIMIT 200 FOR UPDATE OF o,p`,
		`SELECT p.id FROM products p WHERE p.archived AND p.private_expired_at IS NOT NULL AND COALESCE(p.archived_at,p.updated)<=$1::timestamptz-interval '30 days' AND NOT EXISTS(SELECT 1 FROM orders o WHERE o.product_id=p.id) AND NOT EXISTS(SELECT 1 FROM reviews r WHERE r.product_id=p.id) AND NOT EXISTS(SELECT 1 FROM retention_holds h WHERE h.scope='account' AND h.record_id=p.vendor_id AND h.released_at IS NULL) ORDER BY p.id LIMIT 200 FOR UPDATE OF p`,
	}
	for index, query := range queries {
		kind := kinds[index]
		if kind == "account_close" {
			rows, err := tx.QueryContext(ctx, query, now)
			if err != nil {
				return nil, err
			}
			ids, err := retentionScanIDs(rows)
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				t, err := a.retentionDueAccount(ctx, tx, id, now)
				if err != nil {
					return nil, err
				}
				if t != nil {
					out = append(out, *t)
				}
			}
			if len(ids) == retentionBatchSize {
				if _, err = tx.ExecContext(ctx, `SELECT set_config('opsecmkt.retention_notice_more','true',true)`); err != nil {
					return nil, err
				}
			}
			continue
		}
		rows, err := tx.QueryContext(ctx, query, now)
		if err != nil {
			return nil, fmt.Errorf("retention candidates %s: %w", kind, err)
		}
		ids, err := retentionScanIDs(rows)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			out = append(out, retentionTombstone{Kind: kind, ID: id, At: now})
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT o.id FROM orders o JOIN products p ON p.id=o.product_id JOIN escrows es ON es.order_id=o.id
	 WHERE o.financial_closed_at<=$1::timestamptz-interval '180 days' AND NOT `+retentionOrderHeldSQL+`
	 AND (o.financial_retirement_checked_at IS NULL OR o.financial_retirement_checked_at<=$1::timestamptz-interval '30 minutes')
	 ORDER BY o.financial_retirement_checked_at NULLS FIRST,o.financial_closed_at,o.id LIMIT 200 FOR UPDATE OF o,p`, now)
	if err != nil {
		return nil, err
	}
	retireIDs, err := retentionScanIDs(rows)
	if err != nil {
		return nil, err
	}
	for _, id := range retireIDs {
		if _, err = tx.ExecContext(ctx, `UPDATE orders SET financial_retirement_checked_at=$2 WHERE id=$1`, id, now); err != nil {
			return nil, err
		}
		certificate, err := a.prepareRetentionEscrowRetirement(ctx, tx, id, now)
		if err != nil {
			return nil, err
		}
		if certificate != nil {
			out = append(out, retentionTombstone{Kind: "escrow_order", ID: id, At: now, Retirement: certificate})
		}
	}
	if len(retireIDs) == retentionBatchSize {
		if _, err = tx.ExecContext(ctx, `SELECT set_config('opsecmkt.retention_notice_more','true',true)`); err != nil {
			return nil, err
		}
	}
	// Keep financial owner/index rows until their retirement is reviewed. Every
	// unrelated closed account can be removed once its live FK references end.
	rows, err = tx.QueryContext(ctx, `SELECT u.id FROM users u WHERE u.closed_at IS NOT NULL AND (u.retention_checked_at IS NULL OR u.retention_checked_at<=$1::timestamptz-interval '30 minutes') AND NOT EXISTS(SELECT 1 FROM retention_holds h WHERE h.released_at IS NULL AND h.scope='account' AND h.record_id=u.id) ORDER BY u.retention_checked_at NULLS FIRST,u.closed_at,u.id LIMIT 200 FOR UPDATE OF u`, now)
	if err != nil {
		return nil, err
	}
	ids, err := retentionScanIDs(rows)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `UPDATE users SET retention_checked_at=$2 WHERE id=$1`, id, now); err != nil {
			return nil, err
		}
		needed, err := retentionUserReferenced(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if !needed {
			out = append(out, retentionTombstone{Kind: "account", ID: id, At: now})
		}
	}
	if len(ids) == retentionBatchSize {
		if _, err = tx.ExecContext(ctx, `SELECT set_config('opsecmkt.retention_notice_more','true',true)`); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Only enumerated, reviewed dependencies are queried. A new foreign key blocks
// full account erasure until its lifecycle is added; it cannot be silently lost.
func retentionUnknownUserReferences(ctx context.Context, tx *sql.Tx) (bool, error) {
	var unknown bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint fk JOIN pg_attribute a ON a.attrelid=fk.conrelid AND a.attnum=ANY(fk.conkey)
 WHERE fk.contype='f' AND fk.confrelid='users'::regclass AND (
 fk.confkey<>ARRAY[(SELECT attnum FROM pg_attribute WHERE attrelid='users'::regclass AND attname='id')]::smallint[]
 OR (fk.conrelid,a.attname) NOT IN(
 ('sessions'::regclass,'user_id'),
 ('products'::regclass,'vendor_id'),
 ('orders'::regclass,'buyer_id'),
 ('messages'::regclass,'sender_id'),
 ('messages'::regclass,'recipient_id'),
 ('tickets'::regclass,'opener_id'),
 ('tickets'::regclass,'assigned_to'),
 ('ticket_participants'::regclass,'user_id'),
 ('ticket_messages'::regclass,'sender_id'),
 ('ticket_events'::regclass,'actor_id'),
 ('ticket_events'::regclass,'staff_id'),
 ('notifications'::regclass,'user_id'),
 ('audit_events'::regclass,'user_id'),
 ('order_events'::regclass,'actor_id'),
 ('pending_logins'::regclass,'user_id'),
 ('recovery_codes'::regclass,'user_id'),
 ('pgp_challenges'::regclass,'user_id'),
 ('reviews'::regclass,'buyer_id'),
 ('payouts'::regclass,'user_id'),
 ('images'::regclass,'owner_user_id'),
 ('market_branding'::regclass,'updated_by'),
 ('branding_upload_tickets'::regclass,'user_id'),
 ('host_reboot_requests'::regclass,'user_id'),
 ('moderation_reviews'::regclass,'reviewer_user_id'),
 ('vendor_applications'::regclass,'user_id'),
 ('vendor_applications'::regclass,'bond_received_by'),
 ('vendor_applications'::regclass,'decided_by'),
 ('users'::regclass,'vendor_verified_set_by'),
 ('users'::regclass,'banned_by'),
 ('user_escrow_keys'::regclass,'user_id'),
 ('escrow_pubkeys'::regclass,'user_id'),
 ('escrow_index_floors'::regclass,'user_id'),
 ('escrow_external_addresses'::regclass,'user_id'),
 ('settlements'::regclass,'requested_by'),
 ('settlement_bumps'::regclass,'requested_by'),
 ('xmr_settlements'::regclass,'requested_by'),
 ('xmr_settlement_rerequests'::regclass,'requested_by'),
 ('xmr_escrow_collisions'::regclass,'disposed_by'),
 ('xmr_settlement_hand_settled'::regclass,'settled_by'),
 ('provider_episode_deliveries'::regclass,'user_id'))))`).Scan(&unknown)
	return unknown, err
}

func retentionUserReferenced(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	unknown, err := retentionUnknownUserReferences(ctx, tx)
	if err != nil || unknown {
		return true, err
	}
	var referenced bool
	err = tx.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM sessions WHERE user_id=$1)
 OR EXISTS(SELECT 1 FROM products WHERE vendor_id=$1)
 OR EXISTS(SELECT 1 FROM orders WHERE buyer_id=$1)
 OR EXISTS(SELECT 1 FROM messages WHERE sender_id=$1)
 OR EXISTS(SELECT 1 FROM messages WHERE recipient_id=$1)
 OR EXISTS(SELECT 1 FROM notifications WHERE user_id=$1)
 OR EXISTS(SELECT 1 FROM audit_events WHERE user_id=$1)
 OR EXISTS(SELECT 1 FROM order_events WHERE actor_id=$1)
 OR EXISTS(SELECT 1 FROM pending_logins WHERE user_id=$1)
 OR EXISTS(SELECT 1 FROM recovery_codes WHERE user_id=$1)
 OR EXISTS(SELECT 1 FROM pgp_challenges WHERE user_id=$1)
 OR EXISTS(SELECT 1 FROM reviews WHERE buyer_id=$1)
 OR EXISTS(SELECT 1 FROM payouts WHERE user_id=$1)
 OR EXISTS(SELECT 1 FROM images WHERE owner_user_id=$1)
 OR EXISTS(SELECT 1 FROM user_escrow_keys WHERE user_id=$1)
 OR EXISTS(SELECT 1 FROM escrow_pubkeys WHERE user_id=$1)
 OR EXISTS(SELECT 1 FROM escrow_index_floors WHERE user_id=$1)
 OR EXISTS(SELECT 1 FROM escrow_external_addresses WHERE user_id=$1)
 OR EXISTS(SELECT 1 FROM settlements WHERE requested_by=$1)
 OR EXISTS(SELECT 1 FROM settlement_bumps WHERE requested_by=$1)
 OR EXISTS(SELECT 1 FROM xmr_settlements WHERE requested_by=$1)
 OR EXISTS(SELECT 1 FROM xmr_settlement_rerequests WHERE requested_by=$1)
 OR EXISTS(SELECT 1 FROM xmr_escrow_collisions WHERE disposed_by=$1)
 OR EXISTS(SELECT 1 FROM xmr_settlement_hand_settled WHERE settled_by=$1)
 OR EXISTS(SELECT 1 FROM provider_episode_deliveries WHERE user_id=$1)
 OR EXISTS(SELECT 1 FROM branding_upload_tickets WHERE user_id=$1)`, id).Scan(&referenced)
	return referenced, err
}

func (a *App) applyRetentionTombstone(ctx context.Context, tx *sql.Tx, t retentionTombstone) error {
	if t.Kind == "xmr_wallet_destroy" {
		if err := validateRetentionTombstone(t); err != nil {
			return err
		}
		return a.applyRetentionXMRDestruction(ctx, tx, t.XMRDestruction)
	}
	if t.Kind == "escrow_order" {
		if err := validateRetentionTombstone(t); err != nil {
			return err
		}
		return a.applyRetentionEscrowRetirement(ctx, tx, t.Retirement)
	}
	if t.Kind == "account_close" {
		if err := a.applyRetentionAccountFloors(ctx, tx, t); err != nil {
			return err
		}
	}
	return applyRetentionTombstone(ctx, tx, t)
}

func applyRetentionTombstone(ctx context.Context, tx *sql.Tx, t retentionTombstone) error {
	switch t.Kind {
	case "message":
		return retentionExecQuery(ctx, tx, `DELETE FROM messages WHERE id=$1`, t.ID)
	case "ticket_message":
		return retentionExecQuery(ctx, tx, `DELETE FROM ticket_messages WHERE id=$1`, t.ID)
	case "ticket":
		return retentionExecQuery(ctx, tx, `DELETE FROM tickets WHERE id=$1`, t.ID)
	case "notification":
		return retentionExecQuery(ctx, tx, `DELETE FROM notifications WHERE id=$1`, t.ID)
	case "audit":
		id, err := strconv.ParseInt(t.ID, 10, 64)
		if err != nil {
			return err
		}
		return retentionExecQuery(ctx, tx, `DELETE FROM audit_events WHERE id=$1 AND created<=$2`, id, t.At)
	case "image":
		return retentionExecQuery(ctx, tx, `DELETE FROM images WHERE id=$1`, t.ID)
	case "review":
		return retentionExecQuery(ctx, tx, `DELETE FROM reviews WHERE id=$1`, t.ID)
	case "order":
		for _, table := range []string{"deliveries", "reviews", "disputes"} {
			if err := retentionExecQuery(ctx, tx, `DELETE FROM `+table+` WHERE order_id=$1`, t.ID); err != nil {
				return err
			}
		}
		if err := retentionExecQuery(ctx, tx, `UPDATE notifications SET order_id=NULL WHERE order_id=$1`, t.ID); err != nil {
			return err
		}
		if err := retentionExecQuery(ctx, tx, `DELETE FROM order_events WHERE order_id=$1`, t.ID); err != nil {
			return err
		}
		return retentionExecQuery(ctx, tx, `DELETE FROM orders WHERE id=$1`, t.ID)
	case "draft":
		if err := retentionExecQuery(ctx, tx, `UPDATE notifications SET order_id=NULL WHERE order_id=$1`, t.ID); err != nil {
			return err
		}
		if err := retentionExecQuery(ctx, tx, `DELETE FROM order_events WHERE order_id=$1`, t.ID); err != nil {
			return err
		}
		return retentionExecQuery(ctx, tx, `DELETE FROM orders WHERE id=$1`, t.ID)
	case "order_private":
		if err := retentionExecQuery(ctx, tx, `DELETE FROM deliveries WHERE order_id=$1 AND created<=$2`, t.ID, t.At); err != nil {
			return err
		}
		if err := retentionExecQuery(ctx, tx, `UPDATE disputes SET reason='Private evidence expired under retention policy' WHERE order_id=$1 AND created<=$2`, t.ID, t.At); err != nil {
			return err
		}
		if err := retentionExecQuery(ctx, tx, `UPDATE disputes SET resolution=CASE WHEN resolution='' THEN '' ELSE 'Private resolution text expired under retention policy' END WHERE order_id=$1 AND COALESCE(resolution_updated_at,created)<=$2`, t.ID, t.At); err != nil {
			return err
		}
		// Prefixes used by escrow safety decisions must survive until reviewed
		// financial retirement; preserve the fixed class but erase private text.
		if err := retentionExecQuery(ctx, tx, `UPDATE order_events SET note=CASE WHEN note LIKE 'Dispute reply: %' THEN 'Dispute reply: Private evidence expired under retention policy' ELSE '' END WHERE order_id=$1 AND created<=$2 AND strpos(note,$3)<>1 AND strpos(note,$4)<>1`, t.ID, t.At, escrowStallMark, escrowSeenMark); err != nil {
			return err
		}
		return retentionExecQuery(ctx, tx, `UPDATE orders o SET listing_title=CASE WHEN listing_title IS NULL THEN NULL ELSE 'Listing content expired' END,listing_description=CASE WHEN listing_description IS NULL THEN NULL ELSE '' END,
		 private_expired_at=CASE WHEN financial_closed_at IS NOT NULL AND financial_closed_at+interval '30 days'<=$2 AND NOT EXISTS(SELECT 1 FROM deliveries d WHERE d.order_id=o.id AND d.created>$2) AND NOT EXISTS(SELECT 1 FROM disputes d WHERE d.order_id=o.id AND COALESCE(d.resolution_updated_at,d.created)>$2) AND NOT EXISTS(SELECT 1 FROM order_events ev WHERE ev.order_id=o.id AND ev.actor_id IS NOT NULL AND ev.created>$2) THEN $2 ELSE NULL END WHERE id=$1`, t.ID, t.At)
	case "listing_private":
		if err := retentionExecQuery(ctx, tx, `DELETE FROM images WHERE product_id=$1 AND created<=$2`, t.ID, t.At); err != nil {
			return err
		}
		return retentionExecQuery(ctx, tx, `UPDATE products SET title='Withdrawn listing',description='',delivery_content='',admin_archive_reason=NULL,review_rejection_reason=NULL,private_expired_at=$2 WHERE id=$1 AND updated<=$2`, t.ID, t.At)
	case "listing":
		return retentionExecQuery(ctx, tx, `DELETE FROM products WHERE id=$1`, t.ID)
	case "account_close":
		if err := retentionExecQuery(ctx, tx, `SELECT retention_erase_legacy_profile($1)`, t.ID); err != nil {
			return err
		}
		for _, table := range []string{"sessions", "pending_logins", "pgp_challenges", "recovery_codes", "form_submissions", "provider_episode_deliveries", "branding_upload_tickets"} {
			if err := retentionExecQuery(ctx, tx, `DELETE FROM `+table+` WHERE user_id=$1`, t.ID); err != nil {
				return err
			}
		}
		if err := retentionExecQuery(ctx, tx, `DELETE FROM images WHERE owner_user_id=$1 AND kind='avatar'`, t.ID); err != nil {
			return err
		}
		// A-544: a closed account leaves the vendor queue. Its application history (ON DELETE CASCADE) and the
		// administrators named on it or on a verified mark (ON DELETE SET NULL) never block account erasure.
		if err := retentionExecQuery(ctx, tx, `UPDATE vendor_applications SET state='withdrawn',decided_at=$2 WHERE user_id=$1 AND state IN `+vendorOpenStates, t.ID, t.At); err != nil {
			return err
		}
		if err := retentionExecQuery(ctx, tx, `UPDATE products SET archived=true,archived_at=COALESCE(archived_at,$2) WHERE vendor_id=$1`, t.ID, t.At); err != nil {
			return err
		}
		// A-546: a ban's reason goes with the profile (the audit rows keep it for the audit trail's own period); the
		// account stays suspended and closed, and banned_handles keeps its old handle reserved after this rename.
		return retentionExecQuery(ctx, tx, `UPDATE users SET handle='closed_'||id,password_hash='',pgp='',xmpp='',xmpp_enabled=false,xmpp_include_contents=false,xmpp_version=xmpp_version+1,pgp_fingerprint='',pgp_verified_at=NULL,pgp_2fa=false,pgp_changed_at=NULL,pgp_expiry_notice='',totp_secret='',totp_pending='',totp_enabled=false,totp_last_step=0,recovery_reveal='',recovery_reveal_until=NULL,staff_hidden=true,suspended_at=COALESCE(suspended_at,$2),closed_at=COALESCE(closed_at,$2),profile_scrubbed_at=COALESCE(profile_scrubbed_at,$2),retention_restricted_at=NULL,banned_at=NULL,banned_by=NULL,ban_reason=NULL,ban_prior_suspended_at=NULL WHERE id=$1`, t.ID, t.At)
	case "account_restrict":
		if err := retentionExecQuery(ctx, tx, `SELECT retention_erase_legacy_profile($1)`, t.ID); err != nil {
			return err
		}
		if err := retentionExecQuery(ctx, tx, `DELETE FROM images WHERE owner_user_id=$1 AND kind='avatar' AND created<=$2`, t.ID, t.At); err != nil {
			return err
		}
		if err := retentionExecQuery(ctx, tx, `UPDATE products SET archived=true,archived_at=COALESCE(archived_at,$2) WHERE vendor_id=$1 AND updated<=$2`, t.ID, t.At); err != nil {
			return err
		}
		if err := retentionExecQuery(ctx, tx, `UPDATE users SET totp_pending='',recovery_reveal='',recovery_reveal_until=NULL,staff_hidden=true,profile_scrubbed_at=COALESCE(profile_scrubbed_at,$2),retention_restricted_at=COALESCE(retention_restricted_at,$2) WHERE id=$1 AND closed_at IS NULL AND (deletion_requested_at<=$2 OR last_authenticated_at IS NULL OR last_authenticated_at<=$2)`, t.ID, t.At); err != nil {
			return err
		}
		return nil
	case "account":
		return retentionExecQuery(ctx, tx, `DELETE FROM users WHERE id=$1`, t.ID)
	}
	return errors.New("unknown retention tombstone")
}

func retentionExecQuery(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}
