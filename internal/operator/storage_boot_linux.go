//go:build linux

package operator

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func trustedRootPath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("boot storage guard path is unavailable")
	}
	return trustedRootInfo(info, directory)
}

func trustedRootInfo(info os.FileInfo, directory bool) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || !directory && !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return errors.New("boot storage guard requires root-owned real files/directories without group or other write access")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 {
		return errors.New("boot storage guard paths must be owned by root")
	}
	return nil
}

func verifyBootConfiguration(dir string) error {
	for current := dir; ; current = filepath.Dir(current) {
		if err := trustedRootPath(current, true); err != nil {
			return errors.New("production boot configuration and its ancestors must be root-owned real directories without group/other write access")
		}
		if current == "/" {
			break
		}
	}
	if err := trustedRootPath(filepath.Join(dir, "operator.json"), false); err != nil {
		return errors.New("production operator.json must be a root-owned regular file without group/other write access")
	}
	return nil
}

func ensureProductionBootGuard(ctx context.Context, dir string, c Config) error {
	if os.Geteuid() != 0 {
		return errors.New("production service startup requires root to install the encrypted-storage systemd guards")
	}
	if err := trustedRootPath("/run/systemd/system", true); err != nil {
		return errors.New("production automatic restart protection requires a systemd Linux host")
	}
	if err := verifyBootConfiguration(dir); err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return errors.New("cannot locate the installed operator binary for boot protection")
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return errors.New("cannot resolve the installed operator binary")
	}
	for _, ephemeral := range []string{"/tmp", "/var/tmp", "/run"} {
		if binary == ephemeral || strings.HasPrefix(binary, ephemeral+"/") {
			return errors.New("install the operator binary in a persistent root-owned path such as /usr/local/bin/opsecmkt before starting production")
		}
	}
	for current := binary; ; current = filepath.Dir(current) {
		if err := trustedRootPath(current, current != binary); err != nil {
			return err
		}
		if current == "/" {
			break
		}
	}
	evidence, err := operatorStorageInspect(binary)
	if err != nil {
		return errors.New("the persistent operator binary must itself be on verified encrypted storage")
	}
	if err := checkStoragePins([]StorageBinding{storageBinding(binary, evidence)}, c.StorageBindings); err != nil {
		return err
	}
	if err := checkBoundStorage(dir, c); err != nil {
		return err
	}
	unit, err := storageBootUnit(binary, dir)
	if err != nil {
		return err
	}
	for _, service := range []string{"docker.service", "containerd.service"} {
		parent := filepath.Join("/etc/systemd/system", service+".d")
		if err := trustedRootPath("/etc/systemd/system", true); err != nil {
			return err
		}
		if err := os.Mkdir(parent, 0755); err != nil && !os.IsExist(err) {
			return errors.New("cannot prepare storage guard drop-in directory")
		}
		if err := trustedRootPath(parent, true); err != nil {
			return err
		}
		file := filepath.Join(parent, "50-opsecmkt-storage-"+c.Project+".conf")
		if _, err := os.Lstat(file); err == nil {
			if err := trustedRootPath(file, false); err != nil {
				return err
			}
			b, err := os.ReadFile(file)
			if err != nil || string(b) != unit {
				return errors.New("existing storage boot guard differs; review its preserved installation/binary paths before replacing it")
			}
		} else if !os.IsNotExist(err) {
			return errors.New("cannot inspect existing boot guard")
		} else if err := writeExclusive(file, []byte(unit), 0644); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", "daemon-reload")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		return errors.New("cannot load encrypted-storage boot guards; no Docker or application restart was requested")
	}
	return nil
}
