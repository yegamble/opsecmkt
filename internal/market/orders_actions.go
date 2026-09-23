package market

import (
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
	registerAction("/orders/pay", actionSpec{Run: payAction})
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
	err := c.Tx.QueryRowContext(c.Ctx(), "SELECT u.role IN ('vendor','admin') FROM products p JOIN users u ON u.id=p.vendor_id WHERE p.id=$1", productID).Scan(&ok)
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

// maxAwaitingPayment bounds the stock one buyer can hold in unpaid orders (the watcher also expires them).
const maxAwaitingPayment = 3

// payAction: buyer moves draft -> awaiting_payment at the current listing price. transition() refuses when
// no provider serves the currency (409) and the stock hook reserves one unit (409 when none is left). Only
// then is the live test-network wallet asked for an address, so refused requests never create wallet addresses.
func payAction(c *actionCtx) (actionResult, error) {
	ctx := c.Ctx()
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
		// The buyer row lock serialises concurrent requests so the cap cannot be raced.
		if err = c.Tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM orders WHERE buyer_id=u.id AND state='awaiting_payment') FROM users u WHERE u.id=$1 FOR UPDATE", c.User.ID).Scan(&open); err != nil {
			return actionResult{}, err
		}
		if open >= maxAwaitingPayment {
			return actionResult{}, fail(409, "You already have "+strconv.Itoa(maxAwaitingPayment)+" orders awaiting payment. Pay or cancel one before requesting another payment address.")
		}
	}
	if _, err = c.Tx.ExecContext(ctx, "UPDATE orders o SET amount=CASE WHEN o.currency='XMR' THEN p.xmr ELSE p.btc END FROM products p WHERE o.id=$1 AND o.state='draft' AND p.id=o.product_id", o.ID); err != nil {
		return actionResult{}, err
	}
	if _, err = c.A.transition(ctx, c.Tx, o.ID, stateDraft, stateAwaitingPayment, c.User, "Payment address requested"); err != nil {
		return actionResult{}, err
	}
	p := c.A.payments[o.Currency]
	addr, err := p.NewAddress(ctx, o.ID)
	if err != nil || addr == "" || !p.ValidAddress(addr) {
		return actionResult{}, fail(503, "The "+o.Currency+" test-network wallet did not return a usable address. Nothing was changed; try again later.")
	}
	if _, err = c.Tx.ExecContext(ctx, "INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,$2,$3,$4)", o.ID, o.Currency, addr, strings.ToLower(o.Currency)+"-"+p.Network()); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: orderPage(o.ID), Audit: "Requested " + o.Currency + " test-network payment address for order " + shortID(o.ID)}, nil
}

// cancelAction takes the state the viewer saw ("from") so a stale or repeated form is a 409, not a new move.
func cancelAction(c *actionCtx) (actionResult, error) {
	from := c.Form.Get("from")
	if from != stateDraft && from != stateAwaitingPayment && from != statePaid {
		return actionResult{}, fail(400, "Only drafts, orders awaiting payment and paid orders can be cancelled")
	}
	note, err := formNote(c, "Cancelled")
	if err != nil {
		return actionResult{}, err
	}
	o, err := partyOrder(c, c.Form.Get("order_id"))
	if err != nil {
		return actionResult{}, err
	}
	if _, err = c.A.transition(c.Ctx(), c.Tx, o.ID, from, stateCancelled, c.User, note); err != nil {
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
// concurrent confirmation fails the compare-and-set with 409.
func completeAction(c *actionCtx) (actionResult, error) {
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
	if _, err = c.A.transition(c.Ctx(), c.Tx, o.ID, from, stateCompleted, c.User, "Buyer confirmed receipt"); err != nil {
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
