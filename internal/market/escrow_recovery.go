package market

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Recovery and reconciliation of Bitcoin escrow (WR-escrow Phase 1, slice P1-d1; ES-6).
//
// The restore gate. A restored database knows nothing that happened after its backup: a settlement built, co-signed or
// broadcast since then is missing or in an earlier state, and a dispute decided since then can be decided again. While
// the pooled restore gate is set (settings payments_recovery_required, written by scripts/restore.sh or the manual
// protection SQL in UPGRADING.md section 6, cleared only by the operator's SQL), the escrow watcher therefore builds no
// settlement, co-signs none and broadcasts no stored transaction; it still reads deposits and follows broadcast
// settlements to their confirmations (watching continues). The restore also holds every restored settlement that has a
// final transaction (restoredSettlementHold): its transaction is broadcast again only after an administrator checked on
// /admin that it was not already sent or replaced (escrowRebroadcastAction: password step-up, audited), which allows
// exactly one broadcast (sendRebroadcastOK, consumed by that send; while the gate is set, a second confirmation after
// it is refused, sendRebroadcastSent). With ESCROW_BTC off the confirmation route is off and /admin offers no form for
// it (A-360). A held settlement stays held after the gate is cleared, until an administrator confirms it; one the
// watch-only wallet shows on the network is recorded as broadcast without being sent, and one that confirms is no
// longer held. While the gate is set, escrow checkout is refused too (errEscrowRestoring, A-359). Fee bumps (P2-d,
// escrow_bump.go) are not built, co-signed or sent while the gate is set; the restore holds each restored bump with a
// final transaction as it holds a settlement, and the administrator's confirmation of the settlement covers its bumps:
// the market then sends the highest-fee stored transaction once.

func init() {
	registerAction("/admin/escrow-rebroadcast", actionSpec{Roles: []string{"admin"}, OwnTx: true, Off: escrowOff, Run: escrowRebroadcastAction})
	registerLoader("admin", escrowRecoveryLoader)
}

// restoredSettlementHold is the send_error scripts/restore.sh (and the manual protection SQL in UPGRADING.md section
// 6) writes on every restored escrow settlement that has a final transaction (signed or broadcast): the restored
// database may predate its broadcast, or something that replaced it, so it is not broadcast again until an
// administrator checks it (escrowRebroadcastAction). It starts with restoredHoldPrefix, as the restored payouts do.
const restoredSettlementHold = restoredHoldPrefix + " check that this escrow settlement's transaction was not already sent or replaced, then confirm its rebroadcast on the admin page."

// sendRebroadcastOK is the send_error of a settlement whose rebroadcast an administrator confirmed: the watcher sends
// its stored transaction once, gate or not, and then uses it up (sentOnceSQL).
const sendRebroadcastOK = "rebroadcast-confirmed"

// sendRebroadcastSent is the send_error of a settlement whose confirmed rebroadcast was sent while the restore gate was
// set (A-363): while the gate stays set it is not sent again, and a second confirmation is refused; once the gate is
// cleared it behaves as no send_error at all (maySend), and its confirmation clears it.
const sendRebroadcastSent = "rebroadcast-sent"

// restoreGateSQL is true while the restore gate is set, read directly as the pooled payouts read it (sendPayouts).
const restoreGateSQL = "EXISTS(SELECT 1 FROM settings WHERE key='payments_recovery_required' AND value='true')"

// restoreGateSet reports whether the restore gate is set.
func restoreGateSet(ctx context.Context, q rowQuerier) (bool, error) {
	var on bool
	err := q.QueryRowContext(ctx, "SELECT "+restoreGateSQL).Scan(&on)
	return on, err
}

// escrowSweepKey is the setting scripts/restore.sh (and the manual protection SQL in UPGRADING.md section 6) writes
// after a restore: escrow checkout waits (errEscrowSweep) until the escrow watcher's orphan sweep has read and reserved
// the keys of every escrow the restored database lost (reconcileOrphans), which clears it (P2-R10, A-366).
const escrowSweepKey = "escrow_sweep_required"

// escrowSweepRequired reports whether a restore asked for the orphan sweep and none has completed since.
func escrowSweepRequired(ctx context.Context, q rowQuerier) (bool, error) {
	var on bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value='true')", escrowSweepKey).Scan(&on)
	return on, err
}

