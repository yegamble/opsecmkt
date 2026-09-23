package market

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// P6 Transparency: signed audit export. The server signs the exact NDJSON bytes of audit_events with id <= upto
// using an Ed25519 key taken only from AUDIT_SIGNING_KEY (never the database). A valid signature proves the file
// came from a holder of that key; it cannot prove the history was never altered before export.

func init() {
	registerRaw("/admin/audit-export", auditExportHandler)
	registerLoader("admin", func(ctx context.Context, a *App, r *http.Request, d *PageData) error {
		v := auditExportStatus()
		if err := a.db.QueryRowContext(ctx, "SELECT COALESCE(max(id),0) FROM audit_events").Scan(&v.UpTo); err != nil {
			return err
		}
		d.AuditExport = v
		return nil
	})
}

// auditSigningKey parses AUDIT_SIGNING_KEY (64 hex characters = Ed25519 seed). A nil key with nil error means unset.
func auditSigningKey() (ed25519.PrivateKey, error) {
	raw := strings.TrimSpace(os.Getenv("AUDIT_SIGNING_KEY"))
	if raw == "" {
		return nil, nil
	}
	seed, err := hex.DecodeString(raw)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("AUDIT_SIGNING_KEY is invalid (it must be 64 hexadecimal characters)")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// auditExportStatus reports whether signed exports are available and, if so, the public key to pin.
func auditExportStatus() *AuditExportView {
	key, err := auditSigningKey()
	switch {
	case err != nil:
		return &AuditExportView{Error: err.Error() + "; signed audit export is unavailable."}
	case key == nil:
		return &AuditExportView{Error: "AUDIT_SIGNING_KEY is not configured; signed audit export is unavailable."}
	}
	return &AuditExportView{Available: true, PublicKey: hex.EncodeToString(key.Public().(ed25519.PublicKey))}
}

type auditLine struct {
	ID      int64   `json:"id"`
	UserID  *string `json:"user_id"`
	Handle  *string `json:"handle"`
	Action  string  `json:"action"`
	Created string  `json:"created"`
}

// settledAuditMax returns the highest audit id below which no insert can still commit. The SHARE lock waits for
// every transaction currently inserting audit rows, so ids <= the result are final (rows are never updated).
func settledAuditMax(ctx context.Context, db *sql.DB) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "LOCK TABLE audit_events IN SHARE MODE"); err != nil {
		return 0, err
	}
	var n int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(max(id),0) FROM audit_events").Scan(&n); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// auditExport renders audit_events with id <= upto as NDJSON ordered by id. Same upto, same bytes.
func auditExport(ctx context.Context, db *sql.DB, upto int64) ([]byte, error) {
	rows, err := db.QueryContext(ctx, `SELECT e.id, e.user_id, u.handle, e.action, e.created FROM audit_events e LEFT JOIN users u ON u.id=e.user_id WHERE e.id<=$1 ORDER BY e.id`, upto)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var b bytes.Buffer
	enc := json.NewEncoder(&b) // one object per line, fixed field order
	for rows.Next() {
		var l auditLine
		var created time.Time
		if err = rows.Scan(&l.ID, &l.UserID, &l.Handle, &l.Action, &created); err != nil {
			return nil, err
		}
		l.Created = created.UTC().Format(time.RFC3339)
		if err = enc.Encode(l); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), rows.Err()
}

// auditSignature is the detached signature file: Ed25519 over the exact export bytes.
func auditSignature(key ed25519.PrivateKey, export []byte) []byte {
	sum := sha256.Sum256(export)
	return fmt.Appendf(nil, "algorithm: ed25519\npublic_key: %s\nsha256: %s\nsignature: %s\n",
		hex.EncodeToString(key.Public().(ed25519.PublicKey)), hex.EncodeToString(sum[:]), hex.EncodeToString(ed25519.Sign(key, export)))
}

func auditExportHandler(a *App, w http.ResponseWriter, r *http.Request, u *User) {
	if a.db == nil {
		http.Error(w, "Read-only preview: audit export needs PostgreSQL and AUDIT_SIGNING_KEY.", 403)
		return
	}
	if u == nil {
		http.Redirect(w, r, "/login", 303)
		return
	}
	if u.Role != "admin" {
		http.Error(w, "Administrator access required", 403)
		return
	}
	if !a.allow("export:"+u.ID, 10) {
		http.Error(w, "Too many exports. Try again later.", 429)
		return
	}
	key, err := auditSigningKey()
	if err != nil || key == nil {
		http.Error(w, auditExportStatus().Error+" Set a 64-hex-character AUDIT_SIGNING_KEY and restart the application.", 409)
		return
	}
	q := r.URL.Query()
	upto, err := strconv.ParseInt(q.Get("upto"), 10, 64)
	if err != nil || upto < 1 {
		http.Error(w, "upto must be a positive audit event id", 400)
		return
	}
	ctx := r.Context()
	settled, err := settledAuditMax(ctx, a.db)
	if err != nil {
		http.Error(w, "Service unavailable", 503)
		return
	}
	if upto > settled {
		http.Error(w, fmt.Sprintf("upto %d is beyond the latest recorded audit event (%d)", upto, settled), 400)
		return
	}
	export, err := auditExport(ctx, a.db, upto)
	if err != nil {
		http.Error(w, "Unable to export audit events", 500)
		return
	}
	name := fmt.Sprintf("audit-events-upto-%d", upto)
	if q.Get("sig") == "1" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.sig"`)
		w.Write(auditSignature(key, export))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.jsonl"`)
	w.Write(export)
}
