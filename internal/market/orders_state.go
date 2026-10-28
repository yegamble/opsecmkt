package market

import (
	"context"
	"database/sql"
	"slices"
	"strings"
)

// Canonical order states (orders.state). Keep in sync with the CHECK in migrations/001_foundation.sql.
const (
	stateDraft           = "draft"
	stateAwaitingPayment = "awaiting_payment"
	statePaid            = "paid"
	stateShipped         = "shipped"
	stateDelivered       = "delivered"
	stateCompleted       = "completed"
	stateDisputed        = "disputed"
	stateResolved        = "resolved"
	stateCancelled       = "cancelled"
)

var orderStates = []string{stateDraft, stateAwaitingPayment, statePaid, stateShipped, stateDelivered, stateCompleted, stateDisputed, stateResolved, stateCancelled}

// Actor roles resolved per order by actorRoles.
const (
	roleBuyer     = "buyer"
	roleVendor    = "vendor"
	roleModerator = "moderator"
	roleSystem    = "system"
	// roleBanRefund is an administrator's refund of a banned vendor's unpaid or unshipped Monero escrow order (A-652b,
	// admin_ban_orders.go). actorRoles never returns it: transition adds it only for the order and administrator that
	// banOrdersAction marked (withBanRefund), so /orders/cancel, the order page and every other path cannot use it.
	roleBanRefund = "ban_refund"
	// roleHoldRefund is an administrator's refund of a release held for a banned vendor once the hold has lasted
	// ban_hold_refund_days (A-652f, escrow_xmr_hold_refund.go): the one edge out of completed and resolved. As with
	// roleBanRefund, actorRoles never returns it: transition adds it only for the order and administrator that
	// holdRefundAction marked (withHoldRefund).
	roleHoldRefund = "hold_refund"
)

// transitions[from][to] lists the actor roles allowed to make that move. Terminal states map to nothing, except the
// marked staff edge roleHoldRefund from completed and resolved to cancelled (A-652f). The buyer's paid -> cancelled is
// escrowOnly (the no-ship cancel, P2-R5).
var transitions = map[string]map[string][]string{
	stateDraft:           {stateAwaitingPayment: {roleBuyer}, stateCancelled: {roleBuyer}},
	stateAwaitingPayment: {statePaid: {roleSystem}, stateCancelled: {roleBuyer, roleVendor, roleSystem, roleBanRefund}}, // system: payment window expired
	statePaid:            {stateShipped: {roleVendor}, stateDelivered: {roleVendor, roleSystem}, stateCancelled: {roleVendor, roleBuyer, roleBanRefund}, stateDisputed: {roleBuyer, roleVendor}},
	stateShipped:         {stateCompleted: {roleBuyer, roleSystem}, stateDisputed: {roleBuyer, roleVendor}},           // system: escrowOnly
	stateDelivered:       {stateCompleted: {roleBuyer, roleSystem}, stateDisputed: {roleBuyer, roleVendor}},           // system: escrowOnly
	stateDisputed:        {stateResolved: {roleModerator}, stateCompleted: {roleBuyer}, stateCancelled: {roleVendor}}, // A-166: party concessions use only /disputes/concede.
	stateCompleted:       {stateCancelled: {roleHoldRefund}},
	stateResolved:        {stateCancelled: {roleHoldRefund}},
	stateCancelled:       {},
}

// transitionKind restricts a move to one product kind.
var transitionKind = map[[2]string]string{{statePaid, stateShipped}: "physical", {statePaid, stateDelivered}: "digital"}

// escrowOnly restricts a move by one role to escrow orders (Order.Escrow; A-244, ARCH-4): the system completes an
// escrow order (a digital Bitcoin order on funding, escrow_transition.go; a digital Monero one at the end of its dispute
// window, A-709, and a physical one at its release deadline, escrow_deadline.go, escrow_xmr_deadline.go),
// never a pooled one, whose completion stays the buyer's; and the buyer of a paid physical escrow order cancels it once
// the vendor has not shipped it in time (P2-R5, escrow_deadline.go), or of a paid digital Monero one once the vendor
// has not delivered it in time (A-709). On a pooled order these edges are refused (403).
var escrowOnly = map[[3]string]bool{{stateDelivered, stateCompleted, roleSystem}: true, {stateShipped, stateCompleted, roleSystem}: true,
	{statePaid, stateCancelled, roleBuyer}: true}

