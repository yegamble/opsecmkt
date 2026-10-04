package market

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
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
// transfer of the wallet is in the pool. Then change_wallet_password to 32 random bytes that are never stored or
// logged, store and close_wallet; then, in one statement, the sealed seed is wiped, destroyed_at recorded and the wallet
// is destroyed. A destroyed wallet is never opened again. The market cannot reach the wallet volume, so /admin lists the
// destroyed wallets' files for the operator to delete (escrow_xmr_restore.go); backups keep copies until they expire.
// After destruction, the order page of each of its orders says that Monero sent to the order's address since cannot be
// recovered (ES-8).
//
// A wallet the wallet-rpc refuses to open (xmrWalletUnopenable: a definite refusal, a file gone or a password that no
// longer opens it, not a transport error) is destroyed again when destroy_after has passed and its records allow its
// destruction (its password was replaced after the backup a database was restored from, or the market stopped between
// change_wallet_password and its record): the restored seal is wiped. Otherwise it is missing: its free slots are
// retired and the administrators told, once. A missing wallet is tried on every pass; one that opens again is closing.

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
	var due bool
	if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM xmr_escrow_wallets w WHERE w.id=$1 AND w.state='retained' AND "+xmrDestroyableSQL+")", w.id, now).Scan(&due); err != nil || !due {
		return false, err
	}
	if b, err := x.rpc.balance(ctx); err != nil || b.Balance != 0 || b.Unlocked != 0 {
		return false, err
	}
	if busy, err := x.rpc.inPool(ctx); err != nil || busy {
		return false, err
	}
	return true, a.xmrDestroy(ctx, w, now)
}

// xmrDestroy replaces the open wallet's password by 32 random bytes that are never kept, stores and closes it, then
// records it destroyed with its seal wiped. A failure after the new password took effect leaves the wallet unopenable,
// and the next pass destroys it again (xmrWalletUnopenable).
func (a *App) xmrDestroy(ctx context.Context, w xmrWalletRow, now time.Time) error {
	x := a.escrowXMR
	secret := make([]byte, 32)
	rand.Read(secret)
	err := x.rpc.changeWalletPassword(ctx, xmrWalletPassword(x.secret, w.name), hex.EncodeToString(secret))
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
	res, err := a.db.ExecContext(ctx, "UPDATE xmr_escrow_wallets SET state='destroyed',seed_sealed=NULL,destroyed_at=$2,updated=now() WHERE id=$1 AND state='retained' AND destroy_after<=$2", w.id, now)
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

// xmrWalletUnopenable records wallet w, which the wallet-rpc refused to open: destroyed again when its records allow its
// destruction at now, otherwise missing, with its free slots retired and the administrators told once (see the file
// comment).
func (a *App) xmrWalletUnopenable(ctx context.Context, w xmrWalletRow, now time.Time) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	var due bool
	if err = tx.QueryRowContext(ctx, "SELECT w.state,COALESCE("+xmrDestroyableSQL+",false) FROM xmr_escrow_wallets w WHERE w.id=$1 FOR UPDATE", w.id, now).Scan(&state, &due); err != nil {
		return err
	}
	switch {
	case due:
		// Only a retained or missing wallet moves to destroyed (migration 084).
		if _, err = tx.ExecContext(ctx, "UPDATE xmr_escrow_wallets SET state='missing',updated=now() WHERE id=$1 AND state NOT IN ('retained','missing')", w.id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE xmr_escrow_wallets SET state='destroyed',seed_sealed=NULL,destroyed_at=$2,updated=now() WHERE id=$1", w.id, now); err != nil {
			return err
		}
		log.Printf("escrow: Monero escrow wallet %s does not open and was due for destruction: recorded destroyed again, its restored sealed seed wiped", w.name)
	case state == "missing":
		return nil
	default:
		if _, err = tx.ExecContext(ctx, "UPDATE xmr_escrow_wallets SET state='missing',updated=now() WHERE id=$1", w.id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE xmr_escrow_slots SET state='retired',updated=now() WHERE wallet_id=$1 AND state='free'", w.id); err != nil {
			return err
		}
		if err = notifyAdmins(ctx, tx, "Monero escrow wallet "+w.name+" cannot be opened on the escrow wallet-rpc (its file is gone or no longer opens with the market's password), so the market cannot read its accounts or send its settlements; it is marked missing and takes no order. "+
			"Restore its files from a backup of the wallet volume, or restore it from its sealed seed; the market tries it again on every pass."); err != nil {
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
