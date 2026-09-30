package market

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// A-188 (war room 2026-09-27; owner decision 1): the forms around the upload routes (uploads.go). The listing-edit
// page's Photos panel (#photos) lists the listing's stored photos from the database, each with its own remove
// form, and holds the photo upload form; /account#avatar shows the account's avatar with a remove form and holds
// the avatar upload form (vendors, administrators and moderators); /account#staff-list is a moderator's or
// administrator's "Hide me from the public staff list" choice. The upload forms follow the scanner's cached state
// (clamd.go): Ready shows them, Unavailable and stale show them disabled with the paused copy, Disabled shows only
// the "not set up" line. Removing is always possible: it needs no scan. Every form but the two uploads is
// urlencoded. Images are shown through the authenticated route (deskImageURL), never read here.

func init() {
	registerLoader("listing-edit", loadPhotoPanel)
	registerLoader("account", loadAvatarPanel)
	registerAction("/listings/photo/remove", actionSpec{Roles: []string{"vendor", "admin"}, Run: photoRemoveAction})
	registerAction("/account/avatar/remove", actionSpec{Run: avatarRemoveAction})
	registerAction("/account/staff-list", actionSpec{Roles: []string{"admin", "moderator"}, Run: staffListChoiceAction})
}

// UploadsView is an upload panel: listing-edit's Photos panel or /account's avatar panel.
type UploadsView struct {
	// The scanner's state for the upload form: Ready shows it; Paused (Unavailable, or signatures stale) shows
	// it inside a disabled fieldset with Notice; Off (no CLAMD_ADDRESS) shows only Notice, no file input.
	Paused, Off bool
	Notice      string
	// Listing-edit: the listing's photos, oldest first; how many of photosPerListing; the megabytes the listing's
	// vendor's photos and avatar use against uploaderQuota; and why no photo can be added ("" when one can).
	Photos      []StoredImage
	PhotoCount  int
	PhotoLimit  int
	Used, Quota string
	Blocked     string
	// Listing-edit: staff takedowns of the listing's photos with their reasons, newest first (A-190, staffRemovals).
	StaffRemovals []StaffRemoval
	// Account: the viewer's avatar (nil when none); whether the viewer may upload one (vendor, administrator,
	// moderator); whether the viewer is staff (moderator, administrator) and hidden from the public staff list.
	Avatar      *StoredImage
	AvatarForm  bool
	Staff       bool
	StaffHidden bool
}

// StoredImage is one stored photo or avatar as its owner's pages show it: its position, desk-route URLs, size in
// pixels and kilobytes of the full JPEG (octet_length; the bytes are never read here), and description.
type StoredImage struct {
	N               int
	ID, Thumb, Full string
	Width, Height   int
	KB              int64
	Description     string
}

// uploadState fills the scanner part of v from the cached state (never contacting clamd).
func uploadState(v *UploadsView, st scannerState) {
	switch st.Name {
	case scannerDisabled:
		v.Off, v.Notice = true, st.VendorNotice()
	case scannerReady:
	default:
		v.Paused, v.Notice = true, st.VendorNotice()
	}
}

// listingNoticeTail ends the paused copy (VendorNotice); the account page, which has no listing, drops it.
const listingNoticeTail = " Listing details can still be saved."

// quotaMB is n bytes in MB (2^20), rounded up to a tenth so a few kilobytes never read as none: "0", "0.1", "12".
func quotaMB(n int64) string {
	tenths := (n*10 + 1<<20 - 1) >> 20
	return strconv.FormatFloat(float64(tenths)/10, 'f', -1, 64)
}

func storedImage(n int, id string, w, h int, size int64, desc string) StoredImage {
	return StoredImage{N: n, ID: id, Thumb: deskImageURL(id, false), Full: deskImageURL(id, true), Width: w, Height: h, KB: (size + 1023) / 1024, Description: desc}
}

