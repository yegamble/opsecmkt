package market

import (
	"context"
	"html"
	"net/url"
	"strings"
	"testing"
)

// A-704: an administrator's replacement of an account's PGP key needs a one-line verification note of 10–500
// characters. Without one (or with a malformed one) nothing changes: not the key, the factors, the sessions, the
// pending sign-ins, the audit trail or the notifications.
func TestAdminKeyReplacementNeedsVerificationNote(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("note_admin", "admin")
	targetID, _ := e.user("note_target", "buyer")
	agEnableTOTP(e, targetID)
	_ = agPending(e, targetID)
	oldPublic := agStr(e, "SELECT pgp FROM users WHERE id=$1", targetID)
	generation := agInt(e, "SELECT pgp_key_generation FROM users WHERE id=$1", targetID)
	_, newPublic := testPGPKey(t, "note-replacement")
	unchanged := func(name string) {
		t.Helper()
		if agStr(e, "SELECT pgp FROM users WHERE id=$1", targetID) != oldPublic ||
			agInt(e, "SELECT pgp_key_generation FROM users WHERE id=$1", targetID) != generation ||
			agInt(e, "SELECT count(*) FROM users WHERE id=$1 AND totp_enabled", targetID) != 1 ||
			agUserSessions(e, targetID) != 1 ||
			agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", targetID) != 1 ||
			agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1", targetID) != 1 ||
			agInt(e, "SELECT count(*) FROM audit_events") != 0 ||
			agInt(e, "SELECT count(*) FROM notifications") != 0 {
			t.Fatalf("%s: a refused replacement changed the account or its records", name)
		}
	}
	base := func(note string) url.Values {
		f := form("handle", "note_target", "password", testPassword, "replacement_pgp", newPublic)
		if note != "" {
			f.Set("recovery_note", note)
		}
		return f
	}
	for _, tc := range []struct{ name, note string }{
		{"missing", ""},
		{"blank", "          "},
		{"too short", "signed ok"},
		{"too long", strings.Repeat("x", 501)},
		{"two lines", "Signed the fresh text,\nchecked offline"},
		{"control character", "Signed the fresh text\x1b[2J offline"},
		{"text direction", "Signed the fresh text ‮ offline"},
	} {
		agExpect(e, "/admin/reset-factors", admin, base(tc.note), 400, "verification note of 10–500 characters")
		unchanged(tc.name)
	}
	// The retired self-attestation checkbox no longer stands in for the note.
	f := base("")
	f.Set("recovery_identity_verified", "yes")
	agExpect(e, "/admin/reset-factors", admin, f, 400, "verification note of 10–500 characters")
	unchanged("checkbox only")
	// A note without a replacement key is refused rather than silently dropped.
	f = form("handle", "note_target", "password", testPassword, "recovery_note", "Signed the admin-chosen text offline")
	agExpect(e, "/admin/reset-factors", admin, f, 400, "only with a replacement public key")
	unchanged("note without key")
	// Exactly 10 and exactly 500 characters are accepted (counted in characters, not bytes).
	agExpect(e, "/admin/reset-factors", admin, base(strings.Repeat("é", 10)), 303, "")
	if agStr(e, "SELECT pgp FROM users WHERE id=$1", targetID) == oldPublic {
		t.Fatal("a valid note did not replace the key")
	}
	_, nextPublic := testPGPKey(t, "note-replacement-next")
	f = base(strings.Repeat("y", 500))
	f.Set("replacement_pgp", nextPublic)
	agExpect(e, "/admin/reset-factors", admin, f, 303, "")
}

