package market

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/moderation"
)

// MOD72-6: listing hold mode with per-vendor auto-approve (listing_review.go, migration 142).

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

// listingReviewPost is the desk's approve or reject form for listing id as rendered now (its version read with the desk's
// own listingVersionSQL), for legs that are not about the desk page itself; e.do adds a fresh receipt nonce.
func listingReviewPost(e *testEnv, id, action string) url.Values {
	e.t.Helper()
	f := url.Values{"id": {id}, "action": {action}, "password": {testPassword},
		"version": {agStr(e, "SELECT "+listingVersionSQL+" FROM products p WHERE p.id=$1", id)}}
	if action == "reject" {
		f.Set("reason", rejectReason)
	}
	return f
}

// buyerCannotReach checks every buyer and guest path to listing p, titled title, of vendorID, for each session.
func buyerCannotReach(e *testEnv, p, title, vendorID string, sessions ...string) {
	e.t.Helper()
	for _, s := range sessions {
		for _, path := range []string{"/", "/?q=" + url.QueryEscape(title), "/?category=Hardware", "/?region=Worldwide", "/vendor?id=" + vendorID} {
			w := e.do("GET", path, s, nil)
			if w.Code == 404 && strings.HasPrefix(path, "/vendor?") {
				continue // a deactivated vendor's page is hidden altogether (PR #104)
			}
			e.check(w, 200)
			page := w.Body.String()
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

	// The administrator rejects the other: rejected with the reason, never public, and not a takedown: not archived,
	// no takedown reason.
	w = e.do("POST", "/moderator/listing-review", admin, reject)
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/moderator#listing-review" {
		t.Fatalf("reject redirect %q", loc)
	}
	assertRejected(e, rejectMe, rejectReason)
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
	// The vendor sees the rejection and its reason, with no takedown or restore control; buyers never see the reason.
	deskPage := e.body("GET", "/vendor-dashboard", vendor, nil, 200)
	mustContain(t, deskPage, "Rejected — "+rejectReason)
	mustNotContain(t, deskPage, "Taken down", `action="/listings/restore"`)
	mustContain(t, e.body("GET", "/listing-edit?id="+rejectMe, vendor, nil, 200), "Rejected — "+rejectReason)
	mustContain(t, e.body("GET", "/product?id="+rejectMe, vendor, nil, 200), "Rejected — "+rejectReason)
	mustNotContain(t, e.body("GET", "/vendor?id="+vendorID, buyer, nil, 200), rejectReason)
	// Rejecting again, or approving, a rejected listing is refused: it is no longer awaiting review.
	agExpect(e, "/moderator/listing-review", moderator, withField(reject, formNonceField, formNonceFor("/moderator/listing-review")), 409, "not awaiting review")
	mustContain(t, e.body("GET", "/moderator", moderator, nil, 200), "No listings are waiting for review")
}

// assertRejected checks that listing id is rejected with reason and is not a takedown: not archived, no takedown reason.
func assertRejected(e *testEnv, id, reason string) {
	e.t.Helper()
	var archived bool
	var takedown, state, got string
	if err := e.DB.QueryRow("SELECT archived,coalesce(admin_archive_reason,''),review_state,coalesce(review_rejection_reason,'') FROM products WHERE id=$1", id).Scan(&archived, &takedown, &state, &got); err != nil {
		e.t.Fatal(err)
	}
	if archived || takedown != "" || state != "rejected" || got != reason {
		e.t.Fatalf("rejection stored archived=%v takedown=%q state=%q reason=%q", archived, takedown, state, got)
	}
}

func TestListingRejectedVendorEditsAndResubmits(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("resub_admin", "admin")
	vendorID, vendor := e.user("resub_vendor", "vendor")
	_, buyer := e.user("resub_buyer", "buyer")
	_, moderator := e.user("resub_mod", "moderator")
	setListingReviewMode(e, "hold")
	p := e.postListing(vendor, "Resubmit lamp")
	reviewForm := func(action string) url.Values {
		t.Helper()
		f := formOn(t, e.body("GET", "/moderator", moderator, nil, 200), "/moderator/listing-review", `name="id" value="`+p+`"`, `name="action" value="`+action+`"`)
		f.Set("password", testPassword)
		f.Set("reason", rejectReason)
		return f
	}
	edit := func(f url.Values) {
		t.Helper()
		f.Set("id", p)
		f.Set("stock_seen", agStr(e, "SELECT stock::text FROM products WHERE id=$1", p))
		e.check(e.do("POST", "/listings/update", vendor, f), 303)
	}
	// A reject form rendered before the vendor's change is stale, as an approve form is.
	stale := reviewForm("reject")
	changed := listingFields("Resubmit lamp")
	changed.Set("description", "Changed before the moderator rejected it.")
	edit(changed)
	agExpect(e, "/moderator/listing-review", moderator, stale, 409, "changed this listing after you opened")
	if reviewState(e, p) != "pending" {
		t.Fatal("a stale rejection was stored")
	}
	e.check(e.do("POST", "/moderator/listing-review", moderator, reviewForm("reject")), 303)
	assertRejected(e, p, rejectReason)
	buyerCannotReach(e, p, "Resubmit lamp", vendorID, randomToken(), buyer)

	// Switching to publish, or an administrator's edit, does not publish or resubmit it.
	setListingReviewMode(e, "publish")
	adminEdit := listingFields("Resubmit lamp")
	adminEdit.Set("description", "An administrator's edit of a rejected listing.")
	adminEdit.Set("id", p)
	adminEdit.Set("stock_seen", agStr(e, "SELECT stock::text FROM products WHERE id=$1", p))
	e.check(e.do("POST", "/listings/update", admin, adminEdit), 303)
	assertRejected(e, p, rejectReason)
	// A stock-only change by the vendor does not resubmit; the reason stays.
	stock := listingFields("Resubmit lamp")
	stock.Set("description", "An administrator's edit of a rejected listing.")
	stock.Set("stock", "9")
	edit(stock)
	assertRejected(e, p, rejectReason)
	mustContain(t, e.body("GET", "/listing-edit?id="+p, vendor, nil, 200), "Rejected — "+rejectReason)

	// The vendor's change to what buyers see resubmits it, even in publish mode: pending, reason cleared, audited.
	audits := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", vendorID)
	fixed := listingFields("Resubmit lamp, fixed")
	fixed.Set("stock", "9")
	w := e.do("POST", "/listings/update", vendor, withField(withField(fixed, "id", p), "stock_seen", "9"))
	e.check(w, 303)
	if reviewState(e, p) != "pending" || agStr(e, "SELECT coalesce(review_rejection_reason,'') FROM products WHERE id=$1", p) != "" {
		t.Fatal("the vendor's edit did not resubmit the rejected listing")
	}
	if !e.auditExact(vendorID, "Resubmitted listing "+shortID(p)+" for review") {
		t.Fatal("resubmission audit missing")
	}
	if n := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", vendorID); n != audits+2 {
		t.Fatalf("resubmitting edit wrote %d vendor audit rows, want 2 (the edit and the resubmission)", n-audits)
	}
	buyerCannotReach(e, p, "Resubmit lamp, fixed", vendorID, randomToken(), buyer)
	page := e.body("GET", "/listing-edit?id="+p, vendor, nil, 200)
	mustContain(t, page, "Awaiting review")
	mustNotContain(t, page, rejectReason)
	desk := e.body("GET", "/moderator", moderator, nil, 200)
	mustContain(t, desk, "Resubmit lamp, fixed")

	// Approved, it is visible to buyers.
	e.check(e.do("POST", "/moderator/listing-review", moderator, reviewForm("approve")), 303)
	if reviewState(e, p) != "approved" {
		t.Fatal("approval of a resubmitted listing not stored")
	}
	mustContain(t, e.body("GET", "/?q="+url.QueryEscape("Resubmit lamp, fixed"), buyer, nil, 200), "/product?id="+p)
	e.check(e.do("GET", "/product?id="+p, randomToken(), nil), 200)
}

