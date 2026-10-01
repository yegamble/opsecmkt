package market

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
)

// Completing and sending escrow settlements (WR-escrow Phase 1, slice P1-c2; ES-1, ES-2, ES-4). After its builds, each
// escrow watcher pass:
//
//  1. Co-signs each party-signed settlement of an order in a terminal state (cosignSettlements). Only the watcher does
//     this, and only on the pristine stored PSBT: the market's copy (the stored PSBT plus the market key's origin) is
//     signed by the arbiter wallet with SIGHASH_ALL (walletprocesspsbt, "ALL" hard-coded), checked to carry exactly the
//     market's signature on every input, combined market copy first with the stored PSBT carrying the stored party
//     signatures, and finalized. When both parties signed, the two party signatures are finalized alone and the market
//     does not sign (ES-4, A-340). The final transaction is re-checked in Go (checkFinalTx: the stored transaction and
//     txid, the frozen outputs, the fee within the caps, every witness signature SIGHASH_ALL), then by testmempoolaccept
//     with an explicit maxfeerate, and stored (party_signed -> signed, raw_tx) before it is broadcast for the first
//     time. A market-signed PSBT is never stored or shown.
//  2. A party signature that does not verify (finalizepsbt incomplete although the market's signature is there, or
//     testmempoolaccept refusing a script) is discarded and the settlement returns to awaiting a signature, with a
//     fixed order note and a notice to both parties (A-339). When both parties signed and the two do not finalize
//     together, the market co-signs with whichever one verifies and discards the other; only when neither does is the
//     settlement returned.
//  3. Broadcasts each signed settlement's stored raw transaction, only that one (broadcastSettlements): an answer of
//     its txid, or -27 (already confirmed), is sent; it moves signed -> broadcast. A signed settlement is never rebuilt
//     or signed again; a failed broadcast is tried again with the same stored transaction on the next pass.
//  4. Follows each broadcast settlement in the watch-only wallet (trackBroadcasts): enough confirmations -> confirmed;
//     still unconfirmed -> the stored transaction is sent again (idempotent); spent by another transaction (a
//     conflict, or its outputs gone without it) -> external, flagged for staff.
//  5. Follows each confirmed settlement until its transaction has escrowReorgDepth confirmations (trackConfirmeds,
//     P1-d1): back at 0 confirmations (a reorganisation took it out of the chain) -> broadcast, and step 4 sends the
//     stored transaction again; conflicted (another transaction was mined instead) -> external.
//
// Nothing here runs in a form post: transitions stay RPC-free (ARCH-7).

// Fixed classes of a settlement that is not sent yet (settlements.send_error). They leave it where it is; the watcher
// tries again on every pass and logs a class when it changes.
const (
	sendMarketKey = "market-key-missing" // the arbiter wallet did not sign the market's copy (its key range was short)
	sendRejected  = "mempool-rejected"   // testmempoolaccept refused it for a reason other than a signature
	sendBadReply  = "node-bad-reply"     // the node's PSBT or transaction was not the stored one
)

// escrowKeypoolAhead is how far past a market child index the arbiter wallet's key range is extended before it signs
// (keypoolrefill): a fresh Core wallet derives 1000 keys per chain, and signing at index 5000 and 19000 after a refill
// was probed on regtest.
const escrowKeypoolAhead = 1000

// settlementFits reports whether a settlement of kind may be completed for an order in state with dispute outcome
// (ES-2: only in a terminal state, and the outcome the order reached): a release on a completed order or one resolved
// for the vendor, a refund on a cancelled order or one resolved for the buyer, a stray refund on any closed order.
func settlementFits(kind, state, outcome string) bool {
	switch kind {
	case "release":
		return state == stateCompleted || state == stateResolved && outcome == "release"
	case "refund":
		return state == stateCancelled || state == stateResolved && outcome == "refund"
	case "stray_refund":
		return isTerminal(state)
	}
	return false
}

// disputeOutcomeSQL is the outcome of the order o's dispute (empty if none; one dispute per order), for a query that has the orders row o.
const disputeOutcomeSQL = "COALESCE((SELECT d.outcome FROM disputes d WHERE d.order_id=o.id),'')"

// settlementIDs lists the settlements a query selects (an id column).
func (a *App) settlementIDs(ctx context.Context, query string, args ...any) ([]int64, error) {
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// eachSettlement runs step for every settlement of query, joining the errors as watcher errors labelled what.
func (a *App) eachSettlement(ctx context.Context, what, query string, step func(int64) error) error {
	ids, err := a.settlementIDs(ctx, query)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err = step(id); err != nil {
			errs = append(errs, &watchError{label: "escrow settlement " + strconv.FormatInt(id, 10), class: what + ": " + errorClassOf(err), err: err})
		}
	}
	return errors.Join(errs...)
}

