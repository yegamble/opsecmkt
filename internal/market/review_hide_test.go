package market

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// A-125: an administrator hides a public review (it is kept, never deleted) from the product and vendor pages and
// from their rating, and can restore it. Both are password-confirmed, audited without the review text and notified
// to the reviewer; administrators see hidden reviews labelled, and every other role is refused.

const doxx = "The seller is Jane Example of 12 Harbour Street"

var hiddenInput = regexp.MustCompile(`<input type="hidden" name="([a-z_]+)" value="([^"]*)">`)

// reviewForm returns the fields of the /admin/review form on page for op (hide or restore) whose review_id is
// reviewID, as the browser would submit them with the administrator's password.
func reviewForm(t *testing.T, page, op, reviewID string) url.Values {
	t.Helper()
	for _, f := range strings.Split(page, "<form ")[1:] {
		f, _, _ = strings.Cut(f, "</form>")
		if !strings.Contains(f, `action="/admin/review"`) || !strings.Contains(f, `name="action" value="`+op+`"`) || !strings.Contains(f, `name="review_id" value="`+reviewID+`"`) {
			continue
		}
		mustContain(t, f, `name="password" required`)
		v := url.Values{"password": {testPassword}}
		for _, m := range hiddenInput.FindAllStringSubmatch(f, -1) {
			if m[1] != "csrf" {
				v.Set(m[1], m[2])
			}
		}
		return v
	}
	t.Fatalf("no %s form for review %s", op, reviewID)
	return nil
}

