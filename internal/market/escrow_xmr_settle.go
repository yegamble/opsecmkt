package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
)

// Monero escrow settlements (WR-escrow Phase 3, slice X-d; P3-R6, P3-R8; ES-7). The Monero escrow watcher alone builds,
// stores, relays and follows each xmr_settlements row, inside its pass over the wallet that holds the order's account
// (xmrWalletPass: the wallet is open, passed recheckEscrowXMRNetwork and its primary address is the row's). Nothing here
// runs in a form post. In a custodial wallet the market signs alone, so these steps are the whole of a settlement:
//
//  1. Build (requested -> built), never while the restore gate is set, only when the order's state fits the kind
//     (settlementFits, the rule escrow_market_may_sign, migration 081, enforces at store), and only while no other
//     settlement of the order is built and not relayed: get_address of the account's address 0 must be the escrow
//     address; incoming_transfers of the account; the outputs the settlement claims are those of its deposits (a
//     release or refund: every deposit that counts with the confirmation threshold and no settlement; a stray refund:
//     the deposits after close or left out; a re-request: exactly the stored key images of the settlement it
//     supersedes); every other output of the account is frozen and the claimed ones thawed; the claimed outputs must
//     all be unlocked and sum to the deposits claimed, or the build waits (and staff are told if it persists); the
//     plan is planXMRSettlement's (Bitcoin's planSettlement for the same kind) or, for a re-request, the stored plan;
//     transfer with do_not_relay takes the network fee from the claimant's output alone. The reply is checked
//     (checkXMRBuild): exactly the claimed key images, amounts_by_dest the plan less the fee on the claimant's output,
//     the fee within ESCROW_XMR_MAX_FEE and the claimant's output at least the Monero floor; otherwise it is discarded
//     unrelayed, the settlement stays requested and staff are told (a refund too few ends instead: xmrSettleTooSmall).
//     A refund the wallet refuses to build because its coins are at or below the network fee is too few as well
//     (xmrRefundBelowFee, A-467). Then, in one transaction, the outputs, the
//     transaction (hash, metadata, blob, fee, input total, key images) and the claim of each spent deposit
//     (payments.xmr_settlement_id, compare-and-set) are stored.
//  2. Relay (built -> relayed), only after the store, with the stored metadata, never while the restore gate is set, a
//     restore holds it or Monero relays are held in Tor mode (xmrRelayHold, A-515). A built settlement found on a later
//     pass (a relay that timed out after it took effect, or a restart mid-send) is looked up first: the daemon has it ->
//     relayed; otherwise the wallet is asked
//     (get_transfer_by_txid; only -8 means it does not know it) and the same stored metadata is relayed again. relay_tx
//     is idempotent and the metadata spends the same key images, so it cannot pay twice. transfer is never called for a
//     settlement that has a transaction. A refusal (-4) of a transaction the daemon does not have: a stored key image
//     spent -> external, at once; otherwise, since monero-wallet-rpc answers -4 for any failure to commit, a lost
//     connection to its daemon included, the same stored metadata is relayed again on the next passes, and only
//     xmrRelayAttempts refusals in a row -> failed (A-426). Both alert staff; an administrator may re-request a failed
//     one (escrow_xmr_rerequest.go, A-401).
//  3. Follow: relayed -> confirmed at the escrow's confirmation threshold, then until escrowReorgDepth (100)
//     confirmations (reorg_safe_at). A confirmed transaction a reorganisation took out of the chain goes back to
//     relayed, and the relayed step relays it again when the daemon lost it. A relayed transaction the daemon does not
//     have while a stored key image is spent was replaced by a transaction the market did not make: external.
//
// Stray refunds (P3-R8, ES-8): every wallet that may hold coins is read on every pass, so each closed order of the
// wallet with deposits after close or left out (with the threshold, unclaimed) gets a stray refund requested here, one
// in progress at a time. A destroyed wallet (slice X-f, P3-R9) is never opened, so its orders get none.
//
// Why a settlement waits, why it failed, or that a restore holds it, is its send_error (migration 085, as Bitcoin
// escrow's settlements.send_error; the order page says it); a class staff must see is flagged once, when it is first
// recorded. A restore holds a built or relayed settlement by writing restoredSettlementHold there (slice X-f, P3-R10):
// it is then followed but never relayed, until an administrator confirms its relay (sendRebroadcastOK,
// escrow_xmr_restore.go); the stored metadata is then relayed once, under the restore gate too, and the confirmation is
// used up (xmrMaySend).

// Wait classes: the fixed strings xmr_settlements.send_error records, shown on the order page.
const (
	xmrWaitConfirm     = "waiting-confirmations" // no deposit it spends has the confirmation threshold yet
	xmrWaitOutputs     = "outputs-waiting"       // the wallet's outputs do not match the deposits claimed yet (once)
	xmrOutputsMismatch = "outputs-mismatch"      // still so on a later pass, or the wallet found too little: flagged
	xmrAddressMismatch = "address-mismatch"      // get_address does not give the escrow address: flagged
	xmrFeeCap          = "fee-cap"               // the network fee is above ESCROW_XMR_MAX_FEE: flagged
	xmrTooSmall        = "too-small"             // the claimant's output would be below the Monero floor: flagged
	xmrBadBuild        = "build-mismatch"        // the wallet's transaction failed the market's checks: flagged
	xmrNothing         = "nothing-to-settle"     // no deposit is left to spend: failed
	xmrRelayRefused    = "relay-refused"         // the daemon refused the stored transaction xmrRelayAttempts times in a row: failed
	xmrBadMetadata     = "bad-metadata"          // relay_tx cannot parse the stored metadata (-27; only a corrupted row): failed
	// relay_tx refused the stored transaction (-4) while the daemon does not have it and no other transaction spent its
	// coins, recorded with the count of refusals in a row ("relay-refusing 1"): monero-wallet-rpc answers -4 for any
	// failure of commit_tx, a lost connection to its daemon included, so the same stored metadata is relayed again on
	// the next pass, and the xmrRelayAttempts-th refusal in a row is xmrRelayRefused. A relay that takes effect, or the
	// daemon found to have the transaction, ends the count; an error that is not recorded (a timeout) leaves it.
	xmrRelayRefusing = "relay-refusing"
	// transfer refused a destination (-2, WRONG_ADDRESS), with the role of the output validate_address names
	// (destination-refused-claimant, -market_fee or -buyer_return; no suffix when none is named): flagged, and not built
	// again for this settlement, except a refused fee address once ESCROW_XMR_FEE_ADDRESS names another one.
	xmrDestRefused = "destination-refused"
	// transfer refused the build with another wallet error, recorded with its code only ("wallet-refusing rpc:-4"):
	// waited on once, then flagged ("wallet-refused rpc:-4") when a later pass is refused the same way.
	xmrWalletRefusing = "wallet-refusing"
	xmrWalletRefused  = "wallet-refused"
	// the escrow wallet-rpc went away while it built this settlement (the connection ended after the whole request
	// was sent, xmrRPCError.Dropped: monero-wallet-rpc 0.18.5.1 crashes on some destinations): waited on once; if the
	// next transfer call for it drops too, it is set aside, flagged, and skipped until an administrator asks for it
	// again (/admin/escrow-xmr-retry-build), so one settlement cannot stop every other one of its wallet. A recorded
	// outcome in between (a build stored, another wait class) ends the count; an error that is not recorded (a refused
	// connection, a timeout) leaves it. Timeouts are not counted: a wallet-rpc busy with one build makes the next
	// settlements' calls time out too.
	xmrBuildDropped  = "build-dropped"
	xmrBuildSetAside = "build-set-aside"
	// the wallet refused to build a refund or stray refund with a code that may mean its coins are too few for the
	// network fee, and the fee estimate that decides it failed (xmrRefundBelowFee, QA5W1-4): waited on once, then
	// flagged when a later pass fails the same way. It is never classed too few without an estimate, and the wallet's
	// pass goes on with its other orders. A failed estimate and a refusal count as one run of passes without a build
	// (xmrFeeEpisode, QA5W2-1).
	xmrFeeEstimateWaiting     = "fee-estimate-waiting"
	xmrFeeEstimateUnavailable = "fee-estimate-unavailable"
)

// xmrRelayAttempts is the number of passes in a row on which relay_tx may refuse a stored transaction before it is
// recorded failed (xmrRelayRefusing).
const xmrRelayAttempts = 3

// xmrWaitFlagged reports whether staff are told of class.
func xmrWaitFlagged(class string) bool {
	switch class {
	case xmrOutputsMismatch, xmrAddressMismatch, xmrFeeCap, xmrTooSmall, xmrBadBuild, xmrBuildSetAside, xmrFeeEstimateUnavailable:
		return true
	}
	return strings.HasPrefix(class, xmrDestRefused) || strings.HasPrefix(class, xmrWalletRefused+" ")
}

