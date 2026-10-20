package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Monero escrow after a restore from backup (WR-escrow Phase 3, slice X-f; P3-R10), as Bitcoin escrow's recovery
// (escrow_recovery.go). A restored database knows nothing that happened after its backup: escrow wallets and accounts
// created since, settlements built or relayed since. scripts/restore.sh (and the manual protection SQL in UPGRADING.md
// section 6) therefore sets the restore gate, holds every built or relayed Monero settlement with
// restoredSettlementHold in xmr_settlements.send_error, retires every free slot, moves every active or sealed wallet to
// closing (no restored wallet ever offers an account to a new order again; the reconcile adds an account the records
// lack as retired to a wallet that is not active) and sets xmrEscrowSweepKey, which makes Monero escrow checkout answer
// 503 and pauses wallet and slot creation until the sweep below has run.
//
// The sweep runs in the Monero escrow watcher's pass while xmrEscrowSweepKey is set, gate or not, and clears it only
// once one pass did all of it:
//
//   - xmrSweepProbe opens the wallet names escrow-YYYYMM-NN after the newest row of each month, from 12 months before
//     the current month to next month. Nothing the database holds bounds it: an order placed after the restore, or an
//     orphan row an earlier, incomplete sweep inserted, says nothing of when the backup was taken (A-420). A wallet that opens with the derived password
//     and passes the network check is a wallet created after the backup: xmrSealOrphan records it as an orphan with
//     its seed sealed to the pinned key (restore height 0, since its true one is unknown), lists its funded accounts to
//     the administrators and retains it, so it never offers a slot and is destroyed by the usual rule (its accounts are
//     added as retired slots when it is read). A file that does not open is recorded as an orphan, never opened again,
//     and the administrators are told; the probe goes on past it. A month's probe ends at the first name with no file;
//     a file another wallet program has open leaves the sweep incomplete, to be probed again.
//   - every wallet that may hold coins is opened and read (escrow_xmr_watch.go), and one that does not open is recorded
//     missing (escrow_xmr_retain.go; nothing is destroyed during the sweep); a wallet older than the database (fewer
//     accounts than the highest slot names) gets a subaddress lookahead past that slot and a rescan (xmrLookahead), so
//     deposits to the accounts it does not know are seen.
//
// A held settlement is listed on /admin; an administrator who checked the escrow daemon and wallet confirms, with a
// password step-up and an audit row, that its stored transaction was not already relayed or replaced
// (xmrRebroadcastAction). Its send_error becomes sendRebroadcastOK, and the watcher relays the stored metadata once,
// under the gate too, then clears it (escrow_xmr_settle.go); nothing is rebuilt. /admin also lists the destroyed
// wallets' files for the operator to delete, and each missing wallet past its destroy_after, whose destruction an
// administrator who is not a party to its orders confirms with a password step-up once its files are gone and nothing
// is owed (xmrConfirmDestroyedAction, A-419): its seal is wiped then, never by the watcher on that evidence.

func init() {
	registerAction("/admin/escrow-xmr-rebroadcast", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true,
		ReceiptAuthorize: xmrRebroadcastReceiptAuthorize, Off: escrowXMROff, Run: xmrRebroadcastAction})
	registerAction("/admin/escrow-xmr-destroyed", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true,
		ReceiptAuthorize: xmrDestroyedReceiptAuthorize, Off: escrowXMROff, Run: xmrConfirmDestroyedAction})
	registerLoader("admin", xmrEscrowRecoveryLoader)
}

// xmrRebroadcastReceiptAuthorize binds a saved response to a settlement that still belongs to a Monero escrow.
// Mutable hold and relay state stay in the first callback; replay only returns the already committed result.
func xmrRebroadcastReceiptAuthorize(c *actionCtx) error {
	id, err := strconv.ParseInt(c.Form.Get("settlement_id"), 10, 64)
	if err != nil || id <= 0 {
		return fail(400, "Unknown settlement")
	}
	var exists bool
	err = c.Tx.QueryRowContext(c.Ctx(), `SELECT EXISTS(SELECT 1 FROM xmr_settlements s JOIN escrows e ON e.order_id=s.order_id
		WHERE s.id=$1 AND e.coin='XMR')`, id).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return fail(404, "Settlement not found")
	}
	return nil
}

