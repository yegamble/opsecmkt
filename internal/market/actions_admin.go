package market

import (
	"database/sql"
	"strings"
)

func init() {
	registerAction("/admin", actionSpec{Roles: []string{"admin"}, Run: adminAction})
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
		var result sql.Result
		result, err = tx.ExecContext(ctx, "UPDATE users SET role=$1 WHERE id=$2 AND role<>'admin'", role, f.Get("user_id"))
		if err == nil {
			if n, _ := result.RowsAffected(); n != 1 {
				return actionResult{}, fail(404, "Eligible user not found")
			}
		}
		action = "Changed user role"
		if err == nil && role != "vendor" {
			// A user who is no longer a vendor cannot manage listings, so their active listings are archived in
			// this transaction (audited for both accounts). Existing orders are unaffected.
			n := "0"
			err = tx.QueryRowContext(ctx, "WITH a AS (UPDATE products SET archived=true,archived_at=now(),updated=now() WHERE vendor_id=$1 AND NOT archived RETURNING 1) SELECT count(*)::text FROM a", f.Get("user_id")).Scan(&n)
			if err == nil && n != "0" {
				action = "Changed user role to " + role + "; archived " + n + " active listing(s)"
				_, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", f.Get("user_id"), "Archived "+n+" active listing(s): role changed to "+role+" by an administrator")
			}
		}
	case "settings":
		name := strings.TrimSpace(f.Get("site_name"))
		btc, xmr := f.Get("bitcoin_mode"), f.Get("monero_mode")
		valid := func(s string) bool { return s == "disabled" || s == "local" || s == "external" }
		if name == "" || len(name) > 80 || !valid(btc) || !valid(xmr) {
			return actionResult{}, fail(400, "Invalid settings")
		}
		for k, v := range map[string]string{"site_name": name, "bitcoin_mode": btc, "monero_mode": xmr} {
			if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value", k, v); err != nil {
				break
			}
		}
		action = "Saved desired node configuration; operator apply required"
	default:
		return actionResult{}, fail(400, "Unknown action")
	}
	return actionResult{Redirect: "/admin?saved=1", Audit: action}, err
}
