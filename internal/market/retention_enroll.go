package market

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// EnrollRetention establishes the current original database's durable retention
// identity before an encoding migration. It starts no application, reads no
// private content, and performs no replay, expiry, notice delivery or wallet
// operation. Unlike serving startup, this narrowly scoped host operation may
// migrate a non-UTF8 original database. Restored databases must use normal
// current-state replay instead; this command never clears their restore gate.
func EnrollRetention(ctx context.Context, db *sql.DB, getenv func(string) string) error {
	cfg, err := retentionConfigFromEnv(getenv)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return errors.New("RETENTION_ENABLED=on is required for original-database enrollment")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Share startup/account maintenance's advisory lock. The external lock below
	// is nonblocking, so an active retention pass cannot create a lock cycle.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(782493)`); err != nil {
		return err
	}
	var hasSettings bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass('settings') IS NOT NULL`).Scan(&hasSettings); err != nil {
		return err
	}
	var pin string
	if hasSettings {
		var restore string
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM settings WHERE key='retention_instance_id'),''),COALESCE((SELECT value FROM settings WHERE key='retention_restore_required'),'false')`).Scan(&pin, &restore); err != nil {
			return err
		}
		if restore != "false" {
			return errors.New("original-database enrollment refuses a pending or invalid restore gate; supply current external state and run isolated application replay")
		}
	}
	if err = os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(cfg.StateDir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("RETENTION_STATE_DIR must be a private directory (0700)")
	}
	s := &retentionService{dir: cfg.StateDir}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	fresh := false
	err = s.read()
	if errors.Is(err, os.ErrNotExist) && pin == "" {
		now := time.Now().UTC()
		s.state = retentionState{Version: 1, InstanceID: randomToken(), EnabledAt: now, EnrollmentDeadline: now.Add(30 * 24 * time.Hour)}
		fresh = true
		err = s.initializeBackupRegister()
	}
	if err != nil {
		return errors.New("current original enrollment state is missing or invalid; preserve existing state and reconcile it before retrying")
	}
	if pin != "" && pin != s.state.InstanceID {
		return errors.New("retention state belongs to a different database instance")
	}
	// Requests, holds and pending operations need normal replay. A failed first
	// enrollment may have published only factual wallet destruction certificates;
	// those are checked against this same original database below before adoption.
	if len(s.state.Pending)+len(s.state.Holds)+len(s.state.Requests) != 0 {
		return errors.New("existing retention history requires normal current-state replay; original enrollment cannot replace or skip it")
	}
	if _, err = s.backupRegister(); err != nil {
		return err
	}
	if err = migrate(ctx, tx); err != nil {
		return err
	}
	a := &App{db: db, retention: s}
	if err = a.checkRetentionSequence(ctx, tx); err != nil {
		return err
	}
	// Query through the migration transaction: pre-084/095 originals may not yet
	// have the wallet table or destruction columns on another connection. These
	// are already committed destruction facts, never a new seed-erasure action.
	facts, err := snapshotRetentionXMRDestructions(ctx, tx, time.Now().UTC())
	if err != nil {
		return err
	}
	expected := make(map[string]retentionTombstone, len(facts))
	for _, fact := range facts {
		expected[fact.ID] = fact
	}
	for _, saved := range s.state.Tombstones {
		fact, exists := expected[saved.ID]
		if !exists || !sameRetentionEnrollmentFact(saved, fact) {
			return errors.New("existing retention history requires normal current-state replay; original enrollment cannot replace or skip it")
		}
		delete(expected, saved.ID)
	}
	for _, fact := range facts {
		if _, missing := expected[fact.ID]; missing {
			s.state.Tombstones = append(s.state.Tombstones, fact)
		}
	}
	// Publish all known destruction facts before the first ledger becomes usable.
	// An ambiguous DB commit leaves the identical facts available for safe retry.
	if fresh || len(expected) > 0 {
		if err = s.write(); err != nil {
			return err
		}
	}
	if err = a.bindRetentionInstance(ctx, tx); err != nil {
		return err
	}
	if err = a.recordRetentionSequence(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func sameRetentionEnrollmentFact(saved, current retentionTombstone) bool {
	if saved.Kind != "xmr_wallet_destroy" || current.Kind != saved.Kind || saved.CommittedAt == nil {
		return false
	}
	// The original destruction certificate must match exactly; only the later
	// observed commit boundary differs when an interrupted enrollment is retried.
	saved.CommittedAt, current.CommittedAt = nil, nil
	saved.At, current.At = saved.At.UTC(), current.At.UTC()
	a, errA := json.Marshal(saved)
	b, errB := json.Marshal(current)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

// RetentionEnrollmentPath validates host-command configuration before connecting
// to PostgreSQL. The actual operation rechecks it alongside the durable state.
func RetentionEnrollmentPath(getenv func(string) string) (string, error) {
	cfg, err := retentionConfigFromEnv(getenv)
	if err != nil {
		return "", err
	}
	if !cfg.Enabled {
		return "", errors.New("RETENTION_ENABLED=on is required for original-database enrollment")
	}
	return filepath.Clean(cfg.StateDir), nil
}
