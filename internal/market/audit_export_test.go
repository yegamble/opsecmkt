package market

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testAuditSeed = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestAuditSigningKeyConfig(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", "")
	if v := auditExportStatus(); v.Available || !strings.Contains(v.Error, "not configured") {
		t.Fatalf("unset: %+v", v)
	}
	for _, bad := range []string{"abc", strings.Repeat("zz", 32), testAuditSeed + "00", testAuditSeed[:62]} {
		t.Setenv("AUDIT_SIGNING_KEY", bad)
		if k, err := auditSigningKey(); k != nil || err == nil {
			t.Fatalf("%q accepted", bad)
		}
		if v := auditExportStatus(); v.Available || !strings.Contains(v.Error, "invalid") {
			t.Fatalf("invalid: %+v", v)
		}
	}
	t.Setenv("AUDIT_SIGNING_KEY", " "+strings.ToUpper(testAuditSeed)+"\n")
	seed, _ := hex.DecodeString(testAuditSeed)
	want := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if v := auditExportStatus(); !v.Available || v.PublicKey != hex.EncodeToString(want) || v.Error != "" {
		t.Fatalf("valid: %+v", v)
	}
}

func TestAuditSignatureFormat(t *testing.T) {
	seed, _ := hex.DecodeString(testAuditSeed)
	key := ed25519.NewKeyFromSeed(seed)
	export := []byte("{\"id\":1}\n")
	sig := auditSignature(key, export)
	if !bytes.Equal(sig, auditSignature(key, export)) {
		t.Fatal("signature file is not deterministic")
	}
	f := parseSigFile(t, sig)
	sum := sha256.Sum256(export)
	s, _ := hex.DecodeString(f["signature"])
	pub, _ := hex.DecodeString(f["public_key"])
	if len(f) != 4 || f["algorithm"] != "ed25519" || f["sha256"] != hex.EncodeToString(sum[:]) || !ed25519.Verify(pub, export, s) {
		t.Fatalf("signature file: %q", sig)
	}
}

func parseSigFile(t *testing.T, sig []byte) map[string]string {
	t.Helper()
	f := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(sig)), "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("bad line %q", line)
		}
		f[k] = v
	}
	return f
}

