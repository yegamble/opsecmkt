package market

import (
	"context"
	"database/sql"
)

// Terminal escrow transitions request their settlement inside the order transaction.
func init() { registerTransitionHook(paymentsTransitionHook) }

// Minimum payouts (A-99, A-232), in atomic units: the smallest output an escrow settlement pays (payoutFloor, used by
// planSettlement and the Monero settlement), and the lowest listing price (validateListing). The network fee is taken
// from the amount, and the floors keep the fee from swallowing it.
//
// minPayoutBTC applies to every Bitcoin output (release, refund or return). An amount at or below the fee can never be
// sent: on regtest a 3,000-satoshi output came out 6 satoshi negative after the fee while 5,000 was sent. 10,000
// satoshi (0.0001 BTC) clears that fee about three times over. It removes the never-sendable class, not every failure:
// fees move. Listings cannot be priced below it in BTC (validateListing).
//
// minPayoutXMR applies to every Monero payout (release, refund or return; A-232 made releases fee-deducted too,
// matching Bitcoin). A Monero transfer costs very roughly 0.00003-0.0001 XMR in fees at the default priority (more
// for one spending many small outputs), and below 0.001 XMR the fee would take several percent of the amount or
// more, so such a payout is left to staff and such an excess goes to the vendor with the release (splitPayout).
// Listings cannot be priced below it in XMR (validateListing).
const (
	minPayoutBTC = 10000      // 0.0001 BTC
	minPayoutXMR = 1000000000 // 0.001 XMR
)

// payoutFloor is the smallest payout of kind ("release", "refund" or "return") in currency. The floor is the same for
// every kind.
func payoutFloor(currency, kind string) int64 {
	switch currency {
	case "BTC":
		return minPayoutBTC
	case "XMR":
		return minPayoutXMR
	}
	return 0
}

// belowFloor states that a payout of sum is not sent automatically, or returns "" when sum reaches the floor.
func belowFloor(currency, kind string, sum int64) string {
	floor := payoutFloor(currency, kind)
	if sum <= 0 || sum >= floor {
		return ""
	}
	return "below the minimum automatic payout of " + amount(floor, currencyDecimals(currency)) + " " + currency
}

func paymentsTransitionHook(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
	if !o.Escrow || !isTerminal(to) {
		return nil
	}
	return a.escrowSettleTerminal(ctx, tx, o)
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

// countedDeposits is the sum of o's deposits that count toward its price on the forms that state an amount (the
// complete, paid-cancel and concession forms of an order without escrow): credited deposits, even if now conflicted,
// except locked (unlock_time) transfers, deposits first recorded after the order closed (payments.after_close, A-211)
// and deposits staff recorded as refunded by hand (A-31: payments.disposition).
func countedDeposits(ctx context.Context, q rowsQuerier, o *Order) (sum int64, err error) {
	rows, err := q.QueryContext(ctx, "SELECT amount FROM payments WHERE order_id=$1 AND credited AND NOT locked AND NOT after_close AND disposition<>'refunded'", o.ID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var amt int64
		if err = rows.Scan(&amt); err != nil {
			return 0, err
		}
		sum += amt
	}
	return sum, rows.Err()
}

type payoutPart struct {
	kind, recipient, who string
	amount               int64
	large                bool
}

// splitPayout divides an order's counted sum (countedDeposits) into its payouts. A refund returns the whole sum to the
// buyer. A release pays the vendor exactly the price and returns the excess to the buyer as a separate payout,
// unless the excess is below the minimum automatic payout (it then goes to the vendor with the release: sending
// dust back costs more than it is worth and invites fee abuse) or the price itself is below the release floor. A
// refund or return larger than the order price is marked large, a likely mistake an administrator checks first. The
// bound is the price: a refund of up to the price (and a return of an excess up to the price, so a payment of up to
// twice the price) needs no review.
//
// A price below the release floor exists only on listings priced before the listing floor applied (Bitcoin before
// A-99, Monero before A-232; new orders on them are refused, A-216, A-232). The vendor's share alone would be below
// the floor, so the whole sum stays one release; when its excess is more than the price, that release is marked
// large too, so the buyer's overpayment is not paid to the vendor without an administrator's review.
func splitPayout(kind string, o *Order, sum, required int64) []payoutPart {
	if kind == "refund" {
		return []payoutPart{{kind: "refund", recipient: o.BuyerID, who: "buyer", amount: sum, large: sum > required}}
	}
	excess := sum - required
	if belowFloor(o.Currency, "release", required) != "" {
		return []payoutPart{{kind: "release", recipient: o.VendorID, who: "vendor", amount: sum, large: excess > required}}
	}
	if excess <= 0 || belowFloor(o.Currency, "return", excess) != "" {
		return []payoutPart{{kind: "release", recipient: o.VendorID, who: "vendor", amount: sum}}
	}
	return []payoutPart{
		{kind: "release", recipient: o.VendorID, who: "vendor", amount: required},
		{kind: "return", recipient: o.BuyerID, who: "buyer", amount: excess, large: excess > required},
	}
}

// subFloorPriceExcess says why a release whose price is below the release floor (A-216) keeps the excess.
func subFloorPriceExcess(currency string) string {
	return "the price is below the minimum automatic payout of " + amount(payoutFloor(currency, "release"), currencyDecimals(currency)) + " " + currency +
		", so the amount paid over it cannot be returned separately"
}