// xmrFeeEpisode reports whether class records a pass of a refund or stray refund on which transfer built nothing that
// counts with a failed fee estimate (QA5W2-1): the estimate's own classes, a refusal xmrBuildRefused or xmrOutputsWait
// counts (wallet-refusing or -refused, outputs-waiting or -mismatch) and coins too few (too-small, which a stray refund
// waits with). An estimate that fails only on some passes would otherwise restart the refusal's count, and the refusal
// the estimate's, so the refund would wait unflagged for good: the second pass in a row in these classes is flagged,
// as whichever class it records. Staff are told of each of its flagged classes once per run (xmrFlaggedBefore).
func xmrFeeEpisode(class string) bool {
	switch class {
	case xmrFeeEstimateWaiting, xmrFeeEstimateUnavailable, xmrWaitOutputs, xmrOutputsMismatch, xmrTooSmall:
		return true
	}
	return strings.HasPrefix(class, xmrWalletRefusing+" ") || strings.HasPrefix(class, xmrWalletRefused+" ")
}

// xmrFeeEstimateWait reports whether class records a failed fee estimate.
func xmrFeeEstimateWait(class string) bool {
	return class == xmrFeeEstimateWaiting || class == xmrFeeEstimateUnavailable
}

// xmrFlaggedBefore reports whether staff were told already of the flagged class, an xmrFeeEpisode class, in the run
// of passes without a build of requested settlement s (QA5W2-1, QA5W3B-3/-4). A requested settlement only moves
// forward (xmr_settlements_moves), so its run is its whole time requested: the run ends with a build, or when it
// leaves its waiting state. Staff are told once of each distinct flagged class of the run: a refusal flagged, then
// the estimate failing on alternate passes, flips between two classes each told once; a class staff were told of
// recurring after a wait never flagged (-37's outputs-waiting, a first failed estimate) is told nothing new; but a
// different diagnosis (the wallet seeing fewer coins than it listed, or refusing, once the estimate answers; a stray
// refund's coins found too few) is told, once. What was told is read from s's own flag notes (xmrSettleWaits writes
// one per announcement, in tx, which holds s's row lock from its compare-and-set), so it survives a restart. Other
// flagged classes are told whenever they are recorded anew, as before (an administrator's retry of a set-aside
// build, for one, must be told again if it is set aside again).
func (a *App) xmrFlaggedBefore(ctx context.Context, tx *sql.Tx, s *xmrSettleRow, class string) (bool, error) {
	if !xmrWaitFlagged(class) || !xmrFeeEpisode(class) {
		return false, nil
	}
	var told bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND strpos(note,$2)=1)",
		s.OrderID, xmrFlagNotePrefix(s, class, a.escrowXMR.confirmations)).Scan(&told)
	return told, err
}

// xmrFlagNotePrefix is how the order note xmrSettleWaits writes to flag class on settlement s begins.
func xmrFlagNotePrefix(s *xmrSettleRow, class string, threshold int64) string {
	return fmt.Sprintf("Monero escrow settlement %d (%s) not sent: %s", s.ID, settlementKind(s.Kind), xmrSettleWaitText(class, s.Kind, threshold))
}

// xmrSettleWaitText is what the order page says about the wait class of a settlement of kind.
func xmrSettleWaitText(class, kind string, threshold int64) string {
	switch {
	case class == xmrWaitConfirm:
		return "Waiting until a deposit to this escrow has " + strconv.FormatInt(threshold, 10) + " confirmations."
	case class == xmrWaitOutputs:
		return "The escrow wallet does not show this escrow's deposits as spendable yet; the market tries again on its next pass."
	case class == xmrOutputsMismatch:
		return "The escrow wallet's coins at this order's account do not match the deposits recorded for it, so the market builds nothing. It tries again on each pass; market staff were notified."
	case class == xmrAddressMismatch:
		return "The escrow wallet does not give this order's escrow address for its account, so the market builds nothing. Market staff were notified."
	case class == xmrFeeCap:
		return "The network fee would exceed the market's fee cap, so the transaction was discarded before it was sent. The market tries again on each pass; market staff were notified."
	case class == xmrTooSmall:
		return "The coins in this escrow are too few to pay the network fee and an output of at least " + amount(minPayoutXMR, 12) + " XMR, so the market cannot send this settlement. They stay in the escrow; market staff were notified."
	case class == xmrBadBuild:
		return "The escrow wallet returned a transaction that failed the market's checks, so it was discarded before it was sent. The market tries again on each pass; market staff were notified."
	case class == xmrNothing:
		return "No confirmed deposit of this escrow is left to spend, so there is nothing to settle."
	case class == xmrRelayRefused:
		return "The Monero daemon refused the stored transaction and no other transaction spent its coins, so nothing was sent. Market staff were notified; an administrator may ask the market to build it again."
	case class == xmrBadMetadata:
		return "The escrow wallet cannot read the stored transaction (its record is damaged), so nothing was sent. Market staff were notified; an administrator may ask the market to build it again."
	case strings.HasPrefix(class, xmrRelayRefusing+" "):
		refusals, confirmed := xmrRelayRefusals(class)
		text := "The Monero daemon refused this transaction (" + strconv.Itoa(refusals) + " time(s) in a row); no other transaction spent its coins. The market sends the same stored transaction again on its next pass, and records it as not sent if it is refused " + strconv.Itoa(xmrRelayAttempts) + " times in a row."
		if confirmed {
			text += " An administrator confirmed its relay after the market restored its records from a backup, so it is sent while the market reconciles them too."
		}
		return text
	case class == xmrDestRefused+"-claimant" && kind == "release":
		return "The escrow wallet refuses the vendor's claim address as invalid, so the market cannot send this settlement. Market staff were notified and contact the vendor."
	case class == xmrDestRefused+"-claimant" || class == xmrDestRefused+"-buyer_return":
		return "The escrow wallet refuses the buyer's refund address given at checkout as invalid, so the market cannot send this settlement. Market staff were notified and contact the buyer."
	case class == xmrDestRefused+"-market_fee":
		return "The escrow wallet refuses the market fee address as invalid, so the market cannot send this settlement. Market staff were notified; it is built again once the operator has corrected ESCROW_XMR_FEE_ADDRESS."
	case class == xmrDestRefused:
		return "The escrow wallet refuses one of this settlement's destination addresses as invalid, so the market cannot send it. Market staff were notified."
	case strings.HasPrefix(class, xmrWalletRefusing+" "):
		return "The escrow wallet refused to build this transaction (" + strings.TrimPrefix(class, xmrWalletRefusing+" ") + "); the market tries again on its next pass."
	case strings.HasPrefix(class, xmrWalletRefused+" "):
		return "The escrow wallet keeps refusing to build this transaction (" + strings.TrimPrefix(class, xmrWalletRefused+" ") + "). The market tries again on each pass; market staff were notified."
	case class == xmrBuildDropped:
		return "The escrow wallet stopped responding while it built this transaction; the market tries again on its next pass."
	case class == xmrBuildSetAside:
		return "Building this settlement made the escrow wallet stop responding; it is set aside until an administrator asks the market to build it again, so the market goes on with the other settlements. Market staff were notified."
	case class == xmrFeeEstimateWaiting:
		return "The escrow wallet refused to build this transaction, and the market could not get the network fee estimate it needs to tell whether the coins are too few for the fee; the market tries again on its next pass."
	case class == xmrFeeEstimateUnavailable:
		return "The escrow wallet keeps refusing to build this transaction, and the market still cannot get the network fee estimate it needs to tell whether the coins are too few for the fee. The market tries again on each pass; market staff were notified."
	case restoreHeld(class):
		return restoreHeldText
	}
	return ""
}

// xmrStrayEligibleSQL selects the payments rows p a Monero stray refund returns (strayEligibleSQL's rule), for a query
// with the threshold as $2.
const xmrStrayEligibleSQL = "p.xmr_settlement_id IS NULL AND NOT p.locked AND NOT p.spent_outside AND p.confirmations>=$2 AND (p.after_close OR p.escrow_left_out)"

// xmrSettleRow is a Monero settlement as the watcher reads it, with its escrow and order.
type xmrSettleRow struct {
	ID                          int64
	OrderID, Kind, State, Dest  string
	Supersedes                  int64 // the failed settlement this one re-requests; 0 for none
	Account                     int64
	EscrowAddr, Refund, FeeDest string
	FeeBps, Price               int64
	OrderState, Outcome         string
	BuyerID                     string
	TxHash, Metadata, Wait      string // Wait: its send_error
	KeyImages                   []string
}

// xmrSettleWallet runs the settlement steps for every settlement of the open, checked wallet w (see the file comment):
// stray refunds are requested, administrators' re-requests answered, and each settlement built, relayed or followed.
func (a *App) xmrSettleWallet(ctx context.Context, w xmrWalletRow) error {
	gate, err := restoreGateSet(ctx, a.db)
	if err != nil {
		return err
	}
	var errs []error
	if err = a.xmrRequestStrays(ctx, w); err != nil {
		errs = append(errs, err)
	}
	if !gate {
		if err = a.xmrReRequests(ctx, w); err != nil {
			errs = append(errs, err)
		}
	}
	rows, err := a.xmrSettlements(ctx, w.id)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, s := range rows {
		if ctx.Err() != nil {
			break
		}
		var err error
		switch s.State {
		case "requested":
			if !gate {
				err = a.buildXMRSettlement(ctx, s)
			}
		case "built":
			err = a.sendXMRBuilt(ctx, s, gate)
		case "relayed":
			err = a.trackXMRRelayed(ctx, s, gate)
		case "confirmed":
			err = a.trackXMRConfirmed(ctx, s)
		}
		if err != nil {
			errs = append(errs, &watchError{label: "monero escrow settlement " + strconv.FormatInt(s.ID, 10), class: s.State + ": " + xmrErrorClass(err), err: err})
		}
	}
	return errors.Join(errs...)
}

