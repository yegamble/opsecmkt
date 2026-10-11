package market

import (
	"context"
	"database/sql"
)

// Terminal escrow transitions request their settlement inside the order transaction.
func init() { registerTransitionHook(paymentsTransitionHook) }

// Minimum outputs (A-99, A-232), in atomic units: the smallest output an escrow settlement pays (payoutFloor, used by
// planSettlement, planXMRSettlement and the fee bumps), and the lowest listing price (validateListing). The network fee
// is taken from the claimant's output, and the floors keep the fee from swallowing it.
//
// minPayoutBTC applies to every Bitcoin escrow output (release, refund, stray refund, market fee or return of an
// overpayment). An amount at or below the fee can never be sent: on regtest a 3,000-satoshi output came out 6 satoshi
// negative after the fee while 5,000 was sent. 10,000 satoshi (0.0001 BTC) clears that fee about three times over. It
// removes the never-sendable class, not every failure: fees move. Listings cannot be priced below it in BTC
// (validateListing).
//
// minPayoutXMR applies to every Monero escrow output (release, refund, stray refund, market fee or return of an
// overpayment; A-232 made releases fee-deducted too, matching Bitcoin). A Monero transfer costs very roughly
// 0.00003-0.0001 XMR in fees at the default priority (more for one spending many small outputs), and below 0.001 XMR the
// fee would take several percent of the amount or more, so a market fee or overpayment below it is added to the
// vendor's output (planXMRSettlement). Listings cannot be priced below it in XMR (validateListing).
const (
	minPayoutBTC = 10000      // 0.0001 BTC
	minPayoutXMR = 1000000000 // 0.001 XMR
)

// payoutFloor is the smallest escrow output of a settlement of kind ("release", "refund" or "stray_refund") in
// currency. The floor is the same for every kind.
func payoutFloor(currency, kind string) int64 {
	switch currency {
	case "BTC":
		return minPayoutBTC
	case "XMR":
		return minPayoutXMR
	}
	return 0
}

func paymentsTransitionHook(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
	if !o.Escrow || !isTerminal(to) {
		return nil
	}
	return a.escrowSettleTerminal(ctx, tx, o)
}

// payoutSeen is the counted amount an actor confirmed for one order's settlement (the form's amount_seen). Only the
// resolve, concede, complete and cancel actions set it (withPayoutSeen); the watcher's calls carry none.
type payoutSeen struct {
	orderID string
	amount  int64
}

type payoutSeenKey struct{}

func withPayoutSeen(ctx context.Context, orderID string, amount int64) context.Context {
	return context.WithValue(ctx, payoutSeenKey{}, payoutSeen{orderID, amount})
}

// checkPayoutSeen refuses (409) a settlement of sum for o when the actor confirmed a different amount; the action's
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

// escrowCountedSQL sums an escrow order's deposits as escrowSettleTerminal counts them (the amount_seen its forms state,
// checkPayoutSeen), and those of them left out (escrow_left_out), which a release or refund does not spend and a stray
// refund returns. A deposit to a shape-(b) escrow's sibling address is never the order's (A-456a).
const escrowCountedSQL = `SELECT COALESCE(sum(amount),0),COALESCE(sum(amount) FILTER (WHERE escrow_left_out),0) FROM payments
	WHERE order_id=$1 AND confirmations>=0 AND NOT locked AND NOT after_close
	AND NOT (escrow_left_out AND EXISTS (SELECT 1 FROM escrow_siblings s WHERE s.address=payments.address))`

// escrowExcess (A-508) states what a release of an escrow order's deposits (sum: those the release spends) does with
// the amount above the price, as planSettlement and planXMRSettlement build it, or "" when sum is not above the price:
// the excess is its own output of the release, paid in full to the buyer's refund address (the network fee comes from
// the vendor's output alone), unless it is below the minimum output (payoutFloor), when the release adds it to the
// vendor's output. A refund needs no such clause: it returns everything to the buyer's refund address.
func (a *App) escrowExcess(o *Order, sum int64) string {
	dec := currencyDecimals(o.Currency)
	required, _ := parseAmount(o.Amount, dec)
	if sum <= required {
		return ""
	}
	excess := "the " + a.escrowAmount(o, sum-required) + " paid above the price"
	if floor := payoutFloor(o.Currency, "release"); sum-required < floor {
		return excess + " is below the minimum output of " + amount(floor, dec) + " " + o.Currency + ", so the release adds it to the vendor's output"
	}
	return excess + " goes back in full to the buyer's refund address, as its own output of the release"
}

// escrowOverpaid (A-508) is the order page's notice for an escrow order whose coins are not settled yet (paid, shipped,
// delivered or disputed) and that holds more than the price: what a release and a refund each do with the excess
// (escrowExcess). It is "" for any other order.
func (a *App) escrowOverpaid(ctx context.Context, o *Order) (string, error) {
	if !o.Escrow || o.State != statePaid && o.State != stateShipped && o.State != stateDelivered && o.State != stateDisputed {
		return "", nil
	}
	var sum, left int64
	if err := a.db.QueryRowContext(ctx, escrowCountedSQL, o.ID).Scan(&sum, &left); err != nil {
		return "", err
	}
	back := a.escrowExcess(o, sum-left)
	if back == "" {
		return "", nil
	}
	required, _ := parseAmount(o.Amount, currencyDecimals(o.Currency))
	return "This escrow holds " + a.escrowAmount(o, sum-left-required) + " more than the price. On a release to the vendor, " + back +
		". A refund instead returns everything in this escrow (" + a.escrowAmount(o, sum-left) + "), less the network fee, to the buyer's refund address.", nil
}
