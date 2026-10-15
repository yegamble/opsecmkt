package market

import (
	"context"
	"database/sql"
)

// beginEscrowOrderTx keeps financial writes in the same order as party actions:
// lock the order before its escrows, settlements, bumps or payments. Retention
// invalidation triggers on those rows also write the order, even when its
// closure markers are already NULL. NO KEY UPDATE still allows foreign-key
// checks on the order while excluding another financial mutation.
func (a *App) beginEscrowOrderTx(ctx context.Context, orderID string) (*sql.Tx, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE", orderID); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// execEscrowOrder is the single-statement form of beginEscrowOrderTx. Return a
// result only after commit, so callers report changes that actually persisted.
func (a *App) execEscrowOrder(ctx context.Context, orderID, query string, args ...any) (sql.Result, error) {
	tx, err := a.beginEscrowOrderTx(ctx, orderID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}
