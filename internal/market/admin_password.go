package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// ResetAdminPassword is the operator's break-glass for a forgotten administrator password, run on the host
// with database access (cmd/server -reset-admin-password). There is deliberately no web path to it. It
// applies the registration password rule, hashes at bcryptCost and, in one transaction, sets the new hash,
// ends every session and pending sign-in of that administrator and writes an audit row. Only an account
// whose role is admin is changed. Like server startup, it waits for the migration lock and refuses a
// database recording migrations this binary does not include; it never migrates or changes anything else.
func ResetAdminPassword(ctx context.Context, db *sql.DB, handle, password string) error {
	if !validPassword(password) {
		return errors.New("use a password of 12–72 bytes")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(782493)"); err != nil {
		return err
	}
	var installed bool
	if err = tx.QueryRowContext(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&installed); err != nil {
		return err
	}
	if !installed {
		return errors.New("this database has no OPSEC Market schema; start the server against it first")
	}
	if err = refuseUnknownMigrations(ctx, tx, list); err != nil {
		return err
	}
	var id, role string
	err = tx.QueryRowContext(ctx, "SELECT id,role FROM users WHERE handle=$1 FOR UPDATE", handle).Scan(&id, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no account has the handle %q (handles are exact and case-sensitive)", handle)
	}
	if err != nil {
		return err
	}
	if role != "admin" {
		return fmt.Errorf("%q is a %s account, not an administrator; this tool resets administrator passwords only", handle, role)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE users SET password_hash=$2 WHERE id=$1", id, hash); err != nil {
		return err
	}
	ended, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=$1", id)
	if err != nil {
		return err
	}
	n, _ := ended.RowsAffected()
	if _, err = tx.ExecContext(ctx, "DELETE FROM pending_logins WHERE user_id=$1", id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", id, "Password reset by the operator on the host; ended "+strconv.FormatInt(n, 10)+" session(s) and any pending sign-ins"); err != nil {
		return err
	}
	return tx.Commit()
}
