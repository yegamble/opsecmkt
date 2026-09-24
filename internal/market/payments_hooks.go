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

const heldReason = "A credited deposit is conflicted or below the confirmation threshold; payout held until it confirms again or a moderator reviews it."

// suspendedHoldPrefix starts the error on a payout held because its recipient's account is suspended (A-102):
// someone with the account's password may have changed its payout address. Suspending an account holds its
// unsent payouts with suspendedHold, and payouts queued for a suspended recipient start held with it. Only an
// administrator who confirms the payout address was checked releases it (payments_admin.go); restoring the
// account does not, and the watcher lifts only its own hold (heldReason).
const suspendedHoldPrefix = "Suspended account:"

const suspendedHold = suspendedHoldPrefix + " payout held when the recipient's account was suspended; an administrator must check the payout address before releasing it."

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
	// One payout per order: once queued, later deposits are never credited here (settleOrder flags those that
	// confirm as not paid out).
	var queued bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payouts WHERE order_id=$1)", o.ID).Scan(&queued); err != nil || queued {
		return err
	}
	// A configured provider that is only unavailable in this process (node down, e.g. after a restart) keeps its
	// threshold, applied to the confirmations last recorded by the watcher, as the post-funding notice promises.
	// With no provider, or a disabled one (refused as not a test network), only deposits already credited count.
	threshold := int64(math.MaxInt64)
	holdBelow := int64(0) // credited deposits below this hold the payout (conflicted; with a provider, re-confirming)
	a.payMu.RLock()
	p := a.payments[o.Currency]
	if u := a.unavailable[o.Currency]; p == nil && u != nil && !u.disabled {
		p = u.p
	}
	a.payMu.RUnlock()
	if p != nil {
		threshold = int64(p.Confirmations())
		holdBelow = threshold
	}
	// Credited deposits count even if now conflicted (the payout is then held, not dropped); others need the
	// threshold. Locked (unlock_time) transfers never count. The watcher's recordIncoming writes the ledger
	// without the order lock, so the counted rows are locked here: the payout is exactly the rows credited
	// below, and none of them can change (turn conflicted) until this transaction ends. recordIncoming's
	// single-row autocommit statements wait for these locks without holding any other, so there is no cycle.
	rows, err := tx.QueryContext(ctx, "SELECT id,amount FROM payments WHERE order_id=$1 AND NOT locked AND (credited OR confirmations>=$2) FOR UPDATE", o.ID, threshold)
	if err != nil {
		return err
	}
	var ids []int64
	var sum int64
	for rows.Next() {
		var id, amt int64
		if err = rows.Scan(&id, &amt); err != nil {
			rows.Close()
			return err
		}
		ids, sum = append(ids, id), sum+amt
	}
	rows.Close()
	if err = rows.Err(); err != nil || sum == 0 {
		return err
	}
	column := "payout_btc"
	if o.Currency == "XMR" {
		column = "payout_xmr"
	}
	// FOR SHARE waits for a suspension of the recipient in progress (it locks the account row and then holds
	// the account's payouts), so a payout queued meanwhile is either seen and held by it or queued held here.
	var address string
	var suspended bool
	if err = tx.QueryRowContext(ctx, "SELECT "+column+",suspended_at IS NOT NULL FROM users WHERE id=$1 FOR SHARE", recipient).Scan(&address, &suspended); err != nil {
		return err
	}
	// Every credited deposit counted above is row-locked, so this sees the confirmations it was counted with.
	var held bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payments WHERE order_id=$1 AND credited AND confirmations<$2)", o.ID, holdBelow).Scan(&held); err != nil {
		return err
	}
	state, errText := "pending", ""
	switch {
	case suspended:
		state, errText = "held", suspendedHold
	case held:
		state, errText = "held", heldReason
	case address == "":
		state = "blocked"
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,error) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (order_id) DO NOTHING RETURNING id`, o.ID, kind, recipient, o.Currency, sum, address, state, errText).Scan(&id)
	if err == sql.ErrNoRows {
		return nil // already queued
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE payments SET credited=true WHERE id=ANY($1)", ids); err != nil {
		return err
	}
	label := amount(sum, currencyDecimals(o.Currency)) + " " + o.Currency
	note := "Queued TESTNET " + kind + " of " + label + " to the " + who + " (single attempt)."
	switch state {
	case "blocked":
		note = "TESTNET " + kind + " of " + label + " to the " + who + " is blocked until the " + who + " saves a " + o.Currency + " payout address."
	case "held":
		note = "TESTNET " + kind + " of " + label + " to the " + who + " is held: a credited deposit is conflicted or re-confirming."
		if suspended {
			note = "TESTNET " + kind + " of " + label + " to the " + who + " is held until an administrator checks the payout address."
		}
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
