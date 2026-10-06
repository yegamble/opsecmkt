package market

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// Payment-review dispositions (A-31). A moderator or administrator who is not party to the order records one
// outcome for a deposit the payment watcher flagged (payments.flagged) and that is still open on the moderation
// desk: no action (with a reason), refunded to the buyer by hand (the wallet transaction ID and the amount; the
// market sends nothing and writes no payouts row, so the one-settlement rule is untouched) or escalated to an
// administrator (moderators only; an administrator then records one of the other two). Each needs the staff
// member's password (and authenticator code when enrolled) through confirmedTx, which also re-reads their role
// and suspension. It writes one order event (the parties read it; the refund's transaction ID is hidden from the
// vendor by hidePayoutTxids), one audit row and a notification to the buyer (with the refund's transaction ID,
// A-231); an escalation also notifies every administrator who is not a party.

func init() {
	registerAction("/moderator/payment-review", actionSpec{Roles: []string{"moderator", "admin"}, OwnTx: true, Run: paymentReviewAction})
	registerLoader("moderator", adminConfirmLoader)
}

// paymentFlagOpenRule is the one rule (QA462C) deciding whether a payment flag is open, as SQL over a payments row
// whose alias is "@" (paymentFlagOpen substitutes it). The moderation desk lists a flag as open and offers its form
// (loadPaymentReviews), the review handler records a disposition only on an open flag (recordPaymentReview), and a
// late-deposit notice episode is held only by an open flag of a deposit neither credited, nor below the floor, nor
// locked, nor spent outside the market (flagLateDeposits), so a flag that holds it is always one staff can record. A
// flag is open until staff record its disposition (A-31: no action or refunded by hand; one escalated to an
// administrator stays open) while its deposit is a locked transfer, of a payout below the minimum automatic payout
// (A-99), a credited deposit whose regression was announced and has not confirmed again (regress_notice), an escrow
// deposit spent outside the market (P1-d1), an escrow deposit left out of its settlement, credited or not (it no
// longer counts: a stray refund returns it, QA462D-2), or a deposit not credited that does not count for the order: a
// pooled one (after settlement or after the close), or an escrow one recorded after the close. An escrow deposit that
// counts for the order is not open, credited or not (QA462B-1): it is settled by the order's release or refund, never
// by staff.
const paymentFlagOpenRule = `@.flagged AND @.disposition IN ('','escalated') AND (@.locked OR @.below_floor OR @.regress_notice OR @.spent_outside
	OR @.escrow_left_out OR NOT @.credited AND (@.after_close OR NOT EXISTS (SELECT 1 FROM escrows es WHERE es.order_id=@.order_id)))`

// paymentFlagOpen is paymentFlagOpenRule over the payments row aliased alias.
func paymentFlagOpen(alias string) string { return strings.ReplaceAll(paymentFlagOpenRule, "@", alias) }

// reviewOutcomes names each disposition in order events, notifications and audit rows.
var reviewOutcomes = map[string]string{"no_action": "no action", "refunded": "refunded to the buyer by hand", "escalated": "escalated to an administrator"}

