package market

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const brandingLock = 782502
const brandingTicketLifetime = 10 * time.Minute
const brandingUploadPath = "/branding/logo/upload"

var brandingTokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var brandingBytes = &byteBudget{limit: 16 << 20}

// BrandingView contains only public presentation metadata, never an upload
// capability or administrator identity. A logo remains visible on sign-in pages.
type BrandingView struct {
	LogoURL           string
	UploadEnabled     bool
	UploadUnavailable string
}

func init() {
	registerAction("/admin/branding/authorize", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: authorizeBrandingUpload})
	registerAction("/admin/branding/reset", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: resetBranding})
	registerRaw("/admin/branding/upload", brandingHandoff)
	registerBackground(brandingTicketSweeper)
}

// Authorization expiry is enforced synchronously on the database clock. This
// best-effort cleanup also removes expired sealed capabilities on installations
// where the broader retention worker is disabled.
func brandingTicketSweeper(ctx context.Context, a *App) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := a.sweepBrandingTickets(ctx); err != nil && ctx.Err() == nil {
			log.Printf("branding authorization sweep: %s", errorCause(err))
		}
		t.Reset(time.Minute)
	}
}

func (a *App) sweepBrandingTickets(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := a.db.ExecContext(ctx, `WITH expired AS (
	 SELECT user_id FROM branding_upload_tickets WHERE expires<=clock_timestamp()
	 ORDER BY expires LIMIT 1000 FOR UPDATE SKIP LOCKED
	) DELETE FROM branding_upload_tickets t USING expired WHERE t.user_id=expired.user_id`)
	return err
}

func (a *App) brandingUploadAvailability() (bool, string) {
	if a.isolatedUploads && a.adminSocket != "" && a.brandingUploadOrigin == "" {
		return false, "The operator must configure BRANDING_UPLOAD_ORIGIN with the public upload gateway origin before uploading a logo. Originals must not reach the private application listener."
	}
	if !a.isolatedUploads && a.scanner.State().Name != scannerReady {
		return false, "Logo uploads require a ready malware scanner. Configure ClamAV and wait for current signatures."
	}
	return true, ""
}

func (a *App) loadBranding(ctx context.Context) (*BrandingView, error) {
	v := &BrandingView{}
	v.UploadEnabled, v.UploadUnavailable = a.brandingUploadAvailability()
	var version string
	err := a.db.QueryRowContext(ctx, `SELECT version FROM market_branding WHERE singleton`).Scan(&version)
	if err == sql.ErrNoRows {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	v.LogoURL = "/branding/logo/" + version
	return v, nil
}

func authorizeBrandingUpload(c *actionCtx) (actionResult, error) {
	if ready, message := c.A.brandingUploadAvailability(); !ready {
		return actionResult{}, fail(http.StatusServiceUnavailable, message)
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		if _, err := c.Tx.ExecContext(c.Ctx(), `SELECT pg_advisory_xact_lock($1)`, brandingLock); err != nil {
			return actionResult{}, err
		}
		token := randomToken()
		sealed, err := c.A.seal("branding-upload", token)
		if err != nil {
			return actionResult{}, err
		}
		_, err = c.Tx.ExecContext(c.Ctx(), `INSERT INTO branding_upload_tickets(user_id,token_hash,sealed_token,session_hash,expires)
		 VALUES($1,$2,$3,$4,clock_timestamp()+interval '10 minutes') ON CONFLICT(user_id) DO UPDATE
		 SET token_hash=excluded.token_hash,sealed_token=excluded.sealed_token,session_hash=excluded.session_hash,
		 admitted=false,expires=excluded.expires,created=clock_timestamp()`, c.User.ID, digest(token), sealed, digest(c.Token))
		if err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/admin/branding/upload", Audit: "Authorized one logo upload (expires in 10 minutes)"}, nil
	})
}

func resetBranding(c *actionCtx) (actionResult, error) {
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		if _, err := c.Tx.ExecContext(c.Ctx(), `SELECT pg_advisory_xact_lock($1)`, brandingLock); err != nil {
			return actionResult{}, err
		}
		if _, err := c.Tx.ExecContext(c.Ctx(), `DELETE FROM market_branding`); err != nil {
			return actionResult{}, err
		}
		if _, err := c.Tx.ExecContext(c.Ctx(), `DELETE FROM branding_upload_tickets`); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/admin/site#branding", Audit: "Reset marketplace logo to the default; pending logo uploads revoked"}, nil
	})
}

