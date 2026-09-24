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
func viewerTransitions(payments map[string]PaymentProvider, o *Order, u *User) []Transition {
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
			t.Label = "Payment unavailable for " + o.Currency + ": no " + o.Currency + " test-network wallet is available on this server (not configured, or temporarily unreachable)."
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
	rows, err := a.db.QueryContext(ctx, `SELECT e.from_state,e.to_state,COALESCE(u.handle,''),e.actor_id IS NULL,COALESCE(e.actor_id,''),e.note,to_char(e.created,'YYYY-MM-DD HH24:MI "UTC"') FROM order_events e LEFT JOIN users u ON u.id=e.actor_id WHERE e.order_id=$1 ORDER BY e.id`, o.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var ev OrderEvent
		var handle, actorID string
		var system bool
		if err = rows.Scan(&ev.From, &ev.To, &handle, &system, &actorID, &ev.Note, &ev.Created); err != nil {
			rows.Close()
			return err
		}
		ev.From, ev.To, ev.Actor = stateLabel(ev.From), stateLabel(ev.To), eventActor(handle, system, actorID, o)
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
	err = a.db.QueryRowContext(ctx, "SELECT r.id,r.order_id,b.handle,r.rating,r.body,to_char(r.created,'YYYY-MM-DD') FROM reviews r JOIN users b ON b.id=r.buyer_id WHERE r.order_id=$1", o.ID).Scan(&rv.ID, &rv.OrderID, &rv.Buyer, &rv.Rating, &rv.Body, &rv.Created)
	if err == nil {
		d.Reviews = []Review{rv}
	} else if err != sql.ErrNoRows {
		return err
	}
	var disp Dispute
	err = a.db.QueryRowContext(ctx, "SELECT id,order_id,reason,status,resolution,to_char(created,'YYYY-MM-DD HH24:MI \"UTC\"') FROM disputes WHERE order_id=$1", o.ID).Scan(&disp.ID, &disp.OrderID, &disp.Reason, &disp.Status, &disp.Resolution, &disp.Created)
	if err == nil {
		d.Disputes = []Dispute{disp}
	} else if err != sql.ErrNoRows {
		return err
	}
	d.CanReview = u.ID == o.BuyerID && o.State == stateCompleted && len(d.Reviews) == 0
	if d.OrderViewer != roleModerator { // reviewers resolve on the moderation desk; the order page is read-only for them
		d.Transitions = viewerTransitions(a.providers(), o, u)
		if err = a.orderPayouts(ctx, d); err != nil {
			return err
		}
	}
	d.Contacts, err = orderContacts(ctx, a, o, d.OrderViewer)
	return err
}

// orderPayouts (A-122): on a paid, shipped or delivered order the complete and paid-cancel forms state the
// payout they queue (payoutBasis), and counted deposits above the price set Overpaid.
func (a *App) orderPayouts(ctx context.Context, d *PageData) error {
	o := d.Order
	if o.State != statePaid && o.State != stateShipped && o.State != stateDelivered {
		return nil
	}
	_, sum, _, err := a.payoutBasis(ctx, a.db, o, false)
	if err != nil {
		return err
	}
	dec := currencyDecimals(o.Currency)
	if required, _ := parseAmount(o.Amount, dec); sum > required {
		d.Overpaid = amount(sum-required, dec) + " " + o.Currency
	}
	for i, t := range d.Transitions {
		switch {
		case t.To == stateCompleted:
			d.Transitions[i].Payout = &PayoutPreview{Seen: sum, Confirm: payoutStatement("release", o.Vendor, "the vendor", o, sum) + ". This is final."}
		case t.To == stateCancelled && o.State == statePaid:
			d.Transitions[i].Payout = &PayoutPreview{Seen: sum, Confirm: payoutStatement("refund", o.Buyer, "the buyer", o, sum) + ". This is final."}
		}
	}
	return nil
}

// payoutStatement states a payout of sum (payoutBasis) for o: "Release 0.01 BTC (test network) to vendor_x —
// 0.009 BTC more than the price". With nothing counted, later deposits go to later (the recipient's role, or
// "the chosen party" on the resolve form).
func payoutStatement(kind, recipient, later string, o *Order, sum int64) string {
	verb := "Release"
	if kind == "refund" {
		verb = "Refund"
	}
	if sum == 0 {
		return verb + " (test network) to " + recipient + " — nothing counted yet; deposits confirming later go to " + later
	}
	dec := currencyDecimals(o.Currency)
	required, _ := parseAmount(o.Amount, dec)
	return verb + " " + amount(sum, dec) + " " + o.Currency + " (test network) to " + recipient + " — " + priceDifference(sum, required, o.Currency)
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
			Release: payoutStatement("release", o.Vendor, "the chosen party", &o, sum) + state[o.VendorID],
			Refund:  payoutStatement("refund", o.Buyer, "the chosen party", &o, sum) + state[o.BuyerID]}
	}
	return nil
}