func paymentReviewAction(c *actionCtx) (actionResult, error) {
	outcome := c.Form.Get("outcome")
	if _, ok := reviewOutcomes[outcome]; !ok {
		return actionResult{}, fail(400, "Choose no action, refunded by hand or escalate to an administrator")
	}
	if outcome == "escalated" && c.User.Role != "moderator" {
		return actionResult{}, fail(403, "Only a moderator escalates a payment review to an administrator; an administrator records no action or a refund by hand. Nothing was changed.")
	}
	id, err := strconv.ParseInt(c.Form.Get("payment_id"), 10, 64)
	if err != nil || id <= 0 {
		return actionResult{}, fail(404, "Flagged payment not found")
	}
	note := strings.TrimSpace(c.Form.Get("note"))
	if len(note) > 1000 || outcome == "no_action" && len(note) < 10 {
		return actionResult{}, fail(400, "Give the reason in 10–1000 characters (a note on a refund or an escalation is optional, up to 1000). The buyer and the vendor read it.")
	}
	var txid string
	if outcome == "refunded" {
		if txid = strings.ToLower(strings.TrimSpace(c.Form.Get("txid"))); !txidPattern.MatchString(txid) {
			return actionResult{}, fail(400, "Enter the 64-character hexadecimal transaction ID of the refund, as the wallet shows it")
		}
	}
	// Read before the password check (a refused party or unknown payment spends none of the confirmation budget);
	// recordPaymentReview checks all of it again under the row locks.
	var buyer, vendor, cur string
	var deposit int64
	err = c.A.db.QueryRowContext(c.Ctx(), `SELECT o.buyer_id,pr.vendor_id,pm.currency,pm.amount FROM payments pm JOIN orders o ON o.id=pm.order_id
		JOIN products pr ON pr.id=o.product_id WHERE pm.id=$1 AND pm.flagged`, id).Scan(&buyer, &vendor, &cur, &deposit)
	if err == sql.ErrNoRows {
		return actionResult{}, fail(404, "Flagged payment not found")
	}
	if err != nil {
		return actionResult{}, err
	}
	if c.User.ID == buyer || c.User.ID == vendor {
		return actionResult{}, errReviewParty
	}
	var refunded int64
	if outcome == "refunded" {
		dec := currencyDecimals(cur)
		if refunded, err = parseAmount(strings.TrimSpace(c.Form.Get("amount")), dec); err != nil {
			return actionResult{}, fail(400, "Enter the amount refunded in "+cur+", as sent (for example 0.0005)")
		}
		if refunded > deposit {
			return actionResult{}, fail(400, "The amount refunded by hand cannot be more than the deposit ("+amount(deposit, dec)+" "+cur+")")
		}
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		return recordPaymentReview(c, id, outcome, note, txid, refunded)
	})
}

var errReviewParty = fail(403, "You are a party to this order, so another moderator or administrator records its payment review. Nothing was changed.")

