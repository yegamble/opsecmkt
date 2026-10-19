package market

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func decryptAuditExport(t *testing.T, key *openpgp.Entity, ciphertext []byte) ([]byte, error) {
	t.Helper()
	block, err := armor.Decode(bytes.NewReader(ciphertext))
	if err != nil {
		return nil, err
	}
	md, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{key}, nil, nil)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(md.UnverifiedBody)
}

func TestAuditExportRecipientUnset(t *testing.T) {
	r, err := LoadAuditExportRecipient("")
	if err != nil || r != nil || r.Suffix() != "" {
		t.Fatalf("unset recipient: %v %v", r, err)
	}
	data := []byte("{\"id\":1}\n")
	got, err := r.Encrypt(data)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("unset recipient changed the export: %q %v", got, err)
	}
}

func TestAuditExportRecipientRefusals(t *testing.T) {
	e, pub := testSeedKey(t, nil)
	signing, _ := testSeedKey(t, nil)
	signing.Subkeys = nil
	past := time.Now().Add(-48 * time.Hour)
	expired, _ := testSeedKey(t, &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Time: func() time.Time { return past }, KeyLifetimeSecs: 3600})
	revoked, _ := testSeedKey(t, nil)
	if err := revoked.RevokeKey(packet.KeyCompromised, "test-only revocation", nil); err != nil {
		t.Fatal(err)
	}
	var mixed bytes.Buffer
	w, err := armor.Encode(&mixed, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err = e.Subkeys[0].PrivateKey.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	private := testArmor(t, e, true)
	cases := map[string]struct {
		data string
		want error
	}{
		"private armor":                   {private, errAuditExportRecipientPrivate},
		"private primary in public armor": {strings.ReplaceAll(private, "PGP PRIVATE KEY BLOCK", "PGP PUBLIC KEY BLOCK"), errAuditExportRecipientPrivate},
		"private subkey in public armor":  {mixed.String(), errAuditExportRecipientPrivate},
		"multiple keys":                   {pub + "\n" + pub, errAuditExportRecipientInvalid},
		"revoked":                         {testArmor(t, revoked, false), errAuditExportRecipientInvalid},
		"signing only":                    {testArmor(t, signing, false), errAuditExportRecipientUnusable},
		"expired":                         {testArmor(t, expired, false), errAuditExportRecipientUnusable},
		"junk":                            {"private-data-marker not a key", errAuditExportRecipientInvalid},
		"empty":                           {"", errAuditExportRecipientInvalid},
		"oversized":                       {pub + strings.Repeat("\n", auditExportRecipientMaxBytes), errAuditExportRecipientFile},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeSeedKey(t, tc.data)
			if r, err := LoadAuditExportRecipient(path); r != nil || !errors.Is(err, tc.want) {
				t.Fatalf("recipient: %v %v", r, err)
			} else if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "private-data-marker") || !strings.Contains(err.Error(), "AUDIT_EXPORT_RECIPIENT") {
				t.Fatalf("recipient error leaks path/content or omits the setting: %v", err)
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "private-path-marker.asc")
	if r, err := LoadAuditExportRecipient(missing); r != nil || !errors.Is(err, errAuditExportRecipientFile) || strings.Contains(err.Error(), missing) {
		t.Fatalf("missing file: %v %v", r, err)
	}
}

func TestAuditExportRecipientRoundTripAndSignature(t *testing.T) {
	e, pub := testSeedKey(t, nil)
	r, err := LoadAuditExportRecipient(writeSeedKey(t, pub))
	if err != nil || r.Suffix() != ".asc" || r.key.PrivateKey != nil {
		t.Fatalf("recipient: %v %v", r, err)
	}
	t.Setenv("AUDIT_SIGNING_KEY", testAuditSeed)
	signing, err := auditSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("{\"id\":1,\"action\":\"private-marker — café\"}\n")
	sig := auditSignature(signing, data)
	for _, plaintext := range [][]byte{data, sig, {}, {0, 255, 13, 10, 128}} {
		sealed, err := r.Encrypt(plaintext)
		if err != nil || !bytes.HasPrefix(sealed, []byte("-----BEGIN PGP MESSAGE-----")) || bytes.Contains(sealed, []byte("private-marker")) {
			t.Fatalf("encrypt: %q %v", sealed, err)
		}
		got, err := decryptAuditExport(t, e, sealed)
		if err != nil || !bytes.Equal(got, plaintext) {
			t.Fatalf("round trip changed bytes: %q %v", got, err)
		}
		if _, err = decryptAuditExport(t, r.key, sealed); err == nil {
			t.Fatal("public-only key decrypted ciphertext")
		}
		stranger, _ := testSeedKey(t, nil)
		if _, err = decryptAuditExport(t, stranger, sealed); err == nil {
			t.Fatal("unrelated key decrypted ciphertext")
		}
	}
	sealedSig, err := r.Encrypt(sig)
	if err != nil {
		t.Fatal(err)
	}
	decryptedSig, err := decryptAuditExport(t, e, sealedSig)
	if err != nil {
		t.Fatal(err)
	}
	f := parseSigFile(t, decryptedSig)
	s, _ := hex.DecodeString(f["signature"])
	if !ed25519.Verify(signing.Public().(ed25519.PublicKey), data, s) {
		t.Fatal("signature over plaintext failed after offline decryption")
	}
	// A configured recipient that becomes unusable cannot return plaintext.
	r.key.Subkeys = nil
	if got, err := r.Encrypt(data); got != nil || !errors.Is(err, errAuditExportEncryption) || strings.Contains(err.Error(), "private-marker") {
		t.Fatalf("failed encryption did not fail closed: %q %v", got, err)
	}
}

