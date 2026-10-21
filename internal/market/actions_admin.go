package market

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
)

func init() {
	registerAction("/admin", actionSpec{Roles: []string{"admin"}, OwnTx: true, OwnFormNonce: true, Run: adminAction})
	registerAction("/admin/reset-factors", actionSpec{Roles: []string{"admin"}, OwnTx: true,
		ConfirmedFormNonce: true, ReceiptAuthorize: adminResetReceiptAuthorize, Run: adminResetFactorsAction})
	registerAction("/admin/suspend", actionSpec{Roles: []string{"admin"}, OwnTx: true,
		ConfirmedFormNonce: true, ReceiptAuthorize: adminSuspendReceiptAuthorize, Run: adminSuspendAction})
	registerAction("/admin/review", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: adminReviewAction})
	registerLoader("admin", factorAccountsLoader)
	registerLoader("admin", suspendedAccountsLoader)
	registerPreview("admin", func(d *PageData) {
		d.FactorAccounts = []User{{ID: "ghost", Handle: "ghost_circuit", Role: "vendor", Factors: "TOTP"}}
		d.SuspendedAccounts = []User{{ID: "held", Handle: "held_account", Role: "buyer", Suspended: "2026-01-01 00:00 UTC", Factors: "TOTP and PGP sign-in"}}
	})
}

// suspendedAccountsLoader lists up to 100 suspended accounts that are not banned (bannedAccountsLoader lists those),
// most recently suspended first, so they can be restored without remembering the handle; the form takes any handle,
// so accounts past the list work the same.
func suspendedAccountsLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	rows, err := a.db.QueryContext(ctx, `SELECT id,handle,role,to_char(suspended_at,'YYYY-MM-DD HH24:MI "UTC"'),
		CASE WHEN totp_enabled AND pgp_2fa THEN 'TOTP and PGP sign-in'
			WHEN totp_enabled THEN 'TOTP' WHEN pgp_2fa THEN 'PGP sign-in' ELSE '' END
		FROM users
		WHERE suspended_at IS NOT NULL AND banned_at IS NULL ORDER BY suspended_at DESC, handle LIMIT 100`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u User
		if err = rows.Scan(&u.ID, &u.Handle, &u.Role, &u.Suspended, &u.Factors); err != nil {
			return err
		}
		d.SuspendedAccounts = append(d.SuspendedAccounts, u)
	}
	return rows.Err()
}

// The target must still be a different, non-administrator account before a
// confirmed staff receipt can be replayed (self and admin are the refusals). A
// successful suspend/restore, ban/unban or factor reset changes the very state
// their callbacks require on first use,
// so this check intentionally does not require the former suspended/factor
// state. It takes no target lock: replay changes nothing, while a first use
// repeats all target checks under the callback's established lock order.
func adminTargetReceiptAuthorize(c *actionCtx, self, admin error) error {
	handle := strings.TrimSpace(c.Form.Get("handle"))
	var target, role string
	err := c.Tx.QueryRowContext(c.Ctx(), "SELECT id,role FROM users WHERE handle=$1", handle).Scan(&target, &role)
	if err == sql.ErrNoRows {
		return fail(404, "Account not found")
	}
	if err != nil {
		return err
	}
	if target == c.User.ID {
		return self
	}
	if role == "admin" {
		return admin
	}
	return nil
}

func adminSuspendReceiptAuthorize(c *actionCtx) error {
	return adminTargetReceiptAuthorize(c, fail(400, "You cannot suspend your own account."), fail(403, "Administrator accounts cannot be suspended or restored here."))
}

func adminResetReceiptAuthorize(c *actionCtx) error {
	return adminTargetReceiptAuthorize(c, fail(400, "You cannot reset your own second factors here. Use your recovery codes, or follow the operator procedure for administrator lockout."),
		fail(403, "Administrator second factors cannot be reset here; administrator lockout is an operator procedure."))
}

