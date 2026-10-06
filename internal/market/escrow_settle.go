package market

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// Escrow settlements (WR-escrow Phase 1, slice P1-c1; ES-1, ES-2, ES-4, ES-5). A settlement is the one transaction
// that spends an escrow order's escrow: the vendor's claim on a completed order (kind release) or the buyer's
// refund of a cancelled one (kind refund). This file builds it and takes the parties' signatures; the market's
// co-signature, the stored final transaction and its broadcast are escrow_cosign.go (slice P1-c2), so nothing here
// signs with the market key or sends anything.
//
//  1. Requested in state building, RPC-free: the vendor's claim (/escrow/claim, with a fresh destination address the
//     vendor gives then, ES-5), or a refund to the escrow's pinned refund address, written by settleTerminal in the
//     transaction that cancels a funded escrow order (the buyer, the vendor or the expired payment window).
//  2. Built by the escrow watcher (buildSettlements; transitions stay RPC-free, ARCH-7): every recorded output of the
//     escrow that counts (not conflicted, not locked, not first seen after the order closed) must have the
//     confirmation threshold; each is checked unspent with gettxout (value, address, confirmations); the outputs come
//     from the frozen plan (planSettlement): the claimant's output pays the network fee alone, the market fee
//     snapshotted at checkout is never reduced, and an output below the floor is folded into the claimant's with an
//     order note; the fee rate is explicit and capped (escrowMaxFeeRate, escrowMaxFee). createpsbt (no coin selection)
//     and utxoupdatepsbt with the escrow's bare-key descriptor give the PSBT, which is checked in Go and stored with
//     its unsigned txid; the row moves building -> awaiting_signature by compare-and-set in the transaction that
//     writes the outputs and claims each spent payments row (payments.settlement_id, compare-and-set).
//  3. Each party downloads its own copy (settlementCopy): the stored PSBT plus its own key's origin on every input,
//     the other keys bare. Either party pastes it back signed (/escrow/sign, ES-4); every check (checkSettlementPaste)
//     runs before anything is written, and only the party's signatures are kept, never the pasted PSBT. The row
//     moves awaiting_signature -> party_signed by compare-and-set; the database refuses any change to its
//     transaction once a party has signed (migration 075).

func init() {
	registerAction("/escrow/claim", actionSpec{OwnTx: true, OneAtATime: true, Off: escrowOff, Run: escrowClaimAction})
	registerAction("/escrow/sign", actionSpec{Off: escrowOff, Run: escrowSignAction})
	registerRaw("/escrow-psbt", escrowPSBTDownload)
}

const (
	escrowDefaultFeeRate = 2       // sat/vB, ESCROW_BTC_FEE_RATE's default
	escrowMaxFeeRate     = 100     // sat/vB: ESCROW_BTC_FEE_RATE's ceiling and every build's rate cap
	escrowMaxFee         = 100_000 // sats (0.001 BTC): the absolute network fee cap of one settlement
	escrowMaxInputs      = 50      // outputs one settlement spends at most, so a signed copy fits a form post
	// A-342: how long a claim or refund build waits, from its request, for an escrow output recorded before the order
	// closed to confirm, or while the escrow holds more than escrowMaxInputs outputs, before it is built without them
	// (they go to a stray refund). 24 hours is about 144 blocks: an output unconfirmed that long is stuck (a fee too
	// low to be mined, or a transaction that will be replaced), and a buyer pushing many small outputs cannot hold the
	// vendor's claim longer. It matches the watch horizon after an order closes (escrowClosedWatch); the plan sets no
	// setting for it, so it is not configurable.
	escrowBuildWaitMax  = 24 * time.Hour
	escrowBuildWaitText = "24 hours"
	// Upper bounds of a settlement's virtual size, so the fee is never below the rate: 11 vB of fixed fields, 105 vB
	// per 2-of-3 P2WSH input (41 bytes outside the witness; a witness of at most 256 bytes: two 73-byte signatures,
	// the 105-byte script, their length bytes and the empty CHECKMULTISIG item) and 43 vB per output (a script of up
	// to 34 bytes).
	settleTxOverheadVB = 11
	settleInputVB      = 105
	settleOutputVB     = 43
)

// Build refusals, as the fixed classes settlements.build_error records. waiting, unconfirmed, spent and fee-cap leave
// the settlement building and are tried again on every pass; too-small and nothing end it (failed).
const (
	settleWaiting     = "waiting-confirmations"
	settleUnconfirmed = "input-unconfirmed"
	settleSpent       = "input-spent"
	settleMismatch    = "input-mismatch"
	settleFeeCap      = "fee-cap"
	settleTooMany     = "too-many-inputs"
	settleTooSmall    = "too-small"
	settleNothing     = "nothing-to-settle"
)

// settleErrorText is what the order page says about a build refusal.
func settleErrorText(class string, threshold int64) string {
	switch class {
	case settleWaiting:
		return "Waiting until every deposit to this escrow has " + strconv.FormatInt(threshold, 10) + " confirmations."
	case settleUnconfirmed, settleSpent, settleMismatch:
		return "An escrow deposit is not confirmed, or not unspent as recorded, on the market's Bitcoin node. The market tries again on each pass; market staff should check the escrow if this stays."
	case settleFeeCap:
		return "The network fee would exceed the market's fee cap. The market tries again on each pass; market staff must review it."
	case settleTooMany:
		return "This escrow has more deposits than one settlement can spend. Market staff must review it."
	case settleTooSmall:
		return "The coins in this escrow are too few to pay the network fee and an output of at least 0.0001 BTC, so the market cannot build this settlement. They stay in the escrow; the buyer and the vendor can move them together from their own wallets."
	case settleNothing:
		return "No confirmed deposit of this escrow is left to spend, so there is nothing to settle."
	}
	return ""
}

// errClaimAddress and errClaimInvalid refuse a missing or invalid claim address on the escrow node's chain net.
func errClaimAddress(net string) error {
	return fail(400, "Enter the address the claim pays: a fresh "+net+" Bitcoin address from your own wallet, used for nothing else. Nothing was changed.")
}

func errClaimInvalid(net string) error {
	return fail(400, "The claim address is not a valid "+net+" Bitcoin address. Nothing was changed.")
}

var (
	errClaimAddressUsed = fail(409, "That address was already used on this market. Give a fresh address from your wallet. Nothing was changed.")
	errClaimOnlyVendor  = fail(403, "Only the vendor claims the escrow of this order.")
	errClaimState       = fail(409, "The escrow is claimed once its order is completed (or its dispute is decided for the vendor). Reload the page. Nothing was changed.")
	errClaimTwice       = fail(409, "This escrow already has its settlement. Reload the page. Nothing was changed.")
	errClaimNode        = fail(503, "The claim address could not be checked: the market's Bitcoin node did not answer. Nothing was changed; try again later.")
	errSettleNotReady   = fail(409, "This settlement is not waiting for a signature (it is still being built, or it was already signed). Reload the page. Nothing was changed.")
	errSettleSigned     = fail(409, "You already signed this settlement. Reload the page. Nothing was changed.")
	errSettleNone       = fail(404, "Settlement not found")

	// Refusals of a pasted settlement PSBT (ES-2): fixed text, never a quote of the paste.
	errSettleOtherTx   = fail(400, "This is not the settlement transaction shown on this page. Download your copy again, check its outputs, sign it and paste the result. Nothing was changed.")
	errSettleFinal     = fail(400, "The pasted PSBT is already finalized. Sign your copy without finalizing it (in Bitcoin Core: walletprocesspsbt with finalize false) and paste the result. Nothing was changed.")
	errSettleMarketKey = fail(400, "The pasted PSBT carries an entry under the market's key. Sign only your own copy downloaded from this page, in the wallet that holds your escrow key, and paste it unchanged. Nothing was changed.")
	errSettleOtherKey  = fail(400, "The pasted PSBT carries a signature from a key other than your escrow key for this order. Sign only in the wallet that holds the escrow key you registered. Nothing was changed.")
	errSettleUnsigned  = fail(400, "The pasted PSBT does not carry your signature on every input. Sign your copy in the wallet that holds your escrow key and paste the result. Nothing was changed.")
	errSettleSighash   = fail(400, "A signature uses a sighash type other than ALL. Sign with the default sighash type (ALL). Nothing was changed.")
)

