package market

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Monero escrow checkout (WR-escrow Phase 3, slice X-c; P3-R2, P3-R3; ES-7). With ESCROW_XMR=on and Monero escrow ready,
// the buyer of an XMR draft on a listing that takes escrow (escrowEligible: a digital listing with automatic delivery,
// or a physical one with a dispatch window whose vendor can receive an encrypted shipping address, A-83) may pay into
// an account of the market's monthly escrow wallet used only for this order, instead of the pooled wallet.
//
// POST /orders/escrow-xmr makes no RPC. The refund address is parsed offline (parseXMRAddress) and refused when it is
// integrated, of another network, an address of this market's escrow wallets (a slot or a primary address), the fee
// address, or an external destination this buyer account already used. The checkout
// transaction moves the order out of draft, claims a free slot of
// the active wallet by compare-and-set (FOR UPDATE SKIP LOCKED: concurrent checkouts never wait for, or take, the same
// slot) and inserts the escrow at the slot's address with the fee snapshot and, for a physical order, the release
// deadline length and dispatch window. With no free slot (none counts before the watcher has reconciled the active
// wallet after this process started, PAY66-9), while the restore gate is set, after a restore until Monero
// escrow's sweep (xmrEscrowSweepKey) or while Monero escrow is unavailable it answers a fixed 503 and the order stays a
// draft. The escrow watcher made each slot ahead of time (escrow_xmr_watch.go).

func init() {
	registerAction("/orders/escrow-xmr", actionSpec{FormNonce: true, Off: escrowXMROff, Run: xmrEscrowCheckoutAction})
}

// escrowXMROff is ESCROW_XMR off: the Monero escrow action does not exist.
func escrowXMROff(a *App) bool { return a.escrowXMR == nil }

// xmrEscrowReady reports whether Monero escrow passed its checks and was not refused since.
func (a *App) xmrEscrowReady() bool { return a.escrowXMR != nil && a.escrowXMR.ready.Load() }

// escrowReady reports whether escrow in coin can take a checkout now.
func (a *App) escrowReady(coin string) bool {
	switch coin {
	case "XMR":
		return a.xmrEscrowReady()
	}
	return false
}

// xmrCustody is the custody sentence of every Monero escrow page and notice (owner decision O-2, A-402).
const xmrCustody = "held by this market in an account used only for this order, inside a wallet the market opens each month; the market can move these coins"

var (
	errXMREscrowDown      = fail(503, "Monero escrow is unavailable on this market right now. Nothing was charged and your order is still a draft; try again later.")
	errXMREscrowNoSlot    = fail(503, "No Monero escrow account is free right now; the market prepares more within a minute. Nothing was charged and your order is still a draft; try again shortly.")
	errXMREscrowRestoring = fail(503, "Monero escrow is paused while this market restores its records from a backup. Nothing was charged and your order is still a draft; try again later.")
	errXMREscrowSweep     = fail(503, "Monero escrow is paused until this market has checked its escrow wallets after restoring its records from a backup. Nothing was charged and your order is still a draft; try again later.")
	errXMREscrowKind      = fail(409, "Monero escrow requires a deliverable digital listing or a physical listing with a dispatch window. Nothing was changed.")
	errXMREscrowCurrency  = fail(409, "Monero escrow is for Monero (XMR) orders only. Nothing was changed.")
	errXMREscrowSelf      = fail(409, "You cannot buy your own listing. Nothing was changed.")
	errXMRRefundEscrow    = fail(409, "That is an escrow address of this market, not an address of your own wallet. Give a fresh address from your own wallet. Nothing was changed.")
	errXMRRefundWallet    = fail(409, "That is the address of one of this market's escrow wallets, not an address of your own wallet. Give a fresh address from your own wallet. Nothing was changed.")
	errXMRRefundFee       = fail(409, "That is the market fee address, not an address of your own wallet. Give a fresh address from your own wallet. Nothing was changed.")
)