// restoreHeld reports a settlement a restore held (restoredSettlementHold).
func restoreHeld(sendErr string) bool { return strings.HasPrefix(sendErr, restoredHoldPrefix) }

// maySend reports whether the stored transaction of a settlement with send_error sendErr may be sent now: never while
// a restore holds it, and while the gate is set only once an administrator confirmed it.
func (a *App) maySend(ctx context.Context, sendErr string) (bool, error) {
	if restoreHeld(sendErr) {
		return false, nil
	}
	if sendErr == sendRebroadcastOK {
		return true, nil
	}
	gate, err := restoreGateSet(ctx, a.db)
	return !gate, err
}

// sentOnce uses up an administrator's rebroadcast confirmation once the transaction it allowed was sent: while the
// restore gate is set it becomes sendRebroadcastSent, otherwise it is cleared.
func (a *App) sentOnce(ctx context.Context, id int64) error {
	_, err := a.db.ExecContext(ctx, "UPDATE settlements SET send_error="+sentOnceSQL+",updated=now() WHERE id=$1 AND send_error=$2", id, sendRebroadcastOK, sendRebroadcastSent)
	return err
}

// sentOnceSQL is the send_error of a settlement once a send happened: an administrator's confirmation ($2) becomes
// sendRebroadcastSent ($3) while the restore gate is set; anything else is cleared.
const sentOnceSQL = "CASE WHEN send_error=$2 AND " + restoreGateSQL + " THEN $3 ELSE '' END"

// Order-page lines of a settlement paused by the restore gate or held by a restore.
const (
	restoreWhy        = "the market restored its records from a backup and is reconciling them with the blockchain"
	restoreHeldText   = "Held: " + restoreWhy + ", so an administrator checks that this transaction was not already sent or replaced before the market broadcasts it again."
	restoreConfirmedT = "An administrator checked that this transaction was not already sent or replaced; the market broadcasts it once on its next pass."
	restoreSentText   = "An administrator checked that this transaction was not already sent or replaced, and the market broadcast it once; it follows its confirmations but does not send it again until the operator has finished."
)

// restoreStatus replaces a settlement's order-page status while the restore gate pauses it or a restore holds it
// (settlementView): what is paused, why, and what happens next.
func (a *App) restoreStatus(s *EscrowSettlementView, v *EscrowOrderView, state, sendErr, signer, signedAt string, bothSigned bool) {
	sendable := state == "signed" || state == "broadcast"
	stored := "Signed, checked and stored."
	if state == "broadcast" {
		stored = s.Status // it names its transaction, public since its broadcast
	}
	switch {
	case sendable && sendErr == sendRebroadcastOK:
		s.Status = stored + " " + restoreConfirmedT
	case sendable && restoreHeld(sendErr):
		s.Status = stored + " " + restoreHeldText
	case !v.RestorePaused:
	case sendable && sendErr == sendRebroadcastSent:
		s.Status = stored + " " + restoreSentText
	case state == "building":
		s.Status = "Paused: " + restoreWhy + ", so the market builds no settlement until the operator has finished. The escrow is still watched."
	case state == "party_signed" && bothSigned:
		s.Status = "Signed by the " + signer + " and the " + otherParty(signer) + " (first signature " + signedAt + "). " + settleSigChecked + " Paused: " + restoreWhy + ", so the market (which does not sign it) broadcasts the transaction the two parties signed only once the operator has finished. Nothing has been sent yet."
	case state == "party_signed":
		s.Status = "Signed by the " + signer + " (" + signedAt + "). " + settleSigChecked + " Paused: " + restoreWhy + ", so the market co-signs it only once the operator has finished. Nothing has been sent yet."
	case state == "signed":
		s.Status = stored + " Paused: " + restoreWhy + ", so the market does not broadcast it until an administrator has checked that it was not already sent or replaced."
	case state == "broadcast":
		s.Status = stored + " Paused: " + restoreWhy + ", so the market follows its confirmations but does not send it again until an administrator has checked that it was not already sent or replaced."
	}
}

// EscrowRecoveryView is /admin's Bitcoin escrow recovery section: whether the restore gate pauses escrow, the
// settlements whose stored transaction waits for an administrator's confirmation, and the reconciliation lists.
type EscrowRecoveryView struct {
	Gate    bool
	On      bool // ESCROW_BTC is on: the rebroadcast confirmation exists (A-360)
	Held    []EscrowHeldView
	Orphans []EscrowOrphanView
}

