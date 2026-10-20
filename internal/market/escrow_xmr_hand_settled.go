package market

import (
	"database/sql"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Closing a stuck failed Monero settlement's alert (A-613: PAY560-2, PAY613-1, PAY613-3). Two kinds of failed
// settlement never leave state failed, so their escrow staff alerts (A-560) had no end. An administrator who is not a
// party closes one on the order page with a note, recorded in xmr_settlement_hand_settled (migration 128) with a kind
// the settlement fixes:
//   - nothing-owed (*Dismiss settlement N (nothing owed now)*): it failed before any transaction was built
//     (nothing-to-settle: no deposit counted when it was to be built), so nothing was sent and nothing is owed for it
//     now. Dismissing it changes nothing the market does: a deposit of the escrow listed again is handled automatically
//     as usual (on an order whose coins go back to the buyer a stray refund returns it) and flagged for review, so staff
//     must not pay it by hand. A refund too few raises no alert (xmrFailedClosedSQL) and is refused here, as is one
//     that still claims a deposit (xmrClaimsSQL, PAY613-4: a re-request that failed so, A-614): its coins are in the
//     escrow wallet and may be owed, so it keeps its alert and staff settle with the operator.
//   - settled-by-hand (*Mark settlement N settled by hand*): it has a stored transaction whose last re-request was
//     refused for good (xmrReRequestFinal: on-chain or key-image-spent), so it is never superseded; staff checked the
//     escrow wallet and the chain and settled anything still owed with the operator. One with no re-request yet, one
//     whose re-request waits, and one refused in-pool (the transaction may still be mined or dropped) are refused
//     (409): it is re-requested (again) instead. One recorded here is never re-requested again (xmrReRequestAction, and
//     migration 128 refuses the row).
//
// The action follows the refused-deposit disposition (xmrCollisionAction): the party check before the password (a
// refused party spends none of the confirmation budget), the password (and authenticator code when enrolled) through
// confirmedTx, one audit row, a 409 for a second or concurrent record, no order event and no notification, so the
// parties see nothing new; like it, it needs no wallet and works while ESCROW_XMR is off. It is the administrators'
// alone, as the other settlement actions on the order page are (re-request, build again): it closes a money alert. The
// market sends nothing for the settlement either way.

func init() {
	registerAction("/admin/escrow-xmr-settled-by-hand", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true,
		ReceiptAuthorize: xmrSettledByHandReceiptAuthorize, Run: xmrSettledByHandAction})
}

// Kinds of xmr_settlement_hand_settled (migration 128).
const (
	xmrNothingOwed   = "nothing-owed"
	xmrSettledByHand = "settled-by-hand"
)

// xmrReRequestFinal reports whether a re-request refused with class can never be carried out later: the stored
// transaction is in the chain, or a coin it spends is spent. A transaction in the pool may still be mined or dropped.
func xmrReRequestFinal(class string) bool {
	return class == xmrReRequestOnChain || class == xmrReRequestSpent
}

var (
	errSettledByHandParty    = fail(403, "You are a party to this order, so another administrator records this settlement. Nothing was changed.")
	errSettledByHandState    = fail(409, "Only a failed Monero escrow settlement the market will never send can be recorded here: one that failed before any transaction was built (other than a refund too few), or one whose stored transaction's last re-request found it in the chain or its coins spent. Re-request a failed settlement with a stored transaction first. Reload the page. Nothing was changed.")
	errSettledByHandInPool   = fail(409, "The last re-request of this settlement was refused because its stored transaction was in the Monero daemon's pool, so it may still be mined or dropped. Re-request it again later; it can be marked settled by hand only once a re-request finds that transaction in the chain or a coin it spends spent. Nothing was changed.")
	errSettledByHandClaims   = fail(409, "Deposits of this escrow are still claimed by this settlement, so their coins are in the escrow wallet and money may still be owed: it cannot be dismissed as nothing owed. Settle them with the operator. Nothing was changed.")
	errSettledByHandRecorded = fail(409, "This settlement was already recorded (dismissed, or marked settled by hand); see the order page. Nothing was changed.")
)

// xmrSettledByHandParties reads, in q, the buyer and vendor of Monero escrow settlement id (404 for none).
func xmrSettledByHandParties(c *actionCtx, q rowQuerier, id int64) (buyer, vendor string, err error) {
	err = q.QueryRowContext(c.Ctx(), `SELECT o.buyer_id,p.vendor_id FROM xmr_settlements s JOIN orders o ON o.id=s.order_id
		JOIN products p ON p.id=o.product_id JOIN escrows e ON e.order_id=o.id AND e.coin='XMR' WHERE s.id=$1`, id).Scan(&buyer, &vendor)
	if err == sql.ErrNoRows {
		err = errReRequestNone
	}
	return
}

