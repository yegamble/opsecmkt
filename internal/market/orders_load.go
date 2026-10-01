package market

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// P3 Orders: page loaders and preview data for order, product, vendor, vendor-dashboard, disputes, moderator.

func init() {
	registerLoader("order", loadOrderDetail)
	registerLoader("product", func(ctx context.Context, a *App, r *http.Request, d *PageData) error {
		return loadPublicReviews(ctx, a, d, "r.product_id=$1", r.URL.Query().Get("id"))
	})
	registerLoader("vendor", func(ctx context.Context, a *App, r *http.Request, d *PageData) error {
		return loadPublicReviews(ctx, a, d, "p.vendor_id=$1", r.URL.Query().Get("id"))
	})
	registerLoader("product", reviewModerationLoader)
	registerLoader("vendor", reviewModerationLoader)
	registerLoader("vendor-dashboard", loadIncomingOrders)
	registerLoader("disputes", loadDisputes)
	registerLoader("moderator", loadDisputes)
	registerLoader("moderator", loadPaymentReviews)

	registerPreview("order", previewOrder)
	registerPreview("product", previewReviews)
	registerPreview("vendor", previewReviews)
	registerPreview("vendor-dashboard", previewVendorOrders)
	registerPreview("disputes", previewDisputes)
	registerPreview("moderator", previewDisputes)
	registerPreview("moderator", previewPaymentReviews)
}

// transitionActions maps a target state to the form that performs it. Resolution happens on the
// moderator desk and payment confirmation only through the payment watcher, so neither appears here.
var transitionActions = map[string]string{
	stateAwaitingPayment: "/orders/pay",
	stateShipped:         "/orders/ship",
	stateDelivered:       "/orders/deliver",
	stateCompleted:       "/orders/complete",
	stateDisputed:        "/disputes",
	stateCancelled:       "/orders/cancel",
}

func transitionLabel(from, to string) string {
	switch to {
	case stateAwaitingPayment:
		return "Request payment address"
	case stateShipped:
		return "Mark as shipped"
	case stateDelivered:
		return "Deliver digital content"
	case stateCompleted:
		return "Confirm receipt and complete"
	case stateDisputed:
		return "Open a dispute"
	case stateCancelled:
		if from == stateDraft {
			return "Cancel draft"
		}
		return "Cancel order"
	}
	return stateLabel(to)
}

// viewerTransitions lists the moves the viewer may make now, using the same table, roles and kind rules
// as transition(). Requesting payment without a provider is listed with an empty Action (unavailable).
func viewerTransitions(payments map[string]PaymentProvider, netAdj string, o *Order, u *User) []Transition {
	roles := actorRoles(o, u)
	var out []Transition
	for _, to := range orderStates {
		allowed, ok := transitions[o.State][to]
		action := transitionActions[to]
		if !ok || action == "" || !slices.ContainsFunc(roles, func(r string) bool { return slices.Contains(allowed, r) }) {
			continue
		}
		if kind, ok := transitionKind[[2]string{o.State, to}]; ok && o.Kind != kind {
			continue
		}
		t := Transition{To: to, Label: transitionLabel(o.State, to), Action: action}
		if to == stateAwaitingPayment && payments[o.Currency] == nil {
			t.Action = ""
			t.Label = "Payment unavailable for " + o.Currency + ": no " + o.Currency + " " + netAdj + " wallet is available on this server (not configured, or temporarily unreachable)."
		}
		out = append(out, t)
	}
	return out
}

func eventActor(handle string, system bool, actorID string, o *Order) string {
	switch {
	case system:
		return "System"
	case actorID == o.BuyerID:
		return handle + " (buyer)"
	case actorID == o.VendorID:
		return handle + " (vendor)"
	}
	return handle + " (moderator)"
}

// staffReviewer: a moderator or administrator who is not party to the order may read it (never act on it
// from the order page) once it is disputed, keep reading it after resolution, and read any order with a
// payment flagged for review (payments.flagged, set by the watcher's flagOrder).
func staffReviewer(ctx context.Context, a *App, o *Order, u *User) (bool, error) {
	if u == nil || u.ID == o.BuyerID || u.ID == o.VendorID || (u.Role != "moderator" && u.Role != "admin") {
		return false, nil
	}
	if disputeVisible(o) {
		return true, nil
	}
	var flagged bool
	err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payments WHERE order_id=$1 AND flagged)", o.ID).Scan(&flagged)
	return flagged, err
}

// disputeVisible: the order is or was disputed, so a reviewer may also read its digital delivery.
func disputeVisible(o *Order) bool { return o.State == stateDisputed || o.State == stateResolved }

// payoutTxidHidden stands in for a payout's transaction ID in text shown to the order party who is not its
// recipient (A-214).
const payoutTxidHidden = "(transaction ID shown only to its recipient)"