// cosignSettlements completes every party-signed settlement of a closed order (step 1 of the file comment).
func (a *App) cosignSettlements(ctx context.Context, e *escrowBTC) error {
	return a.eachSettlement(ctx, "not co-signed", `SELECT s.id FROM settlements s JOIN orders o ON o.id=s.order_id
		WHERE s.state='party_signed' AND o.state IN ('completed','cancelled','resolved') ORDER BY s.id`, func(id int64) error { return a.cosignSettlement(ctx, e, id) })
}

// partySigs is one party's stored signature on each input of a settlement.
type partySigs struct {
	Party string
	Sigs  []psbtSig
}

// withPartySigs is the stored PSBT with these parties' signatures added to its inputs: what the parties signed,
// rebuilt from the stored copy (a pasted PSBT is never kept).
func withPartySigs(stored *psbtPacket, parties ...partySigs) (string, error) {
	raw := stored.Raw
	for _, p := range parties {
		for i, s := range p.Sigs {
			next, err := parsePSBT(raw)
			if err != nil {
				return "", err
			}
			if raw, err = next.withInputField(i, append([]byte{0x02}, s.PubKey...), s.Sig); err != nil {
				return "", err
			}
		}
	}
	return encodePSBT(raw), nil
}

// settleBuild is a stored settlement as the co-sign step reads it.
type settleBuild struct {
	ID                     int64
	OrderID, Kind, TxID    string
	Signer                 string
	Stored                 *psbtPacket
	NetworkFee, InputTotal int64
	MarketIndex            int64
	Outputs                []settleOutput
	Keys                   map[string]escrowKeyRow
	Parties                []partySigs // the parties with a signature on every input, the first signer first
}