var brandingHandoffTemplate = template.Must(template.New("branding-handoff").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Upload market logo</title><link rel="stylesheet" href="/static/style.css"></head>
<body><main class="auth-panel"><h1>Upload market logo</h1><p>Your authorization lasts 10 minutes and allows one upload. Choose a JPEG, PNG, or simple self-contained SVG. JPEG and PNG inputs are resized and re-encoded as JPEG with a white background; their metadata is removed. Use SVG for transparent artwork. The site always fits the logo inside its fixed display box.</p>
{{if .External}}<p>This file goes directly to the market's separate upload gateway. Your administrator password, authentication code and administrator session are not included in this form.</p>{{end}}
<form method="post" action="{{.Action}}" enctype="multipart/form-data"><input type="hidden" name="ticket" value="{{.Ticket}}"><label for="market-logo-file">Logo file (maximum 4 MiB; SVG maximum 256 KiB)</label><input id="market-logo-file" type="file" name="logo" required accept="image/jpeg,image/png,image/svg+xml,.jpg,.jpeg,.png,.svg"><button type="submit">Upload logo</button></form>
<p>SVG scripts, styles, linked or embedded images, animations and external resources are refused. A failed or interrupted upload may need a new authorization.</p><p><a href="/admin/site#branding">Back to branding settings</a></p></main></body></html>`))

func brandingHandoff(a *App, w http.ResponseWriter, r *http.Request, u *User) {
	if a.preview || u == nil || u.Role != "admin" {
		http.NotFound(w, r)
		return
	}
	var sealed string
	err := a.db.QueryRowContext(r.Context(), `SELECT sealed_token FROM branding_upload_tickets WHERE user_id=$1 AND session_hash=$2 AND NOT admitted AND expires>clock_timestamp()`, u.ID, digest(sessionToken(r))).Scan(&sealed)
	if err == sql.ErrNoRows {
		a.refuse(w, r, u, 409, "No unused logo authorization remains. Return to branding settings and authorize a new upload.")
		return
	}
	if err != nil {
		writeDBFailure(w, r, err)
		return
	}
	token, err := a.open("branding-upload", sealed)
	if err != nil || !brandingTokenPattern.MatchString(token) {
		serverError(w, r, 500, "Unable to open logo authorization", "branding_ticket_unreadable")
		return
	}
	target := a.brandingUploadOrigin + brandingUploadPath
	formAction := "'self'"
	if a.brandingUploadOrigin != "" {
		formAction += " " + a.brandingUploadOrigin
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; form-action "+formAction+"; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var b bytes.Buffer
	if err = brandingHandoffTemplate.Execute(&b, struct {
		Action, Ticket string
		External       bool
	}{target, token, a.brandingUploadOrigin != ""}); err != nil {
		serverError(w, r, 500, "Unable to render logo upload", "branding_handoff_render")
		return
	}
	if r.Method != http.MethodHead {
		w.Write(b.Bytes())
	}
}

func (a *App) serveBrandingLogo(w http.ResponseWriter, r *http.Request, version string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if a.preview || !formNoncePattern.MatchString(version) {
		http.NotFound(w, r)
		return
	}
	var mime string
	var size int
	var body []byte
	if r.Method != http.MethodHead {
		if !brandingBytes.take(maxFullBytes) {
			busy(w)
			return
		}
		defer brandingBytes.give(maxFullBytes)
	}
	query := `SELECT content_type,octet_length(body),body FROM market_branding WHERE singleton AND version=$1`
	if r.Method == http.MethodHead {
		query = `SELECT content_type,octet_length(body),NULL::bytea FROM market_branding WHERE singleton AND version=$1`
	}
	err := a.db.QueryRowContext(r.Context(), query, version).Scan(&mime, &size, &body)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeDBFailure(w, r, err)
		return
	}
	if mime != "image/jpeg" && mime != "image/svg+xml" || size == 0 || size > maxFullBytes {
		serverError(w, r, 500, "Logo unavailable", "invalid_branding_asset")
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", fmt.Sprint(size))
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

type normalizedBranding struct {
	ContentType string
	Body        []byte
	Width       int
	Height      int
}

func validNormalizedBranding(v normalizedBranding) bool {
	if v.Width < 1 || v.Width > 1200 || v.Height < 1 || v.Height > 1200 || len(v.Body) == 0 || len(v.Body) > maxFullBytes {
		return false
	}
	switch v.ContentType {
	case "image/jpeg":
		w, h, ok := normalizedJPEGDimensions(v.Body)
		return ok && w == v.Width && h == v.Height
	case "image/svg+xml":
		clean, w, h, err := normalizeBrandingSVG(v.Body)
		return err == nil && w == v.Width && h == v.Height && bytes.Equal(clean, v.Body)
	}
	return false
}

var errBrandingTicket = fail(409, "Logo authorization expired or was already used. Authorize another upload in the private administrator settings; nothing was changed.")

// withBrandingTicket rechecks current account and session state under the same
// actor/session locks as confirmed forms. No request header or JSON field can
// turn an ordinary upload into a private administrator request.
func (a *App) withBrandingTicket(ctx context.Context, token string, admitted bool, run func(*sql.Tx, string) error) error {
	if a.retentionMaintenanceError() == "state_unavailable" {
		return fail(503, "Retention preservation state is unavailable. Logo changes are paused until the operator repairs it.")
	}
	if !brandingTokenPattern.MatchString(token) {
		return errBrandingTicket
	}
	var actor, sessionHash string
	err := a.db.QueryRowContext(ctx, `SELECT user_id,session_hash FROM branding_upload_tickets WHERE token_hash=$1`, digest(token)).Scan(&actor, &sessionHash)
	if err == sql.ErrNoRows {
		return errBrandingTicket
	}
	if err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role string
	var inactive bool
	err = tx.QueryRowContext(ctx, `SELECT role,suspended_at IS NOT NULL OR closed_at IS NOT NULL OR retention_restricted_at IS NOT NULL FROM users WHERE id=$1 FOR NO KEY UPDATE`, actor).Scan(&role, &inactive)
	if err == sql.ErrNoRows || err == nil && (role != "admin" || inactive) {
		return errBrandingTicket
	}
	if err != nil {
		return err
	}
	var live int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE token_hash=$1 AND user_id=$2 AND expires>clock_timestamp() FOR SHARE`, sessionHash, actor).Scan(&live)
	if err == sql.ErrNoRows {
		return errBrandingTicket
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, brandingLock); err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM branding_upload_tickets WHERE user_id=$1 AND token_hash=$2 AND session_hash=$3 AND admitted=$4 AND expires>clock_timestamp() FOR UPDATE`, actor, digest(token), sessionHash, admitted).Scan(&live)
	if err == sql.ErrNoRows {
		return errBrandingTicket
	}
	if err != nil {
		return err
	}
	if err = run(tx, actor); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return commitOutcomeUnknown{err}
	}
	return nil
}

func (a *App) admitBranding(ctx context.Context, token string) error {
	return a.withBrandingTicket(ctx, token, false, func(tx *sql.Tx, _ string) error {
		_, err := tx.ExecContext(ctx, `UPDATE branding_upload_tickets SET admitted=true,sealed_token='' WHERE token_hash=$1`, digest(token))
		return err
	})
}

func (a *App) finishBranding(ctx context.Context, token string, v normalizedBranding, scanInfected bool) error {
	if len(v.Body) != 0 && !validNormalizedBranding(v) {
		return fail(422, "Logo not saved: the upload service returned an invalid normalized logo. Nothing was stored.")
	}
	return a.withBrandingTicket(ctx, token, true, func(tx *sql.Tx, actor string) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM branding_upload_tickets WHERE token_hash=$1`, digest(token)); err != nil {
			return err
		}
		audit := "Logo upload failed; no branding changed"
		if scanInfected {
			audit = auditUploadRefusedInfected
		}
		if len(v.Body) != 0 {
			_, err := tx.ExecContext(ctx, `INSERT INTO market_branding(singleton,version,content_type,body,width,height,updated_by)
			 VALUES(true,$1,$2,$3,$4,$5,$6) ON CONFLICT(singleton) DO UPDATE SET version=excluded.version,
			 content_type=excluded.content_type,body=excluded.body,width=excluded.width,height=excluded.height,
			 updated_by=excluded.updated_by,updated_at=clock_timestamp()`, randomToken()[:32], v.ContentType, v.Body, v.Width, v.Height, actor)
			if err != nil {
				return err
			}
			audit = "Replaced marketplace logo using a preconfirmed one-use upload authorization"
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO audit_events(user_id,action) VALUES($1,$2)`, actor, audit)
		return err
	})
}

func brandingError(w http.ResponseWriter, r *http.Request, err error) {
	var he *httpError
	var unknown commitOutcomeUnknown
	switch {
	case errors.As(err, &he):
		brandingResultPage(w, he.Code, "Logo not saved", he.Msg)
	case errors.As(err, &unknown):
		brandingResultPage(w, 500, "Logo status unknown", "The change may have been saved. Check the logo and audit trail in private administrator settings before authorizing another upload.")
	default:
		serverError(w, r, 503, "Logo unavailable", errorCause(err))
	}
}

func brandingResultPage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(status)
	fmt.Fprint(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>`+template.HTMLEscapeString(title)+`</title><h1>`+template.HTMLEscapeString(title)+`</h1><p>`+template.HTMLEscapeString(message)+`</p><p>Return to the private administrator settings to check the result.</p><p><a href="/">View market</a></p></html>`)
}

func brandingScannerError(err error) (int, string, bool) {
	var he *httpError
	var sr *scanRefusal
	switch {
	case errors.As(err, &he):
		return he.Code, strings.ReplaceAll(he.Msg, "Photo", "Logo"), false
	case errors.As(err, &sr):
		return sr.Status(), strings.ReplaceAll(sr.Message(""), "Photo", "Logo"), sr.Class == scanInfected
	}
	return 503, "Logo processing did not finish. Nothing was stored. Authorize a new upload and try again later.", false
}
