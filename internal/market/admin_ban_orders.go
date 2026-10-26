package market

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
)

// A-652b (owner decision 2026-10-25, "Refund + hold"; PAY87-2, STAFF87-7): a ban cancels no open order (admin_ban.go),
// so an administrator refunds a banned vendor's unpaid and unshipped Monero escrow orders to the buyers with a separate
// form on the account's page in the user directory, never part of the ban form. Nobody new is paid: each order is
// cancelled through the state machine on the staff edge roleBanRefund, which only this action marks (withBanRefund),
// and escrowSettleTerminal then requests its one refund, of every counted deposit, to the refund address the buyer gave
// at checkout (escrows.refund_address); a deposit after the order closed follows the stray-refund path.
//
// The refund takes the vendor's Monero escrow orders awaiting payment and paid ones (paid is never shipped or
// delivered), oldest first, at most maxBanRefunds per submission, leaving out any whose buyer is the acting
// administrator (another administrator refunds those, as a dispute's party never decides it). Shipped, delivered,
// disputed and finished orders are never touched. The form states a digest of the orders it takes (order id, state,
// counted sum); a different set refuses the whole submission (409, nothing changed). Under the ban's row lock the
// account must still be banned, the restore gate must be clear and a restore's sweep of the escrow wallets complete
// (xmrEscrowSweepKey, PAY89-4: until then the restored records may not yet know every deposit). Each order is then
// locked and checked again; one that moved meanwhile (a ship, a dispute or a payment that committed first) is left out
// and the others go ahead.
// The buyers are notified and the order history records the move; both accounts are audited with the counts.

func init() {
	registerAction("/admin/ban-orders", actionSpec{Roles: []string{"admin"}, OwnTx: true,
		ConfirmedFormNonce: true, ReceiptAuthorize: banOrdersReceiptAuthorize, Run: banOrdersAction})
	registerLoader("admin", banOrdersLoader)
	registerLoader("admin", bannedOpenEscrowLoader)
}

// maxBanRefunds bounds the orders one submission cancels.
const maxBanRefunds = 100

// banRefundNote is the order event and, after "Order X is now Cancelled", the buyer's notification.
const banRefundNote = "Cancelled — the vendor's account is closed, so an administrator cancelled this order; any coins in its escrow go back to the refund address given at checkout"

// banSweepWhy says why refunds wait while a restore's sweep of the escrow wallets has not completed (PAY89-4).
const banSweepWhy = "the market is still checking its escrow wallets after restoring its records from a backup (the restore sweep); it continues by itself once that completes"

var (
	errBanOrdersStale  = fail(409, "These orders changed since the page was shown. Reload the account's page and review them again. Nothing was changed.")
	errBanOrdersPaused = fail(409, "Nothing is refunded while "+restoreWhy+": the operator clears the restore gate first. Nothing was changed.")
	errBanOrdersSweep  = fail(409, "Nothing is refunded while "+banSweepWhy+". Nothing was changed.")
)

// banRefundsPaused reads whether the restore gate is set and whether a restore's sweep has not completed.
func banRefundsPaused(ctx context.Context, q rowQuerier) (gate, sweep bool, err error) {
	err = q.QueryRowContext(ctx, "SELECT "+restoreGateSQL+",EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value='true')", xmrEscrowSweepKey).Scan(&gate, &sweep)
	return gate, sweep, err
}

// BanOrdersView is a banned account's orders section on its page in the user directory (A-652b): its open Monero
// escrow orders as vendor and those whose escrow still holds coins (Rows), and what the refund form takes now.
type BanOrdersView struct {
	Handle  string
	Rows    []BanOrderRow
	More    bool // more rows than shown
	Classes []BanOrderClass
	// Count orders are refunded by the form (Digest), Later more wait for another submission, Mine have the viewer as
	// buyer and are left for another administrator.
	Count, Later, Mine int
	Digest             string
	Paused             string // why refunds are paused ("" when they are not)
}

// BanOrderRow is one listed order. Link: the viewer can read its order page (orderReadable).
type BanOrderRow struct {
	ID, Short, Status, Escrow, Amount string
	Link, Yours                       bool
}

