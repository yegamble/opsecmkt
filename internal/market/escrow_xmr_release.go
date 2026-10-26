package market

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The release of a Monero escrow order (WR-escrow Phase 3, slice X-e; P3-R7, the Phase 2 rules P2-R1 to P2-R4 mapped to
// a custodial account: there are no party signatures). Behind ESCROW_XMR (off by default); fakechain or stagenet (A-449).
//
//  1. Digital claim (/escrow-xmr/claim): the vendor of a completed digital order, or of any order whose dispute was
//     decided for the vendor with no release yet, gives a fresh Monero address and their password, as escrowClaimAction
//     does; the release is requested to it (kind release, state requested).
//  2. Mark shipped (/escrow-xmr/ship): the vendor of a paid physical order gives the claim address and their password;
//     in one transaction the order moves paid -> shipped, the release is requested to that address and
//     escrows.release_at is the escrow's snapshot of release_days from now. The pooled ship form refuses the order.
//  3. Nothing is built before the outcome: the Monero escrow watcher builds a settlement only when settlementFits
//     allows it (escrow_xmr_settle.go), the rule escrow_market_may_sign (migration 081) enforces when 084 stores its
//     transaction. A release requested at shipment therefore waits until the order is completed or decided for the
//     vendor; a decision for the buyer supersedes it (escrowSettleTerminal).
//  4. Confirm receipt (/escrow-xmr/confirm): the buyer of a shipped order gives their password; under the order row lock,
//     rechecked shipped with no dispute, the order moves shipped -> completed by the buyer through xmrConfirmOnly, the
//     one exception to buyerCompleteRefused. A completed order cannot be disputed. Otherwise the release deadline completes
//     it (escrow_xmr_deadline.go).
//
// No step makes an RPC: addresses are parsed offline (checkXMRFormAddress).

func init() {
	registerAction("/escrow-xmr/claim", actionSpec{Roles: escrowPartyRoles, OwnTx: true, OneAtATime: true, Off: escrowXMROff,
		ConfirmedFormNonce: true, ReceiptAuthorize: xmrClaimReceiptAuthorize, Run: xmrEscrowClaimAction})
	registerAction("/escrow-xmr/ship", actionSpec{Roles: escrowPartyRoles, OwnTx: true, OneAtATime: true, Off: escrowXMROff,
		ConfirmedFormNonce: true, ReceiptAuthorize: xmrShipReceiptAuthorize, Run: xmrEscrowShipAction})
	registerAction("/escrow-xmr/confirm", actionSpec{Roles: escrowPartyRoles, OwnTx: true, OneAtATime: true, Off: escrowXMROff,
		ConfirmedFormNonce: true, ReceiptAuthorize: xmrConfirmReceiptAuthorize, Run: xmrConfirmReceiptAction})
}

// A-405: the pooled ship and Complete forms and a moderator's resolve refuse a Monero escrow order with Monero wording
// (errShipPlainForm, errEscrowComplete and errEscrowResolveAdmin are Bitcoin escrow's).
var (
	errShipPlainFormXMR      = fail(409, "This order is paid into a Monero escrow: mark it shipped with the Monero escrow form on the order page, which takes the address the release to you pays, and your password. Nothing was changed.")
	errEscrowCompleteXMR     = fail(409, "This order is paid into a Monero escrow: use Confirm receipt on the order page, which asks for your password, not this form. Nothing was changed.")
	errXMREscrowResolveAdmin = fail(403, "Only an administrator decides a dispute on a Monero escrow order: the decision moves coins from the escrow. Nothing was changed.")
)

var (
	errXMRClaimRefund    = fail(409, "That is the buyer's refund address for this order, not an address of your own wallet. Give a fresh address from your own wallet. Nothing was changed.")
	errConfirmOnlyBuyer  = fail(403, "Only the buyer confirms receipt of this order.")
	errConfirmNotShipped = fail(409, "Only a shipped order's receipt can be confirmed. Reload the page. Nothing was changed.")
	errConfirmDisputed   = fail(409, "This order is disputed: an administrator decides it, and the decision settles the escrow, so receipt cannot be confirmed now. Nothing was changed.")
	errXMRReleaseFinal   = fail(409, "The release to the vendor already has its transaction, built by the market from this order's escrow account, so the escrow pays the vendor and the dispute cannot be decided for the buyer. Decide it for the vendor. Nothing was changed.")
	xmrShippedNote       = "Release requested at shipment (Monero escrow): the vendor gave the address the release pays. Nothing is built or sent before the order is completed: when the buyer confirms receipt, or at the release deadline if the buyer has neither confirmed receipt nor opened a dispute, the market builds the release from this order's escrow account (less the market fee and the network fee; " + escrowExcessRule("XMR") + ") and sends it."
	xmrClaimNote         = "Vendor requested the escrow claim (Monero escrow): the market builds the release from this order's escrow account (less the market fee and the network fee; " + escrowExcessRule("XMR") + ") and sends it. Nothing is sent yet."
	xmrConfirmedNote     = "Receipt confirmed by the buyer (Monero escrow): the order is completed, and the market builds the release of the escrowed coins to the address the vendor gave when marking it shipped, and sends it. This cannot be undone."
)

// xmrClaimForm reads a vendor's Monero claim address from the form, parsed offline (checkXMRFormAddress).
func xmrClaimForm(c *actionCtx) (string, error) {
	addr := strings.TrimSpace(c.Form.Get("address"))
	if err := checkXMRFormAddress(addr, c.A.escrowXMR.formNet()); err != nil {
		return "", fail(400, "The claim address is refused: "+err.Error()+". Nothing was changed.")
	}
	return addr, nil
}

// xmrClaimAddress refuses a claim address for order that this market knows (an escrow, slot or wallet address, the fee
// address, this order's buyer refund address or an external address this vendor
// account used before), each with its own message.
func xmrClaimAddress(c *actionCtx, orderID, addr string) error {
	use, err := xmrAddressUse(c.Ctx(), c.Tx, addr, c.A.escrowXMR.feeAddress, orderID, c.User.ID)
	switch use { // the checkout's refusals of a refund address read the same for a claim address
	case "escrow":
		return errXMRRefundEscrow
	case "wallet":
		return errXMRRefundWallet
	case "fee":
		return errXMRRefundFee
	case "refund":
		return errXMRClaimRefund
	case "used":
		return errClaimAddressUsed
	}
	return err
}

// insertXMRRelease requests the release of order to dest (state requested), requested by the vendor, mapping the
// database's refusals: a second outcome, or a destination another release pays.
func insertXMRRelease(c *actionCtx, orderID, dest string) error {
	_, err := c.Tx.ExecContext(c.Ctx(), "INSERT INTO xmr_settlements(order_id,kind,destination,requested_by) VALUES($1,'release',$2,$3)", orderID, dest, c.User.ID)
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "xmr_settlements_one_outcome":
		return errClaimTwice
	case errors.As(err, &pe) && pe.Code == "23505":
		return errClaimAddressUsed
	}
	return err
}

