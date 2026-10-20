package market

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type retentionEscrowKeyFloor struct {
	KeyHMAC   string `json:"key_hmac"`
	NextIndex int64  `json:"next_index"`
}

// This certificate is journaled outside database rollback before any erase. It
// contains the sparse technical non-reuse state a stale restore must reapply.
// Its order link expires with the normal restore manifest, not with the counters.
type retentionEscrowRetirement struct {
	OrderID         string                    `json:"order_id"`
	Coin            string                    `json:"coin"`
	ClosedAt        time.Time                 `json:"closed_at"`
	CertifiedAt     time.Time                 `json:"certified_at"`
	AddressHashes   []string                  `json:"address_hashes"`
	PubkeyHashes    []string                  `json:"pubkey_hashes,omitempty"`
	KeyFloors       []retentionEscrowKeyFloor `json:"key_floors,omitempty"`
	WalletID        int64                     `json:"wallet_id,omitempty"`
	WalletName      string                    `json:"wallet_name,omitempty"`
	WalletMonth     string                    `json:"wallet_month,omitempty"`
	WalletNextN     int                       `json:"wallet_next_n,omitempty"`
	WatchWalletName string                    `json:"watch_wallet_name,omitempty"`
	CleanupDue      time.Time                 `json:"cleanup_due"`
	MarketNextIndex int64                     `json:"market_next_index,omitempty"`
	FeeNextIndex    int64                     `json:"fee_next_index,omitempty"`
}

var retentionWalletName = regexp.MustCompile(`^escrow-([0-9]{6})-([0-9]{2})$`)

func retentionTechnicalHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func validateRetentionEscrowRetirement(r *retentionEscrowRetirement) error {
	if r == nil || r.OrderID == "" || len(r.OrderID) > 128 || (r.Coin != "BTC" && r.Coin != "XMR") || r.ClosedAt.IsZero() || r.CertifiedAt.Before(r.ClosedAt.Add(retentionEscrowFinancial)) || r.CleanupDue.IsZero() || r.CleanupDue.After(r.CertifiedAt) || len(r.AddressHashes) == 0 || len(r.AddressHashes) > 10000 {
		return errors.New("invalid escrow retirement certificate")
	}
	for _, hashes := range [][]string{r.AddressHashes, r.PubkeyHashes} {
		seen := map[string]bool{}
		for _, h := range hashes {
			b, err := hex.DecodeString(h)
			if err != nil || len(b) != 32 || h != strings.ToLower(h) || seen[h] {
				return errors.New("invalid retirement non-reuse hash")
			}
			seen[h] = true
		}
	}
	for _, f := range r.KeyFloors {
		b, err := hex.DecodeString(f.KeyHMAC)
		if err != nil || len(b) != 32 || f.NextIndex < 1 {
			return errors.New("invalid retirement key floor")
		}
	}
	if r.Coin == "BTC" {
		if len(r.PubkeyHashes) != 3 || r.WalletID != 0 || r.WalletName != "" || len(r.WatchWalletName) < 1 || len(r.WatchWalletName) > 255 || r.MarketNextIndex < 1 || r.FeeNextIndex < 0 || !r.CleanupDue.Equal(r.ClosedAt.Add(retentionEscrowFinancial)) {
			return errors.New("invalid Bitcoin retirement certificate")
		}
	} else {
		month, err := time.Parse("2006-01-02", r.WalletMonth)
		parts := retentionWalletName.FindStringSubmatch(r.WalletName)
		if r.WalletID < 1 || err != nil || month.Day() != 1 || len(parts) != 3 || month.Format("200601") != parts[1] || r.WalletNextN < 2 || r.WalletNextN > 100 || len(r.PubkeyHashes) != 0 || len(r.KeyFloors) != 0 || r.WatchWalletName != "" || r.MarketNextIndex != 0 || r.FeeNextIndex != 0 || r.CleanupDue.After(r.ClosedAt) {
			return errors.New("invalid Monero retirement certificate")
		}
	}
	return nil
}

