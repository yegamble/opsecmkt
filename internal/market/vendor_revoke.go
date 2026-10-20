package market

import (
	"database/sql"
	"strings"
)

// vendorRevokeAction removes selling authority without removing a staff role.
// The target's account lock serializes with listing and upload mutations.
func vendorRevokeAction(c *actionCtx) (actionResult, error) {
	handle := strings.TrimSpace(c.Form.Get("handle"))
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx := c.Ctx(), c.Tx
		if err := waitForWalletLocks(ctx, tx); err != nil {
			return actionResult{}, err
		}
		var target, role string
		var approved bool
		err := tx.QueryRowContext(ctx, "SELECT id,role,vendor_approved FROM users WHERE handle=$1 FOR NO KEY UPDATE", handle).Scan(&target, &role, &approved)
		if err == sql.ErrNoRows {
			return actionResult{}, fail(404, "Account not found")
		}
		if err != nil {
			return actionResult{}, err
		}
		if role == "admin" {
			return actionResult{}, fail(409, "Administrators have selling access while they hold that role.")
		}
		if role != "vendor" && !approved {
			return actionResult{}, fail(409, "This account has no selling approval to remove.")
		}
		if _, err = tx.ExecContext(ctx, "UPDATE users SET vendor_approved=false,vendor_verified=false,role=CASE WHEN role='vendor' THEN 'buyer' ELSE role END WHERE id=$1", target); err != nil {
			return actionResult{}, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE products SET archived=true,archived_at=now(),updated=now() WHERE vendor_id=$1 AND NOT archived", target); err != nil {
			return actionResult{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, "Vendor selling approval removed by administrator "+c.User.Handle+"; active listings archived"); err != nil {
			return actionResult{}, err
		}
		if _, err = insertNotification(ctx, tx, target, "An administrator removed your selling approval. Active listings were archived; existing orders remain readable. Your staff role, if any, is unchanged.", ""); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: adminUserBack(c, handle, "/admin/vendors"), Notice: "vendor-revoked", Audit: "Removed vendor selling approval from " + handle}, nil
	})
}
