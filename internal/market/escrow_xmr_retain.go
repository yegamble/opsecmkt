package market

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"
)

// Retention and destruction of Monero escrow wallets (WR-escrow Phase 3, slice X-f; P3-R9; ES-7, ES-8: one escrow
// wallet per month, kept 90 days after its last order closed).
//
// Retention (xmrRetain, each pass). A wallet's last_closed_at is the latest close (completed, cancelled or resolved) of
// an order whose account it holds, and destroy_after 90 days later; both only move later (migration 084). A closing
// wallet with no open order is retained (from that time on the watcher's clock when none of its orders closed). A retained
// wallet is still read on every pass, so a deposit to a closed order's escrow within those 90 days is returned to the
// order's refund address by a stray refund (escrow_xmr_settle.go).
//
// Destruction (xmrDestroyIfDue, in the wallet's pass, under the watcher's lock, never while the restore gate or the
// sweep is set). Every precondition is required: destroy_after has passed on the watcher's clock; no order of the wallet
// is open; no settlement of it is requested, built or relayed, and none is still followed to 100 confirmations (so the
// last one is 100 deep); get_balance over all accounts shows a zero balance and a zero unlocked balance; and no
// transfer of the wallet is in the pool. A retained wallet past destroy_after that something keeps (coins, a transfer in
// the pool, a settlement still in progress) is flagged to the administrators once, with the accounts that hold coins
// (xmrDestroyHeld). Then the start of its destruction is committed (destroy_started_at, migration 086), then
// change_wallet_password to 32 random bytes that are never stored or logged, store and close_wallet; then, in one
// statement, the sealed seed is wiped, destroyed_at recorded and the wallet is destroyed. A destroyed wallet is never
// opened again. The market cannot reach the wallet volume, so /admin lists the destroyed wallets' files for the
// operator to delete (escrow_xmr_restore.go); backups keep copies until they expire.
// After destruction, the order page of each of its orders says that Monero sent to the order's address since cannot be
// recovered (ES-8).
//
// A wallet the wallet-rpc refuses to open (xmrWalletUnopenable: a definite refusal, not a transport error) is missing:
// its free slots are retired, the administrators are told once and its seal is kept. A file another wallet program has
// open (xmrOpenLocked) is not refused for good and only waits (escrow_xmr_watch.go). A wallet is destroyed again
// automatically only when the market itself began destroying it (A-419, amended): its destroy_started_at is recorded,
// open_wallet refuses it with its own wrong password (xmrOpenPassword), the administrators were never told it holds
// something, its records allow its destruction and neither the restore gate nor the sweep is set. That is a destruction
// interrupted between change_wallet_password and its record (a crash, or a restore from a backup taken in between).
// "Invalid password" alone proves nothing more: a wallet restored from its seed with a mistyped secret, another
// install's file of the same name and a damaged keys file are refused alike. A file that is gone may be a volume not
// yet restored, and the wallet may still hold coins the records do not show (an account paid with no order, a failed
// settlement, account 0), so its seal is never wiped on any of that evidence: a missing wallet past destroy_after is
// listed on /admin, where an administrator who is not a party to its orders confirms its destruction with a password
// step-up (xmrConfirmDestroyedAction, escrow_xmr_restore.go). A missing wallet is tried on every pass; one that opens
// again is closing, or retained when its destruction had begun (its password change never took effect): it is then
// destroyed by the usual rule, every precondition checked again, its destroy_started_at kept as it was.

// xmrRetention is ES-7's retention: a wallet is destroyed no earlier than this after its last order closed. Migration
// 084 requires it, in hours, so that it does not depend on the session's time zone.
const xmrRetention = "interval '2160 hours'"

// xmrClosedSQL is an order state that closes an escrow for good.
const xmrClosedSQL = "('completed','cancelled','resolved')"

// xmrDestroyableSQL holds for wallet w (the row alias) when its records allow its destruction at $2, the watcher's
// clock: destroy_after has passed, no order of it is open, and none of its settlements is live or still followed.
const xmrDestroyableSQL = `w.destroy_after <= $2
	AND NOT EXISTS (SELECT 1 FROM xmr_escrow_slots s JOIN orders o ON o.id=s.order_id WHERE s.wallet_id=w.id AND o.state NOT IN ` + xmrClosedSQL + `)
	AND NOT EXISTS (SELECT 1 FROM xmr_escrow_slots s JOIN xmr_settlements x ON x.order_id=s.order_id WHERE s.wallet_id=w.id
		AND (x.state IN ('requested','built','relayed') OR x.state='confirmed' AND x.reorg_safe_at IS NULL))`

