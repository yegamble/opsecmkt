package market

import (
	"net/url"
	"testing"
)

func TestApprovedModeratorPrivatePhotoCapability(t *testing.T) {
	e, scanner, gateway := staffUploadEnv(t)
	id, session := e.user("private_photo_moderator", "moderator")
	agExec(e, "UPDATE users SET vendor_approved=true WHERE id=$1", id)
	listing := e.product(id, "physical")
	form := func() url.Values {
		return url.Values{"kind": {"photo"}, "id": {listing}, formNonceField: {randomToken()[:32]}}
	}
	page := staffAuthorize(t, e, session, form())
	ticket := staffTicket(t, page.Body.String())
	e.check(uploadThroughGateway(gateway, staffUploadPath, "", staffAvatarParts(ticket, smallJPEG(t))...), 200)
	if e.imageCount() != 1 || scansOf(scanner) != 1 {
		t.Fatal("approved moderator's private upload was not scanned and stored")
	}
	// Revocation between confirmation and gateway admission invalidates the
	// capability, before reading or scanning a second original.
	ticket = staffTicket(t, staffAuthorize(t, e, session, form()).Body.String())
	agExec(e, "UPDATE users SET vendor_approved=false WHERE id=$1", id)
	e.check(uploadThroughGateway(gateway, staffUploadPath, "", staffAvatarParts(ticket, smallJPEG(t))...), 409)
	if e.imageCount() != 1 || scansOf(scanner) != 1 {
		t.Fatal("revoked selling approval still admitted a private photo upload")
	}
	f := form()
	f.Set("password", testPassword)
	e.check(brandingAdminRequest(e, true, "POST", "/admin/uploads/authorize", session, f), 403)
}

func TestRestrictedPrivatePartyCanUploadOnlyDisputeEvidence(t *testing.T) {
	e, scanner, gateway := staffUploadEnv(t)
	p := newPayEnv(t, e)
	admin, err := e.A.sessionUser(t.Context(), p.adminSess)
	if err != nil {
		t.Fatal(err)
	}
	order := e.order(admin.ID, p.productID, "XMR", stateDisputed)
	agExec(e, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Private restricted-party evidence fixture')", randomToken(), order)
	agExec(e, "UPDATE users SET retention_restricted_at=now() WHERE id=$1", admin.ID)
	e.check(brandingAdminRequest(e, true, "GET", orderPage(order), p.adminSess, nil), 200)
	page := staffAuthorize(t, e, p.adminSess, url.Values{"kind": {"evidence"}, "id": {order}, formNonceField: {randomToken()[:32]}})
	ticket := staffTicket(t, page.Body.String())
	e.check(uploadThroughGateway(gateway, staffUploadPath, "", staffAvatarParts(ticket, smallJPEG(t))...), 200)
	if e.imageCount() != 1 || scansOf(scanner) != 1 {
		t.Fatal("restricted party's existing-dispute evidence was not scanned and stored")
	}
	// The narrow exception must not restore avatar or listing uploads.
	for _, kind := range []string{"avatar", "photo"} {
		form := url.Values{"kind": {kind}, "id": {p.productID}, "password": {testPassword}, formNonceField: {randomToken()[:32]}}
		e.check(brandingAdminRequest(e, true, "POST", "/admin/uploads/authorize", p.adminSess, form), 403)
	}
}
