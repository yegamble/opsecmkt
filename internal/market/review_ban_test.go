package market

import (
	"os"
	"strings"
	"testing"
)

// A-649 (owner directive): once a buyer is banned, none of their reviews, past or future, count toward vendor ratings.
// The exclusion is a live join on users.banned_at (reviewCountedSQL), so nothing is written to reviews and lifting the
// ban restores them. The buyer and vendor see the same neutral sentence on the order for a banned buyer's review as for
// one an administrator hid; only administrators see the cause.

const (
	reviewNotShown     = "Not shown on the listing and vendor pages and not counted in the rating."
	reviewBannedLabel  = "Not counted: reviewer's account is banned"
	reviewBannedPolicy = "Reviews by banned accounts are not shown or counted."
)

// bannedReviewFixture: one product with a 1-star review by the buyer who will be banned and a 5-star review by
// another buyer, so the public rating is 3.0 from 2 before the ban and 5.0 from 1 after it.
type bannedReviewFixture struct {
	e                                   *testEnv
	adminID, admin, vendorID, vendor    string
	buyerID, buyer, otherID, other      string
	product, bannedOrder, kept, bodyBad string
	prefix                              string
}

func newBannedReviewFixture(t *testing.T, prefix string) *bannedReviewFixture {
	e := newTestApp(t)
	f := &bannedReviewFixture{e: e, prefix: prefix, bodyBad: "Never arrived and the vendor stopped answering."}
	f.adminID, f.admin = e.user(prefix+"_admin", "admin")
	f.vendorID, f.vendor = e.user(prefix+"_vendor", "vendor")
	f.buyerID, f.buyer = e.user(prefix+"_buyer", "buyer")
	f.otherID, f.other = e.user(prefix+"_other", "buyer")
	f.product = e.product(f.vendorID, "physical")
	f.bannedOrder = e.order(f.buyerID, f.product, "XMR", stateCompleted)
	f.kept = e.order(f.otherID, f.product, "XMR", stateCompleted)
	agExpect(e, "/reviews", f.buyer, form("order_id", f.bannedOrder, "rating", "1", "body", f.bodyBad), 303, "")
	agExpect(e, "/reviews", f.other, form("order_id", f.kept, "rating", "5", "body", "Arrived as described."), 303, "")
	return f
}

func (f *bannedReviewFixture) ban(action string) {
	f.e.t.Helper()
	agExpect(f.e, "/admin/ban", f.admin, banForm(action, f.prefix+"_buyer", testBanReason), 303, "")
}

// publicRating checks the product page, vendor page and catalog card as a guest, the vendor, the other buyer and a
// moderator.
func (f *bannedReviewFixture) publicRating(t *testing.T, summary, card string, banned bool, moderator string) {
	t.Helper()
	for _, s := range []string{randomToken(), f.vendor, f.other, moderator} {
		for _, path := range []string{"/product?id=" + f.product, "/vendor?id=" + f.vendorID} {
			body := f.e.body("GET", path, s, nil, 200)
			mustContain(t, body, summary, "Arrived as described.", reviewBannedPolicy)
			mustNotContain(t, body, reviewBannedLabel, "by a banned account", "by banned accounts are not shown to others")
			if banned {
				mustNotContain(t, body, f.bodyBad)
			} else {
				mustContain(t, body, f.bodyBad)
			}
		}
		mustContain(t, f.e.body("GET", "/", s, nil, 200), card)
	}
}

func TestBannedBuyerReviewsExcludedFromRating(t *testing.T) {
	f := newBannedReviewFixture(t, "rban")
	e := f.e
	_, moderator := e.user("rban_moderator", "moderator")
	f.publicRating(t, "<strong>3.0</strong> / 5 · 2 reviews", `aria-label="Rated 3.0 out of 5 from 2 reviews"`, false, moderator)

	f.ban("ban")
	f.publicRating(t, "<strong>5.0</strong> / 5 · 1 review<", `aria-label="Rated 5.0 out of 5 from 1 reviews"`, true, moderator)
	// Nothing is written to the review: the exclusion is the live ban.
	if agInt(e, "SELECT count(*) FROM reviews WHERE hidden_at IS NOT NULL") != 0 || agInt(e, "SELECT count(*) FROM reviews") != 2 {
		t.Fatal("banning wrote to reviews")
	}

	// A review by the account recorded after the ban (future reviews) does not count either.
	later := e.order(f.buyerID, f.product, "XMR", stateCompleted)
	agExec(e, "INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body) VALUES($1,$2,$3,$4,1,'Written after the ban.')", randomToken(), later, f.product, f.buyerID)
	f.publicRating(t, "<strong>5.0</strong> / 5 · 1 review<", `aria-label="Rated 5.0 out of 5 from 1 reviews"`, true, moderator)
	mustNotContain(t, e.body("GET", "/product?id="+f.product, randomToken(), nil, 200), "Written after the ban.")

	// With every review by a banned account, the public page shows the empty state, the policy and no rating.
	agExec(e, "DELETE FROM reviews WHERE buyer_id=$1", f.otherID)
	body := e.body("GET", "/product?id="+f.product, randomToken(), nil, 200)
	mustContain(t, body, "No verified reviews yet", reviewBannedPolicy)
	mustNotContain(t, body, "review-summary", "Written after the ban.", f.bodyBad)
}