// loadSettleBuild reads a settlement of a closed order in state want (party_signed, or awaiting_signature for
// trackAwaiting), or nil when it is no longer in that state (or its kind does not fit the order's outcome). Only a
// party-signed one is read with its party signatures.
func (a *App) loadSettleBuild(ctx context.Context, id int64, want string) (*settleBuild, error) {
	b := &settleBuild{ID: id}
	var state, stored, orderState, outcome string
	err := a.db.QueryRowContext(ctx, `SELECT s.order_id,s.kind,s.state,s.psbt,s.txid,COALESCE(s.signer,''),s.network_fee,s.input_total,o.state,`+disputeOutcomeSQL+`,es.market_index
		FROM settlements s JOIN orders o ON o.id=s.order_id JOIN escrows es ON es.order_id=s.order_id WHERE s.id=$1`, id).Scan(
		&b.OrderID, &b.Kind, &state, &stored, &b.TxID, &b.Signer, &b.NetworkFee, &b.InputTotal, &orderState, &outcome, &b.MarketIndex)
	if err != nil || state != want || !settlementFits(b.Kind, orderState, outcome) {
		return nil, err
	}
	if b.Stored, err = decodePSBT(stored); err != nil || b.Stored.TxID() != b.TxID {
		return nil, errors.New("escrow: the stored settlement PSBT does not match its txid")
	}
	if b.Keys, err = escrowKeys(ctx, a.db, b.OrderID); err != nil {
		return nil, err
	}
	rows, err := a.db.QueryContext(ctx, "SELECT role,address,sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var o settleOutput
		if err = rows.Scan(&o.Role, &o.Address, &o.Sats); err != nil {
			rows.Close()
			return nil, err
		}
		b.Outputs = append(b.Outputs, o)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if want != "party_signed" {
		return b, nil
	}
	sigs := map[string][]psbtSig{}
	rows, err = a.db.QueryContext(ctx, "SELECT party,input_n,pubkey,signature FROM settlement_signatures WHERE settlement_id=$1 ORDER BY party,input_n", id)
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
		p, err1 := hex.DecodeString(pub)
		s, err2 := hex.DecodeString(sig)
		if err1 != nil || err2 != nil || n != len(sigs[party]) {
			rows.Close()
			return nil, errors.New("escrow: a stored settlement signature is malformed")
		}
		sigs[party] = append(sigs[party], psbtSig{PubKey: p, Sig: s})
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, party := range []string{b.Signer, otherParty(b.Signer)} {
		if len(sigs[party]) == len(b.Stored.Inputs) && slices.IndexFunc(sigs[party], func(s psbtSig) bool { return !slices.Equal(s.PubKey, b.Keys[party].PubKey) }) < 0 {
			b.Parties = append(b.Parties, partySigs{party, sigs[party]})
		}
	}
	if len(b.Parties) == 0 || b.Parties[0].Party != b.Signer {
		return nil, errors.New("escrow: a party-signed settlement does not have its signer's signatures")
	}
	return b, nil
}

func otherParty(party string) string {
	if party == roleBuyer {
		return roleVendor
	}
	return roleBuyer
}

// acceptVerdict is what the final checks found about a finalized transaction.
type acceptVerdict int

const (
	acceptOK       acceptVerdict = iota
	acceptBadSig                 // testmempoolaccept refused a script: a party signature does not verify
	acceptWait                   // refused for another reason, recorded as class; tried again next pass
	acceptExternal               // an input is no longer unspent: spent by another transaction
)

// checkAndAccept re-checks a finalized transaction in Go (checkFinalTx), then asks the node with testmempoolaccept at
// the rate cap. It never broadcasts.
func (a *App) checkAndAccept(ctx context.Context, e *escrowBTC, b *settleBuild, rawHex string) (acceptVerdict, string, error) {
	if class := checkFinalTx(rawHex, b.Stored, b.TxID, b.Outputs, b.InputTotal, b.NetworkFee); class != "" {
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
	spent, err := a.inputsSpent(ctx, e, b.Stored)
	if err != nil {
		return 0, "", err
	}
	if spent {
		return acceptExternal, "", nil
	}
	return acceptWait, sendRejected, nil
}

// inputsSpent reports whether any input of the stored transaction is no longer unspent (gettxout, mempool included).
func (a *App) inputsSpent(ctx context.Context, e *escrowBTC, stored *psbtPacket) (bool, error) {
	for _, in := range stored.Tx.In {
		id := in.TxID
		slices.Reverse(id[:])
		u, err := e.utxo(ctx, hex.EncodeToString(id[:]), in.Vout)
		if err != nil {
			return false, err
		}
		if !u.Found {
			return true, nil
		}
	}
	return false, nil
}

// cosignSettlement completes one party-signed settlement (steps 1 and 2 of the file comment).
func (a *App) cosignSettlement(ctx context.Context, e *escrowBTC, id int64) error {
	b, err := a.loadSettleBuild(ctx, id, "party_signed")
	if err != nil || b == nil {
		return err
	}
	candidates := []string{b.Parties[0].Party}
	if len(b.Parties) == 2 {
		// ES-4, A-340: both parties signed; their two signatures complete it and the market does not sign.
		both, err := withPartySigs(b.Stored, b.Parties...)
		if err != nil {
			return err
		}
		rawHex, complete, err := e.finalizePSBT(ctx, both)
		if err != nil {
			return err
		}
		if complete {
			verdict, class, err := a.checkAndAccept(ctx, e, b, rawHex)
			switch {
			case err != nil:
				return err
			case verdict == acceptOK:
				return a.storeFinal(ctx, b, rawHex, "parties", []string{b.Parties[0].Party, b.Parties[1].Party}, nil)
			case verdict == acceptExternal:
				return a.spentBeforeCosign(ctx, e, b)
			case verdict == acceptWait:
				return a.settlementSendWaits(ctx, id, b.OrderID, class)
			}
		}
		// One of the two does not verify: the market co-signs with whichever one does (A-339).
		candidates = append(candidates, b.Parties[1].Party)
	}
	if err = a.ensureArbiterKey(ctx, e, b); err != nil {
		var re *escrowRPCError
		if errors.As(err, &re) && re.Class == sendMarketKey {
			return a.settlementSendWaits(ctx, id, b.OrderID, sendMarketKey)
		}
		return err
	}
	copyText, err := settlementCopy(encodePSBT(b.Stored.Raw), b.Keys["market"])
	if err != nil {
		return err
	}
	signed, err := e.arbiterSign(ctx, copyText)
	if err != nil {
		return err
	}
	if class := checkMarketSigned(signed.PSBT, b); class != "" {
		return a.settlementSendWaits(ctx, id, b.OrderID, class)
	}
	var bad []string
	for _, party := range candidates {
		i := slices.IndexFunc(b.Parties, func(p partySigs) bool { return p.Party == party })
		partyCopy, err := withPartySigs(b.Stored, b.Parties[i])
		if err != nil {
			return err
		}
		combined, err := e.combinePSBT(ctx, []string{signed.PSBT, partyCopy})
		if err != nil {
			return err
		}
		if cp, err := decodePSBT(combined); err != nil || cp.TxID() != b.TxID {
			return a.settlementSendWaits(ctx, id, b.OrderID, sendBadReply)
		}
		rawHex, complete, err := e.finalizePSBT(ctx, combined)
		if err != nil {
			return err
		}
		if !complete {
			bad = append(bad, party)
			continue
		}
		verdict, class, err := a.checkAndAccept(ctx, e, b, rawHex)
		switch {
		case err != nil:
			return err
		case verdict == acceptOK:
			return a.storeFinal(ctx, b, rawHex, "market", []string{party}, slices.DeleteFunc(slices.Clone(candidates), func(p string) bool { return p == party }))
		case verdict == acceptBadSig:
			bad = append(bad, party)
		case verdict == acceptExternal:
			return a.spentBeforeCosign(ctx, e, b)
		default:
			return a.settlementSendWaits(ctx, id, b.OrderID, class)
		}
	}
	return a.revertSettlement(ctx, b, bad)
}

// checkMarketSigned checks the arbiter wallet's answer: the stored transaction, and on every input exactly one
// signature, the market key's, ending in SIGHASH_ALL. A copy the arbiter did not sign (its key range did not reach the
// market's child key) is sendMarketKey.
func checkMarketSigned(b64 string, b *settleBuild) string {
	p, err := decodePSBT(b64)
	if err != nil || p.TxID() != b.TxID || len(p.Inputs) != len(b.Stored.Inputs) {
		return sendBadReply
	}
	for _, in := range p.Inputs {
		if len(in.PartialSigs) == 0 {
			return sendMarketKey
		}
		if in.Finalized || len(in.PartialSigs) != 1 || !slices.Equal(in.PartialSigs[0].PubKey, b.Keys["market"].PubKey) || in.PartialSigs[0].SighashByte() != 0x01 {
			return sendBadReply
		}
	}
	return ""
}

// ensureArbiterKey makes sure the arbiter wallet can sign for the settlement's market child key before it is asked
// to: the key's P2WPKH address (deriveaddresses) must be the wallet's (getaddressinfo ismine). If it is not, the
// wallet's key range is extended to escrowKeypoolAhead past the market index (keypoolrefill) and checked again.
func (a *App) ensureArbiterKey(ctx context.Context, e *escrowBTC, b *settleBuild) error {
	addrs, err := e.deriveAddresses(ctx, withChecksum("wpkh("+hex.EncodeToString(b.Keys["market"].PubKey)+")"), -1)
	if err != nil {
		return err
	}
	if len(addrs) != 1 {
		return &escrowRPCError{Method: "deriveaddresses", Class: "bad-reply"}
	}
	mine, err := e.arbiterOwns(ctx, addrs[0])
	if err != nil || mine {
		return err
	}
	if err = e.keypoolRefill(ctx, int(b.MarketIndex)+escrowKeypoolAhead); err != nil {
		return err
	}
	if mine, err = e.arbiterOwns(ctx, addrs[0]); err != nil || mine {
		return err
	}
	return &escrowRPCError{Method: "getaddressinfo", Class: sendMarketKey}
}

// settlementSendWaits records why a settlement is not sent yet; it stays where it is and is tried again next pass.
// The class is logged once, when it changes.
func (a *App) settlementSendWaits(ctx context.Context, id int64, orderID, class string) error {
	res, err := a.db.ExecContext(ctx, "UPDATE settlements SET send_error=$2,updated=now() WHERE id=$1 AND send_error<>$2", id, class)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		log.Printf("escrow: settlement of order %s not sent yet: %s", shortID(orderID), class)
	}
	return nil
}

// settlementParties is the order's buyer and vendor.
func settlementParties(ctx context.Context, q rowQuerier, orderID string) (buyer, vendor, state string, err error) {
	err = q.QueryRowContext(ctx, "SELECT o.buyer_id,p.vendor_id,o.state FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=$1", orderID).Scan(&buyer, &vendor, &state)
	return
}

// settleNote writes an order note (no state change, the system as actor) and, when body is set, a notice of it to
// both parties.
func settleNote(ctx context.Context, tx *sql.Tx, orderID, note, body string) error {
	buyer, vendor, state, err := settlementParties(ctx, tx, orderID)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", orderID, state, note); err != nil {
		return err
	}
	if body == "" {
		return nil
	}
	for _, uid := range []string{buyer, vendor} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, "Order "+shortID(orderID)+": "+body); err != nil {
			return err
		}
	}
	return nil
}