func recordPaymentReview(c *actionCtx, id int64, outcome, note, refundTxid string, refunded int64) (actionResult, error) {
	ctx, tx, a := c.Ctx(), c.Tx, c.A
	// The order row first, as the watcher's settleOrder takes it: a late deposit it flags meanwhile sees this
	// disposition, so its notice episode (flagLateDeposits) is decided consistently.
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=(SELECT order_id FROM payments WHERE id=$1 AND flagged) FOR UPDATE OF o", id))
	if err == sql.ErrNoRows {
		return actionResult{}, fail(404, "Flagged payment not found")
	}
	if err != nil {
		return actionResult{}, err
	}
	if c.User.ID == o.BuyerID || c.User.ID == o.VendorID {
		return actionResult{}, errReviewParty
	}
	var txid, disposition string
	var idx, deposit int64
	var credited, open bool
	if err = tx.QueryRowContext(ctx, "SELECT p.txid,p.idx,p.amount,p.credited,p.disposition,"+paymentFlagOpen("p")+" FROM payments p WHERE p.id=$1 AND p.order_id=$2 FOR UPDATE OF p", id, o.ID).
		Scan(&txid, &idx, &deposit, &credited, &disposition, &open); err != nil {
		return actionResult{}, err
	}
	switch {
	case disposition == "no_action" || disposition == "refunded":
		return actionResult{}, fail(409, "This payment review was already recorded ("+reviewOutcomes[disposition]+"); see the order history. Nothing was changed.")
	case disposition == "escalated" && c.User.Role != "admin":
		return actionResult{}, fail(409, "This payment review was escalated to an administrator, who records the outcome. Nothing was changed.")
	case !open && !credited:
		// The open-flag rule (paymentFlagOpenRule, as the desk reads it): an escrow deposit that counts for the order is
		// never open, credited or not (QA462B-1).
		return actionResult{}, fail(409, "This flag is not open: its escrow deposit counts for the order, so there is nothing to review. Nothing was changed.")
	case !open:
		// The open-flag rule: a credited deposit's flag is not open once a regression confirmed again, nor when it was
		// never a regression (the flag of a release too few, A-461, only lets staff open the order).
		return actionResult{}, fail(409, "This flag is not open: its deposit is credited toward the order, which settles it, and no announced regression of it is still open, so there is nothing to review. Nothing was changed.")
	case outcome == "refunded" && o.Escrow:
		// A-330: an escrow deposit is in the order's escrow, never in a wallet staff could refund it from; a credited one
		// left out of its settlement no longer counts for the order (QA462D-2), so this, not the payout, is the reason.
		coin := "Bitcoin"
		if o.Currency == "XMR" {
			coin = "Monero"
		}
		return actionResult{}, fail(409, "This deposit is in the order's "+coin+" escrow, not in a market wallet, so it is never refunded by hand: a stray refund from the escrow returns it to the buyer's refund address. Record no action, or escalate. Nothing was changed.")
	case outcome == "refunded" && credited:
		return actionResult{}, fail(409, "This deposit is counted toward the order's payout, so it is settled through that payout and never refunded by hand (that could pay it twice). Nothing was changed.")
	}
	var admins []string
	if outcome == "escalated" {
		if admins, err = collectStrings(ctx, tx, "SELECT id FROM users WHERE role='admin' AND suspended_at IS NULL AND id<>$1 AND id<>$2", o.BuyerID, o.VendorID); err != nil {
			return actionResult{}, err
		}
		if len(admins) == 0 {
			return actionResult{}, fail(409, "No administrator who is not a party to this order can take this review; record no action or a refund by hand instead. Nothing was changed.")
		}
	}
	// Compare-and-set on the disposition read above: under the row locks it always matches; should a concurrent
	// disposition ever slip past them, this one is refused and nothing else is written (A-252).
	res, err := tx.ExecContext(ctx, "UPDATE payments SET disposition=$2,disposition_note=$3,disposition_txid=$4,disposition_amount=$5,disposed_at=now() WHERE id=$1 AND disposition=$6",
		id, outcome, note, refundTxid, refunded, disposition)
	if err != nil {
		return actionResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Name the outcome that won, as the check above does (A-277): this statement sees the committed row.
		if err = tx.QueryRowContext(ctx, "SELECT disposition FROM payments WHERE id=$1", id).Scan(&disposition); err != nil {
			return actionResult{}, err
		}
		recorded := ""
		if r, ok := reviewOutcomes[disposition]; ok {
			recorded = " (" + r + ")"
		}
		return actionResult{}, fail(409, "This payment review was already recorded"+recorded+"; see the order history. Nothing was changed.")
	}
	dec := currencyDecimals(o.Currency)
	short := shortID(o.ID)
	dep := fmt.Sprintf("%s deposit %s:%d (%s %s)", a.netTag(), truncate(txid, 20), idx, amount(deposit, dec), o.Currency)
	event := "Payment review of " + dep + ": " + reviewOutcomes[outcome] + "."
	body := "Order " + short + ": market staff reviewed the flagged " + dep + " and took no action."
	audit := fmt.Sprintf("Recorded payment review of deposit %s:%d on order %s: %s", txid, idx, short, reviewOutcomes[outcome])
	switch outcome {
	case "no_action":
		event += " Reason: " + note
		body += " Reason: " + note
	case "refunded":
		// The refund's transaction ID: the order page hides it from the vendor (hidePayoutTxids, A-214); the buyer's
		// notification carries it (A-231).
		sent := amount(refunded, dec) + " " + o.Currency
		event = "Payment review of " + dep + ": refunded to the buyer by hand, outside the market's automatic payouts: " + sent + ", transaction " + refundTxid + "."
		body = "Order " + short + ": market staff refunded the flagged " + dep + " to you by hand, outside the market's automatic payouts: " + sent + ". Transaction ID: " + refundTxid
		audit += " " + sent + " with transaction " + refundTxid
	case "escalated":
		body = "Order " + short + ": market staff passed the review of the flagged " + dep + " to an administrator."
	}
	if note != "" && outcome != "no_action" {
		event += " Note: " + note
		body += " Note: " + note
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", o.ID, o.State, c.User.ID, event); err != nil {
		return actionResult{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), o.BuyerID, body); err != nil {
		return actionResult{}, err
	}
	for _, uid := range admins {
		msg := "Payment review escalated for order " + short + ": " + dep + ". Record the outcome under Payment review on the moderation desk."
		if note != "" {
			msg += " Moderator's note: " + note
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, msg); err != nil {
			return actionResult{}, err
		}
	}
	return actionResult{Redirect: "/moderator", Notice: "payment-review", Audit: audit}, nil
}
