package market

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A-188 (war room 2026-09-27; owner decision 1): the upload forms on listing-edit and /account, the scanner
// states they show, the owner's own removal of a photo or avatar, and the staff "hide me" choice (PostgreSQL).

// sectionOf cuts the element with id="<id>" out of a page, up to the next </section>.
func sectionOf(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no #%s section in:\n%s", id, page)
	}
	rest := page[i:]
	if j := strings.Index(rest, "</section>"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// multipartFormFirstInput is the first <input> after the multipart form's opening tag.
var multipartFormFirstInput = regexp.MustCompile(`<form[^>]*enctype="multipart/form-data"[^>]*>\s*<input ([^>]*)>`)

// assertUploadForm checks a Ready upload form: multipart to action, csrf first, a file input taking JPEG and PNG
// that never asks for the camera, and no disabled fieldset.
func assertUploadForm(t *testing.T, section, action, session string, e *testEnv) {
	t.Helper()
	if !strings.Contains(section, `action="`+action+`"`) || !strings.Contains(section, `method="post"`) {
		t.Fatalf("no post form to %s:\n%s", action, section)
	}
	m := multipartFormFirstInput.FindStringSubmatch(section)
	if m == nil || !strings.Contains(m[1], `name="csrf"`) || !strings.Contains(m[1], `type="hidden"`) || !strings.Contains(m[1], e.A.csrf(session)) {
		t.Fatalf("the multipart form does not start with the csrf input:\n%s", section)
	}
	if !strings.Contains(section, `type="file" name="photo" required accept="image/jpeg,image/png"`) || strings.Contains(section, "capture") {
		t.Fatalf("file input:\n%s", section)
	}
	if strings.Contains(section, "<fieldset disabled") {
		t.Fatalf("Ready form is disabled:\n%s", section)
	}
}

// The panels follow the scanner's cached state: Ready shows the forms; Unavailable and stale show them inside a
// disabled fieldset with the paused copy, and a post is refused with a page; Disabled shows no file input at
// all, only the "not set up" line, and a post is refused with a page. Nothing is stored in any of them.
func TestUploadScannerDownFailsClosedWithPage(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, vendor := e.user("forms_state_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	edit := "/listing-edit?id=" + listing
	get := func(path string) string {
		t.Helper()
		w := e.do("GET", path, vendor, nil)
		e.check(w, 200)
		return w.Body.String()
	}
	photos := sectionOf(t, get(edit), "photos")
	assertUploadForm(t, photos, "/listings/photo", vendor, e)
	for _, want := range []string{`name="id" value="` + listing + `"`, `aria-describedby="photo-help photo-privacy"`, `id="photo-help"`, `id="photo-privacy"`,
		"4096 pixels a side and 4096×3072 in all (about 12.6 megapixels)", "Uploading can take up to 90 seconds over Tor; press Upload once and wait for the page to change",
		"Each file is checked by the ClamAV malware scanner; a match is refused and nothing is stored", "Screenshots can show your clock and timezone", "0 of 4 photos"} {
		if !strings.Contains(photos, want) {
			t.Fatalf("photo panel lacks %q:\n%s", want, photos)
		}
	}
	if strings.Index(photos, `id="photo-privacy"`) > strings.Index(photos, `type="file"`) || strings.Index(photos, `id="photo-help"`) > strings.Index(photos, `type="file"`) {
		t.Fatal("the limits and the privacy warning must come before the file input")
	}
	assertUploadForm(t, sectionOf(t, get("/account"), "avatar"), "/account/avatar", vendor, e)

	const paused = "Photo uploads are paused: the malware scanner is not responding. Listing details can still be saved."
	stale := html.EscapeString("Photo uploads are paused: the malware scanner's signatures are older than 30 days.")
	e.A.scanner = newImageScanner(scannerConfig{address: "127.0.0.1:1", maxAgeDays: defaultSignatureAgeDays})
	e.A.scanner.poll(t.Context()) // connection refused: Unavailable
	for _, state := range []struct {
		name, notice string
		setup        func()
	}{
		{"unavailable", paused, func() {}},
		{"stale", stale, func() {
			f.SetVersion(fakeClamdVersion(31 * 24 * time.Hour))
			e.withScanner(f)
		}},
	} {
		state.setup()
		for _, page := range []struct{ path, id string }{{edit, "photos"}, {"/account", "avatar"}} {
			s := sectionOf(t, get(page.path), page.id)
			want := state.notice
			if page.id == "avatar" { // the account page has no listing details to save
				want = strings.TrimSuffix(want, " Listing details can still be saved.")
				if strings.Contains(s, "Listing details") {
					t.Fatalf("%s: the avatar panel mentions listing details:\n%s", state.name, s)
				}
			}
			if !strings.Contains(s, want) || !strings.Contains(s, "<fieldset disabled") || !strings.Contains(s, `type="file"`) {
				t.Fatalf("%s %s: want the paused copy and a disabled fieldset:\n%s", state.name, page.path, s)
			}
			if strings.Index(s, "<fieldset disabled") > strings.Index(s, `type="file"`) {
				t.Fatalf("%s %s: the file input is outside the disabled fieldset", state.name, page.path)
			}
		}
		checkPage(t, e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, smallJPEG(t))...), 503, state.notice)
		checkPage(t, e.upload("/account/avatar", vendor, e.avatarParts(vendor, smallJPEG(t))...), 503, state.notice)
	}

	const off = "Photo uploads are off on this market: they require the ClamAV malware scanner, which the operator has not set up."
	e.A.scanner = nil
	for _, page := range []struct{ path, id string }{{edit, "photos"}, {"/account", "avatar"}} {
		s := sectionOf(t, get(page.path), page.id)
		if !strings.Contains(s, off) || strings.Contains(s, `type="file"`) || strings.Contains(s, "multipart") {
			t.Fatalf("%s: want the off copy and no file input:\n%s", page.path, s)
		}
	}
	checkPage(t, e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, smallJPEG(t))...), 503, off)
	if e.imageCount() != 0 {
		t.Fatalf("%d images stored while the scanner was not Ready", e.imageCount())
	}
	// Listing details still save while uploads are off.
	w := e.do("POST", "/listings/update", vendor, inventoryForm(listing, map[string]string{"stock_seen": "5"}))
	e.check(w, 303)
}

