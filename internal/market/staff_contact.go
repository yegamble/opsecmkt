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

// A-119: the administrator's "Staff contact" (settings key staff_contact; a handle, onion address or XMPP address,
// free text) is the path behind every "contact the market staff". It is shown as text, never as a link, on /canary,
// in the suspended sign-in refusal, as the lost-factors line on /challenge, under a notification that asks its
// reader to contact the staff (the payout-address change notice among them), as "Can't read the image?" help beside
// the CAPTCHA on /login and /register (with, A-517, what to do without the image), otherwise (A-703) as "Locked out
// or reporting a stolen account?" on /login and the closed /register (a members-only /canary redirects a signed-out
// reader to /login, so this is the place they can always reach), in the "Selling needs an
// administrator to give your account the vendor role" line on /register, in the expired-, unreadable- and (strict
// policy) missing-key sign-in refusals (pgpKeyRefusal, strictKeyRefusal), and (A-279)
// with a taken-down listing on the vendor desk, its edit page and its restore refusal, with a photo the malware
// scanner flagged, and on /totp after a sign-in with the last recovery code. When it is exactly
// the handle of a moderator or administrator who is not suspended and whose key can receive encrypted messages
// (canReceiveEncrypted, A-83), a signed-in reader also gets a /messages?to=<handle> link; a signed-out reader could
// not use it. Unset, each place says "Ask the operator of this market." instead, so no page implies a channel that
// does not exist. A-766: a bare handle with no active staff account (demoted, closed, renamed then re-registered, never
// registered) reads as unset for every reader, because such a handle is no one's staff contact, though the vendor rules
// may still let a buyer message it; the setting stays as saved. A-734: a staff handle whose key cannot receive encrypted
// messages is named as text with no promise to message it. /admin/site and its overview alert say what is wrong with
// either (staffContactHealth).

const (
	settingStaffContact  = "staff_contact"
	staffContactMaxRunes = 200
	askOperator          = "Ask the operator of this market."
)

// A-734: the name of each staff-contact warning; the overview shows it alone, /admin/site with what to do.
const (
	staffContactWarnEmpty    = "No staff contact is set."
	staffContactWarnNotStaff = "The staff contact is a handle that is not an active moderator or administrator."
	staffContactWarnNoKey    = "The staff contact's account has no PGP key that can receive encrypted messages."
	staffContactWarnClosed   = "The staff contact is a bare handle while new registrations are closed."
)

func init() {
	registerAction("/admin/staff-contact", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: staffContactAction})
	registerLoader("admin", captchaContactLoader)
	registerLoader("admin", staffContactHealthLoader)
	for _, page := range []string{"canary", "challenge", "login", "register", "notifications", "vendor-dashboard", "listing-edit", "account", "order"} {
		registerLoader(page, staffContactLoader)
	}
}

// staffContactSentence is the plain-text form for a refusal: "Contact the market staff: <contact>." or, unset,
// askOperator. contact is what readers may see (staffContactShown), not the raw setting.
func staffContactSentence(contact string) string {
	if contact == "" {
		return askOperator
	}
	return "Contact the market staff: " + contact + "."
}

// A-702: a signed-out reader is usually locked out of an account and writing to staff to get back in. When the contact
// is a bare handle they can still reach it from a new account; and they are told what staff will ask for, and what
// staff never ask for, so a phishing "staff member" asking for a password or private key is recognisable. A-703: while
// registrations are closed there is no new account to message it from, so they are told that instead. A-734: neither
// line is said of a staff handle whose key cannot receive encrypted messages, and nothing says why: the reader has the
// public contact and no more.
const (
	staffContactNewAccount = "That is a market handle: you can message it from a new account once that account has a PGP key."
	staffContactNoAccount  = "That is a market handle, and new registrations are closed, so only an existing account can message it."
	staffContactReady      = "Have ready your account's handle, your PGP key fingerprint and, if your key expired or was lost, your renewed or replacement public key. Staff never ask for your password, recovery codes or private key."
)

