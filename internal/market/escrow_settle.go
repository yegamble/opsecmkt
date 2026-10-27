package market

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
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
	registerAction("/escrow/claim", actionSpec{Roles: escrowPartyRoles, OwnTx: true, OneAtATime: true, Off: escrowOff,
		ConfirmedFormNonce: true, ReceiptAuthorize: btcClaimReceiptAuthorize, Run: escrowClaimAction})
	registerAction("/escrow/sign", actionSpec{FormNonce: true, Off: escrowOff, Run: escrowSignAction})
	registerAction("/escrow/hardware-sign", actionSpec{FormNonce: true, Off: escrowOff, Run: escrowSignAction})
	registerRaw("/escrow-psbt", escrowPSBTDownload)
}

const (
	escrowDefaultFeeRate = 2       // sat/vB, ESCROW_BTC_FEE_RATE's default
	escrowMaxFeeRate     = 100     // sat/vB: ESCROW_BTC_FEE_RATE's ceiling and every build's rate cap
	escrowMaxFee         = 100_000 // sats (0.001 BTC): the absolute network fee cap of one settlement
	escrowMaxInputs      = 50      // outputs one settlement spends at most, so a signed copy fits a form post
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

// errClaimAddress and errClaimInvalid refuse a missing or invalid claim address on the escrow node's chain net.
func errClaimAddress(net string) error {
	return fail(400, "Enter the address the claim pays: a fresh "+net+" Bitcoin address from your own wallet, used for nothing else. Nothing was changed.")
}

func errClaimInvalid(net string) error {
	return fail(400, "The claim address is not a valid "+net+" Bitcoin address. Nothing was changed.")
}

var (
	errClaimAddressUsed = fail(409, "That address is a market address, this order's buyer refund address, or was already used on this market by your account. Give a fresh address from your wallet. Nothing was changed.")
	errClaimOnlyVendor  = fail(403, "Only the vendor claims the escrow of this order.")
	errClaimState       = fail(409, "The escrow is claimed once its order is completed (or its dispute is decided for the vendor). Reload the page. Nothing was changed.")
	errClaimTwice       = fail(409, "This escrow already has its settlement. Reload the page. Nothing was changed.")
	errClaimNode        = fail(503, "The claim address could not be checked: the market's Bitcoin node did not answer. Nothing was changed; try again later.")
	errSettleNotReady   = fail(409, "This settlement is not waiting for a signature (it is still being built, or it was already signed). Reload the page. Nothing was changed.")
	errSettleSigned     = fail(409, "You already signed this settlement. Reload the page. Nothing was changed.")
	errSettleNone       = fail(404, "Settlement not found")
	// QA5W5A-1: a per-order escrow key (perOrderXpub, siblingKeys) that cannot be had refuses the sign forms with this,
	// never a 500; the order page says settleKeysUnavailable instead of offering a copy.
	errSettleKeys = fail(503, settleKeysUnavailable+" Nothing was changed.")

	// Refusals of a pasted settlement PSBT (ES-2): fixed text, never a quote of the paste.
	errSettleOtherTx   = fail(400, "This is not the settlement transaction shown on this page. Download your copy again, check its outputs, sign it and paste the result. Nothing was changed.")
	errSettleFinal     = fail(400, "The pasted PSBT is already finalized. Sign your copy without finalizing it (in Bitcoin Core: walletprocesspsbt with finalize false) and paste the result. Nothing was changed.")
	errSettleMarketKey = fail(400, "The pasted PSBT carries a signature under the market's key. Sign only your own copy downloaded from this page, in the wallet that holds your escrow key, and paste it unchanged. Nothing was changed.")
	errSettleOtherKey  = fail(400, "The pasted PSBT carries a signature from a key other than your escrow key for this order. Sign only in the wallet that holds the escrow key you registered. Nothing was changed.")
	errSettleUnsigned  = fail(400, "The pasted PSBT does not carry your signature on every input. Sign your copy in the wallet that holds your escrow key and paste the result. Nothing was changed.")
	errSettleSighash   = fail(400, "A signature uses a sighash type other than ALL. Sign with the default sighash type (ALL). Nothing was changed.")
	errSettleBoth      = fail(400, "Paste the signed PSBT or upload its file, not both. Nothing was changed.")
)

// settleKeysUnavailable is what the order page says of a per-order escrow's settlement (or fee bump) whose keys cannot be
// rebuilt from the stored order wallet keys (errPerOrderXpub): no copy is offered to sign.
const settleKeysUnavailable = "This transaction cannot be signed right now: the market cannot rebuild the keys of the address it spends from its records. Nothing has been sent; market staff should check it."

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

// siblingKeys is the three keys of a per-order escrow's sibling address /branch/idx (A-456b, QA5W3A-2): each party's
// key and the market's at /<i>/branch/idx, derived in Go from its per-order extended key (perOrderXpub, as checkout
// stored it: QA5W4A-5, the market's since QA5W5A-1, so the escrow signer is never asked), with the origin of its key in
// the escrow (base, escrowKeys) and that suffix. Their 2-of-3 must pay address, the sibling as checkout stored it once
// Core had derived it from the same keys; otherwise errPerOrderXpub.
func (a *App) siblingKeys(ctx context.Context, base map[string]escrowKeyRow, escrowID int64, branch, idx int, address string) (map[string]escrowKeyRow, error) {
	out := map[string]escrowKeyRow{}
	var leaves [][]byte
	for _, party := range []string{roleBuyer, roleVendor, "market"} {
		k := base[party]
		steps, err := parseOriginPath(k.Suffix)
		if err != nil || len(steps) != 3 {
			return nil, errPerOrderXpub
		}
		order, err := a.perOrderXpub(ctx, escrowID, party)
		if err != nil {
			return nil, err
		}
		leaf, ok := perOrderSibling(order, uint32(branch), uint32(idx))
		if !ok {
			return nil, errPerOrderXpub
		}
		k.Suffix, k.PubKey = "/"+strconv.FormatUint(uint64(steps[0]), 10)+"/"+strconv.Itoa(branch)+"/"+strconv.Itoa(idx), bytes.Clone(leaf.PubKey[:])
		out[party] = k
		leaves = append(leaves, k.PubKey)
	}
	sum := sha256.Sum256(sortedMultisigScript(leaves...))
	if spk, err := escrowAddressScript(a.escrowNetwork(), address); err != nil || !bytes.Equal(spk, append([]byte{0, 32}, sum[:]...)) {
		return nil, errPerOrderXpub
	}
	return out, nil
}

// settlementKeys is the three keys of the script settlement sid of order spends (A-456b): the escrow's own (escrowKeys)
// or, for a stray refund of deposits to a per-order escrow's sibling address, that sibling's (siblingKeys). Every
// deposit a settlement claims is at one address, the one script its build chose (buildStray); a settlement claiming
// deposits at two is refused. A settlement that claims none yet (still being built) has the escrow's keys.
func (a *App) settlementKeys(ctx context.Context, q rowsQuerier, orderID string, sid int64) (map[string]escrowKeyRow, error) {
	keys, err := escrowKeys(ctx, q, orderID)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT p.address,s.branch,s.idx,s.escrow_id
		FROM payments p LEFT JOIN escrow_siblings s ON s.address=p.address AND s.escrow_id=(SELECT id FROM escrows WHERE order_id=p.order_id AND coin='BTC')
		WHERE p.settlement_id=$1 AND p.order_id=$2`, sid, orderID)
	if err != nil {
		return nil, err
	}
	n := 0
	var addr string
	var branch, idx, escrowID sql.NullInt64
	for rows.Next() {
		if err = rows.Scan(&addr, &branch, &idx, &escrowID); err != nil {
			rows.Close()
			return nil, err
		}
		n++
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	switch {
	case n > 1:
		return nil, errors.New("escrow: a settlement claims deposits at more than one address")
	case n == 0 || !branch.Valid:
		return keys, nil
	}
	return a.siblingKeys(ctx, keys, escrowID.Int64, int(branch.Int64), int(idx.Int64), addr)
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
	case stateResolved, stateCompleted:
		var err error
		if outcome, err = escrowDisputeOutcome(ctx, tx, o.ID); err != nil {
			return err
		}
		// Ordinary completed escrow orders already have their release path.
		// A buyer's conceded dispute reaches completed with an explicit
		// release outcome and still needs the amount check and settlement
		// note used by a staff decision for the vendor.
		if o.State == stateCompleted && outcome != "release" {
			return nil
		}
	default:
		return nil
	}
	var sum int64
	var n int
	// A deposit to a shape-(b) escrow's sibling address is never the order's (A-456a): the stray refund returns it.
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(amount),0),count(*) `+escrowCountedFrom, o.ID).Scan(&sum, &n); err != nil {
		return err
	}
	// Existing staff resolutions with no deposits keep their original behavior.
	// A concession explicitly confirms even zero, so a deposit seen after its
	// form was shown must refuse the transition before closing the dispute.
	if concession, ok := ctx.Value(concessionTransitionKey{}).(concessionTransition); n > 0 || ok && concession.orderID == o.ID {
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
		note := "Dispute decided for the vendor: the vendor claims the escrowed " + amount(sum, 8) + " BTC from the order page with a fresh address. The market builds that settlement (less the market fee and the network fee; " + escrowExcessRule("BTC") + "), the buyer or the vendor signs it, and the market co-signs it. Nothing is sent yet."
		switch {
		case o.Currency == "XMR" && liveKind == "release":
			// P3-R7, A-405: the release requested at shipment is the decision's settlement; the Monero escrow watcher alone
			// builds and sends it (escrow_xmr_settle.go), now that the order's outcome allows it.
			note = "Dispute decided for the vendor: the release to the vendor requested when the order was shipped pays the claim address the vendor gave then (less the market fee and the network fee; " + escrowExcessRule("XMR") + "). The market builds it from this order's Monero escrow account and sends it. The release deadline no longer applies. Nothing is sent yet."
		case o.Currency == "XMR":
			note = "Dispute decided for the vendor: the vendor claims the escrowed " + amount(sum, 12) + " XMR from the order page with a fresh Monero address. The market then builds that settlement from this order's escrow account (less the market fee and the network fee; " + escrowExcessRule("XMR") + ") and sends it. Nothing is sent yet."
		case liveKind == "release":
			// P2-R4: the release built at shipment is the decision's settlement; no new claim is asked for.
			note = "Dispute decided for the vendor: the release to the vendor built when the order was shipped pays the claim address the vendor gave then (less the market fee and the network fee; " + escrowExcessRule("BTC") + "). A signature already given on it counts; the buyer or the vendor signs it if neither has, and the market co-signs it. The release deadline no longer applies."
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
		note := "The release to the vendor built when the order was shipped is cancelled: it was not final, and any signature on it no longer counts."
		if _, ok := ctx.Value(holdRefundKey{}).(banRefund); ok {
			// A-652f: the release held for the banned vendor (a claim or the request at shipment), never built.
			note = "The release to the vendor, held while the vendor's account is closed, is cancelled: the market never built or sent it."
		}
		notes = append(notes, note)
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
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		e := c.A.escrow.Load()
		if e == nil {
			return actionResult{}, errEscrowDown
		}
		dest, ok := escrowAddressForm(e.network, c.Form.Get("address"))
		if !ok {
			return actionResult{}, errClaimAddress(c.A.escrowNetwork())
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
		case !o.btcEscrow():
			return actionResult{}, errNoOrder
		case o.VendorID != c.User.ID:
			return actionResult{}, errClaimOnlyVendor
		case !settlementFits("release", o.State, outcome):
			return actionResult{}, errClaimState
		}
		if err = checkClaimAddress(c, e, o.ID, dest); err != nil {
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
	Address       string // the escrow address, or a per-order escrow's sibling address (A-456a)
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
// transaction (unsigned txid), nothing is finalized, no signature is under the market's key, every signature is the
// party's own and ends in SIGHASH_ALL (as does any sighash-type field), and every input carries the party's signature.
// It returns the party's signature on each input; nothing else of the paste is kept, which is why a derivation entry
// (under the market's key or any other) is not refused (P4-R13, relaxed by A-456b for per-order wallets).
func checkSettlementPaste(pasted, txid string, party, market escrowKeyRow) ([]psbtSig, error) {
	return checkSettlementPasteLimit(pasted, txid, party, market, false)
}

func checkSettlementPasteLimit(pasted, txid string, party, market escrowKeyRow, hardware bool) ([]psbtSig, error) {
	var p *psbtPacket
	var err error
	if hardware {
		p, err = decodeHardwarePSBT(pasted)
	} else {
		p, err = decodePSBT(pasted)
	}
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
		// P4-R13, relaxed by A-456b: a derivation entry is never refused, the market key's included (Electrum and Sparrow
		// write one for every key of a multisig): nothing of a paste but the party's signatures is kept, so it is inert.
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
	// A-456b: the signed PSBT is pasted or uploaded as a file (psbt_upload.go), never both.
	pasted, file := strings.TrimSpace(c.Form.Get("psbt")), strings.TrimSpace(c.Form.Get(psbtFileField))
	switch {
	case pasted != "" && file != "":
		return actionResult{}, errSettleBoth
	case file != "":
		pasted = file
	}
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
	keys, err := c.A.settlementKeys(ctx, c.Tx, o.ID, sid)
	if errors.Is(err, errPerOrderXpub) {
		log.Printf("escrow: settlement of order %s not signed: %s", shortID(o.ID), errorCause(err))
		return actionResult{}, errSettleKeys
	}
	if err != nil {
		return actionResult{}, err
	}
	sp, err := decodePSBT(stored)
	if err != nil || sp.TxID() != txid {
		return actionResult{}, errors.New("escrow: the stored settlement PSBT does not match its txid")
	}
	sigs, err := checkSettlementPasteLimit(pasted, txid, keys[party], keys["market"], c.R.URL.Path == "/escrow/hardware-sign")
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
	if _, err = insertNotification(ctx, c.Tx, counterparty, "Order "+shortID(o.ID)+": "+what+notSent, o.ID); err != nil {
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
		a.refuse(w, r, u, 404, pageNotFound)
		return
	}
	ctx := r.Context()
	o, err := scanOrder(a.db.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 AND (o.buyer_id=$2 OR p.vendor_id=$2)", r.URL.Query().Get("id"), u.ID))
	sid, perr := strconv.ParseInt(r.URL.Query().Get("s"), 10, 64)
	if err == sql.ErrNoRows || err == nil && (!o.btcEscrow() || perr != nil) {
		a.refuse(w, r, u, 404, pageNotFound)
		return
	}
	var stored string
	reader, file := roleVendor, "escrow-settlement-"
	if u.ID == o.BuyerID {
		reader = roleBuyer
	}
	bump := r.URL.Query().Get("b")
	hardware := r.URL.Query().Get("hardware")
	if hardware != "" && hardware != "1" {
		a.refuse(w, r, u, 404, pageNotFound)
		return
	}
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
			a.refuse(w, r, u, 404, pageNotFound)
			return
		}
		file = "escrow-fee-bump-"
		err = a.db.QueryRowContext(ctx, `SELECT b.psbt FROM settlement_bumps b JOIN settlements s ON s.id=b.settlement_id WHERE b.id=$1 AND s.id=$2 AND s.order_id=$3 AND s.state='broadcast'
			AND (b.state='awaiting_signature' AND settlement_claimant(s.kind)=$4 OR b.state='party_signed' AND settlement_claimant(s.kind)<>$4 AND b.signer=settlement_claimant(s.kind)
			AND NOT EXISTS(SELECT 1 FROM settlement_bump_signatures g WHERE g.bump_id=b.id AND g.party=$4))`, bid, sid, o.ID, reader).Scan(&stored)
	}
	if err == sql.ErrNoRows {
		a.refuse(w, r, u, 404, pageNotFound)
		return
	}
	var copyText string
	if err == nil {
		var keys map[string]escrowKeyRow
		if keys, err = a.settlementKeys(ctx, a.db, o.ID, sid); err == nil {
			if hardware == "1" {
				e := a.escrow.Load()
				if e == nil {
					err = errHardwareFunding
				} else {
					copyText, err = hardwarePSBTCopy(ctx, stored, keys[reader], func(ctx context.Context, txid string) (string, error) {
						tx, e := e.walletTx(ctx, txid)
						if e != nil || !tx.Found {
							return "", errHardwareFunding
						}
						return tx.Hex, nil
					})
				}
			} else {
				copyText, err = settlementCopy(stored, keys[reader])
			}
		}
	}
	if errors.Is(err, errPerOrderXpub) {
		// QA5W5A-1: a fixed refusal, never a 500 (the order page offers no copy then either).
		log.Printf("escrow: settlement copy of order %s not served: %s", shortID(o.ID), errorCause(err))
		http.Error(w, settleKeysUnavailable, http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		writeEscrowDownloadFailure(w, r, err)
		return
	}
	if hardware == "1" {
		p, err := decodeHardwarePSBT(copyText)
		if err != nil {
			serverError(w, r, 503, "Service unavailable", errorCause(err))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="escrow-hardware-`+file+shortID(o.ID)+`-`+reader+`.psbt"`)
		w.Write(p.Raw)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+shortID(o.ID)+`-`+reader+`.psbt.txt"`)
	w.Write([]byte(copyText + "\n"))
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
