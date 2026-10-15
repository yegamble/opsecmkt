package market

import (
	"html"
	"strings"
	"testing"
)

const (
	captchaWarningMark    = "captcha-contact-warning"
	captchaWarningUnset   = "the CAPTCHA is on, but no staff contact is set."
	captchaWarningHandle  = "the CAPTCHA is on, but the staff contact is only a handle, which can be messaged only from an account."
	captchaFallbackNone   = "This market lists no contact that works without an account, so there is no way to get one without the image. Only the operator of this market can turn the CAPTCHA off."
	captchaFallbackHandle = "That handle can be messaged only from an account, which needs this image first, so it does not help here. Only the operator of this market can turn the CAPTCHA off."
	captchaFallbackOnion  = "You do not need an account to write to that contact: ask the staff to have the market’s operator turn the CAPTCHA off, since accounts are made on this page and there is no other way in."
)

func TestAdminWarnsWhenCaptchaHasNoAccountFreeContact(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("capwarn_admin", "admin")
	setting := func(key, value string) {
		agExec(e, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	}
	adminPage := func() string { return html.UnescapeString(e.body("GET", "/admin", admin, nil, 200)) }

	// CAPTCHA off (the test default): nothing to warn about, whatever the contact.
	mustNotContain(t, adminPage(), captchaWarningMark)

	setting("captcha_required", "true")
	page := adminPage()
	mustContain(t, page, captchaWarningMark, captchaWarningUnset, "turning the CAPTCHA off here")
	if strings.Count(page, captchaWarningMark) != 1 {
		t.Errorf("the warning is shown %d times", strings.Count(page, captchaWarningMark))
	}

	setting("staff_contact", "capwarn_mod")
	mustContain(t, adminPage(), captchaWarningMark, captchaWarningHandle)

	setting("staff_contact", staffContactValue)
	mustNotContain(t, adminPage(), captchaWarningMark)

	setting("staff_contact", "")
	mustContain(t, adminPage(), captchaWarningMark, captchaWarningUnset)

	setting("captcha_required", "false")
	mustNotContain(t, adminPage(), captchaWarningMark)
}

func TestCaptchaFallbackCopyNamesTheWayInOrSaysThereIsNone(t *testing.T) {
	e := newTestApp(t)
	e.user("capcopy_mod", "moderator") // any account: with none, every page redirects to /setup
	agExec(e, "UPDATE settings SET value='true' WHERE key='captcha_required'")
	cases := []struct {
		name, contact, want string
		reject              []string
	}{
		{"unset", "", captchaFallbackNone, []string{captchaFallbackHandle, captchaFallbackOnion}},
		{"handle", "capcopy_mod", captchaFallbackHandle, []string{captchaFallbackNone, captchaFallbackOnion}},
		{"onion", staffContactValue, captchaFallbackOnion, []string{captchaFallbackNone, captchaFallbackHandle}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agExec(e, "INSERT INTO settings(key,value) VALUES('staff_contact',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value", c.contact)
			for _, path := range []string{"/login", "/register"} {
				page := html.UnescapeString(e.body("GET", path, "", nil, 200))
				mustContain(t, page, c.want, captchaHelpLine)
				mustNotContain(t, page, c.reject...)
				if c.contact != "" {
					mustContain(t, page, "Contact the market staff: "+c.contact+".")
				}
			}
		})
	}
	// Without the CAPTCHA there is no fallback copy.
	agExec(e, "UPDATE settings SET value='false' WHERE key='captcha_required'")
	mustNotContain(t, e.body("GET", "/register", "", nil, 200), "captcha-fallback")
}

func TestRegisterSaysSellingNeedsTheVendorRole(t *testing.T) {
	e := newTestApp(t)
	e.user("regsell_buyer", "buyer")
	const sentence = "Selling needs an administrator to give your account the vendor role. "
	page := html.UnescapeString(e.body("GET", "/register", "", nil, 200))
	mustContain(t, page, sentence+askOperatorLine)
	if strings.Count(page, "Selling needs an administrator") != 1 {
		t.Errorf("register repeats the vendor-role sentence")
	}
	agExec(e, "INSERT INTO settings(key,value) VALUES('staff_contact',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value", staffContactValue)
	mustContain(t, html.UnescapeString(e.body("GET", "/register", "", nil, 200)), sentence+"Contact the market staff: "+staffContactValue+".")
}

func TestLayoutDescriptionStatesTheMechanism(t *testing.T) {
	e := newTestApp(t)
	e.user("meta_buyer", "buyer")
	page := html.UnescapeString(e.body("GET", "/login", "", nil, 200))
	mustContain(t, page, `<meta name="description" content="Test-network marketplace with PGP-based accounts and messages and no browser JavaScript.">`)
	mustNotContain(t, page, "privacy-minded")
}
