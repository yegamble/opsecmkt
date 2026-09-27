package market

import (
	"context"
	"database/sql"
	"image"
	"image/color"
	"math"
	"net/http"
	"strconv"
	"sync"
)

// A-189 (war room 2026-09-27, owner decision 1): stored photos and avatars on pages. Pages select image ids
// only, never bytes (TestOnlyImageRoutesSelectImageBytes), and link them on the image routes (image_routes.go).
// Each page picks its images with the predicate of the route it links, so it never shows a broken image:
//
//   - catalog and vendor-page cards: the first public photo's thumbnail (/img/<id>-t), decorative;
//   - the product page: every public photo, the first at full size and the others as thumbnails linking to theirs;
//   - the vendor page: the vendor's avatar thumbnail when it is public;
//   - /canary: while the administrator's "Show a public staff list" is on, the moderators and administrators
//     who have not hidden themselves, with their avatar when it is public;
//   - the order page: the deciding staff member's avatar beside a dispute decision, on /desk/img
//     (loadOrderDetail).
//
// Nowhere else: no avatar on cards, messages or orders.

const settingStaffListPublic = "staff_list_public"

func init() {
	registerLoader("catalog", productPhotosLoader)
	registerLoader("vendor", productPhotosLoader)
	registerLoader("product", productPhotosLoader)
	registerLoader("vendor", vendorAvatarLoader)
	registerLoader("canary", staffListLoader)
	registerAction("/admin/staff-list", actionSpec{Roles: []string{"admin"}, OwnTx: true, Run: staffListAction})
	for _, page := range []string{"catalog", "vendor", "product"} {
		registerPreview(page, previewProductPhotos)
	}
}

// productPhotosLoader sets the public photos of the listings on the page (at most 25 listings, 4 photos each),
// oldest first, in one query.
func productPhotosLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if len(d.Products) == 0 {
		return nil
	}
	ids := make([]string, len(d.Products))
	for i, p := range d.Products {
		ids[i] = p.ID
	}
	rows, err := a.db.QueryContext(ctx, `SELECT i.product_id,i.id,i.width,i.height,i.description,octet_length(i."full") FROM `+imagePublicFrom+`
		WHERE i.product_id=ANY($1::text[]) AND `+imagePublicWhere+` ORDER BY i.product_id,i.created,i.id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	photos := map[string][]storedPhoto{}
	for rows.Next() {
		var product string
		var s storedPhoto
		if err = rows.Scan(&product, &s.id, &s.width, &s.height, &s.description, &s.size); err != nil {
			return err
		}
		photos[product] = append(photos[product], s)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range d.Products {
		setPhotos(&d.Products[i], photos[d.Products[i].ID])
	}
	if d.Page == "product" && d.Product != nil { // A-190: a staff remove form per photo
		for i := range d.Product.Photos {
			d.Product.Photos[i].Takedown = staffRemoveForm(d, d.Product.Photos[i].ID, d.Product.VendorID, "photo", "product", "Remove photo "+strconv.Itoa(i+1))
		}
	}
	return nil
}

// storedPhoto is what a page reads about one stored photo.
type storedPhoto struct {
	id, description string
	width, height   int
	size            int64 // bytes of the full-size JPEG
}

// setPhotos sets p's photos, their URLs on the public route and the text naming them: photo 1's text
// alternative is its description or "Photo 1 of N: <title>"; the link to each other photo's full size says
// "Photo 2 of N, full size (JPEG, 180 KB)".
func setPhotos(p *Product, stored []storedPhoto) {
	p.Photos, p.Thumb = nil, ""
	n := strconv.Itoa(len(stored))
	for i, s := range stored {
		number := "Photo " + strconv.Itoa(i+1) + " of " + n
		ph := ProductPhoto{ID: s.id, Full: publicImageURL(s.id, true), Thumb: publicImageURL(s.id, false), Description: s.description,
			Width: s.width, Height: s.height, Alt: s.description, Label: number + ", full size (JPEG, " + kilobytes(s.size) + ")"}
		if ph.Alt == "" {
			ph.Alt = number + ": " + p.Title
		}
		p.Photos = append(p.Photos, ph)
	}
	if len(p.Photos) > 0 {
		p.Thumb = p.Photos[0].Thumb
	}
}

// kilobytes is n bytes in KB (2^10, as megabytes counts MB), rounded, and at least 1.
func kilobytes(n int64) string { return strconv.FormatInt(max(1, (n+512)>>10), 10) + " KB" }

// vendorAvatarLoader sets the vendor's avatar on the vendor page when the public route serves it: a vendor's
// while they are not suspended; an administrator's only as a listed staff member (imagePublicWhere).
func vendorAvatarLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if len(d.Users) != 1 {
		return nil
	}
	var id string
	err := a.db.QueryRowContext(ctx, `SELECT i.id FROM `+imagePublicFrom+` WHERE i.owner_user_id=$1 AND i.kind='avatar' AND `+imagePublicWhere, d.Users[0].ID).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		return nil
	case err != nil:
		return err
	}
	d.Users[0].Avatar = publicImageURL(id, false)
	d.AvatarTakedown = staffRemoveForm(d, id, d.Users[0].ID, "avatar", "vendor", "Remove this avatar") // A-190
	return nil
}

// staffListPublic reads the administrator's setting: on only for the exact value "true" (a missing row or any
// other value is off, as imagePublicWhere reads it).
func staffListPublic(settings map[string]string) bool {
	return settings[settingStaffListPublic] == "true"
}

// staffListLoader lists, while the setting is on, every moderator and administrator who is not suspended and has
// not hidden themselves (users.staff_hidden, hidden by default), administrators first, with their avatar when the
// public route serves it. Hidden staff are not listed at all.
func staffListLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.StaffListPublic = staffListPublic(d.Settings); !d.StaffListPublic {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT u.handle,u.role,COALESCE((SELECT i.id FROM `+imagePublicFrom+`
		WHERE i.owner_user_id=u.id AND i.kind='avatar' AND `+imagePublicWhere+`),'')
		FROM users u WHERE u.role IN ('admin','moderator') AND u.suspended_at IS NULL AND NOT u.staff_hidden ORDER BY u.role,u.handle LIMIT 100`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u User
		var avatar string
		if err = rows.Scan(&u.Handle, &u.Role, &avatar); err != nil {
			return err
		}
		if avatar != "" {
			u.Avatar = publicImageURL(avatar, false)
		}
		d.StaffList = append(d.StaffList, u)
	}
	return rows.Err()
}