// settleOutput is one output of a settlement plan.
type settleOutput struct {
	Role, Address string // role: claimant, market_fee, buyer_return
	Sats          int64
}

// settlePlan is a settlement's frozen outputs (the claimant's first) and its network fee.
type settlePlan struct {
	Outputs    []settleOutput
	NetworkFee int64
	Folded     []string // order notes: outputs below the floor added to the claimant's
}

// settleVSize is the upper bound of a settlement's virtual size with nIn inputs and nOut outputs.
func settleVSize(nIn, nOut int) int64 {
	return settleTxOverheadVB + settleInputVB*int64(nIn) + settleOutputVB*int64(nOut)
}

// marketFeeSats is the market fee of price at bps basis points, rounded down, without overflow.
func marketFeeSats(price, bps int64) int64 { return price/10000*bps + price%10000*bps/10000 }

// planSettlement is the frozen plan (ES-5) of a settlement of kind spending nIn escrow outputs worth total. A
// release pays the vendor's destination the escrow less the market fee (snapshotted at checkout, never reduced) and
// less any overpayment above price, which returns to the buyer's refund address; a refund pays the buyer's refund
// address everything. The network fee is rate times the size bound and comes from the claimant's output only; the
// market fee or return below floor is added to the claimant's output instead, with a note. It refuses (a build
// class) a fee above the caps and a claimant's output below floor.
func planSettlement(kind string, nIn int, total, price, feeBps int64, feeDest, claimant, refund string, rate, floor int64) (settlePlan, string) {
	var p settlePlan
	var extra []settleOutput
	if kind == "release" {
		if fee := marketFeeSats(price, feeBps); fee > 0 && feeDest != "" {
			if fee >= floor {
				extra = append(extra, settleOutput{"market_fee", feeDest, fee})
			} else {
				p.Folded = append(p.Folded, "The market fee of "+amount(fee, 8)+" BTC is below the minimum output of "+amount(floor, 8)+" BTC, so it was added to the vendor's output.")
			}
		}
		if excess := total - price; excess > 0 {
			if excess >= floor {
				extra = append(extra, settleOutput{"buyer_return", refund, excess})
			} else {
				p.Folded = append(p.Folded, "The overpayment of "+amount(excess, 8)+" BTC is below the minimum output of "+amount(floor, 8)+" BTC, so it was added to the vendor's output.")
			}
		}
	}
	p.NetworkFee = rate * settleVSize(nIn, 1+len(extra))
	if rate < 1 || rate > escrowMaxFeeRate || p.NetworkFee > escrowMaxFee {
		return p, settleFeeCap
	}
	left := total - p.NetworkFee
	for _, o := range extra {
		left -= o.Sats
	}
	if left < floor {
		return p, settleTooSmall
	}
	p.Outputs = append([]settleOutput{{"claimant", claimant, left}}, extra...)
	return p, ""
}

// sortedMultisigScript is the escrow witness script over three compressed keys: 2 <keys sorted> 3 CHECKMULTISIG, the
// script wsh(sortedmulti(2, …)) pays.
func sortedMultisigScript(keys ...[]byte) []byte {
	keys = slices.Clone(keys)
	slices.SortFunc(keys, bytes.Compare)
	ws := []byte{0x52}
	for _, k := range keys {
		ws = append(append(ws, 33), k...)
	}
	return append(ws, 0x53, 0xae)
}

// escrowKeyRow is one escrow_pubkeys row: the party's child key and its full derivation (origin and suffix).
type escrowKeyRow struct {
	Party, Fingerprint, Origin, Suffix string
	PubKey                             []byte
}

// escrowKeys reads the three keys of order's escrow, by party.
func escrowKeys(ctx context.Context, q rowsQuerier, orderID string) (map[string]escrowKeyRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.party,p.pubkey,p.origin_fingerprint,p.origin_path,p.derivation_suffix FROM escrow_pubkeys p
		JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=$1 AND es.coin='BTC'`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := map[string]escrowKeyRow{}
	for rows.Next() {
		var k escrowKeyRow
		var pub string
		if err = rows.Scan(&k.Party, &pub, &k.Fingerprint, &k.Origin, &k.Suffix); err != nil {
			return nil, err
		}
		if k.PubKey, err = hex.DecodeString(pub); err != nil || len(k.PubKey) != 33 {
			return nil, errors.New("escrow: a stored escrow key is malformed")
		}
		keys[k.Party] = k
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(keys) != 3 || keys["buyer"].PubKey == nil || keys["vendor"].PubKey == nil || keys["market"].PubKey == nil {
		return nil, errors.New("escrow: the escrow does not have its three keys")
	}
	return keys, nil
}

// escrowSettleTerminal is settleTerminal for an escrow order (ES-1). A cancelled order holding coins, or one whose
// dispute was decided for the buyer, gets its refund requested (state building, to the pinned refund address); a
// completed one, or one decided for the vendor, waits for the vendor's claim (escrow_decision.go). The settlement
// covers every deposit that counts, confirmed or still confirming: the cancel and resolve forms state that sum
// (checkPayoutSeen). A physical order's release, requested when it was shipped (P2-R1, P2-R4), is the settlement of a
// decision for the vendor; a decision for the buyer supersedes it while it has no final transaction, and is refused
// (409) once it has one (A-371).
func (a *App) escrowSettleTerminal(ctx context.Context, tx *sql.Tx, o *Order) error {
	var outcome string
	switch o.State {
	case stateCancelled:
	case stateResolved:
		var err error
		if outcome, err = escrowDisputeOutcome(ctx, tx, o.ID); err != nil {
			return err
		}
	default:
		return nil
	}
	var sum int64
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(amount),0),count(*) FROM payments WHERE order_id=$1 AND confirmations>=0 AND NOT locked AND NOT after_close`, o.ID).Scan(&sum, &n); err != nil {
		return err
	}
	if n > 0 {
		if err := checkPayoutSeen(ctx, o, sum); err != nil {
			return err
		}
	}
	// The order's live release or refund, if any: a physical order's release was requested when it was shipped (P2-R1).
	var liveID int64
	var liveKind, liveState string
	var liveFinal bool
	// A Monero escrow's settlements are xmr_settlements (migration 084): final once its transaction is stored, and its
	// deposits claimed by payments.xmr_settlement_id.
	table, final, claim := "settlements", "raw_tx", "settlement_id"
	if o.Currency == "XMR" {
		table, final, claim = "xmr_settlements", "tx_hash", "xmr_settlement_id"
	}
	err := tx.QueryRowContext(ctx, "SELECT id,kind,state,"+final+" IS NOT NULL FROM "+table+" WHERE order_id=$1 AND kind IN ('release','refund') AND state<>'superseded' FOR UPDATE", o.ID).Scan(
		&liveID, &liveKind, &liveState, &liveFinal)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if outcome == "release" {
		if n == 0 {
			return nil
		}
		note := "Dispute decided for the vendor: the vendor claims the escrowed " + amount(sum, 8) + " BTC from the order page with a fresh address. The market builds that settlement (less the market fee and the network fee; any amount paid above the price goes back to the buyer), the buyer or the vendor signs it, and the market co-signs it. Nothing is sent yet."
		switch {
		case o.Currency == "XMR" && liveKind == "release":
			// P3-R7, A-405: the release requested at shipment is the decision's settlement; the Monero escrow watcher alone
			// builds and sends it (escrow_xmr_settle.go), now that the order's outcome allows it.
			note = "Dispute decided for the vendor: the release to the vendor requested when the order was shipped pays the claim address the vendor gave then (less the market fee and the network fee; any amount paid above the price goes back to the buyer's refund address). The market builds it from this order's Monero escrow account and sends it. The release deadline no longer applies. Nothing is sent yet."
		case o.Currency == "XMR":
			note = "Dispute decided for the vendor: the vendor claims the escrowed " + amount(sum, 12) + " XMR from the order page with a fresh Monero address. The market then builds that settlement from this order's escrow account (less the market fee and the network fee; any amount paid above the price goes back to the buyer's refund address) and sends it. Nothing is sent yet."
		case liveKind == "release":
			// P2-R4: the release built at shipment is the decision's settlement; no new claim is asked for.
			note = "Dispute decided for the vendor: the release to the vendor built when the order was shipped pays the claim address the vendor gave then (less the market fee and the network fee; any amount paid above the price goes back to the buyer). A signature already given on it counts; the buyer or the vendor signs it if neither has, and the market co-signs it. The release deadline no longer applies."
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, o.State, note)
		return err
	}
	var notes []string
	switch {
	case liveKind == "refund":
		return nil // requested already (a later call for the same cancelled order)
	case liveKind == "release" && (liveFinal || liveState == "external"):
		// A-371: a release with a final transaction (or whose coins were spent on the network) is the outcome already.
		return errReleaseFinal
	case liveKind == "release":
		// A-371, P2-R4: the release built at shipment has no final transaction: it ends here (its signatures with it)
		// and its deposits are free for the refund, in this transaction (the escrow_decision.go pattern).
		if _, err = tx.ExecContext(ctx, "UPDATE "+table+" SET state='superseded',updated=now() WHERE id=$1", liveID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE payments SET "+claim+"=NULL,updated=now() WHERE "+claim+"=$1", liveID); err != nil {
			return err
		}
		notes = append(notes, "The release to the vendor built when the order was shipped is cancelled: it was not final, and any signature on it no longer counts.")
	}
	if n > 0 {
		note := "Escrow refund requested: the market builds a transaction returning the escrowed " + amount(sum, 8) + " BTC, less the network fee, to the buyer's refund address given at checkout; the buyer or the vendor then signs it on the order page. Nothing is sent yet."
		if o.Currency == "XMR" {
			// A Monero escrow's refund is an xmr_settlements row (migration 084), built and sent by the Monero escrow
			// watcher alone from the order's account (P3-R6, P3-R8).
			note = "Escrow refund requested: the market builds a transaction returning the escrowed " + amount(sum, 12) + " XMR, less the network fee, from this order's escrow account to the buyer's refund address given at checkout, and sends it. Nothing is sent yet."
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+table+`(order_id,kind,destination) SELECT order_id,'refund',refund_address FROM escrows WHERE order_id=$1`, o.ID); err != nil {
			return err
		}
		if outcome == "refund" {
			note = "Dispute decided for the buyer. " + note
		}
		notes = append(notes, note)
	}
	for _, n := range notes {
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, o.State, n); err != nil {
			return err
		}
	}
	return nil
}