// BanOrderClass is one kind of order the refund takes: how many and the sum counted in their escrows.
type BanOrderClass struct {
	Label string
	Count int
	Sum   string
}

type banRefundOrder struct {
	id, state string
	sum       int64
}

type banQuerier interface {
	rowsQuerier
	rowQuerier
}

// banRefundable reads the orders of vendorID that a refund by adminID takes now (at most maxBanRefunds, oldest first)
// with their counted sums, how many such orders there are in all, and how many have adminID as buyer.
func banRefundable(ctx context.Context, q banQuerier, vendorID, adminID string) (orders []banRefundOrder, total, mine int, err error) {
	const from = ` FROM orders o JOIN products p ON p.id=o.product_id WHERE p.vendor_id=$1 AND o.state IN ('awaiting_payment','paid')
		AND EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id AND es.coin='XMR')`
	rows, err := q.QueryContext(ctx, `SELECT o.id,o.state,count(*) OVER ()`+from+` AND o.buyer_id<>$2 ORDER BY o.created,o.id LIMIT $3`, vendorID, adminID, maxBanRefunds)
	if err != nil {
		return nil, 0, 0, err
	}
	for rows.Next() {
		var o banRefundOrder
		if err = rows.Scan(&o.id, &o.state, &total); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		orders = append(orders, o)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	for i := range orders {
		if orders[i].sum, err = escrowCounted(ctx, q, orders[i].id); err != nil {
			return nil, 0, 0, err
		}
	}
	err = q.QueryRowContext(ctx, `SELECT count(*)`+from+` AND o.buyer_id=$2`, vendorID, adminID).Scan(&mine)
	return orders, total, mine, err
}

// escrowCounted is the sum of order's deposits that its settlement counts (escrowCountedSQL).
func escrowCounted(ctx context.Context, q rowQuerier, order string) (int64, error) {
	var sum, left int64
	err := q.QueryRowContext(ctx, escrowCountedSQL, order).Scan(&sum, &left)
	return sum, err
}

// banRefundDigest is the form's digest of the orders it takes: each one's id, state and counted sum, in order.
func banRefundDigest(orders []banRefundOrder) string {
	h := sha256.New()
	for _, o := range orders {
		h.Write([]byte(o.id + "|" + o.state + "|" + strconv.FormatInt(o.sum, 10) + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func banOrdersReceiptAuthorize(c *actionCtx) error {
	return adminTargetReceiptAuthorize(c, fail(400, "Your own account is not banned, so its orders are not refunded here."), errBanAdmin)
}

// banOrdersAction cancels and refunds the banned account's unpaid and unshipped Monero escrow orders the form states
// (see the A-652b comment above).
func banOrdersAction(c *actionCtx) (actionResult, error) {
	handle := strings.TrimSpace(c.Form.Get("handle"))
	if handle == "" {
		return actionResult{}, fail(400, "Enter an account handle")
	}
	// The handle is looked up before the password is checked, so a typo does not use a confirmation attempt.
	var target string
	err := c.A.db.QueryRowContext(c.Ctx(), "SELECT id FROM users WHERE handle=$1", handle).Scan(&target)
	if err == sql.ErrNoRows {
		return actionResult{}, fail(404, "No account has the handle \""+handle+"\". Check the spelling: handles are case-sensitive.")
	}
	if err != nil {
		return actionResult{}, err
	}
	digest := c.Form.Get("digest")
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx := c.Ctx(), c.Tx
		// Returning stock locks each listing row, which an escrow checkout may hold while it asks the wallet for an address.
		if err := waitForWalletLocks(ctx, tx); err != nil {
			return actionResult{}, err
		}
		// FOR SHARE conflicts with the ban form's lock, so the ban cannot be lifted while this runs.
		var role string
		var banned bool
		if err := tx.QueryRowContext(ctx, "SELECT role,banned_at IS NOT NULL FROM users WHERE id=$1 FOR SHARE", target).Scan(&role, &banned); err != nil {
			return actionResult{}, err
		}
		switch {
		case role == "admin":
			return actionResult{}, errBanAdmin
		case !banned:
			return actionResult{}, fail(409, handle+" is not banned; nothing was changed.")
		}
		switch gate, sweep, err := banRefundsPaused(ctx, tx); {
		case err != nil:
			return actionResult{}, err
		case gate:
			return actionResult{}, errBanOrdersPaused
		case sweep:
			return actionResult{}, errBanOrdersSweep
		}
		orders, total, mine, err := banRefundable(ctx, tx, target, c.User.ID)
		if err != nil {
			return actionResult{}, err
		}
		if len(orders) == 0 || subtle.ConstantTimeCompare([]byte(digest), []byte(banRefundDigest(orders))) != 1 {
			return actionResult{}, errBanOrdersStale
		}
		var awaiting, paid, refunds, changed int
		var refunded int64
		for _, o := range orders {
			// Locked and read again: a move or a deposit that committed after the list was read leaves this order out.
			var state string
			if err = tx.QueryRowContext(ctx, "SELECT state FROM orders WHERE id=$1 FOR UPDATE", o.id).Scan(&state); err != nil {
				return actionResult{}, err
			}
			sum, err := escrowCounted(ctx, tx, o.id)
			if err != nil {
				return actionResult{}, err
			}
			if state != o.state || sum != o.sum {
				changed++
				continue
			}
			// The settlement must be of the sum checked here (checkPayoutSeen); a deposit counted in between refuses it all.
			if _, err = c.A.transition(withPayoutSeen(withBanRefund(ctx, o.id, c.User.ID), o.id, sum), tx, o.id, o.state, stateCancelled, c.User, banRefundNote); err != nil {
				return actionResult{}, err
			}
			if o.state == stateAwaitingPayment {
				awaiting++
			} else {
				paid++
			}
			if sum > 0 {
				refunds++
				refunded += sum
			}
		}
		if awaiting+paid == 0 {
			return actionResult{}, errBanOrdersStale
		}
		n := strconv.Itoa
		summary := "cancelled " + n(awaiting+paid) + " order(s) (" + n(awaiting) + " awaiting payment, " + n(paid) + " paid); requested " + n(refunds) +
			" refund(s) of " + c.A.escrowAmount(&Order{Currency: "XMR"}, refunded) + " to the buyers' refund addresses"
		if changed > 0 {
			summary += "; " + n(changed) + " changed meanwhile and were left out"
		}
		if mine > 0 {
			summary += "; " + n(mine) + " order(s) with the administrator as buyer left for another administrator"
		}
		if later := total - len(orders); later > 0 {
			summary += "; " + n(later) + " more wait for another submission"
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Open orders refunded by administrator "+c.User.Handle+": "+summary); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: adminUserURL(handle), Notice: "ban-orders", Audit: "Refunded open orders of " + handle + ": " + summary}, nil
	})
}

// banListSQL lists a vendor's Monero escrow orders that are open or whose escrow may still hold coins (no release or
// refund sent), oldest first: id, state, amount, whether $2 (the viewer, role admin) can read the order page and
// whether it is the buyer, and the live release or refund's kind and state ("" for none).
var banListSQL = `SELECT o.id,o.state,o.amount,` + orderReadable("$2", "'admin'") + `,o.buyer_id=$2,
	coalesce((SELECT s.kind||' '||s.state FROM xmr_settlements s WHERE s.order_id=o.id AND s.kind IN ('release','refund') AND s.state<>'superseded'),'')
	FROM orders o JOIN products p ON p.id=o.product_id
	WHERE p.vendor_id=$1 AND EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id AND es.coin='XMR')
	AND (o.state IN ('awaiting_payment','paid','shipped','delivered','disputed') OR o.state IN ('completed','resolved','cancelled')
		AND NOT EXISTS(SELECT 1 FROM xmr_settlements s WHERE s.order_id=o.id AND s.kind IN ('release','refund') AND s.state IN ('relayed','confirmed','external')))
	ORDER BY o.created,o.id LIMIT $3`

// banOrdersLoader fills the orders section of a banned account's page (BanOrdersView).
func banOrdersLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.AdminSection != "users" || d.AdminUserHandle == "" || d.User == nil || d.User.Role != "admin" {
		return nil
	}
	var id string
	var banned bool
	err := a.db.QueryRowContext(ctx, "SELECT id,banned_at IS NOT NULL FROM users WHERE handle=$1", d.AdminUserHandle).Scan(&id, &banned)
	if err == sql.ErrNoRows || err == nil && !banned { // adminUserLoader answers an unknown handle
		return nil
	}
	if err != nil {
		return err
	}
	v := &BanOrdersView{Handle: d.AdminUserHandle}
	xmr := func(n int64) string { return a.escrowAmount(&Order{Currency: "XMR"}, n) }
	type listed struct {
		row               BanOrderRow
		state, settlement string
	}
	rows, err := a.db.QueryContext(ctx, banListSQL, id, d.User.ID, maxBanRefunds+1)
	if err != nil {
		return err
	}
	var list []listed
	for rows.Next() {
		var l listed
		var amt int64
		if err = rows.Scan(&l.row.ID, &l.state, &amt, &l.row.Link, &l.row.Yours, &l.settlement); err != nil {
			rows.Close()
			return err
		}
		l.row.Short, l.row.Status, l.row.Amount = shortID(l.row.ID), stateLabel(l.state), xmr(amt)
		list = append(list, l)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(list) > maxBanRefunds {
		list, v.More = list[:maxBanRefunds], true
	}
	for _, l := range list {
		sum, err := escrowCounted(ctx, a.db, l.row.ID)
		if err != nil {
			return err
		}
		finished := l.state == stateCompleted || l.state == stateResolved || l.state == stateCancelled
		if finished && sum == 0 {
			continue // nothing held
		}
		switch {
		case sum == 0 && l.settlement == "":
			l.row.Escrow = "Nothing received"
		case l.settlement == "":
			l.row.Escrow = xmr(sum) + " counted; no settlement yet"
		default:
			l.row.Escrow = xmr(sum) + " counted; " + l.settlement
		}
		v.Rows = append(v.Rows, l.row)
	}
	orders, total, mine, err := banRefundable(ctx, a.db, id, d.User.ID)
	if err != nil {
		return err
	}
	v.Count, v.Later, v.Mine, v.Digest = len(orders), total-len(orders), mine, banRefundDigest(orders)
	classes := []BanOrderClass{{Label: "Awaiting payment, nothing received"}, {Label: "Awaiting payment, deposit received"}, {Label: "Paid, not shipped"}}
	sums := make([]int64, len(classes))
	for _, o := range orders {
		i := 2
		if o.state == stateAwaitingPayment {
			i = 0
			if o.sum > 0 {
				i = 1
			}
		}
		classes[i].Count++
		sums[i] += o.sum
	}
	for i := range classes {
		classes[i].Sum = xmr(sums[i])
	}
	v.Classes = classes
	switch gate, sweep, err := banRefundsPaused(ctx, a.db); {
	case err != nil:
		return err
	case gate:
		v.Paused = "Refunds are paused: " + restoreWhy + ". The operator clears the restore gate first."
	case sweep:
		v.Paused = "Refunds are paused: " + banSweepWhy + "."
	}
	d.BanOrders = v
	return nil
}

// bannedOpenEscrowLoader counts, for the overview's Users alert, the banned accounts that are the vendor of an open
// escrow order (STAFF87-7).
func bannedOpenEscrowLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.AdminSection != "" || d.User == nil || d.User.Role != "admin" {
		return nil
	}
	return a.db.QueryRowContext(ctx, `SELECT count(DISTINCT p.vendor_id) FROM orders o JOIN products p ON p.id=o.product_id JOIN users u ON u.id=p.vendor_id
		WHERE u.banned_at IS NOT NULL AND o.state IN ('awaiting_payment','paid','shipped','delivered','disputed')
		AND EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id)`).Scan(&d.BannedOpenEscrow)
}