// staffListAction turns the public staff list on or off (owner decision 1(a)). The administrator confirms with
// their password (and authenticator code when enrolled); the change is audited old→new. A form for the state
// already in force changes nothing (409).
func staffListAction(c *actionCtx) (actionResult, error) {
	want := c.Form.Get(settingStaffListPublic)
	if want != "true" && want != "false" {
		return actionResult{}, fail(400, "Choose whether to show the public staff list")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		var old string
		err := c.Tx.QueryRowContext(c.Ctx(), "SELECT value FROM settings WHERE key=$1 FOR UPDATE", settingStaffListPublic).Scan(&old)
		if err != nil && err != sql.ErrNoRows {
			return actionResult{}, err
		}
		onOff := func(on bool) string {
			if on {
				return "on"
			}
			return "off"
		}
		was, now := onOff(old == "true"), onOff(want == "true")
		if was == now {
			return actionResult{}, fail(409, "The public staff list is already "+now+"; nothing was changed.")
		}
		if err = setSetting(c, settingStaffListPublic, want); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/admin?saved=1#staff-list", Audit: "Public staff list changed from " + was + " to " + now}, nil
	})
}

// Preview mode has no database. It shows three sample photos, generated when first needed and re-encoded by the
// upload pipeline's own resize and JPEG steps, and serves them through the public route's writer
// (servePreviewImage): two on the encrypted drive listing, one with a description on the security research one.
type previewPhoto struct {
	id, product, description string
	full, thumb              []byte
	width, height            int
}

var previewPhotos = sync.OnceValue(func() []previewPhoto {
	photos := []previewPhoto{
		{id: "a1890000000000000000000000000001", product: "encrypted-drive"},
		{id: "a1890000000000000000000000000002", product: "encrypted-drive"},
		{id: "a1890000000000000000000000000003", product: "security-research", description: "Preview sample picture: a generated dark card on a green gradient; not a photo of the product"},
	}
	for i := range photos {
		full, thumb := photoImages(previewPicture(i), 1)
		photos[i].full, photos[i].thumb = encodeJPEG(full, photoQuality), encodeJPEG(thumb, thumbQuality)
		photos[i].width, photos[i].height = full.Rect.Dx(), full.Rect.Dy()
	}
	return photos
})

// previewPicture draws sample picture n: a gradient with a tilted dark card, 1000×750 so the pipeline scales it.
func previewPicture(n int) image.Image {
	const w, h = 1000, 750
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	sin, cos := math.Sincos(float64(n+1) * -0.25)
	for y := range h {
		for x := range w {
			c := color.RGBA{uint8(20 + 30*x/w), uint8(60 + 90*y/h), uint8(50 + 40*n), 255}
			// the card: a rectangle about the centre, turned by the angle above
			dx, dy := float64(x-w/2), float64(y-h/2)
			if u, v := dx*cos-dy*sin, dx*sin+dy*cos; math.Abs(u) < 260 && math.Abs(v) < 150 {
				c = color.RGBA{34, 40, 36, 255}
				if math.Abs(u+60) < 110 && math.Abs(v) < 60 {
					c = color.RGBA{16, 30, 22, 255}
				}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// previewProductPhotos gives the preview's sample listings their sample photos.
func previewProductPhotos(d *PageData) {
	for i := range d.Products {
		var stored []storedPhoto
		for _, ph := range previewPhotos() {
			if ph.product == d.Products[i].ID {
				stored = append(stored, storedPhoto{id: ph.id, description: ph.description, width: ph.width, height: ph.height, size: int64(len(ph.full))})
			}
		}
		setPhotos(&d.Products[i], stored)
	}
}

// servePreviewImage answers the public image route in preview mode with a sample photo, or the unknown-id 404.
func servePreviewImage(w http.ResponseWriter, r *http.Request, ref imageRef) {
	for _, ph := range previewPhotos() {
		if ph.id == ref.id {
			b := ph.thumb
			if ref.full {
				b = ph.full
			}
			n := int64(len(b))
			if r.Method == "HEAD" {
				b = nil
			}
			writeImage(w, r, n, b, nil)
			return
		}
	}
	http.NotFound(w, r)
}