// staffContactSignedOut is staffContactSentence for a reader who is not signed in: the sign-in refusals, and /challenge,
// /login and the closed /register (the "staff-contact" partial followed by PageData.StaffContactSignedOut).
func staffContactSignedOut(contact string, registrationOpen, unreachable bool) string {
	return staffContactSentence(contact) + " " + staffContactSignedOutLines(contact, registrationOpen, unreachable)
}

func staffContactSignedOutLines(contact string, registrationOpen, unreachable bool) string {
	switch {
	case !staffContactNeedsAccount(contact) || unreachable:
		return staffContactReady
	case registrationOpen:
		return staffContactNewAccount + " " + staffContactReady
	}
	return staffContactNoAccount + " " + staffContactReady
}

// StaffContactSignedOut is what a page for a signed-out reader adds after the "staff-contact" partial (A-702). The
// read-only preview has no site-access policy and is taken as open.
func (d PageData) StaffContactSignedOut() string {
	open := d.Access == nil || d.Access.RegistrationOpen
	if d.StaffContact == nil {
		return staffContactSignedOutLines("", open, false)
	}
	return staffContactSignedOutLines(d.StaffContact.Text, open, d.StaffContact.Unreachable)
}

// staffContactSetting reads the setting through q (the caller's transaction, when one is open); "" while unset.
func staffContactSetting(ctx context.Context, q rowQuerier) (string, error) {
	var contact string
	err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", settingStaffContact).Scan(&contact)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	return contact, nil
}

// staffContactRefusal is the staff-contact sentence for a refusal to a signed-in reader ("" when it cannot be read).
func staffContactRefusal(ctx context.Context, q rowQuerier) (string, error) {
	contact, err := staffContactSetting(ctx, q)
	if err != nil {
		return "", err
	}
	return staffContactReaderSentence(ctx, q, contact)
}

// staffContactReaderSentence is staffContactSentence of what the setting's value contact shows to readers (A-766);
// "" with the error when that cannot be told, so a failed check never reads as a staff claim or as no contact.
func staffContactReaderSentence(ctx context.Context, q rowQuerier, contact string) (string, error) {
	if staffContactNeedsAccount(contact) {
		id, _, err := staffContactAccount(ctx, q, contact)
		if err != nil {
			return "", err
		}
		if id == "" {
			contact = ""
		}
	}
	return staffContactSentence(contact), nil
}

// signedOutStaffContactRefusal is the staff-contact lines for a sign-in refusal (staffContactSignedOut). A missing
// registrations row counts as closed, as registrationAllowedTx and siteAccessFromSettings read it.
func signedOutStaffContactRefusal(ctx context.Context, q rowQuerier) (string, error) {
	var contact string
	var open bool
	err := q.QueryRowContext(ctx, "SELECT COALESCE((SELECT value FROM settings WHERE key=$1),''), COALESCE((SELECT value FROM settings WHERE key=$2),'')='open'",
		settingStaffContact, settingRegistrations).Scan(&contact, &open)
	if err != nil {
		return "", err
	}
	unreachable := false
	if staffContactNeedsAccount(contact) {
		id, canReceive, err := staffContactAccount(ctx, q, contact)
		if err != nil {
			return "", err
		}
		if id == "" { // A-766: not staff, so it reads as unset
			contact = ""
		}
		unreachable = !canReceive
	}
	return staffContactSignedOut(contact, open, unreachable), nil
}

// suspendedRefusal refuses sign-in to an account an administrator suspended, or banned (A-546: closed, without the
// reason). It is only returned once the password is correct, so it reveals nothing to someone who does not know it, and
// a banned account that sells is told the policy for its payments (A-739, bannedSellerPolicy).
func suspendedRefusal(ctx context.Context, q rowQuerier, userID string, banned bool) error {
	contact, err := signedOutStaffContactRefusal(ctx, q)
	if err != nil {
		return err
	}
	if banned {
		policy, err := bannedSellerPolicy(ctx, q, userID)
		if err != nil {
			return err
		}
		return fail(403, "This account is closed and cannot sign in. "+policy+contact)
	}
	return fail(403, "This account is suspended. "+contact)
}

