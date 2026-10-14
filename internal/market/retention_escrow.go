package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Financial closure is distinct from a terminal order state. In particular,
// failed refunds can still hold coins and a confirmed transaction remains live
// until the watcher has followed it to escrowReorgDepth.
const retentionEscrowMonitoring = 90 * 24 * time.Hour
const retentionEscrowFinancial = 180 * 24 * time.Hour
const retentionEscrowReview = 30 * 24 * time.Hour
const retentionEscrowBatch = 100

var errRetentionEscrowBusy = errors.New("retention is waiting for an escrow watcher")

type retentionEscrowOrder struct {
	id, coin, address, walletState string
	terminalAt                     time.Time
	closedAt, destroyedAt          sql.NullTime
}

// retentionFinancialClosure runs inside the maintenance transaction. Both
// watcher locks stay held until that transaction commits or rolls back, including
// subsequent private-content expiry. It makes no wallet mutation and never
// deletes a financial record or disables a database preservation trigger.
func (a *App) retentionFinancialClosure(ctx context.Context, tx *sql.Tx, now time.Time) error {
	for _, lock := range []int64{escrowWatcherLock, xmrEscrowWatcherLock} {
		var acquired bool
		if err := tx.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock($1)", lock).Scan(&acquired); err != nil {
			return err
		}
		if !acquired {
			return errRetentionEscrowBusy
		}
	}
	// A resolved operational exception is itself identifying history. Expire it
	// rather than preserving an invisible order-linked register indefinitely.
	pruned, err := tx.ExecContext(ctx, `WITH old AS (SELECT x.order_id FROM retention_escrow_exceptions x JOIN orders o ON o.id=x.order_id JOIN products p ON p.id=o.product_id
		WHERE x.resolved_at<=$1::timestamptz-interval '180 days' AND NOT `+retentionOrderHeldSQL+` ORDER BY x.resolved_at,x.order_id LIMIT $2)
		DELETE FROM retention_escrow_exceptions x USING old WHERE x.order_id=old.order_id`, now, retentionEscrowBatch)
	if err != nil {
		return err
	}
	prunedCount, err := pruned.RowsAffected()
	if err != nil {
		return err
	}
	cleaned, err := tx.ExecContext(ctx, `WITH old AS (SELECT coin,wallet_name FROM retention_wallet_cleanup WHERE confirmed_at<=$1::timestamptz-interval '180 days' ORDER BY confirmed_at,coin,wallet_name LIMIT $2) DELETE FROM retention_wallet_cleanup c USING old WHERE c.coin=old.coin AND c.wallet_name=old.wallet_name`, now, retentionEscrowBatch)
	if err != nil {
		return err
	}
	cleanedCount, err := cleaned.RowsAffected()
	if err != nil {
		return err
	}
	// Nonfinancial cancelled drafts need no blockchain observation. Preserve the
	// original deadline when a legal hold is released; a hold does not reopen funds.
	if _, err := tx.ExecContext(ctx, `UPDATE orders o SET financial_closed_at=COALESCE(
		(SELECT max(e.created) FROM order_events e WHERE e.order_id=o.id AND e.to_state=o.state AND e.from_state<>e.to_state),o.updated)
		WHERE o.financial_closed_at IS NULL AND o.state IN ('completed','cancelled','resolved')
		AND NOT EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id)
		AND NOT EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id)
		AND NOT EXISTS(SELECT 1 FROM payouts po WHERE po.order_id=o.id)`); err != nil {
		return err
	}
	// A genuine reopened order must not retain its former closure marker.
	if _, err := tx.ExecContext(ctx, `UPDATE orders SET financial_closed_at=NULL,private_expired_at=NULL
		WHERE financial_closed_at IS NOT NULL AND state NOT IN ('completed','cancelled','resolved')`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT o.id,es.coin,es.address,COALESCE(w.state,''),
		COALESCE((SELECT max(e.created) FROM order_events e WHERE e.order_id=o.id AND e.to_state=o.state AND e.from_state<>e.to_state),o.updated),
		o.financial_closed_at,w.destroyed_at
		FROM orders o JOIN escrows es ON es.order_id=o.id
		LEFT JOIN xmr_escrow_slots sl ON sl.id=es.xmr_slot_id LEFT JOIN xmr_escrow_wallets w ON w.id=sl.wallet_id
		WHERE o.state IN ('completed','cancelled','resolved')
		AND (o.financial_checked_at IS NULL OR o.financial_checked_at<=$2::timestamptz-interval '30 minutes')
		AND (o.financial_closed_at IS NOT NULL OR
		 COALESCE((SELECT max(e.created) FROM order_events e WHERE e.order_id=o.id AND e.to_state=o.state AND e.from_state<>e.to_state),o.updated)<=$2::timestamptz-interval '90 days')
		ORDER BY o.financial_checked_at NULLS FIRST,o.id LIMIT $1 FOR UPDATE OF o`, retentionEscrowBatch, now)
	if err != nil {
		return err
	}
	var orders []retentionEscrowOrder
	for rows.Next() {
		var o retentionEscrowOrder
		if err = rows.Scan(&o.id, &o.coin, &o.address, &o.walletState, &o.terminalAt, &o.closedAt, &o.destroyedAt); err != nil {
			rows.Close()
			return err
		}
		orders = append(orders, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "SELECT set_config('opsecmkt.retention_escrow_more',$1,true)", fmt.Sprint(len(orders) == retentionEscrowBatch || prunedCount == retentionEscrowBatch || cleanedCount == retentionEscrowBatch)); err != nil {
		return err
	}
	for _, o := range orders {
		reason, err := a.retentionEscrowBlocker(ctx, tx, o)
		if err != nil {
			return err
		}
		// Recovery gates stop new conclusions but do not move an already-established
		// deadline. Ordinary late funds or new settlements do reopen financial work.
		if retentionEscrowReopened(reason) && o.closedAt.Valid {
			if _, err = tx.ExecContext(ctx, "UPDATE orders SET financial_closed_at=NULL,private_expired_at=NULL WHERE id=$1", o.id); err != nil {
				return err
			}
			o.closedAt = sql.NullTime{}
		}
		due := !now.Before(o.terminalAt.Add(retentionEscrowMonitoring))
		if reason == "" && (due || o.closedAt.Valid) {
			if o.coin == "BTC" {
				reason = a.retentionBTCObservation(ctx, tx, o, now)
			} else if o.walletState != "destroyed" || !o.destroyedAt.Valid {
				reason = "monero_wallet_not_destroyed"
			}
			if retentionEscrowReopened(reason) && o.closedAt.Valid {
				if _, err = tx.ExecContext(ctx, "UPDATE orders SET financial_closed_at=NULL,private_expired_at=NULL WHERE id=$1", o.id); err != nil {
					return err
				}
				o.closedAt = sql.NullTime{}
			}
			if reason == "" && !o.closedAt.Valid {
				at := now
				if o.coin == "XMR" && o.destroyedAt.Time.After(o.terminalAt) {
					at = o.destroyedAt.Time
				}
				if _, err = tx.ExecContext(ctx, "UPDATE orders SET financial_closed_at=$2 WHERE id=$1 AND financial_closed_at IS NULL", o.id, at); err != nil {
					return err
				}
				o.closedAt = sql.NullTime{Time: at, Valid: true}
			}
		}
		if reason == "" && o.closedAt.Valid && !now.Before(o.closedAt.Time.Add(retentionEscrowFinancial)) {
			// Retiring the database before the external wallets and allocation
			// invariants are retired would resurrect orphans or reuse keys. This
			// build retains the minimum financial state under an explicit exception.
			reason = "bitcoin_wallet_retirement_required"
			if o.coin == "XMR" {
				reason = "monero_financial_retirement_required"
			}
		}
		if due && reason != "" {
			if err = retentionEscrowException(ctx, tx, o.id, reason, now); err != nil {
				return err
			}
		} else if _, err = tx.ExecContext(ctx, `UPDATE retention_escrow_exceptions SET resolved_at=$2,last_checked=$2
			WHERE order_id=$1 AND resolved_at IS NULL`, o.id, now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE orders SET financial_checked_at=$2 WHERE id=$1", o.id, now); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) retentionEscrowBlocker(ctx context.Context, tx *sql.Tx, o retentionEscrowOrder) (string, error) {
	var reason string
	err := tx.QueryRowContext(ctx, `SELECT CASE
		WHEN `+restoreGateSQL+` OR EXISTS(SELECT 1 FROM settings WHERE key IN ('escrow_sweep_required','xmr_escrow_sweep_required') AND value='true') THEN 'recovery_gate'
		WHEN es.recover_requested IS NOT NULL THEN 'pending_recovery'
		WHEN EXISTS(SELECT 1 FROM disputes d WHERE d.order_id=o.id AND d.status='Open') THEN 'dispute_open'
		WHEN EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id AND `+paymentFlagOpen("pm")+`) THEN 'payment_review'
		WHEN `+escrowStaffAlert("o.id")+` THEN 'escrow_alert'
		WHEN EXISTS(SELECT 1 FROM settlements s WHERE s.order_id=o.id AND
			(s.state IN ('building','awaiting_signature','party_signed','signed','broadcast') OR s.state='confirmed' AND s.reorg_safe_at IS NULL))
			OR EXISTS(SELECT 1 FROM settlement_bumps b JOIN settlements s ON s.id=b.settlement_id WHERE s.order_id=o.id AND b.state IN ('building','awaiting_signature','party_signed','signed','broadcast'))
			OR EXISTS(SELECT 1 FROM xmr_settlements s WHERE s.order_id=o.id AND
			(s.state IN ('requested','built','relayed') OR s.state='confirmed' AND s.reorg_safe_at IS NULL))
			OR EXISTS(SELECT 1 FROM xmr_settlement_rerequests r JOIN xmr_settlements s ON s.id=r.settlement_id WHERE s.order_id=o.id AND r.state='requested') THEN 'settlement_pending'
		WHEN es.coin='BTC' AND EXISTS(SELECT 1 FROM payments pm LEFT JOIN settlements s ON s.id=pm.settlement_id WHERE pm.order_id=o.id AND
			(pm.locked OR pm.spent_outside AND NULLIF(pm.spent_by,'') IS NULL
			 OR NOT pm.spent_outside AND pm.confirmations>=0 AND (s.id IS NULL OR
			 (s.state<>'confirmed' OR s.reorg_safe_at IS NULL) AND NOT(s.state='external' AND NULLIF(s.spent_by,'') IS NOT NULL)))) THEN 'deposit_unreconciled'
		WHEN EXISTS(SELECT 1 FROM escrow_watch_unrepaired u WHERE u.escrow_id=es.id) THEN 'watch_incomplete'
		ELSE '' END FROM orders o JOIN escrows es ON es.order_id=o.id WHERE o.id=$1`, o.id).Scan(&reason)
	return reason, err
}

func retentionEscrowReopened(reason string) bool {
	switch reason {
	case "dispute_open", "payment_review", "escrow_alert", "settlement_pending", "deposit_unreconciled":
		return true
	}
	return false
}

// A fresh, pinned and synced watch-wallet read is required: a stale ledger or a
// missing wallet must never be interpreted as an empty escrow. Read siblings too.
// There are deliberately no wallet mutation or signing calls in this path.
func (a *App) retentionBTCObservation(ctx context.Context, tx *sql.Tx, o retentionEscrowOrder, now time.Time) string {
	e := a.escrow.Load()
	if e == nil {
		return "wallet_unavailable"
	}
	check, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	node, err := e.blockchainInfo(check)
	if err != nil || escrowChainID(node.Chain, node.SignetChallenge) != e.chainID {
		return "wallet_unverified"
	}
	if node.Chain != "regtest" {
		info, err := e.networkInfo(check)
		if err != nil {
			return "wallet_unverified"
		}
		node.Connections = info.Connections
	}
	if node.notSynced(now, maxTipAge()) != "" || e.checkWatchWallet(check) != nil {
		return "wallet_unverified"
	}
	if scanning, err := e.watchScanning(check); err != nil || scanning {
		return "watch_incomplete"
	}
	// Stored reorg_safe_at is historical evidence. Retirement also requires the
	// transaction currently paying each outcome to remain 100 blocks deep; a
	// mempool spend alone makes gettxout null and is insufficient finality.
	finals, err := collectStrings(check, tx, `SELECT COALESCE((SELECT b.txid FROM settlement_bumps b WHERE b.settlement_id=s.id AND b.state='confirmed'),s.txid)
		FROM settlements s WHERE s.order_id=$1 AND s.state='confirmed' ORDER BY s.id`, o.id)
	if err != nil {
		return "settlement_pending"
	}
	for _, txid := range finals {
		w, err := e.walletTx(check, txid)
		if err != nil || !w.Found || w.Confirmations < escrowReorgDepth {
			return "settlement_pending"
		}
	}
	addresses := []string{o.address}
	rows, err := tx.QueryContext(check, "SELECT address FROM escrow_siblings WHERE escrow_id=(SELECT id FROM escrows WHERE order_id=$1) ORDER BY branch,idx", o.id)
	if err != nil {
		return "watch_incomplete"
	}
	for rows.Next() {
		var address string
		if err = rows.Scan(&address); err != nil {
			rows.Close()
			return "watch_incomplete"
		}
		addresses = append(addresses, address)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "watch_incomplete"
	}
	for _, address := range addresses {
		if known, err := e.watchKnows(check, address); err != nil || !known {
			return "watch_incomplete"
		}
	}
	deposits, err := e.incoming(check, addresses)
	if err != nil {
		return "wallet_unverified"
	}
	for _, d := range deposits {
		if d.Confirmations < 0 {
			continue
		}
		// The fresh read may find a deposit that the watcher has not recorded.
		// Even an input spent in the mempool is not final financial closure.
		var final bool
		var outside string
		err := tx.QueryRowContext(check, `SELECT COALESCE(s.state='confirmed' AND s.reorg_safe_at IS NOT NULL,false),
			COALESCE(CASE WHEN pm.spent_outside THEN NULLIF(pm.spent_by,'') WHEN s.state='external' THEN NULLIF(s.spent_by,'') END,'')
			FROM payments pm LEFT JOIN settlements s ON s.id=pm.settlement_id WHERE pm.order_id=$1 AND pm.currency='BTC' AND pm.txid=$2 AND pm.idx=$3`,
			o.id, d.TxID, d.Index).Scan(&final, &outside)
		if err != nil || (!final && outside == "") || d.Index < 0 || d.Index > 4294967295 {
			return "deposit_unreconciled"
		}
		if outside != "" {
			w, err := e.walletTx(check, outside)
			if err != nil || !w.Found || w.Confirmations < escrowReorgDepth {
				return "deposit_unreconciled"
			}
		}
		if unspent, _, _, err := e.txOut(check, d.TxID, uint32(d.Index)); err != nil || unspent {
			return "deposit_unreconciled"
		}
	}
	return ""
}

func retentionEscrowException(ctx context.Context, tx *sql.Tx, order, reason string, now time.Time) error {
	var changed bool
	err := tx.QueryRowContext(ctx, `WITH previous AS (SELECT reason,resolved_at FROM retention_escrow_exceptions WHERE order_id=$1),
		written AS (INSERT INTO retention_escrow_exceptions(order_id,reason,first_seen,last_checked,review_due)
		VALUES($1,$2,$3,$3,$4) ON CONFLICT(order_id) DO UPDATE SET reason=EXCLUDED.reason,last_checked=EXCLUDED.last_checked,
		first_seen=CASE WHEN retention_escrow_exceptions.resolved_at IS NOT NULL THEN EXCLUDED.first_seen ELSE retention_escrow_exceptions.first_seen END,
		review_due=CASE WHEN retention_escrow_exceptions.reason<>EXCLUDED.reason OR retention_escrow_exceptions.resolved_at IS NOT NULL
		THEN EXCLUDED.review_due ELSE retention_escrow_exceptions.review_due END,resolved_at=NULL RETURNING 1)
		SELECT NOT EXISTS(SELECT 1 FROM previous WHERE reason=$2 AND resolved_at IS NULL) FROM written`, order, reason, now, now.Add(retentionEscrowReview)).Scan(&changed)
	if err != nil || !changed {
		return err
	}
	return notifyAdmins(ctx, tx, "Order "+shortID(order)+": retention is overdue ("+reason+"). Necessary escrow records are preserved. Review the operational exception within 30 days; do not disable escrow guards or discard recoverable funds.")
}

// RetentionEscrowException is a scoped operational exception, not a legal hold.
// A review becoming overdue never grants permission to destroy financial state.
type RetentionEscrowException struct {
	OrderID, Reason                   string
	FirstSeen, LastChecked, ReviewDue time.Time
	ReviewOverdue                     bool
}

func LoadRetentionEscrowExceptions(ctx context.Context, db *sql.DB, now time.Time, limit int) ([]RetentionEscrowException, error) {
	if limit < 1 || limit > retentionEscrowBatch {
		limit = retentionEscrowBatch
	}
	rows, err := db.QueryContext(ctx, `SELECT order_id,reason,first_seen,last_checked,review_due FROM retention_escrow_exceptions
		WHERE resolved_at IS NULL ORDER BY review_due,order_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetentionEscrowException
	for rows.Next() {
		var v RetentionEscrowException
		if err = rows.Scan(&v.OrderID, &v.Reason, &v.FirstSeen, &v.LastChecked, &v.ReviewDue); err != nil {
			return nil, err
		}
		v.ReviewOverdue = !now.Before(v.ReviewDue)
		out = append(out, v)
	}
	return out, rows.Err()
}

// ReviewRetentionEscrowException is the host maintenance path's explicit review
// receipt. It renews only this order's operational review; it releases no legal
// hold and clears no wallet or deletion safety guard.
func ReviewRetentionEscrowException(ctx context.Context, db *sql.DB, order string, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = reviewRetentionEscrowExceptionTx(ctx, tx, order, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES(NULL,$1)", "Reviewed escrow retention operational exception for order "+shortID(order)); err != nil {
		return err
	}
	return tx.Commit()
}

func reviewRetentionEscrowExceptionTx(ctx context.Context, tx *sql.Tx, order string, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE retention_escrow_exceptions SET review_due=$2 WHERE order_id=$1 AND resolved_at IS NULL`, order, now.Add(retentionEscrowReview))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("no open retention exception for that order")
	}
	return nil
}
