package market

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strings"
)

// A-545 (owner directive 2026-10-18): the user directory, on the Users section (/admin/users, administrators only,
// private administration ingress like every /admin path). The directory searches handles (case-insensitive substring,
// % and _ matched literally) and filters by role, account status and vendor state, newest accounts first, 50 a page.
// Each account's page, /admin/users?handle=<exact handle>, shows only what the market already stores for other
// reasons (role, vendor application and bond, verified mark, suspension and ban, second factors, PGP key, order,
// dispute and listing counts, retention state): no sign-in times, addresses or visits. Its forms post to the existing
// handlers (suspend, ban, second-factor reset, role, vendor application, decision and verified mark) with from=user,
// which returns them here (adminUserBack); a "Message user" link opens /messages?to=<handle>. The viewer's own account
// and other administrators get no forms, as those handlers refuse them.

// UserDirectoryView is the directory: the search and filters as applied ("" = any) and one page of accounts.
type UserDirectoryView struct {
	Find, Role, Status, Vendor string
	Rows                       []UserDirectoryRow
	Paging                     *ActivityPaging
}

// UserDirectoryRow is one account in the directory. Vendor is its vendor state ("" for none).
type UserDirectoryRow struct {
	Handle, Role, Created, Status, Vendor string
	Verified                              bool
}

// UserStateCount is the number of an account's orders in one state, as buyer and as vendor.
type UserStateCount struct {
	Label         string
	Buyer, Vendor int
}

// UserDisputeCounts counts disputes on an account's orders: all, still open, decided for the vendor (release) and
// decided for the buyer (refund).
type UserDisputeCounts struct{ Total, Open, Release, Refund int }

// AdminUserView is one account's page in the directory. Times are UTC text; "" when unset.
type AdminUserView struct {
	ID, Handle, Role, Created, Status          string
	Self                                       bool // the viewing administrator's own account
	Suspended, SuspendedBy                     string
	Banned, BannedBy, BanReason                string
	DeletionRequested, Restricted, Closed      string
	TOTP, PGPSignIn                            bool
	PGPFingerprint, PGPVerified                string
	HasPGP                                     bool
	Verified                                   bool
	VerifiedBy, VerifiedAt                     string
	AutoApprove                                bool                   // MOD72-6: listings publish without staff review (listing_review.go)
	Application                                *VendorApplicationView // the latest vendor application
	Policy                                     VendorPolicyView       // what a new application would follow
	Orders                                     []UserStateCount       // states with any order, in state order
	BuyerOrders, VendorOrders                  int
	DisputesOpened, DisputesAgainst            UserDisputeCounts
	ActiveListings, ArchivedListings           int
	IsBanned, IsSuspended, IsClosing, IsClosed bool
}

// Actionable reports whether the account forms apply: never on the viewer's own account or an administrator's.
func (v *AdminUserView) Actionable() bool { return !v.Self && v.Role != "admin" }

// CanApply reports whether an administrator can open a vendor application for the account (vendorApplyForAction).
func (v *AdminUserView) CanApply() bool {
	return v.Actionable() && v.Role == "buyer" && !v.IsBanned && !v.IsSuspended && !v.IsClosing && !v.IsClosed && (v.Application == nil || !v.Application.Open)
}

var directoryRoles = []string{"buyer", "vendor", "moderator", "admin"}
var directoryStatuses = []string{"active", "suspended", "banned", "closed"}
var directoryVendor = []string{"awaiting_bond", "awaiting_approval", "approved", "verified"}

// directoryStates is the order the account page lists order states in.
var directoryStates = []string{stateDraft, stateAwaitingPayment, statePaid, stateShipped, stateDelivered, stateCompleted, stateDisputed, stateResolved, stateCancelled}

func init() {
	registerLoader("admin", adminUsersLoader)
	registerPreview("admin", func(d *PageData) {
		d.UserDirectory = &UserDirectoryView{Rows: []UserDirectoryRow{
			{Handle: "ghost_circuit", Role: "vendor", Created: "2026-01-03 09:00 UTC", Status: "Active", Vendor: "Approved vendor", Verified: true},
			{Handle: "new_seller", Role: "buyer", Created: "2026-01-02 09:00 UTC", Status: "Active", Vendor: "Applicant: waiting for bond"},
			{Handle: "held_account", Role: "buyer", Created: "2026-01-01 09:00 UTC", Status: "Suspended"},
		}, Paging: activityPagingWithKey("/admin/users", "users_page", 3, "", nil)}
	})
}