// A saved result stays private to current non-party administrators; the settlement's state is left to
// recordXMRSettledByHand so that an exact retry of a completed form retrieves its response.
func xmrSettledByHandReceiptAuthorize(c *actionCtx) error {
	id, err := strconv.ParseInt(c.Form.Get("settlement_id"), 10, 64)
	if err != nil || id <= 0 {
		return errReRequestNone
	}
	buyer, vendor, err := xmrSettledByHandParties(c, c.Tx, id)
	if err != nil {
		return err
	}
	if c.User.ID == buyer || c.User.ID == vendor {
		return errSettledByHandParty
	}
	return nil
}

func xmrSettledByHandAction(c *actionCtx) (actionResult, error) {
	id, err := strconv.ParseInt(c.Form.Get("settlement_id"), 10, 64)
	if err != nil || id <= 0 {
		return actionResult{}, errReRequestNone
	}
	note := strings.TrimSpace(c.Form.Get("note"))
	if n := utf8.RuneCountInString(note); n < 10 || n > 500 {
		return actionResult{}, fail(400, "Write the note in 10–500 characters: for a dismissal, why nothing is owed now; for a settlement marked settled by hand, what the wallet and the chain showed and what was done. Only market staff read it.")
	}
	buyer, vendor, err := xmrSettledByHandParties(c, c.A.db, id)
	if err != nil {
		return actionResult{}, err
	}
	if c.User.ID == buyer || c.User.ID == vendor {
		return actionResult{}, errSettledByHandParty
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		return recordXMRSettledByHand(c, id, note)
	})
}

func recordXMRSettledByHand(c *actionCtx, id int64, note string) (actionResult, error) {
	ctx, tx := c.Ctx(), c.Tx
	// The order row first, then the settlement row, as the re-request takes them.
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=(SELECT order_id FROM xmr_settlements WHERE id=$1) FOR NO KEY UPDATE OF o", id))
	if err == sql.ErrNoRows {
		return actionResult{}, errReRequestNone
	}
	if err != nil {
		return actionResult{}, err
	}
	if c.User.ID == o.BuyerID || c.User.ID == o.VendorID {
		return actionResult{}, errSettledByHandParty
	}
	var kind, last string
	var failed, stored, tooFew, recorded, claims bool
	// The re-request takes the same row lock, so none is recorded between this check and the record.
	if err = tx.QueryRowContext(ctx, `SELECT xs.kind,xs.state='failed',xs.tx_hash IS NOT NULL,xs.kind='refund' AND xs.send_error=$3 AND xs.tx_hash IS NULL,
		EXISTS(SELECT 1 FROM xmr_settlement_hand_settled h WHERE h.settlement_id=xs.id),
		COALESCE((SELECT r.state||' '||r.reason FROM xmr_settlement_rerequests r WHERE r.settlement_id=xs.id ORDER BY r.id DESC LIMIT 1),''),
		`+xmrClaimsSQL+`
		FROM xmr_settlements xs WHERE xs.id=$1 AND xs.order_id=$2 FOR UPDATE`, id, o.ID, xmrTooSmall).Scan(&kind, &failed, &stored, &tooFew, &recorded, &last, &claims); err != nil {
		return actionResult{}, err
	}
	state, reason, _ := strings.Cut(last, " ")
	switch {
	case recorded:
		return actionResult{}, errSettledByHandRecorded
	case failed && stored && state == "requested":
		return actionResult{}, errReRequestPending
	case failed && stored && state == "refused" && !xmrReRequestFinal(reason):
		return actionResult{}, errSettledByHandInPool
	case !failed || tooFew || stored && state != "refused":
		return actionResult{}, errSettledByHandState
	case !stored && claims:
		return actionResult{}, errSettledByHandClaims
	}
	record, audit := xmrNothingOwed, "Dismissed the failed Monero escrow settlement "+strconv.FormatInt(id, 10)+" ("+settlementKind(kind)+") of order "+shortID(o.ID)+": nothing owed now"
	if stored {
		record, audit = xmrSettledByHand, "Marked the failed Monero escrow settlement "+strconv.FormatInt(id, 10)+" ("+settlementKind(kind)+") of order "+shortID(o.ID)+" settled by hand"
	}
	// Under the settlement's row lock a second record finds the first; the key refuses one that ever slipped past.
	res, err := tx.ExecContext(ctx, "INSERT INTO xmr_settlement_hand_settled(settlement_id,kind,note,settled_by) VALUES($1,$2,$3,$4) ON CONFLICT (settlement_id) DO NOTHING", id, record, note, c.User.ID)
	if err != nil {
		return actionResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return actionResult{}, errSettledByHandRecorded
	}
	return actionResult{Redirect: "/moderator", Notice: "xmr-settled-by-hand", Audit: audit}, nil
}
