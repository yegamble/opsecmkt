package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"opsecmkt/internal/onionidentity"
)

func onionFixture(t *testing.T) (onionidentity.Identity, string) {
	t.Helper()
	id, err := onionidentity.Generate(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "identity")
	if err := id.WriteNew(dir); err != nil {
		t.Fatal(err)
	}
	return id, dir
}

func TestInitGeneratesSeparateOnionRolesAndPreservesSourcesAcrossLifecycle(t *testing.T) {
	dir := initAdminTor(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.OnionDir != "onion-public" || c.AdminOnionDir != "onion-admin" || c.OnionAddress == c.AdminOnionAddress {
		t.Fatalf("onion roles share identity or paths: %#v", c)
	}
	before := map[string][]byte{}
	for _, role := range []struct{ path, address string }{{c.OnionDir, c.OnionAddress}, {c.AdminOnionDir, c.AdminOnionAddress}} {
		id, err := readManagedOnion(dir, role.path, role.address)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := invoke(t, "plan", "--dir", dir)
		if err != nil || !strings.Contains(plan, id.Address) || !strings.Contains(plan, "Back up "+role.path) || bytes.Contains([]byte(plan), id.SecretFile()) {
			t.Fatalf("plan omitted public address or exposed identity secret: %v", err)
		}
		for _, name := range onionFileNames {
			path := filepath.Join(role.path, name)
			before[path], err = os.ReadFile(filepath.Join(dir, path))
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(dir, path))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("onion backup source is not private")
			}
		}
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, args := range [][]string{{"install", "--dir", dir}, {"up", "--dir", dir}, {"redeploy", "--dir", dir, "--app-image", testImage}} {
		if out, err := invoke(t, args...); err != nil {
			t.Fatalf("%s: %v %s", args[0], err, out)
		}
	}
	for path, want := range before {
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("lifecycle rotated onion identity: %s", path)
		}
	}
	compose, _, err := render(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	json.Unmarshal(compose, &object)
	services := object["services"].(map[string]any)
	appMounts, _ := json.Marshal(services["app"].(map[string]any)["volumes"])
	if strings.Contains(string(appMounts), "onion-") {
		t.Fatal("application receives onion secret identity mounts")
	}
	public := services["tor-identity-prepare"].(map[string]any)
	publicMounts, _ := json.Marshal(public["volumes"])
	if public["network_mode"] != "none" || len(public["volumes"].([]any)) != 2 || strings.Contains(string(publicMounts), "onion-admin") {
		t.Fatal("public onion helper crosses roles or networks")
	}
}

func TestInitOnionImportPrefixAndInvalidRoleRefuseBeforePublication(t *testing.T) {
	id, source := onionFixture(t)
	base := []string{"--mode", "dev", "--app-image", testImage, "--exposure", "tor", "--tor-image", testImage}
	importFlags := []string{"--onion-address", id.Address, "--onion-public-key", filepath.Join(source, "hs_ed25519_public_key"), "--onion-secret-key", filepath.Join(source, "hs_ed25519_secret_key")}
	dir := filepath.Join(t.TempDir(), "imported")
	args := append([]string{"init", "--dir", dir}, base...)
	args = append(args, importFlags...)
	if out, err := invoke(t, args...); err != nil {
		t.Fatalf("import: %v %s", err, out)
	}
	c, err := loadConfig(dir)
	if err != nil || c.OnionAddress != id.Address {
		t.Fatalf("import changed requested identity: %v", err)
	}
	imported, _ := os.ReadFile(filepath.Join(dir, c.OnionDir, "hs_ed25519_secret_key"))
	if !bytes.Equal(imported, id.SecretFile()) {
		t.Fatal("import changed Tor expanded secret")
	}
	for _, flags := range [][]string{{"--onion-prefix", "abcdefg"}, {"--onion-prefix", "Bad"}, {"--onion-address", id.Address}, append(append([]string{}, importFlags...), "--onion-prefix", "a"), {"--admin-onion-prefix", "a"}} {
		bad := filepath.Join(t.TempDir(), "invalid")
		args := append([]string{"init", "--dir", bad}, base...)
		args = append(args, flags...)
		if _, err := invoke(t, args...); err == nil {
			t.Fatalf("invalid onion configuration accepted: %v", flags)
		}
		if _, err := os.Stat(bad); !os.IsNotExist(err) {
			t.Fatal("invalid onion import published configuration")
		}
	}
	prefixDir := filepath.Join(t.TempDir(), "prefixed")
	args = append([]string{"init", "--dir", prefixDir}, base...)
	args = append(args, "--onion-prefix", "a")
	if _, err := invoke(t, args...); err != nil {
		t.Fatal(err)
	}
	prefixed, err := loadConfig(prefixDir)
	if err != nil || !strings.HasPrefix(prefixed.OnionAddress, "a") {
		t.Fatalf("prefix not respected: %v", err)
	}
}

func TestInitRefusesSharedPublicAdminIdentityAndMismatchedAddress(t *testing.T) {
	id, source := onionFixture(t)
	other, _ := onionFixture(t)
	for _, address := range []string{id.Address, other.Address} {
		dir := filepath.Join(t.TempDir(), "refused")
		args := []string{"init", "--dir", dir, "--mode", "dev", "--app-image", testImage, "--exposure", "tor", "--tor-image", testImage, "--admin-tor-image", testImage, "--admin-client-auth-dir", adminAuthInput(t)}
		for _, role := range []string{"", "admin-"} {
			args = append(args, "--"+role+"onion-address", address, "--"+role+"onion-public-key", filepath.Join(source, "hs_ed25519_public_key"), "--"+role+"onion-secret-key", filepath.Join(source, "hs_ed25519_secret_key"))
		}
		if _, err := invoke(t, args...); err == nil {
			t.Fatal("shared or mismatched onion identity accepted")
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("refused identity published installation")
		}
	}
}

func TestInitOnionGenerationHonorsCancellationAndRemovesStaging(t *testing.T) {
	parent := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	err := Run(ctx, []string{"init", "--dir", filepath.Join(parent, "cancelled"), "--app-image", testImage, "--tor-image", testImage, "--exposure", "tor", "--onion-prefix", "abcdef"}, strings.NewReader(""), &out, &out)
	if err == nil {
		t.Fatal("cancelled onion search succeeded")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal("cancelled onion search left plaintext staging or published keys")
	}
}
