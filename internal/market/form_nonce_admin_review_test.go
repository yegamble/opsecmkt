package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestAdminReviewNonceBindsReplayToFormActorAndSession(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("review_nonce_admin", "admin")
	buyerID, buyer := e.user("review_nonce_buyer", "buyer")
	vendorID, _ := e.user("review_nonce_vendor", "vendor")
	product := e.product(vendorID, "physical")
	order := e.order(buyerID, product, "XMR", stateCompleted)
	e.check(e.do("POST", "/reviews", buyer, url.Values{"order_id": {order}, "rating": {"1"}, "body": {"Needs review."}}), 303)
	reviewID := agStr(e, "SELECT id FROM reviews WHERE order_id=$1", order)

	beforeGET := agInt(e, "SELECT count(*) FROM form_submissions")
	page := e.body("GET", "/product?id="+product, admin, nil, 200)
	form := reviewForm(t, page, "hide", reviewID)
	if got := submissionCount(t, e); got != beforeGET {
		t.Fatalf("review GET changed receipt count from %d to %d", beforeGET, got)
	}
	wrongPassword := url.Values{}
	for key, values := range form {
		wrongPassword[key] = append([]string(nil), values...)
	}
	wrongPassword.Set("password", "incorrect-password")
	e.check(e.do("POST", "/admin/review", admin, wrongPassword), 401)
	badChoice := url.Values{}
	for key, values := range form {
		badChoice[key] = append([]string(nil), values...)
	}
	badChoice.Set("action", "delete")
	e.check(e.do("POST", "/admin/review", admin, badChoice), 400)
	if got := submissionCount(t, e); got != beforeGET {
		t.Fatalf("refused forms persisted %d receipts", got)
	}

	first := e.do("POST", "/admin/review", admin, form)
	e.check(first, 303)
	replay := e.do("POST", "/admin/review", admin, form)
	e.check(replay, 303)
	if first.Header().Get("Location") != replay.Header().Get("Location") || first.Header().Get("Location") != "/product?id="+product+"#reviews-title" {
		t.Fatalf("replay redirect %q, original %q", replay.Header().Get("Location"), first.Header().Get("Location"))
	}
	if got := submissionCount(t, e); got != beforeGET+1 {
		t.Fatalf("successful hide/replay left %d receipts, want %d", got, beforeGET+1)
	}
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Hid review of order %'", adminID); got != 1 {
		t.Fatalf("hide/replay wrote %d administrator audits", got)
	}
	if got := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator hid your review%'", buyerID); got != 1 {
		t.Fatalf("hide/replay wrote %d reviewer notices", got)
	}

	changed := url.Values{}
	for key, values := range form {
		changed[key] = append([]string(nil), values...)
	}
	changed.Set("from", "vendor")
	e.check(e.do("POST", "/admin/review", admin, changed), 409)
	otherSession := randomToken()
	if _, err := e.DB.Exec("INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '1 hour')", digest(otherSession), adminID); err != nil {
		t.Fatal(err)
	}
	e.check(e.do("POST", "/admin/review", otherSession, form), 409)
	_, otherAdmin := e.user("review_nonce_other_admin", "admin")
	e.check(e.do("POST", "/admin/review", otherAdmin, form), 409)
	if _, err := e.DB.Exec("UPDATE users SET role='moderator' WHERE id=$1", adminID); err != nil {
		t.Fatal(err)
	}
	e.check(e.do("POST", "/admin/review", admin, form), 403)
	if got := submissionCount(t, e); got != beforeGET+1 {
		t.Fatalf("mismatched requests changed receipt count to %d", got)
	}
}

func TestAdminReviewNonceLostCommitAcknowledgementReplays(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("review_nonce_lost_admin", "admin")
	buyerID, buyer := e.user("review_nonce_lost_buyer", "buyer")
	vendorID, _ := e.user("review_nonce_lost_vendor", "vendor")
	product := e.product(vendorID, "physical")
	order := e.order(buyerID, product, "XMR", stateCompleted)
	e.check(e.do("POST", "/reviews", buyer, url.Values{"order_id": {order}, "rating": {"1"}, "body": {"Needs review."}}), 303)
	reviewID := agStr(e, "SELECT id FROM reviews WHERE order_id=$1", order)
	form := reviewForm(t, e.body("GET", "/product?id="+product, admin, nil, 200), "hide", reviewID)

	lost := loseNextActionCommit(t, e)
	w := e.do("POST", "/admin/review", admin, form)
	if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
		!strings.Contains(w.Body.String(), "Review change outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
		t.Fatalf("lost acknowledgement: injected=%t status=%d retry=%q body=%q", !lost.Load(), w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	e.check(e.do("POST", "/admin/review", admin, form), 303)
	if got := agInt(e, "SELECT count(*) FROM form_submissions WHERE action_path='/admin/review'"); got != 1 {
		t.Fatalf("lost-ack replay left %d admin review receipts", got)
	}
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Hid review of order %'", adminID); got != 1 {
		t.Fatalf("lost-ack replay wrote %d administrator audits", got)
	}
	if got := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator hid your review%'", buyerID); got != 1 {
		t.Fatalf("lost-ack replay wrote %d reviewer notices", got)
	}
}
