package market

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestReviewListingCreationRejectsDemotedRequestUser(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("listing_stale_vendor", "vendor")
	_, admin := e.user("listing_stale_admin", "admin")
	stale := e.loadUser(vendor)
	e.check(e.do("POST", "/admin", admin, url.Values{"action": {"role"}, "user_id": {vendor}, "role": {"buyer"}}), 303)
	tx, err := e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = listingAction(&actionCtx{A: e.A, Tx: tx, R: httptest.NewRequest("POST", "/listings", nil), User: stale, Form: inventoryForm("", nil)})
	var he *httpError
	if !errors.As(err, &he) || he.Code != 403 {
		t.Fatalf("stale vendor request after committed demotion: %v", err)
	}
	if agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1", vendor) != 0 {
		t.Fatal("demoted vendor published a new listing")
	}
}

func TestReviewDemotionWaitsForListingCreationThenArchivesIt(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("listing_inflight_vendor", "vendor")
	_, admin := e.user("listing_inflight_admin", "admin")
	tx, err := e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = listingAction(&actionCtx{A: e.A, Tx: tx, R: httptest.NewRequest("POST", "/listings", nil), User: e.loadUser(vendor), Form: inventoryForm("", nil)}); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		done <- e.do("POST", "/admin", admin, url.Values{"action": {"role"}, "user_id": {vendor}, "role": {"buyer"}}).Code
	}()
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		if agInt(e, "SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'UPDATE users SET role=$1 WHERE id=$2%' AND wait_event_type='Lock'") > 0 {
			blocked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-done; !blocked || code != 303 {
		t.Fatalf("demotion did not serialize with creation: blocked=%v status=%d", blocked, code)
	}
	if agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1 AND archived", vendor) != 1 ||
		agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1 AND NOT archived", vendor) != 0 {
		t.Fatal("demotion left the concurrently created listing active")
	}
}
