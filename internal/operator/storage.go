package operator

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"opsecmkt/internal/storageguard"
)

// StorageBinding pins the encryption boundary, not a device's unstable /dev name.
// These are public LUKS identifiers; unlock credentials are never retained here.
type StorageBinding struct {
	Path           string   `json:"path"`
	MountPoint     string   `json:"mount_point"`
	MountRoot      string   `json:"mount_root"`
	FilesystemUUID string   `json:"filesystem_uuid"`
	LUKSUUIDs      []string `json:"luks_uuids"`
}

var productionStorageProbe = inspectProductionStorage
var operatorStorageInspect = storageguard.Inspect
var localDockerStorage = inspectLocalDockerStorage
var installProductionBootGuard = ensureProductionBootGuard
var operatorSwapCheck = checkNoActiveSwap
var operatorContainerdRoots = discoverContainerdRoots
var operatorBootConfigCheck = verifyBootConfiguration

func inspectProductionStorage(ctx context.Context, dir string, c Config, imports []string) ([]StorageBinding, error) {
	// Requiring an encrypted OS root also covers host credentials, crash reports,
	// journals and containerd metadata that a dedicated application mount misses.
	if _, err := operatorStorageInspect("/"); err != nil {
		return nil, errors.New("production requires an active LUKS2-encrypted Linux OS root before setup; install the OS with encryption or migrate to a fresh encrypted host; no keys were generated and no disks were formatted")
	}
	if err := operatorSwapCheck(); err != nil {
		return nil, err
	}
	dockerRoot, err := localDockerStorage(ctx, c.DockerContext)
	if err != nil {
		return nil, err
	}
	paths := []string{"/", "/etc", "/root", "/root/.docker/config.json", "/var/log", "/var/lib/containerd", "/var/crash", "/var/lib/systemd/coredump", "/var/lib/apport/coredump", "/var/tmp", "/tmp", "/srv", dir, c.RetentionDir, dockerRoot,
		filepath.Join(dir, "secrets"), filepath.Join(dir, "onion-admin"), filepath.Join(dir, "onion-public"), filepath.Join(dir, "admin-clients")}
	if c.AdminTorImage != "" {
		// Named volumes may be separate mounts even when DockerRootDir is
		// encrypted. Pin the actual retained administrator identity files too.
		identity := filepath.Join(dockerRoot, "volumes", c.Project+"_tor_admin_identity", "_data")
		paths = append(paths, identity)
		for _, name := range []string{"hostname", "hs_ed25519_public_key", "hs_ed25519_secret_key"} {
			paths = append(paths, filepath.Join(identity, name))
		}
	}
	binary, err := os.Executable()
	if err != nil {
		return nil, errors.New("cannot identify the operator binary's persistent storage")
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return nil, errors.New("cannot resolve the operator binary's persistent storage")
	}
	paths = append(paths, binary)
	containerdRoots, err := operatorContainerdRoots()
	if err != nil {
		return nil, err
	}
	paths = append(paths, containerdRoots...)
	if c.AdminSocketDir != "" {
		paths = append(paths, c.AdminSocketDir)
	}
	paths = append(paths, declaredPrivateStoragePaths(dir, c)...)
	paths = append(paths, hostControlStoragePaths(c)...)
	// Resolve private imports before reading them. They are not runtime pins: a
	// later removal of an import source must not prevent a retained install boot.
	for _, path := range imports {
		if path != "" {
			if _, err := operatorStorageInspect(path); err != nil {
				return nil, errors.New("production private input is outside verified LUKS2 storage; move it onto protected storage before initialization")
			}
		}
	}
	seen := map[string]bool{}
	var bindings []StorageBinding
	for _, path := range paths {
		path, err = filepath.Abs(path)
		if err != nil || strings.ContainsAny(path, "\x00\r\n") {
			return nil, errors.New("cannot resolve production storage path")
		}
		path = filepath.Clean(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		evidence, err := operatorStorageInspect(path)
		if err != nil {
			return nil, fmt.Errorf("production storage is not verified LUKS2 at %s; services were not started", path)
		}
		bindings = append(bindings, storageBinding(path, evidence))
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Path < bindings[j].Path })
	return bindings, nil
}

