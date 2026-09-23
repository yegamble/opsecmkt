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
// the payout with a compare-and-set on its state, so repeated or concurrent submissions act once.

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

var txidPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func adminPayoutAction(c *actionCtx) (actionResult, error) {
	op := c.Form.Get("op")
	if op != "release" && op != "requeue" && op != "sent" {
		return actionResult{}, fail(400, "Choose release, requeue or mark sent")
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
	var orderID, recipient, cur, state, orderState string
	var amt int64
	err := tx.QueryRowContext(ctx, `SELECT p.order_id,p.user_id,p.currency,p.amount,p.state,o.state FROM payouts p JOIN orders o ON o.id=p.order_id
		WHERE p.id=$1 FOR UPDATE OF p`, id).Scan(&orderID, &recipient, &cur, &amt, &state, &orderState)
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
		// A payout the watcher itself holds (credited deposit conflicted or re-confirming) stays held while
		// that is still true: sendPayouts would refuse it anyway.
		threshold := int64(0)
		if p := c.A.provider(cur); p != nil {
			threshold = int64(p.Confirmations())
		}
		var unsettled bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payments WHERE order_id=$1 AND credited AND confirmations<$2)", orderID, threshold).Scan(&unsettled); err != nil {
			return actionResult{}, err
		}
		if unsettled && state == "held" {
			return actionResult{}, fail(409, "A credited deposit for this order is still conflicted or below the confirmation threshold. Review it with a moderator; the payout stays held.")
		}
		res, err = tx.ExecContext(ctx, `UPDATE payouts SET state=CASE WHEN address='' THEN 'blocked' ELSE 'pending' END,error='',updated=now()
			WHERE id=$1 AND state='held'`, id)
		note = "Administrator released the held TESTNET payout of " + label + "; it is queued for a single send."
		audit = "Released held payout " + strconv.FormatInt(id, 10) + " (" + label + ") for order " + short
	case "requeue":
		res, err = tx.ExecContext(ctx, `UPDATE payouts SET state=CASE WHEN address='' THEN 'blocked' ELSE 'pending' END,error='',txid='',updated=now()
			WHERE id=$1 AND (state='failed' OR `+stuckSending+`)`, id)
		note = "Administrator requeued the TESTNET payout of " + label + " after checking the wallet; it will be sent once more."
		audit = "Requeued payout " + strconv.FormatInt(id, 10) + " (" + label + ", was " + state + ") for order " + short
	case "sent":
		res, err = tx.ExecContext(ctx, `UPDATE payouts SET state='sent',txid=$2,error='',updated=now()
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
	return actionResult{Redirect: "/admin?saved=1#payouts", Audit: audit}, nil
}