func TestAuditExportHTTPEncryptedAndFailClosed(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", testAuditSeed)
	t.Setenv("AUDIT_EXPORT_RECIPIENT", "")
	e := newTestApp(t)
	_, admin := e.user("sealed_audit_admin", "admin")
	agExec(e, `INSERT INTO audit_events(action) VALUES('private-audit-marker')`)
	upto := int64(agInt(e, "SELECT max(id) FROM audit_events"))
	data, sig, err := ExportAudit(context.Background(), e.DB, 0, upto)
	if err != nil {
		t.Fatal(err)
	}
	plainPage := e.do("GET", "/admin/transparency", admin, nil)
	e.check(plainPage, http.StatusOK)
	if body := plainPage.Body.String(); !strings.Contains(body, "Download export (.jsonl)") || !strings.Contains(body, "Download signature (.sig)") || !strings.Contains(body, "same range always produces byte-identical") || strings.Contains(body, "Decrypt both downloaded files offline") {
		t.Fatal("plaintext export controls or verification guidance changed")
	}
	key, pub := testSeedKey(t, nil)
	path := writeSeedKey(t, pub)
	t.Setenv("AUDIT_EXPORT_RECIPIENT", path)
	for _, tc := range []struct {
		query, suffix string
		want          []byte
	}{{"", ".jsonl.asc", data}, {"&sig=1", ".sig.asc", sig}} {
		w := e.do("GET", fmt.Sprintf("/admin/audit-export?upto=%d%s", upto, tc.query), admin, nil)
		e.check(w, http.StatusOK)
		if w.Header().Get("Content-Type") != "application/pgp-encrypted" || !strings.Contains(w.Header().Get("Content-Disposition"), tc.suffix+`"`) || bytes.Contains(w.Body.Bytes(), []byte("private-audit-marker")) {
			t.Fatalf("encrypted response: headers=%v body=%q", w.Header(), w.Body)
		}
		got, err := decryptAuditExport(t, key, w.Body.Bytes())
		if err != nil || !bytes.Equal(got, tc.want) {
			t.Fatalf("HTTP decrypted bytes differ from raw signed export: %q %v", got, err)
		}
	}
	// Empty ranges are still encrypted, including the detached signature of zero bytes.
	empty := e.do("GET", fmt.Sprintf("/admin/audit-export?upto=%d&from=%d", upto, upto), admin, nil)
	e.check(empty, http.StatusOK)
	if got, err := decryptAuditExport(t, key, empty.Body.Bytes()); err != nil || len(got) != 0 {
		t.Fatalf("empty encrypted range: %q %v", got, err)
	}
	if v := auditExportStatus(); !v.Available || !v.Encrypted || v.Error != "" {
		t.Fatalf("valid recipient disabled exports: %+v", v)
	}
	encryptedPage := e.do("GET", "/admin/transparency", admin, nil)
	e.check(encryptedPage, http.StatusOK)
	if body := encryptedPage.Body.String(); !strings.Contains(body, "Download export (.jsonl.asc)") || !strings.Contains(body, "Download signature (.sig.asc)") || !strings.Contains(body, "Decrypt both downloaded files offline") || !strings.Contains(body, "Encrypted downloads vary") || strings.Contains(body, "same range always produces byte-identical") {
		t.Fatal("encrypted export controls misstated filenames or verification procedure")
	}
	// Replacing a configured key with an invalid file must not fall back to plaintext.
	if err := os.WriteFile(path, []byte("private-invalid-key-marker"), 0600); err != nil {
		t.Fatal(err)
	}
	if v := auditExportStatus(); v.Available || !strings.Contains(v.Error, "AUDIT_EXPORT_RECIPIENT") || strings.Contains(v.Error, path) {
		t.Fatalf("invalid recipient status: %+v", v)
	}
	for _, query := range []string{"", "&sig=1"} {
		w := e.do("GET", fmt.Sprintf("/admin/audit-export?upto=%d%s", upto, query), admin, nil)
		if w.Code != http.StatusConflict || w.Header().Get("Content-Disposition") != "" || !strings.Contains(w.Body.String(), "AUDIT_EXPORT_RECIPIENT") || strings.Contains(w.Body.String(), path) || strings.Contains(w.Body.String(), "private-audit-marker") || strings.Contains(w.Body.String(), "private-invalid-key-marker") {
			t.Fatalf("invalid recipient response leaked data: %d %v %s", w.Code, w.Header(), w.Body)
		}
	}
	page := e.do("GET", "/admin/transparency", admin, nil)
	e.check(page, http.StatusOK)
	if !strings.Contains(page.Body.String(), "AUDIT_EXPORT_RECIPIENT") || !strings.Contains(page.Body.String(), "Correct the configuration named above") || strings.Contains(page.Body.String(), "Set AUDIT_SIGNING_KEY") || strings.Contains(page.Body.String(), "Download export") || strings.Contains(page.Body.String(), path) {
		t.Fatal("admin page did not report recipient unavailability safely")
	}
}