// buyerCompleteRefused lists the buyer's plain Complete edges, refused on an escrow order (A-373, P2-R2): the buyer of
// a Bitcoin escrow order confirms receipt only by signing the release (escrow_release.go), so these edges are refused
// there (409) and never offered, and "confirm receipt" can never lead to a market co-signature.
var buyerCompleteRefused = map[[3]string]bool{{stateShipped, stateCompleted, roleBuyer}: true, {stateDelivered, stateCompleted, roleBuyer}: true}

// xmrConfirmOnly is the one exception to buyerCompleteRefused (P3-R7): the buyer of a shipped Monero escrow order, which
// has no party signatures, or of a delivered digital one in its dispute window (A-709), confirms receipt with a
// password step-up (/escrow-xmr/confirm, escrow_xmr_release.go), and that action alone marks its move
// (withConfirmReceipt). Every other escrow order, and a Monero one through the plain Complete form, still meets
// buyerCompleteRefused.
var xmrConfirmOnly = map[[3]string]bool{{stateShipped, stateCompleted, roleBuyer}: true, {stateDelivered, stateCompleted, roleBuyer}: true}

type confirmReceiptCtx struct{}

// withConfirmReceipt marks the buyer's Confirm receipt of a Monero escrow order (xmrConfirmOnly).
func withConfirmReceipt(ctx context.Context) context.Context {
	return context.WithValue(ctx, confirmReceiptCtx{}, true)
}

func isConfirmReceipt(ctx context.Context) bool { return ctx.Value(confirmReceiptCtx{}) != nil }

type banRefundKey struct{}
type banRefund struct{ orderID, actorID string }

// withBanRefund marks one order's cancellation by administrator actorID as the refund of a banned vendor's order
// (roleBanRefund). Only banOrdersAction sets it, after its own checks.
func withBanRefund(ctx context.Context, orderID, actorID string) context.Context {
	return context.WithValue(ctx, banRefundKey{}, banRefund{orderID, actorID})
}

type holdRefundKey struct{}

// withHoldRefund marks one order's cancellation by administrator actorID as the refund of a release held for a banned
// vendor (roleHoldRefund). Only holdRefundAction sets it, after its own checks.
func withHoldRefund(ctx context.Context, orderID, actorID string) context.Context {
	return context.WithValue(ctx, holdRefundKey{}, banRefund{orderID, actorID})
}

// transitionRoles is actorRoles plus roleBanRefund or roleHoldRefund for the marked cancellation (withBanRefund,
// withHoldRefund) of a Monero escrow order by an administrator who is not party to it.
func transitionRoles(ctx context.Context, o *Order, actor *User, to string) []string {
	roles := actorRoles(o, actor)
	staff := actor != nil && actor.Role == "admin" && to == stateCancelled && o.xmrEscrow() && slices.Equal(roles, []string{roleModerator})
	if m, ok := ctx.Value(banRefundKey{}).(banRefund); ok && staff && m.orderID == o.ID && m.actorID == actor.ID {
		roles = append(roles, roleBanRefund)
	}
	if m, ok := ctx.Value(holdRefundKey{}).(banRefund); ok && staff && m.orderID == o.ID && m.actorID == actor.ID {
		roles = append(roles, roleHoldRefund)
	}
	return roles
}

var (
	errConflict  = fail(409, "This order changed or is no longer in that state. Reload and try again.")
	errForbidden = fail(403, "You cannot make that change to this order.")
	errNoOrder   = fail(404, "Order not found")
)

func stateLabel(s string) string {
	switch s {
	case stateDraft:
		return "Draft — unfunded"
	case stateAwaitingPayment:
		return "Awaiting payment"
	case statePaid:
		return "Paid"
	case stateShipped:
		return "Shipped"
	case stateDelivered:
		return "Delivered"
	case stateCompleted:
		return "Completed"
	case stateDisputed:
		return "Disputed"
	case stateResolved:
		return "Resolved"
	case stateCancelled:
		return "Cancelled"
	}
	return s
}

