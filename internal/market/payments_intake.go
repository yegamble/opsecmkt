package market

func init() {
	registerAction("/admin/payment-intake", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: paymentIntakeAction})
}
func paymentIntakeKey(currency string) string { return "payment_intake_" + currency }

const (
	intakeEscrow = "escrow"
	intakePaused = "false"
)

func paymentIntakeAction(c *actionCtx) (actionResult, error) {
	cur, value := c.Form.Get("currency"), c.Form.Get("enabled")
	if cur != "XMR" || (value != intakeEscrow && value != intakePaused) {
		return actionResult{}, fail(400, "Choose XMR and escrow or paused for new payments")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
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
		out := actionResult{Redirect: "/admin", Notice: "intake-paused", Audit: "Paused new " + cur + " escrow checkouts"}
		if value == intakeEscrow {
			out.Notice = "intake-escrow"
			out.Audit = "Enabled new " + cur + " escrow checkouts"
		}
		return out, nil
	})
}

// lockPaymentIntake refuses (409) a new escrow checkout in currency while the administrator has paused its intake, and
// holds the setting (FOR SHARE) until the checkout's transaction ends.
func lockPaymentIntake(c *actionCtx, currency string) error {
	if currency != "XMR" {
		return fail(400, "This market accepts Monero (XMR) only")
	}
	var value string
	if err := c.Tx.QueryRowContext(c.Ctx(), "SELECT value FROM settings WHERE key=$1 FOR SHARE", paymentIntakeKey(currency)).Scan(&value); err != nil {
		return err
	}
	if value == intakeEscrow {
		return nil
	}
	return fail(409, "New "+currency+" escrow checkouts are paused by the administrator.")
}
