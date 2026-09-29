package market

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"unicode/utf8"
)

// P3 Orders: user-facing order transitions. Every handler either moves the order through
// a.transition (which enforces the state table, actor role, product kind and compare-and-set)
// or returns an *httpError with the reason; nothing reports success without a state change.

const (
	maxDeliveryChars = 32000
	maxReviewChars   = 2000
	maxNoteChars     = 500
)

func init() {
	registerAction("/orders/pay", actionSpec{OneAtATime: true, Run: payAction})
	registerAction("/orders/cancel", actionSpec{Run: cancelAction})
	registerAction("/orders/ship", actionSpec{Run: shipAction})
	registerAction("/orders/deliver", actionSpec{Run: deliverAction})
	registerAction("/orders/complete", actionSpec{Run: completeAction})
	registerAction("/reviews", actionSpec{Run: reviewAction})
}

// partyOrder loads an order the signed-in user buys or sells. Anyone else gets 404, as on GET /order,
// so order identifiers cannot be probed through actions.
func partyOrder(c *actionCtx, id string) (Order, error) {
	o, err := scanOrder(c.Tx.QueryRowContext(c.Ctx(), orderQuery+" WHERE o.id=$1 AND (o.buyer_id=$2 OR p.vendor_id=$2)", id, c.User.ID))
	if err == sql.ErrNoRows {
		return o, errNoOrder
	}
	return o, err
}

// sellerIsVendor reports whether a listing's owner is currently a vendor or administrator. Listings of a
// demoted vendor accept no new drafts, payment requests or restores; existing orders continue.
func sellerIsVendor(c *actionCtx, productID string) (bool, error) {
	var ok bool
	err := c.Tx.QueryRowContext(c.Ctx(), sellerIsVendorSQL, productID).Scan(&ok)
	return ok, err
}

const sellerIsVendorSQL = "SELECT u.role IN ('vendor','admin') FROM products p JOIN users u ON u.id=p.vendor_id WHERE p.id=$1"

// sellerIsVendorLocked is sellerIsVendor for a restore (A-70, A-163): it share-locks the owner's account row until
// the transaction ends, so a concurrent role change either commits first and is seen here, or waits and then
// archives the restored listing in its own archive statement.
func sellerIsVendorLocked(c *actionCtx, productID string) (bool, error) {
	var ok bool
	err := c.Tx.QueryRowContext(c.Ctx(), sellerIsVendorSQL+" FOR SHARE OF u", productID).Scan(&ok)
	return ok, err
}

const sellerNotVendor = "The seller of this listing is no longer a vendor, so it accepts no new orders."

func shortID(id string) string { return id[:min(8, len(id))] }

func orderPage(id string) string { return "/order?id=" + id + "&saved=1" }

// formNote reads the optional free-text note recorded in order_events.
func formNote(c *actionCtx, fallback string) (string, error) {
	note := strings.TrimSpace(c.Form.Get("note"))
	if utf8.RuneCountInString(note) > maxNoteChars {
		return "", fail(400, "Keep the note under 500 characters")
	}
	if note == "" {
		return fallback, nil
	}
	return note, nil
}

// payoutConfirmation reads what the complete, paid-cancel and resolve forms carry (A-161, A-122): the ticked
// confirmation and amount_seen, the counted amount (payoutBasis) the form stated. Both are required (400);
// enqueuePayout compares amount_seen with what it counts (409 when it changed).
func payoutConfirmation(c *actionCtx) (int64, error) {
	if c.Form.Get("payout_confirmed") != "confirmed" {
		return 0, fail(400, "Tick the box confirming the amount and who receives it. Nothing was changed.")
	}
	seen, err := strconv.ParseInt(c.Form.Get("amount_seen"), 10, 64)
	if err != nil || seen < 0 {
		return 0, fail(400, "The form did not say which amount you reviewed. Reload the page and review it again. Nothing was changed.")
	}
	return seen, nil
}

// walletLockWait replaces the application pool's 3 s lock_timeout and 8 s statement_timeout (A-177) in a
// transaction that may queue for the rows a payment request holds while the wallet is asked for an address:
// the buyer (FOR NO KEY UPDATE), the payment-intake setting (FOR SHARE) and the listing, for up to rpcTimeout
// (10 s). Another checkout, a stock edit, an archive or takedown, a cancellation returning stock (stockHook,
// also the watcher's expiry), a payment-intake change, a payout to the buyer (enqueuePayout), and an
// administrator's suspension, second-factor reset or role change then wait for a slow wallet instead of
// failing at 3 s. The statement timeout is raised with it, since it would otherwise end the wait at 8 s. Both
// stay below the 12 s request context, so a longer wait is still refused cleanly (503). The wallet is asked
// only after every check has passed (payAction), so the rows cannot be released before the call without
// letting refused requests create addresses. One account's burst of payment requests queued on its own row
// is cut to one at a time (OneAtATime), so it cannot hold the pool's connections.
const walletLockWait = "11s"

