package market

import (
	"net/http"
	"slices"
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
const staffFactorRequiredMessage = "Staff actions need a second factor on your account. Turn on an authenticator (TOTP) or PGP sign-in, then submit the form again. Nothing was changed."

var errStaffFactorRequired = fail(http.StatusForbidden, staffFactorRequiredMessage)

func staffOnlyRoles(roles []string) bool {
	return len(roles) > 0 && !slices.ContainsFunc(roles, func(role string) bool { return role != "admin" && role != "moderator" })
}
