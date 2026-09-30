package market

import (
	"html"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A-119: the administrator's "Staff contact" setting gives every "contact the market staff" a path. It is set on
// /admin with the administrator's password, audited old→new, and shown escaped on /canary, in the suspended
// sign-in refusal, as the lost-factors line on /challenge, under notifications that ask the reader to contact the
// staff (the payout-address change notice among them), as "Can't read the image?" help beside the CAPTCHA and in
// the expired-key sign-in refusal; unset, each says "Ask the operator of this market." A staff handle whose key can
// receive encrypted messages adds a /messages?to=<handle> link for a signed-in reader.

const (
	askOperatorLine   = "Ask the operator of this market."
	lostFactorsLine   = "Lost your authenticator and recovery codes"
	captchaHelpLine   = "Can’t read the image?"
	staffContactValue = "xmpp:help@staff-example.onion"
)

func staffContactForm(value string, extra ...string) url.Values {
	return form(append([]string{"staff_contact", value, "password", testPassword}, extra...)...)
}

func TestStaffContactSettingIsPasswordConfirmedAndAudited(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("sc_admin", "admin")
	_, mod := e.user("sc_mod", "moderator")
	setting := func() string {
		var v string
		e.DB.QueryRow("SELECT coalesce((SELECT value FROM settings WHERE key='staff_contact'),'')").Scan(&v)
		return v
	}

	f := adminForm(t, e.body("GET", "/admin", admin, nil, 200), "/admin/staff-contact", "")
	mustContain(t, f, `name="staff_contact"`, `maxlength="200"`, `name="password" required`)

	e.check(e.do("POST", "/admin/staff-contact", mod, staffContactForm(staffContactValue)), 403)
	agExpect(e, "/admin/staff-contact", admin, form("staff_contact", staffContactValue), 400, "Enter your current password")
	agExpect(e, "/admin/staff-contact", admin, form("staff_contact", staffContactValue, "password", "not-the-password-at-all"), 401, "Password incorrect")
	agExpect(e, "/admin/staff-contact", admin, staffContactForm(strings.Repeat("a", 201)), 400, "at most 200 characters")
	agExpect(e, "/admin/staff-contact", admin, staffContactForm("line one\nline two"), 400, "one line")
	agExpect(e, "/admin/staff-contact", admin, staffContactForm("right‮to left"), 400, "one line")
	if setting() != "" || e.auditHas(adminID, "Staff contact") {
		t.Fatal("a refused staff contact change was saved or audited")
	}

	w := e.do("POST", "/admin/staff-contact", admin, staffContactForm("  "+staffContactValue+"  "))
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/admin#staff-contact" {
		t.Fatalf("redirect %q", loc)
	}
	if setting() != staffContactValue || !e.auditExact(adminID, "Staff contact changed from (not set) to “"+staffContactValue+"” (confirmed with password)") {
		t.Fatal("staff contact not saved or not audited from (not set)")
	}
	agExpect(e, "/admin/staff-contact", admin, staffContactForm(staffContactValue), 409, "nothing was changed")
	long := strings.Repeat("é", 200) // 200 characters, 400 bytes
	e.check(e.do("POST", "/admin/staff-contact", admin, staffContactForm(long)), 303)
	if setting() != long || !e.auditExact(adminID, "Staff contact changed from “"+staffContactValue+"” to “"+long+"” (confirmed with password)") {
		t.Fatal("staff contact change not audited old→new")
	}
	mustContain(t, adminForm(t, e.body("GET", "/admin", admin, nil, 200), "/admin/staff-contact", ""), `value="`+long+`"`)
	e.check(e.do("POST", "/admin/staff-contact", admin, staffContactForm("")), 303)
	if setting() != "" || !e.auditExact(adminID, "Staff contact changed from “"+long+"” to (not set) (confirmed with password)") {
		t.Fatal("clearing the staff contact not audited")
	}
}

