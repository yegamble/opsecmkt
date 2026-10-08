package market

import (
	"context"
	"database/sql"
	"math"
	"strings"
)

// Payout queueing on terminal transitions, inside the transition's transaction:
// completed -> release to the vendor (the price; any excess is returned to the buyer, see splitPayout);
// cancelled -> refund to the buyer; resolved -> the dispute outcome (release | refund). The amount is every credited or threshold-confirmed deposit for the order (payoutBasis);
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

// largeHoldPrefix starts the hold on a refund or return larger than the order price (splitPayout). Only an
// administrator who confirms the check releases it (payments_admin.go); the watcher never lifts it. It follows
// suspendedHold when both apply, and a restore from backup keeps it after its own prefix (scripts/restore.sh), so
// the prefix must never change; rows written with an earlier text after it still match.
const largeHoldPrefix = "Larger than the order price:"

// largeHold catches likely mistakes, not swaps: refunds and returns up to the order price are sent automatically
// (pay and cancel, underpaid expiry, cancel then send), so the pooled wallet can swap coins up to a listing's price
// without review. A refund is held when it exceeds the price; a return only when the excess itself exceeds it.
const largeHold = largeHoldPrefix + " held as a likely mistake; an administrator checks it before it is sent. Refunds and returns up to the order price are sent automatically."

// heldLarge reports a payout held because it is larger than the order price, including under a suspension or
// restore hold.
func heldLarge(state, payoutErr string) bool {
	return state == "held" && strings.Contains(payoutErr, largeHoldPrefix)
}

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
	if !isTerminal(to) {
		return nil
	}
	return a.settleTerminal(ctx, tx, o)
}

// settleTerminal is the one settlement dispatch for an order in a terminal state (ARCH-1): the transition hook
// and the watcher's retry (settleOrder) both call it, and it is the only caller of enqueuePayout. A pooled order
// queues its payout as before. An escrow order (Order.Escrow) never gets a payouts row (ES-1, invariant 4; the
// database refuses one, migration 074): its funds are in its own escrow, and a pooled payout would pay it from the
// market's wallet while the escrow stays claimable. Its settlement, a transaction spending the escrow, is requested
// here when the order is cancelled holding coins (a refund, escrowSettleTerminal) and built by the escrow watcher.
func (a *App) settleTerminal(ctx context.Context, tx *sql.Tx, o *Order) error {
	if o.Escrow {
		return a.escrowSettleTerminal(ctx, tx, o)
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
// disabled one (refused by the network guard), only deposits already credited count. Locked (unlock_time)
// transfers never count, nor do deposits first recorded after the order closed (payments.after_close, A-211:
// never refunded automatically; settleOrder flags them), nor do deposits staff recorded as refunded by hand (A-31:
// payments.disposition, e.g. a deposit below the minimum automatic payout that a later deposit would otherwise
// lift over it). Credited deposits below holdBelow hold the payout (conflicted; with a provider,
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

// enqueuePayout is idempotent (one settlement per order: a release, with a return of any excess, or a refund). A resolved order whose dispute outcome is not yet set is
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
	var kind, who string
	switch o.State {
	case stateCompleted:
		kind, who = "release", "vendor"
	case stateCancelled:
		kind, who = "refund", "buyer"
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
			kind, who = "release", "vendor"
		case "refund":
			kind, who = "refund", "buyer"
		default:
			return nil
		}
	default:
		return nil
	}
	// One settlement per order (a release and its return, or a refund): once queued, later deposits are never
	// credited here (settleOrder flags those that confirm as not paid out).
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
	if below := belowFloor(o.Currency, kind, sum); below != "" {
		return a.flagBelowFloor(ctx, tx, o, kind, who, below, ids, sum)
	}
	// FOR SHARE waits for a suspension of the recipient in progress (it locks the account row and then holds
	// the account's payouts), so a payout queued meanwhile is either seen and held by it or queued held here. It
	// also waits for a checkout by a refunded buyer, which holds the row while it waits on the wallet (payAction).
	if err = waitForWalletLocks(ctx, tx); err != nil {
		return err
	}
	// Every credited deposit counted above is row-locked, so this sees the confirmations it was counted with.
	var held bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payments WHERE order_id=$1 AND credited AND confirmations<$2)", o.ID, holdBelow).Scan(&held); err != nil {
		return err
	}
	required, _ := parseAmount(o.Amount, currencyDecimals(o.Currency))
	parts := splitPayout(kind, o, sum, required)
	for _, part := range parts {
		if err = a.queuePayout(ctx, tx, o, part, held); err != nil {
			return err
		}
	}
	// Deposits flagged earlier as below the floor are paid now: their flag closes (below_floor).
	if _, err = tx.ExecContext(ctx, "UPDATE payments SET credited=true,below_floor=false WHERE id=ANY($1)", ids); err != nil {
		return err
	}
	// An excess too small to return goes to the vendor with the release; both parties are told. So are they when
	// the excess is not split off because the price itself is below the release floor, and the release is held.
	// Every payout has the network fee taken from it (A-232), so the notice says so (A-250).
	if len(parts) == 1 && parts[0].kind == "release" && sum > required {
		dec := currencyDecimals(o.Currency)
		body := "Order " + o.ID[:min(8, len(o.ID))] + ": the " + a.netTag() + " release of " + amount(sum, dec) + " " + o.Currency + " to the vendor includes " + priceDifference(sum, required, o.Currency) + ", below the minimum automatic payout of " + amount(payoutFloor(o.Currency, "return"), dec) + " " + o.Currency + ", so it is not returned; the network fee is taken from the release."
		if parts[0].large {
			body = "Order " + o.ID[:min(8, len(o.ID))] + ": the " + a.netTag() + " release of " + amount(sum, dec) + " " + o.Currency + " to the vendor includes " + priceDifference(sum, required, o.Currency) + "; " + subFloorPriceExcess(o.Currency) + ", so an administrator reviews the release before it is sent; the network fee is taken from the release."
		}
		for _, uid := range []string{o.BuyerID, o.VendorID} {
			if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, body); err != nil {
				return err
			}
		}
	}
	return nil
}

