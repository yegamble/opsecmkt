package market

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// A-287: the outcome flags that were URL-driven (?photo=, ?avatar=, ?image=removed, ?key=, ?signin=) are one-shot
// notices (A-117). oneShot checks one: the POST's 303 went to want (no flag) and set the notice cookie to code; the
// first render of that page shows text n times (the page and, for a status notice, the title), inside section when
// it has one; a reload or Back (the cookie is gone) shows nothing; and crafted, the page with the old flag and no
// cookie, shows nothing.
func oneShot(t *testing.T, e *testEnv, sess string, w *httptest.ResponseRecorder, want, code, text, section string, n int, crafted string) {
	t.Helper()
	if w.Code != 303 || w.Header().Get("Location") != want {
		t.Fatalf("POST: %d %q, want 303 %q: %s", w.Code, w.Header().Get("Location"), want, w.Body.String())
	}
	c := noticeSetCookie(w)
	if c == nil || c.Value != code {
		t.Fatalf("POST set notice %+v, want %q", c, code)
	}
	page, _, _ := strings.Cut(want, "#") // browsers never send the fragment
	first := e.do("GET", page, sess, nil, &http.Cookie{Name: noticeCookie, Value: c.Value})
	e.check(first, 200)
	b := html.UnescapeString(first.Body.String())
	if got := strings.Count(b, text); got != n {
		t.Fatalf("first render of %s shows %q %d times, want %d:\n%s", page, text, got, n, b)
	}
	if section != "" && !strings.Contains(sectionOf(t, b, section), text) {
		t.Fatalf("notice %q not in #%s:\n%s", code, section, sectionOf(t, b, section))
	}
	if cl := noticeSetCookie(first); cl == nil || cl.MaxAge >= 0 {
		t.Fatalf("first render did not clear the notice cookie: %+v", cl)
	}
	for _, again := range []string{page, crafted} {
		if b := html.UnescapeString(e.body("GET", again, sess, nil, 200)); strings.Contains(b, text) {
			t.Fatalf("GET %s without the cookie shows %q", again, text)
		}
	}
}