// xmrSettlements reads the settlements of wallet walletID that the watcher still works on.
func (a *App) xmrSettlements(ctx context.Context, walletID int64) ([]*xmrSettleRow, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT s.id,s.order_id,s.kind,s.state,s.destination,COALESCE(s.supersedes,0),sl.account_index,es.address,es.refund_address,
		es.fee_destination,es.fee_bps,o.amount,o.state,`+disputeOutcomeSQL+`,o.buyer_id,COALESCE(s.tx_hash,''),COALESCE(s.tx_metadata,''),
		COALESCE(array_to_string(s.key_images,','),''),s.send_error
		FROM xmr_settlements s JOIN escrows es ON es.order_id=s.order_id AND es.coin='XMR' JOIN xmr_escrow_slots sl ON sl.id=es.xmr_slot_id JOIN orders o ON o.id=s.order_id
		WHERE sl.wallet_id=$1 AND (s.state IN ('requested','built','relayed') OR s.state='confirmed' AND s.reorg_safe_at IS NULL) ORDER BY s.id`, walletID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*xmrSettleRow
	for rows.Next() {
		s := &xmrSettleRow{}
		var keyImages string
		if err = rows.Scan(&s.ID, &s.OrderID, &s.Kind, &s.State, &s.Dest, &s.Supersedes, &s.Account, &s.EscrowAddr, &s.Refund, &s.FeeDest, &s.FeeBps, &s.Price,
			&s.OrderState, &s.Outcome, &s.BuyerID, &s.TxHash, &s.Metadata, &keyImages, &s.Wait); err != nil {
			return nil, err
		}
		if keyImages != "" {
			s.KeyImages = strings.Split(keyImages, ",")
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// xmrDeposit is one payments row a settlement spends: a transaction's outputs to the order's account.
type xmrDeposit struct {
	ID     int64
	TxID   string
	Amount int64
}

// xmrDeposits lists the deposits settlement s claims, and whether a deposit it would claim is still confirming. A
// release or refund claims every deposit that counts for the order (not locked, recorded before the order closed, not
// left out) with the confirmation threshold and no settlement; a stray refund those recorded after close or left out;
// a re-request those the settlement it supersedes claimed, moved to it when it was requested.
func (a *App) xmrDeposits(ctx context.Context, s *xmrSettleRow) ([]xmrDeposit, bool, error) {
	which := "p.xmr_settlement_id IS NULL AND NOT p.locked AND NOT p.after_close AND NOT p.escrow_left_out AND NOT p.spent_outside AND p.confirmations>=0"
	switch {
	case s.Supersedes != 0:
		which = "p.xmr_settlement_id=" + strconv.FormatInt(s.ID, 10) + " AND p.confirmations>=0"
	case s.Kind == "stray_refund":
		which = "p.xmr_settlement_id IS NULL AND NOT p.locked AND NOT p.spent_outside AND p.confirmations>=0 AND (p.after_close OR p.escrow_left_out)"
	}
	rows, err := a.db.QueryContext(ctx, "SELECT p.id,p.txid,p.amount,p.confirmations>=$2 FROM payments p WHERE p.order_id=$1 AND "+which+" ORDER BY p.created,p.id", s.OrderID, a.escrowXMR.confirmations)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []xmrDeposit
	pending := false
	for rows.Next() {
		var d xmrDeposit
		var confirmed bool
		if err = rows.Scan(&d.ID, &d.TxID, &d.Amount, &confirmed); err != nil {
			return nil, false, err
		}
		if confirmed || s.Supersedes != 0 {
			out = append(out, d)
		} else {
			pending = true
		}
	}
	return out, pending, rows.Err()
}

// xmrStoredPlan is the stored transaction and plan of a settlement: what its re-request spends and pays.
type xmrStoredPlan struct {
	KeyImages  []string
	InputTotal int64
	Outputs    []settleOutput // in order, the claimant's first, after the fee
}

func (a *App) xmrStoredPlan(ctx context.Context, id int64) (*xmrStoredPlan, error) {
	p := &xmrStoredPlan{}
	var keyImages string
	if err := a.db.QueryRowContext(ctx, "SELECT array_to_string(key_images,','),input_total FROM xmr_settlements WHERE id=$1 AND tx_hash IS NOT NULL", id).Scan(&keyImages, &p.InputTotal); err != nil {
		return nil, err
	}
	p.KeyImages = strings.Split(keyImages, ",")
	rows, err := a.db.QueryContext(ctx, "SELECT role,address,amount FROM xmr_settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var o settleOutput
		if err = rows.Scan(&o.Role, &o.Address, &o.Sats); err != nil {
			return nil, err
		}
		p.Outputs = append(p.Outputs, o)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(p.Outputs) == 0 || p.Outputs[0].Role != "claimant" {
		return nil, errors.New("monero escrow: a stored settlement has no claimant output")
	}
	return p, nil
}

// plan is the stored plan again: the same destinations and amounts, the claimant's output before the fee (the input
// total less every other output), so only the network fee, and with it the claimant's output, may differ (A-401).
func (p *xmrStoredPlan) plan() settlePlan {
	out := slices.Clone(p.Outputs)
	out[0].Sats = p.InputTotal
	for _, o := range out[1:] {
		out[0].Sats -= o.Sats
	}
	return settlePlan{Outputs: out}
}

// planXMRSettlement is the plan of a Monero settlement of kind spending outputs worth total (piconero), the outputs
// planSettlement gives the same kind: a release pays the vendor's destination the escrow less the market fee
// (snapshotted at checkout, never reduced) and less any overpayment above price, which returns to the buyer's refund
// address; a refund or stray refund pays the buyer's refund address everything. A market fee or return below floor is
// added to the claimant's output instead, with a note. The wallet takes the network fee from the claimant's output
// alone (subtract_fee_from_outputs), so the claimant's output here is before the fee.
func planXMRSettlement(kind string, total, price, feeBps int64, feeDest, claimant, refund string, floor int64) settlePlan {
	var p settlePlan
	var extra []settleOutput
	if kind == "release" {
		if fee := marketFeeSats(price, feeBps); fee > 0 && feeDest != "" {
			if fee >= floor {
				extra = append(extra, settleOutput{"market_fee", feeDest, fee})
			} else {
				p.Folded = append(p.Folded, "The market fee of "+amount(fee, 12)+" XMR is below the minimum output of "+amount(floor, 12)+" XMR, so it was added to the vendor's output.")
			}
		}
		if excess := total - price; excess > 0 {
			if excess >= floor {
				extra = append(extra, settleOutput{"buyer_return", refund, excess})
			} else {
				p.Folded = append(p.Folded, "The overpayment of "+amount(excess, 12)+" XMR is below the minimum output of "+amount(floor, 12)+" XMR, so it was added to the vendor's output.")
			}
		}
	}
	left := total
	for _, o := range extra {
		left -= o.Sats
	}
	p.Outputs = append([]settleOutput{{"claimant", claimant, left}}, extra...)
	return p
}

// checkXMRBuild checks the transaction the wallet built for plan from the outputs with keyImages: it spends exactly
// those key images, amounts_by_dest is the plan with the network fee taken from the claimant's output alone (every
// other output exact), the fee is at most maxFee and the claimant's output at least floor. It returns "" or the wait
// class of the first check that fails.
func checkXMRBuild(tx xmrBuiltTx, plan []settleOutput, keyImages []string, maxFee, floor int64) string {
	if !slices.Equal(slices.Sorted(slices.Values(tx.SpentKeyImages)), slices.Sorted(slices.Values(keyImages))) || len(tx.AmountsByDest) != len(plan) {
		return xmrBadBuild
	}
	var sum int64
	for i, o := range plan {
		want := o.Sats
		if i == 0 {
			want -= tx.Fee
		}
		if tx.AmountsByDest[i] != want {
			return xmrBadBuild
		}
		sum += tx.AmountsByDest[i]
	}
	switch {
	case tx.Amount != sum:
		return xmrBadBuild
	case tx.Fee > maxFee:
		return xmrFeeCap
	case tx.AmountsByDest[0] < floor:
		return xmrTooSmall
	}
	return ""
}

// buildXMRSettlement builds one requested settlement (step 1 of the file comment). The RPCs run outside any
// transaction; storeXMRBuilt stores the result by compare-and-set and relayXMRStored relays it.
func (a *App) buildXMRSettlement(ctx context.Context, s *xmrSettleRow) error {
	x := a.escrowXMR
	if !settlementFits(s.Kind, s.OrderState, s.Outcome) {
		return nil // a release requested at shipment waits for the order's outcome: the market never signs before it (081)
	}
	if s.Wait == xmrBuildSetAside {
		return nil // until an administrator asks for it again (xmrRetryBuildAction)
	}
	feeReplaced := false
	if strings.HasPrefix(s.Wait, xmrDestRefused) {
		// A refused destination is never tried again as if the refusal were transient: the claimant's and the buyer's
		// addresses never change for this settlement. A refused fee address is replaced by ESCROW_XMR_FEE_ADDRESS once
		// the operator has set another one that the wallet accepts.
		if s.Wait != xmrDestRefused+"-market_fee" || x.feeAddress == "" || x.feeAddress == s.FeeDest {
			return nil
		}
		if valid, err := x.rpc.validAddress(ctx, x.feeAddress); err != nil || !valid {
			return err
		}
		s.FeeDest, feeReplaced = x.feeAddress, true
	}
	var busy bool
	if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM xmr_settlements WHERE order_id=$1 AND state='built')", s.OrderID).Scan(&busy); err != nil || busy {
		return err // relayed first: two unrelayed transactions of one account could spend the same outputs
	}
	deposits, pending, err := a.xmrDeposits(ctx, s)
	if err != nil {
		return err
	}
	var stored *xmrStoredPlan
	if s.Supersedes != 0 {
		if stored, err = a.xmrStoredPlan(ctx, s.Supersedes); err != nil {
			return err
		}
	}
	switch {
	case len(deposits) == 0 && pending:
		return a.xmrSettleWaits(ctx, s, xmrWaitConfirm)
	case len(deposits) == 0:
		return a.failXMRSettlement(ctx, s, xmrNothing)
	}
	addr, err := x.rpc.address(ctx, s.Account, 0)
	if err != nil {
		return err
	}
	if addr != s.EscrowAddr {
		return a.xmrSettleWaits(ctx, s, xmrAddressMismatch)
	}
	outs, err := x.rpc.incomingTransfers(ctx, s.Account)
	if err != nil {
		return err
	}
	txids := map[string]bool{}
	var want int64
	for _, d := range deposits {
		txids[d.TxID], want = true, want+d.Amount
	}
	if stored != nil {
		want = stored.InputTotal
	}
	// The claimed outputs are thawed and every other output of the account frozen, so the wallet can spend nothing else.
	var keyImages []string
	var total int64
	unlocked := true
	for _, o := range outs {
		claim := txids[o.TxHash]
		if stored != nil {
			claim = slices.Contains(stored.KeyImages, o.KeyImage)
		}
		switch {
		case claim && o.Frozen:
			err = x.rpc.thaw(ctx, o.KeyImage)
		case !claim && !o.Frozen:
			err = x.rpc.freeze(ctx, o.KeyImage)
		}
		if err != nil {
			return err
		}
		if claim {
			keyImages, total, unlocked = append(keyImages, o.KeyImage), total+o.Amount, unlocked && o.Unlocked
		}
	}
	if !unlocked || total != want || stored != nil && len(keyImages) != len(stored.KeyImages) {
		return a.xmrOutputsWait(ctx, s)
	}
	floor := payoutFloor("XMR", s.Kind)
	plan := planXMRSettlement(s.Kind, total, s.Price, s.FeeBps, s.FeeDest, s.Dest, s.Refund, floor)
	if stored != nil {
		plan = stored.plan()
	}
	if plan.Outputs[0].Sats < floor {
		return a.xmrSettleTooSmall(ctx, s, deposits, pending)
	}
	dests := make([]xmrDestination, len(plan.Outputs))
	for i, o := range plan.Outputs {
		dests[i] = xmrDestination{Address: o.Address, Amount: o.Sats}
	}
	tx, err := x.rpc.buildTransfer(ctx, s.Account, dests)
	if err != nil {
		few, ferr := a.xmrRefundBelowFee(ctx, s, plan, keyImages, floor, err)
		if ferr != nil {
			return a.xmrFeeEstimateFailed(ctx, s, ferr)
		}
		if few {
			if s.Wait != xmrTooSmall { // once per settlement and class, not on each pass a stray refund waits (QA5W1-6)
				log.Printf("escrow: Monero escrow settlement of order %s: the wallet refused to build it (%s) and its coins are too few for the network fee and the floor: %s", shortID(s.OrderID), xmrErrorClass(err), xmrTooSmall)
			}
			return a.xmrSettleTooSmall(ctx, s, deposits, pending)
		}
		return a.xmrBuildRefused(ctx, s, plan, err)
	}
	if class := checkXMRBuild(tx, plan.Outputs, keyImages, x.maxFee, floor); class != "" {
		log.Printf("escrow: Monero escrow settlement of order %s: the wallet's transaction was discarded unrelayed: %s", shortID(s.OrderID), class)
		if class == xmrTooSmall {
			return a.xmrSettleTooSmall(ctx, s, deposits, pending)
		}
		return a.xmrSettleWaits(ctx, s, class)
	}
	if feeReplaced {
		plan.Folded = append(plan.Folded, "The escrow wallet had refused the market fee address recorded when this escrow was created, so the market fee was paid to the fee address the operator configured since.")
	}
	ok, err := a.storeXMRBuilt(ctx, s, tx, plan, deposits, total)
	if err != nil || !ok {
		return err
	}
	x.belowFee.Delete(s.ID) // built at last (a later deposit): its too-few refusal is not kept
	s.State, s.TxHash, s.Metadata, s.KeyImages, s.Wait = "built", tx.TxHash, tx.Metadata, keyImages, ""
	return a.relayXMRStored(ctx, s)
}

// xmrSettleTooSmall handles settlement s, which claims deposits, whose coins are too few to pay the network fee and
// leave the claimant the Monero floor (the plan, or the wallet's transaction after its fee). A release (the vendor's
// coins), a stray refund (which a later deposit may let build, A-435), a re-request (which spends its stored key
// images) and a refund while a deposit it would spend still confirms wait with the too-small class, as before. A refund
// otherwise ends (A-437): waiting, it held its deposits for good while a stray refund of later deposits waited or paid
// without them, so coins that together could be refunded stayed in the escrow account (xmrRefundTooFew).
func (a *App) xmrSettleTooSmall(ctx context.Context, s *xmrSettleRow, deposits []xmrDeposit, pending bool) error {
	if s.Kind != "refund" || s.Supersedes != 0 || pending {
		return a.xmrSettleWaits(ctx, s, xmrTooSmall)
	}
	return a.xmrRefundTooFew(ctx, s, deposits)
}

// xmrRefundTooFew ends refund s, whose deposits are too few (requested -> failed, too-small; xmr_settlements_moves
// allows it, and a failed row with no transaction is never re-requested), and leaves those deposits out and flags them
// for the stray refund, in one transaction under the order row lock: one order note (staff told), and a notice to both
// parties, as a Bitcoin refund's failure and leaveOutTooFew do. The stray refund (requested on the next pass, or
// already waiting) returns them with any later deposit, and any deposit that did not count and is mined later
// (xmrLeaveOutUncounted), once together they pay the fee; it waits rather than fails meanwhile, so nothing repeats.
func (a *App) xmrRefundTooFew(ctx context.Context, s *xmrSettleRow, deposits []xmrDeposit) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", s.OrderID))
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "UPDATE xmr_settlements SET state='failed',send_error=$2,updated=now() WHERE id=$1 AND state='requested'", s.ID, xmrTooSmall)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	ids := make([]int64, len(deposits))
	for i, d := range deposits {
		ids[i] = d.ID
	}
	rows, err := tx.QueryContext(ctx, `UPDATE payments SET escrow_left_out=true,`+newFlagSQL+`,updated=now() WHERE order_id=$1 AND id=ANY($2) AND xmr_settlement_id IS NULL
		AND NOT after_close AND NOT escrow_left_out AND NOT spent_outside RETURNING txid,amount`, s.OrderID, ids)
	if err != nil {
		return err
	}
	// The note names each deposit as flagLateDeposits does, so the desk dates its flag from it (QA462D-4).
	var n, sum int64
	var named []string
	for rows.Next() {
		var txid string
		var amt int64
		if err = rows.Scan(&txid, &amt); err != nil {
			rows.Close()
			return err
		}
		n, sum, named = n+1, sum+amt, append(named, "deposit "+truncate(txid, 20)+" ("+amount(amt, 12)+" XMR)")
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	slices.Sort(named)
	refund := "its refund"
	if o.State == stateResolved {
		refund = "the refund its dispute decision requested"
	}
	note := fmt.Sprintf("%d deposit(s) (%s XMR, %s %s) to this order's Monero escrow are not spent by %s: the coins were too few to pay the network fee and an output of at least %s XMR, so the market could not send the refund, which ends. "+
		"They stay in the escrow account; a stray refund returns them, together with any later deposit to this escrow, less the network fee, to the buyer's refund address given at checkout once together they can pay the network fee.",
		n, amount(sum, 12), a.netTag(), a.xmrEscrowNetwork(), refund, amount(minPayoutXMR, 12))
	if len(named) > 0 {
		note += " The deposits: " + strings.Join(named, ", ") + "."
	}
	// Named and marked for review, so the desk dates the flags from the order note (QA462D-4); the parties are told
	// the same without the request to staff.
	if err = a.flagOrder(ctx, tx, &o, note+" Moderator review required.", "Order "+shortID(s.OrderID)+": "+note); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: Monero escrow settlement of order %s failed: %s; its deposits are left out for a stray refund", shortID(s.OrderID), xmrTooSmall)
	return nil
}

// xmrRefundBelowFee reports whether transfer's refusal err of refund or stray refund s's plan, which spends the outputs
// keyImages, is because the coins are too few for the network fee (A-467). monero-wallet-rpc 0.18.5.1, asked to take
// the fee from the claimant's output, refuses coins at or below the fee before it builds anything: -17 (not enough
// money: below the fee of a one-input transaction by its size), -16 (tx not possible: the claimant's output below the
// fee) or -46 (destination amount is zero: equal to it), so checkXMRBuild never sees them; on the fakechain it answered
// -17 at 0.001 and 0.0015 XMR, -16 at 0.0018 XMR and -46 at exactly the fee (A-463 probe; recorded in
// xmr_rpc_shapes_0.18.5.1.json, QA5W1-5). Those codes also mean other things (coins the wallet no longer sees as unlocked or
// confirmed, a wallet behind the chain), so they are not trusted alone: the claimant's output less the fee the wallet
// would take (estimateFee) is compared with the floor, as checkXMRBuild compares a build's. Below it the coins are too
// few whatever the wallet's reason; at or above it the refusal keeps its class (xmrBuildRefused). A fee estimate that
// fails is returned, and xmrFeeEstimateFailed records it on s alone. Releases are not compared: their coins are the
// vendor's, and their refusals keep their classes.
//
// A settlement found too few this way that waits too-small (a stray refund) is not estimated again while the wallet
// refuses the same outputs and claimant amount with the same code (QA5W1-6): the fee would be read on every pass for
// nothing. transfer is still asked on each pass, so a later deposit that lets it build is seen, and new outputs are
// estimated again. The record is in memory only: after a restart the first refusal is estimated again.
func (a *App) xmrRefundBelowFee(ctx context.Context, s *xmrSettleRow, plan settlePlan, keyImages []string, floor int64, err error) (bool, error) {
	var re *xmrRPCError
	if s.Kind != "refund" && s.Kind != "stray_refund" || !errors.As(err, &re) || re.Service != "wallet" || !strings.HasPrefix(re.Class, "rpc:") ||
		re.Code != -16 && re.Code != -17 && re.Code != -46 {
		return false, nil
	}
	x := a.escrowXMR
	seen := fmt.Sprintf("%s %d %d", strings.Join(slices.Sorted(slices.Values(keyImages)), ","), plan.Outputs[0].Sats, re.Code)
	if prev, ok := x.belowFee.Load(s.ID); ok && prev == seen && s.Wait == xmrTooSmall {
		return true, nil
	}
	fee, err := x.rpc.estimateFee(ctx, len(keyImages), len(plan.Outputs)+1)
	if err != nil {
		return false, err
	}
	few := plan.Outputs[0].Sats-fee < floor
	if few {
		x.belowFee.Store(s.ID, seen)
	} else {
		x.belowFee.Delete(s.ID)
	}
	return few, nil
}

// xmrFeeEstimateFailed records that the fee estimate xmrRefundBelowFee needed for settlement s failed with err
// (QA5W1-4), on s alone: xmrFeeEstimateWaiting the first time, xmrFeeEstimateUnavailable (flagged once) when the
// previous pass recorded a class of the same run (xmrFeeEpisode, QA5W2-1: the estimate failed, the wallet refused, or
// the coins were found too few, as a stray refund flagged too small is after a restart). The wallet's pass goes on
// with its other orders (their payment windows, the wallet's retention) and s is tried again on the next pass. Only a
// cancelled pass and a wallet-rpc that lost its open wallet (-13; the next pass opens it again) are returned, as
// xmrBuildRefused returns them.
func (a *App) xmrFeeEstimateFailed(ctx context.Context, s *xmrSettleRow, err error) error {
	if ctx.Err() != nil || xmrNotOpen(err) {
		return err
	}
	class := xmrFeeEstimateWaiting
	if xmrFeeEpisode(s.Wait) {
		class = xmrFeeEstimateUnavailable
	}
	if s.Wait != class {
		log.Printf("escrow: Monero escrow settlement of order %s: the wallet refused to build it and the network fee estimate failed (%s)", shortID(s.OrderID), xmrErrorClass(err))
	}
	return a.xmrSettleWaits(ctx, s, class)
}

// xmrBuildRefused handles transfer's error for settlement s's plan (monero-wallet-rpc 0.18.5.1's codes) that
// xmrRefundBelowFee did not find too few. -17 (not
// enough money: the wallet sees fewer spendable coins than it listed) is an outputs mismatch; -37 (not enough unlocked
// money: they are not spendable yet, as after a reorganisation since incoming_transfers) a wait, never flagged; -2
// (WRONG_ADDRESS) a definite refusal of a destination, named by asking validate_address about each output in turn; any
// other answer of the wallet a refusal recorded with its code alone, flagged when it persists (or follows a failed fee
// estimate, QA5W2-1). A connection that ended after the whole request was sent (the wallet-rpc went away while it
// built) is xmrBuildDropped, then xmrBuildSetAside. Any other transport error (refused, timeout, credentials, a reply that is not one) is returned and
// retried on the next pass, unflagged and not counted.
func (a *App) xmrBuildRefused(ctx context.Context, s *xmrSettleRow, plan settlePlan, err error) error {
	var re *xmrRPCError
	if errors.As(err, &re) && re.Service == "wallet" && re.Dropped {
		if s.Wait == xmrBuildDropped {
			return a.xmrSettleWaits(ctx, s, xmrBuildSetAside)
		}
		return a.xmrSettleWaits(ctx, s, xmrBuildDropped)
	}
	if !errors.As(err, &re) || re.Service != "wallet" || !strings.HasPrefix(re.Class, "rpc:") || re.Code == -13 {
		return err // -13: the wallet-rpc lost its open wallet; the next pass opens it again
	}
	switch re.Code {
	case -17:
		return a.xmrOutputsWait(ctx, s)
	case -37:
		return a.xmrSettleWaits(ctx, s, xmrWaitOutputs)
	case -2:
		class := xmrDestRefused
		for _, o := range plan.Outputs {
			valid, err := a.escrowXMR.rpc.validAddress(ctx, o.Address)
			if err != nil {
				return err
			}
			if !valid {
				class += "-" + o.Role
				break
			}
		}
		return a.xmrSettleWaits(ctx, s, class)
	}
	code := re.Class
	if s.Wait == xmrWalletRefusing+" "+code || s.Wait == xmrWalletRefused+" "+code || xmrFeeEstimateWait(s.Wait) {
		return a.xmrSettleWaits(ctx, s, xmrWalletRefused+" "+code)
	}
	return a.xmrSettleWaits(ctx, s, xmrWalletRefusing+" "+code)
}

// xmrOutputsWait records that the wallet's outputs do not match the deposits claimed: the first time it waits
// (xmrWaitOutputs; the wallet may not have caught up), and from the next pass it is flagged (xmrOutputsMismatch), as
// it is at once after a pass whose fee estimate failed (QA5W2-1).
func (a *App) xmrOutputsWait(ctx context.Context, s *xmrSettleRow) error {
	if s.Wait == xmrWaitOutputs || s.Wait == xmrOutputsMismatch || xmrFeeEstimateWait(s.Wait) {
		return a.xmrSettleWaits(ctx, s, xmrOutputsMismatch)
	}
	return a.xmrSettleWaits(ctx, s, xmrWaitOutputs)
}

// xmrSettleWaits records why settlement s is not built or sent yet; it stays where it is and is tried again next
// pass. A changed class is logged once, and a class in xmrWaitFlagged is flagged to staff once, with a fixed note,
// unless staff were told of it already in this run of passes without a build (xmrFlaggedBefore, QA5W2-1, QA5W3B).
func (a *App) xmrSettleWaits(ctx context.Context, s *xmrSettleRow, class string) error {
	if s.Wait == class {
		return nil
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE xmr_settlements SET send_error=$2,updated=now() WHERE id=$1 AND send_error<>$2", s.ID, class)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil // recorded already (s was read before)
	}
	told, err := a.xmrFlaggedBefore(ctx, tx, s, class)
	if err != nil {
		return err
	}
	if xmrWaitFlagged(class) && !told {
		o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", s.OrderID))
		if err != nil {
			return err
		}
		// The deposits it would spend are flagged so staff can open the order (staffReviewer), as markXMRExternal
		// does; an escrow deposit is never refunded by hand (A-330). A deposit recorded after the order closed is left
		// to flagLateDeposits, which announces it to the buyer and the staff when it flags it (QA462C-4): flagged here
		// first, it would never be announced.
		rows, err := tx.QueryContext(ctx, "UPDATE payments SET flagged=true,updated=now() WHERE order_id=$1 AND (xmr_settlement_id IS NULL OR xmr_settlement_id=$2) AND NOT flagged AND NOT after_close RETURNING txid,amount", s.OrderID, s.ID)
		if err != nil {
			return err
		}
		named, _, err := scanNamed(rows, "XMR")
		if err != nil {
			return err
		}
		// The note names the deposits it flags, so the desk dates their flags (QA462E-3).
		note := xmrFlagNotePrefix(s, class, a.escrowXMR.confirmations)
		if len(named) > 0 {
			note += " Flagged so staff can open the order: " + strings.Join(named, ", ") + "."
		}
		note += " Moderator review required."
		if err = a.flagOrder(ctx, tx, &o, note, ""); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.Wait = class
	if class != xmrWaitConfirm {
		log.Printf("escrow: Monero escrow settlement of order %s not sent yet: %s", shortID(s.OrderID), class)
	}
	return nil
}

// storeXMRBuilt stores a checked build in one transaction (requested -> built by compare-and-set): its outputs, its
// transaction, and the claim of each deposit it spends (payments.xmr_settlement_id, compare-and-set), with order notes.
// A lost race (the settlement changed, or a deposit was claimed meanwhile) stores nothing and reports false; the
// transaction, never relayed, is then discarded.
func (a *App) storeXMRBuilt(ctx context.Context, s *xmrSettleRow, built xmrBuiltTx, plan settlePlan, deposits []xmrDeposit, total int64) (bool, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, "SELECT state FROM xmr_settlements WHERE id=$1 FOR UPDATE", s.ID).Scan(&state); err != nil || state != "requested" {
		return false, err
	}
	for i, o := range plan.Outputs {
		if i == 0 {
			o.Sats = built.AmountsByDest[0]
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO xmr_settlement_outputs(settlement_id,n,role,address,amount) VALUES($1,$2,$3,$4,$5)", s.ID, i, o.Role, o.Address, o.Sats); err != nil {
			return false, err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE xmr_settlements SET state='built',tx_hash=$2,tx_metadata=$3,tx_blob=$4,fee=$5,input_total=$6,key_images=$7::text[],send_error='',updated=now()
		WHERE id=$1 AND state='requested'`, s.ID, built.TxHash, built.Metadata, built.Blob, built.Fee, total, built.SpentKeyImages)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil
	}
	ids := make([]int64, len(deposits))
	for i, d := range deposits {
		ids[i] = d.ID
	}
	res, err = tx.ExecContext(ctx, "UPDATE payments SET xmr_settlement_id=$1,updated=now() WHERE order_id=$2 AND id=ANY($3) AND (xmr_settlement_id IS NULL OR xmr_settlement_id=$1)", s.ID, s.OrderID, ids)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != int64(len(ids)) {
		return false, nil // a deposit was claimed meanwhile: roll back, build again next pass
	}
	next := "and the market sends it now"
	if a.xmrRelayHold() != "" {
		next = "and the market sends it, unchanged, once the operator lets Monero relays go out"
	}
	note := fmt.Sprintf("Monero escrow settlement built (%s): spends %s XMR from this order's escrow account in %d output(s); network fee %s XMR, taken from the %s output. It was checked and stored, %s.",
		settlementKind(s.Kind), amount(total, 12), len(built.SpentKeyImages), amount(built.Fee, 12), settlementClaimant(s.Kind), next)
	for _, n := range append([]string{note}, plan.Folded...) {
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) SELECT id,state,state,NULL,$2 FROM orders WHERE id=$1", s.OrderID, n); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// relayXMRStored relays settlement s's stored transaction (its metadata), and only that one: the same hash answered
// is relayed; a definite refusal is resolved by xmrRelayRefused; metadata the wallet cannot parse (-27, which only a
// corrupted row explains, since the wallet made it) fails the settlement at once rather than being relayed again on
// every pass; any other error (a timeout, say) leaves it as it is, and the next pass looks it up before it relays the
// same metadata again. While xmrRelayHold holds, nothing is relayed and s stays built.
func (a *App) relayXMRStored(ctx context.Context, s *xmrSettleRow) error {
	if a.xmrRelayHold() != "" {
		return nil
	}
	got, err := a.escrowXMR.rpc.relayTx(ctx, s.Metadata)
	var re *xmrRPCError
	switch {
	case err == nil && got == s.TxHash:
		return a.markXMRRelayed(ctx, s, false)
	case err == nil:
		return xmrBadReply("wallet", "relay_tx")
	case errors.As(err, &re) && re.Service == "wallet" && re.Code == -4:
		return a.xmrRelayRefused(ctx, s)
	case errors.As(err, &re) && re.Service == "wallet" && re.Code == -27:
		return a.failXMRSettlement(ctx, s, xmrBadMetadata)
	}
	return err
}