// xmrEscrowClaimAction: the vendor requests the release of a completed Monero escrow order (step 1 of the file comment).
func xmrEscrowClaimAction(c *actionCtx) (actionResult, error) {
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		dest, err := xmrClaimForm(c)
		if err != nil {
			return actionResult{}, err
		}
		ctx := c.Ctx()
		o, err := partyOrderLocked(c, c.Form.Get("order_id"))
		if err != nil {
			return actionResult{}, err
		}
		outcome, err := escrowDisputeOutcome(ctx, c.Tx, o.ID)
		if err != nil {
			return actionResult{}, err
		}
		switch {
		case !o.xmrEscrow():
			return actionResult{}, errNoOrder
		case o.VendorID != c.User.ID:
			return actionResult{}, errClaimOnlyVendor
		case !settlementFits("release", o.State, outcome):
			return actionResult{}, errClaimState
		}
		if err = xmrClaimAddress(c, o.ID, dest); err != nil {
			return actionResult{}, err
		}
		if err = insertXMRRelease(c, o.ID, dest); err != nil {
			return actionResult{}, err
		}
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", o.ID, o.State, c.User.ID, xmrClaimNote); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: orderPage(o.ID), Notice: "escrow-xmr-claim",
			Audit: "Requested the escrow claim of order " + shortID(o.ID) + " (Monero escrow, " + c.A.xmrEscrowNetwork() + ")"}, nil
	})
}

