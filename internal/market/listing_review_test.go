package market

import (
	"context"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MOD72-6: listing hold mode with per-vendor auto-approve (listing_review.go, migration 136).

const rejectReason = "Prohibited item: the description offers a weapon part."

// listingFields is a valid /listings and /listings/update form for title.
func listingFields(title string) url.Values {
	return url.Values{"title": {title}, "description": {"A held listing description."}, "category": {"Hardware"}, "region": {"Worldwide"},
		"kind": {"physical"}, "stock": {"3"}, "price_xmr": {"0.5"}}
}

// postListing creates a listing through the vendor's form and returns its id.
func (e *testEnv) postListing(session, title string) string {
	e.t.Helper()
	w := e.do("POST", "/listings", session, listingFields(title))
	e.check(w, 303)
	id, _, _ := strings.Cut(strings.TrimPrefix(w.Header().Get("Location"), "/listing-edit?id="), "#")
	if len(id) != 64 {
		e.t.Fatalf("listing redirect %q", w.Header().Get("Location"))
	}
	return id
}

func reviewState(e *testEnv, id string) string {
	e.t.Helper()
	return agStr(e, "SELECT review_state FROM products WHERE id=$1", id)
}

func setListingReviewMode(e *testEnv, mode string) {
	e.t.Helper()
	agExec(e, "UPDATE settings SET value=$1 WHERE key='listing_review_mode'", mode)
}

// formOn returns the hidden fields (csrf excepted) of the first form on page that posts to action and contains every
// marker, for submitting as the browser would.
func formOn(t *testing.T, page, action string, markers ...string) url.Values {
	t.Helper()
	for _, f := range strings.Split(page, "<form ")[1:] {
		f, _, _ = strings.Cut(f, "</form>")
		if !strings.Contains(f, `action="`+action+`"`) {
			continue
		}
		ok := true
		for _, m := range markers {
			ok = ok && strings.Contains(f, m)
		}
		if !ok {
			continue
		}
		v := url.Values{}
		for _, m := range hiddenInput.FindAllStringSubmatch(f, -1) {
			if m[1] != "csrf" {
				v.Set(m[1], m[2])
			}
		}
		return v
	}
	t.Fatalf("no form posting to %s with %q", action, markers)
	return nil
}

// buyerCannotReach checks every buyer and guest path to listing p, titled title, of vendorID, for each session.
func buyerCannotReach(e *testEnv, p, title, vendorID string, sessions ...string) {
	e.t.Helper()
	for _, s := range sessions {
		for _, path := range []string{"/", "/?q=" + url.QueryEscape(title), "/?category=Hardware", "/?region=Worldwide", "/vendor?id=" + vendorID} {
			page := e.body("GET", path, s, nil, 200)
			mustNotContain(e.t, page, "id="+p)
			if !strings.Contains(path, "q=") { // the search page echoes the term in its search box
				mustNotContain(e.t, page, title)
			} else {
				mustContain(e.t, page, "No listings found")
			}
		}
		e.check(e.do("GET", "/product?id="+p, s, nil), 404)
		// The checkout page needs a session: a guest is sent to sign in, a signed-in account gets 404.
		if w := e.do("GET", "/checkout?id="+p, s, nil); w.Code != 404 && (w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/login?")) {
			e.t.Fatalf("checkout page of a held listing: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestListingHoldHidesNewListingFromEveryBuyerPath(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("hold_admin", "admin")
	vendorID, vendor := e.user("hold_vendor", "vendor")
	_, other := e.user("hold_other_vendor", "vendor")
	buyerID, buyer := e.user("hold_buyer", "buyer")
	_, moderator := e.user("hold_mod", "moderator")
	if got := agStr(e, "SELECT value FROM settings WHERE key='listing_review_mode'"); got != "publish" {
		t.Fatalf("default listing_review_mode %q, want publish", got)
	}
	// Publish mode (the default) is today's behaviour: a new listing is public at once.
	published := e.postListing(vendor, "Published lantern")
	if reviewState(e, published) != "approved" {
		t.Fatal("publish mode held a new listing")
	}
	mustContain(t, e.body("GET", "/", randomToken(), nil, 200), "Published lantern")

	setListingReviewMode(e, "hold")
	title := "Held lantern kit"
	p := e.postListing(vendor, title)
	if reviewState(e, p) != "pending" {
		t.Fatal("hold mode published a new listing")
	}
	buyerCannotReach(e, p, title, vendorID, randomToken(), buyer, other)
	// A direct POST of a draft is refused like an unknown listing, and creates nothing.
	agExpect(e, "/orders", buyer, form("product_id", p, "currency", "XMR"), 400, "Product unavailable")
	if n := agInt(e, "SELECT count(*) FROM orders WHERE product_id=$1", p); n != 0 {
		t.Fatalf("pending listing took %d orders", n)
	}
	// Another vendor cannot open its edit page either.
	e.check(e.do("GET", "/listing-edit?id="+p, other, nil), 404)

	// The vendor sees it marked on the desk, its product page and its edit page.
	desk := e.body("GET", "/vendor-dashboard", vendor, nil, 200)
	mustContain(t, desk, title, "Awaiting review")
	page := e.body("GET", "/product?id="+p, vendor, nil, 200)
	mustContain(t, page, title, "Awaiting review")
	mustNotContain(t, page, `href="/checkout?id=`+p)
	mustContain(t, e.body("GET", "/listing-edit?id="+p, vendor, nil, 200), "Awaiting review")
	// Staff see it, on its page and on the moderation desk, with approve and reject forms.
	for _, s := range []string{moderator, admin} {
		mustContain(t, e.body("GET", "/product?id="+p, s, nil, 200), title, "Awaiting review")
		desk := e.body("GET", "/moderator", s, nil, 200)
		mustContain(t, desk, "Listings awaiting review", title, "hold_vendor")
		formOn(t, desk, "/moderator/listing-review", `name="id" value="`+p+`"`, `name="action" value="approve"`)
		formOn(t, desk, "/moderator/listing-review", `name="id" value="`+p+`"`, `name="action" value="reject"`, `name="reason" required minlength="10" maxlength="500"`)
	}
	_ = buyerID
}

func TestListingReviewApproveAndReject(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("rev_admin", "admin")
	vendorID, vendor := e.user("rev_vendor", "vendor")
	_, other := e.user("rev_other", "vendor")
	_, buyer := e.user("rev_buyer", "buyer")
	modID, moderator := e.user("rev_mod", "moderator")
	mustContain(t, e.body("GET", "/moderator", moderator, nil, 200), "No listings are waiting for review")
	setListingReviewMode(e, "hold")
	approveMe := e.postListing(vendor, "Approve me lamp")
	rejectMe := e.postListing(vendor, "Reject me lamp")
	desk := e.body("GET", "/moderator", moderator, nil, 200)
	approve := formOn(t, desk, "/moderator/listing-review", `name="id" value="`+approveMe+`"`, `name="action" value="approve"`)
	approve.Set("password", testPassword)
	reject := formOn(t, desk, "/moderator/listing-review", `name="id" value="`+rejectMe+`"`, `name="action" value="reject"`)
	reject.Set("password", testPassword)
	reject.Set("reason", rejectReason)
	if len(approve.Get(formNonceField)) != 32 || len(reject.Get(formNonceField)) != 32 {
		t.Fatal("review forms carry no receipt nonce")
	}

	// Refusals change nothing: other roles, a missing or wrong password, a bad choice or reason, an unknown listing.
	audits, notes := agInt(e, "SELECT count(*) FROM audit_events"), agInt(e, "SELECT count(*) FROM notifications")
	for _, s := range []string{buyer, vendor, other} {
		agExpect(e, "/moderator/listing-review", s, approve, 403, "")
		agExpect(e, "/moderator/listing-review", s, reject, 403, "")
	}
	agExpect(e, "/moderator/listing-review", moderator, withField(approve, "password", ""), 400, "Enter your current password")
	agExpect(e, "/moderator/listing-review", moderator, withField(approve, "action", "publish"), 400, "Choose approve or reject")
	agExpect(e, "/moderator/listing-review", moderator, withField(reject, "reason", "too short"), 400, "10–500 characters")
	agExpect(e, "/moderator/listing-review", moderator, withField(approve, "id", "missing"), 404, "Listing not found")
	if reviewState(e, approveMe) != "pending" || reviewState(e, rejectMe) != "pending" {
		t.Fatal("a refused review changed a listing")
	}
	if agInt(e, "SELECT count(*) FROM audit_events") != audits || agInt(e, "SELECT count(*) FROM notifications") != notes {
		t.Fatal("a refused review wrote an audit row or notification")
	}
	// A wrong password spends this moderator's budget, so a fresh moderator checks it.
	_, mod2 := e.user("rev_mod_two", "moderator")
	agExpect(e, "/moderator/listing-review", mod2, withField(approve, "password", "wrong-password-here"), 401, "Password incorrect")

	// A vendor's change after the desk was rendered makes its form stale: nothing unseen is approved.
	edited := listingFields("Approve me lamp")
	edited.Set("description", "Changed after the moderator opened the desk.")
	edited.Set("id", approveMe)
	edited.Set("stock_seen", "3")
	e.check(e.do("POST", "/listings/update", vendor, edited), 303)
	agExpect(e, "/moderator/listing-review", moderator, approve, 409, "changed this listing after you opened")
	if reviewState(e, approveMe) != "pending" {
		t.Fatal("a stale approval was stored")
	}
	approve = formOn(t, e.body("GET", "/moderator", moderator, nil, 200), "/moderator/listing-review", `name="id" value="`+approveMe+`"`, `name="action" value="approve"`)
	approve.Set("password", testPassword)

	// The moderator approves: public at once, audited on both accounts and notified.
	w := e.do("POST", "/moderator/listing-review", moderator, approve)
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/moderator#listing-review" {
		t.Fatalf("approve redirect %q", loc)
	}
	if reviewState(e, approveMe) != "approved" {
		t.Fatal("approval not stored")
	}
	short := shortID(approveMe)
	if !e.auditExact(modID, "Approved listing "+short+" of rev_vendor (confirmed with password)") {
		t.Fatal("moderator approval audit missing")
	}
	if !e.auditExact(vendorID, "Listing "+short+" approved in review by moderator rev_mod") {
		t.Fatal("vendor approval audit missing")
	}
	mustContain(t, agStr(e, "SELECT body FROM notifications WHERE user_id=$1 ORDER BY created DESC LIMIT 1", vendorID), "Approve me lamp", short, "approved")
	mustContain(t, e.body("GET", "/", buyer, nil, 200), "Approve me lamp")
	e.check(e.do("GET", "/product?id="+approveMe, randomToken(), nil), 200)
	// Approving again is refused: it is no longer awaiting review.
	agExpect(e, "/moderator/listing-review", moderator, withField(approve, formNonceField, formNonceFor("/moderator/listing-review")), 409, "not awaiting review")

	// The administrator rejects the other: archived with the reason (takedown semantics), still pending, never public.
	w = e.do("POST", "/moderator/listing-review", admin, reject)
	e.check(w, 303)
	var archived bool
	var reason, state string
	if err := e.DB.QueryRow("SELECT archived,coalesce(admin_archive_reason,''),review_state FROM products WHERE id=$1", rejectMe).Scan(&archived, &reason, &state); err != nil {
		t.Fatal(err)
	}
	if !archived || reason != rejectReason || state != "pending" {
		t.Fatalf("rejection stored archived=%v reason=%q state=%q", archived, reason, state)
	}
	rshort := shortID(rejectMe)
	if !e.auditExact(adminID, "Rejected listing "+rshort+" of rev_vendor: "+rejectReason+" (confirmed with password)") {
		t.Fatal("administrator rejection audit missing")
	}
	if !e.auditExact(vendorID, "Listing "+rshort+" rejected in review by administrator rev_admin: "+rejectReason) {
		t.Fatal("vendor rejection audit missing")
	}
	note := agStr(e, "SELECT body FROM notifications WHERE user_id=$1 ORDER BY created DESC LIMIT 1", vendorID)
	mustContain(t, note, "Reject me lamp", rshort, rejectReason)
	mustNotContain(t, note, "http", "?id=", "<")
	buyerCannotReach(e, rejectMe, "Reject me lamp", vendorID, randomToken(), buyer)
	mustNotContain(t, e.body("GET", "/moderator", moderator, nil, 200), "Reject me lamp")
	// The vendor sees the rejection and its reason and cannot restore it; the reason stays off public pages.
	deskPage := e.body("GET", "/vendor-dashboard", vendor, nil, 200)
	mustContain(t, deskPage, "Rejected in review", rejectReason)
	mustContain(t, e.body("GET", "/listing-edit?id="+rejectMe, vendor, nil, 200), "Rejected in review", rejectReason)
	agExpect(e, "/listings/restore", vendor, form("id", rejectMe), 409, rejectReason)
	mustContain(t, e.body("GET", "/moderator", moderator, nil, 200), "No listings are waiting for review")
}

func TestListingEditReholdsApprovedListing(t *testing.T) {
	x := newXMREscrowEnv(t)
	e := x.testEnv
	_, admin := e.user("rehold_admin", "admin")
	_, other := e.user("rehold_other", "buyer")
	p := x.physical(x.vendor)
	paid := e.order(x.buyer, p, "XMR", statePaid)
	reviewer, _ := e.user("rehold_reviewer", "buyer")
	done := e.order(reviewer, p, "XMR", stateCompleted)
	agExec(e, "INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body) VALUES($1,$2,$3,$4,4,'Arrived as described')", randomToken(), done, p, reviewer)
	draftBuyer, draftSess := e.user("rehold_draft", "buyer")
	draft := e.order(draftBuyer, p, "XMR", stateDraft)
	setListingReviewMode(e, "hold")
	edit := func(sess string, f url.Values) {
		t.Helper()
		f.Set("id", p)
		f.Set("stock_seen", agStr(e, "SELECT stock::text FROM products WHERE id=$1", p))
		e.check(e.do("POST", "/listings/update", sess, f), 303)
	}
	// A stock-only change, or an administrator's edit, does not re-hold an approved listing.
	f := listingFields("Test listing")
	f.Set("description", "Fixture")
	f.Set("stock", "7")
	edit(x.vendorSess, f)
	if reviewState(e, p) != "approved" {
		t.Fatal("a stock-only change re-held the listing")
	}
	f.Set("description", "Changed by an administrator")
	edit(admin, f)
	if reviewState(e, p) != "approved" {
		t.Fatal("an administrator's edit re-held the listing")
	}
	// The vendor's change to what buyers see re-holds it.
	f.Set("title", "Re-held lantern")
	edit(x.vendorSess, f)
	if reviewState(e, p) != "pending" {
		t.Fatal("an edit to an approved listing was not re-held")
	}
	buyerCannotReach(e, p, "Re-held lantern", x.vendor, randomToken(), other, draftSess)
	// The unreviewed text reaches no one through a review on the vendor page or a draft saved before the edit.
	vendorPage := e.body("GET", "/vendor?id="+x.vendor, randomToken(), nil, 200)
	mustContain(t, vendorPage, "Arrived as described", "Test listing")
	mustNotContain(t, vendorPage, "Re-held lantern")
	draftPage := e.body("GET", "/order?id="+draft, draftSess, nil, 200)
	mustContain(t, draftPage, "Listing awaiting review")
	mustNotContain(t, draftPage, "Re-held lantern", "Changed by an administrator")
	// Existing orders are unaffected: the paid order's buyer still reads the listing and its order.
	mustContain(t, e.body("GET", "/product?id="+p, x.buyerSess, nil, 200), "Re-held lantern")
	e.check(e.do("GET", "/order?id="+paid, x.buyerSess, nil), 200)
	if x.state(paid) != statePaid {
		t.Fatal("re-hold changed an existing order")
	}
	// But an unfunded draft cannot request payment, and no new draft is accepted.
	w := x.checkout(draftSess, draft, testXMRAddrOn(x.refundNet(), "refund "+draft, true))
	if w.Code != 409 || !strings.Contains(refusalText(w.Body.String()), "no longer available") {
		t.Fatalf("checkout of a held listing: %d %s", w.Code, w.Body.String())
	}
	if x.state(draft) != stateDraft {
		t.Fatal("held listing's draft requested payment")
	}
	agExpect(e, "/orders", other, form("product_id", p, "currency", "XMR"), 400, "Product unavailable")
	// An administrator's edit of a pending listing keeps it pending.
	f.Set("description", "Administrator edit while pending")
	edit(admin, f)
	if reviewState(e, p) != "pending" {
		t.Fatal("an administrator's edit approved a pending listing")
	}
}

func TestListingPhotoOnApprovedListingReholds(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, vendor := e.user("photo_rehold_vendor", "vendor")
	p := e.product(vendorID, "physical")
	setListingReviewMode(e, "hold")
	if w := e.upload("/listings/photo", vendor, e.photoParts(vendor, p, smallJPEG(t))...); w.Code != 303 {
		t.Fatalf("photo upload: %d %s", w.Code, w.Body.String())
	}
	if reviewState(e, p) != "pending" {
		t.Fatal("a new photo on an approved listing was not re-held")
	}
	if n := agInt(e, "SELECT count(*) FROM "+imagePublicFrom+" WHERE i.product_id=$1 AND "+imagePublicWhere, p); n != 0 {
		t.Fatal("the public image route serves a held listing's photo")
	}
}

func TestListingAutoApproveVendorPublishesAtOnce(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("auto_admin", "admin")
	vendorID, vendor := e.user("auto_vendor", "vendor")
	_, buyer := e.user("auto_buyer", "buyer")
	_, moderator := e.user("auto_mod", "moderator")
	setListingReviewMode(e, "hold")
	page := e.body("GET", "/admin/vendors", admin, nil, 200)
	on := formOn(t, page, "/admin/vendor-auto-approve")
	on.Set("handle", "auto_vendor")
	on.Set("action", "on")
	on.Set("password", testPassword)
	for _, s := range []string{moderator, vendor, buyer} {
		agExpect(e, "/admin/vendor-auto-approve", s, on, 403, "")
	}
	agExpect(e, "/admin/vendor-auto-approve", admin, withField(on, "handle", "auto_buyer"), 409, "not a vendor")
	agExpect(e, "/admin/vendor-auto-approve", admin, withField(on, "handle", "nobody_here"), 404, "No account")
	if agInt(e, "SELECT count(*) FROM users WHERE listing_auto_approve") != 0 {
		t.Fatal("a refused change set the flag")
	}
	w := e.do("POST", "/admin/vendor-auto-approve", admin, on)
	e.check(w, 303)
	if !e.auditExact(adminID, "Let auto_vendor publish listings without review (confirmed with password)") {
		t.Fatal("administrator audit missing")
	}
	if !e.auditExact(vendorID, "Listings publish without review: set by administrator auto_admin") {
		t.Fatal("vendor audit missing")
	}
	p := e.postListing(vendor, "Trusted lantern")
	if reviewState(e, p) != "approved" {
		t.Fatal("an auto-approved vendor's listing was held")
	}
	mustContain(t, e.body("GET", "/", buyer, nil, 200), "Trusted lantern")
	agExpect(e, "/admin/vendor-auto-approve", admin, withField(on, formNonceField, formNonceFor("/admin/vendor-auto-approve")), 409, "already")
	// The account page shows the flag and offers to remove it.
	userPage := e.body("GET", "/admin/users?handle=auto_vendor", admin, nil, 200)
	off := formOn(t, userPage, "/admin/vendor-auto-approve", `name="action" value="off"`)
	off.Set("password", testPassword)
	w = e.do("POST", "/admin/vendor-auto-approve", admin, off)
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/admin/users?handle=auto_vendor#vendors" {
		t.Fatalf("account page redirect %q", loc)
	}
	if reviewState(e, e.postListing(vendor, "Untrusted lantern")) != "pending" {
		t.Fatal("removing the flag did not restore the hold")
	}
	if reviewState(e, p) != "approved" {
		t.Fatal("removing the flag re-held an approved listing")
	}
}

func TestListingReviewModeIsAdminOnlyAndFailsClosed(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("mode_admin", "admin")
	vendorID, vendor := e.user("mode_vendor", "vendor")
	_, buyer := e.user("mode_buyer", "buyer")
	_, moderator := e.user("mode_mod", "moderator")
	page := e.body("GET", "/admin/vendors", admin, nil, 200)
	mustContain(t, page, "Listing review")
	hold := formOn(t, page, "/admin/listing-review")
	hold.Set("listing_review_mode", "hold")
	hold.Set("password", testPassword)
	for _, s := range []string{moderator, vendor, buyer} {
		agExpect(e, "/admin/listing-review", s, hold, 403, "")
	}
	agExpect(e, "/admin/listing-review", admin, withField(hold, "listing_review_mode", "auto"), 400, "Choose")
	agExpect(e, "/admin/listing-review", admin, withField(hold, "listing_review_mode", "publish"), 409, "already")
	w := e.do("POST", "/admin/listing-review", admin, hold)
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/admin/vendors#listing-review" {
		t.Fatalf("mode redirect %q", loc)
	}
	if !e.auditExact(adminID, "Listing review mode changed from publish to hold (confirmed with password)") {
		t.Fatal("mode audit missing")
	}
	p := e.postListing(vendor, "Mode held lantern")
	if reviewState(e, p) != "pending" {
		t.Fatal("hold mode published")
	}
	// Switching back to publish approves nothing: the held listing stays hidden until staff approve it.
	back := formOn(t, e.body("GET", "/admin/vendors", admin, nil, 200), "/admin/listing-review")
	back.Set("listing_review_mode", "publish")
	back.Set("password", testPassword)
	e.check(e.do("POST", "/admin/listing-review", admin, back), 303)
	if !e.auditExact(adminID, "Listing review mode changed from hold to publish; 1 listing(s) still await review (confirmed with password)") {
		t.Fatal("switch-back audit missing")
	}
	if reviewState(e, p) != "pending" {
		t.Fatal("switching to publish approved a pending listing")
	}
	buyerCannotReach(e, p, "Mode held lantern", vendorID, randomToken(), buyer)
	mustContain(t, e.body("GET", "/moderator", moderator, nil, 200), "Mode held lantern")
	if reviewState(e, e.postListing(vendor, "Mode published lantern")) != "approved" {
		t.Fatal("publish mode held a new listing")
	}
	// A missing or unknown mode value fails closed (holds).
	agExec(e, "UPDATE settings SET value='bogus' WHERE key='listing_review_mode'")
	if reviewState(e, e.postListing(vendor, "Bogus mode lantern")) != "pending" {
		t.Fatal("an unknown mode published")
	}
	agExec(e, "DELETE FROM settings WHERE key='listing_review_mode'")
	if reviewState(e, e.postListing(vendor, "Missing mode lantern")) != "pending" {
		t.Fatal("a missing mode published")
	}
}

func TestListingHoldSurvivesModerationOverflowAndWorker(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("overflow_vendor", "vendor")
	_, buyer := e.user("overflow_buyer", "buyer")
	if err := SetModerationEnabled(context.Background(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	setListingReviewMode(e, "hold")
	var ids []string
	for i := range 5 { // the per-actor capture cap is 3: two of these overflow capture
		ids = append(ids, e.postListing(vendor, "Overflow lantern "+string(rune('A'+i))))
	}
	if n := agInt(e, "SELECT count(DISTINCT target_id) FROM moderation_jobs WHERE target_id=ANY($1::text[])", ids); n >= 5 {
		t.Fatalf("capture did not overflow: %d captured", n)
	}
	if _, err := moderationTestWorker(e).Pass(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		if reviewState(e, id) != "pending" {
			t.Fatalf("listing %d left review after capture or the worker", i)
		}
		buyerCannotReach(e, id, "Overflow lantern "+string(rune('A'+i)), vendorID, buyer)
	}
}

// TestModerationNeverWritesReviewState: the moderation layer (capture triggers, the worker, the engine and the model
// path) never names products.review_state, so no model output or capture outcome can publish or hold a listing.
func TestModerationNeverWritesReviewState(t *testing.T) {
	root := "." // TestMain runs the package tests from the repository root
	var found []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if !strings.HasPrefix(rel, "internal/moderation/") && !strings.HasPrefix(rel, "internal/market/moderation") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err == nil && strings.Contains(string(b), "review_state") {
				found = append(found, rel)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	migrations, err := filepath.Glob("internal/market/migrations/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("no migrations", err)
	}
	for _, m := range migrations {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "review_state") && filepath.Base(m) != "136_listing_review.sql" {
			found = append(found, m)
		}
	}
	if len(found) > 0 {
		t.Fatalf("moderation code or another migration names review_state: %v", found)
	}
	e := newTestApp(t) // skips without TEST_DATABASE_URL, after the source checks above

	if n := agInt(e, "SELECT count(*) FROM pg_proc WHERE prosrc LIKE '%review_state%'"); n != 0 {
		t.Fatalf("%d database functions or triggers name review_state", n)
	}
}
