package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/ramsecrets"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func recoveryKey(t *testing.T) (*openpgp.Entity, string) {
	t.Helper()
	key, err := openpgp.NewEntity("offline test", "", "", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w, err := armor.Encode(&b, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := key.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "public.asc")
	if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return key, path
}
func recoveryInvoke(body []byte, args ...string) (string, error) {
	var out bytes.Buffer
	err := runRecovery(context.Background(), args, bytes.NewReader(body), &out, &out)
	return out.String(), err
}
func recoveryExport(t *testing.T, source string) ([]byte, string) {
	t.Helper()
	key, public := recoveryKey(t)
	archive := filepath.Join(t.TempDir(), "recovery")
	if out, err := recoveryInvoke(nil, "export", "--dir", source, "--recipient-public-key", public, "--out", archive); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	encrypted, err := os.ReadFile(filepath.Join(archive, "private.pgp"))
	if err != nil {
		t.Fatal(err)
	}
	block, err := armor.Decode(bytes.NewReader(encrypted))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{key}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(msg.UnverifiedBody)
	if err != nil {
		t.Fatal(err)
	}
	return plain, archive
}
func TestRecoveryRoundTripPreservesConfigurationAndOnion(t *testing.T) {
	allowRecoveryFilesystem(t)
	t.Setenv("PATH", filepath.Join(t.TempDir(), "no-executables"))
	source := initAdminTor(t)
	c, err := loadConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	receiptBytes, _ := json.Marshal(receipt{Version: 1, Project: c.Project, DockerContext: c.DockerContext})
	if err := os.WriteFile(filepath.Join(source, "installation.json"), receiptBytes, 0600); err != nil {
		t.Fatal(err)
	}
	plain, archive := recoveryExport(t, source)
	target := filepath.Join(t.TempDir(), "restored")
	output, err := recoveryInvoke(plain, "restore", "--dir", target)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := loadConfig(target)
	if err != nil {
		t.Fatal(err)
	}
	if restored.RetentionDir != c.RetentionDir || restored.Project != c.Project || restored.OnionAddress != c.OnionAddress || restored.AdminOnionAddress != c.AdminOnionAddress || len(restored.StorageBindings) != 0 {
		t.Fatal("recovery changed identities/paths or adopted host proof")
	}
	for _, dirs := range []string{"secrets", "admin-clients", "onion-public", "onion-admin"} {
		entries, err := os.ReadDir(filepath.Join(source, dirs))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := filepath.Join(dirs, entry.Name())
			before, _ := os.ReadFile(filepath.Join(source, name))
			after, err := os.ReadFile(filepath.Join(target, name))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("changed %s", name)
			}
			info, _ := os.Lstat(filepath.Join(target, name))
			if info.Mode().Perm() != 0600 {
				t.Fatal("file permissions")
			}
			if strings.Contains(output, strings.TrimSpace(string(before))) && dirs == "secrets" {
				t.Fatal("output leaked secret")
			}
		}
	}
	for _, name := range []string{"private.pgp", "deployment.json", "manifest.json", "RECOVERY.txt"} {
		info, err := os.Lstat(filepath.Join(archive, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("archive permissions: %s", name)
		}
	}
	for _, name := range []string{"compose.json", "server.env", "host-reboot.json", "retention"} {
		if _, err := os.Lstat(filepath.Join(target, name)); !os.IsNotExist(err) {
			t.Fatal("restored excluded state")
		}
	}
	if _, err := recoveryInvoke(plain, "restore", "--dir", target); err == nil {
		t.Fatal("overwrote destination")
	}
	_, public := recoveryKey(t)
	if _, err := recoveryInvoke(nil, "export", "--dir", source, "--recipient-public-key", public, "--out", archive); err == nil {
		t.Fatal("overwrote archive")
	}
}

func allowRecoveryFilesystem(t *testing.T) {
	t.Helper()
	old := recoveryCheckPersistent
	recoveryCheckPersistent = func(string) error { return nil }
	t.Cleanup(func() { recoveryCheckPersistent = old })
}