// TestStaffContactShownWherePromised: every promised place says "Ask the operator of this market." while the
// setting is unset and names the contact once it is set.
func TestStaffContactShownWherePromised(t *testing.T) {
	p := newPayEnv(t)
	e := p.testEnv
	suspendedID, _ := e.user("sc_suspended", "buyer")
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", suspendedID)
	totpID, _ := e.user("sc_totp", "buyer")
	agEnableTOTP(e, totpID)
	// The payout-address change notice (A-102) tells the owner to contact the market staff.
	p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"BTC"}, "address": {"fake-testnet-vendor"}, "password": {testPassword}}), 303)

	suspendedRefusal := func() string {
		w := e.do("POST", "/login", randomToken(), form("handle", "sc_suspended", "password", testPassword))
		if w.Code != 403 || e.cookie(w, "session") != "" || e.cookie(w, "pending") != "" {
			t.Fatalf("suspended sign-in: %d %q", w.Code, w.Body.String())
		}
		return strings.TrimSpace(w.Body.String())
	}
	challenge := func() string {
		anon, pc := e.pendingSignIn("sc_totp")
		return e.body("GET", "/challenge", anon, nil, 200, pc)
	}
	notice := func() string {
		b := html.UnescapeString(e.body("GET", "/notifications", p.vendorSess, nil, 200))
		i := strings.Index(b, "Your BTC payout address was changed")
		if i < 0 {
			t.Fatal("payout-address change notice missing")
		}
		b, _, _ = strings.Cut(b[i:], "</article>")
		return b
	}
	// The CAPTCHA help is shown with the CAPTCHA, which is on only while these pages are read (sign-in posts in
	// this test send no CAPTCHA answer).
	captchaPage := func(path string) string {
		agExec(e, "UPDATE settings SET value='true' WHERE key='captcha_required'")
		defer agExec(e, "UPDATE settings SET value='false' WHERE key='captcha_required'")
		return e.body("GET", path, "", nil, 200)
	}
	pages := func() map[string]string {
		return map[string]string{
			"/canary":    html.UnescapeString(e.body("GET", "/canary", "", nil, 200)),
			"/login":     html.UnescapeString(captchaPage("/login")),
			"/register":  html.UnescapeString(captchaPage("/register")),
			"/challenge": html.UnescapeString(challenge()),
			"notice":     notice(),
		}
	}

	if got := suspendedRefusal(); got != "This account is suspended. "+askOperatorLine {
		t.Fatalf("unset: suspended refusal %q", got)
	}
	for name, b := range pages() {
		mustContain(t, b, askOperatorLine)
		mustNotContain(t, b, "Contact the market staff:")
		switch name {
		case "/login", "/register":
			mustContain(t, b, captchaHelpLine+" "+askOperatorLine)
		case "/challenge":
			mustContain(t, b, lostFactorsLine+"? Only a market administrator can turn your second factors off. "+askOperatorLine)
		}
	}

	agExec(e, "INSERT INTO settings(key,value) VALUES('staff_contact',$1)", staffContactValue)
	want := "Contact the market staff: " + staffContactValue + "."
	if got := suspendedRefusal(); got != "This account is suspended. "+want {
		t.Fatalf("set: suspended refusal %q", got)
	}
	for name, b := range pages() {
		mustContain(t, b, want)
		mustNotContain(t, b, askOperatorLine, `href="/messages?to=`)
		switch name {
		case "/login", "/register":
			mustContain(t, b, captchaHelpLine+" "+want)
		case "/challenge":
			mustContain(t, b, lostFactorsLine+"? Only a market administrator can turn your second factors off. "+want)
		}
	}
	// A notification that does not ask the reader to contact the staff gets no contact line.
	agExec(e, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,'Order 1234abcd: shipped')", randomToken(), p.buyer.ID)
	mustNotContain(t, e.body("GET", "/notifications", p.buyerSess, nil, 200), "Contact the market staff:")

	// A hostile value is text everywhere: escaped in HTML, and the refusal is plain text.
	hostile := `<script>alert(1)</script><a href="http://evil.example">x</a>`
	agExec(e, "UPDATE settings SET value=$1 WHERE key='staff_contact'", hostile)
	for _, b := range []string{e.body("GET", "/canary", "", nil, 200), captchaPage("/login"), challenge(),
		e.body("GET", "/notifications", p.vendorSess, nil, 200)} {
		mustContain(t, b, "&lt;script&gt;alert(1)&lt;/script&gt;&lt;a href=&#34;http://evil.example&#34;&gt;")
		mustNotContain(t, b, "<script", `href="http://evil.example"`)
	}
	w := e.do("POST", "/login", randomToken(), form("handle", "sc_suspended", "password", testPassword))
	if ct := w.Header().Get("Content-Type"); w.Code != 403 || !strings.HasPrefix(ct, "text/plain") || w.Header().Get("X-Content-Type-Options") != "nosniff" ||
		strings.TrimSpace(w.Body.String()) != "This account is suspended. Contact the market staff: "+hostile+"." {
		t.Fatalf("hostile contact in the suspended refusal: %d %q %q", w.Code, ct, w.Body.String())
	}
}