// discardedNote is the fixed A-339 note on a party signature that did not verify.
func discardedNote(party, kind string) string {
	return "The " + party + "'s signature on the escrow settlement (" + settlementKind(kind) + ") did not verify when the market finalized the transaction: it is not a valid signature of this transaction by the " + party + "'s escrow key for this order. It was discarded."
}

// storeFinal stores a checked final transaction before anything is broadcast: party_signed -> signed with raw_tx,
// by compare-and-set, the signatures of each party in discard (A-339) removed first, one order note and a notice to
// both parties. A lost race (the settlement changed meanwhile) changes nothing.
func (a *App) storeFinal(ctx context.Context, b *settleBuild, rawHex, cosigned string, used, discard []string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, signer string
	if err = tx.QueryRowContext(ctx, "SELECT state,COALESCE(signer,'') FROM settlements WHERE id=$1 FOR UPDATE", b.ID).Scan(&state, &signer); err != nil || state != "party_signed" {
		return err
	}
	var have int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM settlement_signatures WHERE settlement_id=$1 AND party=ANY($2)", b.ID, used).Scan(&have); err != nil || have != len(used)*len(b.Stored.Inputs) {
		return err
	}
	var notes []string
	for _, party := range discard {
		if _, err = tx.ExecContext(ctx, "DELETE FROM settlement_signatures WHERE settlement_id=$1 AND party=$2", b.ID, party); err != nil {
			return err
		}
		notes = append(notes, discardedNote(party, b.Kind))
	}
	if slices.Contains(discard, signer) {
		// The first signer's signature did not verify: the signer is the party whose signature completed it.
		if _, err = tx.ExecContext(ctx, "UPDATE settlements SET signer=$2 WHERE id=$1 AND state='party_signed'", b.ID, used[0]); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, "UPDATE settlements SET state='signed',raw_tx=$2,cosigned=$3,final_at=now(),send_error='',updated=now() WHERE id=$1 AND state IN ('party_signed','awaiting_signature') AND txid=$4",
		b.ID, rawHex, cosigned, b.TxID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	what := settlementKind(b.Kind)
	note := "The market co-signed the escrow settlement (" + what + ") with the " + used[0] + "'s signature. The transaction was checked and stored, and the market broadcasts it now."
	if cosigned == "parties" {
		note = "The buyer and the vendor both signed the escrow settlement (" + what + "), so the market did not sign it. The transaction was checked and stored, and the market broadcasts it now."
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

// revertSettlement returns a party-signed settlement whose party signatures did not verify to awaiting a signature
// (A-339): those signatures are removed, the signer cleared, one fixed note per party and a notice to both parties.
func (a *App) revertSettlement(ctx context.Context, b *settleBuild, bad []string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, "SELECT state FROM settlements WHERE id=$1 FOR UPDATE", b.ID).Scan(&state); err != nil || state != "party_signed" {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM settlement_signatures WHERE settlement_id=$1 AND party=ANY($2)", b.ID, bad); err != nil {
		return err
	}
	var left int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM settlement_signatures WHERE settlement_id=$1", b.ID).Scan(&left); err != nil || left != 0 {
		return err // a signature pasted while this ran: leave it for the next pass
	}
	if _, err = tx.ExecContext(ctx, "UPDATE settlements SET state='awaiting_signature',signer=NULL,signed_at=NULL,send_error='',updated=now() WHERE id=$1 AND state='party_signed'", b.ID); err != nil {
		return err
	}
	var notes []string
	for _, party := range bad {
		notes = append(notes, discardedNote(party, b.Kind))
	}
	notes = append(notes, "The settlement waits for a signature again: the buyer or the vendor signs it on the order page. Nothing was sent.")
	for i, n := range notes {
		body := ""
		if i == len(notes)-1 {
			body = strings.Join(notes, " ")
		}
		if err = settleNote(ctx, tx, b.OrderID, n, body); err != nil {
			return err
		}
	}
	log.Printf("escrow: settlement of order %s: a party signature did not verify; it waits for a signature again", shortID(b.OrderID))
	return tx.Commit()
}

