package market

import (
	"net/http"
	"slices"
	"strconv"
)

// A-706 (owner decision 2026-10-28): a staff-only action needs a second factor on the acting account. An action is
// staff-only when its Roles are a non-empty subset of {moderator, admin}; a route a buyer or vendor may also post
// (Roles naming them, or none) is not gated even when staff post it. The enrolled factors are the stored ones,
// users.totp_enabled or users.pgp_2fa: the strict PGP sign-in policy alone does not count, because it can be
// switched off without the account changing and a session started before it was switched on had only the password;
// under that policy turning PGP sign-in on is one confirmed form. Enrolment (/totp/*, /pgp/*), /account, the password
// change, sign-out, page views and the host commands are not staff-only and stay available, so a new or reset
// administrator can enrol and then act.

// staffFactorRequiredMessage is the refusal; refuse links it to /totp and /pgp (RefusalView.EnrolFactor).
const staffFactorRequiredMessage = "Staff actions need a second factor on your account. Turn on an authenticator (TOTP) or PGP sign-in (saving a new PGP key turns PGP sign-in off until you turn it back on), then submit the form again. Nothing was changed."

var errStaffFactorRequired = fail(http.StatusForbidden, staffFactorRequiredMessage)

// grantsStaffRole reports whether changing an account from old to role gives it staff powers it did not have:
// becoming a moderator from outside staff, or an administrator. A-706 (SEC706-2) refuses that for an account with
// neither TOTP nor PGP sign-in, so a phished password cannot enrol the factor for a newly appointed staff account.
// Leaving staff and an administrator becoming a moderator stay allowed.
func grantsStaffRole(old, role string) bool {
	return role == "admin" && old != "admin" || role == "moderator" && old != "moderator" && old != "admin"
}

// endPendingSignInsBeforeFirstFactor is a confirmation.Before for turning on a second factor. When the account has
// none yet it ends the account's pending sign-ins, before the account row is locked, in the order /challenge takes
// them (A-154). A concurrent first factor makes this a harmless extra end of sign-ins in progress.
func endPendingSignInsBeforeFirstFactor(c *actionCtx) error {
	var none bool
	if err := c.Tx.QueryRowContext(c.Ctx(), "SELECT NOT (totp_enabled OR pgp_2fa) FROM users WHERE id=$1", c.User.ID).Scan(&none); err != nil || !none {
		return err
	}
	_, err := c.Tx.ExecContext(c.Ctx(), "DELETE FROM pending_logins WHERE user_id=$1", c.User.ID)
	return err
}

// endOtherSessionsForFirstFactor runs, under the account lock, when a change turns on the account's first second
// factor (SEC706-3): every other session of the account, including one a phisher started with the password alone,
// ends, and so do pending sign-ins, as on a password change. This browser's session stays: a confirmed form receipt
// cannot replay a rotated cookie.
func endOtherSessionsForFirstFactor(c *actionCtx) error {
	ctx, tx, uid := c.Ctx(), c.Tx, c.User.ID
	if _, err := tx.ExecContext(ctx, "DELETE FROM pending_logins WHERE user_id=$1", uid); err != nil {
		return err
	}
	ended, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=$1 AND token_hash<>$2", uid, digest(c.Token))
	if err != nil {
		return err
	}
	n, _ := ended.RowsAffected()
	_, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", uid,
		"Ended "+strconv.FormatInt(n, 10)+" other session(s) and any pending sign-ins: first second factor turned on")
	return err
}

func staffOnlyRoles(roles []string) bool {
	return len(roles) > 0 && !slices.ContainsFunc(roles, func(role string) bool { return role != "admin" && role != "moderator" })
}
