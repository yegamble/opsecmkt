package market

import (
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A-119: the administrator's "Staff contact" setting gives every "contact the market staff" a path. It is set on
// /admin with the administrator's password, audited old→new, and shown escaped on /canary, in the suspended
// sign-in refusal, as the lost-factors line on /challenge, under notifications that ask the reader to contact the
// staff (including security and takedown notices), as "Can't read the image?" help beside the CAPTCHA and in
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

	f := adminForm(t, e.body("GET", "/admin/site", admin, nil, 200), "/admin/staff-contact", "")
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
	if loc := w.Header().Get("Location"); loc != "/admin/site#staff-contact" {
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
	mustContain(t, adminForm(t, e.body("GET", "/admin/site", admin, nil, 200), "/admin/staff-contact", ""), `value="`+long+`"`)
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
	// A retained security notice asks its owner to contact staff; notification rendering adds the configured path.
	agExec(e, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), p.vendor.ID,
		"Your security settings changed. If this was not you, change your password and contact the market staff.")
	// A-279: a listing an administrator took down (vendor desk, edit page, restore refusal), a photo the malware
	// scanner flagged, and /totp after a sign-in with the last recovery code.
	takenDown := e.product(p.vendor.ID, "physical")
	agExec(e, "UPDATE products SET archived=true,archived_at=now(),admin_archive_reason='Counterfeit branding shown.' WHERE id=$1", takenDown)
	photoListing := e.product(p.vendor.ID, "physical")
	clamd := newFakeClamd(t)
	e.withScanner(clamd)
	clamd.SetVerdict(func([]byte) string { return "stream: Win.Test.Staff-Contact FOUND" })
	recoveryID, _ := e.user("sc_recovery", "buyer")
	_, lastCode := agEnableTOTP(e, recoveryID)
	anonRecovery, pcRecovery := e.pendingSignIn("sc_recovery")
	w := e.do("POST", "/challenge", anonRecovery, url.Values{"method": {"totp"}, "recovery": {lastCode}}, pcRecovery)
	recoveryNotice := noticeSetCookie(w) // A-287: the one-shot sign-in notice, sent with each /totp render below
	if w.Code != 303 || w.Header().Get("Location") != "/totp" || recoveryNotice == nil || recoveryNotice.Value != "signin-recovery" {
		t.Fatalf("sign-in with the last recovery code: %d %q", w.Code, w.Header().Get("Location"))
	}
	recoverySess := e.session(w)
	restoreRefusal := func() string {
		w := e.do("POST", "/listings/restore", p.vendorSess, form("id", takenDown))
		if w.Code != 409 {
			t.Fatalf("restore of a taken-down listing: %d %q", w.Code, w.Body.String())
		}
		return refusalText(w.Body.String())
	}
	uploadRefusal := func() string {
		w := e.upload("/listings/photo", p.vendorSess, e.photoParts(p.vendorSess, photoListing, smallJPEG(t))...)
		if w.Code != 422 {
			t.Fatalf("flagged upload: %d %q", w.Code, w.Body.String())
		}
		return html.UnescapeString(w.Body.String())
	}
	uploadRef := regexp.MustCompile(`Reference: [0-9a-f]{8}`)
	const restoreLine = "An administrator took this listing down with the reason “Counterfeit branding shown.”. You can still edit it, but only an administrator can restore it. If you think this is wrong, ask an administrator to review it. "
	const uploadLine = "Photo refused: the malware scanner flagged this file. Nothing was stored. If you think this is a mistake, send the market staff this reference. "
	const totpLine = "You signed in with a recovery code. 0 recovery codes remain. Without your authenticator you cannot turn TOTP off yourself: ask the market staff for help before you sign out. "
	const deskLine = "Only an administrator can restore it. If you think this is wrong, ask an administrator to review it. "
	const editLine = "Only an administrator can restore this listing; you can still edit it. If you think this is wrong, ask an administrator to review it. "
	// lostFactors is /challenge's line (A-289): the web reset serves every account but an administrator's, whose
	// second factors only the operator resets on the server (-reset-admin-factors).
	const lostFactors = lostFactorsLine + "? Only a market administrator can turn your second factors off, or, for an administrator account, the operator on the server. "

	suspendedRefusal := func() string {
		w := e.do("POST", "/login", randomToken(), form("handle", "sc_suspended", "password", testPassword))
		if w.Code != 403 || e.cookie(w, "session") != "" || e.cookie(w, "pending") != "" {
			t.Fatalf("suspended sign-in: %d %q", w.Code, w.Body.String())
		}
		return refusalText(w.Body.String())
	}
	challenge := func() string {
		anon, pc := e.pendingSignIn("sc_totp")
		return e.body("GET", "/challenge", anon, nil, 200, pc)
	}
	notice := func() string {
		b := html.UnescapeString(e.body("GET", "/notifications", p.vendorSess, nil, 200))
		i := strings.Index(b, "Your security settings changed")
		if i < 0 {
			t.Fatal("security notice missing")
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
			"/canary":           html.UnescapeString(e.body("GET", "/canary", "", nil, 200)),
			"/login":            html.UnescapeString(captchaPage("/login")),
			"/register":         html.UnescapeString(captchaPage("/register")),
			"/challenge":        html.UnescapeString(challenge()),
			"notice":            notice(),
			"/vendor-dashboard": html.UnescapeString(e.body("GET", "/vendor-dashboard", p.vendorSess, nil, 200)),
			"/listing-edit":     html.UnescapeString(e.body("GET", "/listing-edit?id="+takenDown, p.vendorSess, nil, 200)),
			"/totp":             html.UnescapeString(e.body("GET", "/totp", recoverySess, nil, 200, recoveryNotice)),
			"upload":            uploadRefusal(),
		}
	}
	// A-279's plain-text refusal and pages, each with its whole line (contact: the staff-contact sentence expected).
	lines := func(b map[string]string, contact string) {
		t.Helper()
		if got := restoreRefusal(); got != restoreLine+contact {
			t.Fatalf("restore refusal %q", got)
		}
		mustContain(t, b["/vendor-dashboard"], deskLine+contact)
		mustContain(t, b["/listing-edit"], editLine+contact)
		mustContain(t, b["/totp"], totpLine+contact)
		mustContain(t, b["upload"], uploadLine+contact+" Reference: ")
		if !uploadRef.MatchString(b["upload"]) {
			t.Fatal("flagged upload refusal without its reference")
		}
	}

	if got := suspendedRefusal(); got != "This account is suspended. "+askOperatorLine {
		t.Fatalf("unset: suspended refusal %q", got)
	}
	unset := pages()
	for name, b := range unset {
		mustContain(t, b, askOperatorLine)
		mustNotContain(t, b, "Contact the market staff:")
		switch name {
		case "/login", "/register":
			mustContain(t, b, captchaHelpLine+" "+askOperatorLine)
		case "/challenge":
			mustContain(t, b, lostFactors+askOperatorLine)
		}
	}
	lines(unset, askOperatorLine)

	agExec(e, "INSERT INTO settings(key,value) VALUES('staff_contact',$1)", staffContactValue)
	want := "Contact the market staff: " + staffContactValue + "."
	if got := suspendedRefusal(); got != "This account is suspended. "+want {
		t.Fatalf("set: suspended refusal %q", got)
	}
	set := pages()
	for name, b := range set {
		mustContain(t, b, want)
		mustNotContain(t, b, askOperatorLine, `href="/messages?to=`)
		switch name {
		case "/login", "/register":
			mustContain(t, b, captchaHelpLine+" "+want)
		case "/challenge":
			mustContain(t, b, lostFactors+want)
		}
	}
	lines(set, want)
	// A notification that does not ask the reader to contact the staff gets no contact line.
	agExec(e, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,'Order 1234abcd: shipped')", randomToken(), p.buyer.ID)
	mustNotContain(t, e.body("GET", "/notifications", p.buyerSess, nil, 200), "Contact the market staff:")

	// A hostile value is text everywhere: escaped in HTML, and the refusal is plain text.
	hostile := `<script>alert(1)</script><a href="http://evil.example">x</a>`
	agExec(e, "UPDATE settings SET value=$1 WHERE key='staff_contact'", hostile)
	for _, b := range []string{e.body("GET", "/canary", "", nil, 200), captchaPage("/login"), challenge(),
		e.body("GET", "/notifications", p.vendorSess, nil, 200), e.body("GET", "/vendor-dashboard", p.vendorSess, nil, 200),
		e.body("GET", "/listing-edit?id="+takenDown, p.vendorSess, nil, 200), e.body("GET", "/totp", recoverySess, nil, 200, recoveryNotice),
		e.upload("/listings/photo", p.vendorSess, e.photoParts(p.vendorSess, photoListing, smallJPEG(t))...).Body.String()} {
		mustContain(t, b, "&lt;script&gt;alert(1)&lt;/script&gt;&lt;a href=&#34;http://evil.example&#34;&gt;")
		mustNotContain(t, b, "<script", `href="http://evil.example"`)
	}
	if got := restoreRefusal(); got != restoreLine+"Contact the market staff: "+hostile+"." {
		t.Fatalf("hostile contact in the restore refusal: %q", got)
	}
	w = e.do("POST", "/login", randomToken(), form("handle", "sc_suspended", "password", testPassword))
	// A-514: the sign-in form again, the refusal escaped in its alert.
	if ct := w.Header().Get("Content-Type"); w.Code != 403 || !strings.HasPrefix(ct, "text/html") || w.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(w.Body.String(), `role="alert">`+template.HTMLEscapeString("This account is suspended. Contact the market staff: "+hostile+".")+`</div>`) ||
		strings.Contains(w.Body.String(), "<script>alert") {
		t.Fatalf("hostile contact in the suspended refusal: %d %q %q", w.Code, ct, w.Body.String())
	}
}

