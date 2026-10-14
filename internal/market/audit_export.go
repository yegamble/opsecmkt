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

// A-240: an export is buffered for its detached Ed25519 signature and can stay in memory throughout a slow
// download. Permit one such buffer per process, holding its slot through the final write (including a failed
// write); ordinary pages and staff actions keep their own admission classes.
var auditExportWork = make(chan struct{}, 1)

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
	recipient, err := LoadAuditExportRecipient(os.Getenv("AUDIT_EXPORT_RECIPIENT"))
	if err != nil {
		return &AuditExportView{Error: err.Error() + "; signed audit export is unavailable."}
	}
	return &AuditExportView{Available: true, Encrypted: recipient != nil, PublicKey: hex.EncodeToString(key.Public().(ed25519.PublicKey))}
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
	return auditExportRange(ctx, db, 0, upto)
}

// auditExportRange selects the exclusive lower and inclusive upper event IDs. Adjacent ranges concatenate
// to the original export; from=0 retains its bytes and detached signature.
func auditExportRange(ctx context.Context, db *sql.DB, from, upto int64) ([]byte, error) {
	// One statement streams every row, so on a large table it runs for longer than the application pool's 8 s
	// statement_timeout allows (A-177) and is exempt from it: 1M rows took about 6 s. The caller's ctx bounds it
	// (auditExportBuildTimeout in the handler); lock_timeout and idle_in_transaction_session_timeout still apply.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.id, e.user_id, u.handle, e.action, e.created FROM audit_events e LEFT JOIN users u ON u.id=e.user_id WHERE e.id>$1 AND e.id<=$2 ORDER BY e.id`, from, upto)
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
	recipient, err := LoadAuditExportRecipient(os.Getenv("AUDIT_EXPORT_RECIPIENT"))
	if err != nil {
		http.Error(w, err.Error()+"; signed audit export is unavailable.", http.StatusConflict)
		return
	}
	q := r.URL.Query()
	upto, err := strconv.ParseInt(q.Get("upto"), 10, 64)
	if err != nil || upto < 1 {
		http.Error(w, "upto must be a positive audit event id", 400)
		return
	}
	var from int64
	if raw := q.Get("from"); raw != "" {
		from, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || from < 0 || from > upto {
			http.Error(w, "from must be a nonnegative audit event id no greater than upto", 400)
			return
		}
	}
	releaseExport, ok := tryTake(auditExportWork)
	if !ok {
		w.Header().Set("Retry-After", busyRetryAfter)
		http.Error(w, "Another audit export is being prepared or sent. Wait for it to finish, then try again.", http.StatusServiceUnavailable)
		return
	}
	defer releaseExport()
	// A-139: the export sets its own bounds. Its build may outlast ServeHTTP's 12 s request context, and over Tor
	// (0.19–0.45 MB/s measured) its download outlasts the 20 s WriteTimeout: a cut download fails verify-audit like
	// tampering. It still stops when the client goes away. The read deadline is moved with the write deadline only
	// defensively: net/http already clears the read deadline once a body-less request (this GET) has been read
	// (connReader.startBackgroundRead), so the server's 15 s ReadTimeout never cancels this handler (A-251). Every
	// other route keeps the server's timeouts (each request resets them).
	rc := http.NewResponseController(w)
	extend := func(d time.Duration) { // an httptest recorder has no deadlines and refuses both
		t := time.Now().Add(d)
		rc.SetReadDeadline(t)
		rc.SetWriteDeadline(t)
	}
	extend(auditExportBuildTimeout + auditExportWriteBase)
	ctx, cancel := context.WithTimeout(arrivalContext(r), auditExportBuildTimeout)
	defer cancel()
	settled, err := settledAuditMax(ctx, a.db)
	if err != nil {
		writeDBFailureContext(ctx, w, r, err)
		return
	}
	if upto > settled {
		http.Error(w, fmt.Sprintf("upto %d is beyond the latest recorded audit event (%d)", upto, settled), 400)
		return
	}
	export, err := auditExportRange(ctx, a.db, from, upto)
	if err != nil {
		if dbBusyForContext(ctx, err) {
			serverError(w, r, 503, errBusyMsg, errorCause(err))
			return
		}
		serverError(w, r, 500, "Unable to export audit events", errorCause(err))
		return
	}
	name := AuditExportName(from, upto)
	body := export
	ext := ".jsonl"
	contentType := "application/x-ndjson"
	if q.Get("sig") == "1" {
		body = auditSignature(key, export)
		ext = ".sig"
		contentType = "text/plain; charset=utf-8"
	}
	body, err = recipient.Encrypt(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if recipient != nil {
		contentType = "application/pgp-encrypted"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+ext+recipient.Suffix()+`"`)
	extend(auditExportWriteWindow(len(body)))
	w.Write(body)
}

// The signed audit export's own bounds (A-139), in place of ServeHTTP's 12 s request context and the server's
// ReadTimeout (15 s) and WriteTimeout (20 s) set in cmd/server.
const (
	// auditExportBuildTimeout bounds the export's database work: one million audit rows took about 6 s.
	auditExportBuildTimeout = time.Minute
	// auditExportWriteBase is the time to send a small file (the signature), the server's WriteTimeout.
	auditExportWriteBase = 20 * time.Second
	// auditExportMinRate is the slowest download, in bytes per second, the window allows: about a sixth of the
	// slowest onion throughput measured (0.19 MB/s).
	auditExportMinRate = 32 << 10
)

// auditExportWriteWindow is the time allowed to send a response of n bytes: 20 s plus one second per 32 KiB,
// without a ceiling (a 100 MB export gets about 53 minutes). Only an administrator can start one, at most 10
// per 10 minutes, and while it is sent it holds its connection but no admission slot or database connection.
func auditExportWriteWindow(n int) time.Duration {
	return auditExportWriteBase + time.Duration(n/auditExportMinRate+1)*time.Second
}
