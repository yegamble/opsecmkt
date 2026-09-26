package market

import (
	"context"
	"database/sql"
	"net/http"
	"regexp"
	"strconv"
	"strings"
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
	if op != "release" && op != "requeue" && op != "sent" && op != "repoint" {
		return actionResult{}, fail(400, "Choose release, requeue, mark sent or use the account's current address")
	}
	id, err := strconv.ParseInt(c.Form.Get("payout_id"), 10, 64)
	if err != nil || id <= 0 {
		return actionResult{}, fail(400, "Unknown payout")
	}
	txid := strings.ToLower(strings.TrimSpace(c.Form.Get("txid")))
	if op == "sent" && !txidPattern.MatchString(txid) {
		return actionResult{}, fail(400, "Enter the 64-character hexadecimal transaction ID found in the wallet")
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
		note = "Administrator released the held TESTNET payout of " + label + "; it is queued for a single send."
		audit = "Released held payout " + strconv.FormatInt(id, 10) + " (" + label + ") for order " + short
		if restored {
			note = "Administrator confirmed the wallet shows no broadcast transaction for the TESTNET payout of " + label + " (held after being restored from backup) and released it; it is queued for a single send."
			audit += "; restored from backup, administrator confirmed the wallet shows no broadcast transaction"
		}
		if suspendedHeld {
			note = "Administrator checked the payout address and released the held TESTNET payout of " + label + "; it is queued for a single send."
			if restored {
				note = "Administrator confirmed the wallet shows no broadcast transaction for the TESTNET payout of " + label + " (held after being restored from backup) and checked the payout address (held for an account suspension), and released it; it is queued for a single send."
			}
			audit += "; held for an account suspension, administrator checked the payout address " + address
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
		note = "Administrator requeued the TESTNET payout of " + label + " after checking the wallet; it will be sent once more."
		audit = "Requeued payout " + strconv.FormatInt(id, 10) + " (" + label + ", was " + state + ") for order " + short
		if restored {
			note = "Administrator confirmed the wallet shows no broadcast transaction for the TESTNET payout of " + label + " (failed, restored from backup) and requeued it; it will be sent once more."
			audit += "; restored from backup, administrator confirmed the wallet shows no broadcast transaction"
		} else if unknown {
			note = "Administrator confirmed the wallet shows no broadcast transaction for the TESTNET payout of " + label + " (last send outcome unknown) and requeued it; it will be sent once more."
			audit += "; last send outcome unknown, administrator confirmed the wallet shows no broadcast transaction"
		}
	case "repoint":
		switch {
		case state == "sending":
			return actionResult{}, fail(409, "This payout is being sent, or its send is stuck and may have been broadcast, so it is never moved to another address: that could pay twice. Nothing was changed.")
		case (state == "held" || state == "failed") && !repointable(state, payoutErr, ambiguous):
			return actionResult{}, fail(409, "This payout may already have been broadcast (its last send had no definite answer, or it was restored from backup), so it is never moved to another address: that could pay twice. Check the wallet, then release, requeue or mark it sent. Nothing was changed.")
		case !repointable(state, payoutErr, ambiguous):
			return actionResult{}, fail(409, "This payout is "+payoutStateLabel(state)+": only a held payout or one the wallet definitely rejected can be moved to the account's current address. Nothing was changed.")
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
		note = "Administrator moved the TESTNET payout of " + label + " to the recipient's current payout address after checking it with them; nothing was sent and its status is unchanged."
		audit = "Moved payout " + strconv.FormatInt(id, 10) + " (" + label + ", " + state + ") for order " + short + " from " + address +
			" to the account's current address " + current + "; administrator checked the address with the account owner"
	case "sent":
		res, err = tx.ExecContext(ctx, `UPDATE payouts SET state='sent',txid=$2,error='',send_ambiguous=false,updated=now()
			WHERE id=$1 AND (state IN ('failed','held') OR `+stuckSending+`)`, id, txid)
		note = "TESTNET payout of " + label + " recorded as sent by an administrator after wallet reconciliation: " + txid
		audit = "Marked payout " + strconv.FormatInt(id, 10) + " (" + label + ", was " + state + ") sent with transaction " + txid + " for order " + short
	}
	if err != nil {
		return actionResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return actionResult{}, fail(409, "This payout is "+payoutStateLabel(state)+"; that action does not apply. Reload the admin page.")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", orderID, orderState, c.User.ID, note); err != nil {
		return actionResult{}, err
	}
	if op == "sent" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), recipient, "Order "+short+": "+note); err != nil {
			return actionResult{}, err
		}
	}
	if op == "repoint" {
		// The recipient hears that the destination changed (the address itself is not repeated), as for their own saves.
		body := "Order " + short + ": an administrator moved your unsent TESTNET payout of " + label + " to your current " + cur +
			" payout address after checking it with you; nothing was sent and its status is unchanged. If you did not confirm this, contact the market staff."
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), recipient, body); err != nil {
			return actionResult{}, err
		}
	}
	return actionResult{Redirect: "/admin?saved=1#payouts", Audit: audit}, nil
}
