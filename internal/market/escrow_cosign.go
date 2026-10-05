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
//     settlement returned. A physical order's release before the order's outcome (shipped) is never co-signed: only
//     both parties' signatures complete it, and when they do not verify together both are discarded (P2-R2, A-372;
//     finalizeParties, escrow_release.go).
//  3. Broadcasts each signed settlement's stored raw transaction, only that one (broadcastSettlements): an answer of
//     its txid, or -27 (already confirmed), is sent; it moves signed -> broadcast. A signed settlement is never rebuilt
//     or signed again; a failed broadcast is tried again with the same stored transaction on the next pass.
//  4. Follows each broadcast settlement in the watch-only wallet (trackBroadcasts) through the market's own
//     transactions for it, its own and each stored fee bump (P2-d, A-375; escrow_bump.go): one with enough
//     confirmations -> confirmed (a fee bump's recorded on the bump); still unconfirmed -> the highest-fee stored
//     transaction is sent again (idempotent), never an earlier one; spent by a transaction outside that set (a
//     conflict, or its outputs gone without it) -> external, flagged for staff. A conflict among the market's own
//     transactions is not external.
//  5. Follows each confirmed settlement until its confirmed transaction has escrowReorgDepth confirmations
//     (trackConfirmeds, P1-d1): back at 0 confirmations (a reorganisation took it out of the chain), or replaced by
//     another of the market's own transactions -> broadcast, and step 4 follows them again; conflicted (another
//     transaction was mined instead) -> external.
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

// cosignSettlements completes every party-signed settlement of a closed order (step 1 of the file comment), and every
// party-signed release of a shipped one, which only both parties' signatures complete (P2-R2, escrow_release.go).
func (a *App) cosignSettlements(ctx context.Context, e *escrowBTC) error {
	return a.eachSettlement(ctx, "not co-signed", `SELECT s.id FROM settlements s JOIN orders o ON o.id=s.order_id
		WHERE s.state='party_signed' AND o.state IN ('completed','cancelled','resolved','shipped') ORDER BY s.id`, func(id int64) error { return a.cosignSettlement(ctx, e, id) })
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
	OrderState, Outcome    string // the order's state and dispute outcome when it was read
	Signer                 string
	Stored                 *psbtPacket
	NetworkFee, InputTotal int64
	MarketIndex            int64
	Outputs                []settleOutput
	Keys                   map[string]escrowKeyRow
	Parties                []partySigs // the parties with a signature on every input, the first signer first
}