// TestStaffContactMessageLink: the /messages?to=<handle> link is added only for a signed-in reader and only when
// the contact is exactly the handle of a moderator or administrator who is not suspended and whose key can receive
// encrypted messages (canReceiveEncrypted, A-83).
func TestStaffContactMessageLink(t *testing.T) {
	e := newTestApp(t)
	modID, modSess := e.user("sc_link_mod", "moderator")
	buyerID, buyer := e.user("sc_link_buyer", "buyer")
	_, key := testPGPKey(t, "sc_link_mod")
	link := `href="/messages?to=sc_link_mod"`
	canary := func(session string) string { return e.body("GET", "/canary", session, nil, 200) }
	set := func(v string) {
		agExec(e, "INSERT INTO settings(key,value) VALUES('staff_contact',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value", v)
	}

	set("sc_link_mod")
	mustNotContain(t, canary(buyer), link) // no key yet
	e.setKey(modID, agSignOnlyKey(t))
	mustNotContain(t, canary(buyer), link) // a key nothing can be encrypted to
	e.setKey(modID, key)
	b := canary(buyer)
	mustContain(t, b, "Contact the market staff: sc_link_mod.", link)
	mustNotContain(t, canary(""), link)      // signed out: the link would only lead to sign-in
	mustNotContain(t, canary(modSess), link) // no link to message yourself

	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", modID)
	mustNotContain(t, canary(buyer), link)
	agExec(e, "UPDATE users SET suspended_at=NULL,role='buyer' WHERE id=$1", modID)
	mustNotContain(t, canary(buyer), link) // not staff
	agExec(e, "UPDATE users SET role='admin' WHERE id=$1", modID)
	mustContain(t, canary(buyer), link)

	// Only the exact handle links; free text naming it does not.
	set("sc_link_mod on this market")
	mustNotContain(t, canary(buyer), `href="/messages?to=`)
	set("sc_link_mod")

	// The payout notice on /notifications carries the same link for its signed-in reader.
	agExec(e, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), buyerID,
		"Your BTC payout address was changed. If you did not make this change, someone else may know your password: change it and contact the market staff.")
	mustContain(t, e.body("GET", "/notifications", buyer, nil, 200), link)
}

// The expired-key refusal (A-155) names the staff contact in the same words (checked here with a key that expires
// between proving it and signing in).
func TestExpiredKeyRefusalNamesStaffContact(t *testing.T) {
	e := newTestApp(t)
	for _, withTOTP := range []bool{false, true} {
		handle := "sc_expired"
		if withTOTP {
			handle = "sc_expired_totp"
		}
		uid, sess := e.user(handle, "buyer")
		created := time.Now().Add(-4 * time.Hour).Truncate(time.Second)
		key := lifecycleKey(t, handle, created, 0, 0)
		provePGP(e, uid, sess, key, armoredPublicKey(t, key))
		renewKey(t, key, created.Add(time.Hour), 3*3600-1800, 0)
		agExec(e, "UPDATE users SET pgp=$2 WHERE id=$1", uid, armoredPublicKey(t, key))
		if withTOTP {
			agEnableTOTP(e, uid)
		}
		agExec(e, "DELETE FROM settings WHERE key='staff_contact'")
		anon, pc := e.pendingSignIn(handle)
		b := e.body("POST", "/challenge/pgp", anon, nil, 409, pc)
		mustContain(t, b, keyExpiredRefusal, askOperatorLine)
		if withTOTP {
			mustContain(t, b, "Lost your authenticator and recovery codes too? "+askOperatorLine)
		}
		agExec(e, "INSERT INTO settings(key,value) VALUES('staff_contact',$1)", staffContactValue)
		b = e.body("POST", "/challenge/pgp", anon, nil, 409, pc)
		mustContain(t, b, keyExpiredRefusal, "Contact the market staff: "+staffContactValue+".")
		mustNotContain(t, b, askOperatorLine)
	}
}
