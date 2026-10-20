package market

import (
	"context"
	"database/sql"
)

type notificationExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// insertNotification records an explicitly supplied order association. It never tries to recover an order ID from
// notification text: older and account-only notices remain unlinked.
func insertNotification(ctx context.Context, q notificationExecer, userID, body, orderID string) (sql.Result, error) {
	return insertScopedNotification(ctx, q, userID, body, orderID, "personal")
}

// Staff broadcasts keep their audience, so a later role change also revokes
// access to old notices and queued external deliveries. Personal notices (such
// as a moderator's own order) stay personal across role changes.
func insertScopedNotification(ctx context.Context, q notificationExecer, userID, body, orderID, audience string) (sql.Result, error) {
	return q.ExecContext(ctx, `INSERT INTO notifications(id,user_id,body,order_id,audience) VALUES($1,$2,$3,NULLIF($4,''),$5)`,
		randomToken(), userID, body, orderID, audience)
}

// notificationAudienceSQL is shared by the frontend and XMPP worker. Its aliases
// are fixed by callers, never supplied by a request.
func notificationAudienceSQL(n, u string) string {
	return `(` + n + `.audience='personal' OR (` + u + `.suspended_at IS NULL AND ` + u + `.closed_at IS NULL AND ` + u + `.retention_restricted_at IS NULL AND (` +
		n + `.audience='admin' AND ` + u + `.role='admin' OR ` + n + `.audience='dispute' AND ` + u + `.role IN ('admin','moderator') OR ` +
		n + `.audience='ticket' AND ` + u + `.role IN ('admin','moderator') AND ((` + n + `.ticket_id IS NULL AND ` + n + `.ticket_history_only) OR EXISTS(SELECT 1 FROM tickets nt WHERE nt.id=` + n + `.ticket_id AND ` + ticketReadableSQL("nt", u+".id", u+".role") + `)))))`
}