// hidePayoutTxids replaces in each text the transaction ID of every payout of order whose recipient is not
// viewer, in full or as truncate(txid, 20), when viewer is the order's buyer or vendor: on Bitcoin a txid reveals
// its recipient's payout address. New order-history notes name no payout txid; this covers notes and dispute
// decisions stored before (the rows stay unchanged). A refund staff recorded as sent to the buyer by hand (A-31,
// payments.disposition_txid) is named in its order-history note and hidden the same way from the vendor, as is the
// rest of a refund an operator sent to the buyer by hand and recorded with UPGRADING.md's relabel block (sections 14
// and 18, A-312), which exists only in that system note. Staff reviewers who are not parties read the text as stored.
func hidePayoutTxids(ctx context.Context, a *App, order, viewer string, texts ...*string) error {
	rows, err := a.db.QueryContext(ctx, `SELECT py.txid FROM payouts py JOIN orders o ON o.id=py.order_id JOIN products p ON p.id=o.product_id
		WHERE py.order_id=$1 AND py.txid<>'' AND py.user_id<>$2 AND $2 IN (o.buyer_id,p.vendor_id)
		UNION SELECT pm.disposition_txid FROM payments pm JOIN orders o ON o.id=pm.order_id JOIN products p ON p.id=o.product_id
		WHERE pm.order_id=$1 AND pm.disposition_txid<>'' AND o.buyer_id<>$2 AND p.vendor_id=$2
		UNION SELECT m[1] FROM order_events e JOIN orders o ON o.id=e.order_id JOIN products p ON p.id=o.product_id
		CROSS JOIN LATERAL regexp_matches(e.note, '^Refund recorded by hand \(UPGRADING\.md section 1[48]\): .* in transaction ([0-9a-f]{64})\.$') m
		WHERE e.order_id=$1 AND e.actor_id IS NULL AND o.buyer_id<>$2 AND p.vendor_id=$2`, order, viewer)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var txid string
		if err = rows.Scan(&txid); err != nil {
			return err
		}
		for _, s := range texts {
			*s = strings.ReplaceAll(strings.ReplaceAll(*s, txid, payoutTxidHidden), truncate(txid, 20), payoutTxidHidden)
		}
	}
	return rows.Err()
}

