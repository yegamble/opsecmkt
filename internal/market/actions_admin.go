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
	registerLoader("admin", factorAccountsLoader)
	registerPreview("admin", func(d *PageData) {
		d.FactorAccounts = []User{{ID: "ghost", Handle: "ghost_circuit", Role: "vendor", Factors: "TOTP"}}
	})
}

// factorAccountsLoader lists every non-administrator account with a second factor for the reset form
// (independent of the admin page's 100 most recent accounts, so older accounts can be recovered too).
func factorAccountsLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	rows, err := a.db.QueryContext(ctx, `SELECT id,handle,role,concat_ws(' and ',CASE WHEN totp_enabled THEN 'TOTP' END,CASE WHEN pgp_2fa THEN 'PGP sign-in' END)
		FROM users WHERE role<>'admin' AND (totp_enabled OR pgp_2fa) ORDER BY handle LIMIT 1000`)
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
	target := c.Form.Get("user_id")
	if target == "" {
		return actionResult{}, fail(400, "Choose an account")
	}
	if target == c.User.ID {
		return actionResult{}, fail(400, "You cannot reset your own second factors here. Use your recovery codes, or follow the operator procedure for administrator lockout.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx := c.Ctx(), c.Tx
		var handle, role string
		var totp, pgp bool
		err := tx.QueryRowContext(ctx, "SELECT handle,role,totp_enabled,pgp_2fa FROM users WHERE id=$1 FOR UPDATE", target).Scan(&handle, &role, &totp, &pgp)
		if err == sql.ErrNoRows {
			return actionResult{}, fail(404, "Account not found")
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
		if f.Get("user_id") == u.ID {
			return actionResult{}, fail(400, "Cannot change your own administrator role")
		}
		var handle, old string
		err = tx.QueryRowContext(ctx, "SELECT handle,role FROM users WHERE id=$1 AND role<>'admin' FOR UPDATE", f.Get("user_id")).Scan(&handle, &old)
		if err == sql.ErrNoRows {
			return actionResult{}, fail(404, "Eligible user not found")
		}
		if err != nil {
			return actionResult{}, err
		}
		// Both accounts get an audit row naming the change; an unchanged role is recorded for the administrator only.
		action = "Role of " + handle + " unchanged (already " + role + ")"
		if old != role {
			action = "Changed role of " + handle + " from " + old + " to " + role
			if _, err = tx.ExecContext(ctx, "UPDATE users SET role=$1 WHERE id=$2", role, f.Get("user_id")); err != nil {
				return actionResult{}, err
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", f.Get("user_id"), "Role changed from "+old+" to "+role+" by an administrator")
		}
		if err == nil && role != "vendor" {
			// A user who is no longer a vendor cannot manage listings, so their active listings are archived in
			// this transaction (audited for both accounts). Existing orders are unaffected.
			n := "0"
			err = tx.QueryRowContext(ctx, "WITH a AS (UPDATE products SET archived=true,archived_at=now(),updated=now() WHERE vendor_id=$1 AND NOT archived RETURNING 1) SELECT count(*)::text FROM a", f.Get("user_id")).Scan(&n)
			if err == nil && n != "0" {
				action += "; archived " + n + " active listing(s)"
				_, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", f.Get("user_id"), "Archived "+n+" active listing(s): role changed to "+role+" by an administrator")
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