// xmrOnNetwork looks settlement s's transaction up in the escrow daemon: found (in its pool or its chain) with its
// confirmations (0 in the pool), or not found (missed_tx).
func (a *App) xmrOnNetwork(ctx context.Context, s *xmrSettleRow) (found bool, confirmations int64, err error) {
	txs, missed, err := a.escrowXMR.rpc.transactions(ctx, []string{s.TxHash})
	switch {
	case err != nil:
		return false, 0, err
	case len(txs) == 1 && len(missed) == 0 && txs[0].Hash == s.TxHash:
		if txs[0].InPool {
			return true, 0, nil
		}
		return true, max(txs[0].Confirmations, 1), nil
	case len(txs) == 0 && slices.Equal(missed, []string{s.TxHash}):
		return false, 0, nil
	}
	return false, 0, xmrBadReply("daemon", "/get_transactions")
}

// xmrSpentElsewhere reports whether a stored key image of s is spent (on chain or in the pool) while the daemon does
// not have s's transaction: another transaction spent its coins. The daemon is asked again after the key images, so a
// transaction of s's that arrived meanwhile is not read as another's.
func (a *App) xmrSpentElsewhere(ctx context.Context, s *xmrSettleRow) (bool, error) {
	spent, err := a.escrowXMR.rpc.keyImagesSpent(ctx, s.KeyImages)
	if err != nil || !slices.ContainsFunc(spent, func(n int) bool { return n != 0 }) {
		return false, err
	}
	found, _, err := a.xmrOnNetwork(ctx, s)
	return !found, err
}

