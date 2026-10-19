package market

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"html/template"
	"net/http"
	"slices"
	"sync"
	"time"
)

// A-551 (owner report 2026-10-18): staff uploads from the private administrator connection in the separated
// topology (UPLOAD_MODE=isolated with ADMIN_SOCKET). That connection reaches the app directly, which never accepts
// originals in this mode, and the public gateway refuses administrator sessions, so the ordinary avatar and
// listing-photo forms could only fail there. As for the logo (branding.go), staff confirm one upload with their
// password (and authenticator code when enrolled) on the private connection; the next page posts only the
// resulting capability and the file to the fixed gateway origin (BRANDING_UPLOAD_ORIGIN). The gateway trades the
// capability for an ordinary upload ticket over its mutual-TLS backend (/v1/uploads/staff-admit), normalizes the
// picture and finishes with /v1/uploads/finish. A capability is bound to the account, its session, the kind
// (avatar or listing photo) and, for a photo, the listing; it is usable once, for 10 minutes, and only after the
// confirming transaction committed (its form receipt). It is held in this process's memory: an app restart
// revokes unused ones. No password, authenticator code, session or CSRF token travels to the gateway.

const (
	staffUploadPath     = "/uploads/staff" // the gateway's capability form; the app refuses its multipart body unread
	staffUploadLifetime = 10 * time.Minute
	staffUploadNoOrigin = "Uploads from the private administrator connection are unavailable: the operator has not set BRANDING_UPLOAD_ORIGIN, the public upload gateway's origin. Originals must not reach the private application listener."
	staffUploadNotStaff = "Uploads from the private administrator connection are for market staff. Upload from the market's public address instead."
)

// staffUploadKinds maps a capability's kind to its upload route.
var staffUploadKinds = map[string]string{"avatar": "/account/avatar", "photo": "/listings/photo"}

const staffUploadTicketGone = "Upload authorization expired or was already used. Authorize another upload on the private administrator connection; nothing was stored."

func init() {
	registerAction("/admin/uploads/authorize", actionSpec{Roles: []string{"admin", "moderator"}, OwnTx: true, ConfirmedFormNonce: true, Run: authorizeStaffUpload})
	registerRaw("/admin/uploads/upload", staffUploadHandoff)
}

// staffUploadCapability is one confirmed, unused staff upload authorization.
type staffUploadCapability struct {
	token, userID string
	session       string // the confirming session (memory only, as an upload ticket's admission)
	nonce         string // the confirming form's receipt: usable only once that transaction committed
	path          string // the upload route: /account/avatar or /listings/photo
	listing       string // a photo's listing
	expires       time.Time
}

// staffUploadStore holds at most one unused capability per account; a newer authorization replaces it.
type staffUploadStore struct {
	mu      sync.Mutex
	pending map[string]staffUploadCapability // by account id
}

func (s *staffUploadStore) prune(now time.Time) {
	for id, c := range s.pending {
		if !now.Before(c.expires) {
			delete(s.pending, id)
		}
	}
}

func (s *staffUploadStore) put(c staffUploadCapability) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	if s.pending == nil {
		s.pending = map[string]staffUploadCapability{}
	}
	s.pending[c.userID] = c
	// An unused capability (and the session token it holds) leaves memory at expiry even if nothing else arrives.
	time.AfterFunc(time.Until(c.expires), func() { s.mu.Lock(); defer s.mu.Unlock(); s.prune(time.Now()) })
}

// forSession is the account's unused capability if the same session confirmed it.
func (s *staffUploadStore) forSession(userID, session string) (staffUploadCapability, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	c, ok := s.pending[userID]
	return c, ok && subtle.ConstantTimeCompare([]byte(c.session), []byte(session)) == 1
}

// take removes and returns the unexpired capability with this token: it is used at most once, whatever follows.
func (s *staffUploadStore) take(token string) (staffUploadCapability, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	for id, c := range s.pending {
		if subtle.ConstantTimeCompare([]byte(c.token), []byte(token)) == 1 {
			delete(s.pending, id)
			return c, true
		}
	}
	return staffUploadCapability{}, false
}

// privateUploadPath says how this request's upload forms work. Outside the separated topology's private
// administrator connection: ordinary forms (false, ""). On it, staff authorize one upload (true, ""), unless no
// gateway origin is configured or the account is not staff (false, why).
func (a *App) privateUploadPath(ctx context.Context, u *User) (bool, string) {
	if !a.isolatedUploads || !privateAdminIngress(ctx) || u == nil {
		return false, ""
	}
	if u.Role != "admin" && u.Role != "moderator" {
		return false, staffUploadNotStaff
	}
	if a.brandingUploadOrigin == "" {
		return false, staffUploadNoOrigin
	}
	return true, ""
}