func inspectLocalDockerStorage(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	query := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "docker", append([]string{"--context", name}, args...)...)
		cmd.Env = dockerEnvironment()
		var output boundedOutput
		output.limit = 16 << 10
		cmd.Stdout = &output
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil || output.truncated {
			return nil, errors.New("cannot verify local Docker storage; run production setup on the Linux Docker host with its daemon available")
		}
		return output.data, nil
	}
	b, err := query("context", "inspect", name, "--format", "{{json .Endpoints.docker.Host}}")
	if err != nil {
		return "", err
	}
	var endpoint string
	if json.Unmarshal(b, &endpoint) != nil || !strings.HasPrefix(endpoint, "unix:///") || strings.ContainsAny(endpoint, "\x00\r\n") {
		return "", errors.New("production storage cannot be verified through remote Docker; run setup on the Linux daemon host using a local unix socket")
	}
	b, err = query("info", "--format", `{"root":{{json .DockerRootDir}},"name":{{json .Name}},"os":{{json .OSType}}}`)
	if err != nil {
		return "", err
	}
	var info struct{ Root, Name, OS string }
	host, hostErr := os.Hostname()
	if json.Unmarshal(b, &info) != nil || hostErr != nil || info.Name != host || info.OS != "linux" || !filepath.IsAbs(info.Root) || strings.ContainsAny(info.Root, "\x00\r\n") {
		return "", errors.New("Docker daemon identity/storage does not match the local Linux host; production storage proof refused")
	}
	return info.Root, nil
}

func checkStoragePins(actual, expected []StorageBinding) error {
	if len(expected) == 0 {
		return errors.New("production installation has no encryption bindings; migrate its preserved state to an encrypted host, then run deploy storage bind --dir DIR")
	}
	for _, current := range actual {
		var parent *StorageBinding
		for i := range expected {
			bound := &expected[i]
			if current.Path == bound.Path || strings.HasPrefix(current.Path, strings.TrimSuffix(bound.Path, "/")+"/") {
				if parent == nil || len(bound.Path) > len(parent.Path) {
					parent = bound
				}
			}
		}
		if parent == nil || !sameStorageIdentity(current, *parent) {
			return fmt.Errorf("encrypted storage identity changed or its expected mount is missing at %s; inspect recovery state before explicitly rebinding", current.Path)
		}
	}
	return nil
}

func storageBinding(path string, evidence storageguard.Evidence) StorageBinding {
	return StorageBinding{Path: path, MountPoint: evidence.MountPoint, MountRoot: evidence.MountRoot, FilesystemUUID: evidence.FilesystemUUID, LUKSUUIDs: slices.Clone(evidence.LUKSUUIDs)}
}

func sameStorageIdentity(a, b StorageBinding) bool {
	return len(b.LUKSUUIDs) != 0 && b.MountPoint != "" && b.MountRoot != "" && b.FilesystemUUID != "" &&
		a.MountPoint == b.MountPoint && a.MountRoot == b.MountRoot && a.FilesystemUUID == b.FilesystemUUID && slices.Equal(a.LUKSUUIDs, b.LUKSUUIDs)
}

func checkProductionStorage(ctx context.Context, dir string, c Config) error {
	if c.Mode != "prod" {
		return nil
	}
	actual, err := productionStorageProbe(ctx, dir, c, nil)
	if err != nil {
		return err
	}
	return checkStoragePins(actual, c.StorageBindings)
}

