//go:build linux

package storageguard

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func platformInspect(path string) (Evidence, error) {
	return inspect(path, inspector{
		mountInfo:      "/proc/self/mountinfo",
		sysRoot:        "/sys",
		cryptStatus:    verifyCryptTarget,
		filesystemUUID: inspectFilesystemUUID,
		device: func(path string) (string, error) {
			var st unix.Stat_t
			if err := unix.Stat(path, &st); err != nil {
				return "", err
			}
			return formatDevice(uint64(st.Dev)), nil
		},
	})
}

func formatDevice(device uint64) string {
	return fmt.Sprintf("%d:%d", unix.Major(device), unix.Minor(device))
}
