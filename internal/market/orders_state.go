package market

import (
	"context"
	"database/sql"
	"slices"
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
)

// transitions[from][to] lists the actor roles allowed to make that move. Terminal states map to nothing.
var transitions = map[string]map[string][]string{
	stateDraft:           {stateAwaitingPayment: {roleBuyer}, stateCancelled: {roleBuyer}},
	stateAwaitingPayment: {statePaid: {roleSystem}, stateCancelled: {roleBuyer, roleVendor}},
	statePaid:            {stateShipped: {roleVendor}, stateDelivered: {roleVendor, roleSystem}, stateCancelled: {roleVendor}, stateDisputed: {roleBuyer, roleVendor}},
	stateShipped:         {stateCompleted: {roleBuyer}, stateDisputed: {roleBuyer, roleVendor}},
	stateDelivered:       {stateCompleted: {roleBuyer}, stateDisputed: {roleBuyer, roleVendor}},
	stateDisputed:        {stateResolved: {roleModerator}},
	stateCompleted:       {},
	stateResolved:        {},
	stateCancelled:       {},
}

// transitionKind restricts a move to one product kind.
var transitionKind = map[[2]string]string{{statePaid, stateShipped}: "physical", {statePaid, stateDelivered}: "digital"}

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
	if roles == nil && (u.Role == "moderator" || u.Role == "admin") {
		roles = append(roles, roleModerator)
	}
	return roles
}

// orderQuery selects the columns scanOrder reads; append WHERE/ORDER clauses.
const orderQuery = `SELECT o.id,p.id,p.title,b.handle,v.handle,o.currency,o.amount,o.state,to_char(o.created,'YYYY-MM-DD HH24:MI'),p.kind,o.buyer_id,p.vendor_id,to_char(o.updated,'YYYY-MM-DD HH24:MI') FROM orders o JOIN products p ON p.id=o.product_id JOIN users b ON b.id=o.buyer_id JOIN users v ON v.id=p.vendor_id`

func scanOrder(s interface{ Scan(...any) error }) (Order, error) {
	var o Order
	var n int64
	err := s.Scan(&o.ID, &o.ProductID, &o.Title, &o.Buyer, &o.Vendor, &o.Currency, &n, &o.State, &o.Created, &o.Kind, &o.BuyerID, &o.VendorID, &o.Updated)
	dec := 8
	if o.Currency == "XMR" {
		dec = 12
	}
	o.Amount = amount(n, dec)
	o.Status = stateLabel(o.State)
	return o, err
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
	allowed, ok := transitions[from][to]
	if !ok || !slices.ContainsFunc(actorRoles(&o, actor), func(r string) bool { return slices.Contains(allowed, r) }) {
		return nil, errForbidden
	}
	if kind, ok := transitionKind[[2]string{from, to}]; ok && o.Kind != kind {
		return nil, fail(403, "This step is only available for "+kind+" orders.")
	}
	if from == stateDraft && to == stateAwaitingPayment && a.payments[o.Currency] == nil {
		return nil, fail(409, "Payment unavailable for "+o.Currency)
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
	for _, uid := range []string{o.BuyerID, o.VendorID} {
		if actor != nil && uid == actor.ID {
			continue
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, body); err != nil {
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
