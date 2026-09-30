package market

import (
	"net/http"
	"slices"
	"strings"
)

// One-shot notices (A-117). A successful signed-in form post names a code (actionResult.Notice); its 303 sets the
// `notice` cookie to that code, and the next page render shows the code's text from this fixed table and clears the
// cookie, so a reload, Back or a crafted link never shows a success that did not just happen. The cookie holds only
// a code, never user text; a code not in the table shows nothing. A notice with a section renders inside that
// section, and the 303 goes to its fragment; one without renders at the top of the page, with no fragment. pages
// names the pages (PageData.Page, space-separated) that render the section: a notice consumed on any other page
// (its 303 left unfollowed while another tab loaded, A-291) renders at the top there instead.
const noticeCookie, noticeMaxAge = "notice", 60

var notices = map[string]struct{ text, section, pages string }{
	// Account and sign-in
	"profile":        {"Profile saved.", "", ""},
	"password":       {"Password changed.", "", ""},
	"signed-out":     {"Signed out.", "", ""},
	"signed-out-all": {"Signed out everywhere: every session of this account has ended.", "", ""},
	"payout-address": {"Payout address saved.", "", ""},
	"pgp-verified":   {"PGP key ownership verified.", "", ""},
	"pgp-2fa-on":     {"PGP sign-in verification turned on.", "", ""},
	"pgp-2fa-off":    {"PGP sign-in verification turned off.", "", ""},
	"my-staff-list":  {"Staff list choice saved.", "staff-list", "account"},
	// Messages, orders and disputes
	"message-sent":     {"Message sent.", "", ""},
	"payment-address":  {"Payment address ready.", "", ""},
	"order-cancelled":  {"Order cancelled.", "", ""},
	"order-shipped":    {"Order marked shipped.", "", ""},
	"order-delivered":  {"Digital content delivered.", "", ""},
	"order-completed":  {"Order completed.", "", ""},
	"review-saved":     {"Review saved.", "", ""},
	"dispute-opened":   {"Dispute opened.", "", ""},
	"dispute-reply":    {"Reply added to the dispute.", "", ""},
	"dispute-resolved": {"Dispute resolved.", "", ""},
	"payment-review":   {"Payment review recorded.", "payment-review", "moderator"},
	// Listings
	"listing-created":    {"Listing created. Add photos here.", "photos", "listing-edit"},
	"listing-saved":      {"Listing saved.", "", ""},
	"listing-archived":   {"Listing archived.", "", ""},
	"listing-restored":   {"Listing restored.", "", ""},
	"listing-taken-down": {"Listing taken down.", "", ""},
	"review-hidden":      {"Review hidden.", "reviews-title", "product vendor"},
	"review-shown":       {"Review restored.", "reviews-title", "product vendor"},
	// Administration
	"settings":         {"Marketplace settings saved.", "", ""},
	"role":             {"Account role saved.", "", ""},
	"captcha":          {"CAPTCHA setting saved.", "", ""},
	"suspended":        {"Account suspended.", "suspend", "admin"},
	"restored":         {"Account restored.", "suspend", "admin"},
	"factors-reset":    {"Second factors reset.", "reset-factors", "admin"},
	"staff-contact":    {"Staff contact saved.", "staff-contact", "admin"},
	"staff-list":       {"Public staff list saved.", "staff-list", "admin"},
	"intake-enabled":   {"New payment addresses enabled.", "payment-intake", "admin"},
	"intake-paused":    {"New payment addresses paused.", "payment-intake", "admin"},
	"payout-released":  {"Payout released.", "payouts", "admin"},
	"payout-requeued":  {"Payout queued again.", "payouts", "admin"},
	"payout-sent":      {"Payout marked sent.", "payouts", "admin"},
	"payout-repointed": {"Payout moved to the current payout address.", "payouts", "admin"},
	"operator-key":     {"Operator PGP key saved.", "transparency", "admin"},
}

// setNotice sets the one-shot notice for code on a successful signed-in post's 303 and returns redirect with the
// notice's section as its fragment. An unknown code sets nothing.
func (a *App) setNotice(w http.ResponseWriter, code, redirect string) string {
	n, ok := notices[code]
	if !ok {
		return redirect
	}
	a.cookie(w, noticeCookie, code, noticeMaxAge)
	if n.section != "" {
		redirect += "#" + n.section
	}
	return redirect
}

// takeNotice reads and clears the request's notice cookie and fills d.Notice and d.NoticeAt from the table.
// Cleared before the page loads, so a render that fails still consumes it. A request without the cookie (a
// cross-site landing withholds it, A-48) sets no cookie.
func (a *App) takeNotice(w http.ResponseWriter, r *http.Request, d *PageData) {
	c, err := r.Cookie(noticeCookie)
	if err != nil {
		return
	}
	a.cookie(w, noticeCookie, "", -1)
	if n, ok := notices[c.Value]; ok {
		d.Notice, d.NoticeAt = n.text, n.section
		if !slices.Contains(strings.Fields(n.pages), d.Page) {
			d.NoticeAt = ""
		}
	}
}

// NoticeIn is the notice to show inside the section with id (templates: {{with $.NoticeIn "suspend"}}).
func (d PageData) NoticeIn(id string) string {
	if d.NoticeAt == id {
		return d.Notice
	}
	return ""
}
