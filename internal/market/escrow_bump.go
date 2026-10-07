package market

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
)

// Same-recipient fee bumps of a broadcast escrow settlement (WR-escrow Phase 2, slice P2-d; P2-R7, A-375). A settlement
// broadcast at a fee rate too low to be mined can be replaced (BIP125: createpsbt marks every input replaceable) by a
// new transaction that pays the same outputs with only the claimant's output smaller, so the network fee is higher.
// Every rule of ES-2 applies to it as to a settlement; the settlement's own row never changes (migration 080):
//
//  1. The claimant (the vendor of a release, the buyer of a refund or stray refund: the network fee comes from that
//     output) asks for it on the order page with a new fee rate and their password (/escrow/bump, RPC-free). The rate
//     must beat the highest stored transaction of the settlement by BIP125's increment, stay within the fee caps and
//     leave the claimant's output at or above the floor (planBump). A newer request closes an open bump.
//  2. The escrow watcher builds it (buildBumps) only while the settlement is broadcast and none of its stored
//     transactions is in a block: from the settlement's stored PSBT, with only the claimant output's value changed
//     (bumpPSBT), checked by checkBump (the same inputs, sequences and output scripts in the same order, every output
//     but the claimant's unchanged, the claimant's smaller, the fee higher by the increment, within the caps, the
//     claimant at or above the floor).
//  3. The claimant signs it first (/escrow/bump-sign, ES-2's paste checks; the pasted PSBT is never stored); then the
//     market co-signs it with the claimant's signature, or the other party signs it too and the market does not. The
//     other party's signature is refused until the claimant has signed, and the market never completes a bump with the
//     other party's signature alone (cosignBump). Every signature of the settlement is void for it: its sighash differs.
//  4. At finalize the watcher checks it again (checkBump, checkFinalTx, every stored transaction still unconfirmed,
//     testmempoolaccept at the rate cap), stores the final transaction and sends it (broadcastBumps, through
//     trackBroadcast).
//
// The watcher's own transactions for a settlement are its own and every bump it built (settlementAttempts, A-382): a
// conflict among them is not a spend outside the market (A-375), only the highest-fee stored one is sent again, a built
// bump found on the network without a stored final transaction is checked and recorded (storeSeenBump), and when a
// bump confirms the settlement is confirmed with that bump recorded as confirmed (escrow_cosign.go). A bump's rate is
// capped so its fee stays within escrowMaxFeeRate at the smallest real size (bumpMaxRate, A-383). While the restore gate
// is set no bump is built, co-signed or sent, and scripts/restore.sh holds a restored bump with a final transaction as
// it holds a settlement (escrow_recovery.go).

func init() {
	registerAction("/escrow/bump", actionSpec{OwnTx: true, OneAtATime: true, Off: escrowOff, Run: escrowBumpAction})
	registerAction("/escrow/bump-sign", actionSpec{Off: escrowOff, Run: escrowBumpSignAction})
}

// escrowBumpIncrement is BIP125's minimum fee increase per vB of the replacement (Bitcoin Core's default incremental
// relay fee, 1 sat/vB, or less in recent releases): a bump pays at least this much more per vB of the settlement's size
// bound than the stored transaction it replaces. With whole sat/vB rates on the same size bound, a higher rate meets it.
const escrowBumpIncrement = 1

// Fixed classes of a fee bump that fails a check (settlement_bumps.build_error and send_error, logs, refusals).
const (
	bumpInputs     = "bump-inputs"     // not the settlement's inputs, sequences, version and lock time, in its order
	bumpRecipients = "bump-recipients" // not the settlement's output scripts, in its order
	bumpOutputs    = "bump-outputs"    // an output other than the claimant's changed, or the claimant's is not smaller
	bumpFeeLow     = "fee-not-higher"  // the fee does not beat the stored transaction it replaces by the increment
	bumpSettled    = "settled"         // the settlement is no longer broadcast, or a transaction of it is in a block
)

var (
	errBumpNone     = fail(404, "Fee bump not found")
	errBumpNotReady = fail(409, "This fee bump is not waiting for your signature (it is still being built, it was already signed, or it was closed). Reload the page. Nothing was changed.")
	errBumpState    = fail(409, "The network fee of a settlement can be raised only while its transaction is broadcast and unconfirmed. Reload the page. Nothing was changed.")
	errBumpRate     = fail(400, "Enter the new network fee rate as a whole number of sat/vB, at least 1 and no higher than the highest rate the form shows. Nothing was changed.")
)

// claimantOf is the party whose output pays the network fee of a settlement of kind: the vendor of a release, the buyer
// of a refund or stray refund (migration 080's settlement_claimant).
func claimantOf(kind string) string {
	if kind == "release" {
		return roleVendor
	}
	return roleBuyer
}

// bumpAttempt is one of the market's own transactions for a settlement: its own (Bump 0) or a fee bump with a final
// transaction.
type bumpAttempt struct {
	Bump           int64
	N              int
	TxID, Raw      string
	Fee, Rate      int64
	State, SendErr string
}

