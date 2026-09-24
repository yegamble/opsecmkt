package market

import (
	"context"
	"database/sql"
	"math"
)

// Payout queueing on terminal transitions, inside the transition's transaction:
// completed -> release to the vendor; cancelled -> refund to the buyer; resolved -> the dispute outcome
// (release | refund). The amount is every credited or threshold-confirmed deposit for the order (payoutBasis);
// nothing is queued without one.

func init() { registerTransitionHook(paymentsTransitionHook) }

// heldReason is the error on a payout the watcher holds; the watcher recognises (and lifts) only holds with this
// exact text, and migration 056 rewrote the earlier wording to it.
const heldReason = "A credited deposit is conflicted or below the confirmation threshold; the payment watcher holds this payout and releases it by itself once the deposit confirms again."

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

// payoutSeen is the counted amount an actor confirmed for one order's payout (the form's amount_seen). Only the
// resolve, complete and paid-cancel actions set it (withPayoutSeen); the watcher's calls carry none.
type payoutSeen struct {
	orderID string
	amount  int64
}

type payoutSeenKey struct{}

func withPayoutSeen(ctx context.Context, orderID string, amount int64) context.Context {
	return context.WithValue(ctx, payoutSeenKey{}, payoutSeen{orderID, amount})
}

// checkPayoutSeen refuses (409) a payout of sum for o when the actor confirmed a different amount; the action's
// transaction then rolls back, so the order and any dispute stay as they were.
func checkPayoutSeen(ctx context.Context, o *Order, sum int64) error {
	if seen, ok := ctx.Value(payoutSeenKey{}).(payoutSeen); ok && seen.orderID == o.ID && seen.amount != sum {
		return fail(409, "The amount changed to "+amount(sum, currencyDecimals(o.Currency))+" "+o.Currency+" — review again. Nothing was changed.")
	}
	return nil
}

// priceDifference compares a counted amount with the order price: "0.009 BTC more than the price",
// "0.0005 BTC less than the price" or "exactly the price".
func priceDifference(sum, required int64, currency string) string {
	dec := currencyDecimals(currency)
	switch {
	case sum > required:
		return amount(sum-required, dec) + " " + currency + " more than the price"
	case sum < required:
		return amount(required-sum, dec) + " " + currency + " less than the price"
	}
	return "exactly the price"
}

// rowsQuerier is a *sql.Tx or *sql.DB.
type rowsQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// payoutBasis is the one rule for what a payout of o pays (A-161): enqueuePayout queues it, and the resolve,
// complete and paid-cancel forms state it. Credited deposits count even if now conflicted (the payout is then
// held, not dropped); others need the confirmation threshold of the currency's provider. A configured provider
// that is only unavailable in this process (node down, e.g. after a restart) keeps its threshold, applied to the
// confirmations last recorded by the watcher, as the post-funding notice promises. With no provider, or a
// disabled one (refused as not a test network), only deposits already credited count. Locked (unlock_time)
// transfers never count. Credited deposits below holdBelow hold the payout (conflicted; with a provider,
// re-confirming). lock row-locks the counted deposits (FOR UPDATE, for enqueuePayout).
func (a *App) payoutBasis(ctx context.Context, q rowsQuerier, o *Order, lock bool) (ids []int64, sum, holdBelow int64, err error) {
	threshold := int64(math.MaxInt64)
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
	query := "SELECT id,amount FROM payments WHERE order_id=$1 AND NOT locked AND (credited OR confirmations>=$2)"
	if lock {
		query += " FOR UPDATE"
	}
	rows, err := q.QueryContext(ctx, query, o.ID, threshold)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, amt int64
		if err = rows.Scan(&id, &amt); err != nil {
			return nil, 0, 0, err
		}
		ids, sum = append(ids, id), sum+amt
	}
	return ids, sum, holdBelow, rows.Err()
}

// enqueuePayout is idempotent (one payout per order). A resolved order whose dispute outcome is not yet set is
// skipped; the watcher retries it for 24 hours after the transition. An amount the actor confirmed
// (withPayoutSeen) must equal the amount counted here under the row locks, or the action is refused (409).
func (a *App) enqueuePayout(ctx context.Context, tx *sql.Tx, o *Order) error {
	var issued bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payment_addresses WHERE order_id=$1)", o.ID).Scan(&issued); err != nil {
		return err
	}
	if !issued {
		return checkPayoutSeen(ctx, o, 0)
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
	// The watcher's recordIncoming writes the ledger without the order lock, so the counted rows are locked here:
	// the payout is exactly the rows credited below, and none of them can change (turn conflicted) until this
	// transaction ends. recordIncoming's single-row autocommit statements wait for these locks without holding
	// any other, so there is no cycle. The confirmed amount is compared only after these locks.
	ids, sum, holdBelow, err := a.payoutBasis(ctx, tx, o, true)
	if err != nil {
		return err
	}
	if err = checkPayoutSeen(ctx, o, sum); err != nil || sum == 0 {
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
	// An overpayment is paid out whole to one party (no split, no automatic refund of the excess); say so.
	required, _ := parseAmount(o.Amount, currencyDecimals(o.Currency))
	if sum > required {
		note += " It includes " + priceDifference(sum, required, o.Currency) + "."
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, o.State, note); err != nil {
		return err
	}
	if kind == "release" && sum > required {
		body := "Order " + o.ID[:min(8, len(o.ID))] + ": the TESTNET release of " + label + " to the vendor includes " + priceDifference(sum, required, o.Currency) + ". The whole counted amount goes to one party; the excess is not refunded automatically."
		for _, uid := range []string{o.BuyerID, o.VendorID} {
			if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, body); err != nil {
				return err
			}
		}
	}
	if state == "blocked" {
		body := "Order " + o.ID[:min(8, len(o.ID))] + ": add a " + o.Currency + " payout address on your account page to receive " + label + " (TESTNET)."
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), recipient, body); err != nil {
			return err
		}
	}
	return nil
}
