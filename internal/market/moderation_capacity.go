package market

import (
	"context"
	"database/sql"
	"errors"
)

const moderationAllocationBytes int64 = 512 << 10

// Each lifetime allocation reserves the worst-case bounded payload of a job,
// one proposal, risk/injection records, review/receipt, and encrypted escalation.
// It is a logical allocation limit, not a physical disk, WAL or backup quota.
type ModerationLimits struct{ MaxPending, MaxRecords, MaxBytes int64 }

func DefaultModerationLimits() ModerationLimits { return ModerationLimits{256, 4096, 256 << 20} }

func ConfigureModerationLimits(ctx context.Context, db *sql.DB, v ModerationLimits) error {
	if v.MaxPending < 1 || v.MaxPending > 10000 || v.MaxRecords < 1 || v.MaxRecords > 1000000 || v.MaxBytes < moderationAllocationBytes || v.MaxBytes > 1<<40 {
		return errors.New("moderation capacity limits invalid")
	}
	res, err := db.ExecContext(ctx, `UPDATE moderation_capacity SET max_pending=$1,max_records=$2,max_bytes=$3 WHERE singleton AND pending<=$1 AND allocated_records<=$2 AND allocated_bytes<=$3`, v.MaxPending, v.MaxRecords, v.MaxBytes)
	if err != nil {
		return errors.New("moderation capacity configuration unavailable")
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return errors.New("moderation capacity limits below existing allocations")
	}
	return nil
}

// Completed v2 captures are references, not archived source. Replaying them
// against an edited source would silently change the evidence. Only legacy
// snapshots can be queued by the explicit replay command, within active bounds.
func ReplayModerationJobs(ctx context.Context, db *sql.DB) (queued, skipped int64, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var available int64
	if err = tx.QueryRowContext(ctx, `SELECT GREATEST(0,max_pending-pending),completed_v2 FROM moderation_capacity WHERE singleton FOR UPDATE`).Scan(&available, &skipped); err != nil {
		return 0, 0, err
	}
	res, err := tx.ExecContext(ctx, `WITH chosen AS(SELECT q.id FROM moderation_jobs q WHERE capture_version=0 AND completed_at IS NOT NULL
	 AND (SELECT count(*) FROM(SELECT 1 FROM moderation_jobs a WHERE a.actor_id=q.actor_id AND a.completed_at IS NULL LIMIT 3) active)<3
	 ORDER BY queued_at,id LIMIT $1 FOR UPDATE SKIP LOCKED) UPDATE moderation_jobs j SET completed_at=NULL FROM chosen WHERE j.id=chosen.id`, available)
	if err != nil {
		return 0, 0, err
	}
	queued, err = res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	err = tx.Commit()
	return
}
