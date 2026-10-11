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
	mustContain(t, initial, `id="seller-readiness"`, "Physical orders:", "BTC", "XMR", "New intake:", "Escrow:")
	mustNotContain(t, initial, "sentinel-private-payout-address", "Pooled provider:")
	if _, err := p.DB.Exec("UPDATE settings SET value=$1 WHERE key=$2", intakePaused, paymentIntakeKey("XMR")); err != nil {
		t.Fatal(err)
	}
	p.A.escrowConfigured = true
	if _, err := p.DB.Exec("UPDATE settings SET value=$1 WHERE key=$2", intakeEscrow, paymentIntakeKey("BTC")); err != nil {
		t.Fatal(err)
	}
	body := html.UnescapeString(p.page("/vendor-dashboard", p.vendorSess))
	mustContain(t, body, "Bitcoin escrow account key:</strong> Needed to accept Bitcoin escrow checkouts", "No current checkout: intake is paused")
	if _, err := p.DB.Exec(`INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,active_at,layout)
 VALUES($1,'opaque-sealed-value',$2,'7450f935','/48h/1h/0h/2h',now(),'per_order')`, p.vendor.ID, strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	p.A.escrow.Store(&escrowBTC{})
	active := html.UnescapeString(p.page("/vendor-dashboard", p.vendorSess))
	mustContain(t, active, "Bitcoin escrow account key:</strong> Active", "per-order signer is not ready")
	mustNotContain(t, active, "opaque-sealed-value", "sentinel-private-payout-address")
	if _, err := p.DB.Exec("UPDATE users SET pgp='',pgp_fingerprint='' WHERE id=$1", p.vendor.ID); err != nil {
		t.Fatal(err)
	}
	withoutEncryption := html.UnescapeString(p.page("/vendor-dashboard", p.vendorSess))
	mustContain(t, withoutEncryption, "Add a usable encryption key on")
	mustNotContain(t, withoutEncryption, "can receive shipping details")
}

func TestVendorReadinessIsVendorScoped(t *testing.T) {
	p := newPayEnv(t)
	if admin := html.UnescapeString(p.page("/vendor-dashboard", p.adminSess)); strings.Contains(admin, `id="seller-readiness"`) {
		t.Fatal("administrator dashboard rendered another user's seller readiness")
	}
	otherID, otherSess := p.user("readiness_other", "vendor")
	if _, err := p.DB.Exec("UPDATE users SET payout_btc=$1 WHERE id=$2", "other-private-address", otherID); err != nil {
		t.Fatal(err)
	}
	body := html.UnescapeString(p.page("/vendor-dashboard", otherSess))
	if !strings.Contains(body, `id="seller-readiness"`) || strings.Contains(body, "other-private-address") || strings.Contains(body, "sentinel-private-payout-address") {
		t.Fatal("seller readiness was missing or exposed a saved destination")
	}
}