func authorizeStaffUpload(c *actionCtx) (actionResult, error) {
	capability, why := c.A.privateUploadPath(c.Ctx(), c.User)
	if !capability {
		if why == "" {
			why = "This form is only for the private administrator connection when uploads use the separate gateway. Use the upload form on the page instead; nothing was changed."
		}
		return actionResult{}, fail(http.StatusConflict, why)
	}
	path := staffUploadKinds[c.Form.Get("kind")]
	if path == "" {
		return actionResult{}, fail(http.StatusBadRequest, "Unknown upload kind. Reload the page and try again; nothing was changed.")
	}
	if !slices.Contains(uploadRoutes[path].roles, c.User.Role) {
		return actionResult{}, fail(http.StatusForbidden, "Your account cannot upload this picture. Nothing was changed.")
	}
	st, err := c.A.uploadScannerState(c.Ctx())
	if err != nil {
		return actionResult{}, err
	}
	if st.Name != scannerReady {
		return actionResult{}, fail(http.StatusServiceUnavailable, st.VendorNotice())
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		capability := staffUploadCapability{token: randomToken(), userID: c.User.ID, session: c.Token, nonce: c.Form.Get(formNonceField), path: path, expires: time.Now().Add(staffUploadLifetime)}
		audit := "Authorized one avatar upload (expires in 10 minutes)"
		if path == "/listings/photo" {
			l, err := lockListing(c, c.Form.Get("id"))
			if err != nil {
				return actionResult{}, err
			}
			switch {
			case l.Takedown != "":
				return actionResult{}, fail(http.StatusConflict, "An administrator took this listing down, so photos cannot be added to it. Nothing was changed.")
			case l.Archived:
				return actionResult{}, fail(http.StatusConflict, "This listing is archived. Restore it before adding photos. Nothing was changed.")
			}
			capability.listing = l.ID
			audit = "Authorized one photo upload for listing " + shortID(l.ID) + " (expires in 10 minutes)"
		}
		c.A.staffUploads.put(capability)
		return actionResult{Redirect: "/admin/uploads/upload", Audit: audit}, nil
	})
}

// committedStaffUpload reports whether the capability's confirming transaction committed: its form receipt exists
// with the saved redirect. A capability put in memory by a transaction that then rolled back is never usable.
func (a *App) committedStaffUpload(ctx context.Context, c staffUploadCapability) (bool, error) {
	var ok bool
	err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM form_submissions WHERE nonce=$1 AND user_id=$2 AND session_hash=$3 AND action_path='/admin/uploads/authorize' AND redirect<>'')`,
		c.nonce, c.userID, digest(c.session)).Scan(&ok)
	return ok, err
}

var staffUploadHandoffTemplate = template.Must(template.New("staff-upload-handoff").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}}</title><link rel="stylesheet" href="/static/style.css"></head>
<body><main class="auth-panel"><h1>{{.Title}}</h1><p>Your authorization lasts 10 minutes and allows one upload. Choose a JPEG or PNG of at most 4 MB, 4096 pixels a side and 4096×3072 in all. {{if .Photo}}It is re-encoded to a JPEG at most 1200 pixels on its longer side.{{else}}It is cropped to its centre square and re-encoded to a JPEG of at most 256×256 pixels; it replaces any earlier avatar.{{end}} Location, camera and other metadata are removed and the original file is not kept.</p>
<p>This file goes directly to the market's separate upload gateway, where ClamAV checks it for known malware. Your password, authentication code and session are not included in this form. Uploading can take up to 90 seconds over Tor; press Upload once and wait for the page to change.</p>
<p>Nothing removes what the picture shows — faces, hands, reflections, the room, screens or handwriting — or the sensor noise that can link photos taken with one camera. Never reuse a photo you have used anywhere else.</p>
<form class="stack" method="post" action="{{.Action}}" enctype="multipart/form-data"><input type="hidden" name="ticket" value="{{.Ticket}}">{{if .Photo}}<label for="staff-photo-description">Photo description (optional)</label><input id="staff-photo-description" name="description" maxlength="250">{{end}}<label for="staff-upload-file">{{if .Photo}}Photo{{else}}Avatar{{end}}</label><input id="staff-upload-file" type="file" name="photo" required accept="image/jpeg,image/png"><button type="submit">{{if .Photo}}Upload photo{{else}}Upload avatar{{end}}</button></form>
<p>A failed or interrupted upload needs a new authorization.</p><p><a href="{{.Back}}">Back</a></p></main></body></html>`))

