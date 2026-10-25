package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCanaryIntegration(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("canary_admin", "admin")
	_, buyer := e.user("canary_buyer", "buyer")
	_, mod := e.user("canary_mod", "moderator")
	operator, operatorPub := canaryTestKey(t, "operator")
	other, otherPub := canaryTestKey(t, "other")
	statement := "No warrants, gag orders or searches as of 2026-09-23.\nNext statement due 2026-10-23."
	signed := canarySign(t, operator, statement, canaryKeyTime.Add(time.Hour))
	anon := randomToken()
	canary := func() string {
		w := e.do("GET", "/canary", anon, nil)
		e.check(w, 200)
		return w.Body.String()
	}
	auditCount := func(like string) (n int) {
		e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", adminID, like).Scan(&n)
		return
	}

	// Nothing configured: no statement, never synthesized text.
	if body := canary(); !strings.Contains(body, "No signed canary published") || strings.Contains(body, "Signature verified") || strings.Contains(body, "INVALID") {
		t.Fatal("fresh market must show no canary")
	}
	// Admin-only actions.
	for _, s := range []string{buyer, mod} {
		e.check(e.do("POST", "/admin/operator-key", s, url.Values{"operator_key": {operatorPub}}), 403)
		e.check(e.do("POST", "/admin/canary", s, url.Values{"canary": {signed}}), 403)
	}
	// Publishing before a key exists is refused.
	w := e.do("POST", "/admin/canary", admin, url.Values{"password": {testPassword}, "canary": {signed}})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "operator PGP key") {
		t.Fatalf("canary without key: %d %s", w.Code, w.Body)
	}
	// Garbage and private keys are rejected with the reason.
	for _, bad := range []string{"", "not a key", "-----BEGIN PGP PRIVATE KEY BLOCK-----\nx\n-----END PGP PRIVATE KEY BLOCK-----"} {
		if w := e.do("POST", "/admin/operator-key", admin, url.Values{"password": {testPassword}, "operator_key": {bad}}); w.Code != 400 {
			t.Fatalf("key %q: %d", bad, w.Code)
		}
	}
	e.check(e.do("POST", "/admin/operator-key", admin, url.Values{"password": {testPassword}, "operator_key": {operatorPub}}), 303)
	_, fp, _ := parsePublicKey(operatorPub)
	var storedFP string
	e.DB.QueryRow("SELECT value FROM settings WHERE key='operator_pgp_fingerprint'").Scan(&storedFP)
	if storedFP != fp || auditCount("Set operator PGP key "+fp+"%") != 1 {
		t.Fatalf("operator key not stored/audited: %q", storedFP)
	}
	if body := canary(); !strings.Contains(body, "No signed canary published") {
		t.Fatal("key without statement must still show no canary")
	}

	// Invalid submissions are rejected with the reason and nothing is stored.
	for text, reason := range map[string]string{
		strings.Replace(signed, "No warrants", "Two warrants", 1):     "does not verify",
		canarySign(t, other, statement, canaryKeyTime.Add(time.Hour)): "does not verify",
		"No warrants. Trust me.":                                      "clearsigned",
		signed + "\nUnsigned postscript":                              "after the signature",
	} {
		w := e.do("POST", "/admin/canary", admin, url.Values{"password": {testPassword}, "canary": {text}})
		if w.Code != 400 || !strings.Contains(w.Body.String(), reason) {
			t.Fatalf("invalid canary accepted or wrong reason: %d %s", w.Code, w.Body)
		}
	}
	var stored int
	e.DB.QueryRow("SELECT count(*) FROM canary").Scan(&stored)
	if stored != 0 {
		t.Fatal("rejected canary was stored")
	}

	// A valid statement (browser CRLF line endings) is stored verbatim and shown verified with dates.
	crlf := strings.ReplaceAll(signed, "\n", "\r\n")
	w = e.do("POST", "/admin/canary", admin, url.Values{"password": {testPassword}, "canary": {crlf}})
	if w.Code != 303 || w.Header().Get("Location") != "/canary" {
		t.Fatalf("valid canary: %d %s", w.Code, w.Body)
	}
	var text string
	e.DB.QueryRow("SELECT clearsigned FROM canary WHERE id=1").Scan(&text)
	if text != strings.TrimSpace(crlf) {
		t.Fatal("statement not stored verbatim")
	}
	if auditCount("Published warrant canary signed 2026-01-02 04:04 UTC by operator key "+fp+" (confirmed with password)") != 1 {
		t.Fatal("canary publication not audited")
	}
	body := canary()
	for _, want := range []string{"Signature verified", "No warrants, gag orders or searches as of 2026-09-23.", groupFingerprint(fp), "2026-01-02 04:04 UTC", "Posted to this site"} {
		if !strings.Contains(body, want) {
			t.Fatalf("verified canary page lacks %q", want)
		}
	}
	if strings.Contains(body, "No signed canary published") || strings.Contains(body, "INVALID") {
		t.Fatal("verified page shows another state too")
	}
	if admin := e.do("GET", "/admin/transparency", admin, nil).Body.String(); !strings.Contains(admin, "Signature verified") || !strings.Contains(admin, groupFingerprint(fp)) {
		t.Fatal("admin page lacks canary status")
	}

	// Rotating the operator key re-verifies on render: the old statement now shows INVALID with the reason.
	e.check(e.do("POST", "/admin/operator-key", admin, url.Values{"password": {testPassword}, "operator_key": {otherPub}}), 303)
	body = canary()
	if !strings.Contains(body, "INVALID") || !strings.Contains(body, "signature does not verify with the configured key") || strings.Contains(body, "Signature verified") {
		t.Fatal("stale canary after key rotation must show INVALID")
	}
	// Tampering in the database is detected on the next render too.
	e.check(e.do("POST", "/admin/operator-key", admin, url.Values{"password": {testPassword}, "operator_key": {operatorPub}}), 303)
	if !strings.Contains(canary(), "Signature verified") {
		t.Fatal("restored key should verify again")
	}
	if _, err := e.DB.Exec("UPDATE canary SET clearsigned=replace(clearsigned,'No warrants','Two warrants')"); err != nil {
		t.Fatal(err)
	}
	if body := canary(); !strings.Contains(body, "INVALID") || strings.Contains(body, "Signature verified") {
		t.Fatal("database tampering not detected")
	}
}

