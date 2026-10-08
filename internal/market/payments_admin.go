package market

import (
	"context"
	"database/sql"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Administrator recovery actions for payouts the watcher will not touch on its own: release a held payout,
// requeue a failed (or stuck sending) payout, or record one as sent after reconciling it with the wallet.
// Each needs the administrator's password (and authenticator code when enrolled), is audited, and changes
// the payout with a compare-and-set on its state, so repeated or concurrent submissions act once. Requeueing
// a payout whose last send may have been broadcast (an ambiguous failure or a stuck send), or releasing one
// held by a restore, also needs an explicit confirmation that the wallet shows no such transaction. Releasing
// one held for an account suspension needs an explicit confirmation that its payout address was checked.
// Moving a held or definitely rejected payout to the recipient's current saved address (A-121) needs an explicit
// confirmation that the administrator checked that address with the account owner; nothing is sent by the move.
// A blocked payout (its recipient saved no payout address, so the watcher never sends it) can be recorded as sent by
// hand with the wallet's transaction ID (A-445, ARCH-12: the pooled wallet's drain gate lists it until then); the
// market does not check any recorded transaction ID against the chain. A pending payout, which the watcher may claim
// at any moment, and a send under way are never marked sent: the state is read under the row lock and compared again,
// and the refusal says that the market queued, is sending or sent it itself, so a payment made by hand would be a
// second one (A-486, R2). To pay a blocked payout by hand safely, an administrator first holds it (op=hold, a required
// staff note; A-486, the blocked half of A-34): a held payout keeps its hold when the recipient saves an address
// (savePayoutAddress), so the watcher cannot send it while the administrator pays it and records it. Releasing such a
// hold needs an explicit confirmation that it was not paid by hand.

func init() {
	registerAction("/admin/payout", actionSpec{Roles: []string{"admin"}, OwnTx: true, Run: adminPayoutAction})
	registerLoader("admin", adminConfirmLoader)
}

// adminConfirmLoader tells the payout forms whether to ask for an authenticator code.
func adminConfirmLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.User == nil || d.Security != nil {
		return nil
	}
	return accountSecurityLoader(ctx, a, r, d)
}

// stuckSending matches a payout claimed for sending that never recorded an outcome (crash or database error
// after the wallet call). A live send finishes within payoutSendTimeout, far below this age.
const stuckSending = "(state='sending' AND updated < now()-interval '5 minutes')"

// restoredHoldPrefix starts the error scripts/restore.sh (and the manual SQL in UPGRADING.md) writes on every
// payout it holds, and on every failed payout it marks as possibly sent. Such a payout may have been sent (or
// requeued and sent) after the backup was taken, so releasing or requeueing it needs the same explicit
// "wallet shows no broadcast" confirmation as requeueing an ambiguous send.
const restoredHoldPrefix = "Restored from backup:"

// heldForSuspension reports a payout held for an account suspension (suspendedHoldPrefix), including one a
// restore from backup held again: the restore keeps the suspension text after its own prefix (A-112), so the
// payout address check is not lost.
func heldForSuspension(state, payoutErr string) bool {
	return state == "held" && (strings.HasPrefix(payoutErr, suspendedHoldPrefix) ||
		strings.HasPrefix(payoutErr, restoredHoldPrefix) && strings.Contains(payoutErr, " "+suspendedHoldPrefix))
}

// handHoldPrefix starts the error of a blocked payout an administrator held to pay it by hand (op=hold, A-486);
// the staff note follows it.
const handHoldPrefix = "Held to be paid by hand:"

// heldForHand reports a payout held to be paid by hand (handHoldPrefix), including one an account suspension or a
// restore from backup held again: both keep the hand-hold text after their own (A-488), so releasing it still needs
// the "nobody paid it by hand" confirmation besides theirs. The watcher never lifts this hold, and saving a payout
// address keeps it.
func heldForHand(state, payoutErr string) bool {
	return state == "held" && (strings.HasPrefix(payoutErr, handHoldPrefix) || strings.Contains(payoutErr, " "+handHoldPrefix))
}

// payoutRefusalLabel names a payout's state in a refusal: payoutStateLabel, but a payout held to be paid by hand says
// so rather than the watcher's hold (A-488).
func payoutRefusalLabel(state, payoutErr string) string {
	if heldForHand(state, payoutErr) {
		return "Held to be paid by hand"
	}
	return payoutStateLabel(state)
}