// broadcastSettlements broadcasts each signed settlement's stored transaction (step 3 of the file comment).
func (a *App) broadcastSettlements(ctx context.Context, e *escrowBTC) error {
	return a.eachSettlement(ctx, "not broadcast", "SELECT id FROM settlements WHERE state='signed' ORDER BY id", func(id int64) error { return a.broadcastSettlement(ctx, e, id) })
}

// sendStored sends a settlement's stored raw transaction: true when the node has it (it answered its txid) or it is
// already confirmed (-27).
func sendStored(ctx context.Context, e *escrowBTC, rawHex, txid string) (bool, error) {
	got, err := e.sendRawTransaction(ctx, rawHex, escrowMaxFeeRate)
	var re *escrowRPCError
	switch {
	case err == nil && got == txid:
		return true, nil
	case err == nil:
		return false, &escrowRPCError{Method: "sendrawtransaction", Class: "bad-reply"}
	case errors.As(err, &re) && re.Code == -27:
		return true, nil
	case errors.As(err, &re) && (re.Code == -25 || re.Code == -26):
		return false, nil // refused: the caller checks its inputs
	}
	return false, err
}

func (a *App) broadcastSettlement(ctx context.Context, e *escrowBTC, id int64) error {
	var orderID, kind, rawHex, txid, psbt, sendErr string
	err := a.db.QueryRowContext(ctx, "SELECT order_id,kind,raw_tx,txid,psbt,send_error FROM settlements WHERE id=$1 AND state='signed'", id).Scan(&orderID, &kind, &rawHex, &txid, &psbt, &sendErr)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	may, err := a.maySend(ctx, sendErr)
	if err != nil || !may {
		if err == nil {
			err = a.seenWhilePaused(ctx, e, id, orderID, kind, txid)
		}
		return err
	}
	sent, err := sendStored(ctx, e, rawHex, txid)
	if err != nil {
		return err
	}
	if !sent {
		stored, err := decodePSBT(psbt)
		if err != nil {
			return err
		}
		spent, err := a.inputsSpent(ctx, e, stored)
		if err != nil || !spent {
			if err == nil {
				err = a.settlementSendWaits(ctx, id, orderID, sendRejected)
			}
			return err
		}
		w, err := e.walletTx(ctx, txid)
		if err != nil {
			return err
		}
		if !w.Found || w.Confirmations < 0 {
			by := firstTxID(w.Conflicts)
			if by == "" {
				by = a.spenderOf(ctx, e, stored)
			}
			return a.markExternal(ctx, id, orderID, by, nil)
		}
		// Its own outputs are spent by this very transaction: the node has it after all.
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE settlements SET state='broadcast',broadcast_at=now(),send_error='',updated=now() WHERE id=$1 AND state='signed'", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	if err = settleNote(ctx, tx, orderID, "The escrow settlement ("+settlementKind(kind)+") was broadcast to the "+a.escrowNetwork()+" network. The order page shows its transaction.",
		"its escrow settlement ("+settlementKind(kind)+") was broadcast to the "+a.escrowNetwork()+" network; the order page shows its transaction and its confirmations."); err != nil {
		return err
	}
	return tx.Commit()
}

