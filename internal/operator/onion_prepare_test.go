package operator

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func torIdentityPrepareArgs(source, destination string) []string {
	uid, gid := os.Geteuid(), os.Getegid()
	if uid == 0 {
		uid = 65532
	}
	if gid == 0 {
		gid = 65532
	}
	args := []string{"--destination", destination, "--uid", strconv.Itoa(uid), "--gid", strconv.Itoa(gid)}
	if source != "" {
		args = append(args, "--identity-source", source)
	}
	return args
}

func TestTorIdentityPrepareInitializesFreshVolumeAndNeverReplacesExistingIdentity(t *testing.T) {
	id, source := onionFixture(t)
	_, other := onionFixture(t)
	destination := t.TempDir()
	var out bytes.Buffer
	for i := 0; i < 2; i++ {
		if err := PrepareTorIdentity(torIdentityPrepareArgs(source, destination), &out); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(destination, "hs_ed25519_secret_key"))
		if err != nil || !bytes.Equal(got, id.SecretFile()) {
			t.Fatal("helper changed selected native Tor identity")
		}
	}
	if err := PrepareTorIdentity(torIdentityPrepareArgs(other, destination), &out); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("mismatch failed to refuse: %v", err)
	}
	if err := PrepareTorIdentity(torIdentityPrepareArgs("", destination), &out); err != nil {
		t.Fatalf("valid populated legacy identity was not preserved: %v", err)
	}
	for _, name := range onionFileNames {
		got, err := os.ReadFile(filepath.Join(destination, name))
		want, readErr := os.ReadFile(filepath.Join(source, name))
		if err != nil || readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("identity bytes replaced: %s", name)
		}
	}
	if bytes.Contains(out.Bytes(), id.SecretFile()) {
		t.Fatal("prepare helper printed private identity material")
	}
}

func TestTorIdentityPrepareRefusesLegacyEmptyPartialAndUnmanagedVolumes(t *testing.T) {
	id, source := onionFixture(t)
	for _, mode := range []string{"legacy-empty", "partial", "unmanaged"} {
		t.Run(mode, func(t *testing.T) {
			destination := t.TempDir()
			selected := source
			switch mode {
			case "legacy-empty":
				selected = ""
			case "partial":
				os.WriteFile(filepath.Join(destination, "hs_ed25519_secret_key"), id.SecretFile(), 0600)
			case "unmanaged":
				os.WriteFile(filepath.Join(destination, "unknown-identity-backup"), []byte("preserve"), 0600)
			}
			before, _ := os.ReadDir(destination)
			if PrepareTorIdentity(torIdentityPrepareArgs(selected, destination), &bytes.Buffer{}) == nil {
				t.Fatal("unsafe volume initialized without explicit adoption")
			}
			after, _ := os.ReadDir(destination)
			if len(after) != len(before) {
				t.Fatal("refusal added or replaced identity files")
			}
			if mode == "partial" {
				got, _ := os.ReadFile(filepath.Join(destination, "hs_ed25519_secret_key"))
				if !bytes.Equal(got, id.SecretFile()) {
					t.Fatal("partial existing secret was replaced")
				}
			}
		})
	}
}

func TestPublicTorIdentityPrepareUsesOnlyMarketplaceSubdirectory(t *testing.T) {
	id, source := onionFixture(t)
	destination := t.TempDir()
	args := append(torIdentityPrepareArgs(source, destination), "--subdir", "marketplace")
	if err := PrepareTorIdentity(args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "marketplace", "hostname"))
	if err != nil || !bytes.Equal(got, id.HostnameFile()) {
		t.Fatal("public identity prepared in wrong volume path")
	}
	for _, bad := range []string{"..", "../escape", "nested/role", "/outside"} {
		args := append(torIdentityPrepareArgs(source, destination), "--subdir", bad)
		if PrepareTorIdentity(args, &bytes.Buffer{}) == nil {
			t.Fatal("unsafe service subdirectory accepted")
		}
	}
}

func TestAdminTorPreparePinsSelectedIdentityBeforeAuthorizationUpdate(t *testing.T) {
	id, source := onionFixture(t)
	_, other := onionFixture(t)
	destination := t.TempDir()
	auth := adminAuthInput(t)
	args := append(torIdentityPrepareArgs(source, destination), "--source", auth)
	if err := PrepareAdminTor(args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(destination, "authorized_clients", "operator.auth"))
	args = append(torIdentityPrepareArgs(other, destination), "--source", auth)
	if err := PrepareAdminTor(args, &bytes.Buffer{}); err == nil {
		t.Fatal("admin helper changed populated onion identity")
	}
	secret, _ := os.ReadFile(filepath.Join(destination, "hs_ed25519_secret_key"))
	after, _ := os.ReadFile(filepath.Join(destination, "authorized_clients", "operator.auth"))
	if !bytes.Equal(secret, id.SecretFile()) || !bytes.Equal(before, after) {
		t.Fatal("mismatch changed identity or authorization")
	}
}
