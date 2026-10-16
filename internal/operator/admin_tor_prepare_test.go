package operator

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"opsecmkt/internal/onionidentity"
)

func runAdminTorPrepare(source, destination string) error {
	uid, gid := os.Geteuid(), os.Getegid()
	if uid == 0 {
		uid = 65532
	}
	if gid == 0 {
		gid = 65532
	}
	return PrepareAdminTor([]string{"--source", source, "--destination", destination, "--uid", strconv.Itoa(uid), "--gid", strconv.Itoa(gid)}, &bytes.Buffer{})
}

func TestAdminTorPreparePreservesIdentityAndReplacesOnlyClientAuthorization(t *testing.T) {
	source := adminAuthInput(t)
	destination := t.TempDir()
	selected, err := onionidentity.Generate(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	identity := selected.SecretFile()
	key := filepath.Join(destination, "hs_ed25519_secret_key")
	if err := os.WriteFile(key, identity, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "hs_ed25519_public_key"), selected.PublicFile(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "hostname"), selected.HostnameFile(), 0600); err != nil {
		t.Fatal(err)
	}
	clients := filepath.Join(destination, "authorized_clients")
	if err := os.Mkdir(clients, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clients, "revoked.auth"), []byte(validAdminAuth), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := runAdminTorPrepare(source, destination); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(key)
		if err != nil || !bytes.Equal(got, identity) {
			t.Fatalf("identity replaced: %q %v", got, err)
		}
		entries, err := os.ReadDir(clients)
		if err != nil || len(entries) != 1 || entries[0].Name() != "operator.auth" {
			t.Fatalf("authorization reconciliation failed: %#v %v", entries, err)
		}
		got, err = os.ReadFile(filepath.Join(clients, "operator.auth"))
		if err != nil || string(got) != validAdminAuth {
			t.Fatalf("public authorization changed: %q %v", got, err)
		}
	}
	for path, want := range map[string]os.FileMode{destination: 0700, clients: 0700, key: 0600, filepath.Join(clients, "operator.auth"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("Tor permissions invalid: %s %v %v", path, info, err)
		}
	}
}

func TestAdminTorPrepareInvalidSnapshotDoesNotTouchVolume(t *testing.T) {
	for _, body := range []string{"", "descriptor:x25519:bad\n"} {
		source, destination := t.TempDir(), t.TempDir()
		if body != "" {
			if err := os.WriteFile(filepath.Join(source, "operator.auth"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(destination, 0755); err != nil {
			t.Fatal(err)
		}
		if runAdminTorPrepare(source, destination) == nil {
			t.Fatal("invalid authorization permitted Tor startup")
		}
		entries, err := os.ReadDir(destination)
		if err != nil || len(entries) != 0 {
			t.Fatal("invalid source modified persistent identity volume")
		}
		info, _ := os.Stat(destination)
		if info.Mode().Perm() != 0755 {
			t.Fatal("invalid source changed persistent identity permissions")
		}
	}
}

func TestAdminTorPrepareRefusesIdentitySymlinkWithoutFollowingIt(t *testing.T) {
	for _, name := range []string{"authorized_clients", "hs_ed25519_secret_key"} {
		destination, outside := t.TempDir(), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(destination, name)); err != nil {
			t.Fatal(err)
		}
		if runAdminTorPrepare(adminAuthInput(t), destination) == nil {
			t.Fatal("symlink identity entry accepted")
		}
		entries, _ := os.ReadDir(outside)
		if len(entries) != 0 {
			t.Fatal("helper followed symlink outside its dedicated volume")
		}
	}
}
