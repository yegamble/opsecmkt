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
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockKnownSchema(ctx, tx); err != nil {
		return err
	}
	// Pending sign-ins go before the account row is locked, in the order /challenge takes them (A-154); a refusal
	// below rolls this back.
	if _, err = tx.ExecContext(ctx, "DELETE FROM pending_logins WHERE user_id IN (SELECT id FROM users WHERE handle=$1)", handle); err != nil {
		return err
	}
	var id, role string
	var closed bool
	err = tx.QueryRowContext(ctx, "SELECT id,role,closed_at IS NOT NULL OR password_hash='' FROM users WHERE handle=$1 FOR UPDATE", handle).Scan(&id, &role, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no account has the handle %q (handles are exact and case-sensitive)", handle)
	}
	if err != nil {
		return err
	}
	if closed {
		return fmt.Errorf("%q is closed; register a fresh account instead of recreating erased credentials", handle)
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
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", id, "Password reset by the operator on the host; ended "+strconv.FormatInt(n, 10)+" session(s) and any pending sign-ins"); err != nil {
		return err
	}
	return tx.Commit()
}

// lockKnownSchema starts a host tool's transaction: it takes advisory lock 782493 (startup migrations and
// first-run setup) and refuses a database without the application schema or recording migrations this binary
// does not include, before anything is changed.
func lockKnownSchema(ctx context.Context, tx *sql.Tx) error {
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(782493)"); err != nil {
		return err
	}
	var schema bool
	if err = tx.QueryRowContext(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&schema); err != nil {
		return err
	}
	if !schema {
		return errors.New("this database has no OPSEC Market schema; start the server against it first")
	}
	return refuseUnknownMigrations(ctx, tx, list)
}