// A-201: refuse photo IDs and other unpublishable packets at the operator-key endpoint,
// without changing settings/audits. Legacy rows still serve a usable, stripped public key.
func TestOperatorKeyPacketPrivacy(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("operator_privacy_admin", "admin")
	key, photo, plain := testPhotoIDKey(t, "operator_packet_privacy")
	_, fp, canonical, err := parseCanonicalPublicKey(plain)
	if err != nil {
		t.Fatal(err)
	}
	e.check(e.do("POST", "/admin/operator-key", admin, url.Values{"password": {testPassword}, "operator_key": {plain}}), 303)
	type row struct {
		key, fp string
		audits  int
	}
	state := func() (r row) {
		t.Helper()
		if err := e.DB.QueryRow(`SELECT
   (SELECT value FROM settings WHERE key='operator_pgp_key'),
   (SELECT value FROM settings WHERE key='operator_pgp_fingerprint'),
   (SELECT count(*) FROM audit_events WHERE user_id=$1)`, adminID).Scan(&r.key, &r.fp, &r.audits); err != nil {
			t.Fatal(err)
		}
		return r
	}
	before := state()
	if before.key != canonical || before.fp != fp {
		t.Fatalf("operator key not canonical: %+v", before)
	}
	variants := testPhotoIDVariants(t, key)
	variants["original photo"] = photoIDVariant{armored: photo}
	signed := canarySign(t, key, "Operator statement.", time.Now())
	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			w := e.do("POST", "/admin/operator-key", admin, url.Values{"password": {testPassword}, "operator_key": {variant.armored}})
			if w.Code != 400 || !strings.Contains(w.Body.String(), "Operator key rejected:") {
				t.Fatalf("photo/private operator key accepted: %d", w.Code)
			}
			if after := state(); after != before {
				t.Fatalf("rejected operator key changed settings/audit: %+v -> %+v", before, after)
			}
		})
	}
	// Simulate a pre-refusal database row. Public and administrator views show only allowed packets.
	e.mustExec("UPDATE settings SET value=$1 WHERE key='operator_pgp_key'", photo)
	e.check(e.do("POST", "/admin/canary", admin, url.Values{"password": {testPassword}, "canary": {signed}}), 303)
	for _, path := range []string{"/canary", "/admin/transparency"} {
		session := ""
		if path == "/admin/transparency" {
			session = admin
		}
		page := e.body("GET", path, session, nil, 200)
		keys := shownKeys(t, page)
		if len(keys) == 0 {
			t.Fatalf("%s omitted the usable operator public key", path)
		}
		for _, packets := range keys {
			if packets != armorPackets(t, canonical) {
				t.Fatalf("%s published a legacy operator photo ID", path)
			}
		}
		mustContain(t, page, "Signature verified", groupFingerprint(fp))
	}
}