// settlementAttempts lists the market's own transactions for settlement id (A-375): its own final transaction, then
// each fee bump of it the market built, in order (A-382: whether or not its final transaction is stored, Raw "" then;
// the parties may have signed and broadcast it themselves, or a restore lost its final transaction).
func settlementAttempts(ctx context.Context, q rowsQuerier, id int64) ([]bumpAttempt, error) {
	rows, err := q.QueryContext(ctx, `SELECT 0,0,txid,raw_tx,network_fee,fee_rate,state,send_error FROM settlements WHERE id=$1 AND raw_tx IS NOT NULL
		UNION ALL SELECT id,n,txid,COALESCE(raw_tx,''),network_fee,fee_rate,state,send_error FROM settlement_bumps WHERE settlement_id=$1 AND txid IS NOT NULL ORDER BY 2`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []bumpAttempt
	for rows.Next() {
		var at bumpAttempt
		if err = rows.Scan(&at.Bump, &at.N, &at.TxID, &at.Raw, &at.Fee, &at.Rate, &at.State, &at.SendErr); err != nil {
			return nil, err
		}
		out = append(out, at)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 || out[0].Bump != 0 {
		return nil, errors.New("escrow: the settlement has no final transaction")
	}
	return out, nil
}

// topAttempt is the index of the stored transaction (one with a final transaction) with the highest fee (the latest of
// equal ones): the only one the market sends again, and the one a new bump must beat.
func topAttempt(at []bumpAttempt) int {
	top := 0
	for i := range at {
		if at[i].Raw != "" && at[i].Fee >= at[top].Fee {
			top = i
		}
	}
	return top
}

// attemptsUnconfirmed reports whether none of the stored transactions is in a block or conflicted with one in a block
// (gettransaction in the watch-only wallet): the settlement is still waiting to be mined, so a bump may replace it.
func (a *App) attemptsUnconfirmed(ctx context.Context, e *escrowBTC, at []bumpAttempt) (bool, error) {
	for _, x := range at {
		w, err := e.walletTx(ctx, x.TxID)
		if err != nil {
			return false, err
		}
		if w.Found && w.Confirmations != 0 {
			return false, nil
		}
	}
	return true, nil
}

// planBump is the plan of a fee bump at rate of a settlement with these frozen outputs (the claimant's among them),
// nIn inputs and network fee parentFee, replacing a stored transaction with fee prevFee at prevRate: the outputs with
// only the claimant's smaller by the extra fee, and the fee. It refuses (a class) a rate not above prevRate or a fee
// short of BIP125's increment over prevFee, a fee above the caps and a claimant's output below floor.
func planBump(chain string, outs []settleOutput, nIn int, parentFee, prevFee, prevRate, rate, floor int64) ([]settleOutput, int64, string) {
	vsize := settleVSize(nIn, len(outs))
	fee := rate * vsize
	switch {
	case rate <= prevRate || fee < prevFee+escrowBumpIncrement*vsize:
		return nil, fee, bumpFeeLow
	case rate > bumpMaxRate(chain, nIn, outs):
		return nil, fee, settleFeeCap
	}
	next := bumpOutputsAt(outs, parentFee, fee)
	if next == nil {
		return nil, fee, bumpOutputs
	}
	for _, o := range next {
		if o.Role == "claimant" && o.Sats < floor {
			return nil, fee, settleTooSmall
		}
	}
	return next, fee, ""
}

// settleSigMin is the shortest signature the paste checks accept: a strict-DER signature (BIP 66: at least 8 bytes)
// and its sighash byte.
const settleSigMin = 9

// settleVSizeMin is a lower bound of the real virtual size of a settlement (or fee bump) with nIn 2-of-3 P2WSH inputs
// and these outputs (A-383): the exact fields outside the witness (each output's script from its address; 22 bytes,
// the shortest, when it does not decode) and each input's witness with two signatures of settleSigMin bytes, rounded
// down. A bump's network fee is its rate on settleVSize's upper bound, so a fee within escrowMaxFeeRate at this size is
// within it at any size the signed transaction can have, which finalize and testmempoolaccept check.
func settleVSizeMin(chain string, nIn int, outs []settleOutput) int64 {
	base := int64(4+4+len(compactSize(nIn))+len(compactSize(len(outs)))) + 41*int64(nIn)
	for _, o := range outs {
		n := 22
		if script, err := escrowAddressScript(chain, o.Address); err == nil {
			n = len(script)
		}
		base += int64(8 + len(compactSize(n)) + n)
	}
	witness := 2 + int64(nIn)*(1+1+2*(1+settleSigMin)+1+105)
	return (4*base + witness) / 4
}

// bumpMaxRate is the highest fee rate (sat/vB) a fee bump of a settlement with nIn inputs and these outputs may ask
// for: its fee (the rate on settleVSize's bound) stays within escrowMaxFee, and within escrowMaxFeeRate at the
// smallest real size the transaction can have (settleVSizeMin, A-383).
func bumpMaxRate(chain string, nIn int, outs []settleOutput) int64 {
	bound := settleVSize(nIn, len(outs))
	return min(int64(escrowMaxFeeRate), escrowMaxFeeRate*settleVSizeMin(chain, nIn, outs)/bound, escrowMaxFee/bound)
}

// bumpOutputsAt is a bump's outputs at network fee fee: the settlement's, the claimant's smaller by fee - parentFee
// (nil when there is no claimant's output).
func bumpOutputsAt(outs []settleOutput, parentFee, fee int64) []settleOutput {
	next := slices.Clone(outs)
	i := slices.IndexFunc(next, func(o settleOutput) bool { return o.Role == "claimant" })
	if i < 0 {
		return nil
	}
	next[i].Sats -= fee - parentFee
	return next
}

// checkBump checks a fee bump's unsigned transaction against its settlement's (P2-R7), at build and again at finalize:
// the same version, lock time and inputs (outpoints and sequences) in the same order; the same output scripts in the
// same order; every output but the claimant's (the market fee, the buyer's return) unchanged and the claimant's
// smaller; a fee (inputTotal less the outputs) higher than prevFee, the stored transaction it replaces, by BIP125's
// increment on the settlement's size bound; within escrowMaxFee and escrowMaxFeeRate; the claimant's output at or above
// floor. It returns the fee and a fixed class ("" when it passes).
func checkBump(chain string, parent, bump psbtTx, parentOuts []settleOutput, inputTotal, prevFee, floor int64) (int64, string) {
	if bump.Version != parent.Version || bump.LockTime != parent.LockTime || !slices.Equal(bump.In, parent.In) {
		return 0, bumpInputs
	}
	if len(bump.Out) != len(parent.Out) || len(parentOuts) != len(parent.Out) {
		return 0, bumpRecipients
	}
	for i := range parent.Out {
		if !bytes.Equal(bump.Out[i].Script, parent.Out[i].Script) {
			return 0, bumpRecipients
		}
	}
	claimant := -1
	var paid int64
	for i, o := range parentOuts {
		switch {
		case o.Role == "claimant":
			claimant = i
			if bump.Out[i].Value >= parent.Out[i].Value {
				return 0, bumpOutputs
			}
		case bump.Out[i].Value != parent.Out[i].Value:
			return 0, bumpOutputs
		}
		paid += bump.Out[i].Value
	}
	if claimant < 0 {
		return 0, bumpOutputs
	}
	vsize := settleVSize(len(parent.In), len(parent.Out))
	fee := inputTotal - paid
	switch {
	case fee <= prevFee || fee-prevFee < escrowBumpIncrement*vsize:
		return fee, bumpFeeLow
	case fee > escrowMaxFee || fee > escrowMaxFeeRate*vsize || fee > escrowMaxFeeRate*settleVSizeMin(chain, len(parent.In), parentOuts):
		return fee, settleFeeCap
	case bump.Out[claimant].Value < floor:
		return fee, settleTooSmall
	}
	return fee, ""
}

// serializeUnsignedTx is a transaction in the serialization without witnesses, as a PSBT holds it.
func serializeUnsignedTx(tx psbtTx) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(tx.Version))
	b = append(b, compactSize(len(tx.In))...)
	for _, in := range tx.In {
		b = append(b, in.TxID[:]...)
		b = binary.LittleEndian.AppendUint32(b, in.Vout)
		b = append(b, 0)
		b = binary.LittleEndian.AppendUint32(b, in.Sequence)
	}
	b = append(b, compactSize(len(tx.Out))...)
	for _, o := range tx.Out {
		b = binary.LittleEndian.AppendUint64(b, uint64(o.Value))
		b = append(b, compactSize(len(o.Script))...)
		b = append(b, o.Script...)
	}
	return binary.LittleEndian.AppendUint32(b, tx.LockTime)
}

// bumpPSBT is a fee bump's PSBT: the settlement's stored PSBT (every input map, with its witness UTXO and witness
// script, and every output map byte for byte) with only the output values of its unsigned transaction set to outs.
func bumpPSBT(stored *psbtPacket, outs []settleOutput) ([]byte, error) {
	if len(outs) != len(stored.Tx.Out) || !bytes.Equal(serializeUnsignedTx(stored.Tx), stored.UnsignedTx) {
		return nil, errors.New("escrow: the stored settlement transaction cannot be rebuilt")
	}
	tx := stored.Tx
	tx.Out = slices.Clone(tx.Out)
	for i, o := range outs {
		tx.Out[i].Value = o.Sats
	}
	r := &psbtReader{b: stored.Raw, off: 5}
	global, end, err := r.readMap()
	if err != nil {
		return nil, err
	}
	out := []byte("psbt\xff")
	for _, kv := range global {
		val := kv[1]
		if bytes.Equal(kv[0], []byte{0x00}) {
			val = serializeUnsignedTx(tx)
		}
		out = append(out, compactSize(len(kv[0]))...)
		out = append(out, kv[0]...)
		out = append(out, compactSize(len(val))...)
		out = append(out, val...)
	}
	return append(append(out, 0), stored.Raw[end+1:]...), nil
}

