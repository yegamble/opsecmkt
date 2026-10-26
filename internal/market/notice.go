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
// names the pages (PageData.Page, space-separated; admin/<slug> for an administration section, A-542, and admin/user
// for an account's page in the user directory, A-545) that render the section: a notice consumed on any other page
// (its 303 left unfollowed while another tab loaded, A-291) renders at the top there instead. A code in
// noticeErrors reports a success that needs the owner's attention, or (signin-code-wrong, A-549) a sign-in that
// moved on to /challenge because its authenticator code was refused, and renders as the page's error alert.
//
// A-287 moved the other outcome flags here (?photo=, ?avatar=, ?image=removed, ?key= and ?signin=). The query
// parameters pages still read select what to show, not what just happened: id, q, category, region, currency and
// page (the listing, search and paging), to (the message recipient), next (where signing in returns, A-126; on
// /totp shown only beside a signin- notice, totpLoader), scanner (the preview's upload and admin scanner states) and the audit
// export's range.
const noticeCookie, noticeMaxAge = "notice", 60

var notices = map[string]struct{ text, section, pages string }{
	"retention-delete":  {"Account deletion requested. Save any records you need before the 30-day deadline.", "retention", "account"},
	"retention-hold":    {"Preservation hold recorded. Review is due within 30 days.", "retention", "admin/system"},
	"retention-release": {"Preservation hold released. Original retention deadlines apply.", "retention", "admin/system"},
	"retention-review":  {"Financial retention blocker reviewed. No funds or recovery records were deleted.", "retention", "admin/system"},
	// Account and sign-in
	"profile":        {"Profile saved.", "", ""},
	"password":       {"Password changed.", "", ""},
	"signed-out":     {"Signed out.", "", ""},
	"signed-out-all": {"Signed out everywhere: every session of this account has ended.", "", ""},
	"pgp-verified":   {"PGP key ownership verified.", "", ""},
	"pgp-2fa-on":     {"PGP sign-in verification turned on.", "", ""},
	"pgp-2fa-off":    {"PGP sign-in verification turned off.", "", ""},
	"my-staff-list":  {"Staff list choice saved.", "staff-list", "account"},
	"avatar-saved":   {"Avatar saved as a re-encoded JPEG; location, camera and other metadata were removed. Check the picture itself for anything that identifies you.", "avatar", "account"},
	"avatar-removed": {"Avatar removed.", "avatar", "account"},
	// A new version of the saved key (renewProfileKey, A-155): kept its ownership proof, renewed an unverified key,
	// or saved as a changed key (an error alert naming why).
	"key-renewed-kept": {"Key updated: the same key (same fingerprint) with new signatures, such as a renewed expiry; its ownership proof and PGP sign-in were kept.", "", ""},
	"key-renewed":      {"Key updated: the same key (same fingerprint) with new signatures, such as a renewed expiry.", "", ""},
	"key-revocation":   {keyChangedAlert("revocation"), "", ""},
	"key-subkey":       {keyChangedAlert("subkey"), "", ""},
	// A recovery-code sign-in (/challenge, A-154): /totp details it from the current state (recoverySignInNotice).
	"signin-recovery": {"You signed in with a recovery code.", "", ""},
	"signin-totp-off": {"You signed in with a recovery code and turned off two-step sign-in with an authenticator app (TOTP); your other recovery codes were deleted.", "", ""},
	// A wrong, expired or reused authenticator code typed on /login with a correct password (A-549); an error alert.
	"signin-code-wrong": {"The authenticator code was not accepted: it was incorrect, expired or already used. Enter the current code from your app below, or use a recovery code.", "", ""},

	// Bitcoin escrow key (escrow_account.go)
	"escrow-key":           {"Escrow key saved. It is inactive until you sign its practice transaction.", "", ""},
	"escrow-key-unchanged": {"That is already your escrow key; nothing was changed.", "", ""},
	"escrow-key-active":    {"Escrow key active: its practice transaction was signed.", "", ""},
	"escrow-created":       {"Escrow created. Check the escrow descriptor in your own wallet before you pay.", "", ""},
	"escrow-claim":         {"Claim requested. The market builds the settlement transaction on its next pass; reload this page to sign it.", "", ""},
	"escrow-signed":        {"Signature accepted. The settlement is signed by you; the signature's validity is checked when the market finalizes the transaction. Nothing has been sent yet: the market co-signs and broadcasts it on its next pass.", "", ""},
	"escrow-signed-both":   {"Signature accepted. Both parties have now signed; the signature's validity is checked when the market finalizes the transaction. Nothing has been sent yet: the market does not sign it, and broadcasts the transaction the two parties signed on its next pass.", "", ""},
	"escrow-bump":          {"Fee bump requested. The market builds the new transaction on its next pass; reload this page to check its outputs and sign it.", "", ""},
	"escrow-bump-signed":   {"Signature accepted. The fee bump is signed by you; the signature's validity is checked when the market finalizes the transaction. Nothing has been sent yet: the market co-signs it and broadcasts it on its next pass, in place of the transaction already sent.", "", ""},
	"escrow-bump-both":     {"Signature accepted. Both parties have now signed the fee bump; the signature's validity is checked when the market finalizes the transaction. Nothing has been sent yet: the market does not sign it, and broadcasts it on its next pass, in place of the transaction already sent.", "", ""},
	"escrow-decision":      {"Dispute decision changed. The order page shows the settlement that pays it.", "", ""},
	"escrow-fee":           {"Market fee saved. It applies to escrows created from now on; existing escrows keep the fee they started with.", "escrow-fee", "admin/payments"},
	"escrow-release-days":  {"Release deadline saved. It applies to escrow orders placed from now on; existing orders keep theirs.", "escrow-release-days", "admin/payments"},
	"escrow-rebroadcast":   {"Rebroadcast confirmed. The market broadcasts the settlement's stored transaction once on its next pass.", "escrow-recovery", "admin/payments"},
	"escrow-xmr-gone":      {"Destruction confirmed. The wallet's sealed seed was wiped; delete its files from the escrow wallet volume if any are left.", "xmr-escrow-recovery", "admin/payments"},
	"escrow-xmr-relay":     {"Relay confirmed. The market relays the settlement's stored transaction once on its next pass.", "xmr-escrow-recovery", "admin/payments"},
	"escrow-xmr-created":   {"Monero escrow created. Send the amount shown below to its escrow address.", "", ""},
	"escrow-xmr-claim":     {"Claim requested. The market builds the release from this order's escrow account and sends it; nothing is sent yet.", "", ""},
	"escrow-xmr-confirmed": {"Receipt confirmed. The order is completed; the market builds the release of the coins in this escrow to the vendor and sends it.", "", ""},
	// A-652d: the vendor is banned, so the release is held (xmrReleaseHeld).
	"escrow-xmr-held":      {"Receipt confirmed. The order is completed. The vendor's account is closed, so the market holds the release of the coins in this escrow and sends nothing while the account is banned; no other address is ever paid.", "", ""},
	"escrow-xmr-retry":     {"Recorded. The market builds this settlement on its next pass; reload this page in a minute.", "", ""},
	"escrow-xmr-rerequest": {"Re-request recorded. On its next pass the market checks that the stored transaction can no longer be mined, then builds the settlement again; reload this page in a minute.", "", ""},
	"escrow-recover":       {"Asked. On its next pass the market reads this escrow and requests a stray refund of what it finds to your refund address; coins too few to pay the network fee stay held, and this page then says so. Reload this page in a minute.", "", ""},

	// Messages, orders and disputes
	"message-sent":     {"Message sent.", "", ""},
	"order-cancelled":  {"Order cancelled.", "", ""},
	"order-shipped":    {"Order marked shipped.", "", ""},
	"order-delivered":  {"Digital content delivered.", "", ""},
	"order-completed":  {"Order completed.", "", ""},
	"review-saved":     {"Review saved.", "", ""},
	"dispute-opened":   {"Dispute opened.", "", ""},
	"dispute-reply":    {"Reply added to the dispute.", "", ""},
	"dispute-resolved": {"Dispute resolved.", "", ""},
	"dispute-conceded": {"Dispute conceded. Check the order and payout or escrow settlement below.", "", ""},
	"payment-review":   {"Payment review recorded.", "payment-review", "moderator"},
	"xmr-collision":    {"Recorded what was done about the refused Monero deposit.", "", ""},
	// A-613: shown on the moderation desk, because the order may no longer be readable once its escrow alert clears.
	"xmr-settled-by-hand": {"Recorded. The failed Monero escrow settlement's escrow alert is cleared; the market sends nothing for it.", "", ""},
	// Listings
	"listing-created":    {"Listing created. Add photos here.", "photos", "listing-edit"},
	"listing-saved":      {"Listing saved.", "", ""},
	"listing-archived":   {"Listing archived.", "", ""},
	"listing-restored":   {"Listing restored.", "", ""},
	"delivery-cleared":   {"Stored delivery text cleared. Existing orders and saved deliveries were not changed.", "", ""},
	"listing-taken-down": {"Listing taken down.", "", ""},
	"review-hidden":      {"Review hidden.", "reviews-title", "product vendor"},
	"review-shown":       {"Review restored.", "reviews-title", "product vendor"},
	"photo-saved":        {"Photo saved as a re-encoded JPEG; location, camera and other metadata were removed. Check the picture itself for anything that identifies you.", "photos", "listing-edit"},
	"evidence-saved":     {"Dispute evidence saved as a scanned, re-encoded JPEG with metadata removed.", "dispute-evidence", "order"},
	"photo-removed":      {"Photo removed.", "photos", "listing-edit"},
	// A staff takedown (image_takedown.go), back on /moderator's Recent photos or on the product or vendor page.
	"image-removed":      {"Image removed. Its owner was notified with your reason.", "recent-photos", "moderator"},
	"image-removed-page": {"Image removed. Its owner was notified with your reason.", "", ""},

	// MOD72-6 hold mode (listing_review.go): the vendor's listing waits for staff review; staff review it on /moderator.
	"listing-created-held": {"Listing created. It waits for staff review: buyers cannot see it until a moderator or administrator approves it. Add photos here.", "photos", "listing-edit"},
	"listing-saved-held":   {"Listing saved. It waits for staff review again: buyers cannot see it until a moderator or administrator approves it. Existing orders are not changed.", "", ""},
	"listing-approved":     {"Listing approved. It is visible in the marketplace; the vendor was notified.", "listing-review", "moderator"},
	"listing-rejected":     {"Listing rejected. It stays hidden until the vendor edits it and it is reviewed again; the vendor was notified with your reason.", "listing-review", "moderator"},
	"listing-review-mode":  {"Listing review saved. Changing it approves nothing: listings already waiting for review still need a moderator or administrator.", "listing-review", "admin/vendors"},
	"vendor-auto-approve":  {"Listing review for this vendor saved. The vendor was notified.", "vendors", "admin/vendors admin/user"},

	// Administration
	"settings":      {"Marketplace settings saved.", "", ""},
	"access":        {"Site access and registration settings saved.", "access", "admin/security"},
	"role":          {"Account role saved.", "", ""},
	"captcha":       {"CAPTCHA setting saved.", "", ""},
	"suspended":     {"Account suspended.", "suspend", "admin/users admin/user"},
	"restored":      {"Account restored.", "suspend", "admin/users admin/user"},
	"banned":        {"Account banned.", "ban", "admin/users admin/user"},
	"unbanned":      {"Ban lifted.", "ban", "admin/users admin/user"},
	"ban-orders":    {"Orders cancelled. The market builds each refund to the buyer's refund address and sends it on its next pass. An order still offered below changed meanwhile or waits for another submission.", "ban-orders", "admin/user"},
	"factors-reset": {"Second factors reset.", "reset-factors", "admin/users admin/user"},
	"staff-contact": {"Staff contact saved.", "staff-contact", "admin/site"},
	"staff-list":    {"Public staff list saved.", "staff-list", "admin/transparency"},
	"intake-paused": {"New payment addresses paused.", "payment-intake", "admin/payments"},
	"intake-escrow": {"New payments set to escrow only.", "payment-intake", "admin/payments"},
	"operator-key":  {"Operator PGP key saved.", "transparency", "admin/transparency"},
	// Vendor onboarding (vendor_applications.go, A-544)
	"vendor-applied":   {"Vendor application submitted. Its status is shown below.", "vendor-application", "account"},
	"vendor-approved":  {"Vendor application approved. Selling access is available from the vendor desk.", "vendor-application", "account"},
	"vendor-revoked":   {"Selling approval removed. Active listings were archived; existing orders and any moderator role remain available.", "vendors", "admin/vendors admin/user"},
	"vendor-withdrawn": {"Vendor application withdrawn.", "vendor-application", "account"},
	"vendor-policy":    {"Vendor policy saved. It applies to new applications; open applications keep their bond and approval mode.", "vendors", "admin/vendors"},
	"vendor-decision":  {"Vendor application updated. The applicant was notified.", "vendors", "admin/vendors admin/user"},
	"vendor-verify":    {"Verified vendor mark saved. The vendor was notified.", "vendors", "admin/vendors admin/user"},
	// The user directory's account page (admin_users.go, A-545): an application opened on the account's behalf.
	"vendor-opened": {"Vendor application opened for this account. The account was notified.", "vendors", "admin/user"},
}

// noticeErrors are the codes shown as the page's error alert rather than a status notice.
var noticeErrors = map[string]bool{"key-revocation": true, "key-subkey": true, "signin-code-wrong": true}

// setNotice sets the one-shot notice for code on a successful signed-in post's 303 (an action's, an upload's, or
// the recovery-code sign-in's with the session it starts) and returns redirect with the notice's section as its
// fragment. An unknown code sets nothing.
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
		d.noticeCode = c.Value
		if noticeErrors[c.Value] {
			d.Error = n.text
			return
		}
		d.Notice, d.NoticeAt = n.text, n.section
		if !slices.Contains(strings.Fields(n.pages), d.noticePage()) {
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