// EscrowHeldView is one settlement whose stored transaction is not broadcast until an administrator confirms it.
type EscrowHeldView struct {
	ID             int64
	OrderID, Short string
	Kind, State    string
	TxID           string
	Restored       bool // held by a restore (else paused by the gate)
	Confirmed      bool // already confirmed: broadcast once on the next pass
	Sent           bool // its confirmed rebroadcast was sent; not sent again while the gate is set (A-363)
}

// EscrowOrphanView is one address the escrow watch-only wallet holds with no escrow row.
type EscrowOrphanView struct {
	Address, OrderID, Received, FirstSeen, LastSeen string
}

// escrowRecoveryLoader fills /admin's escrow recovery section while ESCROW_BTC is on, or while any settlement is held.
func escrowRecoveryLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" {
		return nil
	}
	gate, err := restoreGateSet(ctx, a.db)
	if err != nil {
		return err
	}
	v := &EscrowRecoveryView{Gate: gate, On: a.escrowConfigured}
	// A settlement is also held while a restore holds one of its fee bumps (P2-d, escrow_bump.go).
	rows, err := a.db.QueryContext(ctx, `SELECT id,order_id,kind,state,txid,send_error,EXISTS(SELECT 1 FROM settlement_bumps b WHERE b.settlement_id=s.id AND b.send_error LIKE $1) FROM settlements s
		WHERE raw_tx IS NOT NULL AND state IN ('signed','broadcast') AND (send_error LIKE $1 OR send_error=$2 OR $3
		OR EXISTS(SELECT 1 FROM settlement_bumps b WHERE b.settlement_id=s.id AND b.send_error LIKE $1)) ORDER BY id`, restoredHoldPrefix+"%", sendRebroadcastOK, gate)
	if err != nil {
		return err
	}
	for rows.Next() {
		var h EscrowHeldView
		var sendErr string
		var bumpHeld bool
		if err = rows.Scan(&h.ID, &h.OrderID, &h.Kind, &h.State, &h.TxID, &sendErr, &bumpHeld); err != nil {
			rows.Close()
			return err
		}
		h.Short, h.Restored, h.Confirmed, h.Sent = shortID(h.OrderID), restoreHeld(sendErr) || bumpHeld, sendErr == sendRebroadcastOK, sendErr == sendRebroadcastSent
		h.Kind = settlementKind(h.Kind)
		v.Held = append(v.Held, h)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if v.Orphans, err = escrowOrphans(ctx, a.db); err != nil {
		return err
	}
	if !a.escrowConfigured && len(v.Held) == 0 && len(v.Orphans) == 0 {
		return nil
	}
	d.EscrowRecovery = v
	return nil
}

