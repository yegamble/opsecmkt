package market

import (
	"html"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

// A-190 (war room 2026-09-27, ruling D4, owner decision 1): moderators and administrators take down listing photos
// and avatars (POST /images/remove) by deleting the row, with a reason the owner is told and sees on the listing's
// edit page; /moderator lists the recent public images for the check after a restore (PostgreSQL).

// imageTakedownReason contains characters the pages must escape.
const imageTakedownReason = "Shows a face & a <street sign>; upload a photo of the item alone."

// imageTakedownForm returns the fields of the staff remove form for image id on page, as the browser would submit
// them with reason, and checks it is a urlencoded form inside a <details> with a required 10–500 character reason.
func imageTakedownForm(t *testing.T, page, id, reason string) url.Values {
	t.Helper()
	for _, d := range strings.Split(page, "<details")[1:] {
		d, _, _ = strings.Cut(d, "</details>")
		if !strings.Contains(d, `action="/images/remove"`) || !strings.Contains(d, `name="id" value="`+id+`"`) {
			continue
		}
		mustContain(t, d, "<summary>", `method="post"`, `name="reason" required minlength="10" maxlength="500"`)
		if strings.Contains(d, "enctype") {
			t.Fatalf("the remove form is not urlencoded:\n%s", d)
		}
		v := url.Values{"reason": {reason}}
		for _, m := range hiddenInput.FindAllStringSubmatch(d, -1) {
			if m[1] != "csrf" {
				v.Set(m[1], m[2])
			}
		}
		return v
	}
	t.Fatalf("no staff remove form for image %s", id)
	return nil
}

// takeDown posts the staff remove form for image id with reason as session.
func (e *testEnv) takeDown(session, id, reason string) *httptest.ResponseRecorder {
	return e.do("POST", "/images/remove", session, url.Values{"id": {id}, "reason": {reason}})
}

// imageRows counts the rows with id.
func (e *testEnv) imageRows(id string) (n int) {
	e.t.Helper()
	if err := e.DB.QueryRow("SELECT count(*) FROM images WHERE id=$1", id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// audits returns the audit actions recorded for user, oldest first.
func (e *testEnv) audits(user string) (actions []string) {
	e.t.Helper()
	rows, err := e.DB.Query("SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id", user)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		rows.Scan(&a)
		actions = append(actions, a)
	}
	return actions
}

// notes returns the notification bodies of user, oldest first.
func (e *testEnv) notes(user string) (bodies []string) {
	e.t.Helper()
	rows, err := e.DB.Query("SELECT body FROM notifications WHERE user_id=$1 ORDER BY created,id", user)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		rows.Scan(&b)
		bodies = append(bodies, b)
	}
	return bodies
}

func last(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// D4: a moderator's takedown deletes the row in the action's transaction (photo, a vendor's avatar, and an
// administrator's takedown of a staff avatar); afterwards both routes answer exactly as for an unknown id, and the
// audit rows carry fixed copy, the listing's short id and the reason, never the image id.
func TestModeratorRemovesImageHardDelete(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("td_img_vendor", "vendor")
	moderatorID, moderator := e.user("td_img_moderator", "moderator")
	adminID, admin := e.user("td_img_admin", "admin")
	listing := e.product(vendorID, "physical")
	photo := e.image(vendorID, listing, "photo")
	e.wantPublic(photo, "before the takedown")

	w := e.takeDown(moderator, photo.id, imageTakedownReason)
	if w.Code != 303 || w.Header().Get("Location") != "/moderator#recent-photos" {
		t.Fatalf("takedown: %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	if n := e.imageRows(photo.id); n != 0 {
		t.Fatalf("%d rows after the takedown", n)
	}
	e.wantHidden(photo, "public route after the takedown")
	e.wantDeskHidden(vendor, photo, "owner's desk route after the takedown")
	e.wantDeskHidden(admin, photo, "administrator's desk route after the takedown")
	if got, want := last(e.audits(moderatorID)), "Took down a photo on listing "+listing[:8]+": "+imageTakedownReason; got != want {
		t.Fatalf("moderator's audit %q, want %q", got, want)
	}
	if got, want := last(e.audits(vendorID)), "Photo on listing "+listing[:8]+" removed by staff: "+imageTakedownReason; got != want {
		t.Fatalf("vendor's audit %q, want %q", got, want)
	}

	// A vendor's avatar, and a moderator's avatar (a staff avatar) taken down by an administrator.
	avatar := e.image(vendorID, "", "avatar")
	staffAvatar := e.image(moderatorID, "", "avatar")
	e.check(e.takeDown(moderator, avatar.id, imageTakedownReason), 303)
	e.check(e.takeDown(admin, staffAvatar.id, imageTakedownReason), 303)
	for _, im := range []testImage{avatar, staffAvatar} {
		if n := e.imageRows(im.id); n != 0 {
			t.Fatalf("avatar %s: %d rows after the takedown", im.id, n)
		}
		e.wantHidden(im, "avatar after the takedown")
		e.wantDeskHidden(admin, im, "avatar on the desk route after the takedown")
	}
	if got, want := last(e.audits(moderatorID)), "Avatar removed by staff: "+imageTakedownReason; got != want {
		t.Fatalf("moderator's own avatar audit %q, want %q", got, want)
	}
	if got, want := last(e.audits(adminID)), "Took down the avatar of td_img_moderator: "+imageTakedownReason; got != want {
		t.Fatalf("administrator's audit %q, want %q", got, want)
	}
	for _, uid := range []string{vendorID, moderatorID, adminID} {
		for _, a := range e.audits(uid) {
			for _, im := range []testImage{photo, avatar, staffAvatar} {
				if strings.Contains(a, im.id) || strings.Contains(a, im.id[:8]) {
					t.Fatalf("audit row %q names image %s", a, im.id)
				}
			}
		}
	}
	// Removing it again is the unknown-id answer.
	again, unknown := e.takeDown(moderator, photo.id, imageTakedownReason), e.takeDown(moderator, randomToken()[:32], imageTakedownReason)
	if again.Code != 404 || again.Body.String() != unknown.Body.String() {
		t.Fatalf("second takedown: %d %q, unknown id: %d %q", again.Code, again.Body.String(), unknown.Code, unknown.Body.String())
	}
}

// Every refusal after the role check is the unknown-id 404: an image a moderator may not see (an archived
// listing's photo, a hidden staff member's avatar), the actor's own image (removed with the owner's form) and an
// unknown or malformed id. A reason outside 10–500 characters is refused before the id is looked at. Nothing is
// deleted and nothing recorded.
func TestImageTakedownRefusalsMatchUnknownID(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("td_ref_vendor", "vendor")
	moderatorID, moderator := e.user("td_ref_moderator", "moderator")
	adminID, admin := e.user("td_ref_admin", "admin")
	archived := e.product(vendorID, "physical")
	hiddenPhoto := e.image(vendorID, archived, "photo")
	if _, err := e.DB.Exec("UPDATE products SET archived=true,archived_at=now() WHERE id=$1", archived); err != nil {
		t.Fatal(err)
	}
	hiddenAdminAvatar := e.image(adminID, "", "avatar") // staff are hidden and the list is off by default
	ownAvatar := e.image(moderatorID, "", "avatar")
	ref := e.takeDown(moderator, randomToken()[:32], imageTakedownReason)
	if ref.Code != 404 || !strings.Contains(ref.Body.String(), "Nothing was removed") {
		t.Fatalf("unknown id: %d %q", ref.Code, ref.Body.String())
	}
	for what, id := range map[string]string{"archived listing's photo": hiddenPhoto.id, "hidden administrator's avatar": hiddenAdminAvatar.id,
		"own avatar": ownAvatar.id, "malformed id": "../" + hiddenPhoto.id, "empty id": ""} {
		w := e.takeDown(moderator, id, imageTakedownReason)
		if w.Code != ref.Code || w.Body.String() != ref.Body.String() {
			t.Fatalf("%s: %d %q, want the unknown-id answer %q", what, w.Code, w.Body.String(), ref.Body.String())
		}
	}
	// An administrator may take down the photo of an archived listing, but not their own avatar here.
	if w := e.takeDown(admin, hiddenAdminAvatar.id, imageTakedownReason); w.Code != 404 || w.Body.String() != ref.Body.String() {
		t.Fatalf("administrator's own avatar: %d %q", w.Code, w.Body.String())
	}
	for _, reason := range []string{"", "too short", "   short   ", strings.Repeat("x", 501), "\xff\xfe invalid utf-8 reason"} {
		for _, id := range []string{hiddenPhoto.id, randomToken()[:32]} {
			w := e.takeDown(admin, id, reason)
			want := "10–500 characters"
			if !utf8.ValidString(reason) {
				want = invalidTextMsg // refused where the form is parsed (QA5W3B-5)
			}
			if w.Code != 400 || !strings.Contains(w.Body.String(), want) {
				t.Fatalf("reason %q: %d %q", reason, w.Code, w.Body.String())
			}
		}
	}
	if e.imageCount() != 3 {
		t.Fatalf("%d images after refused takedowns", e.imageCount())
	}
	for _, uid := range []string{vendorID, moderatorID, adminID} {
		if len(e.audits(uid)) != 0 || len(e.notes(uid)) != 0 {
			t.Fatalf("a refused takedown was recorded: %q %q", e.audits(uid), e.notes(uid))
		}
	}
	e.check(e.takeDown(admin, hiddenPhoto.id, strings.Repeat("y", 500)), 303)
	if e.imageRows(hiddenPhoto.id) != 0 {
		t.Fatal("the administrator's takedown of an archived listing's photo left the row")
	}
}

// Buyers and vendors, the image's owner included, cannot use the staff route, and are offered no staff forms.
func TestBuyerAndVendorCannotRemoveOthersImage(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("td_owner_vendor", "vendor")
	_, other := e.user("td_other_vendor", "vendor")
	_, buyer := e.user("td_buyer", "buyer")
	listing := e.product(vendorID, "physical")
	photo := e.image(vendorID, listing, "photo")
	avatar := e.image(vendorID, "", "avatar")
	for _, s := range []string{buyer, other, vendor} {
		for _, id := range []string{photo.id, avatar.id} {
			e.check(e.takeDown(s, id, imageTakedownReason), 403)
		}
		for _, path := range []string{"/product?id=" + listing, "/vendor?id=" + vendorID} {
			if body := e.body("GET", path, s, nil, 200); strings.Contains(body, "/images/remove") {
				t.Fatalf("%s offers a staff remove form to a non-staff account", path)
			}
		}
		e.check(e.do("GET", "/moderator", s, nil), 403)
	}
	if body := e.body("GET", "/product?id="+listing, "", nil, 200); strings.Contains(body, "/images/remove") {
		t.Fatal("the product page offers a staff remove form to a visitor")
	}
	if e.imageCount() != 2 {
		t.Fatalf("%d images after refused takedowns", e.imageCount())
	}
	e.wantPublic(photo, "photo after refused takedowns")
	e.wantPublic(avatar, "avatar after refused takedowns")
}

// A moderator takes images down and nothing more: listing edits, archive, restore, takedown, the listing's edit
// page, photo upload and the owner's photo removal all stay refused.
func TestModeratorCannotEditListing(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, _ := e.user("td_edit_vendor", "vendor")
	_, moderator := e.user("td_edit_moderator", "moderator")
	listing := e.product(vendorID, "physical")
	photo := e.image(vendorID, listing, "photo")
	for path, form := range map[string]url.Values{
		"/listings/update":       inventoryForm(listing, map[string]string{"stock_seen": "5", "title": "Changed by a moderator"}),
		"/listings/archive":      {"id": {listing}},
		"/listings/restore":      {"id": {listing}},
		"/admin/listing":         {"id": {listing}, "action": {"takedown"}, "reason": {imageTakedownReason}, "password": {testPassword}},
		"/listings/photo/remove": {"id": {photo.id}},
	} {
		e.check(e.do("POST", path, moderator, form), 403)
	}
	e.check(e.do("GET", "/listing-edit?id="+listing, moderator, nil), 403)
	checkPage(t, e.upload("/listings/photo", moderator, e.photoParts(moderator, listing, smallJPEG(t))...), 403, "only vendors and administrators")
	var title string
	var archived bool
	if err := e.DB.QueryRow("SELECT title,archived FROM products WHERE id=$1", listing).Scan(&title, &archived); err != nil {
		t.Fatal(err)
	}
	if title != "Test listing" || archived || e.imageCount() != 1 || scansOf(f) != 0 {
		t.Fatalf("after a moderator's refused changes: title %q archived %v, %d images, %d scans", title, archived, e.imageCount(), scansOf(f))
	}
}

// The owner is notified with the reason (link-free, no image id) and sees "Photo removed by staff: <reason>" on the
// listing's edit page on every load, derived from their audit row; an administrator sees it there too; another
// listing of the same vendor does not show it. The takedown is posted through the form on the product page.
func TestTakedownNotifiesOwnerWithReason(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("td_note_vendor", "vendor")
	moderatorID, moderator := e.user("td_note_moderator", "moderator")
	_, admin := e.user("td_note_admin", "admin")
	listing := e.product(vendorID, "physical")
	otherListing := e.product(vendorID, "physical")
	photo := e.image(vendorID, listing, "photo")
	kept := e.image(vendorID, listing, "photo")

	form := imageTakedownForm(t, e.body("GET", "/product?id="+listing, moderator, nil, 200), photo.id, imageTakedownReason)
	w := e.do("POST", "/images/remove", moderator, form)
	if w.Code != 303 || w.Header().Get("Location") != "/product?id="+listing {
		t.Fatalf("takedown from the product page: %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	back := e.body("GET", w.Header().Get("Location"), moderator, nil, 200, noticeSetCookie(w))
	mustContain(t, back, "Image removed. Its owner was notified with your reason.")
	if strings.Contains(back, photo.id) || !strings.Contains(back, kept.id) {
		t.Fatal("the product page after the takedown still shows the photo, or lost the other one")
	}
	wantNote := "Staff removed a photo from your listing “Test listing” (" + listing[:8] + ") with the reason “" + imageTakedownReason +
		"”. The photo was deleted; you cannot undo this. The listing itself is unchanged. You can add another photo from the listing's edit page. If you think this is wrong, contact the market staff."
	if got := e.notes(vendorID); len(got) != 1 || got[0] != wantNote {
		t.Fatalf("vendor's notifications %q, want %q", got, wantNote)
	}
	if len(e.notes(moderatorID)) != 0 {
		t.Fatal("the moderator was notified of their own takedown")
	}
	shown := "Photo removed by staff:</strong> " + html.EscapeString(imageTakedownReason)
	for range 2 { // survives a reload
		for _, s := range []string{vendor, admin} {
			mustContain(t, sectionOf(t, e.body("GET", "/listing-edit?id="+listing, s, nil, 200), "photos"), shown)
		}
	}
	if strings.Contains(e.body("GET", "/listing-edit?id="+otherListing, vendor, nil, 200), "Photo removed by staff") {
		t.Fatal("another listing shows the takedown")
	}
	// The public product page never shows the reason.
	if strings.Contains(e.body("GET", "/product?id="+listing, "", nil, 200), "street sign") {
		t.Fatal("the reason is shown publicly")
	}

	// An avatar, taken down from the vendor page's form.
	avatar := e.image(vendorID, "", "avatar")
	form = imageTakedownForm(t, e.body("GET", "/vendor?id="+vendorID, moderator, nil, 200), avatar.id, "Avatar shows a real name tag.")
	w = e.do("POST", "/images/remove", moderator, form)
	if w.Code != 303 || w.Header().Get("Location") != "/vendor?id="+vendorID {
		t.Fatalf("takedown from the vendor page: %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	wantAvatar := "Staff removed your avatar with the reason “Avatar shows a real name tag.”. It was deleted; you cannot undo this. If you think this is wrong, contact the market staff."
	if got := last(e.notes(vendorID)); got != wantAvatar {
		t.Fatalf("avatar notification %q, want %q", got, wantAvatar)
	}
	for _, n := range e.notes(vendorID) {
		if strings.Contains(n, photo.id) || strings.Contains(n, avatar.id) || strings.Contains(n, "href") {
			t.Fatalf("notification %q names the image or links", n)
		}
	}
}

// Item 5: an administrator adding a photo to, or removing one from, another vendor's listing is recorded in the
// vendor's own audit trail too (fixed copy, the listing's short id). The vendor's own changes are not doubled.
func TestAdminPhotoAuditedToVendor(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, vendor := e.user("td_audit_vendor", "vendor")
	adminID, admin := e.user("td_audit_admin", "admin")
	listing := e.product(vendorID, "physical")
	if w := e.upload("/listings/photo", admin, e.photoParts(admin, listing, smallJPEG(t))...); w.Code != 303 {
		t.Fatalf("administrator's upload: %d %s", w.Code, w.Body.String())
	}
	if got, want := e.audits(vendorID), []string{"Photo added to listing " + listing[:8] + " by an administrator"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("vendor's audit after the administrator's upload %q, want %q", got, want)
	}
	if got := last(e.audits(adminID)); got != "Added a photo to listing "+listing[:8]+" as administrator" {
		t.Fatalf("administrator's audit %q", got)
	}
	e.check(e.do("POST", "/listings/photo/remove", admin, url.Values{"id": {photoIDOf(t, e, listing)}}), 303)
	if got := last(e.audits(vendorID)); got != "Photo removed from listing "+listing[:8]+" by an administrator" {
		t.Fatalf("vendor's audit after the administrator's removal %q", got)
	}
	// The vendor's own upload and removal: one row each, the vendor's own.
	if w := e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, smallJPEG(t))...); w.Code != 303 {
		t.Fatalf("vendor's upload: %d", w.Code)
	}
	e.check(e.do("POST", "/listings/photo/remove", vendor, url.Values{"id": {photoIDOf(t, e, listing)}}), 303)
	want := []string{"Photo added to listing " + listing[:8] + " by an administrator", "Photo removed from listing " + listing[:8] + " by an administrator",
		"Added a photo to listing " + listing[:8], "Removed a photo from listing " + listing[:8]}
	if got := e.audits(vendorID); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("vendor's audit %q, want %q", got, want)
	}
}

// Every staff change to another account's image notifies its owner: an administrator's upload and removal on a
// vendor's listing, a moderator's takedown of a vendor's avatar and an administrator's takedown of a moderator's
// avatar. The owner's own changes notify nobody.
func TestStaffImageChangeNotifiesOwner(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, vendor := e.user("td_notify_vendor", "vendor")
	moderatorID, moderator := e.user("td_notify_moderator", "moderator")
	_, admin := e.user("td_notify_admin", "admin")
	listing := e.product(vendorID, "physical")
	if w := e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, smallJPEG(t))...); w.Code != 303 {
		t.Fatalf("vendor's upload: %d", w.Code)
	}
	e.check(e.do("POST", "/listings/photo/remove", vendor, url.Values{"id": {photoIDOf(t, e, listing)}}), 303)
	if n := e.notes(vendorID); len(n) != 0 {
		t.Fatalf("the vendor's own changes notified: %q", n)
	}
	if w := e.upload("/listings/photo", admin, e.photoParts(admin, listing, smallJPEG(t))...); w.Code != 303 {
		t.Fatalf("administrator's upload: %d", w.Code)
	}
	e.check(e.do("POST", "/listings/photo/remove", admin, url.Values{"id": {photoIDOf(t, e, listing)}}), 303)
	avatar := e.image(vendorID, "", "avatar")
	e.check(e.takeDown(moderator, avatar.id, "Avatar shows a real name tag."), 303)
	want := []string{
		"An administrator added a photo to your listing " + listing[:8] + ".",
		"An administrator removed a photo from your listing " + listing[:8] + ".",
		"Staff removed your avatar with the reason “Avatar shows a real name tag.”. It was deleted; you cannot undo this. If you think this is wrong, contact the market staff.",
	}
	if got := e.notes(vendorID); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("vendor's notifications %q, want %q", got, want)
	}
	staffAvatar := e.image(moderatorID, "", "avatar")
	e.check(e.takeDown(admin, staffAvatar.id, "A staff avatar must not show a face."), 303)
	if got := e.notes(moderatorID); len(got) != 1 || !strings.Contains(got[0], "“A staff avatar must not show a face.”") {
		t.Fatalf("moderator's notifications %q", got)
	}
}

// Beside an administrator's role change of the owner (which holds the owner's users row and archives their
// listings) the takedown still finishes. It never locks a listing, so this is a regression check of the lock order
// (users rows first, as lockListing), not a deadlock reproduction: what the actor's lock is for is proven by
// TestImageTakedownRefusedWhenActorDemotedMidRequest.
func TestImageTakedownDoesNotDeadlockWithRoleChange(t *testing.T) {
	for _, kind := range []string{"photo", "avatar"} {
		t.Run(kind, func(t *testing.T) {
			e := newTestApp(t)
			vendorID, _ := e.user("td_deadlock_vendor", "vendor")
			_, moderator := e.user("td_deadlock_moderator", "moderator")
			product := ""
			if kind == "photo" {
				product = e.product(vendorID, "physical")
			}
			im := e.image(vendorID, product, kind)
			w := besideRoleChange(t, e, vendorID, func() *httptest.ResponseRecorder {
				return e.takeDown(moderator, im.id, imageTakedownReason)
			})
			if w.Code != 303 || e.imageRows(im.id) != 0 {
				t.Fatalf("takedown beside a role change: %d %s, %d rows", w.Code, w.Body.String(), e.imageRows(im.id))
			}
		})
	}
}

// /moderator "Recent photos": the 50 newest images the public route serves, newest first, each with its thumbnail
// on /img, its listing and owner links, when it was added and a remove form; never an image the public route
// refuses (an archived listing's photo, a suspended vendor's, a hidden staff avatar). Staff see <details> remove
// forms on the product page (per photo) and the vendor page (the avatar), never for their own images.
func TestRecentPhotosAndStaffForms(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("td_recent_vendor", "vendor")
	suspendedID, _ := e.user("td_recent_suspended", "vendor")
	_, moderator := e.user("td_recent_moderator", "moderator")
	adminID, admin := e.user("td_recent_admin", "admin")
	listing := e.product(vendorID, "physical")
	archived := e.product(vendorID, "physical")
	suspendedListing := e.product(suspendedID, "physical")
	if _, err := e.DB.Exec("UPDATE products SET archived=true,archived_at=now() WHERE id=$1", archived); err != nil {
		t.Fatal(err)
	}
	hidden := []testImage{e.image(vendorID, archived, "photo"), e.image(suspendedID, suspendedListing, "photo"), e.image(adminID, "", "avatar")}
	if _, err := e.DB.Exec("UPDATE users SET suspended_at=now() WHERE id=$1", suspendedID); err != nil {
		t.Fatal(err)
	}
	avatar := e.image(vendorID, "", "avatar")
	var photos []testImage
	for range 50 {
		photos = append(photos, e.image(vendorID, listing, "photo"))
	}
	// Distinct times, oldest first: the avatar, then photos[0..49].
	if _, err := e.DB.Exec(`UPDATE images SET created=now()-interval '1 hour' WHERE id=$1`, avatar.id); err != nil {
		t.Fatal(err)
	}
	for i, p := range photos {
		if _, err := e.DB.Exec(`UPDATE images SET created=now()-make_interval(mins => $2) WHERE id=$1`, p.id, 50-i); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{moderator, admin} {
		section := sectionOf(t, e.body("GET", "/moderator", s, nil, 200), "recent-photos")
		mustContain(t, section, "Recent photos", publicImageURL(photos[49].id, false), `href="/product?id=`+listing+`"`, `href="/vendor?id=`+vendorID+`"`, "UTC")
		if strings.Count(section, `action="/images/remove"`) != 50 || strings.Count(section, "<details") != 50 {
			t.Fatalf("want 50 remove forms:\n%s", section)
		}
		if strings.Contains(section, avatar.id) {
			t.Fatal("the 51st newest image is listed")
		}
		if strings.Index(section, photos[49].id) > strings.Index(section, photos[0].id) {
			t.Fatal("not newest first")
		}
		for _, im := range hidden {
			if strings.Contains(section, im.id) {
				t.Fatalf("an image the public route refuses is listed: %s", im.id)
			}
		}
		if strings.Contains(section, "/desk/img/") {
			t.Fatal("Recent photos links the desk route")
		}
		imageTakedownForm(t, section, photos[10].id, imageTakedownReason)
	}
	// With fewer images the avatar is listed too, linked to the vendor page.
	for _, p := range photos[:45] {
		e.DB.Exec("DELETE FROM images WHERE id=$1", p.id)
	}
	section := sectionOf(t, e.body("GET", "/moderator", moderator, nil, 200), "recent-photos")
	mustContain(t, section, publicImageURL(avatar.id, false))
	imageTakedownForm(t, section, avatar.id, imageTakedownReason)

	// The product page: one staff form per photo; the vendor page: one for the avatar.
	for _, s := range []string{moderator, admin} {
		product := e.body("GET", "/product?id="+listing, s, nil, 200)
		if n := strings.Count(product, `action="/images/remove"`); n != 5 {
			t.Fatalf("product page: %d staff forms, want 5", n)
		}
		for _, p := range photos[45:] {
			if f := imageTakedownForm(t, product, p.id, imageTakedownReason); f.Get("from") != "product" {
				t.Fatalf("product form fields %v", f)
			}
		}
		vendorPage := e.body("GET", "/vendor?id="+vendorID, s, nil, 200)
		if n := strings.Count(vendorPage, `action="/images/remove"`); n != 1 {
			t.Fatalf("vendor page: %d staff forms, want 1 (the avatar; cards have none)", n)
		}
		if f := imageTakedownForm(t, vendorPage, avatar.id, imageTakedownReason); f.Get("from") != "vendor" {
			t.Fatalf("vendor form fields %v", f)
		}
	}
	// An administrator's own listing and avatar: no staff form for the administrator.
	ownListing := e.product(adminID, "physical")
	e.image(adminID, ownListing, "photo")
	if strings.Contains(e.body("GET", "/product?id="+ownListing, admin, nil, 200), "/images/remove") {
		t.Fatal("an administrator is offered the staff form for their own photo")
	}
	// Owner decision 1: moderators take down vendors' images only, so they get no form for an administrator's.
	if strings.Contains(e.body("GET", "/product?id="+ownListing, moderator, nil, 200), "/images/remove") {
		t.Fatal("a moderator is offered the staff form for an administrator's photo")
	}
}

// PR #27 SHOULD-1: the takedown locks the actor's users row before it re-reads the actor's role, so a takedown
// racing an administrator's demotion of the actor waits for it and is then refused (403), deleting nothing.
// Without the lock the re-read sees the old role; without the re-read nothing checks it.
func TestImageTakedownRefusedWhenActorDemotedMidRequest(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("td_demoted_vendor", "vendor")
	moderatorID, moderator := e.user("td_demoted_moderator", "moderator")
	photo := e.image(vendorID, e.product(vendorID, "physical"), "photo")
	w := besideRoleChange(t, e, moderatorID, func() *httptest.ResponseRecorder {
		return e.takeDown(moderator, photo.id, imageTakedownReason)
	})
	if w.Code != 403 || e.imageRows(photo.id) != 1 {
		t.Fatalf("takedown by a moderator demoted meanwhile: %d %s, %d rows", w.Code, w.Body.String(), e.imageRows(photo.id))
	}
	if len(e.audits(moderatorID)) != 0 || len(e.audits(vendorID)) != 0 || len(e.notes(vendorID)) != 0 {
		t.Fatal("a refused takedown was recorded")
	}
}

// PR #27 SHOULD-2 (owner decision 1: moderators take down vendors' images): a moderator gets the unknown-id 404 for
// an administrator's public avatar, another moderator's decision avatar and a photo on an administrator's own
// listing, although they can see all three; an administrator removes each of them. A moderator's Recent photos
// lists only the images they may remove; an administrator's lists every public image.
func TestModeratorTakesDownOnlyVendorImages(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("td_only_vendor", "vendor")
	buyerID, _ := e.user("td_only_buyer", "buyer")
	_, moderator := e.user("td_only_moderator", "moderator")
	deciderID, _ := e.user("td_only_decider", "moderator")
	adminID, _ := e.user("td_only_admin", "admin")
	_, otherAdmin := e.user("td_only_other_admin", "admin")
	// The administrator is on the public staff list, so their avatar is public.
	if _, err := e.DB.Exec(`UPDATE settings SET value='true' WHERE key='staff_list_public'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`UPDATE users SET staff_hidden=false WHERE id=$1`, adminID); err != nil {
		t.Fatal(err)
	}
	adminAvatar := e.image(adminID, "", "avatar")
	adminPhoto := e.image(adminID, e.product(adminID, "physical"), "photo")
	// The other moderator decided a dispute, so every moderator sees their avatar on the desk route.
	deciderAvatar := e.image(deciderID, "", "avatar")
	order := e.order(buyerID, e.product(vendorID, "physical"), "BTC", "resolved")
	if _, err := e.DB.Exec("INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,'disputed','resolved',$2,'Resolved — refund to buyer')", order, deciderID); err != nil {
		t.Fatal(err)
	}
	e.wantPublic(adminAvatar, "administrator's listed avatar")
	e.wantPublic(adminPhoto, "administrator's listing photo")
	e.wantDesk(moderator, deciderAvatar, "decision avatar for a moderator")
	vendorPhoto := e.image(vendorID, e.product(vendorID, "physical"), "photo")

	section := sectionOf(t, e.body("GET", "/moderator", moderator, nil, 200), "recent-photos")
	if !strings.Contains(section, vendorPhoto.id) || strings.Contains(section, adminAvatar.id) || strings.Contains(section, adminPhoto.id) {
		t.Fatalf("a moderator's Recent photos must list only vendors' images:\n%s", section)
	}
	section = sectionOf(t, e.body("GET", "/moderator", otherAdmin, nil, 200), "recent-photos")
	mustContain(t, section, vendorPhoto.id, adminAvatar.id, adminPhoto.id)
	imageTakedownForm(t, section, adminAvatar.id, imageTakedownReason)

	ref := e.takeDown(moderator, randomToken()[:32], imageTakedownReason)
	for what, im := range map[string]testImage{"administrator's avatar": adminAvatar, "another moderator's decision avatar": deciderAvatar, "administrator's listing photo": adminPhoto} {
		w := e.takeDown(moderator, im.id, imageTakedownReason)
		if w.Code != 404 || w.Body.String() != ref.Body.String() || e.imageRows(im.id) != 1 {
			t.Fatalf("moderator on the %s: %d %q, %d rows; want the unknown-id 404", what, w.Code, w.Body.String(), e.imageRows(im.id))
		}
	}
	for what, im := range map[string]testImage{"administrator's avatar": adminAvatar, "moderator's decision avatar": deciderAvatar, "administrator's listing photo": adminPhoto} {
		if w := e.takeDown(otherAdmin, im.id, imageTakedownReason); w.Code != 303 || e.imageRows(im.id) != 0 {
			t.Fatalf("administrator on the %s: %d %s", what, w.Code, w.Body.String())
		}
	}
	e.check(e.takeDown(moderator, vendorPhoto.id, imageTakedownReason), 303)
}

// PR #27 NIT-1: PostgreSQL refuses a NUL byte in text, so a reason carrying one gets a 400 before anything is
// written, not a 500 from the insert; since QA5W3B-5 it is the form's own refusal, before the handler runs.
func TestImageTakedownReasonWithNULRefused(t *testing.T) {
	e := newTestApp(t)
	vendorID, _ := e.user("td_nul_vendor", "vendor")
	_, moderator := e.user("td_nul_moderator", "moderator")
	photo := e.image(vendorID, e.product(vendorID, "physical"), "photo")
	w := e.takeDown(moderator, photo.id, "A reason with a \x00 NUL byte in it.")
	if w.Code != 400 || !strings.Contains(w.Body.String(), invalidTextMsg) || e.imageRows(photo.id) != 1 {
		t.Fatalf("NUL in the reason: %d %q, %d rows", w.Code, w.Body.String(), e.imageRows(photo.id))
	}
}

// PR #28 review (chair's ruling on REQUIRED 1): a staff avatar is uploaded under the staff privacy rules (hidden
// unless listed), while a vendor's avatar is public. An administrator moving a moderator to another role deletes
// the avatar in that transaction (D4's hard delete), notifies the owner and audits it, so a demotion to vendor
// never publishes it on /img or the vendor page. The admin role form cannot change an administrator, so an
// administrator's hidden avatar stays hidden. A vendor promoted to moderator starts hidden: nothing to delete.
func TestStaffDemotionDeletesAvatar(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("demote_admin", "admin")
	otherAdminID, _ := e.user("demote_other_admin", "admin")
	const note = "Your staff avatar was removed when your role changed; upload a new one if you want one shown."
	const audit = "Staff avatar removed when the role changed"
	for _, to := range []string{"vendor", "buyer"} {
		modID, _ := e.user("demote_mod_to_"+to, "moderator")
		avatar := e.image(modID, "", "avatar")
		e.wantHidden(avatar, "hidden moderator's avatar before the change")
		agExpect(e, "/admin", admin, form("action", "role", "password", testPassword, "role", to, "handle", "demote_mod_to_"+to), 303, "")
		if agStr(e, "SELECT role FROM users WHERE id=$1", modID) != to {
			t.Fatalf("moderator not changed to %s", to)
		}
		if n := e.imageRows(avatar.id); n != 0 {
			t.Fatalf("moderator → %s: %d avatar rows left, want 0", to, n)
		}
		e.wantHidden(avatar, "former moderator's avatar after the change to "+to)
		if to == "vendor" {
			body := e.body("GET", "/vendor?id="+modID, randomToken(), nil, 200)
			mustNotContain(t, body, avatar.id, "/img/")
		}
		if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", modID, note) != 1 {
			t.Fatalf("moderator → %s: owner not notified", to)
		}
		if !e.auditExact(modID, audit) || !e.auditExact(adminID, "Changed role of demote_mod_to_"+to+" from moderator to "+to+"; removed their staff avatar (confirmed with password)") {
			t.Fatalf("moderator → %s: audit rows missing", to)
		}
	}
	// A moderator without an avatar is changed with no avatar note or audit row.
	bareID, _ := e.user("demote_mod_bare", "moderator")
	agExpect(e, "/admin", admin, form("action", "role", "password", testPassword, "role", "vendor", "handle", "demote_mod_bare"), 303, "")
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", bareID) != 0 || e.auditExact(bareID, audit) {
		t.Fatal("a moderator without an avatar got the avatar note or audit row")
	}
	// Administrators' roles are not changed here: the other administrator's hidden avatar stays, and stays hidden.
	adminAvatar := e.image(otherAdminID, "", "avatar")
	agExpect(e, "/admin", admin, form("action", "role", "password", testPassword, "role", "vendor", "handle", "demote_other_admin"), 404, "No buyer, vendor or moderator has the handle")
	if agStr(e, "SELECT role FROM users WHERE id=$1", otherAdminID) != "admin" || e.imageRows(adminAvatar.id) != 1 {
		t.Fatal("the admin role form changed an administrator or their avatar")
	}
	e.wantHidden(adminAvatar, "administrator's hidden avatar after a refused change")
	// A vendor promoted to moderator starts hidden: the avatar is kept and is no longer public.
	vendorID, _ := e.user("promote_vendor", "vendor")
	vendorAvatar := e.image(vendorID, "", "avatar")
	e.wantPublic(vendorAvatar, "vendor's avatar")
	agExpect(e, "/admin", admin, form("action", "role", "password", testPassword, "role", "moderator", "handle", "promote_vendor"), 303, "")
	if e.imageRows(vendorAvatar.id) != 1 || agStr(e, "SELECT staff_hidden::text FROM users WHERE id=$1", vendorID) != "true" {
		t.Fatal("promotion deleted the avatar or did not start hidden")
	}
	e.wantHidden(vendorAvatar, "new moderator's avatar, hidden by default")
	agExec(e, "UPDATE settings SET value='true' WHERE key='staff_list_public'")
	e.wantHidden(vendorAvatar, "new moderator's avatar with the list on, still hidden by default")
	// A moderator who chose to be listed, then was demoted and promoted again, starts hidden again: the choice
	// was made as staff and does not outlive the role (PR #28 re-promotion follow-up).
	listedID, _ := e.user("listed_mod", "moderator")
	agExec(e, "UPDATE users SET staff_hidden=false WHERE id=$1", listedID)
	agExpect(e, "/admin", admin, form("action", "role", "password", testPassword, "role", "vendor", "handle", "listed_mod"), 303, "")
	agExpect(e, "/admin", admin, form("action", "role", "password", testPassword, "role", "moderator", "handle", "listed_mod"), 303, "")
	if agStr(e, "SELECT staff_hidden::text FROM users WHERE id=$1", listedID) != "true" {
		t.Fatal("a re-promoted moderator kept the public staff list choice from before the demotion")
	}
}
