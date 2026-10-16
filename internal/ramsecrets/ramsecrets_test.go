package ramsecrets

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func fixture(t *testing.T) (string, Bundle) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "offline")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "secrets"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`{"version":1,"runtime_uid":1000,"runtime_gid":1000,"retention_dir":"/var/lib/opsecmkt-retention","secret_files":{"APP_SECRET_KEY":"secrets/app-secret-key"}}`)
	body := []byte("disposable-fixture-secret\n")
	for name, data := range map[string][]byte{"operator.json": cfg, "secrets/app-secret-key": body} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, Bundle{Version: 1, Files: []File{{Path: "operator.json", Data: cfg}, {Path: "secrets/app-secret-key", Data: body}}}
}
func fakePlatform() platform {
	return platform{verify: func(string) error { return nil }, mount: func(string) error { return nil }, euid: func() int { return 0 }, chown: func(string, int, int) error { return nil }, isTmpfs: func(string) (bool, error) { return false, nil }, publish: os.Rename}
}
func runFake(t *testing.T, p platform, body []byte, args ...string) (string, error) {
	t.Helper()
	var out, errs bytes.Buffer
	err := run(context.Background(), args, bytes.NewReader(body), &out, &errs, p)
	return out.String() + errs.String(), err
}

func TestPackEncryptsOnlyAllowedInstallationMaterial(t *testing.T) {
	dir, _ := fixture(t)
	key, err := openpgp.NewEntity("offline fixture", "", "", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	var public bytes.Buffer
	w, _ := armor.Encode(&public, openpgp.PublicKeyType, nil)
	if err := key.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	recipient := filepath.Join(t.TempDir(), "recipient.asc")
	os.WriteFile(recipient, public.Bytes(), 0644)
	output := filepath.Join(t.TempDir(), "bundle.pgp")
	text, err := runFake(t, fakePlatform(), nil, "pack", "--dir", dir, "--recipient-public-key", recipient, "--out", output)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, _ := os.ReadFile(output)
	if bytes.Contains(encrypted, []byte("disposable-fixture-secret")) || strings.Contains(text, "disposable-fixture-secret") {
		t.Fatal("plaintext escaped encryption")
	}
	block, err := armor.Decode(bytes.NewReader(encrypted))
	if err != nil {
		t.Fatal(err)
	}
	message, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{key}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := io.ReadAll(message.UnverifiedBody)
	if err != nil {
		t.Fatal(err)
	}
	var bundle Bundle
	if err := json.Unmarshal(plaintext, &bundle); err != nil || len(bundle.Files) != 2 {
		t.Fatalf("invalid packed files: %v", err)
	}
	info, _ := os.Stat(output)
	if info.Mode().Perm() != 0600 {
		t.Fatal("ciphertext permissions")
	}
	if _, err := runFake(t, fakePlatform(), nil, "pack", "--dir", dir, "--recipient-public-key", recipient, "--out", output); err == nil {
		t.Fatal("ciphertext overwritten")
	}
}

func TestUnlockRequiresVerifiedMemoryAndPublishesPrivateFiles(t *testing.T) {
	_, bundle := fixture(t)
	body, _ := json.Marshal(bundle)
	parent := t.TempDir()
	target := filepath.Join(parent, "market")
	verified := ""
	p := fakePlatform()
	p.verify = func(path string) error { verified = path; return nil }
	out, err := runFake(t, p, body, "unlock", "--dir", target)
	if err != nil {
		t.Fatal(err)
	}
	if verified != parent {
		t.Fatalf("verified wrong parent: %s", verified)
	}
	for _, name := range []string{"operator.json", "secrets/app-secret-key"} {
		info, err := os.Lstat(filepath.Join(target, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("private regular output missing")
		}
	}
	for _, name := range []string{"", "secrets"} {
		info, err := os.Lstat(filepath.Join(target, name))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("private directory missing")
		}
	}
	if strings.Contains(out, "disposable-fixture-secret") {
		t.Fatal("printed secret")
	}
	if _, err := runFake(t, p, body, "unlock", "--dir", target); err == nil {
		t.Fatal("overwrote unlocked instance")
	}
}

func TestUnlockRefusesUnsafeBundlesAtomically(t *testing.T) {
	_, original := fixture(t)
	for _, tc := range []struct {
		name   string
		mutate func(*Bundle)
	}{
		{"traversal", func(b *Bundle) { b.Files[1].Path = "../escaped" }},
		{"absolute", func(b *Bundle) { b.Files[1].Path = "/escaped" }},
		{"nested", func(b *Bundle) { b.Files[1].Path = "secrets/nested/key" }},
		{"oversize", func(b *Bundle) { b.Files[1].Data = bytes.Repeat([]byte{'s'}, 65537) }},
		{"duplicate", func(b *Bundle) { b.Files = append(b.Files, b.Files[1]) }},
		{"private recovery", func(b *Bundle) { b.Files[1].Data = []byte("-----BEGIN PGP PRIVATE KEY BLOCK-----") }},
		{"unreferenced", func(b *Bundle) { b.Files = append(b.Files, File{Path: "secrets/extra", Data: []byte("secret")}) }},
		{"retention", func(b *Bundle) { b.Files = append(b.Files, File{Path: "retention/state", Data: []byte("secret")}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := original
			bundle.Files = append([]File{}, original.Files...)
			tc.mutate(&bundle)
			body, _ := json.Marshal(bundle)
			target := filepath.Join(t.TempDir(), "target")
			if _, err := runFake(t, fakePlatform(), body, "unlock", "--dir", target); err == nil {
				t.Fatal("unsafe bundle accepted")
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatal("invalid bundle wrote instance")
			}
		})
	}
}

func TestUnlockRefusesDiskStorageAndNonroot(t *testing.T) {
	_, bundle := fixture(t)
	body, _ := json.Marshal(bundle)
	for _, p := range []platform{
		func() platform { p := fakePlatform(); p.euid = func() int { return 1000 }; return p }(),
		func() platform {
			p := fakePlatform()
			p.verify = func(string) error { return os.ErrPermission }
			return p
		}(),
		func() platform {
			p := fakePlatform()
			p.isTmpfs = func(string) (bool, error) { return true, nil }
			return p
		}(),
	} {
		target := filepath.Join(t.TempDir(), "target")
		if _, err := runFake(t, p, body, "unlock", "--dir", target); err == nil {
			t.Fatal("unsafe RAM/retention boundary accepted")
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatal("refusal wrote files")
		}
	}
}

func TestMountInfoUsesExactContainingMountAndRequiredFlags(t *testing.T) {
	good := "10 2 0:1 / /run/secret rw,nosuid,nodev,noexec - tmpfs tmpfs rw,size=16384k,mode=700\n"
	if err := validateMountInfo("/run/secret/market", []byte(good), false); err != nil {
		t.Fatal(err)
	}
	if err := validateMountInfo("/run/secret/market", []byte(good), true); err == nil {
		t.Fatal("nonmountpoint accepted as exact mount")
	}
	for _, bad := range []string{strings.ReplaceAll(good, ",nodev", ""), strings.ReplaceAll(good, "- tmpfs", "- ext4"), strings.ReplaceAll(good, "/run/secret ", "/run/secret-other ")} {
		if err := validateMountInfo("/run/secret/market", []byte(bad), false); err == nil {
			t.Fatal("bad mount accepted")
		}
	}
	overlay := good + "11 10 8:1 / /run/secret/market rw - ext4 /dev/example rw\n"
	if err := validateMountInfo("/run/secret/market", []byte(overlay), false); err == nil {
		t.Fatal("ignored more-specific disk mount")
	}
}

func TestPackPublicAdminSnapshotAndUnlockRemapsDerivedSocketOnly(t *testing.T) {
	dir, _ := fixture(t)
	configPath := filepath.Join(dir, "operator.json")
	config, _ := os.ReadFile(configPath)
	var fields map[string]any
	if err := json.Unmarshal(config, &fields); err != nil {
		t.Fatal(err)
	}
	fields["admin_auth_dir"] = "admin-clients"
	fields["admin_socket_dir"] = filepath.Join(dir, "admin")
	config, _ = json.Marshal(fields)
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	authDir := filepath.Join(dir, "admin-clients")
	if err := os.Mkdir(authDir, 0755); err != nil {
		t.Fatal(err)
	}
	auth := []byte("descriptor:x25519:" + strings.Repeat("A", 52) + "\n")
	if err := os.WriteFile(filepath.Join(authDir, "operator.auth"), auth, 0444); err != nil {
		t.Fatal(err)
	}
	data, err := collect(dir)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "unlocked")
	if _, err := runFake(t, fakePlatform(), data, "unlock", "--dir", target); err != nil {
		t.Fatal(err)
	}
	config, _ = os.ReadFile(filepath.Join(target, "operator.json"))
	if err := json.Unmarshal(config, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["admin_socket_dir"] != filepath.Join(target, "admin") {
		t.Fatal("derived socket retained offline workstation path")
	}
	info, err := os.Stat(filepath.Join(target, "admin-clients/operator.auth"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unlock did not keep public records private until controller validation")
	}
}

func TestUnlockRejectsUnknownFieldsSymlinkAndTrailingInput(t *testing.T) {
	_, bundle := fixture(t)
	body, _ := json.Marshal(bundle)
	bad := bytes.Replace(body, []byte(`"version":1`), []byte(`"version":1,"unknown":"do-not-disclose"`), 1)
	for _, input := range [][]byte{bad, append(append([]byte{}, body...), []byte(` {}`)...), bytes.Repeat([]byte{'x'}, maxBundle+1)} {
		target := filepath.Join(t.TempDir(), "target")
		out, err := runFake(t, fakePlatform(), input, "unlock", "--dir", target)
		if err == nil || strings.Contains(err.Error(), "do-not-disclose") || strings.Contains(out, "do-not-disclose") {
			t.Fatal("invalid JSON accepted or echoed")
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatal("invalid JSON wrote state")
		}
	}
	target := filepath.Join(t.TempDir(), "target")
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, target); err != nil {
		t.Fatal(err)
	}
	if _, err := runFake(t, fakePlatform(), body, "unlock", "--dir", target); err == nil {
		t.Fatal("accepted symlink instance")
	}
}

func TestPackRejectsUnreferencedAndLinkedKeys(t *testing.T) {
	for _, linked := range []bool{false, true} {
		dir, _ := fixture(t)
		path := filepath.Join(dir, "secrets/extra")
		if linked {
			if err := os.Symlink(filepath.Join(dir, "secrets/app-secret-key"), path); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(path, []byte("should-not-be-bundled"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := collect(dir); err == nil {
			t.Fatal("packed unrelated or linked key")
		}
	}
}