// adminSuspendAction suspends or restores a non-administrator account, looked up by handle. Suspending ends
// the account's sessions and pending sign-ins and blocks sign-in (checked at the password step, when a
// session starts and whenever a session loads); restoring allows sign-in again. Role, listings and orders stay
// as they are: orders waiting on the account stop until it is restored, and the other party can still
// complete, cancel where allowed or dispute them. A suspended moderator is not a dispute resolver, recipient
// or contact (A-118). Both accounts are audited and the account is notified. The administrator confirms
// with their own password (and authenticator code when enrolled). Not available for the administrator's own
// account or another administrator.
func adminSuspendAction(c *actionCtx) (actionResult, error) {
	choice := c.Form.Get("action")
	suspend := choice == "suspend"
	if !suspend && choice != "restore" {
		return actionResult{}, fail(400, "Choose suspend or restore")
	}
	handle := strings.TrimSpace(c.Form.Get("handle"))
	if handle == "" {
		return actionResult{}, fail(400, "Enter an account handle")
	}
	if handle == c.User.Handle {
		return actionResult{}, fail(400, "You cannot suspend your own account.")
	}
	// The handle is looked up before the password is checked, so a typo does not use a confirmation attempt.
	notFound := adminHandleError(c, "No account has the handle \""+handle+"\". Check the spelling: handles are case-sensitive.", func(d *PageData) {
		d.SuspendHandle, d.SuspendChoice = handle, choice
	})
	var target string
	err := c.A.db.QueryRowContext(c.Ctx(), "SELECT id FROM users WHERE handle=$1", handle).Scan(&target)
	if err == sql.ErrNoRows {
		return actionResult{}, notFound
	}
	if err != nil {
		return actionResult{}, err
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx := c.Ctx(), c.Tx
		if err := waitForWalletLocks(ctx, tx); err != nil { // a checkout by the account holds its row (escrowCheckoutAction)
			return actionResult{}, err
		}
		// Pending sign-ins go first, before the account row is locked: /challenge locks its pending sign-in and
		// then the account row, so taking them in the same order cannot deadlock. A refusal rolls this back.
		if _, err := tx.ExecContext(ctx, "DELETE FROM pending_logins WHERE user_id=$1", target); err != nil {
			return actionResult{}, err
		}
		// FOR NO KEY UPDATE, not FOR UPDATE: it still excludes session starts (FOR SHARE), but not the key-share
		// locks of inserts referencing the account (notifications, order events) that a watcher pass may hold.
		var role string
		var suspended, banned bool
		err := tx.QueryRowContext(ctx, "SELECT role,suspended_at IS NOT NULL,banned_at IS NOT NULL FROM users WHERE id=$1 FOR NO KEY UPDATE", target).Scan(&role, &suspended, &banned)
		if err == sql.ErrNoRows {
			return actionResult{}, notFound
		}
		if err != nil {
			return actionResult{}, err
		}
		if role == "admin" {
			return actionResult{}, fail(403, "Administrator accounts cannot be suspended or restored here.")
		}
		if banned { // A-546: a ban is lifted only with its own form and a reason (admin_ban.go)
			return actionResult{}, fail(409, handle+" is banned; nothing was changed. Lift the ban under Ban or unban an account, with a reason, to let it sign in again.")
		}
		if !suspend {
			if !suspended {
				return actionResult{}, fail(409, handle+" is not suspended; nothing was changed.")
			}
			if _, err = tx.ExecContext(ctx, "UPDATE users SET suspended_at=NULL WHERE id=$1", target); err != nil {
				return actionResult{}, err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Account restored by administrator "+c.User.Handle); err != nil {
				return actionResult{}, err
			}
			note := "An administrator restored your account. You can sign in again."
			if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), target, note); err != nil {
				return actionResult{}, err
			}
			if err = notifyActiveCounterparties(ctx, tx, target, handle, accountRestoredStatus); err != nil {
				return actionResult{}, err
			}
			return actionResult{Redirect: adminUserBack(c, handle, "/admin/users"), Notice: "restored", Audit: "Restored account " + handle}, nil
		}
		if suspended {
			return actionResult{}, fail(409, handle+" is already suspended; nothing was changed.")
		}
		if _, err = tx.ExecContext(ctx, "UPDATE users SET suspended_at=now() WHERE id=$1", target); err != nil {
			return actionResult{}, err
		}
		ended, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=$1", target)
		if err != nil {
			return actionResult{}, err
		}
		n, _ := ended.RowsAffected()
		sessions := strconv.FormatInt(n, 10) + " session(s) and any pending sign-ins"
		note := "An administrator suspended your account and signed you out everywhere. A suspended account cannot sign in; your listings and orders were not changed."
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Account suspended by administrator "+c.User.Handle+"; ended "+sessions); err != nil {
			return actionResult{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), target, note); err != nil {
			return actionResult{}, err
		}
		if err = notifyActiveCounterparties(ctx, tx, target, handle, accountSuspendedStatus); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: adminUserBack(c, handle, "/admin/users"), Notice: "suspended", Audit: "Suspended account " + handle + "; ended " + sessions}, nil
	})
}

