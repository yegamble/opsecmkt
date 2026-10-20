package market

import (
	"database/sql"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Settled by hand (A-613, PAY560-2). A Monero escrow settlement that failed before any transaction was built is never
// re-requested, and one with a stored transaction whose last re-request was refused (that transaction, or another
// spending its coins, was found in the pool or the chain) is never superseded, so their escrow staff alerts (A-560) had
// no end. A refund too few raises none (xmrFailedClosedSQL); any other such failure stays an alert until an
// administrator who is not a party records, on the order page, that what it owed was settled by hand with the operator
// (for a refused re-request, after checking the escrow wallet and the chain): xmr_settlement_hand_settled (migration
// 125), one record per settlement with a note of 10-500 characters. One with a stored transaction and no refused
// re-request is re-requested instead (409, and 409 while a re-request waits); one marked settled by hand is never
// re-requested again (xmrReRequestAction, and migration 125 refuses the row). The action follows the refused-deposit disposition
// (xmrCollisionAction): the party check before the password (a refused party spends none of the confirmation budget),
// the password (and authenticator code when enrolled) through confirmedTx, one audit row, a 409 for a second or
// concurrent record, no order event and no notification, so the parties see nothing new; like it, it needs no wallet
// and works while ESCROW_XMR is off. It is the administrators' alone, as the other settlement actions on the order page
// are (re-request, build again): it closes a money alert. The market sends nothing for the settlement either way.

func init() {
	registerAction("/admin/escrow-xmr-settled-by-hand", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true,
		ReceiptAuthorize: xmrSettledByHandReceiptAuthorize, Run: xmrSettledByHandAction})
}

var (
	errSettledByHandParty    = fail(403, "You are a party to this order, so another administrator marks its settlement settled by hand. Nothing was changed.")
	errSettledByHandState    = fail(409, "Only a failed Monero escrow settlement the market will not send again can be marked settled by hand: one that failed before any transaction was built (other than a refund too few), or one whose stored transaction's last re-request was refused. Re-request a failed settlement with a stored transaction first. Reload the page. Nothing was changed.")
	errSettledByHandRecorded = fail(409, "This settlement was already marked settled by hand; see the order page. Nothing was changed.")
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
		return actionResult{}, fail(400, "Say what was settled by hand, and how, in 10–500 characters: for example, paid to the vendor by the operator in transaction …, or nothing was owed. Only market staff read it.")
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
	var failed, stored, tooFew, recorded bool
	// The re-request takes the same row lock, so none is recorded between this check and the record.
	if err = tx.QueryRowContext(ctx, `SELECT xs.kind,xs.state='failed',xs.tx_hash IS NOT NULL,xs.kind='refund' AND xs.send_error=$3 AND xs.tx_hash IS NULL,
		EXISTS(SELECT 1 FROM xmr_settlement_hand_settled h WHERE h.settlement_id=xs.id),
		COALESCE((SELECT r.state FROM xmr_settlement_rerequests r WHERE r.settlement_id=xs.id ORDER BY r.id DESC LIMIT 1),'')
		FROM xmr_settlements xs WHERE xs.id=$1 AND xs.order_id=$2 FOR UPDATE`, id, o.ID, xmrTooSmall).Scan(&kind, &failed, &stored, &tooFew, &recorded, &last); err != nil {
		return actionResult{}, err
	}
	switch {
	case recorded:
		return actionResult{}, errSettledByHandRecorded
	case failed && stored && last == "requested":
		return actionResult{}, errReRequestPending
	case !failed || tooFew || stored && last != "refused":
		return actionResult{}, errSettledByHandState
	}
	// Under the settlement's row lock a second record finds the first; the key refuses one that ever slipped past.
	res, err := tx.ExecContext(ctx, "INSERT INTO xmr_settlement_hand_settled(settlement_id,note,settled_by) VALUES($1,$2,$3) ON CONFLICT (settlement_id) DO NOTHING", id, note, c.User.ID)
	if err != nil {
		return actionResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return actionResult{}, errSettledByHandRecorded
	}
	audit := "Marked the failed Monero escrow settlement " + strconv.FormatInt(id, 10) + " (" + settlementKind(kind) + ") of order " + shortID(o.ID) + " settled by hand"
	return actionResult{Redirect: "/moderator", Notice: "xmr-settled-by-hand", Audit: audit}, nil
}
