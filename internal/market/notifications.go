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
	return q.ExecContext(ctx, `INSERT INTO notifications(id,user_id,body,order_id) VALUES($1,$2,$3,NULLIF($4,''))`,
		randomToken(), userID, body, orderID)
}
