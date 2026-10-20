package market

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

type retentionConfig struct {
	Enabled  bool
	StateDir string
}

type retentionHold struct {
	ID               string     `json:"id"`
	Scope            string     `json:"scope"`
	RecordID         string     `json:"record_id"`
	Authority        string     `json:"authority"`
	Reason           string     `json:"reason"`
	Owner            string     `json:"owner"`
	Readers          string     `json:"readers"`
	ReleaseCondition string     `json:"release_condition"`
	BeganAt          time.Time  `json:"began_at"`
	ReviewAt         time.Time  `json:"review_at"`
	ReleasedAt       *time.Time `json:"released_at"`
}

type retentionTombstone struct {
	Kind           string                     `json:"kind"`
	ID             string                     `json:"id"`
	At             time.Time                  `json:"at"`
	CommittedAt    *time.Time                 `json:"committed_at,omitempty"`
	Retirement     *retentionEscrowRetirement `json:"retirement,omitempty"`
	KeyFloors      []retentionEscrowKeyFloor  `json:"key_floors,omitempty"`
	XMRDestruction *retentionXMRDestruction   `json:"xmr_destruction,omitempty"`
}

type retentionRequest struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

// A pending operation is deliberately distinct from committed tombstones. The
// transaction marker proves a commit after a crash; an absent marker could also
// mean an old restored database, so startup must stop instead of guessing.
type retentionOperation struct {
	ID          string               `json:"id"`
	Tombstones  []retentionTombstone `json:"tombstones,omitempty"`
	Request     *retentionRequest    `json:"request,omitempty"`
	Hold        *retentionHold       `json:"hold,omitempty"`
	ReleaseHold string               `json:"release_hold,omitempty"`
	At          time.Time            `json:"at"`
}

type retentionState struct {
	Version            int                  `json:"version"`
	InstanceID         string               `json:"instance_id"`
	Sequence           uint64               `json:"sequence"`
	EnabledAt          time.Time            `json:"enabled_at"`
	EnrollmentDeadline time.Time            `json:"enrollment_deadline"`
	Tombstones         []retentionTombstone `json:"tombstones"`
	Holds              []retentionHold      `json:"holds"`
	Requests           []retentionRequest   `json:"requests"`
	Pending            []retentionOperation `json:"pending"`
}

type retentionEnvelope struct {
	Checksum string          `json:"checksum"`
	State    json.RawMessage `json:"state"`
}

type retentionService struct {
	dir     string
	mu      sync.Mutex
	state   retentionState // guarded by the mutex AND the interprocess file lock
	failure atomic.Pointer[string]
}

var errRetentionBusy = fail(409, "Retention maintenance is busy. Retry this request shortly.")
var errRetentionStateUnavailable = errors.New("retention external state is unavailable")

const retentionStateLimit = 64 << 20

func (s *retentionService) lock() (func(), error) {
	// A confirmed form already owns a users row. Never wait for a sweeper which
	// owns the manifest lock and may be waiting for that same row.
	if !s.mu.TryLock() {
		return nil, errRetentionBusy
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "retention.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		s.mu.Unlock()
		return nil, s.unavailable(err)
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		s.mu.Unlock()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errRetentionBusy
		}
		return nil, s.unavailable(err)
	}
	return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close(); s.mu.Unlock() }, nil
}

func (s *retentionService) unavailable(err error) error {
	class := "state_unavailable"
	s.failure.Store(&class)
	return errors.Join(errRetentionStateUnavailable, err)
}

func (s *retentionService) read() (returnErr error) {
	defer func() {
		if returnErr != nil {
			class := "state_unavailable"
			s.failure.Store(&class)
			returnErr = errors.Join(errRetentionStateUnavailable, returnErr)
		}
	}()
	p := filepath.Join(s.dir, "retention-state.json")
	b, err := readPrivateRetentionFile(p, retentionStateLimit)
	if err != nil {
		return err
	}
	var envelope retentionEnvelope
	if err = strictRetentionJSON(b, &envelope); err != nil {
		return fmt.Errorf("invalid retention state: %w", err)
	}
	if err = requireRetentionJSONFields(b, "checksum", "state"); err != nil {
		return err
	}
	sum := sha256.Sum256(envelope.State)
	if envelope.Checksum != hex.EncodeToString(sum[:]) {
		return errors.New("retention state checksum mismatch")
	}
	var state retentionState
	if err = strictRetentionJSON(envelope.State, &state); err != nil {
		return err
	}
	if err = requireRetentionJSONFields(envelope.State, "version", "instance_id", "sequence", "enabled_at", "enrollment_deadline", "tombstones", "holds", "requests", "pending"); err != nil {
		return err
	}
	if err = validateRetentionState(state); err != nil {
		return err
	}
	s.state = state
	return nil
}