func waitForWalletLocks(ctx context.Context, tx *sql.Tx) error {
	for _, setting := range []string{"lock_timeout", "statement_timeout"} {
		if _, err := tx.ExecContext(ctx, "SET LOCAL "+setting+" = '"+walletLockWait+"'"); err != nil {
			return err
		}
	}
	return nil
}

// maxAwaitingPayment bounds the stock one buyer can hold in unpaid orders (the watcher also expires them).
const maxAwaitingPayment = 3

// payAction: buyer moves draft -> awaiting_payment at the current listing price. transition() refuses when
// no provider serves the currency (409) and the stock hook reserves one unit (409 when none is left). Only
// then is the live wallet asked for an address, so refused requests never create wallet addresses.
func payAction(c *actionCtx) (actionResult, error) {
	ctx := c.Ctx()
	if err := waitForWalletLocks(ctx, c.Tx); err != nil {
		return actionResult{}, err
	}
	o, err := partyOrder(c, c.Form.Get("order_id"))
	if err != nil {
		return actionResult{}, err
	}
	if ok, err := sellerIsVendor(c, o.ProductID); err != nil {
		return actionResult{}, err
	} else if !ok && o.State == stateDraft {
		return actionResult{}, fail(409, sellerNotVendor+" Cancel this draft.")
	}
	if o.BuyerID == c.User.ID {
		var open int
		// The buyer row lock serialises concurrent requests so the cap cannot be raced. The count must be a
		// separate statement: under READ COMMITTED it then takes its snapshot after the lock is held and sees
		// the orders a concurrent request committed while this one waited. FOR NO KEY UPDATE, not FOR UPDATE:
		// it is held while the wallet is asked for an address, and inserts that reference the buyer
		// (notifications, messages, order events by anyone) take a key-share lock that FOR UPDATE would block.
		if _, err = c.Tx.ExecContext(ctx, "SELECT 1 FROM users WHERE id=$1 FOR NO KEY UPDATE", c.User.ID); err != nil {
			return actionResult{}, err
		}
		if err = c.Tx.QueryRowContext(ctx, "SELECT count(*) FROM orders WHERE buyer_id=$1 AND state='awaiting_payment'", c.User.ID).Scan(&open); err != nil {
			return actionResult{}, err
		}
		if open >= maxAwaitingPayment {
			return actionResult{}, fail(409, "You already have "+strconv.Itoa(maxAwaitingPayment)+" orders awaiting payment. Pay or cancel one before requesting another payment address.")
		}
	}
	if err = lockPaymentIntake(c, o.Currency); err != nil {
		return actionResult{}, err
	}
	if _, err = c.Tx.ExecContext(ctx, "UPDATE orders o SET amount=CASE WHEN o.currency='XMR' THEN p.xmr ELSE p.btc END FROM products p WHERE o.id=$1 AND o.state='draft' AND p.id=o.product_id", o.ID); err != nil {
		return actionResult{}, err
	}
	if _, err = c.A.transition(ctx, c.Tx, o.ID, stateDraft, stateAwaitingPayment, c.User, "Payment address requested"); err != nil {
		return actionResult{}, err
	}
	p := c.A.provider(o.Currency)
	// The watcher may disable a provider while transition's stock reservation
	// waits for a row lock. Recheck before calling it and roll back that reservation.
	if p == nil {
		return actionResult{}, fail(409, "Payment unavailable for "+o.Currency)
	}
	addr, err := p.NewAddress(ctx, o.ID)
	if err != nil || addr == "" || !p.ValidAddress(addr) {
		return actionResult{}, fail(503, "The "+o.Currency+" "+c.A.netAdj()+" wallet did not return a usable address. Nothing was changed; try again later.")
	}
	if _, err = c.Tx.ExecContext(ctx, "INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,$2,$3,$4)", o.ID, o.Currency, addr, strings.ToLower(o.Currency)+"-"+p.Network()); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: orderPage(o.ID), Audit: "Requested " + o.Currency + " " + c.A.netAdj() + " payment address for order " + shortID(o.ID)}, nil
}

// hasDeposits reports whether an order has received a deposit that can be refunded: not conflicted and not a
// locked transfer (those are never paid out automatically), confirmed or still confirming.
func hasDeposits(ctx context.Context, q rowQuerier, orderID string) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payments WHERE order_id=$1 AND confirmations>=0 AND NOT locked)", orderID).Scan(&ok)
	return ok, err
}

