package market

import (
	"context"
	"net/http"
)

func init() {
	registerAction("/admin/payment-intake", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: paymentIntakeAction})
	registerLoader("order", paymentIntakeOrderLoader)
}

func paymentIntakeKey(currency string) string { return "payment_intake_" + currency }

// The values of payment_intake_<COIN> (migration 052; 'escrow' since A-444, P4-R8, with no migration). On admits pooled
// payment requests and escrow checkouts, escrow only admits escrow checkouts alone (the cutover lever), paused admits
// neither. Any other value reads as paused: binaries older than A-444 refuse every value but 'true'.
const (
	intakeOn     = "true"
	intakeEscrow = "escrow"
	intakePaused = "false"
)

// intakeKind is what a caller of lockPaymentIntake would issue: a pooled payment address or an escrow.
type intakeKind int

const (
	kindPooled intakeKind = iota
	kindEscrow
)

// escrowOnlyRefusal refuses a pooled payment request while currency's intake is escrow only.
func escrowOnlyRefusal(currency string) string {
	return "New " + currency + " payments on this market use escrow only, so the market's pooled wallet issues no new payment address. Pay with escrow checkout on the order page if this listing offers it. Nothing was changed."
}

// paymentIntakeAction sets one currency's intake: on, escrow only or paused. On needs the currency's pooled wallet
// configured; escrow only and paused do not, so a market can cut over to escrow, or stop taking payments, with no
// pooled wallet (P4-R8).
func paymentIntakeAction(c *actionCtx) (actionResult, error) {
	cur, value := c.Form.Get("currency"), c.Form.Get("enabled")
	if (cur != "BTC" && cur != "XMR") || (value != intakeOn && value != intakeEscrow && value != intakePaused) {
		return actionResult{}, fail(400, "Choose BTC or XMR and on, escrow only or paused for new payments")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		// Provisioning remains an operator task. A configured but unreachable wallet
		// can have its policy changed; enabling does not bypass its network checks.
		if value == intakeOn && !c.A.pooledConfigured(cur) {
			return actionResult{}, fail(409, "Configure the "+cur+" wallet RPC before turning on new pooled payments. Escrow only and paused need no pooled wallet.")
		}
		// A checkout holds the setting FOR SHARE while it waits on the wallet (lockPaymentIntake).
		if err := waitForWalletLocks(c.Ctx(), c.Tx); err != nil {
			return actionResult{}, err
		}
		res, err := c.Tx.ExecContext(c.Ctx(), "UPDATE settings SET value=$2 WHERE key=$1", paymentIntakeKey(cur), value)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return actionResult{}, fail(409, "Payment intake settings are unavailable; restart after applying migrations")
		}
		out := actionResult{Redirect: "/admin", Notice: "intake-paused", Audit: "Paused new " + cur + " payment addresses; existing deposits and payouts continue"}
		switch value {
		case intakeOn:
			out.Notice, out.Audit = "intake-enabled", "Enabled new "+cur+" payment addresses ("+c.A.netName()+")"
		case intakeEscrow:
			out.Notice, out.Audit = "intake-escrow", "Set new "+cur+" payments to escrow only: no new pooled payment addresses; escrow checkouts, existing deposits and payouts continue"
		}
		return out, nil
	})
}

// pooledConfigured reports whether currency has a pooled wallet configured in this process, available or not.
func (a *App) pooledConfigured(currency string) bool {
	a.payMu.RLock()
	defer a.payMu.RUnlock()
	return a.payments[currency] != nil || a.unavailable[currency] != nil
}

// The shared row lock lasts through address issuance and transaction commit.
// An administrator's pause waits for accepted requests; once it returns, later
// requests read the paused value and cannot issue an address. Escrow only refuses
// a pooled request (kindPooled) and admits an escrow checkout (kindEscrow).
func lockPaymentIntake(c *actionCtx, currency string, kind intakeKind) error {
	var value string
	if err := c.Tx.QueryRowContext(c.Ctx(), "SELECT value FROM settings WHERE key=$1 FOR SHARE", paymentIntakeKey(currency)).Scan(&value); err != nil {
		return err
	}
	switch {
	case value == intakeOn, value == intakeEscrow && kind == kindEscrow:
		return nil
	case value == intakeEscrow:
		return fail(409, escrowOnlyRefusal(currency))
	}
	return fail(409, "New "+currency+" payment addresses are paused by the administrator. Existing deposits and payouts continue.")
}

// paymentIntakeOrderLoader replaces the buyer's pooled payment request on a draft while its currency is paused or
// escrow only; under escrow only, a listing that cannot use escrow says why (listingEscrowBlock).
func paymentIntakeOrderLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.Order == nil || d.VendorSuspensionReason != "" {
		return nil
	}
	value := d.Settings[paymentIntakeKey(d.Order.Currency)]
	var label string
	switch value {
	case intakePaused:
		label = "New " + d.Order.Currency + " payment addresses are paused by the administrator. Existing deposits and payouts continue."
	case intakeEscrow:
		label = "New " + d.Order.Currency + " payments on this market use escrow only, so the market's pooled wallet issues no new payment address."
	default:
		return nil
	}
	for i := range d.Transitions {
		if d.Transitions[i].To != stateAwaitingPayment {
			continue
		}
		if value == intakeEscrow {
			reason, _, err := a.listingEscrowBlock(ctx, a.db, d.Order.ProductID, d.Order.Currency)
			if err != nil {
				return err
			}
			if reason != "" {
				label += " " + reason + " This draft cannot be paid until that changes; you can cancel it."
			}
		}
		d.Transitions[i].Action, d.Transitions[i].Label = "", label
	}
	return nil
}