// loadSettleBuild reads a settlement in state want (party_signed, or awaiting_signature for trackAwaiting, or
// broadcast for a fee bump) of a closed order, or the release of a shipped (or disputed) one (settlementBuildable,
// A-376), or nil when it is no longer in that state (or its kind does not fit the order's state and outcome). Only a
// party-signed one is read with its party signatures. The market signs only what settlementFits allows.
func (a *App) loadSettleBuild(ctx context.Context, id int64, want string) (*settleBuild, error) {
	b := &settleBuild{ID: id}
	var state, stored string
	err := a.db.QueryRowContext(ctx, `SELECT s.order_id,s.kind,s.state,s.psbt,s.txid,COALESCE(s.signer,''),s.network_fee,s.input_total,o.state,`+disputeOutcomeSQL+`,es.market_index
		FROM settlements s JOIN orders o ON o.id=s.order_id JOIN escrows es ON es.order_id=s.order_id AND es.coin='BTC' WHERE s.id=$1`, id).Scan(
		&b.OrderID, &b.Kind, &state, &stored, &b.TxID, &b.Signer, &b.NetworkFee, &b.InputTotal, &b.OrderState, &b.Outcome, &b.MarketIndex)
	if err != nil || state != want || !settlementBuildable(b.Kind, b.OrderState, b.Outcome) {
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
	if !settlementFits(b.Kind, b.OrderState, b.Outcome) {
		// P2-R2, A-372: a release before its order's outcome completes with both parties' signatures only; the market
		// never signs it, and nothing below (ensureArbiterKey, arbiterSign) is reached for it.
		return a.finalizeParties(ctx, e, b)
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
// both parties. A lost race (the settlement changed meanwhile) changes nothing. A release both parties signed before
// its order's outcome (P2-R2) is stored only while the order, locked first, is still shipped with no dispute, and the
// order is completed in the same transaction.
func (a *App) storeFinal(ctx context.Context, b *settleBuild, rawHex, cosigned string, used, discard []string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	complete := !settlementFits(b.Kind, b.OrderState, b.Outcome)
	if complete {
		if ok, err := lockShipped(ctx, tx, b.OrderID); err != nil || !ok || cosigned != "parties" {
			return err
		}
	}
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
	res, err := tx.ExecContext(ctx, "UPDATE settlements SET state='signed',raw_tx=$2,cosigned=$3,final_at=now(),send_error='',updated=now() WHERE id=$1 AND state='party_signed' AND txid=$4",
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
	if complete {
		if err = a.completeShipped(ctx, tx, b.OrderID, escrowReleasedNote); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// revertSettlement returns a party-signed settlement whose party signatures did not verify to awaiting a signature
// (A-339): those signatures are removed, the signer cleared, one fixed note per party and a notice to both parties.
func (a *App) revertSettlement(ctx context.Context, b *settleBuild, bad []string) error {
	var notes []string
	for _, party := range bad {
		notes = append(notes, discardedNote(party, b.Kind))
	}
	return a.revertSettlementWith(ctx, b, bad, append(notes, "The settlement waits for a signature again: the buyer or the vendor signs it on the order page. Nothing was sent."))
}

// revertSettlementWith is revertSettlement with its notes: each is an order note, and the last carries a notice of all
// of them to both parties.
func (a *App) revertSettlementWith(ctx context.Context, b *settleBuild, bad, notes []string) error {
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
	// An administrator's confirmation is used up here (sentOnceSQL, escrow_recovery.go; A-363).
	res, err := tx.ExecContext(ctx, "UPDATE settlements SET state='broadcast',broadcast_at=now(),send_error="+sentOnceSQL+",updated=now() WHERE id=$1 AND state='signed'",
		id, sendRebroadcastOK, sendRebroadcastSent)
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

// trackBroadcast follows one broadcast settlement through the market's own transactions for it (A-375): its own and
// each stored fee bump (settlementAttempts, escrow_bump.go). One of them with enough confirmations confirms it; one in a
// block below that is waited for; a conflict with a transaction outside that set makes it external, and a conflict
// only with the market's own is waited for. Otherwise only the highest-fee stored transaction is sent again.
func (a *App) trackBroadcast(ctx context.Context, e *escrowBTC, id int64) error {
	var orderID, kind, psbt, sendErr string
	err := a.db.QueryRowContext(ctx, "SELECT order_id,kind,psbt,send_error FROM settlements WHERE id=$1 AND state='broadcast'", id).Scan(&orderID, &kind, &psbt, &sendErr)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	attempts, err := settlementAttempts(ctx, a.db, id)
	if err != nil {
		return err
	}
	own := map[string]bool{}
	for _, at := range attempts {
		own[at.TxID] = true
	}
	ws := make([]escrowWalletTx, len(attempts))
	mined, inBlock, conflicted, pending, listed, outside := -1, false, false, false, false, ""
	for i, at := range attempts {
		w, err := e.walletTx(ctx, at.TxID)
		if err != nil {
			return err
		}
		ws[i] = w
		switch {
		case !w.Found:
		case w.Confirmations >= e.confirmations:
			if mined < 0 {
				mined = i
			}
		case w.Confirmations > 0:
			inBlock = true
		case w.Confirmations < 0:
			conflicted = true
			for _, c := range w.Conflicts {
				if firstTxID([]string{c}) == "" {
					continue
				}
				listed = true
				if !own[c] && outside == "" {
					outside = c
				}
			}
		default:
			pending = true
		}
	}
	// A-382: a built fee bump with no stored final transaction whose transaction the wallet holds (the parties
	// broadcast it, or a restore lost its final transaction) is checked and recorded as broadcast; it is then followed
	// like any stored one.
	for i, at := range attempts {
		if at.Raw == "" && ws[i].Found && ws[i].Confirmations >= 0 {
			stored, err := a.storeSeenBump(ctx, id, at.Bump, ws[i].Hex)
			if err != nil {
				return err
			}
			if stored {
				return a.trackBroadcast(ctx, e, id)
			}
		}
	}
	// While a restore holds it, or the restore gate is set and no administrator confirmed it, it is followed but never
	// sent again (escrow_recovery.go).
	may, err := a.maySend(ctx, sendErr)
	if err != nil {
		return err
	}
	switch {
	case mined >= 0:
		return a.confirmSettlement(ctx, id, orderID, kind, attempts[mined], ws[mined].Confirmations)
	case inBlock:
		return nil
	case conflicted && (outside != "" || !listed):
		return a.markExternal(ctx, id, orderID, outside, nil)
	case conflicted:
		return nil // conflicted only with the market's own transactions: the one in the chain is followed (A-375)
	}
	top := topAttempt(attempts)
	at := attempts[top]
	if !ws[top].Found {
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
			// (sendrawtransaction answers its txid, or -27 for one already in a block), or another of the market's own
			// transactions spends them. While sending is paused it waits.
			if !may || restoreHeld(at.SendErr) {
				return nil
			}
			sent, err := sendStored(ctx, e, at.Raw, at.TxID)
			if err == nil {
				err = a.attemptSent(ctx, id, orderID, kind, at, sent)
			}
			if err != nil || sent {
				return err
			}
			if by := a.spenderOf(ctx, e, stored); !pending && !own[by] {
				return a.markExternal(ctx, id, orderID, by, nil)
			}
			return nil
		}
	}
	// Unconfirmed (or not seen by the wallet, its inputs still unspent): the highest-fee stored transaction is sent
	// again, unless sending is paused or a restore holds that transaction.
	if !may || restoreHeld(at.SendErr) {
		return nil
	}
	sent, err := sendStored(ctx, e, at.Raw, at.TxID)
	if err != nil {
		return err
	}
	return a.attemptSent(ctx, id, orderID, kind, at, sent)
}

// confirmSettlement records a broadcast settlement as confirmed by one of the market's own transactions for it, at
// confirmations: broadcast -> confirmed; when that transaction is a fee bump's, the bump is recorded as confirmed (the
// settlement's own frozen columns stay as they are); an open fee bump is closed. A restore's hold on it, or a
// confirmation not yet used, ends here.
func (a *App) confirmSettlement(ctx context.Context, id int64, orderID, kind string, at bumpAttempt, confirmations int64) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE settlements SET state='confirmed',confirmed_at=now(),send_error='',updated=now() WHERE id=$1 AND state='broadcast'", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	what := settlementKind(kind)
	note := "The escrow settlement (" + what + ") is confirmed on the " + a.escrowNetwork() + " network (" + strconv.FormatInt(confirmations, 10) + " confirmations)."
	if at.Bump != 0 {
		// A fee bump sent by someone else before the market recorded its own send is broadcast first (migration 080).
		for _, q := range []string{"UPDATE settlement_bumps SET state='broadcast',broadcast_at=now(),updated=now() WHERE id=$1 AND state='signed'",
			"UPDATE settlement_bumps SET state='confirmed',confirmed_at=now(),send_error='',updated=now() WHERE id=$1 AND state='broadcast'"} {
			if _, err = tx.ExecContext(ctx, q, at.Bump); err != nil {
				return err
			}
		}
		note += " The confirmed transaction is its fee bump " + strconv.Itoa(at.N) + ", which replaced the transaction sent before; that one can no longer confirm. The order page shows it."
	}
	if err = closeOpenBumps(ctx, tx, id); err != nil {
		return err
	}
	if err = settleNote(ctx, tx, orderID, note, "its escrow settlement ("+what+") is confirmed on the "+a.escrowNetwork()+" network."); err != nil {
		return err
	}
	return tx.Commit()
}

// closeOpenBumps closes a settlement's open fee bump (building, waiting for or with a signature, no final transaction)
// once the settlement no longer waits to be mined.
func closeOpenBumps(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, "UPDATE settlement_bumps SET state='superseded',updated=now() WHERE settlement_id=$1 AND state IN ('building','awaiting_signature','party_signed')", id)
	return err
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
	if class := checkFinalTx(e.network, w.Hex, b.Stored, b.TxID, b.Outputs, b.InputTotal, b.NetworkFee); class != "" {
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
	// A release on the network before its order's outcome (P2-R2, A-376): the order, locked first, is completed in this
	// transaction while it is still shipped with no dispute; a disputed order stays as it is.
	var complete bool
	if !settlementFits(b.Kind, b.OrderState, b.Outcome) {
		if complete, err = lockShipped(ctx, tx, b.OrderID); err != nil {
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
	if complete {
		if err = a.completeShipped(ctx, tx, b.OrderID, escrowSeenNote); err != nil {
			return err
		}
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
	// The confirmed transaction is the settlement's own, or the fee bump recorded as confirmed (P2-d).
	var orderID, kind, txid string
	err := a.db.QueryRowContext(ctx, `SELECT s.order_id,s.kind,COALESCE((SELECT b.txid FROM settlement_bumps b WHERE b.settlement_id=s.id AND b.state='confirmed'),s.txid)
		FROM settlements s WHERE s.id=$1 AND s.state='confirmed' AND s.reorg_safe_at IS NULL`, id).Scan(&orderID, &kind, &txid)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	w, err := e.walletTx(ctx, txid)
	ownConflict := false
	if err == nil && w.Found && w.Confirmations < 0 {
		// A-375: replaced in the chain by another of the market's own transactions for it (the original, or another
		// fee bump): not external. It goes back to broadcast, and trackBroadcast confirms the one that was mined.
		attempts, aerr := settlementAttempts(ctx, a.db, id)
		if aerr != nil {
			return aerr
		}
		for _, at := range attempts {
			ownConflict = ownConflict || at.TxID != txid && slices.Contains(w.Conflicts, at.TxID)
		}
	}
	switch {
	case err != nil:
		return err
	case !w.Found:
		return nil // the wallet does not know it (it missed it, or was restored): nothing to conclude; asked again next pass
	case w.Confirmations < 0 && !ownConflict:
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
	// A confirmed fee bump goes back to broadcast with it (migration 080 allows the same move).
	if _, err = tx.ExecContext(ctx, "UPDATE settlement_bumps SET state='broadcast',confirmed_at=NULL,updated=now() WHERE settlement_id=$1 AND state='confirmed'", id); err != nil {
		return err
	}
	what, net := settlementKind(kind), a.escrowNetwork()
	next := "The market sends the stored transaction again and follows it until it is confirmed again."
	if gate {
		next = "The market does not send it again while it reconciles its records after a restore from a backup; it follows it until it is confirmed again."
	}
	note := "The transaction of the escrow settlement (" + what + ") is no longer in the " + net + " chain: a reorganisation of the chain took it out of the block that confirmed it. It is the same transaction, back to waiting for confirmations. " + next
	body := "its escrow settlement (" + what + ") is no longer confirmed after a reorganisation of the " + net + " chain; it is the same transaction, waiting for confirmations again."
	if ownConflict {
		note = "The transaction of the escrow settlement (" + what + ") is no longer in the " + net + " chain: a reorganisation of the chain replaced it with another of the market's own transactions for this settlement (the one sent first, or a fee bump), which the market now follows until it is confirmed."
		body = "its escrow settlement (" + what + ") is no longer confirmed after a reorganisation of the " + net + " chain; another of the market's own transactions for it is waiting for confirmations."
	}
	if err = settleNote(ctx, tx, orderID, note, body); err != nil {
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
	// P2-d: an open fee bump is closed, and a fee bump recorded as confirmed is not any more (migration 080).
	if err = closeOpenBumps(ctx, tx, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE settlement_bumps SET state='broadcast',confirmed_at=NULL,updated=now() WHERE settlement_id=$1 AND state='confirmed'", id); err != nil {
		return err
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
