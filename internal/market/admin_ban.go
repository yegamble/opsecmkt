package market

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A-546 (owner decision 2026-10-18): a ban is an administrator's permanent closure of an account, distinct from a
// reversible suspension (actions_admin.go). Migration 123 stores it in users.banned_at, banned_by and ban_reason, and
// requires a banned account to be suspended too, so every rule that refuses or skips a suspended account applies to a
// banned one: sign-in (refused with a neutral message once the password is correct), sessions, new drafts and payment
// requests (lockNewIntake), messages (activeVendorSQL; messageRecipient also refuses every other route to it), dispute
// resolvers and staff notices, and public images. A ban also ends the account's sessions and pending sign-ins,
// archives its active listings, withdraws an open vendor application, removes a verified vendor mark and turns off
// listing auto-approval (A-650: the vendor's listings wait for review again in hold mode); a banned moderator becomes
// a buyer. Its handle, in lower case, is kept in banned_handles, so registration and setup refuse it
// (refuseTakenHandle) even after retention closes and erases the account. Open orders are otherwise left alone (an
// administrator refunds a banned vendor's unpaid and unshipped ones separately, admin_ban_orders.go, A-652b): the ban
// only moves the release deadline of a banned vendor's shipped Monero escrow orders with no dispute to at least 72 hours
// after the ban and tells each buyer (xmrBanDeadlines, A-652d), since their release is then held (A-652c). The other
// party can still complete, concede or dispute them, an independent administrator decides disputes, and settlements
// proceed. Lifting a ban (unban) restores the suspension the account had before it (ban_prior_suspended_at)
// or, with none, allows sign-in again; listings stay archived, the verified mark and auto-approval stay off (an account
// banned before A-650 that still has auto-approval loses it here) and no staff role returns.
// Both need a reason and the administrator's password (and authenticator code when enrolled); both accounts are
// audited, with the reason. Administrator accounts, the caller's own included, cannot be banned, so neither can the
// last administrator.

func init() {
	registerAction("/admin/ban", actionSpec{Roles: []string{"admin"}, OwnTx: true,
		ConfirmedFormNonce: true, ReceiptAuthorize: adminBanReceiptAuthorize, Run: adminBanAction})
	registerLoader("admin", bannedAccountsLoader)
	registerPreview("admin", func(d *PageData) {
		d.BannedAccounts = []User{{ID: "closed", Handle: "closed_vendor", Role: "vendor", Banned: "2026-01-02 00:00 UTC", BannedBy: "admin", BanReason: "Took payment for orders and shipped nothing."}}
	})
}

// Ban reasons are read by administrators and recorded in the audit trail; they must say something.
const minBanReason, maxBanReason = 10, 500

var errBanReason = fail(400, "Give a reason of 10–500 characters, on one line. Nothing was changed.")

// cleanBanReason trims a ban or unban reason and checks it: valid UTF-8, one line of printable characters (no control
// or text-direction characters, which are not graphic), 10–500 characters.
func cleanBanReason(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if !utf8.ValidString(v) || strings.IndexFunc(v, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
		return "", errBanReason
	}
	if n := utf8.RuneCountInString(v); n < minBanReason || n > maxBanReason {
		return "", errBanReason
	}
	return v, nil
}

func adminBanReceiptAuthorize(c *actionCtx) error {
	return adminTargetReceiptAuthorize(c, fail(400, "You cannot ban your own account."), errBanAdmin)
}

var errBanAdmin = fail(403, "Administrator accounts cannot be banned here, so neither can the last administrator.")

// bannedAccountsLoader lists up to 100 banned accounts, most recently banned first, with who banned them and why.
func bannedAccountsLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	rows, err := a.db.QueryContext(ctx, `SELECT u.id,u.handle,u.role,to_char(u.banned_at,'YYYY-MM-DD HH24:MI "UTC"'),coalesce(b.handle,''),u.ban_reason
		FROM users u LEFT JOIN users b ON b.id=u.banned_by
		WHERE u.banned_at IS NOT NULL ORDER BY u.banned_at DESC, u.handle LIMIT 100`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u User
		if err = rows.Scan(&u.ID, &u.Handle, &u.Role, &u.Banned, &u.BannedBy, &u.BanReason); err != nil {
			return err
		}
		d.BannedAccounts = append(d.BannedAccounts, u)
	}
	return rows.Err()
}

// withCountedReviews appends to a ban or unban audit summary how many reviews by account id the change takes out of
// or puts back into the ratings (A-649): those on completed orders an administrator has not hidden, which count
// whenever the account is not banned (reviewCountedSQL). Nothing is appended when there are none.
func withCountedReviews(ctx context.Context, tx *sql.Tx, id, summary, effect string) (string, error) {
	var n int64
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM reviews r JOIN orders o ON o.id=r.order_id WHERE r.buyer_id=$1 AND o.state='completed' AND r.hidden_at IS NULL", id).Scan(&n); err != nil || n == 0 {
		return summary, err
	}
	return summary + "; " + strconv.FormatInt(n, 10) + " review(s) by the account " + effect, nil
}

