package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AuditExportName preserves the whole-history download name and names an explicit nonzero lower bound.
func AuditExportName(from, upto int64) string {
	if from == 0 {
		return fmt.Sprintf("audit-events-upto-%d", upto)
	}
	return fmt.Sprintf("audit-events-from-%d-upto-%d", from, upto)
}

// ExportAudit is the host operator's read-only export. It checks the existing schema and settled upper ID,
// reads the selected history and signs it, without migrations, setup, wallets, HTTP or background work.
// Database errors are returned as classes, never driver text containing connection or record data.
func ExportAudit(ctx context.Context, db *sql.DB, from, upto int64) ([]byte, []byte, error) {
	if from < 0 || upto < 1 || from > upto {
		return nil, nil, errors.New("give a nonnegative from and positive upto, with from no greater than upto")
	}
	key, err := auditSigningKey()
	if err != nil {
		return nil, nil, err
	}
	if key == nil {
		return nil, nil, errors.New("AUDIT_SIGNING_KEY required")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, errors.New("database schema check failed: " + errorCause(err))
	}
	defer tx.Rollback()
	if err = lockKnownSchema(ctx, tx); err != nil {
		return nil, nil, errors.New("database schema check failed; use a binary matching the database migrations")
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, errors.New("database schema check failed: " + errorCause(err))
	}
	settled, err := settledAuditMax(ctx, db)
	if err != nil {
		return nil, nil, errors.New("database audit boundary check failed: " + errorCause(err))
	}
	if upto > settled {
		return nil, nil, fmt.Errorf("upto %d is beyond the latest recorded audit event (%d)", upto, settled)
	}
	data, err := auditExportRange(ctx, db, from, upto)
	if err != nil {
		return nil, nil, errors.New("database audit export failed: " + errorCause(err))
	}
	return data, auditSignature(key, data), nil
}
