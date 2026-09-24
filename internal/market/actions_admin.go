package market

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
)

func init() {
	registerAction("/admin", actionSpec{Roles: []string{"admin"}, Run: adminAction})
	registerAction("/admin/reset-factors", actionSpec{Roles: []string{"admin"}, OwnTx: true, Run: adminResetFactorsAction})
	registerAction("/admin/suspend", actionSpec{Roles: []string{"admin"}, OwnTx: true, Run: adminSuspendAction})
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
			if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), target, "An administrator restored your account. You can sign in again."); err != nil {
				return actionResult{}, err
			}
			return actionResult{Redirect: "/admin?saved=1#suspend", Audit: "Restored account " + handle}, nil
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
		// watcher lifts that one by itself. Payouts queued later start held (enqueuePayout).
		heldRes, err := tx.ExecContext(ctx, `UPDATE payouts SET state='held',error=$2,updated=now()
			WHERE user_id=$1 AND (state IN ('pending','blocked') OR (state='held' AND error=$3))`, target, suspendedHold, heldReason)
		if err != nil {
			return actionResult{}, err
		}
		if h, _ := heldRes.RowsAffected(); h > 0 {
			sessions += "; held " + strconv.FormatInt(h, 10) + " unsent payout(s)"
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Account suspended by administrator "+c.User.Handle+"; ended "+sessions); err != nil {
			return actionResult{}, err
		}
		note := "An administrator suspended your account and signed you out everywhere. A suspended account cannot sign in; your listings and orders were not changed."
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), target, note); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/admin?saved=1#suspend", Audit: "Suspended account " + handle + "; ended " + sessions}, nil
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
		for _, q := range []string{
			"UPDATE users SET totp_enabled=false,totp_secret='',totp_pending='',totp_last_step=0,recovery_reveal='',recovery_reveal_until=NULL,pgp_2fa=false WHERE id=$1",
			"DELETE FROM recovery_codes WHERE user_id=$1",
			"DELETE FROM pending_logins WHERE user_id=$1",
		} {
			if _, err = tx.ExecContext(ctx, q, target); err != nil {
				return actionResult{}, err
			}
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
		return actionResult{Redirect: "/admin?saved=1#reset-factors", Audit: "Reset second factors of " + handle + ": " + off + " turned off; ended " + sessions}, nil
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
		a.renderStatus(w, d, 404)
	}}
}

func adminAction(c *actionCtx) (actionResult, error) {
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
		var target, old string
		err = tx.QueryRowContext(ctx, "SELECT id,role FROM users WHERE handle=$1 AND role<>'admin' FOR UPDATE", handle).Scan(&target, &old)
		if err == sql.ErrNoRows {
			return actionResult{}, adminHandleError(c, "No buyer, vendor or moderator has the handle \""+handle+"\". Check the spelling: handles are case-sensitive, and administrator roles cannot be changed here.", func(d *PageData) {
				d.RoleHandle, d.RoleChoice = handle, role
			})
		}
		if err != nil {
			return actionResult{}, err
		}
		// Both accounts get an audit row naming the change; an unchanged role is recorded for the administrator only.
		action = "Role of " + handle + " unchanged (already " + role + ")"
		if old != role {
			action = "Changed role of " + handle + " from " + old + " to " + role
			if _, err = tx.ExecContext(ctx, "UPDATE users SET role=$1 WHERE id=$2", role, target); err != nil {
				return actionResult{}, err
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Role changed from "+old+" to "+role+" by an administrator")
		}
		if err == nil && role != "vendor" {
			// A user who is no longer a vendor cannot manage listings, so their active listings are archived in
			// this transaction (audited for both accounts). Existing orders are unaffected.
			n := "0"
			err = tx.QueryRowContext(ctx, "WITH a AS (UPDATE products SET archived=true,archived_at=now(),updated=now() WHERE vendor_id=$1 AND NOT archived RETURNING 1) SELECT count(*)::text FROM a", target).Scan(&n)
			if err == nil && n != "0" {
				action += "; archived " + n + " active listing(s)"
				_, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Archived "+n+" active listing(s): role changed to "+role+" by an administrator")
			}
		}
	case "settings":
		name := strings.TrimSpace(f.Get("site_name"))
		if name == "" || len(name) > 80 {
			return actionResult{}, fail(400, "Invalid settings")
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('site_name',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value", name)
		action = "Saved marketplace settings"
	default:
		return actionResult{}, fail(400, "Unknown action")
	}
	return actionResult{Redirect: "/admin?saved=1", Audit: action}, err
}
