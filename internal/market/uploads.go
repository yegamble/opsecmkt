package market

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// A-186 (war room 2026-09-27, rulings C3, C5, C6 and D10): the only two routes that take a file. POST
// /listings/photo (vendors and administrators; the listing's id in the form) adds a photo to a listing, and POST
// /account/avatar (vendors, administrators and moderators — the owner's staff avatar decision; always the
// actor's own) sets the actor's avatar, replacing any earlier one. Besides them the escrow sign form, POST
// /escrow/sign, and A-459's bounded hardware sign forms take multipart PSBT files (psbt_upload.go); any other POST
// with a multipart body is refused with a 415 page before its body is read (ServeHTTP), and every other route keeps
// its 64 KiB form cap.
//
// Order of work, each refusal a rendered HTML page (renderUploadPage), never text/plain:
//
//  1. Origin and Sec-Fetch-Site (shared with every form post, postOriginError).
//  2. Content-Length required (absent or chunked: 411) and at most uploadCeiling (else 413); both unread.
//  3. The session lookup under a sessionLookupWork slot; signed in, with the route's role; the write:<user> and
//     upload:<user> budgets; a userPostWork slot and inside it an uploadWork slot; one upload at a time per
//     account (in memory, released when the handler returns, however it returns).
//  4. Only now the connection's read deadline becomes now+90 s and its write deadline now+150 s: an anonymous,
//     expired or unauthorised sender keeps the server's unextended 15 s ReadTimeout and holds no slot.
//  5. The body, through multipart.NewReader over io.LimitReader(r.Body, uploadBodyLimit) with the bytes
//     counted: parts csrf (first, at most 128 bytes), optional id, optional description (at most 250
//     characters), photo (at most 4 MB, read into one buffer sized from Content-Length); id and description
//     must be valid UTF-8 without a NUL byte, as every other form's fields (QA5W3B-5). A part's file name
//     and Content-Type are never read. Never http.MaxBytesReader (when exceeded it makes the server close the
//     connection, racing the answer against the unsent body: the browser shows a reset instead of the page),
//     never ParseMultipartForm or FormFile (they spill to temporary files).
//  6. Every refusal from step 1 on (but step 2's) first reads the rest of the body into io.Discard, at most
//     uploadCeiling bytes, under whichever read deadline is in force, so the browser gets the page and not a
//     reset; nothing is buffered. Past the ceiling or the deadline the reset stands.
//  7. A fresh context: the caps (inspectImage), the scan (one zINSTREAM of the bytes received), the decode
//     and re-encode under imageDecodeWork, then one transaction (storeImage) that re-checks the session and
//     role, locks the listing (not archived, not taken down) and the owner's row, checks the quotas and the
//     global limit, replaces an earlier avatar, inserts, audits, and commits; then 303.
//
// Memory: see maxConns in cmd/server.

const (
	uploadCeiling         = uploadCap + 4<<20  // largest Content-Length accepted, and the most a refusal drains
	uploadBodyLimit       = uploadCap + 64<<10 // the most the multipart reader consumes: the photo plus the form around it
	uploadBudget          = 20                 // uploads per account per ten minutes (upload:<user>)
	photosPerListing      = 4
	csrfPartLimit         = 128
	idPartLimit           = 64
	descriptionLimit      = 250 // characters
	defaultImageStorageMB = 2048
	imagesLock            = 186063 // pg_advisory_xact_lock key: one image insert checks the global limit at a time
)

var (
	// uploadWork admits two uploads at once (inside a userPostWork slot); imageDecodeWork one decode (inside it).
	uploadWork      = make(chan struct{}, 2)
	imageDecodeWork = make(chan struct{}, 1)
	// The connection deadlines set once an upload is admitted, and the bound on the work after its body has
	// arrived (tests shorten them).
	uploadReadWindow  = 90 * time.Second
	uploadWriteWindow = 150 * time.Second
	uploadWorkTimeout = 45 * time.Second
	// uploaderQuota bounds the bytes (full + thumbnail) of one account's photos and avatar.
	uploaderQuota int64 = 50 << 20
)

