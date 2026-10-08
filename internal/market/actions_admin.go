package market

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
)

func init() {
	registerAction("/admin", actionSpec{Roles: []string{"admin"}, OwnTx: true, Run: adminAction})
	registerAction("/admin/reset-factors", actionSpec{Roles: []string{"admin"}, OwnTx: true, Run: adminResetFactorsAction})
	registerAction("/admin/suspend", actionSpec{Roles: []string{"admin"}, OwnTx: true, Run: adminSuspendAction})
	registerAction("/admin/review", actionSpec{Roles: []string{"admin"}, OwnTx: true, Run: adminReviewAction})
	registerLoader("admin", factorAccountsLoader)
	registerLoader("admin", suspendedAccountsLoader)
	registerPreview("admin", func(d *PageData) {
		d.FactorAccounts = []User{{ID: "ghost", Handle: "ghost_circuit", Role: "vendor", Factors: "TOTP"}}
		d.SuspendedAccounts = []User{{ID: "held", Handle: "held_account", Role: "buyer", Suspended: "2026-01-01 00:00 UTC"}}
	})
}

// suspendedAccountsLoader lists up to 100 suspended accounts, most recently suspended first, so they can be
// restored without remembering the handle; the form takes any handle, so accounts past the list work the same.
func suspendedAccountsLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	rows, err := a.db.QueryContext(ctx, `SELECT id,handle,role,to_char(suspended_at,'YYYY-MM-DD HH24:MI "UTC"') FROM users
		WHERE suspended_at IS NOT NULL ORDER BY suspended_at DESC, handle LIMIT 100`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u User
		if err = rows.Scan(&u.ID, &u.Handle, &u.Role, &u.Suspended); err != nil {
			return err
		}
		d.SuspendedAccounts = append(d.SuspendedAccounts, u)
	}
	return rows.Err()
}

// adminSuspendAction suspends or restores a non-administrator account, looked up by handle. Suspending ends
// the account's sessions and pending sign-ins and blocks sign-in (checked at the password step, when a
// session starts and whenever a session loads) and holds the account's unsent payouts for an administrator's
// payout address check; restoring allows sign-in again and leaves those holds. Role, listings and orders stay
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
		if err := waitForWalletLocks(ctx, tx); err != nil { // a checkout by the account holds its row (payAction)
			return actionResult{}, err
		}
		// Pending sign-ins go first, before the account row is locked: /challenge locks its pending sign-in and
		// then the account row, so taking them in the same order cannot deadlock. A refusal rolls this back.
		if _, err := tx.ExecContext(ctx, "DELETE FROM pending_logins WHERE user_id=$1", target); err != nil {
			return actionResult{}, err
		}
		// FOR NO KEY UPDATE, not FOR UPDATE: it still excludes session starts and payout queueing (both take FOR
		// SHARE), but not the key-share locks of inserts referencing the account (notifications, order events).
		// The watcher can hold one of this account's payouts and then insert such a row; with FOR UPDATE, the
		// payout update below would deadlock with it.
		var role string
		var suspended bool
		err := tx.QueryRowContext(ctx, "SELECT role,suspended_at IS NOT NULL FROM users WHERE id=$1 FOR NO KEY UPDATE", target).Scan(&role, &suspended)
		if err == sql.ErrNoRows {
			return actionResult{}, notFound
		}
		if err != nil {
			return actionResult{}, err
		}
		if role == "admin" {
			return actionResult{}, fail(403, "Administrator accounts cannot be suspended or restored here.")
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
			// Restoring does not release payouts held for the suspension (A-102); the owner is told how many wait
			// (A-120), and where each one's next step is.
			holds, err := collectStrings(ctx, tx, "SELECT error FROM payouts WHERE user_id=$1 AND state='held'", target)
			if err != nil {
				return actionResult{}, err
			}
			held := 0
			for _, e := range holds {
				if heldForSuspension("held", e) {
					held++
				}
			}
			note := "An administrator restored your account. You can sign in again."
			if held > 0 {
				note += " " + strconv.Itoa(held) + " payout(s) to you stay held until an administrator checks your payout address; your account page lists them."
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), target, note); err != nil {
				return actionResult{}, err
			}
			return actionResult{Redirect: "/admin", Notice: "restored", Audit: "Restored account " + handle}, nil
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
		// Unsent payouts to the account are held until an administrator checks their payout address: whoever
		// the account was suspended for may have changed it (A-102). The watcher's own hold is replaced, since the
		// watcher lifts that one by itself. A hold for a payout larger than the order price, or one an administrator
		// made to pay the payout by hand (A-488), is kept after the suspension text, so releasing it still needs that
		// review or confirmation too. A hold a restore from backup wrote (restoredHoldPrefix) keeps its text first and
		// gains the suspension's after it, unless it already carries one (A-489): heldForSuspension then asks for the
		// address check besides the wallet check, and a later restore keeps both. Payouts queued later start held
		// (enqueuePayout).
		heldRes, err := tx.ExecContext(ctx, `UPDATE payouts SET state='held',updated=now(),
			error=CASE WHEN state='held' AND error LIKE $6 || '%' THEN error || ' ' || $2
				WHEN state='held' AND (error LIKE $4 || '%' OR error LIKE $5 || '%') THEN $2 || ' ' || error ELSE $2 END
			WHERE user_id=$1 AND (state IN ('pending','blocked') OR (state='held' AND (error=$3 OR error LIKE $4 || '%' OR error LIKE $5 || '%'
				OR (error LIKE $6 || '%' AND error NOT LIKE '% ' || $7 || '%'))))`,
			target, suspendedHold, heldReason, largeHoldPrefix, handHoldPrefix, restoredHoldPrefix, suspendedHoldPrefix)
		if err != nil {
			return actionResult{}, err
		}
		note := "An administrator suspended your account and signed you out everywhere. A suspended account cannot sign in; your listings and orders were not changed."
		if h, _ := heldRes.RowsAffected(); h > 0 {
			sessions += "; held " + strconv.FormatInt(h, 10) + " unsent payout(s)"
			note += " " + strconv.FormatInt(h, 10) + " unsent payout(s) to you are held until an administrator checks your payout address."
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Account suspended by administrator "+c.User.Handle+"; ended "+sessions); err != nil {
			return actionResult{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), target, note); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/admin", Notice: "suspended", Audit: "Suspended account " + handle + "; ended " + sessions}, nil
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
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), buyerID, note); err != nil {
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
		if err := waitForWalletLocks(ctx, tx); err != nil { // a checkout by the account holds its row (payAction)
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
		if len(what) == 0 {
			return actionResult{}, fail(409, handle+" has no second factor enrolled; nothing was changed.")
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
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Second factors reset by administrator "+c.User.Handle+": "+off+" turned off; ended "+sessions); err != nil {
			return actionResult{}, err
		}
		note := "An administrator reset your two-factor sign-in (" + off + " turned off) and signed you out everywhere. Sign in with your password, then set up TOTP or PGP sign-in again from your account."
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), target, note); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/admin", Notice: "factors-reset", Audit: "Reset second factors of " + handle + ": " + off + " turned off; ended " + sessions}, nil
	})
}