// escrowClaimAction: the vendor of a completed escrow order requests the claim, giving the address it pays (ES-5: a
// fresh address, taken now and never at signing), confirmed with their password (and authenticator code when
// enrolled). The escrow watcher builds the transaction (buildSettlements).
func escrowClaimAction(c *actionCtx) (actionResult, error) {
	e := c.A.escrow.Load()
	if e == nil {
		return actionResult{}, errEscrowDown
	}
	dest, ok := escrowAddressForm(c.Form.Get("address"))
	if !ok {
		return actionResult{}, errClaimAddress(c.A.escrowNetwork())
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		o, err := partyOrder(c, c.Form.Get("order_id"))
		if err != nil {
			return actionResult{}, err
		}
		outcome, err := escrowDisputeOutcome(ctx, c.Tx, o.ID)
		if err != nil {
			return actionResult{}, err
		}
		switch {
		case !o.btcEscrow():
			return actionResult{}, errNoOrder
		case o.VendorID != c.User.ID:
			return actionResult{}, errClaimOnlyVendor
		case !settlementFits("release", o.State, outcome):
			return actionResult{}, errClaimState
		}
		if err = checkClaimAddress(c, e, dest); err != nil {
			return actionResult{}, err
		}
		if err = insertRelease(c, o.ID, dest); err != nil {
			return actionResult{}, err
		}
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", o.ID, o.State, c.User.ID,
			"Vendor requested the escrow claim: the market builds the settlement transaction, which the buyer or the vendor then signs. Nothing is sent yet."); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: orderPage(o.ID) + "#escrow-settlement", Notice: "escrow-claim", Audit: "Requested the escrow claim of order " + shortID(o.ID) + " (" + c.A.escrowNetwork() + ")"}, nil
	})
}

// settleInput is one payments row a settlement spends.
type settleInput struct {
	ID            int64
	TxID          string
	Vout          uint32
	Sats          int64
	Confirmations int64
}

// buildSettlements builds every settlement in state building (the escrow watcher, once per pass).
func (a *App) buildSettlements(ctx context.Context, e *escrowBTC) error {
	rows, err := a.db.QueryContext(ctx, "SELECT id FROM settlements WHERE state='building' ORDER BY id")
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err = a.buildSettlement(ctx, e, id); err != nil {
			errs = append(errs, &watchError{label: "escrow settlement " + strconv.FormatInt(id, 10), class: "not built: " + errorClassOf(err), err: err})
		}
	}
	return errors.Join(errs...)
}

