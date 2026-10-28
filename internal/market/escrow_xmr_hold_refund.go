package market

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// A-652f (owner decisions 2026-10-26): the end of a ban hold. A release held for a banned vendor (A-652c,
// xmrReleaseHeld) that is still held ban_hold_refund_days after the hold began, with the ban still in place, is turned
// into a refund to the order's pinned buyer refund address (escrows.refund_address) by an administrator who is not a
// party, one order at a time, on the order page (POST /admin/escrow-xmr-hold-refund: password and authenticator code,
// a reason of 20–5000 characters, a receipt nonce). This applies to an order the buyer confirmed, one completed at its
// release deadline and one decided (or conceded) for the vendor, and also, with no release row at all, to a completed
// digital order whose vendor never claimed and to one whose dispute was decided (or conceded) for the vendor before any
// release was requested (a physical order disputed before it was shipped). Nobody new is paid: the order moves
// completed (or resolved) -> cancelled on the staff edge roleHoldRefund, which only this action marks (withHoldRefund),
// and escrowSettleTerminal then supersedes the requested release (if any) and inserts exactly one refund to
// escrows.refund_address. No settlement kind, output role or insert site is added (TestNoForfeitSettlementKind).
//
// The hold began at GREATEST(users.banned_at, the order's move into its current state: its completion, or the dispute's
// decision or concession): lifting the ban ends the hold
// (the watcher then builds the release), and a new ban starts the clock again. The days are an administrator setting
// (banHoldRefundDaysSetting: 30 to 3650, default 90; password step-up; audited old -> new).
//
// Locks, in this order, in the confirming transaction: the vendor's account row FOR SHARE (a ban or unban takes it FOR
// NO KEY UPDATE, so the ban cannot be lifted while this runs), then the order row FOR UPDATE, then the live release FOR
// UPDATE. A ban locks its shipped orders before the vendor's account row (lockBanShippedOrders, PAY90-2) and an order
// message locks its order before a partner's account (FOR SHARE, which does not conflict with this one); neither is a
// cycle with this order: this action takes only a completed or resolved order, which a ban never locks (it locks
// shipped and delivered orders only), and a ban or unban takes no order lock after the account row except the ban's
// NOWAIT pass over those orders. The watcher locks the order and then the settlement (storeXMRBuilt), the same order as here. Where there is no
// release, none can appear meanwhile: every path that inserts one (the vendor's claim, insertXMRRelease after
// partyOrderLocked; an administrator's re-request, of a failed release with a transaction, which is refused here anyway)
// first locks the order row FOR NO KEY UPDATE, which waits for this FOR UPDATE and then finds the order cancelled
// (settlementFits refuses a release); one that committed first is read after the lock and changes the digest (409). A
// banned vendor has no session to claim with, and xmr_settlements_one_outcome refuses a second live outcome besides.
//
// Under those locks: the order is a Monero escrow order, completed, or resolved for the vendor; the vendor is banned
// and has been for the days; the live release is requested, was never built and supersedes nothing, and no release of
// the order ever had a transaction (a release that may be on the network, or a re-request of one, is never superseded);
// or there is no release row at all and the order is a completed digital one or was decided for the vendor; no deposit
// was spent outside the market and no settlement of the order was spent by another transaction (external); the order's
// escrow wallet is not destroyed (A-672: a refund from it could never be built); something is counted; the restore gate
// is clear and a restore's sweep of the escrow wallets has completed. The form states a digest of the release, the
// order's state, the counted sum and when the hold began; any change refuses it (409). Every refusal says "Nothing was
// changed".
//
// Afterwards a deposit that arrives late, or one left out, goes to the stray-refund path as for any cancelled order;
// both parties are notified (transition) and the order history records it; the administrator's and the vendor's
// accounts are audited with the reason; the order's review stops counting (reviews count only on completed orders) and
// the vendor's reputation figures count it as a held-release refund (reputation.go), never a no-ship or vendor cancel.

const (
	banHoldRefundDaysSetting = "ban_hold_refund_days"
	defaultBanHoldRefundDays = 90
	minBanHoldRefundDays     = 30
	maxBanHoldRefundDays     = 3650
	minHoldRefundReason      = 20
	maxHoldRefundReason      = 5000
)