// autoApproveOff ends a ban or unban audit summary that turned off the account's listing auto-approval (A-650).
const autoApproveOff = "; listing auto-approval turned off"

// adminBanAction bans or lifts the ban on a non-administrator account, looked up by handle (see the A-546 comment
// above). It takes the locks in the suspension's order (adminSuspendAction): wallet lock wait, pending sign-ins, then
// the account row; a ban of an account not yet banned first locks its shipped orders as vendor (lockBanShippedOrders).
func adminBanAction(c *actionCtx) (actionResult, error) {
	choice := c.Form.Get("action")
	ban := choice == "ban"
	if !ban && choice != "unban" {
		return actionResult{}, fail(400, "Choose ban or unban")
	}
	handle := strings.TrimSpace(c.Form.Get("handle"))
	if handle == "" {
		return actionResult{}, fail(400, "Enter an account handle")
	}
	if handle == c.User.Handle {
		return actionResult{}, fail(400, "You cannot ban your own account.")
	}
	reason, err := cleanBanReason(c.Form.Get("reason"))
	if err != nil {
		return actionResult{}, err
	}
	// The audit rows continue after the reason ("…: R; ended …"), so a final full stop is left out there (SEC-546-6);
	// ban_reason keeps it as written.
	said := strings.TrimSuffix(reason, ".")
	// The handle is looked up before the password is checked, so a typo does not use a confirmation attempt.
	notFound := adminHandleError(c, "No account has the handle \""+handle+"\". Check the spelling: handles are case-sensitive.", func(d *PageData) {
		d.BanHandle, d.BanChoice, d.BanReason = handle, choice, reason
	})
	var target string
	var wasBanned bool
	err = c.A.db.QueryRowContext(c.Ctx(), "SELECT id,banned_at IS NOT NULL FROM users WHERE handle=$1", handle).Scan(&target, &wasBanned)
	if err == sql.ErrNoRows {
		return actionResult{}, notFound
	}
	if err != nil {
		return actionResult{}, err
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx, by := c.Ctx(), c.Tx, "administrator "+c.User.Handle
		if err := waitForWalletLocks(ctx, tx); err != nil { // a checkout by the account holds its row (escrowCheckoutAction)
			return actionResult{}, err
		}
		if ban && !wasBanned { // before the account row (PAY90-2); a repeated ban is refused below and changes no order
			if err := lockBanShippedOrders(ctx, tx, target); err != nil {
				return actionResult{}, err
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM pending_logins WHERE user_id=$1", target); err != nil {
			return actionResult{}, err
		}
		var role string
		var banned, closed, verified, auto bool
		err := tx.QueryRowContext(ctx, "SELECT role,banned_at IS NOT NULL,closed_at IS NOT NULL,vendor_verified,listing_auto_approve FROM users WHERE id=$1 FOR NO KEY UPDATE", target).
			Scan(&role, &banned, &closed, &verified, &auto)
		if err == sql.ErrNoRows {
			return actionResult{}, notFound
		}
		if err != nil {
			return actionResult{}, err
		}
		if role == "admin" {
			return actionResult{}, errBanAdmin
		}
		audit := func(row string) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, row)
			return err
		}
		if !ban {
			switch {
			case !banned:
				return actionResult{}, fail(409, handle+" is not banned; nothing was changed.")
			case closed: // a closed account stays suspended (retention.go); lifting its ban would not reopen it
				return actionResult{}, fail(409, handle+" is closed; nothing was changed.")
			}
			// The suspension the account had before the ban (if any) comes back with its own time; otherwise none.
			var stillSuspended bool
			if err = tx.QueryRowContext(ctx, `UPDATE users SET suspended_at=ban_prior_suspended_at,banned_at=NULL,banned_by=NULL,ban_reason=NULL,ban_prior_suspended_at=NULL
				WHERE id=$1 RETURNING suspended_at IS NOT NULL`, target).Scan(&stillSuspended); err != nil {
				return actionResult{}, err
			}
			// The reservation goes unless a case variant of the handle (a legacy database) is banned too.
			if _, err = tx.ExecContext(ctx, `DELETE FROM banned_handles h WHERE h.handle=lower($1)
				AND NOT EXISTS(SELECT 1 FROM users u WHERE lower(u.handle)=h.handle AND u.banned_at IS NOT NULL AND u.id<>$2)`, handle, target); err != nil {
				return actionResult{}, err
			}
			summary, note := "", "An administrator lifted the ban on your account. You can sign in again. Your listings stay archived: publish them again from the vendor desk if you still sell."
			if stillSuspended {
				summary = "; it stays suspended, as it was before the ban"
				note = "An administrator lifted the ban on your account. It stays suspended, as it was before the ban, so it cannot sign in until an administrator restores it. Your listings stay archived."
			}
			if summary, err = withCountedReviews(ctx, tx, target, summary, "count toward ratings again"); err != nil {
				return actionResult{}, err
			}
			if auto { // only an account banned before A-650 can still have it: lifting the ban never leaves it on
				if _, err = tx.ExecContext(ctx, "UPDATE users SET listing_auto_approve=false WHERE id=$1", target); err != nil {
					return actionResult{}, err
				}
				summary += autoApproveOff
			}
			if err = audit("Ban lifted by " + by + ": " + said + summary); err != nil {
				return actionResult{}, err
			}
			if _, err = insertNotification(ctx, tx, target, note, ""); err != nil {
				return actionResult{}, err
			}
			status := accountRestoredStatus // order partners were told the account is closed
			if stillSuspended {
				status = accountSuspendedStatus
			}
			if err = notifyActiveCounterparties(ctx, tx, target, handle, status); err != nil {
				return actionResult{}, err
			}
			return actionResult{Redirect: adminUserBack(c, handle, "/admin/users"), Notice: "unbanned", Audit: "Lifted the ban on " + handle + ": " + said + summary}, nil
		}
		switch {
		case banned:
			return actionResult{}, fail(409, handle+" is already banned; nothing was changed.")
		case closed:
			return actionResult{}, fail(409, handle+" is already closed; nothing was changed.")
		}
		if _, err = tx.ExecContext(ctx, `UPDATE users SET banned_at=now(),banned_by=$2,ban_reason=$3,ban_prior_suspended_at=suspended_at,suspended_at=COALESCE(suspended_at,now()),
			listing_auto_approve=false,vendor_verified=false,vendor_verified_set_by=CASE WHEN vendor_verified THEN $2 ELSE vendor_verified_set_by END,
			vendor_verified_set_at=CASE WHEN vendor_verified THEN now() ELSE vendor_verified_set_at END WHERE id=$1`, target, c.User.ID, reason); err != nil {
			return actionResult{}, err
		}
		ended, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=$1", target)
		if err != nil {
			return actionResult{}, err
		}
		n, _ := ended.RowsAffected()
		summary := "; ended " + strconv.FormatInt(n, 10) + " session(s) and any pending sign-ins"
		if summary, err = withCountedReviews(ctx, tx, target, summary, "stopped counting toward ratings"); err != nil {
			return actionResult{}, err
		}
		// A-652d: each buyer of the account's shipped orders gets at least 72 hours from now to dispute, and is told.
		moved, told, err := xmrBanDeadlines(ctx, tx, target)
		if err != nil {
			return actionResult{}, err
		}
		if told > 0 {
			summary += "; told the buyers of " + strconv.Itoa(told) + " shipped order(s) they may dispute until at least 72 hours from now (" + strconv.Itoa(moved) + " release deadline(s) moved)"
		}
		if auto {
			summary += autoApproveOff
		}
		if err = audit("Account banned by " + by + ": " + said + summary); err != nil {
			return actionResult{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO banned_handles(handle) VALUES(lower($1)) ON CONFLICT DO NOTHING", handle); err != nil {
			return actionResult{}, err
		}
		if role == "moderator" { // a banned moderator leaves staff for good, as the admin role form does it
			if _, err = tx.ExecContext(ctx, "UPDATE users SET role='buyer' WHERE id=$1", target); err != nil {
				return actionResult{}, err
			}
			if err = audit("Role changed from moderator to buyer by " + by + " (account banned)"); err != nil {
				return actionResult{}, err
			}
			more, err := roleChanged(ctx, tx, target, "moderator", "buyer", by)
			if err != nil {
				return actionResult{}, err
			}
			summary += "; changed role from moderator to buyer" + more
		}
		var archived string
		if err = tx.QueryRowContext(ctx, "WITH a AS (UPDATE products SET archived=true,archived_at=now(),updated=now() WHERE vendor_id=$1 AND NOT archived RETURNING 1) SELECT count(*)::text FROM a", target).Scan(&archived); err != nil {
			return actionResult{}, err
		}
		if archived != "0" {
			summary += "; archived " + archived + " active listing(s)"
			if err = audit("Archived " + archived + " active listing(s): account banned by " + by); err != nil {
				return actionResult{}, err
			}
		}
		withdrawn, err := tx.ExecContext(ctx, `UPDATE vendor_applications SET state='withdrawn',decided_by=$2,decided_at=now(),note='Account banned'
			WHERE user_id=$1 AND state IN `+vendorOpenStates, target, c.User.ID)
		if err != nil {
			return actionResult{}, err
		}
		if n, _ := withdrawn.RowsAffected(); n > 0 {
			summary += "; withdrew the open vendor application"
			if err = audit("Vendor application withdrawn: account banned by " + by); err != nil {
				return actionResult{}, err
			}
		}
		if verified {
			summary += "; removed the verified vendor mark"
			if err = audit("Verified vendor mark removed by " + by + ": account banned"); err != nil {
				return actionResult{}, err
			}
		}
		note := "An administrator closed your account permanently and signed you out everywhere. It cannot sign in, and your active listings were archived. Existing orders continue for the other party and the market staff."
		if _, err = insertNotification(ctx, tx, target, note, ""); err != nil {
			return actionResult{}, err
		}
		if err = notifyActiveCounterparties(ctx, tx, target, handle, accountClosedStatus); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: adminUserBack(c, handle, "/admin/users"), Notice: "banned", Audit: "Banned account " + handle + ": " + said + summary}, nil
	})
}
