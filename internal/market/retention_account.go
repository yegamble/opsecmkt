package market

import (
	"context"
	"database/sql"
	"time"
)

func (a *App) retentionDueAccount(ctx context.Context, tx *sql.Tx, id string, now time.Time) (*retentionTombstone, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE users SET retention_checked_at=$2 WHERE id=$1`, id, now); err != nil {
		return nil, err
	}
	blocked, err := retentionAccountObligations(ctx, tx, id, now)
	if err != nil {
		return nil, err
	}
	reason := "open_escrow"
	var continuity, restricted bool
	if err = tx.QueryRowContext(ctx, `SELECT u.retention_restricted_at IS NOT NULL,u.role='admin' AND NOT EXISTS(SELECT 1 FROM users other WHERE other.id<>u.id AND other.role='admin' AND other.closed_at IS NULL AND other.suspended_at IS NULL AND other.password_hash<>'' AND other.retention_restricted_at IS NULL AND other.deletion_requested_at IS NULL AND (other.inactivity_notified_at IS NULL OR other.inactivity_notified_at>$2::timestamptz-interval '30 days') FOR SHARE NOWAIT) FROM users u WHERE u.id=$1`, id, now).Scan(&restricted, &continuity); err != nil {
		return nil, err
	}
	if continuity {
		blocked = true
		reason = "administrator_continuity"
	}
	if blocked {
		if _, err = tx.ExecContext(ctx, `INSERT INTO retention_account_exceptions(user_id,reason,first_seen,last_checked,review_due)
		 SELECT $1,$2,COALESCE(profile_scrubbed_at,$3),$3,COALESCE(profile_scrubbed_at,$3)+interval '30 days' FROM users WHERE id=$1
		 ON CONFLICT(user_id) DO UPDATE SET reason=EXCLUDED.reason,last_checked=EXCLUDED.last_checked,resolved_at=NULL`, id, reason, now); err != nil {
			return nil, err
		}
		if restricted {
			return nil, nil
		}
		return &retentionTombstone{Kind: "account_restrict", ID: id, At: now}, nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE retention_account_exceptions SET resolved_at=COALESCE(resolved_at,$2),last_checked=$2 WHERE user_id=$1`, id, now); err != nil {
		return nil, err
	}
	floors, err := prepareRetentionAccountKeyFloors(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return &retentionTombstone{Kind: "account_close", ID: id, At: now, KeyFloors: floors}, nil
}

func reviewRetentionAccountException(ctx context.Context, tx *sql.Tx, id, owner string) error {
	if owner == "" {
		return fail(400, "Record the named preservation owner.")
	}
	res, err := tx.ExecContext(ctx, `UPDATE retention_account_exceptions SET owner=$2,review_due=now()+interval '30 days',last_checked=now() WHERE user_id=$1 AND resolved_at IS NULL`, id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fail(404, "Open account retention exception not found.")
	}
	return nil
}

func (a *App) applyRetentionAccountFloors(ctx context.Context, tx *sql.Tx, t retentionTombstone) error {
	for _, f := range t.KeyFloors {
		if _, err := tx.ExecContext(ctx, `INSERT INTO retention_escrow_key_floors(key_hmac,next_index) VALUES($1,$2) ON CONFLICT(key_hmac) DO UPDATE SET next_index=GREATEST(retention_escrow_key_floors.next_index,EXCLUDED.next_index)`, f.KeyHMAC, f.NextIndex); err != nil {
			return err
		}
	}
	// Do not discard an unexplained index floor without a root certificate.
	// Existing escrow references keep their key material until order retirement.
	for _, f := range t.KeyFloors {
		if _, err := tx.ExecContext(ctx, `DELETE FROM escrow_index_floors x WHERE x.user_id=$1 AND EXISTS(SELECT 1 FROM user_escrow_keys k WHERE k.user_id=x.user_id AND k.xpub_hmac=$2) AND NOT EXISTS(SELECT 1 FROM escrow_pubkeys p WHERE p.user_id=x.user_id)`, t.ID, f.KeyHMAC); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_escrow_keys k WHERE k.user_id=$1 AND k.xpub_hmac=$2 AND NOT EXISTS(SELECT 1 FROM escrow_pubkeys p WHERE p.user_id=k.user_id)`, t.ID, f.KeyHMAC); err != nil {
			return err
		}
	}
	return nil
}
