//go:build linux

package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"opsecmkt/internal/hostcontrol"
)

func ensureRootDirectory(path string, mode os.FileMode) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("host-agent directory must be absolute")
	}
	if path != "/" {
		if err := ensureRootDirectory(filepath.Dir(path), 0755); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		if err := os.Mkdir(path, mode); err != nil {
			return errors.New("cannot create private host-agent directory")
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	} else if err != nil {
		return errors.New("cannot inspect host-agent directory")
	}
	if err := trustedRootPath(path, true); err != nil {
		return err
	}
	if mode == 0700 {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != mode {
			return errors.New("host-agent state directory must have mode 0700")
		}
	}
	return nil
}

func verifyHostControlSocket(c Config) error {
	for _, target := range []struct {
		path   string
		socket bool
		mode   os.FileMode
	}{
		{c.HostControlDir, false, 0750}, {filepath.Join(c.HostControlDir, "control.sock"), true, 0660},
	} {
		info, err := os.Lstat(target.path)
		if err != nil {
			return errors.New("private host-agent socket is unavailable; application start refused")
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || st.Gid != uint32(c.RuntimeGID) || info.Mode().Perm() != target.mode ||
			(target.socket && info.Mode()&os.ModeSocket == 0) || (!target.socket && !info.IsDir()) {
			return errors.New("private host-agent socket/directory ownership or permissions are invalid; application start refused")
		}
	}
	return nil
}

func readHostRebootPolicy(dir string, c Config) (hostcontrol.UnlockPolicy, error) {
	var p hostcontrol.UnlockPolicy
	file := hostControlPolicyFile(c.Project)
	if err := trustedRootPath(file, false); err != nil {
		return p, err
	}
	if info, err := os.Lstat(file); err != nil || info.Mode().Perm() != 0600 {
		return p, errors.New("host-agent policy must have mode 0600")
	}
	b, err := privateFile(file, 4096)
	if err != nil {
		return p, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF || p.Version != 1 || p.DeploymentDir != dir {
		return p, errors.New("existing host-agent policy differs or is invalid; operator review required")
	}
	return p, nil
}

func hostAgentBinary(c Config) (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", errors.New("cannot locate persistent host-agent binary")
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return "", errors.New("cannot resolve persistent host-agent binary")
	}
	for _, path := range []string{"/tmp", "/var/tmp", "/run"} {
		if binary == path || strings.HasPrefix(binary, path+"/") {
			return "", errors.New("host-agent binary must be installed persistently, such as /usr/local/bin/opsecmkt")
		}
	}
	for current := binary; ; current = filepath.Dir(current) {
		if err := trustedRootPath(current, current != binary); err != nil {
			return "", err
		}
		if current == "/" {
			break
		}
	}
	evidence, err := operatorStorageInspect(binary)
	if err != nil {
		return "", errors.New("host-agent binary must reside on verified encrypted storage")
	}
	if err := checkStoragePins([]StorageBinding{storageBinding(binary, evidence)}, c.StorageBindings); err != nil {
		return "", err
	}
	return binary, nil
}

func ensureHostRebootService(ctx context.Context, dir string, c Config) error {
	if os.Geteuid() != 0 {
		return errors.New("host-agent setup requires root on the application host")
	}
	if err := trustedRootPath("/run/systemd/system", true); err != nil {
		return errors.New("host-agent setup requires systemd")
	}
	if err := verifyBootConfiguration(dir); err != nil {
		return err
	}
	if err := checkBoundStorage(dir, c); err != nil {
		return err
	}
	binary, err := hostAgentBinary(c)
	if err != nil {
		return err
	}
	unit, err := hostRebootUnit(binary, dir, c)
	if err != nil {
		return err
	}
	if err := ensureRootDirectory(filepath.Dir(hostControlPolicyFile(c.Project)), 0755); err != nil {
		return err
	}
	if err := ensureRootDirectory(hostControlStateDir(c.Project), 0700); err != nil {
		return err
	}
	policy := hostControlPolicyFile(c.Project)
	if _, err := os.Lstat(policy); os.IsNotExist(err) {
		b, err := json.Marshal(hostcontrol.UnlockPolicy{Version: 1, DeploymentDir: dir, UnlockRehearsed: false})
		if err != nil {
			return err
		}
		if err := writeExclusive(policy, append(b, '\n'), 0600); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Dir(policy)); err != nil {
			return err
		}
	} else if err != nil {
		return errors.New("cannot inspect host-agent policy")
	}
	if _, err := readHostRebootPolicy(dir, c); err != nil {
		return err
	}
	if err := trustedRootPath("/etc/systemd/system", true); err != nil {
		return err
	}
	name := "opsecmkt-host-control-" + c.Project + ".service"
	file := filepath.Join("/etc/systemd/system", name)
	if _, err := os.Lstat(file); err == nil {
		if err := trustedRootPath(file, false); err != nil {
			return err
		}
		b, err := os.ReadFile(file)
		if err != nil || string(b) != unit {
			return errors.New("existing host-agent unit differs; review paths and identity before replacing it")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("cannot inspect existing host-agent unit")
	} else if err := writeExclusive(file, []byte(unit), 0644); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", name}} {
		callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		cmd := exec.CommandContext(callCtx, "/usr/bin/systemctl", args...)
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		err := cmd.Run()
		cancel()
		if err != nil {
			return errors.New("cannot enable private host-agent service; no reboot was requested")
		}
	}
	// The notifying agent creates its socket directory itself without requiring
	// a host NSS group matching a container GID. Recheck before app startup.
	for i := 0; i < 50; i++ {
		if verifyHostControlSocket(c) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return verifyHostControlSocket(c)
}

func writeHostRebootReadiness(dir string, c Config) error {
	if os.Geteuid() != 0 {
		return errors.New("only root may acknowledge host unlock readiness")
	}
	if err := verifyBootConfiguration(dir); err != nil {
		return err
	}
	if err := checkBoundStorage(dir, c); err != nil {
		return err
	}
	p, err := readHostRebootPolicy(dir, c)
	if err != nil {
		return err
	}
	// Do not enable a new empty receipt history after losing existing state.
	ledger := filepath.Join(hostControlStateDir(c.Project), hostcontrol.LedgerFileName)
	if err := trustedRootPath(ledger, false); err != nil {
		return errors.New("start the disabled host agent and verify its persistent receipt ledger before marking readiness")
	}
	if _, err := privateFile(ledger, 8<<20); err != nil {
		return err
	}
	p.UnlockRehearsed = true
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || len(strings.TrimSpace(string(boot))) != 36 {
		return errors.New("cannot bind unlock readiness to the current Linux boot")
	}
	p.BootID = strings.TrimSpace(string(boot))
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return atomicWrite(hostControlPolicyFile(c.Project), append(b, '\n'), 0600)
}
