package market

import (
	"context"
	"database/sql"
	"time"
)

// A cleanup entry is protected operational inventory; it contains no order or
// participant lookup. Confirmation attests actual host cleanup, never a timer.
type RetentionWalletCleanup struct {
	Coin, WalletName                                 string
	DueAt, FirstRegisteredAt, LastChecked, ReviewDue time.Time
	Overdue, ReviewOverdue                           bool
}

func LoadRetentionWalletCleanup(ctx context.Context, db *sql.DB, now time.Time, limit int) ([]RetentionWalletCleanup, error) {
	if limit < 1 || limit > retentionEscrowBatch {
		limit = retentionEscrowBatch
	}
	rows, err := db.QueryContext(ctx, `SELECT coin,wallet_name,due_at,first_registered_at,last_checked,review_due FROM retention_wallet_cleanup WHERE confirmed_at IS NULL ORDER BY review_due,due_at,coin,wallet_name LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RetentionWalletCleanup
	for rows.Next() {
		var row RetentionWalletCleanup
		if err = rows.Scan(&row.Coin, &row.WalletName, &row.DueAt, &row.FirstRegisteredAt, &row.LastChecked, &row.ReviewDue); err != nil {
			return nil, err
		}
		row.Overdue = !now.Before(row.DueAt)
		row.ReviewOverdue = !now.Before(row.ReviewDue)
		result = append(result, row)
	}
	return result, rows.Err()
}

// Verified destruction of an exact original watch wallet removes its labels
// physically. This proof permits BTC-off startup after an old restore without
// resurrecting the destroyed wallet. A configured wallet is still read/scrubbed
// on every start; restoring an old copy cannot bypass that read.
func reconcileRetentionWalletAttestations(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `UPDATE retention_escrow_retired_addresses r SET label_checked=true FROM retention_wallet_cleanup c WHERE r.coin='BTC' AND NOT r.label_checked AND c.coin='BTC' AND c.confirmed_at IS NOT NULL AND r.wallet_hash=encode(sha256(convert_to(c.wallet_name,'UTF8')),'hex')`)
	return err
}

// Acquire before the fresh eligibility read, rather than only inside the final
// UPDATE trigger: a hold committed between an older read and an erase must be
// visible to that read under PostgreSQL's statement snapshots.
func lockRetentionXMRWallet(ctx context.Context, tx *sql.Tx, id int64) (bool, error) {
	var locked bool
	err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('retention-xmr-wallet:'||$1::bigint::text,0))`, id).Scan(&locked)
	return locked, err
}