// xmrLookaheadMajor is monero-wallet-rpc's default account lookahead, never lowered; minor lookahead 1 is enough, since
// every escrow address is an account's address 0.
const xmrLookaheadMajor = 50

var errXMRSweepSeedKey = errors.New("monero escrow: a wallet created after the backup was found, and its seed cannot be sealed while ESCROW_XMR_SEED_KEY is not the pinned seed key")

// xmrSweepProbe probes the wallet names that may have been created after the backup (see the file comment).
func (a *App) xmrSweepProbe(ctx context.Context, now time.Time) error {
	x := a.escrowXMR
	for m := monthStart(now).AddDate(-1, 0, 0); !m.After(monthStart(now).AddDate(0, 1, 0)); m = m.AddDate(0, 1, 0) {
		var n int
		if err := a.db.QueryRowContext(ctx, "SELECT COALESCE(max(substr(name,15,2)::int),0) FROM xmr_escrow_wallets WHERE month=$1::date", m.Format("2006-01-02")).Scan(&n); err != nil {
			return err
		}
	month:
		for n++; n <= 99; n++ {
			name := fmt.Sprintf("escrow-%s-%02d", m.Format("200601"), n)
			var retired bool
			if err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM retention_xmr_retired_wallets WHERE name_hash=$1)`, retentionTechnicalHash(name)).Scan(&retired); err != nil {
				return err
			}
			if retired {
				continue
			} // Never re-seal a deliberately destroyed seed from a stale wallet-volume copy.
			found, err := x.rpc.probeWallet(ctx, name, xmrWalletPassword(x.secret, name))
			switch {
			case err == nil && !found:
				break month
			case xmrOpenRefusal(err) == xmrOpenLocked:
				return err // another wallet program has it open: it cannot be told apart yet
			case xmrWalletDenied(err):
				err = a.xmrForeignWallet(ctx, name)
			case err == nil:
				err = a.xmrSealOrphan(ctx, 0, name)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// xmrForeignWallet records wallet file name, which the sweep found but which does not open with the market's password,
// as an orphan (never opened again; the next wallet takes another name) and tells the administrators.
func (a *App) xmrForeignWallet(ctx context.Context, name string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO xmr_escrow_wallets(name,month,state) VALUES($1,to_date(substr($1,8,6),'YYYYMM'),'orphan')", name); err != nil {
		return err
	}
	if err = notifyAdmins(ctx, tx, "Monero escrow wallet "+name+" exists on the escrow wallet-rpc but does not open with the market's password (a file left by another install?). The market recorded it as an orphan and never uses it; check that wallet yourself."); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: Monero escrow wallet %s exists but does not open with the market's password; recorded as an orphan, not used", name)
	return nil
}

// xmrSealOrphan records the open wallet name, which opened with the market's password and which this database did not
// create (a wallet created after the backup it was restored from), as an orphan sealed to the pinned seed key, lists
// its funded accounts to the administrators and retains it: it never offers a slot. id is its row, left creating or
// orphan, or 0 when it has none.
func (a *App) xmrSealOrphan(ctx context.Context, id int64, name string) error {
	x := a.escrowXMR
	if !x.canCreate.Load() {
		return errXMRSweepSeedKey
	}
	if err := a.recheckEscrowXMRNetwork(ctx); err != nil {
		return err
	}
	primary, err := x.rpc.primaryAddress(ctx)
	if err != nil {
		return err
	}
	mnemonic, err := x.rpc.mnemonic(ctx)
	if err != nil {
		return err
	}
	accounts, err := x.rpc.accounts(ctx)
	if err != nil {
		return err
	}
	sealed, fpr, err := x.seedKey.sealSeed(mnemonic)
	if err != nil {
		return err
	}
	var funded []string
	for _, acc := range accounts {
		if acc.Balance > 0 {
			funded = append(funded, "account "+strconv.FormatInt(acc.Index, 10)+" ("+amount(acc.Balance, 12)+" XMR)")
		}
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Restore height 0: the wallet's own is unknown, and a restore from its seed then scans the whole chain.
	if id == 0 {
		err = tx.QueryRowContext(ctx, `INSERT INTO xmr_escrow_wallets(name,month,primary_address,restore_height,seed_sealed,seed_key_fpr,sealed_at,state)
			VALUES($1,to_date(substr($1,8,6),'YYYYMM'),$2,0,$3,$4,now(),'orphan') RETURNING id`, name, primary, sealed, fpr).Scan(&id)
	} else {
		var res sql.Result
		if res, err = tx.ExecContext(ctx, `UPDATE xmr_escrow_wallets SET primary_address=$2,restore_height=0,seed_sealed=$3,seed_key_fpr=$4,sealed_at=now(),state='orphan',updated=now()
			WHERE id=$1 AND state IN ('creating','orphan') AND seed_sealed IS NULL`, id, primary, sealed, fpr); err == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				err = errXMRWalletNotWritten
			}
		}
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE xmr_escrow_wallets SET state='retained',last_closed_at=$2,destroy_after=$2::timestamptz+"+xmrRetention+",updated=now() WHERE id=$1", id, x.clock()); err != nil {
		return err
	}
	coins := "None of its accounts holds coins."
	if len(funded) > 0 {
		coins = "Funded accounts: " + strings.Join(funded, ", ") + ". Check them for coins of orders this database lost."
	}
	if err = notifyAdmins(ctx, tx, "Monero escrow wallet "+name+" was found on the escrow wallet-rpc with no record in this database (created after the backup it was restored from?). "+
		"The market sealed its seed to the seed key and keeps it, but never takes an order into it; it is destroyed by the usual rule once nothing is left in it. "+coins); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: Monero escrow wallet %s, which this database did not create, recorded as an orphan, its seed sealed; %d funded account(s) listed for the administrators", name, len(funded))
	return x.rpc.store(ctx)
}

// xmrLookahead gives the open wallet w, when it knows fewer accounts than its highest slot names (a wallet older than
// the database), a subaddress lookahead past that slot and rescans, so a deposit to an account it does not know is
// seen (see the file comment). The lookahead is written into the wallet's keys file, so later refreshes keep it.
func (a *App) xmrLookahead(ctx context.Context, w xmrWalletRow) error {
	var highest int64
	if err := a.db.QueryRowContext(ctx, "SELECT COALESCE(max(account_index),0) FROM xmr_escrow_slots WHERE wallet_id=$1", w.id).Scan(&highest); err != nil {
		return err
	}
	return a.xmrRaiseLookahead(ctx, w.name, highest)
}

// xmrRaiseLookahead gives the open wallet name, when it knows fewer accounts than highest (the highest account its
// records name), a subaddress lookahead past it and rescans: xmrLookahead for an escrow wallet, and the vendor bond
// wallet's turn in the sweep (vendor_bond.go).
func (a *App) xmrRaiseLookahead(ctx context.Context, name string, highest int64) error {
	x := a.escrowXMR
	accounts, err := x.rpc.accounts(ctx)
	if err != nil {
		return err
	}
	known := int64(len(accounts)) - 1
	if highest <= known {
		return nil
	}
	if err = x.rpc.setSubaddressLookahead(ctx, xmrWalletPassword(x.secret, name), max(highest-known+1, xmrLookaheadMajor), 1); err != nil {
		return err
	}
	log.Printf("escrow: Monero wallet %s knows %d account(s) and its records name account %d: lookahead raised past it, rescanning", name, known, highest)
	return x.rpc.rescan(ctx)
}

// XMREscrowRecoveryView is /admin's Monero escrow recovery section (escrow_xmr_restore.go, escrow_xmr_retain.go).
type XMREscrowRecoveryView struct {
	On        bool   // ESCROW_XMR is on: the relay and destruction confirmations exist
	Gate      bool   // the restore gate is set (A-559)
	Sweep     bool   // a restore asked for the sweep, which has not completed
	Paused    string // what the gate and the sweep pause ("" for neither, xmrRestorePaused)
	Refused   int64  // requests refused by a restore pause since this process started (App.pausedCount)
	Held      []XMREscrowHeldView
	Destroyed []XMRDestroyedWalletView
	Missing   []XMRMissingWalletView
}

// xmrRestorePaused is the staff text naming what a restore pauses and what still runs (A-559, STAFF66-2), shown on the
// staff attention summary (/admin, /moderator) and the Monero escrow recovery view: gate is the restore gate
// (payments_recovery_required: escrow_xmr_create.go, escrow_xmr_settle.go, escrow_xmr_rerequest.go,
// escrow_xmr_deadline.go, escrow_xmr_watch.go, escrow_xmr_retain.go, vendor_bond.go), sweep the restore sweep
// (xmrEscrowSweepKey: checkout, wallet and slot creation, destruction and bond addresses only). "" for neither. Callers
// pass neither while Monero escrow is off: it then pauses nothing, and a market that never ran escrow shows no restore.
func xmrRestorePaused(gate, sweep bool) string {
	switch {
	case gate:
		s := "Monero escrow is paused after a restore from backup (the restore gate is set): new Monero escrow checkouts are refused, " +
			"and the market builds and relays no settlement (a held one an administrator confirms under Admin → Payments is relayed once), carries out no re-request, " +
			"releases no order at its deadline and sends no deadline reminder, creates no escrow wallet, account or vendor bond address, records no vendor bond as paid and destroys no escrow wallet. " +
			"It still watches deposits and follows relayed settlements to their confirmations. " +
			"The operator clears the gate once the restored records are reconciled with the wallets (docs/testnet-runbook.md, Clear the gate)."
		if sweep {
			s += " The restore sweep has not completed yet: the market is still checking its escrow wallets."
		}
		return s
	case sweep:
		return "Monero escrow checkout is paused after a restore from backup until the market has checked its escrow wallets (the restore sweep): " +
			"new Monero escrow checkouts are refused, no escrow wallet, account or vendor bond address is created, no vendor bond is recorded as paid and no escrow wallet is destroyed. " +
			"Settlements, relays and release deadlines run as usual. Checkout reopens by itself once the sweep completes."
	}
	return ""
}

// xmrSweepDoneNotice tells the administrators, once, that the restore sweep completed while the restore gate is still
// set (A-559): nothing reopens until the operator clears it.
const xmrSweepDoneNotice = "Monero escrow's restore sweep is complete: every escrow wallet was checked and each one created after the backup was recorded and kept. " +
	"The restore gate is still set, so new Monero escrow checkouts, settlement building and relay, re-requests, release deadlines and new escrow wallets stay paused " +
	"until the operator clears it once the restored records are reconciled with the wallets (docs/testnet-runbook.md, Clear the gate). See Admin → Payments."

// XMRMissingWalletView is one missing escrow wallet past its destroy_after: an administrator may confirm its destruction.
type XMRMissingWalletView struct {
	ID                 int64
	Name, DestroyAfter string
}

// XMREscrowHeldView is one Monero settlement a restore holds, or whose relay an administrator confirmed.
type XMREscrowHeldView struct {
	ID                  int64
	OrderID, Short      string
	Kind, State, TxHash string
	Confirmed           bool // an administrator confirmed its relay: relayed on the next pass
}

// XMRDestroyedWalletView is one destroyed escrow wallet whose files the operator deletes from the wallet volume.
type XMRDestroyedWalletView struct {
	Name, DestroyedAt string
}

// xmrEscrowRecoveryLoader fills /admin's Monero escrow recovery section while ESCROW_XMR is on, or while a settlement
// is held or a wallet was destroyed.
func xmrEscrowRecoveryLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" {
		return nil
	}
	v := &XMREscrowRecoveryView{On: a.escrowXMR != nil, Refused: a.pausedCount.Load()}
	if err := a.db.QueryRowContext(ctx, "SELECT "+restoreGateSQL+",EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value='true')", xmrEscrowSweepKey).Scan(&v.Gate, &v.Sweep); err != nil {
		return err
	}
	v.Paused = xmrRestorePaused(v.Gate && v.On, v.Sweep && v.On) // with Monero escrow off nothing is paused, and nothing shows
	rows, err := a.db.QueryContext(ctx, `SELECT id,order_id,kind,state,tx_hash,send_error FROM xmr_settlements
		WHERE state IN ('built','relayed') AND (send_error LIKE $1 OR send_error=$2 OR send_error LIKE $3) ORDER BY id LIMIT 200`, restoredHoldPrefix+"%", sendRebroadcastOK,
		xmrRelayRefusing+" % "+xmrRelayConfirmedMark)
	if err != nil {
		return err
	}
	for rows.Next() {
		var h XMREscrowHeldView
		var sendErr string
		if err = rows.Scan(&h.ID, &h.OrderID, &h.Kind, &h.State, &h.TxHash, &sendErr); err != nil {
			rows.Close()
			return err
		}
		h.Short, h.Kind, h.Confirmed = shortID(h.OrderID), settlementKind(h.Kind), xmrRelayConfirmed(sendErr)
		v.Held = append(v.Held, h)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	rows, err = a.db.QueryContext(ctx, `SELECT name,to_char(destroyed_at AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"') FROM xmr_escrow_wallets
		WHERE state='destroyed' ORDER BY destroyed_at DESC,name LIMIT 200`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var w XMRDestroyedWalletView
		if err = rows.Scan(&w.Name, &w.DestroyedAt); err != nil {
			rows.Close()
			return err
		}
		v.Destroyed = append(v.Destroyed, w)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	now := time.Now()
	if a.escrowXMR != nil {
		now = a.escrowXMR.clock()
	}
	rows, err = a.db.QueryContext(ctx, `SELECT id,name,to_char(destroy_after AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"') FROM xmr_escrow_wallets
		WHERE state='missing' AND destroy_after<=$1 ORDER BY destroy_after,name LIMIT 200`, now)
	if err != nil {
		return err
	}
	for rows.Next() {
		var w XMRMissingWalletView
		if err = rows.Scan(&w.ID, &w.Name, &w.DestroyAfter); err != nil {
			rows.Close()
			return err
		}
		v.Missing = append(v.Missing, w)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if !v.On && len(v.Held) == 0 && len(v.Destroyed) == 0 && len(v.Missing) == 0 {
		return nil
	}
	d.XMREscrowRecovery = v
	return nil
}

// xmrRebroadcastAction is an administrator's confirmation, after checking the escrow daemon and the escrow wallet, that
// a held Monero settlement's stored transaction was not already relayed or replaced: the watcher then relays it once.
// Password step-up (and authenticator code when enrolled), compare-and-set on the settlement's state and send_error, an
// order note and an audit row, as escrowRebroadcastAction for Bitcoin escrow.
func xmrRebroadcastAction(c *actionCtx) (actionResult, error) {
	id, err := strconv.ParseInt(c.Form.Get("settlement_id"), 10, 64)
	if err != nil || id <= 0 {
		return actionResult{}, fail(400, "Unknown settlement")
	}
	if c.Form.Get("not_sent") != "confirmed" {
		return actionResult{}, fail(400, "Check the escrow daemon and the escrow wallet first, then confirm that this transaction was not already relayed and that no other transaction spent the escrow's coins. Nothing was changed.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		if _, err := c.Tx.ExecContext(ctx, "SELECT o.id FROM orders o JOIN xmr_settlements s ON s.order_id=o.id WHERE s.id=$1 FOR NO KEY UPDATE OF o", id); err != nil {
			return actionResult{}, err
		}
		var orderID, kind, state, sendErr string
		err := c.Tx.QueryRowContext(ctx, "SELECT order_id,kind,state,send_error FROM xmr_settlements WHERE id=$1 FOR UPDATE", id).Scan(&orderID, &kind, &state, &sendErr)
		if err == sql.ErrNoRows {
			return actionResult{}, fail(404, "Settlement not found")
		}
		if err != nil {
			return actionResult{}, err
		}
		switch {
		case state != "built" && state != "relayed":
			return actionResult{}, fail(409, "This settlement has no stored transaction waiting to be relayed. Nothing was changed.")
		case xmrRelayConfirmed(sendErr):
			return actionResult{}, fail(409, "Its relay is already confirmed: the market relays it on its next pass. Nothing was changed.")
		case !restoreHeld(sendErr):
			return actionResult{}, fail(409, "This settlement is not held: the market relays it by itself. Nothing was changed.")
		}
		res, err := c.Tx.ExecContext(ctx, "UPDATE xmr_settlements SET send_error=$2,updated=now() WHERE id=$1 AND state=$3 AND send_error=$4", id, sendRebroadcastOK, state, sendErr)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return actionResult{}, fail(409, "The settlement changed meanwhile. Reload Admin → Payments (/admin/payments). Nothing was changed.")
		}
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) SELECT id,state,state,$2,$3 FROM orders WHERE id=$1", orderID, c.User.ID,
			"An administrator checked that this Monero escrow settlement's transaction was not already relayed or replaced after the market restored its records from a backup; the market relays it once on its next pass."); err != nil {
			return actionResult{}, err
		}
		audit := "Confirmed the relay of Monero escrow settlement " + strconv.FormatInt(id, 10) + " (" + settlementKind(kind) + ") of order " + shortID(orderID) +
			" after a restore from backup: the administrator checked that its transaction was not already relayed or replaced (" + c.A.xmrEscrowNetwork() + ")"
		return actionResult{Redirect: "/admin/payments", Notice: "escrow-xmr-relay", Audit: audit}, nil
	})
}

// xmrConfirmDestroyedAction is an administrator's confirmation that a missing escrow wallet past its destroy_after is
// gone from the wallet volume and owes nothing: its sealed seed is wiped and it is recorded destroyed (A-419). Only an
// administrator who is not a party to any of its orders, with a password step-up, an explicit check and an audit row;
// refused for a wallet that is not missing, not past destroy_after or with an order or settlement still open, and while
// the restore gate or the sweep is set.
func xmrConfirmDestroyedAction(c *actionCtx) (actionResult, error) {
	id, err := strconv.ParseInt(c.Form.Get("wallet_id"), 10, 64)
	if err != nil || id <= 0 {
		return actionResult{}, fail(400, "Unknown wallet")
	}
	if c.Form.Get("gone") != "confirmed" {
		return actionResult{}, fail(400, "Check the escrow wallet volume and its backups first, then confirm that the wallet's files are gone and that nothing in it is owed to anyone. Nothing was changed.")
	}
	return retentionConfirmedMutation(c, func(c *actionCtx) (string, actionResult, error) {
		ctx := c.Ctx()
		now := c.A.escrowXMR.clock()
		locked, err := lockRetentionXMRWallet(ctx, c.Tx, id)
		if err != nil {
			return "", actionResult{}, err
		}
		if !locked {
			return "", actionResult{}, fail(409, "Preservation registration is in progress for this wallet. Retry after it completes; no wallet recovery state was erased.")
		}
		var name, state string
		var allowed, party, paused bool
		err = c.Tx.QueryRowContext(ctx, `SELECT w.name,w.state,COALESCE(`+xmrDestroyableSQL+`,false),
				EXISTS(SELECT 1 FROM xmr_escrow_slots s JOIN orders o ON o.id=s.order_id JOIN products p ON p.id=o.product_id WHERE s.wallet_id=w.id AND (o.buyer_id=$3 OR p.vendor_id=$3)),
				`+restoreGateSQL+` OR EXISTS(SELECT 1 FROM settings WHERE key=$4 AND value='true')
			FROM xmr_escrow_wallets w WHERE w.id=$1 FOR UPDATE`, id, now, c.User.ID, xmrEscrowSweepKey).Scan(&name, &state, &allowed, &party, &paused)
		if err == sql.ErrNoRows {
			return "", actionResult{}, fail(404, "Wallet not found")
		}
		if err != nil {
			return "", actionResult{}, err
		}
		switch {
		case party:
			return "", actionResult{}, fail(403, "You are a party to an order of this wallet, so you cannot confirm its destruction.")
		case state != "missing":
			return "", actionResult{}, fail(409, "Only a missing escrow wallet's destruction is confirmed here. Reload Admin → Payments (/admin/payments). Nothing was changed.")
		case paused:
			return "", actionResult{}, fail(409, "The market is reconciling a restore from backup: no escrow wallet is destroyed until it has finished. Nothing was changed.")
		case !allowed:
			return "", actionResult{}, fail(409, "This wallet is not past its destruction date, or an order or settlement of it is still open. Nothing was changed.")
		}
		opID, err := c.A.prepareRetentionXMRDestruction(ctx, c.Tx, id, now)
		if err != nil {
			return "", actionResult{}, err
		}
		if _, err = c.Tx.ExecContext(ctx, "UPDATE xmr_escrow_wallets SET state='destroyed',seed_sealed=NULL,destroyed_at=$2,updated=now() WHERE id=$1 AND state='missing'", id, now); err != nil {
			return opID, actionResult{}, err
		}
		audit := "Confirmed the destruction of Monero escrow wallet " + name + ", missing and past its destruction date: the administrator checked that its files are gone and that nothing in it is owed; its sealed seed was wiped (" + c.A.xmrEscrowNetwork() + ")"
		return opID, actionResult{Redirect: "/admin/payments", Notice: "escrow-xmr-gone", Audit: audit}, nil
	})
}
