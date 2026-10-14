package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// CheckRoleInput applies SetRole's rule for the new role before anything else is done.
func CheckRoleInput(role string) error {
	switch role {
	case "buyer", "vendor", "moderator", "admin":
		return nil
	}
	return fmt.Errorf("the role must be buyer, vendor, moderator or admin, not %q", role)
}

// SetRole is the operator's host command for changing any account's role (cmd/server -set-role HANDLE ROLE), and
// the only way to add or remove an administrator: the admin form never assigns or changes that role (A-156). In
// one transaction under advisory lock 782493 (so two runs happen one after the other) it refuses to remove the
// last administrator that is not suspended and to make a suspended account an administrator, sets the role, does
// what the admin form does for the new role (roleChanged: leaving staff removes the staff avatar, leaving vendor
// archives the listings) and audits "Role changed from OLD to NEW by host command" on the account. Removing an
// administrator also ends its pending sign-ins and sessions, as a suspension does. Like the other host tools it
// refuses a database recording migrations this binary does not include. It returns a summary of the change.
func SetRole(ctx context.Context, db *sql.DB, handle, role string) (string, error) {
	if err := CheckRoleInput(role); err != nil {
		return "", err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err = lockKnownSchema(ctx, tx); err != nil {
		return "", err
	}
	// Pending sign-ins of an administrator being removed go before the account row is locked, in the order
	// /challenge takes them (A-154). Only this command, under the lock held now, changes whether an account is an
	// administrator, so the role read here stays true; a refusal below rolls this back.
	if role != "admin" {
		if _, err = tx.ExecContext(ctx, "DELETE FROM pending_logins WHERE user_id IN (SELECT id FROM users WHERE handle=$1 AND role='admin')", handle); err != nil {
			return "", err
		}
	}
	var id, old string
	var suspended, closed, closing bool
	err = tx.QueryRowContext(ctx, "SELECT id,role,suspended_at IS NOT NULL,closed_at IS NOT NULL OR password_hash='',deletion_requested_at IS NOT NULL OR retention_restricted_at IS NOT NULL FROM users WHERE handle=$1 FOR UPDATE", handle).Scan(&id, &old, &suspended, &closed, &closing)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no account has the handle %q (handles are exact and case-sensitive)", handle)
	}
	if err != nil {
		return "", err
	}
	switch {
	case closed:
		return "", fmt.Errorf("%q is closed; register a new active account instead of restoring erased sign-in material", handle)
	case old == role:
		return "", fmt.Errorf("%q is already a %s account; nothing was changed", handle, role)
	case role == "admin" && suspended:
		return "", fmt.Errorf("%q is suspended; restore it under Admin → Suspend or restore an account before making it an administrator", handle)
	case role == "admin" && closing:
		return "", fmt.Errorf("%q is awaiting account closure; choose a new active account as replacement administrator", handle)
	}
	if old == "admin" {
		var others int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM users WHERE role='admin' AND suspended_at IS NULL AND closed_at IS NULL AND password_hash<>'' AND deletion_requested_at IS NULL AND retention_restricted_at IS NULL AND id<>$1", id).Scan(&others); err != nil {
			return "", err
		}
		if others == 0 {
			return "", fmt.Errorf("%q is the last administrator; make another account an administrator first", handle)
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE users SET role=$2 WHERE id=$1", id, role); err != nil {
		return "", err
	}
	summary := "Role of " + handle + " changed from " + old + " to " + role
	if old == "admin" {
		ended, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=$1", id)
		if err != nil {
			return "", err
		}
		n, _ := ended.RowsAffected()
		summary += "; ended " + strconv.FormatInt(n, 10) + " session(s) and any pending sign-ins"
	}
	// disputeEvidence reads this row's "Role changed from OLD to NEW" to know when the account left staff (A-294).
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", id, "Role changed from "+old+" to "+role+" by host command"); err != nil {
		return "", err
	}
	more, err := roleChanged(ctx, tx, id, old, role, "host command")
	if err != nil {
		return "", err
	}
	return summary + more, tx.Commit()
}