// adminReviewAction hides a public review or restores a hidden one (A-125), from the review's own form on the
// product or vendor page (returning there). A hidden review is kept: it leaves the public pages and their rating,
// and the buyer and vendor still see it, labelled, on the order. The administrator confirms with their own password
// (and authenticator code when enrolled); both accounts are audited and the reviewer is notified, naming only the
// short order ID: the review text may be the reason for hiding it, so it is never copied anywhere.
func adminReviewAction(c *actionCtx) (actionResult, error) {
	choice := c.Form.Get("action")
	hide := choice == "hide"
	if !hide && choice != "restore" {
		return actionResult{}, fail(400, "Choose hide or restore")
	}
	id := c.Form.Get("review_id")
	notFound := fail(404, "Review not found")
	// The review is looked up before the password is checked, so a stale form does not use a confirmation attempt.
	var exists bool
	if err := c.A.db.QueryRowContext(c.Ctx(), "SELECT EXISTS(SELECT 1 FROM reviews WHERE id=$1)", id).Scan(&exists); err != nil {
		return actionResult{}, err
	}
	if !exists {
		return actionResult{}, notFound
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx := c.Ctx(), c.Tx
		var orderID, productID, vendorID, buyerID string
		var hidden bool
		err := tx.QueryRowContext(ctx, `SELECT r.order_id,r.product_id,p.vendor_id,r.buyer_id,r.hidden_at IS NOT NULL
			FROM reviews r JOIN products p ON p.id=r.product_id WHERE r.id=$1 FOR NO KEY UPDATE OF r`, id).Scan(&orderID, &productID, &vendorID, &buyerID, &hidden)
		if err == sql.ErrNoRows {
			return actionResult{}, notFound
		}
		if err != nil {
			return actionResult{}, err
		}
		short := shortID(orderID)
		verb, target, note := "Hid", "hidden", "An administrator hid your review of order "+short+" from the listing and vendor pages, and it no longer counts in the rating. It is kept: you and the vendor still see it on the order."
		if !hide {
			verb, target, note = "Restored", "restored", "An administrator restored your review of order "+short+" to the listing and vendor pages."
		}
		switch {
		case hide && hidden:
			return actionResult{}, fail(409, "This review is already hidden; nothing was changed.")
		case !hide && !hidden:
			return actionResult{}, fail(409, "This review is not hidden; nothing was changed.")
		}
		if _, err = tx.ExecContext(ctx, "UPDATE reviews SET hidden_at=CASE WHEN $2 THEN now() END WHERE id=$1", id, hide); err != nil {
			return actionResult{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", buyerID, "Review of order "+short+" "+target+" by administrator "+c.User.Handle); err != nil {
			return actionResult{}, err
		}
		if _, err = insertNotification(ctx, tx, buyerID, note, orderID); err != nil {
			return actionResult{}, err
		}
		back := "/product?id=" + productID
		if c.Form.Get("from") == "vendor" {
			back = "/vendor?id=" + vendorID
		}
		notice := "review-hidden"
		if !hide {
			notice = "review-shown"
		}
		return actionResult{Redirect: back, Notice: notice, Audit: verb + " review of order " + short}, nil
	})
}

// factorAccountsLoader lists up to 100 non-administrator accounts with a second factor, by handle, as a
// reminder beside the reset form; the form takes any handle, so accounts past the list are reset the same way.
func factorAccountsLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	rows, err := a.db.QueryContext(ctx, `SELECT id,handle,role,concat_ws(' and ',CASE WHEN totp_enabled THEN 'TOTP' END,CASE WHEN pgp_2fa THEN 'PGP sign-in' END)
		FROM users WHERE role<>'admin' AND (totp_enabled OR pgp_2fa) ORDER BY handle LIMIT 100`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u User
		if err = rows.Scan(&u.ID, &u.Handle, &u.Role, &u.Factors); err != nil {
			return err
		}
		d.FactorAccounts = append(d.FactorAccounts, u)
	}
	return rows.Err()
}