// adminHandleError answers an admin form whose handle matched no eligible account with 404 and the admin
// page again: msg in its error alert and what was typed kept in the form (set by keep).
func adminHandleError(c *actionCtx, msg string, keep func(d *PageData)) error {
	return &httpError{Code: 404, Msg: msg, Render: func(w http.ResponseWriter) {
		a := c.A
		d := a.pageData(c.R, "admin", c.Token, c.User)
		if err := a.load(c.R, &d); err != nil {
			http.Error(w, msg, 404)
			return
		}
		d.Error = msg
		keep(&d)
		a.renderStatus(w, c.R, d, 404)
	}}
}

func adminAction(c *actionCtx) (actionResult, error) {
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
		return confirmedTx(c, true, adminChange)
	case "settings":
		return confirmedTx(c, false, adminChange)
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
		err = tx.QueryRowContext(ctx, "SELECT id,role FROM users WHERE handle=$1 AND role<>'admin' FOR UPDATE", handle).Scan(&target, &old)
		if err == sql.ErrNoRows {
			return actionResult{}, adminRoleHandleError(c, handle, role)
		}
		if err != nil {
			return actionResult{}, err
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
		}
		if err == nil {
			var more string
			more, err = roleChanged(ctx, tx, target, old, role, "administrator "+u.Handle)
			action += more
		}
	case "settings":
		name := strings.TrimSpace(f.Get("site_name"))
		if name == "" || len(name) > 80 {
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
	notice := "role"
	if f.Get("action") == "settings" {
		notice = "settings"
	}
	return actionResult{Redirect: "/admin", Notice: notice, Audit: action}, err
}

// roleChanged does what an account's new role entails, in tx after its users row is locked and its role set from
// old to role. The admin form and the -set-role host command (SetRole) share it; by names who changed the role in
// the account's audit rows (the administrator handle or "host command"). It returns what to add to the change's
// summary.
func roleChanged(ctx context.Context, tx *sql.Tx, target, old, role, by string) (string, error) {
	summary := ""
	avatarRemoved := false
	archivedListings := "0"
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
	if role != "vendor" {
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
		if old == "admin" && role != "admin" {
			note += " Your existing sessions and pending sign-ins were ended."
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), target, note); err != nil {
			return "", err
		}
	}
	return summary, nil
}