// xmrRetain records each wallet's last order close and retains each closing wallet with no open order at now (see the
// file comment).
func (a *App) xmrRetain(ctx context.Context, now time.Time) error {
	if _, err := a.db.ExecContext(ctx, `WITH closed AS (SELECT s.wallet_id,max(e.created) AS at FROM xmr_escrow_slots s
			JOIN orders o ON o.id=s.order_id AND o.state IN `+xmrClosedSQL+`
			JOIN order_events e ON e.order_id=o.id AND e.to_state=o.state AND e.from_state IS DISTINCT FROM e.to_state
			GROUP BY s.wallet_id)
		UPDATE xmr_escrow_wallets w SET last_closed_at=closed.at,destroy_after=closed.at+`+xmrRetention+`,updated=now() FROM closed
		WHERE w.id=closed.wallet_id AND w.state IN ('active','closing','retained','missing') AND (w.last_closed_at IS NULL OR w.last_closed_at<closed.at)`); err != nil {
		return err
	}
	_, err := a.db.ExecContext(ctx, `UPDATE xmr_escrow_wallets w SET state='retained',last_closed_at=COALESCE(last_closed_at,$1),
		destroy_after=COALESCE(destroy_after,$1::timestamptz+`+xmrRetention+`),updated=now()
		WHERE state='closing' AND NOT EXISTS (SELECT 1 FROM xmr_escrow_slots s JOIN orders o ON o.id=s.order_id WHERE s.wallet_id=w.id AND o.state NOT IN `+xmrClosedSQL+`)`, now)
	return err
}

// xmrDestroyIfDue destroys the open, checked, retained wallet w when every precondition holds at now (see the file
// comment), and reports whether it did.
func (a *App) xmrDestroyIfDue(ctx context.Context, w xmrWalletRow, now time.Time) (bool, error) {
	x := a.escrowXMR
	var past, allowed bool
	err := a.db.QueryRowContext(ctx, "SELECT COALESCE(w.destroy_after<=$2,false),COALESCE("+xmrDestroyableSQL+",false) FROM xmr_escrow_wallets w WHERE w.id=$1 AND w.state='retained'", w.id, now).Scan(&past, &allowed)
	if err == sql.ErrNoRows || err == nil && !past {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	b, err := x.rpc.balance(ctx)
	if err != nil {
		return false, err
	}
	busy, err := x.rpc.inPool(ctx)
	if err != nil {
		return false, err
	}
	if !allowed || b.Balance != 0 || b.Unlocked != 0 || busy {
		return false, a.xmrDestroyHeld(ctx, w)
	}
	return true, a.xmrDestroy(ctx, w, now)
}

// xmrDestroyHeldKey is the setting that records that the administrators were told retained wallet id is kept past its
// destroy_after.
func xmrDestroyHeldKey(id int64) string {
	return "escrow_xmr_destroy_held_" + strconv.FormatInt(id, 10)
}

// xmrDestroyHeld tells the administrators, once, that the open retained wallet w is past destroy_after but kept, with
// the accounts that hold coins, so that someone settles or returns them.
func (a *App) xmrDestroyHeld(ctx context.Context, w xmrWalletRow) error {
	var told bool
	if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key=$1)", xmrDestroyHeldKey(w.id)).Scan(&told); err != nil || told {
		return err
	}
	accounts, err := a.escrowXMR.rpc.accounts(ctx)
	if err != nil {
		return err
	}
	var funded []string
	for _, acc := range accounts {
		if acc.Balance > 0 {
			funded = append(funded, "account "+strconv.FormatInt(acc.Index, 10)+" ("+amount(acc.Balance, 12)+" XMR)")
		}
	}
	held := "No account holds coins, but a settlement of it is still in progress or a transfer of it is in the pool."
	if len(funded) > 0 {
		held = "Accounts holding coins: " + strings.Join(funded, ", ") + "."
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO NOTHING", xmrDestroyHeldKey(w.id))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if err = notifyAdmins(ctx, tx, "Monero escrow wallet "+w.name+" is past its destruction date (90 days after its last order closed) but the market keeps it: something is still in it. "+held+
		" Settle or return what it holds; the market destroys it once nothing is left."); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: Monero escrow wallet %s is past its destruction date but still holds something; kept, administrators notified", w.name)
	return nil
}