// uploadRoute is one of the two upload routes.
type uploadRoute struct {
	kind  uploadKind
	roles []string
}

var uploadRoutes = map[string]uploadRoute{
	"/listings/photo": {kind: uploadListing, roles: []string{"vendor", "admin"}},
	"/account/avatar": {kind: uploadAvatar, roles: []string{"vendor", "admin", "moderator"}},
}

// isMultipart reports a multipart request body.
func isMultipart(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "multipart/")
}

// imageStorageLimitFromEnv reads IMAGE_STORAGE_LIMIT_MB, the bytes all stored images may use (default 2048 MB).
func imageStorageLimitFromEnv(getenv func(string) string) (int64, error) {
	s := strings.TrimSpace(getenv("IMAGE_STORAGE_LIMIT_MB"))
	if s == "" {
		return defaultImageStorageMB << 20, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 || n > 1<<20 {
		return 0, errors.New("IMAGE_STORAGE_LIMIT_MB must be a whole number of megabytes from 1 to 1048576 (default 2048): the most all stored photos and avatars may use")
	}
	return n << 20, nil
}

// imageLimit is the global image storage limit in bytes.
func (a *App) imageLimit() int64 {
	if a.storageLimit > 0 {
		return a.storageLimit
	}
	return defaultImageStorageMB << 20
}

// The uploader's copy of refusals that are not about the picture itself.
var (
	errUploadForm        = &httpError{Code: http.StatusBadRequest, Msg: "Photo not saved: the form arrived incomplete or out of order. Reload the page and try again. Nothing was stored."}
	errUploadNoPhoto     = &httpError{Code: http.StatusBadRequest, Msg: "Photo not saved: choose a JPEG or PNG file to upload. Nothing was stored."}
	errUploadDescription = &httpError{Code: http.StatusBadRequest, Msg: "Photo not saved: the description is longer than 250 characters. Nothing was stored."}
	errUploadText        = &httpError{Code: http.StatusBadRequest, Msg: "Photo not saved: a field is not valid UTF-8 or contains a NUL character. Nothing was stored."}
	errUploadCSRF        = &httpError{Code: http.StatusForbidden, Msg: "Form expired. Reload and try again. Nothing was stored."}
	errUploadTimeout     = &httpError{Code: http.StatusRequestTimeout, Msg: "Photo not saved: the upload did not finish within 90 seconds. Nothing was stored. Try again, or try a smaller picture."}
	errUploadSignedOut   = &httpError{Code: http.StatusUnauthorized, Msg: "Photo not saved: your session has ended. Sign in again, then upload the photo again. Nothing was stored."}
)

// upload is one request to an upload route.
type upload struct {
	a     *App
	w     http.ResponseWriter
	r     *http.Request
	route uploadRoute
	token string // the request's token (session cookie, or the anonymous one)
	user  *User
	back  string // where the refusal page links back to
}

// serveUpload handles POST to an upload route; ServeHTTP calls it before reading any of the body. session is the
// session cookie's value ("" when none), token the request's token (sessionToken).
func (a *App) serveUpload(w http.ResponseWriter, r *http.Request, route uploadRoute, session, token string) {
	u := &upload{a: a, w: w, r: r, route: route, token: token, back: "/account#avatar"}
	if route.kind == uploadListing {
		u.back = "/vendor-dashboard"
	}
	if msg := postOriginError(r); msg != "" {
		u.refuse(http.StatusForbidden, msg)
		return
	}
	switch {
	case len(r.TransferEncoding) > 0 || r.ContentLength < 0 || r.Header.Get("Content-Length") == "":
		u.render(http.StatusLengthRequired, "Photo not saved: the upload did not say how large it is (no Content-Length). Nothing was stored.")
		return
	case r.ContentLength > uploadCeiling:
		u.render(http.StatusRequestEntityTooLarge, errImageTooLarge.Msg)
		return
	}
	if a.preview {
		u.refuse(http.StatusForbidden, "Read-only preview. Start with PostgreSQL to save changes.")
		return
	}
	if session != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		done, ok := takeLookupSlot(ctx)
		if !ok {
			cancel()
			u.refuse(http.StatusServiceUnavailable, errBusyMsg)
			return
		}
		user, err := a.sessionUser(ctx, session)
		done()
		cancel()
		if err != nil {
			u.drain()
			u.serverError(http.StatusServiceUnavailable, errorCause(err))
			return
		}
		u.user = user
	}
	if u.user == nil {
		u.refuse(errUploadSignedOut.Code, errUploadSignedOut.Msg)
		return
	}
	if !slices.Contains(route.roles, u.user.Role) {
		msg := "Photo not saved: only vendors and administrators can add listing photos."
		if route.kind == uploadAvatar {
			msg = "Avatar not saved: avatars are for vendors and market staff."
		}
		u.refuse(http.StatusForbidden, msg)
		return
	}
	if !a.allow("write:"+u.user.ID, 100) {
		u.refuse(http.StatusTooManyRequests, "Too many changes. Try again later. Nothing was stored.")
		return
	}
	if !a.allow("upload:"+u.user.ID, uploadBudget) {
		u.refuse(http.StatusTooManyRequests, "Photo not saved: an account can upload at most 20 photos or avatars in 10 minutes. Nothing was stored. Try again later.")
		return
	}
	release, ok := tryTake(userPostWork)
	if !ok {
		u.refuse(http.StatusServiceUnavailable, errBusyMsg)
		return
	}
	defer release()
	releaseUpload, ok := tryTake(uploadWork)
	if !ok {
		u.refuse(http.StatusServiceUnavailable, "Photo not saved: the market is processing other uploads. Nothing was stored. Try again in a few seconds.")
		return
	}
	defer releaseUpload()
	done, ok := a.enterOneAtATime("upload", u.user.ID)
	if !ok {
		w.Header().Set("Retry-After", busyRetryAfter)
		u.refuse(http.StatusTooManyRequests, "Photo not saved: another upload from your account is still in progress. Wait for it to finish, then reload the page to see its result.")
		return
	}
	defer done()
	// Admitted: only now may the body take longer than the server's ReadTimeout, and the answer be written after
	// its WriteTimeout (both counted from the request's arrival). An httptest recorder supports neither.
	rc := http.NewResponseController(w)
	now := time.Now()
	rc.SetReadDeadline(now.Add(uploadReadWindow))
	rc.SetWriteDeadline(now.Add(uploadWriteWindow))
	if st := a.scanner.State(); st.Name != scannerReady {
		u.refuse(http.StatusServiceUnavailable, st.VendorNotice())
		return
	}
	form, err := u.readForm()
	if err != nil {
		var he *httpError
		errors.As(err, &he)
		u.refuse(he.Code, he.Msg)
		return
	}
	u.drain() // anything after the closing boundary
	if route.kind == uploadListing {
		u.back = "/listing-edit?id=" + url.QueryEscape(form.id) + "#photos"
	}
	// The body has arrived: the rest has its own bound. The request's context still ends if the client goes away.
	ctx, cancel := context.WithTimeout(r.Context(), uploadWorkTimeout)
	defer cancel()
	info, err := inspectImage(form.photo)
	if err != nil {
		var he *httpError
		errors.As(err, &he)
		u.render(he.Code, he.Msg)
		return
	}
	scanned, err := a.scanner.Scan(ctx, route.kind, form.photo)
	if err != nil {
		var refusal *scanRefusal
		if !errors.As(err, &refusal) {
			u.serverError(http.StatusServiceUnavailable, errorCause(err))
			return
		}
		if aerr := a.auditScanRefusal(ctx, u.user.ID, err); aerr != nil {
			http5xxLog.Printf("upload audit failed cause=%s", errorCause(aerr))
		}
		if refusal.Status() == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", busyRetryAfter)
		}
		// A flagged file names the staff contact (A-279); unreadable, the line is left out rather than guessed.
		var contact string
		if refusal.Class == scanInfected && a.db != nil {
			var cerr error
			if contact, cerr = staffContactRefusal(ctx, a.db); cerr != nil {
				http5xxLog.Printf("upload staff contact read failed cause=%s", errorCause(cerr))
			}
		}
		u.render(refusal.Status(), refusal.Message(contact))
		return
	}
	form.photo = nil
	img, err := decodeInSlot(ctx, scanned, info, route.kind)
	if err != nil {
		var he *httpError
		if !errors.As(err, &he) {
			u.render(http.StatusServiceUnavailable, "Photo not saved: the market is busy. Nothing was stored. Try again in a few seconds.")
			return
		}
		u.render(he.Code, he.Msg)
		return
	}
	redirect, err := a.storeImage(ctx, r, session, u.user, route, form, img)
	var he *httpError
	switch {
	case err == nil:
		a.scanner.RecordStored()
		notice := "photo-saved" // a one-shot notice, as an action's (A-117, A-287); its section is the fragment
		if route.kind == uploadAvatar {
			notice = "avatar-saved"
		}
		http.Redirect(w, r, a.setNotice(w, notice, redirect), http.StatusSeeOther)
	case errors.As(err, &he):
		u.render(he.Code, he.Msg)
	case dbBusy(err):
		u.serverError(http.StatusServiceUnavailable, errorCause(err))
	default:
		u.serverError(http.StatusInternalServerError, errorCause(err))
	}
}