// adminResetFactorsAction recovers a user who lost their second factors: it turns TOTP off (secret, pending
// secret, step counter, recovery codes and any unrevealed codes) and PGP sign-in off, keeping the PGP key and
// its verification. It ends the user's sessions and pending sign-ins, audits both accounts and notifies the
// user. The administrator confirms with their own password (and authenticator code when enrolled). Not
// available for the administrator's own account or another administrator; administrator lockout is an
// operator procedure (docs/operator-guide.md).
func adminResetFactorsAction(c *actionCtx) (actionResult, error) {
	replacement := strings.TrimSpace(c.Form.Get("replacement_pgp"))
	var replacementFingerprint string
	if replacement != "" {
		if c.Form.Get("recovery_identity_verified") != "yes" {
			return actionResult{}, fail(400, "Confirm that the account holder's identity was verified out of band before replacing their public key.")
		}
		canonical, fingerprint, err := validateRecoveryPublicKey(replacement)
		if err != nil {
			return actionResult{}, err
		}
		replacement, replacementFingerprint = canonical, fingerprint
	}
	handle := strings.TrimSpace(c.Form.Get("handle"))
	if handle == "" {
		return actionResult{}, fail(400, "Enter an account handle")
	}
	if handle == c.User.Handle {
		return actionResult{}, fail(400, "You cannot reset your own second factors here. Use your recovery codes, or follow the operator procedure for administrator lockout.")
	}
	// The handle is looked up before the password is checked, so a typo does not use a confirmation attempt.
	notFound := adminHandleError(c, "No account has the handle \""+handle+"\". Check the spelling: handles are case-sensitive.", func(d *PageData) { d.ResetHandle = handle })
	var target string
	err := c.A.db.QueryRowContext(c.Ctx(), "SELECT id FROM users WHERE handle=$1", handle).Scan(&target)
	if err == sql.ErrNoRows {
		return actionResult{}, notFound
	}
	if err != nil {
		return actionResult{}, err
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx := c.Ctx(), c.Tx
		if err := waitForWalletLocks(ctx, tx); err != nil { // a checkout by the account holds its row (escrowCheckoutAction)
			return actionResult{}, err
		}
		// Pending sign-ins, then recovery codes, go before the account row is locked, in the order /challenge takes
		// them (A-154); a refusal below rolls this back.
		for _, q := range []string{"DELETE FROM pending_logins WHERE user_id=$1", "DELETE FROM recovery_codes WHERE user_id=$1"} {
			if _, err := tx.ExecContext(ctx, q, target); err != nil {
				return actionResult{}, err
			}
		}
		var role string
		var totp, pgp bool
		err := tx.QueryRowContext(ctx, "SELECT role,totp_enabled,pgp_2fa FROM users WHERE id=$1 FOR UPDATE", target).Scan(&role, &totp, &pgp)
		if err == sql.ErrNoRows {
			return actionResult{}, notFound
		}
		if err != nil {
			return actionResult{}, err
		}
		if role == "admin" {
			return actionResult{}, fail(403, "Administrator second factors cannot be reset here; administrator lockout is an operator procedure.")
		}
		var what []string
		if totp {
			what = append(what, "TOTP and recovery codes")
		}
		if pgp {
			what = append(what, "PGP sign-in")
		}
		if len(what) == 0 && replacement == "" {
			return actionResult{}, fail(409, handle+" has no second factor enrolled; nothing was changed.")
		}
		if replacement != "" {
			if err = replaceRecoveryPublicKey(ctx, tx, target, replacement); err != nil {
				return actionResult{}, err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE users SET totp_enabled=false,totp_secret='',totp_pending='',totp_last_step=0,recovery_reveal='',recovery_reveal_until=NULL,pgp_2fa=false WHERE id=$1", target); err != nil {
			return actionResult{}, err
		}
		ended, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=$1", target)
		if err != nil {
			return actionResult{}, err
		}
		n, _ := ended.RowsAffected()
		sessions := strconv.FormatInt(n, 10) + " session(s)"
		off := strings.Join(what, " and ")
		changes := off + " turned off"
		if replacement != "" {
			changes = "PGP public key replaced (fingerprint " + replacementFingerprint + "; identity verified out of band)"
			if off != "" {
				changes += "; " + off + " turned off"
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Second factors reset by administrator "+c.User.Handle+": "+changes+"; ended "+sessions); err != nil {
			return actionResult{}, err
		}
		note := "An administrator reset your two-factor sign-in (" + off + " turned off) and signed you out everywhere. Sign in with your password, then set up TOTP or PGP sign-in again from your account."
		if replacement != "" {
			note = "An administrator performed identity-verified recovery and replaced your PGP public key (fingerprint " + replacementFingerprint + "). All sessions and pending sign-ins ended; optional second factors were reset. Your password is unchanged. Ownership of the new key remains unverified until you prove it. A market requiring PGP at sign-in still requires decryption with your new key."
		} else if required, err := pgpLoginRequired(ctx, tx); err != nil {
			return actionResult{}, err
		} else if required {
			note = "An administrator reset your optional second factors and signed you out everywhere. This market still requires PGP decryption at sign-in. If your bound key is lost, identity-verified replacement-key recovery is required."
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), target, note); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: adminUserBack(c, handle, "/admin/users"), Notice: "factors-reset", Audit: "Reset second factors of " + handle + ": " + changes + "; ended " + sessions}, nil
	})
}