// cancelAction takes the state the viewer saw ("from") so a stale or repeated form is a 409, not a new move.
// Cancelling a paid order refunds every counted deposit, so it needs the payout confirmation; so does
// cancelling an order awaiting payment once a deposit has arrived (an underpayment is refunded).
func cancelAction(c *actionCtx) (actionResult, error) {
	from := c.Form.Get("from")
	if from != stateDraft && from != stateAwaitingPayment && from != statePaid {
		return actionResult{}, fail(400, "Only drafts, orders awaiting payment and paid orders can be cancelled")
	}
	note, err := formNote(c, "Cancelled")
	if err != nil {
		return actionResult{}, err
	}
	var seen int64
	if from == statePaid {
		if seen, err = payoutConfirmation(c); err != nil {
			return actionResult{}, err
		}
	}
	o, err := partyOrder(c, c.Form.Get("order_id"))
	if err != nil {
		return actionResult{}, err
	}
	ctx := c.Ctx()
	if from == stateAwaitingPayment {
		deposits, err := hasDeposits(ctx, c.Tx, o.ID)
		if err != nil {
			return actionResult{}, err
		}
		if deposits {
			if c.Form.Get("amount_seen") == "" {
				return actionResult{}, fail(409, "A deposit arrived for this order after the page was shown. Reload it to review the refund cancelling queues. Nothing was changed.")
			}
			if seen, err = payoutConfirmation(c); err != nil {
				return actionResult{}, err
			}
			ctx = withPayoutSeen(ctx, o.ID, seen)
		}
	}
	if from == statePaid {
		ctx = withPayoutSeen(ctx, o.ID, seen)
	}
	if _, err = c.A.transition(ctx, c.Tx, o.ID, from, stateCancelled, c.User, note); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: orderPage(o.ID), Audit: "Cancelled order " + shortID(o.ID) + " (was " + stateLabel(from) + ")"}, nil
}

func shipAction(c *actionCtx) (actionResult, error) {
	note, err := formNote(c, "Marked shipped")
	if err != nil {
		return actionResult{}, err
	}
	o, err := partyOrder(c, c.Form.Get("order_id"))
	if err != nil {
		return actionResult{}, err
	}
	if _, err = c.A.transition(c.Ctx(), c.Tx, o.ID, statePaid, stateShipped, c.User, note); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: orderPage(o.ID), Audit: "Marked order " + shortID(o.ID) + " shipped"}, nil
}

// deliverAction: the vendor releases digital content. The deliveries row is written in the same
// transaction as paid -> delivered, so content never exists for an order that was not delivered.
func deliverAction(c *actionCtx) (actionResult, error) {
	content := c.Form.Get("content")
	if strings.TrimSpace(content) == "" || utf8.RuneCountInString(content) > maxDeliveryChars {
		return actionResult{}, fail(400, "Delivery content must contain 1–32000 characters")
	}
	o, err := partyOrder(c, c.Form.Get("order_id"))
	if err != nil {
		return actionResult{}, err
	}
	if _, err = c.A.transition(c.Ctx(), c.Tx, o.ID, statePaid, stateDelivered, c.User, "Digital content delivered by vendor"); err != nil {
		return actionResult{}, err
	}
	if err = insertDelivery(c.Ctx(), c.Tx, o.ID, content); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: orderPage(o.ID), Audit: "Delivered digital content for order " + shortID(o.ID)}, nil
}

// completeAction: the buyer confirms receipt. The from-state follows the product kind, so a repeated or
// concurrent confirmation fails the compare-and-set with 409. Completing releases every counted deposit to the
// vendor, so it needs the payout confirmation.
func completeAction(c *actionCtx) (actionResult, error) {
	seen, err := payoutConfirmation(c)
	if err != nil {
		return actionResult{}, err
	}
	o, err := partyOrder(c, c.Form.Get("order_id"))
	if err != nil {
		return actionResult{}, err
	}
	from := stateShipped
	switch o.Kind {
	case "physical":
	case "digital":
		from = stateDelivered
	default:
		return actionResult{}, fail(403, "Only shipped or delivered orders can be completed")
	}
	if _, err = c.A.transition(withPayoutSeen(c.Ctx(), o.ID, seen), c.Tx, o.ID, from, stateCompleted, c.User, "Buyer confirmed receipt"); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: orderPage(o.ID), Audit: "Completed order " + shortID(o.ID)}, nil
}

// reviewAction: one review per completed order, by its buyer. "Verified purchase" on public pages rests
// on this check and the UNIQUE(order_id) constraint.
func reviewAction(c *actionCtx) (actionResult, error) {
	rating, err := strconv.Atoi(c.Form.Get("rating"))
	if err != nil || rating < 1 || rating > 5 {
		return actionResult{}, fail(400, "Choose a rating from 1 to 5")
	}
	body := strings.TrimSpace(c.Form.Get("body"))
	if utf8.RuneCountInString(body) > maxReviewChars {
		return actionResult{}, fail(400, "Keep the review under 2000 characters")
	}
	o, err := partyOrder(c, c.Form.Get("order_id"))
	if err != nil {
		return actionResult{}, err
	}
	if o.BuyerID != c.User.ID {
		return actionResult{}, fail(403, "Only the buyer can review this order")
	}
	if o.State != stateCompleted {
		return actionResult{}, fail(409, "Only completed orders can be reviewed")
	}
	res, err := c.Tx.ExecContext(c.Ctx(), "INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT (order_id) DO NOTHING", randomToken(), o.ID, o.ProductID, c.User.ID, rating, body)
	if err != nil {
		return actionResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return actionResult{}, fail(409, "You already reviewed this order")
	}
	return actionResult{Redirect: orderPage(o.ID), Audit: "Reviewed order " + shortID(o.ID)}, nil
}