// bannedSellerPolicy is the sentence a banned account that sells reads after a correct password (A-739, owner decision),
// "" for any other: it sells when its role is vendor, it was approved to sell, or an order was placed on one of its listings
// (a ban makes a moderator a buyer, whose orders stay held). The sentence is the same for every such account, so it
// states no order, count or amount. The days are the period the held-release refund enforces (holdRefundDaysShown).
func bannedSellerPolicy(ctx context.Context, q rowQuerier, userID string) (string, error) {
	var sells bool
	if err := q.QueryRowContext(ctx, `SELECT u.role='vendor' OR u.vendor_approved OR EXISTS(SELECT 1 FROM orders o JOIN products p ON p.id=o.product_id WHERE p.vendor_id=u.id)
		FROM users u WHERE u.id=$1`, userID).Scan(&sells); err != nil {
		return "", err
	}
	if !sells {
		return "", nil
	}
	days, err := holdRefundDaysShown(ctx, q)
	if err != nil {
		return "", err
	}
	return "While it is closed, payments to this account not yet sent are held, with no other address paid in their place, and after " + strconv.Itoa(days) + " days held an administrator may refund a held release to the buyer. ", nil
}

// staffContactNeedsAccount: a contact that is exactly a handle can be messaged only from an account, so it is no
// way in for someone who cannot solve the CAPTCHA (A-517). Anything else (an onion or XMPP address, a sentence) is
// taken to work without one; the setting is free text and the application cannot tell more than that.
func staffContactNeedsAccount(contact string) bool { return handlePattern.MatchString(contact) }

// captchaContactLoader (admin page, A-517) sets d.CaptchaNoContact while the image is being shown on the public sign-in
// (A-725: forced, or in the default mode under the attack signal) and the staff contact is unset or a bare handle: a
// person who cannot read the image (it has no audio or text alternative) then has no way to ask anyone for help
// without first getting past it.
func captchaContactLoader(_ context.Context, a *App, _ *http.Request, d *PageData) error {
	contact := d.Settings[settingStaffContact]
	shown := a.captchaKindFor(context.Background(), captchaModeOf(d.Settings["captcha_required"]), false) == captchaKindImage
	d.CaptchaNoContact = shown && (contact == "" || staffContactNeedsAccount(contact))
	return nil
}

// staffContactAccount finds the staff member a bare-handle contact names, as /messages would reach them: an active
// moderator or administrator (messageRecipient; a ban always also sets suspended_at) and whether their key can receive
// encrypted messages (inspectMessage's 409). id is "" when the handle is not active staff.
func staffContactAccount(ctx context.Context, q rowQuerier, handle string) (id string, canReceive bool, err error) {
	err = q.QueryRowContext(ctx, "SELECT id FROM users WHERE handle=$1 AND role IN ('moderator','admin') AND suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL", handle).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	canReceive, err = userCanReceiveEncrypted(ctx, q, id)
	return id, canReceive, err
}

// staffContactHealth is every warning the administrator should see about the contact (A-734). It only checks what a
// reader would need: a bare handle has to be messageable, and is no way in for someone locked out of a closed market.
// Other free text is taken to work, as staffContactNeedsAccount does.
func staffContactHealth(ctx context.Context, q rowQuerier, contact string, registrationOpen bool) ([]StaffContactWarning, error) {
	if contact == "" {
		return []StaffContactWarning{{staffContactWarnEmpty, "Every place that names it says “" + askOperator + "”, so someone locked out has no one to write to. Save a handle, onion address or XMPP address below."}}, nil
	}
	if !staffContactNeedsAccount(contact) {
		return nil, nil
	}
	var warnings []StaffContactWarning
	id, canReceive, err := staffContactAccount(ctx, q, contact)
	switch {
	case err != nil:
		return nil, err
	case id == "":
		warnings = append(warnings, StaffContactWarning{staffContactWarnNotStaff, "Every place that names the contact says “" + askOperator + "” instead, without this handle, so someone locked out has no one to write to. Save the handle of an active staff account, or another address."})
	case !canReceive:
		warnings = append(warnings, StaffContactWarning{staffContactWarnNoKey, "A message to it is refused (no key saved, or the saved one is unreadable, expired or revoked, or has no current encryption subkey), so the sign-in page does not offer to message it. Ask that account to save a key that can, or save another address."})
	}
	if !registrationOpen {
		warnings = append(warnings, StaffContactWarning{staffContactWarnClosed, "Only an existing account can message a handle, so someone locked out of their account has no way to reach it. Save an onion or XMPP address, or open registrations under Security & access."})
	}
	return warnings, nil
}

