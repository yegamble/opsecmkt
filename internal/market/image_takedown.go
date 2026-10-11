package market

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A-190 (war room 2026-09-27, ruling D4, owner decision 1): moderators take down vendors' listing photos and
// avatars, and administrators any photo or avatar, staff avatars included, with POST /images/remove. A takedown deletes the row (both sizes) in the
// action's transaction: no stub, no hidden flag, no purge log (D4). It records one audit row for the actor and one
// for the owner (fixed copy, a photo's listing by its short id, and the reason; never the image id) and notifies
// the owner with the reason. An owner removes their own images with A-188's forms, never here. Moderators gain no
// other listing capability: editing, archiving, taking down and restoring listings stay the vendor's and the
// administrator's (inventory_actions.go).
//
// The forms: /moderator "Recent photos" (the 50 newest images the public route serves, vendors' only for a
// moderator; the list to re-check after a database restore, which brings removed images back), a <details> form
// per photo on the product page and for the avatar on the vendor page, shown only to staff who may take that image
// down (mayTakeDown).
// The owner sees "Photo removed by staff: <reason>" on the listing's edit page, read from their own audit row.

func init() {
	registerAction("/images/remove", actionSpec{Roles: []string{"moderator", "admin"}, FormNonce: true, Run: imageTakedownAction})
	registerLoader("moderator", recentImagesLoader)
	registerPreview("moderator", previewRecentImages)
}

// recentImagesLimit is how many images /moderator lists.
const recentImagesLimit = 50

// ImageTakedown is a staff remove form for one image: its id, what it is ("photo" or "avatar"), the page it is on
// ("product" or "vendor", to return there; "" for /moderator) and the form's summary.
type ImageTakedown struct {
	CSRF, ID, What, From, Label string
}

// RecentImage is one image on /moderator's Recent photos, as the public route serves it.
type RecentImage struct {
	Thumb, Full    string
	Photo          bool
	Width, Height  int    // of the thumbnail as shown
	Listing, Title string // a photo's listing
	OwnerLink      string // the owner's public page: the vendor page, or the staff list for a moderator
	Owner, Added   string
	Takedown       *ImageTakedown // nil for the viewer's own image
}

// StaffRemoval is a staff takedown of one of a listing's photos, from the owner's audit row.
type StaffRemoval struct{ Reason, When string }

// staffViewer reports whether u may take images down.
func staffViewer(u *User) bool { return u != nil && (u.Role == "moderator" || u.Role == "admin") }

// mayTakeDown reports whether u may take down a public image of owner, whose role is ownerRole: an administrator
// any image but their own, a moderator a vendor's only (owner decision 1). imageTakedownAction enforces the same
// rule in SQL.
func mayTakeDown(u *User, owner, ownerRole string) bool {
	switch {
	case u == nil || owner == u.ID:
		return false
	case u.Role == "admin":
		return true
	}
	return u.Role == "moderator" && ownerRole == "vendor"
}

// staffRemoveForm is the staff remove form for image id owned by owner (whose role is ownerRole) on d's page, or
// nil when the viewer may not take it down (mayTakeDown).
func staffRemoveForm(d *PageData, id, owner, ownerRole, what, from, label string) *ImageTakedown {
	if !mayTakeDown(d.User, owner, ownerRole) {
		return nil
	}
	return &ImageTakedown{CSRF: d.CSRF, ID: id, What: what, From: from, Label: label}
}

// Audit copy. The owner's photo row is also what listing-edit reads back (staffRemovals), so its prefix
// must not change without keeping the old one readable.
func photoTakedownOwnerPrefix(listing string) string {
	return "Photo on listing " + shortID(listing) + " removed by staff: "
}