func TestListingRejectionReasonFollowsListingRetention(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	vendorID, _ := e.user("resub_retention_vendor", "vendor")
	p := e.product(vendorID, "physical")
	buyerID, _ := e.user("resub_retention_buyer", "buyer")
	e.order(buyerID, p, "XMR", stateCompleted) // an order keeps the withdrawn listing row, so its purge is observable
	agExec(e, `UPDATE products SET review_state='rejected',review_rejection_reason=$2,archived=true,archived_at=now()-interval '45 days',updated=now()-interval '45 days' WHERE id=$1`, p, rejectReason)
	ageRetentionEnrollment(t, e.A, time.Now().UTC().Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, time.Now().UTC())
	if n := agInt(e, `SELECT count(*) FROM products WHERE id=$1 AND private_expired_at IS NOT NULL AND review_rejection_reason IS NULL AND title='Withdrawn listing'`, p); n != 1 {
		t.Fatal("retention kept a withdrawn listing's rejection reason")
	}
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
	draftBuyer, draftSess := e.user("rehold_draft", "buyer") // e.user seeds the key and session proof a checkout needs
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

	// QA72-3: the earlier-order exception covers a listing awaiting review, not a rejected one. While pending, the paid
	// buyer reads the listing and its photo; once rejected, neither, though the order and its snapshot stay readable and
	// the vendor and staff still see the listing and the reason.
	photo := randomToken()[:32]
	agExec(e, `INSERT INTO images(id,owner_user_id,product_id,kind,"full",thumb,width,height) VALUES($1,$2,$3,'photo','\xffd8'::bytea,'\xffd8'::bytea,1,1)`, photo, x.vendor, p)
	deskPhoto := func(viewer, role string) int {
		return agInt(e, `SELECT count(*) FROM images i JOIN users o ON o.id=i.owner_user_id WHERE i.id=$1 AND `+imageDeskWhere, photo, viewer, role)
	}
	if deskPhoto(x.buyer, "buyer") != 1 {
		t.Fatal("the paid buyer cannot see a pending listing's photo")
	}
	_, moderator := e.user("rehold_mod", "moderator")
	modID := agStr(e, "SELECT user_id FROM sessions WHERE token_hash=$1", digest(moderator))
	e.check(e.do("POST", "/moderator/listing-review", moderator, listingReviewPost(e, p, "reject")), 303)
	assertRejected(e, p, rejectReason)
	e.check(e.do("GET", "/product?id="+p, x.buyerSess, nil), 404)
	if deskPhoto(x.buyer, "buyer") != 0 {
		t.Fatal("the paid buyer's desk route serves a rejected listing's photo")
	}
	order := e.body("GET", "/order?id="+paid, x.buyerSess, nil, 200)
	mustContain(t, order, "Test listing")
	mustNotContain(t, order, "Re-held lantern", rejectReason)
	for _, s := range []string{x.vendorSess, moderator, admin} {
		mustContain(t, e.body("GET", "/product?id="+p, s, nil, 200), "Re-held lantern", "Rejected — "+rejectReason)
	}
	if deskPhoto(x.vendor, "vendor") != 1 || deskPhoto(modID, "moderator") != 1 || deskPhoto(x.admin, "admin") != 1 {
		t.Fatal("the vendor or staff lost the rejected listing's photo")
	}
}