// Preparation is read-only. The caller owns the manifest and both watcher locks
// and journals the returned certificate before applying it in the same tx.
func (a *App) prepareRetentionEscrowRetirement(ctx context.Context, tx *sql.Tx, order string, now time.Time) (*retentionEscrowRetirement, error) {
	var o retentionEscrowOrder
	var held bool
	err := tx.QueryRowContext(ctx, `SELECT o.id,es.coin,es.address,COALESCE(w.state,''),o.updated,o.financial_closed_at,w.destroyed_at,`+retentionOrderHeldSQL+`
		FROM orders o JOIN products p ON p.id=o.product_id JOIN escrows es ON es.order_id=o.id
		LEFT JOIN xmr_escrow_slots sl ON sl.id=es.xmr_slot_id LEFT JOIN xmr_escrow_wallets w ON w.id=sl.wallet_id WHERE o.id=$1 FOR UPDATE OF o,p`, order).
		Scan(&o.id, &o.coin, &o.address, &o.walletState, &o.terminalAt, &o.closedAt, &o.destroyedAt, &held)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if held || !o.closedAt.Valid || now.Before(o.closedAt.Time.Add(retentionEscrowFinancial)) {
		return nil, nil
	}
	reason, err := a.retentionEscrowBlocker(ctx, tx, o)
	if err != nil {
		return nil, err
	}
	if reason == "" {
		if o.coin == "BTC" {
			reason = a.retentionBTCObservation(ctx, tx, o, now)
		} else if o.walletState != "destroyed" || !o.destroyedAt.Valid {
			reason = "monero_wallet_not_destroyed"
		}
	}
	if reason != "" {
		return nil, retentionEscrowException(ctx, tx, order, reason, now)
	}
	r := &retentionEscrowRetirement{OrderID: order, Coin: o.coin, ClosedAt: o.closedAt.Time, CertifiedAt: now}
	addresses := []string{o.address}
	var feeAddress string
	if err = tx.QueryRowContext(ctx, `SELECT fee_destination FROM escrows WHERE order_id=$1`, order).Scan(&feeAddress); err != nil {
		return nil, err
	}
	if feeAddress != "" {
		addresses = append(addresses, feeAddress)
	}
	if o.coin == "BTC" {
		r.WatchWalletName = a.escrow.Load().watchName // observation proved this pinned adapter available
		r.CleanupDue = r.ClosedAt.Add(retentionEscrowFinancial)
		if err = tx.QueryRowContext(ctx, `SELECT GREATEST(es.market_index+1,m.last_value+CASE WHEN m.is_called THEN 1 ELSE 0 END,(SELECT allocation_next_index FROM retention_escrow_global_floors WHERE kind='market')),GREATEST(COALESCE(es.fee_index+1,0),f.last_value+CASE WHEN f.is_called THEN 1 ELSE 0 END,(SELECT allocation_next_index FROM retention_escrow_global_floors WHERE kind='fee')) FROM escrows es CROSS JOIN escrow_market_index m CROSS JOIN escrow_fee_index f WHERE es.order_id=$1`, order).Scan(&r.MarketNextIndex, &r.FeeNextIndex); err != nil {
			return nil, err
		}
		more, err := collectStrings(ctx, tx, `SELECT address FROM escrow_siblings WHERE escrow_id=(SELECT id FROM escrows WHERE order_id=$1) ORDER BY branch,idx`, order)
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, more...)
		keys, err := collectStrings(ctx, tx, `SELECT pubkey FROM escrow_pubkeys WHERE escrow_id=(SELECT id FROM escrows WHERE order_id=$1) ORDER BY party`, order)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			r.PubkeyHashes = append(r.PubkeyHashes, retentionTechnicalHash(k))
		}
		rows, err := tx.QueryContext(ctx, `SELECT pk.key_hmac,GREATEST(max(CASE WHEN pk.derivation_suffix LIKE '/0/%' THEN split_part(pk.derivation_suffix,'/',3) ELSE split_part(pk.derivation_suffix,'/',2) END::bigint)+1,COALESCE(max(f.next_index),1))
			FROM escrow_pubkeys pk LEFT JOIN escrow_index_floors f ON f.user_id=pk.user_id WHERE pk.escrow_id=(SELECT id FROM escrows WHERE order_id=$1) AND pk.key_hmac IS NOT NULL GROUP BY pk.key_hmac ORDER BY pk.key_hmac`, order)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var f retentionEscrowKeyFloor
			if err = rows.Scan(&f.KeyHMAC, &f.NextIndex); err != nil {
				rows.Close()
				return nil, err
			}
			r.KeyFloors = append(r.KeyFloors, f)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	} else {
		var primary string
		err = tx.QueryRowContext(ctx, `SELECT w.id,w.name,to_char(w.month,'YYYY-MM-DD'),GREATEST(substr(w.name,15,2)::int+1,COALESCE(n.next_n,1)),w.primary_address,COALESCE(w.destroy_after,w.destroyed_at)
			FROM escrows es JOIN xmr_escrow_slots sl ON sl.id=es.xmr_slot_id JOIN xmr_escrow_wallets w ON w.id=sl.wallet_id LEFT JOIN retention_xmr_wallet_names n ON n.month=w.month WHERE es.order_id=$1`, order).
			Scan(&r.WalletID, &r.WalletName, &r.WalletMonth, &r.WalletNextN, &primary, &r.CleanupDue)
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, primary)
		more, err := collectStrings(ctx, tx, `SELECT address FROM xmr_escrow_slots WHERE wallet_id=$1 AND order_id IS NULL ORDER BY id`, r.WalletID)
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, more...)
	}
	seen := map[string]bool{}
	for _, address := range addresses {
		h := retentionTechnicalHash(address)
		if !seen[h] {
			r.AddressHashes = append(r.AddressHashes, h)
			seen[h] = true
		}
	}
	if err = validateRetentionEscrowRetirement(r); err != nil {
		return nil, err
	}
	return r, nil
}

