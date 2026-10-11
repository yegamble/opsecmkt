package market

import (
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func renderedActionNonces(t *testing.T, body, action string) []string {
	t.Helper()
	forms := regexp.MustCompile(`(?s)<form\b[^>]*action="`+regexp.QuoteMeta(action)+`"[^>]*>.*?</form>`).FindAllString(body, -1)
	if len(forms) == 0 {
		t.Fatalf("no forms for %s", action)
	}
	nonceRE := regexp.MustCompile(`name="form_nonce" value="([0-9a-f]{32})"`)
	seen := make(map[string]bool, len(forms))
	nonces := make([]string, 0, len(forms))
	for _, form := range forms {
		match := nonceRE.FindStringSubmatch(form)
		if match == nil || seen[match[1]] {
			t.Fatalf("form for %s has no unique nonce:\n%s", action, form)
		}
		seen[match[1]] = true
		nonces = append(nonces, match[1])
	}
	return nonces
}

func TestMediaRemovalFormsRenderNoncesWithoutReceipts(t *testing.T) {
	e := newTestApp(t)
	ownerID, owner := e.user("nonce_media_owner", "vendor")
	_, moderator := e.user("nonce_media_moderator", "moderator")
	listing := e.product(ownerID, "physical")
	e.image(ownerID, listing, "photo")
	e.image(ownerID, listing, "photo")
	e.image(ownerID, "", "avatar")

	if got := submissionCount(t, e); got != 0 {
		t.Fatalf("fixture creation wrote %d form receipts", got)
	}
	edit := e.do("GET", "/listing-edit?id="+listing, owner, nil)
	e.check(edit, 200)
	if got := len(renderedActionNonces(t, edit.Body.String(), "/listings/photo/remove")); got != 2 {
		t.Fatalf("listing edit rendered %d photo removal forms", got)
	}
	account := e.do("GET", "/account", owner, nil)
	e.check(account, 200)
	if got := len(renderedActionNonces(t, account.Body.String(), "/account/avatar/remove")); got != 1 {
		t.Fatalf("account rendered %d avatar removal forms", got)
	}
	product := e.do("GET", "/product?id="+listing, moderator, nil)
	e.check(product, 200)
	if got := len(renderedActionNonces(t, product.Body.String(), "/images/remove")); got != 2 {
		t.Fatalf("product rendered %d staff photo takedown forms", got)
	}
	vendor := e.do("GET", "/vendor?id="+ownerID, moderator, nil)
	e.check(vendor, 200)
	if got := len(renderedActionNonces(t, vendor.Body.String(), "/images/remove")); got != 1 {
		t.Fatalf("vendor page rendered %d staff avatar takedown forms", got)
	}
	recent := e.do("GET", "/moderator", moderator, nil)
	e.check(recent, 200)
	if got := len(renderedActionNonces(t, recent.Body.String(), "/images/remove")); got != 3 {
		t.Fatalf("moderator recent images rendered %d takedown forms", got)
	}
	if got := submissionCount(t, e); got != 0 {
		t.Fatalf("GET rendering wrote %d form receipts", got)
	}
}

func TestMediaRemovalReceiptsReplayAndBindActorSessionAndBody(t *testing.T) {
	e := newTestApp(t)
	ownerID, owner := e.user("nonce_media_replay_owner", "vendor")
	_, other := e.user("nonce_media_replay_other", "vendor")
	moderatorID, moderator := e.user("nonce_media_replay_mod", "moderator")
	_, admin := e.user("nonce_media_replay_admin", "admin")
	listing := e.product(ownerID, "physical")
	photo := e.image(ownerID, listing, "photo")
	avatar := e.image(ownerID, "", "avatar")

	edit := e.do("GET", "/listing-edit?id="+listing, owner, nil)
	e.check(edit, 200)
	photoForm := url.Values{"id": {photo.id}, formNonceField: {renderedFormNonce(t, edit.Body.String(), "/listings/photo/remove")}}
	photoFirst := e.do("POST", "/listings/photo/remove", owner, photoForm)
	e.check(photoFirst, 303)
	photoReplay := e.do("POST", "/listings/photo/remove", owner, photoForm)
	e.check(photoReplay, 303)
	if photoFirst.Header().Get("Location") != photoReplay.Header().Get("Location") || noticeSetCookie(photoReplay) == nil || noticeSetCookie(photoReplay).Value != "photo-removed" {
		t.Fatal("photo removal replay did not return its original redirect and notice")
	}
	otherSession := randomToken()
	if _, err := e.DB.Exec("INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '1 hour')", digest(otherSession), ownerID); err != nil {
		t.Fatal(err)
	}
	changedPhoto := url.Values{}
	for key, values := range photoForm {
		changedPhoto[key] = values
	}
	changedPhoto.Set("id", randomToken()[:32])
	photoBodyMismatch := e.do("POST", "/listings/photo/remove", owner, changedPhoto)
	e.check(photoBodyMismatch, 409)
	if got := e.do("POST", "/listings/photo/remove", otherSession, photoForm); got.Code != 409 || got.Body.String() != photoBodyMismatch.Body.String() {
		t.Fatalf("same actor with a different session got a distinguishable response: %d %q", got.Code, got.Body.String())
	}
	// A new rendered submission after the removal follows the route's existing unknown-image 404 policy.
	stalePhoto := url.Values{"id": {photo.id}, formNonceField: {randomToken()[:32]}}
	e.check(e.do("POST", "/listings/photo/remove", owner, stalePhoto), 404)
	if e.imageRows(photo.id) != 0 || listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", ownerID, "Removed a photo from listing "+listing[:8]) != 1 {
		t.Fatal("photo replay duplicated effects or failed to delete the photo")
	}

	account := e.do("GET", "/account", owner, nil)
	e.check(account, 200)
	avatarForm := url.Values{formNonceField: {renderedFormNonce(t, account.Body.String(), "/account/avatar/remove")}}
	avatarFirst := e.do("POST", "/account/avatar/remove", owner, avatarForm)
	e.check(avatarFirst, 303)
	if got := e.do("POST", "/account/avatar/remove", owner, avatarForm); got.Code != 303 || got.Header().Get("Location") != avatarFirst.Header().Get("Location") {
		t.Fatalf("avatar replay: %d %q", got.Code, got.Header().Get("Location"))
	}
	if got := e.do("POST", "/account/avatar/remove", other, avatarForm); got.Code != 409 {
		t.Fatalf("another actor using the avatar receipt: %d %s", got.Code, got.Body.String())
	}
	if got := e.do("POST", "/account/avatar/remove", owner, url.Values{formNonceField: {randomToken()[:32]}}); got.Code != 409 {
		t.Fatalf("distinct removal after the avatar was deleted: %d", got.Code)
	}
	if e.imageRows(avatar.id) != 0 || listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Removed avatar'", ownerID) != 1 {
		t.Fatal("avatar replay duplicated effects or failed to delete the avatar")
	}

	staffAvatar := e.image(ownerID, "", "avatar")
	vendorPage := e.do("GET", "/vendor?id="+ownerID, moderator, nil)
	e.check(vendorPage, 200)
	staffForm := imageTakedownForm(t, vendorPage.Body.String(), staffAvatar.id, imageTakedownReason)
	staffFirst := e.do("POST", "/images/remove", moderator, staffForm)
	e.check(staffFirst, 303)
	changedReason := url.Values{}
	for key, values := range staffForm {
		changedReason[key] = values
	}
	changedReason.Set("reason", "A different reason for this removed avatar.")
	e.check(e.do("POST", "/images/remove", moderator, changedReason), 409)
	if got := e.do("POST", "/images/remove", admin, staffForm); got.Code != 409 {
		t.Fatalf("different staff actor using takedown receipt: %d %s", got.Code, got.Body.String())
	}
	staffReplay := e.do("POST", "/images/remove", moderator, staffForm)
	e.check(staffReplay, 303)
	if staffFirst.Header().Get("Location") != staffReplay.Header().Get("Location") || noticeSetCookie(staffReplay) == nil || noticeSetCookie(staffReplay).Value != "image-removed-page" {
		t.Fatal("staff takedown replay did not return its original result")
	}
	if e.imageRows(staffAvatar.id) != 0 || listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", moderatorID, "Took down the avatar of nonce_media_replay_owner: "+imageTakedownReason) != 1 || listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", ownerID, "Avatar removed by staff: "+imageTakedownReason) != 1 || len(e.notes(ownerID)) != 1 {
		t.Fatal("staff takedown replay duplicated or omitted its durable effects")
	}
	var receipts string
	if err := e.DB.QueryRow("SELECT string_agg(s::text, '') FROM form_submissions s").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{photo.id, avatar.id, imageTakedownReason, "A different reason for this removed avatar."} {
		if strings.Contains(receipts, private) {
			t.Fatalf("receipt retained submitted image data or reason %q", private)
		}
	}
	if listingFormCount(t, e, "SELECT count(*) FROM form_submissions WHERE action_path IN ('/listings/photo/remove','/account/avatar/remove','/images/remove')") != 3 {
		t.Fatal("refused or replayed removals created extra receipts")
	}
}

func TestConcurrentPhotoRemovalStoresOneEffect(t *testing.T) {
	e := newTestApp(t)
	ownerID, owner := e.user("nonce_media_race_owner", "vendor")
	listing := e.product(ownerID, "physical")
	photo := e.image(ownerID, listing, "photo")
	page := e.do("GET", "/listing-edit?id="+listing, owner, nil)
	e.check(page, 200)
	form := url.Values{"id": {photo.id}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/listings/photo/remove")}}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- e.do("POST", "/listings/photo/remove", owner, form).Code
		}()
	}
	wg.Wait()
	close(results)
	for code := range results {
		if code != 303 {
			t.Fatalf("concurrent exact photo removal returned %d", code)
		}
	}
	if e.imageRows(photo.id) != 0 || listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", ownerID, "Removed a photo from listing "+listing[:8]) != 1 || submissionCount(t, e) != 1 {
		t.Fatal("concurrent photo removal did not commit exactly one deletion, audit and receipt")
	}
}
