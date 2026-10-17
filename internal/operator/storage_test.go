package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"opsecmkt/internal/storageguard"
)

const testStorageUUID = "01234567-89ab-cdef-0123-456789abcdef"

func testStorageEvidence(path string) storageguard.Evidence {
	return storageguard.Evidence{Path: path, MountPoint: "/", MountRoot: "/", FilesystemUUID: "abcdef01-2345-6789-abcd-ef0123456789", LUKSUUIDs: []string{testStorageUUID}}
}

func testStorageBinding(path string) StorageBinding {
	return storageBinding(path, testStorageEvidence(path))
}

func fakeEncryptedProductionHost(t *testing.T) {
	t.Helper()
	previousProbe, previousInstall, previousInspect := productionStorageProbe, installProductionBootGuard, operatorStorageInspect
	previousSwap := operatorSwapCheck
	previousBootConfig := operatorBootConfigCheck
	productionStorageProbe = func(_ context.Context, dir string, c Config, _ []string) ([]StorageBinding, error) {
		return []StorageBinding{testStorageBinding("/"), testStorageBinding(dir), testStorageBinding(c.RetentionDir)}, nil
	}
	installProductionBootGuard = func(context.Context, string, Config) error { return nil }
	operatorStorageInspect = func(path string) (storageguard.Evidence, error) {
		return testStorageEvidence(path), nil
	}
	operatorSwapCheck = func() error { return nil }
	operatorBootConfigCheck = func(string) error { return nil }
	t.Cleanup(func() {
		productionStorageProbe, installProductionBootGuard, operatorStorageInspect = previousProbe, previousInstall, previousInspect
		operatorSwapCheck = previousSwap
		operatorBootConfigCheck = previousBootConfig
	})
}

func TestProductionInitRefusesPlainStorageBeforeStagingOrImports(t *testing.T) {
	previous := productionStorageProbe
	t.Cleanup(func() { productionStorageProbe = previous })
	productionStorageProbe = func(_ context.Context, _ string, _ Config, imports []string) ([]StorageBinding, error) {
		if !slicesContain(imports, "/must-not-be-read/private.key") {
			t.Fatal("private imports omitted from storage check")
		}
		return nil, errors.New("test unencrypted host")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "production")
	_, err := invoke(t, "init", "--dir", dir, "--mode", "prod", "--app-image", testImage, "--tls-key", "/must-not-be-read/private.key")
	if err == nil || err.Error() != "test unencrypted host" {
		t.Fatalf("expected encryption refusal before private input read: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed preflight wrote/staged state: %v %v", entries, err)
	}
}

func TestInvalidModeCannotStageImportedKeysBeforeRefusal(t *testing.T) {
	parent := t.TempDir()
	_, err := invoke(t, "init", "--mode", "PROd", "--dir", filepath.Join(parent, "new"), "--app-image", testImage, "--app-secret-key-file", "/must-not-be-read/private.key")
	if err == nil || !strings.Contains(err.Error(), "mode must be dev or prod") {
		t.Fatalf("invalid mode reached key import: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid mode staged private state: %v %v", entries, err)
	}
}

