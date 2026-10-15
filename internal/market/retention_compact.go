package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A preserved old dump extends only tombstones it could contain. Review dates
// are deliberately not expiry dates: an overdue legal hold still protects both
// the backup and its restore-deletion history.
type retentionBackupHold struct {
	CaseID            string     `json:"case_id"`
	Kind              string     `json:"kind"`
	ArchivePath       string     `json:"archive_path"`
	SHA256            string     `json:"sha256"`
	CreatedAt         time.Time  `json:"created_at"`
	ExpiresAt         time.Time  `json:"expires_at"`
	Reason            string     `json:"reason"`
	Owner             string     `json:"owner"`
	AuthorizedReaders []string   `json:"authorized_readers"`
	PermittedUse      string     `json:"permitted_use"`
	StartedAt         time.Time  `json:"started_at"`
	ReviewedAt        time.Time  `json:"reviewed_at"`
	NextReviewAt      time.Time  `json:"next_review_at"`
	ReleaseCondition  string     `json:"release_condition"`
	ReleasedAt        *time.Time `json:"released_at"`
}

type retentionBackupRegister struct {
	Version int                   `json:"version"`
	Holds   []retentionBackupHold `json:"holds"`
}

func (s *retentionService) backupRegister() ([]retentionBackupHold, error) {
	p := filepath.Join(s.dir, "backup-preservations.json")
	b, err := readPrivateRetentionFile(p, 4<<20)
	if err != nil {
		return nil, err
	}
	var r retentionBackupRegister
	if err = strictRetentionJSON(b, &r); err != nil {
		return nil, err
	}
	if r.Version != 1 || r.Holds == nil {
		return nil, errors.New("invalid backup preservation register")
	}
	var raw struct {
		Version json.RawMessage              `json:"version"`
		Holds   []map[string]json.RawMessage `json:"holds"`
	}
	if err = strictRetentionJSON(b, &raw); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for _, h := range raw.Holds {
		if len(h) != 15 {
			return nil, errors.New("backup preservation record has missing fields")
		}
		for _, key := range []string{"created_at", "expires_at", "started_at", "reviewed_at", "next_review_at", "released_at"} {
			value, ok := h[key]
			if !ok {
				return nil, errors.New("backup preservation record has missing clocks")
			}
			if key == "released_at" && string(value) == "null" {
				continue
			}
			var stamp string
			if err = json.Unmarshal(value, &stamp); err != nil {
				return nil, err
			}
			if !retentionBackupClock.MatchString(stamp) {
				return nil, errors.New("backup preservation clocks must use UTC seconds")
			}
			parsed, parseErr := time.Parse(time.RFC3339, stamp)
			if parseErr != nil || parsed.Year() < 1970 {
				return nil, errors.New("invalid backup preservation clock")
			}
		}
	}
	for _, h := range r.Holds {
		if h.Kind != "postgresql_dump" && h.Kind != "audit_export" {
			return nil, errors.New("unknown backup preservation kind")
		}
		hash, decodeErr := hex.DecodeString(h.SHA256)
		days := 30
		if h.Kind == "audit_export" {
			days = 180
		}
		if !retentionBackupText(h.CaseID) || !retentionBackupText(h.ArchivePath) || !filepath.IsAbs(h.ArchivePath) || filepath.Clean(h.ArchivePath) != h.ArchivePath || decodeErr != nil || len(hash) != 32 || strings.ToLower(h.SHA256) != h.SHA256 || h.CreatedAt.IsZero() || h.CreatedAt.After(now) || !h.ExpiresAt.Equal(h.CreatedAt.Add(time.Duration(days)*24*time.Hour)) || !retentionBackupText(h.Owner) || !retentionBackupText(h.Reason) || len(h.AuthorizedReaders) == 0 || !retentionBackupText(h.PermittedUse) || !retentionBackupText(h.ReleaseCondition) || h.StartedAt.IsZero() || h.ReviewedAt.IsZero() || h.StartedAt.After(h.ReviewedAt) || h.ReviewedAt.After(now) || !h.NextReviewAt.After(h.ReviewedAt) || h.NextReviewAt.After(h.ReviewedAt.Add(30*24*time.Hour)) || h.ReleasedAt != nil && (h.ReleasedAt.Before(h.StartedAt) || h.ReleasedAt.After(now)) {
			return nil, errors.New("incomplete backup preservation record")
		}
		for _, reader := range h.AuthorizedReaders {
			if !retentionBackupText(reader) {
				return nil, errors.New("invalid backup preservation reader")
			}
		}
	}
	return r.Holds, nil
}

var retentionBackupClock = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)

func retentionBackupText(s string) bool { return strings.TrimSpace(s) != "" && len([]rune(s)) <= 2048 }

func (s *retentionService) initializeBackupRegister() error {
	p := filepath.Join(s.dir, "backup-preservations.json")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	b, _ := json.Marshal(retentionBackupRegister{Version: 1, Holds: []retentionBackupHold{}})
	_, err = f.Write(b)
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
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func retentionBackupNeedsManifest(holds []retentionBackupHold, at, now time.Time) bool {
	for _, h := range holds {
		if h.Kind != "postgresql_dump" || h.CreatedAt.After(at) {
			continue
		}
		if h.ReleasedAt == nil || now.Before(h.ReleasedAt.Add(8*24*time.Hour)) {
			return true
		}
	}
	return false
}

func (a *App) compactRetention(ctx context.Context, now time.Time) error {
	s := a.retention
	holds, err := s.backupRegister()
	if err != nil {
		return errors.Join(errRetentionStateUnavailable, err)
	}
	changed := false
	var tombstones []retentionTombstone
	for _, t := range s.state.Tombstones {
		if t.CommittedAt == nil {
			return errors.Join(errRetentionStateUnavailable, errors.New("deletion has no observed commit boundary"))
		}
		if now.Before(t.CommittedAt.Add(37*24*time.Hour)) || retentionBackupNeedsManifest(holds, *t.CommittedAt, now) {
			tombstones = append(tombstones, t)
		} else {
			changed = true
		}
	}
	s.state.Tombstones = tombstones
	var records []retentionHold
	for _, h := range s.state.Holds {
		if h.ReleasedAt == nil || now.Before(h.ReleasedAt.Add(180*24*time.Hour)) || retentionBackupNeedsManifest(holds, *h.ReleasedAt, now) {
			records = append(records, h)
		} else {
			changed = true
		}
	}
	s.state.Holds = records
	var requests []retentionRequest
	for _, r := range s.state.Requests {
		var closed bool
		if err = a.db.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM users WHERE id=$1 AND closed_at IS NULL)`, r.ID).Scan(&closed); err != nil {
			return err
		}
		if !closed || now.Before(r.At.Add(67*24*time.Hour)) || retentionBackupNeedsManifest(holds, r.At.Add(30*24*time.Hour), now) {
			requests = append(requests, r)
		} else {
			changed = true
		}
	}
	s.state.Requests = requests
	if changed {
		return s.write()
	}
	return nil
}