// adminUserURL is an account's page in the directory.
func adminUserURL(handle string) string { return "/admin/users?handle=" + url.QueryEscape(handle) }

// adminUserBack is where an account form returns: the account's page when it was posted from there (from=user),
// otherwise fallback, the section page that holds the form.
func adminUserBack(c *actionCtx, handle, fallback string) string {
	if c.Form.Get("from") == "user" {
		return adminUserURL(handle)
	}
	return fallback
}

// oneOf is v when it is one of allowed, else "".
func oneOf(v string, allowed []string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return ""
}

// accountStatusLabel names an account's state, a ban or closure first.
func accountStatusLabel(banned, closed, closing, suspended bool) string {
	switch {
	case banned:
		return "Banned"
	case closed:
		return "Closed"
	case suspended && closing:
		return "Suspended, closing"
	case suspended:
		return "Suspended"
	case closing:
		return "Closing"
	}
	return "Active"
}

func adminUsersLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.AdminSection != "users" || d.User == nil || d.User.Role != "admin" {
		return nil
	}
	if d.AdminUserHandle != "" {
		return adminUserLoader(ctx, a, d)
	}
	return userDirectoryLoader(ctx, a, r, d)
}

// The account's vendor state in the directory: the vendor role, else an open application.
const directoryVendorSQL = `CASE WHEN u.role='vendor' THEN 'Approved vendor' ELSE coalesce((SELECT CASE a.state WHEN 'awaiting_bond' THEN 'Applicant: waiting for bond'
	ELSE 'Applicant: waiting for approval' END FROM vendor_applications a WHERE a.user_id=u.id AND a.state IN ` + vendorOpenStates + ` LIMIT 1),'') END`

// The directory's filter: $1 the search, $2 its literal ILIKE pattern, $3 role, $4 status, $5 vendor state ("" = any).
// E'\\' is one backslash whatever standard_conforming_strings an external database sets.
const directoryFilterSQL = ` WHERE ($1='' OR u.handle ILIKE $2 ESCAPE E'\\') AND ($3='' OR u.role=$3)
	AND ($4='' OR ($4='active' AND u.suspended_at IS NULL AND u.closed_at IS NULL AND u.deletion_requested_at IS NULL AND u.retention_restricted_at IS NULL)
		OR ($4='suspended' AND u.suspended_at IS NOT NULL AND u.banned_at IS NULL AND u.closed_at IS NULL)
		OR ($4='banned' AND u.banned_at IS NOT NULL)
		OR ($4='closed' AND (u.closed_at IS NOT NULL OR u.deletion_requested_at IS NOT NULL OR u.retention_restricted_at IS NOT NULL)))
	AND ($5='' OR ($5='verified' AND u.vendor_verified) OR ($5='approved' AND u.role='vendor')
		OR ($5 IN ('awaiting_bond','awaiting_approval') AND EXISTS(SELECT 1 FROM vendor_applications a WHERE a.user_id=u.id AND a.state=$5)))`

func userDirectoryLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	q := r.URL.Query()
	v := &UserDirectoryView{Find: cleanParam(q.Get("find"), 64), Role: oneOf(q.Get("role"), directoryRoles),
		Status: oneOf(q.Get("status"), directoryStatuses), Vendor: oneOf(q.Get("vendor"), directoryVendor)}
	args := []any{v.Find, likePattern(v.Find), v.Role, v.Status, v.Vendor}
	var total int
	if err := a.db.QueryRowContext(ctx, "SELECT count(*) FROM users u"+directoryFilterSQL, args...).Scan(&total); err != nil {
		return err
	}
	keep := url.Values{}
	for k, x := range map[string]string{"find": v.Find, "role": v.Role, "status": v.Status, "vendor": v.Vendor} {
		if x != "" {
			keep.Set(k, x)
		}
	}
	v.Paging = activityPagingWithKey("/admin/users", "users_page", total, q.Get("users_page"), keep)
	rows, err := a.db.QueryContext(ctx, `SELECT u.handle,u.role,to_char(u.created,'YYYY-MM-DD HH24:MI "UTC"'),u.banned_at IS NOT NULL,u.closed_at IS NOT NULL,
		u.deletion_requested_at IS NOT NULL OR u.retention_restricted_at IS NOT NULL,u.suspended_at IS NOT NULL,`+directoryVendorSQL+`,u.vendor_verified
		FROM users u`+directoryFilterSQL+` ORDER BY u.created DESC,u.id DESC LIMIT $6 OFFSET $7`, append(args, activityPageSize, v.Paging.Offset())...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var row UserDirectoryRow
		var banned, closed, closing, suspended bool
		if err = rows.Scan(&row.Handle, &row.Role, &row.Created, &banned, &closed, &closing, &suspended, &row.Vendor, &row.Verified); err != nil {
			return err
		}
		row.Status = accountStatusLabel(banned, closed, closing, suspended)
		v.Rows = append(v.Rows, row)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	d.UserDirectory = v
	return nil
}

