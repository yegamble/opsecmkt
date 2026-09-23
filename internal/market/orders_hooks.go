package market

import (
	"context"
	"database/sql"
	"strings"
	"unicode/utf8"
)

// P3 Orders: side effects that must happen in the same transaction as a state change.

func init() {
	registerTransitionHook(stockHook)
	registerTransitionHook(autoDeliveryHook)
}

// stockHook reserves one unit when a buyer requests payment and returns it when a reserved order is
// cancelled. Drafts never hold stock.
func stockHook(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
	switch {
	case from == stateDraft && to == stateAwaitingPayment:
		res, err := tx.ExecContext(ctx, "UPDATE products SET stock=stock-1 WHERE id=$1 AND stock>0 AND NOT archived", o.ProductID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fail(409, "This listing is out of stock or no longer available")
		}
	case to == stateCancelled && (from == stateAwaitingPayment || from == statePaid):
		_, err := tx.ExecContext(ctx, "UPDATE products SET stock=stock+1 WHERE id=$1", o.ProductID)
		return err
	}
	return nil
}

// autoDeliveryHook releases a digital listing's stored delivery content as soon as the payment watcher
// marks the order paid, by recording the delivery and moving paid -> delivered as the system actor.
// Content that cannot be delivered automatically (blank or over the limit) leaves the order paid for
// the vendor to deliver by hand; it never blocks the payment transition.
func autoDeliveryHook(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
	if to != statePaid || o.Kind != "digital" {
		return nil
	}
	var content string
	if err := tx.QueryRowContext(ctx, "SELECT delivery_content FROM products WHERE id=$1", o.ProductID).Scan(&content); err != nil {
		return err
	}
	if strings.TrimSpace(content) == "" || utf8.RuneCountInString(content) > maxDeliveryChars {
		return nil
	}
	next, err := a.transition(ctx, tx, o.ID, statePaid, stateDelivered, nil, "Automatic delivery after confirmed payment")
	if err != nil {
		return err
	}
	if err = insertDelivery(ctx, tx, o.ID, content); err != nil {
		return err
	}
	o.State, o.Status = next.State, next.Status
	return nil
}

func insertDelivery(ctx context.Context, tx *sql.Tx, orderID, content string) error {
	res, err := tx.ExecContext(ctx, "INSERT INTO deliveries(order_id,content) VALUES($1,$2) ON CONFLICT (order_id) DO NOTHING", orderID, content)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fail(409, "Content was already delivered for this order")
	}
	return nil
}
