package market

import (
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestVendorCapabilityRoleMatrix(t *testing.T) {
	e := newTestApp(t)
	admin := e.setup()
	setVendorPolicy(e, "0", "manual", "")
	modID, mod := e.user("cap_mod", "moderator")
	_, buyer := e.user("cap_buyer", "buyer")
	_, vendor := e.user("cap_vendor", "vendor")
	for _, session := range []string{mod, buyer} {
		e.body("GET", "/vendor-dashboard", session, nil, 403)
	}
	for _, session := range []string{admin, vendor} {
		e.body("GET", "/vendor-dashboard", session, nil, 200)
	}
	e.check(e.do("POST", "/account/vendor-application", mod, url.Values{"action": {"apply"}}), 303)
	app := latestVendorApp(e, modID)
	adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {app.ID}, "action": {"approve"}}, 303)
	if userRole(e, modID) != "moderator" {
		t.Fatal("vendor approval changed the moderator's staff role")
	}
	e.body("GET", "/moderator", mod, nil, 200)
	page := e.body("GET", "/vendor-dashboard", mod, nil, 200)
	mustContain(t, page, "Vendor desk", "Moderation")
	form := url.Values{"title": {"Moderator digital listing"}, "description": {"Test content"}, "category": {"Digital"}, "region": {"Worldwide"}, "kind": {"digital"}, "price_xmr": {"1"}, "stock": {"3"}}
	e.check(e.do("POST", "/listings", mod, form), 303)
	product := agStr(e, "SELECT id FROM products WHERE vendor_id=$1", modID)
	e.body("GET", "/vendor?id="+modID, buyer, nil, 200)
	e.body("GET", "/listing-edit?id="+product, mod, nil, 200)
	e.body("GET", "/admin", mod, nil, 403)
	adminVendorPost(e, admin, "/admin/vendor-revoke", url.Values{"handle": {"cap_mod"}}, 303)
	if userRole(e, modID) != "moderator" || agInt(e, "SELECT count(*) FROM products WHERE vendor_id=$1 AND archived", modID) != 1 {
		t.Fatal("revocation failed to preserve moderation or archive listings")
	}
	e.body("GET", "/vendor-dashboard", mod, nil, 403)
	e.body("GET", "/moderator", mod, nil, 200)
	e.check(e.do("POST", "/listings", mod, form), 403)
}

func TestAdministratorIsAutomaticallyVerifiedVendor(t *testing.T) {
	e := newTestApp(t)
	admin := e.setup()
	id := agStr(e, "SELECT id FROM users WHERE handle='admin_user'")
	product := e.product(id, "digital")
	for _, path := range []string{"/", "/product?id=" + product, "/vendor?id=" + id} {
		mustContain(t, e.body("GET", path, admin, nil, 200), "Verified vendor")
	}
	var active bool
	if err := e.DB.QueryRow("SELECT "+activeVendorSQL("u")+" FROM users u WHERE u.id=$1", id).Scan(&active); err != nil || !active {
		t.Fatalf("administrator is not an active vendor: %v %v", active, err)
	}
}

func TestVendorCapabilityDoesNotSurviveAdministratorDemotion(t *testing.T) {
	e := newTestApp(t)
	e.user("cap_root", "admin")
	adminID, _ := e.user("cap_admin_only", "admin")
	product := e.product(adminID, "digital")
	if _, err := SetRole(t.Context(), e.DB, "cap_admin_only", "moderator"); err != nil {
		t.Fatal(err)
	}
	if agStr(e, "SELECT vendor_approved::text FROM users WHERE id=$1", adminID) != "false" || agInt(e, "SELECT count(*) FROM products WHERE id=$1 AND archived", product) != 1 {
		t.Fatal("administrator-derived selling access persisted after demotion")
	}
	// Genuine approval is retained when a vendor becomes a moderator, and
	// through a later administrator promotion and demotion.
	vendorID, _ := e.user("cap_approved", "vendor")
	product = e.product(vendorID, "digital")
	for _, role := range []string{"moderator", "admin", "moderator"} {
		if _, err := SetRole(t.Context(), e.DB, "cap_approved", role); err != nil {
			t.Fatal(err)
		}
	}
	if agStr(e, "SELECT vendor_approved::text FROM users WHERE id=$1", vendorID) != "true" || agInt(e, "SELECT count(*) FROM products WHERE id=$1 AND NOT archived", product) != 1 {
		t.Fatal("promotion into staff lost existing approval or hid its listing")
	}
}