const userTimeSQL = `'YYYY-MM-DD HH24:MI "UTC"'`

// adminUserLoader fills the account page for d.AdminUserHandle (exact handle); an unknown handle is 404.
func adminUserLoader(ctx context.Context, a *App, d *PageData) error {
	v := &AdminUserView{Policy: vendorPolicyFromSettings(d.Settings)}
	err := a.db.QueryRowContext(ctx, `SELECT u.id,u.handle,u.role,to_char(u.created,`+userTimeSQL+`),
		coalesce(to_char(u.suspended_at,`+userTimeSQL+`),''),coalesce(to_char(u.banned_at,`+userTimeSQL+`),''),coalesce(b.handle,''),coalesce(u.ban_reason,''),
		coalesce(to_char(u.deletion_requested_at,`+userTimeSQL+`),''),coalesce(to_char(u.retention_restricted_at,`+userTimeSQL+`),''),coalesce(to_char(u.closed_at,`+userTimeSQL+`),''),
		u.totp_enabled,u.pgp_2fa,u.pgp<>'',u.pgp_fingerprint,coalesce(to_char(u.pgp_verified_at,`+userTimeSQL+`),''),
		u.vendor_verified,coalesce(vb.handle,''),coalesce(to_char(u.vendor_verified_set_at,`+userTimeSQL+`),''),u.listing_auto_approve,
		(SELECT count(*) FROM products p WHERE p.vendor_id=u.id AND NOT p.archived),(SELECT count(*) FROM products p WHERE p.vendor_id=u.id AND p.archived)
		FROM users u LEFT JOIN users b ON b.id=u.banned_by LEFT JOIN users vb ON vb.id=u.vendor_verified_set_by WHERE u.handle=$1`, d.AdminUserHandle).Scan(
		&v.ID, &v.Handle, &v.Role, &v.Created, &v.Suspended, &v.Banned, &v.BannedBy, &v.BanReason, &v.DeletionRequested, &v.Restricted, &v.Closed,
		&v.TOTP, &v.PGPSignIn, &v.HasPGP, &v.PGPFingerprint, &v.PGPVerified, &v.Verified, &v.VerifiedBy, &v.VerifiedAt, &v.AutoApprove, &v.ActiveListings, &v.ArchivedListings)
	if err == sql.ErrNoRows {
		return fail(http.StatusNotFound, "No account has the handle \""+d.AdminUserHandle+"\". Check the spelling: handles are case-sensitive.")
	}
	if err != nil {
		return err
	}
	v.Self = v.ID == d.User.ID
	v.IsBanned, v.IsSuspended, v.IsClosed = v.Banned != "", v.Suspended != "", v.Closed != ""
	v.IsClosing = !v.IsClosed && (v.DeletionRequested != "" || v.Restricted != "")
	v.Status = accountStatusLabel(v.IsBanned, v.IsClosed, v.IsClosing, v.IsSuspended)
	if v.IsSuspended && !v.IsBanned {
		// Who suspended it is in the account's audit trail ("Account suspended by administrator X; ended …"), written in
		// the transaction that set suspended_at (so at the same now()); an older row belongs to an earlier suspension.
		var action string
		err = a.db.QueryRowContext(ctx, `SELECT e.action FROM audit_events e JOIN users u ON u.id=e.user_id WHERE e.user_id=$1
			AND e.action LIKE 'Account suspended by administrator %' AND e.created>=u.suspended_at ORDER BY e.created DESC,e.id DESC LIMIT 1`, v.ID).Scan(&action)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		by, _, _ := strings.Cut(strings.TrimPrefix(action, "Account suspended by administrator "), ";")
		v.SuspendedBy = by
	}
	app, err := scanVendorApp(a.db.QueryRowContext(ctx, "SELECT "+vendorAppColumns+` FROM vendor_applications a JOIN users u ON u.id=a.user_id
		WHERE a.user_id=$1 ORDER BY a.created DESC,a.id DESC LIMIT 1`, v.ID))
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if app.State == "approved" && v.Role != "vendor" {
			app.RoleRemoved, app.Status = true, "Approved; the vendor role was removed later"
		}
		if err = a.fillVendorBond(ctx, &app, false, true); err != nil { // A-547: XMR received and flags
			return err
		}
		v.Application = &app
	}
	counts := map[string]UserStateCount{}
	rows, err := a.db.QueryContext(ctx, `SELECT o.state,count(*) FILTER (WHERE o.buyer_id=$1),count(*) FILTER (WHERE p.vendor_id=$1)
		FROM orders o JOIN products p ON p.id=o.product_id WHERE o.buyer_id=$1 OR p.vendor_id=$1 GROUP BY o.state`, v.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var c UserStateCount
		if err = rows.Scan(&state, &c.Buyer, &c.Vendor); err != nil {
			return err
		}
		counts[state] = c
		v.BuyerOrders += c.Buyer
		v.VendorOrders += c.Vendor
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, s := range directoryStates {
		if c, ok := counts[s]; ok {
			c.Label = stateLabel(s)
			v.Orders = append(v.Orders, c)
		}
	}
	// The party who opened each dispute is the actor of the order's move into disputed (as orders_load.go reads it).
	o, g := &v.DisputesOpened, &v.DisputesAgainst
	err = a.db.QueryRowContext(ctx, `WITH d AS (SELECT d.outcome,(SELECT e.actor_id FROM order_events e WHERE e.order_id=d.order_id AND e.to_state='disputed'
			AND e.from_state<>'disputed' ORDER BY e.id DESC LIMIT 1) AS opener
		FROM disputes d JOIN orders o ON o.id=d.order_id JOIN products p ON p.id=o.product_id WHERE o.buyer_id=$1 OR p.vendor_id=$1)
		SELECT count(*) FILTER (WHERE opener=$1),count(*) FILTER (WHERE opener=$1 AND outcome=''),count(*) FILTER (WHERE opener=$1 AND outcome='release'),
			count(*) FILTER (WHERE opener=$1 AND outcome='refund'),count(*) FILTER (WHERE opener IS DISTINCT FROM $1),
			count(*) FILTER (WHERE opener IS DISTINCT FROM $1 AND outcome=''),count(*) FILTER (WHERE opener IS DISTINCT FROM $1 AND outcome='release'),
			count(*) FILTER (WHERE opener IS DISTINCT FROM $1 AND outcome='refund') FROM d`, v.ID).Scan(
		&o.Total, &o.Open, &o.Release, &o.Refund, &g.Total, &g.Open, &g.Release, &g.Refund)
	if err != nil {
		return err
	}
	d.AdminUser = v
	return nil
}

