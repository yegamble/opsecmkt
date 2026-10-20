package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestVendorVacationAndDeactivationVisibility(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("availability_vendor", "vendor")
	buyerID, buyer := e.user("availability_buyer", "buyer")
	_, publicKey := testPGPKey(t, "availability-vendor")
	agExec(e, "UPDATE users SET pgp=$1 WHERE id=$2", publicKey, vendorID)
	product := e.product(vendorID, "digital")
	e.order(buyerID, product, "XMR", stateAwaitingPayment)
	mode := func(value string, off bool) {
		t.Helper()
		f := url.Values{"mode": {value}}
		if off {
			f.Set("messages_off_while_away", "on")
		}
		e.check(e.do("POST", "/account/vendor-availability", vendor, f), 303)
	}
	mode("vacation", true)
	page := e.body("GET", "/vendor?id="+vendorID, "", nil, 200)
	mustContain(t, page, "On vacation", "Buyer messages are paused")
	mustNotContain(t, page, "Message vendor")
	mustContain(t, e.body("GET", "/product?id="+product, buyer, nil, 200), "On vacation")
	mustNotContain(t, e.body("GET", "/product?id="+product, buyer, nil, 200), "Review order draft")
	mustContain(t, e.body("GET", "/", "", nil, 200), product, "On vacation")

	mode("deactivated", true)
	e.check(e.do("GET", "/vendor?id="+vendorID, "", nil), 404)
	e.check(e.do("GET", "/product?id="+product, "", nil), 404)
	mustNotContain(t, e.body("GET", "/", "", nil, 200), product)
	// Existing customer work remains readable, but cannot become a new purchase.
	page = e.body("GET", "/product?id="+product, buyer, nil, 200)
	mustContain(t, page, "deactivated their storefront")
	mustNotContain(t, page, "Review order draft", "Message vendor")
	mode("active", false)
	mustContain(t, e.body("GET", "/vendor?id="+vendorID, "", nil, 200), "Message vendor")
	mustContain(t, e.body("GET", "/", "", nil, 200), product)
}

func TestVendorLastSeenRequiresOptInAndIsErasedOnOptOut(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("activity_vendor", "vendor")
	_, publicKey := testPGPKey(t, "activity-vendor")
	agExec(e, "UPDATE users SET pgp=$1,vendor_last_seen='2001-02-03 04:00:00+00' WHERE id=$2", publicKey, vendorID)
	page := e.body("GET", "/vendor?id="+vendorID, "", nil, 200)
	mustNotContain(t, page, "Last seen:", "2001-02-03")
	// The model also withholds the timestamp; hiding only HTML would leak it to
	// any future API or template using this shared loader.
	d := PageData{Page: "vendor"}
	r := httptest.NewRequest("GET", "/vendor?id="+vendorID, nil)
	if err := vendorAvailabilityLoader(r.Context(), e.A, r, &d); err != nil {
		t.Fatal(err)
	}
	if d.VendorAvailability.LastSeen != "" {
		t.Fatal("hidden last seen reached the public page model")
	}
	e.check(e.do("POST", "/account/vendor-availability", vendor, url.Values{"mode": {"active"}, "show_last_seen": {"on"}}), 303)
	e.check(e.do("GET", "/account", vendor, nil), 200)
	page = e.body("GET", "/vendor?id="+vendorID, "", nil, 200)
	mustContain(t, page, "Last seen:", "(approximate)")
	mustNotContain(t, page, "2001-02-03")
	e.check(e.do("POST", "/account/vendor-availability", vendor, url.Values{"mode": {"active"}}), 303)
	var cleared bool
	if err := e.DB.QueryRow("SELECT vendor_last_seen IS NULL FROM users WHERE id=$1", vendorID).Scan(&cleared); err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Fatal("opting out retained the saved activity timestamp")
	}
	mustNotContain(t, e.body("GET", "/vendor?id="+vendorID, "", nil, 200), "Last seen:")
}

func TestVendorPreferencesRecheckRoleAndPublicKey(t *testing.T) {
	e := newTestApp(t)
	_, buyer := e.user("preferences_buyer", "buyer")
	e.check(e.do("POST", "/account/vendor-availability", buyer, url.Values{"mode": {"deactivated"}}), 403)
	vendorID, vendor := e.keylessUser("preferences_vendor", "vendor")
	e.check(e.do("POST", "/account/vendor-availability", vendor, url.Values{"mode": {"vacation"}}), 409)
	e.check(e.do("POST", "/account/vendor-availability", vendor, url.Values{"mode": {"deactivated"}}), 303)
	if state := agStr(e, "SELECT vendor_deactivated::text FROM users WHERE id=$1", vendorID); state != "true" {
		t.Fatalf("keyless legacy vendor could not hide their store: %s", state)
	}
	_, publicKey := testPGPKey(t, "preferences-vendor")
	agExec(e, "UPDATE users SET pgp=$1 WHERE id=$2", publicKey, vendorID)
	e.check(e.do("POST", "/account/vendor-availability", vendor, url.Values{"mode": {"unknown"}}), 400)
	e.check(e.do("POST", "/account/vendor-availability", vendor, url.Values{"mode": {"active"}}), 303)
	if !strings.Contains(e.body("GET", "/account", vendor, nil, 200), "name=\"mode\"") {
		t.Fatal("vendor preference form is absent")
	}
}
