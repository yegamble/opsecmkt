package market

import (
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
	w := e.do("POST", "/admin/canary", admin, url.Values{"canary": {signed}})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "operator PGP key") {
		t.Fatalf("canary without key: %d %s", w.Code, w.Body)
	}
	// Garbage and private keys are rejected with the reason.
	for _, bad := range []string{"", "not a key", "-----BEGIN PGP PRIVATE KEY BLOCK-----\nx\n-----END PGP PRIVATE KEY BLOCK-----"} {
		if w := e.do("POST", "/admin/operator-key", admin, url.Values{"operator_key": {bad}}); w.Code != 400 {
			t.Fatalf("key %q: %d", bad, w.Code)
		}
	}
	e.check(e.do("POST", "/admin/operator-key", admin, url.Values{"operator_key": {operatorPub}}), 303)
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
		w := e.do("POST", "/admin/canary", admin, url.Values{"canary": {text}})
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
	w = e.do("POST", "/admin/canary", admin, url.Values{"canary": {crlf}})
	if w.Code != 303 || w.Header().Get("Location") != "/canary" {
		t.Fatalf("valid canary: %d %s", w.Code, w.Body)
	}
	var text string
	e.DB.QueryRow("SELECT clearsigned FROM canary WHERE id=1").Scan(&text)
	if text != strings.TrimSpace(crlf) {
		t.Fatal("statement not stored verbatim")
	}
	if auditCount("Published warrant canary signed 2026-01-02 04:04 UTC by operator key "+fp) != 1 {
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
	if admin := e.do("GET", "/admin", admin, nil).Body.String(); !strings.Contains(admin, "Signature verified") || !strings.Contains(admin, groupFingerprint(fp)) {
		t.Fatal("admin page lacks canary status")
	}

	// Rotating the operator key re-verifies on render: the old statement now shows INVALID with the reason.
	e.check(e.do("POST", "/admin/operator-key", admin, url.Values{"operator_key": {otherPub}}), 303)
	body = canary()
	if !strings.Contains(body, "INVALID") || !strings.Contains(body, "signature does not verify with the configured key") || strings.Contains(body, "Signature verified") {
		t.Fatal("stale canary after key rotation must show INVALID")
	}
	// Tampering in the database is detected on the next render too.
	e.check(e.do("POST", "/admin/operator-key", admin, url.Values{"operator_key": {operatorPub}}), 303)
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