// xmrRelayRefused resolves a definite refusal of s's stored transaction: the daemon has it after all -> relayed;
// another transaction spent a stored key image -> external, at once; otherwise the refusal is counted (xmrRelayRefusing)
// and the same stored metadata relayed again on the next pass, until the xmrRelayAttempts-th refusal in a row ->
// failed. External and failed alert staff.
func (a *App) xmrRelayRefused(ctx context.Context, s *xmrSettleRow) error {
	found, _, err := a.xmrOnNetwork(ctx, s)
	if err != nil {
		return err
	}
	if found {
		return a.markXMRRelayed(ctx, s, true)
	}
	elsewhere, err := a.xmrSpentElsewhere(ctx, s)
	if err != nil {
		return err
	}
	if elsewhere {
		return a.markXMRExternal(ctx, s)
	}
	before, confirmed := xmrRelayRefusals(s.Wait)
	if refusals := before + 1; refusals < xmrRelayAttempts {
		class := xmrRelayRefusing + " " + strconv.Itoa(refusals)
		if confirmed {
			class += " " + xmrRelayConfirmedMark
		}
		return a.xmrSettleWaits(ctx, s, class)
	}
	return a.failXMRSettlement(ctx, s, xmrRelayRefused)
}

// xmrRelayConfirmedMark ends the wait class of a counted refusal (xmrRelayRefusing) of a held settlement whose relay an
// administrator confirmed (sendRebroadcastOK): "relay-refusing 1 confirmed". The confirmation then lasts through the
// count, so the same stored metadata is still relayed under the restore gate until it takes effect or fails (closing
// QA F7).
const xmrRelayConfirmedMark = "confirmed"