// xmrEscrowCheckoutAction: the buyer moves a draft to awaiting_payment into a Monero escrow (see the file comment).
func xmrEscrowCheckoutAction(c *actionCtx) (actionResult, error) {
	a, ctx := c.A, c.Ctx()
	if err := waitForWalletLocks(ctx, c.Tx); err != nil {
		return actionResult{}, err
	}
	x := a.escrowXMR
	if !a.xmrEscrowReady() {
		return actionResult{}, errXMREscrowDown
	}
	refund := strings.TrimSpace(c.Form.Get("refund_address"))
	if refund == "" {
		return actionResult{}, fail(400, "Enter a refund address: a Monero standard address or subaddress from your own wallet (95 characters, starting with "+xmrPrefixes(x.network())+" "+xmrOnNetwork(x.network())+"), used for nothing else. A refund of this escrow is paid there, and it cannot be changed later. Nothing was changed.")
	}
	if err := checkXMRFormAddress(refund, x.formNet()); err != nil {
		return actionResult{}, fail(400, "The refund address is refused: "+err.Error()+". Nothing was changed.")
	}
	o, err := partyOrder(c, c.Form.Get("order_id"))
	if err != nil {
		return actionResult{}, err
	}
	switch {
	case o.BuyerID != c.User.ID:
		return actionResult{}, errEscrowOnlyBuyer
	case o.State != stateDraft:
		return actionResult{}, errConflict
	case o.Currency != "XMR":
		return actionResult{}, errXMREscrowCurrency
	case o.VendorID == o.BuyerID:
		return actionResult{}, errXMREscrowSelf
	}
	var gate, sweep bool
	if err = c.Tx.QueryRowContext(ctx, "SELECT "+restoreGateSQL+",EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value='true')", xmrEscrowSweepKey).Scan(&gate, &sweep); err != nil {
		return actionResult{}, err
	}
	switch {
	case gate:
		return actionResult{}, errXMREscrowRestoring
	case sweep:
		return actionResult{}, errXMREscrowSweep
	}
	if err = lockNewIntake(c, o.BuyerID, o.VendorID, false); err != nil {
		return actionResult{}, err
	}
	// The buyer's cap on orders awaiting payment, serialised on the buyer's row as in escrowCheckoutAction.
	var open int
	if err = c.Tx.QueryRowContext(ctx, "SELECT count(*) FROM orders WHERE buyer_id=$1 AND state='awaiting_payment'", c.User.ID).Scan(&open); err != nil {
		return actionResult{}, err
	}
	if open >= maxAwaitingPayment {
		return actionResult{}, fail(409, "You already have "+strconv.Itoa(maxAwaitingPayment)+" orders awaiting payment. Pay or cancel one before requesting another payment address.")
	}
	if err = lockPaymentIntake(c, o.Currency); err != nil {
		return actionResult{}, err
	}
	eligible, price, dispatchDays, err := escrowEligible(ctx, c.Tx, &o)
	if err != nil {
		return actionResult{}, err
	} else if !eligible {
		return actionResult{}, errXMREscrowKind
	} else if price < payoutFloor(o.Currency, "release") {
		message, err := subFloorListingForProduct(ctx, c.Tx, o.ProductID, o.Currency)
		if err != nil {
			return actionResult{}, err
		}
		return actionResult{}, fail(400, message)
	}
	releaseDays := 0
	if o.Kind == "physical" {
		if ok, err := userCanReceiveEncrypted(ctx, c.Tx, o.VendorID); err != nil {
			return actionResult{}, err
		} else if !ok {
			return actionResult{}, fail(409, "Escrow unavailable. "+noVendorKeyReason+" Nothing was changed.")
		}
		if releaseDays, err = escrowReleaseDays(ctx, c.Tx); err != nil {
			return actionResult{}, err
		}
	} else {
		dispatchDays = 0 // a digital escrow has no dispatch window or release deadline
	}
	if err = checkXMRRefund(ctx, c.Tx, refund, x.feeAddress, c.User.ID); err != nil {
		return actionResult{}, err
	}
	if _, err = c.Tx.ExecContext(ctx, "UPDATE orders SET amount=$2 WHERE id=$1 AND state='draft'", o.ID, price); err != nil {
		return actionResult{}, err
	}
	if _, err = a.transition(withCheckoutPrice(context.WithValue(ctx, escrowCheckoutCtx{}, true)), c.Tx, o.ID, stateDraft, stateAwaitingPayment, c.User, "Monero escrow address requested"); err != nil {
		return actionResult{}, err
	}
	// The claim: a free slot of the active wallet, by compare-and-set; a slot another checkout holds is skipped. Only
	// once the watcher has reconciled that wallet in this process (PAY66-9): before, a restored database's free slot
	// may be an account the wallet already gave out.
	var slot int64
	var addr string
	err = c.Tx.QueryRowContext(ctx, `UPDATE xmr_escrow_slots SET state='claimed',order_id=$1,updated=now() WHERE id=(
		SELECT s.id FROM xmr_escrow_slots s JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE s.state='free' AND w.state='active' AND w.id=$2
		ORDER BY s.id LIMIT 1 FOR UPDATE OF s SKIP LOCKED) AND state='free' RETURNING id,address`, o.ID, x.reconciled.Load()).Scan(&slot, &addr)
	if err == sql.ErrNoRows {
		return actionResult{}, errXMREscrowNoSlot
	}
	if err != nil {
		return actionResult{}, err
	}
	feeBps, feeAddr := x.feeBps, x.feeAddress
	if feeBps == 0 {
		feeAddr = ""
	}
	_, err = c.Tx.ExecContext(ctx, `INSERT INTO escrows(order_id,coin,address,refund_address,fee_bps,fee_destination,release_days,dispatch_days,xmr_slot_id)
		VALUES($1,'XMR',$2,$3,$4,$5,NULLIF($6,0),NULLIF($7,0),$8)`, o.ID, addr, refund, feeBps, feeAddr, releaseDays, dispatchDays, slot)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "escrow_external_addresses_pkey" {
		return actionResult{}, errEscrowRefundUsed // another checkout or claim for this account reserved it first
	}
	if err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: orderPage(o.ID), Notice: "escrow-xmr-created", Audit: "Created Monero escrow (" + a.xmrEscrowNetwork() + ") for order " + shortID(o.ID)}, nil
}