func slicesContain(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func TestProductionStartCommandsRefuseChangedStorageBeforeDockerOrWrites(t *testing.T) {
	dir := initProduction(t)
	before, err := os.ReadFile(filepath.Join(dir, "operator.json"))
	if err != nil {
		t.Fatal(err)
	}
	productionStorageProbe = func(context.Context, string, Config, []string) ([]StorageBinding, error) {
		return []StorageBinding{{Path: "/", LUKSUUIDs: []string{"different-storage"}}}, nil
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "called")
	script := "#!/bin/sh\nprintf called > '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, command := range []string{"install", "up", "doctor", "redeploy"} {
		args := []string{command, "--dir", dir}
		if command == "redeploy" {
			args = append(args, "--app-image", strings.Replace(testImage, "aaaa", "bbbb", 1))
		}
		if _, err := invoke(t, args...); err == nil || !strings.Contains(err.Error(), "identity changed") {
			t.Fatalf("%s bypassed changed storage: %v", command, err)
		}
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatal("storage failure invoked Docker")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "operator.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("refusal changed preserved configuration")
	}
	if _, err := invoke(t, "plan", "--dir", dir); err != nil {
		t.Fatalf("read-only plan unavailable during recovery: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "app-secret-key")); err != nil {
		t.Fatal("storage failure removed original keys")
	}
}

func TestStoragePinsRejectMissingReplacedAndNestedPlainBoundaries(t *testing.T) {
	data := testStorageBinding("/data")
	data.LUKSUUIDs, data.MountPoint = []string{"data-uuid"}, "/data"
	bound := []StorageBinding{testStorageBinding("/"), data}
	for _, actual := range [][]StorageBinding{
		{{Path: "/data/new/key", LUKSUUIDs: []string{testStorageUUID}}},
		{{Path: "/other", LUKSUUIDs: nil}},
		{{Path: "/other", LUKSUUIDs: []string{"different"}}},
	} {
		if checkStoragePins(actual, bound) == nil {
			t.Fatalf("accepted changed boundary: %#v", actual)
		}
	}
	child := data
	child.Path = "/data/new/key"
	if err := checkStoragePins([]StorageBinding{child}, bound); err != nil {
		t.Fatal(err)
	}
	if checkStoragePins(bound, nil) == nil {
		t.Fatal("grandfathered an installation with no encryption proof")
	}
}

func TestStorageBindPreservesKeysConfigAndReceipt(t *testing.T) {
	dir := initProduction(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.StorageBindings = nil // Existing installation, migrated with its original keys.
	b, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(dir, "operator.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(filepath.Join(dir, "secrets", "app-secret-key"))
	if _, err := invoke(t, "storage", "bind", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	after, err := loadConfig(dir)
	if err != nil || len(after.StorageBindings) == 0 {
		t.Fatalf("bindings not saved: %v", err)
	}
	after.StorageBindings = nil
	if !reflect.DeepEqual(c, after) {
		t.Fatal("binding changed existing configuration")
	}
	retained, _ := os.ReadFile(filepath.Join(dir, "secrets", "app-secret-key"))
	if !bytes.Equal(key, retained) {
		t.Fatal("binding rotated an application key")
	}
}

func TestPinsRejectWrongFilesystemWithinSameLUKSContainer(t *testing.T) {
	bound := testStorageBinding("/srv/state")
	bound.MountPoint = "/srv/state"
	for _, change := range []func(*StorageBinding){
		func(b *StorageBinding) { b.MountPoint = "/" },
		func(b *StorageBinding) { b.FilesystemUUID = "different-filesystem" },
		func(b *StorageBinding) { b.MountRoot = "/wrong-bind-source" },
	} {
		actual := bound
		change(&actual)
		if checkStoragePins([]StorageBinding{actual}, []StorageBinding{bound}) == nil {
			t.Fatalf("same crypt UUID bypassed mount identity pin: %#v", actual)
		}
	}
}

func TestStorageBootCheckDoesNotNeedDockerAndNeverDestroys(t *testing.T) {
	dir := initProduction(t)
	if _, err := invoke(t, "storage", "check-bound", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	operatorStorageInspect = func(string) (storageguard.Evidence, error) {
		return storageguard.Evidence{}, errors.New("locked or missing")
	}
	if _, err := invoke(t, "storage", "check-bound", "--dir", dir); err == nil || !strings.Contains(err.Error(), "startup refused") {
		t.Fatalf("boot guard failed open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "operator.json")); err != nil {
		t.Fatal("boot failure removed configuration")
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "app-secret-key")); err != nil {
		t.Fatal("boot failure removed a key")
	}
}

func TestBootChecksDeclaredKeyBeyondItsEncryptedParent(t *testing.T) {
	dir := initProduction(t)
	operatorStorageInspect = func(path string) (storageguard.Evidence, error) {
		if path == filepath.Join(dir, "secrets", "app-secret-key") {
			return storageguard.Evidence{}, errors.New("plain file bind mount")
		}
		return testStorageEvidence(path), nil
	}
	if _, err := invoke(t, "storage", "check-bound", "--dir", dir); err == nil || !strings.Contains(err.Error(), "private runtime file") {
		t.Fatalf("boot skipped declared key storage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "app-secret-key")); err != nil {
		t.Fatal("failed boot check modified the key")
	}
}

func TestStorageBootUnitUsesReadOnlyCommandAndRefusesSubstitutions(t *testing.T) {
	unit, err := storageBootUnit("/usr/local/bin/opsecmkt", "/srv/market with spaces")
	if err != nil || !strings.Contains(unit, `ExecStartPre="/usr/local/bin/opsecmkt" deploy storage check-bound --dir "/srv/market with spaces"`) {
		t.Fatalf("invalid systemd guard: %v %s", err, unit)
	}
	for _, forbidden := range []string{"luksFormat", "destroy", "emergency", "wipe", "docker info"} {
		if strings.Contains(unit, forbidden) {
			t.Fatalf("boot unit contains mutating or circular dependency %q", forbidden)
		}
	}
	if _, err := storageBootUnit("/usr/bin/evil%h", "/srv/market"); err == nil {
		t.Fatal("systemd substitution accepted")
	}
}

func TestContainerdRootRejectsImportsAndResolvesExplicitRoots(t *testing.T) {
	for _, tc := range []struct {
		name, config, want string
		argv               []string
		refuse             bool
	}{
		{name: "default", config: "version=2\n", argv: []string{"containerd"}, want: "/var/lib/containerd"},
		{name: "custom", config: "version=2\nroot=\"/secure/content\"\n[grpc]\nroot=\"ignored\"\n", argv: []string{"containerd"}, want: "/secure/content"},
		{name: "quoted key", config: "\"root\"=\"/secure/content\"\n", argv: []string{"containerd"}, want: "/secure/content"},
		{name: "flag", config: "imports=[\"/other.toml\"]\n", argv: []string{"containerd", "--root=/secure/actual"}, want: "/secure/actual"},
		{name: "imports", config: "root=\"/var/lib/containerd\"\nimports=[\"/plain-root.toml\"]\n", argv: []string{"containerd"}, refuse: true},
		{name: "quoted imports", config: "\"imports\"=[\"/plain-root.toml\"]\n", argv: []string{"containerd"}, refuse: true},
		{name: "relative config", argv: []string{"containerd", "--config", "relative.toml"}, refuse: true},
		{name: "relative root", config: "root=\"relative\"\n", argv: []string{"containerd"}, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := containerdRoot(tc.argv, func(string, int64) ([]byte, error) { return []byte(tc.config), nil })
			if tc.refuse {
				if err == nil {
					t.Fatal("unverifiable containerd configuration accepted")
				}
			} else if err != nil || root != tc.want {
				t.Fatalf("root=%q err=%v, want %q", root, err, tc.want)
			}
		})
	}
}

func TestBootRefusesActiveSwapWithoutDeletingState(t *testing.T) {
	dir := initProduction(t)
	operatorSwapCheck = func() error { return errors.New("active swap refused") }
	if _, err := invoke(t, "storage", "check-bound", "--dir", dir); err == nil || !strings.Contains(err.Error(), "active swap") {
		t.Fatalf("boot check bypassed swap policy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "app-secret-key")); err != nil {
		t.Fatal("swap refusal removed retained keys")
	}
}

func TestProductionProbeChecksActualStoresAndPrivateImports(t *testing.T) {
	fakeEncryptedProductionHost(t)
	previousDocker, previousContainerd := localDockerStorage, operatorContainerdRoots
	t.Cleanup(func() { localDockerStorage, operatorContainerdRoots = previousDocker, previousContainerd })
	localDockerStorage = func(context.Context, string) (string, error) { return "/protected/docker", nil }
	operatorContainerdRoots = func() ([]string, error) { return []string{"/protected/containerd"}, nil }
	var observed []string
	operatorStorageInspect = func(path string) (storageguard.Evidence, error) {
		observed = append(observed, path)
		return testStorageEvidence(path), nil
	}
	c := Config{DockerContext: "default", RetentionDir: "/protected/ledger", Project: "test-project", AdminTorImage: testImage}
	if _, err := inspectProductionStorage(context.Background(), "/protected/app", c, []string{"/protected/input.key"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/protected/app", "/protected/docker", "/protected/docker/volumes/test-project_tor_admin_identity/_data/hs_ed25519_secret_key", "/protected/containerd", "/protected/ledger", "/protected/input.key", "/var/crash", "/var/lib/systemd/coredump", "/var/tmp"} {
		if !slicesContain(observed, path) {
			t.Fatalf("missing storage boundary: %s", path)
		}
	}
}
