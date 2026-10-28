package market

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

// StaffRosterEntry is private to the administrator desk, independently of public staff-list choices.
// SecondFactor names the enrolled sign-in factors, or says that staff actions are refused without one (A-706).
type StaffRosterEntry struct{ Handle, Role, Suspended, KeyStatus, SecondFactor string }

// staffWithoutFactorSQL counts the active staff accounts that A-706 refuses staff actions (no TOTP, no PGP sign-in).
const staffWithoutFactorSQL = `SELECT count(*) FROM users WHERE role IN ('admin','moderator') AND suspended_at IS NULL AND closed_at IS NULL AND NOT (totp_enabled OR pgp_2fa)`

func init() { registerLoader("admin", loadStaffRoster) }

func loadStaffRoster(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" {
		return nil
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var factor bool
	err = tx.QueryRowContext(ctx, `SELECT totp_enabled OR pgp_2fa FROM users WHERE id=$1 AND role='admin' AND suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL`, d.User.ID).Scan(&factor)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var total, withoutFactor int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role IN ('admin','moderator')`).Scan(&total); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, staffWithoutFactorSQL).Scan(&withoutFactor); err != nil {
		return err
	}
	paging := activityPagingWithKey("/admin/users", "staff_page", total, r.URL.Query().Get("staff_page"), nil)
	rows, err := tx.QueryContext(ctx, `SELECT handle,role,COALESCE(to_char(suspended_at AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"'),''),pgp,
		concat_ws(' and ',CASE WHEN totp_enabled THEN 'TOTP' END,CASE WHEN pgp_2fa THEN 'PGP sign-in' END) FROM users WHERE role IN ('admin','moderator') ORDER BY CASE role WHEN 'admin' THEN 0 ELSE 1 END,handle,id LIMIT $1 OFFSET $2`, activityPageSize, paging.Offset())
	if err != nil {
		return err
	}
	defer rows.Close()
	entries := make([]StaffRosterEntry, 0, activityPageSize)
	now := time.Now()
	for rows.Next() {
		var v StaffRosterEntry
		var pgp string
		if err = rows.Scan(&v.Handle, &v.Role, &v.Suspended, &pgp, &v.SecondFactor); err != nil {
			return err
		}
		if v.SecondFactor == "" {
			v.SecondFactor = "None: staff actions refused"
		}
		v.KeyStatus = "No saved key"
		if pgp != "" {
			v.KeyStatus = "Cannot receive encrypted messages"
			if key, _, _, parseErr := parseCanonicalPublicKey(pgp); parseErr == nil && canReceiveEncrypted(key, now) {
				v.KeyStatus = "Can receive encrypted messages"
			}
		}
		entries = append(entries, v)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	d.StaffRoster, d.StaffRosterPaging = entries, paging
	d.StaffWithoutFactor, d.StaffFactorMissing = withoutFactor, !factor
	return nil
}
