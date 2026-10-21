package operator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionAdminInstallWithPrivateUmask(t *testing.T) {
	const childMarker = "OPSECMKT_TEST_PRIVATE_UMASK"
	if os.Getenv(childMarker) != "1" {
		// Change only the child's mask; umask is process-wide and must not race
		// unrelated tests or their file creation in this test process.
		cmd := exec.Command("sh", "-c", `umask 077; exec "$@"`, "operator-umask-test", os.Args[0], "-test.run=^TestProductionAdminInstallWithPrivateUmask$", "-test.count=1")
		cmd.Env = append(os.Environ(), childMarker+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("production install under umask 077: %v\n%s", err, out)
		}
		return
	}
	fakeEncryptedProductionHost(t)
	inputs := t.TempDir()
	cert, key := testTLSMaterial(t, false)
	dbCA, _ := testTLSMaterial(t, false) // A-589: the database CA is not the upload CA
	for name, body := range map[string][]byte{"database.url": []byte("postgres://market:dedicated-password@db.internal/market?sslmode=verify-full"), "ca.pem": cert, "database-ca.pem": dbCA, "server.pem": cert, "server.key": key} {
		if err := os.WriteFile(filepath.Join(inputs, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), "production")
	args := []string{"init", "--dir", dir, "--mode", "prod", "--app-image", testImage, "--app-host", "app.internal", "--gateway-url", "http://gateway.example.onion", "--database-url-file", filepath.Join(inputs, "database.url"), "--database-ca", filepath.Join(inputs, "database-ca.pem"), "--tls-cert", filepath.Join(inputs, "server.pem"), "--tls-key", filepath.Join(inputs, "server.key"), "--upload-ca", filepath.Join(inputs, "ca.pem"), "--upload-cert", filepath.Join(inputs, "server.pem"), "--upload-key", filepath.Join(inputs, "server.key"), "--exposure", "tor", "--admin-tor-image", testImage, "--admin-client-auth-dir", adminAuthInput(t)}
	if out, err := invoke(t, args...); err != nil {
		t.Fatalf("init: %v (%s)", err, out)
	}
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, command := range []string{"install", "up"} {
		if out, err := invoke(t, command, "--dir", dir); err != nil {
			t.Fatalf("%s: %v (%s)", command, err, out)
		}
		for path, want := range map[string]os.FileMode{
			c.AdminSocketDir:                                    0750,
			filepath.Join(dir, c.AdminAuthDir):                  0755,
			filepath.Join(dir, c.AdminAuthDir, "operator.auth"): 0444,
		} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != want {
				t.Fatalf("runtime path permissions: %s: %v %v; want %o", path, info, err, want)
			}
		}
	}
}

func TestPrepareAdminSocketRefusesExistingUnsafePaths(t *testing.T) {
	for _, kind := range []string{"private-mode", "public-mode", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "admin")
			switch kind {
			case "private-mode", "public-mode":
				mode := os.FileMode(0700)
				if kind == "public-mode" {
					mode = 0755
				}
				if err := os.Mkdir(path, mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(path, []byte("retain"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			c := Config{AdminSocketDir: path, RuntimeUID: os.Geteuid(), RuntimeGID: os.Getegid()}
			if err := prepareAdminSocket(c); err == nil || !strings.Contains(err.Error(), "mode-0750") {
				t.Fatalf("existing unsafe path accepted: %v", err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("existing unsafe path modified: %v %v", after, err)
			}
		})
	}
}