func requireRetentionJSONFields(b []byte, fields ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(b, &object); err != nil {
		return err
	}
	if len(object) != len(fields) {
		return errors.New("retention JSON has missing required fields")
	}
	for _, field := range fields {
		if _, ok := object[field]; !ok {
			return errors.New("retention JSON has missing required fields")
		}
	}
	return nil
}

func readPrivateRetentionFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("retention state must be a private regular file (0600)")
	}
	if info.Size() > limit {
		return nil, errors.New("retention JSON exceeds its safe read limit")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, before) || info.Size() != before.Size() || !info.ModTime().Equal(before.ModTime()) {
		return nil, errors.New("retention JSON changed during validation")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("retention JSON exceeds its safe read limit")
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) || !os.SameFile(after, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("retention JSON changed during validation")
	}
	return b, nil
}

func strictRetentionJSON(b []byte, v any) error {
	if err := uniqueRetentionJSONKeys(b); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("retention JSON has trailing data")
	}
	return nil
}

func uniqueRetentionJSONKeys(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("retention JSON nesting exceeds its safe limit")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					if !ok || seen[name] {
						return errors.New("retention JSON contains a duplicate object key")
					}
					seen[name] = true
					if err = value(depth + 1); err != nil {
						return err
					}
				}
				end, err := d.Token()
				if err != nil {
					return err
				}
				if end != json.Delim('}') {
					return errors.New("invalid retention JSON object")
				}
			case '[':
				for d.More() {
					if err = value(depth + 1); err != nil {
						return err
					}
				}
				end, err := d.Token()
				if err != nil {
					return err
				}
				if end != json.Delim(']') {
					return errors.New("invalid retention JSON array")
				}
			default:
				return errors.New("invalid retention JSON delimiter")
			}
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("retention JSON has trailing data")
	}
	return nil
}