// A-704: both audit rows of a web replacement name the old and the new fingerprint and the verification note,
// rendered escaped; the host command's row names the old and the new fingerprint too.
func TestAdminKeyReplacementAuditsOldAndNewFingerprint(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("audit_key_admin", "admin")
	targetID, _ := e.user("audit_key_target", "vendor")
	agEnableTOTP(e, targetID)
	_, oldPublic := testPGPKey(t, "audit-key-old")
	agEnablePGP(e, targetID, oldPublic)
	oldFingerprint := agStr(e, "SELECT pgp_fingerprint FROM users WHERE id=$1", targetID)
	_, newPublic := testPGPKey(t, "audit-key-new")
	_, newFingerprint, _, err := parseCanonicalPublicKey(newPublic)
	if err != nil || oldFingerprint == "" || oldFingerprint == newFingerprint {
		t.Fatalf("fingerprints %q %q: %v", oldFingerprint, newFingerprint, err)
	}
	note := `Signed fresh text <b>"X-7"</b> & checked offline against the old key.`
	agExpect(e, "/admin/reset-factors", admin, form("handle", "audit_key_target", "password", testPassword,
		"replacement_pgp", newPublic, "recovery_note", "  "+note+"  "), 303, "")
	said := strings.TrimSuffix(note, ".") // the row continues after the note, as after a ban reason
	change := "PGP public key replaced fingerprint " + oldFingerprint + " -> " + newFingerprint + "; verified by: " + said +
		"; TOTP and recovery codes and PGP sign-in turned off; ended 1 session(s)"
	if !e.auditExact(targetID, "Second factors reset by administrator audit_key_admin: "+change) {
		t.Fatalf("target audit: %q", agStr(e, "SELECT action FROM audit_events WHERE user_id=$1", targetID))
	}
	if !e.auditExact(adminID, "Reset second factors of audit_key_target: "+change+" (confirmed with password)") {
		t.Fatalf("administrator audit: %q", agStr(e, "SELECT action FROM audit_events WHERE user_id=$1", adminID))
	}
	if agStr(e, "SELECT pgp_fingerprint FROM users WHERE id=$1", targetID) != newFingerprint {
		t.Fatal("replacement not saved")
	}
	page := e.body("GET", "/admin/system?audit_handle=audit_key_target", admin, nil, 200)
	mustContain(t, page, html.EscapeString(said), oldFingerprint+" -&gt; "+newFingerprint)
	mustNotContain(t, page, `<b>"X-7"</b>`)

	// A keyless account bound by an administrator reads "none" for the old fingerprint.
	keylessID, _ := e.keylessUser("audit_key_keyless", "buyer")
	_, firstPublic := testPGPKey(t, "audit-key-first")
	_, firstFingerprint, _, _ := parseCanonicalPublicKey(firstPublic)
	agExpect(e, "/admin/reset-factors", admin, form("handle", "audit_key_keyless", "password", testPassword,
		"replacement_pgp", firstPublic, "recovery_note", "Known in person; signed the admin text"), 303, "")
	if !e.auditExact(keylessID, "Second factors reset by administrator audit_key_admin: PGP public key replaced fingerprint none -> "+
		firstFingerprint+"; verified by: Known in person; signed the admin text; ended 1 session(s)") {
		t.Fatalf("keyless audit: %q", agStr(e, "SELECT action FROM audit_events WHERE user_id=$1", keylessID))
	}

	// The host command records OLD -> NEW for an administrator's replaced key.
	hostID, _ := e.user("audit_key_host", "admin")
	agEnableTOTP(e, hostID)
	hostOld := agStr(e, "SELECT pgp_fingerprint FROM users WHERE id=$1", hostID)
	_, hostPublic := testPGPKey(t, "audit-key-host")
	_, hostNew, _, _ := parseCanonicalPublicKey(hostPublic)
	summary, err := ResetAdminFactorsWithPublicKey(context.Background(), e.DB, "audit_key_host", hostPublic)
	if err != nil {
		t.Fatal(err)
	}
	hostChange := "TOTP and recovery codes turned off; PGP public key replaced fingerprint " + hostOld + " -> " + hostNew +
		" (ownership unverified); ended 1 session(s) and any pending sign-ins"
	if !e.auditExact(hostID, "Second factors reset by host command: "+hostChange) || summary != "Second factors of administrator audit_key_host reset: "+hostChange {
		t.Fatalf("host audit %q, summary %q", agStr(e, "SELECT action FROM audit_events WHERE user_id=$1", hostID), summary)
	}
}

// A-704: an account's page offers the reset and lost-key replacement whenever it has a PGP key or a second factor,
// with the guide's procedure and what the market's sign-in policy means for the account; posting it returns there.
func TestAccountPageOffersKeyRecoveryWithoutFactors(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("page_key_admin", "admin")
	targetID, _ := e.user("page_key_target", "buyer") // a saved key, no TOTP and no PGP sign-in
	e.keylessUser("page_key_none", "buyer")
	anchor := "docs/operator-guide.md#replace-a-lost-pgp-key"
	back := "/admin/users?handle=page_key_target"

	page := e.body("GET", back, admin, nil, 200)
	mustNotContain(t, page, "No second factor is enrolled.")
	f := adminForm(t, page, "/admin/reset-factors", "")
	mustContain(t, f, `name="handle" value="page_key_target"`, `name="from" value="user"`, `name="replacement_pgp"`,
		`name="recovery_note"`, `minlength="10"`, `maxlength="500"`, anchor, `name="password" required`,
		"Browse-only sign-in is allowed", "Replace the PGP key of page_key_target")
	mustNotContain(t, f, "recovery_identity_verified")
	agExec(e, "UPDATE settings SET value='false' WHERE key=$1", settingAllowLoginWithoutPGP)
	f = adminForm(t, e.body("GET", back, admin, nil, 200), "/admin/reset-factors", "")
	mustContain(t, f, "This market requires PGP decryption at every sign-in")

	// The Users section's form links the same procedure and asks for the note instead of a checkbox.
	users := adminForm(t, e.body("GET", "/admin/users", admin, nil, 200), "/admin/reset-factors", "")
	mustContain(t, users, anchor, `name="recovery_note"`)
	mustNotContain(t, users, "recovery_identity_verified", "account-recovery procedure")

	// Replacing from the account page returns to it with its notice and the new fingerprint.
	_, newPublic := testPGPKey(t, "page-key-new")
	_, newFingerprint, _, _ := parseCanonicalPublicKey(newPublic)
	w := e.do("POST", "/admin/reset-factors", admin, form("handle", "page_key_target", "from", "user", "password", testPassword,
		"replacement_pgp", newPublic, "recovery_note", "Signed the admin-chosen text with the old key"))
	page = userPageNotice(t, e, w, admin, back+"#reset-factors")
	mustContain(t, page, "PGP public key replaced.", "fingerprint "+newFingerprint+"; ownership not verified")
	if agStr(e, "SELECT pgp_fingerprint FROM users WHERE id=$1", targetID) != newFingerprint {
		t.Fatal("replacement from the account page not saved")
	}

	// An account with neither a key nor a factor has nothing to reset or replace.
	none := e.body("GET", "/admin/users?handle=page_key_none", admin, nil, 200)
	mustContain(t, none, "No second factor is enrolled and no PGP key is saved.")
	mustNotContain(t, none, `action="/admin/reset-factors"`)
}