// bumpErrorText is what the order page and a refusal say about a fee bump that failed a check.
func bumpErrorText(class, claimant string) string {
	switch class {
	case bumpFeeLow:
		return "The fee rate is not high enough to replace the transaction already sent."
	case settleFeeCap:
		return "At that rate the network fee could exceed the market's fee cap (" + amount(escrowMaxFee, 8) + " BTC, and " + strconv.Itoa(escrowMaxFeeRate) + " sat/vB at the smallest size the signed transaction can have)."
	case settleTooSmall:
		return "At that rate the " + claimant + "'s output would fall below the minimum output of " + amount(minPayoutBTC, 8) + " BTC."
	case bumpSettled:
		return "The settlement's transaction is already in a block (or the settlement is no longer waiting to be mined), so there is nothing to replace."
	case "":
		return ""
	}
	return "The fee bump failed the market's checks, so nothing was sent."
}

// escrowBumpAction: the claimant of a broadcast escrow settlement asks to raise its network fee (P2-R7), giving the new
// rate and confirming with their password (and authenticator code when enrolled). RPC-free: the escrow watcher builds
// the bump (buildBumps). An open bump of the settlement is closed in the same transaction.
func escrowBumpAction(c *actionCtx) (actionResult, error) {
	if c.A.escrow.Load() == nil {
		return actionResult{}, errEscrowDown
	}
	rate, err := strconv.ParseInt(strings.TrimSpace(c.Form.Get("fee_rate")), 10, 64)
	if err != nil || rate < 1 || rate > escrowMaxFeeRate {
		return actionResult{}, errBumpRate
	}
	sid, err := strconv.ParseInt(c.Form.Get("settlement_id"), 10, 64)
	if err != nil {
		return actionResult{}, errSettleNone
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		o, err := partyOrder(c, c.Form.Get("order_id"))
		if err != nil {
			return actionResult{}, err
		}
		if !o.btcEscrow() {
			return actionResult{}, errNoOrder
		}
		var kind, state, stored string
		var parentFee int64
		err = c.Tx.QueryRowContext(ctx, "SELECT kind,state,COALESCE(psbt,''),COALESCE(network_fee,0) FROM settlements WHERE id=$1 AND order_id=$2 FOR UPDATE", sid, o.ID).Scan(&kind, &state, &stored, &parentFee)
		if err == sql.ErrNoRows {
			return actionResult{}, errSettleNone
		}
		if err != nil {
			return actionResult{}, err
		}
		claimant, party := claimantOf(kind), roleBuyer
		if c.User.ID == o.VendorID {
			party = roleVendor
		}
		if party != claimant {
			return actionResult{}, fail(403, "Only the "+claimant+" raises the network fee of "+settlementKind(kind)+": the fee comes from the "+claimant+"'s output. Nothing was changed.")
		}
		if state != "broadcast" {
			return actionResult{}, errBumpState
		}
		sp, err := decodePSBT(stored)
		if err != nil {
			return actionResult{}, err
		}
		attempts, err := settlementAttempts(ctx, c.Tx, sid)
		if err != nil {
			return actionResult{}, err
		}
		outs, err := settlementOutputs(ctx, c.Tx, sid)
		if err != nil {
			return actionResult{}, err
		}
		prev := attempts[topAttempt(attempts)]
		switch _, _, class := planBump(c.A.escrowNetwork(), outs, len(sp.Tx.In), parentFee, prev.Fee, prev.Rate, rate, payoutFloor("BTC", kind)); class {
		case "":
		case bumpFeeLow:
			return actionResult{}, fail(400, "Enter a fee rate higher than the "+strconv.FormatInt(prev.Rate, 10)+" sat/vB of the transaction already sent: a replacement must pay a higher fee. Nothing was changed.")
		case settleFeeCap:
			// A-383: the cap holds at the smallest size the signed transaction can have, so a bump never waits forever.
			return actionResult{}, fail(400, bumpErrorText(class, claimant)+" The highest fee rate this settlement can take is "+strconv.FormatInt(bumpMaxRate(c.A.escrowNetwork(), len(sp.Tx.In), outs), 10)+" sat/vB. Nothing was changed.")
		default:
			return actionResult{}, fail(400, bumpErrorText(class, claimant)+" Nothing was changed.")
		}
		res, err := c.Tx.ExecContext(ctx, "UPDATE settlement_bumps SET state='superseded',updated=now() WHERE settlement_id=$1 AND state IN ('building','awaiting_signature','party_signed')", sid)
		if err != nil {
			return actionResult{}, err
		}
		closed, _ := res.RowsAffected()
		if _, err = c.Tx.ExecContext(ctx, `INSERT INTO settlement_bumps(settlement_id,n,requested_by,fee_rate) SELECT $1,COALESCE(max(n),0)+1,$2,$3 FROM settlement_bumps WHERE settlement_id=$1`,
			sid, c.User.ID, rate); err != nil {
			return actionResult{}, err
		}
		note := "The " + claimant + " asked to raise the network fee of the escrow settlement (" + settlementKind(kind) + ") to " + strconv.FormatInt(rate, 10) +
			" sat/vB: the market builds a fee bump, a new transaction paying the same outputs with only the " + claimant + "'s output smaller by the extra fee. The " + claimant +
			" signs it first; then the market co-signs it, or the other party may sign it too. Nothing is sent yet."
		if closed > 0 {
			note += " The earlier fee bump that was not sent was closed."
		}
		counterparty := o.BuyerID
		if party == roleBuyer {
			counterparty = o.VendorID
		}
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", o.ID, o.State, c.User.ID, note); err != nil {
			return actionResult{}, err
		}
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), counterparty, "Order "+shortID(o.ID)+": "+note); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: orderPage(o.ID) + settlementAnchor(kind, sid), Notice: "escrow-bump",
			Audit: "Asked for a fee bump of the escrow settlement of order " + shortID(o.ID) + " to " + strconv.FormatInt(rate, 10) + " sat/vB (" + c.A.escrowNetwork() + ")"}, nil
	})
}

// settlementAnchor is the order-page anchor of a settlement's section.
func settlementAnchor(kind string, id int64) string {
	if kind == "stray_refund" {
		return "#escrow-settlement-" + strconv.FormatInt(id, 10)
	}
	return "#escrow-settlement"
}