// escrowOrphans lists the orphan escrow addresses the watcher recorded (escrowReconcileOrphans).
func escrowOrphans(ctx context.Context, q rowsQuerier) ([]EscrowOrphanView, error) {
	rows, err := q.QueryContext(ctx, `SELECT address,order_id,received,to_char(first_seen,'YYYY-MM-DD HH24:MI "UTC"'),to_char(last_seen,'YYYY-MM-DD HH24:MI "UTC"')
		FROM escrow_orphans ORDER BY first_seen,address LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EscrowOrphanView
	for rows.Next() {
		var o EscrowOrphanView
		var sats int64
		if err = rows.Scan(&o.Address, &o.OrderID, &sats, &o.FirstSeen, &o.LastSeen); err != nil {
			return nil, err
		}
		o.Received = amount(sats, 8)
		if o.Received == "" {
			o.Received = "0"
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// escrowRebroadcastAction is an administrator's confirmation, after checking the chain and the escrow's watch-only
// wallet, that a held settlement's stored transaction was not already sent or replaced: the watcher then broadcasts it
// once. Password step-up (and authenticator code when enrolled), compare-and-set on the settlement's state and
// send_error, an order note and an audit row.
func escrowRebroadcastAction(c *actionCtx) (actionResult, error) {
	id, err := strconv.ParseInt(c.Form.Get("settlement_id"), 10, 64)
	if err != nil || id <= 0 {
		return actionResult{}, fail(400, "Unknown settlement")
	}
	if c.Form.Get("not_sent") != "confirmed" {
		return actionResult{}, fail(400, "Check the blockchain and the escrow's watch-only wallet first, then confirm that this transaction was not already sent and that no other transaction spent the escrow's coins. Nothing was changed.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		var orderID, kind, state, sendErr string
		var raw sql.NullString
		err := c.Tx.QueryRowContext(ctx, "SELECT order_id,kind,state,send_error,raw_tx FROM settlements WHERE id=$1 FOR UPDATE", id).Scan(&orderID, &kind, &state, &sendErr, &raw)
		if err == sql.ErrNoRows {
			return actionResult{}, fail(404, "Settlement not found")
		}
		if err != nil {
			return actionResult{}, err
		}
		gate, err := restoreGateSet(ctx, c.Tx)
		if err != nil {
			return actionResult{}, err
		}
		var bumpHeld bool
		if err = c.Tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settlement_bumps WHERE settlement_id=$1 AND send_error LIKE $2)", id, restoredHoldPrefix+"%").Scan(&bumpHeld); err != nil {
			return actionResult{}, err
		}
		switch {
		case !raw.Valid || state != "signed" && state != "broadcast":
			return actionResult{}, fail(409, "This settlement has no stored transaction waiting to be broadcast. Nothing was changed.")
		case sendErr == sendRebroadcastOK:
			return actionResult{}, fail(409, "Its rebroadcast is already confirmed: the market broadcasts it once on its next pass. Nothing was changed.")
		case sendErr == sendRebroadcastSent && gate:
			return actionResult{}, fail(409, "Its confirmed rebroadcast was already sent: the market follows its confirmations and does not send it again while it reconciles its records after a restore. Nothing was changed.")
		case !restoreHeld(sendErr) && !gate && !bumpHeld:
			return actionResult{}, fail(409, "This settlement is not held: the market broadcasts it by itself. Nothing was changed.")
		}
		res, err := c.Tx.ExecContext(ctx, "UPDATE settlements SET send_error=$2,updated=now() WHERE id=$1 AND state=$3 AND send_error=$4", id, sendRebroadcastOK, state, sendErr)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return actionResult{}, fail(409, "The settlement changed meanwhile. Reload the admin page. Nothing was changed.")
		}
		// P2-d: the confirmation covers the settlement's fee bumps too; the market then sends the highest-fee stored
		// transaction once (trackBroadcast).
		if _, err = c.Tx.ExecContext(ctx, "UPDATE settlement_bumps SET send_error='',updated=now() WHERE settlement_id=$1 AND send_error LIKE $2", id, restoredHoldPrefix+"%"); err != nil {
			return actionResult{}, err
		}
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) SELECT id,state,state,$2,$3 FROM orders WHERE id=$1", orderID, c.User.ID,
			"An administrator checked that this escrow settlement's transaction was not already sent or replaced after the market restored its records from a backup; the market broadcasts it once on its next pass."); err != nil {
			return actionResult{}, err
		}
		audit := "Confirmed the rebroadcast of escrow settlement " + strconv.FormatInt(id, 10) + " (" + settlementKind(kind) + ") of order " + shortID(orderID) +
			" after a restore from backup: the administrator checked that its transaction was not already sent or replaced (" + c.A.escrowNetwork() + ")"
		return actionResult{Redirect: "/admin", Notice: "escrow-rebroadcast", Audit: audit}, nil
	})
}

// seenWhilePaused follows a signed settlement whose broadcast is paused or held: when the watch-only wallet has its
// transaction (in the mempool or a block, not conflicted), it was sent after all, so the settlement is recorded as
// broadcast, without sending anything, and a restore's hold on it ends.
func (a *App) seenWhilePaused(ctx context.Context, e *escrowBTC, id int64, orderID, kind, txid string) error {
	w, err := e.walletTx(ctx, txid)
	if err != nil || !w.Found || w.Confirmations < 0 {
		return err
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
	what := settlementKind(kind)
	if err = settleNote(ctx, tx, orderID, "The transaction of the escrow settlement ("+what+") is already on the "+a.escrowNetwork()+" network: the market's watch-only wallet has it, although the market did not send it while its broadcasts were paused. It is recorded as broadcast; the order page shows its transaction.",
		"its escrow settlement ("+what+") is on the "+a.escrowNetwork()+" network; the order page shows its transaction and its confirmations."); err != nil {
		return err
	}
	return tx.Commit()
}

// Reconciliation reads the chain for what the database cannot know (ES-6):
//
//   - findOutsideSpends: an escrow deposit with no settlement of the market covering it (payments.settlement_id NULL)
//     that is no longer unspent (gettxout, mempool included) was spent by a transaction the market did not make: the
//     buyer and the vendor co-spending it directly, say. It is marked (payments.spent_outside, with the spending
//     transaction in spent_by when the watch-only wallet names it, walletSpenders) and flagged for staff as markExternal
//     flags a settlement's deposits: one order note, a notice to both parties and to every moderator and administrator.
//     It never counts towards funding and no settlement spends it. A deposit a settlement claims is followed by that
//     settlement instead (external there, with the same spender lookup), and so is a deposit of an order whose
//     settlement is being built (the build checks each input with gettxout and waits on one that is spent, P1-c1).
//     Only deposits in a block are checked: a deposit still in the mempool is not yet in gettxout's view for certain.
//   - reconcileOrphans, on the escrow watcher's daily check: every address the watch-only wallet holds
//     (watchedAddresses: listreceivedbyaddress with include_empty, never listdescriptors) with no escrows row is listed
//     in escrow_orphans for administrators: a descriptor imported for a checkout that rolled back after the import, or
//     an escrow created after the backup a restored database came from. The orphan sweep (P2-R10, A-366) also reads
//     the three keys of each orphan not yet swept (getaddressinfo, watchKeys) and reserves them in
//     escrow_reserved_keys, so checkout never hands one out again inside another escrow address. After a restore
//     (escrowSweepKey) it runs on every pass, gate or not, until a sweep has read every orphan; that sweep clears the
//     setting in the same transaction that reserves the keys.

// spenderOf is the transaction the watch-only wallet names as spending an input of the stored settlement transaction,
// or "" when it names none (or cannot be read: the spend is flagged without it).
func (a *App) spenderOf(ctx context.Context, e *escrowBTC, stored *psbtPacket) string {
	want := map[string]bool{}
	for _, in := range stored.Tx.In {
		id := in.TxID
		slices.Reverse(id[:])
		want[hex.EncodeToString(id[:])+":"+strconv.FormatUint(uint64(in.Vout), 10)] = true
	}
	found, err := e.walletSpenders(ctx, want)
	if err != nil {
		log.Printf("escrow: the spending transaction could not be looked up: %s", errorClassOf(err))
		return ""
	}
	for _, by := range found {
		return by
	}
	return ""
}

// trackAwaiting follows each settlement waiting for a party's signature (A-356). The market spends nothing before a
// party signs, so an input of the stored transaction that is no longer unspent (gettxout, mempool included) was spent
// without the market: when the watch-only wallet holds the stored transaction itself, the buyer and the vendor signed
// the copies they downloaded and broadcast it, and it is checked and recorded as broadcast (spentBeforeCosign, as for a
// party-signed one, A-344); any other transaction makes the settlement external, flagged for staff, so its order page
// stops asking for a signature that can never complete. A physical order's release before the outcome (shipped or
// disputed) still waits for a party's signature once one party signed, and the market never spends it either, so a
// party-signed one is followed the same way (A-385): the parties may have combined the stored signature with one made
// offline and broadcast it. It runs while the restore gate is set too: it only watches.
func (a *App) trackAwaiting(ctx context.Context, e *escrowBTC) error {
	return a.eachSettlement(ctx, "not followed", `SELECT s.id FROM settlements s JOIN orders o ON o.id=s.order_id WHERE s.state='awaiting_signature'
		OR s.state='party_signed' AND s.kind='release' AND o.state IN ('shipped','disputed') ORDER BY s.id`, func(id int64) error {
		b, err := a.loadSettleBuild(ctx, id, "awaiting_signature")
		if err == nil && b == nil {
			if b, err = a.loadSettleBuild(ctx, id, "party_signed"); b != nil && !releaseBeforeOutcome(b.Kind, b.OrderState) {
				return nil // the order reached its outcome meanwhile: cosignSettlements completes it
			}
		}
		if err != nil || b == nil {
			return err
		}
		spent, err := a.inputsSpent(ctx, e, b.Stored)
		if err != nil || !spent {
			return err
		}
		return a.spentBeforeCosign(ctx, e, b)
	})
}

// findOutsideSpends checks the deposits with no settlement of the orders read in this pass (the first bullet above).
func (a *App) findOutsideSpends(ctx context.Context, e *escrowBTC, orders []string, unread map[string]bool) error {
	rows, err := a.db.QueryContext(ctx, `SELECT p.id,p.order_id,p.txid,p.idx FROM payments p WHERE p.order_id=ANY($1) AND p.settlement_id IS NULL
		AND NOT p.locked AND p.confirmations>=1 AND NOT p.spent_outside
		AND NOT EXISTS (SELECT 1 FROM settlements s WHERE s.order_id=p.order_id AND s.state='building') ORDER BY p.id`, orders)
	if err != nil {
		return err
	}
	type dep struct {
		id            int64
		order, txid   string
		vout          int64
		outpointLabel string
	}
	var deps []dep
	for rows.Next() {
		var d dep
		if err = rows.Scan(&d.id, &d.order, &d.txid, &d.vout); err != nil {
			rows.Close()
			return err
		}
		if !unread[d.order] {
			d.outpointLabel = d.txid + ":" + strconv.FormatInt(d.vout, 10)
			deps = append(deps, d)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	spent := map[string][]dep{}
	want := map[string]bool{}
	var order []string
	for _, d := range deps {
		u, err := e.utxo(ctx, d.txid, uint32(d.vout))
		if err != nil {
			return &watchError{label: "escrow order " + shortID(d.order), class: "deposit not checked: " + errorClassOf(err), err: err}
		}
		if u.Found {
			continue
		}
		if spent[d.order] == nil {
			order = append(order, d.order)
		}
		spent[d.order] = append(spent[d.order], d)
		want[d.outpointLabel] = true
	}
	if len(want) == 0 {
		return nil
	}
	by, err := e.walletSpenders(ctx, want)
	if err != nil {
		log.Printf("escrow: the transaction spending an escrow deposit could not be looked up: %s", errorClassOf(err))
		by = map[string]string{}
	}
	var errs []error
	for _, o := range order {
		ids, spenders := []int64{}, map[int64]string{}
		for _, d := range spent[o] {
			ids = append(ids, d.id)
			spenders[d.id] = by[d.outpointLabel]
		}
		if err = a.markOutsideSpend(ctx, o, ids, spenders); err != nil {
			errs = append(errs, &watchError{label: "escrow order " + shortID(o), class: "outside spend not recorded: " + errorCause(err), err: err})
		}
	}
	return errors.Join(errs...)
}

// markOutsideSpend marks an order's deposits spent outside the market and flags them, once (the first bullet above).
func (a *App) markOutsideSpend(ctx context.Context, orderID string, ids []int64, spenders map[int64]string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", orderID))
	if err != nil {
		return err
	}
	var sum, n int64
	var txids, named []string
	for _, id := range ids {
		var by any
		if s := spenders[id]; s != "" {
			by = s
		}
		var amt int64
		var txid string
		err = tx.QueryRowContext(ctx, `UPDATE payments SET spent_outside=true,spent_by=$2,`+newFlagSQL+`,updated=now() WHERE id=$1 AND order_id=$3
			AND settlement_id IS NULL AND NOT spent_outside RETURNING txid,amount`, id, by, orderID).Scan(&txid, &amt)
		if err == sql.ErrNoRows {
			continue // claimed or marked meanwhile
		}
		if err != nil {
			return err
		}
		sum, n, named = sum+amt, n+1, append(named, depositName(txid, amt, "BTC"))
		if s := spenders[id]; s != "" && !slices.Contains(txids, s) {
			txids = append(txids, s)
		}
	}
	if n == 0 {
		return nil
	}
	which := "the market's watch-only wallet does not name it"
	if len(txids) > 0 {
		which = "transaction " + strings.Join(txids, ", ")
	}
	// The note names the deposits, so the desk dates their flags by it (QA462F-1).
	slices.Sort(named)
	note := fmt.Sprintf("%d deposit(s) to this escrow (%s BTC) were spent by a transaction this market did not make (%s), with no settlement of the market covering them: those coins have left the escrow, and no settlement of the market will spend them.%s Moderator review required.",
		n, amount(sum, 8), which, namedSentence(named))
	if err = a.flagOrder(ctx, tx, &o, note, "Order "+shortID(orderID)+": coins at its escrow were spent by a transaction this market did not make. Market staff were notified."); err != nil {
		return err
	}
	log.Printf("escrow: order %s: %d escrow deposit(s) spent by a transaction the market did not make", shortID(orderID), n)
	return tx.Commit()
}

// escrowOrderLabel is the order ID in a watch-only wallet label "escrow:<order ID>", or "".
func escrowOrderLabel(label string) string {
	id, ok := strings.CutPrefix(label, "escrow:")
	if !ok || id == "" || len(id) > 100 || strings.IndexFunc(id, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-')
	}) >= 0 {
		return ""
	}
	return id
}

// reconcileOrphans lists the watch-only wallet's addresses with no escrows row (the second bullet above), replacing
// the previous list: an address that has an escrow row again, or that the wallet no longer holds, leaves it. It reserves
// the keys of each orphan not yet in escrow_reserved_keys (the orphan sweep), and once every orphan's keys were read it
// clears escrowSweepKey; an orphan whose keys could not be read leaves the setting as it was, and the error is returned.
func (a *App) reconcileOrphans(ctx context.Context, e *escrowBTC) error {
	watched, err := e.watchedAddresses(ctx)
	if err != nil {
		return &watchError{label: "escrow", class: "orphan check failed: " + errorClassOf(err), err: err}
	}
	addrs := make([]string, 0, len(watched))
	for _, w := range watched {
		if _, ok := escrowAddressForm(e.network, w.Address); ok {
			addrs = append(addrs, w.Address)
		}
	}
	// The sweep's reads come before the transaction: the orphans as the database sees them now, less those swept before.
	unswept, err := a.unsweptOrphans(ctx, addrs)
	if err != nil {
		return err
	}
	keys := map[string][][]byte{}
	var sweepErr error
	for _, addr := range unswept {
		k, err := e.watchKeys(ctx, addr)
		if err != nil {
			sweepErr = &watchError{label: "escrow", class: "orphan sweep incomplete: " + errorClassOf(err), err: err}
			continue
		}
		keys[addr] = k
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	known, err := collectStrings(ctx, tx, "SELECT address FROM escrows WHERE coin='BTC' AND address=ANY($1)", addrs)
	if err != nil {
		return err
	}
	// Never nil: a nil slice is sent as NULL, and NOT (address=ANY(NULL)) is NULL, so the DELETE below would keep every
	// row once the wallet holds no orphan at all (A-358).
	orphans := []string{}
	added, reserved := 0, int64(0)
	for _, w := range watched {
		if !slices.Contains(addrs, w.Address) || slices.Contains(known, w.Address) {
			continue
		}
		orphans = append(orphans, w.Address)
		var inserted bool
		if err = tx.QueryRowContext(ctx, `INSERT INTO escrow_orphans(address,order_id,received) VALUES($1,$2,$3)
			ON CONFLICT (address) DO UPDATE SET order_id=EXCLUDED.order_id,received=EXCLUDED.received,last_seen=now() RETURNING (xmax=0)`,
			w.Address, escrowOrderLabel(w.Label), max(w.Received, 0)).Scan(&inserted); err != nil {
			return err
		}
		if inserted {
			added++
		}
		for _, k := range keys[w.Address] {
			res, err := tx.ExecContext(ctx, "INSERT INTO escrow_reserved_keys(pubkey,address) VALUES($1,$2) ON CONFLICT (pubkey) DO NOTHING", hex.EncodeToString(k), w.Address)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			reserved += n
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM escrow_orphans WHERE NOT (address=ANY($1))", orphans); err != nil {
		return err
	}
	if sweepErr == nil {
		if _, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", escrowSweepKey); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if added > 0 {
		log.Printf("escrow: %d address(es) in the watch-only wallet have no escrow record; listed on the admin page", added)
	}
	if reserved > 0 {
		log.Printf("escrow: reserved %d child key(s) of escrow addresses with no escrow record; no new escrow uses them", reserved)
	}
	return sweepErr
}

// unsweptOrphans is the addresses among addrs with no escrows row whose keys the orphan sweep has not reserved yet.
func (a *App) unsweptOrphans(ctx context.Context, addrs []string) ([]string, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT a FROM unnest($1::text[]) a WHERE NOT EXISTS (SELECT 1 FROM escrows WHERE coin='BTC' AND address=a)
		AND NOT EXISTS (SELECT 1 FROM escrow_reserved_keys WHERE address=a) ORDER BY a`, addrs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