func TestAuditExportIntegration(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", testAuditSeed)
	e := newTestApp(t)
	adminID, admin := e.user("export_admin", "admin")
	_, buyer := e.user("export_buyer", "buyer")
	_, mod := e.user("export_mod", "moderator")
	for _, q := range []string{
		`INSERT INTO audit_events(user_id,action) VALUES($1,'First <event> & "quotes"')`,
		`INSERT INTO audit_events(user_id,action) VALUES($1,'Second event')`,
		`INSERT INTO audit_events(user_id,action) VALUES(NULL,'System event')`,
	} {
		args := []any{adminID}
		if strings.Contains(q, "NULL") {
			args = nil
		}
		if _, err := e.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	var upto int64
	if err := e.DB.QueryRow("SELECT max(id) FROM audit_events").Scan(&upto); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/admin/audit-export?upto=%d", upto)

	// Access control: anonymous -> sign in, non-admins -> 403.
	w := e.do("GET", path, randomToken(), nil)
	if w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous export: %d %v", w.Code, w.Header())
	}
	for _, s := range []string{buyer, mod} {
		if w := e.do("GET", path, s, nil); w.Code != 403 || strings.Contains(w.Body.String(), "System event") {
			t.Fatalf("non-admin export: %d %s", w.Code, w.Body)
		}
	}

	w = e.do("GET", path, admin, nil)
	e.check(w, 200)
	if w.Header().Get("Content-Type") != "application/x-ndjson" || !strings.Contains(w.Header().Get("Content-Disposition"), fmt.Sprintf(`attachment; filename="audit-events-upto-%d.jsonl"`, upto)) {
		t.Fatalf("export headers: %v", w.Header())
	}
	export := w.Body.Bytes()
	lines := strings.Split(strings.TrimSuffix(string(export), "\n"), "\n")
	if int64(len(lines)) != upto {
		t.Fatalf("expected %d lines, got %q", upto, export)
	}
	var last struct {
		ID      int64   `json:"id"`
		UserID  *string `json:"user_id"`
		Handle  *string `json:"handle"`
		Action  string  `json:"action"`
		Created string  `json:"created"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last.ID != upto || last.UserID != nil || last.Handle != nil || last.Action != "System event" || !strings.HasSuffix(last.Created, "Z") {
		t.Fatalf("last line %q: %+v %v", lines[len(lines)-1], last, err)
	}
	u := func(hex string) string { return `\` + "u00" + hex } // JSON escapes of < > & (encoding/json default)
	wantAction := "First " + u("3c") + "event" + u("3e") + " " + u("26") + ` \"quotes\"`
	if !strings.HasPrefix(lines[len(lines)-3], fmt.Sprintf(`{"id":%d,"user_id":"%s","handle":"export_admin","action":"%s","created":"`, upto-2, adminID, wantAction)) {
		t.Fatalf("field order/escaping changed: %s", lines[len(lines)-3])
	}

	w = e.do("GET", path+"&sig=1", admin, nil)
	e.check(w, 200)
	sig := w.Body.Bytes()
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || !strings.Contains(w.Header().Get("Content-Disposition"), ".sig") {
		t.Fatalf("sig headers: %v", w.Header())
	}
	f := parseSigFile(t, sig)
	pub, _ := hex.DecodeString(f["public_key"])
	s, _ := hex.DecodeString(f["signature"])
	if f["public_key"] != auditExportStatus().PublicKey || !ed25519.Verify(pub, export, s) {
		t.Fatalf("signature does not verify: %q", sig)
	}

	// Later events and restarts do not change the bytes for the same upto.
	if _, err := e.DB.Exec(`INSERT INTO audit_events(user_id,action) VALUES($1,'Later event')`, adminID); err != nil {
		t.Fatal(err)
	}
	e.restart()
	if again := e.do("GET", path, admin, nil).Body.Bytes(); !bytes.Equal(again, export) {
		t.Fatalf("export not byte-stable:\n%s\n%s", export, again)
	}
	if again := e.do("GET", path+"&sig=1", admin, nil).Body.Bytes(); !bytes.Equal(again, sig) {
		t.Fatal("signature not byte-stable")
	}
	earlier := e.do("GET", fmt.Sprintf("/admin/audit-export?upto=%d", upto-1), admin, nil).Body.String()
	if earlier != strings.Join(lines[:len(lines)-1], "\n")+"\n" {
		t.Fatalf("smaller upto is not a prefix: %q", earlier)
	}

	// Verifier CLI round trip with the pinned key; tampering fails.
	dir := t.TempDir()
	jsonl, sigPath, tampered := filepath.Join(dir, "export.jsonl"), filepath.Join(dir, "export.sig"), filepath.Join(dir, "tampered.jsonl")
	os.WriteFile(jsonl, export, 0o600)
	os.WriteFile(sigPath, sig, 0o600)
	os.WriteFile(tampered, bytes.Replace(export, []byte("Second event"), []byte("Second evenT"), 1), 0o600)
	bin := filepath.Join(dir, "verify-audit")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/verify-audit").CombinedOutput(); err != nil {
		t.Fatalf("build verifier: %v %s", err, out)
	}
	if out, err := exec.Command(bin, "-pub", f["public_key"], jsonl, sigPath).CombinedOutput(); err != nil || !strings.Contains(string(out), fmt.Sprintf("OK: %d events", upto)) {
		t.Fatalf("verifier rejected a genuine export: %v %s", err, out)
	}
	if out, err := exec.Command(bin, "-pub", f["public_key"], tampered, sigPath).CombinedOutput(); err == nil || !strings.Contains(string(out), "FAIL") {
		t.Fatalf("verifier accepted a tampered export: %s", out)
	}
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if out, err := exec.Command(bin, "-pub", hex.EncodeToString(otherPub), jsonl, sigPath).CombinedOutput(); err == nil {
		t.Fatalf("verifier accepted an unpinned key: %s", out)
	}

	// Range validation and admin page.
	for _, bad := range []string{"", "upto=0", "upto=-1", "upto=abc", fmt.Sprintf("upto=%d", upto+100)} {
		if w := e.do("GET", "/admin/audit-export?"+bad, admin, nil); w.Code != 400 {
			t.Fatalf("%q: %d", bad, w.Code)
		}
	}
	page := e.do("GET", "/admin", admin, nil)
	e.check(page, 200)
	if body := page.Body.String(); !strings.Contains(body, f["public_key"]) || !strings.Contains(body, fmt.Sprintf(`value="%d"`, upto+1)) || !strings.Contains(body, "Download signature") {
		t.Fatal("admin page lacks export controls")
	}
	if body := e.do("GET", "/canary", randomToken(), nil).Body.String(); !strings.Contains(body, f["public_key"]) || !strings.Contains(body, "Signed exports enabled") {
		t.Fatal("public key not published on /canary")
	}

	// Rate limit: 10 exports per 10 minutes per admin (this test already used 9 successful + 5 rejected).
	e.A.limits = map[string]bucket{}
	for i := 0; i < 10; i++ {
		e.check(e.do("GET", path, admin, nil), 200)
	}
	e.check(e.do("GET", path, admin, nil), 429)
}

