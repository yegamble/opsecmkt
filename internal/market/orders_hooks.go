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
		// A checkout of the same listing may hold its row while it waits on the wallet (payAction).
		if err := waitForWalletLocks(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE products SET stock=stock+1 WHERE id=$1", o.ProductID)
		return err
	}
	return nil
}

// autoDeliveryHook releases a digital listing's stored delivery content as soon as the payment watcher
// marks the order paid, by recording the delivery and moving paid -> delivered as the system actor.
// Content that cannot be delivered automatically (blank or over the limit) leaves the order paid for
// the vendor to deliver by hand; it never blocks the payment transition. Neither is a listing an administrator
// took down (A-163) delivered: the order stays paid with a history note, for the vendor to deliver or cancel.
func autoDeliveryHook(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
	if to != statePaid || o.Kind != "digital" {
		return nil
	}
	var content string
	var takenDown bool
	// FOR KEY SHARE waits for a takedown in progress (it holds the listing row FOR UPDATE) and then reads its result,
	// without blocking stock reservations on the same listing.
	if err := tx.QueryRowContext(ctx, "SELECT delivery_content,admin_archive_reason IS NOT NULL FROM products WHERE id=$1 FOR KEY SHARE", o.ProductID).Scan(&content, &takenDown); err != nil {
		return err
	}
	if strings.TrimSpace(content) == "" || utf8.RuneCountInString(content) > maxDeliveryChars {
		return nil
	}
	if takenDown {
		note := "Not delivered automatically: an administrator took this listing down. The vendor can deliver by hand or cancel and refund."
		switch {
		case o.Escrow && o.Currency == "XMR": // a Monero escrow's refund is built and sent by the market alone (P3-R8)
			note = "Not delivered automatically: an administrator took this listing down. The vendor can deliver by hand, or cancel: the escrow is then refunded to the buyer's refund address by a transaction the market builds and sends."
		case o.Escrow: // cancelling a funded escrow order requests its refund, a settlement (escrowSettleTerminal)
			note = "Not delivered automatically: an administrator took this listing down. The vendor can deliver by hand, or cancel: the escrow is then refunded to the buyer by a settlement the buyer or the vendor signs."
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, statePaid, note)
		return err
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