// xmrRelayRefusals reads a send_error: the refusals in a row it counts (xmrRelayRefusing; 0 for any other class), and
// whether it carries an administrator's confirmation of the relay (sendRebroadcastOK, alone or with the count).
func xmrRelayRefusals(sendErr string) (refusals int, confirmed bool) {
	rest, ok := strings.CutPrefix(sendErr, xmrRelayRefusing+" ")
	if !ok {
		return 0, sendErr == sendRebroadcastOK
	}
	rest, confirmed = strings.CutSuffix(rest, " "+xmrRelayConfirmedMark)
	refusals, _ = strconv.Atoi(rest)
	return refusals, confirmed
}

// xmrRelayConfirmed reports whether send_error sendErr carries an administrator's confirmation of the relay.
func xmrRelayConfirmed(sendErr string) bool {
	_, confirmed := xmrRelayRefusals(sendErr)
	return confirmed
}

// xmrMaySend reports whether the stored transaction of a settlement with send_error sendErr may be relayed now, as
// maySend does for Bitcoin escrow: never while a restore holds it, and while the restore gate is set only once an
// administrator confirmed it (that confirmation lasts through counted refusals, xmrRelayConfirmedMark).
func xmrMaySend(gate bool, sendErr string) bool {
	return !restoreHeld(sendErr) && (xmrRelayConfirmed(sendErr) || !gate)
}

// xmrTorRelayHeld is xmrRelayHold's reason (fixed text, for the log and /admin).
const xmrTorRelayHeld = "Tor mode without the node Tor overlay"

// xmrRelayHold is why Monero escrow relays wait ("" = they may be sent), as broadcastHold is Bitcoin escrow's (A-441):
// in APP_MODE=tor, monero-wallet-rpc's relay_tx goes out through its daemon from this host's IP address, so nothing is
// relayed unless COMPOSE_FILE has xmrNodeTorOverlay or ESCROW_XMR_TOR_RELAY=clearnet-accepted (A-515; Monero over Tor
// is unverified, A-95). Settlements are still built, checked and stored; once a restart with either relays, the stored
// transaction is relayed unchanged (sendXMRBuilt). An administrator's confirmation after a restore (sendRebroadcastOK)
// waits too, unused. Both conditions are read at startup only.
func (a *App) xmrRelayHold() string {
	if a.mode != "tor" || a.escrowXMR == nil || a.escrowXMR.torRelayOK {
		return ""
	}
	return xmrTorRelayHeld
}