// loadPublicReviews fills public reviews. Only reviews keyed to completed orders count; reviewer handles
// and order ids are never shown publicly and dates are coarsened to the month.
func loadPublicReviews(ctx context.Context, a *App, d *PageData, where, id string) error {
	base := " FROM reviews r JOIN orders o ON o.id=r.order_id JOIN products p ON p.id=r.product_id WHERE o.state='completed' AND " + where
	var sum ReviewSummary
	var avg float64
	if err := a.db.QueryRowContext(ctx, "SELECT count(*),COALESCE(avg(r.rating),0)::float8"+base, id).Scan(&sum.Count, &avg); err != nil {
		return err
	}
	sum.Average = fmt.Sprintf("%.1f", avg)
	d.ReviewSummary = &sum
	rows, err := a.db.QueryContext(ctx, "SELECT r.id,r.rating,r.body,to_char(r.created,'YYYY-MM'),p.title"+base+" ORDER BY r.created DESC LIMIT 50", id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rv Review
		if err = rows.Scan(&rv.ID, &rv.Rating, &rv.Body, &rv.Created, &rv.Product); err != nil {
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
		if err = rows.Scan(&v.ID, &v.OrderID, &v.Reason, &v.Status, &v.Resolution, &v.Created, &v.NoResolver); err != nil {
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
	const scope = `SELECT d.id,d.order_id,d.reason,d.status,d.resolution,to_char(d.created,'YYYY-MM-DD HH24:MI "UTC"'),d.status='Open' AND NOT EXISTS(SELECT 1 FROM users s WHERE s.role IN ('moderator','admin') AND s.suspended_at IS NULL AND s.id<>o.buyer_id AND s.id<>p.vendor_id) FROM disputes d JOIN orders o ON o.id=d.order_id JOIN products p ON p.id=o.product_id WHERE (o.buyer_id=$1 OR p.vendor_id=$1 OR $2 IN ('admin','moderator'))`
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
	return a.resolvePayouts(ctx, d)
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
	d.Transitions = viewerTransitions(nil, d.Order, d.User)
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
	d.Disputes, d.OpenDisputes = []Dispute{{ID: "sample-dispute", OrderID: o.ID, Reason: "Sample dispute for the read-only preview. No order or funds exist.", Status: "Open", Created: "Sample"}}, 1
	d.DisputeOrders = map[string]Order{o.ID: o}
	// A sample counted amount equal to the price; the sample buyer has no payout address.
	sum, _ := parseAmount(o.Amount, currencyDecimals(o.Currency))
	d.DisputePayouts = map[string]PayoutPreview{"sample-dispute": {Seen: sum, Confirm: resolveConfirm,
		Release: payoutStatement("release", o.Vendor, "the chosen party", &o, sum),
		Refund:  payoutStatement("refund", o.Buyer, "the chosen party", &o, sum) + "; no payout address: the payout waits for one"}}
}

// paymentReviewLimit caps the other (not open) flags on the moderation desk's payment-review list.
const paymentReviewLimit = 100

// loadPaymentReviews lists deposits the payment watcher flagged for staff review (payments.flagged): every open
// flag, oldest first and never capped, then the paymentReviewLimit most recently flagged others, with their
// count. A flag is open while it has no settling event: a locked transfer or a late deposit (no disposition
// action exists yet), or a credited deposit whose regression was announced and has not confirmed again
// (payments.regress_notice). The reason comes from the deposit's current confirmations. The flag time is the
// latest watcher flag event for the deposit in order_events: flagOrder writes a system note naming it
// (truncate(txid, 20)) and ending "Moderator review required.".
func loadPaymentReviews(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || (d.User.Role != "moderator" && d.User.Role != "admin") {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `WITH v AS (SELECT pm.id,pm.order_id,o.state,pm.currency,pm.amount,pm.txid,pm.idx,pm.credited,pm.locked,pm.confirmations,
			pm.regress_notice,(pm.locked OR NOT pm.credited OR pm.regress_notice) AS open,f.at
		FROM payments pm JOIN orders o ON o.id=pm.order_id
		LEFT JOIN LATERAL (SELECT max(e.created) AS at FROM order_events e WHERE e.order_id=pm.order_id AND e.actor_id IS NULL
			AND e.note LIKE '%Moderator review required.%'
			AND strpos(e.note, ' '||CASE WHEN length(pm.txid)<=20 THEN pm.txid ELSE left(pm.txid,20)||'…' END||' ')>0) f ON true
		WHERE pm.flagged)
		SELECT order_id,state,currency,amount,txid,idx,credited,locked,confirmations,regress_notice,open,COALESCE(to_char(at,'YYYY-MM-DD HH24:MI "UTC"'),''),
			(SELECT count(*) FROM v WHERE NOT open)
		FROM (SELECT * FROM v WHERE open UNION ALL (SELECT * FROM v WHERE NOT open ORDER BY at DESC NULLS LAST,id DESC LIMIT $1)) s
		ORDER BY open DESC,CASE WHEN open THEN at END ASC NULLS FIRST,CASE WHEN open THEN id END,at DESC NULLS LAST,id DESC`, paymentReviewLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v PaymentReview
		var amt, confs int64
		var credited, locked, regressed bool
		if err = rows.Scan(&v.OrderID, &v.OrderState, &v.Currency, &amt, &v.TxID, &v.Index, &credited, &locked, &confs, &regressed, &v.Open, &v.Flagged, &d.PaymentReviewOthers); err != nil {
			return err
		}
		v.Amount, v.OrderState = amount(amt, currencyDecimals(v.Currency)), stateLabel(v.OrderState)
		threshold := a.confirmationThreshold(v.Currency)
		switch {
		case locked:
			v.Reason = "Locked transfer (unlock time), not counted or paid out"
		case !credited:
			v.Reason = "Deposit confirmed after settlement, not paid out"
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
	d.PaymentReviews = []PaymentReview{{OrderID: previewFlaggedID, OrderState: stateLabel(statePaid), Currency: "BTC", Amount: "0.001",
		TxID: "a6fca84477376ef06de0fa66091d6435fbd7be1abe70d867cdcc06529fbc952b", Reason: "Credited deposit conflicted or missing", Flagged: "Sample", Open: true}}
	d.PaymentReviewsOpen = 1
}
