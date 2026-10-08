package market

import (
	"context"
	"net/http"
)

func init() { registerLoader("account", recentSignInsLoader) }

func recentSignInsLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT action,to_char(created,'YYYY-MM-DD HH24:MI "UTC"') FROM audit_events
		WHERE user_id=$1 AND (action='Signed in' OR action LIKE 'Signed in with %')
		ORDER BY created DESC,id DESC LIMIT 5`, d.User.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var event Event
		if err = rows.Scan(&event.Action, &event.Created); err != nil {
			return err
		}
		d.RecentSignIns = append(d.RecentSignIns, event)
	}
	return rows.Err()
}