func init() {
	registerAction("/admin/escrow-xmr-hold-refund", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true,
		ReceiptAuthorize: holdRefundReceiptAuthorize, Run: holdRefundAction})
	registerAction("/admin/ban-hold-refund-days", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: banHoldRefundDaysAction})
	registerLoader("admin", banHoldRefundDaysLoader)
}

// holdRefundNote is the order event and, after "Order X is now Cancelled", both parties' notification.
const holdRefundNote = "Cancelled — the vendor's account stayed closed while the release to the vendor was held, so an administrator refunded this order instead: the coins in its escrow go back to the buyer's refund address given at checkout, and nothing is paid to the vendor"

var (
	errHoldRefundParty     = fail(403, "You are a party to this order, so another administrator decides it. Nothing was changed.")
	errHoldRefundState     = fail(409, "Only a completed Monero escrow order, or one whose dispute was decided for the vendor, whose release to a banned vendor is held, can be refunded here. Nothing was changed.")
	errHoldRefundNotBanned = fail(409, "The vendor's account is not banned, so the release is not held: the market builds and sends it as usual. Nothing was changed.")
	errHoldRefundOnNetwork = fail(409, "The release to the vendor is not a held request: it was built or may be on the network (built, relayed, confirmed, spent by another transaction, failed with a transaction, or a re-request of one), or it failed, so it is never replaced by a refund. Nothing was changed.")
	errHoldRefundNoRelease = fail(409, "This order has no release to the vendor: only a completed digital order whose vendor never claimed, or an order whose dispute was decided for the vendor before any release was requested, is refunded without one. Nothing was changed.")
	errHoldRefundSpent     = fail(409, "A deposit of this escrow was spent outside the market, or a settlement's coins were spent by another transaction: settle it with the operator. Nothing was changed.")
	errHoldRefundDestroyed = fail(409, "This order's escrow wallet was destroyed, so the market can never open it to send a refund: settle it with the operator. Nothing was changed.")
	errHoldRefundNothing   = fail(409, "Nothing is counted in this order's escrow, so there is nothing to refund. Nothing was changed.")
	errHoldRefundStale     = fail(409, "This order's release or escrow changed since the page was shown. Reload the order page and review it again. Nothing was changed.")
)