func TestAdminHidesAndRestoresReview(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("hide_admin", "admin")
	vendorID, vendor := e.user("hide_vendor", "vendor")
	buyerID, buyer := e.user("hide_buyer", "buyer")
	product := e.product(vendorID, "physical")
	bad := e.order(buyerID, product, "BTC", stateCompleted)
	good := e.order(buyerID, product, "BTC", stateCompleted)
	agExpect(e, "/reviews", buyer, form("order_id", bad, "rating", "1", "body", doxx), 303, "")
	agExpect(e, "/reviews", buyer, form("order_id", good, "rating", "5", "body", "Arrived as described."), 303, "")
	badID := agStr(e, "SELECT id FROM reviews WHERE order_id=$1", bad)
	productPage, vendorPage := "/product?id="+product, "/vendor?id="+vendorID
	for _, path := range []string{productPage, vendorPage} {
		mustContain(t, e.body("GET", path, randomToken(), nil, 200), doxx, "<strong>3.0</strong> / 5 · 2 reviews")
		// Nobody but an administrator is offered the form.
		for _, s := range []string{randomToken(), buyer, vendor} {
			mustNotContain(t, e.body("GET", path, s, nil, 200), `action="/admin/review"`, badID)
		}
	}

	// Refusals change nothing: other roles (moderators included), a missing or wrong password, an unknown review,
	// an unknown choice, and a forged or cross-site request.
	_, moderator := e.user("hide_moderator", "moderator")
	hide := reviewForm(t, e.body("GET", productPage, admin, nil, 200), "hide", badID)
	with := func(k, v string) url.Values {
		f := url.Values{}
		for key, vals := range hide {
			f[key] = append([]string(nil), vals...)
		}
		f.Set(k, v)
		return f
	}
	before := agInt(e, "SELECT count(*) FROM audit_events")
	for _, s := range []string{moderator, vendor, buyer} {
		agExpect(e, "/admin/review", s, with("from", "product"), 403, "")
	}
	agExpect(e, "/admin/review", randomToken(), with("from", "product"), 401, "Sign in required")
	agExpect(e, "/admin/review", admin, with("password", ""), 400, "Enter your current password")
	agExpect(e, "/admin/review", admin, with("password", "wrong-password-here"), 401, "Password incorrect")
	agExpect(e, "/admin/review", admin, with("review_id", "no-such-review"), 404, "Review not found")
	agExpect(e, "/admin/review", admin, with("action", "delete"), 400, "Choose hide or restore")
	for _, tc := range []struct{ csrf, site string }{{e.A.csrf(randomToken()), ""}, {"", ""}, {e.A.csrf(admin), "cross-site"}} {
		f := with("csrf", tc.csrf)
		r := httptest.NewRequest("POST", "/admin/review", strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", tc.site)
		r.AddCookie(&http.Cookie{Name: "session", Value: admin})
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		e.check(w, 403)
	}
	if agInt(e, "SELECT count(*) FROM reviews WHERE hidden_at IS NOT NULL") != 0 || agInt(e, "SELECT count(*) FROM audit_events") != before ||
		agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", buyerID) != 0 {
		t.Fatal("a refused request changed state")
	}

	// The administrator hides it from the product page and returns there.
	w := e.do("POST", "/admin/review", admin, hide)
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != productPage+"&saved=1#reviews-title" {
		t.Fatalf("redirect %q", loc)
	}
	agExpect(e, "/admin/review", admin, hide, 409, "already hidden")
	if agInt(e, "SELECT count(*) FROM reviews WHERE id=$1 AND hidden_at IS NOT NULL AND body=$2", badID, doxx) != 1 {
		t.Fatal("review not hidden, or not kept")
	}
	short := shortID(bad)
	if !e.auditExact(adminID, "Hid review of order "+short+" (confirmed with password)") ||
		!e.auditExact(buyerID, "Review of order "+short+" hidden by administrator hide_admin") {
		t.Fatal("hide not audited on both accounts")
	}
	if agInt(e, "SELECT count(*) FROM audit_events WHERE action LIKE '%Jane Example%'") != 0 {
		t.Fatal("the audit copies the review text")
	}
	note := "An administrator hid your review of order " + short + " from the listing and vendor pages, and it no longer counts in the rating. It is kept: you and the vendor still see it on the order."
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", buyerID, note) != 1 {
		t.Fatal("reviewer not notified")
	}

	// Gone publicly, rating included; the administrator sees it labelled, with a restore form.
	for _, path := range []string{productPage, vendorPage} {
		for _, s := range []string{randomToken(), buyer, vendor, moderator} {
			body := e.body("GET", path, s, nil, 200)
			mustContain(t, body, "Arrived as described.", "<strong>5.0</strong> / 5 · 1 review<")
			mustNotContain(t, body, doxx, "Hidden by an administrator")
		}
		page := e.body("GET", path, admin, nil, 200)
		mustContain(t, page, doxx, "Hidden by an administrator", "<strong>5.0</strong> / 5 · 1 review<", "1 hidden review is not shown to others or counted.")
		reviewForm(t, page, "restore", badID)
	}
	// The parties still see it on the order, labelled; the buyer cannot publish a second one.
	for _, s := range []string{buyer, vendor} {
		body := e.body("GET", "/order?id="+bad, s, nil, 200)
		mustContain(t, body, doxx, "Hidden from the listing and vendor pages by an administrator.")
		mustNotContain(t, body, "Publish review")
	}

	// Restoring from the vendor page returns there; it is audited and notified the same way.
	restore := reviewForm(t, e.body("GET", vendorPage, admin, nil, 200), "restore", badID)
	w = e.do("POST", "/admin/review", admin, restore)
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != vendorPage+"&saved=1#reviews-title" {
		t.Fatalf("redirect %q", loc)
	}
	agExpect(e, "/admin/review", admin, restore, 409, "is not hidden")
	if !e.auditExact(adminID, "Restored review of order "+short+" (confirmed with password)") ||
		!e.auditExact(buyerID, "Review of order "+short+" restored by administrator hide_admin") {
		t.Fatal("restore not audited on both accounts")
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", buyerID, "An administrator restored your review of order "+short+" to the listing and vendor pages.") != 1 {
		t.Fatal("reviewer not notified of the restore")
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%http%'", buyerID) != 0 {
		t.Fatal("a notification carries a link")
	}
	mustContain(t, e.body("GET", productPage, randomToken(), nil, 200), doxx, "<strong>3.0</strong> / 5 · 2 reviews")

	// An enrolled administrator also gives a current authenticator code; the form asks for it.
	key, _ := agEnableTOTP(e, adminID)
	page := e.body("GET", productPage, admin, nil, 200)
	mustContain(t, adminForm(t, page, "/admin/review", "hide"), `name="code" required`)
	agExpect(e, "/admin/review", admin, hide, 401, "")
	hide.Set("code", agTOTPNow(key))
	agExpect(e, "/admin/review", admin, hide, 303, "")
	if !e.auditExact(adminID, "Hid review of order "+short+" (confirmed with password and authenticator code)") {
		t.Fatal("TOTP-confirmed hide not audited")
	}
	// With every review hidden, the public page shows the empty state and no rating.
	agExec(e, "UPDATE reviews SET hidden_at=now()")
	body := e.body("GET", productPage, randomToken(), nil, 200)
	mustContain(t, body, "No verified reviews yet")
	mustNotContain(t, body, "review-summary")
	// /canary#records tells users administrators can hide reviews.
	mustContain(t, e.body("GET", "/canary", randomToken(), nil, 200), "Administrators can hide a review from the public pages; it is kept, not deleted")
}
