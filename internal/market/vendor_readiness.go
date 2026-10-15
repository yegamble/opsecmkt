package market

import (
	"context"
	"net/http"
)

func init() {
	registerLoader("vendor-dashboard", loadVendorReadiness)
	registerPreview("vendor-dashboard", func(d *PageData) {
		d.VendorReadiness = &VendorReadinessView{
			EncryptionUsable: true,
			Currencies: []VendorCurrencyReadiness{
				{Currency: "XMR", Intake: "Escrow", Escrow: "Available", Checkout: "Available"},
			},
		}
	})
}

func loadVendorReadiness(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "vendor" {
		return nil
	}
	v := &VendorReadinessView{}
	ok, err := userCanReceiveEncrypted(ctx, a.db, d.User.ID)
	if err != nil {
		return err
	}
	v.EncryptionUsable = ok

	v.Currencies = []VendorCurrencyReadiness{
		vendorCurrencyReadiness(a, d, "XMR", a.escrowXMR != nil, a.xmrEscrowReady(), false, false, false, ""),
	}
	d.VendorReadiness = v
	return nil
}

func vendorCurrencyReadiness(a *App, d *PageData, currency string, escrowOn, escrowReady, hasEscrowKey, escrowKeyActive, perOrder bool, signerState string) VendorCurrencyReadiness {
	mode := d.Settings[paymentIntakeKey(currency)]
	v := VendorCurrencyReadiness{Currency: currency, EscrowKeyActive: escrowKeyActive}
	if mode == intakeEscrow {
		v.Intake = "Escrow"
	} else {
		v.Intake = "Paused"
	}
	if escrowOn {
		if escrowReady {
			v.Escrow = "Ready at last check"
		} else {
			v.Escrow = "Configured; not ready at last check"
		}
	} else {
		v.Escrow = "Off"
	}

	v.EscrowKeyRequired = hasEscrowKey && mode == intakeEscrow
	if mode != intakeEscrow {
		v.Checkout = "No current checkout: intake is paused"
	} else if escrowReady {
		v.Checkout = "Escrow checkout ready at last check (eligible listings only)"
	} else {
		v.Checkout = "No current checkout: escrow is not ready"
	}
	return v
}
