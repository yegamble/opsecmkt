package market

import (
	"html"
	"strings"
	"testing"
)

func TestVendorReadinessTracksEscrowIntakeWithoutLeakingHistoricalAddress(t *testing.T) {
	p := newPayEnv(t)
	if _, err := p.DB.Exec("UPDATE users SET payout_btc=$1 WHERE id=$2", "sentinel-private-payout-address", p.vendor.ID); err != nil {
		t.Fatal(err)
	}
	initial := html.UnescapeString(p.page("/vendor-dashboard", p.vendorSess))
	mustContain(t, initial, `id="seller-readiness"`, "Physical orders:", "XMR", "New intake:", "Escrow:")
	mustNotContain(t, initial, "sentinel-private-payout-address", "Pooled provider:")
	if _, err := p.DB.Exec("UPDATE settings SET value=$1 WHERE key=$2", intakePaused, paymentIntakeKey("XMR")); err != nil {
		t.Fatal(err)
	}
	body := html.UnescapeString(p.page("/vendor-dashboard", p.vendorSess))
	mustContain(t, body, "No current checkout: intake is paused")
	mustNotContain(t, body, "Bitcoin", "BTC", "escrow account key")
	if _, err := p.DB.Exec("UPDATE settings SET value=$1 WHERE key=$2", intakeEscrow, paymentIntakeKey("XMR")); err != nil {
		t.Fatal(err)
	}
	active := html.UnescapeString(p.page("/vendor-dashboard", p.vendorSess))
	mustContain(t, active, "No current checkout: escrow is not ready")
	mustNotContain(t, active, "sentinel-private-payout-address", "Bitcoin", "BTC")
	if _, err := p.DB.Exec("UPDATE users SET pgp='',pgp_fingerprint='' WHERE id=$1", p.vendor.ID); err != nil {
		t.Fatal(err)
	}
	withoutEncryption := html.UnescapeString(p.page("/vendor-dashboard", p.vendorSess))
	mustContain(t, withoutEncryption, "Add a usable encryption key on")
	mustNotContain(t, withoutEncryption, "can receive shipping details")
}

func TestVendorReadinessIsVendorScoped(t *testing.T) {
	p := newPayEnv(t)
	admin := html.UnescapeString(p.page("/vendor-dashboard", p.adminSess))
	mustContain(t, admin, `id="seller-readiness"`, "can receive shipping details")
	_, keylessAdmin := p.keylessUser("readiness_keyless_admin", "admin")
	withoutEncryption := html.UnescapeString(p.page("/vendor-dashboard", keylessAdmin))
	mustContain(t, withoutEncryption, `id="seller-readiness"`, "Add a usable encryption key on")
	mustNotContain(t, withoutEncryption, "can receive shipping details")
	modID, modSess := p.keylessUser("readiness_moderator", "moderator")
	p.body("GET", "/vendor-dashboard", modSess, nil, 403)
	agExec(p.testEnv, "UPDATE users SET vendor_approved=true WHERE id=$1", modID)
	approvedModerator := html.UnescapeString(p.page("/vendor-dashboard", modSess))
	mustContain(t, approvedModerator, `id="seller-readiness"`, "Add a usable encryption key on")
	mustNotContain(t, approvedModerator, "can receive shipping details")
	otherID, otherSess := p.user("readiness_other", "vendor")
	if _, err := p.DB.Exec("UPDATE users SET payout_btc=$1 WHERE id=$2", "other-private-address", otherID); err != nil {
		t.Fatal(err)
	}
	body := html.UnescapeString(p.page("/vendor-dashboard", otherSess))
	mustContain(t, body, "can receive shipping details")
	if !strings.Contains(body, `id="seller-readiness"`) || strings.Contains(body, "other-private-address") || strings.Contains(body, "sentinel-private-payout-address") {
		t.Fatal("seller readiness was missing or exposed a saved destination")
	}
}