func TestAuditExportUnavailableWithoutKey(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", "")
	e := newTestApp(t)
	_, admin := e.user("nokey_admin", "admin")
	w := e.do("GET", "/admin/audit-export?upto=1", admin, nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "AUDIT_SIGNING_KEY") || strings.Contains(w.Body.String(), "{") {
		t.Fatalf("export without key: %d %s", w.Code, w.Body)
	}
	page := e.do("GET", "/admin", admin, nil).Body.String()
	if !strings.Contains(page, "AUDIT_SIGNING_KEY is not configured") || strings.Contains(page, "Download export") {
		t.Fatal("admin page must show export as unavailable")
	}
	if body := e.do("GET", "/canary", randomToken(), nil).Body.String(); !strings.Contains(body, "Signed exports unavailable") {
		t.Fatal("canary page must show export as unavailable")
	}
	t.Setenv("AUDIT_SIGNING_KEY", "not-hex")
	if w := e.do("GET", "/admin/audit-export?upto=1", admin, nil); w.Code != 409 || !strings.Contains(w.Body.String(), "invalid") {
		t.Fatalf("invalid key: %d %s", w.Code, w.Body)
	}
}

// The export's upper bound is only accepted once every lower id is settled, so concurrent writers cannot
// later insert a row inside an already exported range.
func TestAuditExportSettledUnderConcurrentWrites(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", testAuditSeed)
	e := newTestApp(t)
	adminID, _ := e.user("race_admin", "admin")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				e.DB.Exec(`INSERT INTO audit_events(user_id,action) VALUES($1,'concurrent')`, adminID)
			}
		}()
	}
	var exports [][]byte
	var uptos []int64
	for i := 0; i < 5; i++ {
		settled, err := settledAuditMax(t.Context(), e.DB)
		if err != nil {
			t.Fatal(err)
		}
		b, err := auditExport(t.Context(), e.DB, settled)
		if err != nil {
			t.Fatal(err)
		}
		exports, uptos = append(exports, b), append(uptos, settled)
	}
	wg.Wait()
	for i, b := range exports {
		after, err := auditExport(t.Context(), e.DB, uptos[i])
		if err != nil || !bytes.Equal(after, b) {
			t.Fatalf("export up to %d changed after concurrent writers finished", uptos[i])
		}
	}
}
