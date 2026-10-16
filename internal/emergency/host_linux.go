//go:build linux

package emergency

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Keep the original block device open throughout the manual operation. Every
// cryptsetup child inherits that descriptor at fd 3, so an intervening rename
// of the operator-supplied device path cannot redirect the erase command.
type systemHost struct {
	device *os.File
	path   string
	rdev   uint64
	dev    uint64
	ino    uint64
	uuid   string
}

const inheritedDevicePath = "/proc/self/fd/3"

func (h *systemHost) Close() error {
	if h.device == nil {
		return nil
	}
	err := h.device.Close()
	h.device = nil
	h.path, h.uuid = "", ""
	return err
}

func cryptsetupBinary() (string, error) {
	for _, path := range []string{"/usr/sbin/cryptsetup", "/sbin/cryptsetup"} {
		info, err := os.Stat(path)
		var stat unix.Stat_t
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0022 == 0 && info.Mode().Perm()&0111 != 0 && unix.Stat(path, &stat) == nil && stat.Uid == 0 {
			return path, nil
		}
	}
	return "", errors.New("emergency: install cryptsetup in the system sbin directory")
}

func (h *systemHost) checkDevice(ctx context.Context, path string) error {
	if os.Geteuid() != 0 {
		return errors.New("emergency: LUKS operations require root on Linux")
	}
	var node unix.Stat_t
	if unix.Lstat(path, &node) != nil || node.Mode&unix.S_IFMT != unix.S_IFBLK {
		return errors.New("emergency: device must be an existing block device without a symlink")
	}
	if h.device != nil {
		if path != h.path || !h.sameDevice(&node) {
			return errors.New("emergency: selected block device changed after initial validation")
		}
		uuid, err := readLUKSIdentity(ctx, h.device)
		if err != nil {
			return err
		}
		if uuid != h.uuid {
			return errors.New("emergency: selected LUKS identity changed after initial validation")
		}
		return nil
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("emergency: cannot hold the selected block device open")
	}
	device := os.NewFile(uintptr(fd), "emergency-luks-device")
	keep := false
	defer func() {
		if !keep {
			device.Close()
		}
	}()
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || opened.Mode&unix.S_IFMT != unix.S_IFBLK || opened.Rdev != node.Rdev || opened.Dev != node.Dev || opened.Ino != node.Ino {
		return errors.New("emergency: selected block device changed while opening it")
	}
	uuid, err := readLUKSIdentity(ctx, device)
	if err != nil {
		return err
	}
	h.device, h.path, h.rdev, h.dev, h.ino, h.uuid = device, path, opened.Rdev, opened.Dev, opened.Ino, uuid
	keep = true
	return nil
}

func (h *systemHost) sameDevice(node *unix.Stat_t) bool {
	var held unix.Stat_t
	return node.Rdev == h.rdev && node.Dev == h.dev && node.Ino == h.ino &&
		unix.Fstat(int(h.device.Fd()), &held) == nil && held.Mode&unix.S_IFMT == unix.S_IFBLK &&
		held.Rdev == h.rdev && held.Dev == h.dev && held.Ino == h.ino
}

func readLUKSIdentity(ctx context.Context, device *os.File) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	binary, err := cryptsetupBinary()
	if err != nil {
		return "", err
	}
	cmd := deviceCommand(ctx, binary, device, "isLuks", inheritedDevicePath)
	if cmd.Run() != nil {
		return "", errors.New("emergency: target is not a readable LUKS block device")
	}
	var output uuidOutput
	cmd = deviceCommand(ctx, binary, device, "luksUUID", inheritedDevicePath)
	cmd.Stdout = &output
	if cmd.Run() != nil {
		return "", errors.New("emergency: cannot read selected LUKS identity")
	}
	uuid := strings.ToLower(strings.TrimSuffix(string(output.data), "\n"))
	if !validLUKSUUID(uuid) {
		return "", errors.New("emergency: invalid selected LUKS identity")
	}
	return uuid, nil
}

func deviceCommand(ctx context.Context, binary string, device *os.File, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	cmd.ExtraFiles = []*os.File{device}
	cmd.WaitDelay = time.Second
	return cmd
}

type uuidOutput struct{ data []byte }

func (o *uuidOutput) Write(p []byte) (int, error) {
	if len(o.data)+len(p) > 64 {
		return 0, io.ErrShortBuffer
	}
	o.data = append(o.data, p...)
	return len(p), nil
}

func validLUKSUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (h *systemHost) erase(ctx context.Context, path string) error {
	if h.device == nil {
		return errors.New("emergency: erase requires an initially validated block device")
	}
	if err := h.checkDevice(ctx, path); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	binary, err := cryptsetupBinary()
	if err != nil {
		return err
	}
	cmd := deviceCommand(ctx, binary, h.device, "luksErase", "--batch-mode", inheritedDevicePath)
	if cmd.Run() != nil {
		return errors.New("emergency: LUKS keyslot erasure failed or timed out; inspect device state before retrying")
	}
	return nil
}
func (*systemHost) reboot() error {
	f, err := os.OpenFile("/proc/sysrq-trigger", os.O_WRONLY, 0)
	if err != nil {
		return errors.New("emergency: cannot request immediate reboot")
	}
	defer f.Close()
	if _, err = f.Write([]byte("b")); err != nil {
		return errors.New("emergency: immediate reboot request failed")
	}
	return nil
}
