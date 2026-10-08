package market

import (
	"html"
	"strings"
	"testing"
)

func TestVendorReadinessTracksIntakeAndNeverRendersWalletDetails(t *testing.T) {
	p := newPayEnv(t)
	if _, err := p.DB.Exec("UPDATE users SET payout_btc=$1 WHERE id=$2", "sentinel-private-payout-address", p.vendor.ID); err != nil {
		t.Fatal(err)
	}
	p.A.payMu.Lock()
	p.A.unavailable["XMR"] = &unavailableProvider{p: newFakeProvider("XMR", 10), err: "sentinel-private-node-error"}
	p.A.payMu.Unlock()

	pooled := html.UnescapeString(p.page("/vendor-dashboard", p.vendorSess))
	for _, want := range []string{
		"id=\"seller-readiness\"", "Physical orders:", "can receive shipping details",
		"BTC", "Pooled and escrow", "Pooled provider:</strong> Configured; availability is checked when paying", "Payout address:</strong> Saved; check it on your account",
		"XMR", "Configured; last check unavailable", "No current checkout: no usable provider or escrow setup is ready",
		"Addresses, keys and wallet errors are not shown here.",
	} {
		if !strings.Contains(pooled, want) {
			t.Errorf("pooled readiness lacks %q", want)
		}
	}
	for _, secret := range []string{"sentinel-private-payout-address", "sentinel-private-node-error"} {
		if strings.Contains(pooled, secret) {
			t.Errorf("seller readiness exposed %q", secret)
		}
	}
	if strings.Contains(pooled, "Bitcoin escrow account key:") {
		t.Fatal("Bitcoin escrow key requirement appeared while Bitcoin escrow is off")
	}

	if _, err := p.DB.Exec("UPDATE settings SET value=$1 WHERE key=$2", intakeEscrow, paymentIntakeKey("BTC")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB.Exec("UPDATE settings SET value=$1 WHERE key=$2", intakePaused, paymentIntakeKey("XMR")); err != nil {
		t.Fatal(err)
	}
	p.A.escrowConfigured = true
	escrowOnly := html.UnescapeString(p.page("/vendor-dashboard", p.vendorSess))
	for _, want := range []string{
		"BTC", "Escrow only", "No current checkout: Bitcoin escrow or this account's key/signer is not ready",
		"Payout address:</strong> Not needed for escrow-only or paused intake",
		"Bitcoin escrow account key:</strong> Needed to accept Bitcoin escrow checkouts",
		"XMR", "Paused", "No current checkout: intake is paused",
	} {
		if !strings.Contains(escrowOnly, want) {
			t.Errorf("escrow/paused readiness lacks %q", want)
		}
	}
	if strings.Contains(escrowOnly, "Payout address:</strong> Saved") {
		t.Fatal("escrow-only intake treated a saved profile payout address as required")
	}

	if _, err := p.DB.Exec(`INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,active_at,layout)
		VALUES($1,'opaque-sealed-value',$2,'7450f935','/48h/1h/0h/2h',now(),'per_order')`, p.vendor.ID, strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	p.A.escrow.Store(&escrowBTC{})
	active := html.UnescapeString(p.page("/vendor-dashboard", p.vendorSess))
	if !strings.Contains(active, "Bitcoin escrow account key:</strong> Active") || !strings.Contains(active, "per-order signer is not ready") || strings.Contains(active, "opaque-sealed-value") {
		t.Fatal("active per-order account key was not recognized safely")
	}
	if _, err := p.DB.Exec("UPDATE users SET pgp='',pgp_fingerprint='' WHERE id=$1", p.vendor.ID); err != nil {
		t.Fatal(err)
	}
	withoutEncryption := html.UnescapeString(p.page("/vendor-dashboard", p.vendorSess))
	if !strings.Contains(withoutEncryption, "Add a usable encryption key on") || strings.Contains(withoutEncryption, "can receive shipping details") {
		t.Fatal("physical readiness did not reflect the vendor's unusable encryption key")
	}
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
