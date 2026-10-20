package market

import (
	"bytes"
	"context"
	"database/sql"
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
	registerAction("/escrow/bump", actionSpec{OwnTx: true, OneAtATime: true, Roles: escrowPartyRoles,
		ConfirmedFormNonce: true, ReceiptAuthorize: btcBumpReceiptAuthorize, Off: escrowOff, Run: escrowBumpAction})
	registerAction("/escrow/bump-sign", actionSpec{FormNonce: true, Off: escrowOff, Run: escrowBumpSignAction})
	registerAction("/escrow/hardware-bump-sign", actionSpec{FormNonce: true, Off: escrowOff, Run: escrowBumpSignAction})
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
	rate, err := strconv.ParseInt(strings.TrimSpace(c.Form.Get("fee_rate")), 10, 64)
	if err != nil || rate < 1 || rate > escrowMaxFeeRate {
		return actionResult{}, errBumpRate
	}
	sid, err := strconv.ParseInt(c.Form.Get("settlement_id"), 10, 64)
	if err != nil {
		return actionResult{}, errSettleNone
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		if c.A.escrow.Load() == nil {
			return actionResult{}, errEscrowDown
		}
		ctx := c.Ctx()
		o, err := partyOrderLocked(c, c.Form.Get("order_id"))
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
		if _, err = insertNotification(ctx, c.Tx, counterparty, "Order "+shortID(o.ID)+": "+note, o.ID); err != nil {
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
	keys, err := c.A.settlementKeys(ctx, c.Tx, o.ID, sid) // a sibling stray refund's bump: the sibling's keys (A-456b)
	if errors.Is(err, errPerOrderXpub) {
		log.Printf("escrow: fee bump of order %s not signed: %s", shortID(o.ID), errorCause(err))
		return actionResult{}, errSettleKeys // QA5W5A-1: never a 500
	}
	if err != nil {
		return actionResult{}, err
	}
	if sp, err := decodePSBT(stored); err != nil || sp.TxID() != txid {
		return actionResult{}, errors.New("escrow: the stored fee bump PSBT does not match its txid")
	}
	pasted := c.Form.Get("psbt")
	if c.R.URL.Path == "/escrow/hardware-bump-sign" {
		if pasted != "" && c.Form.Get(psbtFileField) != "" {
			return actionResult{}, errSettleBoth
		}
		if pasted == "" {
			pasted = c.Form.Get(psbtFileField)
		}
	}
	sigs, err := checkSettlementPasteLimit(pasted, txid, keys[party], keys["market"], c.R.URL.Path == "/escrow/hardware-bump-sign")
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
	if _, err = insertNotification(ctx, c.Tx, counterparty, "Order "+shortID(o.ID)+": "+what, o.ID); err != nil {
		return actionResult{}, err
	}
	return res, nil
}