// repointable reports a payout an administrator may move to its recipient's current saved address (A-121): held
// (but not by a restore from backup, after which it may have been sent) or failed with a definite wallet rejection.
// A payout being sent, or one whose send may have been broadcast, is never moved: that could pay twice. Saving an
// address never moves a payout that already has one, so a stolen password cannot re-point it.
func repointable(state, payoutErr string, ambiguous bool) bool {
	if strings.HasPrefix(payoutErr, restoredHoldPrefix) {
		return false
	}
	return state == "held" || state == "failed" && !ambiguous
}

var txidPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func adminPayoutAction(c *actionCtx) (actionResult, error) {
	op := c.Form.Get("op")
	if op != "release" && op != "requeue" && op != "sent" && op != "repoint" && op != "hold" {
		return actionResult{}, fail(400, "Choose release, requeue, mark sent, hold to pay by hand or use the account's current address")
	}
	id, err := strconv.ParseInt(c.Form.Get("payout_id"), 10, 64)
	if err != nil || id <= 0 {
		return actionResult{}, fail(400, "Unknown payout")
	}
	txid := strings.ToLower(strings.TrimSpace(c.Form.Get("txid")))
	if op == "sent" && !txidPattern.MatchString(txid) {
		return actionResult{}, fail(400, "Enter the 64-character hexadecimal transaction ID found in the wallet")
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(c.Form.Get("note"))); op == "hold" && (n < 10 || n > 500) {
		return actionResult{}, fail(400, "Give the reason for holding this payout in 10–500 characters (staff only: the administrators' payouts list and the audit log show it). Nothing was changed.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) { return changePayout(c, id, op, txid) })
}

func changePayout(c *actionCtx, id int64, op, txid string) (actionResult, error) {
	ctx, tx := c.Ctx(), c.Tx
	var orderID, recipient, cur, state, orderState, payoutErr, address string
	var amt int64
	var ambiguous, stuck bool
	// stuck repeats stuckSending with the payout alias (orders also has state and updated).
	err := tx.QueryRowContext(ctx, `SELECT p.order_id,p.user_id,p.currency,p.amount,p.state,o.state,p.send_ambiguous,
		(p.state='sending' AND p.updated < now()-interval '5 minutes'),p.error,p.address FROM payouts p JOIN orders o ON o.id=p.order_id
		WHERE p.id=$1 FOR UPDATE OF p`, id).Scan(&orderID, &recipient, &cur, &amt, &state, &orderState, &ambiguous, &stuck, &payoutErr, &address)
	if err == sql.ErrNoRows {
		return actionResult{}, fail(404, "Payout not found")
	}
	if err != nil {
		return actionResult{}, err
	}
	label := amount(amt, currencyDecimals(cur)) + " " + cur
	short := orderID[:min(8, len(orderID))]
	var res sql.Result
	var note, audit string
	// Every payout is sent with the network fee taken from its amount (A-232): a release, requeue or move states
	// the amount with the fee sentence the other payout notes carry (A-250, A-270). Mark sent records a payout
	// already paid and says nothing about the fee.
	const feeTaken = " The network fee is taken from it."
	switch op {
	case "release":
		// A payout held by a restore may have been sent after the backup was taken: releasing it without checking
		// the wallet could pay twice.
		restored := state == "held" && strings.HasPrefix(payoutErr, restoredHoldPrefix)
		if restored && c.Form.Get("not_broadcast") != "confirmed" {
			return actionResult{}, fail(400, "This payout was held by a restore from backup and may already have been sent. Check the wallet, then confirm that no transaction was broadcast to release it, or mark it sent with the wallet's transaction ID.")
		}
		// A payout held for an account suspension may be going to an address someone else saved with the
		// account's password.
		suspendedHeld := heldForSuspension(state, payoutErr)
		if suspendedHeld && address == "" {
			return actionResult{}, fail(409, "This payout was held when the recipient's account was suspended and has no payout address to check. It can be released after the recipient saves one. Nothing was changed.")
		}
		if suspendedHeld && c.Form.Get("address_checked") != "confirmed" {
			return actionResult{}, fail(409, "This payout was held when the recipient's account was suspended, and its payout address may have been changed by someone else. Check the payout address with the account owner, then confirm that you checked it to release the payout. Nothing was changed.")
		}
		// A payout held to be paid by hand (op=hold) may already have been paid: releasing it would let the watcher
		// send it once the recipient saves an address.
		handHeld := heldForHand(state, payoutErr)
		if handHeld && c.Form.Get("not_paid") != "confirmed" {
			return actionResult{}, fail(409, "This payout was held so that an administrator could pay it by hand. Releasing it lets the market send it once the recipient has a payout address, so release it only if nobody paid it by hand (check the pooled wallet), and confirm that. If it was paid, record it as sent with the wallet's transaction ID instead. Nothing was changed.")
		}
		// A refund or return larger than the order price is held as a likely mistake (largeHold).
		large := heldLarge(state, payoutErr)
		if large && c.Form.Get("large_reviewed") != "confirmed" {
			return actionResult{}, fail(409, "This payout is larger than the order price and was held as a likely mistake (refunds and returns up to the price are sent automatically, so this check does not stop swaps up to a listing's price). Review the order and its deposits, then confirm that you reviewed it to release the payout. Nothing was changed.")
		}
		// A payout the watcher itself holds (credited deposit conflicted or re-confirming) stays held while
		// that is still true: sendPayouts would refuse it anyway, and the watcher releases it once it re-confirms.
		var unsettled bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payments WHERE order_id=$1 AND credited AND confirmations<$2)", orderID, c.A.confirmationThreshold(cur)).Scan(&unsettled); err != nil {
			return actionResult{}, err
		}
		if unsettled && state == "held" {
			if payoutErr == heldReason {
				return actionResult{}, fail(409, "A credited deposit for this order is still conflicted or below the confirmation threshold, so this payout stays held; the payment watcher releases it by itself once the deposit confirms again. Nothing was changed.")
			}
			// A hold only an administrator lifts (restore, suspension): the watcher keeps its ledger current.
			return actionResult{}, fail(409, "A credited deposit for this order is still conflicted or below the confirmation threshold, so this payout stays held. Release it after the deposit confirms again (the order page shows its confirmations). Nothing was changed.")
		}
		res, err = tx.ExecContext(ctx, `UPDATE payouts SET state=CASE WHEN address='' THEN 'blocked' ELSE 'pending' END,error='',updated=now()
			WHERE id=$1 AND state='held'`, id)
		note = "Administrator released the held " + c.A.netTag() + " payout of " + label + "; it is queued for a single send."
		audit = "Released held payout " + strconv.FormatInt(id, 10) + " (" + label + ") for order " + short
		if restored {
			note = "Administrator confirmed the wallet shows no broadcast transaction for the " + c.A.netTag() + " payout of " + label + " (held after being restored from backup) and released it; it is queued for a single send."
			audit += "; restored from backup, administrator confirmed the wallet shows no broadcast transaction"
		}
		if suspendedHeld {
			note = "Administrator checked the payout address and released the held " + c.A.netTag() + " payout of " + label + "; it is queued for a single send."
			if restored {
				note = "Administrator confirmed the wallet shows no broadcast transaction for the " + c.A.netTag() + " payout of " + label + " (held after being restored from backup) and checked the payout address (held for an account suspension), and released it; it is queued for a single send."
			}
			audit += "; held for an account suspension, administrator checked the payout address " + address
		}
		if handHeld {
			note = "Administrator released the " + c.A.netTag() + " payout of " + label + " that was held to be paid by hand, after confirming that it was not paid by hand; it is queued for a single send once the recipient has a payout address."
			audit += "; held to be paid by hand, administrator confirmed it was not paid by hand"
		}
		note += feeTaken
		if large {
			note += " It is larger than the order price; an administrator reviewed it before releasing it."
			audit += "; larger than the order price, administrator reviewed it"
		}
	case "requeue":
		// A send with no definite answer may be on the network: sending again without checking could pay twice.
		// So may a failure restored from backup, requeued and sent after the backup was taken (the restore also
		// marks it ambiguous). A definite wallet rejection (nothing broadcast) needs no extra confirmation.
		restored := state == "failed" && strings.HasPrefix(payoutErr, restoredHoldPrefix)
		unknown := (state == "failed" && (ambiguous || restored)) || stuck
		if restored && c.Form.Get("not_broadcast") != "confirmed" {
			return actionResult{}, fail(400, "This failed payout was restored from backup and may have been requeued and sent after the backup was taken. Check the wallet, then confirm that no transaction was broadcast to requeue it, or mark it sent with the wallet's transaction ID.")
		}
		if unknown && c.Form.Get("not_broadcast") != "confirmed" {
			return actionResult{}, fail(400, "This payout's last send had no definite answer from the wallet and may have been broadcast. Check the wallet, then confirm that no transaction was broadcast to requeue it, or mark it sent with the wallet's transaction ID.")
		}
		res, err = tx.ExecContext(ctx, `UPDATE payouts SET state=CASE WHEN address='' THEN 'blocked' ELSE 'pending' END,error='',txid='',send_ambiguous=false,updated=now()
			WHERE id=$1 AND (state='failed' OR `+stuckSending+`)`, id)
		note = "Administrator requeued the " + c.A.netTag() + " payout of " + label + " after checking the wallet; it will be sent once more."
		audit = "Requeued payout " + strconv.FormatInt(id, 10) + " (" + label + ", was " + state + ") for order " + short
		if restored {
			note = "Administrator confirmed the wallet shows no broadcast transaction for the " + c.A.netTag() + " payout of " + label + " (failed, restored from backup) and requeued it; it will be sent once more."
			audit += "; restored from backup, administrator confirmed the wallet shows no broadcast transaction"
		} else if unknown {
			note = "Administrator confirmed the wallet shows no broadcast transaction for the " + c.A.netTag() + " payout of " + label + " (last send outcome unknown) and requeued it; it will be sent once more."
			audit += "; last send outcome unknown, administrator confirmed the wallet shows no broadcast transaction"
		}
		note += feeTaken
	case "repoint":
		switch {
		case state == "sending":
			return actionResult{}, fail(409, "This payout is being sent, or its send is stuck and may have been broadcast, so it is never moved to another address: that could pay twice. Nothing was changed.")
		case (state == "held" || state == "failed") && !repointable(state, payoutErr, ambiguous):
			return actionResult{}, fail(409, "This payout may already have been broadcast (its last send had no definite answer, or it was restored from backup), so it is never moved to another address: that could pay twice. Check the wallet, then release, requeue or mark it sent. Nothing was changed.")
		case !repointable(state, payoutErr, ambiguous):
			return actionResult{}, fail(409, "This payout is "+payoutRefusalLabel(state, payoutErr)+": only a held payout or one the wallet definitely rejected can be moved to the account's current address. Nothing was changed.")
		}
		column := "payout_btc"
		if cur == "XMR" {
			column = "payout_xmr"
		}
		var current string
		if err = tx.QueryRowContext(ctx, "SELECT "+column+" FROM users WHERE id=$1", recipient).Scan(&current); err != nil {
			return actionResult{}, err
		}
		if current == "" {
			return actionResult{}, fail(409, "The recipient has no saved "+cur+" payout address, so there is nothing to move this payout to. Nothing was changed.")
		}
		if address == "" {
			return actionResult{}, fail(409, "This payout has no address yet; it gains the recipient's address when they save one. Nothing was changed.")
		}
		if current == address {
			return actionResult{}, fail(409, "This payout already uses the account's current "+cur+" payout address. Nothing was changed.")
		}
		// The administrator checked the two addresses the page showed: either changing since then voids the check.
		if c.Form.Get("from_address") != address || c.Form.Get("to_address") != current {
			return actionResult{}, fail(409, "The payout's address or the account's saved "+cur+" payout address changed since the page loaded. Reload the admin page and check the addresses shown with the account owner again. Nothing was changed.")
		}
		if c.Form.Get("address_checked") != "confirmed" {
			return actionResult{}, fail(400, "Anyone with the account's password can change its saved payout address. Check the account's current address with the account owner through a channel you trust, then confirm that you checked it to move the payout. Nothing was changed.")
		}
		// Compare-and-set on the state, address and error read above: a concurrent release, requeue, claim or
		// second move changes one of them, and this one then changes nothing.
		res, err = tx.ExecContext(ctx, `UPDATE payouts SET address=$2,updated=now()
			WHERE id=$1 AND state=$3 AND address=$4 AND error=$5 AND send_ambiguous=$6 AND state IN ('held','failed')`, id, current, state, address, payoutErr, ambiguous)
		note = "Administrator moved the " + c.A.netTag() + " payout of " + label + " to the recipient's current payout address after checking it with them; nothing was sent and its status is unchanged." + feeTaken
		audit = "Moved payout " + strconv.FormatInt(id, 10) + " (" + label + ", " + state + ") for order " + short + " from " + address +
			" to the account's current address " + current + "; administrator checked the address with the account owner"
	case "hold":
		// Only a blocked payout (no payout address, so the watcher cannot send it): the pending half of A-34 is not
		// built. The compare-and-set repeats the state check made under the row lock.
		if state != "blocked" {
			return actionResult{}, fail(409, "Only a blocked payout (its recipient saved no payout address) can be held to be paid by hand; this payout is "+payoutRefusalLabel(state, payoutErr)+". Nothing was changed.")
		}
		staffNote := strings.TrimSpace(c.Form.Get("note"))
		res, err = tx.ExecContext(ctx, `UPDATE payouts SET state='held',error=$2,updated=now() WHERE id=$1 AND state='blocked'`, id, handHoldPrefix+" "+staffNote)
		// The order history, which both parties read, does not repeat the staff note.
		note = c.A.netTag() + " payout of " + label + " held by an administrator to be paid by hand; the market does not send it while it is held."
		audit = "Held blocked payout " + strconv.FormatInt(id, 10) + " (" + label + ") for order " + short + " to pay it by hand: " + staffNote
	case "sent":
		// The market's own send is queued, under way or done: a payment made by hand would be a second one.
		switch {
		case state == "pending":
			return actionResult{}, fail(409, "The market has queued this payout and will send it itself, so a payment you made by hand would be a second payment. Reconcile with the recipient. Nothing was changed.")
		case state == "sending" && !stuck:
			return actionResult{}, fail(409, "The market is sending this payout itself right now, so a payment you made by hand would be a second payment. Reconcile with the recipient. Nothing was changed.")
		case state == "sent":
			return actionResult{}, fail(409, "This payout is already recorded as sent (by the market's wallet, or by an administrator), so a payment you made by hand would be a second payment. Reconcile with the recipient. Nothing was changed.")
		}
		res, err = tx.ExecContext(ctx, `UPDATE payouts SET state='sent',txid=$2,error='',send_ambiguous=false,updated=now()
			WHERE id=$1 AND (state IN ('failed','held','blocked') OR `+stuckSending+`)`, id, txid)
		// The order history names no txid (both parties read it); the recipient's notification does (A-214).
		note = c.A.netTag() + " payout of " + label + " recorded as sent by an administrator after wallet reconciliation."
		audit = "Marked payout " + strconv.FormatInt(id, 10) + " (" + label + ", was " + state + ") sent with transaction " + txid + " for order " + short
		if state == "blocked" || heldForHand(state, payoutErr) {
			why := "the recipient had saved no payout address"
			if address != "" { // saved after the payout was held (A-489)
				why = "the recipient had no payout address when it was held"
			}
			note = c.A.netTag() + " payout of " + label + " recorded as sent by hand by an administrator: " + why + ". It will not be sent again."
			audit += "; paid by hand (" + why + "); the market did not check the transaction on the chain"
		}
	}
	if err != nil {
		return actionResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return actionResult{}, fail(409, "This payout is "+payoutRefusalLabel(state, payoutErr)+"; that action does not apply. Nothing was changed. Reload the admin page.")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", orderID, orderState, c.User.ID, note); err != nil {
		return actionResult{}, err
	}
	if op == "sent" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), recipient, "Order "+short+": "+note+" Transaction ID: "+txid); err != nil {
			return actionResult{}, err
		}
	}
	if op == "repoint" {
		// The recipient hears that the destination changed (the address itself is not repeated), as for their own saves.
		body := "Order " + short + ": an administrator moved your unsent " + c.A.netTag() + " payout of " + label + " to your current " + cur +
			" payout address after checking it with you; nothing was sent and its status is unchanged." + feeTaken + " If you did not confirm this, contact the market staff."
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), recipient, body); err != nil {
			return actionResult{}, err
		}
	}
	notice := "payout-released"
	switch op {
	case "requeue":
		notice = "payout-requeued"
	case "sent":
		notice = "payout-sent"
	case "repoint":
		notice = "payout-repointed"
	case "hold":
		notice = "payout-held"
	}
	return actionResult{Redirect: "/admin", Notice: notice, Audit: audit}, nil
}
