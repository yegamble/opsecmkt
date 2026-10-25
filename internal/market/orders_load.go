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
	registerLoader("moderator", loadEscrowAlerts)

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
	stateShipped:   "/orders/ship",
	stateDelivered: "/orders/deliver",
	stateCompleted: "/orders/complete",
	stateDisputed:  "/disputes",
	stateCancelled: "/orders/cancel",
}

func transitionLabel(from, to string) string {
	switch to {
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
func viewerTransitions(o *Order, u *User) []Transition {
	roles := actorRoles(o, u)
	var out []Transition
	for _, to := range orderStates {
		if !o.Escrow && !(o.State == stateDraft && to == stateCancelled) {
			continue
		}
		if o.State == stateDisputed && (to == stateCompleted || to == stateCancelled) {
			continue // A-166 uses the dedicated confirmed concession form, never the ordinary action.
		}
		allowed, ok := transitions[o.State][to]
		action := transitionActions[to]
		if !ok || action == "" || !slices.ContainsFunc(roles, func(r string) bool {
			return slices.Contains(allowed, r) && !(o.Escrow && buyerCompleteRefused[[3]string{o.State, to, r}]) && (o.Escrow || !escrowOnly[[3]string{o.State, to, r}])
		}) {
			continue
		}
		if kind, ok := transitionKind[[2]string{o.State, to}]; ok && o.Kind != kind {
			continue
		}
		t := Transition{To: to, Label: transitionLabel(o.State, to), Action: action}
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
// from the order page) once it has a disputes row, keep reading it after a resolution or concession, read any order with a
// payment flagged for review (payments.flagged, set by the watcher's flagOrder), and read an escrow order
// while it has an open staff alert (escrowStaffAlert, A-470). orderReadable is the same rule in SQL.
func staffReviewer(ctx context.Context, a *App, o *Order, u *User) (bool, error) {
	if u == nil || u.ID == o.BuyerID || u.ID == o.VendorID || (u.Role != "moderator" && u.Role != "admin") {
		return false, nil
	}
	if disputeVisible(o) {
		return true, nil
	}
	var flagged bool
	err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM disputes WHERE order_id=$1) OR EXISTS(SELECT 1 FROM payments WHERE order_id=$1 AND flagged) OR "+escrowStaffAlert("$1"), o.ID).Scan(&flagged)
	return flagged, err
}

// escrowStaffAlert is SQL that is true while the escrow order whose id is the SQL expression order has an open staff
// alert (A-470): an escrow notice flagOrder sent to every moderator and administrator that has not cleared. Staff may
// read the order (staffReviewer, orderReadable) while it is open, whether or not a deposit is flagged; no action is
// offered for it. Each alert is read from the state that opens and clears it, never from a column of its own:
//   - the broadcast stall (A-441, escrow_stall.go): a settlement in state broadcast whose latest stall or seen note is
//     the stall note (escrowStalledSQL); it clears when the watch-only wallet sees the transaction (the seen note) or
//     the settlement leaves broadcast (confirmed, or spent outside the market, which flags the deposits);
//   - a release too few (A-461, failSettlement): a release that failed too-small, as long as it stands (state failed);
//     it clears when the release is superseded (an administrator's changed dispute decision, escrow_decision.go). Its
//     deposits are flagged as well (QA462-3), and those flags are never open (paymentFlagOpenRule), so no review of
//     them clears it;
//   - a Monero escrow deposit refused as another order's escrow account (A-480, xmrFlagCollision): an open record in
//     xmr_escrow_collisions (xmrCollisionOpen); it clears when staff record what was done about it on the order
//     page (xmrCollisionAction). It flags no deposit (none is in the ledger) and the order may have no settlement.
//   - a requested Monero escrow settlement older than 24 hours: it stays visible while requested and clears when the
//     settlement changes state. The threshold is for staff visibility, not a measured stagenet settlement bound.
//
// A new escrow notice through flagOrder either flags the deposits it concerns (as every other one does) or adds its
// open state here, and its reason to the moderation desk's list (loadEscrowAlerts).
func escrowStaffAlert(order string) string {
	return "(EXISTS(SELECT 1 FROM settlements s WHERE s.order_id=" + order + " AND (s.state='broadcast' AND " + escrowStalledSQL +
		" OR s.kind='release' AND s.state='failed' AND s.build_error='" + settleTooSmall + "')) OR " + xmrCollisionOpen(order) +
		" OR EXISTS(SELECT 1 FROM xmr_settlements xs WHERE xs.order_id=" + order + " AND xs.state='requested' AND xs.created<now()-interval '24 hours'))"
}

// escrowStaffAlertCandidates bounds desk and badge scans to orders with a possible alert source; the detailed helper
// decides whether any candidate is actually alerting now.
func escrowStaffAlertCandidates(order string) string {
	return "(" + order + " IN (SELECT order_id FROM settlements WHERE state IN ('broadcast','failed')) OR " +
		order + " IN (SELECT order_id FROM xmr_escrow_collisions WHERE disposed_at IS NULL) OR " +
		order + " IN (SELECT order_id FROM xmr_settlements WHERE state='requested' AND created<now()-interval '24 hours'))"
}

// xmrCollisionOpen is SQL that is true while the order whose id is the SQL expression order has an open record of a
// refused Monero escrow deposit (A-480).
func xmrCollisionOpen(order string) string {
	return "EXISTS(SELECT 1 FROM xmr_escrow_collisions c WHERE c.order_id=" + order + " AND c.disposed_at IS NULL)"
}

// escrowAlertLimit bounds the moderation desk's list of escrow orders with an open staff alert.
const escrowAlertLimit = 100

// loadEscrowAlerts lists, on the moderation desk, the escrow orders with an open staff alert (escrowStaffAlert, the
// rule that lets staff read them), newest first, each linked to its order page, so that a moderator can open one from
// the notice: a notification names only a short order ID, and a stall or a refused Monero deposit flags no deposit, so
// the payment review list does not show it. Newest first by when the order's open alert opened (the latest, when it
// has several: a stall by its stall note, a release too few by its settlement's last update, which recorded the
// failure, a refused deposit by its record's noted time, a requested Monero settlement by the time it crossed 24 hours;
// QA5W2-3), not by the order's age, because the alert a
// notification just announced is the newest; ties by order ID. When more than escrowAlertLimit are open the desk says
// how many it leaves out (EscrowAlertsMore). Nothing here is an action (a refused deposit's disposition is recorded on
// the order page). The prefilter names every table an alert is read from: an order with a refused deposit may have no
// settlement. The requested-settlement candidate list is included explicitly because it has no Bitcoin settlement or
// collision row.
func loadEscrowAlerts(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || (d.User.Role != "moderator" && d.User.Role != "admin") {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT o.id,o.state,
		EXISTS(SELECT 1 FROM settlements s WHERE s.order_id=o.id AND s.state='broadcast' AND `+escrowStalledSQL+`),
		EXISTS(SELECT 1 FROM settlements s WHERE s.order_id=o.id AND s.kind='release' AND s.state='failed' AND s.build_error='`+settleTooSmall+`'),
		`+xmrCollisionOpen("o.id")+`,
		EXISTS(SELECT 1 FROM xmr_settlements xs WHERE xs.order_id=o.id AND xs.state='requested' AND xs.created<now()-interval '24 hours'),
		count(*) OVER ()
		FROM orders o WHERE `+escrowStaffAlertCandidates("o.id")+` AND `+escrowStaffAlert("o.id")+`
		ORDER BY GREATEST((SELECT max(`+escrowStallOpenedSQL+`) FROM settlements s WHERE s.order_id=o.id AND s.state='broadcast' AND `+escrowStalledSQL+`),
			(SELECT max(s.updated) FROM settlements s WHERE s.order_id=o.id AND s.kind='release' AND s.state='failed' AND s.build_error='`+settleTooSmall+`'),
			(SELECT max(x.noted) FROM xmr_escrow_collisions x WHERE x.order_id=o.id AND x.disposed_at IS NULL),
			(SELECT max(xs.created+interval '24 hours') FROM xmr_settlements xs WHERE xs.order_id=o.id AND xs.state='requested' AND xs.created<now()-interval '24 hours')) DESC NULLS LAST,o.id DESC
		LIMIT $1`, escrowAlertLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	var total int
	for rows.Next() {
		var v EscrowAlertView
		if err = rows.Scan(&v.ID, &v.State, &v.Stall, &v.TooFew, &v.Refused, &v.Requested, &total); err != nil {
			return err
		}
		v.State = stateLabel(v.State)
		d.EscrowAlerts = append(d.EscrowAlerts, v)
	}
	d.EscrowAlertsMore = max(0, total-len(d.EscrowAlerts))
	return rows.Err()
}

// disputeVisible: the order is or was disputed, so a reviewer may also read its digital delivery.
func disputeVisible(o *Order) bool { return o.State == stateDisputed || o.State == stateResolved }

// payoutTxidHidden stands in for a refund's transaction ID in text shown to the vendor (A-214).
const payoutTxidHidden = "(transaction ID shown only to its recipient)"

// hidePayoutTxids replaces in each text, when viewer is the order's vendor, the transaction ID of a refund sent to
// the buyer by hand, in full or as truncate(txid, 20): on Bitcoin a txid reveals its recipient's address. A refund
// staff recorded as sent by hand (A-31, payments.disposition_txid) is named in its order-history note, as is a refund
// an operator recorded with UPGRADING.md's relabel block (sections 14 and 18, A-312), which exists only in that system
// note; the rows stay unchanged. Staff reviewers who are not parties read the text as stored.
func hidePayoutTxids(ctx context.Context, a *App, order, viewer string, texts ...*string) error {
	rows, err := a.db.QueryContext(ctx, `SELECT pm.disposition_txid FROM payments pm JOIN orders o ON o.id=pm.order_id JOIN products p ON p.id=o.product_id
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
	var staffDispute bool
	if d.OrderViewer == roleModerator && !disputeVisible(o) {
		if err = a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM disputes WHERE order_id=$1)", o.ID).Scan(&staffDispute); err != nil {
			return err
		}
	}
	if d.OrderViewer == roleModerator && !disputeVisible(o) && !staffDispute {
		// Staff read this order because a deposit is flagged or its escrow has an open staff alert. The help says which
		// (QA5W1-1): a flag keeps the payment review text, and the alert alone names itself and links the desk's list.
		var flagged, stall, tooFew, refused, requested bool
		if err = a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM payments WHERE order_id=$1 AND flagged),
			EXISTS(SELECT 1 FROM settlements s WHERE s.order_id=$1 AND s.state='broadcast' AND `+escrowStalledSQL+`),
			EXISTS(SELECT 1 FROM settlements s WHERE s.order_id=$1 AND s.kind='release' AND s.state='failed' AND s.build_error='`+settleTooSmall+`'),
			`+xmrCollisionOpen("$1")+`,
			EXISTS(SELECT 1 FROM xmr_settlements xs WHERE xs.order_id=$1 AND xs.state='requested' AND xs.created<now()-interval '24 hours')`, o.ID).Scan(&flagged, &stall, &tooFew, &refused, &requested); err != nil {
			return err
		}
		if !flagged && (stall || tooFew || refused || requested) {
			d.StaffAlert = &EscrowAlertView{ID: o.ID, Stall: stall, TooFew: tooFew, Refused: refused, Requested: requested}
		}
	}
	if d.OrderViewer == roleModerator {
		// A-480: staff who are not parties see the order's refused Monero deposits, and record what was done about an
		// open one here; the parties see only the order note.
		if err = loadXMRCollisions(ctx, a, r, d); err != nil {
			return err
		}
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
	d.DeliveryWithheld = d.OrderViewer == roleModerator && !disputeVisible(o) && !staffDispute
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
	if o.State == stateDisputed && len(d.Disputes) == 1 && disp.Status == "Open" &&
		(o.BuyerID != o.VendorID) && (d.OrderViewer == roleBuyer || d.OrderViewer == roleVendor) {
		if d.Concession, err = a.concessionPreview(ctx, o, d.OrderViewer); err != nil {
			return err
		}
		if d.Security == nil {
			if d.Security, _, _, err = a.securityView(ctx, u.ID); err != nil {
				return err
			}
		}
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
		d.Transitions = viewerTransitions(o, u)
		if d.Overpaid, err = a.escrowOverpaid(ctx, o); err != nil {
			return err
		}
	}
	if err = a.escrowOrder(ctx, d); err != nil {
		return err
	}
	if d.Contacts, err = orderContacts(ctx, a, o, d.OrderViewer); err != nil {
		return err
	}
	vendorKeyGate(d)
	if d.OrderViewer == roleVendor && o.Kind == "physical" && o.State == statePaid {
		ok, err := userCanReceiveEncrypted(ctx, a.db, o.VendorID)
		if err != nil {
			return err
		}
		d.VendorKeyUnusable = !ok
	}
	if err := vendorKeyChangeGate(ctx, a, d); err != nil {
		return err
	}
	if err := suspendedVendorOrderGate(ctx, a, d); err != nil {
		return err
	}
	if err := vendorBondOrderGate(ctx, a, d); err != nil {
		return err
	}
	return subFloorOrderGate(ctx, a, d)
}

// A suspended vendor keeps every existing order and its history, but the buyer's stale draft can take no new
// payment. Replace all three intake forms with one reason; cancellation remains available.
func suspendedVendorOrderGate(ctx context.Context, a *App, d *PageData) error {
	if d.Order == nil || d.Order.State != stateDraft || d.OrderViewer != roleBuyer {
		return nil
	}
	var suspended bool
	if err := a.db.QueryRowContext(ctx, "SELECT suspended_at IS NOT NULL FROM users WHERE id=$1", d.Order.VendorID).Scan(&suspended); err != nil {
		return err
	}
	if !suspended {
		return nil
	}
	d.VendorSuspensionReason = "Unavailable. " + sellerSuspended
	for i := range d.Transitions {
		if d.Transitions[i].To == stateAwaitingPayment {
			d.Transitions[i].Action = ""
			d.Transitions[i].Label = sellerSuspended + " You can cancel this draft."
		}
	}
	if d.EscrowCheckout != nil {
		d.EscrowCheckout.Reason = sellerSuspended
	}
	if d.XMREscrowCheckout != nil {
		d.XMREscrowCheckout.Reason = sellerSuspended
	}
	return nil
}

// subFloorOrderGate (A-230(b), A-241(c)) keeps a buyer's legacy draft from offering a payment step that the
// current listing price cannot settle above the escrow settlement floor. The POST remains guarded independently.
func subFloorOrderGate(ctx context.Context, a *App, d *PageData) error {
	o := d.Order
	if o == nil || o.State != stateDraft || d.OrderViewer != roleBuyer {
		return nil
	}
	var btc, xmr int64
	if err := a.db.QueryRowContext(ctx, "SELECT btc,xmr FROM products WHERE id=$1", o.ProductID).Scan(&btc, &xmr); err != nil {
		return err
	}
	price, otherPrice := btc, xmr
	if o.Currency == "XMR" {
		price, otherPrice = xmr, btc
	}
	if price >= payoutFloor(o.Currency, "release") {
		return nil
	}
	message := subFloorListing(o.Currency, otherPrice)
	for i := range d.Transitions {
		if d.Transitions[i].To != stateAwaitingPayment {
			continue
		}
		if d.Transitions[i].Action == "" && d.Transitions[i].Label != "" {
			d.Transitions[i].Label += " " + message
		} else {
			d.Transitions[i].Action, d.Transitions[i].Label = "", message
		}
	}
	return nil
}

const resolveConfirm = "I checked the amount and who receives it for the outcome I chose. Resolving is final."

// resolvePayouts (A-161) fills the resolve form of every open dispute on a disputed order the staff viewer is not
// party to: each escrow outcome's settlement (escrowDisputePreview).
func (a *App) resolvePayouts(ctx context.Context, d *PageData) error {
	d.DisputePayouts = map[string]PayoutPreview{}
	if d.User == nil || (d.User.Role != "moderator" && d.User.Role != "admin") {
		return nil
	}
	for _, v := range d.Disputes[:d.OpenDisputes] {
		o, ok := d.DisputeOrders[v.OrderID]
		// A dispute on an order without escrow has no outcome to offer.
		if !ok || !o.Escrow || o.State != stateDisputed || d.User.ID == o.BuyerID || d.User.ID == o.VendorID {
			continue
		}
		// Decided by an administrator and paid by a settlement from the escrow (escrow_decision.go).
		p, err := a.escrowDisputePreview(ctx, &o)
		if err != nil {
			return err
		}
		d.DisputePayouts[v.ID] = p
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

// loadIncomingOrders shows all current vendor work, newest change first, plus a bounded closed history.
// Drafts and cancellations without an escrow or a payment (cancelledCustomerWork) are buyer-side abandoned checkout
// attempts.
func loadIncomingOrders(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	ctxQ := `p.vendor_id=$1 AND (
		o.state IN ('awaiting_payment','paid','shipped','delivered','disputed') OR
		o.id IN (SELECT o2.id FROM orders o2 JOIN products p2 ON p2.id=o2.product_id WHERE p2.vendor_id=$1 AND (
			o2.state IN ('completed','resolved') OR
			(o2.state='cancelled' AND ` + cancelledCustomerWork("o2") + `)
		) ORDER BY o2.updated DESC,o2.id DESC LIMIT $2)
	)`
	orders, err := queryOrders(ctx, a, orderQuery+" WHERE "+ctxQ+" ORDER BY o.updated DESC,o.id DESC", d.User.ID, historyLimit+1)
	if err != nil {
		return err
	}
	var terminalCount int
	if err = a.db.QueryRowContext(ctx, `SELECT count(*) FROM orders o JOIN products p ON p.id=o.product_id
		WHERE p.vendor_id=$1 AND (o.state IN ('completed','resolved') OR
		(o.state='cancelled' AND `+cancelledCustomerWork("o")+`))`, d.User.ID).Scan(&terminalCount); err != nil {
		return err
	}
	if terminalCount > historyLimit {
		d.HistoryLimit = historyLimit
	}
	if len(orders) > 0 && terminalCount > historyLimit {
		// Keep all active work and only the newest historyLimit terminal rows. The SQL has
		// fetched one extra terminal row so the count remains independent of active volume.
		kept, terminal := make([]Order, 0, len(orders)), 0
		for _, o := range orders {
			switch o.State {
			case stateCompleted, stateResolved, stateCancelled:
				if terminal >= historyLimit {
					continue
				}
				terminal++
			}
			kept = append(kept, o)
		}
		orders = kept
	}
	d.IncomingOrders = orders
	if err = a.db.QueryRowContext(ctx, "SELECT count(*) FROM orders o JOIN products p ON p.id=o.product_id WHERE p.vendor_id=$1 AND o.state=$2", d.User.ID, statePaid).Scan(&d.NeedsAction); err != nil {
		return err
	}
	return a.db.QueryRowContext(ctx, `SELECT count(*) FROM disputes d JOIN orders o ON o.id=d.order_id
		JOIN products p ON p.id=o.product_id WHERE p.vendor_id=$1 AND d.status='Open'`, d.User.ID).Scan(&d.VendorOpenDisputes)
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
// historyLimit. Each dispute gets its order summary; an open one is marked NoResolver when no unsuspended
// moderator or administrator independent of the parties is available.
func loadDisputes(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	d.DisputeOrders = map[string]Order{}
	if d.User == nil {
		return nil
	}
	if d.Page == "disputes" {
		var counts DisputePartyCounts
		if err := a.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE d.status='Open'),
			count(*) FILTER (WHERE d.status<>'Open'),count(*) FILTER (WHERE d.status LIKE 'Withdrawn — conceded by %')
			FROM disputes d JOIN orders o ON o.id=d.order_id JOIN products p ON p.id=o.product_id
			WHERE o.buyer_id=$1 OR p.vendor_id=$1`, d.User.ID).Scan(&counts.Open, &counts.Closed, &counts.Withdrawn); err != nil {
			return err
		}
		d.DisputePartyCounts = &counts
	}
	d.DisputePartyFilter = ""
	partyFilter := ""
	queryArgs := []any{d.User.ID, d.User.Role}
	resolvedLimitArg := "$3"
	if d.Page == "moderator" {
		d.DisputePartyFilter = strings.TrimSpace(r.URL.Query().Get("party"))
		partyFilter = ` AND ($3='' OR EXISTS(SELECT 1 FROM users buyer WHERE buyer.id=o.buyer_id AND buyer.handle=$3) OR EXISTS(SELECT 1 FROM users vendor WHERE vendor.id=p.vendor_id AND vendor.handle=$3))`
		queryArgs = append(queryArgs, d.DisputePartyFilter)
		resolvedLimitArg = "$4"
	}
	scope := `SELECT d.id,d.order_id,d.reason,d.status,d.resolution,to_char(d.created,'YYYY-MM-DD HH24:MI "UTC"'),d.status='Open' AND NOT EXISTS(SELECT 1 FROM users s WHERE s.role IN ('moderator','admin') AND s.suspended_at IS NULL AND s.id<>o.buyer_id AND s.id<>p.vendor_id),` +
		disputeRecordCols + ` FROM disputes d JOIN orders o ON o.id=d.order_id JOIN products p ON p.id=o.product_id` + disputeReplyJoin + ` WHERE (o.buyer_id=$1 OR p.vendor_id=$1 OR $2 IN ('admin','moderator'))` + partyFilter
	open, err := queryDisputes(ctx, a, scope+" AND d.status='Open' ORDER BY d.created,d.id", queryArgs...)
	if err != nil {
		return err
	}
	resolvedArgs := append(slices.Clone(queryArgs), historyLimit+1)
	resolved, err := queryDisputes(ctx, a, scope+" AND d.status<>'Open' ORDER BY d.created DESC,d.id DESC LIMIT "+resolvedLimitArg, resolvedArgs...)
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

// previewPaidOrder replaces the preview draft with a paid sample order and its timeline. No wallet is connected.
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
}