// staffContactHealthLoader (admin page, A-734) sets d.StaffContactWarnings on the two views that show them: /admin/site
// and its alert on the overview. The check runs on each view, so a warning stays until the cause is fixed, wherever it
// was fixed (a moderator's new key, a suspension lifted, registrations opened).
func staffContactHealthLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) (err error) {
	if d.AdminSection == "" || d.AdminSection == "site" {
		d.StaffContactWarnings, err = staffContactHealth(ctx, a.db, d.Settings[settingStaffContact], d.Settings[settingRegistrations] == "open")
	}
	return err
}

// staffContactLoader sets d.StaffContact from the setting (nil while unset, and A-766 for a bare handle that is not
// active staff; the "staff-contact" partial then says askOperator), whether a message can reach a bare handle and, for a
// signed-in reader other than that staff member, the handle to link. An error fails the page, naming no one.
func staffContactLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	contact := d.Settings[settingStaffContact]
	if contact == "" {
		return nil
	}
	if !staffContactNeedsAccount(contact) {
		d.StaffContact = &StaffContactView{Text: contact}
		return nil
	}
	id, canReceive, err := staffContactAccount(ctx, a.db, contact)
	if err != nil || id == "" {
		return err
	}
	d.StaffContact = &StaffContactView{Text: contact, NeedsAccount: true, Unreachable: !canReceive}
	if canReceive && d.User != nil && d.User.ID != id {
		d.StaffContact.Handle = contact
	}
	return nil
}

// AsksForStaff reports a notification that tells its reader to contact the market staff (the payout-address change
// notice among them); /notifications shows the staff contact under it.
func (n Notification) AsksForStaff() bool {
	return strings.Contains(strings.ToLower(n.Body), "contact the market staff")
}

// cleanStaffContact checks the submitted contact: surrounding space trimmed, at most staffContactMaxRunes
// characters, valid UTF-8, and only printable characters and spaces (no line break, control or bidirectional
// formatting character). Empty clears the setting.
func cleanStaffContact(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if !utf8.ValidString(v) || strings.IndexFunc(v, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
		return "", fail(400, "Staff contact not saved: write it on one line, with no control or text-direction characters.")
	}
	if utf8.RuneCountInString(v) > staffContactMaxRunes {
		return "", fail(400, "Staff contact not saved: use at most 200 characters.")
	}
	return v, nil
}

// staffContactAction saves the staff contact. The administrator confirms with their password (and authenticator
// code when enrolled); the change is audited old→new. Saving the value already in force changes nothing (409).
func staffContactAction(c *actionCtx) (actionResult, error) {
	want, err := cleanStaffContact(c.Form.Get(settingStaffContact))
	if err != nil {
		return actionResult{}, err
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		// The row is created first so that two first saves serialise on it: FOR UPDATE locks nothing that is absent.
		if _, err := c.Tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,'') ON CONFLICT(key) DO NOTHING", settingStaffContact); err != nil {
			return actionResult{}, err
		}
		var old string
		if err := c.Tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1 FOR UPDATE", settingStaffContact).Scan(&old); err != nil {
			return actionResult{}, err
		}
		if old == want {
			return actionResult{}, fail(409, "The staff contact is already that; nothing was changed.")
		}
		if err := setSetting(c, settingStaffContact, want); err != nil {
			return actionResult{}, err
		}
		quoted := func(v string) string {
			if v == "" {
				return "(not set)"
			}
			return "“" + v + "”"
		}
		return actionResult{Redirect: "/admin/site", Notice: "staff-contact", Audit: "Staff contact changed from " + quoted(old) + " to " + quoted(want)}, nil
	})
}