func validateRetentionState(s retentionState) error {
	instance, err := hex.DecodeString(s.InstanceID)
	if s.Version != 1 || err != nil || len(instance) != 32 || s.EnabledAt.IsZero() || !s.EnrollmentDeadline.Equal(s.EnabledAt.Add(30*24*time.Hour)) {
		return errors.New("retention state has an invalid version, instance or enrollment boundary")
	}
	seen := map[string]bool{}
	for _, h := range s.Holds {
		if err := validateRetentionHold(h); err != nil {
			return err
		}
		if seen[h.ID] {
			return errors.New("duplicate retention hold")
		}
		seen[h.ID] = true
	}
	for _, t := range s.Tombstones {
		if err := validateRetentionTombstone(t); err != nil {
			return err
		}
		if t.CommittedAt == nil {
			return errors.New("committed retention deletion has no observed commit boundary")
		}
	}
	for _, r := range s.Requests {
		if r.ID == "" || r.At.IsZero() {
			return errors.New("invalid retention request")
		}
	}
	seen = map[string]bool{}
	for _, op := range s.Pending {
		if op.ID == "" || seen[op.ID] || op.At.IsZero() {
			return errors.New("invalid pending retention operation")
		}
		seen[op.ID] = true
		for _, t := range op.Tombstones {
			if err := validateRetentionTombstone(t); err != nil {
				return err
			}
		}
		if op.Request != nil && (op.Request.ID == "" || op.Request.At.IsZero()) {
			return errors.New("invalid pending deletion request")
		}
		if op.Hold != nil {
			if err := validateRetentionHold(*op.Hold); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateRetentionTombstone(t retentionTombstone) error {
	switch t.Kind {
	case "message", "ticket_message", "ticket", "notification", "audit", "draft", "order", "order_private", "listing_private", "listing", "review", "image", "account_close", "account_restrict", "account", "escrow_order", "xmr_wallet_destroy":
	default:
		return errors.New("unknown retention tombstone scope")
	}
	if t.ID == "" || t.At.IsZero() {
		return errors.New("invalid retention tombstone")
	}
	if t.CommittedAt != nil && t.CommittedAt.Before(t.At) {
		return errors.New("retention commit boundary precedes its content cutoff")
	}
	if t.Kind == "escrow_order" {
		if err := validateRetentionEscrowRetirement(t.Retirement); err != nil {
			return err
		}
		if t.Retirement.OrderID != t.ID || !t.Retirement.CertifiedAt.Equal(t.At) {
			return errors.New("escrow retirement certificate does not match its manifest scope")
		}
	} else if t.Retirement != nil {
		return errors.New("unexpected retirement certificate scope")
	}
	if t.Kind == "xmr_wallet_destroy" {
		if err := validateRetentionXMRDestruction(t.XMRDestruction); err != nil {
			return err
		}
		if t.XMRDestruction.WalletName != t.ID || !t.XMRDestruction.DestroyedAt.Equal(t.At) {
			return errors.New("Monero destruction certificate does not match its manifest scope")
		}
	} else if t.XMRDestruction != nil {
		return errors.New("unexpected Monero destruction certificate scope")
	}
	if len(t.KeyFloors) > 0 && t.Kind != "account_close" {
		return errors.New("unexpected account key-floor scope")
	}
	for _, f := range t.KeyFloors {
		hash, err := hex.DecodeString(f.KeyHMAC)
		if err != nil || len(hash) != 32 || f.NextIndex < 1 {
			return errors.New("invalid account key-floor certificate")
		}
	}
	return nil
}

func validateRetentionHold(h retentionHold) error {
	switch h.Scope {
	case "account", "message", "order", "audit":
	default:
		return fail(400, "Choose an account, message, order or audit preservation scope.")
	}
	if h.ID == "" || len(h.ID) > 128 || h.RecordID == "" || len(h.RecordID) > 128 || !textLength(h.Authority, 1, 1000) || !textLength(h.Reason, 1, 2000) || !textLength(h.Owner, 1, 200) || !textLength(h.Readers, 1, 1000) || !textLength(h.ReleaseCondition, 1, 2000) || h.BeganAt.IsZero() || h.ReviewAt.Before(h.BeganAt) || h.ReviewAt.After(time.Now().UTC().Add(30*24*time.Hour)) {
		return fail(400, "Record the preservation authority, reason, owner, authorized readers, release condition and a review date within 30 days.")
	}
	for _, field := range []string{h.Authority, h.Reason, h.Owner, h.Readers, h.ReleaseCondition} {
		if strings.TrimSpace(field) == "" {
			return fail(400, "Preservation metadata must not be empty.")
		}
	}
	if h.Scope == "audit" {
		if id, err := strconv.ParseInt(h.RecordID, 10, 64); err != nil || id < 1 {
			return fail(400, "Use the numeric audit event ID for preservation.")
		}
	}
	if h.ReleasedAt != nil && (h.ReleasedAt.Before(h.BeganAt) || h.ReleasedAt.After(time.Now().UTC())) {
		return errors.New("invalid preservation release clock")
	}
	return nil
}

func (s *retentionService) write() (returnErr error) {
	defer func() {
		if returnErr != nil {
			class := "state_unavailable"
			s.failure.Store(&class)
			returnErr = errors.Join(errRetentionStateUnavailable, returnErr)
		}
	}()
	s.state.Sequence++
	b, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	envelope, err := json.Marshal(retentionEnvelope{hex.EncodeToString(sum[:]), b})
	if err != nil {
		return err
	}
	if len(envelope) > retentionStateLimit {
		return errors.New("retention state exceeds its safe storage limit; provision capacity before erasure")
	}
	f, err := os.CreateTemp(s.dir, ".retention-state-*")
	if err != nil {
		return err
	}
	p := f.Name()
	defer os.Remove(p)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(envelope)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(p, filepath.Join(s.dir, "retention-state.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (a *App) initRetention(ctx context.Context, cfg retentionConfig) error {
	var pin string
	var restore bool
	if err := a.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM settings WHERE key='retention_instance_id'),''),COALESCE((SELECT value='true' FROM settings WHERE key='retention_restore_required'),false)`).Scan(&pin, &restore); err != nil {
		return err
	}
	if !cfg.Enabled {
		if pin != "" || restore {
			return errors.New("retention previously enrolled or restore pending: RETENTION_ENABLED and the current external RETENTION_STATE_DIR are required")
		}
		return nil
	}
	if cfg.StateDir == "" || !filepath.IsAbs(cfg.StateDir) {
		return errors.New("RETENTION_STATE_DIR must name an absolute external durable directory")
	}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
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
	err = s.read()
	if errors.Is(err, os.ErrNotExist) && pin == "" && !restore {
		now := time.Now().UTC()
		s.state = retentionState{Version: 1, InstanceID: randomToken(), EnabledAt: now, EnrollmentDeadline: now.Add(30 * 24 * time.Hour)}
		s.state.Tombstones, err = snapshotRetentionXMRDestructions(ctx, a.db, now)
		if err == nil {
			err = s.initializeBackupRegister()
		}
		if err == nil {
			err = s.write()
		}
	}
	if err != nil {
		unlock()
		return fmt.Errorf("current external retention state is required before traffic: %w", err)
	}
	if pin != "" && pin != s.state.InstanceID {
		unlock()
		return errors.New("retention state belongs to a different database instance")
	}
	a.retention = s
	if err = a.checkRetentionSequence(ctx, a.db); err != nil {
		unlock()
		return err
	}
	if err = a.resolveRetentionPending(ctx); err == nil {
		var tx *sql.Tx
		if tx, err = a.db.BeginTx(ctx, nil); err == nil {
			err = a.bindRetentionInstance(ctx, tx)
			if err == nil {
				err = a.replayRetention(ctx, tx)
			}
			if err == nil {
				err = a.recordRetentionSequence(ctx, tx)
			}
			if err == nil {
				var notified bool
				if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM settings WHERE key='retention_enrollment_notified')`).Scan(&notified); err == nil && !notified {
					rows, queryErr := tx.QueryContext(ctx, `SELECT id FROM users WHERE created<=$1 AND closed_at IS NULL`, s.state.EnabledAt)
					if queryErr != nil {
						err = queryErr
					} else {
						var ids []string
						ids, err = retentionScanIDs(rows)
						for _, id := range ids {
							if err != nil {
								break
							}
							_, err = tx.ExecContext(ctx, `INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)`, randomToken(), id, "Retention is now enabled. After a 30-day enrollment grace, individual messages expire after 90 days, notifications after 30 days, and audit records after 180 days. Historical accounts must reconfirm activity within 90 days. Save needed messages and deliveries locally. Preservation exceptions apply.")
						}
						if err == nil {
							_, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('retention_enrollment_notified','true') ON CONFLICT DO NOTHING`)
						}
					}
				}
			}
			if err == nil {
				err = tx.Commit()
			} else {
				tx.Rollback()
			}
		}
	}
	unlock()
	if err != nil {
		return err
	}
	// Startup replay and expiry complete before restoring either user traffic or
	// any adapter's payment-sending background workers.
	if err = a.runRetention(ctx); err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('retention_restore_required','false') ON CONFLICT(key) DO UPDATE SET value='false'`)
	return err
}

func (a *App) resolveRetentionPending(ctx context.Context) error {
	s := a.retention
	for len(s.state.Pending) > 0 {
		op := s.state.Pending[0]
		var committed bool
		if err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM retention_operations WHERE id=$1)`, op.ID).Scan(&committed); err != nil {
			return err
		}
		if !committed {
			return errors.New("unresolved retention commit: the database has no operation marker; keep restore isolated and reconcile the external journal")
		}
		s.commitOperation(op)
		if err := s.write(); err != nil {
			return err
		}
	}
	return nil
}

func (s *retentionService) commitOperation(op retentionOperation) {
	observed := time.Now().UTC()
	if observed.Before(op.At) {
		observed = op.At
	}
	for _, t := range op.Tombstones {
		at := observed
		t.CommittedAt = &at
		s.state.Tombstones = append(s.state.Tombstones, t)
	}
	if op.Request != nil {
		s.state.Requests = append(s.state.Requests, *op.Request)
	}
	if op.Hold != nil {
		found := false
		for i := range s.state.Holds {
			if s.state.Holds[i].ID == op.Hold.ID {
				s.state.Holds[i] = *op.Hold
				found = true
				break
			}
		}
		if !found {
			s.state.Holds = append(s.state.Holds, *op.Hold)
		}
	}
	if op.ReleaseHold != "" {
		for i := range s.state.Holds {
			if s.state.Holds[i].ID == op.ReleaseHold {
				at := op.At
				s.state.Holds[i].ReleasedAt = &at
			}
		}
	}
	for i, p := range s.state.Pending {
		if p.ID == op.ID {
			s.state.Pending = append(s.state.Pending[:i], s.state.Pending[i+1:]...)
			break
		}
	}
}

func (a *App) prepareRetentionOperation(ctx context.Context, tx *sql.Tx, op retentionOperation) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO retention_operations(id) VALUES($1)`, op.ID); err != nil {
		return err
	}
	a.retention.state.Pending = append(a.retention.state.Pending, op)
	if err := a.retention.write(); err != nil {
		return err
	}
	return a.recordRetentionSequence(ctx, tx)
}

type retentionSequenceReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (a *App) checkRetentionSequence(ctx context.Context, q retentionSequenceReader) error {
	var pin string
	var sequence uint64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM settings WHERE key='retention_instance_id'),''),COALESCE((SELECT value::bigint FROM settings WHERE key='retention_manifest_sequence'),0)`).Scan(&pin, &sequence); err != nil {
		return err
	}
	if pin != "" && pin != a.retention.state.InstanceID {
		return a.retention.unavailable(errors.New("external retention state belongs to a different database instance; current state is required"))
	}
	if sequence > a.retention.state.Sequence {
		class := "state_unavailable"
		a.retention.failure.Store(&class)
		return errors.Join(errRetentionStateUnavailable, errors.New("external retention state is older than the database checkpoint; current state is required"))
	}
	return nil
}

func (a *App) bindRetentionInstance(ctx context.Context, tx *sql.Tx) error {
	var bound string
	err := tx.QueryRowContext(ctx, `INSERT INTO settings(key,value) VALUES('retention_instance_id',$1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value WHERE settings.value=EXCLUDED.value RETURNING value`, a.retention.state.InstanceID).Scan(&bound)
	if err == sql.ErrNoRows {
		return a.retention.unavailable(errors.New("database retention identity is already bound to a different instance"))
	}
	return err
}
func (a *App) recordRetentionSequence(ctx context.Context, tx *sql.Tx) error {
	var written string
	err := tx.QueryRowContext(ctx, `INSERT INTO settings(key,value) VALUES('retention_manifest_sequence',$1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value WHERE settings.value::bigint<=EXCLUDED.value::bigint RETURNING value`, fmt.Sprint(a.retention.state.Sequence)).Scan(&written)
	if err == sql.ErrNoRows {
		return errors.Join(errRetentionStateUnavailable, errors.New("retention checkpoint cannot move backwards"))
	}
	return err
}

func (a *App) finalizeRetentionOperation(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	s := a.retention
	if s == nil {
		return errors.New("retention is not enabled")
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err = s.read(); err != nil {
		return err
	}
	if err = a.checkRetentionSequence(ctx, a.db); err != nil {
		return err
	}
	var committed bool
	if err = a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM retention_operations WHERE id=$1)`, id).Scan(&committed); err != nil {
		return err
	}
	if !committed {
		return errors.New("retention operation was not committed")
	}
	for _, op := range s.state.Pending {
		if op.ID == id {
			s.commitOperation(op)
			return s.write()
		}
	}
	return nil
}

func (a *App) abortRetentionOperation(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	s := a.retention
	if s == nil {
		return nil
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err = s.read(); err != nil {
		return err
	}
	if err = a.checkRetentionSequence(ctx, a.db); err != nil {
		return err
	}
	var committed bool
	if err = a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM retention_operations WHERE id=$1)`, id).Scan(&committed); err != nil {
		return err
	}
	if committed {
		return errors.New("cannot abort a committed retention operation")
	}
	for i, op := range s.state.Pending {
		if op.ID == id {
			s.state.Pending = append(s.state.Pending[:i], s.state.Pending[i+1:]...)
			return s.write()
		}
	}
	return nil
}