func loadOrderDetail(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	o, u := d.Order, d.User
	if o == nil || u == nil {
		return sql.ErrNoRows
	}
	reviewer, err := staffReviewer(ctx, a, o, u)
	switch {
	case err != nil:
		return err
	case u.ID == o.BuyerID:
		d.OrderViewer = roleBuyer
	case u.ID == o.VendorID:
		d.OrderViewer = roleVendor
	case reviewer:
		d.OrderViewer = roleModerator
	default:
		return sql.ErrNoRows
	}
	// A dispute decision carries the deciding staff member's avatar (A-189, owner decision 1(c)) when the desk
	// route serves it to this viewer: its own predicate (imageDeskWhere, $2 viewer and $3 role) picks it, so the
	// page never links an avatar that route would refuse. No other entry shows an avatar.
	rows, err := a.db.QueryContext(ctx, `SELECT e.from_state,e.to_state,COALESCE(u.handle,''),e.actor_id IS NULL,COALESCE(e.actor_id,''),e.note,to_char(e.created,'YYYY-MM-DD HH24:MI "UTC"'),
		CASE WHEN e.from_state='disputed' AND e.to_state='resolved' THEN COALESCE((SELECT i.id FROM images i JOIN users o ON o.id=i.owner_user_id
			WHERE i.owner_user_id=e.actor_id AND i.kind='avatar' AND `+imageDeskWhere+`),'') ELSE '' END
		FROM order_events e LEFT JOIN users u ON u.id=e.actor_id WHERE e.order_id=$1 ORDER BY e.id`, o.ID, u.ID, u.Role)
	if err != nil {
		return err
	}
	for rows.Next() {
		var ev OrderEvent
		var handle, actorID, avatar string
		var system bool
		if err = rows.Scan(&ev.From, &ev.To, &handle, &system, &actorID, &ev.Note, &ev.Created, &avatar); err != nil {
			rows.Close()
			return err
		}
		if isDisputeReply(ev.From, ev.To, actorID, ev.Note, o) {
			// The reply itself is shown once, under Dispute (A-124).
			ev.Note = "Replied to the dispute; the reply is shown under Dispute."
		}
		ev.From, ev.To, ev.Actor = stateLabel(ev.From), stateLabel(ev.To), eventActor(handle, system, actorID, o)
		if avatar != "" {
			ev.Avatar = deskImageURL(avatar, false)
		}
		d.OrderEvents = append(d.OrderEvents, ev)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// A reviewer of a payment flag sees no delivery content unless the order is or was disputed.
	d.DeliveryWithheld = d.OrderViewer == roleModerator && !disputeVisible(o)
	var dv DeliveryView
	if !d.DeliveryWithheld {
		err = a.db.QueryRowContext(ctx, "SELECT content,to_char(created,'YYYY-MM-DD HH24:MI \"UTC\"') FROM deliveries WHERE order_id=$1", o.ID).Scan(&dv.Content, &dv.Created)
		if err == nil {
			d.Delivery = &dv
		} else if err != sql.ErrNoRows {
			return err
		}
	}
	var rv Review
	err = a.db.QueryRowContext(ctx, "SELECT r.id,r.order_id,b.handle,r.rating,r.body,to_char(r.created,'YYYY-MM-DD'),r.hidden_at IS NOT NULL FROM reviews r JOIN users b ON b.id=r.buyer_id WHERE r.order_id=$1", o.ID).Scan(&rv.ID, &rv.OrderID, &rv.Buyer, &rv.Rating, &rv.Body, &rv.Created, &rv.Hidden)
	if err == nil {
		d.Reviews = []Review{rv}
	} else if err != sql.ErrNoRows {
		return err
	}
	var disp Dispute
	err = a.db.QueryRowContext(ctx, `SELECT d.id,d.order_id,d.reason,d.status,d.resolution,to_char(d.created,'YYYY-MM-DD HH24:MI "UTC"'),`+disputeRecordCols+`
		FROM disputes d JOIN orders o ON o.id=d.order_id JOIN products p ON p.id=o.product_id`+disputeReplyJoin+` WHERE d.order_id=$1`, o.ID).Scan(
		&disp.ID, &disp.OrderID, &disp.Reason, &disp.Status, &disp.Resolution, &disp.Created, &disp.Opener, &disp.ReplyRole, &disp.Reply, &disp.ReplyCreated)
	if err == nil {
		d.Disputes = []Dispute{disp}
	} else if err != sql.ErrNoRows {
		return err
	}
	if d.OrderViewer != roleModerator {
		var texts []*string
		for i := range d.OrderEvents {
			texts = append(texts, &d.OrderEvents[i].Note)
		}
		if len(d.Disputes) == 1 {
			texts = append(texts, &d.Disputes[0].Resolution)
		}
		if err = hidePayoutTxids(ctx, a, o.ID, u.ID, texts...); err != nil {
			return err
		}
	}
	d.CanReview = u.ID == o.BuyerID && o.State == stateCompleted && len(d.Reviews) == 0
	if d.OrderViewer != roleModerator { // reviewers resolve on the moderation desk; the order page is read-only for them
		d.Transitions = viewerTransitions(a.providers(), a.netAdj(), o, u)
		if !o.Escrow { // an escrow order queues no payout (settleTerminal)
			if err = a.orderPayouts(ctx, d); err != nil {
				return err
			}
		}
	}
	if err = a.escrowOrder(ctx, d); err != nil {
		return err
	}
	if d.Contacts, err = orderContacts(ctx, a, o, d.OrderViewer); err != nil {
		return err
	}
	vendorKeyGate(d)
	return vendorKeyChangeGate(ctx, a, d)
}

// orderPayouts (A-122): on a paid, shipped or delivered order the complete and paid-cancel forms state the
// payout they queue (payoutBasis), and counted deposits above the price set Overpaid. On an order awaiting
// payment that has received a deposit, the cancel form states the refund it queues (an underpayment).
func (a *App) orderPayouts(ctx context.Context, d *PageData) error {
	o := d.Order
	if o.State == stateAwaitingPayment {
		deposits, err := hasDeposits(ctx, a.db, o.ID)
		if err != nil || !deposits {
			return err
		}
		_, sum, _, err := a.payoutBasis(ctx, a.db, o, false)
		if err != nil {
			return err
		}
		for i, t := range d.Transitions {
			if t.To == stateCancelled {
				d.Transitions[i].Payout = &PayoutPreview{Seen: sum, Confirm: payoutStatement("refund", o.Buyer, "the buyer", a.netName(), o, sum) + ". This is final."}
			}
		}
		return nil
	}
	if o.State != statePaid && o.State != stateShipped && o.State != stateDelivered {
		return nil
	}
	_, sum, _, err := a.payoutBasis(ctx, a.db, o, false)
	if err != nil {
		return err
	}
	dec := currencyDecimals(o.Currency)
	if required, _ := parseAmount(o.Amount, dec); sum > required {
		// The split the complete form states (payoutStatement): a return (maybe held), or the excess kept.
		parts := splitPayout("release", o, sum, required)
		v := &OverpaidView{Excess: amount(sum-required, dec) + " " + o.Currency, Kept: len(parts) == 1}
		switch {
		case v.Kept && parts[0].large: // a legacy price below the release floor (A-216)
			v.Held, v.Reason = true, subFloorPriceExcess(o.Currency)
		case v.Kept:
			v.Reason = "the excess is below the minimum automatic payout of " + amount(payoutFloor(o.Currency, "return"), dec) + " " + o.Currency + ", so it is not returned"
		default:
			v.Held = parts[1].large
		}
		if !v.Kept && d.OrderViewer == roleBuyer {
			column := "payout_btc"
			if o.Currency == "XMR" {
				column = "payout_xmr"
			}
			if err = a.db.QueryRowContext(ctx, "SELECT "+column+"='' FROM users WHERE id=$1", o.BuyerID).Scan(&v.NoAddress); err != nil {
				return err
			}
		}
		d.Overpaid = v
	}
	for i, t := range d.Transitions {
		switch {
		case t.To == stateCompleted:
			d.Transitions[i].Payout = &PayoutPreview{Seen: sum, Confirm: payoutStatement("release", o.Vendor, "the vendor", a.netName(), o, sum) + ". This is final."}
		case t.To == stateCancelled && o.State == statePaid:
			d.Transitions[i].Payout = &PayoutPreview{Seen: sum, Confirm: payoutStatement("refund", o.Buyer, "the buyer", a.netName(), o, sum) + ". This is final."}
		}
	}
	return nil
}

// payoutStatement states the payouts of sum (payoutBasis) for o on network net (App.netName), as splitPayout
// makes them: "Refund 0.01 BTC (test network) to buyer_x — exactly the price; network fee deducted", or for a
// release with an excess worth returning "Release 0.01 BTC (test network) to vendor_x (the price) and return the
// 0.009 BTC paid over the price to buyer_x; network fee deducted from each", saying so when a refund or return
// waits for an administrator (larger than the price), a release keeps an excess because the price is below the
// release floor (A-216), or the sum is below the minimum automatic payout (A-99; then nothing is sent automatically
// and no fee is named). Every payout on both coins is sent with the fee taken from it (A-232). With nothing
// counted, later deposits go to later (the recipient's role, or "the chosen party" on the resolve form).
func payoutStatement(kind, recipient, later, net string, o *Order, sum int64) string {
	verb := "Release"
	if kind == "refund" {
		verb = "Refund"
	}
	if sum == 0 {
		return verb + " (" + net + ") to " + recipient + " — nothing counted yet; deposits confirming later go to " + later
	}
	dec := currencyDecimals(o.Currency)
	required, _ := parseAmount(o.Amount, dec)
	// The same split enqueuePayout makes: a release pays the price and returns an excess worth sending.
	parts := splitPayout(kind, o, sum, required)
	if len(parts) == 2 {
		statement := "Release " + amount(parts[0].amount, dec) + " " + o.Currency + " (" + net + ") to " + recipient + " (the price) and return the " +
			amount(parts[1].amount, dec) + " " + o.Currency + " paid over the price to " + o.Buyer + "; network fee deducted from each"
		if parts[1].large {
			statement += "; the return is more than the price, so an administrator reviews it before it is sent"
		}
		return statement
	}
	statement := verb + " " + amount(sum, dec) + " " + o.Currency + " (" + net + ") to " + recipient + " — " + priceDifference(sum, required, o.Currency)
	switch {
	case kind == "release" && parts[0].large:
		statement += "; " + subFloorPriceExcess(o.Currency) + "; an administrator reviews the release before it is sent"
	case kind == "release" && sum > required:
		statement += ", below the minimum automatic payout, so it is not returned"
	case kind == "refund" && sum > required:
		statement += "; more than the price, so an administrator reviews it before it is sent"
	}
	if below := belowFloor(o.Currency, kind, sum); below != "" {
		return statement + "; " + below + ": not sent automatically, staff are asked to review it"
	}
	return statement + "; network fee deducted"
}

const resolveConfirm = "I checked the amount and who receives it for the outcome I chose. Resolving is final."

// resolvePayouts (A-161) fills the resolve form of every open dispute on a disputed order the staff viewer is not
// party to: each outcome's payout (payoutBasis) and the recipient's payout state.
func (a *App) resolvePayouts(ctx context.Context, d *PageData) error {
	d.DisputePayouts = map[string]PayoutPreview{}
	if d.User == nil || (d.User.Role != "moderator" && d.User.Role != "admin") {
		return nil
	}
	for _, v := range d.Disputes[:d.OpenDisputes] {
		o, ok := d.DisputeOrders[v.OrderID]
		if !ok || o.State != stateDisputed || d.User.ID == o.BuyerID || d.User.ID == o.VendorID {
			continue
		}
		if o.Escrow { // decided by an administrator and paid by a settlement from the escrow (escrow_decision.go)
			p, err := a.escrowDisputePreview(ctx, &o)
			if err != nil {
				return err
			}
			d.DisputePayouts[v.ID] = p
			continue
		}
		_, sum, _, err := a.payoutBasis(ctx, a.db, &o, false)
		if err != nil {
			return err
		}
		column := "payout_btc"
		if o.Currency == "XMR" {
			column = "payout_xmr"
		}
		state := map[string]string{}
		rows, err := a.db.QueryContext(ctx, "SELECT id,"+column+"='',suspended_at IS NOT NULL FROM users WHERE id=$1 OR id=$2", o.BuyerID, o.VendorID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var noAddress, suspended bool
			if err = rows.Scan(&id, &noAddress, &suspended); err != nil {
				rows.Close()
				return err
			}
			switch {
			case suspended:
				state[id] = "; account suspended: an administrator must check the address"
			case noAddress:
				state[id] = "; no payout address: the payout waits for one"
			}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		d.DisputePayouts[v.ID] = PayoutPreview{Seen: sum, Confirm: resolveConfirm,
			Release: payoutStatement("release", o.Vendor, "the chosen party", a.netName(), &o, sum) + state[o.VendorID],
			Refund:  payoutStatement("refund", o.Buyer, "the chosen party", a.netName(), &o, sum) + state[o.BuyerID]}
	}
	return nil
}

// reviewModerationLoader tells an administrator's hide and restore review forms (A-125), and listing takedown and
// restore forms (A-163), whether to ask for an authenticator code.
func reviewModerationLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" {
		return nil
	}
	return adminConfirmLoader(ctx, a, r, d)
}