// settlementOutputs reads a settlement's frozen outputs, in transaction order.
func settlementOutputs(ctx context.Context, q rowsQuerier, id int64) ([]settleOutput, error) {
	rows, err := q.QueryContext(ctx, "SELECT role,address,sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var outs []settleOutput
	for rows.Next() {
		var o settleOutput
		if err = rows.Scan(&o.Role, &o.Address, &o.Sats); err != nil {
			return nil, err
		}
		outs = append(outs, o)
	}
	return outs, rows.Err()
}

// escrowBumpSignAction: a party pastes their signed copy of a fee bump (P2-R7). The claimant signs first; the other
// party may sign once the claimant has (the market then does not sign). Every ES-2 check runs before anything is
// written, and only the party's signature on each input is kept, never the pasted PSBT.
func escrowBumpSignAction(c *actionCtx) (actionResult, error) {
	ctx := c.Ctx()
	o, err := partyOrder(c, c.Form.Get("order_id"))
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
	bid, err := strconv.ParseInt(c.Form.Get("bump_id"), 10, 64)
	if err != nil {
		return actionResult{}, errBumpNone
	}
	var sid int64
	var kind, state, txid, stored, signer string
	var signed bool
	err = c.Tx.QueryRowContext(ctx, `SELECT b.settlement_id,s.kind,b.state,COALESCE(b.txid,''),COALESCE(b.psbt,''),COALESCE(b.signer,''),
		EXISTS(SELECT 1 FROM settlement_bump_signatures g WHERE g.bump_id=b.id AND g.party=$3)
		FROM settlement_bumps b JOIN settlements s ON s.id=b.settlement_id WHERE b.id=$1 AND s.order_id=$2 FOR UPDATE OF b`, bid, o.ID, party).Scan(&sid, &kind, &state, &txid, &stored, &signer, &signed)
	if err == sql.ErrNoRows {
		return actionResult{}, errBumpNone
	}
	if err != nil {
		return actionResult{}, err
	}
	claimant := claimantOf(kind)
	second := state == "party_signed" && party != claimant && signer == claimant
	switch {
	case signed:
		return actionResult{}, errSettleSigned
	case state == "awaiting_signature" && party != claimant:
		return actionResult{}, fail(409, "The "+claimant+" signs a fee bump of "+settlementKind(kind)+" first: the network fee comes from the "+claimant+"'s output. Once the "+claimant+" has signed it, you may sign it too. Nothing was changed.")
	case state != "awaiting_signature" && !second:
		return actionResult{}, errBumpNotReady
	}
	keys, err := settlementKeys(ctx, c.Tx, o.ID, sid) // a sibling stray refund's bump: the sibling's keys (A-456b)
	if err != nil {
		return actionResult{}, err
	}
	if sp, err := decodePSBT(stored); err != nil || sp.TxID() != txid {
		return actionResult{}, errors.New("escrow: the stored fee bump PSBT does not match its txid")
	}
	sigs, err := checkSettlementPaste(c.Form.Get("psbt"), txid, keys[party], keys["market"])
	if err != nil {
		return actionResult{}, err
	}
	for i, s := range sigs {
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO settlement_bump_signatures(bump_id,input_n,party,pubkey,signature) VALUES($1,$2,$3,$4,$5)",
			bid, i, party, hex.EncodeToString(s.PubKey), hex.EncodeToString(s.Sig)); err != nil {
			return actionResult{}, err
		}
	}
	if !second {
		res, err := c.Tx.ExecContext(ctx, "UPDATE settlement_bumps SET state='party_signed',signer=$2,signed_at=now(),updated=now() WHERE id=$1 AND state='awaiting_signature' AND txid=$3", bid, party, txid)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return actionResult{}, errBumpNotReady
		}
	}
	what := "The " + party + " signed the fee bump of the escrow settlement (" + settlementKind(kind) + "). " + settleSigChecked +
		" Nothing has been sent yet: the market co-signs it on its next pass and broadcasts it in place of the transaction already sent, unless the other party signs it too."
	res := actionResult{Redirect: orderPage(o.ID) + settlementAnchor(kind, sid), Notice: "escrow-bump-signed", Audit: "Signed a fee bump of the escrow settlement of order " + shortID(o.ID) + " (" + c.A.escrowNetwork() + ")"}
	if second {
		what = "The " + party + " also signed the fee bump of the escrow settlement (" + settlementKind(kind) + "): with both parties' signatures the market does not sign it. " + settleSigChecked +
			" Nothing has been sent yet: the market broadcasts the transaction the two parties signed on its next pass, in place of the transaction already sent."
		res.Notice = "escrow-bump-both"
	}
	if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", o.ID, o.State, c.User.ID, what); err != nil {
		return actionResult{}, err
	}
	counterparty := o.BuyerID
	if party == roleBuyer {
		counterparty = o.VendorID
	}
	if _, err = c.Tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), counterparty, "Order "+shortID(o.ID)+": "+what); err != nil {
		return actionResult{}, err
	}
	return res, nil
}

// buildBumps builds every requested fee bump (step 2 of the file comment; not while the restore gate is set).
func (a *App) buildBumps(ctx context.Context, e *escrowBTC) error {
	return a.eachSettlement(ctx, "fee bump not built", "SELECT settlement_id FROM settlement_bumps WHERE state='building' ORDER BY settlement_id",
		func(id int64) error { return a.buildBump(ctx, e, id) })
}

func (a *App) buildBump(ctx context.Context, e *escrowBTC, sid int64) error {
	var bid, rate int64
	var orderID string
	err := a.db.QueryRowContext(ctx, "SELECT b.id,b.fee_rate,s.order_id FROM settlement_bumps b JOIN settlements s ON s.id=b.settlement_id WHERE b.settlement_id=$1 AND b.state='building'", sid).Scan(&bid, &rate, &orderID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	p, err := a.loadSettleBuild(ctx, sid, "broadcast")
	if err != nil {
		return err
	}
	if p == nil {
		return a.failBump(ctx, bid, orderID, bumpSettled)
	}
	attempts, err := settlementAttempts(ctx, a.db, sid)
	if err != nil {
		return err
	}
	if ok, err := a.attemptsUnconfirmed(ctx, e, attempts); err != nil || !ok {
		if err == nil {
			err = a.failBump(ctx, bid, orderID, bumpSettled)
		}
		return err
	}
	prev, floor := attempts[topAttempt(attempts)], payoutFloor("BTC", p.Kind)
	outs, fee, class := planBump(e.network, p.Outputs, len(p.Stored.Tx.In), p.NetworkFee, prev.Fee, prev.Rate, rate, floor)
	if class != "" {
		return a.failBump(ctx, bid, orderID, class)
	}
	raw, err := bumpPSBT(p.Stored, outs)
	if err != nil {
		return err
	}
	bp, err := parsePSBT(raw)
	if err != nil {
		return err
	}
	if got, class := checkBump(e.network, p.Stored.Tx, bp.Tx, p.Outputs, p.InputTotal, prev.Fee, floor); class != "" || got != fee {
		return errors.New("escrow: a built fee bump failed its own check: " + class)
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE settlement_bumps SET state='awaiting_signature',psbt=$2,txid=$3,network_fee=$4,build_error='',updated=now() WHERE id=$1 AND state='building'",
		bid, encodePSBT(raw), bp.TxID(), fee)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	claimant := claimantOf(p.Kind)
	what := settlementKind(p.Kind)
	note := "Fee bump built for the escrow settlement (" + what + "): network fee " + amount(fee, 8) + " BTC at " + strconv.FormatInt(rate, 10) + " sat/vB, taken from the " +
		claimant + "'s output; every other output is unchanged. The " + claimant + " signs it first. Nothing is sent yet."
	if err = settleNote(ctx, tx, orderID, note, "the fee bump of its escrow settlement ("+what+") is ready: the "+claimant+" checks its outputs on the order page and signs it first."); err != nil {
		return err
	}
	return tx.Commit()
}

// failBump ends a fee bump that cannot be built (building -> failed), with an order note and a notice to the claimant's
// counterparty and the claimant.
func (a *App) failBump(ctx context.Context, bid int64, orderID, class string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind string
	if err = tx.QueryRowContext(ctx, "SELECT s.kind FROM settlement_bumps b JOIN settlements s ON s.id=b.settlement_id WHERE b.id=$1", bid).Scan(&kind); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "UPDATE settlement_bumps SET state='failed',build_error=$2,updated=now() WHERE id=$1 AND state='building'", bid, class)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	text := "The fee bump of the escrow settlement (" + settlementKind(kind) + ") was not built. " + bumpErrorText(class, claimantOf(kind)) + " Nothing was sent."
	if err = settleNote(ctx, tx, orderID, text, text); err != nil {
		return err
	}
	log.Printf("escrow: fee bump for order %s not built: %s", shortID(orderID), class)
	return tx.Commit()
}

