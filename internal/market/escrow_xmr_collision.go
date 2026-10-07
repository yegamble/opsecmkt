package market

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Refused Monero escrow deposits (A-480). The escrow watcher records each deposit the database refused as another
// order's escrow account (payments_xmr_escrow_idx) once per order and transaction in xmr_escrow_collisions
// (xmrFlagCollision, migration 090). While a record is open the order has an escrow staff alert (escrowStaffAlert):
// moderators and administrators who are not parties read the order and the moderation desk lists it. One of them
// records what was done about the deposit outside the market (refunded by hand, verified as the other order's deposit,
// …) on the order page, as the payment-review disposition is recorded (paymentReviewAction): moderators and
// administrators only, never a party, confirmed with the password (and authenticator code when enrolled) through
// confirmedTx, one audit row, and 409 for a second or concurrent disposition. The note is for staff only: no order
// event and no notification is written, so the parties see only the order note they saw before.

func init() {
	registerAction("/moderator/xmr-collision", actionSpec{Roles: []string{"moderator", "admin"}, OwnTx: true, Run: xmrCollisionAction})
}

var errCollisionParty = fail(403, "You are a party to this order, so another moderator or administrator records what was done about its refused deposit. Nothing was changed.")

var errCollisionNotFound = fail(404, "Refused deposit not found")

var errCollisionRecorded = fail(409, "What was done about this refused deposit was already recorded; see the order page. Nothing was changed.")

func xmrCollisionAction(c *actionCtx) (actionResult, error) {
	id, err := strconv.ParseInt(c.Form.Get("collision_id"), 10, 64)
	if err != nil || id <= 0 {
		return actionResult{}, errCollisionNotFound
	}
	note := strings.TrimSpace(c.Form.Get("note"))
	if n := utf8.RuneCountInString(note); n < 10 || n > 500 {
		return actionResult{}, fail(400, "Say what was done in 10–500 characters: for example, refunded to the buyer by hand in transaction …, or verified as the other order's deposit. Only market staff read it.")
	}
	// Read before the password check (a refused party or unknown record spends none of the confirmation budget);
	// recordXMRCollision checks it again under the row locks.
	var buyer, vendor string
	err = c.A.db.QueryRowContext(c.Ctx(), `SELECT o.buyer_id,pr.vendor_id FROM xmr_escrow_collisions x JOIN orders o ON o.id=x.order_id
		JOIN products pr ON pr.id=o.product_id WHERE x.id=$1`, id).Scan(&buyer, &vendor)
	if err == sql.ErrNoRows {
		return actionResult{}, errCollisionNotFound
	}
	if err != nil {
		return actionResult{}, err
	}
	if c.User.ID == buyer || c.User.ID == vendor {
		return actionResult{}, errCollisionParty
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		return recordXMRCollision(c, id, note)
	})
}

func recordXMRCollision(c *actionCtx, id int64, note string) (actionResult, error) {
	ctx, tx := c.Ctx(), c.Tx
	// The order row first, as the watcher's xmrFlagCollision takes it.
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=(SELECT order_id FROM xmr_escrow_collisions WHERE id=$1) FOR UPDATE OF o", id))
	if err == sql.ErrNoRows {
		return actionResult{}, errCollisionNotFound
	}
	if err != nil {
		return actionResult{}, err
	}
	if c.User.ID == o.BuyerID || c.User.ID == o.VendorID {
		return actionResult{}, errCollisionParty
	}
	var txid string
	var disposed bool
	if err = tx.QueryRowContext(ctx, "SELECT txid,disposed_at IS NOT NULL FROM xmr_escrow_collisions WHERE id=$1 AND order_id=$2 FOR UPDATE", id, o.ID).Scan(&txid, &disposed); err != nil {
		return actionResult{}, err
	}
	if disposed {
		return actionResult{}, errCollisionRecorded
	}
	// Compare-and-set on the open record: under the row locks it always matches; should a concurrent disposition ever
	// slip past them, this one is refused and nothing else is written (as recordPaymentReview, A-252).
	res, err := tx.ExecContext(ctx, "UPDATE xmr_escrow_collisions SET disposition_note=$2,disposed_at=now(),disposed_by=$3 WHERE id=$1 AND disposed_at IS NULL", id, note, c.User.ID)
	if err != nil {
		return actionResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return actionResult{}, errCollisionRecorded
	}
	audit := fmt.Sprintf("Recorded what was done about refused Monero escrow deposit %s on order %s", txid, shortID(o.ID))
	return actionResult{Redirect: "/moderator", Notice: "xmr-collision", Audit: audit}, nil
}

// loadXMRCollisions fills the order page's refused Monero deposits for staff who are not parties (the caller checks),
// oldest first, with the disposition form for an open one (and whether it asks for an authenticator code).
func loadXMRCollisions(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	rows, err := a.db.QueryContext(ctx, `SELECT x.id,x.txid,x.txid_truncated,x.wallet_name,x.account,to_char(x.noted,'YYYY-MM-DD HH24:MI "UTC"'),
		COALESCE(to_char(x.disposed_at,'YYYY-MM-DD HH24:MI "UTC"'),''),COALESCE(u.handle,''),x.disposition_note
		FROM xmr_escrow_collisions x LEFT JOIN users u ON u.id=x.disposed_by WHERE x.order_id=$1 ORDER BY x.id`, d.Order.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	form := false
	for rows.Next() {
		var v XMRCollisionView
		if err = rows.Scan(&v.ID, &v.TxID, &v.Truncated, &v.Wallet, &v.Account, &v.Noted, &v.Disposed, &v.By, &v.Note); err != nil {
			return err
		}
		v.Form = v.Disposed == ""
		form = form || v.Form
		d.XMRCollisions = append(d.XMRCollisions, v)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if form {
		return adminConfirmLoader(ctx, a, r, d)
	}
	return nil
}