// xmrDestroy records that the market began destroying the open wallet (destroy_started_at, migration 086), replaces
// its password by 32 random bytes that are never kept, stores and closes it, then records it destroyed with its seal
// wiped. A failure after the new password took effect leaves the wallet unopenable, and the next pass destroys it again
// on that record (xmrWalletUnopenable).
func (a *App) xmrDestroy(ctx context.Context, w xmrWalletRow, now time.Time) error {
	x := a.escrowXMR
	res, err := a.db.ExecContext(ctx, "UPDATE xmr_escrow_wallets SET destroy_started_at=COALESCE(destroy_started_at,$2),updated=now() WHERE id=$1 AND state='retained' AND destroy_after<=$2", w.id, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errXMRWalletNotWritten
	}
	secret := make([]byte, 32)
	rand.Read(secret)
	err = x.rpc.changeWalletPassword(ctx, xmrWalletPassword(x.secret, w.name), hex.EncodeToString(secret))
	clear(secret)
	if err != nil {
		return err
	}
	if err = x.rpc.store(ctx); err != nil {
		return err
	}
	if err = x.rpc.closeWallet(ctx); err != nil {
		return err
	}
	res, err = a.db.ExecContext(ctx, "UPDATE xmr_escrow_wallets SET state='destroyed',seed_sealed=NULL,destroyed_at=$2,updated=now() WHERE id=$1 AND state='retained' AND destroy_after<=$2", w.id, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errXMRWalletNotWritten
	}
	log.Printf("escrow: Monero escrow wallet %s destroyed (90 days after its last order closed): its password was replaced by one never kept and its sealed seed wiped; delete its files from the escrow wallet volume", w.name)
	return nil
}

// xmrWalletDenied reports a definite refusal by the wallet-rpc: it answered with an error (rpc:<code>), as opposed to a
// transport failure (a timeout, unreachable, a reply that is not one), which is retried.
func xmrWalletDenied(err error) bool {
	var re *xmrRPCError
	return errors.As(err, &re) && re.Service == "wallet" && strings.HasPrefix(re.Class, "rpc:")
}

// xmrWalletUnopenable records wallet w, which the wallet-rpc refused to open: destroyed again when mayDestroy (its own
// wrong-password refusal, with neither the restore gate nor the sweep set), the market itself began destroying it
// (destroy_started_at), the administrators were never told it holds something (xmrDestroyHeld) and its records allow
// its destruction at now; otherwise missing, its seal kept, its free slots retired and the administrators told once
// (see the file comment).
func (a *App) xmrWalletUnopenable(ctx context.Context, w xmrWalletRow, now time.Time, mayDestroy bool) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	var due bool
	if err = tx.QueryRowContext(ctx, "SELECT w.state,COALESCE("+xmrDestroyableSQL+` AND w.destroy_started_at IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM settings WHERE key=$3),false) FROM xmr_escrow_wallets w WHERE w.id=$1 FOR UPDATE`, w.id, now, xmrDestroyHeldKey(w.id)).Scan(&state, &due); err != nil {
		return err
	}
	due = due && mayDestroy
	switch {
	case due:
		// A wallet whose destruction began is retained or missing (migration 086), and moves to destroyed.
		if _, err = tx.ExecContext(ctx, "UPDATE xmr_escrow_wallets SET state='destroyed',seed_sealed=NULL,destroyed_at=$2,updated=now() WHERE id=$1", w.id, now); err != nil {
			return err
		}
		log.Printf("escrow: Monero escrow wallet %s no longer opens with the market's password after the market began destroying it: recorded destroyed, its sealed seed wiped", w.name)
	case state == "missing":
		return nil
	default:
		if _, err = tx.ExecContext(ctx, "UPDATE xmr_escrow_wallets SET state='missing',updated=now() WHERE id=$1", w.id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE xmr_escrow_slots SET state='retired',updated=now() WHERE wallet_id=$1 AND state='free'", w.id); err != nil {
			return err
		}
		if err = notifyAdmins(ctx, tx, "Monero escrow wallet "+w.name+" cannot be opened on the escrow wallet-rpc (its file is gone or no longer opens with the market's password), so the market cannot read its accounts or send its settlements; it is marked missing and takes no order, and its sealed seed is kept. "+
			"Restore its files from a backup of the wallet volume, or restore it from its sealed seed; the market tries it again on every pass. Once it is past its destruction date, the admin page lets you confirm its destruction."); err != nil {
			return err
		}
		log.Printf("escrow: Monero escrow wallet %s cannot be opened; marked missing, administrators notified", w.name)
	}
	return tx.Commit()
}

// xmrAfterDestroyText is the order page's line once the wallet that held the order's account was destroyed at at (ES-8).
func xmrAfterDestroyText(at time.Time) string {
	return "Monero sent to this order's address after " + at.UTC().Format("2006-01-02 15:04 UTC") + " cannot be recovered: the escrow wallet that held its account was destroyed, 90 days after its last order closed."
}