// actorRoles: nil actor = system; buyer/vendor by id; moderator/admin act as moderator only on orders they are not party to.
func actorRoles(o *Order, u *User) []string {
	if u == nil {
		return []string{roleSystem}
	}
	var roles []string
	if u.ID == o.BuyerID {
		roles = append(roles, roleBuyer)
	}
	if u.ID == o.VendorID {
		roles = append(roles, roleVendor)
	}
	if roles == nil && !u.RetentionRestricted && (u.Role == "moderator" || u.Role == "admin") {
		roles = append(roles, roleModerator)
	}
	return roles
}

// orderQuery selects the columns scanOrder reads; append WHERE/ORDER clauses. A draft reads the listing as it is
// now, except a listing whose edit awaits staff review (MOD72-6): its unreviewed title and description are not shown.
const orderQuery = `SELECT o.id,p.id,
	CASE WHEN o.state='draft' AND p.review_state<>'approved' THEN 'Listing awaiting review' WHEN o.state='draft' THEN p.title ELSE COALESCE(o.listing_title,'Listing terms were not recorded for this order') END,
	b.handle,v.handle,o.currency,o.amount,o.quantity,o.state,to_char(o.created,'YYYY-MM-DD HH24:MI "UTC"'),
	CASE WHEN o.state='draft' THEN p.kind ELSE COALESCE(o.listing_kind,p.kind) END,
	o.buyer_id,p.vendor_id,to_char(o.updated,'YYYY-MM-DD HH24:MI "UTC"'),
	EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id),
	CASE WHEN o.state='draft' THEN p.kind ELSE COALESCE(o.listing_kind,'Not recorded') END,
	CASE WHEN o.state='draft' AND p.review_state<>'approved' THEN '' WHEN o.state='draft' THEN p.description ELSE COALESCE(o.listing_description,'') END,
	o.listing_title IS NOT NULL
	FROM orders o JOIN products p ON p.id=o.product_id JOIN users b ON b.id=o.buyer_id JOIN users v ON v.id=p.vendor_id`

func scanOrder(s interface{ Scan(...any) error }) (Order, error) {
	var o Order
	var n int64
	// Escrow: the order has an escrows row (escrow checkout, escrow_create.go); every other order is pooled.
	err := s.Scan(&o.ID, &o.ProductID, &o.Title, &o.Buyer, &o.Vendor, &o.Currency, &n, &o.Quantity, &o.State, &o.Created, &o.Kind, &o.BuyerID, &o.VendorID, &o.Updated, &o.Escrow, &o.KindDisplay, &o.Description, &o.ListingRecorded)
	dec := 8
	if o.Currency == "XMR" {
		dec = 12
	}
	o.Amount = amount(n, dec)
	o.Status = stateLabel(o.State)
	return o, err
}

// btcEscrow reports whether o is settled through a Bitcoin escrow. An escrow's coin is its order's currency (migration
// 084), so a Monero escrow order is Escrow but not btcEscrow: the pay-twice guards key on Escrow, and Bitcoin escrow
// code (escrow_*.go) acts only on btcEscrow orders.
func (o Order) btcEscrow() bool { return o.Escrow && o.Currency == "BTC" }

// xmrEscrow reports whether o is settled through a Monero escrow (escrow_xmr_*.go).
func (o Order) xmrEscrow() bool { return o.Escrow && o.Currency == "XMR" }

// noticeDetail is a sentence an action adds to the notice transition sends about one order's move to state to
// (withNoticeDetail): an escrow order's release deadline in the shipped and dispute notices (P2-R9, escrow_deadline.go).
type noticeDetail struct{ orderID, to, text string }

type noticeDetailKey struct{}

func withNoticeDetail(ctx context.Context, orderID, to, text string) context.Context {
	return context.WithValue(ctx, noticeDetailKey{}, noticeDetail{orderID, to, text})
}

