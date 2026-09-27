package market

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A-186 / D10: the quotas are counted in SQL inside the insert transaction, under the owner's row lock, the
// listing's lock and the images advisory lock, so concurrent uploads cannot pass them together.

// liftUploadGuards lets one account run many uploads at once (the in-memory one-per-account lock and the two
// upload slots), so the database's own checks are what a test sees.
func liftUploadGuards(t *testing.T) {
	oldLimit, oldWork := oneAtATimeLimit, uploadWork
	oneAtATimeLimit, uploadWork = 100, make(chan struct{}, 16)
	t.Cleanup(func() { oneAtATimeLimit, uploadWork = oldLimit, oldWork })
}

// concurrently runs each post at the same time and returns the responses' status codes.
func concurrently(posts ...func() *httptest.ResponseRecorder) []int {
	codes := make([]int, len(posts))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, p := range posts {
		wg.Go(func() {
			<-start
			codes[i] = p().Code
		})
	}
	close(start)
	wg.Wait()
	return codes
}

func countCodes(codes []int) map[int]int {
	m := map[int]int{}
	for _, c := range codes {
		m[c]++
	}
	return m
}

// imageSize is the bytes (full + thumbnail) a fixture takes once re-encoded.
func imageSize(t *testing.T, kind uploadKind, data []byte) int64 {
	out := mustProcess(t, kind, data)
	return int64(len(out.full) + len(out.thumb))
}

func TestPhotoPerListingCapConcurrent(t *testing.T) {
	e, _ := uploadEnv(t)
	liftUploadGuards(t)
	vendorID, vendor := e.user("cap_race_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	var posts []func() *httptest.ResponseRecorder
	for range 7 {
		posts = append(posts, func() *httptest.ResponseRecorder {
			return e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, smallJPEG(t))...)
		})
	}
	if got := countCodes(concurrently(posts...)); got[303] != 4 || got[409] != 3 {
		t.Fatalf("codes %v, want four 303 and three 409", got)
	}
	var n int
	e.DB.QueryRow("SELECT count(*) FROM images WHERE product_id=$1", listing).Scan(&n)
	if n != 4 {
		t.Fatalf("%d photos on the listing", n)
	}
	checkPage(t, e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, smallJPEG(t))...), 409, "already has 4 photos")
}

func TestVendorByteQuotaRace(t *testing.T) {
	e, _ := uploadEnv(t)
	liftUploadGuards(t)
	vendorID, vendor := e.user("quota_race_vendor", "vendor")
	photo := smallJPEG(t)
	size := imageSize(t, uploadListing, photo)
	old := uploaderQuota
	uploaderQuota = 3*size + size/2 // room for three
	t.Cleanup(func() { uploaderQuota = old })
	listings := []string{e.product(vendorID, "physical"), e.product(vendorID, "physical")}
	var posts []func() *httptest.ResponseRecorder
	for i := range 6 {
		posts = append(posts, func() *httptest.ResponseRecorder {
			return e.upload("/listings/photo", vendor, e.photoParts(vendor, listings[i%2], photo)...)
		})
	}
	if got := countCodes(concurrently(posts...)); got[303] != 3 || got[409] != 3 {
		t.Fatalf("codes %v, want three 303 and three 409", got)
	}
	var used int64
	e.DB.QueryRow(`SELECT sum(octet_length("full")+octet_length(thumb)) FROM images WHERE owner_user_id=$1`, vendorID).Scan(&used)
	if used > uploaderQuota {
		t.Fatalf("%d bytes stored, quota %d", used, uploaderQuota)
	}
	w := e.upload("/account/avatar", vendor, e.avatarParts(vendor, photo)...)
	checkPage(t, w, 409, "more than 0 MB") // the quota is below a megabyte here; the copy names it
}

func TestGlobalStorageLimit(t *testing.T) {
	e, _ := uploadEnv(t)
	liftUploadGuards(t)
	photo := smallJPEG(t)
	size := imageSize(t, uploadAvatar, photo)
	e.A.storageLimit = 2*size + size/2 // room for two
	var posts []func() *httptest.ResponseRecorder
	var sessions []string
	for i := range 5 {
		_, s := e.user("global_vendor_"+string(rune('a'+i)), "vendor")
		sessions = append(sessions, s)
		posts = append(posts, func() *httptest.ResponseRecorder { return e.upload("/account/avatar", s, e.avatarParts(s, photo)...) })
	}
	codes := concurrently(posts...)
	if got := countCodes(codes); got[303] != 2 || got[409] != 3 {
		t.Fatalf("codes %v, want two 303 and three 409", got)
	}
	_, late := e.user("global_late", "vendor")
	checkPage(t, e.upload("/account/avatar", late, e.avatarParts(late, photo)...), 409, "Photo storage on this market is full; contact the operator")
	// Replacing an avatar counts the new one instead of the old, so it still fits.
	for i, c := range codes {
		if c == 303 {
			if w := e.upload("/account/avatar", sessions[i], e.avatarParts(sessions[i], photo)...); w.Code != 303 {
				t.Fatalf("replace at the limit: %d %s", w.Code, w.Body.String())
			}
			break
		}
	}
	if e.imageCount() != 2 {
		t.Fatalf("%d rows", e.imageCount())
	}
}

// D10: the 21st upload in ten minutes is refused with 429, before its body is kept.
func TestUploadBudget20Per10Min(t *testing.T) {
	e, f := uploadEnv(t)
	_, vendor := e.user("budget_vendor", "vendor")
	photo := smallJPEG(t)
	for i := range uploadBudget {
		if w := e.upload("/account/avatar", vendor, e.avatarParts(vendor, photo)...); w.Code != 303 {
			t.Fatalf("upload %d: %d %s", i+1, w.Code, w.Body.String())
		}
	}
	scans := scansOf(f)
	w := e.upload("/account/avatar", vendor, e.avatarParts(vendor, photo)...)
	checkPage(t, w, 429, "at most 20 photos or avatars in 10 minutes")
	if w.Header().Get("Retry-After") == "" || scansOf(f) != scans || e.imageCount() != 1 {
		t.Fatalf("Retry-After %q, scanned %v, rows %d", w.Header().Get("Retry-After"), scansOf(f) != scans, e.imageCount())
	}
	// Another account is unaffected.
	_, other := e.user("budget_other", "vendor")
	if w = e.upload("/account/avatar", other, e.avatarParts(other, photo)...); w.Code != 303 {
		t.Fatalf("other account: %d", w.Code)
	}
}

// D10/C5: a second upload from one account while the first is in flight is refused at once with 429.
func TestOneUploadPerAccount(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, vendor := e.user("one_at_a_time_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	entered, release := stallScans(t, f)
	first := e.uploadAsync("/listings/photo", vendor, e.photoParts(vendor, listing, smallJPEG(t))...)
	waitEntered(t, entered)
	w := e.upload("/account/avatar", vendor, e.avatarParts(vendor, smallJPEG(t))...)
	checkPage(t, w, 429, "another upload from your account is still in progress")
	if w.Header().Get("Retry-After") != busyRetryAfter {
		t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
	}
	release()
	if w = <-first; w.Code != 303 {
		t.Fatalf("first: %d", w.Code)
	}
	if w = e.upload("/account/avatar", vendor, e.avatarParts(vendor, smallJPEG(t))...); w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "avatar=saved") {
		t.Fatalf("after the first: %d", w.Code)
	}
}
