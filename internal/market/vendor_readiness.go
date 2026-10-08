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
				{Currency: "BTC", Intake: "Pooled and escrow", Provider: "Available", Escrow: "Available", PooledIntake: true, PayoutNeeded: true, PayoutSaved: true, EscrowKeyRequired: true, EscrowKeyActive: true, Checkout: "Available"},
				{Currency: "XMR", Intake: "Pooled and escrow", Provider: "Available", Escrow: "Available", PooledIntake: true, PayoutNeeded: true, PayoutSaved: true, Checkout: "Available"},
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
	var payoutBTC, payoutXMR string
	if err := a.db.QueryRowContext(ctx, "SELECT payout_btc,payout_xmr FROM users WHERE id=$1", d.User.ID).Scan(&payoutBTC, &payoutXMR); err != nil {
		return err
	}
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
		vendorCurrencyReadiness(a, d, "BTC", payoutBTC, a.escrowConfigured, btcEscrowAvailable, a.escrowConfigured, btcKeyActive, btcPerOrder, a.escrowSignerState()),
		vendorCurrencyReadiness(a, d, "XMR", payoutXMR, a.escrowXMR != nil, a.xmrEscrowReady(), false, false, false, ""),
	}
	d.VendorReadiness = v
	return nil
}

func vendorCurrencyReadiness(a *App, d *PageData, currency, payout string, escrowOn, escrowReady, hasEscrowKey, escrowKeyActive, perOrder bool, signerState string) VendorCurrencyReadiness {
	mode := d.Settings[paymentIntakeKey(currency)]
	if mode != intakeOn && mode != intakeEscrow {
		mode = intakePaused
	}
	v := VendorCurrencyReadiness{Currency: currency, PayoutSaved: payout != "", EscrowKeyActive: escrowKeyActive}
	providerAvailable := a.provider(currency) != nil
	providerConfigured := a.pooledConfigured(currency)
	switch mode {
	case intakeOn:
		v.Intake, v.PooledIntake, v.PayoutNeeded = "Pooled and escrow", true, true
	case intakeEscrow:
		v.Intake = "Escrow only"
	default:
		v.Intake = "Paused"
	}
	if providerAvailable {
		v.Provider = "Configured; availability is checked when paying"
	} else if providerConfigured {
		v.Provider = "Configured; last check unavailable"
	} else {
		v.Provider = "Not configured"
	}
	if escrowOn && currency == "BTC" {
		var blockers []string
		if a.escrow.Load() == nil {
			blockers = append(blockers, "Bitcoin startup checks have not passed")
		}
		if hasEscrowKey && !escrowKeyActive {
			blockers = append(blockers, "your Bitcoin escrow account key is not active")
		}
		if perOrder && signerState != signerReady {
			blockers = append(blockers, "per-order signer is not ready")
		}
		if len(blockers) > 0 {
			v.Escrow = strings.Join(blockers, "; ")
		} else if escrowReady {
			v.Escrow = "Ready at last check"
		} else {
			v.Escrow = "Configured; not ready at last check"
		}
	} else if escrowOn {
		if escrowReady {
			v.Escrow = "Ready at last check"
		} else {
			v.Escrow = "Configured; not ready at last check"
		}
	} else {
		v.Escrow = "Off"
	}
	v.EscrowKeyRequired = hasEscrowKey && mode != intakePaused
	if v.PayoutNeeded {
		if v.PayoutSaved {
			v.Payout = "Saved; check it on your account"
		} else {
			v.Payout = "Needed for pooled payouts"
		}
	} else {
		v.Payout = "Not needed for escrow-only or paused intake"
	}
	if mode == intakePaused {
		v.Checkout = "No current checkout: intake is paused"
	} else if mode == intakeEscrow {
		if escrowReady {
			v.Checkout = "Escrow checkout ready at last check (eligible listings only)"
		} else if currency == "BTC" {
			v.Checkout = "No current checkout: Bitcoin escrow or this account's key/signer is not ready"
		} else {
			v.Checkout = "No current checkout: Monero escrow is not ready"
		}
	} else if providerAvailable {
		v.Checkout = "Pooled checkout configured; wallet availability is checked when paying"
	} else if escrowReady {
		v.Checkout = "Escrow checkout ready at last check (eligible listings only)"
	} else {
		v.Checkout = "No current checkout: no usable provider or escrow setup is ready"
	}
	return v
}
