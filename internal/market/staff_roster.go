package market

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

// StaffRosterEntry is private to the administrator desk, independently of public staff-list choices.
type StaffRosterEntry struct{ Handle, Role, Suspended, KeyStatus string }

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
	var allowed bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND role='admin' AND suspended_at IS NULL)`, d.User.ID).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	var total int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role IN ('admin','moderator')`).Scan(&total); err != nil {
		return err
	}
	paging := activityPagingWithKey("/admin", "staff_page", total, r.URL.Query().Get("staff_page"), nil)
	rows, err := tx.QueryContext(ctx, `SELECT handle,role,COALESCE(to_char(suspended_at AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"'),''),pgp FROM users WHERE role IN ('admin','moderator') ORDER BY CASE role WHEN 'admin' THEN 0 ELSE 1 END,handle,id LIMIT $1 OFFSET $2`, activityPageSize, paging.Offset())
	if err != nil {
		return err
	}
	defer rows.Close()
	entries := make([]StaffRosterEntry, 0, activityPageSize)
	now := time.Now()
	for rows.Next() {
		var v StaffRosterEntry
		var pgp string
		if err = rows.Scan(&v.Handle, &v.Role, &v.Suspended, &pgp); err != nil {
			return err
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
	return nil
}
