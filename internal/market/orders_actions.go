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
	registerAction("/orders/cancel", actionSpec{FormNonce: true, Run: cancelAction})
	registerAction("/orders/ship", actionSpec{FormNonce: true, Run: shipAction})
	registerAction("/orders/deliver", actionSpec{FormNonce: true, Run: deliverAction})
	registerAction("/orders/complete", actionSpec{FormNonce: true, Run: completeAction})
	registerAction("/reviews", actionSpec{FormNonce: true, Run: reviewAction})
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

// partyOrderLocked is partyOrder with the order row locked until the transaction ends, before any other row of the
// order (the lock order of transition and storeFinal), for an action whose checks must see the order as it commits.
// FOR NO KEY UPDATE still serialises against another such action, transition's UPDATE and lockShipped's FOR UPDATE,
// but not against the KEY SHARE an order_events insert's foreign key takes, so the watcher's storeSettlement and
// storeFinal (settlement row, then an order note) never deadlock with a paste (QA-F2-1).
func partyOrderLocked(c *actionCtx, id string) (Order, error) {
	o, err := scanOrder(c.Tx.QueryRowContext(c.Ctx(), orderQuery+" WHERE o.id=$1 AND (o.buyer_id=$2 OR p.vendor_id=$2) FOR NO KEY UPDATE OF o", id, c.User.ID))
	if err == sql.ErrNoRows {
		return o, errNoOrder
	}
	return o, err
}

const sellerIsVendorSQL = "SELECT u.role IN ('vendor','admin') FROM products p JOIN users u ON u.id=p.vendor_id WHERE p.id=$1"

// sellerIsVendorLocked is sellerIsVendor for a restore (A-70, A-163): it share-locks the owner's account row until
// the transaction ends, so a concurrent role change either commits first and is seen here, or waits and then
// archives the restored listing in its own archive statement.
func sellerIsVendorLocked(c *actionCtx, productID string) (bool, error) {
	var ok bool
	err := c.Tx.QueryRowContext(c.Ctx(), sellerIsVendorSQL+" FOR SHARE OF u", productID).Scan(&ok)
	if err == nil && ok {
		var id string
		err = c.Tx.QueryRowContext(c.Ctx(), "SELECT vendor_id FROM products WHERE id=$1", productID).Scan(&id)
		if err == nil {
			err = lockVendorBond(c, id)
		}
	}
	return ok, err
}

const sellerNotVendor = "The seller of this listing is no longer a vendor, so it accepts no new orders."
const sellerSuspended = "The seller of this listing is suspended, so it accepts no new orders. Existing orders continue."

// lockNewIntake serializes a new draft or payment request with suspension and role changes. A suspension updates the
// vendor's account under FOR NO KEY UPDATE, which conflicts with this FOR SHARE until the new intake commits. The
// buyer's stronger lock serializes the awaiting-payment cap. Both account locks use NOWAIT: a confirmed administrator
// action locks its actor before its target, which can be the two parties here in reverse order. Returning the
// existing retryable database-busy 503 on contention avoids holding one account while waiting for the other.
// Account locks still follow id order for reciprocal vendor purchases and do not block foreign-key inserts.
func lockNewIntake(c *actionCtx, buyerID, vendorID string, draft bool) error {
	lockBuyer := func() error {
		var one int
		return c.Tx.QueryRowContext(c.Ctx(), "SELECT 1 FROM users WHERE id=$1 FOR NO KEY UPDATE NOWAIT", buyerID).Scan(&one)
	}
	var role string
	var suspended bool
	lockVendor := func() error {
		return c.Tx.QueryRowContext(c.Ctx(), "SELECT role,suspended_at IS NOT NULL FROM users WHERE id=$1 FOR SHARE NOWAIT", vendorID).Scan(&role, &suspended)
	}
	if buyerID != "" && buyerID < vendorID {
		if err := lockBuyer(); err != nil {
			return err
		}
	}
	if err := lockVendor(); err != nil {
		return err
	}
	if buyerID != "" && buyerID > vendorID {
		if err := lockBuyer(); err != nil {
			return err
		}
	}
	if role != "vendor" && role != "admin" {
		if draft {
			return fail(400, sellerNotVendor)
		}
		return fail(409, sellerNotVendor+" Cancel this draft.")
	}
	if suspended {
		if draft {
			return fail(409, "Unavailable. "+sellerSuspended+" Nothing was changed.")
		}
		return fail(409, "Unavailable. "+sellerSuspended+" Cancel this draft.")
	}
	return lockVendorBond(c, vendorID)
}

