package market

import (
	"database/sql"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// A-187 (war room 2026-09-27, rulings D7 and D8, owner decision 1): the two routes that serve stored photos and
// avatars (images table, migration 063). No other query reads their bytes (TestOnlyImageRoutesSelectImageBytes):
// pages list image ids and link here.
//
//   - GET/HEAD /img/<id>-t|-f, the public route: ServeHTTP branches to it before any cookie or session code, so
//     it reads no session in public mode and sets no cookie. Members-only mode verifies a live session first.
//     It takes an imageWork slot (admitClass) instead of a page slot.
//     One primary-key read joined to the public predicate (imagePublicWhere).
//   - GET/HEAD /desk/img/<id>-t|-f, the authenticated route, for the owner's own images whatever their state
//     (an archived listing's on listing-edit), any image for an administrator, and a staff member's avatar for
//     the buyer, vendor and staff of an order on which that member recorded the dispute decision (owner
//     decision 1(c)), and an archived listing's photos for whoever may read an order on it (A-264). It takes a
//     page slot and looks the session cookie up like a page does (under that slot),
//     and it too sets no cookie.
//
// <id> is the 32 lowercase hex characters of the row's id (128 random bits, storeImage); -t is the thumbnail and
// -f the full image. A malformed path, and any image the request may not see, gets the same 404 as an unknown
// image id (status, headers and body; no cookie on either route), so an id cannot be probed
// (TestImageUnknownAndHiddenIdentical); a POST gets 405. A GET also reserves the image's bytes from its route's
// budget (imageBytes, deskImageBytes) until they are written (serveImage).

// imageWork admits 16 concurrent image requests on the public route. Its own class: a catalog page of 25
// thumbnails fills it rather than the page class, and a flood of image requests cannot refuse pages.
var imageWork = make(chan struct{}, 16)

// imageBytes bounds the image bytes that responses hold from their database read until their Write returns
// (PR #23 review R-1). The image slot is released when a response starts, so without it every client that never
// reads would pin up to 2 MiB until the server's write timeout, bounded only by the connection cap.
var imageBytes = &byteBudget{limit: 64 << 20}

// deskImageBytes is the same bound for /desk/img, kept apart so that a signed-in account (any role, and anyone
// can register) flooding the desk route with unknown ids, each reserving 2 MiB before its query under the 64
// page slots, cannot refuse public images (PR #23 re-check N-1; TestDeskImageFloodLeavesPublicImages).
var deskImageBytes = &byteBudget{limit: 32 << 20}

// byteBudget counts bytes reserved against a limit. It never waits: a reservation that does not fit is refused.
type byteBudget struct {
	mu          sync.Mutex
	used, limit int64
}

// take reserves n bytes if they fit.
func (b *byteBudget) take(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit {
		return false
	}
	b.used += n
	return true
}

// give returns n reserved bytes.
func (b *byteBudget) give(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
}

// inUse is the number of bytes reserved.
func (b *byteBudget) inUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

var imagePathForm = regexp.MustCompile(`^([0-9a-f]{32})-([tf])$`)

// imageRef is a parsed image path.
type imageRef struct {
	id   string
	full bool
}

// parseImagePath parses what follows /img/ or /desk/img/.
func parseImagePath(rest string) (imageRef, bool) {
	m := imagePathForm.FindStringSubmatch(rest)
	if m == nil {
		return imageRef{}, false
	}
	return imageRef{id: m[1], full: m[2] == "f"}, true
}

// publicImageURL is the relative URL of an image on the public route: pages render it in <img src> for images
// that pass imagePublicWhere (catalog cards, product and vendor pages, the public staff list).
func publicImageURL(id string, full bool) string { return "/img/" + id + imageSuffix(full) }

// deskImageURL is the relative URL of an image on the authenticated route: the owner's listing-edit and
// account pages, a staff avatar beside a dispute decision on the order page, and an archived listing's photos on
// its product page (A-264).
func deskImageURL(id string, full bool) string { return "/desk/img/" + id + imageSuffix(full) }

func imageSuffix(full bool) string {
	if full {
		return "-f"
	}
	return "-t"
}

// imagePublicFrom and imagePublicWhere are the public visibility predicate over images i, their owner o and a
// photo's listing p. A photo: the listing is published (not archived, not taken down, approved in listing review:
// MOD72-6, listing_review.go) and its vendor is still a vendor (or an administrator, whose own listings are public
// too) and not suspended or deactivated. An avatar: a vendor's that is not suspended or deactivated; a moderator's
// or administrator's only while the "public staff list" setting is on and
// that account has not hidden itself (owner decision 1(d); both are off/hidden by default, and any other value
// or a missing setting counts as off). A page showing an image on the public route selects its id with this
// predicate, so it never links an image that route would refuse.
const (
	imagePublicFrom  = `images i JOIN users o ON o.id=i.owner_user_id LEFT JOIN products p ON p.id=i.product_id`
	imagePublicWhere = `o.suspended_at IS NULL AND (
 (i.kind='photo' AND p.vendor_id=i.owner_user_id AND NOT p.archived AND NOT o.vendor_deactivated AND p.admin_archive_reason IS NULL AND p.review_state='approved' AND o.role IN ('vendor','admin'))
 OR (i.kind='avatar' AND ((o.role='vendor' AND NOT o.vendor_deactivated) OR (o.role IN ('admin','moderator') AND NOT o.staff_hidden
   AND EXISTS(SELECT 1 FROM settings WHERE key='staff_list_public' AND value='true')))))`
)

// imageDeskWhere is the authenticated route's predicate over images i and owner o for viewer $2 with role $3:
// the owner; an administrator; for the avatar of a moderator or administrator who is not suspended, the
// buyer or vendor of an order on which that member moved the dispute to resolved, or a moderator (who may open
// every resolved order, orders_load.go); or, for a photo of an archived or deactivated-vendor listing, whoever may
// read an order placed on that listing (orderReadable, the rule that opens the archived listing's product page, A-164 and A-264).
// MOD72-6: a listing not approved for buyers (pending or rejected) is treated like an archived one for those order
// readers (non-draft orders only), and a moderator also sees its photos while it is pending and not archived, to
// review it (listing_review.go).
// Everyone else, moderators included for other vendors' images, is refused. Inside the last EXISTS, o and p are
// the order and its listing (orderReadable's aliases), not the image's owner.
var imageDeskWhere = `(i.owner_user_id=$2 OR $3='admin'
 OR (i.kind='avatar' AND o.role IN ('admin','moderator') AND o.suspended_at IS NULL AND EXISTS(
   SELECT 1 FROM orders od JOIN products pr ON pr.id=od.product_id
   WHERE od.state='resolved' AND (od.buyer_id=$2 OR pr.vendor_id=$2 OR $3='moderator')
     AND EXISTS(SELECT 1 FROM order_events e WHERE e.order_id=od.id AND e.from_state='disputed' AND e.to_state='resolved' AND e.actor_id=i.owner_user_id)))
 OR (i.kind='photo' AND $3='moderator' AND EXISTS(SELECT 1 FROM products pp
   WHERE pp.id=i.product_id AND pp.review_state='pending' AND NOT pp.archived AND pp.vendor_id=i.owner_user_id))
 OR (i.kind='photo' AND EXISTS(SELECT 1 FROM products p JOIN orders o ON o.product_id=p.id JOIN users v ON v.id=p.vendor_id
   WHERE p.id=i.product_id AND (p.archived OR v.vendor_deactivated OR (p.review_state<>'approved' AND o.state<>'draft')) AND p.vendor_id=i.owner_user_id AND ` + orderReadable("$2", "$3") + `)))`

// imageSelect starts an image route's query: the length of one size of the image and, for a GET, its bytes (a
// HEAD reads only the length). The only way a query reads image bytes; no other file may use it
// (TestOnlyImageRoutesSelectImageBytes).
func imageSelect(full, head bool) string {
	col := "i.thumb"
	if full {
		col = `i."full"`
	}
	if head {
		return "SELECT octet_length(" + col + "),NULL::bytea FROM "
	}
	return "SELECT octet_length(" + col + ")," + col + " FROM "
}

// servePublicImage reads a session only when the current site policy requires membership.
func (a *App) servePublicImage(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method == "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	ref, ok := parseImagePath(rest)
	if ok && a.preview { // no database: the preview's generated sample photos (image_pages.go)
		servePreviewImage(w, r, ref)
		return
	}
	if !ok || a.db == nil {
		http.NotFound(w, r)
		return
	}
	policy, err := a.readSiteAccess(r.Context())
	if err != nil {
		writeDBFailure(w, r, err)
		return
	}
	r = accessRequest(r, policy)
	if policy.MembersOnly {
		cookie, err := r.Cookie("session")
		if err != nil || len(cookie.Value) != 64 {
			http.NotFound(w, r)
			return
		}
		user, err := a.sessionUser(r.Context(), cookie.Value)
		if err != nil {
			writeDBFailure(w, r, err)
			return
		}
		if user == nil || a.publicAdministrator(r.Context(), user) {
			http.NotFound(w, r)
			return
		}
	}
	a.serveImage(w, r, imageBytes, ref, imagePublicFrom+` WHERE i.id=$1 AND `+imagePublicWhere, ref.id)
}

// serveDeskImage answers GET/HEAD /desk/img/<rest> for the session in the request's session cookie. It reads no
// other cookie and sets none.
func (a *App) serveDeskImage(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method == "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	ref, ok := parseImagePath(rest)
	c, err := r.Cookie("session")
	if !ok || a.db == nil || err != nil || len(c.Value) != 64 {
		http.NotFound(w, r)
		return
	}
	u, err := a.sessionUser(r.Context(), c.Value)
	if err != nil {
		writeDBFailure(w, r, err)
		return
	}
	if u == nil || a.publicAdministrator(r.Context(), u) {
		http.NotFound(w, r)
		return
	}
	policy, err := a.readSiteAccess(r.Context())
	if err != nil {
		writeDBFailure(w, r, err)
		return
	}
	r = accessRequest(r, policy)
	a.serveImage(w, r, deskImageBytes, ref, `images i JOIN users o ON o.id=i.owner_user_id WHERE i.id=$1 AND `+imageDeskWhere, ref.id, u.ID, u.Role)
}

// serveImage runs an image route's query (from: what follows imageSelect) and answers with the image. A GET
// first reserves the largest stored size of what it asked for from budget, without waiting (the busy 503
// with Retry-After when it does not fit, before any query, so the answer is the same for every id), keeps what
// the image needs once read until its Write returns, and gives it all back however the handler ends (a client
// that went away, a panic). A HEAD reads only the length and reserves nothing.
func (a *App) serveImage(w http.ResponseWriter, r *http.Request, budget *byteBudget, ref imageRef, from string, args ...any) {
	head := r.Method == "HEAD"
	var reserved int64
	if !head {
		reserved = maxThumbBytes // the largest stored sizes (images.go, migration 063's CHECKs)
		if ref.full {
			reserved = maxFullBytes
		}
		if !budget.take(reserved) {
			busy(w)
			return
		}
		defer func() { budget.give(reserved) }()
	}
	var n int64
	var b []byte
	err := a.db.QueryRowContext(r.Context(), imageSelect(ref.full, head)+from, args...).Scan(&n, &b)
	if err == nil && !head {
		budget.give(reserved - int64(len(b)))
		reserved = int64(len(b))
	}
	writeImage(w, r, n, b, err)
}

// writeImage answers with an image of n bytes b (nil for HEAD) under D7's header set exactly (the page headers
// ServeHTTP set are replaced), or the unknown-id 404 when there is no row, or 503 on a database error.
func writeImage(w http.ResponseWriter, r *http.Request, n int64, b []byte, err error) {
	switch {
	case err == sql.ErrNoRows:
		http.NotFound(w, r)
		return
	case dbBusyForContext(r.Context(), err):
		serverError(w, r, 503, errBusyMsg, errorCause(err))
		return
	case err != nil:
		writeDBFailure(w, r, err)
		return
	}
	h := w.Header()
	for _, k := range []string{"Referrer-Policy", "X-Frame-Options", "Permissions-Policy"} {
		h.Del(k)
	}
	h.Set("Content-Type", "image/jpeg")
	h.Set("Content-Length", strconv.FormatInt(n, 10))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// D7: a day in the browser's private cache, not revalidated (no validators: an id's bytes never change).
	h.Set("Cache-Control", "private, max-age=86400, immutable")
	if policy := requestSiteAccess(r); policy != nil && policy.MembersOnly {
		h.Set("Cache-Control", "no-store")
	}
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Content-Disposition", `inline; filename="image.jpg"`)
	w.WriteHeader(http.StatusOK)
	if r.Method != "HEAD" {
		w.Write(b)
	}
}

// imageRoute names an image route for routeOf and admitClass ("" for any other path).
func imageRoute(path string) string {
	switch {
	case strings.HasPrefix(path, "/img/"):
		return "/img/"
	case strings.HasPrefix(path, "/desk/img/"):
		return "/desk/img/"
	}
	return ""
}
