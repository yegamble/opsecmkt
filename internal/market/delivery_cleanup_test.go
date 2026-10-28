package market

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func cleanupDemote(t *testing.T, e *testEnv, handle, role string) (string, string) {
	t.Helper()
	e.user("cleanup_admin", "admin")
	id, session := e.user(handle, "vendor")
	e.candidateFactor(handle)
	if _, err := SetRole(context.Background(), e.DB, handle, role); err != nil {
		t.Fatal(err)
	}
	return id, session
}

func cleanupListing(t *testing.T, e *testEnv, owner, title, content string) string {
	t.Helper()
	id := e.product(owner, "digital")
	if _, err := e.DB.Exec(`UPDATE products SET title=$2,archived=true,archived_at=now(),delivery_content=$3 WHERE id=$1`, id, title, content); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDemotedOwnerCanClearOnlyArchivedDeliveryText(t *testing.T) {
	e := newTestApp(t)
	e.user("cleanup_admin", "admin")
	owner, session := e.user("cleanup_buyer", "vendor")
	id := e.product(owner, "digital")
	if _, err := e.DB.Exec("UPDATE products SET title='Old download listing',delivery_content='secret-licence-and-password' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := SetRole(context.Background(), e.DB, "cleanup_buyer", "buyer"); err != nil {
		t.Fatal(err)
	}
	var archived bool
	if err := e.DB.QueryRow("SELECT archived FROM products WHERE id=$1", id).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if !archived {
		t.Fatal("demoting the owner did not archive their listing")
	}
	other, _ := e.user("cleanup_other", "vendor")
	otherID := cleanupListing(t, e, other, "Other owner's secret title", "other-private-template")
	_, admin := e.user("cleanup_admin_user", "admin")

	page := e.body("GET", "/delivery-cleanup", session, nil, 200)
	mustContain(t, page, "Old download listing", id)
	mustNotContain(t, page, "secret-licence-and-password", "other-private-template", "Other owner's secret title", otherID)
	if strings.Contains(page, `action="/listings/update"`) || strings.Contains(page, `name="delivery_content"`) {
		t.Fatal("cleanup page unexpectedly exposes listing-edit controls")
	}
	account := e.body("GET", "/account", session, nil, 200)
	mustContain(t, account, `href="/delivery-cleanup"`)
	// Keeping the owner's erasure path does not restore selling authority or
	// permit edits, archive changes, or publication of the archived listing.
	for _, path := range []string{"/listings/update", "/listings/archive", "/listings/restore"} {
		e.check(e.do("POST", path, session, inventoryForm(id, nil)), 403)
	}

	bad := e.do("POST", "/delivery-cleanup", session, url.Values{"id": {id}, "password": {"wrong password"}})
	e.check(bad, 401)
	if got := agStr(e, "SELECT delivery_content FROM products WHERE id=$1", id); got != "secret-licence-and-password" {
		t.Fatal("wrong password changed stored delivery text")
	}
	key, _ := agEnableTOTP(e, owner)
	confirmedPage := e.body("GET", "/delivery-cleanup", session, nil, 200)
	mustContain(t, confirmedPage, `name="code" required`)
	wrongCode := e.do("POST", "/delivery-cleanup", session, url.Values{"id": {id}, "password": {testPassword}, "code": {e.wrongTOTP(owner)}})
	e.check(wrongCode, 401)
	if got := agStr(e, "SELECT delivery_content FROM products WHERE id=$1", id); got != "secret-licence-and-password" {
		t.Fatal("wrong authenticator code changed stored delivery text")
	}

	// An administrator can manage the listing elsewhere but cannot read or clear another account's template here.
	adminPage := e.body("GET", "/delivery-cleanup", admin, nil, 200)
	mustNotContain(t, adminPage, "other-private-template", "Other owner's secret title", otherID)
	adminPost := e.do("POST", "/delivery-cleanup", admin, url.Values{"id": {otherID}, "password": {testPassword}})
	e.check(adminPost, 404)

	cleared := e.do("POST", "/delivery-cleanup", session, url.Values{"id": {id}, "password": {testPassword}, "code": {agTOTPNow(key)}})
	e.check(cleared, 303)
	if !strings.Contains(cleared.Header().Get("Location"), "/delivery-cleanup") {
		t.Fatalf("redirect=%q", cleared.Header().Get("Location"))
	}
	if got := agStr(e, "SELECT delivery_content FROM products WHERE id=$1", id); got != "" {
		t.Fatalf("delivery text remains %q", got)
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Stored automatic-delivery text was cleared from archived listing %'", owner) != 1 ||
		!e.auditHas(owner, "Cleared stored automatic delivery from archived listing") {
		t.Fatal("clear did not record exactly one owner notification and audit")
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%secret-licence-and-password%'", owner) != 0 {
		t.Fatal("notification repeated secret delivery text")
	}
	if got := e.body("GET", "/account", session, nil, 200); strings.Contains(got, `href="/delivery-cleanup"`) {
		t.Fatal("account retained cleanup link after all owned templates were cleared")
	}
}

func TestDeliveryCleanupPaginationAndAwaitingPaymentGuard(t *testing.T) {
	e := newTestApp(t)
	owner, session := cleanupDemote(t, e, "cleanup_moderator", "moderator")
	const total = 51
	var ids []string
	for i := 0; i < total; i++ {
		id := cleanupListing(t, e, owner, fmt.Sprintf("Archived item %02d", i), fmt.Sprintf("secret-%02d", i))
		if _, err := e.DB.Exec("UPDATE products SET created='2026-01-01 00:00:00+00' WHERE id=$1", id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	var awaitingID string
	if err := e.DB.QueryRow("SELECT id FROM products WHERE vendor_id=$1 ORDER BY id LIMIT 1", owner).Scan(&awaitingID); err != nil {
		t.Fatal(err)
	}
	buyer, _ := e.user("cleanup_awaiting_buyer", "buyer")
	e.order(buyer, awaitingID, "XMR", stateAwaitingPayment)

	first := e.body("GET", "/delivery-cleanup", session, nil, 200)
	mustContain(t, first, "Showing 1–50 of 51", "Next →")
	for _, id := range ids {
		if strings.Contains(first, id) && strings.Contains(first, "secret-") {
			t.Fatal("delivery content leaked in page")
		}
	}
	last := e.body("GET", "/delivery-cleanup?page=2", session, nil, 200)
	mustContain(t, last, "Showing 51–51 of 51", "Page 2 of 2")
	if got := strings.Count(last, "Listing ID:"); got != 1 {
		t.Fatalf("last page shows %d listings, want 1", got)
	}
	clamped := e.body("GET", "/delivery-cleanup?page=999", session, nil, 200)
	mustContain(t, clamped, "Showing 51–51 of 51")

	guarded := e.do("POST", "/delivery-cleanup", session, url.Values{"id": {awaitingID}, "password": {testPassword}})
	e.check(guarded, 409)
	if got := agStr(e, "SELECT delivery_content FROM products WHERE id=$1", awaitingID); !strings.HasPrefix(got, "secret-") {
		t.Fatal("awaiting-payment refusal cleared delivery content")
	}
	if agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Stored automatic-delivery text was cleared from archived listing %'", owner) != 0 {
		t.Fatal("refused clear wrote notification")
	}
}

func TestDeliveryCleanupPreservesExistingDeliveredSnapshotAndHidesOtherOwners(t *testing.T) {
	e := newTestApp(t)
	owner, session := cleanupDemote(t, e, "cleanup_history_owner", "buyer")
	product := cleanupListing(t, e, owner, "Private archived item", "current-template-secret")
	other, _ := e.user("cleanup_history_other", "vendor")
	otherProduct := cleanupListing(t, e, other, "Other owner title", "other-template-secret")
	otherBuyer, _ := e.user("cleanup_history_order_buyer", "buyer")
	order := e.order(otherBuyer, otherProduct, "XMR", "delivered")
	if _, err := e.DB.Exec("INSERT INTO deliveries(order_id,content) VALUES($1,$2)", order, "historical-delivery-snapshot"); err != nil {
		t.Fatal(err)
	}
	ownerBuyer, _ := e.user("cleanup_owner_order_buyer", "buyer")
	ownerOrder := e.order(ownerBuyer, product, "XMR", "delivered")
	if _, err := e.DB.Exec("INSERT INTO deliveries(order_id,content) VALUES($1,$2)", ownerOrder, "existing-owner-delivery-snapshot"); err != nil {
		t.Fatal(err)
	}

	body := e.body("GET", "/delivery-cleanup", session, nil, 200)
	mustContain(t, body, "Private archived item", product)
	mustNotContain(t, body, "current-template-secret", "Other owner title", otherProduct, "other-template-secret")
	_, stranger := e.user("cleanup_stranger", "buyer")
	if got := e.body("GET", "/delivery-cleanup", stranger, nil, 200); strings.Contains(got, "Private archived item") || strings.Contains(got, product) {
		t.Fatal("another buyer learned this former vendor's listing identifiers")
	}

	post := e.do("POST", "/delivery-cleanup", session, url.Values{"id": {product}, "password": {testPassword}})
	e.check(post, 303)
	if got := agStr(e, "SELECT content FROM deliveries WHERE order_id=$1", ownerOrder); got != "existing-owner-delivery-snapshot" {
		t.Fatalf("existing delivery snapshot changed: %q", got)
	}
	if got := agStr(e, "SELECT content FROM deliveries WHERE order_id=$1", order); got != "historical-delivery-snapshot" {
		t.Fatalf("other historical snapshot changed: %q", got)
	}
}