// bumpBuild is a party-signed fee bump as the co-sign step reads it: the bump as a settlement (its PSBT, txid, fee,
// outputs, signer and party signatures, the claimant's first), its settlement and the stored transactions it replaces.
type bumpBuild struct {
	*settleBuild
	BumpID   int64
	Parent   *settleBuild
	Attempts []bumpAttempt
	PrevFee  int64
}

// loadBump reads the party-signed fee bump of a broadcast settlement of a closed order, or nil when there is none.
func (a *App) loadBump(ctx context.Context, sid int64) (*bumpBuild, error) {
	p, err := a.loadSettleBuild(ctx, sid, "broadcast")
	if err != nil || p == nil {
		return nil, err
	}
	b := &bumpBuild{Parent: p, settleBuild: &settleBuild{ID: sid, OrderID: p.OrderID, Kind: p.Kind, InputTotal: p.InputTotal, MarketIndex: p.MarketIndex, Keys: p.Keys}}
	var stored string
	err = a.db.QueryRowContext(ctx, "SELECT id,psbt,txid,signer,network_fee FROM settlement_bumps WHERE settlement_id=$1 AND state='party_signed'", sid).Scan(
		&b.BumpID, &stored, &b.TxID, &b.Signer, &b.NetworkFee)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if b.Stored, err = decodePSBT(stored); err != nil || b.Stored.TxID() != b.TxID {
		return nil, errors.New("escrow: the stored fee bump PSBT does not match its txid")
	}
	if b.Outputs = bumpOutputsAt(p.Outputs, p.NetworkFee, b.NetworkFee); b.Outputs == nil {
		return nil, errors.New("escrow: the settlement has no claimant's output")
	}
	if b.Attempts, err = settlementAttempts(ctx, a.db, sid); err != nil {
		return nil, err
	}
	b.PrevFee = b.Attempts[topAttempt(b.Attempts)].Fee
	sigs := map[string][]psbtSig{}
	rows, err := a.db.QueryContext(ctx, "SELECT party,input_n,pubkey,signature FROM settlement_bump_signatures WHERE bump_id=$1 ORDER BY party,input_n", b.BumpID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var party, pub, sig string
		var n int
		if err = rows.Scan(&party, &n, &pub, &sig); err != nil {
			rows.Close()
			return nil, err
		}
		pk, err1 := hex.DecodeString(pub)
		sg, err2 := hex.DecodeString(sig)
		if err1 != nil || err2 != nil || n != len(sigs[party]) {
			rows.Close()
			return nil, errors.New("escrow: a stored fee bump signature is malformed")
		}
		sigs[party] = append(sigs[party], psbtSig{PubKey: pk, Sig: sg})
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	claimant := claimantOf(b.Kind)
	for _, party := range []string{claimant, otherParty(claimant)} {
		if len(sigs[party]) == len(b.Stored.Inputs) && slices.IndexFunc(sigs[party], func(s psbtSig) bool { return !slices.Equal(s.PubKey, b.Keys[party].PubKey) }) < 0 {
			b.Parties = append(b.Parties, partySigs{party, sigs[party]})
		}
	}
	if b.Signer != claimant || len(b.Parties) == 0 || b.Parties[0].Party != claimant {
		return nil, errors.New("escrow: a party-signed fee bump does not have its claimant's signatures")
	}
	return b, nil
}

// cosignBumps completes every party-signed fee bump of a broadcast settlement of a closed order (steps 3 and 4 of the
// file comment).
func (a *App) cosignBumps(ctx context.Context, e *escrowBTC) error {
	return a.eachSettlement(ctx, "fee bump not co-signed", `SELECT s.id FROM settlements s JOIN settlement_bumps b ON b.settlement_id=s.id JOIN orders o ON o.id=s.order_id
		WHERE b.state='party_signed' AND s.state='broadcast' AND o.state IN ('completed','cancelled','resolved') ORDER BY s.id`, func(id int64) error { return a.cosignBump(ctx, e, id) })
}

// cosignBump completes one party-signed fee bump. With both parties' signatures it is finalized without the market;
// otherwise, or when those two do not verify together, the market co-signs it with the claimant's signature only: a
// bump is never completed by the other party and the market. A claimant's signature that does not verify is discarded
// with any other party's, and the bump waits for the claimant again.
func (a *App) cosignBump(ctx context.Context, e *escrowBTC, sid int64) error {
	b, err := a.loadBump(ctx, sid)
	if err != nil || b == nil {
		return err
	}
	// P2-R7 at finalize: the bump's shape and fee again, and every stored transaction of the settlement still unconfirmed.
	if _, class := checkBump(e.network, b.Parent.Stored.Tx, b.Stored.Tx, b.Parent.Outputs, b.InputTotal, b.PrevFee, payoutFloor("BTC", b.Kind)); class != "" {
		return a.bumpSendWaits(ctx, b.BumpID, b.OrderID, class)
	}
	if ok, err := a.attemptsUnconfirmed(ctx, e, b.Attempts); err != nil || !ok {
		return err // a stored transaction is in a block: trackBroadcast settles it and closes this bump
	}
	claimant := b.Parties[0]
	var discard []string
	if len(b.Parties) == 2 {
		both, err := withPartySigs(b.Stored, b.Parties...)
		if err != nil {
			return err
		}
		rawHex, complete, err := e.finalizePSBT(ctx, both)
		if err != nil {
			return err
		}
		if complete {
			verdict, class, err := a.acceptBump(ctx, e, b, rawHex)
			switch {
			case err != nil:
				return err
			case verdict == acceptOK:
				return a.storeBumpFinal(ctx, b, rawHex, "parties", []string{b.Parties[0].Party, b.Parties[1].Party}, nil)
			case verdict == acceptWait:
				return a.bumpSendWaits(ctx, b.BumpID, b.OrderID, class)
			}
		}
		// The two do not verify together: the market co-signs with the claimant's alone, and the other party's goes.
		discard = []string{b.Parties[1].Party}
	}
	// The arbiter wallet signs a Bitcoin Core escrow's bump, the escrow signer a per-order escrow's (marketSign, A-456b).
	signed, class, err := a.marketSign(ctx, e, b.settleBuild)
	if err != nil {
		return err
	}
	if class != "" {
		return a.bumpSendWaits(ctx, b.BumpID, b.OrderID, class)
	}
	claimantCopy, err := withPartySigs(b.Stored, claimant)
	if err != nil {
		return err
	}
	combined, err := e.combinePSBT(ctx, []string{signed, claimantCopy})
	if err != nil {
		return err
	}
	if cp, err := decodePSBT(combined); err != nil || cp.TxID() != b.TxID {
		return a.bumpSendWaits(ctx, b.BumpID, b.OrderID, sendBadReply)
	}
	rawHex, complete, err := e.finalizePSBT(ctx, combined)
	if err != nil {
		return err
	}
	if !complete {
		return a.revertBump(ctx, b)
	}
	verdict, class, err := a.acceptBump(ctx, e, b, rawHex)
	switch {
	case err != nil:
		return err
	case verdict == acceptOK:
		return a.storeBumpFinal(ctx, b, rawHex, "market", []string{claimant.Party}, discard)
	case verdict == acceptBadSig:
		return a.revertBump(ctx, b)
	}
	return a.bumpSendWaits(ctx, b.BumpID, b.OrderID, class)
}

// acceptBump re-checks a finalized fee bump in Go (checkFinalTx: its stored transaction and txid, its outputs, its fee
// within the caps, every witness signature SIGHASH_ALL), then asks the node with testmempoolaccept at the rate cap,
// which applies the replacement rules against the transaction already sent. It never broadcasts.
func (a *App) acceptBump(ctx context.Context, e *escrowBTC, b *bumpBuild, rawHex string) (acceptVerdict, string, error) {
	if class := checkFinalTx(e.network, rawHex, b.Stored, b.TxID, b.Outputs, b.InputTotal, b.NetworkFee); class != "" {
		return acceptWait, class, nil
	}
	allowed, reject, err := e.testMempoolAccept(ctx, rawHex, escrowMaxFeeRate)
	switch {
	case err != nil:
		return 0, "", err
	case allowed:
		return acceptOK, "", nil
	case strings.Contains(reject, "script-verify-flag"):
		return acceptBadSig, "", nil
	}
	return acceptWait, sendRejected, nil
}

// storeBumpFinal stores a checked final fee bump before it is sent: party_signed -> signed with raw_tx, by
// compare-and-set under the settlement's row lock (which must still be broadcast), the signatures of each party in
// discard removed first, an order note and a notice to both parties.
func (a *App) storeBumpFinal(ctx context.Context, b *bumpBuild, rawHex, cosigned string, used, discard []string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var parentState, state string
	if err = tx.QueryRowContext(ctx, "SELECT state FROM settlements WHERE id=$1 FOR UPDATE", b.ID).Scan(&parentState); err != nil || parentState != "broadcast" {
		return err
	}
	if err = tx.QueryRowContext(ctx, "SELECT state FROM settlement_bumps WHERE id=$1 FOR UPDATE", b.BumpID).Scan(&state); err != nil || state != "party_signed" {
		return err
	}
	var have int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM settlement_bump_signatures WHERE bump_id=$1 AND party=ANY($2)", b.BumpID, used).Scan(&have); err != nil || have != len(used)*len(b.Stored.Inputs) {
		return err
	}
	var notes []string
	for _, party := range discard {
		if _, err = tx.ExecContext(ctx, "DELETE FROM settlement_bump_signatures WHERE bump_id=$1 AND party=$2", b.BumpID, party); err != nil {
			return err
		}
		notes = append(notes, discardedBumpNote(party))
	}
	res, err := tx.ExecContext(ctx, "UPDATE settlement_bumps SET state='signed',raw_tx=$2,cosigned=$3,final_at=now(),send_error='',updated=now() WHERE id=$1 AND state='party_signed' AND txid=$4",
		b.BumpID, rawHex, cosigned, b.TxID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	what := settlementKind(b.Kind)
	note := "The market co-signed the fee bump of the escrow settlement (" + what + ") with the " + used[0] + "'s signature. The transaction was checked and stored, and the market broadcasts it now in place of the transaction already sent."
	if cosigned == "parties" {
		note = "The buyer and the vendor both signed the fee bump of the escrow settlement (" + what + "), so the market did not sign it. The transaction was checked and stored, and the market broadcasts it now in place of the transaction already sent."
	}
	notes = append(notes, note)
	for i, n := range notes {
		body := ""
		if i == len(notes)-1 {
			body = strings.Join(notes, " ")
		}
		if err = settleNote(ctx, tx, b.OrderID, n, body); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// discardedBumpNote is the fixed note on a fee bump signature that did not verify (A-339 for bumps).
func discardedBumpNote(party string) string {
	return "The " + party + "'s signature on the fee bump did not verify when the market finalized the transaction: it is not a valid signature of this transaction by the " + party + "'s escrow key for this order. It was discarded."
}

// revertBump returns a party-signed fee bump whose claimant's signature did not verify to awaiting a signature: every
// signature on it is removed (the other party's cannot complete it without the claimant's), the signer cleared, a fixed
// note and a notice to both parties.
func (a *App) revertBump(ctx context.Context, b *bumpBuild) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, "SELECT state FROM settlement_bumps WHERE id=$1 FOR UPDATE", b.BumpID).Scan(&state); err != nil || state != "party_signed" {
		return err
	}
	var others int
	claimant := claimantOf(b.Kind)
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM settlement_bump_signatures WHERE bump_id=$1 AND party<>$2", b.BumpID, claimant).Scan(&others); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM settlement_bump_signatures WHERE bump_id=$1", b.BumpID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE settlement_bumps SET state='awaiting_signature',signer=NULL,signed_at=NULL,send_error='',updated=now() WHERE id=$1 AND state='party_signed'", b.BumpID); err != nil {
		return err
	}
	notes := []string{discardedBumpNote(claimant)}
	if others > 0 {
		notes = append(notes, "The "+otherParty(claimant)+"'s signature on it was removed too: a fee bump needs the "+claimant+"'s signature first.")
	}
	notes = append(notes, "The fee bump waits for the "+claimant+"'s signature again. Nothing was sent.")
	for i, n := range notes {
		body := ""
		if i == len(notes)-1 {
			body = strings.Join(notes, " ")
		}
		if err = settleNote(ctx, tx, b.OrderID, n, body); err != nil {
			return err
		}
	}
	log.Printf("escrow: fee bump for order %s: the claimant's signature did not verify; it waits for a signature again", shortID(b.OrderID))
	return tx.Commit()
}