// xmrEscrowShipAction: the vendor of a paid physical Monero escrow order marks it shipped (step 2 of the file comment).
func xmrEscrowShipAction(c *actionCtx) (actionResult, error) {
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		note, err := formNote(c, "Marked shipped")
		if err != nil {
			return actionResult{}, err
		}
		dest, err := xmrClaimForm(c)
		if err != nil {
			return actionResult{}, err
		}
		ctx := c.Ctx()
		o, err := partyOrder(c, c.Form.Get("order_id"))
		switch {
		case err != nil:
			return actionResult{}, err
		case !o.xmrEscrow():
			return actionResult{}, errNoOrder
		case o.VendorID != c.User.ID:
			return actionResult{}, errShipOnlyVendor
		case o.State != statePaid:
			return actionResult{}, shipConflict(c, o.ID)
		}
		if err = xmrClaimAddress(c, o.ID, dest); err != nil {
			return actionResult{}, err
		}
		// P2-R9: the buyer's shipped notice states the release deadline, the value release_at takes below.
		var deadline time.Time
		err = c.Tx.QueryRowContext(ctx, "SELECT now()+make_interval(days => release_days) FROM escrows WHERE order_id=$1 AND coin='XMR' AND release_days IS NOT NULL AND release_at IS NULL", o.ID).Scan(&deadline)
		if err == sql.ErrNoRows {
			return actionResult{}, errShipNoDeadline
		}
		if err != nil {
			return actionResult{}, err
		}
		ctx = withNoticeDetail(ctx, o.ID, stateShipped, "Release deadline: "+utcUntil(deadline)+". Confirm receipt on the order page once the goods have arrived; if they have not arrived, open a dispute before then. "+
			"If you do neither, the order completes at that time and the market releases the coins in this Monero escrow to the vendor: if you cannot confirm receipt, you need do nothing.")
		if _, err = c.A.transition(ctx, c.Tx, o.ID, statePaid, stateShipped, c.User, note); err == errConflict {
			return actionResult{}, shipConflict(c, o.ID) // P2-R5: the buyer's no-ship cancel took the order row first
		} else if err != nil {
			return actionResult{}, err
		}
		res, err := c.Tx.ExecContext(ctx, "UPDATE escrows SET release_at=now()+make_interval(days => release_days) WHERE order_id=$1 AND coin='XMR' AND release_days IS NOT NULL AND release_at IS NULL", o.ID)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return actionResult{}, errShipNoDeadline
		}
		if err = insertXMRRelease(c, o.ID, dest); err != nil {
			return actionResult{}, err
		}
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, stateShipped, xmrShippedNote); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: orderPage(o.ID) + "#xmr-escrow-deadline", Notice: "order-shipped",
			Audit: "Marked order " + shortID(o.ID) + " shipped (Monero escrow, " + c.A.xmrEscrowNetwork() + ")"}, nil
	})
}

// xmrResolveConfirm labels the resolve form's confirmation of a Monero escrow order: its decision is final once made
// (chair ruling on QA-Xe F4, P3-R7), as the market builds and sends it on its next pass.
const xmrResolveConfirm = "I checked the amount and who receives it for the outcome I chose. The market sends these coins on its next pass; this cannot be undone."

