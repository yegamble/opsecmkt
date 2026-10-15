package market

// escrowUnchanged is everything a refused settling action must leave as it was on order.
func (x *escrowEnv) escrowUnchanged(order string) string {
	x.t.Helper()
	return agStr(x.testEnv, `SELECT o.state||' events='||(SELECT count(*) FROM order_events WHERE order_id=o.id)||
		' settlements='||(SELECT count(*) FROM settlements WHERE order_id=o.id)||
		' disputes='||coalesce((SELECT string_agg(status||':'||outcome,',') FROM disputes WHERE order_id=o.id),'')||
		' notifications='||(SELECT count(*) FROM notifications)||' receipts='||(SELECT count(*) FROM form_submissions)||
		' audit='||(SELECT count(*) FROM audit_events)
		FROM orders o WHERE o.id=$1`, order)
}
