package market

import (
	"context"
	"net/http"
)

func init() {
	registerAction("/admin/payment-intake", actionSpec{Roles: []string{"admin"}, OwnTx: true, Run: paymentIntakeAction})
	registerLoader("order", paymentIntakeOrderLoader)
}

func paymentIntakeKey(currency string) string { return "payment_intake_" + currency }

func paymentIntakeAction(c *actionCtx) (actionResult, error) {
	cur, enabled := c.Form.Get("currency"), c.Form.Get("enabled")
	if (cur != "BTC" && cur != "XMR") || (enabled != "true" && enabled != "false") {
		return actionResult{}, fail(400, "Choose BTC or XMR and enable or pause new payments")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		// Provisioning remains an operator task. A configured but unreachable wallet
		// can have its policy changed; enabling does not bypass its network checks.
		c.A.payMu.RLock()
		configured := c.A.payments[cur] != nil || c.A.unavailable[cur] != nil
		c.A.payMu.RUnlock()
		if !configured {
			return actionResult{}, fail(409, "Configure the "+cur+" test-network wallet RPC before changing payment intake")
		}
		res, err := c.Tx.ExecContext(c.Ctx(), "UPDATE settings SET value=$2 WHERE key=$1", paymentIntakeKey(cur), enabled)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return actionResult{}, fail(409, "Payment intake settings are unavailable; restart after applying migrations")
		}
		action := "Paused new " + cur + " payment addresses; existing deposits and payouts continue"
		if enabled == "true" {
			action = "Enabled new " + cur + " payment addresses (test networks only)"
		}
		return actionResult{Redirect: "/admin?saved=1#payment-intake", Audit: action}, nil
	})
}

// The shared row lock lasts through address issuance and transaction commit.
// An administrator's pause waits for accepted requests; once it returns, later
// requests read the paused value and cannot issue an address.
func lockPaymentIntake(c *actionCtx, currency string) error {
	var enabled string
	if err := c.Tx.QueryRowContext(c.Ctx(), "SELECT value FROM settings WHERE key=$1 FOR SHARE", paymentIntakeKey(currency)).Scan(&enabled); err != nil {
		return err
	}
	if enabled != "true" {
		return fail(409, "New "+currency+" payment addresses are paused by the administrator. Existing deposits and payouts continue.")
	}
	return nil
}

func paymentIntakeOrderLoader(_ context.Context, _ *App, _ *http.Request, d *PageData) error {
	if d.Order == nil || d.Settings[paymentIntakeKey(d.Order.Currency)] != "false" {
		return nil
	}
	for i := range d.Transitions {
		if d.Transitions[i].To == stateAwaitingPayment {
			d.Transitions[i].Action = ""
			d.Transitions[i].Label = "New " + d.Order.Currency + " payment addresses are paused by the administrator. Existing deposits and payouts continue."
		}
	}
	return nil
}