// bumpSendWaits records why a fee bump is not sent yet (it stays where it is and is tried again next pass), never over
// a restore's hold. The class is logged once, when it changes.
func (a *App) bumpSendWaits(ctx context.Context, bid int64, orderID, class string) error {
	res, err := a.db.ExecContext(ctx, "UPDATE settlement_bumps SET send_error=$2,updated=now() WHERE id=$1 AND send_error<>$2 AND send_error NOT LIKE $3", bid, class, restoredHoldPrefix+"%")
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		log.Printf("escrow: fee bump for order %s not sent yet: %s", shortID(orderID), class)
	}
	return nil
}

// storeSeenBump records a built fee bump of broadcast settlement sid whose transaction (rawHex) the watch-only wallet
// holds although the market stored no final transaction for it (A-382, A-344 for bumps): the buyer and the vendor
// signed the copies they downloaded and broadcast it themselves, or a restored database lost its final transaction. It
// is checked like any final transaction (checkFinalTx against the bump's stored PSBT, its outputs and fee) and stored,
// cosigned 'parties' when its witnesses carry both parties' stored signatures and 'network' otherwise, and the bump
// moves to signed and broadcast in one transaction (migration 082), with an honest note. It reports whether it stored
// it; a transaction that fails the checks is not recorded (the bump's send_error says why) and is not called external.
func (a *App) storeSeenBump(ctx context.Context, sid, bid int64, rawHex string) (bool, error) {
	p, err := a.loadSettleBuild(ctx, sid, "broadcast")
	if err != nil || p == nil {
		return false, err
	}
	b := &settleBuild{ID: sid, OrderID: p.OrderID, Kind: p.Kind, InputTotal: p.InputTotal, Keys: p.Keys}
	var stored string
	if err = a.db.QueryRowContext(ctx, "SELECT psbt,txid,network_fee FROM settlement_bumps WHERE id=$1 AND settlement_id=$2 AND raw_tx IS NULL AND txid IS NOT NULL", bid, sid).Scan(
		&stored, &b.TxID, &b.NetworkFee); err == sql.ErrNoRows {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if b.Stored, err = decodePSBT(stored); err != nil || b.Stored.TxID() != b.TxID {
		return false, errors.New("escrow: the stored fee bump PSBT does not match its txid")
	}
	if b.Outputs = bumpOutputsAt(p.Outputs, p.NetworkFee, b.NetworkFee); b.Outputs == nil {
		return false, errors.New("escrow: the settlement has no claimant's output")
	}
	if class := checkFinalTx(a.escrowNetwork(), rawHex, b.Stored, b.TxID, b.Outputs, b.InputTotal, b.NetworkFee); class != "" {
		return false, a.bumpSendWaits(ctx, bid, b.OrderID, class)
	}
	// Who signed it: the parties whose stored signatures are in its witnesses.
	sigs := map[string][]psbtSig{}
	rows, err := a.db.QueryContext(ctx, "SELECT party,pubkey,signature FROM settlement_bump_signatures WHERE bump_id=$1 ORDER BY party,input_n", bid)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var party, pub, sig string
		if err = rows.Scan(&party, &pub, &sig); err != nil {
			rows.Close()
			return false, err
		}
		pk, _ := hex.DecodeString(pub)
		sg, _ := hex.DecodeString(sig)
		sigs[party] = append(sigs[party], psbtSig{PubKey: pk, Sig: sg})
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, err
	}
	for _, party := range []string{roleBuyer, roleVendor} {
		if len(sigs[party]) == len(b.Stored.Inputs) {
			b.Parties = append(b.Parties, partySigs{party, sigs[party]})
		}
	}
	seen := signersSeen(rawHex, b)
	cosigned, who := "network", "Who signed it is not recorded"
	switch len(seen) {
	case 2:
		cosigned, who = "parties", "The buyer and the vendor both signed it (their signatures are the ones the market stored)"
	case 1:
		who = "It carries the " + seen[0] + "'s signature; who signed it besides the " + seen[0] + " is not recorded"
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var parentState string
	if err = tx.QueryRowContext(ctx, "SELECT state FROM settlements WHERE id=$1 FOR UPDATE", sid).Scan(&parentState); err != nil || parentState != "broadcast" {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE settlement_bumps SET state='signed',raw_tx=$2,cosigned=$3,final_at=now(),send_error='',updated=now()
		WHERE id=$1 AND raw_tx IS NULL AND state IN ('awaiting_signature','party_signed','superseded') AND txid=$4`, bid, rawHex, cosigned, b.TxID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil
	}
	if _, err = tx.ExecContext(ctx, "UPDATE settlement_bumps SET state='broadcast',broadcast_at=now(),updated=now() WHERE id=$1 AND state='signed'", bid); err != nil {
		return false, err
	}
	what, net := settlementKind(b.Kind), a.escrowNetwork()
	note := "The transaction of a fee bump of the escrow settlement (" + what + ") was already on the " + net + " network when the market came to follow it. " + who +
		". The market found it in its watch-only wallet, checked it against the fee bump it built and recorded it as broadcast; the market did not sign or broadcast it."
	if err = settleNote(ctx, tx, b.OrderID, note, "a fee bump of its escrow settlement ("+what+") was already on the "+net+" network; the order page shows its transaction and its confirmations."); err != nil {
		return false, err
	}
	log.Printf("escrow: fee bump for order %s: its transaction was already on the network; recorded as broadcast", shortID(b.OrderID))
	return true, tx.Commit()
}

// broadcastBumps sends each newly stored fee bump at once (trackBroadcast sends the highest-fee stored transaction).
func (a *App) broadcastBumps(ctx context.Context, e *escrowBTC) error {
	return a.eachSettlement(ctx, "fee bump not broadcast", `SELECT DISTINCT s.id FROM settlements s JOIN settlement_bumps b ON b.settlement_id=s.id
		WHERE s.state='broadcast' AND b.state='signed' ORDER BY s.id`, func(id int64) error { return a.trackBroadcast(ctx, e, id) })
}

// attemptSent records a send of a stored transaction (sendStored's answer): an administrator's confirmation is used up
// (sentOnce), a fee bump sent for the first time moves signed -> broadcast with an order note and a notice, and a bump
// the node refused records why.
func (a *App) attemptSent(ctx context.Context, id int64, orderID, kind string, at bumpAttempt, sent bool) error {
	if err := a.sentOnce(ctx, id); err != nil || at.Bump == 0 {
		return err
	}
	if !sent {
		return a.bumpSendWaits(ctx, at.Bump, orderID, sendRejected)
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE settlement_bumps SET send_error='',updated=now() WHERE id=$1 AND send_error<>'' AND send_error NOT LIKE $2", at.Bump, restoredHoldPrefix+"%"); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "UPDATE settlement_bumps SET state='broadcast',broadcast_at=now(),updated=now() WHERE id=$1 AND state='signed'", at.Bump)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		what := settlementKind(kind)
		if err = settleNote(ctx, tx, orderID, "The fee bump of the escrow settlement ("+what+") was broadcast to the "+a.escrowNetwork()+
			" network: it replaces the transaction sent before, with a higher network fee. The order page shows its transaction.",
			"the fee bump of its escrow settlement ("+what+") was broadcast to the "+a.escrowNetwork()+" network; the order page shows its transaction."); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// bumpView adds a settlement's fee bumps to its view (newest first, with each one's outputs while it is open), the
// transaction broadcast or confirmed when it is a bump's, and for the claimant of a broadcast settlement the fee bump
// form; for a party while a bump waits for their signature, their copy and the paste form.
func (a *App) bumpView(ctx context.Context, d *PageData, v *EscrowOrderView, s *EscrowSettlementView, id int64, kind, state, parentTxID, parentSendErr string, parentFee int64) error {
	if state != "broadcast" && state != "confirmed" && state != "external" {
		return nil
	}
	claimant := claimantOf(kind)
	outs, err := settlementOutputs(ctx, a.db, id)
	if err != nil {
		return err
	}
	// The bump whose transaction the settlement shows: the confirmed one, or the highest-fee one broadcast.
	var shown int64
	if err = a.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT id FROM settlement_bumps WHERE settlement_id=$1 AND state=$2 AND $2 IN ('broadcast','confirmed')
		ORDER BY network_fee DESC,n DESC LIMIT 1),0)`, id, state).Scan(&shown); err != nil {
		return err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id,n,state,fee_rate,COALESCE(network_fee,0),COALESCE(psbt,''),COALESCE(txid,''),COALESCE(raw_tx,''),COALESCE(signer,''),
		build_error,send_error,COALESCE(to_char(signed_at,'YYYY-MM-DD HH24:MI "UTC"'),''),EXISTS(SELECT 1 FROM settlement_bump_signatures g WHERE g.bump_id=b.id AND g.party=$2),
		(SELECT count(DISTINCT party)=2 FROM settlement_bump_signatures g WHERE g.bump_id=b.id)
		FROM settlement_bumps b WHERE settlement_id=$1 ORDER BY n DESC LIMIT 10`, id, d.OrderViewer)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		bv := &EscrowBumpView{SettlementID: id}
		var bstate, psbt, txid, raw, signer, buildErr, sendErr, signedAt string
		var rate, fee int64
		var readerSigned, both bool
		if err = rows.Scan(&bv.ID, &bv.N, &bstate, &rate, &fee, &psbt, &txid, &raw, &signer, &buildErr, &sendErr, &signedAt, &readerSigned, &both); err != nil {
			return err
		}
		bv.FeeRate = strconv.FormatInt(rate, 10)
		if fee > 0 {
			bv.NetworkFee = amount(fee, 8)
		}
		pending := ""
		if t := settleSendText(sendErr); t != "" {
			pending = " " + t + " The market tries again on each pass."
		}
		paused := " Paused: " + restoreWhy + ", so the market builds, co-signs and sends no fee bump until the operator has finished."
		switch bstate {
		case "building":
			bv.Status = "Requested: the market builds it on its next pass. Reload in a minute."
			if v.RestorePaused {
				bv.Status = strings.TrimSpace(paused)
			}
		case "awaiting_signature":
			bv.Status = "Waiting for the " + claimant + "'s signature: the " + claimant + " signs a fee bump first; then the market co-signs it, or the " + otherParty(claimant) + " may sign it too. Nothing has been sent yet."
		case "party_signed":
			who, next := "the "+claimant+" ("+signedAt+")", " The market co-signs it and broadcasts it on its next pass, in place of the transaction already sent."
			if both {
				who, next = "the "+claimant+" and the "+otherParty(claimant)+" (first signature "+signedAt+")", " The market does not sign it, and broadcasts it on its next pass, in place of the transaction already sent."
			}
			if v.RestorePaused {
				next = paused
			}
			bv.Status = "Signed by " + who + ". " + settleSigChecked + next + " Nothing has been sent yet." + pending
		case "signed":
			bv.Status = "Signed, checked and stored; the market broadcasts it on its next pass in place of the transaction already sent." + pending
			switch {
			case restoreHeld(sendErr):
				bv.Status = "Signed, checked and stored. " + restoreHeldText
			case v.RestorePaused:
				bv.Status = "Signed, checked and stored." + paused
			}
		case "broadcast":
			bv.Status = "Broadcast: transaction " + txid + " replaces the transaction sent before." + pending
			if restoreHeld(sendErr) {
				bv.Status = "Broadcast: transaction " + txid + ". " + restoreHeldText
			}
		case "confirmed":
			bv.Status = "Confirmed: transaction " + txid + "."
		case "failed":
			bv.Status = "Not built. " + bumpErrorText(buildErr, claimant) + " Nothing was sent."
		case "superseded":
			bv.Status = "Closed without being sent: a newer fee bump was requested, or the settlement was confirmed or spent first."
		}
		if (bstate == "signed" || bstate == "broadcast") && state != "broadcast" {
			bv.Status = "Not confirmed: another transaction of this settlement was confirmed instead, or the escrow's coins were spent outside the market, so this one can no longer confirm."
		}
		if bv.ID == shown {
			s.TxID, s.RawTx = txid, raw
			if state == "confirmed" {
				s.Status = "Confirmed on the " + a.escrowNetwork() + " network: transaction " + txid + " (fee bump " + strconv.Itoa(bv.N) + ", which replaced transaction " + parentTxID + ")."
			} else if !restoreHeld(sendErr) && !restoreHeld(parentSendErr) && parentSendErr != sendRebroadcastOK && !v.RestorePaused {
				s.Status = "Broadcast to the " + a.escrowNetwork() + " network: transaction " + txid + " (fee bump " + strconv.Itoa(bv.N) + " at " + bv.FeeRate + " sat/vB, which replaces transaction " +
					parentTxID + "). Waiting for " + strconv.FormatInt(v.Threshold, 10) + " confirmations."
			}
		}
		if fee > 0 && (bstate == "awaiting_signature" || bstate == "party_signed") {
			for _, o := range bumpOutputsAt(outs, parentFee, fee) {
				bv.Outputs = append(bv.Outputs, SettlementOutputView{Address: o.Address, Amount: amount(o.Sats, 8), Role: settlementOutputRole(kind, o.Role)})
			}
		}
		reader := d.OrderViewer
		canSign := bstate == "awaiting_signature" && reader == claimant || bstate == "party_signed" && reader == otherParty(claimant) && !readerSigned && signer == claimant
		if canSign && (reader == roleBuyer || reader == roleVendor) && a.escrowConfigured && state == "broadcast" {
			keys, err := settlementKeys(ctx, a.db, d.Order.ID, id)
			if err != nil {
				return err
			}
			if bv.PSBT, err = settlementCopy(psbt, keys[reader]); err != nil {
				return err
			}
			bv.CanSign, bv.SignToo = true, bstate == "party_signed"
		}
		s.Bumps = append(s.Bumps, bv)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	s.CanBump = state == "broadcast" && d.OrderViewer == claimant && a.escrowConfigured && v.Available
	if !s.CanBump {
		return nil
	}
	attempts, err := settlementAttempts(ctx, a.db, id)
	if err != nil {
		return err
	}
	var nIn int
	if err = a.db.QueryRowContext(ctx, "SELECT count(*) FROM payments WHERE settlement_id=$1", id).Scan(&nIn); err != nil {
		return err
	}
	// A-383: the form offers only rates the request accepts; none is left once the highest is already sent.
	top, highest := attempts[topAttempt(attempts)].Rate, bumpMaxRate(a.escrowNetwork(), nIn, outs)
	if s.CanBump = top < highest; !s.CanBump {
		return nil
	}
	s.BumpRate, s.BumpMin, s.BumpMax = strconv.FormatInt(top, 10), strconv.FormatInt(top+1, 10), strconv.FormatInt(highest, 10)
	if d.Security == nil {
		d.Security, _, _, err = a.securityView(ctx, d.User.ID)
	}
	return err
}