// buildSettlement builds one settlement (step 2 of the file comment). The RPCs run outside any transaction; the
// result is stored by compare-and-set, so of two builds of one settlement one stores and the other changes nothing.
func (a *App) buildSettlement(ctx context.Context, e *escrowBTC, id int64) error {
	var orderID, kind, state, dest, escrowAddr, refund, feeDest, orderState, outcome, buildErr string
	var price, feeBps int64
	var bounded bool
	err := a.db.QueryRowContext(ctx, `SELECT s.order_id,s.kind,s.state,COALESCE(s.destination,''),es.address,es.refund_address,es.fee_bps,es.fee_destination,o.amount,o.state,
		s.created < now()-make_interval(secs => $2),`+disputeOutcomeSQL+`,s.build_error
		FROM settlements s JOIN escrows es ON es.order_id=s.order_id AND es.coin='BTC' JOIN orders o ON o.id=s.order_id WHERE s.id=$1`, id, escrowBuildWaitMax.Seconds()).Scan(
		&orderID, &kind, &state, &dest, &escrowAddr, &refund, &feeBps, &feeDest, &price, &orderState, &bounded, &outcome, &buildErr)
	if err != nil || state != "building" {
		return err
	}
	if dest == "" || !settlementBuildable(kind, orderState, outcome) {
		return errors.New("escrow: the settlement does not fit its order's state or dispute decision")
	}
	keys, err := escrowKeys(ctx, a.db, orderID)
	if err != nil {
		return err
	}
	// A claim or refund spends the outputs that count (recorded before the order closed, not left out by a bounded
	// build); a stray refund spends the confirmed others (escrow_stray.go).
	query := `SELECT id,txid,idx,amount,confirmations FROM payments p WHERE order_id=$1 AND confirmations>=0 AND NOT locked AND NOT after_close AND NOT escrow_left_out
		AND NOT spent_outside AND settlement_id IS NULL AND $2::bigint>0 ORDER BY created,id`
	if kind == "stray_refund" {
		query = `SELECT id,txid,idx,amount,confirmations FROM payments p WHERE order_id=$1 AND ` + strayEligibleSQL + ` ORDER BY created,id`
	}
	rows, err := a.db.QueryContext(ctx, query, orderID, e.confirmations)
	if err != nil {
		return err
	}
	var ins []settleInput
	for rows.Next() {
		var in settleInput
		var idx int64
		if err = rows.Scan(&in.ID, &in.TxID, &idx, &in.Sats, &in.Confirmations); err != nil {
			rows.Close()
			return err
		}
		if idx < 0 || idx > 1<<31 || len(in.TxID) != 64 {
			rows.Close()
			return errors.New("escrow: a recorded escrow output is malformed")
		}
		in.Vout = uint32(idx)
		ins = append(ins, in)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	unconfirmed := slices.ContainsFunc(ins, func(in settleInput) bool { return in.Confirmations < e.confirmations })
	var left []settleInput
	// could is the value of every confirmed deposit it could spend (for a stray refund, all of strayEligibleSQL's, also
	// beyond the 50 it spends), recorded with the fee rate if it fails too-small: strayHeldSQL compares them with what a
	// stray refund could spend now and the current rate (A-434; a cancelled order's refund too, A-435, whose deposits
	// still confirming a bounded build leaves out: once confirmed they are worth more).
	var could int64
	for _, in := range ins {
		if in.Confirmations >= e.confirmations {
			could += in.Sats
		}
	}
	if kind == "stray_refund" {
		// A-436: an input worth no more than its own spending cost at the current rate (one 2-of-3 P2WSH input's size
		// bound times the rate) only takes from the refund, so it is left out of the build while another input remains;
		// it stays eligible for a later stray refund. could still counts it, so a skip never looks like growth. With
		// every input uneconomic the build keeps them all and fails too-small, as before.
		if kept := slices.DeleteFunc(slices.Clone(ins), func(in settleInput) bool { return in.Sats <= settleInputVB*e.feeRate }); len(kept) > 0 {
			ins = kept
		}
	}
	switch {
	case len(ins) == 0:
		return a.failSettlement(ctx, id, orderID, settleNothing, 0, 0, nil)
	case kind == "stray_refund" && len(ins) > escrowMaxInputs:
		ins, _ = boundedInputs(ins, e.confirmations) // the 50 largest; the rest wait for the next stray refund
	case kind == "stray_refund":
	case !bounded && len(ins) > escrowMaxInputs:
		return a.settlementWaits(ctx, id, orderID, settleTooMany)
	case !bounded && unconfirmed:
		return a.settlementWaits(ctx, id, orderID, settleWaiting)
	case bounded && (unconfirmed || len(ins) > escrowMaxInputs):
		// A-342: the wait is over; build without what it waited on, which a stray refund returns once confirmed.
		if ins, left = boundedInputs(ins, e.confirmations); len(ins) == 0 {
			return a.failSettlement(ctx, id, orderID, settleNothing, 0, 0, nil, left...)
		}
	}
	var total int64
	for _, in := range ins {
		total += in.Sats
	}
	plan, refused := planSettlement(kind, len(ins), total, price, feeBps, feeDest, dest, refund, e.feeRate, payoutFloor("BTC", kind))
	switch refused {
	case settleTooSmall:
		return a.failSettlement(ctx, id, orderID, refused, could, e.feeRate, ins, left...)
	case settleFeeCap:
		return a.settlementWaits(ctx, id, orderID, refused)
	}
	// Each input as the node sees it now: unspent (not even by a transaction in its mempool), confirmed, and the
	// output the ledger recorded (value and escrow address).
	for _, in := range ins {
		u, err := e.utxo(ctx, in.TxID, in.Vout)
		switch {
		case err != nil:
			return err
		case !u.Found:
			return a.buildInputSpent(ctx, e, id, orderID, buildErr, ins)
		case u.Confirmations < e.confirmations:
			return a.settlementWaits(ctx, id, orderID, settleUnconfirmed)
		case u.Sats != in.Sats || u.Address != escrowAddr:
			return a.settlementWaits(ctx, id, orderID, settleMismatch)
		}
	}
	outpoints := make([]escrowOutpoint, len(ins))
	for i, in := range ins {
		outpoints[i] = escrowOutpoint{TxID: in.TxID, Vout: in.Vout}
	}
	payments := make([]escrowPayment, len(plan.Outputs))
	for i, o := range plan.Outputs {
		payments[i] = escrowPayment{Address: o.Address, Sats: o.Sats}
	}
	unsigned, err := e.createPSBT(ctx, outpoints, payments)
	if err != nil {
		return err
	}
	up, err := decodePSBT(unsigned)
	if err != nil || !settlementTxMatches(up, ins, plan) {
		return &escrowRPCError{Method: "createpsbt", Class: "bad-reply"}
	}
	ws := sortedMultisigScript(keys["buyer"].PubKey, keys["vendor"].PubKey, keys["market"].PubKey)
	bare := withChecksum("wsh(sortedmulti(2," + hex.EncodeToString(keys["buyer"].PubKey) + "," + hex.EncodeToString(keys["vendor"].PubKey) + "," + hex.EncodeToString(keys["market"].PubKey) + "))")
	filled, err := e.utxoUpdatePSBT(ctx, unsigned, []string{bare})
	if err != nil {
		return err
	}
	fp, err := decodePSBT(filled)
	if err != nil || fp.TxID() != up.TxID() || !settlementTxMatches(fp, ins, plan) || !settlementInputsFilled(fp, ins, ws) {
		return &escrowRPCError{Method: "utxoupdatepsbt", Class: "bad-reply"}
	}
	// Core writes a key-origin entry for each bare key too (its own fingerprint, an empty path). The stored copy has
	// none, so a party's copy carries exactly one, its own, and a pasted entry under the market's key is always
	// foreign (checkSettlementPaste).
	raw, err := fp.withoutInputFields(func(_ int, key []byte) bool { return key[0] == 0x06 })
	if err != nil {
		return err
	}
	pristine, err := parsePSBT(raw)
	if err != nil || pristine.TxID() != fp.TxID() || slices.ContainsFunc(pristine.Inputs, func(in psbtInput) bool { return len(in.Derivations) > 0 }) {
		return &escrowRPCError{Method: "utxoupdatepsbt", Class: "bad-reply"}
	}
	return a.storeSettlement(ctx, id, orderID, kind, encodePSBT(raw), fp.TxID(), total, e.feeRate, plan, ins, left)
}

// buildInputSpent handles a build input that is no longer unspent. The first time, the build waits (settleSpent, the
// P1-c1 wait): a spend still in the mempool may yet be evicted or replaced. Still spent on the next pass, it was spent
// by a transaction the market did not make (no settlement of the market spends a deposit before the settlement that
// claims it is built), so the settlement is external (A-356): each spent deposit is marked spent outside the market,
// with the transaction that spent it when the watch-only wallet names it (walletSpenders), and flagged for staff.
func (a *App) buildInputSpent(ctx context.Context, e *escrowBTC, id int64, orderID, buildErr string, ins []settleInput) error {
	if buildErr != settleSpent {
		return a.settlementWaits(ctx, id, orderID, settleSpent)
	}
	want := map[string]bool{}
	var spent []settleInput
	for _, in := range ins {
		u, err := e.utxo(ctx, in.TxID, in.Vout)
		if err != nil {
			return err
		}
		if !u.Found {
			spent = append(spent, in)
			want[in.TxID+":"+strconv.FormatUint(uint64(in.Vout), 10)] = true
		}
	}
	if len(spent) == 0 {
		return nil // the spend left the mempool meanwhile: the next pass builds
	}
	by, err := e.walletSpenders(ctx, want)
	if err != nil {
		log.Printf("escrow: the transaction spending an escrow deposit could not be looked up: %s", errorClassOf(err))
		by = map[string]string{}
	}
	deposits, first := map[int64]string{}, ""
	for _, in := range spent {
		s := by[in.TxID+":"+strconv.FormatUint(uint64(in.Vout), 10)]
		deposits[in.ID] = s
		if first == "" {
			first = s
		}
	}
	return a.markExternal(ctx, id, orderID, first, deposits)
}

// settlementTxMatches checks a PSBT's unsigned transaction against the build: exactly the inputs asked for, in
// order, and the planned output values, in order (createpsbt did no coin selection and added no change).
func settlementTxMatches(p *psbtPacket, ins []settleInput, plan settlePlan) bool {
	if len(p.Tx.In) != len(ins) || len(p.Tx.Out) != len(plan.Outputs) || len(p.Inputs) != len(ins) {
		return false
	}
	for i, in := range ins {
		id := p.Tx.In[i].TxID
		slices.Reverse(id[:])
		if hex.EncodeToString(id[:]) != in.TxID || p.Tx.In[i].Vout != in.Vout {
			return false
		}
	}
	for i, o := range plan.Outputs {
		if p.Tx.Out[i].Value != o.Sats {
			return false
		}
	}
	return true
}

// settlementInputsFilled checks what utxoupdatepsbt added to each input: the recorded value paid to the escrow's
// script (P2WSH of ws) and ws as the witness script, no signature or final field, and key-origin entries only as
// Core writes them for the descriptor's bare keys (one of ws's keys, an empty path).
func settlementInputsFilled(p *psbtPacket, ins []settleInput, ws []byte) bool {
	sum := sha256.Sum256(ws)
	spk := append([]byte{0, 32}, sum[:]...)
	for i, in := range p.Inputs {
		if in.WitnessUTXO == nil || in.WitnessUTXO.Value != ins[i].Sats || !bytes.Equal(in.WitnessUTXO.Script, spk) || !bytes.Equal(in.WitnessScript, ws) ||
			len(in.PartialSigs) > 0 || in.Finalized {
			return false
		}
		for _, d := range in.Derivations {
			if len(d.Path) != 0 || len(d.PubKey) != 33 || !bytes.Contains(ws, append([]byte{33}, d.PubKey...)) {
				return false
			}
		}
	}
	return true
}

// storeSettlement stores a built settlement: its outputs, the PSBT and txid (building -> awaiting_signature by
// compare-and-set), each input's payments row claimed by compare-and-set, an order note and a notice to both parties.
// A lost race (another build stored first, or an output already claimed) changes nothing.
func (a *App) storeSettlement(ctx context.Context, id int64, orderID, kind, psbt, txid string, total, rate int64, plan settlePlan, ins, left []settleInput) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, buyer, vendor, orderState string
	if err = tx.QueryRowContext(ctx, `SELECT s.state,o.buyer_id,p.vendor_id,o.state FROM settlements s JOIN orders o ON o.id=s.order_id JOIN products p ON p.id=o.product_id
		WHERE s.id=$1 FOR UPDATE OF s`, id).Scan(&state, &buyer, &vendor, &orderState); err != nil || state != "building" {
		return err
	}
	for i, o := range plan.Outputs {
		if _, err = tx.ExecContext(ctx, "INSERT INTO settlement_outputs(settlement_id,n,role,address,sats) VALUES($1,$2,$3,$4,$5)", id, i, o.Role, o.Address, o.Sats); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE settlements SET state='awaiting_signature',psbt=$2,txid=$3,fee_rate=$4,network_fee=$5,input_total=$6,build_error='',updated=now()
		WHERE id=$1 AND state='building'`, id, psbt, txid, rate, plan.NetworkFee, total)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	claimed := make([]int64, len(ins))
	for i, in := range ins {
		claimed[i] = in.ID
	}
	res, err = tx.ExecContext(ctx, "UPDATE payments SET settlement_id=$1,updated=now() WHERE order_id=$2 AND id=ANY($3) AND settlement_id IS NULL", id, orderID, claimed)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != int64(len(ins)) {
		return nil // an output was claimed meanwhile: roll back, build again next pass
	}
	if err = leaveOut(ctx, tx, orderID, kind, buyer, "was built without", left); err != nil {
		return err
	}
	what := settlementKind(kind)
	waits, body := "It waits for a signature from the buyer or the vendor.", "Order "+shortID(orderID)+": its escrow settlement ("+what+") is ready to sign. Check its outputs on the order page, sign your copy in your own wallet and paste it there; either you or the other party can sign. "+a.settleNotSent()
	if releaseBeforeOutcome(kind, orderState) {
		// P2-R2: a shipped order's release waits for both parties; the buyer's signature confirms receipt.
		waits = "It waits for the vendor's and the buyer's signatures; the market never signs it before the order is completed."
		body = "Order " + shortID(orderID) + ": its release to the vendor is ready to sign. Check its outputs on the order page: the vendor signs it now, and the buyer signs it once the goods have arrived (the buyer's signature confirms receipt and ends the right to open a dispute). " + physicalReleaseNotSent(a.escrowNetwork())
	}
	note := fmt.Sprintf("Escrow settlement built (%s): spends %s BTC from the escrow in %d output(s); network fee %s BTC, taken from the %s output. %s Nothing is sent yet.",
		what, amount(total, 8), len(ins), amount(plan.NetworkFee, 8), settlementClaimant(kind), waits)
	for _, n := range append([]string{note}, plan.Folded...) {
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) SELECT id,state,state,NULL,$2 FROM orders WHERE id=$1", orderID, n); err != nil {
			return err
		}
	}
	for _, uid := range []string{buyer, vendor} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, body); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// boundedInputs splits a build's inputs once its wait is over (A-342): it keeps the confirmed ones, at most
// escrowMaxInputs of the largest, in their order, and leaves out the rest.
func boundedInputs(ins []settleInput, threshold int64) (kept, left []settleInput) {
	var confirmed []int
	for i, in := range ins {
		if in.Confirmations >= threshold {
			confirmed = append(confirmed, i)
		} else {
			left = append(left, in)
		}
	}
	slices.SortStableFunc(confirmed, func(i, j int) int { return cmp.Compare(ins[j].Sats, ins[i].Sats) })
	if len(confirmed) > escrowMaxInputs {
		for _, i := range confirmed[escrowMaxInputs:] {
			left = append(left, ins[i])
		}
		confirmed = confirmed[:escrowMaxInputs]
	}
	slices.Sort(confirmed)
	for _, i := range confirmed {
		kept = append(kept, ins[i])
	}
	return kept, left
}

// leaveOut marks the outputs a bounded build left out (A-342) for the stray refund, in the transaction that stores (or
// fails) the settlement, with an order note and a notice to the buyer, whose refund address they are returned to.
func leaveOut(ctx context.Context, tx *sql.Tx, orderID, kind, buyer, verb string, left []settleInput) error {
	if len(left) == 0 {
		return nil
	}
	ids := make([]int64, len(left))
	var sum int64
	for i, in := range left {
		ids[i], sum = in.ID, sum+in.Sats
	}
	if _, err := tx.ExecContext(ctx, "UPDATE payments SET escrow_left_out=true,updated=now() WHERE order_id=$1 AND id=ANY($2) AND settlement_id IS NULL", orderID, ids); err != nil {
		return err
	}
	n := "1 deposit"
	if len(left) > 1 {
		n = strconv.Itoa(len(left)) + " deposits"
	}
	note := "This escrow's settlement (" + settlementKind(kind) + ") " + verb + " " + n + " (" + amount(sum, 8) + " BTC) to this escrow: it waited " + escrowBuildWaitText +
		" for a deposit recorded before the order closed to confirm, or for an escrow holding more deposits than one settlement spends. Those deposits stay in the escrow; once confirmed, they are returned to the buyer's refund address by a stray refund."
	if _, err := tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) SELECT id,state,state,NULL,$2 FROM orders WHERE id=$1", orderID, note); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), buyer, "Order "+shortID(orderID)+": "+note)
	return err
}

// settleNotSent is the honest line on every settlement that has not been broadcast (ES-10), naming the escrow node's
// chain (A-331).
func (a *App) settleNotSent() string {
	return "Nothing has been sent yet: once one party has signed, the market co-signs the settlement on its next pass and broadcasts it to the " + a.escrowNetwork() + " network; once both parties have signed, the market does not sign it and broadcasts the transaction they signed."
}

// settleNotSentBoth is the same line once both parties have signed (A-350): the market does not sign, it broadcasts
// the transaction the two signatures finalize to.
func (a *App) settleNotSentBoth() string {
	return "Nothing has been sent yet: the market does not sign it, and broadcasts the transaction the two parties signed to the " + a.escrowNetwork() + " network on its next pass."
}

// settleSigChecked says when a pasted signature's validity is checked (A-343, A-349): the paste checks only its shape.
const settleSigChecked = "A signature's validity is checked when the market finalizes the transaction."

func settlementKind(kind string) string {
	switch kind {
	case "release":
		return "the vendor's claim"
	case "stray_refund":
		return "a stray refund to the buyer"
	}
	return "a refund to the buyer"
}

func settlementClaimant(kind string) string {
	if kind == "release" {
		return "vendor's"
	}
	return "buyer's refund"
}

// settlementWaits records why a settlement is not built yet; it stays building and is tried again next pass. The
// class is logged once, when it changes.
func (a *App) settlementWaits(ctx context.Context, id int64, orderID, class string) error {
	res, err := a.db.ExecContext(ctx, "UPDATE settlements SET build_error=$2,updated=now() WHERE id=$1 AND state='building' AND build_error<>$2", id, class)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 && class != settleWaiting {
		log.Printf("escrow: settlement of order %s not built yet: %s", shortID(orderID), class)
	}
	return nil
}

// failSettlement ends a settlement that can never be built (building -> failed), with an order note and a notice to
// both parties. The coins stay in the escrow. could, when not 0, is the value of every deposit the settlement could
// spend, and rate, when not 0, the fee rate (sat/vB) it failed at; they are stored as the failed settlement's
// input_total and fee_rate (never shown: a failed settlement has no fee; settlements_built allows both on a failed
// row), which a stray refund that failed too-small is compared with before another is requested (strayHeldSQL, A-434).
// A refund that fails too-small on an order cancelled or resolved for the buyer leaves out tried, the deposits it could
// not spend (leaveOutTooFew, A-435, A-437), and its failure holds them as a stray refund's would; a release's deposits
// stay counted, and its too-small failure also tells staff (A-461).
func (a *App) failSettlement(ctx context.Context, id int64, orderID, class string, could, rate int64, tried []settleInput, left ...settleInput) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE settlements SET state='failed',build_error=$2,input_total=NULLIF($3::bigint,0),fee_rate=NULLIF($4::bigint,0),updated=now() WHERE id=$1 AND state='building'", id, class, could, rate)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	text := settleErrorText(class, 0)
	var buyer, vendor, kind string
	if err = tx.QueryRowContext(ctx, "SELECT o.buyer_id,p.vendor_id,s.kind FROM orders o JOIN products p ON p.id=o.product_id JOIN settlements s ON s.order_id=o.id WHERE s.id=$1", id).Scan(&buyer, &vendor, &kind); err != nil {
		return err
	}
	if kind == "release" && class == settleTooSmall {
		// A-461: the same note also tells staff, once, as a Monero release too few does (xmrSettleWaits): the vendor's
		// coins stay in the escrow, counted, and an administrator may change a dispute's decision.
		o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1", orderID))
		if err != nil {
			return err
		}
		if err = a.flagOrder(ctx, tx, &o, "Escrow settlement not built: "+text, ""); err != nil {
			return err
		}
	} else if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) SELECT id,state,state,NULL,$2 FROM orders WHERE id=$1", orderID, "Escrow settlement not built: "+text); err != nil {
		return err
	}
	for _, uid := range []string{buyer, vendor} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, "Order "+shortID(orderID)+": its escrow settlement could not be built. "+text); err != nil {
			return err
		}
	}
	if err = leaveOut(ctx, tx, orderID, kind, buyer, "could not use", left); err != nil {
		return err
	}
	if kind == "refund" && class == settleTooSmall {
		o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1", orderID))
		if err != nil {
			return err
		}
		if err = a.leaveOutTooFew(ctx, tx, &o, id, tried); err != nil {
			return err
		}
	}
	log.Printf("escrow: settlement of order %s not built: %s", shortID(orderID), class)
	return tx.Commit()
}

// settlementCopy is a party's copy of a stored settlement PSBT (ES-2, ES-3): the same transaction with the party's
// own key origin (a BIP32 derivation field) on every input, so the party's wallet finds its key; the other keys stay
// bare. It never carries anything but the stored copy and that one field per input.
func settlementCopy(stored string, k escrowKeyRow) (string, error) {
	path, err := parseOriginPath(k.Origin + k.Suffix)
	if err != nil {
		return "", err
	}
	fp, err := hex.DecodeString(k.Fingerprint)
	if err != nil || len(fp) != 4 {
		return "", errors.New("escrow: a stored key origin is malformed")
	}
	value := slices.Clone(fp)
	for _, step := range path {
		value = binary.LittleEndian.AppendUint32(value, step)
	}
	p, err := decodePSBT(stored)
	if err != nil {
		return "", err
	}
	raw := p.Raw
	for i := range p.Inputs {
		next, err := parsePSBT(raw)
		if err != nil {
			return "", err
		}
		if raw, err = next.withInputField(i, append([]byte{0x06}, k.PubKey...), value); err != nil {
			return "", err
		}
	}
	return encodePSBT(raw), nil
}

// checkSettlementPaste applies ES-2's checks to a pasted PSBT, before anything is stored: it is the stored
// transaction (unsigned txid), nothing is finalized, nothing sits under the market's key (a signature or derivation
// of its key, or a derivation from the arbiter fingerprint), every signature is the party's own and ends in
// SIGHASH_ALL (as does any sighash-type field), and every input carries the party's signature. It returns the
// party's signature on each input; nothing else of the paste is kept.
func checkSettlementPaste(pasted, txid string, party, market escrowKeyRow) ([]psbtSig, error) {
	p, err := decodePSBT(pasted)
	if err != nil {
		var pe *psbtError
		if errors.As(err, &pe) {
			return nil, fail(400, pe.Error()+" Nothing was changed.")
		}
		return nil, err
	}
	if p.TxID() != txid {
		return nil, errSettleOtherTx
	}
	sigs := make([]psbtSig, len(p.Inputs))
	for i, in := range p.Inputs {
		if in.Finalized {
			return nil, errSettleFinal
		}
		for _, d := range in.Derivations {
			if bytes.Equal(d.PubKey, market.PubKey) || hex.EncodeToString(d.Fingerprint[:]) == market.Fingerprint {
				return nil, errSettleMarketKey
			}
		}
		for _, s := range in.PartialSigs {
			if bytes.Equal(s.PubKey, market.PubKey) {
				return nil, errSettleMarketKey
			}
		}
		if in.HasSighashType && in.SighashType != 1 {
			return nil, errSettleSighash
		}
		for _, s := range in.PartialSigs {
			if !bytes.Equal(s.PubKey, party.PubKey) {
				return nil, errSettleOtherKey
			}
			if s.SighashByte() != 0x01 {
				return nil, errSettleSighash
			}
			sigs[i] = s
		}
		if sigs[i].Sig == nil {
			return nil, errSettleUnsigned
		}
	}
	return sigs, nil
}

// escrowSignAction: the buyer or the vendor of an escrow order pastes their signed copy of a settlement awaiting a
// signature (ES-2, ES-4). Either party may sign first; the one who does is recorded. The pasted PSBT is checked in
// full before anything is written and never stored: only the party's signature on each input is kept.
func escrowSignAction(c *actionCtx) (actionResult, error) {
	ctx := c.Ctx()
	// A-386: the order row first, as the dispute action takes it, so a buyer's signature and the buyer's dispute never
	// both take effect.
	o, err := partyOrderLocked(c, c.Form.Get("order_id"))
	if err != nil {
		return actionResult{}, err
	}
	if !o.btcEscrow() {
		return actionResult{}, errNoOrder
	}
	party := roleBuyer
	if c.User.ID == o.VendorID {
		party = roleVendor
	}
	sid, err := strconv.ParseInt(c.Form.Get("settlement_id"), 10, 64)
	if err != nil {
		return actionResult{}, errSettleNone
	}
	var kind, state, txid, stored, signer string
	var signed bool
	err = c.Tx.QueryRowContext(ctx, `SELECT kind,state,COALESCE(txid,''),COALESCE(psbt,''),COALESCE(signer,''),EXISTS(SELECT 1 FROM settlement_signatures WHERE settlement_id=s.id AND party=$3)
		FROM settlements s WHERE id=$1 AND order_id=$2 FOR UPDATE`, sid, o.ID, party).Scan(&kind, &state, &txid, &stored, &signer, &signed)
	if err == sql.ErrNoRows {
		return actionResult{}, errSettleNone
	}
	if err != nil {
		return actionResult{}, err
	}
	// A-340 (ES-4): the other party may sign a party-signed settlement too; with both signatures the market does not.
	second := state == "party_signed" && signer != party
	switch {
	case signed:
		return actionResult{}, errSettleSigned
	case state != "awaiting_signature" && !second:
		return actionResult{}, errSettleNotReady
	case party == roleBuyer && kind == "release" && o.State == stateDisputed:
		// P2-R2: the buyer's signature confirms receipt; while the order is disputed the decision settles it instead.
		return actionResult{}, errSignDisputed
	}
	keys, err := escrowKeys(ctx, c.Tx, o.ID)
	if err != nil {
		return actionResult{}, err
	}
	sp, err := decodePSBT(stored)
	if err != nil || sp.TxID() != txid {
		return actionResult{}, errors.New("escrow: the stored settlement PSBT does not match its txid")
	}
	sigs, err := checkSettlementPaste(c.Form.Get("psbt"), txid, keys[party], keys["market"])
	if err != nil {
		return actionResult{}, err
	}
	for i, s := range sigs {
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO settlement_signatures(settlement_id,input_n,party,pubkey,signature) VALUES($1,$2,$3,$4,$5)",
			sid, i, party, hex.EncodeToString(s.PubKey), hex.EncodeToString(s.Sig)); err != nil {
			return actionResult{}, err
		}
	}
	if !second {
		res, err := c.Tx.ExecContext(ctx, "UPDATE settlements SET state='party_signed',signer=$2,signed_at=now(),updated=now() WHERE id=$1 AND state='awaiting_signature' AND txid=$3", sid, party, txid)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return actionResult{}, errSettleNotReady
		}
	}
	what, note, notSent := "the "+party+" signed its escrow settlement ("+settlementKind(kind)+"). "+settleSigChecked+" ",
		"The "+party+" signed the escrow settlement ("+settlementKind(kind)+"). "+settleSigChecked+" ", c.A.settleNotSent()
	res := actionResult{Redirect: orderPage(o.ID) + "#escrow-settlement", Notice: "escrow-signed", Audit: "Signed the escrow settlement of order " + shortID(o.ID) + " (" + c.A.escrowNetwork() + ")"}
	if second {
		what = "the " + party + " also signed its escrow settlement (" + settlementKind(kind) + "): with both parties' signatures the market does not sign it. " + settleSigChecked + " "
		note = "The " + party + " also signed the escrow settlement (" + settlementKind(kind) + "): with both parties' signatures the market does not sign it. " + settleSigChecked + " "
		notSent = c.A.settleNotSentBoth()
		res.Notice = "escrow-signed-both"
	}
	if releaseBeforeOutcome(kind, o.State) {
		// P2-R2: the market never signs a shipped order's release; the buyer's signature confirms receipt.
		if !second {
			notSent = physicalReleaseNotSent(c.A.escrowNetwork())
		}
		if party == roleBuyer {
			what, note = what+"The buyer's signature confirms receipt of the order. ", note+"The buyer's signature confirms receipt of the order. "
		}
	}
	if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", o.ID, o.State, c.User.ID,
		note+notSent); err != nil {
		return actionResult{}, err
	}
	counterparty := o.BuyerID
	if party == roleBuyer {
		counterparty = o.VendorID
	}
	if _, err = c.Tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), counterparty,
		"Order "+shortID(o.ID)+": "+what+notSent); err != nil {
		return actionResult{}, err
	}
	return res, nil
}

// escrowPSBTDownload serves GET /escrow-psbt?id=<order>&s=<settlement>: the reader's own copy of a settlement
// awaiting a signature (settlementCopy), or with &b=<bump> of a fee bump awaiting the reader's signature, as a text
// file of base64. With ESCROW_BTC off, for anyone but the order's buyer and vendor, and for a settlement (or bump) not
// awaiting the reader's signature it answers as an unknown page does.
func escrowPSBTDownload(a *App, w http.ResponseWriter, r *http.Request, u *User) {
	if !a.escrowConfigured || a.db == nil || u == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	o, err := scanOrder(a.db.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 AND (o.buyer_id=$2 OR p.vendor_id=$2)", r.URL.Query().Get("id"), u.ID))
	sid, perr := strconv.ParseInt(r.URL.Query().Get("s"), 10, 64)
	if err == sql.ErrNoRows || err == nil && (!o.btcEscrow() || perr != nil) {
		http.NotFound(w, r)
		return
	}
	var stored string
	reader, file := roleVendor, "escrow-settlement-"
	if u.ID == o.BuyerID {
		reader = roleBuyer
	}
	bump := r.URL.Query().Get("b")
	if err == nil && bump == "" {
		// A copy is served while the settlement waits for a signature, and to the party who has not signed a
		// party-signed one (A-340: it may sign too).
		err = a.db.QueryRowContext(ctx, `SELECT psbt FROM settlements s WHERE id=$1 AND order_id=$2 AND (state='awaiting_signature'
			OR state='party_signed' AND NOT EXISTS(SELECT 1 FROM settlement_signatures g WHERE g.settlement_id=s.id AND g.party=$3))`, sid, o.ID, reader).Scan(&stored)
	} else if err == nil {
		// P2-d: a fee bump's copy is served to its claimant while it waits for a signature, and to the other party once
		// the claimant has signed it and while the other party has not (escrow_bump.go).
		bid, perr := strconv.ParseInt(bump, 10, 64)
		if perr != nil {
			http.NotFound(w, r)
			return
		}
		file = "escrow-fee-bump-"
		err = a.db.QueryRowContext(ctx, `SELECT b.psbt FROM settlement_bumps b JOIN settlements s ON s.id=b.settlement_id WHERE b.id=$1 AND s.id=$2 AND s.order_id=$3 AND s.state='broadcast'
			AND (b.state='awaiting_signature' AND settlement_claimant(s.kind)=$4 OR b.state='party_signed' AND settlement_claimant(s.kind)<>$4 AND b.signer=settlement_claimant(s.kind)
			AND NOT EXISTS(SELECT 1 FROM settlement_bump_signatures g WHERE g.bump_id=b.id AND g.party=$4))`, bid, sid, o.ID, reader).Scan(&stored)
	}
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	var copyText string
	if err == nil {
		var keys map[string]escrowKeyRow
		if keys, err = escrowKeys(ctx, a.db, o.ID); err == nil {
			copyText, err = settlementCopy(stored, keys[reader])
		}
	}
	if err != nil {
		serverError(w, r, 503, "Service unavailable", errorCause(err))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+shortID(o.ID)+`-`+reader+`.psbt.txt"`)
	w.Write([]byte(copyText + "\n"))
}

// escrowSettlementView fills the settlement part of the escrow panel: the settlement of order (its release or
// refund) with its state in honest words, its frozen outputs, and for a party a copy to sign and the paste form
// while it waits for a signature (and, for the party who has not signed, while it is party-signed, A-340); staff see
// it read-only. Its raw transaction is shown only once it was broadcast. A vendor of a completed order (or one resolved
// for the vendor) with no settlement yet is offered the claim form.
func (a *App) escrowSettlementView(ctx context.Context, d *PageData, v *EscrowOrderView) error {
	o := d.Order
	var id int64
	err := a.db.QueryRowContext(ctx, "SELECT id FROM settlements WHERE order_id=$1 AND kind IN ('release','refund') AND state<>'superseded'", o.ID).Scan(&id)
	switch {
	case err == nil:
		if v.Settlement, err = a.settlementView(ctx, d, v, id); err != nil {
			return err
		}
		v.Settlements = append(v.Settlements, v.Settlement)
	case err == sql.ErrNoRows:
		outcome, err := escrowDisputeOutcome(ctx, a.db, o.ID)
		if err != nil {
			return err
		}
		v.Claim = d.OrderViewer == roleVendor && settlementFits("release", o.State, outcome) && v.Available
		if v.Claim && d.Security == nil {
			if d.Security, _, _, err = a.securityView(ctx, d.User.ID); err != nil {
				return err
			}
		}
	default:
		return err
	}
	return a.escrowStrayView(ctx, d, v)
}

// settleSendText is what the order page says about a settlement the market could not complete or send yet.
func settleSendText(class string) string {
	switch class {
	case "":
		return ""
	case sendMarketKey:
		return "The market's escrow key could not sign it yet."
	case sendRejected:
		return "The market's Bitcoin node did not accept the transaction yet."
	case sendRebroadcastOK, sendRebroadcastSent:
		return "" // restoreStatus says it
	}
	if restoreHeld(class) {
		return "" // restoreStatus says it
	}
	return "The market's Bitcoin node returned a transaction that failed the market's checks, so nothing was sent."
}

// settlementOutputRole is whom an output of a settlement of kind pays, as the expected-outputs table says it.
func settlementOutputRole(kind, role string) string {
	switch role {
	case "claimant":
		if kind != "release" {
			return "The buyer's refund address (the refund, less the network fee)"
		}
		return "The vendor (the claim, less the network fee)"
	case "market_fee":
		return "The market fee"
	case "buyer_return":
		return "The buyer's refund address (the amount paid above the price)"
	}
	return ""
}

// settlementView is one settlement of the order as the reader sees it.
func (a *App) settlementView(ctx context.Context, d *PageData, v *EscrowOrderView, id int64) (*EscrowSettlementView, error) {
	s := &EscrowSettlementView{ID: id}
	var kind, state, buildErr, sendErr, signer, cosigned, stored, txid, raw, spentBy string
	var fee, total sql.NullInt64
	var signedAt sql.NullString
	var readerSigned bool
	err := a.db.QueryRowContext(ctx, `SELECT kind,state,build_error,send_error,COALESCE(signer,''),COALESCE(cosigned,''),COALESCE(psbt,''),COALESCE(txid,''),
		COALESCE(raw_tx,''),COALESCE(spent_by,''),network_fee,input_total,to_char(signed_at,'YYYY-MM-DD HH24:MI "UTC"'),
		EXISTS(SELECT 1 FROM settlement_signatures g WHERE g.settlement_id=s.id AND g.party=$2)
		FROM settlements s WHERE id=$1`, id, d.OrderViewer).Scan(
		&kind, &state, &buildErr, &sendErr, &signer, &cosigned, &stored, &txid, &raw, &spentBy, &fee, &total, &signedAt, &readerSigned)
	if err != nil {
		return nil, err
	}
	s.State = state
	// P2-R2: a physical order's release before its outcome completes with both parties' signatures only.
	s.PartiesOnly = releaseBeforeOutcome(kind, d.Order.State)
	switch {
	case kind == "release" && d.Order.Kind == "physical":
		s.Title = "Release to the vendor"
	case kind == "release":
		s.Title = "The vendor's claim"
	case kind == "refund":
		s.Title = "Refund to the buyer"
	default:
		s.Title = "Recovery of coins that do not count for the order: sent after it closed, left out of its settlement, or not counted when it was cancelled (a stray refund to the buyer)"
	}
	pending := settleSendText(sendErr)
	if pending != "" {
		pending = " " + pending + " The market tries again on each pass; market staff should check it if this stays."
	}
	var buyerSigned, vendorSigned bool
	if state == "party_signed" {
		if err = a.db.QueryRowContext(ctx, "SELECT COALESCE(bool_or(party='buyer'),false),COALESCE(bool_or(party='vendor'),false) FROM settlement_signatures WHERE settlement_id=$1", id).Scan(
			&buyerSigned, &vendorSigned); err != nil {
			return nil, err
		}
	}
	bothSigned := buyerSigned && vendorSigned
	switch state {
	case "building":
		s.Status = "Being built: the market builds the transaction from the escrow's confirmed deposits on its next pass. Reload in a minute."
		switch t := settleErrorText(buildErr, v.Threshold); {
		case !v.Available:
			s.Status = "Waiting: Bitcoin escrow is unavailable on this market right now, so the market builds this settlement once it is available again."
		case t != "":
			s.Status = "Not built yet. " + t
		}
	case "awaiting_signature":
		s.Status = "Waiting for a signature from the buyer or the vendor. " + a.settleNotSent()
		switch {
		case s.PartiesOnly:
			s.Status = "Waiting for the vendor's and the buyer's signatures. " + physicalReleaseNotSent(a.escrowNetwork())
		case kind == "release" && d.Order.Kind == "physical" && d.Order.State == stateCompleted:
			// P2-R3: only the release deadline completes a physical order whose release nobody signed (escrow_deadline.go).
			s.Status = "The release deadline passed with no dispute, so the order is completed. The release waits for the vendor's signature: once the vendor has signed it on this page, the market co-signs it and broadcasts it. Nothing has been sent yet."
		}
	case "party_signed":
		s.Status = "Signed by the " + signer + " (" + signedAt.String + "). " + settleSigChecked + " The market co-signs it and broadcasts it on its next pass. Nothing has been sent yet." + pending
		switch {
		case bothSigned && s.PartiesOnly && d.Order.State == stateDisputed:
			s.Status = "Signed by the " + signer + " and the " + otherParty(signer) + " (first signature " + signedAt.String + "). The order is disputed, so nothing is sent until an administrator decides the dispute. Nothing has been sent yet."
		case bothSigned:
			// A-353: with both parties' signatures the market does not sign; it broadcasts the transaction they signed.
			s.Status = "Signed by the " + signer + " and the " + otherParty(signer) + " (first signature " + signedAt.String + "). " + settleSigChecked + " The market does not sign it, and broadcasts the transaction the two parties signed on its next pass. Nothing has been sent yet." + pending
		case s.PartiesOnly:
			s.Status = "Signed by the " + signer + " (" + signedAt.String + "). Waiting for the " + otherParty(signer) + "'s signature. " + settleSigChecked + " " + physicalReleaseNotSent(a.escrowNetwork()) + pending
		}
	case "signed":
		by := "the market, with the " + signer + "'s signature"
		if cosigned == "parties" {
			by = "the buyer and the vendor (the market did not sign)"
		}
		s.Status = "Signed by " + by + ", checked and stored; the market broadcasts it on its next pass. Nothing has been sent yet." + pending
	case "broadcast":
		s.Status, s.TxID, s.RawTx = "Broadcast to the "+a.escrowNetwork()+" network: transaction "+txid+". Waiting for "+strconv.FormatInt(v.Threshold, 10)+" confirmations.", txid, raw
	case "confirmed":
		s.Status, s.TxID, s.RawTx = "Confirmed on the "+a.escrowNetwork()+" network: transaction "+txid+".", txid, raw
	case "external":
		by := ""
		if spentBy != "" {
			by = " (" + spentBy + ")"
		}
		s.Status = "Spent outside the market: the coins of this escrow were spent by a transaction this market did not make" + by + ", so this settlement will not be sent. Market staff were notified."
		if stored == "" {
			// A-356: never built; a deposit it was to spend was spent first.
			s.Status = "Spent outside the market: an escrow deposit this settlement was to spend was spent by a transaction this market did not make" + by + ", so the market cannot build it. Market staff were notified."
		}
	case "failed":
		s.Status = "Not built. " + settleErrorText(buildErr, v.Threshold)
	default:
		s.Status = state
	}
	a.restoreStatus(s, v, state, sendErr, signer, signedAt.String, bothSigned)
	if fee.Valid && total.Valid {
		s.NetworkFee, s.InputTotal = amount(fee.Int64, 8), amount(total.Int64, 8)
		rows, err := a.db.QueryContext(ctx, "SELECT role,address,sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var out SettlementOutputView
			var role string
			var sats int64
			if err = rows.Scan(&role, &out.Address, &sats); err != nil {
				rows.Close()
				return nil, err
			}
			out.Amount, out.Role = amount(sats, 8), settlementOutputRole(kind, role)
			s.Outputs = append(s.Outputs, out)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, err
		}
	}
	// P2-d: its fee bumps, and the fee bump form for the claimant of a broadcast settlement (escrow_bump.go).
	if err = a.bumpView(ctx, d, v, s, id, kind, state, txid, sendErr, fee.Int64); err != nil {
		return nil, err
	}
	// The buyer of a disputed order does not sign its release (escrowSignAction): the decision settles it.
	canSign := (state == "awaiting_signature" || state == "party_signed" && !readerSigned) && !(s.PartiesOnly && d.Order.State == stateDisputed && d.OrderViewer == roleBuyer)
	if canSign && (d.OrderViewer == roleBuyer || d.OrderViewer == roleVendor) && a.escrowConfigured {
		keys, err := escrowKeys(ctx, a.db, d.Order.ID)
		if err != nil {
			return nil, err
		}
		if s.PSBT, err = settlementCopy(stored, keys[d.OrderViewer]); err != nil {
			return nil, err
		}
		s.CanSign, s.SignToo = true, state == "party_signed"
	}
	if s.PartiesOnly && d.Order.State == stateShipped {
		v.PartiesOnly = true
		if next := releaseNextStep(state, map[string]bool{roleBuyer: buyerSigned, roleVendor: vendorSigned}); next != "" {
			v.NextStep = next
		}
	}
	return s, nil
}