// loadPublicReviews fills public reviews. Only reviews keyed to completed orders count; reviewer handles
// and order ids are never shown publicly and dates are coarsened to the month. A review an administrator hid
// (A-125) is left out of the list and the rating; administrators see it listed, labelled, with a restore form.
func loadPublicReviews(ctx context.Context, a *App, d *PageData, where, id string) error {
	base := " FROM reviews r JOIN orders o ON o.id=r.order_id JOIN products p ON p.id=r.product_id WHERE o.state='completed' AND " + where
	admin := d.User != nil && d.User.Role == "admin"
	var sum ReviewSummary
	var avg float64
	if err := a.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE r.hidden_at IS NULL),
		COALESCE(avg(r.rating) FILTER (WHERE r.hidden_at IS NULL),0)::float8,count(*) FILTER (WHERE r.hidden_at IS NOT NULL)`+base, id).Scan(&sum.Count, &avg, &sum.Hidden); err != nil {
		return err
	}
	sum.Average = fmt.Sprintf("%.1f", avg)
	listed := " AND r.hidden_at IS NULL"
	if admin {
		listed = ""
	} else {
		sum.Hidden = 0
	}
	d.ReviewSummary = &sum
	rows, err := a.db.QueryContext(ctx, "SELECT r.id,r.rating,r.body,to_char(r.created,'YYYY-MM'),p.title,r.hidden_at IS NOT NULL"+base+listed+" ORDER BY r.created DESC LIMIT 50", id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rv Review
		if err = rows.Scan(&rv.ID, &rv.Rating, &rv.Body, &rv.Created, &rv.Product, &rv.Hidden); err != nil {
			return err
		}
		if d.Page != "vendor" {
			rv.Product = ""
		}
		d.Reviews = append(d.Reviews, rv)
	}
	return rows.Err()
}

// Stars renders a rating as five glyphs for display (screen readers get the number separately).
func (r Review) Stars() string {
	n := max(0, min(5, r.Rating))
	return strings.Repeat("★", n) + strings.Repeat("☆", 5-n)
}

// historyLimit caps closed history (resolved disputes, orders needing no vendor action) on the dispute and
// vendor desks. Open work is never capped: every open dispute and every paid order is always listed.
const historyLimit = 100

func queryOrders(ctx context.Context, a *App, query string, args ...any) ([]Order, error) {
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// loadIncomingOrders lists orders on the viewer's own listings: every paid order first (the work queue,
// oldest first; NeedsAction counts them), then the most recent other orders up to historyLimit.
func loadIncomingOrders(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	paid, err := queryOrders(ctx, a, orderQuery+" WHERE p.vendor_id=$1 AND o.state=$2 ORDER BY o.created,o.id", d.User.ID, statePaid)
	if err != nil {
		return err
	}
	rest, err := queryOrders(ctx, a, orderQuery+" WHERE p.vendor_id=$1 AND o.state<>$2 ORDER BY o.created DESC,o.id DESC LIMIT $3", d.User.ID, statePaid, historyLimit+1)
	if err != nil {
		return err
	}
	if len(rest) > historyLimit {
		rest, d.HistoryLimit = rest[:historyLimit], historyLimit
	}
	d.IncomingOrders, d.NeedsAction = append(paid, rest...), len(paid)
	return nil
}

func queryDisputes(ctx context.Context, a *App, query string, args ...any) ([]Dispute, error) {
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dispute
	for rows.Next() {
		var v Dispute
		if err = rows.Scan(&v.ID, &v.OrderID, &v.Reason, &v.Status, &v.Resolution, &v.Created, &v.NoResolver, &v.Opener, &v.ReplyRole, &v.Reply, &v.ReplyCreated); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// loadDisputes lists the viewer's disputes (on their own orders, or all of them for moderators and
// administrators): every open dispute first, oldest first, then the most recent resolved ones up to
// historyLimit. Each dispute gets its order summary; an open one is marked NoResolver while every
// moderator and administrator who is not suspended is a party to its order.
func loadDisputes(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	d.DisputeOrders = map[string]Order{}
	if d.User == nil {
		return nil
	}
	const scope = `SELECT d.id,d.order_id,d.reason,d.status,d.resolution,to_char(d.created,'YYYY-MM-DD HH24:MI "UTC"'),d.status='Open' AND NOT EXISTS(SELECT 1 FROM users s WHERE s.role IN ('moderator','admin') AND s.suspended_at IS NULL AND s.id<>o.buyer_id AND s.id<>p.vendor_id),` +
		disputeRecordCols + ` FROM disputes d JOIN orders o ON o.id=d.order_id JOIN products p ON p.id=o.product_id` + disputeReplyJoin + ` WHERE (o.buyer_id=$1 OR p.vendor_id=$1 OR $2 IN ('admin','moderator'))`
	open, err := queryDisputes(ctx, a, scope+" AND d.status='Open' ORDER BY d.created,d.id", d.User.ID, d.User.Role)
	if err != nil {
		return err
	}
	resolved, err := queryDisputes(ctx, a, scope+" AND d.status<>'Open' ORDER BY d.created DESC,d.id DESC LIMIT $3", d.User.ID, d.User.Role, historyLimit+1)
	if err != nil {
		return err
	}
	if len(resolved) > historyLimit {
		resolved, d.HistoryLimit = resolved[:historyLimit], historyLimit
	}
	d.Disputes, d.OpenDisputes = append(open, resolved...), len(open)
	if len(d.Disputes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(d.Disputes))
	for _, v := range d.Disputes {
		ids = append(ids, v.OrderID)
	}
	orders, err := queryOrders(ctx, a, orderQuery+" WHERE o.id = ANY($1)", ids)
	if err != nil {
		return err
	}
	for _, o := range orders {
		d.DisputeOrders[o.ID] = o
	}
	for i := range d.Disputes {
		o := d.DisputeOrders[d.Disputes[i].OrderID]
		if d.Disputes[i].Resolution != "" && (d.User.ID == o.BuyerID || d.User.ID == o.VendorID) {
			if err = hidePayoutTxids(ctx, a, o.ID, d.User.ID, &d.Disputes[i].Resolution); err != nil {
				return err
			}
		}
	}
	if err = a.resolvePayouts(ctx, d); err != nil {
		return err
	}
	return a.disputeEvidence(ctx, d)
}

// disputeReplyPrefix starts the note of the order_events row that stores a dispute reply (A-124): a disputed ->
// disputed event whose actor is the buyer or the vendor. No state changes; the transition table is not involved.
const disputeReplyPrefix = "Dispute reply: "

// disputeRecordCols reads, for a dispute d on order o of listing p (with disputeReplyJoin), the opener's role and the
// reply's role, text and time ("" when absent). The opener is the actor of the latest move to disputed.
const disputeRecordCols = `COALESCE((SELECT CASE e.actor_id WHEN o.buyer_id THEN 'buyer' WHEN p.vendor_id THEN 'vendor' ELSE '' END FROM order_events e
		WHERE e.order_id=d.order_id AND e.to_state='disputed' AND e.from_state<>'disputed' ORDER BY e.id DESC LIMIT 1),''),COALESCE(r.role,''),COALESCE(r.body,''),COALESCE(r.at,'')`

const disputeReplyJoin = ` LEFT JOIN LATERAL (SELECT CASE e.actor_id WHEN o.buyer_id THEN 'buyer' ELSE 'vendor' END AS role,substr(e.note,length('Dispute reply: ')+1) AS body,
		to_char(e.created,'YYYY-MM-DD HH24:MI "UTC"') AS at FROM order_events e WHERE e.order_id=d.order_id AND e.from_state='disputed' AND e.to_state='disputed'
		AND e.actor_id IN (o.buyer_id,p.vendor_id) AND e.note LIKE 'Dispute reply: %' ORDER BY e.id LIMIT 1) r ON true`

// isDisputeReply reports whether an order_events row (raw states) is a dispute reply on o.
func isDisputeReply(from, to, actorID, note string, o *Order) bool {
	return from == stateDisputed && to == stateDisputed && (actorID == o.BuyerID || actorID == o.VendorID) && strings.HasPrefix(note, disputeReplyPrefix)
}

// disputeEvidenceQuery counts, per open dispute in $1, the messages each party sent to each moderator or administrator
// since the dispute opened. left_staff: when each account was last recorded leaving staff, from its role-change audit
// row (A-294; both writers, adminAction and SetRole, word it "Role changed from OLD to NEW by …"). A message counts
// when its recipient is staff now, or left staff after the message was sent (listed as no longer staff). A-301: the
// first LIKE is the predicate of the partial index audit_events_role_change (migration 072); without it the planner
// cannot use the index and scans every audit row, one per sign-in. The recipients are one UNION of current and former
// staff, not an OR on users, so the few of them are estimated as few and their messages read by messages_recipient.
const disputeEvidenceQuery = `WITH left_staff AS (SELECT user_id,max(created) AS at FROM audit_events
			WHERE action LIKE 'Role changed from %' AND (action LIKE 'Role changed from moderator to %' OR action LIKE 'Role changed from admin to %')
			GROUP BY user_id)
		SELECT d.id,m.sender_id=o.buyer_id,s.handle,count(*),to_char(max(m.created),'YYYY-MM-DD HH24:MI "UTC"'),s.suspended_at IS NOT NULL,s.role NOT IN ('moderator','admin')
		FROM disputes d JOIN orders o ON o.id=d.order_id JOIN products p ON p.id=o.product_id
		JOIN users s ON s.id IN (SELECT id FROM users WHERE role IN ('moderator','admin') UNION SELECT user_id FROM left_staff) AND s.id<>o.buyer_id AND s.id<>p.vendor_id
		LEFT JOIN left_staff l ON l.user_id=s.id
		JOIN messages m ON m.recipient_id=s.id AND m.created>=d.created AND m.sender_id IN (o.buyer_id,p.vendor_id) AND (s.role IN ('moderator','admin') OR l.at>=m.created)
		WHERE d.id=ANY($1) GROUP BY d.id,m.sender_id=o.buyer_id,s.handle,s.suspended_at,s.role ORDER BY s.handle`

// disputeEvidence (A-162) lists, for every open dispute a moderator or administrator viewer is not party to, how
// many messages each party sent to each moderator or administrator since the dispute opened, and when the latest was
// sent. It reads only who sent a message to whom and when, never a body: a resolver learns who holds encrypted
// evidence and asks them, without anyone but the recipient reading it. A holder suspended since is still listed,
// marked Suspended (A-280); so is one removed from staff after the message, marked Former (A-294).
func (a *App) disputeEvidence(ctx context.Context, d *PageData) error {
	d.DisputeEvidence = map[string]*DisputeEvidence{}
	if d.User == nil || (d.User.Role != "moderator" && d.User.Role != "admin") {
		return nil
	}
	var ids []string
	for _, v := range d.Disputes[:d.OpenDisputes] {
		o, ok := d.DisputeOrders[v.OrderID]
		if !ok || d.User.ID == o.BuyerID || d.User.ID == o.VendorID {
			continue
		}
		ids = append(ids, v.ID)
		d.DisputeEvidence[v.ID] = &DisputeEvidence{}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, disputeEvidenceQuery, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var fromBuyer bool
		var h EvidenceHolder
		if err = rows.Scan(&id, &fromBuyer, &h.Handle, &h.Count, &h.Latest, &h.Suspended, &h.Former); err != nil {
			return err
		}
		ev := d.DisputeEvidence[id]
		if fromBuyer {
			ev.Buyer = append(ev.Buyer, h)
		} else {
			ev.Vendor = append(ev.Vendor, h)
		}
	}
	return rows.Err()
}

// Preview order IDs and txids have the real 64-hex shape so the read-only preview exercises the same layout
// (page head, breadcrumbs, tables) as real orders. They are sha256 digests of fixed labels; no order exists.
const (
	previewDraftID    = "171abe22fd1a69af6d19320aa8548d251d0d8c4e9bb7cebfa4619778e6df0b54" // preview-draft
	previewPaidID     = "8cf1aaf1bfa545a92504cdfe264306ee5f9d9ac801736734b173d8631a891cd9" // preview-paid
	previewIncomingID = "c0005ef3175f03034f45675fd03f35f35582e643cbfc75e4f3a8b53a1562f54b" // preview-incoming
	previewDisputedID = "8c4ed43bdc4d7d6925e39713751066e19e4b37833545946bbeaa14d5dced7df2" // preview-disputed
	previewFlaggedID  = "fa3d78b76018852580157c02a484def10cc4354335961a0c9e835e9563a3fc1e" // preview-flagged
)

// previewPaidOrder replaces the preview draft with a paid sample order that has one confirmed sample deposit, so
// the preview shows the deposits table. No wallet is connected; the status says the deposit is a sample.
func previewPaidOrder(d *PageData) {
	if d.Order == nil {
		return
	}
	o := d.Order
	o.ID, o.State, o.Status = previewPaidID, statePaid, stateLabel(statePaid)
	d.OrderEvents = []OrderEvent{
		{From: stateLabel(stateDraft), To: stateLabel(stateAwaitingPayment), Actor: o.Buyer + " (buyer)", Created: "Sample order"},
		{From: stateLabel(stateAwaitingPayment), To: stateLabel(statePaid), Actor: "System", Created: "Sample order"},
	}
	d.Payment = &PaymentView{Network: "sample", Testnet: true, Currency: o.Currency, Required: o.Amount, Received: o.Amount,
		Unconfirmed: "0", Confirmations: 3, Threshold: 3, Issued: true, Monitored: true,
		Status:   "Preview sample: no wallet is connected and no funds exist",
		Deposits: []PaymentDeposit{{TxID: "926bb57bc9bcbc21ddd00aa2e6f70f9d445378bcbaf4e55b6bfa53010ba85ddb", Amount: o.Amount, Confirmations: 3, State: "Confirmed"}}}
}

func previewOrder(d *PageData) {
	if d.Order == nil || d.User == nil {
		return
	}
	d.Transitions = viewerTransitions(nil, "test-network", d.Order, d.User)
}

func previewReviews(d *PageData) {
	d.Reviews = []Review{{ID: "sample-review", Rating: 4, Body: "Sample review for the read-only preview. Real reviews come only from completed orders.", Created: "Sample"}}
	if d.Page == "vendor" && len(d.Products) > 0 {
		d.Reviews[0].Product = d.Products[0].Title
	}
	d.ReviewSummary = &ReviewSummary{Count: 1, Average: "4.0"}
}

func previewVendorOrders(d *PageData) {
	if len(d.Orders) == 0 || d.User == nil {
		return
	}
	o := d.Orders[0]
	o.ID, o.Buyer, o.BuyerID, o.Vendor, o.VendorID = previewIncomingID, "sample_buyer", "sample-buyer", d.User.Handle, d.User.ID
	o.State, o.Status = statePaid, stateLabel(statePaid)
	d.IncomingOrders, d.NeedsAction = []Order{o}, 1
}

func previewDisputes(d *PageData) {
	if len(d.Orders) == 0 {
		return
	}
	o := d.Orders[0]
	o.ID, o.State, o.Status = previewDisputedID, stateDisputed, stateLabel(stateDisputed)
	o.BuyerID, o.Buyer, o.VendorID = "sample-buyer", "sample_buyer", "sample-vendor"
	d.Disputes, d.OpenDisputes = []Dispute{{ID: "sample-dispute", OrderID: o.ID, Reason: "Sample dispute for the read-only preview.\nNo order or funds exist.", Status: "Open", Created: "Sample",
		Opener: roleBuyer, ReplyRole: roleVendor, Reply: "Sample reply from the vendor.\nNothing was stored.", ReplyCreated: "Sample"}}, 1
	d.DisputeOrders = map[string]Order{o.ID: o}
	d.DisputeEvidence = map[string]*DisputeEvidence{"sample-dispute": {Buyer: []EvidenceHolder{{Handle: "sample_moderator", Count: 1, Latest: "Sample"}}}}
	// A sample counted amount equal to the price; the sample buyer has no payout address.
	sum, _ := parseAmount(o.Amount, currencyDecimals(o.Currency))
	d.DisputePayouts = map[string]PayoutPreview{"sample-dispute": {Seen: sum, Confirm: resolveConfirm,
		Release: payoutStatement("release", o.Vendor, "the chosen party", "test network", &o, sum),
		Refund:  payoutStatement("refund", o.Buyer, "the chosen party", "test network", &o, sum) + "; no payout address: the payout waits for one"}}
}

// paymentReviewLimit caps the other (not open) flags on the moderation desk's payment-review list.
const paymentReviewLimit = 100

// loadPaymentReviews lists deposits the payment watcher flagged for staff review (payments.flagged): every open
// flag, oldest first and never capped, then the paymentReviewLimit most recently flagged others, with their
// count. A flag is open until staff record its disposition (A-31: no action or refunded by hand; one escalated to
// an administrator stays open) while it is a locked transfer, a late deposit or a deposit of a payout below the
// minimum automatic payout (payments.below_floor, A-99), or a credited deposit whose regression was announced and
// has not confirmed again (payments.regress_notice). A disposed flag is not listed (the order history and the
// audit log keep the disposition). The reason comes from the deposit's current confirmations. The flag time is the latest watcher flag event for the deposit in order_events: flagOrder writes a system note naming it
// (truncate(txid, 20)) and ending "Moderator review required.".
func loadPaymentReviews(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || (d.User.Role != "moderator" && d.User.Role != "admin") {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `WITH v AS (SELECT pm.id,pm.order_id,o.state,pr.id AS product_id,pr.title,pm.currency,pm.amount,pm.txid,pm.idx,pm.credited,pm.locked,pm.confirmations,
			pm.regress_notice,pm.below_floor,(pm.locked OR NOT pm.credited OR pm.regress_notice OR pm.below_floor) AS open,f.at,
			pm.disposition='escalated' AS escalated,COALESCE(to_char(pm.disposed_at,'YYYY-MM-DD'),'') AS disposed,(o.buyer_id=$2 OR pr.vendor_id=$2) AS party
		FROM payments pm JOIN orders o ON o.id=pm.order_id JOIN products pr ON pr.id=o.product_id
		LEFT JOIN LATERAL (SELECT max(e.created) AS at FROM order_events e WHERE e.order_id=pm.order_id AND e.actor_id IS NULL
			AND e.note LIKE '%Moderator review required.%'
			AND strpos(e.note, ' '||CASE WHEN length(pm.txid)<=20 THEN pm.txid ELSE left(pm.txid,20)||'…' END||' ')>0) f ON true
		WHERE pm.flagged AND pm.disposition IN ('','escalated'))
		SELECT id,order_id,state,product_id,title,currency,amount,txid,idx,credited,locked,confirmations,regress_notice,below_floor,open,COALESCE(to_char(at,'YYYY-MM-DD HH24:MI "UTC"'),''),
			escalated,disposed,party,(SELECT count(*) FROM v WHERE NOT open)
		FROM (SELECT * FROM v WHERE open UNION ALL (SELECT * FROM v WHERE NOT open ORDER BY at DESC NULLS LAST,id DESC LIMIT $1)) s
		ORDER BY open DESC,CASE WHEN open THEN at END ASC NULLS FIRST,CASE WHEN open THEN id END,at DESC NULLS LAST,id DESC`, paymentReviewLimit, d.User.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v PaymentReview
		var amt, confs int64
		var credited, locked, regressed, belowFloor bool
		var disposed string
		if err = rows.Scan(&v.ID, &v.OrderID, &v.OrderState, &v.ProductID, &v.Title, &v.Currency, &amt, &v.TxID, &v.Index, &credited, &locked, &confs, &regressed, &belowFloor, &v.Open, &v.Flagged,
			&v.Escalated, &disposed, &v.Party, &d.PaymentReviewOthers); err != nil {
			return err
		}
		// The disposition form (paymentReviewAction): open flags of orders the viewer is not party to; an escalated
		// one only for an administrator. A credited deposit is never refunded by hand.
		v.Form = v.Open && !v.Party && (!v.Escalated || d.User.Role == "admin")
		v.Refundable = !credited
		v.Amount, v.OrderState = amount(amt, currencyDecimals(v.Currency)), stateLabel(v.OrderState)
		threshold := a.confirmationThreshold(v.Currency)
		switch {
		case locked:
			v.Reason = "Locked transfer (unlock time), not counted or paid out"
		case belowFloor && (!credited || confs >= threshold):
			v.Reason = "Below the minimum automatic payout, not paid out"
			if confs < 0 {
				v.Reason += "; now conflicted or missing"
			}
		case !credited:
			v.Reason = "Deposit after settlement or after the order closed, not paid out"
			if confs < 0 {
				v.Reason += "; now conflicted or missing"
			}
		case confs < 0:
			v.Reason = "Credited deposit conflicted or missing"
		case threshold > 0 && confs < threshold:
			v.Reason = fmt.Sprintf("Credited deposit below threshold (%d of %d confirmations)", confs, threshold)
		case threshold == 0 && regressed:
			v.Reason = fmt.Sprintf("Credited deposit below threshold (%d confirmations)", confs)
		default:
			v.Reason = fmt.Sprintf("Credited deposit confirmed again (%d confirmations)", confs)
		}
		if v.Escalated {
			v.Reason += "; escalated to an administrator on " + disposed
		}
		if v.Open {
			d.PaymentReviewsOpen++
		}
		d.PaymentReviews = append(d.PaymentReviews, v)
	}
	if d.PaymentReviewOthers > paymentReviewLimit {
		d.PaymentReviewLimit = paymentReviewLimit
	}
	return rows.Err()
}

func previewPaymentReviews(d *PageData) {
	d.PaymentReviews = []PaymentReview{{OrderID: previewFlaggedID, OrderState: stateLabel(statePaid), Currency: "BTC", Amount: "0.001", ProductID: d.Products[0].ID, Title: d.Products[0].Title,
		TxID: "a6fca84477376ef06de0fa66091d6435fbd7be1abe70d867cdcc06529fbc952b", Reason: "Credited deposit conflicted or missing", Flagged: "Sample", Open: true}}
	d.PaymentReviewsOpen = 1
}