func firstTxID(ids []string) string {
	for _, id := range ids {
		if len(id) == 64 && isLowerHexFold(id) && strings.ToLower(id) == id {
			return id
		}
	}
	return ""
}

// trackBroadcasts follows each broadcast settlement (step 4 of the file comment).
func (a *App) trackBroadcasts(ctx context.Context, e *escrowBTC) error {
	return a.eachSettlement(ctx, "not followed", "SELECT id FROM settlements WHERE state='broadcast' ORDER BY id", func(id int64) error { return a.trackBroadcast(ctx, e, id) })
}

func (a *App) trackBroadcast(ctx context.Context, e *escrowBTC, id int64) error {
	var orderID, kind, rawHex, txid, psbt, sendErr string
	err := a.db.QueryRowContext(ctx, "SELECT order_id,kind,raw_tx,txid,psbt,send_error FROM settlements WHERE id=$1 AND state='broadcast'", id).Scan(&orderID, &kind, &rawHex, &txid, &psbt, &sendErr)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	w, err := e.walletTx(ctx, txid)
	if err != nil {
		return err
	}
	// While a restore holds it, or the restore gate is set and no administrator confirmed it, it is followed but never
	// sent again (escrow_recovery.go).
	may, err := a.maySend(ctx, sendErr)
	if err != nil {
		return err
	}
	switch {
	case w.Found && w.Confirmations < 0:
		return a.markExternal(ctx, id, orderID, firstTxID(w.Conflicts), nil)
	case w.Found && w.Confirmations >= e.confirmations:
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// Confirmed: a restore's hold on it, or a confirmation not yet used, ends here.
		res, err := tx.ExecContext(ctx, "UPDATE settlements SET state='confirmed',confirmed_at=now(),send_error='',updated=now() WHERE id=$1 AND state='broadcast'", id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		what := settlementKind(kind)
		if err = settleNote(ctx, tx, orderID, "The escrow settlement ("+what+") is confirmed on the "+a.escrowNetwork()+" network ("+strconv.FormatInt(w.Confirmations, 10)+" confirmations).",
			"its escrow settlement ("+what+") is confirmed on the "+a.escrowNetwork()+" network."); err != nil {
			return err
		}
		return tx.Commit()
	case w.Found && w.Confirmations > 0:
		return nil
	case !w.Found:
		stored, err := decodePSBT(psbt)
		if err != nil {
			return err
		}
		spent, err := a.inputsSpent(ctx, e, stored)
		if err != nil {
			return err
		}
		if spent {
			// Spent, and the wallet does not know this transaction: it is external unless the node still has it
			// (sendrawtransaction answers its txid, or -27 for one already in a block). While sending is paused it waits.
			if !may {
				return nil
			}
			sent, err := sendStored(ctx, e, rawHex, txid)
			if err == nil {
				err = a.sentOnce(ctx, id)
			}
			if err != nil || sent {
				return err
			}
			return a.markExternal(ctx, id, orderID, a.spenderOf(ctx, e, stored), nil)
		}
	}
	// Unconfirmed (or not seen by the wallet, its inputs still unspent): the stored transaction is sent again, unless
	// sending is paused.
	if !may {
		return nil
	}
	if _, err = sendStored(ctx, e, rawHex, txid); err != nil {
		return err
	}
	return a.sentOnce(ctx, id)
}

// spentBeforeCosign settles a party-signed settlement whose escrow outputs are already spent when the market comes to
// complete it (A-344). When the watch-only wallet holds the stored transaction itself (by its txid, in the mempool or
// a block, not conflicted), the parties broadcast it themselves, or a restored database held an older party-signed
// row: its hex is checked like any final transaction (checkFinalTx) and stored, and the settlement moves to broadcast
// (storeSeen); trackBroadcast follows it from there. Only another transaction makes it external.
func (a *App) spentBeforeCosign(ctx context.Context, e *escrowBTC, b *settleBuild) error {
	w, err := e.walletTx(ctx, b.TxID)
	if err != nil {
		return err
	}
	if !w.Found || w.Confirmations < 0 {
		by := firstTxID(w.Conflicts)
		if by == "" {
			by = a.spenderOf(ctx, e, b.Stored)
		}
		return a.markExternal(ctx, b.ID, b.OrderID, by, nil)
	}
	if class := checkFinalTx(w.Hex, b.Stored, b.TxID, b.Outputs, b.InputTotal, b.NetworkFee); class != "" {
		// The stored txid, so the planned outputs, but a transaction that fails the market's checks: it is not recorded
		// as the settlement's, nor called external; the order page and the log say it waits.
		return a.settlementSendWaits(ctx, b.ID, b.OrderID, class)
	}
	return a.storeSeen(ctx, b, w.Hex)
}

// signersSeen is the parties whose stored signature is in every input's witness of the final transaction rawHex.
func signersSeen(rawHex string, b *settleBuild) []string {
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return nil
	}
	t, err := parseSignedTx(raw)
	if err != nil {
		return nil
	}
	var seen []string
	for _, p := range b.Parties {
		every := len(t.Witness) == len(p.Sigs)
		for i := 0; every && i < len(p.Sigs); i++ {
			every = len(t.Witness[i]) == 4 && (slices.Equal(t.Witness[i][1], p.Sigs[i].Sig) || slices.Equal(t.Witness[i][2], p.Sigs[i].Sig))
		}
		if every {
			seen = append(seen, p.Party)
		}
	}
	return seen
}