// staffUploadHandoff renders the capability form for the account's own unused authorization from this session.
func staffUploadHandoff(a *App, w http.ResponseWriter, r *http.Request, u *User) {
	if a.preview || u == nil || (u.Role != "admin" && u.Role != "moderator") {
		http.NotFound(w, r)
		return
	}
	c, ok := a.staffUploads.forSession(u.ID, sessionToken(r))
	if ok {
		committed, err := a.committedStaffUpload(r.Context(), c)
		if err != nil {
			writeDBFailure(w, r, err)
			return
		}
		ok = committed
	}
	if !ok || a.brandingUploadOrigin == "" {
		a.refuse(w, r, u, http.StatusConflict, "No unused upload authorization remains. Return to the upload form and authorize a new upload.")
		return
	}
	photo := c.path == "/listings/photo"
	title, back := "Upload avatar", "/account#avatar"
	if photo {
		title, back = "Upload listing photo", "/listing-edit?id="+c.listing+"#photos"
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; form-action 'self' "+a.brandingUploadOrigin+"; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var b bytes.Buffer
	if err := staffUploadHandoffTemplate.Execute(&b, struct {
		Title, Action, Ticket, Back string
		Photo                       bool
	}{title, a.brandingUploadOrigin + staffUploadPath, c.token, back, photo}); err != nil {
		serverError(w, r, 500, "Unable to render the upload form", "staff_upload_handoff_render")
		return
	}
	if r.Method != http.MethodHead {
		w.Write(b.Bytes())
	}
}

type staffGatewayAdmission struct{ Ticket string }

// admitGatewayStaffUpload trades a capability for an ordinary upload ticket bound to the capability's account,
// session, kind and listing. The capability is spent here whatever follows.
func (a *App) admitGatewayStaffUpload(w http.ResponseWriter, r *http.Request) {
	var v staffGatewayAdmission
	if readUploadJSON(r, 1024, &v) != nil {
		http.Error(w, "Invalid upload authorization", http.StatusBadRequest)
		return
	}
	refuse := func(code int, msg string) { brandingResultPage(w, code, "Upload not accepted", msg) }
	if !brandingTokenPattern.MatchString(v.Ticket) {
		refuse(http.StatusConflict, staffUploadTicketGone)
		return
	}
	c, ok := a.staffUploads.take(v.Ticket)
	if !ok {
		refuse(http.StatusConflict, staffUploadTicketGone)
		return
	}
	committed, err := a.committedStaffUpload(r.Context(), c)
	if err != nil {
		serverError(w, r, http.StatusServiceUnavailable, "Upload authorization unavailable", errorCause(err))
		return
	}
	actor, err := a.sessionUser(r.Context(), c.session)
	if err != nil {
		serverError(w, r, http.StatusServiceUnavailable, "Upload authorization unavailable", errorCause(err))
		return
	}
	route := uploadRoutes[c.path]
	if !committed || actor == nil || actor.ID != c.userID || (actor.Role != "admin" && actor.Role != "moderator") || !slices.Contains(route.roles, actor.Role) {
		refuse(http.StatusConflict, staffUploadTicketGone)
		return
	}
	if actor.RetentionRestricted {
		refuse(http.StatusForbidden, retentionRestrictedMessage)
		return
	}
	if !a.allowWrite(actor.ID) || !a.allowWriteBudget("upload:"+actor.ID, uploadBudget) {
		refuse(http.StatusTooManyRequests, "Photo not saved: too many uploads. Try again later. Nothing was stored.")
		return
	}
	release, ok := a.enterOneAtATime("upload", actor.ID)
	if !ok {
		refuse(http.StatusTooManyRequests, "Photo not saved: another upload from your account is in progress. Nothing was stored.")
		return
	}
	ticket, ok := a.uploadTickets.put(uploadTicket{admission: uploadAdmission{Path: c.path, Session: c.session}, actor: actor, expires: time.Now().Add(uploadTicketLifetime), release: release, capability: true, listing: c.listing})
	if !ok {
		release()
		refuse(http.StatusServiceUnavailable, errBusyMsg)
		return
	}
	kind := "avatar"
	if route.kind == uploadListing {
		kind = "photo"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"ticket": ticket, "kind": kind})
}

// staffUploadResult answers a capability upload on the public gateway: a standalone page with no cookie, session
// field or link into the private connection.
func staffUploadResult(w http.ResponseWriter, kind uploadKind, code int, msg string) {
	title := "Photo not saved"
	if kind == uploadAvatar {
		title = "Avatar not saved"
	}
	if code == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", busyRetryAfter)
	}
	brandingResultPage(w, code, title, msg)
}