func TestUnbanRestoresReviews(t *testing.T) {
	f := newBannedReviewFixture(t, "runban")
	e := f.e
	_, moderator := e.user("runban_moderator", "moderator")
	// A review an administrator hid stays hidden through the ban and after it is lifted.
	hiddenOrder := e.order(f.buyerID, f.product, "XMR", stateCompleted)
	agExpect(e, "/reviews", f.buyer, form("order_id", hiddenOrder, "rating", "2", "body", "Hidden by staff before the ban."), 303, "")
	agExec(e, "UPDATE reviews SET hidden_at=now() WHERE order_id=$1", hiddenOrder)

	f.ban("ban")
	f.publicRating(t, "<strong>5.0</strong> / 5 · 1 review<", `aria-label="Rated 5.0 out of 5 from 1 reviews"`, true, moderator)
	f.ban("unban")
	f.publicRating(t, "<strong>3.0</strong> / 5 · 2 reviews", `aria-label="Rated 3.0 out of 5 from 2 reviews"`, false, moderator)
	mustNotContain(t, e.body("GET", "/product?id="+f.product, randomToken(), nil, 200), "Hidden by staff before the ban.")
	if agInt(e, "SELECT count(*) FROM reviews WHERE hidden_at IS NOT NULL") != 1 {
		t.Fatal("the ban or unban changed an administrator's hide")
	}
}

// reviewSection returns the order page's Review panel.
func reviewSection(t *testing.T, page string) string {
	t.Helper()
	_, s, ok := strings.Cut(page, `<section class="panel" aria-labelledby="order-review">`)
	if !ok {
		t.Fatal("no review panel on the order page")
	}
	s, _, _ = strings.Cut(s, "</section>")
	return s
}

func TestBannedBuyerReviewVendorTextMatchesHide(t *testing.T) {
	f := newBannedReviewFixture(t, "rtext")
	e := f.e
	_, moderator := e.user("rtext_moderator", "moderator")
	hiderID, hider := e.user("rtext_hider", "buyer")
	hiddenOrder := e.order(hiderID, f.product, "XMR", stateCompleted)
	agExpect(e, "/reviews", hider, form("order_id", hiddenOrder, "rating", "1", "body", f.bodyBad), 303, "")
	agExec(e, "UPDATE reviews SET created='2026-01-02 03:04:05+00'")
	agExec(e, "UPDATE reviews SET hidden_at=now() WHERE order_id=$1", hiddenOrder)
	// Both orders are disputed and resolved, so a moderator may read them too (staffReviewer).
	for _, o := range []string{hiddenOrder, f.bannedOrder} {
		agExec(e, "INSERT INTO disputes(id,order_id,reason,status,outcome) VALUES($1,$2,'Fixture dispute reason text','Resolved','release')", randomToken(), o)
	}
	f.ban("ban")

	for _, s := range []string{f.vendor, moderator} {
		hidden := reviewSection(t, e.body("GET", "/order?id="+hiddenOrder, s, nil, 200))
		banned := reviewSection(t, e.body("GET", "/order?id="+f.bannedOrder, s, nil, 200))
		mustContain(t, hidden, reviewNotShown, f.bodyBad)
		mustNotContain(t, hidden, "Hidden by an administrator", "Hidden from the listing", "banned")
		if hidden != banned {
			t.Fatalf("the order page tells a hidden review from a banned buyer's:\nhidden: %s\nbanned: %s", hidden, banned)
		}
	}
	// The buyer whose review was hidden reads the same sentence.
	mustContain(t, reviewSection(t, e.body("GET", "/order?id="+hiddenOrder, hider, nil, 200)), reviewNotShown, f.bodyBad)
	// A counted review carries no such sentence.
	mustNotContain(t, reviewSection(t, e.body("GET", "/order?id="+f.kept, f.vendor, nil, 200)), reviewNotShown, "Not shown")
}

