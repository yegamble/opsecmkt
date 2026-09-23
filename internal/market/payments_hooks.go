package market

import (
	"context"
	"database/sql"
	"math"
)

// Payout queueing on terminal transitions, inside the transition's transaction:
// completed -> release to the vendor; cancelled -> refund to the buyer; resolved -> the dispute outcome
// (release | refund). The amount is every credited or threshold-confirmed deposit for the order; nothing is
// queued without one.

func init() { registerTransitionHook(paymentsTransitionHook) }

func paymentsTransitionHook(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
	if !isTerminal(to) {
		return nil
	}
	return a.enqueuePayout(ctx, tx, o)
}

// enqueuePayout is idempotent (one payout per order). A resolved order whose dispute outcome is not yet set is
// skipped; the watcher retries it for 24 hours after the transition.
func (a *App) enqueuePayout(ctx context.Context, tx *sql.Tx, o *Order) error {
	var issued bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payment_addresses WHERE order_id=$1)", o.ID).Scan(&issued); err != nil || !issued {
		return err
	}
	var kind, recipient, who string
	switch o.State {
	case stateCompleted:
		kind, recipient, who = "release", o.VendorID, "vendor"
	case stateCancelled:
		kind, recipient, who = "refund", o.BuyerID, "buyer"
	case stateResolved:
		var outcome string
		err := tx.QueryRowContext(ctx, "SELECT outcome FROM disputes WHERE order_id=$1", o.ID).Scan(&outcome)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		switch outcome {
		case "release":
			kind, recipient, who = "release", o.VendorID, "vendor"
		case "refund":
			kind, recipient, who = "refund", o.BuyerID, "buyer"
		default:
			return nil
		}
	default:
		return nil
	}
	threshold := int64(math.MaxInt64) // provider removed: only deposits already credited count
	if p := a.payments[o.Currency]; p != nil {
		threshold = int64(p.Confirmations())
	}
	// Credited deposits count even if now conflicted (the payout is then held, not dropped); others need the threshold.
	const settled = "order_id=$1 AND (credited OR confirmations>=$2)"
	var sum int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(sum(amount),0) FROM payments WHERE "+settled, o.ID, threshold).Scan(&sum); err != nil || sum == 0 {
		return err
	}
	column := "payout_btc"
	if o.Currency == "XMR" {
		column = "payout_xmr"
	}
	var address string
	if err := tx.QueryRowContext(ctx, "SELECT "+column+" FROM users WHERE id=$1", recipient).Scan(&address); err != nil {
		return err
	}
	var held bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payments WHERE order_id=$1 AND credited AND confirmations<0)", o.ID).Scan(&held); err != nil {
		return err
	}
	state, errText := "pending", ""
	switch {
	case held:
		state, errText = "held", "A credited deposit is conflicted; payout held for review."
	case address == "":
		state = "blocked"
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,error) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (order_id) DO NOTHING RETURNING id`, o.ID, kind, recipient, o.Currency, sum, address, state, errText).Scan(&id)
	if err == sql.ErrNoRows {
		return nil // already queued
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE payments SET credited=true WHERE "+settled, o.ID, threshold); err != nil {
		return err
	}
	label := amount(sum, currencyDecimals(o.Currency)) + " " + o.Currency
	note := "Queued TESTNET " + kind + " of " + label + " to the " + who + " (single attempt)."
	switch state {
	case "blocked":
		note = "TESTNET " + kind + " of " + label + " to the " + who + " is blocked until the " + who + " saves a " + o.Currency + " payout address."
	case "held":
		note = "TESTNET " + kind + " of " + label + " to the " + who + " is held: a credited deposit is conflicted."
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, o.State, note); err != nil {
		return err
	}
	if state == "blocked" {
		body := "Order " + o.ID[:min(8, len(o.ID))] + ": add a " + o.Currency + " payout address on your account page to receive " + label + " (TESTNET)."
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), recipient, body); err != nil {
			return err
		}
	}
	return nil
}
