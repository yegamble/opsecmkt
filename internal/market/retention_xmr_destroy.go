package market

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"
)

// A seed erasure has an earlier restore boundary than financial retirement.
// This exact-wallet decision contains neither seed nor order/participant data.
type retentionXMRDestruction struct {
	WalletName        string     `json:"wallet_name"`
	WalletMonth       string     `json:"wallet_month"`
	WalletNextN       int        `json:"wallet_next_n"`
	WalletAddressHash string     `json:"wallet_address_hash,omitempty"`
	LastClosedAt      time.Time  `json:"last_closed_at"`
	DestroyAfter      time.Time  `json:"destroy_after"`
	DestroyedAt       time.Time  `json:"destroyed_at"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
}

func validateRetentionXMRDestruction(d *retentionXMRDestruction) error {
	if d == nil {
		return errors.New("missing Monero destruction certificate")
	}
	month, err := time.Parse("2006-01-02", d.WalletMonth)
	parts := retentionWalletName.FindStringSubmatch(d.WalletName)
	if err != nil || month.Day() != 1 || len(parts) != 3 || month.Format("200601") != parts[1] || d.WalletNextN < 2 || d.WalletNextN > 100 || d.LastClosedAt.IsZero() || d.DestroyAfter.Before(d.LastClosedAt.Add(retentionEscrowMonitoring)) || d.DestroyedAt.Before(d.DestroyAfter) {
		return errors.New("invalid Monero destruction certificate")
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || n < 1 || n > 99 || d.WalletNextN <= n {
		return errors.New("invalid Monero wallet allocation floor")
	}
	if d.WalletAddressHash != "" {
		h, err := hex.DecodeString(d.WalletAddressHash)
		if err != nil || len(h) != 32 || d.WalletAddressHash != strings.ToLower(d.WalletAddressHash) {
			return errors.New("invalid Monero wallet fingerprint")
		}
	}
	if d.StartedAt != nil && (d.StartedAt.Before(d.DestroyAfter) || d.StartedAt.After(d.DestroyedAt)) {
		return errors.New("invalid Monero destruction start")
	}
	return nil
}

type retentionXMRDestructionQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func snapshotRetentionXMRDestructions(ctx context.Context, q retentionXMRDestructionQuerier, observed time.Time) ([]retentionTombstone, error) {
	rows, err := q.QueryContext(ctx, `SELECT w.name,to_char(w.month,'YYYY-MM-DD'),GREATEST(substr(w.name,15,2)::int+1,COALESCE(n.next_n,1)),w.last_closed_at,w.destroy_after,w.destroyed_at,w.destroy_started_at,COALESCE(w.primary_address,'') FROM xmr_escrow_wallets w LEFT JOIN retention_xmr_wallet_names n ON n.month=w.month WHERE w.state='destroyed' AND w.seed_sealed IS NULL ORDER BY w.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []retentionTombstone
	for rows.Next() {
		var d retentionXMRDestruction
		var started sql.NullTime
		var address string
		if err = rows.Scan(&d.WalletName, &d.WalletMonth, &d.WalletNextN, &d.LastClosedAt, &d.DestroyAfter, &d.DestroyedAt, &started, &address); err != nil {
			return nil, err
		}
		if address != "" {
			d.WalletAddressHash = retentionTechnicalHash(address)
		}
		if started.Valid {
			at := started.Time
			d.StartedAt = &at
		}
		if err = validateRetentionXMRDestruction(&d); err != nil {
			return nil, err
		}
		committed := observed
		if committed.Before(d.DestroyedAt) {
			committed = d.DestroyedAt
		}
		result = append(result, retentionTombstone{Kind: "xmr_wallet_destroy", ID: d.WalletName, At: d.DestroyedAt, CommittedAt: &committed, XMRDestruction: &d})
	}
	return result, rows.Err()
}