func shortID(id string) string { return id[:min(8, len(id))] }

func orderPage(id string) string { return "/order?id=" + id }

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
// confirmation and amount_seen, the counted amount the form stated. Both are required (400); the settlement
// request compares amount_seen with what it counts (checkPayoutSeen, 409 when it changed).
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
// transaction that may queue for the rows an escrow checkout holds while the escrow node is asked for an address:
// the buyer (FOR NO KEY UPDATE), the payment-intake setting (FOR SHARE) and the listing, for up to rpcTimeout
// (10 s). Another checkout, a stock edit, an archive or takedown, a cancellation returning stock (stockHook,
// also the watcher's expiry), a payment-intake change, and an
// administrator's suspension, second-factor reset or role change then wait for a slow wallet instead of
// failing at 3 s. The statement timeout is raised with it, since it would otherwise end the wait at 8 s. Both
// stay below the 12 s request context, so a longer wait is still refused cleanly (503). The wallet is asked
// only after every check has passed (escrowCheckoutAction), so the rows cannot be released before the call without
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

// hasDeposits reports whether an order has received a deposit that can be refunded: not conflicted and not a
// locked transfer (those are never paid out automatically), confirmed or still confirming.
func hasDeposits(ctx context.Context, q rowQuerier, orderID string) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payments WHERE order_id=$1 AND confirmations>=0 AND NOT locked)", orderID).Scan(&ok)
	return ok, err
}

// cancelAction takes the state the viewer saw ("from") so a stale or newly submitted form is a 409, not a
// new move. routes.post replays the original 303 for the exact successful form nonce before reaching here.
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
	if from == statePaid && o.Escrow && o.BuyerID == c.User.ID {
		// P2-R5: the buyer's no-ship cancel, checked under the order row lock the vendor's "Mark shipped" also takes.
		if o, err = partyOrderLocked(c, o.ID); err != nil {
			return actionResult{}, err
		}
		if err = noShipCancel(c, &o); err != nil {
			return actionResult{}, err
		}
	}
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
	return actionResult{Redirect: orderPage(o.ID), Notice: "order-cancelled", Audit: "Cancelled order " + shortID(o.ID) + " (was " + stateLabel(from) + ")"}, nil
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
	switch { // P2-R1: an escrow order is shipped with its release's claim address (escrowShipAction, xmrEscrowShipAction)
	case o.xmrEscrow():
		return actionResult{}, errShipPlainFormXMR
	case o.Escrow:
		return actionResult{}, errShipPlainForm
	}
	if _, err = c.A.transition(c.Ctx(), c.Tx, o.ID, statePaid, stateShipped, c.User, note); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: orderPage(o.ID), Notice: "order-shipped", Audit: "Marked order " + shortID(o.ID) + " shipped"}, nil
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
	return actionResult{Redirect: orderPage(o.ID), Notice: "order-delivered", Audit: "Delivered digital content for order " + shortID(o.ID)}, nil
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
	return actionResult{Redirect: orderPage(o.ID), Notice: "order-completed", Audit: "Completed order " + shortID(o.ID)}, nil
}

// reviewAction: one review per completed order, by its buyer. The completed-order badge on public pages rests
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
	if _, err = insertNotification(c.Ctx(), c.Tx, o.VendorID, "A new review was posted on order "+shortID(o.ID)+". Read it on the order page.", o.ID); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: orderPage(o.ID), Notice: "review-saved", Audit: "Reviewed order " + shortID(o.ID)}, nil
}