// decodeInSlot re-encodes under an imageDecodeWork slot, waiting for one while ctx lasts.
func decodeInSlot(ctx context.Context, u scannedUpload, info imageInfo, kind uploadKind) (encodedImage, error) {
	select {
	case imageDecodeWork <- struct{}{}:
	case <-ctx.Done():
		return encodedImage{}, ctx.Err()
	}
	defer func() { <-imageDecodeWork }()
	return reencodeImage(u, info, kind)
}

// uploadForm is what the multipart body carried besides the csrf token.
type uploadForm struct {
	id, description string
	photo           []byte
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// readForm reads the multipart body: csrf first (checked against the request's token), then optionally id and
// description, then photo, and nothing after it.
func (u *upload) readForm() (uploadForm, error) {
	var f uploadForm
	mt, params, err := mime.ParseMediaType(u.r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return f, errUploadForm
	}
	body := &countingReader{r: io.LimitReader(u.r.Body, uploadBodyLimit)}
	mr := multipart.NewReader(body, params["boundary"])
	// failed classifies a read error: the connection's deadline, the byte limit, or a malformed body.
	failed := func(err error) error {
		var ne net.Error
		switch {
		case errors.As(err, &ne) && ne.Timeout():
			return errUploadTimeout
		case body.n >= uploadBodyLimit:
			return errImageTooLarge
		}
		return errUploadForm
	}
	p, err := mr.NextRawPart()
	if err != nil {
		return f, failed(err)
	}
	if p.FormName() != "csrf" {
		return f, errUploadCSRF
	}
	sent, err := readSmallPart(p, csrfPartLimit)
	if err != nil {
		return f, failed(err)
	}
	if subtle.ConstantTimeCompare(sent, []byte(u.a.csrf(u.token))) != 1 {
		return f, errUploadCSRF
	}
	for seen := ""; ; {
		if p, err = mr.NextRawPart(); err != nil {
			if err == io.EOF {
				return f, errUploadNoPhoto
			}
			return f, failed(err)
		}
		name := p.FormName()
		switch {
		case name == "id" && seen == "":
			v, err := readSmallPart(p, idPartLimit)
			if err != nil {
				return f, failed(err)
			}
			if f.id = string(v); !formTextValid(url.Values{"id": {f.id}}) {
				return f, errUploadText
			}
		case name == "description" && (seen == "" || seen == "id"):
			v, err := readSmallPart(p, 4*descriptionLimit)
			if err != nil {
				return f, failed(err)
			}
			d := strings.TrimSpace(string(v))
			if !formTextValid(url.Values{"description": {d}}) {
				return f, errUploadText
			}
			if utf8.RuneCountInString(d) > descriptionLimit {
				return f, errUploadDescription
			}
			f.description = d
		case name == "photo":
			buf := bytes.NewBuffer(make([]byte, 0, min(u.r.ContentLength, uploadCap+1)+bytes.MinRead))
			n, err := buf.ReadFrom(io.LimitReader(p, uploadCap+1))
			switch {
			case err != nil:
				return f, failed(err)
			case n > uploadCap:
				return f, errImageTooLarge
			case n == 0:
				return f, errUploadNoPhoto
			}
			f.photo = buf.Bytes()
			if _, err = mr.NextRawPart(); err != io.EOF {
				if err == nil {
					return f, errUploadForm // a part after the photo
				}
				return f, failed(err)
			}
			return f, nil
		default:
			return f, errUploadForm
		}
		seen = name
	}
}

// readSmallPart reads a form field of at most limit bytes; a longer one is malformed.
func readSmallPart(p *multipart.Part, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(p, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = errUploadForm
	}
	return b, err
}

// drain reads what is left of the body into io.Discard, at most uploadCeiling bytes, under the read deadline
// in force, so a browser still sending gets the answer instead of a connection reset.
func (u *upload) drain() {
	io.CopyN(io.Discard, u.r.Body, uploadCeiling)
}

// refuse drains the body, then answers with the refusal page.
func (u *upload) refuse(code int, msg string) {
	u.drain()
	u.render(code, msg)
}

// render answers with the refusal page: the sign-in page when the sender is not (or no longer) signed in,
// else the upload refusal page linking back to where the form was.
func (u *upload) render(code int, msg string) {
	if code == http.StatusUnauthorized {
		d := u.a.pageData(u.r, "login", u.token, nil)
		d.Title, d.Error = "Sign in", msg
		u.a.loadRefusalPage(u.r, &d)
		u.a.renderStatus(u.w, u.r, d, code)
		return
	}
	title, label := "Photo not saved", "Back to the listing"
	switch {
	case u.route.kind == uploadAvatar:
		title, label = "Avatar not saved", "Back to your account"
	case u.back == "/vendor-dashboard":
		label = "Back to the vendor desk"
	}
	u.a.renderUploadPage(u.w, u.r, u.user, u.token, code, title, msg, u.back, label)
}

// serverError logs the "http 5xx" line with a reference (serverError's format, A-141) and renders the refusal
// page carrying the reference.
func (u *upload) serverError(code int, cause string) {
	ref := randomToken()[:8]
	http5xxLog.Printf("http 5xx kind=action name=%s status=%d ref=%s cause=%s", u.r.URL.Path, code, ref, cause)
	msg := "Photo not saved: the market could not store it. Nothing was stored. Reference: " + ref
	if code == http.StatusServiceUnavailable {
		u.w.Header().Set("Retry-After", busyRetryAfter)
		msg = "Photo not saved: the market is busy. Nothing was stored. Try again in a few seconds. Reference: " + ref
	}
	u.render(code, msg)
}

// renderUploadPage answers with web/templates/partials/upload-error.html: msg in its error alert and a link back.
func (a *App) renderUploadPage(w http.ResponseWriter, r *http.Request, user *User, token string, code int, title, msg, back, backLabel string) {
	d := a.pageData(r, "upload-error", token, user)
	d.Title, d.Error, d.UploadBack, d.UploadBackLabel = title, msg, back, backLabel
	a.loadRefusalPage(r, &d)
	a.renderStatus(w, r, d, code)
}

// loadRefusalPage loads a refusal page's settings and shared data like a page view does: under a pageWork slot,
// so a flood of refused posts reads the database no more than a flood of pages. With every slot taken, or no
// database, the page is rendered without them.
func (a *App) loadRefusalPage(r *http.Request, d *PageData) {
	if a.db == nil {
		return
	}
	release, ok := tryTake(pageWork)
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	a.load(r.WithContext(ctx), d) // best effort: the refusal is the point
}

// refuseMultipartElsewhere answers a multipart post to any other route without reading its body.
func (a *App) refuseMultipartElsewhere(w http.ResponseWriter, r *http.Request, token string) {
	a.renderUploadPage(w, r, nil, token, http.StatusUnsupportedMediaType, "Form not accepted", "This form cannot take files. Nothing was changed.", "", "")
}

// sessionUser returns the signed-in user of session, or nil when it is not a live session of an account that
// is not suspended (a suspended account's sessions never authenticate; suspension also deletes them).
func (a *App) sessionUser(ctx context.Context, session string) (*User, error) {
	u := &User{}
	err := a.db.QueryRowContext(ctx, `SELECT u.id,u.handle,u.role,u.pgp,u.xmpp FROM users u JOIN sessions s ON s.user_id=u.id WHERE s.token_hash=$1 AND s.expires>now() AND u.suspended_at IS NULL`, digest(session)).Scan(&u.ID, &u.Handle, &u.Role, &u.PGP, &u.XMPP)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// storeImage stores a re-encoded upload in one transaction and returns where to redirect. It re-checks what may
// have changed while the body arrived and the scan ran: the session still exists and the actor still has the
// route's role; the listing (row-locked by lockListing) is the actor's, or the actor is an administrator, and is
// neither archived nor taken down. Under the images advisory lock, which one image insert holds at a time, it
// checks the quotas in SQL: 4 photos a listing, 50 MB an owner (full + thumbnail bytes), IMAGE_STORAGE_LIMIT_MB
// in all. An avatar replaces the owner's earlier one (deleted in the same transaction). A photo belongs to the
// listing's vendor, whoever added it.
//
// Lock order: the users rows of the actor and the listing's vendor (FOR KEY SHARE, which the inserts' foreign
// keys take anyway), then the listing, then the advisory lock. An administrator's role change, suspension or
// takedown locks the users row and then the vendor's listings, so taking the listing first and the users rows
// after (the foreign keys would) could deadlock with it (TestUploadStoreDoesNotDeadlockWithRoleChange). Taking
// the users rows first also makes the role re-check wait for such a change and see its result.
func (a *App) storeImage(ctx context.Context, r *http.Request, session string, actor *User, route uploadRoute, f uploadForm, img encodedImage) (string, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	// Row locks may wait as long as an administrator's change that waits on a checkout's listing (actions_admin.go).
	if err = waitForWalletLocks(ctx, tx); err != nil {
		return "", err
	}
	var role string
	err = tx.QueryRowContext(ctx, `SELECT u.role FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND u.id=$2 AND s.expires>now() AND u.suspended_at IS NULL FOR KEY SHARE OF u`, digest(session), actor.ID).Scan(&role)
	if err == sql.ErrNoRows {
		return "", errUploadSignedOut
	}
	if err != nil {
		return "", err
	}
	// A staff avatar was admitted under the staff privacy rules (hidden unless listed); if the account is no
	// longer staff it would be published as a vendor's avatar, so it is refused too (A-208(a),
	// TestStaffAvatarRefusedAfterScanDemotion).
	staff := func(r string) bool { return r == "moderator" || r == "admin" }
	if !slices.Contains(route.roles, role) || (route.kind == uploadAvatar && staff(actor.Role) && !staff(role)) {
		return "", &httpError{Code: http.StatusForbidden, Msg: "Photo not saved: your account's role changed while it was uploading. Nothing was stored."}
	}
	current := *actor
	current.Role = role
	owner, audit, ownerAudit, notify, redirect := current.ID, "", "", "", "/account"
	var product any
	if route.kind == uploadListing {
		// lockListing takes the listing's vendor's users row before the listing (the vendor never changes).
		c := &actionCtx{A: a, R: r.WithContext(ctx), Tx: tx, User: &current}
		l, err := lockListing(c, f.id)
		if err != nil {
			var he *httpError
			if errors.As(err, &he) {
				return "", &httpError{Code: he.Code, Msg: "Photo not saved: listing not found. Nothing was stored."}
			}
			return "", err
		}
		switch {
		case l.Takedown != "":
			return "", &httpError{Code: http.StatusConflict, Msg: "Photo not saved: an administrator took this listing down, so photos cannot be added to it. Nothing was stored."}
		case l.Archived:
			return "", &httpError{Code: http.StatusConflict, Msg: "Photo not saved: this listing is archived. Restore it before adding photos. Nothing was stored."}
		}
		owner, product = l.VendorID, l.ID
		audit = listingAudit("Added a photo to", c, l)
		redirect = "/listing-edit?id=" + url.QueryEscape(l.ID)
		if owner != current.ID { // A-190: recorded in the vendor's own audit trail too
			notify = "An administrator added a photo to your listing " + shortID(l.ID) + "."
			ownerAudit = "Photo added to listing " + shortID(l.ID) + " by an administrator"
		}
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, imagesLock); err != nil {
		return "", err
	}
	if route.kind == uploadAvatar {
		res, err := tx.ExecContext(ctx, `DELETE FROM images WHERE owner_user_id=$1 AND kind='avatar'`, owner)
		if err != nil {
			return "", err
		}
		audit = "Added avatar"
		if n, _ := res.RowsAffected(); n > 0 {
			audit = "Replaced avatar"
		}
	}
	size := int64(len(img.full) + len(img.thumb))
	var photos, ownerBytes, allBytes int64
	err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM images WHERE product_id=$1),
 (SELECT coalesce(sum(octet_length("full")+octet_length(thumb)),0) FROM images WHERE owner_user_id=$2),
 (SELECT coalesce(sum(octet_length("full")+octet_length(thumb)),0) FROM images)`, product, owner).Scan(&photos, &ownerBytes, &allBytes)
	if err != nil {
		return "", err
	}
	switch {
	case route.kind == uploadListing && photos >= photosPerListing:
		return "", &httpError{Code: http.StatusConflict, Msg: "Photo not saved: this listing already has 4 photos, the most allowed. Remove one first. Nothing was stored."}
	case ownerBytes+size > uploaderQuota:
		return "", &httpError{Code: http.StatusConflict, Msg: fmt.Sprintf("Photo not saved: the photos and avatar of this account would use more than %d MB, the most stored per account. Remove a photo first. Nothing was stored.", uploaderQuota>>20)}
	case allBytes+size > a.imageLimit():
		return "", &httpError{Code: http.StatusConflict, Msg: "Photo storage on this market is full; contact the operator. Nothing was stored."}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO images(id,owner_user_id,product_id,kind,"full",thumb,width,height,description) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		randomToken()[:32], owner, product, map[uploadKind]string{uploadListing: "photo", uploadAvatar: "avatar"}[route.kind], img.full, img.thumb, img.width, img.height, imageDescription(route, f)); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return "", &httpError{Code: http.StatusConflict, Msg: "Avatar not saved: another avatar upload finished first. Reload the page to see it. Nothing was stored."}
		}
		return "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", current.ID, audit); err != nil {
		return "", err
	}
	if ownerAudit != "" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", owner, ownerAudit); err != nil {
			return "", err
		}
	}
	if notify != "" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), owner, notify); err != nil {
			return "", err
		}
	}
	return redirect, tx.Commit()
}

// imageDescription is the stored description: a photo's, read aloud to blind buyers; an avatar has none (it is
// shown with empty alt text, and nothing should be kept that nothing shows).
func imageDescription(route uploadRoute, f uploadForm) string {
	if route.kind == uploadAvatar {
		return ""
	}
	return f.description
}

// ImageStorageView is the admin image scanner panel's storage line (A-185's panel, A-186's table): how many
// photos and avatars are stored, the megabytes they use and IMAGE_STORAGE_LIMIT_MB.
type ImageStorageView struct {
	Count       int64
	Used, Limit string // megabytes, "12.3" and "2048"
}

func init() {
	registerLoader("admin", func(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
		var n, used int64
		if err := a.db.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(octet_length("full")+octet_length(thumb)),0) FROM images`).Scan(&n, &used); err != nil {
			return err
		}
		d.ImageStorage = &ImageStorageView{Count: n, Used: megabytes(used), Limit: strconv.FormatInt(a.imageLimit()>>20, 10)}
		return nil
	})
	registerPreview("admin", func(d *PageData) {
		d.ImageStorage = &ImageStorageView{Count: 3, Used: "0.4", Limit: strconv.Itoa(defaultImageStorageMB)}
	})
}

// megabytes is n bytes in MB (2^20) to one decimal.
func megabytes(n int64) string { return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) }