// Caller owns the exact wallet lock and has checked live funds or explicit
// operator destruction evidence. The intent is durable before any password RPC;
// its database marker commits only in the same transaction as seed erasure.
func (a *App) prepareRetentionXMRDestruction(ctx context.Context, tx *sql.Tx, id int64, now time.Time) (string, error) {
	if a.retention == nil {
		return "", nil
	}
	var d retentionXMRDestruction
	var started sql.NullTime
	var allowed bool
	var address string
	err := tx.QueryRowContext(ctx, `SELECT w.name,to_char(w.month,'YYYY-MM-DD'),GREATEST(substr(w.name,15,2)::int+1,COALESCE(n.next_n,1)),w.last_closed_at,w.destroy_after,w.destroy_started_at,COALESCE(`+xmrDestroyableSQL+`,false),COALESCE(w.primary_address,'') FROM xmr_escrow_wallets w LEFT JOIN retention_xmr_wallet_names n ON n.month=w.month WHERE w.id=$1 FOR UPDATE OF w`, id, now).Scan(&d.WalletName, &d.WalletMonth, &d.WalletNextN, &d.LastClosedAt, &d.DestroyAfter, &started, &allowed, &address)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", errors.New("Monero destruction no longer allowed")
	}
	d.DestroyedAt = now
	if address != "" {
		d.WalletAddressHash = retentionTechnicalHash(address)
	}
	if started.Valid {
		at := started.Time
		d.StartedAt = &at
	}
	if err = validateRetentionXMRDestruction(&d); err != nil {
		return "", err
	}
	s := a.retention
	unlock, err := s.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if err = s.read(); err != nil {
		return "", err
	}
	if err = a.checkRetentionSequence(ctx, tx); err != nil {
		return "", err
	}
	if len(s.state.Pending) > 0 {
		return "", fail(409, "A retention operation awaits reconciliation. Wallet recovery state was preserved.")
	}
	op := retentionOperation{ID: randomToken(), At: now, Tombstones: []retentionTombstone{{Kind: "xmr_wallet_destroy", ID: d.WalletName, At: now, XMRDestruction: &d}}}
	if err = a.prepareRetentionOperation(ctx, tx, op); err != nil {
		return "", err
	}
	return op.ID, nil
}

func (a *App) finishRetentionXMRDestruction(ctx context.Context, id string) {
	if id == "" {
		return
	}
	check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.finalizeRetentionOperation(check, id); err != nil {
		log.Print("Monero destruction committed; external retention finalization awaits reconciliation")
	}
}

func (a *App) applyRetentionXMRDestruction(ctx context.Context, tx *sql.Tx, d *retentionXMRDestruction) error {
	if err := validateRetentionXMRDestruction(d); err != nil {
		return err
	}
	var address, seed sql.NullString
	identityErr := tx.QueryRowContext(ctx, `SELECT primary_address,seed_sealed FROM xmr_escrow_wallets WHERE name=$1 FOR UPDATE`, d.WalletName).Scan(&address, &seed)
	if identityErr != nil && !errors.Is(identityErr, sql.ErrNoRows) {
		return identityErr
	}
	if identityErr == nil && (address.Valid && d.WalletAddressHash != "" && retentionTechnicalHash(address.String) != d.WalletAddressHash || seed.Valid && (d.WalletAddressHash == "" || !address.Valid)) {
		return errors.New("Monero destruction certificate does not match restored wallet identity")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO retention_xmr_retired_wallets(name_hash) VALUES($1) ON CONFLICT DO NOTHING`, retentionTechnicalHash(d.WalletName)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO retention_xmr_wallet_names(month,next_n) VALUES($1::date,$2) ON CONFLICT(month) DO UPDATE SET next_n=GREATEST(retention_xmr_wallet_names.next_n,EXCLUDED.next_n)`, d.WalletMonth, d.WalletNextN); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT retention_wallet_cleanup_register('XMR',$1,$2,$3)`, d.WalletName, d.DestroyAfter, d.DestroyedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO retention_xmr_destroy_permits(wallet_name,transaction_id,last_closed_at,destroy_after,destroyed_at,started_at) VALUES($1,txid_current(),$2,$3,$4,$5) ON CONFLICT(wallet_name) DO UPDATE SET transaction_id=EXCLUDED.transaction_id,last_closed_at=EXCLUDED.last_closed_at,destroy_after=EXCLUDED.destroy_after,destroyed_at=EXCLUDED.destroyed_at,started_at=EXCLUDED.started_at`, d.WalletName, d.LastClosedAt, d.DestroyAfter, d.DestroyedAt, d.StartedAt); err != nil {
		return err
	}
	// A committed original decision is authoritative over a stale backup's state.
	// Never delete payment, settlement or order rows at this earlier boundary.
	if _, err := tx.ExecContext(ctx, `UPDATE xmr_escrow_wallets SET state='destroyed',seed_sealed=NULL,last_closed_at=$2,destroy_after=$3,destroyed_at=$4,destroy_started_at=$5,updated=now() WHERE name=$1 AND (state<>'destroyed' OR seed_sealed IS NOT NULL)`, d.WalletName, d.LastClosedAt, d.DestroyAfter, d.DestroyedAt, d.StartedAt); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM retention_xmr_destroy_permits WHERE wallet_name=$1`, d.WalletName)
	return err
}