// imageTakedownAction deletes a photo or avatar for a moderator or administrator. The reason (10–500 characters
// after trimming, as for a listing takedown, with no NUL, which PostgreSQL text refuses) is checked first, so it
// tells nothing about the id. An administrator may take down any image but their own; a moderator only a vendor's
// image that the public route serves (owner decision 1: moderators take down vendors' images; staff avatars and
// administrators' listing photos are for administrators). Anything else, the actor's own image and an unknown id
// get the same 404.
//
// Lock order (lockListing's): the users rows of the actor and the owner (FOR KEY SHARE, which the audit and
// notification inserts' foreign keys take anyway), then the image row. The actor's role is read again under that
// lock, so a takedown racing an administrator's demotion of the actor waits for the demotion and is then refused
// (403; TestImageTakedownRefusedWhenActorDemotedMidRequest).
func imageTakedownAction(c *actionCtx) (actionResult, error) {
	reason := strings.TrimSpace(strings.ReplaceAll(c.Form.Get("reason"), "\r\n", "\n"))
	if n := utf8.RuneCountInString(reason); !utf8.ValidString(reason) || strings.ContainsRune(reason, 0) || n < minTakedownReason || n > maxTakedownReason {
		return actionResult{}, fail(400, "Give the owner a reason of 10–500 characters. Nothing was removed.")
	}
	notFound := fail(404, "Image not found. Nothing was removed.")
	ctx, tx, id := c.Ctx(), c.Tx, c.Form.Get("id")
	var kind, owner, handle, ownerRole, listing, title string
	err := tx.QueryRowContext(ctx, `SELECT i.kind,i.owner_user_id,o.handle,o.role,coalesce(p.id,''),coalesce(p.title,'') FROM `+imagePublicFrom+`
		WHERE i.id=$1 AND i.owner_user_id<>$2 AND ($3='admin' OR (o.role='vendor' AND `+imagePublicWhere+`))`, id, c.User.ID, c.User.Role).Scan(&kind, &owner, &handle, &ownerRole, &listing, &title)
	if err == sql.ErrNoRows {
		return actionResult{}, notFound
	}
	if err != nil {
		return actionResult{}, err
	}
	// Row locks may wait as long as an administrator's change that waits on a checkout (storeImage).
	if err = waitForWalletLocks(ctx, tx); err != nil {
		return actionResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM users WHERE id=$1 OR id=$2 FOR KEY SHARE`, c.User.ID, owner); err != nil {
		return actionResult{}, err
	}
	var role string
	if err = tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id=$1`, c.User.ID).Scan(&role); err != nil {
		return actionResult{}, err
	}
	if !staffViewer(&User{Role: role}) {
		return actionResult{}, fail(403, "Moderator access required")
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM images WHERE id=$1 AND owner_user_id=$2`, id, owner)
	if err != nil {
		return actionResult{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 { // removed meanwhile
		return actionResult{}, notFound
	}
	var audit, ownerAudit, note string
	back := "" // the product or vendor page the form was on; "" for /moderator
	if kind == "photo" {
		audit = "Took down a photo on listing " + shortID(listing) + ": " + reason
		ownerAudit = photoTakedownOwnerPrefix(listing) + reason
		note = "Staff removed a photo from your listing “" + title + "” (" + shortID(listing) + ") with the reason “" + reason +
			"”. The photo was deleted; you cannot undo this. The listing itself is unchanged. You can add another photo from the listing's edit page. If you think this is wrong, contact the market staff."
		if c.Form.Get("from") == "product" {
			back = "/product?id=" + url.QueryEscape(listing)
		}
	} else {
		audit = "Took down the avatar of " + handle + ": " + reason
		ownerAudit = "Avatar removed by staff: " + reason
		note = "Staff removed your avatar with the reason “" + reason + "”. It was deleted; you cannot undo this. If you think this is wrong, contact the market staff."
		if c.Form.Get("from") == "vendor" && (ownerRole == "vendor" || ownerRole == "admin") {
			back = "/vendor?id=" + url.QueryEscape(owner)
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", owner, ownerAudit); err != nil {
		return actionResult{}, err
	}
	// Not notifyOwner: its copy is for sign-in and key events ("someone else may know your password").
	if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), owner, note); err != nil {
		return actionResult{}, err
	}
	// The one-shot notice confirms it to the staff member (A-287): in Recent photos, or at the top of the page.
	out := actionResult{Redirect: "/moderator", Notice: "image-removed", Audit: audit}
	if back != "" {
		out.Notice, out.Redirect = "image-removed-page", back
	}
	return out, nil
}

// recentImagesLoader lists the newest images the public route serves (imagePublicWhere), newest first, for
// /moderator: moderators may not see other images (A-187), and these are the ones anyone can. A moderator sees
// only vendors' images, the ones they may take down; an administrator every public image.
func recentImagesLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	rows, err := a.db.QueryContext(ctx, `SELECT i.id,i.kind,i.owner_user_id,o.handle,o.role,coalesce(p.id,''),coalesce(p.title,''),
		to_char(i.created,'YYYY-MM-DD HH24:MI "UTC"') FROM `+imagePublicFrom+` WHERE `+imagePublicWhere+` AND ($2='admin' OR o.role='vendor')
		ORDER BY i.created DESC,i.id DESC LIMIT $1`, recentImagesLimit, d.User.Role)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind, owner, role string
		var im RecentImage
		if err = rows.Scan(&id, &kind, &owner, &im.Owner, &role, &im.Listing, &im.Title, &im.Added); err != nil {
			return err
		}
		d.RecentImages = append(d.RecentImages, recentImage(d, id, kind == "photo", owner, role, im))
	}
	return rows.Err()
}

// recentImage completes im, image id owned by owner (with role), for d's viewer.
func recentImage(d *PageData, id string, photo bool, owner, role string, im RecentImage) RecentImage {
	n := len(d.RecentImages) + 1
	im.Thumb, im.Full, im.Photo = publicImageURL(id, false), publicImageURL(id, true), photo
	im.Width, im.Height = 160, 120
	what := "photo"
	if !photo {
		im.Width, im.Height, what = 64, 64, "avatar"
	}
	im.OwnerLink = "/vendor?id=" + url.QueryEscape(owner)
	if role == "moderator" {
		im.OwnerLink = "/canary#staff"
	}
	im.Takedown = staffRemoveForm(d, id, owner, role, what, "", "Remove image "+strconv.Itoa(n))
	return im
}

// staffRemovals reads the staff takedowns of listing p's photos from its vendor's audit rows, newest first (the
// five latest), for the listing's edit page (loadPhotoPanel), which only its vendor and administrators may open.
// The audit row is the takedown's permanent record, never rewritten (D4 keeps no other); the notification carries
// the same reason but in prose, and a takedown's short listing id is unique enough within one vendor's listings.
func staffRemovals(ctx context.Context, a *App, p *Product) ([]StaffRemoval, error) {
	prefix := photoTakedownOwnerPrefix(p.ID)
	rows, err := a.db.QueryContext(ctx, `SELECT action,to_char(created,'YYYY-MM-DD HH24:MI "UTC"') FROM audit_events
		WHERE user_id=$1 AND starts_with(action,$2) ORDER BY id DESC LIMIT 5`, p.VendorID, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StaffRemoval
	for rows.Next() {
		var action string
		var s StaffRemoval
		if err = rows.Scan(&action, &s.When); err != nil {
			return nil, err
		}
		s.Reason = strings.TrimPrefix(action, prefix)
		out = append(out, s)
	}
	return out, rows.Err()
}

// previewRecentImages shows the preview's sample photos in Recent photos, each with its remove form.
func previewRecentImages(d *PageData) {
	for _, ph := range previewPhotos() {
		for _, p := range d.Products {
			if p.ID == ph.product {
				im := RecentImage{Listing: p.ID, Title: p.Title, Owner: p.Vendor, Added: "Sample photo"}
				d.RecentImages = append(d.RecentImages, recentImage(d, ph.id, true, p.VendorID, "vendor", im))
			}
		}
	}
}