// adminHandleError answers an admin form whose handle matched no eligible account with 404 and the Users section
// that holds every such form (A-542) again: msg in its error alert and what was typed kept in the form (set by keep).
func adminHandleError(c *actionCtx, msg string, keep func(d *PageData)) error {
	return &httpError{Code: 404, Msg: msg, Render: func(w http.ResponseWriter) {
		a := c.A
		d := a.pageData(c.R, "admin/users", c.Token, c.User)
		if err := a.load(c.R, &d); err != nil {
			a.refuse(w, c.R, c.User, 404, msg)
			return
		}
		d.Error = msg
		keep(&d)
		a.renderStatus(w, c.R, d, 404)
	}}
}

func adminAction(c *actionCtx) (actionResult, error) {
	required, completed, err := completedOwnFormMode(c)
	if err != nil {
		return actionResult{}, err
	}
	if completed {
		return replayOwnForm(c, required)
	}
	switch c.Form.Get("action") {
	case "role":
		role := c.Form.Get("role")
		if role != "buyer" && role != "vendor" && role != "moderator" {
			return actionResult{}, fail(400, "Choose buyer, vendor, or moderator")
		}
		handle := strings.TrimSpace(c.Form.Get("handle"))
		if handle == "" {
			return actionResult{}, fail(400, "Enter an account handle")
		}
		if handle == c.User.Handle {
			return actionResult{}, fail(400, "Cannot change your own administrator role")
		}
		// Keep unknown handles out of the password-attempt budget, as the other admin forms do.
		// adminChange repeats the lookup under its own row lock after authentication.
		var exists bool
		if err := c.A.db.QueryRowContext(c.Ctx(), "SELECT EXISTS(SELECT 1 FROM users WHERE handle=$1 AND role<>'admin')", handle).Scan(&exists); err != nil {
			return actionResult{}, err
		}
		if !exists {
			return actionResult{}, adminRoleHandleError(c, handle, role)
		}
		return ownConfirmedTx(c, true, adminChange)
	case "settings":
		return ownConfirmedTx(c, false, adminChange)
	default:
		return actionResult{}, fail(400, "Unknown action")
	}
}

func adminRoleHandleError(c *actionCtx, handle, role string) error {
	return adminHandleError(c, "No buyer, vendor or moderator has the handle \""+handle+"\". Check the spelling: handles are case-sensitive, and administrator roles cannot be changed here.", func(d *PageData) {
		d.RoleHandle, d.RoleChoice = handle, role
	})
}

