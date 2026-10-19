package market

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A-119: the administrator's "Staff contact" (settings key staff_contact; a handle, onion address or XMPP address,
// free text) is the path behind every "contact the market staff". It is shown as text, never as a link, on /canary,
// in the suspended sign-in refusal, as the lost-factors line on /challenge, under a notification that asks its
// reader to contact the staff (the payout-address change notice among them), as "Can't read the image?" help beside
// the CAPTCHA on /login and /register (with, A-517, what to do without the image), in the "Selling needs an
// administrator to give your account the vendor role" line on /register, in the expired- and unreadable-key sign-in refusals (pgpKeyRefusal), and (A-279)
// with a taken-down listing on the vendor desk, its edit page and its restore refusal, with a photo the malware
// scanner flagged, and on /totp after a sign-in with the last recovery code. When it is exactly
// the handle of a moderator or administrator who is not suspended and whose key can receive encrypted messages
// (canReceiveEncrypted, A-83), a signed-in reader also gets a /messages?to=<handle> link; a signed-out reader could
// not use it. Unset, each place says "Ask the operator of this market." instead, so no page implies a channel that
// does not exist.

const (
	settingStaffContact  = "staff_contact"
	staffContactMaxRunes = 200
	askOperator          = "Ask the operator of this market."
)

func init() {
	registerAction("/admin/staff-contact", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: staffContactAction})
	registerLoader("admin", captchaContactLoader)
	for _, page := range []string{"canary", "challenge", "login", "register", "notifications", "vendor-dashboard", "listing-edit", "account", "order"} {
		registerLoader(page, staffContactLoader)
	}
}

// staffContactSentence is the plain-text form for a refusal: "Contact the market staff: <contact>." or, unset,
// askOperator.
func staffContactSentence(contact string) string {
	if contact == "" {
		return askOperator
	}
	return "Contact the market staff: " + contact + "."
}

// staffContactRefusal reads the setting through q (the caller's transaction, when one is open) for a refusal.
func staffContactRefusal(ctx context.Context, q rowQuerier) (string, error) {
	var contact string
	err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", settingStaffContact).Scan(&contact)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	return staffContactSentence(contact), nil
}

// suspendedRefusal refuses sign-in to an account an administrator suspended. It is only returned once the password
// is correct, so it reveals nothing to someone who does not know it.
func suspendedRefusal(ctx context.Context, q rowQuerier) error {
	contact, err := staffContactRefusal(ctx, q)
	if err != nil {
		return err
	}
	return fail(403, "This account is suspended. "+contact)
}

// staffContactNeedsAccount: a contact that is exactly a handle can be messaged only from an account, so it is no
// way in for someone who cannot solve the CAPTCHA (A-517). Anything else (an onion or XMPP address, a sentence) is
// taken to work without one; the setting is free text and the application cannot tell more than that.
func staffContactNeedsAccount(contact string) bool { return handlePattern.MatchString(contact) }

// captchaContactLoader (admin page, A-517) sets d.CaptchaNoContact while the CAPTCHA is required and the staff
// contact is unset or a bare handle: a person who cannot read the image (it has no audio or text alternative) then
// has no way to ask anyone for help without first getting past it.
func captchaContactLoader(_ context.Context, _ *App, _ *http.Request, d *PageData) error {
	contact := d.Settings[settingStaffContact]
	d.CaptchaNoContact = captchaRequired(d.Settings) && (contact == "" || staffContactNeedsAccount(contact))
	return nil
}

// staffContactLoader sets d.StaffContact from the setting (nil while unset; the "staff-contact" partial then says
// askOperator) and, for a signed-in reader other than that staff member, the handle to link.
func staffContactLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	contact := d.Settings[settingStaffContact]
	if contact == "" {
		return nil
	}
	d.StaffContact = &StaffContactView{Text: contact, NeedsAccount: staffContactNeedsAccount(contact)}
	if d.User == nil || !handlePattern.MatchString(contact) {
		return nil
	}
	var id string
	err := a.db.QueryRowContext(ctx, "SELECT id FROM users WHERE handle=$1 AND role IN ('moderator','admin') AND suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL AND id<>$2", contact, d.User.ID).Scan(&id)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	ok, err := userCanReceiveEncrypted(ctx, a.db, id)
	if ok {
		d.StaffContact.Handle = contact
	}
	return err
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