// checkBoundStorage deliberately needs no running Docker API: systemd invokes
// it before starting Docker, using the paths pinned while the daemon was online.
func checkBoundStorage(dir string, c Config) error {
	if c.Mode != "prod" || len(c.StorageBindings) == 0 {
		return errors.New("storage boot checks require a production installation with verified encryption bindings")
	}
	if err := operatorSwapCheck(); err != nil {
		return err
	}
	root := false
	for _, binding := range c.StorageBindings {
		if binding.Path == "/" {
			root = true
		}
		evidence, err := operatorStorageInspect(binding.Path)
		if err != nil || !sameStorageIdentity(storageBinding(binding.Path, evidence), binding) {
			return fmt.Errorf("expected encrypted storage unavailable at %s; service startup refused", binding.Path)
		}
	}
	if !root {
		return errors.New("encrypted root binding is missing; service startup refused")
	}
	// Init pins the future secrets directory before keys exist. Check every
	// declared file as well at boot so a later plain bind mount over one key
	// cannot hide behind its still-encrypted parent directory.
	for _, path := range append(declaredPrivateStoragePaths(dir, c), hostControlStoragePaths(c)...) {
		evidence, err := operatorStorageInspect(path)
		if err != nil || checkStoragePins([]StorageBinding{storageBinding(path, evidence)}, c.StorageBindings) != nil {
			return errors.New("a declared private runtime file is outside its expected encrypted filesystem; startup refused")
		}
	}
	return nil
}

func declaredPrivateStoragePaths(dir string, c Config) []string {
	var paths []string
	for _, files := range []map[string]string{c.SecretFiles, c.TLSFiles} {
		for _, relative := range files {
			paths = append(paths, filepath.Join(dir, relative))
		}
	}
	for _, identity := range []string{c.OnionDir, c.AdminOnionDir} {
		if identity != "" {
			for _, name := range []string{"hostname", "hs_ed25519_public_key", "hs_ed25519_secret_key"} {
				paths = append(paths, filepath.Join(dir, identity, name))
			}
		}
	}
	sort.Strings(paths)
	return paths
}

func runBoundStorage(ctx context.Context, args []string, out, errOut io.Writer) error {
	f := flag.NewFlagSet("deploy storage "+args[0], flag.ContinueOnError)
	f.SetOutput(errOut)
	directory := f.String("dir", "./opsecmkt-deployment", "preserved production installation directory")
	binary := f.String("binary", "/usr/local/bin/opsecmkt", "absolute installed binary path for boot-unit output")
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("storage binding commands take flags only")
	}
	dir, err := installationPath(*directory)
	if err != nil {
		return err
	}
	if err := operatorBootConfigCheck(dir); err != nil {
		return err
	}
	c, err := loadConfig(dir)
	if err != nil {
		return err
	}
	if c.Mode != "prod" {
		return errors.New("storage bindings are for production installations")
	}
	if args[0] == "bind" {
		unlock, err := lockInstallation(dir)
		if err != nil {
			return err
		}
		defer unlock()
		c, err = loadConfig(dir)
		if err != nil {
			return err
		}
		c.StorageBindings, err = productionStorageProbe(ctx, dir, c, nil)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
		if err := atomicWrite(filepath.Join(dir, "operator.json"), append(b, '\n'), 0600); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "verified encryption identities bound; original application keys, endpoints, project and volumes retained")
		return err
	}
	if err := checkBoundStorage(dir, c); err != nil {
		return err
	}
	if args[0] == "check-bound" {
		_, err = fmt.Fprintln(out, "expected LUKS2 storage available")
		return err
	}
	unit, err := storageBootUnit(*binary, dir)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, unit)
	return err
}

func storageBootUnit(binary, dir string) (string, error) {
	if !filepath.IsAbs(binary) || strings.ContainsAny(binary, "\x00\r\n%$") || strings.ContainsAny(dir, "%$\x00\r\n") {
		return "", errors.New("boot-unit requires safe absolute binary and installation paths without systemd substitutions")
	}
	// Quote argv for systemd, never a shell. Substitutions are refused above.
	quote := func(value string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
	}
	return fmt.Sprintf("# OPSECMKT encrypted storage guard; failure stops startup and never changes disks or keys.\n[Service]\nExecStartPre=%s deploy storage check-bound --dir %s\n", quote(binary), quote(dir)), nil
}
