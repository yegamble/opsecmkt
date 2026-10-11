package market

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

func init() { registerLoader("admin", loadAdminRuntime) }

func loadAdminRuntime(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" {
		return nil
	}
	var version int
	err := a.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT max(version) FROM schema_migrations),0) FROM users WHERE id=$1 AND role='admin' AND suspended_at IS NULL`, d.User.ID).Scan(&version)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	d.SchemaVersion = version
	if !a.startedAt.IsZero() {
		d.ServerStarted = a.startedAt.UTC().Format(time.RFC3339)
	}
	return nil
}