// A-69 (HUNT2-C-3, formerly TestHuntCanaryExpiryReportedAsForgery): an expired operator key, an expired canary
// signature and a canary dated two minutes after the server clock are refused with their own reasons, not the
// forgery reason "does not verify with the configured key"; an expired operator key is shown as Expired, not
// Configured; a forged statement is still reported as a forgery.
func TestCanaryExpiryReportedWithReason(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("expiry_canary_admin", "admin")
	now := time.Now()
	const forgery = "does not verify with the configured key"
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		form.Set("password", testPassword)
		return e.do("POST", path, admin, form)
	}
	keyPanel := func() string {
		page := e.body("GET", "/admin/transparency", admin, nil, 200)
		i := strings.Index(page, `id="operator-key-title"`)
		j := strings.Index(page, `id="canary-admin-title"`)
		if i < 0 || j < i {
			t.Fatal("operator key panel not found")
		}
		return page[i:j]
	}

	// 1. An operator key that expired a day after canaryKeyTime, with a canary it signed while it was valid.
	expired, expiredPub := expiringCanaryKey(t)
	e.check(post("/admin/operator-key", url.Values{"operator_key": {expiredPub}}), 303)
	if panel := keyPanel(); strings.Contains(panel, "Configured") || !strings.Contains(panel, `badge danger">Expired`) || !strings.Contains(panel, "expired on 2026-01-03 03:04 UTC") {
		t.Fatalf("expired operator key status: %s", panel)
	}
	w := post("/admin/canary", url.Values{"canary": {canarySign(t, expired, "No warrants.", canaryKeyTime.Add(time.Hour))}})
	if w.Code != 400 || strings.Contains(w.Body.String(), forgery) || !strings.Contains(w.Body.String(), "the configured key expired on 2026-01-03 03:04 UTC; extend its expiry") {
		t.Fatalf("canary from an expired key: %d %s", w.Code, w.Body)
	}

	// 2. A current key; a stored canary whose signature was valid for one day and was made two days ago.
	op, opPub := canaryTestKey(t, "operator")
	e.check(post("/admin/operator-key", url.Values{"operator_key": {opPub}}), 303)
	if panel := keyPanel(); !strings.Contains(panel, `badge green">Configured`) || strings.Contains(panel, "Expired") {
		t.Fatalf("current operator key status: %s", panel)
	}
	stale := canarySignExpiring(t, op, "No warrants as of two days ago. Valid for one day.", now.Add(-48*time.Hour), 24*time.Hour)
	if _, err := e.DB.Exec("INSERT INTO canary(id,clearsigned,posted) VALUES(1,$1,now()-interval '47 hours')", stale); err != nil {
		t.Fatal(err)
	}
	page := e.body("GET", "/canary", randomToken(), nil, 200)
	if !strings.Contains(page, "INVALID") || strings.Contains(page, "Signature verified") || strings.Contains(page, forgery) || !strings.Contains(page, "the signature expired on") {
		t.Fatalf("genuine expired canary on /canary: %s", page)
	}

	// 3. The operator's offline clock two minutes ahead of the server: refused as skew, nothing replaced.
	w = post("/admin/canary", url.Values{"canary": {canarySign(t, op, "No warrants.", now.Add(2*time.Minute))}})
	if w.Code != 400 || strings.Contains(w.Body.String(), forgery) || !strings.Contains(w.Body.String(), "after this server&#39;s clock") {
		t.Fatalf("canary dated two minutes ahead: %d %s", w.Code, w.Body)
	}
	var stored string
	if err := e.DB.QueryRow("SELECT clearsigned FROM canary WHERE id=1").Scan(&stored); err != nil || stored != stale {
		t.Fatalf("refused canary replaced the stored one: %v", err)
	}

	// 4. A forged statement is still a forgery.
	forged := strings.Replace(canarySign(t, op, "No warrants.", now.Add(-time.Hour)), "No warrants", "No subpoenas", 1)
	if w = post("/admin/canary", url.Values{"canary": {forged}}); w.Code != 400 || !strings.Contains(w.Body.String(), forgery) {
		t.Fatalf("forged canary: %d %s", w.Code, w.Body)
	}
}