// banHoldRefundDays is the hold length in force: the administrator's setting, or the default when none was saved. A
// stored value outside the range is refused rather than guessed (503).
func banHoldRefundDays(ctx context.Context, q rowQuerier) (int, error) {
	var v sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT (SELECT value FROM settings WHERE key=$1)`, banHoldRefundDaysSetting).Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return defaultBanHoldRefundDays, nil
	}
	n, err := strconv.Atoi(v.String)
	if err != nil || n < minBanHoldRefundDays || n > maxBanHoldRefundDays {
		return 0, fail(503, "The held-release refund setting is unreadable. Nothing was changed.")
	}
	return n, nil
}

// holdRefundDaysShown is banHoldRefundDays for the advisory texts of a banned vendor's order (completion notes, Confirm
// receipt help): an unreadable stored value shows the default rather than failing the buyer's confirmation, the release
// deadline or the page; the refund itself still refuses it (503).
func holdRefundDaysShown(ctx context.Context, q rowQuerier) (int, error) {
	days, err := banHoldRefundDays(ctx, q)
	var he *httpError
	if errors.As(err, &he) {
		return defaultBanHoldRefundDays, nil
	}
	return days, err
}

// holdRefundLater is the sentence the order history adds to a banned vendor's completion (PAY90-3): the buyer may still
// be refunded.
func holdRefundLater(days int) string {
	return "If the vendor's account is still closed " + strconv.Itoa(days) + " days from now, an administrator may refund the coins to the buyer's refund address given at checkout instead."
}

// BanHoldRefundDaysView is the held-release refund period in force on Admin → Payments and the bounds its form accepts.
type BanHoldRefundDaysView struct {
	Days     string
	Min, Max int
}

func banHoldRefundDaysLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" {
		return nil
	}
	n, err := banHoldRefundDays(ctx, a.db)
	if err != nil {
		return err
	}
	d.BanHoldRefundDays = &BanHoldRefundDaysView{Days: strconv.Itoa(n), Min: minBanHoldRefundDays, Max: maxBanHoldRefundDays}
	return nil
}

// banHoldRefundDaysAction saves the held-release refund period: a whole number of days from 30 to 3650, confirmed with
// the administrator's password (and authenticator code when enrolled). The audit row names the old and new days only.
func banHoldRefundDaysAction(c *actionCtx) (actionResult, error) {
	days, err := strconv.Atoi(strings.TrimSpace(c.Form.Get("hold_refund_days")))
	if err != nil || days < minBanHoldRefundDays || days > maxBanHoldRefundDays {
		return actionResult{}, fail(400, "Enter the held-release refund period as a whole number of days from "+strconv.Itoa(minBanHoldRefundDays)+" to "+strconv.Itoa(maxBanHoldRefundDays)+". Nothing was changed.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		if err := lockSetting(ctx, c.Tx, banHoldRefundDaysSetting); err != nil {
			return actionResult{}, err
		}
		old, err := banHoldRefundDays(ctx, c.Tx)
		if err != nil {
			return actionResult{}, err
		}
		if old == days {
			return actionResult{}, fail(409, "The held-release refund period is already that many days; nothing was changed.")
		}
		if err = setSetting(c, banHoldRefundDaysSetting, strconv.Itoa(days)); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/admin/payments", Notice: "ban-hold-refund-days",
			Audit: "Changed the held-release refund period for banned vendors: " + strconv.Itoa(old) + " → " + strconv.Itoa(days) + " days"}, nil
	})
}

// holdRefundOrderSQL is SQL, over an orders alias o joined to products p, that is true while o is a banned vendor's
// finished Monero escrow order whose release was never sent: completed or resolved, no refund, and no release that was
// built, re-requested, failed or spent elsewhere (a requested one is held; A-652f). It is the order page's extra read
// rule for a non-party administrator (holdRefundReadSQL): a completed digital order whose vendor never claimed has no
// escrow staff alert.
const holdRefundOrderSQL = `(o.state IN ('completed','resolved') AND EXISTS(SELECT 1 FROM users hv WHERE hv.id=p.vendor_id AND hv.banned_at IS NOT NULL)
	AND EXISTS(SELECT 1 FROM escrows hes WHERE hes.order_id=o.id AND hes.coin='XMR')
	AND NOT EXISTS(SELECT 1 FROM xmr_settlements hs WHERE hs.order_id=o.id AND hs.kind IN ('release','refund')
		AND (hs.kind='refund' OR hs.tx_hash IS NOT NULL OR hs.supersedes IS NOT NULL OR hs.state NOT IN ('requested','superseded'))))`

// holdRefundReadSQL is holdRefundOrderSQL for viewer with role (SQL placeholders), an administrator who can act: the
// order page's read rule adds it to orderReadable's for administrators only.
func holdRefundReadSQL(viewer, role string) string {
	return "(" + role + "='admin' AND EXISTS(SELECT 1 FROM users staff WHERE staff.id=" + viewer +
		" AND staff.suspended_at IS NULL AND staff.closed_at IS NULL AND staff.retention_restricted_at IS NULL) AND " + holdRefundOrderSQL + ")"
}

// holdRefundRow is what the action and the order page check of one order.
type holdRefundRow struct {
	state, kind, buyerID, vendorID, vendor, outcome string
	banned, due, flagged                            bool
	heldSince, dueAt                                time.Time
	release                                         int64 // the live release, 0 for none
	releaseState                                    string
	releaseBuilt, releaseReRequest                  bool // it has a transaction; it supersedes a settlement
	everBuilt, anyRelease, liveRefund, spent        bool
	destroyed                                       bool // the order's escrow wallet is destroyed (A-672)
	sum                                             int64
}

const holdRefundSQL = `SELECT o.state,COALESCE(o.listing_kind,p.kind),o.buyer_id,p.vendor_id,v.handle,` + disputeOutcomeSQL + `,v.banned_at IS NOT NULL,
	h.since,h.since+make_interval(days => $2::int),h.since+make_interval(days => $2::int)<=now(),
	COALESCE(rel.id,0),COALESCE(rel.state,''),COALESCE(rel.tx_hash IS NOT NULL,false),COALESCE(rel.supersedes IS NOT NULL,false),
	EXISTS(SELECT 1 FROM xmr_settlements s WHERE s.order_id=o.id AND s.kind='release' AND s.tx_hash IS NOT NULL),
	EXISTS(SELECT 1 FROM xmr_settlements s WHERE s.order_id=o.id AND s.kind='release'),
	EXISTS(SELECT 1 FROM xmr_settlements s WHERE s.order_id=o.id AND s.kind='refund' AND s.state<>'superseded'),
	EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id AND pm.spent_outside) OR EXISTS(SELECT 1 FROM xmr_settlements s WHERE s.order_id=o.id AND s.state='external'),
	EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id AND pm.flagged),
	EXISTS(SELECT 1 FROM xmr_escrow_slots sl JOIN xmr_escrow_wallets w ON w.id=sl.wallet_id WHERE sl.id=es.xmr_slot_id AND w.state='destroyed')
	FROM orders o JOIN products p ON p.id=o.product_id JOIN users v ON v.id=p.vendor_id JOIN escrows es ON es.order_id=o.id AND es.coin='XMR'
	CROSS JOIN LATERAL (SELECT GREATEST(v.banned_at,COALESCE((SELECT max(e.created) FROM order_events e WHERE e.order_id=o.id AND e.to_state=o.state AND e.from_state<>e.to_state),o.updated)) AS since) h
	LEFT JOIN LATERAL (SELECT s.id,s.state,s.tx_hash,s.supersedes FROM xmr_settlements s WHERE s.order_id=o.id AND s.kind='release' AND s.state<>'superseded') rel ON true
	WHERE o.id=$1`

// readHoldRefund reads order's holdRefundRow for a hold of days; sql.ErrNoRows when it is not a Monero escrow order.
func readHoldRefund(ctx context.Context, q rowQuerier, order string, days int) (*holdRefundRow, error) {
	r := &holdRefundRow{}
	if err := q.QueryRowContext(ctx, holdRefundSQL, order, days).Scan(&r.state, &r.kind, &r.buyerID, &r.vendorID, &r.vendor, &r.outcome, &r.banned,
		&r.heldSince, &r.dueAt, &r.due, &r.release, &r.releaseState, &r.releaseBuilt, &r.releaseReRequest,
		&r.everBuilt, &r.anyRelease, &r.liveRefund, &r.spent, &r.flagged, &r.destroyed); err != nil {
		return nil, err
	}
	var err error
	r.sum, err = escrowCounted(ctx, q, order)
	return r, err
}

// held reports whether r is a hold this action ends: a banned vendor's finished order whose requested release is held,
// or a completed digital one never claimed. The order page shows the section for those only.
func (r *holdRefundRow) held() bool {
	return r.banned && (r.state == stateCompleted || r.state == stateResolved && r.outcome == "release") && !r.liveRefund && !r.everBuilt &&
		(r.release != 0 && r.releaseState == "requested" && !r.releaseBuilt && !r.releaseReRequest || r.release == 0 && r.unrequested())
}

// unrequested reports whether r, an order with no live release, never had one and is one of the two kinds refunded
// without a release: a completed digital order whose vendor never claimed (owner decision 2026-10-26), or one whose
// dispute was decided (or conceded) for the vendor before any release was requested, as a physical order disputed
// before it was shipped (owner decision 2026-10-26, A-652f fix round 1).
func (r *holdRefundRow) unrequested() bool {
	return !r.anyRelease && (r.outcome == "release" || r.state == stateCompleted && r.kind == "digital")
}

// refusal is why r cannot be refunded now (nil when it can), each message ending "Nothing was changed."
func (r *holdRefundRow) refusal(days int) error {
	switch {
	case r.state != stateCompleted && !(r.state == stateResolved && r.outcome == "release") || r.liveRefund:
		return errHoldRefundState
	case !r.banned:
		return errHoldRefundNotBanned
	case r.everBuilt || r.release != 0 && (r.releaseState != "requested" || r.releaseBuilt || r.releaseReRequest):
		return errHoldRefundOnNetwork
	case r.release == 0 && !r.unrequested():
		return errHoldRefundNoRelease
	case r.spent:
		return errHoldRefundSpent
	case r.destroyed:
		return errHoldRefundDestroyed
	case r.sum <= 0:
		return errHoldRefundNothing
	case !r.due:
		return fail(409, "The release has been held for less than "+strconv.Itoa(days)+" days (since "+utcFrom(r.heldSince)+"): it can be refunded to the buyer from "+utcFrom(r.dueAt)+" if the vendor's account is still closed then. Nothing was changed.")
	}
	return nil
}

// holdRefundDigest is the form's digest of what it refunds: the order, its state, its live release and that release's
// state, the counted sum and when the hold began.
func holdRefundDigest(order string, r *holdRefundRow) string {
	h := sha256.Sum256([]byte("A-652f hold refund\x00" + order + "|" + r.state + "|" + strconv.FormatInt(r.release, 10) + "|" + r.releaseState + "|" +
		strconv.FormatInt(r.sum, 10) + "|" + r.heldSince.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(h[:])
}

// holdRefundParties reads order's buyer and vendor (404 unless it is a Monero escrow order).
func holdRefundParties(ctx context.Context, q rowQuerier, order string) (buyer, vendor string, err error) {
	err = q.QueryRowContext(ctx, `SELECT o.buyer_id,p.vendor_id FROM orders o JOIN products p ON p.id=o.product_id
		JOIN escrows es ON es.order_id=o.id AND es.coin='XMR' WHERE o.id=$1`, order).Scan(&buyer, &vendor)
	if err == sql.ErrNoRows {
		err = errNoOrder
	}
	return
}

// A saved result stays private to current non-party administrators; the order's state is left to the action, so that an
// exact retry of a completed form retrieves its response.
func holdRefundReceiptAuthorize(c *actionCtx) error {
	buyer, vendor, err := holdRefundParties(c.Ctx(), c.Tx, c.Form.Get("order_id"))
	if err != nil {
		return err
	}
	if c.User.ID == buyer || c.User.ID == vendor {
		return errHoldRefundParty
	}
	return nil
}

// holdRefundAction refunds one order's held release to its buyer (see the A-652f comment above).
func holdRefundAction(c *actionCtx) (actionResult, error) {
	order := c.Form.Get("order_id")
	// One line in the audit rows: line breaks and runs of spaces become one space; control and text-direction characters
	// are refused, as in a ban's reason (cleanBanReason).
	reason := strings.Join(strings.Fields(c.Form.Get("reason")), " ")
	if n := utf8.RuneCountInString(reason); n < minHoldRefundReason || n > maxHoldRefundReason || !utf8.ValidString(reason) ||
		strings.IndexFunc(reason, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
		return actionResult{}, fail(400, "Write the reason in "+strconv.Itoa(minHoldRefundReason)+"–"+strconv.Itoa(maxHoldRefundReason)+" characters, with no control or text-direction characters: why the buyer is refunded instead of the vendor being paid. It is written to your audit trail and the vendor's. Nothing was changed.")
	}
	// The party check is made before the password, so a refused party spends none of the confirmation budget.
	buyer, vendor, err := holdRefundParties(c.Ctx(), c.A.db, order)
	if err != nil {
		return actionResult{}, err
	}
	if c.User.ID == buyer || c.User.ID == vendor {
		return actionResult{}, errHoldRefundParty
	}
	digest := c.Form.Get("digest")
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx := c.Ctx(), c.Tx
		if err := waitForWalletLocks(ctx, tx); err != nil {
			return actionResult{}, err
		}
		// The locks in the order the A-652f comment gives: the vendor's account, the order, the live release.
		if _, err := tx.ExecContext(ctx, "SELECT 1 FROM users WHERE id=$1 FOR SHARE", vendor); err != nil {
			return actionResult{}, err
		}
		if _, err := tx.ExecContext(ctx, "SELECT 1 FROM orders WHERE id=$1 FOR UPDATE", order); err != nil {
			return actionResult{}, err
		}
		if _, err := tx.ExecContext(ctx, "SELECT 1 FROM xmr_settlements WHERE order_id=$1 AND kind='release' AND state<>'superseded' FOR UPDATE", order); err != nil {
			return actionResult{}, err
		}
		days, err := banHoldRefundDays(ctx, tx)
		if err != nil {
			return actionResult{}, err
		}
		r, err := readHoldRefund(ctx, tx, order, days)
		if err == sql.ErrNoRows {
			return actionResult{}, errNoOrder
		}
		if err != nil {
			return actionResult{}, err
		}
		switch gate, sweep, err := banRefundsPaused(ctx, tx); {
		case err != nil:
			return actionResult{}, err
		case gate:
			return actionResult{}, errBanOrdersPaused
		case sweep:
			return actionResult{}, errBanOrdersSweep
		}
		if err = r.refusal(days); err != nil {
			return actionResult{}, err
		}
		if subtle.ConstantTimeCompare([]byte(digest), []byte(holdRefundDigest(order, r))) != 1 {
			return actionResult{}, errHoldRefundStale
		}
		// The refund must be of the sum checked here (checkPayoutSeen); escrowSettleTerminal supersedes the release and
		// inserts the one refund to the escrow's refund address.
		if _, err = c.A.transition(withPayoutSeen(withHoldRefund(ctx, order, c.User.ID), order, r.sum), tx, order, r.state, stateCancelled, c.User, holdRefundNote); err != nil {
			return actionResult{}, err
		}
		short, sum := shortID(order), c.A.escrowAmount(&Order{Currency: "XMR"}, r.sum)
		release := "no release was ever requested"
		if r.release != 0 {
			release = "release " + strconv.FormatInt(r.release, 10) + " superseded"
		}
		summary := "order " + short + " (" + stateLabel(r.state) + " -> Cancelled, Monero escrow); " + release + "; refund of " + sum +
			" requested to the buyer's refund address; held since " + utcFrom(r.heldSince) + " (" + strconv.Itoa(days) + "-day period); reason: " + reason
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", vendor, "Held release refunded to the buyer by administrator "+c.User.Handle+": "+summary); err != nil {
			return actionResult{}, err
		}
		// The buyer's row says what the notification says, and no more: no administrator, period or reason (those are
		// on the administrator's and the vendor's rows, as a ban's reason is on the banned account's).
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", buyer,
			"Order "+short+" refunded: the vendor's account stayed closed while the release to the vendor was held, so an administrator cancelled the order and requested a refund of "+sum+" to your refund address given at checkout"); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: adminUserURL(r.vendor), Notice: "hold-refund", Audit: "Refunded the held release of " + r.vendor + "'s " + summary}, nil
	})
}

// HoldRefundView is the order page's section for an administrator who is not a party, on a banned vendor's order whose
// release is held (A-652f): when the hold began, from when it can be refunded, and the form once it can (Eligible).
type HoldRefundView struct {
	Eligible, Unclaimed bool
	Days                int
	HeldSince, DueAt    string
	Sum, Digest, Why    string
	// Read: the administrator reads this order for the hold alone (no dispute, no flagged deposit, no escrow alert).
	Read bool
}

// holdRefundView fills d.HoldRefund on the order page of a held release (holdRefundRow.held), for a non-party
// administrator.
func (a *App) holdRefundView(ctx context.Context, d *PageData) error {
	o := d.Order
	if d.OrderViewer != roleModerator || d.User == nil || d.User.Role != "admin" || d.User.RetentionRestricted || !o.xmrEscrow() ||
		o.State != stateCompleted && o.State != stateResolved {
		return nil
	}
	days, unreadable := banHoldRefundDays(ctx, a.db)
	var he *httpError
	switch {
	case errors.As(unreadable, &he): // the section says so and offers no form
		days = defaultBanHoldRefundDays
	case unreadable != nil:
		return unreadable
	}
	r, err := readHoldRefund(ctx, a.db, o.ID, days)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil || !r.held() {
		return err
	}
	v := &HoldRefundView{Days: days, Unclaimed: r.release == 0, HeldSince: utcFrom(r.heldSince), DueAt: utcFrom(r.dueAt),
		Sum: a.escrowAmount(o, r.sum), Digest: holdRefundDigest(o.ID, r), Read: d.StaffAlert == nil && !r.flagged && r.outcome == ""}
	gate, sweep, err := banRefundsPaused(ctx, a.db)
	switch {
	case err != nil:
		return err
	case unreadable != nil:
		v.Why = strings.TrimSuffix(unreadable.Error(), " Nothing was changed.")
	case gate:
		v.Why = "Refunds are paused: " + restoreWhy + ". The operator clears the restore gate first."
	case sweep:
		v.Why = "Refunds are paused: " + banSweepWhy + "."
	default:
		if refused := r.refusal(days); refused != nil {
			v.Why = strings.TrimSuffix(refused.Error(), " Nothing was changed.")
		}
	}
	if v.Eligible = v.Why == ""; v.Eligible && d.Security == nil {
		if d.Security, _, _, err = a.securityView(ctx, d.User.ID); err != nil {
			return err
		}
	}
	d.HoldRefund = v
	return nil
}