// xmrDisputePreview is escrowDisputePreview for a Monero escrow order (A-405): the counted escrowed amount in XMR, the
// transaction each outcome has the market build from the order's account, and the release requested at shipment.
func (a *App) xmrDisputePreview(ctx context.Context, o *Order) (PayoutPreview, error) {
	var sum, left int64
	var release, final bool
	if err := a.db.QueryRowContext(ctx, escrowCountedSQL, o.ID).Scan(&sum, &left); err != nil {
		return PayoutPreview{}, err
	}
	if err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM xmr_settlements WHERE order_id=$1 AND kind='release' AND state<>'superseded'),
		EXISTS(SELECT 1 FROM xmr_settlements WHERE order_id=$1 AND kind='release' AND state<>'superseded' AND (tx_hash IS NOT NULL OR state='external'))`, o.ID).Scan(&release, &final); err != nil {
		return PayoutPreview{}, err
	}
	escrowed, fees := a.escrowAmount(o, sum), escrowReleaseFees(a.escrowExcess(o, sum-left))
	p := PayoutPreview{Seen: sum, Confirm: xmrResolveConfirm,
		Release: "Release to " + o.Vendor + ": the vendor claims the escrowed " + escrowed + " with a fresh address, and the market builds the release from this order's escrow account and sends it " + fees,
		Refund:  "Refund to " + o.Buyer + ": the escrowed " + escrowed + ", less the network fee, go back to the buyer's refund address by a refund the market builds from this order's escrow account and sends",
		Escrow:  "This is a Monero escrow order (" + a.xmrEscrowNetShort() + "): its coins are " + xmrCustody + ". The decision is paid from this order's escrow account by a transaction the market builds and sends: a refund goes to the buyer's refund address given at checkout; a release pays the address the vendor gave when marking the order shipped, or one the vendor gives afterwards to claim to. On its next pass after you resolve, the market sends the coins to the vendor or refunds the buyer, as you decide: this cannot be undone, and the decision cannot be changed once it is made."}
	switch {
	case final:
		p.Release = "Release to " + o.Vendor + ": the release to the vendor already has its transaction, so the escrow pays the vendor"
		p.Refund = "Refund to " + o.Buyer + ": refused, because the release to the vendor already has its transaction"
	case release:
		p.Release = "Release to " + o.Vendor + ": the release to the vendor requested when the order was shipped pays the escrowed " + escrowed + " to the address the vendor gave then " + fees
		p.Refund += "; the release to the vendor requested when the order was shipped is cancelled"
	}
	return p, nil
}

// xmrConfirmReceiptAction: the buyer of a shipped Monero escrow order confirms receipt (step 4 of the file comment).
func xmrConfirmReceiptAction(c *actionCtx) (actionResult, error) {
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		o, err := partyOrderLocked(c, c.Form.Get("order_id"))
		switch {
		case err != nil:
			return actionResult{}, err
		case !o.xmrEscrow():
			return actionResult{}, errNoOrder
		case o.BuyerID != c.User.ID:
			return actionResult{}, errConfirmOnlyBuyer
		}
		// Checked in a statement after the lock, so a dispute committed while this waited is seen (releaseIfDue's rule).
		var shipped, disputed bool
		if err = c.Tx.QueryRowContext(ctx, "SELECT o.state='shipped',EXISTS(SELECT 1 FROM disputes d WHERE d.order_id=o.id) FROM orders o WHERE o.id=$1", o.ID).Scan(&shipped, &disputed); err != nil {
			return actionResult{}, err
		}
		switch {
		case disputed:
			return actionResult{}, errConfirmDisputed
		case !shipped:
			return actionResult{}, errConfirmNotShipped
		}
		notice := "escrow-xmr-confirmed"
		detail, note := "The buyer confirmed receipt (Monero escrow): the market builds the release of the escrowed coins to the address you gave when marking the order shipped, and sends it.", xmrConfirmedNote
		// A-652d: while the vendor is banned the release is held (xmrReleaseHeld), so the notes say so. A ban committed
		// after this read leaves the usual wording; the order page then shows the hold.
		if banned, err := orderVendorBanned(ctx, c.Tx, o.ID); err != nil {
			return actionResult{}, err
		} else if banned {
			notice = "escrow-xmr-held"
			detail = "The buyer confirmed receipt (Monero escrow). Your account is closed, so the market holds the release of the escrowed coins: it builds and sends nothing while the account is banned."
			note = "Receipt confirmed by the buyer (Monero escrow): the order is completed. " + xmrHeldNote + " This cannot be undone."
		}
		ctx = withNoticeDetail(withConfirmReceipt(ctx), o.ID, stateCompleted, detail)
		if _, err = c.A.transition(ctx, c.Tx, o.ID, stateShipped, stateCompleted, c.User, note); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: orderPage(o.ID), Notice: notice,
			Audit: "Confirmed receipt of order " + shortID(o.ID) + " (Monero escrow, " + c.A.xmrEscrowNetwork() + ")"}, nil
	})
}