// PR #111's digital delivery method is shown to buyers as "delivered by the vendor" or "automatically delivered": a
// vendor's change between the two re-holds an approved listing; one between the two automatic methods does not.
func TestListingDigitalDeliveryModeChangeReholds(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("digital_mode_vendor", "vendor")
	p := e.product(vendorID, "digital")
	agExec(e, "UPDATE products SET digital_delivery_mode='reusable' WHERE id=$1", p)
	setListingReviewMode(e, "hold")
	e.check(e.do("POST", "/listings/digital-mode", vendor, form("id", p, "mode", "inventory")), 303)
	if reviewState(e, p) != "approved" {
		t.Fatal("switching between automatic delivery methods re-held the listing")
	}
	e.check(e.do("POST", "/listings/digital-mode", vendor, form("id", p, "mode", "manual")), 303)
	if reviewState(e, p) != "pending" {
		t.Fatal("switching to vendor-delivered did not re-hold the listing")
	}
	buyerCannotReach(e, p, "Test listing", vendorID, randomToken())
}

// PR #111's taxonomy fields are part of what buyers see: changing only the additional shipping regions re-holds an
// approved listing.
func TestListingShippingRegionChangeReholds(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("regions_vendor", "vendor")
	p := e.product(vendorID, "physical")
	setListingReviewMode(e, "hold")
	f := listingFields("Test listing")
	f.Set("description", "Fixture")
	f.Set("stock", "5")
	f.Set("id", p)
	f.Set("stock_seen", "5")
	f.Add("shipping_regions", "Europe")
	e.check(e.do("POST", "/listings/update", vendor, f), 303)
	if got := agStr(e, "SELECT array_to_string(shipping_regions,',') FROM products WHERE id=$1", p); got != "Europe" {
		t.Fatalf("shipping regions saved %q", got)
	}
	if reviewState(e, p) != "pending" {
		t.Fatal("a change to the additional shipping regions did not re-hold the listing")
	}
}