func (a *App) applyRetentionEscrowRetirement(ctx context.Context, tx *sql.Tx, r *retentionEscrowRetirement) error {
	if err := validateRetentionEscrowRetirement(r); err != nil {
		return err
	}
	walletName := r.WatchWalletName
	cleanupObserved := r.CertifiedAt
	if r.Coin == "XMR" {
		walletName = r.WalletName
		cleanupObserved = r.ClosedAt
	}
	if err := retentionEscrowExec(ctx, tx, `SELECT retention_wallet_cleanup_register($1,$2,$3,$4)`, r.Coin, walletName, r.CleanupDue, cleanupObserved); err != nil {
		return err
	}
	// Global counters reproduce the allocation highwater even when a snapshot
	// predates this certificate and every retired order row. Their row locks are
	// shared with the allocators; no racing setval can regress a live sequence.
	if r.Coin == "BTC" {
		for _, counter := range []struct {
			kind string
			next int64
		}{{"market", r.MarketNextIndex}, {"fee", r.FeeNextIndex}} {
			kind, next := counter.kind, counter.next
			if err := retentionEscrowExec(ctx, tx, `INSERT INTO retention_escrow_global_floors(kind,next_index,allocation_next_index) VALUES($1,$2,$2) ON CONFLICT(kind) DO UPDATE SET next_index=GREATEST(retention_escrow_global_floors.next_index,EXCLUDED.next_index),allocation_next_index=GREATEST(retention_escrow_global_floors.allocation_next_index,EXCLUDED.allocation_next_index)`, kind, next); err != nil {
				return err
			}
		}
	}
	for _, h := range r.AddressHashes {
		if err := retentionEscrowExec(ctx, tx, `INSERT INTO retention_escrow_retired_addresses(coin,address_hash,wallet_hash) VALUES($1,$2,$3) ON CONFLICT(coin,address_hash) DO UPDATE SET label_checked=false,wallet_hash=EXCLUDED.wallet_hash`, r.Coin, h, retentionTechnicalHash(walletName)); err != nil {
			return err
		}
	}
	for _, h := range r.PubkeyHashes {
		if err := retentionEscrowExec(ctx, tx, `INSERT INTO retention_escrow_retired_keys(pubkey_hash) VALUES($1) ON CONFLICT DO NOTHING`, h); err != nil {
			return err
		}
	}
	for _, f := range r.KeyFloors {
		if err := retentionEscrowExec(ctx, tx, `INSERT INTO retention_escrow_key_floors(key_hmac,next_index) VALUES($1,$2) ON CONFLICT(key_hmac) DO UPDATE SET next_index=GREATEST(retention_escrow_key_floors.next_index,EXCLUDED.next_index)`, f.KeyHMAC, f.NextIndex); err != nil {
			return err
		}
	}
	if r.Coin == "XMR" {
		if err := retentionEscrowExec(ctx, tx, `INSERT INTO retention_xmr_retired_wallets(name_hash) VALUES($1) ON CONFLICT DO NOTHING`, retentionTechnicalHash(r.WalletName)); err != nil {
			return err
		}
		if err := retentionEscrowExec(ctx, tx, `INSERT INTO retention_xmr_wallet_names(month,next_n) VALUES($1::date,$2) ON CONFLICT(month) DO UPDATE SET next_n=GREATEST(retention_xmr_wallet_names.next_n,EXCLUDED.next_n)`, r.WalletMonth, r.WalletNextN); err != nil {
			return err
		}
	}
	// On live expiry the adapter is available and the labels are removed before
	// the personal database rows. Startup replay precedes adapter initialization;
	// startEscrow/reconcileOrphans scrub matching retired hashes before use then.
	if r.Coin == "BTC" {
		if e := a.escrow.Load(); e != nil {
			if err := a.scrubRetiredEscrowLabels(ctx, tx, e); err != nil {
				return err
			}
		}
	}
	if err := retentionEscrowExec(ctx, tx, `INSERT INTO retention_escrow_permits(order_id,transaction_id,wallet_id,closed_at,certified_at) VALUES($1,txid_current(),NULLIF($2::bigint,0),$3,$4)
		ON CONFLICT(order_id) DO UPDATE SET transaction_id=EXCLUDED.transaction_id,wallet_id=EXCLUDED.wallet_id,closed_at=EXCLUDED.closed_at,certified_at=EXCLUDED.certified_at`, r.OrderID, r.WalletID, r.ClosedAt, r.CertifiedAt); err != nil {
		return err
	}
	// Expire external destination history for this order, except a destination
	// another retained order still needs. Future use after the retention window
	// is permitted; market-owned retired addresses remain globally forbidden.
	if err := retentionEscrowExec(ctx, tx, `DELETE FROM escrow_external_addresses x WHERE
		EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id JOIN escrows es ON es.order_id=o.id WHERE o.id=$1 AND x.coin=es.coin AND
		 ((x.user_id=o.buyer_id AND x.address=es.refund_address) OR x.user_id=p.vendor_id AND
		 (EXISTS(SELECT 1 FROM settlements s WHERE s.order_id=o.id AND s.kind='release' AND s.destination=x.address) OR EXISTS(SELECT 1 FROM xmr_settlements s WHERE s.order_id=o.id AND s.kind='release' AND s.destination=x.address))))
		AND NOT EXISTS(SELECT 1 FROM escrows es JOIN orders o ON o.id=es.order_id WHERE o.id<>$1 AND o.buyer_id=x.user_id AND es.coin=x.coin AND es.refund_address=x.address)
		AND NOT EXISTS(SELECT 1 FROM settlements s JOIN orders o ON o.id=s.order_id JOIN products p ON p.id=o.product_id WHERE o.id<>$1 AND p.vendor_id=x.user_id AND s.kind='release' AND s.destination=x.address AND x.coin='BTC')
		AND NOT EXISTS(SELECT 1 FROM xmr_settlements s JOIN orders o ON o.id=s.order_id JOIN products p ON p.id=o.product_id WHERE o.id<>$1 AND p.vendor_id=x.user_id AND s.kind='release' AND s.destination=x.address AND x.coin='XMR')`, r.OrderID); err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM xmr_escrow_collisions WHERE order_id=$1`,
		`DELETE FROM xmr_settlement_rerequests WHERE settlement_id IN(SELECT id FROM xmr_settlements WHERE order_id=$1)`,
		`DELETE FROM xmr_settlement_hand_settled WHERE settlement_id IN(SELECT id FROM xmr_settlements WHERE order_id=$1)`,
		`DELETE FROM payments WHERE order_id=$1`,
		`DELETE FROM settlement_bump_signatures WHERE bump_id IN(SELECT b.id FROM settlement_bumps b JOIN settlements s ON s.id=b.settlement_id WHERE s.order_id=$1)`,
		`DELETE FROM settlement_bumps WHERE settlement_id IN(SELECT id FROM settlements WHERE order_id=$1)`,
		`DELETE FROM settlement_signatures WHERE settlement_id IN(SELECT id FROM settlements WHERE order_id=$1)`,
		`DELETE FROM settlement_outputs WHERE settlement_id IN(SELECT id FROM settlements WHERE order_id=$1)`,
		`DELETE FROM xmr_settlement_outputs WHERE settlement_id IN(SELECT id FROM xmr_settlements WHERE order_id=$1)`,
		`DELETE FROM settlements WHERE order_id=$1`,
		`DELETE FROM xmr_settlements WHERE order_id=$1`,
		`DELETE FROM escrow_orphans WHERE order_id=$1`,
		`DELETE FROM escrow_pubkeys WHERE escrow_id IN(SELECT id FROM escrows WHERE order_id=$1)`,
		`DELETE FROM escrow_siblings WHERE escrow_id IN(SELECT id FROM escrows WHERE order_id=$1)`,
		`DELETE FROM escrows WHERE order_id=$1`,
		`DELETE FROM xmr_escrow_slots WHERE order_id=$1`,
		`DELETE FROM deliveries WHERE order_id=$1`,
		`DELETE FROM images WHERE order_id=$1`,
		`DELETE FROM reviews WHERE order_id=$1`,
		`DELETE FROM disputes WHERE order_id=$1`,
		`UPDATE notifications SET order_id=NULL WHERE order_id=$1`,
		`DELETE FROM order_events WHERE order_id=$1`,
	} {
		if err := retentionEscrowExec(ctx, tx, q, r.OrderID); err != nil {
			return fmt.Errorf("certified escrow retirement: %w", err)
		}
	}
	if err := retentionEscrowExec(ctx, tx, `DELETE FROM escrow_reserved_keys WHERE encode(sha256(convert_to(pubkey,'UTF8')),'hex')=ANY($1)`, r.PubkeyHashes); err != nil {
		return err
	}
	if r.Coin == "XMR" {
		// Shared wallets are removed only once every claimed order has retired.
		if err := retentionEscrowExec(ctx, tx, `DELETE FROM xmr_escrow_slots WHERE wallet_id=$1 AND order_id IS NULL AND retention_wallet_delete_allowed($1)`, r.WalletID); err != nil {
			return err
		}
		if err := retentionEscrowExec(ctx, tx, `DELETE FROM xmr_escrow_wallets WHERE id=$1 AND retention_wallet_delete_allowed($1)`, r.WalletID); err != nil {
			return err
		}
	}
	if err := retentionEscrowExec(ctx, tx, `DELETE FROM orders WHERE id=$1`, r.OrderID); err != nil {
		return err
	}
	if err := retentionEscrowExec(ctx, tx, `DELETE FROM retention_escrow_permits WHERE order_id=$1`, r.OrderID); err != nil {
		return err
	}
	return a.retentionUnusedAccountKeys(ctx, tx)
}

func (a *App) retentionUnusedAccountKeys(ctx context.Context, tx *sql.Tx) error {
	// Closed accounts no longer need key profiles once their last escrow has
	// retired. Preserve the root's allocation high water without a user mapping.
	_, err := tx.ExecContext(ctx, `INSERT INTO retention_escrow_key_floors(key_hmac,next_index)
	 SELECT k.xpub_hmac,GREATEST(COALESCE(f.next_index,1),1) FROM user_escrow_keys k JOIN users u ON u.id=k.user_id LEFT JOIN escrow_index_floors f ON f.user_id=u.id
	 WHERE u.closed_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM escrow_pubkeys p WHERE p.user_id=u.id)
	 ON CONFLICT(key_hmac) DO UPDATE SET next_index=GREATEST(retention_escrow_key_floors.next_index,EXCLUDED.next_index)`)
	if err != nil {
		return err
	}
	for _, table := range []string{"user_escrow_keys", "escrow_index_floors"} {
		if _, err = tx.ExecContext(ctx, `DELETE FROM `+table+` k USING users u WHERE k.user_id=u.id AND u.closed_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM escrow_pubkeys p WHERE p.user_id=u.id)`); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM escrow_external_addresses x USING users u WHERE x.user_id=u.id AND u.closed_at IS NOT NULL
	 AND NOT EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id JOIN escrows es ON es.order_id=o.id WHERE o.buyer_id=u.id OR p.vendor_id=u.id)`)
	return err
}

