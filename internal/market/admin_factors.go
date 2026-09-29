package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ResetAdminFactors is the operator's break-glass for an administrator who lost their second factors, run on the
// host with database access (cmd/server -reset-admin-factors HANDLE, A-137): after a SETUP_TOKEN/APP_SECRET_KEY
// rotation the stored TOTP secret can no longer be read, and the web reset (adminResetFactorsAction) refuses
// administrators. In one transaction under advisory lock 782493 it does what the web reset does to the account:
// turns TOTP off (secret, pending secret, step counter, recovery codes and any unrevealed codes) and PGP sign-in
// off, keeping the PGP key and its verification, ends every session and pending sign-in, audits "Second factors
// reset by host command: …" and notifies the account. It refuses an account that is not an administrator, an
// unknown handle, an administrator with no second factor enrolled and, like the other host tools, a database
// recording migrations this binary does not include. It returns a summary of the change.
func ResetAdminFactors(ctx context.Context, db *sql.DB, handle string) (string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err = lockKnownSchema(ctx, tx); err != nil {
		return "", err
	}
	// Pending sign-ins, then recovery codes, go before the account row is locked, in the order /challenge takes
	// them (A-154), as in the web reset. Only -set-role, under the lock held now, changes whether an account is an
	// administrator, so the role read here stays true; a refusal below rolls this back.
	for _, q := range []string{
		"DELETE FROM pending_logins WHERE user_id IN (SELECT id FROM users WHERE handle=$1 AND role='admin')",
		"DELETE FROM recovery_codes WHERE user_id IN (SELECT id FROM users WHERE handle=$1 AND role='admin')",
	} {
		if _, err = tx.ExecContext(ctx, q, handle); err != nil {
			return "", err
		}
	}
	var id, role string
	var totp, pgp bool
	err = tx.QueryRowContext(ctx, "SELECT id,role,totp_enabled,pgp_2fa FROM users WHERE handle=$1 FOR UPDATE", handle).Scan(&id, &role, &totp, &pgp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no account has the handle %q (handles are exact and case-sensitive)", handle)
	}
	if err != nil {
		return "", err
	}
	if role != "admin" {
		return "", fmt.Errorf("%q is a %s account, not an administrator; an administrator resets its second factors on the web under Admin → Reset a user's second factors", handle, role)
	}
	var what []string
	if totp {
		what = append(what, "TOTP and recovery codes")
	}
	if pgp {
		what = append(what, "PGP sign-in")
	}
	if len(what) == 0 {
		return "", fmt.Errorf("administrator %q has no second factor enrolled; nothing was changed (it signs in with its password alone)", handle)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE users SET totp_enabled=false,totp_secret='',totp_pending='',totp_last_step=0,recovery_reveal='',recovery_reveal_until=NULL,pgp_2fa=false WHERE id=$1", id); err != nil {
		return "", err
	}
	ended, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=$1", id)
	if err != nil {
		return "", err
	}
	n, _ := ended.RowsAffected()
	off := strings.Join(what, " and ")
	sessions := "ended " + strconv.FormatInt(n, 10) + " session(s) and any pending sign-ins"
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", id, "Second factors reset by host command: "+off+" turned off; "+sessions); err != nil {
		return "", err
	}
	note := "The operator reset your two-factor sign-in on the host (" + off + " turned off) and signed you out everywhere. Set up TOTP or PGP sign-in again from your account."
	if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), id, note); err != nil {
		return "", err
	}
	return "Second factors of administrator " + handle + " reset: " + off + " turned off; " + sessions, tx.Commit()
}