// photoID is the id of the only photo on listing.
func photoIDOf(t *testing.T, e *testEnv, listing string) string {
	t.Helper()
	var id string
	if err := e.DB.QueryRow("SELECT id FROM images WHERE product_id=$1", listing).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A vendor sees a stored photo listed from the database after the upload's redirect, with its size and
// description, and removes it with its own form: the row is deleted, the removal audited, the image gone from
// both routes. An administrator removing a vendor's photo is audited as such and the vendor is notified.
func TestVendorRemovesOwnPhoto(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, vendor := e.user("remove_own_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	parts := e.photoParts(vendor, listing, smallJPEG(t))
	parts = append(parts[:2], append([]part{{name: "description", body: []byte("A blue box on a table")}}, parts[2:]...)...)
	w := e.upload("/listings/photo", vendor, parts...)
	if w.Code != 303 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, smallJPEG(t))...)
	loc, _, _ := strings.Cut(w.Header().Get("Location"), "#") // browsers never send the fragment
	page := e.do("GET", loc, vendor, nil)
	e.check(page, 200)
	body := page.Body.String()
	if !strings.Contains(body, "Photo saved as a re-encoded JPEG; location, camera and other metadata were removed. Check the picture itself for anything that identifies you.") {
		t.Fatalf("no success notice after %s", loc)
	}
	var ids []string
	var kb int
	rows, err := e.DB.Query(`SELECT id,(octet_length("full")+1023)/1024 FROM images WHERE product_id=$1 ORDER BY created,id`, listing)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		rows.Scan(&id, &kb)
		ids = append(ids, id)
	}
	rows.Close()
	photos := sectionOf(t, body, "photos")
	for _, want := range []string{deskImageURL(ids[0], false), deskImageURL(ids[1], false), "JPEG, 200×150, ", "A blue box on a table", "No description", "2 of 4 photos · 0.1 MB of 50 MB",
		`action="/listings/photo/remove"`, `name="id" value="` + ids[0] + `"`} {
		if !strings.Contains(photos, want) {
			t.Fatalf("photo list lacks %q:\n%s", want, photos)
		}
	}
	if strings.Contains(photos, "/img/"+ids[0]) && !strings.Contains(photos, "/desk/img/"+ids[0]) {
		t.Fatal("the owner's list must use the desk route")
	}
	// The remove forms are urlencoded: no enctype on them.
	for _, f := range regexp.MustCompile(`<form[^>]*action="/listings/photo/remove"[^>]*>`).FindAllString(photos, -1) {
		if strings.Contains(f, "enctype") {
			t.Fatalf("remove form %s is not urlencoded", f)
		}
	}
	w = e.do("POST", "/listings/photo/remove", vendor, url.Values{"id": {ids[0]}})
	if w.Code != 303 || w.Header().Get("Location") != "/listing-edit?id="+listing+"&photo=removed#photos" {
		t.Fatalf("remove: %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	var left int
	e.DB.QueryRow("SELECT count(*) FROM images WHERE id=$1", ids[0]).Scan(&left)
	if left != 0 || e.imageCount() != 1 {
		t.Fatalf("after removal: row %d, %d images", left, e.imageCount())
	}
	var audit string
	e.DB.QueryRow("SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id DESC LIMIT 1", vendorID).Scan(&audit)
	if audit != "Removed a photo from listing "+listing[:8] {
		t.Fatalf("audit %q", audit)
	}
	for _, path := range []string{deskImageURL(ids[0], false), deskImageURL(ids[0], true), publicImageURL(ids[0], false)} {
		if w := e.do("GET", path, vendor, nil); w.Code != 404 {
			t.Fatalf("%s after removal: %d", path, w.Code)
		}
	}
	after := e.do("GET", "/listing-edit?id="+listing+"&photo=removed", vendor, nil).Body.String()
	if !strings.Contains(after, "Photo removed.") || strings.Contains(after, ids[0]) || !strings.Contains(after, "1 of 4 photos") {
		t.Fatalf("page after removal:\n%s", sectionOf(t, after, "photos"))
	}
	e.check(e.do("POST", "/listings/photo/remove", vendor, url.Values{"id": {ids[0]}}), 404) // already gone

	// An administrator removes the vendor's other photo: audited as administrator, the vendor notified.
	adminID, admin := e.user("remove_admin", "admin")
	e.check(e.do("POST", "/listings/photo/remove", admin, url.Values{"id": {ids[1]}}), 303)
	e.DB.QueryRow("SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id DESC LIMIT 1", adminID).Scan(&audit)
	if audit != "Removed a photo from listing "+listing[:8]+" as administrator" {
		t.Fatalf("admin audit %q", audit)
	}
	var note string
	e.DB.QueryRow("SELECT body FROM notifications WHERE user_id=$1", vendorID).Scan(&note)
	if note != "An administrator removed a photo from your listing "+listing[:8]+"." {
		t.Fatalf("notification %q", note)
	}
	if e.imageCount() != 0 {
		t.Fatalf("%d images left", e.imageCount())
	}
}

// Another vendor, a buyer and a moderator cannot remove a vendor's photo; an unknown id and an avatar's id get
// the same answer as another vendor's photo, so ids cannot be probed. Nothing is deleted.
func TestVendorCannotRemoveOthersPhoto(t *testing.T) {
	e, _ := uploadEnv(t)
	ownerID, owner := e.user("photo_owner", "vendor")
	_, other := e.user("photo_other", "vendor")
	_, buyer := e.user("photo_buyer", "buyer")
	_, moderator := e.user("photo_moderator", "moderator")
	listing := e.product(ownerID, "physical")
	if w := e.upload("/listings/photo", owner, e.photoParts(owner, listing, smallJPEG(t))...); w.Code != 303 {
		t.Fatalf("upload %d", w.Code)
	}
	if w := e.upload("/account/avatar", owner, e.avatarParts(owner, smallJPEG(t))...); w.Code != 303 {
		t.Fatalf("avatar %d", w.Code)
	}
	photo := photoIDOf(t, e, listing)
	var avatar string
	e.DB.QueryRow("SELECT id FROM images WHERE kind='avatar'").Scan(&avatar)
	var bodies []string
	for _, id := range []string{photo, randomToken()[:32], avatar} {
		w := e.do("POST", "/listings/photo/remove", other, url.Values{"id": {id}})
		e.check(w, 404)
		bodies = append(bodies, w.Body.String())
	}
	if bodies[0] != bodies[1] || bodies[0] != bodies[2] {
		t.Fatalf("answers differ: %q", bodies)
	}
	e.check(e.do("POST", "/listings/photo/remove", buyer, url.Values{"id": {photo}}), 403)
	e.check(e.do("POST", "/listings/photo/remove", moderator, url.Values{"id": {photo}}), 403)
	// The other vendor's listing-edit for the owner's listing is a 404 too, so the list is never shown.
	e.check(e.do("GET", "/listing-edit?id="+listing, other, nil), 404)
	if e.imageCount() != 2 {
		t.Fatalf("%d images after refused removals", e.imageCount())
	}
}

// Vendors, administrators and moderators get the avatar form on /account, see their current avatar through the
// desk route and remove it; staff also read that a staff photo links them across every dispute decision. A buyer
// gets no avatar form. The removal takes no id: it is always the poster's own.
func TestAvatarFormAndRemoval(t *testing.T) {
	e, _ := uploadEnv(t)
	_, buyer := e.user("avatar_form_buyer", "buyer")
	if body := e.do("GET", "/account", buyer, nil).Body.String(); strings.Contains(body, `id="avatar"`) || strings.Contains(body, "/account/avatar") {
		t.Fatal("a buyer is offered the avatar form")
	}
	const staffLine = "a staff photo links your identity across every dispute decision"
	for _, role := range []string{"vendor", "admin", "moderator"} {
		id, s := e.user("avatar_form_"+role, role)
		section := sectionOf(t, e.do("GET", "/account", s, nil).Body.String(), "avatar")
		assertUploadForm(t, section, "/account/avatar", s, e)
		if staff := role != "vendor"; strings.Contains(section, staffLine) != staff {
			t.Fatalf("%s: staff privacy line shown=%v:\n%s", role, !staff, section)
		}
		if strings.Contains(section, "/account/avatar/remove") {
			t.Fatalf("%s: remove form without an avatar", role)
		}
		w := e.upload("/account/avatar", s, e.avatarParts(s, smallJPEG(t))...)
		if w.Code != 303 {
			t.Fatalf("%s upload: %d %s", role, w.Code, w.Body.String())
		}
		var avatar string
		e.DB.QueryRow("SELECT id FROM images WHERE owner_user_id=$1 AND kind='avatar'", id).Scan(&avatar)
		loc, _, _ := strings.Cut(w.Header().Get("Location"), "#")
		page := e.do("GET", loc, s, nil).Body.String()
		section = sectionOf(t, page, "avatar")
		if !strings.Contains(page, "Avatar saved as a re-encoded JPEG") || !strings.Contains(section, deskImageURL(avatar, true)) || !strings.Contains(section, `action="/account/avatar/remove"`) {
			t.Fatalf("%s: avatar not shown after upload:\n%s", role, section)
		}
		if w := e.do("GET", deskImageURL(avatar, true), s, nil); w.Code != 200 {
			t.Fatalf("%s: own avatar on the desk route: %d", role, w.Code)
		}
	}
	// Removal: the poster's own avatar only, whatever the form says.
	ownerID, owner := e.user("avatar_remove_owner", "vendor")
	otherID, other := e.user("avatar_remove_other", "vendor")
	for _, s := range []string{owner, other} {
		if w := e.upload("/account/avatar", s, e.avatarParts(s, smallJPEG(t))...); w.Code != 303 {
			t.Fatalf("upload %d", w.Code)
		}
	}
	w := e.do("POST", "/account/avatar/remove", owner, url.Values{"id": {otherID}, "user_id": {otherID}})
	if w.Code != 303 || w.Header().Get("Location") != "/account?avatar=removed#avatar" {
		t.Fatalf("remove: %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	count := func(uid string) (n int) {
		e.DB.QueryRow("SELECT count(*) FROM images WHERE owner_user_id=$1", uid).Scan(&n)
		return n
	}
	if count(ownerID) != 0 || count(otherID) != 1 {
		t.Fatalf("owner %d other %d avatars after removal", count(ownerID), count(otherID))
	}
	var audit string
	e.DB.QueryRow("SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id DESC LIMIT 1", ownerID).Scan(&audit)
	if audit != "Removed avatar" {
		t.Fatalf("audit %q", audit)
	}
	page := e.do("GET", "/account?avatar=removed", owner, nil).Body.String()
	if !strings.Contains(page, "Avatar removed.") || strings.Contains(sectionOf(t, page, "avatar"), "/account/avatar/remove") {
		t.Fatalf("page after removal:\n%s", sectionOf(t, page, "avatar"))
	}
	e.check(e.do("POST", "/account/avatar/remove", owner, nil), 409) // nothing left to remove
	// A demoted account keeps a way to remove the avatar it no longer shows.
	e.DB.Exec("UPDATE users SET role='buyer' WHERE id=$1", otherID)
	section := sectionOf(t, e.do("GET", "/account", other, nil).Body.String(), "avatar")
	if strings.Contains(section, `type="file"`) || !strings.Contains(section, `action="/account/avatar/remove"`) {
		t.Fatalf("demoted account's avatar section:\n%s", section)
	}
	e.check(e.do("POST", "/account/avatar/remove", other, nil), 303)
	if count(otherID) != 0 {
		t.Fatal("demoted account's avatar not removed")
	}
}

// A moderator uploads an avatar through the same pipeline as a vendor and sees it on /account.
func TestModeratorAvatarUpload(t *testing.T) {
	e, f := uploadEnv(t)
	id, moderator := e.user("avatar_moderator", "moderator")
	w := e.upload("/account/avatar", moderator, e.avatarParts(moderator, fixturePNG(t, fixturePicture(300, 200)))...)
	if w.Code != 303 || w.Header().Get("Location") != "/account?avatar=saved#avatar" || scansOf(f) != 1 {
		t.Fatalf("moderator avatar: %d %q scans %d", w.Code, w.Header().Get("Location"), scansOf(f))
	}
	var avatar string
	if err := e.DB.QueryRow("SELECT id FROM images WHERE owner_user_id=$1 AND kind='avatar' AND width=200 AND height=200", id).Scan(&avatar); err != nil {
		t.Fatal(err)
	}
	section := sectionOf(t, e.do("GET", "/account", moderator, nil).Body.String(), "avatar")
	if !strings.Contains(section, deskImageURL(avatar, true)) {
		t.Fatalf("moderator's avatar not shown:\n%s", section)
	}
}

// Owner decision 1(b): moderators and administrators choose whether to be hidden from the public staff list
// (hidden by default). Each change is audited old→new with fixed copy; saving the same choice records nothing;
// vendors and buyers can neither see nor set it.
func TestStaffHideChoiceAudited(t *testing.T) {
	e := newTestApp(t)
	hidden := func(id string) (h bool) {
		t.Helper()
		if err := e.DB.QueryRow("SELECT staff_hidden FROM users WHERE id=$1", id).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	audits := func(id string) (actions []string) {
		rows, _ := e.DB.Query("SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id", id)
		defer rows.Close()
		for rows.Next() {
			var a string
			rows.Scan(&a)
			actions = append(actions, a)
		}
		return actions
	}
	for _, role := range []string{"moderator", "admin"} {
		id, s := e.user("hide_"+role, role)
		section := sectionOf(t, e.do("GET", "/account", s, nil).Body.String(), "staff-list")
		if !strings.Contains(section, `action="/account/staff-list"`) || !strings.Contains(section, "Hide me from the public staff list") ||
			!regexp.MustCompile(`name="hide" value="yes" checked`).MatchString(section) || strings.Contains(section, "multipart") ||
			!strings.Contains(section, "hidden staff avatars are shown only to the parties of orders they decided") {
			t.Fatalf("%s: staff list choice:\n%s", role, section)
		}
		w := e.do("POST", "/account/staff-list", s, nil) // the box unticked: shown
		if w.Code != 303 || w.Header().Get("Location") != "/account?saved=1#staff-list" || hidden(id) {
			t.Fatalf("%s show: %d %q hidden=%v", role, w.Code, w.Header().Get("Location"), hidden(id))
		}
		section = sectionOf(t, e.do("GET", "/account", s, nil).Body.String(), "staff-list")
		if strings.Contains(section, "checked") {
			t.Fatalf("%s: box still ticked after choosing to be shown", role)
		}
		e.check(e.do("POST", "/account/staff-list", s, nil), 303) // unchanged: no audit row
		e.check(e.do("POST", "/account/staff-list", s, url.Values{"hide": {"yes"}}), 303)
		if !hidden(id) {
			t.Fatalf("%s: not hidden again", role)
		}
		want := []string{"Public staff list choice changed: hidden → shown", "Public staff list choice changed: shown → hidden"}
		if got := audits(id); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%s audit %q, want %q", role, got, want)
		}
	}
	for _, role := range []string{"vendor", "buyer"} {
		id, s := e.user("hide_"+role, role)
		if body := e.do("GET", "/account", s, nil).Body.String(); strings.Contains(body, "/account/staff-list") || strings.Contains(body, "Hide me from the public staff list") {
			t.Fatalf("%s is offered the staff list choice", role)
		}
		e.check(e.do("POST", "/account/staff-list", s, nil), 403)
		if !hidden(id) || len(audits(id)) != 0 {
			t.Fatalf("%s changed the flag or was audited", role)
		}
	}
}

// A listing that cannot take another photo says why instead of showing the form (the post would be refused
// with 409), and still lists its photos with their remove forms.
func TestPhotoFormBlockedStates(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, vendor := e.user("blocked_photos_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	for range photosPerListing {
		if _, err := e.DB.Exec(`INSERT INTO images(id,owner_user_id,product_id,kind,"full",thumb,width,height) VALUES($1,$2,$3,'photo','\x00','\x00',1,1)`, randomToken()[:32], vendorID, listing); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ setup, want string }{
		{"", "This listing has 4 photos, the most allowed. Remove one to add another."},
		{"UPDATE products SET archived=true,archived_at=now() WHERE id=$1", "This listing is archived. Restore it before adding photos."},
		{"UPDATE products SET admin_archive_reason='Taken down for the test' WHERE id=$1", "An administrator took this listing down, so photos cannot be added to it."},
	} {
		if c.setup != "" {
			if _, err := e.DB.Exec(c.setup, listing); err != nil {
				t.Fatal(err)
			}
		}
		s := sectionOf(t, e.do("GET", "/listing-edit?id="+listing, vendor, nil).Body.String(), "photos")
		if !strings.Contains(s, c.want) || strings.Contains(s, `type="file"`) || strings.Count(s, `action="/listings/photo/remove"`) != photosPerListing || !strings.Contains(s, "4 of 4 photos") {
			t.Fatalf("want %q, no file input and 4 remove forms:\n%s", c.want, s)
		}
	}
}

// Publishing a new listing lands on its edit page's photo panel.
func TestPublishLandsOnPhotos(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("publish_photos", "vendor")
	w := e.do("POST", "/listings", vendor, inventoryForm("", nil))
	e.check(w, 303)
	var id string
	if err := e.DB.QueryRow("SELECT id FROM products WHERE vendor_id=$1", vendorID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if loc := w.Header().Get("Location"); loc != "/listing-edit?id="+id+"&saved=1#photos" {
		t.Fatalf("redirect %q", loc)
	}
	page := e.do("GET", "/listing-edit?id="+id+"&saved=1", vendor, nil)
	e.check(page, 200)
	if !strings.Contains(page.Body.String(), `id="photos"`) {
		t.Fatal("no photo panel on the new listing's edit page")
	}
}
