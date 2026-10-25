package market

import (
	"net/url"
	"testing"
)

func TestModerationDeterministicScamAppearsInPrivateReviewQueue(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("qa_scam_established_vendor", "vendor")
	_, admin := e.user("qa_scam_queue_admin", "admin")
	agExec(e, `UPDATE users SET created=now()-interval '90 days' WHERE id=$1`, vendor)
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	product := e.titledListing(vendor, "Guaranteed profit if you send your seed")
	if count, err := moderationTestWorker(e).Pass(t.Context(), 1); count != 1 || err != nil {
		t.Fatalf("pass count=%d err=%v", count, err)
	}
	var id, disposition string
	if err := e.DB.QueryRow(`SELECT id,disposition FROM moderation_suggestions WHERE target_id=$1 AND reason_codes ? 'SCAM_TEMPLATE'`, product).Scan(&id, &disposition); err != nil {
		t.Fatal(err)
	}
	if disposition != "QUEUE_HUMAN" {
		t.Fatalf("scam finding hidden with disposition %s", disposition)
	}
	if count := agInt(e, `SELECT unreviewed FROM moderation_capacity WHERE singleton`); count != 1 {
		t.Fatalf("unreviewed=%d", count)
	}
	e.A.adminSocket = "/run/qa-private-admin/http.sock"
	page := privateHostRequest(e, "GET", "/admin/moderation", admin, nil, true)
	e.check(page, 200)
	mustContain(t, page.Body.String(), id, "SCAM_TEMPLATE", "QUEUE_HUMAN")
	e.check(e.do("GET", "/admin/moderation", admin, nil), 404)
	if count := agInt(e, `SELECT count(*) FROM products WHERE id=$1 AND NOT archived`, product); count != 1 {
		t.Fatal("classification changed listing state")
	}
}

func TestModerationQueueOverflowPreservesListingAndDisputeHTTPFlows(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	e := x.testEnv
	vendor, vendorSession := x.vendor, x.vendorSess
	adminID, admin := e.user("qa_overflow_admin", "admin")
	originalProduct := x.physical(vendor)
	order, buyerSession := x.paidXMRPhysical(originalProduct)
	limits := DefaultModerationLimits()
	limits.MaxPending = 1
	if err := ConfigureModerationLimits(t.Context(), e.DB, limits); err != nil {
		t.Fatal(err)
	}
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	e.product(vendor, "digital")
	listingForm := url.Values{"title": {"Listing while moderation capture is full"}, "description": {"A normal product description"}, "category": {"Hardware"}, "region": {"Worldwide"}, "kind": {"physical"}, "price_xmr": {"0.5"}, "stock": {"5"}}
	created := e.do("POST", "/listings", vendorSession, listingForm)
	e.check(created, 303)
	location, err := url.Parse(created.Header().Get("Location"))
	if err != nil || location.Path != "/listing-edit" {
		t.Fatalf("listing response Location=%q err=%v", created.Header().Get("Location"), err)
	}
	target := location.Query().Get("id")
	public := e.do("GET", "/product?id="+target, "", nil)
	e.check(public, 200)
	mustContain(t, public.Body.String(), listingForm.Get("title"))
	e.check(e.do("POST", "/disputes", buyerSession, url.Values{"order_id": {order}, "reason": {"The supplied parcel differs from its listing description."}}), 303)
	if count := agInt(e, `SELECT count(*) FROM disputes WHERE order_id=$1 AND status='Open'`, order); count != 1 {
		t.Fatal("overflow lost the dispute")
	}
	if state := agStr(e, `SELECT state FROM orders WHERE id=$1`, order); state != stateDisputed {
		t.Fatalf("dispute state=%s", state)
	}
	if count := agInt(e, `SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Dispute opened on order %'`, adminID); count != 1 {
		t.Fatalf("normal staff notices=%d", count)
	}
	if count := agInt(e, `SELECT pending FROM moderation_capacity WHERE singleton`); count != 1 {
		t.Fatalf("pending=%d exceeds capacity", count)
	}
	if count := agInt(e, `SELECT count(*) FROM moderation_suggestions`); count != 0 {
		t.Fatal("overflow invented a clean moderation decision")
	}
	for _, kind := range []string{"listing", "dispute"} {
		if count := agInt(e, `SELECT occurrences FROM moderation_overflow WHERE kind=$1 AND reason='pending_capacity'`, kind); count != 1 {
			t.Fatalf("%s overflow notices=%d", kind, count)
		}
	}
	e.A.adminSocket = "/run/qa-private-admin/http.sock"
	warning := privateHostRequest(e, "GET", "/admin/moderation", admin, nil, true)
	e.check(warning, 200)
	mustContain(t, warning.Body.String(), "pending_capacity", "Marketplace writes succeeded")
}