func TestOutcomeFlagsAreOneShotNotices(t *testing.T) {
	const (
		photoSaved    = "Photo saved as a re-encoded JPEG; location, camera and other metadata were removed. Check the picture itself for anything that identifies you."
		avatarSaved   = "Avatar saved as a re-encoded JPEG; location, camera and other metadata were removed. Check the picture itself for anything that identifies you."
		imageRemoved  = "Image removed. Its owner was notified with your reason."
		keyKept       = "Key updated: the same key (same fingerprint) with new signatures, such as a renewed expiry; its ownership proof and PGP sign-in were kept."
		keyRenewed    = "Key updated: the same key (same fingerprint) with new signatures, such as a renewed expiry."
		keyRevocation = "Your key was saved as a changed key, not a renewal: the pasted key is missing a revocation signature"
	)
	e, _ := uploadEnv(t)

	t.Run("photo", func(t *testing.T) {
		vendorID, vendor := e.user("a287_photo", "vendor")
		listing := e.product(vendorID, "physical")
		edit := "/listing-edit?id=" + listing
		w := e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, smallJPEG(t))...)
		oneShot(t, e, vendor, w, edit+"#photos", "photo-saved", photoSaved, "photos", 2, edit+"&photo=saved")
		w = e.do("POST", "/listings/photo/remove", vendor, url.Values{"id": {photoIDOf(t, e, listing)}})
		oneShot(t, e, vendor, w, edit+"#photos", "photo-removed", "Photo removed.", "photos", 2, edit+"&photo=removed")
	})

	t.Run("avatar", func(t *testing.T) {
		_, vendor := e.user("a287_avatar", "vendor")
		w := e.upload("/account/avatar", vendor, e.avatarParts(vendor, smallJPEG(t))...)
		oneShot(t, e, vendor, w, "/account#avatar", "avatar-saved", avatarSaved, "avatar", 2, "/account?avatar=saved")
		w = e.do("POST", "/account/avatar/remove", vendor, nil)
		oneShot(t, e, vendor, w, "/account#avatar", "avatar-removed", "Avatar removed.", "avatar", 2, "/account?avatar=removed")

		// A demoted account removes the avatar it can no longer replace: its section, which then has neither an
		// avatar nor a form, still carries the notice.
		demotedID, demoted := e.user("a287_demoted", "vendor")
		if w := e.upload("/account/avatar", demoted, e.avatarParts(demoted, smallJPEG(t))...); w.Code != 303 {
			t.Fatalf("upload %d", w.Code)
		}
		agExec(e, "UPDATE users SET role='buyer' WHERE id=$1", demotedID)
		w = e.do("POST", "/account/avatar/remove", demoted, nil)
		oneShot(t, e, demoted, w, "/account#avatar", "avatar-removed", "Avatar removed.", "avatar", 2, "/account?avatar=removed")
	})

	t.Run("image-takedown", func(t *testing.T) {
		vendorID, _ := e.user("a287_td_vendor", "vendor")
		_, moderator := e.user("a287_td_moderator", "moderator")
		listing := e.product(vendorID, "physical")
		w := e.takeDown(moderator, e.image(vendorID, listing, "photo").id, imageTakedownReason)
		oneShot(t, e, moderator, w, "/moderator#recent-photos", "image-removed", imageRemoved, "recent-photos", 2, "/moderator?image=removed")

		photo := e.image(vendorID, listing, "photo")
		w = e.do("POST", "/images/remove", moderator, imageTakedownForm(t, e.body("GET", "/product?id="+listing, moderator, nil, 200), photo.id, imageTakedownReason))
		oneShot(t, e, moderator, w, "/product?id="+listing, "image-removed-page", imageRemoved, "", 2, "/product?id="+listing+"&image=removed")

		avatar := e.image(vendorID, "", "avatar")
		w = e.do("POST", "/images/remove", moderator, imageTakedownForm(t, e.body("GET", "/vendor?id="+vendorID, moderator, nil, 200), avatar.id, imageTakedownReason))
		oneShot(t, e, moderator, w, "/vendor?id="+vendorID, "image-removed-page", imageRemoved, "", 2, "/vendor?id="+vendorID+"&image=removed")
	})

	t.Run("key", func(t *testing.T) {
		created := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
		// A verified key renewed keeps its proof and says so.
		uid, sess := e.user("a287_key_kept", "vendor")
		key := lifecycleKey(t, "a287_key_kept", created, 0, 0)
		provePGP(e, uid, sess, key, armoredPublicKey(t, key))
		renewKey(t, key, time.Now().Truncate(time.Second), 2*365*86400, 2*365*86400)
		w := e.do("POST", "/account", sess, url.Values{"pgp": {armoredPublicKey(t, key)}, "password": {testPassword}})
		oneShot(t, e, sess, w, "/account", "key-renewed-kept", keyKept, "", 2, "/account?key=renewed")

		// An unverified key renewed.
		_, sess = e.user("a287_key_renewed", "vendor")
		key = lifecycleKey(t, "a287_key_renewed", created, 0, 0)
		e.check(e.do("POST", "/account", sess, url.Values{"pgp": {armoredPublicKey(t, key)}, "password": {testPassword}}), 303)
		renewKey(t, key, time.Now().Truncate(time.Second), 2*365*86400, 2*365*86400)
		w = e.do("POST", "/account", sess, url.Values{"pgp": {armoredPublicKey(t, key)}, "password": {testPassword}})
		oneShot(t, e, sess, w, "/account", "key-renewed", keyRenewed, "", 2, "/account?key=renewed")

		// A version that drops a revocation is saved as a changed key: an error alert (not in the title), once.
		uid, sess = e.user("a287_key_revocation", "vendor")
		key = twoSubkeyKey(t, "a287_key_revocation", created)
		if err := key.RevokeSubkey(&key.Subkeys[0], packet.KeyCompromised, "lost", nil); err != nil {
			t.Fatal(err)
		}
		provePGP(e, uid, sess, key, armoredPublicKey(t, key))
		key.Subkeys[0].Revocations = nil
		w = e.do("POST", "/account", sess, url.Values{"pgp": {armoredPublicKey(t, key)}, "password": {testPassword}})
		oneShot(t, e, sess, w, "/account", "key-revocation", keyRevocation, "", 1, "/account?key=revocation")
	})

	// ?signin= reported the /challenge sign-in's outcome; the notice text still reads the current state on /totp, and
	// ?next= (a link target, not an outcome) stays in the URL but is shown only beside the notice.
	t.Run("signin", func(t *testing.T) {
		id, _ := e.user("a287_signin", "buyer")
		_, recovery := agEnableTOTP(e, id)
		a154Codes(e, id, 1)
		anon, pc := e.pendingSignIn("a287_signin")
		w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}}, pc)
		sess := e.session(w)
		oneShot(t, e, sess, w, "/totp", "signin-recovery", "You signed in with a recovery code. 1 recovery code remains.", "", 2, "/totp?signin=recovery")

		id, _ = e.user("a287_signin_off", "buyer")
		_, recovery = agEnableTOTP(e, id)
		anon, pc, _ = a126Login(e, "a287_signin_off", "/orders")
		w = e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}, "turn_off_totp": {"1"}}, pc)
		sess = e.session(w)
		const off = "You signed in with a recovery code and turned off two-step sign-in with an authenticator app (TOTP)"
		oneShot(t, e, sess, w, "/totp?next=%2Forders", "signin-totp-off", off, "", 2, "/totp?signin=totp-off&next=%2Forders")
		// The continue link rides on the notice: shown with it (a fresh cookie here), never without it.
		b := e.body("GET", "/totp?next=%2Forders", sess, nil, 200, &http.Cookie{Name: noticeCookie, Value: "signin-totp-off"})
		if !strings.Contains(b, `href="/orders">Continue to Orders</a>`) {
			t.Fatal("/totp with the sign-in notice lacks the continue link")
		}
		if b := e.body("GET", "/totp?signin=totp-off&next=%2Forders", sess, nil, 200); strings.Contains(b, "Continue to") {
			t.Fatal("a crafted /totp link shows the continue link")
		}
	})
}