// previewAdminUser is the preview's account page for handle, from the directory's sample rows (no database).
func previewAdminUser(d *PageData, handle string) bool {
	if handle != "ghost_circuit" {
		return false
	}
	d.AdminUser = &AdminUserView{ID: "ghost", Handle: handle, Role: "vendor", Created: "2026-01-03 09:00 UTC", Status: "Active",
		TOTP: true, HasPGP: true, PGPFingerprint: "0123 4567 89AB CDEF 0123  4567 89AB CDEF 0123 4567", PGPVerified: "2026-01-04 09:00 UTC",
		Verified: true, VerifiedBy: "preview_user", VerifiedAt: "2026-01-05 09:00 UTC",
		Application: &VendorApplicationView{State: "approved", Status: vendorStateLabel("approved"), Bond: "0", Created: "2026-01-03 09:05 UTC", Decided: "2026-01-03 10:00 UTC"},
		Policy:      VendorPolicyView{Bond: "0.5", HasBond: true},
		Orders: []UserStateCount{{Label: stateLabel(stateShipped), Vendor: 2}, {Label: stateLabel(stateCompleted), Buyer: 1, Vendor: 14},
			{Label: stateLabel(stateResolved), Vendor: 1}},
		BuyerOrders: 1, VendorOrders: 17, DisputesAgainst: UserDisputeCounts{Total: 1, Release: 1}, ActiveListings: 3, ArchivedListings: 1}
	d.AdminUserHandle = handle
	return true
}
