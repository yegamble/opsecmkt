package market

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
)

// A-288: a wallet restored from an older backup or seed hands out addresses again from where the backup left off,
// but the database has already issued the later ones to orders (docs/testnet-runbook.md, "Restore the wallets").
// Saving such an address hits payment_addresses.address's unique constraint. The buyer gets a fixed refusal
// instead of the generic 500 and the order stays a draft (the action's transaction, with its reservation, is rolled
// back). The refusal is counted per currency in payment_status, logged with the currency only (A-170: no address,
// wallet or driver text), and shown on /admin until an administrator dismisses it: a later payment request that
// succeeds does not clear it, because buyers' retries walk the wallet past the issued addresses without the rescan
// the runbook asks for. Nothing is retried within the request, and further requests are not held back: each one
// asks the wallet again, which moves its index on by one address the database already holds, so retries bring the
// wallet closer to handing out fresh addresses rather than using any up.

func init() {
	registerAction("/admin/payment-address-collisions", actionSpec{Roles: []string{"admin"}, Run: dismissAddressCollisionsAction})
}

// addressAlreadyIssued reports the unique violation on payment_addresses.address, and nothing else: any other
// failure saving the address (including a second address row for the order) stays a server error.
func addressAlreadyIssued(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "payment_addresses_address_key"
}

// addressCollisionRefusal answers the buyer once the action's transaction has been rolled back, then records the
// collision on its own connection (never inside the failed transaction, which PostgreSQL has aborted).
func addressCollisionRefusal(c *actionCtx, currency, network string) error {
	msg := currency + " payment addresses are being restored on this market. Nothing was charged and your order is still a draft; try again later."
	return &httpError{Code: http.StatusServiceUnavailable, Msg: msg, Render: func(w http.ResponseWriter) {
		c.A.recordAddressCollision(c.Ctx(), currency, network)
		serverError(w, c.R, http.StatusServiceUnavailable, msg, "payment-address-already-issued")
	}}
}

// recordAddressCollision counts one refused payment request for currency and logs it. A failure to record is
// logged by class; the buyer's refusal is the same either way.
func (a *App) recordAddressCollision(ctx context.Context, currency, network string) {
	var n int
	err := a.db.QueryRowContext(ctx, `INSERT INTO payment_status(currency,network,last_poll,last_error,address_collisions,address_collision_last) VALUES($1,$2,NULL,'',1,now())
		ON CONFLICT(currency) DO UPDATE SET address_collisions=payment_status.address_collisions+1,address_collision_last=now() RETURNING address_collisions`, currency, network).Scan(&n)
	if err != nil {
		log.Printf("payments: %s wallet returned a payment address already issued to another order; payment request refused, order kept as a draft; recording it for /admin failed: %s", currency, errorCause(err))
		return
	}
	log.Printf("payments: %s wallet returned a payment address already issued to another order (restored from an older backup?); payment request refused, order kept as a draft; collisions=%d; see \"Restore the wallets\" in docs/testnet-runbook.md", currency, n)
}

// dismissAddressCollisionsAction clears a currency's /admin warning. seen is the count the administrator was shown,
// so a collision recorded after the page was rendered (or a second dismissal) is refused rather than hidden.
func dismissAddressCollisionsAction(c *actionCtx) (actionResult, error) {
	cur := c.Form.Get("currency")
	seen, err := strconv.Atoi(c.Form.Get("seen"))
	if (cur != "BTC" && cur != "XMR") || err != nil || seen < 1 {
		return actionResult{}, fail(400, "Choose BTC or XMR and the warning shown on the page")
	}
	res, err := c.Tx.ExecContext(c.Ctx(), "UPDATE payment_status SET address_collisions=0,address_collision_last=NULL WHERE currency=$1 AND address_collisions=$2", cur, seen)
	if err != nil {
		return actionResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return actionResult{}, fail(409, "The "+cur+" issued-address warning changed after the page was shown. Reload the admin page to review it. Nothing was changed.")
	}
	requests := "payment requests"
	if seen == 1 {
		requests = "payment request"
	}
	return actionResult{Redirect: "/admin", Notice: "address-warning",
		Audit: "Dismissed the " + cur + " issued-address warning (" + strconv.Itoa(seen) + " " + requests + " refused)"}, nil
}