// loadPhotoPanel loads the Photos panel of the listing loadListingEdit loaded (for its vendor or an
// administrator).
func loadPhotoPanel(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.Product == nil {
		return nil
	}
	v := &UploadsView{PhotoLimit: photosPerListing, Quota: strconv.FormatInt(uploaderQuota>>20, 10)}
	uploadState(v, a.scanner.State())
	rows, err := a.db.QueryContext(ctx, `SELECT id,width,height,octet_length("full"),description FROM images WHERE product_id=$1 AND kind='photo' ORDER BY created,id`, d.Product.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, desc string
		var w, h int
		var size int64
		if err := rows.Scan(&id, &w, &h, &size, &desc); err != nil {
			return err
		}
		v.Photos = append(v.Photos, storedImage(len(v.Photos)+1, id, w, h, size, desc))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var used int64
	if err := a.db.QueryRowContext(ctx, `SELECT coalesce(sum(octet_length("full")+octet_length(thumb)),0) FROM images WHERE owner_user_id=$1`, d.Product.VendorID).Scan(&used); err != nil {
		return err
	}
	v.PhotoCount, v.Used = len(v.Photos), quotaMB(used)
	if v.StaffRemovals, err = staffRemovals(ctx, a, d.Product); err != nil {
		return err
	}
	switch {
	case d.Listing != nil && d.Listing.TakedownReason != "":
		v.Blocked = "An administrator took this listing down, so photos cannot be added to it. Its photos can still be removed."
	case d.Product.Archived:
		v.Blocked = "This listing is archived. Restore it before adding photos."
	case v.PhotoCount >= photosPerListing:
		v.Blocked = "This listing has 4 photos, the most allowed. Remove one to add another."
	}
	d.Uploads = v
	return nil
}

// loadAvatarPanel loads /account's avatar panel and, for staff, the public staff list choice. An account that
// may no longer upload an avatar (a demoted vendor) still sees one it has, with its remove form.
func loadAvatarPanel(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	role := d.User.Role
	v := &UploadsView{AvatarForm: slices.Contains(uploadRoutes["/account/avatar"].roles, role), Staff: role == "admin" || role == "moderator"}
	uploadState(v, a.scanner.State())
	v.Notice = strings.TrimSuffix(v.Notice, listingNoticeTail)
	var id sql.NullString
	var w, h, size sql.NullInt64
	err := a.db.QueryRowContext(ctx, `SELECT i.id,i.width,i.height,octet_length(i."full"),u.staff_hidden FROM users u LEFT JOIN images i ON i.owner_user_id=u.id AND i.kind='avatar' WHERE u.id=$1`, d.User.ID).Scan(&id, &w, &h, &size, &v.StaffHidden)
	if err != nil {
		return err
	}
	if id.Valid {
		img := storedImage(1, id.String, int(w.Int64), int(h.Int64), size.Int64, "")
		v.Avatar = &img
	}
	d.Uploads = v
	return nil
}

// photoRemoveAction deletes one of a listing's photos for the listing's vendor or an administrator (the
// lockListing predicate), in any state of the listing. Any other photo, an avatar and an unknown id get the same
// 404. The removal is audited with fixed copy (listingAudit); when an administrator removed it, the vendor's own
// audit trail records it too and the vendor is notified (as storeImage does for an administrator's addition).
func photoRemoveAction(c *actionCtx) (actionResult, error) {
	notFound := fail(404, "Photo not found. Nothing was removed.")
	id := c.Form.Get("id")
	var product string
	err := c.Tx.QueryRowContext(c.Ctx(), `SELECT product_id FROM images WHERE id=$1 AND kind='photo'`, id).Scan(&product)
	if err == sql.ErrNoRows {
		return actionResult{}, notFound
	}
	if err != nil {
		return actionResult{}, err
	}
	l, err := lockListing(c, product)
	var he *httpError
	if errors.As(err, &he) && he.Code == 404 {
		return actionResult{}, notFound
	}
	if err != nil {
		return actionResult{}, err
	}
	res, err := c.Tx.ExecContext(c.Ctx(), `DELETE FROM images WHERE id=$1 AND product_id=$2`, id, l.ID)
	if err != nil {
		return actionResult{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 { // removed meanwhile
		return actionResult{}, notFound
	}
	if l.VendorID != c.User.ID { // A-190: in the vendor's own audit trail too
		if _, err = c.Tx.ExecContext(c.Ctx(), "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", l.VendorID, "Photo removed from listing "+shortID(l.ID)+" by an administrator"); err != nil {
			return actionResult{}, err
		}
		if _, err = c.Tx.ExecContext(c.Ctx(), "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), l.VendorID, "An administrator removed a photo from your listing "+shortID(l.ID)+"."); err != nil {
			return actionResult{}, err
		}
	}
	return actionResult{Redirect: "/listing-edit?id=" + url.QueryEscape(l.ID), Notice: "photo-removed", Audit: listingAudit("Removed a photo from", c, l)}, nil
}

// avatarRemoveAction deletes the signed-in account's own avatar, whatever its role now; the form carries no id.
func avatarRemoveAction(c *actionCtx) (actionResult, error) {
	res, err := c.Tx.ExecContext(c.Ctx(), `DELETE FROM images WHERE owner_user_id=$1 AND kind='avatar'`, c.User.ID)
	if err != nil {
		return actionResult{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return actionResult{}, fail(409, "You have no avatar to remove. Nothing was changed.")
	}
	return actionResult{Redirect: "/account", Notice: "avatar-removed", Audit: "Removed avatar"}, nil
}

// staffListChoiceAction sets a moderator's or administrator's "Hide me from the public staff list" choice
// (users.staff_hidden; owner decision 1(b)): the box ticked (hide=yes) hides, unticked shows. A change is audited
// old→new with fixed copy; saving the same choice changes and records nothing. The role is re-checked under the
// row lock, so an account demoted meanwhile changes nothing.
func staffListChoiceAction(c *actionCtx) (actionResult, error) {
	hide := c.Form.Get("hide") == "yes"
	var role string
	var old bool
	if err := c.Tx.QueryRowContext(c.Ctx(), `SELECT role,staff_hidden FROM users WHERE id=$1 FOR NO KEY UPDATE`, c.User.ID).Scan(&role, &old); err != nil {
		return actionResult{}, err
	}
	if role != "admin" && role != "moderator" {
		return actionResult{}, fail(403, "Only moderators and administrators appear on the staff list. Nothing was changed.")
	}
	res := actionResult{Redirect: "/account", Notice: "my-staff-list"}
	if old == hide {
		return res, nil
	}
	if _, err := c.Tx.ExecContext(c.Ctx(), `UPDATE users SET staff_hidden=$2 WHERE id=$1`, c.User.ID, hide); err != nil {
		return actionResult{}, err
	}
	res.Audit = "Public staff list choice changed: " + staffListWord(old) + " → " + staffListWord(hide)
	return res, nil
}

func staffListWord(hidden bool) string {
	if hidden {
		return "hidden"
	}
	return "shown"
}

// previewUploads fills the read-only preview's upload panels (listing-edit and account): a sample in the Ready
// state with no stored photos, or with ?scanner=off or ?scanner=paused the Disabled or Unavailable state, so the
// preview shows each layout. The preview has no scanner; posting is refused like every preview post.
func previewUploads(d *PageData, scanner string) {
	st := scannerState{Name: scannerReady}
	switch scanner {
	case "off":
		st.Name = scannerDisabled
	case "paused":
		st.Name = scannerUnavailable
	}
	v := &UploadsView{PhotoLimit: photosPerListing, Used: "0", Quota: strconv.FormatInt(uploaderQuota>>20, 10), AvatarForm: true}
	uploadState(v, st)
	if d.Page == "account" {
		v.Notice = strings.TrimSuffix(v.Notice, listingNoticeTail)
	}
	d.Uploads = v
}