// transition moves an order from -> to inside tx: row lock, rule check, compare-and-set update,
// order_events row, counterparty notification, then transitionHooks. actor nil = system.
// Errors are *httpError (errConflict 409, errForbidden 403, errNoOrder 404, or a hook's) or database errors.
func (a *App) transition(ctx context.Context, tx *sql.Tx, orderID, from, to string, actor *User, note string) (*Order, error) {
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", orderID))
	if err == sql.ErrNoRows {
		return nil, errNoOrder
	}
	if err != nil {
		return nil, err
	}
	if o.State != from {
		return nil, errConflict
	}
	if !o.Escrow && !(from == stateDraft && to == stateCancelled) && !(from == stateDraft && to == stateAwaitingPayment && isEscrowCheckout(ctx)) {
		return nil, fail(409, "Historical pooled orders are read-only")
	}
	if from == stateDisputed && (to == stateCompleted || to == stateCancelled) {
		concession, ok := ctx.Value(concessionTransitionKey{}).(concessionTransition)
		if !ok || actor == nil || concession.orderID != orderID || concession.to != to || concession.actorID != actor.ID {
			return nil, errForbidden
		}
	}
	if o.Escrow && slices.ContainsFunc(actorRoles(&o, actor), func(r string) bool {
		edge := [3]string{from, to, r}
		return buyerCompleteRefused[edge] && !(xmrConfirmOnly[edge] && o.xmrEscrow() && isConfirmReceipt(ctx))
	}) {
		if o.xmrEscrow() {
			return nil, errEscrowCompleteXMR
		}
		return nil, errEscrowComplete
	}
	allowed, ok := transitions[from][to]
	if !ok || !slices.ContainsFunc(transitionRoles(ctx, &o, actor, to), func(r string) bool {
		return slices.Contains(allowed, r) && (o.Escrow || !escrowOnly[[3]string{from, to, r}])
	}) {
		return nil, errForbidden
	}
	if kind, ok := transitionKind[[2]string{from, to}]; ok && o.Kind != kind {
		return nil, fail(403, "This step is only available for "+kind+" orders.")
	}
	if from == stateDraft && to == stateAwaitingPayment && (!isEscrowCheckout(ctx) || !a.escrowReady(o.Currency)) {
		return nil, fail(409, "Escrow checkout unavailable for "+o.Currency)
	}
	res, err := tx.ExecContext(ctx, "UPDATE orders SET state=$2,updated=now() WHERE id=$1 AND state=$3", orderID, to, from)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, errConflict
	}
	var actorID any
	if actor != nil {
		actorID = actor.ID
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$3,$4,$5)", orderID, from, to, actorID, note); err != nil {
		return nil, err
	}
	body := "Order " + orderID[:min(8, len(orderID))] + " is now " + stateLabel(to)
	// A move neither party made (the watcher's, a moderator's) says why with its event note (A-123): "Cancelled —
	// payment window expired", "Resolved — refund to buyer". A party's own note stays in the history only.
	if note != "" && (actor == nil || (actor.ID != o.BuyerID && actor.ID != o.VendorID)) {
		body += " — " + strings.TrimPrefix(note, stateLabel(to)+" — ")
	}
	if to == stateDisputed {
		// Only the party who did not open the dispute is notified; it may put one reply on record (A-124).
		body += ". You may put one reply to the dispute on record from the order page before an administrator decides."
	}
	if d, ok := ctx.Value(noticeDetailKey{}).(noticeDetail); ok && d.orderID == orderID && d.to == to {
		body = strings.TrimSuffix(body, ".") + ". " + d.text
	}
	// Draft cancellation is an abandoned checkout cleanup, not vendor work; notifying the vendor
	// would let buyers bury incoming orders with repeated create/cancel cycles (A-165).
	for _, uid := range []string{o.BuyerID, o.VendorID} {
		if actor != nil && uid == actor.ID {
			continue
		}
		if from == stateDraft && to == stateCancelled && uid == o.VendorID {
			continue
		}
		if _, err = insertNotification(ctx, tx, uid, body, orderID); err != nil {
			return nil, err
		}
	}
	o.State, o.Status = to, stateLabel(to)
	for _, h := range transitionHooks {
		if err = h(ctx, a, tx, &o, from, to); err != nil {
			return nil, err
		}
	}
	return &o, nil
}
