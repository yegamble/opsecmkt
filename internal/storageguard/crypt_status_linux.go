//go:build linux

package storageguard

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func verifyCryptTarget(device, node string) error {
	if os.Geteuid() != 0 {
		return errors.New("crypt target verification requires Linux root")
	}
	name := filepath.Base(node)
	number := strings.TrimPrefix(name, "dm-")
	if !strings.HasPrefix(name, "dm-") || number == "" {
		return errors.New("crypt target must be an actual device-mapper block device")
	}
	if _, err := strconv.ParseUint(number, 10, 32); err != nil {
		return errors.New("invalid device-mapper block device name")
	}
	path, err := cryptDevicePath(device, node, name)
	if err != nil {
		return err
	}
	binary, err := cryptsetupBinary()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "status", path)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	var output statusOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if cmd.Run() != nil || output.exceeded {
		return errors.New("cryptsetup status failed or exceeded its inspection limit")
	}
	return validateCryptStatus(output.data)
}

func cryptDevicePath(device, node, kernelName string) (string, error) {
	paths := []string{filepath.Join("/dev", kernelName)}
	// Minimal systems without udev may expose only cryptsetup's mapper node.
	// Accept it only when its block-device identity matches this kernel map.
	name, err := boundedRead(filepath.Join(node, "dm", "name"), 128)
	if err == nil {
		name := strings.TrimSpace(string(name))
		if name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00\r\n") {
			paths = append(paths, filepath.Join("/dev/mapper", name))
		}
	}
	for _, path := range paths {
		var st unix.Stat_t
		if unix.Stat(path, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFBLK && device == formatDevice(uint64(st.Rdev)) {
			return path, nil
		}
	}
	return "", errors.New("crypt target device node does not match the inspected mapping")
}

func cryptsetupBinary() (string, error) {
	for _, path := range []string{"/usr/sbin/cryptsetup", "/sbin/cryptsetup"} {
		info, err := os.Lstat(path)
		var st unix.Stat_t
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0022 == 0 && info.Mode().Perm()&0111 != 0 && unix.Lstat(path, &st) == nil && st.Uid == 0 && trustedBinaryParents(path) {
			return path, nil
		}
	}
	return "", errors.New("install a trusted root-owned cryptsetup binary in the system sbin directory")
}

func trustedBinaryParents(path string) bool {
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		var st unix.Stat_t
		if unix.Lstat(parent, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != 0 || st.Mode&0022 != 0 {
			return false
		}
		if parent == "/" {
			return true
		}
	}
}

type statusOutput struct {
	data     []byte
	exceeded bool
}

func (o *statusOutput) Write(data []byte) (int, error) {
	if len(o.data)+len(data) > 16<<10 {
		o.exceeded = true
		return 0, io.ErrShortBuffer
	}
	o.data = append(o.data, data...)
	return len(data), nil
}
