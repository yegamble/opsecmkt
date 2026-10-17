//go:build linux

package storageguard

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func inspectFilesystemUUID(device, node string) (string, error) {
	if os.Geteuid() != 0 {
		return "", errors.New("filesystem identity verification requires Linux root")
	}
	var binary string
	for _, candidate := range []string{"/usr/sbin/blkid", "/sbin/blkid", "/usr/bin/blkid", "/bin/blkid"} {
		info, err := os.Lstat(candidate)
		var st unix.Stat_t
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0022 == 0 && info.Mode().Perm()&0111 != 0 && unix.Lstat(candidate, &st) == nil && st.Uid == 0 && trustedBinaryParents(candidate) {
			binary = candidate
			break
		}
	}
	if binary == "" {
		return "", errors.New("install a trusted root-owned blkid binary in a system executable directory")
	}
	// /dev/block also covers partitions and logical volumes above encryption.
	// A matching device number is required before probing its filesystem header.
	paths := []string{filepath.Join("/dev/block", device), filepath.Join("/dev", filepath.Base(node))}
	if mapped, err := cryptDevicePath(device, node, filepath.Base(node)); err == nil {
		paths = append(paths, mapped)
	}
	var path string
	for _, candidate := range paths {
		var st unix.Stat_t
		if unix.Stat(candidate, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFBLK && formatDevice(uint64(st.Rdev)) == device {
			path = candidate
			break
		}
	}
	if path == "" {
		return "", errors.New("filesystem block node does not match the actual mounted device")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--probe", "--match-tag", "UUID", "--output", "value", path)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	var output filesystemUUIDOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if cmd.Run() != nil || output.exceeded {
		return "", errors.New("filesystem UUID header probe failed or exceeded its inspection limit")
	}
	uuid := strings.TrimSpace(string(output.data))
	if !canonicalUUID(uuid) {
		return "", errors.New("filesystem header did not report one canonical UUID")
	}
	return uuid, nil
}

type filesystemUUIDOutput struct {
	data     []byte
	exceeded bool
}

func (o *filesystemUUIDOutput) Write(data []byte) (int, error) {
	if len(o.data)+len(data) > 64 {
		o.exceeded = true
		return 0, io.ErrShortBuffer
	}
	o.data = append(o.data, data...)
	return len(data), nil
}