// adminChange runs after confirmedTx has locked and rechecked the administrator row.
func adminChange(c *actionCtx) (actionResult, error) {
	ctx, f, tx, u := c.Ctx(), c.Form, c.Tx, c.User
	var err error
	action := ""
	switch f.Get("action") {
	case "role":
		role := f.Get("role")
		if role != "buyer" && role != "vendor" && role != "moderator" {
			return actionResult{}, fail(400, "Choose buyer, vendor, or moderator")
		}
		handle := strings.TrimSpace(f.Get("handle"))
		if handle == "" {
			return actionResult{}, fail(400, "Enter an account handle")
		}
		if handle == u.Handle {
			return actionResult{}, fail(400, "Cannot change your own administrator role")
		}
		// Leaving vendor archives the listings, whose rows a checkout may hold while it waits on the wallet.
		if err = waitForWalletLocks(ctx, tx); err != nil {
			return actionResult{}, err
		}
		var target, old string
		var banned bool
		err = tx.QueryRowContext(ctx, "SELECT id,role,banned_at IS NOT NULL FROM users WHERE handle=$1 AND role<>'admin' FOR UPDATE", handle).Scan(&target, &old, &banned)
		if err == sql.ErrNoRows {
			return actionResult{}, adminRoleHandleError(c, handle, role)
		}
		if err != nil {
			return actionResult{}, err
		}
		if banned && role != "buyer" { // A-546: lifting the ban must not hand back a staff or vendor role (SEC-546-2)
			return actionResult{}, fail(409, handle+" is banned; nothing was changed.")
		}
		// Both accounts get an audit row naming the change; an unchanged role is recorded for the administrator only.
		// The account's row ("Role changed from OLD to NEW …") also tells disputeEvidence when it left staff (A-294).
		action = "Role of " + handle + " unchanged (already " + role + ")"
		if old != role {
			action = "Changed role of " + handle + " from " + old + " to " + role
			if _, err = tx.ExecContext(ctx, "UPDATE users SET role=$1 WHERE id=$2", role, target); err != nil {
				return actionResult{}, err
			}
			actor := "administrator " + u.Handle
			_, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Role changed from "+old+" to "+role+" by "+actor)
			if err == nil && role == "vendor" { // A-544: the assignment is an approved vendor application
				err = vendorRoleAssigned(ctx, tx, target, u.ID, "Role assigned by an administrator")
			}
		}
		if err == nil {
			var more string
			more, err = roleChanged(ctx, tx, target, old, role, "administrator "+u.Handle)
			action += more
		}
	case "settings":
		if strings.TrimSpace(f.Get("site_name")) == "" {
			return actionResult{}, fail(400, "Invalid settings")
		}
		name, nameErr := setupSiteName(f.Get("site_name"))
		if nameErr != nil {
			return actionResult{}, fail(400, "Invalid settings")
		}
		var previous string
		err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='site_name' FOR UPDATE").Scan(&previous)
		if err != nil && err != sql.ErrNoRows {
			return actionResult{}, err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('site_name',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value", name)
		action = "Saved marketplace site name from " + strconv.Quote(previous) + " to " + strconv.Quote(name)
	default:
		return actionResult{}, fail(400, "Unknown action")
	}
	// Each form returns to the section that holds it (A-542): the role form to Users (or the account's page in the user
	// directory it was posted from, A-545), the market name to Site & branding.
	notice := "role"
	back := adminUserBack(c, strings.TrimSpace(f.Get("handle")), "/admin/users")
	if f.Get("action") == "settings" {
		notice = "settings"
		back = "/admin/site"
	}
	return actionResult{Redirect: back, Notice: notice, Audit: action}, err
}

// roleChanged does what an account's new role entails, in tx after its users row is locked and its role set from
// old to role. The admin form and the -set-role host command (SetRole) share it; by names who changed the role in
// the account's audit rows (the administrator handle or "host command"). It returns what to add to the change's
// summary.
func roleChanged(ctx context.Context, tx *sql.Tx, target, old, role, by string) (string, error) {
	summary := ""
	avatarRemoved, markRemoved, applicationWithdrawn := false, false, false
	archivedListings := "0"
	// A-583: the verified vendor mark belongs to the vendor role. Leaving it (for any role, staff included) removes the
	// mark, audited on the account, so a later promotion back to vendor does not bring it back. A suspension only hides
	// the badge (verifiedVendorSQL). An administrator holds no mark (verified by role; the verify form refuses them), so
	// leaving that role clears one kept from before A-583 too, even back to vendor.
	if old != role && (role != "vendor" || old == "admin") {
		res, err := tx.ExecContext(ctx, "UPDATE users SET vendor_verified=false WHERE id=$1 AND vendor_verified", target)
		if err != nil {
			return "", err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			markRemoved = true
			summary += "; removed their verified vendor mark"
			if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Verified vendor mark removed: role changed to "+role+" by "+by); err != nil {
				return "", err
			}
		}
	}
	// A-583: a staff role cannot be decided as an application (approval takes buyers and moderators; administrators
	// already sell), so a promotion to moderator or administrator withdraws an open one, audited; a moderator may apply
	// again. A promotion to vendor closes it as an approved assignment instead (vendorRoleAssigned).
	if old != role && (role == "moderator" || role == "admin") {
		res, err := tx.ExecContext(ctx, "UPDATE vendor_applications SET state='withdrawn',decided_at=now(),note=$2 WHERE user_id=$1 AND state IN "+vendorOpenStates, target, "Role changed to "+role)
		if err != nil {
			return "", err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			applicationWithdrawn = true
			summary += "; withdrew their open vendor application"
			if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Vendor application withdrawn: role changed to "+role+" by "+by); err != nil {
				return "", err
			}
		}
	}
	// A move into staff retains an existing vendor approval. Administrator
	// access itself is derived from the current role and never creates approval.
	if role == "vendor" || old == "vendor" && (role == "moderator" || role == "admin") {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET vendor_approved=true WHERE id=$1", target); err != nil {
			return "", err
		}
	} else if role == "buyer" {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET vendor_approved=false,vendor_verified=false WHERE id=$1", target); err != nil {
			return "", err
		}
	}
	if old != role && (old == "moderator" || old == "admin") && role != "moderator" {
		// PR #28 review: a staff avatar was uploaded under the staff rules (hidden unless listed); as a
		// vendor's it would be public. It is deleted here (D4's hard delete), after the caller's users row lock.
		// The public staff list choice was made as staff too: it is reset to the default (hidden), so a later
		// promotion starts hidden again.
		if _, err := tx.ExecContext(ctx, "UPDATE users SET staff_hidden=true WHERE id=$1", target); err != nil {
			return "", err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM images WHERE owner_user_id=$1 AND kind='avatar'", target)
		if err != nil {
			return "", err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			summary += "; removed their staff avatar"
			avatarRemoved = true
			if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Staff avatar removed when the role changed"); err != nil {
				return "", err
			}
		}
	}
	var selling bool
	if err := tx.QueryRowContext(ctx, "SELECT role IN ('vendor','admin') OR vendor_approved FROM users WHERE id=$1", target).Scan(&selling); err != nil {
		return "", err
	}
	if !selling {
		// A user who is no longer a vendor cannot manage listings, so their active listings are archived in
		// this transaction (audited on the account). Existing orders are unaffected.
		if err := tx.QueryRowContext(ctx, "WITH a AS (UPDATE products SET archived=true,archived_at=now(),updated=now() WHERE vendor_id=$1 AND NOT archived RETURNING 1) SELECT count(*)::text FROM a", target).Scan(&archivedListings); err != nil {
			return "", err
		}
		if archivedListings != "0" {
			summary += "; archived " + archivedListings + " active listing(s)"
			if _, err := tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Archived "+archivedListings+" active listing(s): role changed to "+role+" by "+by); err != nil {
				return "", err
			}
		}
	}
	if old != role {
		note := "Your account role changed from " + old + " to " + role + "."
		if archivedListings != "0" {
			note += " Your " + archivedListings + " active listing(s) were archived; existing orders remain readable."
		}
		if avatarRemoved {
			note += " Your staff avatar was removed."
		}
		if markRemoved {
			note += " Your verified vendor mark was removed."
		}
		if applicationWithdrawn {
			note += " Your open vendor application was withdrawn."
		}
		if old == "admin" && role != "admin" {
			note += " Your existing sessions and pending sign-ins were ended."
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), target, note); err != nil {
			return "", err
		}
	}
	return summary, nil
}