func TestApprovedModeratorLifecycleAndStaleForms(t *testing.T) {
	e := newTestApp(t)
	id, session := e.user("cap_lifecycle", "moderator")
	agExec(e, "UPDATE users SET vendor_approved=true WHERE id=$1", id)
	product := e.product(id, "digital")
	buyerID, _ := e.user("cap_history_buyer", "buyer")
	order := e.order(buyerID, product, "XMR", stateDelivered)
	for _, field := range []string{"suspended_at", "closed_at", "retention_restricted_at"} {
		agExec(e, "UPDATE users SET "+field+"=now() WHERE id=$1", id)
		var active bool
		if err := e.DB.QueryRow("SELECT "+activeVendorSQL("u")+" FROM users u WHERE u.id=$1", id).Scan(&active); err != nil || active {
			t.Fatalf("%s account active as a vendor: %v %v", field, active, err)
		}
		agExec(e, "UPDATE users SET "+field+"=NULL WHERE id=$1", id)
	}
	// A request that obtained the old session snapshot still fails the locked
	// check if approval is removed before its actual listing mutation.
	stale := &User{ID: id, Handle: "cap_lifecycle", Role: "moderator", VendorApproved: true}
	agExec(e, "UPDATE users SET vendor_approved=false WHERE id=$1", id)
	for _, path := range []string{"/listings", "/listings/update"} {
		f := inventoryForm(product, map[string]string{"kind": "digital"})
		r := httptest.NewRequest("POST", path, nil)
		f.Set(formNonceField, formNonceFor(path))
		r.PostForm = f
		w := httptest.NewRecorder()
		e.A.post(w, r, stale, session)
		if w.Code != 403 {
			t.Fatalf("stale %s form accepted: %d", path, w.Code)
		}
	}
	e.body("GET", "/order?id="+order, session, nil, 200)
	if agStr(e, "SELECT title FROM products WHERE id=$1", product) != "Test listing" {
		t.Fatal("stale edit changed listing")
	}
}

func TestApprovedModeratorPhotoUploadRechecksSellingApproval(t *testing.T) {
	e, scanner := uploadEnv(t)
	id, session := e.user("cap_photo_mod", "moderator")
	agExec(e, "UPDATE users SET vendor_approved=true WHERE id=$1", id)
	product := e.product(id, "physical")
	e.check(e.upload("/listings/photo", session, e.photoParts(session, product, smallJPEG(t))...), 303)
	photo := agStr(e, "SELECT id FROM images WHERE owner_user_id=$1", id)
	e.body("GET", publicImageURL(photo, false), randomToken(), nil, 200)
	entered, release := stallScans(t, scanner)
	done := e.uploadAsync("/listings/photo", session, e.photoParts(session, product, smallJPEG(t))...)
	waitEntered(t, entered)
	agExec(e, "UPDATE users SET vendor_approved=false WHERE id=$1", id)
	release()
	w := <-done
	if w.Code != 403 || e.imageCount() != 1 || scansOf(scanner) != 2 {
		t.Fatalf("revoked photo upload: status=%d images=%d scans=%d", w.Code, e.imageCount(), scansOf(scanner))
	}
	e.body("GET", publicImageURL(photo, false), randomToken(), nil, 404)
}

func TestSellingCapabilityDoesNotExposeHiddenStaffContacts(t *testing.T) {
	e := newTestApp(t)
	buyerID, buyerSession := e.user("cap_contact_buyer", "buyer")
	sender := &User{ID: buyerID, Handle: "cap_contact_buyer", Role: "buyer"}
	for _, role := range []string{"admin", "moderator"} {
		id, _ := e.user("cap_hidden_"+role, role)
		agExec(e, "UPDATE users SET staff_hidden=true,vendor_approved=$2 WHERE id=$1", id, role == "moderator")
		_, key := testPGPKey(t, "cap_hidden_"+role)
		e.setKey(id, key)
		handle := "cap_hidden_" + role
		for _, name := range []string{handle, "cap_missing_" + role} {
			got, err := messageRecipient(t.Context(), e.DB, sender, name)
			if err != nil || got != "" {
				t.Fatalf("hidden staff lookup %s exposed recipient: %q %v", name, got, err)
			}
			page := e.body("GET", "/messages?to="+name, buyerSession, nil, 200)
			mustContain(t, page, messageNotPermitted)
			mustNotContain(t, page, "BEGIN PGP PUBLIC KEY BLOCK")
		}
		product := e.product(id, "digital")
		got, err := messageRecipient(t.Context(), e.DB, sender, handle)
		if err != nil || got != id {
			t.Fatalf("published staff seller is not contactable: %q %v", got, err)
		}
		agExec(e, "UPDATE products SET archived=true WHERE id=$1", product)
		if got, err = messageRecipient(t.Context(), e.DB, sender, handle); err != nil || got != "" {
			t.Fatalf("archived staff seller remained publicly contactable: %q %v", got, err)
		}
		// Existing order partners keep their legitimate contact route.
		e.order(buyerID, product, "XMR", stateDraft)
		if got, err = messageRecipient(t.Context(), e.DB, sender, handle); err != nil || got != id {
			t.Fatalf("order partner lost staff seller contact: %q %v", got, err)
		}
	}
}