// checkXMRRefund refuses a refund address this market already knows: an escrow or slot address, an escrow wallet's
// primary address, the fee address (configured or snapshotted), or an external
// destination this account already used. Other accounts' external history stays private.
func checkXMRRefund(ctx context.Context, q rowQuerier, refund, feeAddress, userID string) error {
	use, err := xmrAddressUse(ctx, q, refund, feeAddress, "", userID)
	switch use {
	case "escrow":
		return errXMRRefundEscrow
	case "wallet":
		return errXMRRefundWallet
	case "fee":
		return errXMRRefundFee
	case "used":
		return errEscrowRefundUsed
	}
	return err
}

// xmrAddressUse names how this market already knows the Monero address addr: "escrow" (an escrow or slot address),
// "wallet" (an escrow wallet's primary address), "fee" (the fee address feeAddress, or one an escrow snapshotted),
// "refund" (the refund address of the escrow of order, when order is given), "used"
// (an external refund/claim address userID already used), or "".
func xmrAddressUse(ctx context.Context, q rowQuerier, addr, feeAddress, order, userID string) (string, error) {
	var use string
	err := q.QueryRowContext(ctx, `SELECT CASE
		WHEN EXISTS(SELECT 1 FROM xmr_escrow_slots WHERE address=$1) OR EXISTS(SELECT 1 FROM escrows WHERE address=$1)
		 OR EXISTS(SELECT 1 FROM retention_escrow_retired_addresses WHERE coin='XMR' AND address_hash=encode(sha256(convert_to($1,'UTF8')),'hex')) THEN 'escrow'
		WHEN EXISTS(SELECT 1 FROM xmr_escrow_wallets WHERE primary_address=$1) THEN 'wallet'
		WHEN $1=$2 OR EXISTS(SELECT 1 FROM escrows WHERE fee_destination=$1) THEN 'fee'
		WHEN EXISTS(SELECT 1 FROM escrows WHERE order_id=$3 AND refund_address=$1) THEN 'refund'
		WHEN EXISTS(SELECT 1 FROM escrow_external_addresses WHERE user_id=$4 AND coin='XMR' AND address=$1) THEN 'used'
		ELSE '' END`, addr, feeAddress, order, userID).Scan(&use)
	return use, err
}