func TestRecoveryRefusesMalformedTamperedAndUnsafeDocuments(t *testing.T) {
	allowRecoveryFilesystem(t)
	plain, _ := recoveryExport(t, initAdminTor(t))
	for _, tc := range []struct {
		name   string
		mutate func(*recoveryDocument)
	}{
		{"version", func(d *recoveryDocument) { d.Version = 9 }},
		{"metadata", func(d *recoveryDocument) { d.Deployment.Project = "other" }},
		{"hash", func(d *recoveryDocument) { d.SHA256["operator.json"] = strings.Repeat("0", 64) }},
		{"extra hash", func(d *recoveryDocument) { d.SHA256["../escape"] = strings.Repeat("0", 64) }},
		{"traversal", func(d *recoveryDocument) {
			mutateRecoveryBundle(t, d, func(b *ramsecrets.Bundle) { b.Files[0].Path = "../escape" })
		}},
		{"duplicate file", func(d *recoveryDocument) {
			mutateRecoveryBundle(t, d, func(b *ramsecrets.Bundle) { b.Files = append(b.Files, b.Files[0]) })
		}},
		{"oversized file", func(d *recoveryDocument) {
			mutateRecoveryBundle(t, d, func(b *ramsecrets.Bundle) { b.Files[0].Data = bytes.Repeat([]byte{'x'}, 65537) })
		}},
		{"onion mismatch", func(d *recoveryDocument) {
			mutateRecoveryBundle(t, d, func(b *ramsecrets.Bundle) {
				for i := range b.Files {
					if b.Files[i].Path == "onion-public/hostname" {
						b.Files[i].Data = []byte("wrong.onion\n")
					}
				}
			})
		}},
		{"unknown configuration", func(d *recoveryDocument) {
			mutateRecoveryBundle(t, d, func(b *ramsecrets.Bundle) {
				for i := range b.Files {
					if b.Files[i].Path == "operator.json" {
						b.Files[i].Data = append([]byte(`{"unexpected":true,`), b.Files[i].Data[1:]...)
					}
				}
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d recoveryDocument
			if err := json.Unmarshal(plain, &d); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&d)
			data, _ := json.Marshal(d)
			assertRecoveryRefusal(t, data)
		})
	}
	for _, data := range [][]byte{nil, []byte(`{`), append(plain, []byte(`{}`)...), bytes.Replace(plain, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1), bytes.Repeat([]byte{'x'}, recoveryLimit+1)} {
		assertRecoveryRefusal(t, data)
	}
}
func mutateRecoveryBundle(t *testing.T, d *recoveryDocument, mutate func(*ramsecrets.Bundle)) {
	t.Helper()
	var b ramsecrets.Bundle
	if err := json.Unmarshal(d.Bundle, &b); err != nil {
		t.Fatal(err)
	}
	mutate(&b)
	d.Bundle, _ = json.Marshal(b)
	d.SHA256 = map[string]string{}
	for _, f := range b.Files {
		d.SHA256[f.Path] = recoveryHash(f.Data)
	}
}
func assertRecoveryRefusal(t *testing.T, data []byte) {
	t.Helper()
	parent := t.TempDir()
	target := filepath.Join(parent, "refused")
	if _, err := recoveryInvoke(data, "restore", "--dir", target); err == nil {
		t.Fatal("unsafe recovery accepted")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal("refusal wrote/staged plaintext or target")
	}
}
func TestRecoveryProductionRequiresFenceAndEncryptedHostAndRebind(t *testing.T) {
	allowRecoveryFilesystem(t)
	source := initProduction(t)
	plain, _ := recoveryExport(t, source)
	if _, err := recoveryInvoke(plain, "restore", "--dir", filepath.Join(t.TempDir(), "restored")); err == nil || !strings.Contains(err.Error(), "old-host-fenced") {
		t.Fatalf("missing fence: %v", err)
	}
	old := productionStorageProbe
	t.Cleanup(func() { productionStorageProbe = old })
	productionStorageProbe = func(context.Context, string, Config, []string) ([]StorageBinding, error) {
		return nil, errors.New("unencrypted replacement")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "restored")
	if _, err := recoveryInvoke(plain, "restore", "--dir", target, "--old-host-fenced"); err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("missing encryption: %v", err)
	}
	entries, _ := os.ReadDir(parent)
	if len(entries) != 0 {
		t.Fatal("unencrypted restore staged plaintext")
	}
	productionStorageProbe = old
	if _, err := recoveryInvoke(plain, "restore", "--dir", target, "--old-host-fenced"); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.StorageBindings) != 0 {
		t.Fatal("old-host storage adopted")
	}
	if err := checkProductionStorage(context.Background(), target, c); err == nil || !strings.Contains(err.Error(), "storage bind") {
		t.Fatalf("restored production could start without rebinding: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(source, "operator.json"))
	if err != nil {
		t.Fatal(err)
	}
	var original Config
	if err := json.Unmarshal(before, &original); err != nil {
		t.Fatal(err)
	}
	for _, paths := range []map[string]string{original.SecretFiles, original.PublicFiles, original.TLSFiles} {
		for _, path := range paths {
			a, _ := os.ReadFile(filepath.Join(source, path))
			b, err := os.ReadFile(filepath.Join(target, path))
			if err != nil || !bytes.Equal(a, b) {
				t.Fatalf("changed configuration material %s", path)
			}
		}
	}
}
func TestRecoveryRefusesSymlinksPermissionsAndPrivateKeyFlags(t *testing.T) {
	allowRecoveryFilesystem(t)
	source := initDev(t)
	_, public := recoveryKey(t)
	parent := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
	if _, err := recoveryInvoke(nil, "export", "--dir", source, "--recipient-public-key", public, "--out", filepath.Join(link, "archive")); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	if err := os.Chmod(filepath.Join(source, "secrets/app-secret-key"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := recoveryInvoke(nil, "export", "--dir", source, "--recipient-public-key", public, "--out", filepath.Join(parent, "archive")); err == nil {
		t.Fatal("public secret accepted")
	}
	for _, args := range [][]string{{"restore", "--dir", filepath.Join(parent, "target"), "--bundle", "plaintext.json"}, {"restore", "--dir", filepath.Join(parent, "target"), "--private-key", "offline.asc"}} {
		if _, err := recoveryInvoke(nil, args...); err == nil {
			t.Fatal("plaintext/private key flag accepted")
		}
	}
}
func TestRecoveryManifestAndCiphertextDoNotExposeSecrets(t *testing.T) {
	source := initDev(t)
	plain, archive := recoveryExport(t, source)
	var m recoveryManifest
	data, err := os.ReadFile(filepath.Join(archive, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	secret, _ := os.ReadFile(filepath.Join(source, "secrets/app-secret-key"))
	for name, hash := range m.SHA256 {
		data, err := os.ReadFile(filepath.Join(archive, name))
		if err != nil || recoveryHash(data) != hash {
			t.Fatal("archive checksum mismatch")
		}
		if bytes.Contains(data, bytes.TrimSpace(secret)) || bytes.Contains(data, plain) {
			t.Fatalf("plaintext in %s", name)
		}
	}
}
func TestRecoveryRejectsLegacyVolumeOnlyOnion(t *testing.T) {
	source := initAdminTor(t)
	data, err := os.ReadFile(filepath.Join(source, "operator.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	c.AdminOnionDir = ""
	c.AdminOnionAddress = ""
	data, _ = json.Marshal(c)
	if err := os.WriteFile(filepath.Join(source, "operator.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	_, public := recoveryKey(t)
	if _, err := recoveryInvoke(nil, "export", "--dir", source, "--recipient-public-key", public, "--out", filepath.Join(t.TempDir(), "archive")); err == nil || !strings.Contains(err.Error(), "volume identity") {
		t.Fatalf("legacy identity omitted: %v", err)
	}
}

func TestRecoveryPreservesExpiredTLSWithoutAllowingServiceStart(t *testing.T) {
	allowRecoveryFilesystem(t)
	source := initDev(t)
	c, err := loadConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	cert, key := testTLSMaterial(t, true)
	for name, data := range map[string][]byte{"expired.pem": cert, "expired.key": key} {
		if err := os.WriteFile(filepath.Join(source, "secrets", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	c.TLSFiles = map[string]string{"TLS_CERT": "secrets/expired.pem", "TLS_KEY": "secrets/expired.key"}
	data, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(source, "operator.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	plain, _ := recoveryExport(t, source)
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := recoveryInvoke(plain, "restore", "--dir", target); err != nil {
		t.Fatal(err)
	}
	restored, err := loadConfig(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateMaterial(target, restored); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("restore weakened deploy TLS gate: %v", err)
	}
	for name, want := range map[string][]byte{"expired.pem": cert, "expired.key": key} {
		got, err := os.ReadFile(filepath.Join(target, "secrets", name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("recovery rotated expired material")
		}
	}
}

func TestRecoveryPreservesExcludedWorkerReferenceBeforeWorkerIsRestored(t *testing.T) {
	allowRecoveryFilesystem(t)
	source := initDev(t)
	c, err := loadConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	reports := filepath.Join(t.TempDir(), "reports")
	if err := os.Mkdir(reports, 0700); err != nil {
		t.Fatal(err)
	}
	c.DependencyReportDir = reports
	c.PublicFiles = map[string]string{"DEPENDENCY_REPORT_PUBLIC_KEY": "secrets/worker-public"}
	if err := os.WriteFile(filepath.Join(source, "secrets/worker-public"), []byte(strings.Repeat("ab", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(source, "operator.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	plain, _ := recoveryExport(t, source)
	if err := os.Remove(reports); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := recoveryInvoke(plain, "restore", "--dir", target); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(target, "operator.json"))
	if err != nil {
		t.Fatal(err)
	}
	var restored Config
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.DependencyReportDir != reports || restored.PublicFiles["DEPENDENCY_REPORT_PUBLIC_KEY"] != c.PublicFiles["DEPENDENCY_REPORT_PUBLIC_KEY"] {
		t.Fatal("excluded worker references changed")
	}
	if _, err := loadConfig(target); err == nil {
		t.Fatal("normal install validation ignored missing worker reference")
	}
}

func TestRecoveryPreservesExpiredConfiguredPublicPGPWithoutWeakeningDeploy(t *testing.T) {
	allowRecoveryFilesystem(t)
	key, err := openpgp.NewEntity("expired configured recipient", "", "", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Time: func() time.Time { return time.Now().Add(-2 * time.Hour) }, KeyLifetimeSecs: 3600})
	if err != nil {
		t.Fatal(err)
	}
	var public bytes.Buffer
	w, err := armor.Encode(&public, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := key.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := key.EncryptionKey(time.Now()); ok {
		t.Fatal("fixture encryption key did not expire")
	}
	source := initDev(t)
	c, err := loadConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	c.PublicFiles = map[string]string{"AUDIT_EXPORT_RECIPIENT": "secrets/expired-recipient.asc", "ESCROW_XMR_SEED_KEY": "secrets/expired-recipient.asc"}
	if err := os.WriteFile(filepath.Join(source, "secrets/expired-recipient.asc"), public.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(source, "operator.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	plain, _ := recoveryExport(t, source)
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := recoveryInvoke(plain, "restore", "--dir", target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "secrets/expired-recipient.asc"))
	if err != nil || !bytes.Equal(got, public.Bytes()) {
		t.Fatal("configured expired public key changed during recovery")
	}
	restored, err := loadConfig(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateMaterial(target, restored); err == nil || !strings.Contains(err.Error(), "currently usable") {
		t.Fatalf("normal deployment accepted expired recipient: %v", err)
	}
}

func TestRecoveryRefusesMalformedAndConcealedPrivateConfiguredPGP(t *testing.T) {
	allowRecoveryFilesystem(t)
	key, publicPath := recoveryKey(t)
	public, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	var concealed bytes.Buffer
	w, err := armor.Encode(&concealed, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := key.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	source := initDev(t)
	c, err := loadConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	c.PublicFiles = map[string]string{"AUDIT_EXPORT_RECIPIENT": "secrets/recipient.asc"}
	if err := os.WriteFile(filepath.Join(source, "secrets/recipient.asc"), public, 0600); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(source, "operator.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	plain, _ := recoveryExport(t, source)
	for name, invalid := range map[string][]byte{"malformed": []byte("not a PGP key"), "private packets in public armor": concealed.Bytes(), "multiple keys": append(append([]byte{}, public...), public...)} {
		t.Run(name, func(t *testing.T) {
			var d recoveryDocument
			if err := json.Unmarshal(plain, &d); err != nil {
				t.Fatal(err)
			}
			mutateRecoveryBundle(t, &d, func(b *ramsecrets.Bundle) {
				for i := range b.Files {
					if b.Files[i].Path == "secrets/recipient.asc" {
						b.Files[i].Data = invalid
					}
				}
			})
			encoded, _ := json.Marshal(d)
			assertRecoveryRefusal(t, encoded)
			if err := os.WriteFile(filepath.Join(source, "secrets/recipient.asc"), invalid, 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "refused-export")
			if _, err := recoveryInvoke(nil, "export", "--dir", source, "--recipient-public-key", publicPath, "--out", target); err == nil {
				t.Fatal("export accepted invalid configured PGP")
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatal("failed export created archive")
			}
		})
	}
}