// storeSeen records a final transaction found already on the network as a party-signed settlement's, or as one's
// still waiting for a signature (A-356: the parties signed the copies they downloaded and sent it themselves):
// party_signed or awaiting_signature -> signed (raw_tx) -> broadcast in one transaction, by compare-and-set, cosigned
// 'parties' when its witnesses carry both parties' stored signatures and 'network' otherwise, with an honest note and a
// notice to both parties.
func (a *App) storeSeen(ctx context.Context, b *settleBuild, rawHex string) error {
	seen := signersSeen(rawHex, b)
	cosigned, who := "network", "who signed it is not recorded"
	switch len(seen) {
	case 2:
		cosigned, who = "parties", "The buyer and the vendor both signed it (their signatures are the ones the market stored)"
	case 1:
		who = "it carries the " + seen[0] + "'s signature; who signed it besides the " + seen[0] + " is not recorded"
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE settlements SET state='signed',raw_tx=$2,cosigned=$3,final_at=now(),send_error='',updated=now() WHERE id=$1 AND state IN ('party_signed','awaiting_signature') AND txid=$4",
		b.ID, rawHex, cosigned, b.TxID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	if _, err = tx.ExecContext(ctx, "UPDATE settlements SET state='broadcast',broadcast_at=now(),updated=now() WHERE id=$1 AND state='signed'", b.ID); err != nil {
		return err
	}
	what, net := settlementKind(b.Kind), a.escrowNetwork()
	if cosigned == "network" {
		who = strings.ToUpper(who[:1]) + who[1:]
	}
	note := "The transaction of the escrow settlement (" + what + ") was already on the " + net + " network when the market came to complete it. " + who +
		". The market found it in its watch-only wallet, checked it against the stored transaction and recorded it as broadcast; the market did not sign or broadcast it."
	if err = settleNote(ctx, tx, b.OrderID, note, "its escrow settlement ("+what+") was already on the "+net+" network; the order page shows its transaction and its confirmations."); err != nil {
		return err
	}
	log.Printf("escrow: settlement of order %s: its transaction was already on the network; recorded as broadcast", shortID(b.OrderID))
	return tx.Commit()
}

// escrowReorgDepth is how many confirmations a confirmed settlement's transaction needs before the watcher stops
// following it (trackConfirmeds). A depth, not a time: a reorganisation is a matter of blocks, and a node that was
// down still catches one up when it returns. 100 is Bitcoin's own coinbase maturity, the depth Core treats as final
// for newly mined coins; a deeper reorganisation is not handled here.
const escrowReorgDepth = 100

// trackConfirmeds follows each confirmed settlement not yet escrowReorgDepth deep (step 5 of the file comment).
func (a *App) trackConfirmeds(ctx context.Context, e *escrowBTC) error {
	return a.eachSettlement(ctx, "not followed", "SELECT id FROM settlements WHERE state='confirmed' AND reorg_safe_at IS NULL ORDER BY id", func(id int64) error { return a.trackConfirmed(ctx, e, id) })
}

func (a *App) trackConfirmed(ctx context.Context, e *escrowBTC, id int64) error {
	var orderID, kind, txid string
	err := a.db.QueryRowContext(ctx, "SELECT order_id,kind,txid FROM settlements WHERE id=$1 AND state='confirmed' AND reorg_safe_at IS NULL", id).Scan(&orderID, &kind, &txid)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	w, err := e.walletTx(ctx, txid)
	switch {
	case err != nil:
		return err
	case !w.Found:
		return nil // the wallet does not know it (it missed it, or was restored): nothing to conclude; asked again next pass
	case w.Confirmations < 0:
		return a.markExternal(ctx, id, orderID, firstTxID(w.Conflicts), nil)
	case w.Confirmations >= escrowReorgDepth:
		_, err = a.db.ExecContext(ctx, "UPDATE settlements SET reorg_safe_at=now(),updated=now() WHERE id=$1 AND state='confirmed' AND reorg_safe_at IS NULL", id)
		return err
	case w.Confirmations > 0:
		return nil // still in a block (a shallower reorganisation may have lowered its count)
	}
	// Back in the mempool, or nowhere: a reorganisation took it out of the chain. The only move back a settlement with a
	// final transaction may make (migration 077); trackBroadcast sends the same stored transaction again.
	gate, err := restoreGateSet(ctx, a.db)
	if err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE settlements SET state='broadcast',confirmed_at=NULL,updated=now() WHERE id=$1 AND state='confirmed' AND reorg_safe_at IS NULL", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	what, net := settlementKind(kind), a.escrowNetwork()
	next := "The market sends the stored transaction again and follows it until it is confirmed again."
	if gate {
		next = "The market does not send it again while it reconciles its records after a restore from a backup; it follows it until it is confirmed again."
	}
	if err = settleNote(ctx, tx, orderID, "The transaction of the escrow settlement ("+what+") is no longer in the "+net+" chain: a reorganisation of the chain took it out of the block that confirmed it. It is the same transaction, back to waiting for confirmations. "+next,
		"its escrow settlement ("+what+") is no longer confirmed after a reorganisation of the "+net+" chain; it is the same transaction, waiting for confirmations again."); err != nil {
		return err
	}
	log.Printf("escrow: settlement of order %s: its transaction left the chain (reorganisation); back to broadcast", shortID(orderID))
	return tx.Commit()
}

// markExternal records that the escrow outputs of a settlement were spent by a transaction the market did not make
// (spentBy when the wallet names it): state external, its inputs flagged so staff can open the order, a note and a
// notice to the parties and to the staff. A confirmed settlement becomes external when a reorganisation replaced it
// with a conflicting transaction. A settlement waiting for a signature becomes external when an input it spends is
// spent (A-356, trackAwaiting), and so does one still being built (buildInputSpent): it has claimed no deposit, so
// deposits names the deposits the build found spent, with the transaction that spent each one ("" when the wallet
// does not name it), and each is marked spent outside the market as markOutsideSpend marks one.
func (a *App) markExternal(ctx context.Context, id int64, orderID, spentBy string, deposits map[int64]string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var by any
	if spentBy != "" {
		by = spentBy
	}
	var kind, was string
	err = tx.QueryRowContext(ctx, "SELECT kind,state FROM settlements WHERE id=$1 FOR UPDATE", id).Scan(&kind, &was)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE settlements SET state='external',spent_by=$2,reorg_safe_at=NULL,updated=now()
		WHERE id=$1 AND state IN ('building','awaiting_signature','party_signed','signed','broadcast','confirmed')`, id, by)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	if was == "building" {
		for dep, s := range deposits {
			var depBy any
			if s != "" {
				depBy = s
			}
			if _, err = tx.ExecContext(ctx, `UPDATE payments SET spent_outside=true,spent_by=$2,flagged=true,updated=now() WHERE id=$1 AND order_id=$3
				AND settlement_id IS NULL AND NOT spent_outside`, dep, depBy, orderID); err != nil {
				return err
			}
		}
	} else if _, err = tx.ExecContext(ctx, "UPDATE payments SET flagged=true,updated=now() WHERE settlement_id=$1", id); err != nil {
		return err
	}
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1", orderID))
	if err != nil {
		return err
	}
	what := settlementKind(kind)
	which := "the market's watch-only wallet does not name it"
	if spentBy != "" {
		which = "transaction " + spentBy
	}
	note := "The coins of this escrow's settlement (" + what + ") were spent by a transaction this market did not make. The settlement is recorded as spent outside the market and will not be sent. Moderator review required."
	switch was {
	case "confirmed":
		note = "The escrow settlement (" + what + ") was confirmed, but a reorganisation of the chain replaced it with a conflicting transaction this market did not make, which spent the escrow's coins instead. The settlement is recorded as spent outside the market. Moderator review required."
	case "building":
		note = "An escrow deposit this settlement (" + what + ") was to spend was spent by a transaction this market did not make (" + which + "), so the market cannot build it. The settlement is recorded as spent outside the market, and no settlement of the market will spend that deposit. Moderator review required."
	case "awaiting_signature":
		note = "An escrow deposit this settlement (" + what + ") spends was spent by a transaction this market did not make (" + which + ") before any party signed it, so the settlement can never be completed and no signature is asked for any more. The settlement is recorded as spent outside the market and will not be sent. Moderator review required."
	}
	if err = a.flagOrder(ctx, tx, &o, note, "Order "+shortID(orderID)+": the coins of its escrow settlement ("+what+") were spent by a transaction this market did not make. Market staff were notified."); err != nil {
		return err
	}
	log.Printf("escrow: settlement of order %s: its escrow outputs were spent by another transaction (external)", shortID(orderID))
	return tx.Commit()
}