// sendXMRBuilt handles a built settlement found on a later pass (a relay whose answer was lost, or a restart mid-send):
// the daemon has its transaction -> relayed, without sending anything (under the restore gate too); otherwise, unless
// the gate is set, a restore holds it or Monero relays are held in Tor mode (xmrRelayHold), the wallet is asked (an
// error there is never read as "not known") and the same stored metadata is relayed again. transfer is never called
// again.
func (a *App) sendXMRBuilt(ctx context.Context, s *xmrSettleRow, gate bool) error {
	found, _, err := a.xmrOnNetwork(ctx, s)
	if err != nil {
		return err
	}
	if found {
		return a.markXMRRelayed(ctx, s, true)
	}
	if !xmrMaySend(gate, s.Wait) || a.xmrRelayHold() != "" {
		return nil
	}
	if _, _, err = a.escrowXMR.rpc.transferByTxid(ctx, s.Account, s.TxHash); err != nil {
		return err
	}
	return a.relayXMRStored(ctx, s)
}

// markXMRRelayed records s as relayed (built -> relayed by compare-and-set), with a note and a notice to both parties.
// seen: the daemon already had it (a relay whose answer was lost).
func (a *App) markXMRRelayed(ctx context.Context, s *xmrSettleRow, seen bool) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE xmr_settlements SET state='relayed',relayed_at=now(),send_error=CASE WHEN send_error LIKE $2 THEN send_error ELSE '' END,updated=now()
		WHERE id=$1 AND state='built'`, s.ID, restoredHoldPrefix+"%")
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	what := settlementKind(s.Kind)
	note := "The Monero escrow settlement (" + what + ") was sent to the " + a.xmrEscrowNetwork() + " network: transaction " + s.TxHash + ". The order page shows it and its confirmations."
	if seen {
		note = "The Monero escrow settlement (" + what + ") is on the " + a.xmrEscrowNetwork() + " network: the escrow daemon has its stored transaction " + s.TxHash + ", sent before the market recorded it. The order page shows it and its confirmations."
	}
	if err = settleNote(ctx, tx, s.OrderID, note, "its Monero escrow settlement ("+what+") was sent to the "+a.xmrEscrowNetwork()+" network; the order page shows its transaction and its confirmations."); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.State = "relayed"
	return nil
}

// trackXMRRelayed follows a relayed settlement (step 3 of the file comment): mined with the confirmation threshold ->
// confirmed; in the pool or a block below it -> waited for; unknown to the daemon -> external when another
// transaction spent a stored key image, else the same stored metadata is relayed again (not under the restore gate, a
// restore's hold or xmrRelayHold), with relayXMRStored's handling of a refusal.
func (a *App) trackXMRRelayed(ctx context.Context, s *xmrSettleRow, gate bool) error {
	found, confirmations, err := a.xmrOnNetwork(ctx, s)
	if err == nil && found && strings.HasPrefix(s.Wait, xmrRelayRefusing+" ") {
		// The daemon has it after all: the count of refusals ends.
		if _, err = a.db.ExecContext(ctx, "UPDATE xmr_settlements SET send_error='',updated=now() WHERE id=$1 AND send_error=$2", s.ID, s.Wait); err != nil {
			return err
		}
	}
	switch {
	case err != nil:
		return err
	case found && confirmations >= a.escrowXMR.confirmations:
		return a.confirmXMRSettlement(ctx, s, confirmations)
	case found:
		return nil
	}
	elsewhere, err := a.xmrSpentElsewhere(ctx, s)
	if err != nil {
		return err
	}
	if elsewhere {
		return a.markXMRExternal(ctx, s)
	}
	if !xmrMaySend(gate, s.Wait) || a.xmrRelayHold() != "" {
		return nil
	}
	got, err := a.escrowXMR.rpc.relayTx(ctx, s.Metadata)
	var re *xmrRPCError
	switch {
	case err == nil && got == s.TxHash:
		log.Printf("escrow: Monero escrow settlement of order %s: the daemon had lost its transaction; the stored transaction was relayed again", shortID(s.OrderID))
		// An administrator's confirmation allowed this one relay (escrow_xmr_restore.go); a relay that took effect ends a
		// count of refusals.
		_, err = a.db.ExecContext(ctx, "UPDATE xmr_settlements SET send_error='',updated=now() WHERE id=$1 AND (send_error=$2 OR send_error LIKE $3)", s.ID, sendRebroadcastOK, xmrRelayRefusing+" %")
		return err
	case err == nil:
		return xmrBadReply("wallet", "relay_tx")
	case errors.As(err, &re) && re.Service == "wallet" && re.Code == -4:
		return a.xmrRelayRefused(ctx, s)
	case errors.As(err, &re) && re.Service == "wallet" && re.Code == -27:
		return a.failXMRSettlement(ctx, s, xmrBadMetadata)
	}
	return err
}

// confirmXMRSettlement records s as confirmed (relayed -> confirmed) at confirmations, with a note and a notice to both
// parties.
func (a *App) confirmXMRSettlement(ctx context.Context, s *xmrSettleRow, confirmations int64) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE xmr_settlements SET state='confirmed',confirmed_at=now(),updated=now() WHERE id=$1 AND state='relayed'", s.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	what := settlementKind(s.Kind)
	if err = settleNote(ctx, tx, s.OrderID, "The Monero escrow settlement ("+what+") is confirmed on the "+a.xmrEscrowNetwork()+" network ("+strconv.FormatInt(confirmations, 10)+" confirmations).",
		"its Monero escrow settlement ("+what+") is confirmed on the "+a.xmrEscrowNetwork()+" network."); err != nil {
		return err
	}
	return tx.Commit()
}

// trackXMRConfirmed follows a confirmed settlement until its transaction has escrowReorgDepth confirmations. Back in
// the pool or unknown to the daemon (a reorganisation took it out of the chain), it goes back to relayed, the one
// move back a stored transaction makes (migration 084), and trackXMRRelayed relays the same transaction again when
// the daemon lost it, or finds it spent by another.
func (a *App) trackXMRConfirmed(ctx context.Context, s *xmrSettleRow) error {
	found, confirmations, err := a.xmrOnNetwork(ctx, s)
	switch {
	case err != nil:
		return err
	case confirmations >= escrowReorgDepth:
		_, err = a.db.ExecContext(ctx, "UPDATE xmr_settlements SET reorg_safe_at=now(),updated=now() WHERE id=$1 AND state='confirmed' AND reorg_safe_at IS NULL", s.ID)
		return err
	case found && confirmations > 0:
		return nil // still in a block (a shallower reorganisation may have lowered its count)
	}
	gate, err := restoreGateSet(ctx, a.db)
	if err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE xmr_settlements SET state='relayed',confirmed_at=NULL,updated=now() WHERE id=$1 AND state='confirmed' AND reorg_safe_at IS NULL", s.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	what := settlementKind(s.Kind)
	next := "The market sends the stored transaction again if the daemon lost it, and follows it until it is confirmed again."
	if gate {
		next = "The market does not send it again while it reconciles its records after a restore from a backup; it follows it until it is confirmed again."
	}
	note := "The transaction of the Monero escrow settlement (" + what + ") is no longer in the " + a.xmrEscrowNetwork() + " chain: a reorganisation of the chain took it out of the block that confirmed it. It is the same transaction, back to waiting for confirmations. " + next
	if err = settleNote(ctx, tx, s.OrderID, note, "its Monero escrow settlement ("+what+") is no longer confirmed after a reorganisation of the "+a.xmrEscrowNetwork()+" chain; it is the same transaction, waiting for confirmations again."); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: Monero escrow settlement of order %s: its transaction left the chain (reorganisation); back to relayed", shortID(s.OrderID))
	return nil
}

// markXMRExternal records that another transaction spent the coins of s's stored transaction (built, relayed or
// confirmed -> external): its deposits are flagged so staff can open the order, with a note and a notice to the
// parties and to the staff.
func (a *App) markXMRExternal(ctx context.Context, s *xmrSettleRow) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE xmr_settlements SET state='external',reorg_safe_at=NULL,send_error='',updated=now() WHERE id=$1 AND state IN ('built','relayed','confirmed')", s.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	// The deposits it flags are named in the note, so the desk dates their flags by it (QA462F-1).
	rows, err := tx.QueryContext(ctx, "UPDATE payments SET "+newFlagSQL+",updated=now() WHERE xmr_settlement_id=$1 RETURNING txid,amount", s.ID)
	if err != nil {
		return err
	}
	named, _, err := scanNamed(rows, "XMR")
	if err != nil {
		return err
	}
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1", s.OrderID))
	if err != nil {
		return err
	}
	what := settlementKind(s.Kind)
	note := "The coins of this order's Monero escrow settlement (" + what + ") were spent by a transaction this market did not make, so its stored transaction " + s.TxHash +
		" can never confirm. The settlement is recorded as spent outside the market." + namedSentence(named) + " Moderator review required."
	if err = a.flagOrder(ctx, tx, &o, note, "Order "+shortID(s.OrderID)+": the coins of its Monero escrow settlement ("+what+") were spent by a transaction this market did not make. Market staff were notified."); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: Monero escrow settlement of order %s: its coins were spent by another transaction (external)", shortID(s.OrderID))
	return nil
}

// failXMRSettlement ends a settlement that cannot be sent (requested, built or relayed -> failed) with its class: no
// deposit left to spend, or its stored transaction refused for good. A refused transaction's deposits stay claimed by
// it and are flagged, so an administrator can open the order and re-request it (A-401); the staff and the parties are
// told. The coins stay in the escrow account.
func (a *App) failXMRSettlement(ctx context.Context, s *xmrSettleRow, class string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE xmr_settlements SET state='failed',send_error=$2,updated=now() WHERE id=$1 AND state IN ('requested','built','relayed')", s.ID, class)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	// The deposits it flags are named in the note, so the desk dates their flags by it (QA462F-1).
	rows, err := tx.QueryContext(ctx, "UPDATE payments SET "+newFlagSQL+",updated=now() WHERE xmr_settlement_id=$1 RETURNING txid,amount", s.ID)
	if err != nil {
		return err
	}
	named, _, err := scanNamed(rows, "XMR")
	if err != nil {
		return err
	}
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1", s.OrderID))
	if err != nil {
		return err
	}
	what := settlementKind(s.Kind)
	text := xmrSettleWaitText(class, s.Kind, a.escrowXMR.confirmations)
	if err = a.flagOrder(ctx, tx, &o, "Monero escrow settlement "+strconv.FormatInt(s.ID, 10)+" ("+what+") failed: "+text+namedSentence(named)+" Moderator review required.",
		"Order "+shortID(s.OrderID)+": its Monero escrow settlement ("+what+") was not sent. "+text); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: Monero escrow settlement of order %s failed: %s", shortID(s.OrderID), class)
	return nil
}

// xmrRequestStrays requests a stray refund (P3-R8) for each closed Monero escrow order of wallet w with deposits to
// return and none in progress, as requestStray does for Bitcoin escrow: to the pinned refund address, in one
// transaction under the escrow row's lock, with an order note and a notice to the buyer.
func (a *App) xmrRequestStrays(ctx context.Context, w xmrWalletRow) error {
	threshold := a.escrowXMR.confirmations
	rows, err := a.db.QueryContext(ctx, `SELECT es.order_id FROM escrows es JOIN xmr_escrow_slots sl ON sl.id=es.xmr_slot_id JOIN orders o ON o.id=es.order_id
		WHERE sl.wallet_id=$1 AND es.coin='XMR' AND o.state IN ('completed','cancelled','resolved') AND EXISTS (SELECT 1 FROM payments p WHERE p.order_id=es.order_id AND `+xmrStrayEligibleSQL+`)
		AND NOT EXISTS (SELECT 1 FROM xmr_settlements s WHERE s.order_id=es.order_id AND s.kind='stray_refund' AND s.state IN ('requested','built','relayed')) ORDER BY es.id`, w.id, threshold)
	if err != nil {
		return err
	}
	var due []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		due = append(due, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, id := range due {
		if err = a.xmrRequestStray(ctx, id); err != nil {
			return &watchError{label: "monero escrow order " + shortID(id), class: "stray refund not requested: " + errorCause(err), err: err}
		}
	}
	return nil
}

func (a *App) xmrRequestStray(ctx context.Context, orderID string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var buyer, state string
	var open bool
	var sum, n int64
	if err = tx.QueryRowContext(ctx, `SELECT o.buyer_id,o.state,
			EXISTS(SELECT 1 FROM xmr_settlements s WHERE s.order_id=es.order_id AND s.kind='stray_refund' AND s.state IN ('requested','built','relayed')),
			(SELECT COALESCE(sum(p.amount),0) FROM payments p WHERE p.order_id=es.order_id AND `+xmrStrayEligibleSQL+`),
			(SELECT count(*) FROM payments p WHERE p.order_id=es.order_id AND `+xmrStrayEligibleSQL+`)
		FROM escrows es JOIN orders o ON o.id=es.order_id WHERE es.order_id=$1 AND es.coin='XMR' FOR UPDATE OF es`, orderID, a.escrowXMR.confirmations).Scan(&buyer, &state, &open, &sum, &n); err != nil {
		return err
	}
	if !isTerminal(state) || open || n == 0 {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO xmr_settlements(order_id,kind,destination) SELECT order_id,'stray_refund',refund_address FROM escrows WHERE order_id=$1`, orderID); err != nil {
		return err
	}
	note := "Stray refund requested: " + amount(sum, 12) + " XMR in " + strconv.FormatInt(n, 10) + " deposit(s) to this order's Monero escrow that do not count for the order (sent once it had closed, or left out of its settlement) go back, less the network fee, to the buyer's refund address given at checkout. The market builds and sends the transaction. Nothing is sent yet."
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", orderID, state, note); err != nil {
		return err
	}
	if _, err = insertNotification(ctx, tx, buyer, "Order "+shortID(orderID)+": "+note, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

// xmrLeaveOutTopUps marks, for a Monero escrow order whose release or refund is built (or relayed or confirmed), every
// deposit that counts and that no settlement spends as left out (leaveOutTopUps's rule, A-374, P3-R8), in tx under the
// order row lock: one order note, a notice to the buyer and to staff, only for the deposits newly marked. The stray
// refund returns them to the buyer's refund address once the order is closed and they are confirmed.
func (a *App) xmrLeaveOutTopUps(ctx context.Context, tx *sql.Tx, o *Order) error {
	var kind string
	err := tx.QueryRowContext(ctx, "SELECT kind FROM xmr_settlements WHERE order_id=$1 AND kind IN ('release','refund') AND state IN ('built','relayed','confirmed')", o.ID).Scan(&kind)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE payments SET escrow_left_out=true,`+newFlagSQL+`,updated=now() WHERE order_id=$1 AND xmr_settlement_id IS NULL AND confirmations>=0
		AND NOT locked AND NOT after_close AND NOT escrow_left_out AND NOT spent_outside RETURNING txid,amount`, o.ID)
	if err != nil {
		return err
	}
	named, sum, err := scanNamed(rows, "XMR")
	if err != nil || len(named) == 0 {
		return err
	}
	what := "its release to the vendor"
	if kind == "refund" {
		what = "its refund to the buyer"
	}
	count := "1 deposit"
	if n := len(named); n > 1 {
		count = strconv.Itoa(n) + " deposits"
	}
	// Named, and the order note asks for review, so the desk dates the flags (QA462E-2); the buyer's notice does not ask.
	note := count + " (" + amount(sum, 12) + " XMR, " + a.netTag() + " " + a.xmrEscrowNetwork() + ") arrived at this order's Monero escrow after " + what + " was built, so that settlement does not spend them. " +
		"They stay in the escrow account; once the order is closed and they are confirmed, a stray refund returns them, less the network fee, to the buyer's refund address. Do not send more coins to this escrow. " +
		"The deposits: " + strings.Join(named, ", ") + "."
	if err = a.flagOrder(ctx, tx, o, note+" Moderator review required.", ""); err != nil {
		return err
	}
	_, err = insertNotification(ctx, tx, o.BuyerID, "Order "+shortID(o.ID)+": "+note, o.ID)
	return err
}

// xmrLeaveOutUncounted marks, for a Monero escrow order whose coins go back to the buyer (coinsToBuyer: cancelled, or
// resolved for the buyer, A-437) with no refund that spends or will spend its deposits (none requested, or the one
// requested failed, too few included (xmrRefundTooFew), or was spent outside), every deposit that has reached the
// confirmation threshold, that no settlement spends and that is neither after close nor left out as left out (A-424):
// one the wallet read as conflicted (double_spend_seen) or no longer listed when the order was cancelled or its dispute
// decided, so it did not count, mined since. As xmrLeaveOutTopUps does, in tx under the order row lock: one order note,
// a notice to the buyer and to staff, only for the deposits newly marked. The stray refund returns them to the buyer's
// refund address.
func (a *App) xmrLeaveOutUncounted(ctx context.Context, tx *sql.Tx, o *Order, threshold int64) error {
	if back, err := coinsToBuyer(ctx, tx, o); err != nil || !back {
		return err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE payments SET escrow_left_out=true,`+newFlagSQL+`,updated=now() WHERE order_id=$1 AND xmr_settlement_id IS NULL
		AND confirmations>=$2 AND NOT locked AND NOT after_close AND NOT escrow_left_out AND NOT spent_outside
		AND NOT EXISTS (SELECT 1 FROM xmr_settlements WHERE order_id=$1 AND kind='refund' AND state IN ('requested','built','relayed','confirmed')) RETURNING txid,amount`, o.ID, threshold)
	if err != nil {
		return err
	}
	named, sum, err := scanNamed(rows, "XMR")
	if err != nil || len(named) == 0 {
		return err
	}
	when := "it was cancelled"
	if o.State == stateResolved {
		when = "its dispute was decided"
	}
	note := fmt.Sprintf("%d deposit(s) (%s XMR, %s %s) to this order's Monero escrow did not count for the order when %s (the escrow wallet read them as conflicted, or no longer listed them), and no refund spends them. "+
		"They have confirmed since and stay in the escrow account; a stray refund returns them, less the network fee, to the buyer's refund address. Do not send more coins to this escrow. "+
		"The deposits: %s.",
		len(named), amount(sum, 12), a.netTag(), a.xmrEscrowNetwork(), when, strings.Join(named, ", "))
	// Named, and the order note asks for review, so the desk dates the flags (QA462E-2); the buyer's notice does not ask.
	if err = a.flagOrder(ctx, tx, o, note+" Moderator review required.", ""); err != nil {
		return err
	}
	_, err = insertNotification(ctx, tx, o.BuyerID, "Order "+shortID(o.ID)+": "+note, o.ID)
	return err
}
