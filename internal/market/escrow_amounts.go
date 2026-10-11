package market

import (
	"context"
	"database/sql"
	"math"
)

// Terminal escrow transitions request their settlement inside the order transaction.
func init() { registerTransitionHook(paymentsTransitionHook) }

// Minimum automatic payouts (A-99, A-232), in atomic units. A payout whose counted sum is below its floor is never
// queued: enqueuePayout flags its deposits for staff review once instead (flagBelowFloor). Every payout on both
// coins is sent with the network fee taken from the amount (sendPayouts, SendFeeFromAmount), so no payout costs the
// market's pooled wallet its fee; the floors keep the fee from swallowing the payout.
//
// minPayoutBTC applies to every Bitcoin payout (release, refund or return). Bitcoin Core's sendtoaddress is called
// with subtractfeefromamount, so an amount at or below the fee can never be sent: on regtest a 3,000-satoshi payout
// came out 6 satoshi negative after the fee while 5,000 was sent. 10,000 satoshi (0.0001 BTC) clears that fee about
// three times over. It removes the never-sendable class, not every failure: fees move, so a payout just above the
// floor can still fail and is requeued by an administrator as before. Listings cannot be priced below it in BTC
// (validateListing).
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

// payoutFloor is the smallest sum enqueuePayout queues for a payout of kind ("release", "refund" or "return") in
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

// payoutBasis is the one rule for what a payout of o pays (A-161): enqueuePayout queues it, and the resolve,
// complete and paid-cancel forms state it. Credited deposits count even if now conflicted (the payout is then
// held, not dropped); others need the confirmation threshold of the currency's provider. A configured provider
// that is only unavailable in this process (node down, e.g. after a restart) keeps its threshold, applied to the
// confirmations last recorded by the watcher, as the post-funding notice promises. With no provider, or a
// disabled one (refused by the network guard), only deposits already credited count. Locked (unlock_time)
// transfers never count, nor do deposits first recorded after the order closed (payments.after_close, A-211:
// never refunded automatically; settleOrder flags them), nor do deposits staff recorded as refunded by hand (A-31:
// payments.disposition, e.g. a deposit below the minimum automatic payout that a later deposit would otherwise
// lift over it). Credited deposits below holdBelow hold the payout (conflicted; with a provider,
// re-confirming). lock row-locks the counted deposits (FOR UPDATE, for enqueuePayout).
func (a *App) payoutBasis(ctx context.Context, q rowsQuerier, o *Order, lock bool) (ids []int64, sum, holdBelow int64, err error) {
	threshold := int64(math.MaxInt64)
	holdBelow = 0
	query := "SELECT id,amount FROM payments WHERE order_id=$1 AND NOT locked AND NOT after_close AND disposition<>'refunded' AND (credited OR confirmations>=$2)"
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

type payoutPart struct {
	kind, recipient, who string
	amount               int64
	large                bool
}

// splitPayout divides an order's counted sum (payoutBasis) into its payouts. A refund returns the whole sum to the
// buyer. A release pays the vendor exactly the price and returns the excess to the buyer as a separate payout,
// unless the excess is below the minimum automatic payout (it then goes to the vendor with the release: sending
// dust back costs more than it is worth and invites fee abuse) or the price itself is below the release floor. A
// refund or return larger than the order price is marked large, a likely mistake an administrator checks first. The
// bound is the price: a refund of up to the price (and a return of an excess up to the price, so a payment of up to
// twice the price) is sent automatically, so the pooled wallet can swap coins up to a listing's price without
// review (largeHold).
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

// queuePayout inserts one payout of o (at most one of each kind per order) with its starting state: held for a
// suspended recipient (suspendedHold, plus largeHold when large), held when large (largeHold), held while a
// credited deposit is conflicted or re-confirming (heldReason, lifted by the watcher by itself, so it never
// replaces a hold only an administrator lifts), blocked without a payout address, otherwise pending. It writes the
// order note and tells the recipient what happens next; a large payout is also announced to administrators.