func previewOrder(d *PageData) {
	if d.Order == nil || d.User == nil {
		return
	}
	d.Transitions = viewerTransitions(d.Order, d.User)
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
	d.DisputePayouts = map[string]PayoutPreview{}
}

// paymentReviewLimit caps the other (not open) flags on the moderation desk's payment-review list.
const paymentReviewLimit = 100

// loadPaymentReviews lists deposits the payment watcher flagged for staff review (payments.flagged): every open
// flag, oldest first and never capped, then the paymentReviewLimit most recently flagged others, with their
// count. Whether a flag is open is the one rule paymentFlagOpenRule (QA462C), which the review handler reads too
// (and only a flag it opens holds a late-deposit notice episode): a locked transfer, a deposit below the escrow
// minimum output (A-99; historical rows, nothing sets it now), a credited deposit whose regression was announced and has not confirmed again, an
// escrow deposit spent outside the market, or a deposit not credited that does not count for the order (pooled, or an
// escrow one recorded after the close or left out of its settlement), until staff record its disposition (an
// escalated one stays open). An escrow deposit that counts for the order is not open, credited or not (QA462B-1). A
// disposed flag is not listed (the order history and the audit log keep the disposition). The reason comes from the
// deposit's current confirmations, against the escrow's threshold for an escrow deposit. The flag time is the latest
// watcher flag event for the deposit in order_events: flagOrder writes a system note naming it (truncate(txid, 20))
// and ending "Moderator review required.".
func loadPaymentReviews(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || (d.User.Role != "moderator" && d.User.Role != "admin") {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `WITH v AS (SELECT pm.id,pm.order_id,o.state,pr.id AS product_id,CASE WHEN o.state='draft' THEN pr.title ELSE COALESCE(o.listing_title,'Listing terms were not recorded for this order') END AS title,pm.currency,pm.amount,pm.txid,pm.idx,pm.credited,pm.locked,pm.confirmations,
			pm.regress_notice,pm.below_floor,`+paymentFlagOpen("pm")+` AS open,f.at,
			pm.disposition='escalated' AS escalated,COALESCE(to_char(pm.disposed_at,'YYYY-MM-DD'),'') AS disposed,(o.buyer_id=$2 OR pr.vendor_id=$2) AS party,
			c.escrow,pm.spent_outside,COALESCE(pm.spent_by,'') AS spent_by,c.counted,pm.escrow_left_out AND NOT pm.after_close AS left_out
		FROM payments pm JOIN orders o ON o.id=pm.order_id JOIN products pr ON pr.id=o.product_id
		JOIN escrows esc ON esc.order_id=o.id
		CROSS JOIN LATERAL (SELECT e.escrow,e.escrow AND NOT pm.after_close AND NOT pm.escrow_left_out AND NOT pm.spent_outside AS counted
			FROM (SELECT EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=pm.order_id) AS escrow) e) c
		LEFT JOIN LATERAL (SELECT max(e.created) AS at FROM order_events e WHERE e.order_id=pm.order_id AND e.actor_id IS NULL
			AND lower(e.note) LIKE '%moderator review required.%'
			AND (strpos(e.note,'deposit '||CASE WHEN length(pm.txid)<=20 THEN pm.txid ELSE left(pm.txid,20)||'…' END||' '||
				CASE WHEN pm.currency='XMR' AND EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=pm.order_id)
					THEN 'account '||(pm.idx&4294967295)::text ELSE 'output '||pm.idx::text END||' (')>0
			OR strpos(e.note,CASE WHEN length(pm.txid)<=20 THEN pm.txid ELSE left(pm.txid,20)||'…' END||':'||pm.idx::text||' (')>0
			OR (strpos(e.note,'deposit '||CASE WHEN length(pm.txid)<=20 THEN pm.txid ELSE left(pm.txid,20)||'…' END||' output ')=0
					AND strpos(e.note,'deposit '||CASE WHEN length(pm.txid)<=20 THEN pm.txid ELSE left(pm.txid,20)||'…' END||' account ')=0
					AND strpos(e.note,CASE WHEN length(pm.txid)<=20 THEN pm.txid ELSE left(pm.txid,20)||'…' END||':')=0
					AND NOT EXISTS(SELECT 1 FROM order_events indexed WHERE indexed.order_id=pm.order_id AND indexed.actor_id IS NULL
						AND (strpos(indexed.note,'deposit '||CASE WHEN length(pm.txid)<=20 THEN pm.txid ELSE left(pm.txid,20)||'…' END||' output ')>0
							OR strpos(indexed.note,'deposit '||CASE WHEN length(pm.txid)<=20 THEN pm.txid ELSE left(pm.txid,20)||'…' END||' account ')>0
							OR strpos(indexed.note,CASE WHEN length(pm.txid)<=20 THEN pm.txid ELSE left(pm.txid,20)||'…' END||':')>0))
					AND strpos(e.note, ' '||CASE WHEN length(pm.txid)<=20 THEN pm.txid ELSE left(pm.txid,20)||'…' END||' ')>0))) f ON true
		WHERE pm.flagged AND pm.disposition IN ('','escalated'))
		SELECT id,order_id,state,product_id,title,currency,amount,txid,idx,credited,locked,confirmations,regress_notice,below_floor,open,COALESCE(to_char(at,'YYYY-MM-DD HH24:MI "UTC"'),''),
			escalated,disposed,party,escrow,spent_outside,spent_by,counted,left_out,(SELECT count(*) FROM v WHERE NOT open)
		FROM (SELECT * FROM v WHERE open UNION ALL (SELECT * FROM v WHERE NOT open ORDER BY at DESC NULLS LAST,id DESC LIMIT $1)) s
		ORDER BY open DESC,CASE WHEN open THEN at END ASC NULLS FIRST,CASE WHEN open THEN id END,at DESC NULLS LAST,id DESC`, paymentReviewLimit, d.User.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v PaymentReview
		var amt, confs int64
		var credited, locked, regressed, belowFloor, escrow, outside, counted, leftOut bool
		var disposed, spentBy string
		if err = rows.Scan(&v.ID, &v.OrderID, &v.OrderState, &v.ProductID, &v.Title, &v.Currency, &amt, &v.TxID, &v.Index, &credited, &locked, &confs, &regressed, &belowFloor, &v.Open, &v.Flagged,
			&v.Escalated, &disposed, &v.Party, &escrow, &outside, &spentBy, &counted, &leftOut, &d.PaymentReviewOthers); err != nil {
			return err
		}
		// The disposition form (paymentReviewAction): open flags of orders the viewer is not party to; an escalated
		// one only for an administrator. A credited deposit is never refunded by hand, nor is an escrow deposit: its
		// coins are in the order's escrow, never in a wallet staff could send from, and a stray refund returns them
		// (A-330).
		v.Form = v.Open && !v.Party && (!v.Escalated || d.User.Role == "admin")
		v.Refundable, v.Escrow = !credited && !escrow, escrow
		v.Amount, v.OrderState = amount(amt, currencyDecimals(v.Currency)), stateLabel(v.OrderState)
		threshold := int64(0)
		if escrow {
			// An escrow deposit's own confirmation threshold, as its order page reads it (escrowOrder, xmrEscrowOrder),
			// not the pooled wallet's (0 when no pooled provider is configured).
			switch e := a.escrow.Load(); {
			case v.Currency == "XMR" && a.escrowXMR != nil:
				threshold = a.escrowXMR.confirmations
			case v.Currency == "XMR":
				threshold = xmrMinConfirmations
			case e != nil:
				threshold = e.confirmations
			default:
				threshold = 3
			}
		}
		switch {
		case outside:
			// P1-d1: an escrow deposit no settlement of the market covered, spent by another transaction.
			v.Reason = "Escrow deposit spent by a transaction this market did not make, with no settlement of the market covering it"
			if spentBy != "" {
				v.Reason += " (transaction " + spentBy + ")"
			}
		case locked:
			v.Reason = "Locked transfer (unlock time), not counted or paid out"
		case belowFloor && (!credited || confs >= threshold):
			v.Reason = "Below the escrow minimum output, not paid as its own output"
			if confs < 0 {
				v.Reason += "; now conflicted or missing"
			}
		case !credited && counted:
			// QA462B-1: an escrow deposit recorded before the order closed but not credited at funding (still
			// confirming, or conflicted and mined since) counts for the order, as a credited one does: its flag is not
			// open and the desk offers no action on it (A-330).
			v.Reason = "Escrow deposit counted for the order, though not credited toward its funding (recorded before the order closed); no stray refund returns it"
			if confs < 0 {
				v.Reason += "; now conflicted or missing"
			}
		case escrow && leftOut:
			// Left out of its settlement before the close (a top-up after it was built, one that did not count when the
			// order was cancelled, one a refund too few could not spend, credited or not: QA462D-2): not a deposit after
			// the close, and no longer counted.
			v.Reason = "Escrow deposit left out of its settlement, not counted; a stray refund from the escrow returns it to the buyer"
			if confs < 0 {
				v.Reason += "; now conflicted or missing"
			}
		case !credited && escrow:
			v.Reason = "Deposit to an escrow after the order closed, not counted; a stray refund from the escrow returns it to the buyer"
			if confs < 0 {
				v.Reason += "; now conflicted or missing"
			}
		case !credited:
			v.Reason = "Deposit after settlement or after the order closed, not paid out"
			if confs < 0 {
				v.Reason += "; now conflicted or missing"
			} else if confs == 0 || confs < threshold {
				v.Reason = fmt.Sprintf("Deposit still unconfirmed (%d confirmations), not paid out; verify confirmation in the wallet before paying by hand", confs)
				if threshold > 0 {
					v.Reason = fmt.Sprintf("Deposit still confirming (%d of %d confirmations), not paid out; verify confirmation in the wallet before paying by hand", confs, threshold)
				}
			}
		case escrow && !regressed && !belowFloor:
			// QA462D-3: a credited escrow deposit flagged with no regression (a release too few, A-461; an external
			// settlement's deposits) counts for the order: its flag is not open, and it never "confirmed again". One below
			// the floor is open and keeps the reasons below while it confirms.
			v.Reason = "Escrow deposit counted for the order (credited toward its funding); the order's release or refund settles it"
			if confs < 0 {
				v.Reason += "; now conflicted or missing"
			} else if confs < threshold {
				v.Reason += fmt.Sprintf("; now %d of %d confirmations", confs, threshold)
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
