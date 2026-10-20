package market

import (
	"context"
	"database/sql"
	"net/http"
)

// VendorAvailabilityView contains own storefront preferences on /account, or only
// the availability and opted-in activity information published on /vendor.
type VendorAvailabilityView struct {
	Vacation, Deactivated, MessagesOffWhileAway, ShowLastSeen bool

	LastSeen string
}

func init() {
	registerAction("/account/vendor-availability", actionSpec{Roles: []string{"vendor"}, FormNonce: true, Run: vendorAvailabilityAction})
	registerLoader("*", vendorActivityLoader)
	registerLoader("account", vendorAvailabilityLoader)
	registerLoader("vendor", vendorAvailabilityLoader)
}

// A vendor can pause public trading without losing access to existing orders.
// These helpers take only aliases chosen by application code, never request data.
func availableForPurchaseSQL(alias string) string {
	return "(" + activeVendorSQL(alias) + " AND NOT " + alias + ".vendor_vacation)"
}

// This is an additional restriction on otherwise permitted contacts. Deactivated
// vendors are reached only through existing customer work, never public discovery.
func vendorAcceptsBuyerMessagesSQL(alias string) string {
	return "(NOT ((" + alias + ".vendor_vacation OR " + alias + ".vendor_deactivated) AND " + alias + ".vendor_messages_off_while_away))"
}

func requireVendorPublicKey(ctx context.Context, tx *sql.Tx, userID string) error {
	usable, err := userCanReceiveEncrypted(ctx, tx, userID)
	if err != nil {
		return err
	}
	if !usable {
		return fail(http.StatusConflict, "Vendors must keep a usable PGP public key in their account. Add or renew the public key before enabling the storefront. Keep your private key on your own device.")
	}
	return nil
}

func vendorAvailabilityAction(c *actionCtx) (actionResult, error) {
	mode := c.Form.Get("mode")
	if mode != "active" && mode != "vacation" && mode != "deactivated" {
		return actionResult{}, fail(http.StatusBadRequest, "Choose active, on vacation, or deactivated.")
	}
	var eligible bool
	if err := c.Tx.QueryRowContext(c.Ctx(), `SELECT role='vendor' AND suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL
		FROM users WHERE id=$1 FOR UPDATE`, c.User.ID).Scan(&eligible); err != nil {
		return actionResult{}, err
	}
	if !eligible {
		return actionResult{}, errAccountChanged
	}
	// A legacy vendor without a key can hide their store while fixing the key.
	if mode != "deactivated" {
		if err := requireVendorPublicKey(c.Ctx(), c.Tx, c.User.ID); err != nil {
			return actionResult{}, err
		}
	}
	_, err := c.Tx.ExecContext(c.Ctx(), `UPDATE users SET vendor_vacation=$2,vendor_deactivated=$3,
		vendor_messages_off_while_away=$4,vendor_show_last_seen=$5,
		vendor_last_seen=CASE WHEN $5 THEN vendor_last_seen ELSE NULL END WHERE id=$1`,
		c.User.ID, mode == "vacation", mode == "deactivated", c.Form.Get("messages_off_while_away") == "on", c.Form.Get("show_last_seen") == "on")
	return actionResult{Redirect: "/account#vendor-availability", Audit: "Updated vendor availability and activity privacy preferences"}, err
}

// Activity is recorded only after opting in. Keep at most the current hour, with
// at most one write per hour; opting out also erases the saved activity timestamp.
func vendorActivityLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if r.Method != http.MethodGet || d.User == nil || d.User.Role != "vendor" {
		return nil
	}
	_, err := a.db.ExecContext(ctx, `UPDATE users SET vendor_last_seen=date_trunc('hour',now())
		WHERE id=$1 AND role='vendor' AND vendor_show_last_seen
		AND (vendor_last_seen IS NULL OR vendor_last_seen<date_trunc('hour',now()))`, d.User.ID)
	return err
}

func vendorAvailabilityLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	var userID string
	switch d.Page {
	case "account":
		if d.User == nil || d.User.Role != "vendor" {
			return nil
		}
		userID = d.User.ID
	case "vendor":
		userID = r.URL.Query().Get("id")
	}
	var v VendorAvailabilityView
	// The CASE expression deliberately withholds hidden activity even from the
	// page model. Public templates never receive a hidden last-seen timestamp.
	err := a.db.QueryRowContext(ctx, `SELECT vendor_vacation,vendor_deactivated,vendor_messages_off_while_away,vendor_show_last_seen,
		CASE WHEN vendor_show_last_seen THEN coalesce(to_char(vendor_last_seen,'YYYY-MM-DD HH24:00 "UTC"'),'') ELSE '' END
		FROM users WHERE id=$1`, userID).Scan(&v.Vacation, &v.Deactivated, &v.MessagesOffWhileAway, &v.ShowLastSeen, &v.LastSeen)
	if err != nil {
		return err
	}
	d.VendorAvailability = &v
	return nil
}