// TestStaffContactMessageLink: the /messages?to=<handle> link is added only for a signed-in reader and only when
// the contact is exactly the handle of a moderator or administrator who is not suspended and whose key can receive
// encrypted messages (canReceiveEncrypted, A-83).
func TestStaffContactMessageLink(t *testing.T) {
	e := newTestApp(t)
	modID, modSess := e.keylessUser("sc_link_mod", "moderator")
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

	// A-279: a taken-down listing's vendor desk card and edit page carry the same link for the vendor.
	vendorID, vendor := e.user("sc_link_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	agExec(e, "UPDATE products SET archived=true,archived_at=now(),admin_archive_reason='Counterfeit branding shown.' WHERE id=$1", listing)
	for _, path := range []string{"/vendor-dashboard", "/listing-edit?id=" + listing} {
		mustContain(t, e.body("GET", path, vendor, nil, 200), "ask an administrator to review it. Contact the market staff: sc_link_mod. <a class=\"text-link\" "+link)
	}

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

// A-298: on the flagged-upload path (A-279) a staff contact that cannot be read leaves the contact line out rather
// than guessing "Ask the operator", still answers 422 with the reference, and logs only the cause class: never the
// raw database error (which names the relation) or the setting's value.
func TestStaffContactUnreadableOnUpload(t *testing.T) {
	e := newTestApp(t)
	vendorID, vs := e.user("sc_unreadable", "vendor")
	listing := e.product(vendorID, "physical")
	clamd := newFakeClamd(t)
	e.withScanner(clamd)
	clamd.SetVerdict(func([]byte) string { return "stream: Win.Test.Unreadable FOUND" })
	const secret = "xmpp:secret-contact@staff-example.onion"
	agExec(e, "INSERT INTO settings(key,value) VALUES('staff_contact',$1)", secret)
	const refused = "Photo refused: the malware scanner flagged this file. Nothing was stored. If you think this is a mistake, send the market staff this reference. "
	upload := func() string {
		t.Helper()
		w := e.upload("/listings/photo", vs, e.photoParts(vs, listing, smallJPEG(t))...)
		if w.Code != 422 {
			t.Fatalf("flagged upload: %d %q", w.Code, w.Body.String())
		}
		return html.UnescapeString(w.Body.String())
	}
	// Readable, the line names the contact, so the unreadable case below is not vacuous.
	mustContain(t, upload(), refused+"Contact the market staff: "+secret+". Reference: ")

	logs := captureLog(t)
	agExec(e, "ALTER TABLE settings RENAME TO settings_a298")
	defer agExec(e, "ALTER TABLE settings_a298 RENAME TO settings")
	body := upload()
	got := logs.String()
	if !strings.Contains(got, "upload staff contact read failed cause=42P01\n") {
		t.Errorf("log lacks the fixed cause class: %q", got)
	}
	for _, leak := range []string{secret, "settings", "does not exist", "relation"} {
		if strings.Contains(got, leak) {
			t.Errorf("log contains %q: %q", leak, got)
		}
	}
	if !regexp.MustCompile(regexp.QuoteMeta(refused) + `Reference: [0-9a-f]{8}\b`).MatchString(body) {
		i := max(strings.Index(body, "Photo refused"), 0)
		t.Errorf("unreadable contact: the refusal is not followed directly by its reference: %q", body[i:min(len(body), i+300)])
	}
	mustNotContain(t, body, secret, askOperatorLine, "Contact the market staff")
}
