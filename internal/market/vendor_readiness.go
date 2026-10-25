package market

import (
	"context"
	"net/http"
	"strings"
)

func init() {
	registerLoader("vendor-dashboard", loadVendorReadiness)
	registerPreview("vendor-dashboard", func(d *PageData) {
		d.VendorReadiness = &VendorReadinessView{
			EncryptionUsable: true,
			Currencies: []VendorCurrencyReadiness{
				{Currency: "BTC", Intake: "Escrow", Escrow: "Available", EscrowKeyRequired: true, EscrowKeyActive: true, Checkout: "Available"},
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
	var btcKeyLayout string
	if a.escrowConfigured {
		err := a.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT layout FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL),'')`, d.User.ID).Scan(&btcKeyLayout)
		if err != nil {
			return err
		}
	}
	btcKeyActive := btcKeyLayout != ""
	btcPerOrder := btcKeyActive && btcKeyLayout != "core"
	btcEscrowAvailable := a.escrow.Load() != nil && btcKeyActive && (!btcPerOrder || (a.perOrderAvailable() && a.escrowSignerState() == signerReady))
	v.Currencies = []VendorCurrencyReadiness{
		vendorCurrencyReadiness(a, d, "BTC", a.escrowConfigured, btcEscrowAvailable, a.escrowConfigured, btcKeyActive, btcPerOrder, a.escrowSignerState()),
		vendorCurrencyReadiness(a, d, "XMR", a.escrowXMR != nil, a.xmrEscrowReady(), false, false, false, ""),
	}
	if ok, err := vendorBondSatisfied(ctx, a.db, d.User.ID, false); err != nil {
		return err
	} else if !ok {
		for i := range v.Currencies {
			v.Currencies[i].Checkout = "No current checkout: the required vendor bond is not confirmed"
		}
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
	if currency == "BTC" && escrowOn {
		var blockers []string
		if a.escrow.Load() == nil {
			blockers = append(blockers, "Bitcoin startup checks have not passed")
		}
		if !escrowKeyActive {
			blockers = append(blockers, "your Bitcoin escrow account key is not active")
		}
		if perOrder && signerState != signerReady {
			blockers = append(blockers, "per-order signer is not ready")
		}
		if len(blockers) > 0 {
			v.Escrow = strings.Join(blockers, "; ")
		}
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
