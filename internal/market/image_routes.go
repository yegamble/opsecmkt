package market

import (
	"database/sql"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// A-187 (war room 2026-09-27, rulings D7 and D8, owner decision 1): the two routes that serve stored photos and
// avatars (images table, migration 063). No other query reads their bytes (TestOnlyImageRoutesSelectImageBytes):
// pages list image ids and link here.
//
//   - GET/HEAD /img/<id>-t|-f, the public route: ServeHTTP branches to it before any cookie or session code, so
//     it reads no session and sets no cookie, and it takes an imageWork slot (admitClass) instead of a page slot.
//     One primary-key read joined to the public predicate (imagePublicWhere).
//   - GET/HEAD /desk/img/<id>-t|-f, the authenticated route, for the owner's own images whatever their state
//     (an archived listing's on listing-edit), any image for an administrator, and a staff member's avatar for
//     the buyer, vendor and staff of an order on which that member recorded the dispute decision (owner
//     decision 1(c)). It takes a page slot and looks the session cookie up like a page does (under that slot),
//     and it too sets no cookie.
//
// <id> is the 32 lowercase hex characters of the row's id (128 random bits, storeImage); -t is the thumbnail and
// -f the full image. Anything else, and any image the request may not see, gets http.NotFound: the same status,
// headers and body as an unknown route, so an id cannot be probed (TestImageUnknownAndHiddenIdentical).

// imageWork admits 16 concurrent image requests on the public route. Its own class: a catalog page of 25
// thumbnails fills it rather than the page class, and a flood of image requests cannot refuse pages.
var imageWork = make(chan struct{}, 16)

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
// account pages, and a staff avatar beside a dispute decision on the order page.
func deskImageURL(id string, full bool) string { return "/desk/img/" + id + imageSuffix(full) }

func imageSuffix(full bool) string {
	if full {
		return "-f"
	}
	return "-t"
}

// imagePublicFrom and imagePublicWhere are the public visibility predicate over images i, their owner o and a
// photo's listing p. A photo: the listing is published (not archived, not taken down) and its vendor is still a
// vendor (or an administrator, whose own listings are public too) and not suspended. An avatar: a vendor's
// that is not suspended; a moderator's or administrator's only while the "public staff list" setting is on and
// that account has not hidden itself (owner decision 1(d); both are off/hidden by default, and any other value
// or a missing setting counts as off). A page showing an image on the public route selects its id with this
// predicate, so it never links an image that route would refuse.
const (
	imagePublicFrom  = `images i JOIN users o ON o.id=i.owner_user_id LEFT JOIN products p ON p.id=i.product_id`
	imagePublicWhere = `o.suspended_at IS NULL AND (
 (i.kind='photo' AND p.vendor_id=i.owner_user_id AND NOT p.archived AND p.admin_archive_reason IS NULL AND o.role IN ('vendor','admin'))
 OR (i.kind='avatar' AND (o.role='vendor' OR (o.role IN ('admin','moderator') AND NOT o.staff_hidden
   AND EXISTS(SELECT 1 FROM settings WHERE key='staff_list_public' AND value='true')))))`
)

// imageDeskWhere is the authenticated route's predicate over images i and owner o for viewer $2 with role $3:
// the owner; an administrator; or, for the avatar of a moderator or administrator who is not suspended, the
// buyer or vendor of an order on which that member moved the dispute to resolved, or a moderator (who may open
// every resolved order, orders_load.go). Everyone else, moderators included for vendors' images, is refused.
const imageDeskWhere = `(i.owner_user_id=$2 OR $3='admin'
 OR (i.kind='avatar' AND o.role IN ('admin','moderator') AND o.suspended_at IS NULL AND EXISTS(
   SELECT 1 FROM orders od JOIN products pr ON pr.id=od.product_id
   WHERE od.state='resolved' AND (od.buyer_id=$2 OR pr.vendor_id=$2 OR $3='moderator')
     AND EXISTS(SELECT 1 FROM order_events e WHERE e.order_id=od.id AND e.from_state='disputed' AND e.to_state='resolved' AND e.actor_id=i.owner_user_id))))`

// The two columns an image route selects; only these queries read image bytes.
const (
	imageThumbSQL = `SELECT i.thumb FROM `
	imageFullSQL  = `SELECT i."full" FROM `
)

func imageColumn(full bool) string {
	if full {
		return imageFullSQL
	}
	return imageThumbSQL
}

// servePublicImage answers GET/HEAD /img/<rest>. ServeHTTP calls it before reading any cookie.
func (a *App) servePublicImage(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method == "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	ref, ok := parseImagePath(rest)
	if !ok || a.db == nil {
		http.NotFound(w, r)
		return
	}
	var b []byte
	err := a.db.QueryRowContext(r.Context(), imageColumn(ref.full)+imagePublicFrom+` WHERE i.id=$1 AND `+imagePublicWhere, ref.id).Scan(&b)
	a.writeImage(w, r, b, err)
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
		serverError(w, r, 503, "Service unavailable", errorCause(err))
		return
	}
	if u == nil {
		http.NotFound(w, r)
		return
	}
	var b []byte
	err = a.db.QueryRowContext(r.Context(), imageColumn(ref.full)+`images i JOIN users o ON o.id=i.owner_user_id WHERE i.id=$1 AND `+imageDeskWhere, ref.id, u.ID, u.Role).Scan(&b)
	a.writeImage(w, r, b, err)
}

// writeImage answers with image bytes b under D7's header set exactly (the page headers ServeHTTP set are
// replaced), or the unknown-route 404 when there is no row, or 503 on a database error.
func (a *App) writeImage(w http.ResponseWriter, r *http.Request, b []byte, err error) {
	switch {
	case err == sql.ErrNoRows:
		http.NotFound(w, r)
		return
	case dbBusy(err):
		serverError(w, r, 503, errBusyMsg, errorCause(err))
		return
	case err != nil:
		serverError(w, r, 503, "Service unavailable", errorCause(err))
		return
	}
	h := w.Header()
	for _, k := range []string{"Referrer-Policy", "X-Frame-Options", "Permissions-Policy"} {
		h.Del(k)
	}
	h.Set("Content-Type", "image/jpeg")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// D7: a day in the browser's private cache, not revalidated (no validators: an id's bytes never change).
	h.Set("Cache-Control", "private, max-age=86400, immutable")
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