// A party may still need to claim or sign on a terminal order. Only verified
// financial closure ends that access obligation; mere historical FKs do not.
func retentionAccountObligations(ctx context.Context, tx *sql.Tx, userID string, now time.Time) (bool, error) {
	var needed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id JOIN escrows es ON es.order_id=o.id
	 WHERE (o.buyer_id=$1 OR p.vendor_id=$1) AND (o.financial_closed_at IS NULL OR o.state NOT IN('completed','cancelled','resolved')
	 OR EXISTS(SELECT 1 FROM retention_escrow_exceptions x WHERE x.order_id=o.id AND x.resolved_at IS NULL AND x.reason IN('wallet_unavailable','wallet_unverified','watch_incomplete','recovery_gate','pending_recovery'))))`, userID).Scan(&needed)
	return needed, err
}

func prepareRetentionAccountKeyFloors(ctx context.Context, tx *sql.Tx, userID string) ([]retentionEscrowKeyFloor, error) {
	rows, err := tx.QueryContext(ctx, `SELECT k.xpub_hmac,GREATEST(COALESCE(f.next_index,1),1) FROM user_escrow_keys k LEFT JOIN escrow_index_floors f ON f.user_id=k.user_id
	 WHERE k.user_id=$1 AND NOT EXISTS(SELECT 1 FROM escrow_pubkeys p WHERE p.user_id=k.user_id)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []retentionEscrowKeyFloor
	for rows.Next() {
		var f retentionEscrowKeyFloor
		if err = rows.Scan(&f.KeyHMAC, &f.NextIndex); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

type retentionEscrowQuerier interface {
	rowQuerier
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func retiredEscrowAddressHashes(ctx context.Context, q retentionEscrowQuerier, addresses []escrowWatched) (map[string]bool, error) {
	hashes := make([]string, 0, len(addresses))
	for _, address := range addresses {
		hashes = append(hashes, retentionTechnicalHash(address.Address))
	}
	rows, err := q.QueryContext(ctx, `SELECT address_hash FROM retention_escrow_retired_addresses WHERE coin='BTC' AND address_hash=ANY($1)`, hashes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	retired := map[string]bool{}
	for rows.Next() {
		var h string
		if err = rows.Scan(&h); err != nil {
			return nil, err
		}
		retired[h] = true
	}
	return retired, rows.Err()
}

func (a *App) scrubRetiredEscrowLabels(ctx context.Context, q retentionEscrowQuerier, e *escrowBTC) error {
	var any bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM retention_escrow_retired_addresses WHERE coin='BTC')`).Scan(&any); err != nil {
		return err
	}
	if !any {
		return nil
	}
	addresses, err := e.watchedAddresses(ctx)
	if err != nil {
		return err
	}
	retired, err := retiredEscrowAddressHashes(ctx, q, addresses)
	if err != nil {
		return err
	}
	for _, address := range addresses {
		if retired[retentionTechnicalHash(address.Address)] && address.Label != "retired" {
			if err = e.retireAddressLabel(ctx, address.Address); err != nil {
				return fmt.Errorf("retired wallet label cleanup failed: %w", err)
			}
		}
	}
	_, err = q.ExecContext(ctx, `UPDATE retention_escrow_retired_addresses SET label_checked=true WHERE coin='BTC' AND NOT label_checked`)
	return err
}

// SQL is drawn only from fixed statements at the unexported callers.
func retentionEscrowExec(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}