func TestBannedBuyerReviewAdminLabel(t *testing.T) {
	f := newBannedReviewFixture(t, "rlabel")
	e := f.e
	agExec(e, "INSERT INTO disputes(id,order_id,reason,status,outcome) VALUES($1,$2,'Fixture dispute reason text','Resolved','release')", randomToken(), f.bannedOrder)
	f.ban("ban")
	badID := agStr(e, "SELECT id FROM reviews WHERE order_id=$1", f.bannedOrder)
	for _, path := range []string{"/product?id=" + f.product, "/vendor?id=" + f.vendorID} {
		page := e.body("GET", path, f.admin, nil, 200)
		mustContain(t, page, f.bodyBad, reviewBannedLabel, "<strong>5.0</strong> / 5 · 1 review<",
			"1 review by a banned account is not shown to others or counted.", reviewBannedPolicy)
		mustNotContain(t, page, "hidden review")
		// The administrator can still hide it, so it stays off the public pages if the ban is lifted.
		reviewForm(t, page, "hide", badID)
	}
	order := reviewSection(t, e.body("GET", "/order?id="+f.bannedOrder, f.admin, nil, 200))
	mustContain(t, order, reviewNotShown, reviewBannedLabel)
	mustNotContain(t, order, "Hidden by an administrator")
	// Hidden as well: both causes are labelled, and the review is counted once, as hidden.
	agExec(e, "UPDATE reviews SET hidden_at=now() WHERE id=$1", badID)
	page := e.body("GET", "/product?id="+f.product, f.admin, nil, 200)
	mustContain(t, page, reviewBannedLabel, "Hidden by an administrator", "1 hidden review is not shown to others or counted.")
	mustNotContain(t, page, "by a banned account is not shown")
	mustContain(t, reviewSection(t, e.body("GET", "/order?id="+f.bannedOrder, f.admin, nil, 200)), reviewBannedLabel, "Hidden by an administrator")
}

func TestBanAuditCountsReviews(t *testing.T) {
	f := newBannedReviewFixture(t, "raudit")
	e := f.e
	// A second counted review, one an administrator hid and one on an order that is not completed: two stop counting.
	second := e.order(f.buyerID, f.product, "XMR", stateCompleted)
	agExpect(e, "/reviews", f.buyer, form("order_id", second, "rating", "4", "body", "Second order."), 303, "")
	hidden := e.order(f.buyerID, f.product, "XMR", stateCompleted)
	agExpect(e, "/reviews", f.buyer, form("order_id", hidden, "rating", "2", "body", "Hidden one."), 303, "")
	agExec(e, "UPDATE reviews SET hidden_at=now() WHERE order_id=$1", hidden)
	disputed := e.order(f.buyerID, f.product, "XMR", stateCompleted)
	agExpect(e, "/reviews", f.buyer, form("order_id", disputed, "rating", "2", "body", "Then disputed."), 303, "")
	agExec(e, "UPDATE orders SET state='disputed' WHERE id=$1", disputed)

	handle := "raudit_buyer"
	f.ban("ban")
	if !e.auditExact(f.buyerID, "Account banned by administrator raudit_admin: "+banAudit+"; ended 1 session(s) and any pending sign-ins; 2 review(s) by the account stopped counting toward ratings") ||
		!e.auditExact(f.adminID, "Banned account "+handle+": "+banAudit+"; ended 1 session(s) and any pending sign-ins; 2 review(s) by the account stopped counting toward ratings (confirmed with password)") {
		t.Fatal("the ban audit does not count the reviews that stopped counting")
	}
	agExpect(e, "/admin/ban", f.admin, banForm("unban", handle, testLiftReason), 303, "")
	if !e.auditExact(f.buyerID, "Ban lifted by administrator raudit_admin: "+liftAudit+"; 2 review(s) by the account count toward ratings again") ||
		!e.auditExact(f.adminID, "Lifted the ban on "+handle+": "+liftAudit+"; 2 review(s) by the account count toward ratings again (confirmed with password)") {
		t.Fatal("the unban audit does not count the reviews that count again")
	}
	// An account with no counted reviews keeps the audit text it had.
	agExpect(e, "/admin/ban", f.admin, banForm("ban", "raudit_vendor", testBanReason), 303, "")
	if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE '%review(s)%'", f.vendorID) != 0 {
		t.Fatal("a ban of an account with no counted reviews mentions reviews")
	}
}

// TestReviewCountedSQLShared: the rating (load.go) and the public review summaries and lists (orders_load.go) read
// whether a review counts from one fragment, so the ban exclusion cannot drift between them. The behaviour itself is
// checked above; this catches a new query that tests hidden_at alone.
func TestReviewCountedSQLShared(t *testing.T) {
	for _, file := range []string{"load.go", "orders_load.go"} {
		src, err := os.ReadFile("internal/market/" + file) // the tests run from the repository root (TestMain)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), `reviewCountedSQL("r")`) {
			t.Fatalf("%s does not use reviewCountedSQL", file)
		}
		if strings.Contains(string(src), "r.hidden_at IS NULL") {
			t.Fatalf("%s tests r.hidden_at IS NULL itself instead of using reviewCountedSQL", file)
		}
	}
}
