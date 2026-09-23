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