// payoutPart is one payout of a terminal order: its kind ("release", "refund" or "return"), recipient and amount.
// large marks a refund or return of more than the order price, or a release that keeps an excess of more than a
// price below the release floor (A-216), held for an administrator (largeHold).
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
func (a *App) queuePayout(ctx context.Context, tx *sql.Tx, o *Order, part payoutPart, held bool) error {
	column := "payout_btc"
	if o.Currency == "XMR" {
		column = "payout_xmr"
	}
	var address string
	var suspended bool
	if err := tx.QueryRowContext(ctx, "SELECT "+column+",suspended_at IS NOT NULL FROM users WHERE id=$1 FOR SHARE", part.recipient).Scan(&address, &suspended); err != nil {
		return err
	}
	state, errText := "pending", ""
	switch {
	case suspended && part.large:
		state, errText = "held", suspendedHold+" "+largeHold
	case suspended:
		state, errText = "held", suspendedHold
	case part.large:
		state, errText = "held", largeHold
	case held:
		state, errText = "held", heldReason
	case address == "":
		state = "blocked"
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,error) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (order_id,kind) DO NOTHING RETURNING id`, o.ID, part.kind, part.recipient, o.Currency, part.amount, address, state, errText).Scan(&id)
	if err == sql.ErrNoRows {
		return nil // already queued
	}
	if err != nil {
		return err
	}
	short := o.ID[:min(8, len(o.ID))]
	label := amount(part.amount, currencyDecimals(o.Currency)) + " " + o.Currency
	what := a.netTag() + " " + part.kind + " of " + label
	if part.kind == "return" {
		what = a.netTag() + " return of " + label + " (paid over the price)"
	}
	// Every payout is sent with the network fee taken from its amount (A-232): each note and notice stating the
	// amount says so (A-250).
	fee := "; the network fee is taken from the " + part.kind + "."
	note := "Queued " + what + " to the " + part.who + " (single attempt)"
	switch {
	case suspended:
		note = what + " to the " + part.who + " is held until an administrator checks the payout address"
	case part.large:
		note = what + " to the " + part.who + " is held: it is more than the order price, so an administrator checks it before it is sent"
	case held:
		note = what + " to the " + part.who + " is held: a credited deposit is conflicted or re-confirming"
	case address == "":
		note = what + " to the " + part.who + " is blocked until the " + part.who + " saves a " + o.Currency + " payout address"
	}
	note += fee
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, o.State, note); err != nil {
		return err
	}
	var body string
	switch {
	case part.large:
		body = "Order " + short + ": the " + what + " is more than the order price, so it is held until an administrator checks it" + fee
		if suspended {
			body += " An administrator checks the payout address with you before releasing it; if it is still held after a few days, message an administrator."
		} else if address != "" {
			body += " Nothing is needed from you; if it is still held after a few days, message an administrator."
		} else {
			body += " Save a " + o.Currency + " payout address on your account page; if it is still held after a few days, message an administrator."
		}
		body += " Contact the market staff if you need help."
	case address == "" && !suspended:
		body = "Order " + short + ": add a " + o.Currency + " payout address on your account page to receive " + label + " (" + a.netTag() + "); the network fee is taken from it."
	case part.kind == "return":
		body = "Order " + short + ": " + label + " you paid over the price is being returned to your saved " + o.Currency + " payout address; the network fee is taken from it."
	}
	if body != "" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), part.recipient, body); err != nil {
			return err
		}
	}
	if part.large {
		admins, err := collectStrings(ctx, tx, "SELECT id FROM users WHERE role='admin' AND suspended_at IS NULL ORDER BY id")
		if err != nil {
			return err
		}
		body = "Payout review needed for order " + short + ": " + note + " Release it on the admin page after checking it."
		for _, uid := range admins {
			// Only a sole administrator may review their own order's hold (A-219). When another
			// administrator exists, a buyer/vendor administrator receives only the party notice above.
			if len(admins) > 1 && (uid == o.BuyerID || uid == o.VendorID) {
				continue
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, body); err != nil {
				return err
			}
		}
	}
	return nil
}

// flagBelowFloor replaces the payout of sum (below the floor, see payoutFloor) with a payment-review flag on its
// counted deposits: payments.flagged (staff read access, the desk) and below_floor, which keeps the flag open on
// the desk and stops later passes counting the same deposits from flagging them again. Only deposits not yet
// marked are flagged, with one order note, one notification to the buyer and the vendor, and one to every
// moderator and administrator not party to the order. The deposits stay as they were (credited or not) and the
// order keeps its terminal state.
func (a *App) flagBelowFloor(ctx context.Context, tx *sql.Tx, o *Order, kind, who, below string, ids []int64, sum int64) error {
	rows, err := tx.QueryContext(ctx, "UPDATE payments SET "+newFlagSQL+",below_floor=true WHERE id=ANY($1) AND NOT below_floor RETURNING txid,amount", ids)
	if err != nil {
		return err
	}
	dec := currencyDecimals(o.Currency)
	var deposits []string
	for rows.Next() {
		var txid string
		var amt int64
		if err = rows.Scan(&txid, &amt); err != nil {
			rows.Close()
			return err
		}
		deposits = append(deposits, "deposit "+truncate(txid, 20)+" ("+amount(amt, dec)+" "+o.Currency+")")
	}
	rows.Close()
	if err = rows.Err(); err != nil || len(deposits) == 0 {
		return err
	}
	why := "a smaller Bitcoin payout cannot cover the network fee taken from it"
	if o.Currency == "XMR" {
		why = "the network fee would take too large a share of it"
	}
	payout := a.netTag() + " " + kind + " of " + amount(sum, dec) + " " + o.Currency + " to the " + who
	note := payout + " was not queued: it is " + below + " (" + why + "). Not paid out: " + strings.Join(deposits, ", ") + ". Moderator review required."
	party := "Order " + o.ID[:min(8, len(o.ID))] + ": the " + payout + " is " + below + " and was not sent. The market staff have been notified."
	return a.flagOrder(ctx, tx, o, note, party)
}