// A vendor's new photo resubmits a rejected listing, as an edit does.
func TestListingPhotoResubmitsRejectedListing(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, vendor := e.user("photo_resub_vendor", "vendor")
	_, moderator := e.user("photo_resub_mod", "moderator")
	setListingReviewMode(e, "hold")
	p := e.postListing(vendor, "Photo resubmit lamp")
	e.check(e.do("POST", "/moderator/listing-review", moderator, listingReviewPost(e, p, "reject")), 303)
	assertRejected(e, p, rejectReason)
	if w := e.upload("/listings/photo", vendor, e.photoParts(vendor, p, smallJPEG(t))...); w.Code != 303 {
		t.Fatalf("photo upload: %d %s", w.Code, w.Body.String())
	}
	if reviewState(e, p) != "pending" || agStr(e, "SELECT coalesce(review_rejection_reason,'') FROM products WHERE id=$1", p) != "" {
		t.Fatal("a new photo did not resubmit the rejected listing")
	}
	if !e.auditExact(vendorID, "Resubmitted listing "+shortID(p)+" for review") {
		t.Fatal("resubmission by photo was not audited")
	}
	if agInt(e, "SELECT count(*) FROM products WHERE id=$1 AND review_state='pending' AND NOT archived", p) != 1 {
		t.Fatal("the resubmitted listing is not in the review queue")
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

// A listing and its photos are public only while the listing is approved (MOD72-6) and its vendor is not deactivated
// (PR #104): each rule hides it on its own.
func TestListingPublicNeedsApprovalAndActiveVendor(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, vendor := e.user("both_rules_vendor", "vendor")
	p := e.postListing(vendor, "Both rules lamp")
	if w := e.upload("/listings/photo", vendor, e.photoParts(vendor, p, smallJPEG(t))...); w.Code != 303 {
		t.Fatalf("photo upload: %d %s", w.Code, w.Body.String())
	}
	publicPhotos := func() int {
		return agInt(e, "SELECT count(*) FROM "+imagePublicFrom+" WHERE i.product_id=$1 AND "+imagePublicWhere, p)
	}
	if publicPhotos() != 1 {
		t.Fatal("an approved listing of an active vendor has no public photo")
	}
	e.check(e.do("GET", "/product?id="+p, randomToken(), nil), 200)
	for _, c := range []struct{ name, hide, show string }{
		{"deactivated vendor", "UPDATE users SET vendor_deactivated=true WHERE id=$1", "UPDATE users SET vendor_deactivated=false WHERE id=$1"},
		{"pending review", "UPDATE products SET review_state='pending' WHERE vendor_id=$1", "UPDATE products SET review_state='approved' WHERE vendor_id=$1"},
		{"rejected in review", "UPDATE products SET review_state='rejected',review_rejection_reason='" + rejectReason + "' WHERE vendor_id=$1", "UPDATE products SET review_state='approved',review_rejection_reason=NULL WHERE vendor_id=$1"},
	} {
		agExec(e, c.hide, vendorID)
		buyerCannotReach(e, p, "Both rules lamp", vendorID, randomToken())
		if publicPhotos() != 0 {
			t.Fatalf("%s: the public image route serves the listing's photo", c.name)
		}
		agExec(e, c.show, vendorID)
		e.check(e.do("GET", "/product?id="+p, randomToken(), nil), 200)
	}
}

// QA72-1: a photo added to a listing that is still pending makes desk forms rendered before it stale, even when the
// save itself is not held (publish mode here), so a moderator never approves or rejects a photo they have not seen.
func TestListingPhotoOnPendingListingStalesDeskForms(t *testing.T) {
	e, _ := uploadEnv(t)
	_, vendor := e.user("photo_stale_vendor", "vendor")
	_, moderator := e.user("photo_stale_mod", "moderator")
	setListingReviewMode(e, "hold")
	p := e.postListing(vendor, "Stale photo lamp")
	desk := e.body("GET", "/moderator", moderator, nil, 200)
	approve := formOn(t, desk, "/moderator/listing-review", `name="id" value="`+p+`"`, `name="action" value="approve"`)
	approve.Set("password", testPassword)
	reject := formOn(t, desk, "/moderator/listing-review", `name="id" value="`+p+`"`, `name="action" value="reject"`)
	reject.Set("password", testPassword)
	reject.Set("reason", rejectReason)
	setListingReviewMode(e, "publish")
	if w := e.upload("/listings/photo", vendor, e.photoParts(vendor, p, smallJPEG(t))...); w.Code != 303 {
		t.Fatalf("photo upload: %d %s", w.Code, w.Body.String())
	}
	agExpect(e, "/moderator/listing-review", moderator, approve, 409, "changed this listing after you opened")
	agExpect(e, "/moderator/listing-review", moderator, reject, 409, "changed this listing after you opened")
	if reviewState(e, p) != "pending" {
		t.Fatal("a stale review form changed the listing")
	}
}

// QA72-2: an AUTO_APPLY moderation proposal for a pending listing leaves it pending and hidden.
func TestListingHoldSurvivesAutoApply(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("autoapply_vendor", "vendor")
	_, buyer := e.user("autoapply_buyer", "buyer")
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	setListingReviewMode(e, "hold")
	p := e.postListing(vendor, "Auto apply lamp")
	cfg := moderation.DefaultConfig()
	cfg.Enabled = true
	worker := &ModerationWorker{DB: e.DB, Engine: moderation.Engine{Config: cfg, Model: benignModerationUIModel{}}}
	if _, err := worker.Pass(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	if got := agStr(e, "SELECT disposition FROM moderation_suggestions WHERE target_id=$1", p); got != string(moderation.AutoApply) {
		t.Fatalf("the fake model's proposal was routed %s, not AUTO_APPLY", got)
	}
	if reviewState(e, p) != "pending" {
		t.Fatal("an AUTO_APPLY proposal changed the listing's review state")
	}
	buyerCannotReach(e, p, "Auto apply lamp", vendorID, randomToken(), buyer)
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

// TestModerationNeverWritesReviewState: the moderation layer (capture triggers, the worker in internal/market and its
// cmd/server launcher, the engine and the model path) never names the review columns or settings, writes products, or
// refers to anything listing_review.go declares (holdListing, listingReviewAction and every helper added there), so no
// model output or capture outcome can publish, hold or reject a listing (QA72-2).
func TestModerationNeverWritesReviewState(t *testing.T) {
	fset := token.NewFileSet()
	writer, err := parser.ParseFile(fset, "internal/market/listing_review.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	writers := map[string]bool{}
	for _, d := range writer.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name != "init" {
				writers[d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch spec := spec.(type) {
				case *ast.ValueSpec:
					for _, n := range spec.Names {
						writers[n.Name] = true
					}
				case *ast.TypeSpec:
					writers[spec.Name.Name] = true
				}
			}
		}
	}
	for _, name := range []string{"holdListing", "listingHeld", "listingReviewAction", "listingReviewModeAction", "vendorAutoApproveAction"} {
		if !writers[name] {
			t.Fatalf("listing_review.go no longer declares %s: update this guard", name)
		}
	}
	forbidden := regexp.MustCompile(`review_state|review_rejection_reason|listing_auto_approve|listing_review_mode|/moderator/listing-review|(?i:update\s+products\b)`)
	scanned := map[string]int{}
	var found []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel := filepath.ToSlash(path)
			group := ""
			for _, prefix := range []string{"internal/moderation/", "internal/market/moderation", "cmd/server/moderation"} {
				if strings.HasPrefix(rel, prefix) {
					group = prefix
				}
			}
			if group == "" {
				return nil
			}
			scanned[group]++
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if m := forbidden.Find(src); m != nil {
				found = append(found, rel+": "+string(m))
			}
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && writers[id.Name] {
					found = append(found, rel+": "+id.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, prefix := range []string{"internal/moderation/", "internal/market/moderation", "cmd/server/moderation"} {
		if scanned[prefix] == 0 {
			t.Fatalf("the guard scanned no %s* file", prefix)
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
		if strings.Contains(string(b), "review_state") && filepath.Base(m) != "142_listing_review.sql" {
			found = append(found, m)
		}
	}
	if len(found) > 0 {
		t.Fatalf("moderation code or another migration reaches listing review: %v", found)
	}
	e := newTestApp(t) // skips without TEST_DATABASE_URL, after the source checks above

	if n := agInt(e, "SELECT count(*) FROM pg_proc WHERE prosrc LIKE '%review_state%'"); n != 0 {
		t.Fatalf("%d database functions or triggers name review_state", n)
	}
}
